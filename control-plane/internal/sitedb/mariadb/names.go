package mariadb

import (
	"crypto/rand"
	"crypto/sha1" //nolint:gosec // G505: MariaDB's mysql_native_password hash format is defined with SHA-1
	"encoding/hex"
	"fmt"
	"strings"

	"github.com/rchouinard/pressfleet/control-plane/internal/id"
	"github.com/rchouinard/pressfleet/control-plane/internal/redact"
)

// namePrefix starts every site database and user name. Site IDs can start
// with a digit, and the prefix also marks the objects as PressFleet's.
const namePrefix = "pf_"

// siteName is a validated site database name, which is also the site's
// MariaDB user name. It can only be built by newSiteName, so holding one
// proves the name is "pf_" followed by a site ID: lowercase letters and
// digits, 29 characters in all (MariaDB allows 64 for databases and 80 for
// users). That is what makes the quoting below safe.
type siteName struct{ s string }

// newSiteName validates siteID and returns its database and user name.
func newSiteName(siteID string) (siteName, error) {
	if !id.Valid(siteID) {
		return siteName{}, fmt.Errorf("%w: %q", ErrInvalidSiteID, siteID)
	}
	return siteName{s: namePrefix + siteID}, nil
}

func (n siteName) String() string { return n.s }

// ident returns the name as a quoted identifier, for CREATE DATABASE and
// DROP DATABASE. Doubling backticks is MariaDB's escape inside a quoted
// identifier; a valid name has none, so this is defense in depth.
func (n siteName) ident() string {
	return "`" + strings.ReplaceAll(n.s, "`", "``") + "`"
}

// grantIdent returns the name as a quoted database identifier for GRANT.
// In GRANT, _ and % in a database name are wildcards, so "pf_x" would also
// match "pfax"; escaping them grants exactly this one database.
func (n siteName) grantIdent() string {
	s := strings.ReplaceAll(n.s, `\`, `\\`)
	s = strings.ReplaceAll(s, "_", `\_`)
	s = strings.ReplaceAll(s, "%", `\%`)
	return "`" + strings.ReplaceAll(s, "`", "``") + "`"
}

// account returns the site's MariaDB account, 'pf_<id>'@'%'. The user
// connects from its site's php container, whose address isn't fixed, so the
// host part is the wildcard; the platform network is not published.
func (n siteName) account() string {
	return "'" + strings.ReplaceAll(n.s, "'", "''") + "'@'%'"
}

// newPassword returns a random password for a site user: 26 base32
// characters, 130 bits from crypto/rand.
func newPassword() redact.Secret {
	return redact.Secret(rand.Text())
}

// nativePasswordHash returns the mysql_native_password hash of password, as
// used by IDENTIFIED BY PASSWORD: "*" and the uppercase hex SHA-1 of the
// SHA-1 of the password.
//
// MariaDB can't bind a password as a statement parameter (IDENTIFIED BY ? is
// a syntax error, even in a prepared statement), so the password would have
// to be escaped into the SQL text. Sending the hash instead means the
// plaintext never appears in a statement, the processlist, or the server's
// logs, and the value spliced into SQL is always 41 characters of [*0-9A-F],
// which needs no escaping.
func nativePasswordHash(password redact.Secret) string {
	h1 := sha1.Sum([]byte(password.Reveal())) //nolint:gosec // G401: required by the mysql_native_password format
	h2 := sha1.Sum(h1[:])                     //nolint:gosec // G401: required by the mysql_native_password format
	return "*" + strings.ToUpper(hex.EncodeToString(h2[:]))
}
