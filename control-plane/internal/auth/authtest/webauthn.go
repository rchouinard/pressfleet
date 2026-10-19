// Package authtest holds test helpers for package auth: a software WebAuthn
// authenticator, so registration and login can be tested without a browser
// or a security key.
package authtest

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"slices"

	"github.com/go-webauthn/webauthn/protocol/webauthncbor"
)

// Authenticator is a software WebAuthn authenticator holding one ECDSA
// P-256 (ES256) credential, created by its first registration, with
// attestation none. Its fields say what the next response reports, so a
// test can break one thing at a time: the origin, the relying party ID,
// the flags, or the signature counter. It isn't safe for concurrent use.
type Authenticator struct {
	// Origin is the origin the "browser" reports in clientDataJSON.
	Origin string
	// RPID, if set, is hashed into the authenticator data instead of the
	// relying party ID in the options.
	RPID string
	// UserPresent and UserVerified are the UP and UV flags. New
	// authenticators set both.
	UserPresent  bool
	UserVerified bool
	// BackupEligible and BackupState are the BE and BS flags of a synced
	// passkey.
	BackupEligible bool
	BackupState    bool
	// SignCount is the signature counter. Each assertion adds one before
	// signing, unless ZeroCounter is set, as for synced passkeys.
	SignCount   uint32
	ZeroCounter bool
	// Transports are reported at registration.
	Transports []string
	// AAGUID identifies the authenticator model.
	AAGUID [16]byte
	// Challenge, if set, replaces the challenge from the options, as a
	// client answering another ceremony would.
	Challenge string

	key          *ecdsa.PrivateKey
	credentialID []byte
	userHandle   []byte
}

// Errors from an authenticator that, like a browser, refuses to answer.
var (
	// ErrExcluded means the options' excludeCredentials lists the
	// authenticator's credential: it is already registered.
	ErrExcluded = errors.New("authtest: the credential is already registered (excludeCredentials)")
	// ErrNotAllowed means the options' allowCredentials doesn't list the
	// authenticator's credential, or it has none yet.
	ErrNotAllowed = errors.New("authtest: the credential isn't allowed (allowCredentials)")
)

// NewAuthenticator returns an authenticator that reports origin, with
// user presence and user verification.
func NewAuthenticator(origin string) *Authenticator {
	return &Authenticator{
		Origin: origin, UserPresent: true, UserVerified: true,
		Transports: []string{"usb"},
		AAGUID:     [16]byte{0x50, 0x46, 0x2d, 0x74, 0x65, 0x73, 0x74}, // "PF-test"
	}
}

// CredentialID returns the credential's ID, or nil before registration.
func (a *Authenticator) CredentialID() []byte { return a.credentialID }

// UserHandle returns the user handle the credential was registered with.
func (a *Authenticator) UserHandle() []byte { return a.userHandle }

var b64 = base64.RawURLEncoding

// descriptor is a PublicKeyCredentialDescriptorJSON.
type descriptor struct {
	ID string `json:"id"`
}

func listed(ds []descriptor, id []byte) bool {
	return slices.ContainsFunc(ds, func(d descriptor) bool {
		got, err := b64.DecodeString(d.ID)
		return err == nil && string(got) == string(id)
	})
}

// Register answers navigator.credentials.create() options (the JSON form
// of PublicKeyCredentialCreationOptions, or an object with it under
// "publicKey") with a RegistrationResponseJSON. The first registration
// creates the credential; later ones register the same credential again
// unless excludeCredentials lists it.
func (a *Authenticator) Register(options []byte) (json.RawMessage, error) {
	var opts struct {
		Challenge string `json:"challenge"`
		RP        struct {
			ID string `json:"id"`
		} `json:"rp"`
		User struct {
			ID string `json:"id"`
		} `json:"user"`
		ExcludeCredentials []descriptor `json:"excludeCredentials"`
	}
	if err := decodeOptions(options, &opts); err != nil {
		return nil, err
	}
	if a.credentialID != nil && listed(opts.ExcludeCredentials, a.credentialID) {
		return nil, ErrExcluded
	}
	handle, err := b64.DecodeString(opts.User.ID)
	if err != nil {
		return nil, fmt.Errorf("authtest: user.id: %w", err)
	}
	if a.key == nil {
		if a.key, err = ecdsa.GenerateKey(elliptic.P256(), rand.Reader); err != nil {
			return nil, err
		}
		a.credentialID = make([]byte, 32)
		_, _ = rand.Read(a.credentialID)
	}
	a.userHandle = handle

	clientData, err := a.clientData("webauthn.create", opts.Challenge)
	if err != nil {
		return nil, err
	}
	coseKey, err := a.coseKey()
	if err != nil {
		return nil, err
	}
	authData := a.authData(opts.RP.ID, true)
	authData = append(authData, a.AAGUID[:]...)
	authData = binary.BigEndian.AppendUint16(authData, uint16(len(a.credentialID))) //nolint:gosec // G115: 32
	authData = append(authData, a.credentialID...)
	authData = append(authData, coseKey...)
	attestation, err := webauthncbor.Marshal(map[string]any{
		"fmt": "none", "attStmt": map[string]any{}, "authData": authData,
	})
	if err != nil {
		return nil, err
	}
	return json.Marshal(map[string]any{
		"id":    b64.EncodeToString(a.credentialID),
		"rawId": b64.EncodeToString(a.credentialID),
		"type":  "public-key",
		"response": map[string]any{
			"clientDataJSON":    b64.EncodeToString(clientData),
			"attestationObject": b64.EncodeToString(attestation),
			"transports":        a.Transports,
		},
		"clientExtensionResults":  map[string]any{},
		"authenticatorAttachment": "cross-platform",
	})
}

