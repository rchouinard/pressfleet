package store_test

import (
	"context"
	"errors"
	"reflect"
	"slices"
	"testing"
	"time"

	"github.com/rchouinard/pressfleet/control-plane/internal/id"
	"github.com/rchouinard/pressfleet/control-plane/internal/store"
	"github.com/rchouinard/pressfleet/control-plane/internal/store/storetest"
)

func TestAlertChannels(t *testing.T) {
	ctx := context.Background()
	storetest.Run(t, func(t *testing.T, db *store.DB) {
		channels := store.NewAlertChannels(db)

		hook := store.AlertChannel{
			Name: "Hook", Type: store.ChannelWebhook, Enabled: true, MinSeverity: store.SeverityWarning,
			RuleID: id.New(), ClientGroupID: id.New(), Config: `{"format":"slack"}`,
		}
		if err := channels.Create(ctx, &hook); err != nil {
			t.Fatal(err)
		}
		if !id.Valid(hook.ID) || hook.CreatedAt.IsZero() || hook.UpdatedAt != hook.CreatedAt {
			t.Errorf("Create did not fill in the channel: %+v", hook)
		}
		got, err := channels.Get(ctx, hook.ID)
		if err != nil || !reflect.DeepEqual(got, hook) {
			t.Errorf("Get = %+v, %v; want %+v", got, err, hook)
		}

		// No filters and no secret round trip as empty.
		mail := store.AlertChannel{Name: "Mail", Type: store.ChannelEmail, MinSeverity: store.SeverityInfo, Config: `{"to":["a@example.com"]}`}
		if err := channels.Create(ctx, &mail); err != nil {
			t.Fatal(err)
		}
		if got, _ := channels.Get(ctx, mail.ID); got.Enabled || got.RuleID != "" || got.ClientGroupID != "" || got.SecretRef != "" {
			t.Errorf("Get = %+v", got)
		}

		if err := channels.Create(ctx, &store.AlertChannel{Name: "Hook", Type: store.ChannelEmail, MinSeverity: store.SeverityInfo, Config: "{}"}); !errors.Is(err, store.ErrConflict) {
			t.Errorf("duplicate name = %v, want ErrConflict", err)
		}
		if err := channels.Create(ctx, &store.AlertChannel{Name: "Dangling", Type: store.ChannelEmail, MinSeverity: store.SeverityInfo, Config: "{}", SecretRef: id.New()}); !errors.Is(err, store.ErrReference) {
			t.Errorf("secret that doesn't exist = %v, want ErrReference", err)
		}
		if _, err := channels.Get(ctx, id.New()); !errors.Is(err, store.ErrNotFound) {
			t.Errorf("Get missing = %v", err)
		}

		// List: ordered by name, filtered, paged.
		list, err := channels.List(ctx, store.AlertChannelFilter{})
		if err != nil || len(list) != 2 || list[0].Name != "Hook" || list[1].Name != "Mail" {
			t.Errorf("List = %+v, %v", list, err)
		}
		on := true
		if list, _ = channels.List(ctx, store.AlertChannelFilter{Enabled: &on}); len(list) != 1 || list[0].ID != hook.ID {
			t.Errorf("List enabled = %+v", list)
		}
		if list, _ = channels.List(ctx, store.AlertChannelFilter{Type: store.ChannelEmail}); len(list) != 1 || list[0].ID != mail.ID {
			t.Errorf("List email = %+v", list)
		}
		if list, _ = channels.List(ctx, store.AlertChannelFilter{AfterName: "Hook", AfterID: hook.ID, Limit: 5}); len(list) != 1 || list[0].ID != mail.ID {
			t.Errorf("List after Hook = %+v", list)
		}

		// Update.
		mail.Name, mail.Enabled, mail.RuleID = "Mail 2", true, id.New()
		if err := channels.Update(ctx, &mail); err != nil {
			t.Fatal(err)
		}
		if got, _ := channels.Get(ctx, mail.ID); got.Name != "Mail 2" || !got.Enabled || got.RuleID != mail.RuleID || !got.UpdatedAt.After(got.CreatedAt) {
			t.Errorf("after Update = %+v", got)
		}
		mail.Name = "Hook"
		if err := channels.Update(ctx, &mail); !errors.Is(err, store.ErrConflict) {
			t.Errorf("rename to a taken name = %v", err)
		}
		missing := store.AlertChannel{ID: id.New(), Name: "x", Type: store.ChannelEmail, MinSeverity: store.SeverityInfo, Config: "{}"}
		if err := channels.Update(ctx, &missing); !errors.Is(err, store.ErrNotFound) {
			t.Errorf("Update missing = %v", err)
		}

		// Delete.
		if err := channels.Delete(ctx, hook.ID); err != nil {
			t.Fatal(err)
		}
		if err := channels.Delete(ctx, hook.ID); !errors.Is(err, store.ErrNotFound) {
			t.Errorf("Delete twice = %v", err)
		}
	})
}

