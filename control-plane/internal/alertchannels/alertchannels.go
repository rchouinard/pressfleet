// Package alertchannels delivers alert events (package alerts) to the
// places people read them: SMTP email and webhooks, which cover Slack,
// Teams, and anything else that accepts an HTTP POST (design doc §7.3).
//
// # Channels
//
// A channel is a managed resource, created through the API. It has a type
// ("email" or "webhook"), settings that aren't secret (SMTP host and
// recipients, the webhook format), and credentials that are: the SMTP
// password, and a webhook's URL (Slack and Teams URLs grant the right to
// post, so they are credentials) and signing secret. The credentials are
// one JSON document encrypted with the master key by package secrets, and
// the API only ever returns a redacted form.
//
// # Routing
//
// A channel receives the events of alerts at or above its minimum severity,
// and, if set, only those of one rule or one client group. By default a
// channel receives everything. Firing and resolved events are routed the
// same way.
//
// # Delivery
//
// [Service] is the [alerts.Notifier]. Notify records one delivery per
// matching enabled channel in the database and returns, which tells the
// engine the event is accepted; a worker (Run) sends the deliveries. A
// delivery that fails is retried with exponential backoff until it
// succeeds or has used its attempts, when it is marked failed and stays
// listed for the dashboard. Deliveries are unique per alert event and
// channel, so an event offered again (after a restart, say) never reaches a
// channel that already has it, and one channel's failure doesn't repeat
// the others. Failures that retrying can't fix, such as a receiver
// answering 404 or a blocked address, fail at once.
//
// Webhook targets are guarded against server-side request forgery: the
// dialer refuses loopback, link-local (cloud metadata included), and other
// local addresses after name resolution, redirects aren't followed, and
// per-channel SMTP servers are held to the same rule. The platform SMTP
// relay is configured by an administrator and isn't.
package alertchannels

import (
	"context"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/mail"
	"net/url"
	"slices"
	"strings"
	"time"
	"unicode"

	"github.com/rchouinard/pressfleet/control-plane/internal/alerts"
	"github.com/rchouinard/pressfleet/control-plane/internal/redact"
	"github.com/rchouinard/pressfleet/control-plane/internal/secrets"
	"github.com/rchouinard/pressfleet/control-plane/internal/store"
)

// Errors callers branch on.
var (
	// ErrNotFound means there is no such channel.
	ErrNotFound = errors.New("alert channel not found")
	// ErrNameTaken means another channel has the name.
	ErrNameTaken = errors.New("an alert channel with that name already exists")
)

// Limits on channels.
const (
	MaxNameLength   = 100
	MaxRecipients   = 20
	MaxURLLength    = 2048
	MaxSecretLength = 512
)

// FieldError is one thing wrong with a channel. Field is a JSON Pointer
// into the API's request body.
type FieldError struct {
	Field  string
	Detail string
}

// ValidationError lists everything wrong with a channel.
type ValidationError struct {
	Fields []FieldError
}

func (e *ValidationError) Error() string {
	parts := make([]string, len(e.Fields))
	for i, f := range e.Fields {
		parts[i] = f.Field + ": " + f.Detail
	}
	return "invalid alert channel: " + strings.Join(parts, "; ")
}

// Email is the settings of an email channel. With no Host the channel sends
// through the platform's SMTP relay (the smtp.* settings) and only the
// recipients matter; with a Host it has its own server.
type Email struct {
	Host       string   `json:"host,omitempty"`
	Port       int      `json:"port,omitempty"`
	Encryption string   `json:"encryption,omitempty"`
	Username   string   `json:"username,omitempty"`
	FromEmail  string   `json:"from_email,omitempty"`
	FromName   string   `json:"from_name,omitempty"`
	To         []string `json:"to"`
}

// Webhook is the settings of a webhook channel that aren't secret.
type Webhook struct {
	// Format is FormatGeneric (the default), FormatSlack, or FormatTeams.
	Format string `json:"format"`
}

