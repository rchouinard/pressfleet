package kubernetes

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/rchouinard/pressfleet/control-plane/internal/redact"
)

// Registry credentials for image pinning. The control plane asks a registry
// anonymously first, and only when it answers 401 does it read the image
// pull secrets (kubernetes.image_pull_secrets, in the platform namespace),
// the same ones kubelet pulls with, and answer the challenge with the entry
// that names the registry. The platform's Role already lets the control
// plane read those Secrets (it copies them into site namespaces).

// registryCredential is a user and password for one registry, from an image
// pull secret.
type registryCredential struct {
	user     string
	password redact.Secret
}

// header is the credential as an HTTP Basic Authorization header value.
func (c *registryCredential) header() string {
	return "Basic " + base64.StdEncoding.EncodeToString([]byte(c.user+":"+c.password.Reveal()))
}

// registryClient is the registry client; with creds, a copy that refuses to
// follow a redirect to another host, so credentials never leave the host
// they were meant for.
func (p *Provisioner) registryClient(creds bool) *http.Client {
	if !creds {
		return p.registry
	}
	c := *p.registry
	prev := c.CheckRedirect
	c.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		if req.URL.Host != via[0].URL.Host {
			return fmt.Errorf("%w: refusing to send credentials on a redirect from %s to %s", ErrRegistry, via[0].URL.Host, req.URL.Host)
		}
		if prev != nil {
			return prev(req, via)
		}
		if len(via) >= 10 {
			return errors.New("stopped after 10 redirects")
		}
		return nil
	}
	return &c
}

// registryAuth answers a registry's 401 challenge: the Authorization header
// value to retry with, and whether it carries credentials from the pull
// secrets. A Bearer challenge gets a token from the token service the
// challenge names (the Docker registry token protocol), with Basic
// credentials if c has any and anonymously otherwise; a Basic challenge is
// answered with c, or fails if there is none.
func (p *Provisioner) registryAuth(ctx context.Context, challenge string, ref imageRef, c *registryCredential) (authz string, creds bool, err error) {
	scheme, params, _ := strings.Cut(challenge, " ")
	switch {
	case strings.EqualFold(scheme, "Basic") && c != nil:
		return c.header(), true, nil
	case strings.EqualFold(scheme, "Bearer"):
		tok, err := p.registryToken(ctx, params, challenge, ref, c)
		if err != nil {
			return "", false, err
		}
		return "Bearer " + tok, false, nil
	case strings.EqualFold(scheme, "Basic"):
		return "", false, fmt.Errorf("%w: %s wants credentials, and no image pull secret (kubernetes.image_pull_secrets) has any for it", ErrRegistry, ref.host)
	default:
		return "", false, fmt.Errorf("%w: %s sent a challenge that isn't supported (%q)", ErrRegistry, ref.host, challenge)
	}
}

// registryToken gets a pull token from the token service a registry's
// challenge names. c's credentials go to the service only over HTTPS or to
// the registry's own host, and never follow a redirect to another host.
func (p *Provisioner) registryToken(ctx context.Context, params, challenge string, ref imageRef, c *registryCredential) (string, error) {
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
	send := c != nil && (realm.Scheme == "https" || realm.Host == ref.host)
	if send {
		req.Header.Set("Authorization", c.header())
	}
	resp, err := p.registryClient(send).Do(req)
	if err != nil {
		return "", fmt.Errorf("%w: getting a token: %w", ErrRegistry, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if send && (resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden) {
		return "", fmt.Errorf("%w: %s refused the credentials in the image pull secrets: %s", ErrRegistry, realm.Host, resp.Status)
	}
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

// pullCredentials finds the credentials for host in the image pull secrets
// of the platform namespace, or nil if no secret has an entry for it. A
// secret that can't be read is logged and skipped.
func (p *Provisioner) pullCredentials(ctx context.Context, host string) *registryCredential {
	for _, name := range p.pullSecrets {
		s, err := p.typed.CoreV1().Secrets(p.platformNamespace).Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			// A public image still pins without it, so don't fail here: the
			// registry's answer says whether credentials were needed.
			p.log.WarnContext(ctx, "can't read an image pull secret for pinning", "secret", name, "namespace", p.platformNamespace, "error", err)
			continue
		}
		if c := credentialFor(s.Data[corev1.DockerConfigJsonKey], host); c != nil {
			return c
		}
	}
	return nil
}

// credentialFor reads a .dockerconfigjson and returns its credentials for
// the registry host. Entries match by host alone (Docker writes them as
// host, https://host or https://host/v1/), and the names of Docker Hub all
// match its API host. A document or entry that doesn't parse names no one.
func credentialFor(dockerConfig []byte, host string) *registryCredential {
	var cfg struct {
		Auths map[string]struct {
			Username string `json:"username"`
			Password string `json:"password"`
			Auth     string `json:"auth"`
		} `json:"auths"`
	}
	if json.Unmarshal(dockerConfig, &cfg) != nil {
		return nil
	}
	for key, e := range cfg.Auths {
		if normalizeRegistryHost(key) != host {
			continue
		}
		user, password := e.Username, e.Password
		if e.Auth != "" {
			raw, err := base64.StdEncoding.DecodeString(e.Auth)
			if err != nil {
				continue
			}
			u, pw, ok := strings.Cut(string(raw), ":")
			if !ok {
				continue
			}
			user, password = u, pw
		}
		if user == "" {
			continue
		}
		return &registryCredential{user: user, password: redact.Secret(password)}
	}
	return nil
}

// normalizeRegistryHost reduces an auths key to the registry host as
// parseImageRef names it.
func normalizeRegistryHost(key string) string {
	if _, rest, ok := strings.Cut(key, "://"); ok {
		key = rest
	}
	host, _, _ := strings.Cut(key, "/")
	host = strings.ToLower(host)
	if host == "docker.io" || host == "index.docker.io" {
		return "registry-1.docker.io"
	}
	return host
}
