package mariadb

import (
	"context"
	"errors"
	"slices"
	"testing"

	"github.com/rchouinard/pressfleet/control-plane/internal/dbsession"
	"github.com/rchouinard/pressfleet/control-plane/internal/sitedb"
	"github.com/rchouinard/pressfleet/control-plane/internal/store"
	"github.com/rchouinard/pressfleet/control-plane/internal/store/storetest"
)

const testSessionID = "01j9z8y7x6w5v4t3s2r1q0p9s5"

func TestTemporaryUsers(t *testing.T) {
	storetest.Run(t, func(t *testing.T, db *store.DB) {
		ctx := context.Background()
		f := newFixture(t, db)

		// No database, no temporary user.
		if _, err := f.m.CreateTemporaryUser(ctx, testSiteID, testSessionID, sitedb.AccessReadOnly); !errors.Is(err, sitedb.ErrDatabaseNotFound) {
			t.Fatalf("CreateTemporaryUser without a database: err = %v, want ErrDatabaseNotFound", err)
		}
		if _, err := f.m.CreateDatabase(ctx, testSiteID); err != nil {
			t.Fatal(err)
		}

		for _, access := range []sitedb.Access{sitedb.AccessReadOnly, sitedb.AccessReadWrite} {
			c, err := f.m.CreateTemporaryUser(ctx, testSiteID, testSessionID, access)
			if err != nil {
				t.Fatal(err)
			}
			want := "pft_" + testSessionID
			if c.Username != want || c.Database != "pf_"+testSiteID || len(c.Password.Reveal()) < 20 {
				t.Errorf("credentials = %s on %s (password %d characters)", c.Username, c.Database, len(c.Password.Reveal()))
			}
			_, u, ok := f.srv.state(want)
			if !ok || u.hash != nativePasswordHash(c.Password) || u.grant != "pf_"+testSiteID ||
				u.access != access || u.maxConns != TemporaryUserConnections {
				t.Errorf("server user = %+v, %v", u, ok)
			}
		}
		// The site's own user is untouched.
		if _, u, ok := f.srv.state("pf_" + testSiteID); !ok || u.access != "" {
			t.Errorf("site user = %+v, %v", u, ok)
		}

		ids, err := f.m.ListTemporaryUsers(ctx)
		if err != nil || !slices.Equal(ids, []string{testSessionID}) {
			t.Errorf("ListTemporaryUsers = %v, %v", ids, err)
		}
		for range 2 { // dropping is idempotent
			if err := f.m.DropTemporaryUser(ctx, testSessionID); err != nil {
				t.Fatal(err)
			}
		}
		if ids, err := f.m.ListTemporaryUsers(ctx); err != nil || len(ids) != 0 {
			t.Errorf("ListTemporaryUsers after drop = %v, %v", ids, err)
		}
	})
}

func TestTemporaryUsersRejectBadInput(t *testing.T) {
	storetest.Run(t, func(t *testing.T, db *store.DB) {
		ctx := context.Background()
		f := newFixture(t, db)
		bad := "x'@'%'; DROP USER root; --"
		if _, err := f.m.CreateTemporaryUser(ctx, testSiteID, bad, sitedb.AccessReadOnly); !errors.Is(err, ErrInvalidSessionID) {
			t.Errorf("CreateTemporaryUser err = %v, want ErrInvalidSessionID", err)
		}
		if _, err := f.m.CreateTemporaryUser(ctx, bad, testSessionID, sitedb.AccessReadOnly); !errors.Is(err, ErrInvalidSiteID) {
			t.Errorf("CreateTemporaryUser err = %v, want ErrInvalidSiteID", err)
		}
		if _, err := f.m.CreateTemporaryUser(ctx, testSiteID, testSessionID, "everything"); err == nil {
			t.Error("CreateTemporaryUser accepted an unknown access")
		}
		if err := f.m.DropTemporaryUser(ctx, bad); !errors.Is(err, ErrInvalidSessionID) {
			t.Errorf("DropTemporaryUser err = %v, want ErrInvalidSessionID", err)
		}
		if f.srv.calls != 0 {
			t.Error("bad input reached the server")
		}
	})
}

// Users that merely share the prefix aren't PressFleet's.
func TestListTemporaryUsersIgnoresOthers(t *testing.T) {
	f := newFixture(t, storetest.Open(t, store.SQLite))
	f.srv.users["pft_not-an-id"] = fakeUser{}
	f.srv.users["pft_"+testSessionID] = fakeUser{}
	ids, err := f.m.ListTemporaryUsers(context.Background())
	if err != nil || !slices.Equal(ids, []string{testSessionID}) {
		t.Errorf("ListTemporaryUsers = %v, %v", ids, err)
	}
}

func TestTempNames(t *testing.T) {
	n, err := newTempName(testSessionID)
	if err != nil {
		t.Fatal(err)
	}
	if n.account() != "'pft_"+testSessionID+"'@'%'" || n.quoted() != "'pft_"+testSessionID+"'" {
		t.Errorf("account = %s, quoted = %s", n.account(), n.quoted())
	}
	// A site user's name is never a temporary one.
	site, _ := newSiteName(testSiteID)
	if _, ok := tempSessionID(site.String()); ok {
		t.Errorf("%s parsed as a temporary user", site)
	}
	if got, ok := tempSessionID(n.String()); !ok || got != testSessionID {
		t.Errorf("tempSessionID(%s) = %q, %v", n, got, ok)
	}
	// Package dbsession shows the names without asking the server.
	if dbsession.Username(testSessionID) != n.String() || dbsession.Database(testSiteID) != site.String() {
		t.Errorf("dbsession names %s and %s, want %s and %s",
			dbsession.Username(testSessionID), dbsession.Database(testSiteID), n, site)
	}
}