// Secrets are the credentials of a channel, as input. A nil field leaves
// the stored value as it is, and a pointer to the empty string clears it.
type Secrets struct {
	// SMTPPassword is the password of an email channel's own SMTP server.
	SMTPPassword *redact.Secret
	// WebhookURL is the URL a webhook channel posts to.
	WebhookURL *redact.Secret
	// SigningSecret, if set, signs webhook requests (see HeaderSignature).
	SigningSecret *redact.Secret
}

// blob is what is encrypted for a channel.
type blob struct {
	SMTPPassword  string `json:"smtp_password,omitempty"`
	WebhookURL    string `json:"webhook_url,omitempty"`
	SigningSecret string `json:"signing_secret,omitempty"`
}

func (b blob) empty() bool { return b == blob{} }

func (b blob) apply(in Secrets) blob {
	if in.SMTPPassword != nil {
		b.SMTPPassword = in.SMTPPassword.Reveal()
	}
	if in.WebhookURL != nil {
		b.WebhookURL = in.WebhookURL.Reveal()
	}
	if in.SigningSecret != nil {
		b.SigningSecret = in.SigningSecret.Reveal()
	}
	return b
}

// Input is a new channel.
type Input struct {
	Name          string
	Type          store.AlertChannelType
	Enabled       bool
	MinSeverity   store.AlertSeverity
	RuleID        string
	ClientGroupID string
	// Email is required for email channels, Webhook for webhook channels.
	Email   *Email
	Webhook *Webhook
	Secrets Secrets
}

// Patch changes the fields of a channel that are set. The type can't
// change.
type Patch struct {
	Name          *string
	Enabled       *bool
	MinSeverity   *store.AlertSeverity
	RuleID        *string
	ClientGroupID *string
	// Email and Webhook change the settings of the matching type that are
	// set in them.
	Email   *EmailPatch
	Webhook *WebhookPatch
	Secrets Secrets
}

// EmailPatch changes the settings of an email channel that are set.
type EmailPatch struct {
	Host       *string
	Port       *int
	Encryption *string
	Username   *string
	FromEmail  *string
	FromName   *string
	To         *[]string
}

func (p EmailPatch) apply(e *Email) {
	if p.Host != nil {
		e.Host = *p.Host
	}
	if p.Port != nil {
		e.Port = *p.Port
	}
	if p.Encryption != nil {
		e.Encryption = *p.Encryption
	}
	if p.Username != nil {
		e.Username = *p.Username
	}
	if p.FromEmail != nil {
		e.FromEmail = *p.FromEmail
	}
	if p.FromName != nil {
		e.FromName = *p.FromName
	}
	if p.To != nil {
		e.To = slices.Clone(*p.To)
	}
}

// WebhookPatch changes the settings of a webhook channel that are set.
type WebhookPatch struct {
	Format *string
}

// Channel is a channel as the API shows it: credentials are reduced to
// whether they are set, and a webhook URL to its scheme and host.
type Channel struct {
	ID            string
	Name          string
	Type          store.AlertChannelType
	Enabled       bool
	MinSeverity   store.AlertSeverity
	RuleID        string
	ClientGroupID string
	Email         *Email
	Webhook       *Webhook
	// UsesPlatformRelay is true for an email channel without its own host.
	UsesPlatformRelay bool
	HasPassword       bool
	// WebhookURL is the redacted URL, "https://hooks.slack.com/…".
	WebhookURL       string
	HasSigningSecret bool
	CreatedAt        time.Time
	UpdatedAt        time.Time
}

