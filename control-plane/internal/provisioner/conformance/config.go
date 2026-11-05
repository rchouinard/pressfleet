package conformance

import (
	"fmt"
	"strings"
	"testing"

	"github.com/rchouinard/pressfleet/control-plane/internal/id"
	"github.com/rchouinard/pressfleet/control-plane/internal/provisioner"
	"github.com/rchouinard/pressfleet/control-plane/internal/wpconfig"
)

// environmentProbe prints what WordPress sees of a site's environment:
// WordPress's own answer, whether the constants are defined, and the mail
// switch's value.
const environmentProbe = `
echo wp_get_environment_type(), '|',
	defined( '` + wpconfig.ConstEnvironmentType + `' ) ? constant( '` + wpconfig.ConstEnvironmentType + `' ) : 'undefined', '|',
	defined( '` + wpconfig.ConstDisableMail + `' ) ? var_export( constant( '` + wpconfig.ConstDisableMail + `' ), true ) : 'undefined';`

// testEnvironment checks that SiteConfig's Environment and DisableMail (§5.11)
// reach the site's PHP as WP_ENVIRONMENT_TYPE and the mail switch's
// constant, on a new site and, through UpdateSite, on a running one, and
// that provisioner.HasMailSwitch answers through ExecWPCLI.
func testEnvironment(t *testing.T, env Env) {
	f := newFixture(t, env)
	ctx := context10m(t)
	cfg := f.create(ctx, t)
	f.install(ctx, t, cfg, "Environment")
	seen := func(cfg *provisioner.SiteConfig) (wordpress, environment, mail string) {
		t.Helper()
		out := f.wp(ctx, t, cfg, "eval", environmentProbe)
		parts := strings.Split(out, "|")
		if len(parts) != 3 {
			t.Fatalf("environment probe printed %q", out)
		}
		return parts[0], parts[1], parts[2]
	}
	want := func(cfg *provisioner.SiteConfig, what, wpType, constant, mail string) {
		t.Helper()
		gotWP, gotEnv, gotMail := seen(cfg)
		if gotWP != wpType {
			t.Errorf("%s: wp_get_environment_type() = %q, want %q", what, gotWP, wpType)
		}
		// A production site may define the constant or leave it out.
		if gotEnv != constant && (constant != "production" || gotEnv != "undefined") {
			t.Errorf("%s: %s = %q, want %q", what, wpconfig.ConstEnvironmentType, gotEnv, constant)
		}
		if gotMail != mail && (mail != "false" || gotMail != "undefined") {
			t.Errorf("%s: %s = %q, want %q", what, wpconfig.ConstDisableMail, gotMail, mail)
		}
	}

	// The defaults: production, with mail on.
	want(cfg, "default", "production", "production", "false")

	// A change reaches the running site through UpdateSite.
	cfg.Environment, cfg.DisableMail = provisioner.EnvironmentStaging, true
	if _, err := f.d.UpdateSite(ctx, cfg); err != nil {
		t.Fatal(err)
	}
	want(cfg, "staging with mail off", "staging", "staging", "true")

	// The two are independent.
	cfg.DisableMail = false
	if _, err := f.d.UpdateSite(ctx, cfg); err != nil {
		t.Fatal(err)
	}
	want(cfg, "staging with mail on", "staging", "staging", "false")
	cfg.Environment, cfg.DisableMail = provisioner.EnvironmentProduction, true
	if _, err := f.d.UpdateSite(ctx, cfg); err != nil {
		t.Fatal(err)
	}
	want(cfg, "production with mail off", "production", "production", "true")

	// Back to the defaults.
	cfg.Environment, cfg.DisableMail = "", false
	if _, err := f.d.UpdateSite(ctx, cfg); err != nil {
		t.Fatal(err)
	}
	want(cfg, "back to the defaults", "production", "production", "false")

	// A new site has them from the start.
	staging := f.create(ctx, t, func(c *provisioner.SiteConfig) {
		c.Environment, c.DisableMail = provisioner.EnvironmentStaging, true
	})
	f.install(ctx, t, staging, "Staging")
	want(staging, "new staging site", "staging", "staging", "true")

	// The image has the mail switch, found through ExecWPCLI the way
	// staging creation looks for it, before WordPress is installed too.
	for _, c := range []*provisioner.SiteConfig{cfg, staging} {
		present, err := provisioner.HasMailSwitch(ctx, f.d, c.ID)
		if err != nil || !present {
			t.Errorf("HasMailSwitch(%s) = %v, %v; want true", c.ID, present, err)
		}
	}
	fresh := f.create(ctx, t)
	if present, err := provisioner.HasMailSwitch(ctx, f.d, fresh.ID); err != nil || !present {
		t.Errorf("HasMailSwitch before WordPress is installed = %v, %v; want true", present, err)
	}
	for _, c := range []provisioner.Component{provisioner.ComponentPHP, provisioner.ComponentNginx} {
		f.exec(ctx, t, fresh, c, "test", "-f", provisioner.MailSwitchPath)
	}

	// The check errors for a site that isn't running or doesn't exist, and
	// never answers "absent" for them.
	if err := f.d.StopSite(ctx, fresh.ID); err != nil {
		t.Fatal(err)
	}
	_, err := provisioner.HasMailSwitch(ctx, f.d, fresh.ID)
	wantErr(t, "HasMailSwitch on a stopped site", err, provisioner.ErrSiteNotRunning)
	_, err = provisioner.HasMailSwitch(ctx, f.d, id.New())
	wantErr(t, "HasMailSwitch on a missing site", err, provisioner.ErrSiteNotFound)
}

