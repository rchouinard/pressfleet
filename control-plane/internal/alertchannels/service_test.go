package alertchannels_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"mime"
	"mime/multipart"
	"mime/quotedprintable"
	"net/http"
	"net/mail"
	"strings"
	"testing"
	"time"

	"github.com/rchouinard/pressfleet/control-plane/internal/alertchannels"
	"github.com/rchouinard/pressfleet/control-plane/internal/alertchannels/alertchannelstest"
	"github.com/rchouinard/pressfleet/control-plane/internal/alerts"
	"github.com/rchouinard/pressfleet/control-plane/internal/id"
	"github.com/rchouinard/pressfleet/control-plane/internal/redact"
	"github.com/rchouinard/pressfleet/control-plane/internal/secrets"
	"github.com/rchouinard/pressfleet/control-plane/internal/secrets/secretstest"
	"github.com/rchouinard/pressfleet/control-plane/internal/store"
	"github.com/rchouinard/pressfleet/control-plane/internal/store/storetest"
)

// newService returns a service on db that may connect to loopback (the
// fakes listen there) and retries at once. mutate adjusts the options.
func newService(t *testing.T, db *store.DB, mutate func(*alertchannels.Options)) *alertchannels.Service {
	t.Helper()
	opts := alertchannels.Options{
		DB:                db,
		Secrets:           secrets.NewStore(db, secretstest.Keyring(t, 1)),
		AllowLocalTargets: true,
		WebhookTimeout:    5 * time.Second,
		MaxAttempts:       3,
		Backoff:           func(int) time.Duration { return time.Millisecond },
		PollInterval:      5 * time.Millisecond,
		Logger:            slog.New(slog.DiscardHandler),
	}
	if mutate != nil {
		mutate(&opts)
	}
	s, err := alertchannels.New(opts)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// runWorker runs the delivery worker until the test ends.
func runWorker(t *testing.T, s *alertchannels.Service) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); _ = s.Run(ctx) }()
	t.Cleanup(func() { cancel(); <-done })
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func secret(s string) *redact.Secret { r := redact.Secret(s); return &r }

func webhookInput(name, url string) alertchannels.Input {
	return alertchannels.Input{
		Name: name, Type: store.ChannelWebhook, Enabled: true,
		Webhook: &alertchannels.Webhook{}, Secrets: alertchannels.Secrets{WebhookURL: secret(url)},
	}
}

func emailInput(name string, to ...string) alertchannels.Input {
	return alertchannels.Input{Name: name, Type: store.ChannelEmail, Enabled: true, Email: &alertchannels.Email{To: to}}
}

// events makes alerts of a rule and their events.
type fixture struct {
	t  *testing.T
	db *store.DB
}

func (f fixture) rule(name, group string) store.AlertRule {
	f.t.Helper()
	r := store.AlertRule{
		Name: name, Scope: store.AlertScopeSite, Kind: store.AlertKindExpression, Expr: "x", Severity: store.SeverityCritical,
		Enabled: true, ClientGroupID: group,
	}
	if err := store.NewAlertRules(f.db).Create(context.Background(), &r); err != nil {
		f.t.Fatal(err)
	}
	return r
}

func (f fixture) group(name string) string {
	f.t.Helper()
	g := store.ClientGroup{Name: name}
	if err := store.NewClientGroups(f.db).Create(context.Background(), &g); err != nil {
		f.t.Fatal(err)
	}
	return g.ID
}

// firing inserts a firing alert and returns its firing event.
func (f fixture) firing(rule store.AlertRule, severity store.AlertSeverity, group string) alerts.Event {
	f.t.Helper()
	now := time.Now().UTC().Truncate(time.Second)
	a := store.Alert{
		RuleID: rule.ID, Fingerprint: id.New(), Labels: map[string]string{"site_id": "site-1", "team": "<ops>"},
		SiteID: "site-1", ClientGroupID: group, State: store.AlertFiring, Severity: severity,
		Summary: "Site example.com is down", Description: "The HTTP check failed for 2 minutes.", Value: 0,
		StartedAt: now.Add(-2 * time.Minute), FiredAt: &now,
	}
	if err := store.NewAlerts(f.db).Insert(context.Background(), &a); err != nil {
		f.t.Fatal(err)
	}
	return alerts.Event{Type: store.AlertEventFiring, Alert: a, Rule: rule}
}

// resolved resolves the alert of a firing event and returns the resolved
// event.
func (f fixture) resolved(e alerts.Event) alerts.Event {
	f.t.Helper()
	a := e.Alert
	if err := store.NewAlerts(f.db).Resolve(context.Background(), &a, time.Now().UTC()); err != nil {
		f.t.Fatal(err)
	}
	return alerts.Event{Type: store.AlertEventResolved, Alert: a, Rule: e.Rule}
}

func deliveries(t *testing.T, db *store.DB, f store.AlertDeliveryFilter) []store.AlertDelivery {
	t.Helper()
	list, err := store.NewAlertDeliveries(db).List(context.Background(), f)
	if err != nil {
		t.Fatal(err)
	}
	return list
}

func allDone(t *testing.T, db *store.DB, want int, state store.DeliveryState) func() bool {
	return func() bool {
		n := 0
		for _, d := range deliveries(t, db, store.AlertDeliveryFilter{}) {
			if d.State == state {
				n++
			}
		}
		return n == want
	}
}