// Options configures a [Service].
type Options struct {
	// DB is the state database.
	DB *store.DB
	// Secrets encrypts channel credentials.
	Secrets secrets.Store
	// PlatformSMTP is the platform's SMTP relay (the smtp.* settings), which
	// email channels without a host of their own send through. Nil if
	// outbound email isn't configured.
	PlatformSMTP *Relay
	// AllowLocalTargets lets channels connect to loopback, link-local, and
	// other local addresses (alerts.webhook_allow_local_targets).
	AllowLocalTargets bool
	// WebhookTimeout bounds one webhook request and one SMTP delivery; the
	// default applies if zero.
	WebhookTimeout time.Duration
	// MaxAttempts is how many times a delivery is tried before it is
	// marked failed; the default applies if zero.
	MaxAttempts int
	// Backoff returns the wait after a failed attempt (1 is the first);
	// DefaultBackoff if nil.
	Backoff func(attempt int) time.Duration
	// PollInterval is how often the worker looks for deliveries that are
	// due, besides when a new one is recorded; the default applies if zero.
	PollInterval time.Duration
	// RootCAs, if set, replaces the system roots for TLS connections, for
	// tests.
	RootCAs *x509.CertPool
	// Now returns the current time; time.Now if nil.
	Now func() time.Time
	// Logger is required.
	Logger *slog.Logger
}

// Defaults for [Options].
const (
	DefaultWebhookTimeout = 10 * time.Second
	DefaultMaxAttempts    = 8
	DefaultPollInterval   = 15 * time.Second
	maxBackoff            = 15 * time.Minute
)

// DefaultBackoff waits 30 seconds after the first failure and doubles the
// wait after each, up to 15 minutes: with the default eight attempts a
// delivery is given up on after about 45 minutes.
func DefaultBackoff(attempt int) time.Duration {
	d := 30 * time.Second
	for i := 1; i < attempt && d < maxBackoff; i++ {
		d *= 2
	}
	return min(d, maxBackoff)
}

// Service manages channels and delivers alert events to them. It implements
// [alerts.Notifier].
type Service struct {
	db           *store.DB
	secrets      secrets.Store
	platform     *Relay
	allowLocal   bool
	timeout      time.Duration
	maxAttempts  int
	backoff      func(int) time.Duration
	pollInterval time.Duration
	rootCAs      *x509.CertPool
	now          func() time.Time
	log          *slog.Logger
	httpClient   *http.Client
	wake         chan struct{}
}

var _ alerts.Notifier = (*Service)(nil)

// New returns a Service.
func New(opts Options) (*Service, error) {
	if opts.DB == nil || opts.Logger == nil {
		return nil, errors.New("alertchannels: DB and Logger are required")
	}
	s := &Service{
		db: opts.DB, secrets: opts.Secrets, platform: opts.PlatformSMTP, allowLocal: opts.AllowLocalTargets,
		timeout: opts.WebhookTimeout, maxAttempts: opts.MaxAttempts, backoff: opts.Backoff, pollInterval: opts.PollInterval,
		rootCAs: opts.RootCAs, now: opts.Now, log: opts.Logger, wake: make(chan struct{}, 1),
	}
	if s.timeout <= 0 {
		s.timeout = DefaultWebhookTimeout
	}
	if s.maxAttempts <= 0 {
		s.maxAttempts = DefaultMaxAttempts
	}
	if s.backoff == nil {
		s.backoff = DefaultBackoff
	}
	if s.pollInterval <= 0 {
		s.pollInterval = DefaultPollInterval
	}
	if s.now == nil {
		s.now = time.Now
	}
	s.httpClient = newHTTPClient(s.timeout, s.allowLocal, s.rootCAs)
	return s, nil
}

// RedactURL reduces a URL to its scheme and host, plus an ellipsis if
// something follows: "https://hooks.slack.com/…". Webhook URLs carry their
// credentials in the path or query.
func RedactURL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return ""
	}
	out := u.Scheme + "://" + u.Hostname()
	if p := u.Port(); p != "" {
		out += ":" + p
	}
	if (u.Path != "" && u.Path != "/") || u.RawQuery != "" || u.User != nil {
		out += "/…"
	}
	return out
}

