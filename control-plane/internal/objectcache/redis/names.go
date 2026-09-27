package redis

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strconv"
	"strings"

	"github.com/rchouinard/pressfleet/control-plane/internal/id"
	"github.com/rchouinard/pressfleet/control-plane/internal/objectcache"
	"github.com/rchouinard/pressfleet/control-plane/internal/redact"
)

// userPrefix starts every site user name, marking the users as
// PressFleet's; Reconcile only ever deletes users with this prefix.
const userPrefix = "pf_"

// siteUser is a validated site user: its name, key prefix, and database.
// Names can only be built by newSiteUser from a valid site ID (26 lowercase
// base32 characters), so they and the key pattern never contain spaces or
// glob characters, which is what makes them safe as ACL SETUSER arguments.
type siteUser struct {
	siteID string
	name   string
}

// newSiteUser validates siteID and returns its user.
func newSiteUser(siteID string) (siteUser, error) {
	if !id.Valid(siteID) {
		return siteUser{}, fmt.Errorf("%w: %q", ErrInvalidSiteID, siteID)
	}
	return siteUser{siteID: siteID, name: userPrefix + siteID}, nil
}

func (u siteUser) String() string { return u.name }

// keyPattern is the ACL key pattern the user is restricted to.
func (u siteUser) keyPattern() string { return "~" + objectcache.KeyPrefix(u.siteID) + "*" }

// siteIDFromUser returns the site ID of a PressFleet site user name, or
// false for any other user.
func siteIDFromUser(name string) (string, bool) {
	s, ok := strings.CutPrefix(name, userPrefix)
	if !ok || !id.Valid(s) {
		return "", false
	}
	return s, true
}

// siteRules are the ACL SETUSER rules of a site user, applied in order.
//
// The user starts from nothing (reset), gets its password as a hash (so the
// plaintext never appears in the command, the slow log, or MONITOR), its
// key prefix, no Pub/Sub channels, and its one database (db=, a Valkey 9
// rule that also confines SELECT, MOVE, COPY ... DB, and scripts). Then
// every command minus those that reach beyond the site's own keys and
// database:
//
//   - @admin and @dangerous: server administration and commands that act
//     on the whole server or keyspace (CONFIG, DEBUG, MONITOR, CLIENT LIST
//     and KILL, KEYS, FLUSHALL, SWAPDB, MIGRATE, SORT with BY/GET patterns,
//     ...).
//   - @pubsub: channels are shared by all databases.
//   - @scripting, except EVAL/EVALSHA (and their _RO forms), SCRIPT LOAD,
//     and SCRIPT EXISTS: functions (FUNCTION, FCALL) and SCRIPT FLUSH, KILL,
//     DEBUG, and SHOW act on state shared by every user. Commands a script
//     calls are checked against the user's ACL like direct ones.
//   - CLUSTER, and CLIENT and MEMORY except the subcommands that only
//     concern the caller's own connection or keys.
//
// INFO and FLUSHDB are @dangerous but given back: the Redis Object Cache
// plugin runs INFO on every connection (and disables itself if it fails),
// and FLUSHDB only reaches the site's own database. INFO discloses server
// aggregates (memory, per-database key counts, command statistics), never
// key names or values.
//
// SCAN, DBSIZE, and RANDOMKEY have no key argument, so the key prefix
// can't confine them, but the database rule does: they only see the site's
// own database, which only holds its own keys. That is what closes the SCAN
// leak a prefix-only design has (see the package documentation).
func siteRules(u siteUser, db int, passwordHash string) []string {
	return []string{
		"reset", "on", "#" + passwordHash, u.keyPattern(), "resetchannels", "db=" + strconv.Itoa(db),
		"+@all", "-@admin", "-@dangerous", "-@pubsub",
		"-@scripting", "+eval", "+evalsha", "+eval_ro", "+evalsha_ro", "+script|load", "+script|exists",
		"-cluster",
		"-client", "+client|id", "+client|getname", "+client|setname", "+client|info", "+client|setinfo",
		"-memory", "+memory|usage",
		"+info", "+flushdb",
	}
}

// newPassword returns a random password for a site user: 26 base32
// characters, 130 bits from crypto/rand.
func newPassword() redact.Secret {
	return redact.Secret(rand.Text())
}

// passwordHash returns the hex SHA-256 of password, the form of an ACL
// "#<hash>" rule.
func passwordHash(password redact.Secret) string {
	sum := sha256.Sum256([]byte(password.Reveal()))
	return hex.EncodeToString(sum[:])
}

// validHash reports whether h has the form passwordHash produces.
func validHash(h string) bool {
	if len(h) != sha256.Size*2 {
		return false
	}
	_, err := hex.DecodeString(h)
	return err == nil && strings.ToLower(h) == h
}
