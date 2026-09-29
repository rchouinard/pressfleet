package store_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/rchouinard/pressfleet/control-plane/internal/id"
	"github.com/rchouinard/pressfleet/control-plane/internal/store"
	"github.com/rchouinard/pressfleet/control-plane/internal/store/storetest"
)

func TestDatabaseSessions(t *testing.T) {
	ctx := context.Background()
	storetest.Run(t, func(t *testing.T, db *store.DB) {
		sessions := store.NewDatabaseSessions(db)
		siteID, userID, otherUser := id.New(), id.New(), id.New()
		now := time.Now().UTC().Truncate(time.Second)

		// Create fills in the active state, keeping a given ID.
		s := &store.DatabaseSession{ID: id.New(), SiteID: siteID, UserID: userID, Access: "read_only",
			Status: store.DatabaseSessionEnded, CreatedAt: now, ExpiresAt: now.Add(time.Hour)}
		wantID := s.ID
		if err := sessions.Create(ctx, s); err != nil {
			t.Fatal(err)
		}
		if s.ID != wantID || s.Status != store.DatabaseSessionActive || !s.CreatedAt.Equal(now) || !s.EndedAt.IsZero() || !s.DroppedAt.IsZero() {
			t.Errorf("created = %+v", s)
		}
		got, err := sessions.Get(ctx, s.ID)
		if err != nil {
			t.Fatal(err)
		}
		if got != *s {
			t.Errorf("Get = %+v, want %+v", got, *s)
		}
		if !got.Active(now) || got.Active(now.Add(time.Hour)) {
			t.Error("Active doesn't follow the expiry")
		}
		if err := sessions.Create(ctx, &store.DatabaseSession{ID: s.ID, SiteID: siteID, UserID: userID, Access: "read_only", ExpiresAt: now}); !errors.Is(err, store.ErrConflict) {
			t.Errorf("Create with a taken ID: %v, want ErrConflict", err)
		}
		if err := sessions.Create(ctx, &store.DatabaseSession{SiteID: siteID}); err == nil {
			t.Error("Create accepted a session without a user, access, or expiry")
		}
		if _, err := sessions.Get(ctx, id.New()); !errors.Is(err, store.ErrNotFound) {
			t.Errorf("Get of a missing session: %v, want ErrNotFound", err)
		}

		// A second, shorter session for the same user, and one of another
		// user on another site.
		short := &store.DatabaseSession{SiteID: siteID, UserID: userID, Access: "read_write", ExpiresAt: now.Add(time.Minute)}
		other := &store.DatabaseSession{SiteID: id.New(), UserID: otherUser, Access: "read_only", ExpiresAt: now.Add(time.Hour)}
		for _, d := range []*store.DatabaseSession{short, other} {
			if err := sessions.Create(ctx, d); err != nil {
				t.Fatal(err)
			}
		}
		ids := func(list []store.DatabaseSession, err error) []string {
			t.Helper()
			if err != nil {
				t.Fatal(err)
			}
			var out []string
			for _, d := range list {
				out = append(out, d.ID)
			}
			return out
		}
		if got := ids(sessions.ListActive(ctx, siteID)); len(got) != 2 {
			t.Errorf("ListActive = %v", got)
		}
		if got := ids(sessions.ListActiveForUser(ctx, siteID, userID, now.Add(2*time.Minute))); len(got) != 1 || got[0] != s.ID {
			t.Errorf("ListActiveForUser after the short one expired = %v", got)
		}
		if got := ids(sessions.ListActiveForUser(ctx, siteID, otherUser, now)); len(got) != 0 {
			t.Errorf("ListActiveForUser of another user = %v", got)
		}
		if got := ids(sessions.ListDue(ctx, now.Add(2*time.Minute), 0)); len(got) != 1 || got[0] != short.ID {
			t.Errorf("ListDue = %v, want the short session", got)
		}
		active, err := sessions.ActiveIDs(ctx)
		if err != nil || len(active) != 3 || !active[short.ID] {
			t.Errorf("ActiveIDs = %v, %v", active, err)
		}

		// Nothing to drop while active.
		if got := ids(sessions.ListUndropped(ctx, 0)); len(got) != 0 {
			t.Errorf("ListUndropped with every session active = %v", got)
		}
		if changed, err := sessions.MarkDropped(ctx, short.ID, now); err != nil || changed {
			t.Errorf("MarkDropped of an active session = %v, %v", changed, err)
		}
		if _, err := sessions.End(ctx, short.ID, store.DatabaseSessionActive, "", "", now); err == nil {
			t.Error("End accepted the active status")
		}

		// End once; a second end changes nothing.
		endAt := now.Add(90 * time.Second)
		if changed, err := sessions.End(ctx, short.ID, store.DatabaseSessionExpired, "expired", "", endAt); err != nil || !changed {
			t.Fatalf("End = %v, %v", changed, err)
		}
		if changed, err := sessions.End(ctx, short.ID, store.DatabaseSessionEnded, "ended", userID, endAt); err != nil || changed {
			t.Errorf("second End = %v, %v", changed, err)
		}
		if changed, err := sessions.End(ctx, s.ID, store.DatabaseSessionEnded, "ended", otherUser, endAt); err != nil || !changed {
			t.Fatalf("End by a user = %v, %v", changed, err)
		}
		got, err = sessions.Get(ctx, s.ID)
		if err != nil {
			t.Fatal(err)
		}
		if got.Status != store.DatabaseSessionEnded || got.EndedByUserID != otherUser || got.EndReason != "ended" || !got.EndedAt.Equal(endAt) {
			t.Errorf("ended = %+v", got)
		}
		if got := ids(sessions.ListUndropped(ctx, 0)); len(got) != 2 {
			t.Errorf("ListUndropped = %v, want both ended sessions", got)
		}
		if changed, err := sessions.MarkDropped(ctx, short.ID, endAt); err != nil || !changed {
			t.Errorf("MarkDropped = %v, %v", changed, err)
		}
		if changed, err := sessions.MarkDropped(ctx, short.ID, endAt); err != nil || changed {
			t.Errorf("second MarkDropped = %v, %v", changed, err)
		}
		if got := ids(sessions.ListUndropped(ctx, 0)); len(got) != 1 || got[0] != s.ID {
			t.Errorf("ListUndropped after a drop = %v", got)
		}
		if got, err := sessions.Get(ctx, short.ID); err != nil || !got.DroppedAt.Equal(endAt) || got.EndedByUserID != "" {
			t.Errorf("dropped = %+v, %v", got, err)
		}
		if got := ids(sessions.ListActive(ctx, siteID)); len(got) != 0 {
			t.Errorf("ListActive after ending both = %v", got)
		}
	})
}
