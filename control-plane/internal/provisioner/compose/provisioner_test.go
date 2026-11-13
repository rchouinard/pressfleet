package compose_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/rchouinard/pressfleet/control-plane/internal/catalog"
	"github.com/rchouinard/pressfleet/control-plane/internal/config"
	"github.com/rchouinard/pressfleet/control-plane/internal/objectcache"
	"github.com/rchouinard/pressfleet/control-plane/internal/provisioner"
	"github.com/rchouinard/pressfleet/control-plane/internal/provisioner/compose"
	"github.com/rchouinard/pressfleet/control-plane/internal/redact"
	"github.com/rchouinard/pressfleet/control-plane/internal/secrets"
	"github.com/rchouinard/pressfleet/control-plane/internal/sitedb"
	"github.com/rchouinard/pressfleet/control-plane/internal/store"
	"github.com/rchouinard/pressfleet/control-plane/internal/wpconfig"
)

// memSecrets is an in-memory compose.SecretReader.
type memSecrets map[secrets.Ref]redact.Secret

func (m memSecrets) Get(_ context.Context, ref secrets.Ref) (redact.Secret, error) {
	s, ok := m[ref]
	if !ok {
		return "", fmt.Errorf("get secret %s: %w", ref, store.ErrNotFound)
	}
	return s, nil
}

// imageMap is a compose.ImageResolver keyed by PHP version.
type imageMap map[string]catalog.Images

func (m imageMap) Images(php, _ string) (catalog.Images, error) {
	img, ok := m[php]
	if !ok {
		return catalog.Images{}, fmt.Errorf("%w: PHP version %q", catalog.ErrInvalidVersion, php)
	}
	return img, nil
}

// siteSecrets stores a site's database credentials and salts in m and
// returns their references.
func siteSecrets(t *testing.T, m memSecrets, siteID string, creds sitedb.Credentials) (dbRef, saltsRef string) {
	t.Helper()
	db, err := sitedb.MarshalCredentials(creds)
	if err != nil {
		t.Fatal(err)
	}
	salts, err := wpconfig.MarshalSalts(wpconfig.GenerateSalts())
	if err != nil {
		t.Fatal(err)
	}
	dbRef, saltsRef = siteID+"-db", siteID+"-salts"
	m[secrets.Ref(dbRef)], m[secrets.Ref(saltsRef)] = db, salts
	return dbRef, saltsRef
}

const testMariaDB = "pressfleet-mariadb-1"

// fakeDocker simulates the docker CLI calls the provisioner makes, for one
// site, and records them.
type fakeDocker struct {
	t            *testing.T
	calls        []string // "compose <subcommand> ..." without the project flags, or the plain docker args
	containers   map[string]compose.Container
	phpStartedAt time.Time
	// mariaDBNetworks are the networks the MariaDB container is on; nil
	// means the container doesn't exist.
	mariaDBNetworks map[string]bool
	// fail makes calls whose recorded form starts with the key fail.
	fail map[string]error
	// phpUser is the php container's configured user.
	phpUser string
	// exec is what `compose exec` returns; execArgs records its arguments.
	execOut  []byte
	execErr  error
	execArgs []string
	// execCalls records the arguments of every exec; blockWPCLI makes a
	// WP-CLI run wait for its context to end.
	execCalls  [][]string
	blockWPCLI bool
	// execStdin is the standard input of the last docker call, nil if none.
	execStdin *string
}

func newFakeDocker(t *testing.T) *fakeDocker {
	return &fakeDocker{
		t:               t,
		containers:      map[string]compose.Container{},
		mariaDBNetworks: map[string]bool{"pressfleet_platform": true},
		phpUser:         "20042:20042",
	}
}

var errNoSuchContainer = &compose.CommandError{Stderr: "Error response from daemon: No such container: x", Err: errors.New("exit status 1")}

