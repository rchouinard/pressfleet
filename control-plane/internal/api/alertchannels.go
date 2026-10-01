package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"slices"

	"github.com/rchouinard/pressfleet/control-plane/internal/alertchannels"
	"github.com/rchouinard/pressfleet/control-plane/internal/api/gen"
	"github.com/rchouinard/pressfleet/control-plane/internal/authz"
	"github.com/rchouinard/pressfleet/control-plane/internal/httpserver"
	"github.com/rchouinard/pressfleet/control-plane/internal/id"
	"github.com/rchouinard/pressfleet/control-plane/internal/redact"
	"github.com/rchouinard/pressfleet/control-plane/internal/store"
)

// Alert channels (#76, design doc §7.3). Channels are platform settings
// (design doc §4.6): Admins manage them, and every role can read them, with
// credentials reduced to whether they are set. Delivery is package
// alertchannels'.

var errNoAlertChannels = errors.New("api: no alert channel service configured")

// ListAlertChannels lists alert channels by name. The cursor encodes the
// name and ID of the last channel on the previous page.
func (h *Handlers) ListAlertChannels(ctx context.Context, req gen.ListAlertChannelsRequestObject) (gen.ListAlertChannelsResponseObject, error) {
	if _, err := h.authorize(ctx, authz.ViewMonitoring, authz.Platform()); err != nil {
		return nil, err
	}
	if h.channels == nil {
		return nil, errNoAlertChannels
	}
	p := req.Params
	limit := pageLimit(p.Limit)
	f := alertchannels.ListFilter{Type: store.AlertChannelType(deref(p.Type)), Enabled: p.Enabled, Limit: limit + 1}
	if c := p.Cursor; c != nil {
		var ok bool
		if f.AfterName, f.AfterID, ok = decodeNameCursor(*c); !ok {
			return nil, httpserver.NewProblem(http.StatusBadRequest, "The cursor is invalid; pass the next_cursor of a previous page.")
		}
	}
	list, err := h.channels.List(ctx, f)
	if err != nil {
		return nil, err
	}
	list, next := page(list, limit, func(c alertchannels.Channel) string { return encodeNameCursor(c.Name, c.ID) })
	body := gen.AlertChannelList{Items: make([]gen.AlertChannel, len(list)), NextCursor: next}
	for i, c := range list {
		body.Items[i] = alertChannel(c)
	}
	return gen.ListAlertChannels200JSONResponse(body), nil
}

// CreateAlertChannel creates an alert channel.
func (h *Handlers) CreateAlertChannel(ctx context.Context, req gen.CreateAlertChannelRequestObject) (gen.CreateAlertChannelResponseObject, error) {
	if _, err := h.authorize(ctx, authz.ManagePlatformSettings, authz.Platform()); err != nil {
		return nil, err
	}
	if h.channels == nil {
		return nil, errNoAlertChannels
	}
	b := req.Body
	in := alertchannels.Input{
		Name:          b.Name,
		Type:          store.AlertChannelType(b.Type),
		Enabled:       b.Enabled == nil || *b.Enabled,
		MinSeverity:   store.AlertSeverity(deref(b.MinSeverity)),
		RuleID:        deref(b.RuleID),
		ClientGroupID: deref(b.ClientGroupID),
	}
	if e := b.Email; e != nil {
		in.Email = &alertchannels.Email{
			Host: deref(e.Host), Port: deref(e.Port), Username: deref(e.Username),
			FromEmail: deref(e.FromEmail), FromName: deref(e.FromName), To: slices.Clone(e.To),
		}
		if e.Encryption != nil {
			in.Email.Encryption = string(*e.Encryption)
		}
		in.Secrets.SMTPPassword = secretPtr(e.Password)
	}
	if w := b.Webhook; w != nil {
		in.Webhook = &alertchannels.Webhook{Format: string(deref(w.Format))}
		in.Secrets.WebhookURL = secretPtr(w.URL)
		in.Secrets.SigningSecret = secretPtr(w.SigningSecret)
	}
	c, err := h.channels.Create(ctx, in)
	if err != nil {
		return nil, alertChannelError(err, in.Name)
	}
	setAuditTarget(ctx, authz.AlertChannel(c.ID))
	location := "/api/v1/alert-channels/" + c.ID
	return gen.CreateAlertChannel201JSONResponse{
		Body:    alertChannel(c),
		Headers: gen.CreateAlertChannel201ResponseHeaders{Location: &location},
	}, nil
}

// GetAlertChannel returns an alert channel.
func (h *Handlers) GetAlertChannel(ctx context.Context, req gen.GetAlertChannelRequestObject) (gen.GetAlertChannelResponseObject, error) {
	if _, err := h.authorize(ctx, authz.ViewMonitoring, authz.AlertChannel(req.AlertChannelID)); err != nil {
		return nil, err
	}
	if h.channels == nil {
		return nil, errNoAlertChannels
	}
	c, err := h.channels.Get(ctx, req.AlertChannelID)
	if err != nil {
		return nil, alertChannelError(err, "")
	}
	return gen.GetAlertChannel200JSONResponse(alertChannel(c)), nil
}