// Create validates and stores a channel.
func (s *Service) Create(ctx context.Context, in Input) (Channel, error) {
	c := store.AlertChannel{
		Name: strings.TrimSpace(in.Name), Type: in.Type, Enabled: in.Enabled, MinSeverity: in.MinSeverity,
		RuleID: in.RuleID, ClientGroupID: in.ClientGroupID,
	}
	if c.MinSeverity == "" {
		c.MinSeverity = store.SeverityInfo
	}
	var out Channel
	err := s.db.InTx(ctx, func(q store.Querier) error {
		b := blob{}.apply(in.Secrets)
		cfg, err := s.checkAndEncode(ctx, q, &c, in.Email, in.Webhook, b)
		if err != nil {
			return err
		}
		c.Config = cfg
		if !b.empty() {
			ref, err := s.secrets.With(q).Put(ctx, secrets.KindAlertChannel, mustJSON(b))
			if err != nil {
				return err
			}
			c.SecretRef = string(ref)
		}
		if err := store.NewAlertChannels(q).Create(ctx, &c); err != nil {
			return mapStoreError(err)
		}
		out, err = view(c, b)
		return err
	})
	return out, err
}

// Get returns a channel.
func (s *Service) Get(ctx context.Context, channelID string) (Channel, error) {
	c, err := store.NewAlertChannels(s.db).Get(ctx, channelID)
	if err != nil {
		return Channel{}, mapStoreError(err)
	}
	b, err := s.openBlob(ctx, s.db, c)
	if err != nil {
		return Channel{}, err
	}
	return view(c, b)
}

// ListFilter selects and pages through channels.
type ListFilter struct {
	Type    store.AlertChannelType
	Enabled *bool
	// AfterName and AfterID are the cursor of the previous page's last
	// channel.
	AfterName, AfterID string
	Limit              int
}