func (f *fakeDocker) exec(ctx context.Context, stdin io.Reader, args ...string) ([]byte, error) {
	f.execStdin = nil
	if stdin != nil {
		b, err := io.ReadAll(stdin)
		if err != nil {
			return nil, err
		}
		in := string(b)
		f.execStdin = &in
	}
	call := args
	if args[0] == "compose" {
		call = append([]string{"compose"}, args[5:]...)
	}
	rec := strings.Join(call, " ")
	f.calls = append(f.calls, rec)
	if f.blockWPCLI && call[0] == "compose" && slices.Contains(call, "pressfleet-wpcli") {
		// A WP-CLI run that lasts until its context ends, as the killed
		// docker CLI's would.
		f.execCalls = append(f.execCalls, call[1:])
		<-ctx.Done()
		return nil, &compose.CommandError{Err: errors.New("signal: killed")}
	}
	for prefix, err := range f.fail {
		if strings.HasPrefix(rec, prefix) {
			return nil, err
		}
	}
	switch {
	case args[0] == "compose":
		return f.compose(call[1:])
	case args[0] == "container" && args[1] == "inspect":
		return f.inspect(args[len(args)-1])
	case args[0] == "network" && args[1] == "connect":
		f.mariaDBNetworks[args[len(args)-2]] = true
		return nil, nil
	case args[0] == "network" && args[1] == "disconnect":
		delete(f.mariaDBNetworks, args[2])
		return nil, nil
	}
	f.t.Fatalf("unexpected docker call: %q", args)
	return nil, nil
}

func (f *fakeDocker) compose(args []string) ([]byte, error) {
	set := func(state, health string) {
		for _, svc := range []string{"php", "nginx"} {
			f.containers[svc] = compose.Container{Name: "pf-site-" + svc + "-1", Service: svc, State: state, Health: health}
		}
	}
	switch args[0] {
	case "ps":
		var out []byte
		for _, svc := range []string{"php", "nginx"} {
			if c, ok := f.containers[svc]; ok {
				b, _ := json.Marshal(c)
				out = append(append(out, b...), '\n')
			}
		}
		return out, nil
	case "up":
		switch {
		case slices.Contains(args, "--no-start"):
			set("created", "")
		case slices.Contains(args, "php") && slices.Contains(args, "--force-recreate"):
			f.containers["php"] = compose.Container{Name: "pf-site-php-1", Service: "php", State: "running", Health: "healthy"}
			f.phpStartedAt = time.Now()
		default:
			set("running", "healthy")
			if f.phpStartedAt.IsZero() {
				f.phpStartedAt = time.Now()
			}
		}
	case "start":
		set("running", "healthy")
		f.phpStartedAt = time.Now()
	case "stop":
		set("exited", "")
	case "exec":
		f.execArgs = args
		f.execCalls = append(f.execCalls, args)
		return f.execOut, f.execErr
	case "run", "kill":
		return nil, nil
	case "down":
		clear(f.containers)
	default:
		f.t.Fatalf("unexpected compose call: %q", args)
	}
	return nil, nil
}

func (f *fakeDocker) inspect(name string) ([]byte, error) {
	var info map[string]any
	switch {
	case name == testMariaDB:
		if f.mariaDBNetworks == nil {
			return nil, errNoSuchContainer
		}
		nets := map[string]any{}
		for n := range f.mariaDBNetworks {
			nets[n] = map[string]any{}
		}
		info = map[string]any{"State": map[string]any{"Running": true}, "NetworkSettings": map[string]any{"Networks": nets}}
	case strings.HasSuffix(name, "-php-1"):
		c, ok := f.containers["php"]
		if !ok {
			return nil, errNoSuchContainer
		}
		info = map[string]any{
			"Config": map[string]any{"User": f.phpUser},
			"State":  map[string]any{"Running": c.State == "running", "StartedAt": f.phpStartedAt.Format(time.RFC3339Nano)},
		}
	default:
		return nil, errNoSuchContainer
	}
	return json.Marshal([]any{info})
}

// commands returns the recorded calls that change state, dropping reads.
func (f *fakeDocker) commands() []string {
	var out []string
	for _, c := range f.calls {
		if strings.HasPrefix(c, "compose ps") || strings.HasPrefix(c, "container inspect") {
			continue
		}
		out = append(out, c)
	}
	return out
}