// parseMail splits a received message into headers and its two parts.
func parseMail(t *testing.T, data string) (hdr mail.Header, text, html string) {
	t.Helper()
	m, err := mail.ReadMessage(strings.NewReader(data))
	if err != nil {
		t.Fatalf("parsing the message: %v\n%s", err, data)
	}
	mt, params, err := mime.ParseMediaType(m.Header.Get("Content-Type"))
	if err != nil || mt != "multipart/alternative" {
		t.Fatalf("Content-Type = %q, %v", m.Header.Get("Content-Type"), err)
	}
	mr := multipart.NewReader(m.Body, params["boundary"])
	for {
		p, err := mr.NextPart()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		body, err := io.ReadAll(quotedprintable.NewReader(p))
		if err != nil {
			t.Fatal(err)
		}
		switch ct, _, _ := mime.ParseMediaType(p.Header.Get("Content-Type")); ct {
		case "text/plain":
			text = string(body)
		case "text/html":
			html = string(body)
		}
	}
	return m.Header, text, html
}

func TestFiringAndResolvedReachBothChannelTypes(t *testing.T) {
	ctx := context.Background()
	storetest.Run(t, func(t *testing.T, db *store.DB) {
		smtpd := alertchannelstest.NewSMTPServer(t, alertchannelstest.SMTPSTARTTLS)
		hook := alertchannelstest.NewWebhook(t)
		svc := newService(t, db, func(o *alertchannels.Options) { o.RootCAs = smtpd.RootCAs })
		runWorker(t, svc)
		fx := fixture{t, db}

		_, err := svc.Create(ctx, alertchannels.Input{
			Name: "Ops email", Type: store.ChannelEmail, Enabled: true,
			Email: &alertchannels.Email{
				Host: smtpd.Host, Port: smtpd.Port, Username: "relay", FromEmail: "alerts@example.com", FromName: "PressFleet",
				To: []string{"ops@example.com", "oncall@example.com"},
			},
			Secrets: alertchannels.Secrets{SMTPPassword: secret("smtp-pass")},
		})
		if err != nil {
			t.Fatal(err)
		}
		in := webhookInput("Hook", hook.URL)
		in.Secrets.SigningSecret = secret("shh")
		if _, err := svc.Create(ctx, in); err != nil {
			t.Fatal(err)
		}

		fire := fx.firing(fx.rule("Site down", ""), store.SeverityCritical, "")
		if err := svc.Notify(ctx, fire); err != nil {
			t.Fatal(err)
		}
		waitFor(t, "firing deliveries", allDone(t, db, 2, store.DeliveryDelivered))

		// Email: encrypted, authenticated, both parts, the right fields.
		mails := smtpd.Mails()
		if len(mails) != 1 {
			t.Fatalf("mails = %d, want 1", len(mails))
		}
		m := mails[0]
		if !m.Encrypted || m.Username != "relay" || m.Password != "smtp-pass" || m.From != "alerts@example.com" ||
			strings.Join(m.To, ",") != "ops@example.com,oncall@example.com" {
			t.Errorf("mail envelope = %+v", m)
		}
		hdr, text, html := parseMail(t, m.Data)
		if got := hdr.Get("Subject"); got != "[PressFleet] [FIRING] critical: Site example.com is down" {
			t.Errorf("Subject = %q", got)
		}
		if got := hdr.Get("From"); got != `"PressFleet" <alerts@example.com>` {
			t.Errorf("From = %q", got)
		}
		for _, want := range []string{"[FIRING] critical: Site example.com is down", "The HTTP check failed", "Site down", "site-1", "team = <ops>"} {
			if !strings.Contains(text, want) {
				t.Errorf("text part lacks %q:\n%s", want, text)
			}
		}
		if !strings.Contains(html, "<html>") || !strings.Contains(html, "&lt;ops&gt;") || strings.Contains(html, "<ops>") {
			t.Errorf("html part isn't escaped HTML:\n%s", html)
		}

		// Webhook: stable generic payload, signed.
		reqs := hook.Requests()
		if len(reqs) != 1 {
			t.Fatalf("webhook requests = %d, want 1", len(reqs))
		}
		r := reqs[0]
		if r.Method != http.MethodPost || r.Path != "/hook/secret-token" || r.Header.Get("Content-Type") != "application/json" ||
			r.Header.Get(alertchannels.HeaderEvent) != "firing" || r.Header.Get(alertchannels.HeaderDelivery) != fire.Key() {
			t.Errorf("request = %+v", r)
		}
		if want := alertchannels.Sign("shh", r.Header.Get(alertchannels.HeaderTimestamp), r.Body); r.Header.Get(alertchannels.HeaderSignature) != want {
			t.Errorf("signature = %q, want %q", r.Header.Get(alertchannels.HeaderSignature), want)
		}
		var payload struct {
			Version int    `json:"version"`
			Event   string `json:"event"`
			Key     string `json:"key"`
			Text    string `json:"text"`
			Alert   struct {
				ID       string            `json:"id"`
				RuleName string            `json:"rule_name"`
				State    string            `json:"state"`
				Severity string            `json:"severity"`
				SiteID   string            `json:"site_id"`
				Labels   map[string]string `json:"labels"`
				FiredAt  *time.Time        `json:"fired_at"`
				Resolved *time.Time        `json:"resolved_at"`
			} `json:"alert"`
		}
		if err := json.Unmarshal(r.Body, &payload); err != nil {
			t.Fatal(err)
		}
		if payload.Version != 1 || payload.Event != "firing" || payload.Key != fire.Key() || !strings.HasPrefix(payload.Text, "[FIRING] critical: Site example.com is down") ||
			payload.Alert.ID != fire.Alert.ID || payload.Alert.RuleName != "Site down" || payload.Alert.State != "firing" ||
			payload.Alert.SiteID != "site-1" || payload.Alert.Labels["team"] != "<ops>" || payload.Alert.FiredAt == nil || payload.Alert.Resolved != nil {
			t.Errorf("payload = %s", r.Body)
		}

		// Resolved goes to both, too.
		res := fx.resolved(fire)
		if err := svc.Notify(ctx, res); err != nil {
			t.Fatal(err)
		}
		waitFor(t, "resolved deliveries", allDone(t, db, 4, store.DeliveryDelivered))
		if mails = smtpd.Mails(); len(mails) != 2 {
			t.Fatalf("mails = %d, want 2", len(mails))
		}
		if hdr, _, _ := parseMail(t, mails[1].Data); !strings.HasPrefix(hdr.Get("Subject"), "[PressFleet] [RESOLVED] critical:") {
			t.Errorf("resolved Subject = %q", hdr.Get("Subject"))
		}
		reqs = hook.Requests()
		if len(reqs) != 2 || reqs[1].Header.Get(alertchannels.HeaderEvent) != "resolved" {
			t.Fatalf("webhook requests = %d", len(reqs))
		}
		var resolved struct {
			Event string `json:"event"`
			Alert struct {
				State      string     `json:"state"`
				ResolvedAt *time.Time `json:"resolved_at"`
			} `json:"alert"`
		}
		if err := json.Unmarshal(reqs[1].Body, &resolved); err != nil || resolved.Event != "resolved" || resolved.Alert.State != "resolved" || resolved.Alert.ResolvedAt == nil {
			t.Errorf("resolved payload = %s (%v)", reqs[1].Body, err)
		}

		// Offering an event again (a restart, a retried engine pass) doesn't
		// send it again.
		for _, e := range []alerts.Event{fire, res} {
			if err := svc.Notify(ctx, e); err != nil {
				t.Fatal(err)
			}
		}
		time.Sleep(50 * time.Millisecond)
		if len(smtpd.Mails()) != 2 || len(hook.Requests()) != 2 || len(deliveries(t, db, store.AlertDeliveryFilter{})) != 4 {
			t.Errorf("an event offered twice was delivered twice: %d mails, %d requests", len(smtpd.Mails()), len(hook.Requests()))
		}

		// The delivery history says so, with the channel names.
		for _, d := range deliveries(t, db, store.AlertDeliveryFilter{AlertID: fire.Alert.ID}) {
			if d.Attempts != 1 || d.DeliveredAt == nil || d.LastError != "" || (d.ChannelName != "Ops email" && d.ChannelName != "Hook") {
				t.Errorf("delivery = %+v", d)
			}
		}
	})
}

