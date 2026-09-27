package ingress

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"

	"github.com/rchouinard/pressfleet/control-plane/internal/redact"
)

// DNS-01 credentials (config.Config.ACME.DNSProvider).
//
// Traefik's ACME client, lego, reads a DNS provider's credentials from
// environment variables named by the provider (CF_DNS_API_TOKEN for
// Cloudflare, AWS_ACCESS_KEY_ID and AWS_SECRET_ACCESS_KEY for Route 53, and
// so on), or from files named by <VARIABLE>_FILE variables. PressFleet gets
// them to Traefik like this:
//
//  1. pressfleet init reads them (VARIABLE=value lines) and seals them with
//     the master key as the setting config.ACMEDNSCredentialsSetting
//     (secrets.Keyring.SealSetting). The config file and the rendered
//     platform project, which passes them to the control plane, only hold
//     that ciphertext.
//  2. The control plane decrypts them when it starts and writes one file per
//     variable, named after the variable, into DNSCredentialsDir. That
//     directory is a memory-only (tmpfs) volume the platform project shares
//     between the control plane (read-write) and Traefik (read-only), so
//     plaintext never reaches a disk and disappears when the platform stops.
//     It then writes the DNSCredentialsReady marker.
//  3. Traefik's entrypoint waits for the marker, then starts Traefik with
//     <VARIABLE>_FILE pointing at each file, so the values never appear in
//     Traefik's environment or the container's configuration.
//
// Lego reads the files when Traefik first needs the DNS provider, and
// Traefik keeps the provider, so Traefik must be recreated after the
// credentials change.

// DNSCredentialsDir is where the control plane writes the decrypted DNS
// provider credentials and where Traefik reads them, in both containers.
const DNSCredentialsDir = "/run/pressfleet/acme-dns" //nolint:gosec // G101: a path, not a credential

// DNSCredentialsReady is the marker file in DNSCredentialsDir that says the
// control plane has written the credentials (possibly none).
const DNSCredentialsReady = ".ready"

// ErrInvalidDNSCredentials means DNS provider credentials couldn't be
// parsed.
var ErrInvalidDNSCredentials = errors.New("invalid DNS provider credentials")

// DNSCredentials maps the environment variables a DNS provider reads to
// their values.
type DNSCredentials map[string]redact.Secret

// credentialName matches environment variable names. Names ending in _FILE
// are refused: Traefik gets <name>_FILE for every credential, and lego
// would read a _FILE_FILE variable as nothing at all.
var credentialName = regexp.MustCompile(`^[A-Z][A-Z0-9_]{0,127}$`)

// maxCredentialsSize bounds what ParseDNSCredentials reads. Real provider
// credentials are a few variables of at most a few kilobytes.
const maxCredentialsSize = 64 << 10

// ParseDNSCredentials reads VARIABLE=value lines, the format of lego's
// documentation and of Docker env files. Blank lines and lines starting
// with # are skipped. Values are taken literally up to the end of the line
// (quotes are part of the value), with surrounding whitespace trimmed.
// Errors name the line, never its value.
func ParseDNSCredentials(r io.Reader) (DNSCredentials, error) {
	creds := DNSCredentials{}
	sc := bufio.NewScanner(io.LimitReader(r, maxCredentialsSize+1))
	sc.Buffer(make([]byte, 0, 4096), maxCredentialsSize+1)
	size, n := 0, 0
	for sc.Scan() {
		n++
		size += len(sc.Bytes()) + 1
		if size > maxCredentialsSize {
			return nil, fmt.Errorf("%w: more than %d bytes", ErrInvalidDNSCredentials, maxCredentialsSize)
		}
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		name, value, ok := strings.Cut(line, "=")
		name, value = strings.TrimSpace(name), strings.TrimSpace(value)
		switch {
		case !ok:
			return nil, fmt.Errorf("%w: line %d is not VARIABLE=value", ErrInvalidDNSCredentials, n)
		case !credentialName.MatchString(name):
			return nil, fmt.Errorf("%w: line %d: %q is not an environment variable name like CF_DNS_API_TOKEN", ErrInvalidDNSCredentials, n, name)
		case strings.HasSuffix(name, "_FILE"):
			return nil, fmt.Errorf("%w: line %d: give %s's value itself, not a file", ErrInvalidDNSCredentials, n, name)
		case value == "":
			return nil, fmt.Errorf("%w: line %d: %s has no value", ErrInvalidDNSCredentials, n, name)
		}
		if _, dup := creds[name]; dup {
			return nil, fmt.Errorf("%w: line %d: %s is given twice", ErrInvalidDNSCredentials, n, name)
		}
		creds[name] = redact.Secret(value)
	}
	if err := sc.Err(); err != nil {
		if errors.Is(err, bufio.ErrTooLong) {
			return nil, fmt.Errorf("%w: more than %d bytes", ErrInvalidDNSCredentials, maxCredentialsSize)
		}
		return nil, fmt.Errorf("reading DNS provider credentials: %w", err)
	}
	if len(creds) == 0 {
		return nil, fmt.Errorf("%w: no VARIABLE=value lines", ErrInvalidDNSCredentials)
	}
	return creds, nil
}

// Names returns the variable names, sorted. Names aren't secret.
func (c DNSCredentials) Names() []string {
	names := make([]string, 0, len(c))
	for name := range c {
		names = append(names, name)
	}
	slices.Sort(names)
	return names
}

// Marshal returns the credentials as VARIABLE=value lines, sorted, which
// ParseDNSCredentials reads back.
func (c DNSCredentials) Marshal() redact.Secret {
	var b strings.Builder
	for _, name := range c.Names() {
		b.WriteString(name + "=" + c[name].Reveal() + "\n")
	}
	return redact.Secret(b.String())
}

// WriteDNSCredentials writes one file per credential into dir (normally
// DNSCredentialsDir), readable only by its owner, removes files of
// credentials no longer given, and then writes the DNSCredentialsReady
// marker. c may be empty: the marker then tells Traefik to start without
// credentials.
func WriteDNSCredentials(dir string, c DNSCredentials) error {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return fmt.Errorf("writing DNS provider credentials: %w", err)
	}
	for _, e := range entries {
		if _, keep := c[e.Name()]; !keep && e.Name() != DNSCredentialsReady {
			if err := os.Remove(filepath.Join(dir, e.Name())); err != nil {
				return fmt.Errorf("removing old DNS provider credential: %w", err)
			}
		}
	}
	for _, name := range c.Names() {
		if err := writeFileAtomic(dir, name, []byte(c[name].Reveal()), 0o400); err != nil {
			return fmt.Errorf("writing DNS provider credential %s: %w", name, err)
		}
	}
	if err := writeFileAtomic(dir, DNSCredentialsReady, nil, 0o444); err != nil {
		return fmt.Errorf("writing DNS provider credentials marker: %w", err)
	}
	return nil
}

// writeFileAtomic replaces dir/name, so Traefik never reads a partly
// written value.
func writeFileAtomic(dir, name string, content []byte, mode os.FileMode) error {
	f, err := os.CreateTemp(dir, ".tmp-*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer func() { _ = os.Remove(tmp) }() // no-op after the rename
	if err := f.Chmod(mode); err != nil {
		_ = f.Close()
		return err
	}
	if _, err := f.Write(content); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, filepath.Join(dir, name))
}
