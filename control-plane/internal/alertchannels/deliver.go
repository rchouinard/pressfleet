package alertchannels

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/rchouinard/pressfleet/control-plane/internal/alerts"
	"github.com/rchouinard/pressfleet/control-plane/internal/redact"
	"github.com/rchouinard/pressfleet/control-plane/internal/store"
)

// maxConcurrent bounds the deliveries in flight, so one slow receiver
// doesn't hold up the others for long and a burst of alerts doesn't open a
// burst of connections.
const maxConcurrent = 4

// batchSize is how many due deliveries one pass takes.
const batchSize = 100

// maxErrorLength bounds the error text kept with a delivery.
const maxErrorLength = 500

// permanentError marks a failure that trying again won't fix.
type permanentError struct{ err error }

func (e permanentError) Error() string { return e.err.Error() }
func (e permanentError) Unwrap() error { return e.err }

func permanent(err error) error { return permanentError{err} }

// matches reports whether a channel receives the event.
func matches(c store.AlertChannel, e alerts.Event) bool {
	if store.SeverityRank(e.Alert.Severity) < store.SeverityRank(c.MinSeverity) {
		return false
	}
	if c.RuleID != "" && c.RuleID != e.Rule.ID {
		return false
	}
	if g := c.ClientGroupID; g != "" && g != e.Alert.ClientGroupID && g != e.Rule.ClientGroupID {
		return false
	}
	return true
}