func TestEmailTransports(t *testing.T) {
	ctx := context.Background()
	storetest.Run(t, func(t *testing.T, db *store.DB) {
		fx := fixture{t, db}
		fire := fx.firing(fx.rule("Site down", ""), store.SeverityWarning, "")

		t.Run("implicit TLS", func(t *testing.T) {
			smtpd := alertchannelstest.NewSMTPServer(t, alertchannelstest.SMTPImplicitTLS)
			svc := newService(t, db, func(o *alertchannels.Options) { o.RootCAs = smtpd.RootCAs })
			ch, err := svc.Create(ctx, alertchannels.Input{
				Name: "tls", Type: store.ChannelEmail, Enabled: true,
				Email: &alertchannels.Email{Host: smtpd.Host, Port: smtpd.Port, Encryption: "tls", FromEmail: "a@example.com", To: []string{"b@example.com"}},
			})
			if err != nil {
				t.Fatal(err)
			}
			res, err := svc.Test(ctx, ch.ID)
			if err != nil || !res.Success {
				t.Fatalf("Test = %+v, %v", res, err)
			}
			if m := smtpd.Mails(); len(m) != 1 || !m[0].Encrypted {
				t.Errorf("mails = %+v", m)
			}
		})

		t.Run("none", func(t *testing.T) {
			smtpd := alertchannelstest.NewSMTPServer(t, alertchannelstest.SMTPPlain)
			svc := newService(t, db, nil)
			ch, err := svc.Create(ctx, alertchannels.Input{
				Name: "plain", Type: store.ChannelEmail, Enabled: true,
				Email:   &alertchannels.Email{Host: smtpd.Host, Port: smtpd.Port, Encryption: "none", Username: "u", FromEmail: "a@example.com", To: []string{"b@example.com"}},
				Secrets: alertchannels.Secrets{SMTPPassword: secret("p")},
			})
			if err != nil {
				t.Fatal(err)
			}
			if res, err := svc.Test(ctx, ch.ID); err != nil || !res.Success {
				t.Fatalf("Test = %+v, %v", res, err)
			}
			if m := smtpd.Mails(); len(m) != 1 || m[0].Encrypted || m[0].Username != "u" || m[0].Password != "p" {
				t.Errorf("mails = %+v", m)
			}
		})

		t.Run("STARTTLS is required", func(t *testing.T) {
			smtpd := alertchannelstest.NewSMTPServer(t, alertchannelstest.SMTPPlain) // doesn't offer it
			svc := newService(t, db, nil)
			ch, err := svc.Create(ctx, alertchannels.Input{
				Name: "needs starttls", Type: store.ChannelEmail, Enabled: true,
				Email: &alertchannels.Email{Host: smtpd.Host, Port: smtpd.Port, FromEmail: "a@example.com", To: []string{"b@example.com"}},
			})
			if err != nil {
				t.Fatal(err)
			}
			res, err := svc.Test(ctx, ch.ID)
			if err != nil || res.Success || !strings.Contains(res.Error, "STARTTLS") {
				t.Errorf("Test = %+v, %v", res, err)
			}
			if len(smtpd.Mails()) != 0 {
				t.Error("a message was sent without encryption")
			}
		})

		t.Run("platform relay", func(t *testing.T) {
			smtpd := alertchannelstest.NewSMTPServer(t, alertchannelstest.SMTPSTARTTLS)
			svc := newService(t, db, func(o *alertchannels.Options) {
				o.RootCAs = smtpd.RootCAs
				o.PlatformSMTP = &alertchannels.Relay{
					Host: smtpd.Host, Port: smtpd.Port, Encryption: "starttls", Username: "platform", Password: "platform-pass",
					FromEmail: "pressfleet@example.com", FromName: "PressFleet",
				}
			})
			ch, err := svc.Create(ctx, emailInput("platform", "ops@example.com"))
			if err != nil {
				t.Fatal(err)
			}
			if !ch.UsesPlatformRelay {
				t.Error("channel without a host doesn't use the platform relay")
			}
			runWorker(t, svc)
			if err := svc.Notify(ctx, fire); err != nil {
				t.Fatal(err)
			}
			waitFor(t, "the delivery", func() bool { return len(smtpd.Mails()) == 1 })
			m := smtpd.Mails()[0]
			if m.From != "pressfleet@example.com" || m.Username != "platform" || m.Password != "platform-pass" || !m.Encrypted {
				t.Errorf("mail = %+v", m)
			}
		})

		t.Run("no platform relay", func(t *testing.T) {
			svc := newService(t, db, nil)
			_, err := svc.Create(ctx, emailInput("nowhere", "ops@example.com"))
			var ve *alertchannels.ValidationError
			if !errors.As(err, &ve) || ve.Fields[0].Field != "/email/host" {
				t.Errorf("Create without a host and without a platform relay = %v", err)
			}
		})
	})
}

