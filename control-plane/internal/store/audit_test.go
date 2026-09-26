package store_test

import (
	"context"
	"testing"
	"time"

	"github.com/rchouinard/pressfleet/control-plane/internal/id"
	"github.com/rchouinard/pressfleet/control-plane/internal/store"
	"github.com/rchouinard/pressfleet/control-plane/internal/store/storetest"
)

func TestAuditEvents(t *testing.T) {
	ctx := context.Background()
	storetest.Run(t, func(t *testing.T, db *store.DB) {
		audit := store.NewAuditEvents(db)
		base := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
		alice, bob, token, site := id.New(), id.New(), id.New(), id.New()
		events := []store.AuditEvent{
			{OccurredAt: base, ActorUserID: alice, Action: "self.login", Result: store.AuditSuccess, SourceIP: "192.0.2.1"},
			{OccurredAt: base.Add(time.Hour), ActorUserID: alice, ActorTokenID: token, Action: "site.update",
				TargetType: "site", TargetID: site, Result: store.AuditFailure, SourceIP: "192.0.2.1"},
			{OccurredAt: base.Add(2 * time.Hour), ActorUserID: bob, Action: "site.delete",
				TargetType: "site", TargetID: site, Result: store.AuditDenied, SourceIP: "2001:db8::1"},
			{OccurredAt: base.Add(3 * time.Hour), Action: "self.login", Result: store.AuditFailure, SourceIP: "198.51.100.7"},
		}
		for i := range events {
			if err := audit.Insert(ctx, &events[i]); err != nil {
				t.Fatal(err)
			}
			if !id.Valid(events[i].ID) {
				t.Fatalf("Insert didn't set an ID: %+v", events[i])
			}
		}
		if err := audit.Insert(ctx, &store.AuditEvent{Action: "x", Result: "maybe"}); err == nil {
			t.Error("Insert with an invalid result succeeded")
		}
		if err := audit.Insert(ctx, &store.AuditEvent{Result: store.AuditSuccess}); err == nil {
			t.Error("Insert without an action succeeded")
		}

		// Round trip, newest first.
		page, err := audit.List(ctx, store.AuditFilter{})
		if err != nil {
			t.Fatal(err)
		}
		if len(page.Events) != 4 || page.NextCursor != "" {
			t.Fatalf("List = %+v", page)
		}
		for i, e := range page.Events {
			if want := events[len(events)-1-i]; e != want {
				t.Errorf("event %d = %+v, want %+v", i, e, want)
			}
		}

		ids := func(f store.AuditFilter) []string {
			t.Helper()
			var out []string
			for {
				p, err := audit.List(ctx, f)
				if err != nil {
					t.Fatal(err)
				}
				for _, e := range p.Events {
					out = append(out, e.ID)
				}
				if p.NextCursor == "" {
					return out
				}
				f.Before = p.NextCursor
			}
		}
		e0, e1, e2, e3 := events[0].ID, events[1].ID, events[2].ID, events[3].ID
		tests := []struct {
			name string
			f    store.AuditFilter
			want []string
		}{
			{"paged", store.AuditFilter{Limit: 1}, []string{e3, e2, e1, e0}},
			{"actor", store.AuditFilter{ActorUserID: alice}, []string{e1, e0}},
			{"action", store.AuditFilter{Action: "self.login"}, []string{e3, e0}},
			{"target", store.AuditFilter{TargetType: "site", TargetID: site}, []string{e2, e1}},
			{"since", store.AuditFilter{Since: base.Add(time.Hour)}, []string{e3, e2, e1}},
			{"until", store.AuditFilter{Until: base.Add(time.Hour)}, []string{e0}},
			{"range and actor", store.AuditFilter{Since: base.Add(30 * time.Minute), Until: base.Add(3 * time.Hour), ActorUserID: bob}, []string{e2}},
			{"no match", store.AuditFilter{ActorUserID: id.New()}, nil},
		}
		for _, tt := range tests {
			got := ids(tt.f)
			if len(got) != len(tt.want) {
				t.Errorf("%s: got %v, want %v", tt.name, got, tt.want)
				continue
			}
			for i := range got {
				if got[i] != tt.want[i] {
					t.Errorf("%s: got %v, want %v", tt.name, got, tt.want)
					break
				}
			}
		}

		n, err := audit.DeleteBefore(ctx, base.Add(90*time.Minute))
		if err != nil || n != 2 {
			t.Fatalf("DeleteBefore = %d, %v; want 2", n, err)
		}
		if got := ids(store.AuditFilter{}); len(got) != 2 || got[0] != e3 || got[1] != e2 {
			t.Errorf("after DeleteBefore: %v", got)
		}
	})
}
