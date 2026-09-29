package e2e_test

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-sql-driver/mysql"
	"golang.org/x/crypto/ssh"

	"github.com/rchouinard/pressfleet/control-plane/internal/api/client"
	"github.com/rchouinard/pressfleet/control-plane/internal/catalog"
	"github.com/rchouinard/pressfleet/control-plane/internal/e2e"
	"github.com/rchouinard/pressfleet/control-plane/internal/id"
	"github.com/rchouinard/pressfleet/control-plane/internal/provisioner/compose"
)

// TestDatabaseSessions checks temporary database sessions (#115, design
// doc §5.8) on a real platform with two sites:
//
//   - an Operator can't open a read-write session; an Admin can, writes
//     through the tunnel, and ending the session through the API ends its
//     open connection and refuses new tunnels;
//   - an Operator's read-only session reads the site's database through
//     an `ssh -L` style tunnel (direct-tcpip on the SFTP gateway), sees only
//     that database, and can't write; the gateway forwards to nothing else
//     and not for another site;
//   - at expiry the open connection stops and new tunnels are refused, and
//     the sweep drops the MariaDB user and records the expiry;
//   - a temporary user left behind on MariaDB is dropped by the sweep;
//   - the audit log has the sessions and never the passwords.
func TestDatabaseSessions(t *testing.T) {
	var projects []compose.Project
	// Registered before Start, so it runs after the platform teardown: see
	// TestSiteCreateDelete.
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		for _, project := range projects {
			if res, err := project.Resources(ctx); err == nil && !res.Empty() {
				t.Logf("removing leftover site project %s", project.Name)
				if err := project.Down(ctx, compose.DownOptions{Volumes: true}); err != nil {
					t.Errorf("removing leftover site project %s: %v", project.Name, err)
				}
			}
		}
	})
	p := e2e.Start(t)
	ctx := t.Context()

	cat, err := catalog.Load()
	if err != nil {
		t.Fatal(err)
	}
	php, ok := cat.DefaultPHPVersion(time.Now())
	if !ok {
		t.Fatal("no default PHP version")
	}
	images, err := cat.Images(php, cat.LatestWordPress().Version)
	if err != nil {
		t.Fatal(err)
	}
	e2e.EnsureImages(t, images.PHP, images.Nginx)

	groupID := createGroup(t, p, "E2E DB Session Agency")
	var sites []client.Site
	for _, name := range []string{"E2E DB One", "E2E DB Two"} {
		created := createSite(t, p, groupID, name, php)
		sites = append(sites, created.Site)
		projects = append(projects, compose.Project{Name: compose.SiteProjectName(created.Site.ID), Dir: t.TempDir()})
		waitJob(t, p, created.Job.ID)
	}
	one, two := sites[0], sites[1]
	oneProject := projects[0]

	operator := newUser(t, p, "db-operator@e2e.test", client.Operator)
	opSigner, _ := registerKey(t, operator)
	adminSigner, _ := registerKey(t, p.Client)
	var passwords []string

	open := func(c *client.ClientWithResponses, siteID string, body client.DatabaseSessionCreate) client.DatabaseSessionOpened {
		t.Helper()
		resp, err := c.CreateSiteDatabaseSessionWithResponse(ctx, siteID, body)
		if err != nil || resp.JSON201 == nil {
			t.Fatalf("opening a database session: %v, %d %s", err, resp.StatusCode(), resp.Body)
		}
		passwords = append(passwords, resp.JSON201.Credentials.Password)
		return *resp.JSON201
	}

	// Operators can't open read-write sessions.
	rw := client.DatabaseSessionAccessReadWrite
	if resp, err := operator.CreateSiteDatabaseSessionWithResponse(ctx, one.ID, client.DatabaseSessionCreate{Access: &rw}); err != nil ||
		resp.StatusCode() != http.StatusForbidden {
		t.Fatalf("operator opening a read-write session: %v, %d %s", err, resp.StatusCode(), resp.Body)
	}

	// An Admin's read-write session writes through the tunnel.
	adminSession := open(p.Client, one.ID, client.DatabaseSessionCreate{Access: &rw})
	dbAddr := tunnelTarget(t, adminSession.Tunnel.SSHCommand, one.Slug)
	adminSSH, err := dialSFTP(p, one.Slug, adminSigner)
	if err != nil {
		t.Fatalf("admin connecting to the gateway: %v", err)
	}
	defer func() { _ = adminSSH.Close() }()
	adminDB := tunnelDB(t, adminSSH, dbAddr, adminSession.Credentials)
	const marker = "pf-e2e-db-session-9c1e"
	if _, err := adminDB.ExecContext(ctx, "INSERT INTO wp_options (option_name, option_value, autoload) VALUES ('pf_e2e_marker', ?, 'no')", marker); err != nil {
		t.Fatalf("read-write session writing: %v", err)
	}
	if got := strings.TrimSpace(execOut(t, oneProject, "wp", "option", "get", "pf_e2e_marker")); got != marker {
		t.Errorf("the site reads %q, want the value written through the tunnel", got)
	}
	adminConn, err := adminDB.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := adminConn.PingContext(ctx); err != nil {
		t.Fatal(err)
	}

	// Ending it through the API ends the open connection and new tunnels.
	if resp, err := p.Client.EndSiteDatabaseSessionWithResponse(ctx, one.ID, adminSession.Session.ID); err != nil || resp.StatusCode() != http.StatusNoContent {
		t.Fatalf("ending the session: %v, %d %s", err, resp.StatusCode(), resp.Body)
	}
	if err := queryWithTimeout(ctx, adminConn, "SELECT 1"); err == nil {
		t.Error("an ended session's open connection still works")
	}
	_ = adminConn.Close()
	if _, err := adminSSH.Dial("tcp", dbAddr); err == nil {
		t.Error("the gateway forwarded for an ended session")
	}

	// An Operator's read-only session, which expires in a minute.
	ttl := 60
	roSession := open(operator, one.ID, client.DatabaseSessionCreate{TTLSeconds: &ttl})
	ro := roSession.Session
	if ro.Access != client.DatabaseSessionAccessReadOnly || ro.Database != "pf_"+one.ID {
		t.Errorf("read-only session = %+v", ro)
	}
	opSSH, err := dialSFTP(p, one.Slug, opSigner)
	if err != nil {
		t.Fatalf("operator connecting to the gateway: %v", err)
	}
	defer func() { _ = opSSH.Close() }()
	roDB := tunnelDB(t, opSSH, dbAddr, roSession.Credentials)
	roConn, err := roDB.Conn(ctx)
	if err != nil {
		t.Fatalf("connecting through the tunnel: %v", err)
	}
	defer func() { _ = roConn.Close() }()

	var siteURL string
	if err := roConn.QueryRowContext(ctx, "SELECT option_value FROM wp_options WHERE option_name = 'siteurl'").Scan(&siteURL); err != nil ||
		!strings.Contains(siteURL, one.PreviewDomain) {
		t.Errorf("siteurl through the tunnel = %q, %v", siteURL, err)
	}
	var dbs []string
	rows, err := roConn.QueryContext(ctx, "SHOW DATABASES")
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			t.Fatal(err)
		}
		dbs = append(dbs, s)
	}
	_ = rows.Close()
	if strings.Join(dbs, ",") != "information_schema,pf_"+one.ID {
		t.Errorf("the session sees databases %v, want only its site's", dbs)
	}
	var v string
	if err := roConn.QueryRowContext(ctx, "SELECT option_value FROM `pf_"+two.ID+"`.wp_options LIMIT 1").Scan(&v); !accessDenied(err) {
		t.Errorf("reading the other site's database: err = %v, want access denied", err)
	}
	for _, q := range []string{
		"INSERT INTO wp_options (option_name, option_value) VALUES ('pf_e2e_ro', 'x')",
		"UPDATE wp_options SET option_value = 'x' WHERE option_name = 'blogname'",
		"DELETE FROM wp_options WHERE option_name = 'pf_e2e_marker'",
		"CREATE TABLE pf_e2e (x INT)",
		"DROP TABLE wp_options",
	} {
		if _, err := roConn.ExecContext(ctx, q); !accessDenied(err) {
			t.Errorf("read-only %q: err = %v, want access denied", q, err)
		}
	}

	// The gateway forwards to the database only, and only for the site the
	// session is on.
	for _, addr := range []string{"redis:6379", "mariadb:22", "127.0.0.1:3306", "control-plane:8080"} {
		if c, err := opSSH.Dial("tcp", addr); err == nil {
			_ = c.Close()
			t.Errorf("the gateway forwarded to %s", addr)
		}
	}
	opTwo, err := dialSFTP(p, two.Slug, opSigner)
	if err != nil {
		t.Fatalf("operator connecting to site two: %v", err)
	}
	if c, err := opTwo.Dial("tcp", dbAddr); err == nil {
		_ = c.Close()
		t.Error("the gateway forwarded for a site without a session")
	}
	_ = opTwo.Close()

	// A temporary user left behind, as a crash would leave it.
	orphan := "pft_" + id.New()
	rootQuery(t, p, fmt.Sprintf("CREATE USER '%s'@'%%' IDENTIFIED BY 'orphan-pw'", orphan))

	// At expiry the open connection stops and new tunnels are refused.
	time.Sleep(time.Until(ro.ExpiresAt) + 2*time.Second)
	if err := queryWithTimeout(ctx, roConn, "SELECT 1"); err == nil {
		t.Error("an expired session's open connection still works")
	}
	if c, err := opSSH.Dial("tcp", dbAddr); err == nil {
		_ = c.Close()
		t.Error("the gateway forwarded for an expired session")
	}

	// The sweep, every minute, drops the expired user and the leftover one,
	// and records the expiry.
	var left string
	for deadline := time.Now().Add(3 * time.Minute); ; time.Sleep(5 * time.Second) {
		left = strings.TrimSpace(rootQuery(t, p, `SELECT GROUP_CONCAT(User) FROM mysql.user WHERE User LIKE 'pft\_%'`))
		if left == "NULL" && len(auditEvents(t, p, "site.database.session.expire")) > 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("temporary users after expiry: %s", left)
		}
	}
	expired := auditEvents(t, p, "site.database.session.expire")
	if len(expired) != 1 || deref(expired[0].TargetID) != one.ID || (*expired[0].Details)["session_id"] != ro.ID {
		t.Errorf("expiry events = %+v", expired)
	}
	resp, err := operator.ListSiteDatabaseSessionsWithResponse(ctx, one.ID)
	if err != nil || resp.JSON200 == nil || len(resp.JSON200.Items) != 0 {
		t.Errorf("active sessions after expiry: %v, %s", err, resp.Body)
	}

	// The audit log: opens and the end, with the session IDs; never a
	// password.
	opened := auditEvents(t, p, "site.database.session.create")
	openedRW := auditEvents(t, p, "site.database.session.create_read_write")
	ended := auditEvents(t, p, "site.database.session.end")
	if len(opened) != 1 || len(openedRW) != 2 || len(ended) != 1 {
		t.Errorf("audit events: %d read-only opens, %d read-write opens (one denied), %d ends", len(opened), len(openedRW), len(ended))
	}
	if len(opened) == 1 && (*opened[0].Details)["session_id"] != ro.ID {
		t.Errorf("open event = %+v", opened[0])
	}
	all, err := p.Client.ListAuditEventsWithResponse(ctx, &client.ListAuditEventsParams{})
	if err != nil || all.JSON200 == nil {
		t.Fatal(err)
	}
	for _, pw := range passwords {
		if strings.Contains(string(all.Body), pw) {
			t.Error("a session password is in the audit log")
		}
	}
	logs, err := p.Project.Logs(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, pw := range passwords {
		if strings.Contains(string(logs), pw) {
			t.Error("a session password is in the control plane's logs")
		}
	}
}

