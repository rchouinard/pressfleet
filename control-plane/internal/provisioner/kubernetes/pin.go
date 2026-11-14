package kubernetes

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/rchouinard/pressfleet/control-plane/internal/provisioner"
)

// Image pinning (§5.5). Nodes pull images themselves, by the reference in
// the pod spec, so the driver never pulls one: it asks the image's registry
// which build a tag names, through the registry API, and pins the site to
// that digest.

var _ provisioner.ImagePinner = (*Provisioner)(nil)

// ErrRegistry is returned when an image's registry can't say which build a
// tag names: it can't be reached, refuses the request, or doesn't have the
// image.
var ErrRegistry = errors.New("kubernetes: image registry")

// DefaultRegistryTimeout bounds each request to a registry.
const DefaultRegistryTimeout = 30 * time.Second

// manifestTypes are the manifests a tag may name, multi-platform indexes
// first: the digest of the index is what a node resolves for its own
// platform.
var manifestTypes = strings.Join([]string{
	"application/vnd.oci.image.index.v1+json",
	"application/vnd.docker.distribution.manifest.list.v2+json",
	"application/vnd.oci.image.manifest.v1+json",
	"application/vnd.docker.distribution.manifest.v2+json",
}, ", ")

// Limits on what a registry sends back.
const (
	maxManifestBytes = 4 << 20
	maxTokenBytes    = 64 << 10
)

var (
	digestRE    = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)
	imageNameRE = regexp.MustCompile(`^[a-z0-9][a-z0-9._:/-]*$`)
	pinnedRE    = regexp.MustCompile(`^[a-z0-9][a-z0-9._:/-]*@sha256:[0-9a-f]{64}$`)
)

// PinImage implements provisioner.ImagePinner: the reference
// <repository>@sha256:<digest> of the build that the tag image names. The
// first call for a tag asks the registry; later ones return the same build
// until PullImage finds a newer one. A reference that is pinned already is
// returned as it is.
func (p *Provisioner) PinImage(ctx context.Context, image string) (string, error) {
	if pinnedRE.MatchString(image) {
		return image, nil
	}
	p.pinsMu.Lock()
	pinned, ok := p.pins[image]
	p.pinsMu.Unlock()
	if ok {
		return pinned, nil
	}
	return p.refreshPin(ctx, image)
}

// PullImage implements provisioner.ImagePinner. Nodes pull by digest, so it
// pulls nothing: it asks the registry for the tag's newest build, which
// PinImage returns from then on.
func (p *Provisioner) PullImage(ctx context.Context, image string) error {
	if pinnedRE.MatchString(image) {
		return nil
	}
	_, err := p.refreshPin(ctx, image)
	return err
}

// refreshPin asks the registry for the build the tag names and remembers it.
func (p *Provisioner) refreshPin(ctx context.Context, image string) (string, error) {
	ref, err := parseImageRef(image)
	if err != nil {
		return "", err
	}
	digest, err := p.manifestDigest(ctx, ref)
	if err != nil {
		return "", fmt.Errorf("pinning %s: %w", image, err)
	}
	pinned := ref.name + "@" + digest
	p.pinsMu.Lock()
	p.pins[image] = pinned
	p.pinsMu.Unlock()
	return pinned, nil
}

// imageRef is a tag reference, split up for the registry API.
type imageRef struct {
	// name is the reference without its tag, as the pinned reference
	// keeps it.
	name string
	// host is the registry's API host, and repository the repository in
	// it.
	host, repository, tag string
}

// parseImageRef splits a reference as Docker reads it: a first component
// with a dot or a port, or localhost, is the registry, otherwise Docker
// Hub; no tag means latest.
func parseImageRef(image string) (imageRef, error) {
	name, tag := image, "latest"
	if i := strings.LastIndex(image, ":"); i > strings.LastIndex(image, "/") {
		name, tag = image[:i], image[i+1:]
	}
	if !imageNameRE.MatchString(name) || tag == "" || strings.Contains(image, "@") {
		return imageRef{}, fmt.Errorf("%w: %q isn't an image tag reference", ErrRegistry, image)
	}
	ref := imageRef{name: name, tag: tag, host: "registry-1.docker.io", repository: name}
	if first, rest, ok := strings.Cut(name, "/"); ok && (strings.ContainsAny(first, ".:") || first == "localhost") {
		ref.host, ref.repository = first, rest
	}
	if ref.host == "docker.io" || ref.host == "index.docker.io" {
		ref.host = "registry-1.docker.io"
	}
	if ref.host == "registry-1.docker.io" && !strings.Contains(ref.repository, "/") {
		ref.repository = "library/" + ref.repository
	}
	return ref, nil
}