// UpdateAlertChannel changes the fields of an alert channel that are
// present.
func (h *Handlers) UpdateAlertChannel(ctx context.Context, req gen.UpdateAlertChannelRequestObject) (gen.UpdateAlertChannelResponseObject, error) {
	if _, err := h.authorize(ctx, authz.ManagePlatformSettings, authz.AlertChannel(req.AlertChannelID)); err != nil {
		return nil, err
	}
	if h.channels == nil {
		return nil, errNoAlertChannels
	}
	b := req.Body
	p := alertchannels.Patch{Name: b.Name, RuleID: b.RuleID, ClientGroupID: b.ClientGroupID}
	if b.MinSeverity != nil {
		sev := store.AlertSeverity(*b.MinSeverity)
		p.MinSeverity = &sev
	}
	if e := b.Email; e != nil {
		p.Email = &alertchannels.EmailPatch{
			Host: e.Host, Port: e.Port, Username: e.Username, FromEmail: e.FromEmail, FromName: e.FromName, To: e.To,
		}
		if e.Encryption != nil {
			enc := string(*e.Encryption)
			p.Email.Encryption = &enc
		}
		p.Secrets.SMTPPassword = secretPtr(e.Password)
	}
	if w := b.Webhook; w != nil {
		p.Webhook = &alertchannels.WebhookPatch{}
		if w.Format != nil {
			f := string(*w.Format)
			p.Webhook.Format = &f
		}
		p.Secrets.WebhookURL = secretPtr(w.URL)
		p.Secrets.SigningSecret = secretPtr(w.SigningSecret)
	}
	c, err := h.channels.Update(ctx, req.AlertChannelID, p)
	if err != nil {
		return nil, alertChannelError(err, deref(b.Name))
	}
	return gen.UpdateAlertChannel200JSONResponse(alertChannel(c)), nil
}

// DeleteAlertChannel deletes an alert channel with its credentials and
// delivery history.
func (h *Handlers) DeleteAlertChannel(ctx context.Context, req gen.DeleteAlertChannelRequestObject) (gen.DeleteAlertChannelResponseObject, error) {
	if _, err := h.authorize(ctx, authz.ManagePlatformSettings, authz.AlertChannel(req.AlertChannelID)); err != nil {
		return nil, err
	}
	if h.channels == nil {
		return nil, errNoAlertChannels
	}
	if err := h.channels.Delete(ctx, req.AlertChannelID); err != nil {
		return nil, alertChannelError(err, "")
	}
	return gen.DeleteAlertChannel204Response{}, nil
}

// EnableAlertChannel enables an alert channel.
func (h *Handlers) EnableAlertChannel(ctx context.Context, req gen.EnableAlertChannelRequestObject) (gen.EnableAlertChannelResponseObject, error) {
	c, err := h.setAlertChannelEnabled(ctx, req.AlertChannelID, true)
	if err != nil {
		return nil, err
	}
	return gen.EnableAlertChannel200JSONResponse(alertChannel(c)), nil
}

// DisableAlertChannel disables an alert channel.
func (h *Handlers) DisableAlertChannel(ctx context.Context, req gen.DisableAlertChannelRequestObject) (gen.DisableAlertChannelResponseObject, error) {
	c, err := h.setAlertChannelEnabled(ctx, req.AlertChannelID, false)
	if err != nil {
		return nil, err
	}
	return gen.DisableAlertChannel200JSONResponse(alertChannel(c)), nil
}

func (h *Handlers) setAlertChannelEnabled(ctx context.Context, channelID string, enabled bool) (alertchannels.Channel, error) {
	if _, err := h.authorize(ctx, authz.ManagePlatformSettings, authz.AlertChannel(channelID)); err != nil {
		return alertchannels.Channel{}, err
	}
	if h.channels == nil {
		return alertchannels.Channel{}, errNoAlertChannels
	}
	c, err := h.channels.Update(ctx, channelID, alertchannels.Patch{Enabled: &enabled})
	if err != nil {
		return alertchannels.Channel{}, alertChannelError(err, "")
	}
	return c, nil
}

// TestAlertChannel sends a test notification through an alert channel.
func (h *Handlers) TestAlertChannel(ctx context.Context, req gen.TestAlertChannelRequestObject) (gen.TestAlertChannelResponseObject, error) {
	if _, err := h.authorize(ctx, authz.ManagePlatformSettings, authz.AlertChannel(req.AlertChannelID)); err != nil {
		return nil, err
	}
	if h.channels == nil {
		return nil, errNoAlertChannels
	}
	res, err := h.channels.Test(ctx, req.AlertChannelID)
	if err != nil {
		return nil, alertChannelError(err, "")
	}
	return gen.TestAlertChannel200JSONResponse{
		Success: res.Success, Error: optional(res.Error), DurationMs: res.Duration.Milliseconds(),
	}, nil
}