func TestRouting(t *testing.T) {
	ctx := context.Background()
	storetest.Run(t, func(t *testing.T, db *store.DB) {
		hooks := map[string]*alertchannelstest.Webhook{}
		svc := newService(t, db, nil)
		fx := fixture{t, db}
		groupA, groupB := fx.group("A"), fx.group("B")
		down, disk := fx.rule("Site down", ""), fx.rule("Disk", "")

		create := func(name string, mutate func(*alertchannels.Input)) {
			hooks[name] = alertchannelstest.NewWebhook(t)
			in := webhookInput(name, hooks[name].URL)
			mutate(&in)
			if _, err := svc.Create(ctx, in); err != nil {
				t.Fatal(err)
			}
		}
		create("all", func(*alertchannels.Input) {})
		create("critical only", func(in *alertchannels.Input) { in.MinSeverity = store.SeverityCritical })
		create("warning up", func(in *alertchannels.Input) { in.MinSeverity = store.SeverityWarning })
		create("down rule", func(in *alertchannels.Input) { in.RuleID = down.ID })
		create("group A", func(in *alertchannels.Input) { in.ClientGroupID = groupA })
		create("disabled", func(in *alertchannels.Input) { in.Enabled = false })

		got := func(e alerts.Event) map[string]bool {
			t.Helper()
			before := len(deliveries(t, db, store.AlertDeliveryFilter{}))
			if err := svc.Notify(ctx, e); err != nil {
				t.Fatal(err)
			}
			out := map[string]bool{}
			for _, d := range deliveries(t, db, store.AlertDeliveryFilter{AlertID: e.Alert.ID}) {
				out[d.ChannelName] = true
			}
			if len(deliveries(t, db, store.AlertDeliveryFilter{}))-before != len(out) {
				t.Errorf("deliveries = %v", out)
			}
			return out
		}
		same := func(got map[string]bool, want ...string) {
			t.Helper()
			if len(got) != len(want) {
				t.Errorf("channels = %v, want %v", got, want)
				return
			}
			for _, w := range want {
				if !got[w] {
					t.Errorf("channels = %v, want %v", got, want)
				}
			}
		}
		same(got(fx.firing(down, store.SeverityCritical, groupA)), "all", "critical only", "warning up", "down rule", "group A")
		same(got(fx.firing(disk, store.SeverityWarning, groupB)), "all", "warning up")
		same(got(fx.firing(disk, store.SeverityInfo, "")), "all")
		same(got(fx.firing(down, store.SeverityInfo, groupB)), "all", "down rule")

		// A channel gets an alert of a rule limited to its group, too.
		limited := fx.rule("Limited", groupA)
		same(got(fx.firing(limited, store.SeverityInfo, "")), "all", "group A")
	})
}

func mustList(t *testing.T, svc *alertchannels.Service) []alertchannels.Channel {
	t.Helper()
	list, err := svc.List(context.Background(), alertchannels.ListFilter{})
	if err != nil {
		t.Fatal(err)
	}
	return list
}

func TestNotifyWithoutChannels(t *testing.T) {
	storetest.Run(t, func(t *testing.T, db *store.DB) {
		svc := newService(t, db, nil)
		fx := fixture{t, db}
		if err := svc.Notify(context.Background(), fx.firing(fx.rule("Down", ""), store.SeverityInfo, "")); err != nil {
			t.Errorf("Notify with no channels = %v", err)
		}
		if n := len(deliveries(t, db, store.AlertDeliveryFilter{})); n != 0 {
			t.Errorf("deliveries = %d", n)
		}
	})
}

