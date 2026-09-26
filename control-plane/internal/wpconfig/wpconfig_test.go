package wpconfig

import (
	"bytes"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/rchouinard/pressfleet/control-plane/internal/redact"
	"github.com/rchouinard/pressfleet/control-plane/internal/sitedb"
)

var update = flag.Bool("update", false, "rewrite golden files in testdata/")

// testSalts are fixed so the golden files are stable. Two contain the
// characters that need escaping.
func testSalts() Salts {
	return Salts{
		AuthKey:        "auth-key-0123456789",
		SecureAuthKey:  "secure-auth-key !@#$%^&*()",
		LoggedInKey:    "logged-in-key []{}<>~`+=,.;:/?|",
		NonceKey:       "nonce-key",
		AuthSalt:       `auth-salt-with-'quote'`,
		SecureAuthSalt: `secure-auth-salt-with-\backslash\`,
		LoggedInSalt:   "logged-in-salt",
		NonceSalt:      "nonce-salt",
	}
}

func testParams() Params {
	return Params{
		DB: sitedb.Credentials{
			Database: "site_01jabc",
			Username: "site_01jabc",
			Password: `pa$$'word\`,
		},
		DBHost: "pf-mariadb:3306",
		Salts:  testSalts(),
	}
}

func fullParams() Params {
	p := testParams()
	p.TablePrefix = "wp_x7_"
	p.Redis = &Redis{Host: "pf-redis", Port: 6379, Username: "site-01jabc", Password: "redis'pw", Prefix: "01jabc:"}
	p.SMTP = &SMTP{
		Host: "smtp.example.com", Port: 587, Username: "mailer", Password: `smtp\pw`,
		Encryption: "tls", FromEmail: "noreply@example.com", FromName: "Acme's Site",
	}
	p.Constants = []Constant{
		{Name: "WP_DEBUG", Value: false},
		{Name: "WP_POST_REVISIONS", Value: 10},
		{Name: "WP_ENVIRONMENT_TYPE", Value: "staging"},
		{Name: "ACME_API_KEY", Value: redact.Secret("k'); system('id'); //")},
		{Name: "ACME_NEGATIVE", Value: -1},
	}
	return p
}

func TestRenderGolden(t *testing.T) {
	for _, tc := range []struct {
		golden string
		params Params
	}{
		{"minimal.php.golden", testParams()},
		{"full.php.golden", fullParams()},
	} {
		t.Run(tc.golden, func(t *testing.T) {
			got, err := Render(tc.params)
			if err != nil {
				t.Fatal(err)
			}
			golden := filepath.Join("testdata", tc.golden)
			if *update {
				if err := os.WriteFile(golden, got, 0o644); err != nil {
					t.Fatal(err)
				}
			}
			want, err := os.ReadFile(golden)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(got, want) {
				t.Errorf("output differs from %s (run go test -update to accept):\n%s", golden, got)
			}
		})
	}
}

// TestRenderContract checks the constants images/README.md requires, so they
// survive golden updates.
func TestRenderContract(t *testing.T) {
	out, err := Render(testParams())
	if err != nil {
		t.Fatal(err)
	}
	s := string(out)
	for _, want := range []string{
		"define( 'WPMU_PLUGIN_DIR', '/opt/pressfleet/mu-plugins' );",
		"define( 'WPMU_PLUGIN_URL', '/_pressfleet/mu-plugins' );",
		"define( 'FS_METHOD', 'direct' );",
		"define( 'WP_AUTO_UPDATE_CORE', false );",
		"define( 'DISABLE_WP_CRON', true );",
		"define( 'DB_HOST', 'pf-mariadb:3306' );",
		"define( 'DB_CHARSET', 'utf8mb4' );",
		"$table_prefix = 'wp_';",
		"\tdefine( 'ABSPATH', __DIR__ . '/' );",
	} {
		if !strings.Contains(s, want+"\n") {
			t.Errorf("missing %q", want)
		}
	}
	if !strings.HasPrefix(s, "<?php\n") {
		t.Error("does not start with <?php")
	}
	if !strings.HasSuffix(s, "require_once ABSPATH . 'wp-settings.php';\n") {
		t.Error("does not end by loading wp-settings.php")
	}
	if strings.Contains(s, "?>") {
		t.Error("contains a closing tag")
	}
	for _, name := range []string{"AUTH_KEY", "SECURE_AUTH_KEY", "LOGGED_IN_KEY", "NONCE_KEY", "AUTH_SALT", "SECURE_AUTH_SALT", "LOGGED_IN_SALT", "NONCE_SALT"} {
		if !strings.Contains(s, "define( '"+name+"', ") {
			t.Errorf("salt %s not defined", name)
		}
	}
	// Go-live changes the URL in the database (§5.2); getenv() can't work
	// with clear_env = yes; unset M2 settings emit nothing.
	for _, absent := range []string{"'WP_HOME'", "'WP_SITEURL'", "getenv", "WP_REDIS_", "PRESSFLEET_SMTP_", "Site constants"} {
		if strings.Contains(s, absent) {
			t.Errorf("output contains %q", absent)
		}
	}
}