type fixture struct {
	docker *fakeDocker
	prov   *compose.Provisioner
	layout config.Layout
	cfg    *provisioner.SiteConfig
	sec    memSecrets
	// routesDir stands in for Traefik's file provider directory.
	routesDir string
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	layout := config.Layout{Root: t.TempDir()}
	if err := layout.Ensure(); err != nil {
		t.Fatal(err)
	}
	sec := memSecrets{}
	dbRef, saltsRef := siteSecrets(t, sec, testSiteID, sitedb.Credentials{Database: "site_db", Username: "site_user", Password: testDBSecret})
	docker := newFakeDocker(t)
	routesDir := filepath.Join(layout.PlatformDir(), "traefik", "dynamic")
	if err := os.MkdirAll(routesDir, 0o755); err != nil {
		t.Fatal(err)
	}
	prov, err := compose.NewProvisioner(compose.ProvisionerConfig{
		Layout:            layout,
		TraefikDynamicDir: routesDir,
		Secrets:           sec,
		Images: imageMap{
			"8.4": {PHP: "site-php:php8.4-wp7.1.2", Nginx: "site-nginx:wp7.1.2"},
			"8.3": {PHP: "site-php:php8.3-wp7.1.2", Nginx: "site-nginx:wp7.1.2"},
		},
		MariaDBContainer: testMariaDB,
		Docker:           docker.exec,
	})
	if err != nil {
		t.Fatal(err)
	}
	cfg := testSiteRender().Site
	cfg.DBSecretRef, cfg.WPSaltsSecretRef = dbRef, saltsRef
	return &fixture{docker: docker, prov: prov, layout: layout, cfg: cfg, sec: sec, routesDir: routesDir}
}

func (f *fixture) siteDir(t *testing.T) string {
	t.Helper()
	dir, err := f.layout.SiteDir(testSiteID)
	if err != nil {
		t.Fatal(err)
	}
	return dir
}

var (
	network  = compose.SiteProjectName(testSiteID)
	upWait   = "compose up --detach --remove-orphans --wait --wait-timeout 180"
	connect  = "network connect --gw-priority -1 --alias pf-mariadb " + network + " " + testMariaDB
	recreate = "compose up --detach --remove-orphans --force-recreate -- php"
)

func wantCommands(t *testing.T, f *fakeDocker, want ...string) {
	t.Helper()
	if got := f.commands(); !slices.Equal(got, want) {
		t.Errorf("docker commands:\n got %q\nwant %q", got, want)
	}
	f.calls = nil
}

func TestNewProvisionerValidates(t *testing.T) {
	_, err := compose.NewProvisioner(compose.ProvisionerConfig{Layout: config.Layout{Root: "relative"}, WaitTimeout: -1, WPCLIMaxOutput: -1})
	if err == nil {
		t.Fatal("NewProvisioner accepted an empty config")
	}
	for _, want := range []string{"absolute", "secret reader", "image resolver", "MariaDB container", "timeouts", "output limit"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not mention %q", err, want)
		}
	}
}

func TestCreateSite(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	st, err := f.prov.CreateSite(ctx, f.cfg)
	if err != nil {
		t.Fatal(err)
	}
	if st.ID != testSiteID || st.Phase != provisioner.SiteRunning {
		t.Errorf("status = %+v", st)
	}
	wantCommands(t, f.docker, upWait, connect)

	// The resolved secrets and images were rendered.
	dir := f.siteDir(t)
	wpConfig, err := os.ReadFile(filepath.Join(dir, compose.SiteWPConfigFile))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(wpConfig), "'site_user'") || !strings.Contains(string(wpConfig), `db-$ecret\'pw`) {
		t.Errorf("wp-config.php lacks the database credentials:\n%s", wpConfig)
	}
	if !strings.Contains(string(wpConfig), "$table_prefix = 'wp_';") {
		t.Errorf("wp-config.php lacks the default table prefix:\n%s", wpConfig)
	}
	composeFile, err := os.ReadFile(filepath.Join(dir, compose.SiteComposeFile))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(composeFile), `"site-php:php8.4-wp7.1.2"`) {
		t.Errorf("compose.yaml lacks the php image:\n%s", composeFile)
	}

	// Creating again converges without recreating anything or reconnecting.
	if _, err := f.prov.CreateSite(ctx, f.cfg); err != nil {
		t.Fatal(err)
	}
	wantCommands(t, f.docker, upWait)
}

