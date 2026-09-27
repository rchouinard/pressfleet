package secrets

import (
	"encoding/base64"
	"errors"
	"fmt"
	"strings"

	"github.com/rchouinard/pressfleet/control-plane/internal/redact"
)

// Sealed settings are credentials in the configuration (the config file and
// PRESSFLEET_* variables), such as the SMTP relay password. They are set at
// install time by the host CLI, which never touches the state database
// (§4.7), so they can't live in the secrets table. Instead the CLI encrypts
// them with the master key, and the config file, the rendered platform
// project, and the data directory's backups only ever hold ciphertext:
//
//	pfenc1:<key ID>:<base64url of the ciphertext>
//
// The ciphertext is sealed like a stored secret, with the setting's name in
// place of the row, so a value can't be moved to another setting.
const sealedSettingPrefix = "pfenc1:"

// kindSetting is the kind authenticated with every sealed setting.
const kindSetting Kind = "setting"

// ErrNotSealed means a setting that must be encrypted with the master key
// holds something else, typically a plaintext password.
var ErrNotSealed = errors.New("value is not encrypted with the master key")

// SealSetting encrypts plaintext as the value of the named setting (e.g.
// "smtp.password") with the current key.
func (r *Keyring) SealSetting(name string, plaintext redact.Secret) string {
	keyID, ciphertext := r.seal(settingRow(name), kindSetting, plaintext)
	return sealedSettingPrefix + keyID + ":" + base64.RawURLEncoding.EncodeToString(ciphertext)
}

// OpenSetting decrypts the value of the named setting made by SealSetting.
// It returns ErrNotSealed if the value doesn't have SealSetting's format,
// ErrUnknownKey if it was encrypted with another master key, and ErrDecrypt
// if it was modified or sealed for another setting.
func (r *Keyring) OpenSetting(name, sealed string) (redact.Secret, error) {
	keyID, ciphertext, err := parseSealedSetting(sealed)
	if err != nil {
		return "", fmt.Errorf("%s: %w", name, err)
	}
	plain, err := r.open(settingRow(name), kindSetting, keyID, ciphertext)
	if err != nil {
		return "", fmt.Errorf("%s: %w", name, err)
	}
	return plain, nil
}

// IsSealedSetting reports whether s has the format SealSetting produces. It
// does not check that s decrypts.
func IsSealedSetting(s string) bool {
	_, _, err := parseSealedSetting(s)
	return err == nil
}

func parseSealedSetting(s string) (keyID string, ciphertext []byte, err error) {
	rest, ok := strings.CutPrefix(s, sealedSettingPrefix)
	if !ok {
		return "", nil, ErrNotSealed
	}
	keyID, encoded, ok := strings.Cut(rest, ":")
	if !ok || keyID == "" {
		return "", nil, ErrNotSealed
	}
	ciphertext, err = base64.RawURLEncoding.DecodeString(encoded)
	if err != nil || len(ciphertext) == 0 {
		return "", nil, ErrNotSealed
	}
	return keyID, ciphertext, nil
}

// settingRow is what a sealed setting's AAD binds it to in place of a
// secret ID. The prefix keeps it apart from secret IDs, which are ULIDs.
func settingRow(name string) string { return "setting:" + name }
