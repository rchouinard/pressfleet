package metrics

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"slices"
	"sync"
	"time"

	"github.com/rchouinard/pressfleet/control-plane/internal/redact"
	"github.com/rchouinard/pressfleet/control-plane/internal/secrets"
	"github.com/rchouinard/pressfleet/control-plane/internal/store"
)

// Uptime check tuning. The timeout is per request; the service's collector
// timeout still bounds the whole run.
const (
	uptimeRequestTimeout = 10 * time.Second
	uptimeConcurrency    = 8
)

// AliasSource lists a site's alias domains. [store.Sites] implements it.
type AliasSource interface {
	Aliases(ctx context.Context, siteID string) ([]string, error)
}

// SecretSource reads the preview password. [secrets.Store] implements it.
type SecretSource interface {
	Get(ctx context.Context, ref secrets.Ref) (redact.Secret, error)
}

// UptimeOptions configure an [UptimeCollector].
type UptimeOptions struct {
	// Every is the collection interval; zero means the service default.
	Every time.Duration
	// Aliases and Secrets are required.
	Aliases AliasSource
	Secrets SecretSource
	// Client sends the checks. It is injectable so tests can point it at
	// httptest servers. The collector relies on seeing each domain's own
	// response and certificate, so the client must not follow redirects;
	// [NewUptimeClient] is the default.
	Client *http.Client
	// Logger is required.
	Logger *slog.Logger
}

// UptimeCollector checks each serving site over HTTPS from the control
// plane (design doc §7.1) and reports:
//
//	pressfleet_site_up                                           gauge, 1 if the site answered below 500
//	pressfleet_site_http_check_status_code                       gauge, 0 if there was no response
//	pressfleet_site_http_check_duration_seconds                  gauge, time to the response headers
//	pressfleet_site_tls_certificate_expiry_timestamp_seconds     gauge, {domain}, Unix time of the leaf certificate's NotAfter
//
// The availability metrics come from the site's main domain: the primary
// domain, or the preview domain before go-live. The certificate metric
// comes from a request to every domain of the site (preview, primary, and
// aliases). When the preview domain is protected, only it is sent the site's
// basic auth credentials (§5.1), so a protected preview is up, not a 401.
//
// Only running sites are checked, so a stopped, provisioning, updating, or
// deleted site never reads as down. A site in maintenance mode answers 503
// by design; that counts as up. A certificate that fails verification makes
// the request fail, so no expiry sample is written for the domain, and
// pressfleet_site_up goes to 0 instead.
//
// The checks run from the PressFleet host, so a full host outage silences
// them rather than reporting 0 (§7.3).
type UptimeCollector struct {
	opts UptimeOptions
}

var _ Collector = (*UptimeCollector)(nil)