// TestImportedTablePrefix checks that an imported site's table prefix
// (§5.7) reaches wp-config.php.
func TestImportedTablePrefix(t *testing.T) {
	f := newFixture(t)
	f.cfg.TablePrefix = "wpx7_"
	if _, err := f.prov.CreateSite(context.Background(), f.cfg); err != nil {
		t.Fatal(err)
	}
	wpConfig, err := os.ReadFile(filepath.Join(f.siteDir(t), compose.SiteWPConfigFile))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(wpConfig), "$table_prefix = 'wpx7_';") {
		t.Errorf("wp-config.php lacks the imported table prefix:\n%s", wpConfig)
	}
}

// TestObjectCacheSettings checks that a site's object cache credentials
// reach wp-config.php as the Redis Object Cache plugin's settings (#39), and
// that a reference the provisioner can't resolve fails the render.
func TestObjectCacheSettings(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	creds, err := objectcache.MarshalCredentials(objectcache.Credentials{
		Username: "pf_" + testSiteID, Password: `cache'pw\`, Database: 42, KeyPrefix: objectcache.KeyPrefix(testSiteID),
	})
	if err != nil {
		t.Fatal(err)
	}
	f.sec["cache-ref"] = creds
	f.cfg.RedisSecretRef = "cache-ref"
	if _, err := f.prov.CreateSite(ctx, f.cfg); err != nil {
		t.Fatal(err)
	}
	wpConfig, err := os.ReadFile(filepath.Join(f.siteDir(t), compose.SiteWPConfigFile))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"define( 'WP_REDIS_HOST', 'pf-redis' );",
		"define( 'WP_REDIS_PORT', 6379 );",
		`define( 'WP_REDIS_PASSWORD', array( 'pf_` + testSiteID + `', 'cache\'pw\\' ) );`,
		"define( 'WP_REDIS_DATABASE', 42 );",
		"define( 'WP_REDIS_PREFIX', '" + testSiteID + ":' );",
		"define( 'WP_REDIS_SELECTIVE_FLUSH', false );",
	} {
		if !strings.Contains(string(wpConfig), want) {
			t.Errorf("wp-config.php lacks %s", want)
		}
	}

	f.cfg.RedisSecretRef = "missing-ref"
	if _, err := f.prov.UpdateSite(ctx, f.cfg); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("update with an unknown object cache reference: %v", err)
	}
}

func TestUpdateSiteRecreatesPHPWhenWPConfigChanges(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	if _, err := f.prov.CreateSite(ctx, f.cfg); err != nil {
		t.Fatal(err)
	}
	f.docker.calls = nil

	// A new image is Compose's to notice: a plain up.
	f.cfg.PHPVersion = "8.3"
	if _, err := f.prov.UpdateSite(ctx, f.cfg); err != nil {
		t.Fatal(err)
	}
	wantCommands(t, f.docker, upWait)

	// A new database password changes only wp-config.php, which Compose
	// can't see: php is recreated first.
	sec := memSecrets{}
	f.cfg.DBSecretRef, f.cfg.WPSaltsSecretRef = siteSecrets(t, sec, testSiteID,
		sitedb.Credentials{Database: "site_db", Username: "site_user", Password: "new-password"})
	f.prov = newProvisionerWith(t, f, sec)
	if _, err := f.prov.UpdateSite(ctx, f.cfg); err != nil {
		t.Fatal(err)
	}
	wantCommands(t, f.docker, recreate, upWait)
}

// TestUpdateSiteAppliesSecuritySet checks that a changed security set
// reaches a site through UpdateSite, which is how the control plane applies
// it to existing sites at startup (sites.ReconcileJobType).
func TestUpdateSiteAppliesSecuritySet(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	if _, err := f.prov.CreateSite(ctx, f.cfg); err != nil {
		t.Fatal(err)
	}
	wpConfig := filepath.Join(f.siteDir(t), compose.SiteWPConfigFile)
	if got, _ := os.ReadFile(wpConfig); strings.Contains(string(got), "DISALLOW_FILE_EDIT") {
		t.Fatalf("security set is off, but wp-config.php has it:\n%s", got)
	}
	f.docker.calls = nil

	sec := config.DefaultWordPressSecurity()
	sec.PasswordHashing = config.PasswordHashingArgon2id
	f.prov = newProvisionerWith(t, f, f.sec, func(c *compose.ProvisionerConfig) { c.WordPressSecurity = sec })
	if _, err := f.prov.UpdateSite(ctx, f.cfg); err != nil {
		t.Fatal(err)
	}
	wantCommands(t, f.docker, recreate, upWait)
	got, err := os.ReadFile(wpConfig)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"define( 'DISALLOW_FILE_EDIT', true );",
		"define( 'PRESSFLEET_BLOCK_USER_ENUMERATION', true );",
		"define( 'PRESSFLEET_PASSWORD_POLICY_MIN_LENGTH', 12 );",
		"define( 'PRESSFLEET_PASSWORD_HASHING', 'argon2id' );",
	} {
		if !strings.Contains(string(got), want) {
			t.Errorf("wp-config.php lacks %q", want)
		}
	}

	// Unchanged settings recreate nothing.
	if _, err := f.prov.UpdateSite(ctx, f.cfg); err != nil {
		t.Fatal(err)
	}
	wantCommands(t, f.docker, upWait)
}

// newProvisionerWith returns a provisioner for f that reads secrets from sec.
// opts adjust its configuration.
func newProvisionerWith(t *testing.T, f *fixture, sec memSecrets, opts ...func(*compose.ProvisionerConfig)) *compose.Provisioner {
	t.Helper()
	cfg := compose.ProvisionerConfig{
		Layout: f.layout, Secrets: sec, MariaDBContainer: testMariaDB, Docker: f.docker.exec,
		Images: imageMap{"8.4": {PHP: "a", Nginx: "b"}, "8.3": {PHP: "c", Nginx: "d"}},
	}
	for _, o := range opts {
		o(&cfg)
	}
	p, err := compose.NewProvisioner(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

// The platform's SMTP relay goes into every site's wp-config.php, and a
// changed relay reaches an existing site when it is next converged.
func TestSMTPRenderedIntoWPConfig(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	sec := memSecrets{}
	f.cfg.DBSecretRef, f.cfg.WPSaltsSecretRef = siteSecrets(t, sec, testSiteID,
		sitedb.Credentials{Database: "site_db", Username: "site_user", Password: "pw"})
	withSMTP := func(smtp *wpconfig.SMTP) *compose.Provisioner {
		p, err := compose.NewProvisioner(compose.ProvisionerConfig{
			Layout: f.layout, Secrets: sec, MariaDBContainer: testMariaDB, Docker: f.docker.exec,
			Images: imageMap{"8.4": {PHP: "a", Nginx: "b"}}, SMTP: smtp,
		})
		if err != nil {
			t.Fatal(err)
		}
		return p
	}
	wpConfig := func() string {
		b, err := os.ReadFile(filepath.Join(f.siteDir(t), compose.SiteWPConfigFile))
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}

	if _, err := withSMTP(nil).CreateSite(ctx, f.cfg); err != nil {
		t.Fatal(err)
	}
	if got := wpConfig(); strings.Contains(got, "PRESSFLEET_SMTP_") {
		t.Errorf("wp-config.php has SMTP settings without a relay:\n%s", got)
	}
	f.docker.calls = nil

	relay := &wpconfig.SMTP{Host: "smtp.example.com", Port: 587, Username: "relay", Password: "relay-pw", Encryption: "tls"}
	if _, err := withSMTP(relay).UpdateSite(ctx, f.cfg); err != nil {
		t.Fatal(err)
	}
	got := wpConfig()
	for _, want := range []string{
		"define( 'PRESSFLEET_SMTP_HOST', 'smtp.example.com' );",
		"define( 'PRESSFLEET_SMTP_PASSWORD', 'relay-pw' );",
		"define( 'PRESSFLEET_SMTP_ENCRYPTION', 'tls' );",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("wp-config.php lacks %s:\n%s", want, got)
		}
	}
	wantCommands(t, f.docker, recreate, upWait)
}

// The platform's login public key and each site's ID go into every site's
// wp-config.php (§5.9), and an existing site gets them when next converged.
func TestLoginConstantsRenderedIntoWPConfig(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	sec := memSecrets{}
	f.cfg.DBSecretRef, f.cfg.WPSaltsSecretRef = siteSecrets(t, sec, testSiteID,
		sitedb.Credentials{Database: "site_db", Username: "site_user", Password: "pw"})
	withKey := func(key string) *compose.Provisioner {
		p, err := compose.NewProvisioner(compose.ProvisionerConfig{
			Layout: f.layout, Secrets: sec, MariaDBContainer: testMariaDB, Docker: f.docker.exec,
			Images: imageMap{"8.4": {PHP: "a", Nginx: "b"}}, LoginPublicKey: key,
		})
		if err != nil {
			t.Fatal(err)
		}
		return p
	}
	wpConfig := func() string {
		b, err := os.ReadFile(filepath.Join(f.siteDir(t), compose.SiteWPConfigFile))
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}

	if _, err := withKey("").CreateSite(ctx, f.cfg); err != nil {
		t.Fatal(err)
	}
	if got := wpConfig(); strings.Contains(got, "PRESSFLEET_LOGIN_") || strings.Contains(got, "PRESSFLEET_SITE_ID") {
		t.Errorf("wp-config.php has login constants without a key:\n%s", got)
	}
	f.docker.calls = nil

	if _, err := withKey("a2V5").UpdateSite(ctx, f.cfg); err != nil {
		t.Fatal(err)
	}
	got := wpConfig()
	for _, want := range []string{
		"define( 'PRESSFLEET_SITE_ID', '" + testSiteID + "' );",
		"define( 'PRESSFLEET_LOGIN_PUBLIC_KEY', 'a2V5' );",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("wp-config.php lacks %s:\n%s", want, got)
		}
	}
	wantCommands(t, f.docker, recreate, upWait)
}

func TestConvergeRecreatesPHPAfterFailedAttempt(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	if _, err := f.prov.CreateSite(ctx, f.cfg); err != nil {
		t.Fatal(err)
	}
	// An earlier attempt rendered a new wp-config.php but failed before
	// recreating php: the file is newer than the running container.
	f.docker.phpStartedAt = time.Now().Add(-time.Hour)
	f.docker.calls = nil
	if _, err := f.prov.UpdateSite(ctx, f.cfg); err != nil {
		t.Fatal(err)
	}
	wantCommands(t, f.docker, recreate, upWait)
}

func TestUpdateSiteKeepsStoppedSiteStopped(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	if _, err := f.prov.CreateSite(ctx, f.cfg); err != nil {
		t.Fatal(err)
	}
	if err := f.prov.StopSite(ctx, testSiteID); err != nil {
		t.Fatal(err)
	}
	f.docker.calls = nil
	f.cfg.PHPVersion = "8.3"
	st, err := f.prov.UpdateSite(ctx, f.cfg)
	if err != nil {
		t.Fatal(err)
	}
	if st.Phase != provisioner.SiteStopped {
		t.Errorf("phase = %s, want stopped", st.Phase)
	}
	wantCommands(t, f.docker, "compose up --no-start --remove-orphans")
}

func TestStartStopSite(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	if _, err := f.prov.CreateSite(ctx, f.cfg); err != nil {
		t.Fatal(err)
	}
	f.docker.calls = nil
	if err := f.prov.StopSite(ctx, testSiteID); err != nil {
		t.Fatal(err)
	}
	if st, err := f.prov.GetSite(ctx, testSiteID); err != nil || st.Phase != provisioner.SiteStopped {
		t.Fatalf("after stop: %+v, %v", st, err)
	}
	// MariaDB was recreated meanwhile and lost the site network.
	delete(f.docker.mariaDBNetworks, network)
	if err := f.prov.StartSite(ctx, testSiteID); err != nil {
		t.Fatal(err)
	}
	if st, err := f.prov.GetSite(ctx, testSiteID); err != nil || st.Phase != provisioner.SiteRunning {
		t.Fatalf("after start: %+v, %v", st, err)
	}
	wantCommands(t, f.docker, "compose stop", connect, "compose start --wait --wait-timeout 180")
}

func TestAttachSharedServices(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	if _, err := f.prov.CreateSite(ctx, f.cfg); err != nil {
		t.Fatal(err)
	}
	f.docker.calls = nil
	// Already connected: nothing to do.
	if err := f.prov.AttachSharedServices(ctx, testSiteID); err != nil {
		t.Fatal(err)
	}
	wantCommands(t, f.docker)
	// MariaDB was recreated and lost the site network.
	delete(f.docker.mariaDBNetworks, network)
	if err := f.prov.AttachSharedServices(ctx, testSiteID); err != nil {
		t.Fatal(err)
	}
	wantCommands(t, f.docker, connect)
	// MariaDB is gone altogether.
	f.docker.mariaDBNetworks = nil
	if err := f.prov.AttachSharedServices(ctx, testSiteID); !errors.Is(err, compose.ErrSharedServiceUnavailable) {
		t.Errorf("err = %v, want ErrSharedServiceUnavailable", err)
	}
}

func TestDeleteSite(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	if _, err := f.prov.CreateSite(ctx, f.cfg); err != nil {
		t.Fatal(err)
	}
	f.docker.calls = nil
	if err := f.prov.DeleteSite(ctx, testSiteID); err != nil {
		t.Fatal(err)
	}
	wantCommands(t, f.docker, "network disconnect "+network+" "+testMariaDB, "compose down --remove-orphans --volumes")
	if _, err := os.Stat(f.siteDir(t)); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("site directory still exists: %v", err)
	}

	// Deleting again succeeds and doesn't try to disconnect.
	if err := f.prov.DeleteSite(ctx, testSiteID); err != nil {
		t.Fatal(err)
	}
	wantCommands(t, f.docker, "compose down --remove-orphans --volumes")

	// So does deleting with MariaDB gone.
	f.docker.mariaDBNetworks = nil
	if err := f.prov.DeleteSite(ctx, testSiteID); err != nil {
		t.Fatal(err)
	}
}

func TestMissingSite(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	calls := map[string]func() error{
		"get":    func() error { _, err := f.prov.GetSite(ctx, testSiteID); return err },
		"update": func() error { _, err := f.prov.UpdateSite(ctx, f.cfg); return err },
		"start":  func() error { return f.prov.StartSite(ctx, testSiteID) },
		"stop":   func() error { return f.prov.StopSite(ctx, testSiteID) },
		"attach": func() error { return f.prov.AttachSharedServices(ctx, testSiteID) },
	}
	for name, call := range calls {
		t.Run(name, func(t *testing.T) {
			if err := call(); !errors.Is(err, provisioner.ErrSiteNotFound) {
				t.Errorf("err = %v, want ErrSiteNotFound", err)
			}
		})
	}
	if got := f.docker.commands(); len(got) != 0 {
		t.Errorf("changed state for a missing site: %q", got)
	}
}

func TestCreateSiteErrors(t *testing.T) {
	tests := []struct {
		name   string
		modify func(*fixture)
		want   error
	}{
		{"invalid site ID", func(f *fixture) { f.cfg.ID = "../x" }, compose.ErrInvalidSite},
		{"invalid config", func(f *fixture) { f.cfg.UID = 0 }, compose.ErrInvalidSite},
		{"unknown PHP version", func(f *fixture) { f.cfg.PHPVersion = "5.6" }, compose.ErrInvalidSite},
		{"missing secret", func(f *fixture) { f.cfg.DBSecretRef = "gone" }, store.ErrNotFound},
		{"salts ref points at credentials", func(f *fixture) { f.cfg.WPSaltsSecretRef = f.cfg.DBSecretRef }, nil},
		{"MariaDB missing", func(f *fixture) { f.docker.mariaDBNetworks = nil }, compose.ErrSharedServiceUnavailable},
		{"up fails", func(f *fixture) {
			f.docker.fail = map[string]error{"compose up": &compose.CommandError{Err: errors.New("exit status 1")}}
		}, compose.ErrCommandFailed},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newFixture(t)
			tt.modify(f)
			_, err := f.prov.CreateSite(context.Background(), f.cfg)
			if err == nil || (tt.want != nil && !errors.Is(err, tt.want)) {
				t.Errorf("err = %v, want %v", err, tt.want)
			}
		})
	}
}

func TestInvalidSiteID(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	for _, id := range []string{"", "../etc", "UPPER", "a b"} {
		if _, err := f.prov.GetSite(ctx, id); !errors.Is(err, compose.ErrInvalidSite) {
			t.Errorf("GetSite(%q) = %v", id, err)
		}
		if err := f.prov.DeleteSite(ctx, id); !errors.Is(err, compose.ErrInvalidSite) {
			t.Errorf("DeleteSite(%q) = %v", id, err)
		}
	}
	if len(f.docker.calls) != 0 {
		t.Errorf("ran docker for invalid IDs: %q", f.docker.calls)
	}
}

// TestPurgeCache checks how the page cache is purged: through the running
// php container as the site's UID, or a one-off php container when the site
// is stopped (#41).
func TestPurgeCache(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	if err := f.prov.PurgeCache(ctx, testSiteID); !errors.Is(err, provisioner.ErrSiteNotFound) {
		t.Errorf("PurgeCache of a missing site = %v", err)
	}
	if _, err := f.prov.CreateSite(ctx, f.cfg); err != nil {
		t.Fatal(err)
	}
	f.docker.calls = nil

	if err := f.prov.PurgeCache(ctx, testSiteID); err != nil {
		t.Fatal(err)
	}
	wantCommands(t, f.docker, "compose exec --no-TTY --user 20042:20042 -- php find /var/cache/pressfleet/fastcgi -mindepth 1 -delete")

	if err := f.prov.StopSite(ctx, testSiteID); err != nil {
		t.Fatal(err)
	}
	f.docker.calls = nil
	if err := f.prov.PurgeCache(ctx, testSiteID); err != nil {
		t.Fatal(err)
	}
	wantCommands(t, f.docker, "compose run --rm --no-deps --no-TTY --entrypoint find -- php /var/cache/pressfleet/fastcgi -mindepth 1 -delete")

	// A php container that runs as anything but a site UID is refused, not
	// used.
	if err := f.prov.StartSite(ctx, testSiteID); err != nil {
		t.Fatal(err)
	}
	f.docker.phpUser = "0:0"
	f.docker.calls = nil
	if err := f.prov.PurgeCache(ctx, testSiteID); !errors.Is(err, compose.ErrUnexpectedUser) {
		t.Errorf("PurgeCache as root = %v", err)
	}
	wantCommands(t, f.docker)

	f.docker.phpUser = "20042:20042"
	f.docker.fail = map[string]error{"compose exec": &compose.CommandError{Stderr: "find: permission denied", Err: errors.New("exit status 1")}}
	if err := f.prov.PurgeCache(ctx, testSiteID); err == nil || !strings.Contains(err.Error(), "purge page cache") {
		t.Errorf("failed purge = %v", err)
	}
}

// TestReloadCode checks that reloading a site's code signals PHP-FPM in a
// running php container, and does nothing for a stopped site.
func TestReloadCode(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	if err := f.prov.ReloadCode(ctx, testSiteID); !errors.Is(err, provisioner.ErrSiteNotFound) {
		t.Errorf("ReloadCode of a missing site = %v", err)
	}
	if _, err := f.prov.CreateSite(ctx, f.cfg); err != nil {
		t.Fatal(err)
	}
	f.docker.calls = nil

	if err := f.prov.ReloadCode(ctx, testSiteID); err != nil {
		t.Fatal(err)
	}
	wantCommands(t, f.docker, "compose kill --signal SIGUSR2 -- php")

	if err := f.prov.StopSite(ctx, testSiteID); err != nil {
		t.Fatal(err)
	}
	f.docker.calls = nil
	if err := f.prov.ReloadCode(ctx, testSiteID); err != nil {
		t.Fatal(err)
	}
	wantCommands(t, f.docker)

	if err := f.prov.StartSite(ctx, testSiteID); err != nil {
		t.Fatal(err)
	}
	f.docker.calls = nil
	f.docker.fail = map[string]error{"compose kill": &compose.CommandError{Stderr: "no such container", Err: errors.New("exit status 1")}}
	if err := f.prov.ReloadCode(ctx, testSiteID); err == nil || !strings.Contains(err.Error(), "reload code") {
		t.Errorf("failed reload = %v", err)
	}
}