// List returns channels ordered by name. A channel whose credentials can't
// be decrypted is listed without them, and logged, so one damaged secret
// doesn't hide the others.
func (s *Service) List(ctx context.Context, f ListFilter) ([]Channel, error) {
	list, err := store.NewAlertChannels(s.db).List(ctx, store.AlertChannelFilter{
		Type: f.Type, Enabled: f.Enabled, AfterName: f.AfterName, AfterID: f.AfterID, Limit: f.Limit,
	})
	if err != nil {
		return nil, err
	}
	out := make([]Channel, 0, len(list))
	for _, c := range list {
		b, err := s.openBlob(ctx, s.db, c)
		if err != nil {
			s.log.Error("decrypting the credentials of an alert channel failed", "channel_id", c.ID, "error", err)
			b = blob{}
		}
		v, err := view(c, b)
		if err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, nil
}

// Update applies a patch to a channel.
func (s *Service) Update(ctx context.Context, channelID string, p Patch) (Channel, error) {
	var out Channel
	err := s.db.InTx(ctx, func(q store.Querier) error {
		channels := store.NewAlertChannels(q)
		c, err := channels.Get(ctx, channelID)
		if err != nil {
			return mapStoreError(err)
		}
		b, err := s.openBlob(ctx, q, c)
		if err != nil {
			return err
		}
		var email *Email
		var webhook *Webhook
		if email, webhook, err = decodeConfig(c); err != nil {
			return err
		}
		if p.Name != nil {
			c.Name = strings.TrimSpace(*p.Name)
		}
		if p.Enabled != nil {
			c.Enabled = *p.Enabled
		}
		if p.MinSeverity != nil {
			c.MinSeverity = *p.MinSeverity
		}
		if p.RuleID != nil {
			c.RuleID = *p.RuleID
		}
		if p.ClientGroupID != nil {
			c.ClientGroupID = *p.ClientGroupID
		}
		if (p.Email != nil && c.Type != store.ChannelEmail) || (p.Webhook != nil && c.Type != store.ChannelWebhook) {
			return &ValidationError{Fields: []FieldError{{Field: "/type", Detail: "a channel's type can't change; the settings sent are for another type"}}}
		}
		if p.Email != nil {
			p.Email.apply(email)
		}
		if p.Webhook != nil && p.Webhook.Format != nil {
			webhook.Format = *p.Webhook.Format
		}
		b = b.apply(p.Secrets)
		cfg, err := s.checkAndEncode(ctx, q, &c, email, webhook, b)
		if err != nil {
			return err
		}
		c.Config = cfg
		oldRef := secrets.Ref(c.SecretRef)
		switch {
		case b.empty():
			c.SecretRef = ""
		case oldRef == "":
			ref, err := s.secrets.With(q).Put(ctx, secrets.KindAlertChannel, mustJSON(b))
			if err != nil {
				return err
			}
			c.SecretRef = string(ref)
		default:
			if err := s.secrets.With(q).Update(ctx, oldRef, mustJSON(b)); err != nil {
				return err
			}
		}
		if err := channels.Update(ctx, &c); err != nil {
			return mapStoreError(err)
		}
		if b.empty() && oldRef != "" {
			if err := s.secrets.With(q).Delete(ctx, oldRef); err != nil {
				return err
			}
		}
		out, err = view(c, b)
		return err
	})
	return out, err
}

// Delete removes a channel, its credentials, and its deliveries.
func (s *Service) Delete(ctx context.Context, channelID string) error {
	return s.db.InTx(ctx, func(q store.Querier) error {
		channels := store.NewAlertChannels(q)
		c, err := channels.Get(ctx, channelID)
		if err != nil {
			return mapStoreError(err)
		}
		if err := channels.Delete(ctx, channelID); err != nil {
			return mapStoreError(err)
		}
		if c.SecretRef != "" {
			return s.secrets.With(q).Delete(ctx, secrets.Ref(c.SecretRef))
		}
		return nil
	})
}

func mapStoreError(err error) error {
	switch {
	case errors.Is(err, store.ErrNotFound):
		return fmt.Errorf("%w: %w", ErrNotFound, err)
	case errors.Is(err, store.ErrConflict):
		return fmt.Errorf("%w: %w", ErrNameTaken, err)
	}
	return err
}

func mustJSON(v any) redact.Secret {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err) // a struct of strings
	}
	return redact.Secret(b)
}

// openBlob decrypts a channel's credentials; none is the empty blob.
func (s *Service) openBlob(ctx context.Context, q store.Querier, c store.AlertChannel) (blob, error) {
	if c.SecretRef == "" {
		return blob{}, nil
	}
	plain, err := s.secrets.With(q).Get(ctx, secrets.Ref(c.SecretRef))
	if err != nil {
		return blob{}, fmt.Errorf("alert channel %s credentials: %w", c.ID, err)
	}
	var b blob
	if err := json.Unmarshal([]byte(plain.Reveal()), &b); err != nil {
		return blob{}, fmt.Errorf("alert channel %s credentials: %w", c.ID, errors.New("malformed"))
	}
	return b, nil
}

func decodeConfig(c store.AlertChannel) (*Email, *Webhook, error) {
	switch c.Type {
	case store.ChannelEmail:
		var e Email
		if err := json.Unmarshal([]byte(c.Config), &e); err != nil {
			return nil, nil, fmt.Errorf("alert channel %s config: %w", c.ID, err)
		}
		return &e, nil, nil
	case store.ChannelWebhook:
		var w Webhook
		if err := json.Unmarshal([]byte(c.Config), &w); err != nil {
			return nil, nil, fmt.Errorf("alert channel %s config: %w", c.ID, err)
		}
		return nil, &w, nil
	}
	return nil, nil, fmt.Errorf("alert channel %s has unknown type %q", c.ID, c.Type)
}