// TestRetries checks that a failing channel is retried without resending to
// the channels that succeeded, gives up after the attempt cap, and is
// reported with its error.
func TestRetries(t *testing.T) {
	ctx := context.Background()
	storetest.Run(t, func(t *testing.T, db *store.DB) {
		good, flaky, broken, gone := alertchannelstest.NewWebhook(t), alertchannelstest.NewWebhook(t), alertchannelstest.NewWebhook(t), alertchannelstest.NewWebhook(t)
		flaky.SetStatus(503)
		broken.SetStatus(500)
		gone.SetStatus(404)
		svc := newService(t, db, nil)
		for name, h := range map[string]*alertchannelstest.Webhook{"good": good, "flaky": flaky, "broken": broken, "gone": gone} {
			in := webhookInput(name, h.URL)
			in.Secrets.SigningSecret = secret("signing-secret-value")
			if _, err := svc.Create(ctx, in); err != nil {
				t.Fatal(err)
			}
		}
		fx := fixture{t, db}
		fire := fx.firing(fx.rule("Site down", ""), store.SeverityCritical, "")
		if err := svc.Notify(ctx, fire); err != nil {
			t.Errorf("Notify = %v; a failing channel must not make the engine offer the event again", err)
		}
		runWorker(t, svc)

		byName := func() map[string]store.AlertDelivery {
			out := map[string]store.AlertDelivery{}
			for _, d := range deliveries(t, db, store.AlertDeliveryFilter{}) {
				out[d.ChannelName] = d
			}
			return out
		}
		waitFor(t, "the cap", func() bool {
			d := byName()
			return d["good"].State == store.DeliveryDelivered && d["flaky"].State == store.DeliveryFailed &&
				d["broken"].State == store.DeliveryFailed && d["gone"].State == store.DeliveryFailed
		})
		d := byName()
		if n := len(good.Requests()); n != 1 || d["good"].Attempts != 1 {
			t.Errorf("good channel got %d requests, %d attempts; want 1", n, d["good"].Attempts)
		}
		// MaxAttempts is 3 in these tests; a 404 can't succeed, so it isn't retried.
		if n := len(flaky.Requests()); n != 3 || d["flaky"].Attempts != 3 {
			t.Errorf("flaky channel got %d requests, %d attempts; want 3", n, d["flaky"].Attempts)
		}
		if n := len(broken.Requests()); n != 3 {
			t.Errorf("broken channel got %d requests, want 3", n)
		}
		if n := len(gone.Requests()); n != 1 || d["gone"].Attempts != 1 {
			t.Errorf("gone channel got %d requests, %d attempts; want 1", n, d["gone"].Attempts)
		}
		if got := d["flaky"].LastError; !strings.Contains(got, "HTTP 503") {
			t.Errorf("LastError = %q", got)
		}
		for name, del := range d {
			if strings.Contains(del.LastError, "secret-token") || strings.Contains(del.LastError, "signing-secret-value") || strings.Contains(del.LastError, "127.0.0.1") {
				t.Errorf("%s: LastError leaks the URL or secret: %q", name, del.LastError)
			}
		}

		// A new offer of the same event doesn't revive failed deliveries or
		// resend delivered ones.
		if err := svc.Notify(ctx, fire); err != nil {
			t.Fatal(err)
		}
		time.Sleep(50 * time.Millisecond)
		if len(good.Requests()) != 1 || len(flaky.Requests()) != 3 {
			t.Error("a delivery was repeated")
		}
	})
}

// TestRecoveryAfterFailure checks that a channel that comes back gets the
// event on a retry, once.
func TestRecoveryAfterFailure(t *testing.T) {
	ctx := context.Background()
	storetest.Run(t, func(t *testing.T, db *store.DB) {
		hook := alertchannelstest.NewWebhook(t)
		hook.SetStatus(502)
		svc := newService(t, db, func(o *alertchannels.Options) {
			o.MaxAttempts = 50
			o.Backoff = func(int) time.Duration { return 20 * time.Millisecond }
		})
		if _, err := svc.Create(ctx, webhookInput("hook", hook.URL)); err != nil {
			t.Fatal(err)
		}
		fx := fixture{t, db}
		if err := svc.Notify(ctx, fx.firing(fx.rule("Down", ""), store.SeverityCritical, "")); err != nil {
			t.Fatal(err)
		}
		runWorker(t, svc)
		waitFor(t, "a failed attempt", func() bool {
			d := deliveries(t, db, store.AlertDeliveryFilter{})
			return len(d) == 1 && d[0].Attempts >= 1 && d[0].State == store.DeliveryPending && d[0].LastError != ""
		})
		hook.SetStatus(200)
		waitFor(t, "delivery", allDone(t, db, 1, store.DeliveryDelivered))
		d := deliveries(t, db, store.AlertDeliveryFilter{})[0]
		if d.LastError != "" || d.DeliveredAt == nil || d.Attempts < 2 {
			t.Errorf("delivery = %+v", d)
		}
		n := len(hook.Requests())
		time.Sleep(100 * time.Millisecond)
		if len(hook.Requests()) != n {
			t.Error("a delivered event was sent again")
		}
	})
}

func TestDisabledChannelAfterQueueing(t *testing.T) {
	ctx := context.Background()
	storetest.Run(t, func(t *testing.T, db *store.DB) {
		hook := alertchannelstest.NewWebhook(t)
		svc := newService(t, db, nil)
		ch, err := svc.Create(ctx, webhookInput("hook", hook.URL))
		if err != nil {
			t.Fatal(err)
		}
		fx := fixture{t, db}
		if err := svc.Notify(ctx, fx.firing(fx.rule("Down", ""), store.SeverityCritical, "")); err != nil {
			t.Fatal(err)
		}
		off := false
		if _, err := svc.Update(ctx, ch.ID, alertchannels.Patch{Enabled: &off}); err != nil {
			t.Fatal(err)
		}
		runWorker(t, svc)
		waitFor(t, "the delivery to fail", allDone(t, db, 1, store.DeliveryFailed))
		if len(hook.Requests()) != 0 {
			t.Error("a disabled channel was sent to")
		}
		if d := deliveries(t, db, store.AlertDeliveryFilter{})[0]; !strings.Contains(d.LastError, "disabled") {
			t.Errorf("LastError = %q", d.LastError)
		}
	})
}