// Notify implements [alerts.Notifier]. It records a delivery of the event
// for every enabled channel that matches it, and wakes the worker. Once
// recorded the event is the service's to deliver, retries included, so
// Notify returns an error only if the database can't be written, which
// makes the engine offer the event again. An event no channel wants is
// accepted too. Offering an event twice records nothing twice.
func (s *Service) Notify(ctx context.Context, e alerts.Event) error {
	enabled := true
	channels, err := store.NewAlertChannels(s.db).List(ctx, store.AlertChannelFilter{Enabled: &enabled})
	if err != nil {
		return fmt.Errorf("notify: %w", err)
	}
	msg := newMessage(e)
	snapshot, err := json.Marshal(msg)
	if err != nil {
		return fmt.Errorf("notify: %w", err)
	}
	var queued int
	err = s.db.InTx(ctx, func(q store.Querier) error {
		deliveries := store.NewAlertDeliveries(q)
		for _, c := range channels {
			if !matches(c, e) {
				continue
			}
			if _, err := deliveries.Enqueue(ctx, e.Alert.ID, e.Type, c.ID, string(snapshot)); err != nil {
				if errors.Is(err, store.ErrReference) {
					// The channel or the alert was deleted since it was
					// listed; nothing to deliver there.
					continue
				}
				return err
			}
			queued++
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("notify: %w", err)
	}
	s.log.Info("alert event routed", "event", string(e.Type), "alert_id", e.Alert.ID, "rule", e.Rule.Name,
		"severity", string(e.Alert.Severity), "site_id", e.Alert.SiteID, "summary", e.Alert.Summary, "channels", queued)
	if queued > 0 {
		select {
		case s.wake <- struct{}{}:
		default:
		}
	}
	return nil
}

// Run delivers pending deliveries until ctx ends: when Notify records one,
// when one comes due for a retry, and at start, which picks up what a
// restart interrupted.
func (s *Service) Run(ctx context.Context) error {
	ticker := time.NewTicker(s.pollInterval)
	defer ticker.Stop()
	for {
		s.deliverDue(ctx)
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
		case <-s.wake:
		}
	}
}

// deliverDue makes one attempt at every delivery that is due.
func (s *Service) deliverDue(ctx context.Context) {
	for ctx.Err() == nil {
		due, err := store.NewAlertDeliveries(s.db).Due(ctx, s.now(), batchSize)
		if err != nil {
			if ctx.Err() == nil {
				s.log.Error("listing alert deliveries that are due failed", "error", err)
			}
			return
		}
		if len(due) == 0 {
			return
		}
		var wg sync.WaitGroup
		sem := make(chan struct{}, maxConcurrent)
		for _, d := range due {
			sem <- struct{}{}
			wg.Go(func() {
				defer func() { <-sem }()
				s.attempt(ctx, d)
			})
		}
		wg.Wait()
		if len(due) < batchSize {
			return
		}
	}
}

// attempt tries one delivery and records the outcome.
func (s *Service) attempt(ctx context.Context, d store.AlertDelivery) {
	deliveries := store.NewAlertDeliveries(s.db)
	log := s.log.With("delivery_id", d.ID, "alert_id", d.AlertID, "event", string(d.EventType), "channel_id", d.ChannelID, "channel", d.ChannelName)
	attempts := d.Attempts + 1
	var err error
	ch, gerr := store.NewAlertChannels(s.db).Get(ctx, d.ChannelID)
	switch {
	case gerr != nil:
		if ctx.Err() != nil || errors.Is(gerr, store.ErrNotFound) {
			return // shutting down, or the channel went away with its deliveries
		}
		err = gerr
	case !ch.Enabled:
		err = permanent(errors.New("the channel was disabled before the event could be delivered"))
	default:
		var msg Message
		if jerr := json.Unmarshal([]byte(d.Event), &msg); jerr != nil {
			err = permanent(fmt.Errorf("decode the stored event: %w", jerr))
		} else {
			msg.SentAt = s.now()
			err = s.send(ctx, ch, msg)
		}
	}
	if ctx.Err() != nil {
		return // interrupted by shutdown: not the channel's fault, so the attempt doesn't count
	}
	at := s.now()
	if err == nil {
		if err := deliveries.MarkDelivered(ctx, d.ID, attempts, at); err != nil && !errors.Is(err, store.ErrNotFound) {
			log.Error("recording an alert delivery failed; it may be sent again", "error", err)
			return
		}
		log.Info("alert delivered", "attempts", attempts)
		return
	}
	var next time.Time
	var perm permanentError
	if !errors.As(err, &perm) && attempts < s.maxAttempts {
		next = at.Add(s.backoff(attempts))
	}
	text := truncate(err.Error(), maxErrorLength)
	if merr := deliveries.MarkAttemptFailed(ctx, d.ID, attempts, text, next, at); merr != nil && !errors.Is(merr, store.ErrNotFound) {
		log.Error("recording a failed alert delivery failed", "error", merr)
		return
	}
	if next.IsZero() {
		log.Error("alert delivery failed for good", "attempts", attempts, "error", text)
	} else {
		log.Warn("alert delivery failed; it will be retried", "attempts", attempts, "retry_at", next, "error", text)
	}
}

func truncate(s string, n int) string {
	if r := []rune(s); len(r) > n {
		return string(r[:n]) + "…"
	}
	return s
}

// send delivers a message through the channel. The error never contains the
// channel's credentials.
func (s *Service) send(ctx context.Context, c store.AlertChannel, m Message) error {
	ctx, cancel := context.WithTimeout(ctx, s.timeout)
	defer cancel()
	b, err := s.openBlob(ctx, s.db, c)
	if err != nil {
		return err
	}
	err = s.sendWith(ctx, c, b, m)
	return scrub(err, b)
}

func (s *Service) sendWith(ctx context.Context, c store.AlertChannel, b blob, m Message) error {
	email, webhook, err := decodeConfig(c)
	if err != nil {
		return permanent(err)
	}
	switch c.Type {
	case store.ChannelEmail:
		relay := Relay{}
		dialer := guardedDialer(s.timeout, s.allowLocal)
		if email.Host != "" {
			relay = Relay{
				Host: email.Host, Port: email.Port, Encryption: email.Encryption, Username: email.Username,
				Password: redact.Secret(b.SMTPPassword), FromEmail: email.FromEmail, FromName: email.FromName,
			}
		} else {
			if s.platform == nil {
				return permanent(errors.New("the platform SMTP relay (smtp.host) isn't configured"))
			}
			relay = *s.platform
			if email.FromEmail != "" {
				relay.FromEmail, relay.FromName = email.FromEmail, email.FromName
			}
			dialer = guardedDialer(s.timeout, true) // set up by an administrator
		}
		return s.sendEmail(ctx, relay, email.To, m, dialer)
	case store.ChannelWebhook:
		return s.sendWebhook(ctx, b.WebhookURL, webhook.Format, b.SigningSecret, m)
	}
	return permanent(fmt.Errorf("unknown channel type %q", c.Type))
}

// scrub removes the channel's credentials from an error message, in case a
// library repeated one.
func scrub(err error, b blob) error {
	if err == nil {
		return nil
	}
	msg := err.Error()
	clean := msg
	for _, secret := range []string{b.WebhookURL, b.SMTPPassword, b.SigningSecret} {
		if secret != "" {
			clean = strings.ReplaceAll(clean, secret, "[REDACTED]")
		}
	}
	if clean == msg {
		return err
	}
	var perm permanentError
	if errors.As(err, &perm) {
		return permanent(errors.New(clean))
	}
	return errors.New(clean)
}

// TestResult is the outcome of a test send.
type TestResult struct {
	// Success is whether the channel accepted the message.
	Success bool
	// Error says why it didn't; it never contains credentials.
	Error    string
	Duration time.Duration
}

// Test sends a synthetic notification through a channel, whether or not it
// is enabled, and reports what happened. A failed send is a result, not an
// error; the error is for channels that don't exist or can't be read.
func (s *Service) Test(ctx context.Context, channelID string) (TestResult, error) {
	c, err := store.NewAlertChannels(s.db).Get(ctx, channelID)
	if err != nil {
		return TestResult{}, mapStoreError(err)
	}
	start := s.now()
	msg := testMessage(c.Name, start)
	err = s.send(ctx, c, msg)
	res := TestResult{Success: err == nil, Duration: s.now().Sub(start)}
	if err != nil {
		if ctx.Err() != nil {
			return TestResult{}, ctx.Err()
		}
		res.Error = truncate(err.Error(), maxErrorLength)
		s.log.Warn("alert channel test failed", "channel_id", c.ID, "channel", c.Name, "error", res.Error)
		return res, nil
	}
	s.log.Info("alert channel test sent", "channel_id", c.ID, "channel", c.Name)
	return res, nil
}