func TestSeverityRank(t *testing.T) {
	ranks := []int{
		store.SeverityRank("bogus"), store.SeverityRank(store.SeverityInfo),
		store.SeverityRank(store.SeverityWarning), store.SeverityRank(store.SeverityCritical),
	}
	if !slices.IsSorted(ranks) || ranks[0] == ranks[1] || ranks[1] == ranks[2] || ranks[2] == ranks[3] {
		t.Errorf("ranks of unknown, info, warning, critical = %v, want strictly increasing", ranks)
	}
}

func TestAlertDeliveries(t *testing.T) {
	ctx := context.Background()
	storetest.Run(t, func(t *testing.T, db *store.DB) {
		rule := sampleRule("Down")
		if err := store.NewAlertRules(db).Create(ctx, &rule); err != nil {
			t.Fatal(err)
		}
		now := time.Now().UTC()
		alert := store.Alert{RuleID: rule.ID, Fingerprint: "f", State: store.AlertFiring, Severity: store.SeverityCritical, StartedAt: now, FiredAt: &now}
		if err := store.NewAlerts(db).Insert(ctx, &alert); err != nil {
			t.Fatal(err)
		}
		channels := store.NewAlertChannels(db)
		a, b := store.AlertChannel{Name: "a", Type: store.ChannelWebhook, MinSeverity: store.SeverityInfo, Config: "{}"}, store.AlertChannel{Name: "b", Type: store.ChannelWebhook, MinSeverity: store.SeverityInfo, Config: "{}"}
		for _, c := range []*store.AlertChannel{&a, &b} {
			if err := channels.Create(ctx, c); err != nil {
				t.Fatal(err)
			}
		}
		deliveries := store.NewAlertDeliveries(db)

		// Enqueue is idempotent per alert, event, and channel.
		for i, want := range []bool{true, false} {
			if got, err := deliveries.Enqueue(ctx, alert.ID, store.AlertEventFiring, a.ID, `{"n":1}`); err != nil || got != want {
				t.Errorf("Enqueue #%d = %v, %v; want %v", i+1, got, err, want)
			}
		}
		if got, err := deliveries.Enqueue(ctx, alert.ID, store.AlertEventFiring, b.ID, `{"n":2}`); err != nil || !got {
			t.Errorf("Enqueue for another channel = %v, %v", got, err)
		}
		if got, err := deliveries.Enqueue(ctx, alert.ID, store.AlertEventResolved, a.ID, `{"n":3}`); err != nil || !got {
			t.Errorf("Enqueue of another event = %v, %v", got, err)
		}
		if _, err := deliveries.Enqueue(ctx, id.New(), store.AlertEventFiring, a.ID, "{}"); !errors.Is(err, store.ErrReference) {
			t.Errorf("Enqueue for a missing alert = %v", err)
		}
		if _, err := deliveries.Enqueue(ctx, alert.ID, store.AlertEventFiring, id.New(), "{}"); !errors.Is(err, store.ErrReference) {
			t.Errorf("Enqueue for a missing channel = %v", err)
		}

		// Everything is due, oldest first.
		due, err := deliveries.Due(ctx, time.Now().Add(time.Second), 10)
		if err != nil || len(due) != 3 {
			t.Fatalf("Due = %+v, %v", due, err)
		}
		first := due[0]
		if first.AlertID != alert.ID || first.EventType != store.AlertEventFiring || first.ChannelName != "a" || first.State != store.DeliveryPending ||
			first.Attempts != 0 || first.Event != `{"n":1}` || first.DeliveredAt != nil {
			t.Errorf("first due = %+v", first)
		}
		if limited, _ := deliveries.Due(ctx, time.Now().Add(time.Second), 2); len(limited) != 2 {
			t.Errorf("Due with a limit = %d", len(limited))
		}
		if early, _ := deliveries.Due(ctx, now.Add(-time.Hour), 10); len(early) != 0 {
			t.Errorf("Due before anything is due = %d", len(early))
		}

		// A failed attempt with a retry time stays pending but isn't due
		// until then; without one it fails for good.
		retryAt := time.Now().Add(time.Hour)
		if err := deliveries.MarkAttemptFailed(ctx, first.ID, 1, "HTTP 500", retryAt, time.Now()); err != nil {
			t.Fatal(err)
		}
		got, err := deliveries.Get(ctx, first.ID)
		if err != nil || got.State != store.DeliveryPending || got.Attempts != 1 || got.LastError != "HTTP 500" || got.NextAttemptAt.Before(time.Now().Add(30*time.Minute)) {
			t.Errorf("after a failed attempt = %+v, %v", got, err)
		}
		if due, _ = deliveries.Due(ctx, time.Now().Add(time.Second), 10); len(due) != 2 {
			t.Errorf("Due after backing off = %d, want 2", len(due))
		}
		if err := deliveries.MarkAttemptFailed(ctx, first.ID, 2, "HTTP 500", time.Time{}, time.Now()); err != nil {
			t.Fatal(err)
		}
		if got, _ = deliveries.Get(ctx, first.ID); got.State != store.DeliveryFailed || got.Attempts != 2 {
			t.Errorf("after the last attempt = %+v", got)
		}
		if due, _ = deliveries.Due(ctx, time.Now().Add(2*time.Hour), 10); len(due) != 2 {
			t.Errorf("a failed delivery is still due: %d", len(due))
		}

		// Delivered.
		if err := deliveries.MarkDelivered(ctx, due[0].ID, 1, time.Now()); err != nil {
			t.Fatal(err)
		}
		if got, _ = deliveries.Get(ctx, due[0].ID); got.State != store.DeliveryDelivered || got.DeliveredAt == nil || got.LastError != "" || got.Attempts != 1 {
			t.Errorf("after delivery = %+v", got)
		}
		if err := deliveries.MarkDelivered(ctx, id.New(), 1, time.Now()); !errors.Is(err, store.ErrNotFound) {
			t.Errorf("MarkDelivered missing = %v", err)
		}

		// List: newest first, filtered, paged.
		list, err := deliveries.List(ctx, store.AlertDeliveryFilter{AlertID: alert.ID})
		if err != nil || len(list) != 3 || list[0].ID < list[1].ID {
			t.Fatalf("List = %+v, %v", list, err)
		}
		if l, _ := deliveries.List(ctx, store.AlertDeliveryFilter{ChannelID: b.ID}); len(l) != 1 {
			t.Errorf("List by channel = %d", len(l))
		}
		if l, _ := deliveries.List(ctx, store.AlertDeliveryFilter{State: store.DeliveryFailed}); len(l) != 1 || l[0].ID != first.ID {
			t.Errorf("List failed = %+v", l)
		}
		if l, _ := deliveries.List(ctx, store.AlertDeliveryFilter{Before: list[0].ID, Limit: 1}); len(l) != 1 || l[0].ID != list[1].ID {
			t.Errorf("List page 2 = %+v", l)
		}

		// Deleting a channel deletes its deliveries; so does deleting the alert.
		if err := channels.Delete(ctx, b.ID); err != nil {
			t.Fatal(err)
		}
		if l, _ := deliveries.List(ctx, store.AlertDeliveryFilter{}); len(l) != 2 {
			t.Errorf("deliveries after deleting a channel = %d, want 2", len(l))
		}
		if _, err := db.ExecContext(ctx, "DELETE FROM alerts WHERE id = ?", alert.ID); err != nil {
			t.Fatal(err)
		}
		if l, _ := deliveries.List(ctx, store.AlertDeliveryFilter{}); len(l) != 0 {
			t.Errorf("deliveries after deleting the alert = %d, want 0", len(l))
		}
	})
}