func TestTestSend(t *testing.T) {
	ctx := context.Background()
	storetest.Run(t, func(t *testing.T, db *store.DB) {
		hook := alertchannelstest.NewWebhook(t)
		svc := newService(t, db, nil)
		ch, err := svc.Create(ctx, webhookInput("hook", hook.URL))
		if err != nil {
			t.Fatal(err)
		}
		res, err := svc.Test(ctx, ch.ID)
		if err != nil || !res.Success || res.Error != "" {
			t.Fatalf("Test = %+v, %v", res, err)
		}
		reqs := hook.Requests()
		if len(reqs) != 1 || reqs[0].Header.Get(alertchannels.HeaderEvent) != "test" {
			t.Fatalf("requests = %+v", reqs)
		}
		var body struct {
			Event string `json:"event"`
			Text  string `json:"text"`
		}
		if err := json.Unmarshal(reqs[0].Body, &body); err != nil || body.Event != "test" || !strings.HasPrefix(body.Text, "[TEST]") {
			t.Errorf("body = %s", reqs[0].Body)
		}

		// A failing receiver is a result, not an error, and a disabled
		// channel can be tested.
		hook.SetStatus(500)
		off := false
		if _, err := svc.Update(ctx, ch.ID, alertchannels.Patch{Enabled: &off}); err != nil {
			t.Fatal(err)
		}
		res, err = svc.Test(ctx, ch.ID)
		if err != nil || res.Success || !strings.Contains(res.Error, "HTTP 500") || strings.Contains(res.Error, "secret-token") {
			t.Errorf("Test of a failing channel = %+v, %v", res, err)
		}
		if n := len(deliveries(t, db, store.AlertDeliveryFilter{})); n != 0 {
			t.Errorf("a test left %d deliveries", n)
		}
		if _, err := svc.Test(ctx, id.New()); !errors.Is(err, alertchannels.ErrNotFound) {
			t.Errorf("Test of a missing channel = %v", err)
		}
	})
}

func TestSSRFProtection(t *testing.T) {
	ctx := context.Background()
	storetest.Run(t, func(t *testing.T, db *store.DB) {
		hook := alertchannelstest.NewWebhook(t)
		svc := newService(t, db, func(o *alertchannels.Options) { o.AllowLocalTargets = false })

		// Obviously bad addresses are refused when the channel is saved.
		for _, u := range []string{"http://127.0.0.1:9/x", "http://[::1]/x", "http://169.254.169.254/latest/meta-data", "http://0.0.0.0/x"} {
			_, err := svc.Create(ctx, webhookInput("bad", u))
			var ve *alertchannels.ValidationError
			if !errors.As(err, &ve) || ve.Fields[0].Field != "/webhook/url" || strings.Contains(ve.Error(), "meta-data") {
				t.Errorf("Create(%s) = %v", u, err)
			}
		}
		// Other schemes and shapes too.
		for _, u := range []string{"ftp://example.com/x", "example.com/x", "https://", "http://exa mple.com/"} {
			if _, err := svc.Create(ctx, webhookInput("bad", u)); err == nil {
				t.Errorf("Create(%s) succeeded", u)
			}
		}

		// A name that resolves to loopback passes validation but is refused
		// when connecting, and the error doesn't repeat the URL.
		localhost := strings.Replace(hook.URL, "127.0.0.1", "localhost", 1)
		ch, err := svc.Create(ctx, webhookInput("by name", localhost))
		if err != nil {
			t.Fatal(err)
		}
		res, err := svc.Test(ctx, ch.ID)
		if err != nil || res.Success || !strings.Contains(res.Error, "not allowed") || strings.Contains(res.Error, "secret-token") {
			t.Errorf("Test = %+v, %v", res, err)
		}
		if len(hook.Requests()) != 0 {
			t.Error("a blocked address was connected to")
		}

		// Per-channel SMTP servers are held to the same rule.
		smtpd := alertchannelstest.NewSMTPServer(t, alertchannelstest.SMTPPlain)
		if _, err := svc.Create(ctx, alertchannels.Input{
			Name: "smtp", Type: store.ChannelEmail, Enabled: true,
			Email: &alertchannels.Email{Host: smtpd.Host, Port: smtpd.Port, Encryption: "none", FromEmail: "a@example.com", To: []string{"b@example.com"}},
		}); err == nil {
			t.Error("an SMTP server on loopback was accepted")
		}
	})
}

func TestRedirectsAreNotFollowed(t *testing.T) {
	ctx := context.Background()
	storetest.Run(t, func(t *testing.T, db *store.DB) {
		target, hook := alertchannelstest.NewWebhook(t), alertchannelstest.NewWebhook(t)
		hook.SetStatus(302)
		hook.SetRedirect(target.URL)
		svc := newService(t, db, nil)
		ch, err := svc.Create(ctx, webhookInput("redirecting", hook.URL))
		if err != nil {
			t.Fatal(err)
		}
		res, err := svc.Test(ctx, ch.ID)
		if err != nil || res.Success || !strings.Contains(res.Error, "redirect") {
			t.Errorf("Test = %+v, %v", res, err)
		}
		if len(target.Requests()) != 0 {
			t.Error("the redirect was followed")
		}
	})
}

