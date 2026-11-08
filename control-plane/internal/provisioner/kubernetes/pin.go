package kubernetes

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
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
// tag names, with an anonymous token if the registry wants one (GHCR and
// Docker Hub want one even for public images).
func (p *Provisioner) manifestDigest(ctx context.Context, ref imageRef) (string, error) {
	u := registryURL(ref.host, "/v2/"+ref.repository+"/manifests/"+url.PathEscape(ref.tag), p.insecureRegistries)
	token := ""
	for attempt := 0; ; attempt++ {
		resp, err := p.registryRequest(ctx, http.MethodHead, u, token)
		if err != nil {
			return "", err
		}
		_ = resp.Body.Close()
		switch {
		case resp.StatusCode == http.StatusUnauthorized && attempt == 0:
			if token, err = p.registryToken(ctx, resp.Header.Get("WWW-Authenticate"), ref); err != nil {
				return "", err
			}
			continue
		case resp.StatusCode == http.StatusOK:
			if d := resp.Header.Get("Docker-Content-Digest"); digestRE.MatchString(d) {
				return d, nil
			}
			// Not every registry names the digest; it is the hash of the
			// manifest.
			return p.hashManifest(ctx, u, token)
		default:
			return "", fmt.Errorf("%w: %s answered %s", ErrRegistry, ref.host, resp.Status)
		}
	}
}

// hashManifest fetches the manifest and returns its digest.
func (p *Provisioner) hashManifest(ctx context.Context, u, token string) (string, error) {
	resp, err := p.registryRequest(ctx, http.MethodGet, u, token)
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

func (p *Provisioner) registryRequest(ctx context.Context, method, u, token string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, method, u, nil)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrRegistry, err)
	}
	req.Header.Set("Accept", manifestTypes)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := p.registry.Do(req)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrRegistry, err)
	}
	return resp, nil
}

// registryToken gets an anonymous pull token from the token service a
// registry's challenge names (the Docker registry token protocol).
func (p *Provisioner) registryToken(ctx context.Context, challenge string, ref imageRef) (string, error) {
	scheme, params, ok := strings.Cut(challenge, " ")
	if !ok || !strings.EqualFold(scheme, "Bearer") {
		return "", fmt.Errorf("%w: %s wants credentials (%q), and only anonymous pulls are supported", ErrRegistry, ref.host, challenge)
	}
	ch := parseChallenge(params)
	realm, err := url.Parse(ch["realm"])
	if err != nil || (realm.Scheme != "https" && realm.Scheme != "http") || realm.Host == "" {
		return "", fmt.Errorf("%w: %s names no usable token service (%q)", ErrRegistry, ref.host, challenge)
	}
	q := realm.Query()
	if s := ch["service"]; s != "" {
		q.Set("service", s)
	}
	scope := ch["scope"]
	if scope == "" {
		scope = "repository:" + ref.repository + ":pull"
	}
	q.Set("scope", scope)
	realm.RawQuery = q.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, realm.String(), nil)
	if err != nil {
		return "", fmt.Errorf("%w: %w", ErrRegistry, err)
	}
	resp, err := p.registry.Do(req)
	if err != nil {
		return "", fmt.Errorf("%w: getting a token: %w", ErrRegistry, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("%w: getting a token: %s", ErrRegistry, resp.Status)
	}
	var tok struct {
		Token       string `json:"token"`
		AccessToken string `json:"access_token"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxTokenBytes)).Decode(&tok); err != nil {
		return "", fmt.Errorf("%w: reading the token: %w", ErrRegistry, err)
	}
	if t := firstNonEmpty(tok.Token, tok.AccessToken); t != "" {
		return t, nil
	}
	return "", fmt.Errorf("%w: the token service sent no token", ErrRegistry)
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