func view(c store.AlertChannel, b blob) (Channel, error) {
	email, webhook, err := decodeConfig(c)
	if err != nil {
		return Channel{}, err
	}
	out := Channel{
		ID: c.ID, Name: c.Name, Type: c.Type, Enabled: c.Enabled, MinSeverity: c.MinSeverity, RuleID: c.RuleID,
		ClientGroupID: c.ClientGroupID, Email: email, Webhook: webhook, CreatedAt: c.CreatedAt, UpdatedAt: c.UpdatedAt,
		HasPassword: b.SMTPPassword != "", HasSigningSecret: b.SigningSecret != "", WebhookURL: RedactURL(b.WebhookURL),
	}
	if email != nil {
		out.UsesPlatformRelay = email.Host == ""
		if email.To == nil {
			email.To = []string{}
		}
	}
	return out, nil
}

// checkAndEncode validates a channel with its settings and credentials and
// returns the settings as JSON. It normalizes c and the settings (defaults,
// trimmed addresses).
func (s *Service) checkAndEncode(ctx context.Context, q store.Querier, c *store.AlertChannel, email *Email, webhook *Webhook, b blob) (string, error) {
	v := &validation{}
	if c.Name == "" || len([]rune(c.Name)) > MaxNameLength {
		v.add("/name", fmt.Sprintf("must be 1 to %d characters", MaxNameLength))
	}
	if !c.Type.Valid() {
		v.add("/type", `must be "email" or "webhook"`)
	}
	if !c.MinSeverity.Valid() {
		v.add("/min_severity", `must be "info", "warning", or "critical"`)
	}
	if c.RuleID != "" {
		if _, err := store.NewAlertRules(q).Get(ctx, c.RuleID); errors.Is(err, store.ErrNotFound) {
			v.add("/rule_id", "no such alert rule")
		} else if err != nil {
			return "", err
		}
	}
	if c.ClientGroupID != "" {
		if _, err := store.NewClientGroups(q).Get(ctx, c.ClientGroupID); errors.Is(err, store.ErrNotFound) {
			v.add("/client_group_id", "no such client group")
		} else if err != nil {
			return "", err
		}
	}
	var config any
	switch c.Type {
	case store.ChannelEmail:
		if email == nil {
			v.add("/email", "is required for an email channel")
			break
		}
		if webhook != nil || b.WebhookURL != "" || b.SigningSecret != "" {
			v.add("/webhook", "doesn't apply to an email channel")
		}
		e := *email
		s.checkEmail(v, &e, b)
		config = e
	case store.ChannelWebhook:
		if webhook == nil {
			v.add("/webhook", "is required for a webhook channel")
			break
		}
		if email != nil || b.SMTPPassword != "" {
			v.add("/email", "doesn't apply to a webhook channel")
		}
		w := *webhook
		s.checkWebhook(v, &w, b)
		config = w
	}
	if len(v.fields) > 0 {
		return "", &ValidationError{Fields: v.fields}
	}
	enc, err := json.Marshal(config)
	if err != nil {
		return "", err
	}
	return string(enc), nil
}

type validation struct{ fields []FieldError }

func (v *validation) add(field, detail string) {
	v.fields = append(v.fields, FieldError{field, detail})
}