// Assert answers navigator.credentials.get() options (the JSON form of
// PublicKeyCredentialRequestOptions, or an object with it under
// "publicKey") with an AuthenticationResponseJSON.
func (a *Authenticator) Assert(options []byte) (json.RawMessage, error) {
	var opts struct {
		Challenge        string       `json:"challenge"`
		RPID             string       `json:"rpId"`
		AllowCredentials []descriptor `json:"allowCredentials"`
	}
	if err := decodeOptions(options, &opts); err != nil {
		return nil, err
	}
	if a.key == nil || (len(opts.AllowCredentials) > 0 && !listed(opts.AllowCredentials, a.credentialID)) {
		return nil, ErrNotAllowed
	}
	if !a.ZeroCounter {
		a.SignCount++
	}
	clientData, err := a.clientData("webauthn.get", opts.Challenge)
	if err != nil {
		return nil, err
	}
	authData := a.authData(opts.RPID, false)
	clientHash := sha256.Sum256(clientData)
	digest := sha256.Sum256(append(slices.Clone(authData), clientHash[:]...))
	sig, err := ecdsa.SignASN1(rand.Reader, a.key, digest[:])
	if err != nil {
		return nil, err
	}
	return json.Marshal(map[string]any{
		"id":    b64.EncodeToString(a.credentialID),
		"rawId": b64.EncodeToString(a.credentialID),
		"type":  "public-key",
		"response": map[string]any{
			"clientDataJSON":    b64.EncodeToString(clientData),
			"authenticatorData": b64.EncodeToString(authData),
			"signature":         b64.EncodeToString(sig),
			"userHandle":        b64.EncodeToString(a.userHandle),
		},
		"clientExtensionResults":  map[string]any{},
		"authenticatorAttachment": "cross-platform",
	})
}

// decodeOptions decodes options, unwrapping a "publicKey" member.
func decodeOptions(options []byte, v any) error {
	var wrapped struct {
		PublicKey json.RawMessage `json:"publicKey"`
	}
	if err := json.Unmarshal(options, &wrapped); err == nil && len(wrapped.PublicKey) > 0 {
		options = wrapped.PublicKey
	}
	if err := json.Unmarshal(options, v); err != nil {
		return fmt.Errorf("authtest: decoding options: %w", err)
	}
	return nil
}

func (a *Authenticator) clientData(typ, challenge string) ([]byte, error) {
	if a.Challenge != "" {
		challenge = a.Challenge
	}
	return json.Marshal(map[string]any{
		"type": typ, "challenge": challenge, "origin": a.Origin, "crossOrigin": false,
	})
}

// Authenticator data flags (WebAuthn §6.1).
const (
	flagUP = 1 << 0
	flagUV = 1 << 2
	flagBE = 1 << 3
	flagBS = 1 << 4
	flagAT = 1 << 6
)

// authData returns the authenticator data up to the signature counter,
// with the attested credential data flag for a registration.
func (a *Authenticator) authData(rpID string, attested bool) []byte {
	if a.RPID != "" {
		rpID = a.RPID
	}
	rpHash := sha256.Sum256([]byte(rpID))
	var flags byte
	for _, f := range []struct {
		set bool
		bit byte
	}{{a.UserPresent, flagUP}, {a.UserVerified, flagUV}, {a.BackupEligible, flagBE}, {a.BackupState, flagBS}, {attested, flagAT}} {
		if f.set {
			flags |= f.bit
		}
	}
	data := append(rpHash[:], flags)
	return binary.BigEndian.AppendUint32(data, a.SignCount)
}

// coseKey returns the credential's public key as a COSE_Key: EC2 (1),
// ES256 (-7), P-256 (1), and the coordinates.
func (a *Authenticator) coseKey() ([]byte, error) {
	pub, err := a.key.PublicKey.Bytes() // 0x04 || X || Y
	if err != nil {
		return nil, err
	}
	return webauthncbor.Marshal(map[int]any{1: 2, 3: -7, -1: 1, -2: pub[1:33], -3: pub[33:65]})
}