// tunnelTarget returns the database address of an ssh -L command, after
// checking the command's shape.
func tunnelTarget(t *testing.T, command, slug string) string {
	t.Helper()
	f := strings.Fields(command)
	if len(f) != 7 || f[0] != "ssh" || f[1] != "-N" || f[2] != "-L" || f[4] != "-p" || !strings.HasPrefix(f[6], slug+"@") {
		t.Fatalf("ssh command = %q", command)
	}
	_, target, ok := strings.Cut(f[3], ":")
	if !ok {
		t.Fatalf("ssh command = %q", command)
	}
	return target
}

var tunnelNets atomic.Int64

// tunnelDB opens a pool that reaches the database through the SSH
// connection's local forwarding, as `ssh -L` does.
func tunnelDB(t *testing.T, c *ssh.Client, addr string, creds struct {
	Database string `json:"database"`
	Password string `json:"password"`
	Username string `json:"username"`
}) *sql.DB {
	t.Helper()
	netName := fmt.Sprintf("pf-e2e-tunnel-%d", tunnelNets.Add(1))
	mysql.RegisterDialContext(netName, func(ctx context.Context, addr string) (net.Conn, error) {
		return c.DialContext(ctx, "tcp", addr)
	})
	mc := mysql.NewConfig()
	mc.Net, mc.Addr, mc.User, mc.Passwd, mc.DBName = netName, addr, creds.Username, creds.Password, creds.Database
	// No read or write timeouts: SSH channels don't support deadlines, and
	// the driver fails connections it can't set them on.
	mc.Timeout = 30 * time.Second
	mc.Logger = &mysql.NopLogger{}
	conn, err := mysql.NewConnector(mc)
	if err != nil {
		t.Fatal(err)
	}
	db := sql.OpenDB(conn)
	t.Cleanup(func() { _ = db.Close() })
	return db
}

