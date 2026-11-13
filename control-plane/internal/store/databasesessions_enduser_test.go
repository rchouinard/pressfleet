package store_test

import (
	"context"
	"testing"
	"time"

	"github.com/rchouinard/pressfleet/control-plane/internal/id"
	"github.com/rchouinard/pressfleet/control-plane/internal/store"
	"github.com/rchouinard/pressfleet/control-plane/internal/store/storetest"
)

func TestDatabaseSessionsEndAllForUser(t *testing.T) {
	ctx := context.Background()
	storetest.Run(t, func(t *testing.T, db *store.DB) {
		sessions := store.NewDatabaseSessions(db)
		userID, otherUser := id.New(), id.New()
		now := time.Now().UTC().Truncate(time.Second)
		var mine []*store.DatabaseSession
		for _, uid := range []string{userID, userID, otherUser} {
			d := &store.DatabaseSession{SiteID: id.New(), UserID: uid, Access: "read_write", ExpiresAt: now.Add(time.Hour)}
			if err := sessions.Create(ctx, d); err != nil {
				t.Fatal(err)
			}
			if uid == userID {
				mine = append(mine, d)
			}
		}
		n, err := sessions.EndAllForUser(ctx, userID, "access_revoked", now)
		if err != nil || n != 2 {
			t.Fatalf("EndAllForUser = %d, %v, want 2", n, err)
		}
		for _, d := range mine {
			got, err := sessions.Get(ctx, d.ID)
			if err != nil || got.Status != store.DatabaseSessionEnded || got.EndReason != "access_revoked" || !got.EndedAt.Equal(now) {
				t.Errorf("session = %+v, %v", got, err)
			}
		}
		if undropped, err := sessions.ListUndropped(ctx, 0); err != nil || len(undropped) != 2 {
			t.Errorf("ListUndropped = %d, %v, want the 2 ended sessions", len(undropped), err)
		}
		if n, err := sessions.EndAllForUser(ctx, userID, "access_revoked", now); err != nil || n != 0 {
			t.Errorf("second EndAllForUser = %d, %v, want 0", n, err)
		}
		if active, err := sessions.ActiveIDs(ctx); err != nil || len(active) != 1 {
			t.Errorf("ActiveIDs = %v, %v, want the other user's session", active, err)
		}
	})
}