// registryURL is the URL of path on the registry host: HTTPS, except for a
// registry on the loopback interface or one of insecure
// (kubernetes.insecure_registries), which development setups run without
// TLS.
func registryURL(host, path string, insecure []string) string {
	scheme := "https"
	h := host
	if hh, _, err := net.SplitHostPort(host); err == nil {
		h = hh
	}
	if ip := net.ParseIP(h); h == "localhost" || (ip != nil && ip.IsLoopback()) || slices.Contains(insecure, host) {
		scheme = "http"
	}
	return scheme + "://" + host + path
}

// manifestDigest asks the registry for the digest of the manifest the
// tag names. It asks anonymously first, with an anonymous token if the
// registry wants one (GHCR and Docker Hub want one even for public images),
// and only when the registry demands credentials does it look for some in
// the image pull secrets (pin_auth.go).
func (p *Provisioner) manifestDigest(ctx context.Context, ref imageRef) (string, error) {
	u := registryURL(ref.host, "/v2/"+ref.repository+"/manifests/"+url.PathEscape(ref.tag), p.insecureRegistries)
	authz, creds := "", false
	for attempt := 0; ; attempt++ {
		resp, err := p.registryRequest(ctx, http.MethodHead, u, authz, creds)
		if err != nil {
			return "", err
		}
		_ = resp.Body.Close()
		switch {
		case resp.StatusCode == http.StatusUnauthorized && attempt == 0:
			c := p.pullCredentials(ctx, ref.host)
			if authz, creds, err = p.registryAuth(ctx, resp.Header.Get("WWW-Authenticate"), ref, c); err != nil {
				return "", err
			}
			continue
		case resp.StatusCode == http.StatusUnauthorized:
			return "", fmt.Errorf("%w: %s refused the credentials it was given for %s", ErrRegistry, ref.host, ref.repository)
		case resp.StatusCode == http.StatusOK:
			if d := resp.Header.Get("Docker-Content-Digest"); digestRE.MatchString(d) {
				return d, nil
			}
			// Not every registry names the digest; it is the hash of the
			// manifest.
			return p.hashManifest(ctx, u, authz, creds)
		default:
			return "", fmt.Errorf("%w: %s answered %s", ErrRegistry, ref.host, resp.Status)
		}
	}
}

// hashManifest fetches the manifest and returns its digest.
func (p *Provisioner) hashManifest(ctx context.Context, u, authz string, creds bool) (string, error) {
	resp, err := p.registryRequest(ctx, http.MethodGet, u, authz, creds)
	if err != nil {
		return "", err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("%w: fetching the manifest: %s", ErrRegistry, resp.Status)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxManifestBytes+1))
	if err != nil {
		return "", fmt.Errorf("%w: fetching the manifest: %w", ErrRegistry, err)
	}
	if len(body) > maxManifestBytes {
		return "", fmt.Errorf("%w: the manifest is larger than %d bytes", ErrRegistry, maxManifestBytes)
	}
	sum := sha256.Sum256(body)
	return "sha256:" + hex.EncodeToString(sum[:]), nil
}

// registryRequest sends one request with the Authorization header value
// authz, if any. When authz carries pull-secret credentials (creds), the
// request doesn't follow a redirect to another host.
func (p *Provisioner) registryRequest(ctx context.Context, method, u, authz string, creds bool) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, method, u, nil)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrRegistry, err)
	}
	req.Header.Set("Accept", manifestTypes)
	if authz != "" {
		req.Header.Set("Authorization", authz)
	}
	resp, err := p.registryClient(creds).Do(req)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrRegistry, err)
	}
	return resp, nil
}

// parseChallenge reads the key="value" pairs of a WWW-Authenticate
// challenge. Values may hold commas, as scopes do.
func parseChallenge(s string) map[string]string {
	out := map[string]string{}
	for s != "" {
		s = strings.TrimLeft(s, ", ")
		key, rest, ok := strings.Cut(s, "=")
		if !ok {
			break
		}
		var value string
		if strings.HasPrefix(rest, `"`) {
			end := strings.Index(rest[1:], `"`)
			if end < 0 {
				break
			}
			value, s = rest[1:end+1], rest[end+2:]
		} else {
			value, s, _ = strings.Cut(rest, ",")
		}
		out[strings.ToLower(strings.TrimSpace(key))] = value
	}
	return out
}
