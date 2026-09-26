// Package secrets encrypts stored secrets with the platform master key
// (design doc §4.1 and §6): database and Redis passwords, Restic repository
// keys, DNS provider and S3 credentials.
//
// # Master key
//
// The master key is 32 random bytes, written as text for operators:
//
//	pfmk1:<base64url of the key followed by a 4-byte checksum>
//
// The prefix names the format version, and the checksum turns a truncated or
// mistyped key into a clear "garbled" error at startup rather than a
// confusing "wrong key". [GenerateMasterKey] makes one; [LoadMasterKey]
// reads it from the environment or a key file, which must not live in the
// data directory (the data directory is backed up, and the key must be kept
// separate from the host and its backups).
//
// # Encryption
//
// Secrets are sealed with AES-256-GCM using a random 96-bit nonce per
// encryption. The AES key is derived from the master key with HKDF rather
// than used directly, so other purposes can derive independent keys from the
// same master key later. A stored ciphertext is
//
//	format byte (1) || nonce (12) || ciphertext || tag (16)
//
// The additional authenticated data binds each ciphertext to its row: the
// format, the key ID, the secret ID, and the kind. A ciphertext copied into
// another row, or a row whose kind was edited, fails to decrypt.
//
// # Key IDs and rotation
//
// Each master key has a key ID derived from it, stored with every secret
// (secrets.key_id) and authenticated with the ciphertext. A [Keyring] finds
// the key for a secret by its ID. Rotation is not implemented yet; it will
// load the new key and the retired ones into the keyring, re-encrypt every
// secret whose key_id isn't the current key's (the column is indexed for
// that), and record the change in the master_keys table.
//
// # Wrong-key detection
//
// The first time a key is used with a database, [Verify] records its key ID
// and a check value (an HMAC of a constant under the key) as the current key
// in master_keys. On every later start, a different key fails fast with
// [ErrKeyMismatch] instead of failing on some later decryption.
//
// # Storage
//
// [Store] wraps a [store.Querier] like the store repositories do, so it works
// on a *store.DB or inside [store.DB.InTx], where provisioning creates a site
// and its secrets atomically. Plaintext only travels as [redact.Secret].
package secrets