func TestChannelsAreRedactedAndEncrypted(t *testing.T) {
	ctx := context.Background()
	const url = "https://hooks.slack.com/services/T000/B000/XXXXSECRET"
	storetest.Run(t, func(t *testing.T, db *store.DB) {
		svc := newService(t, db, nil)
		in := webhookInput("Slack", url)
		in.Webhook.Format = alertchannels.FormatSlack
		in.Secrets.SigningSecret = secret("signing-secret")
		ch, err := svc.Create(ctx, in)
		if err != nil {
			t.Fatal(err)
		}
		mail, err := svc.Create(ctx, alertchannels.Input{
			Name: "Own SMTP", Type: store.ChannelEmail, Enabled: true,
			Email:   &alertchannels.Email{Host: "smtp.example.com", Username: "u", FromEmail: "a@example.com", To: []string{"b@example.com"}},
			Secrets: alertchannels.Secrets{SMTPPassword: secret("smtp-password")},
		})
		if err != nil {
			t.Fatal(err)
		}

		if ch.WebhookURL != "https://hooks.slack.com/…" || !ch.HasSigningSecret || ch.Webhook.Format != "slack" || ch.Email != nil {
			t.Errorf("webhook channel = %+v", ch)
		}
		if !mail.HasPassword || mail.UsesPlatformRelay || mail.Email.Encryption != "starttls" || mail.WebhookURL != "" {
			t.Errorf("email channel = %+v", mail)
		}
		for _, v := range []any{ch, mail, mustList(t, svc)} {
			if out := strings.Join([]string{jsonOf(t, v)}, ""); strings.Contains(out, "XXXXSECRET") || strings.Contains(out, "signing-secret") || strings.Contains(out, "smtp-password") {
				t.Errorf("a view leaks a credential: %s", out)
			}
		}

		// At rest: not in the channel row, and the secret row is ciphertext.
		var config, ref string
		if err := db.QueryRowContext(ctx, "SELECT config, secret_ref FROM alert_channels WHERE id = ?", ch.ID).Scan(&config, &ref); err != nil {
			t.Fatal(err)
		}
		var ciphertext []byte
		var kind string
		if err := db.QueryRowContext(ctx, "SELECT kind, ciphertext FROM secrets WHERE id = ?", ref).Scan(&kind, &ciphertext); err != nil {
			t.Fatal(err)
		}
		if strings.Contains(config, "XXXXSECRET") || strings.Contains(string(ciphertext), "XXXXSECRET") || strings.Contains(string(ciphertext), "hooks.slack.com") || kind != "alert-channel-credentials" {
			t.Errorf("stored config = %q, kind = %q, ciphertext contains the URL", config, kind)
		}

		// Updating other fields keeps the credentials; a new URL replaces
		// it; an empty string removes the signing secret.
		name := "Slack #ops"
		got, err := svc.Update(ctx, ch.ID, alertchannels.Patch{Name: &name})
		if err != nil || got.WebhookURL != "https://hooks.slack.com/…" || !got.HasSigningSecret || got.Name != name {
			t.Errorf("Update name = %+v, %v", got, err)
		}
		got, err = svc.Update(ctx, ch.ID, alertchannels.Patch{Secrets: alertchannels.Secrets{SigningSecret: secret(""), WebhookURL: secret("https://chat.example.com:8443/hook/abc?token=1")}})
		if err != nil || got.WebhookURL != "https://chat.example.com:8443/…" || got.HasSigningSecret {
			t.Errorf("Update credentials = %+v, %v", got, err)
		}
		// The URL can't be removed from a webhook channel.
		if _, err := svc.Update(ctx, ch.ID, alertchannels.Patch{Secrets: alertchannels.Secrets{WebhookURL: secret("")}}); err == nil {
			t.Error("removed the URL of a webhook channel")
		}

		// Deleting a channel deletes its secret.
		var before int
		_ = db.QueryRowContext(ctx, "SELECT COUNT(*) FROM secrets WHERE kind = 'alert-channel-credentials'").Scan(&before)
		if err := svc.Delete(ctx, ch.ID); err != nil {
			t.Fatal(err)
		}
		var after int
		_ = db.QueryRowContext(ctx, "SELECT COUNT(*) FROM secrets WHERE kind = 'alert-channel-credentials'").Scan(&after)
		if before != 2 || after != 1 {
			t.Errorf("secrets before and after delete = %d, %d; want 2, 1", before, after)
		}
		if _, err := svc.Get(ctx, ch.ID); !errors.Is(err, alertchannels.ErrNotFound) {
			t.Errorf("Get after delete = %v", err)
		}
		if err := svc.Delete(ctx, ch.ID); !errors.Is(err, alertchannels.ErrNotFound) {
			t.Errorf("Delete twice = %v", err)
		}
	})
}