// testObjectCache checks that a site given object cache credentials can use
// the cache the driver points sites at, and has no cache settings without
// them.
func testObjectCache(t *testing.T, env Env) {
	if env.ObjectCache == nil {
		t.Skip("Env.ObjectCache is not set")
	}
	f := newFixture(t, env)
	ctx := context10m(t)
	cfg := f.create(ctx, t)
	f.install(ctx, t, cfg, "Object cache")
	if got := f.wp(ctx, t, cfg, "eval", "echo defined( 'WP_REDIS_HOST' ) ? 'defined' : 'undefined';"); got != "undefined" {
		t.Errorf("a site without object cache credentials has WP_REDIS_HOST: %s", got)
	}

	secret, creds, err := objectCacheFor(env, cfg.ID)
	if err != nil {
		t.Fatal(err)
	}
	cfg.RedisSecretRef = cfg.ID + "-redis"
	f.setup.Secrets.put(cfg.RedisSecretRef, secret)
	cfg.EnableCache = true
	if _, err := f.d.UpdateSite(ctx, cfg); err != nil {
		t.Fatal(err)
	}
	// The site talks to the server with its own settings: the connection,
	// the database, and the prefix its credentials allow.
	const script = `
$r = new Redis();
$r->connect( WP_REDIS_HOST, WP_REDIS_PORT );
$r->auth( WP_REDIS_PASSWORD );
$r->select( WP_REDIS_DATABASE );
$k = WP_REDIS_PREFIX . 'pf-conformance';
$r->set( $k, 'ok' );
echo WP_REDIS_DATABASE, '|', WP_REDIS_PREFIX, '|', $r->get( $k );
$r->del( $k );`
	want := fmt.Sprintf("%d|%s|ok", creds.Database, creds.KeyPrefix)
	if got := f.wp(ctx, t, cfg, "eval", script); got != want {
		t.Errorf("object cache from the site = %q, want %q", got, want)
	}
}

// testPinImage checks that PinImage turns a site image tag into a reference
// that SiteConfig accepts, that pinning again gives the same one, and that a
// site runs with pinned images.
func testPinImage(t *testing.T, env Env) {
	f := newFixture(t, env)
	ctx := context10m(t)
	images := env.Images[env.PHPVersion]
	pins := map[provisioner.Component]string{}
	for c, tag := range map[provisioner.Component]string{provisioner.ComponentPHP: images.PHP, provisioner.ComponentNginx: images.Nginx} {
		pinned, err := f.d.PinImage(ctx, tag)
		if err != nil {
			t.Fatalf("PinImage(%s): %v", tag, err)
		}
		if again, err := f.d.PinImage(ctx, tag); err != nil || again != pinned {
			t.Errorf("PinImage(%s) again = %q, %v; want %s", tag, again, err, pinned)
		}
		pins[c] = pinned
	}
	cfg := f.siteConfig(t)
	cfg.PinnedImages = pins
	if err := cfg.Validate(); err != nil {
		t.Fatalf("a config with the pins is invalid: %v", err)
	}
	f.cleanupSite(t, cfg.ID)
	if st, err := f.d.CreateSite(ctx, cfg); err != nil || st.Phase != provisioner.SiteRunning {
		t.Fatalf("CreateSite with pinned images = %+v, %v", st, err)
	}
	f.install(ctx, t, cfg, "Pinned")
	if _, err := f.d.UpdateSite(ctx, cfg); err != nil {
		t.Errorf("UpdateSite with pinned images: %v", err)
	}
}
