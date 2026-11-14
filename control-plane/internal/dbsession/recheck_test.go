package dbsession_test

import (
	"context"
	"testing"
	"time"

	"github.com/rchouinard/pressfleet/control-plane/internal/auth"
	"github.com/rchouinard/pressfleet/control-plane/internal/dbsession"
	"github.com/rchouinard/pressfleet/control-plane/internal/id"
	"github.com/rchouinard/pressfleet/control-plane/internal/sftpgw"
	"github.com/rchouinard/pressfleet/control-plane/internal/sitedb"
	"github.com/rchouinard/pressfleet/control-plane/internal/store"
	"github.com/rchouinard/pressfleet/control-plane/internal/store/storetest"
)

// The gateway's recheck keeps open tunnels only while the user has a session
// their current role allows, and revokes the ones it doesn't.
func TestForwardsAllowed(t *testing.T) {
	ctx := context.Background()
	storetest.Run(t, func(t *testing.T, db *store.DB) {
		e := newEnv(t, db, gateway)
		site := e.site(t, store.SiteRunning, true)
		admin := id.New()
		k := keys{"admin-key": {UserID: admin, Role: store.RoleAdmin, Method: auth.MethodSSHKey, SSHKeyID: "admin-key"}}
		rc, ok := e.svc.Forwarding(k).(sftpgw.ForwardRechecker)
		if !ok {
			t.Fatal("the policy doesn't implement sftpgw.ForwardRechecker")
		}
		conn := sftpgw.Identity{SessionID: id.New(), UserID: admin, SSHKeyID: "admin-key", SiteID: site.ID}
		allowed := func(want bool) {
			t.Helper()
			got, err := rc.ForwardsAllowed(ctx, conn)
			if err != nil || got != want {
				t.Errorf("ForwardsAllowed = %v, %v, want %v", got, err, want)
			}
		}

		allowed(false) // no session
		rw, err := e.svc.Open(ctx, site.ID, admin, sitedb.AccessReadWrite, time.Hour)
		if err != nil {
			t.Fatal(err)
		}
		allowed(true)

		// An Admin demoted to Operator loses the read-write session.
		demoted := k["admin-key"]
		demoted.Role = store.RoleOperator
		k["admin-key"] = demoted
		allowed(false)
		if d := e.session(t, rw.Session.ID); d.Status != store.DatabaseSessionEnded || d.EndReason != dbsession.ReasonRevoked {
			t.Errorf("revoked session = %+v", d)
		}
		if _, ok := e.users.TemporaryUsers()[rw.Session.ID]; ok {
			t.Error("the revoked session's user remains")
		}

		// A removed key ends the tunnels too.
		delete(k, "admin-key")
		allowed(false)
	})
}