func jsonOf(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestChannelValidation(t *testing.T) {
	ctx := context.Background()
	storetest.Run(t, func(t *testing.T, db *store.DB) {
		svc := newService(t, db, func(o *alertchannels.Options) {
			o.PlatformSMTP = &alertchannels.Relay{Host: "relay.example.com", FromEmail: "pf@example.com"}
		})
		valid := emailInput("ok", "ops@example.com")
		if _, err := svc.Create(ctx, valid); err != nil {
			t.Fatal(err)
		}
		if _, err := svc.Create(ctx, valid); !errors.Is(err, alertchannels.ErrNameTaken) {
			t.Errorf("duplicate name = %v", err)
		}

		tests := []struct {
			name   string
			mutate func(*alertchannels.Input)
			field  string
		}{
			{"empty name", func(in *alertchannels.Input) { in.Name = " " }, "/name"},
			{"unknown type", func(in *alertchannels.Input) { in.Type = "sms" }, "/type"},
			{"unknown severity", func(in *alertchannels.Input) { in.MinSeverity = "urgent" }, "/min_severity"},
			{"missing rule", func(in *alertchannels.Input) { in.RuleID = id.New() }, "/rule_id"},
			{"missing group", func(in *alertchannels.Input) { in.ClientGroupID = id.New() }, "/client_group_id"},
			{"no email settings", func(in *alertchannels.Input) { in.Email = nil }, "/email"},
			{"no recipients", func(in *alertchannels.Input) { in.Email.To = nil }, "/email/to"},
			{"display name recipient", func(in *alertchannels.Input) { in.Email.To = []string{"Ops <ops@example.com>"} }, "/email/to"},
			{"header injection", func(in *alertchannels.Input) { in.Email.To = []string{"a@example.com\r\nBcc: x@example.com"} }, "/email/to"},
			{"too many recipients", func(in *alertchannels.Input) {
				in.Email.To = nil
				for range 21 {
					in.Email.To = append(in.Email.To, "a@example.com")
				}
			}, "/email/to"},
			{"bad encryption", func(in *alertchannels.Input) {
				in.Email.Host, in.Email.FromEmail, in.Email.Encryption = "smtp.example.com", "a@example.com", "ssl"
			}, "/email/encryption"},
			{"own host needs a sender", func(in *alertchannels.Input) { in.Email.Host = "smtp.example.com" }, "/email/from_email"},
			{"username without password", func(in *alertchannels.Input) {
				in.Email.Host, in.Email.FromEmail, in.Email.Username = "smtp.example.com", "a@example.com", "u"
			}, "/email/username"},
			{"port without host", func(in *alertchannels.Input) { in.Email.Port = 25 }, "/email/host"},
			{"bad port", func(in *alertchannels.Input) {
				in.Email.Host, in.Email.FromEmail, in.Email.Port = "smtp.example.com", "a@example.com", 70000
			}, "/email/port"},
			{"host with a scheme", func(in *alertchannels.Input) {
				in.Email.Host, in.Email.FromEmail = "smtp://example.com", "a@example.com"
			}, "/email/host"},
			{"webhook settings on email", func(in *alertchannels.Input) { in.Webhook = &alertchannels.Webhook{} }, "/webhook"},
		}
		for _, tt := range tests {
			t.Run(tt.name, func(t *testing.T) {
				in := emailInput("x", "ops@example.com")
				tt.mutate(&in)
				_, err := svc.Create(ctx, in)
				var ve *alertchannels.ValidationError
				if !errors.As(err, &ve) {
					t.Fatalf("Create = %v, want a validation error", err)
				}
				found := false
				for _, f := range ve.Fields {
					found = found || f.Field == tt.field
				}
				if !found {
					t.Errorf("fields = %+v, want one for %s", ve.Fields, tt.field)
				}
			})
		}

		webhookTests := []struct {
			name   string
			mutate func(*alertchannels.Input)
			field  string
		}{
			{"no URL", func(in *alertchannels.Input) { in.Secrets.WebhookURL = nil }, "/webhook/url"},
			{"no settings", func(in *alertchannels.Input) { in.Webhook = nil }, "/webhook"},
			{"bad format", func(in *alertchannels.Input) { in.Webhook.Format = "discord" }, "/webhook/format"},
			{"password on a webhook", func(in *alertchannels.Input) { in.Secrets.SMTPPassword = secret("x") }, "/email"},
			{"long URL", func(in *alertchannels.Input) {
				in.Secrets.WebhookURL = secret("https://example.com/" + strings.Repeat("a", 2100))
			}, "/webhook/url"},
		}
		for _, tt := range webhookTests {
			t.Run(tt.name, func(t *testing.T) {
				in := webhookInput("w", "https://example.com/hook")
				tt.mutate(&in)
				_, err := svc.Create(ctx, in)
				var ve *alertchannels.ValidationError
				if !errors.As(err, &ve) || ve.Fields[0].Field != tt.field {
					t.Errorf("Create = %v, want a validation error for %s", err, tt.field)
				}
			})
		}

		// The type of a channel can't change, and updates are validated.
		ch, err := svc.Create(ctx, webhookInput("typed", "https://example.com/hook"))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := svc.Update(ctx, ch.ID, alertchannels.Patch{Email: &alertchannels.EmailPatch{}}); err == nil {
			t.Error("email settings accepted for a webhook channel")
		}
		bad := "not a format"
		if _, err := svc.Update(ctx, ch.ID, alertchannels.Patch{Webhook: &alertchannels.WebhookPatch{Format: &bad}}); err == nil {
			t.Error("a bad format was accepted on update")
		}
		if got, err := svc.Get(ctx, ch.ID); err != nil || got.Webhook.Format != "generic" {
			t.Errorf("channel after failed updates = %+v, %v", got, err)
		}
		other := "ok"
		if _, err := svc.Update(ctx, ch.ID, alertchannels.Patch{Name: &other}); !errors.Is(err, alertchannels.ErrNameTaken) {
			t.Errorf("rename to a taken name = %v", err)
		}
	})
}

func TestListPaging(t *testing.T) {
	ctx := context.Background()
	storetest.Run(t, func(t *testing.T, db *store.DB) {
		svc := newService(t, db, nil)
		for _, n := range []string{"b", "a", "c"} {
			if _, err := svc.Create(ctx, webhookInput(n, "https://example.com/"+n)); err != nil {
				t.Fatal(err)
			}
		}
		off := false
		ch, err := svc.Create(ctx, webhookInput("d", "https://example.com/d"))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := svc.Update(ctx, ch.ID, alertchannels.Patch{Enabled: &off}); err != nil {
			t.Fatal(err)
		}
		all := mustList(t, svc)
		var names []string
		for _, c := range all {
			names = append(names, c.Name)
		}
		if strings.Join(names, "") != "abcd" {
			t.Errorf("names = %v", names)
		}
		page, err := svc.List(ctx, alertchannels.ListFilter{AfterName: "a", AfterID: all[0].ID, Limit: 2})
		if err != nil || len(page) != 2 || page[0].Name != "b" || page[1].Name != "c" {
			t.Errorf("page = %+v, %v", page, err)
		}
		disabled, err := svc.List(ctx, alertchannels.ListFilter{Enabled: &off})
		if err != nil || len(disabled) != 1 || disabled[0].Name != "d" {
			t.Errorf("disabled = %+v, %v", disabled, err)
		}
		if got, err := svc.List(ctx, alertchannels.ListFilter{Type: store.ChannelEmail}); err != nil || len(got) != 0 {
			t.Errorf("email channels = %+v, %v", got, err)
		}
	})
}