func (s *Service) checkEmail(v *validation, e *Email, b blob) {
	e.Host = strings.TrimSpace(e.Host)
	e.Username = strings.TrimSpace(e.Username)
	e.FromEmail = strings.TrimSpace(e.FromEmail)
	if e.To == nil {
		e.To = []string{}
	}
	if len(e.To) == 0 || len(e.To) > MaxRecipients {
		v.add("/email/to", fmt.Sprintf("must list 1 to %d recipients", MaxRecipients))
	}
	for i, to := range e.To {
		e.To[i] = strings.TrimSpace(to)
		if !plainAddress(e.To[i]) {
			v.add("/email/to", fmt.Sprintf("%q is not an email address (use user@example.com, without a display name)", to))
		}
	}
	if e.FromEmail != "" && !plainAddress(e.FromEmail) {
		v.add("/email/from_email", "must be an email address (user@example.com)")
	}
	if len([]rune(e.FromName)) > MaxNameLength || strings.ContainsFunc(e.FromName, unicode.IsControl) {
		v.add("/email/from_name", "must be at most 100 characters, without line breaks")
	}
	if e.Host == "" {
		// The platform relay: nothing else about the server may be set.
		if s.platform == nil {
			v.add("/email/host", "is required: the platform SMTP relay (smtp.host) isn't configured")
		} else if e.FromEmail == "" && s.platform.FromEmail == "" {
			v.add("/email/from_email", "is required: the platform SMTP relay has no sender (smtp.from_email)")
		}
		if e.Port != 0 || e.Encryption != "" || e.Username != "" || b.SMTPPassword != "" {
			v.add("/email/host", "is required to set the port, encryption, username, or password of a server of the channel's own")
		}
		return
	}
	if strings.ContainsAny(e.Host, " /\\@:?#[]") || len(e.Host) > 253 {
		// A bare IPv6 address isn't accepted here; use a name.
		if net.ParseIP(e.Host) == nil {
			v.add("/email/host", "must be a host name or IP address, without a scheme or port")
		}
	}
	if err := checkLiteralHost(e.Host, s.allowLocal); err != nil {
		v.add("/email/host", err.Error())
	}
	if e.Port < 0 || e.Port > 65535 {
		v.add("/email/port", "must be between 1 and 65535, or 0 for the standard port")
	}
	switch e.Encryption {
	case "":
		e.Encryption = EncryptionSTARTTLS
	case EncryptionSTARTTLS, EncryptionTLS, EncryptionNone:
	default:
		v.add("/email/encryption", `must be "starttls", "tls", or "none"`)
	}
	if e.FromEmail == "" {
		v.add("/email/from_email", "is required with an SMTP server of the channel's own")
	}
	if (e.Username == "") != (b.SMTPPassword == "") {
		v.add("/email/username", "and the password must be set together, or both left empty")
	}
	if len(b.SMTPPassword) > MaxSecretLength {
		v.add("/email/password", "is too long")
	}
}

func (s *Service) checkWebhook(v *validation, w *Webhook, b blob) {
	switch w.Format {
	case "":
		w.Format = FormatGeneric
	case FormatGeneric, FormatSlack, FormatTeams:
	default:
		v.add("/webhook/format", `must be "generic", "slack", or "teams"`)
	}
	if b.WebhookURL == "" {
		v.add("/webhook/url", "is required")
	} else if err := s.checkWebhookURL(b.WebhookURL); err != nil {
		v.add("/webhook/url", err.Error())
	}
	if len(b.SigningSecret) > MaxSecretLength {
		v.add("/webhook/signing_secret", "is too long")
	}
}

// checkWebhookURL returns why raw can't be a webhook URL. Its message never
// repeats the URL, which is a credential.
func (s *Service) checkWebhookURL(raw string) error {
	if len(raw) > MaxURLLength {
		return fmt.Errorf("must be at most %d characters", MaxURLLength)
	}
	u, err := url.Parse(raw)
	switch {
	case err != nil || u.Host == "":
		return errors.New("must be an absolute http or https URL")
	case u.Scheme != "https" && u.Scheme != "http":
		return errors.New("must use http or https")
	case strings.ContainsFunc(raw, unicode.IsSpace):
		return errors.New("must not contain spaces")
	}
	return checkLiteralHost(u.Hostname(), s.allowLocal)
}

// plainAddress reports whether s is a bare email address.
func plainAddress(s string) bool {
	a, err := mail.ParseAddress(s)
	return err == nil && a.Address == s && !strings.ContainsAny(s, "\r\n")
}