// queryWithTimeout runs q on conn, giving up after a while: a connection
// that was ended may fail at once or only when the next packet is lost.
func queryWithTimeout(ctx context.Context, conn *sql.Conn, q string) error {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	var n int
	return conn.QueryRowContext(ctx, q).Scan(&n)
}

// accessDenied reports whether err is MariaDB refusing access to a
// database or table.
func accessDenied(err error) bool {
	var me *mysql.MySQLError
	return errors.As(err, &me) && (me.Number == 1044 || me.Number == 1142)
}

// rootQuery runs SQL on the platform MariaDB as root and returns the
// output. The password is read inside the container.
func rootQuery(t *testing.T, p *e2e.Platform, query string) string {
	t.Helper()
	out, err := p.Project.Exec(t.Context(), "mariadb", "sh", "-c",
		`exec mariadb --user=root --password="$(cat /run/secrets/mariadb_root_password)" --batch --skip-column-names -e "$1"`, "sh", query)
	if err != nil {
		t.Fatalf("querying MariaDB: %v: %s", err, out)
	}
	return string(out)
}

func auditEvents(t *testing.T, p *e2e.Platform, action string) []client.AuditEvent {
	t.Helper()
	resp, err := p.Client.ListAuditEventsWithResponse(t.Context(), &client.ListAuditEventsParams{Action: &action})
	if err != nil || resp.JSON200 == nil {
		t.Fatalf("audit events: %v", err)
	}
	return resp.JSON200.Items
}