func TestLiteral(t *testing.T) {
	tests := []struct{ in, want string }{
		{"", `''`},
		{"plain", `'plain'`},
		{"it's", `'it\'s'`},
		{`back\slash`, `'back\\slash'`},
		{`trailing\`, `'trailing\\'`},
		{`\'`, `'\\\''`},
		{`'); system('id'); //`, `'\'); system(\'id\'); //'`},
		{"?> <?php echo 1;", `'?> <?php echo 1;'`},
		{`$var {$x} \n`, `'$var {$x} \\n'`},
		{"line\nbreak\r\x00nul", "'line\nbreak\r\x00nul'"},
		{"\xff\xfe", "'\xff\xfe'"},
	}
	for _, tt := range tests {
		if got := literal(tt.in); got != tt.want {
			t.Errorf("literal(%q) = %q, want %q", tt.in, got, tt.want)
		}
		if got, err := parseSingleQuoted(literal(tt.in)); err != nil || got != tt.in {
			t.Errorf("parse(literal(%q)) = %q, %v", tt.in, got, err)
		}
	}
}

// FuzzLiteral checks that every string round-trips through PHP's
// single-quoted string rules and can't end the literal early.
func FuzzLiteral(f *testing.F) {
	for _, s := range []string{"", "'", `\`, `\'`, `'\`, "a'b\\c", "?>", "\x00\n"} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) {
		lit := literal(s)
		got, err := parseSingleQuoted(lit)
		if err != nil {
			t.Fatalf("literal(%q) = %q: %v", s, lit, err)
		}
		if got != s {
			t.Fatalf("literal(%q) round-trips to %q", s, got)
		}
	})
}

// parseSingleQuoted implements PHP's lexing of a single-quoted string: the
// literal must end exactly at its last byte, and only \\ and \' are escapes.
func parseSingleQuoted(lit string) (string, error) {
	if len(lit) < 2 || lit[0] != '\'' {
		return "", errors.New("no opening quote")
	}
	var b strings.Builder
	for i := 1; i < len(lit); i++ {
		switch c := lit[i]; {
		case c == '\\' && i+1 < len(lit) && (lit[i+1] == '\\' || lit[i+1] == '\''):
			b.WriteByte(lit[i+1])
			i++
		case c == '\'':
			if i != len(lit)-1 {
				return "", fmt.Errorf("string ends at byte %d of %d", i, len(lit))
			}
			return b.String(), nil
		default:
			b.WriteByte(c)
		}
	}
	return "", errors.New("unterminated string")
}

func TestRenderValues(t *testing.T) {
	p := testParams()
	p.Constants = []Constant{
		{Name: "A_STRING", Value: "x'y"},
		{Name: "A_SECRET", Value: redact.Secret(`s\`)},
		{Name: "A_TRUE", Value: true},
		{Name: "A_FALSE", Value: false},
		{Name: "AN_INT", Value: 42},
		{Name: "lower_case_ok", Value: ""},
	}
	out, err := Render(p)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		`define( 'A_STRING', 'x\'y' );`,
		`define( 'A_SECRET', 's\\' );`,
		`define( 'A_TRUE', true );`,
		`define( 'A_FALSE', false );`,
		`define( 'AN_INT', 42 );`,
		`define( 'lower_case_ok', '' );`,
		`define( 'DB_PASSWORD', 'pa$$\'word\\' );`,
	} {
		if !bytes.Contains(out, []byte(want+"\n")) {
			t.Errorf("missing %s", want)
		}
	}
}

func TestRenderInvalid(t *testing.T) {
	const secret = "TOPSECRET"
	tests := map[string]struct {
		mutate func(*Params)
		want   string
	}{
		"no database":         {func(p *Params) { p.DB.Database = "" }, "database name is required"},
		"no password":         {func(p *Params) { p.DB.Password = "" }, "database password is required"},
		"no host":             {func(p *Params) { p.DBHost = "" }, "database host is required"},
		"bad prefix":          {func(p *Params) { p.TablePrefix = "wp-'" }, "table prefix"},
		"missing salt":        {func(p *Params) { p.Salts.NonceSalt = "" }, "salt NONCE_SALT is empty"},
		"bad constant name":   {func(p *Params) { p.Constants = []Constant{{Name: "X'); " + secret, Value: "v"}} }, "PHP identifier"},
		"leading digit":       {func(p *Params) { p.Constants = []Constant{{Name: "1ABC", Value: "v"}} }, "PHP identifier"},
		"empty name":          {func(p *Params) { p.Constants = []Constant{{Value: "v"}} }, "PHP identifier"},
		"long name":           {func(p *Params) { p.Constants = []Constant{{Name: strings.Repeat("A", 129), Value: "v"}} }, "PHP identifier"},
		"reserved":            {func(p *Params) { p.Constants = []Constant{{Name: "DISABLE_WP_CRON", Value: false}} }, "set by the platform"},
		"reserved case":       {func(p *Params) { p.Constants = []Constant{{Name: "wp_home", Value: secret}} }, "set by the platform"},
		"reserved salt":       {func(p *Params) { p.Constants = []Constant{{Name: "AUTH_KEY", Value: secret}} }, "set by the platform"},
		"reserved prefix":     {func(p *Params) { p.Constants = []Constant{{Name: "DB_PASSWORD2", Value: secret}} }, "reserved for the platform"},
		"redis prefix":        {func(p *Params) { p.Constants = []Constant{{Name: "WP_REDIS_HOST", Value: secret}} }, "reserved for the platform"},
		"duplicate":           {func(p *Params) { p.Constants = []Constant{{Name: "X", Value: 1}, {Name: "x", Value: 2}} }, "defined twice"},
		"float value":         {func(p *Params) { p.Constants = []Constant{{Name: "X", Value: 1.5}} }, "must be a string, bool, or int"},
		"nil value":           {func(p *Params) { p.Constants = []Constant{{Name: "X"}} }, "must be a string, bool, or int"},
		"redis incomplete":    {func(p *Params) { p.Redis = &Redis{Host: "pf-redis", Port: 6379, Password: secret} }, "redis: host, username"},
		"redis port":          {func(p *Params) { r := fullParams().Redis; r.Port = 0; p.Redis = r }, "redis: port"},
		"smtp no host":        {func(p *Params) { p.SMTP = &SMTP{Port: 25, Password: secret} }, "smtp: host is required"},
		"smtp bad encryption": {func(p *Params) { s := fullParams().SMTP; s.Encryption = "starttls"; p.SMTP = s }, "smtp: encryption"},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			p := testParams()
			p.DB.Password = secret
			tt.mutate(&p)
			out, err := Render(p)
			if !errors.Is(err, ErrInvalidParams) {
				t.Fatalf("err = %v, want ErrInvalidParams", err)
			}
			if out != nil {
				t.Error("output returned with an error")
			}
			if !strings.Contains(err.Error(), tt.want) {
				t.Errorf("err = %v, want it to mention %q", err, tt.want)
			}
			if strings.Contains(err.Error(), secret) || strings.Contains(err.Error(), "pa$$") {
				t.Errorf("error leaks a secret: %v", err)
			}
		})
	}
}

func TestGenerateSalts(t *testing.T) {
	charset := regexp.MustCompile(`^[a-zA-Z0-9!@#$%^&*()\-_ \[\]{}<>~` + "`" + `+=,.;:/?|]{64}$`)
	seen := map[string]bool{}
	for range 20 {
		s := GenerateSalts()
		for _, f := range s.fields() {
			v := f.value.Reveal()
			if !charset.MatchString(v) {
				t.Errorf("%s = %q: wrong length or character set", f.name, v)
			}
			if seen[v] {
				t.Errorf("duplicate salt %q", v)
			}
			seen[v] = true
		}
	}
	// Every character of the alphabet shows up across many draws, so none is
	// excluded by an off-by-one in the sampling.
	all := randomString(20000)
	for _, c := range saltChars {
		if !strings.ContainsRune(all, c) {
			t.Errorf("character %q never generated", c)
		}
	}
	if len(saltChars) != 92 {
		t.Errorf("alphabet has %d characters, want WordPress's 92", len(saltChars))
	}
}

func TestSaltsRoundTrip(t *testing.T) {
	s := GenerateSalts()
	enc, err := MarshalSalts(s)
	if err != nil {
		t.Fatal(err)
	}
	if fmt.Sprint(enc) != redact.Placeholder {
		t.Error("encoded salts are not redacted when formatted")
	}
	got, err := UnmarshalSalts(enc)
	if err != nil {
		t.Fatal(err)
	}
	if got != s {
		t.Error("salts differ after a round trip")
	}
	// The stored form is a JSON object keyed by constant name.
	enc, err = MarshalSalts(testSalts())
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(enc.Reveal(), `"AUTH_SALT":"auth-salt-with-'quote'"`) {
		t.Errorf("unexpected encoding: %s", enc.Reveal())
	}

	if _, err := MarshalSalts(Salts{}); err == nil {
		t.Error("MarshalSalts accepted empty salts")
	}
}

func TestUnmarshalSaltsInvalid(t *testing.T) {
	valid, err := MarshalSalts(testSalts())
	if err != nil {
		t.Fatal(err)
	}
	tests := map[string]redact.Secret{
		"not json":    "TOPSECRET",
		"empty":       "{}",
		"missing key": redact.Secret(strings.Replace(valid.Reveal(), `"NONCE_SALT"`, `"OTHER_SALT"`, 1)),
		"empty value": redact.Secret(strings.Replace(valid.Reveal(), `"nonce-salt"`, `""`, 1)),
		"extra key":   redact.Secret(strings.Replace(valid.Reveal(), "{", `{"EXTRA":"TOPSECRET",`, 1)),
		"wrong type":  `{"AUTH_KEY":1}`,
	}
	for name, in := range tests {
		t.Run(name, func(t *testing.T) {
			_, err := UnmarshalSalts(in)
			if err == nil {
				t.Fatal("no error")
			}
			if strings.Contains(err.Error(), "TOPSECRET") || strings.Contains(err.Error(), "salt-with") {
				t.Errorf("error leaks input: %v", err)
			}
		})
	}
}