// NewUptimeClient returns the production HTTP client: it doesn't follow
// redirects or reuse connections (each check does its own TLS handshake,
// so the certificate seen is current), and times out after 10 seconds.
func NewUptimeClient() *http.Client {
	return &http.Client{
		Timeout: uptimeRequestTimeout,
		Transport: &http.Transport{
			DisableKeepAlives: true,
			TLSClientConfig:   &tls.Config{MinVersion: tls.VersionTLS12},
		},
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
}

// NewUptimeCollector returns the collector.
func NewUptimeCollector(opts UptimeOptions) (*UptimeCollector, error) {
	switch {
	case opts.Aliases == nil:
		return nil, errors.New("uptime collector: an alias source is required")
	case opts.Secrets == nil:
		return nil, errors.New("uptime collector: a secret source is required")
	case opts.Logger == nil:
		return nil, errors.New("uptime collector: a logger is required")
	}
	if opts.Client == nil {
		opts.Client = NewUptimeClient()
	}
	return &UptimeCollector{opts: opts}, nil
}

// Name implements [Collector].
func (*UptimeCollector) Name() string { return "uptime" }

// Interval implements [Collector].
func (c *UptimeCollector) Interval() time.Duration { return c.opts.Every }

// Collect implements [Collector]. Sites are checked concurrently; a site
// whose domains or credentials can't be read is reported in the error, and
// the others still get their samples.
func (c *UptimeCollector) Collect(ctx context.Context, targets []Target) ([]Sample, error) {
	var (
		mu   sync.Mutex
		out  []Sample
		errs []error
		wg   sync.WaitGroup
		sem  = make(chan struct{}, uptimeConcurrency)
	)
	for _, t := range targets {
		if t.Site.Status != store.SiteRunning {
			continue
		}
		wg.Go(func() {
			select {
			case sem <- struct{}{}:
				defer func() { <-sem }()
			case <-ctx.Done():
				return
			}
			samples, err := c.checkSite(ctx, t)
			mu.Lock()
			defer mu.Unlock()
			out = append(out, samples...)
			if err != nil {
				errs = append(errs, err)
			}
		})
	}
	wg.Wait()
	return out, errors.Join(errs...)
}

// result is the outcome of one request.
type result struct {
	status   int
	duration time.Duration
	expiry   time.Time // zero without a TLS response
}

func (c *UptimeCollector) checkSite(ctx context.Context, t Target) ([]Sample, error) {
	site := t.Site
	aliases, err := c.opts.Aliases.Aliases(ctx, site.ID)
	if err != nil {
		return nil, fmt.Errorf("site %s: list aliases: %w", site.ID, err)
	}
	var (
		auth     *redact.Secret
		firstErr error
	)
	if site.PreviewProtection && site.PreviewPasswordSecretRef != "" {
		pw, err := c.opts.Secrets.Get(ctx, secrets.Ref(site.PreviewPasswordSecretRef))
		if err != nil {
			// Check anyway: without credentials the preview answers 401,
			// which still tells whether Traefik and the site are alive.
			firstErr = fmt.Errorf("site %s: read preview password: %w", site.ID, err)
		} else {
			auth = &pw
		}
	}

	mainDomain := site.PreviewDomain
	if site.PrimaryDomain != "" {
		mainDomain = site.PrimaryDomain
	}
	var domains []string
	for _, d := range slices.Concat([]string{site.PreviewDomain, site.PrimaryDomain}, aliases) {
		if d != "" && !slices.Contains(domains, d) {
			domains = append(domains, d)
		}
	}

	var out []Sample
	for _, d := range domains {
		var creds *redact.Secret
		if d == site.PreviewDomain {
			creds = auth
		}
		res, err := c.probe(ctx, d, site.PreviewUsername, creds)
		if ctx.Err() != nil {
			return out, ctx.Err()
		}
		if err != nil {
			c.opts.Logger.Debug("uptime check failed", "site_id", site.ID, "domain", d, "error", err)
		}
		if !res.expiry.IsZero() {
			out = append(out, t.Sample("pressfleet_site_tls_certificate_expiry_timestamp_seconds",
				float64(res.expiry.Unix()), "domain", d))
		}
		if d != mainDomain {
			continue
		}
		up := err == nil && (res.status < 500 || (site.Maintenance && res.status == http.StatusServiceUnavailable))
		out = append(out,
			t.Sample("pressfleet_site_up", b2f(up)),
			t.Sample("pressfleet_site_http_check_status_code", float64(res.status)),
			t.Sample("pressfleet_site_http_check_duration_seconds", res.duration.Seconds()),
		)
	}
	return out, firstErr
}

// probe requests https://domain/ and reports the status, the time to the
// response headers, and the leaf certificate's expiry.
func (c *UptimeCollector) probe(ctx context.Context, domain, username string, password *redact.Secret) (result, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://"+domain+"/", nil)
	if err != nil {
		return result{}, fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("User-Agent", "PressFleet-Uptime/1")
	if password != nil {
		req.SetBasicAuth(username, password.Reveal())
	}
	start := time.Now()
	resp, err := c.opts.Client.Do(req)
	took := time.Since(start)
	if err != nil {
		return result{duration: took}, fmt.Errorf("check %s: %w", domain, err)
	}
	defer resp.Body.Close()
	res := result{status: resp.StatusCode, duration: took}
	if resp.TLS != nil && len(resp.TLS.PeerCertificates) > 0 {
		res.expiry = resp.TLS.PeerCertificates[0].NotAfter
	}
	return res, nil
}

func b2f(b bool) float64 {
	if b {
		return 1
	}
	return 0
}