// ListAlertDeliveries lists the delivery history of alert events, newest
// first. The cursor is the ID of the last delivery on the previous page.
func (h *Handlers) ListAlertDeliveries(ctx context.Context, req gen.ListAlertDeliveriesRequestObject) (gen.ListAlertDeliveriesResponseObject, error) {
	if _, err := h.authorize(ctx, authz.ViewMonitoring, authz.Platform()); err != nil {
		return nil, err
	}
	if h.db == nil {
		return nil, errNoDatabase
	}
	p := req.Params
	limit := pageLimit(p.Limit)
	f := store.AlertDeliveryFilter{
		AlertID: deref(p.AlertID), ChannelID: deref(p.ChannelID), State: store.DeliveryState(deref(p.State)), Limit: limit + 1,
	}
	if p.Cursor != nil {
		if !id.Valid(*p.Cursor) {
			return nil, httpserver.NewProblem(http.StatusBadRequest, "The cursor is invalid; pass the next_cursor of a previous page.")
		}
		f.Before = *p.Cursor
	}
	list, err := store.NewAlertDeliveries(h.db).List(ctx, f)
	if err != nil {
		return nil, err
	}
	list, next := page(list, limit, func(d store.AlertDelivery) string { return d.ID })
	body := gen.AlertDeliveryList{Items: make([]gen.AlertDelivery, len(list)), NextCursor: next}
	for i, d := range list {
		body.Items[i] = alertDelivery(d)
	}
	return gen.ListAlertDeliveries200JSONResponse(body), nil
}

// secretPtr wraps a request's credential. Nil stays nil, which leaves the
// stored value alone; the empty string removes it.
func secretPtr(s *string) *redact.Secret {
	if s == nil {
		return nil
	}
	v := redact.Secret(*s)
	return &v
}

// alertChannelError maps service errors to API errors.
func alertChannelError(err error, name string) error {
	var ve *alertchannels.ValidationError
	switch {
	case errors.As(err, &ve):
		out := &ValidationError{Fields: make([]httpserver.FieldError, len(ve.Fields))}
		for i, f := range ve.Fields {
			out.Fields[i] = httpserver.FieldError{Field: f.Field, Detail: f.Detail}
		}
		return out
	case errors.Is(err, alertchannels.ErrNotFound):
		return fmt.Errorf("alert channel: %w", ErrNotFound)
	case errors.Is(err, alertchannels.ErrNameTaken):
		return fmt.Errorf("%w: an alert channel named %q already exists", ErrConflict, name)
	}
	return err
}

func alertChannel(c alertchannels.Channel) gen.AlertChannel {
	out := gen.AlertChannel{
		ID:            c.ID,
		Name:          c.Name,
		Type:          gen.AlertChannelType(c.Type),
		Enabled:       c.Enabled,
		MinSeverity:   gen.AlertSeverity(c.MinSeverity),
		RuleID:        optional(c.RuleID),
		ClientGroupID: optional(c.ClientGroupID),
		CreatedAt:     c.CreatedAt,
		UpdatedAt:     c.UpdatedAt,
	}
	if e := c.Email; e != nil {
		out.Email = &gen.AlertChannelEmail{
			To: e.To, UsesPlatformRelay: c.UsesPlatformRelay, Host: e.Host, Port: e.Port, Encryption: e.Encryption,
			Username: e.Username, HasPassword: c.HasPassword, FromEmail: e.FromEmail, FromName: e.FromName,
		}
	}
	if w := c.Webhook; w != nil {
		out.Webhook = &gen.AlertChannelWebhook{
			Format: gen.WebhookFormat(w.Format), URL: c.WebhookURL, HasSigningSecret: c.HasSigningSecret,
		}
	}
	return out
}

func alertDelivery(d store.AlertDelivery) gen.AlertDelivery {
	out := gen.AlertDelivery{
		ID:          d.ID,
		AlertID:     d.AlertID,
		Event:       gen.AlertEventType(d.EventType),
		ChannelID:   d.ChannelID,
		ChannelName: d.ChannelName,
		State:       gen.AlertDeliveryState(d.State),
		Attempts:    d.Attempts,
		LastError:   optional(d.LastError),
		DeliveredAt: d.DeliveredAt,
		CreatedAt:   d.CreatedAt,
		UpdatedAt:   d.UpdatedAt,
	}
	if d.State == store.DeliveryPending {
		out.NextAttemptAt = &d.NextAttemptAt
	}
	return out
}
