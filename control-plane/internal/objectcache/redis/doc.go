// Package redis implements [objectcache.Manager] against the platform's
// shared object cache, a Valkey server (design doc §3, §5.1 step 4, §6). It
// speaks the Redis protocol through package resp.
//
// # Isolation
//
// Each site gets an ACL user pf_<site-id> with a random password, confined
// to keys starting with "<site-id>:" (~<site-id>:*) and to one logical
// database of its own (db=<n>, a Valkey 9 rule), with commands that reach
// beyond either denied (see siteRules). The server's default user is
// disabled, so every client authenticates.
//
// The key prefix alone can't isolate sites: SCAN, DBSIZE, and RANDOMKEY
// take no key argument, so ACL key patterns don't apply to them, and a site
// allowed to SCAN could list every other site's key names. Those names are
// sensitive: WordPress caches users by login and email address
// (userlogins:<login>, useremail:<email>). Denying SCAN isn't enough,
// because the Redis Object Cache plugin needs it: its selective flush
// (WP_REDIS_SELECTIVE_FLUSH) and every group flush (wp_cache_flush_group)
// run an EVAL script that loops over SCAN 0 MATCH '<prefix>*' and DELs what
// it finds, and commands inside a script are checked against the caller's
// ACL just like direct ones, so denying SCAN breaks the flushes; allowing
// EVAL with SCAN would still leak, since nothing forces the MATCH argument.
// KEYS has the same problem and blocks the server besides.
//
// A database per site closes the leak instead of working around it: SCAN,
// KEYS-like enumeration, DBSIZE, and RANDOMKEY only ever see the caller's
// database, which only holds its own keys, and SELECT, MOVE, COPY ... DB,
// SWAPDB, and FLUSHALL can't reach another database. It also makes a flush
// cheap and safe: FLUSHDB empties just the site's database, where the
// selective flush's SCAN walks the whole shared keyspace in one blocking
// script. The key prefix stays as a second fence.
//
// # Plugin configuration
//
// The Redis Object Cache plugin works with these users when configured
// with (#39):
//
//	WP_REDIS_HOST     pf-redis (the platform alias on the site network)
//	WP_REDIS_PORT     6379
//	WP_REDIS_PASSWORD ['pf_<site-id>', '<password>'] (username and password)
//	WP_REDIS_DATABASE <n> (the stored Credentials.Database; never 0)
//	WP_REDIS_PREFIX   '<site-id>:' (objectcache.KeyPrefix, colon included:
//	                  the plugin appends "<table prefix>:<group>:<key>")
//
// WP_REDIS_SELECTIVE_FLUSH may be on or off: both work, and with a database
// per site off is simpler and faster (FLUSHDB instead of a SCAN script).
// WP_REDIS_UNFLUSHABLE_GROUPS must stay unset: with selective flush on, the
// plugin passes ":<group>:" strings as script KEYS, which fall outside the
// key pattern and make every flush fail. WP_REDIS_CLUSTER, WP_REDIS_SERVERS,
// and replication settings don't apply. The plugin's INFO call on connect is
// allowed; without it the plugin disables itself.
//
// # State and idempotency
//
// The site_object_caches table records each site's user, database, and a
// reference to the secret holding its [objectcache.Credentials]. The row
// and the secret are written in one transaction and deleted in one
// transaction, so a credentials secret is never orphaned.
//
// CreateUser:
//
//  1. If the site has no row, pick the lowest database no row holds,
//     generate a password, and store the secret and the row in one
//     transaction. The unique database column settles two sites picking the
//     same database: the loser picks again.
//  2. Read the credentials from the secret.
//  3. Converge the server: if the user doesn't exist, flush its database
//     first, then ACL SETUSER with the full rule list (reset first, so the
//     result doesn't depend on what was there). SETUSER is atomic and
//     doesn't disconnect the user's clients.
//
// DeleteUser:
//
//  1. Delete the row and the secret in one transaction, remembering the
//     database.
//  2. ACL DELUSER, which also disconnects the user's clients.
//  3. FLUSHDB ASYNC on the database. On a retry after step 1, the database
//     is read from the user on the server; after step 2 it is lost, and the
//     data stays until evicted or until the database's next user, whose
//     creation flushes it.
//
// Flushing whenever a user is created, not only when one is deleted, is
// what makes reusing databases safe: a database can only hold another
// site's data while that site's user is gone, and the server keeps nothing
// across restarts (no persistence), so the flush never discards data a
// live user could still read.
//
// # Restarts and reconciliation
//
// Users created with ACL SETUSER live only in the server's memory; the
// platform renders a read-only config and no ACL file, so a restarted
// server knows only the admin and healthcheck users. [Manager.Reconcile]
// recreates every recorded user with its stored password and deletes site
// users that have no row. [Manager.Watch] runs it when the control plane
// starts, whenever the server's run_id changes (a restart, polled every
// few seconds), and periodically as a safety net. An ACL file written with
// ACL SAVE would survive restarts too, but not a recreated container or a
// lost volume, so reconciliation would be needed anyway; with it, the ACL
// file would add nothing but state.
//
// Reconcile runs alongside jobs without a lock. It lists the server's users
// before reading the rows, and a job's CreateUser commits its row before
// creating its user, so it never deletes the user of a site being created.
// If it recreates a user that a concurrent DeleteUser has just deleted, the
// row is already gone (DeleteUser deletes the row first), and Reconcile's
// second read of the rows, after recreating users, finds that and deletes
// the user again.
//
// # Admin connection
//
// The manager connects as the admin user (config redis.admin_user) with the
// password read from a file, by default the platform's Compose secret at
// /run/secrets/redis_admin_password, opening a connection per operation. It
// re-reads the file for every connection, and doesn't connect until needed,
// so the control plane starts while the server is down.
package redis
