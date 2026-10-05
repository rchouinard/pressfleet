package e2e

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Published releases (#100), which upgrade tests install first.

// ReleaseRepository is the GitHub repository releases are published in.
const ReleaseRepository = "rchouinard/pressfleet"

// releaseCLIAsset is the CLI binary's asset name in a release.
const releaseCLIAsset = "pressfleet-linux-amd64"

// releaseTimeout bounds downloading a release's CLI.
const releaseTimeout = 5 * time.Minute

// errNoRelease says a release doesn't exist (or isn't visible without a
// token, while the repository is private).
var errNoRelease = errors.New("release not found")

// ReleaseCLI returns the path of the pressfleet CLI of a published release,
// downloaded from GitHub and checked against the release's SHA256SUMS. It
// is cached in the user's cache directory, and checked again on each use.
//
// The download authenticates with GH_TOKEN or GITHUB_TOKEN when one is set,
// which a private repository needs (locally, `GH_TOKEN=$(gh auth token)`;
// make test-e2e does that when gh is installed). If the release doesn't
// exist, the test is skipped, except in CI (CI=true), where the release the
// test needs must exist.
func ReleaseCLI(t *testing.T, version string) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), releaseTimeout)
	defer cancel()
	cache, err := os.UserCacheDir()
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(cache, "pressfleet-e2e", "releases", version)
	path, err := releaseCLI(ctx, dir, version)
	if errors.Is(err, errNoRelease) && os.Getenv("CI") != "true" {
		t.Skipf("release %s of %s: %v (while the repository is private, set GH_TOKEN)", version, ReleaseRepository, err)
	}
	if err != nil {
		t.Fatalf("getting the CLI of release %s: %v", version, err)
	}
	return path
}

// releaseCLI downloads the CLI of a release into dir unless it is there,
// and verifies it.
func releaseCLI(ctx context.Context, dir, version string) (string, error) {
	path := filepath.Join(dir, "pressfleet")
	sumsPath := filepath.Join(dir, "SHA256SUMS")
	if verifyRelease(path, sumsPath) == nil {
		return path, nil
	}
	assets, err := releaseAssets(ctx, version)
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return "", err
	}
	for name, dst := range map[string]string{"SHA256SUMS": sumsPath, releaseCLIAsset: path} {
		url, ok := assets[name]
		if !ok {
			return "", fmt.Errorf("release %s has no asset %s", version, name)
		}
		if err := download(ctx, url, dst); err != nil {
			return "", fmt.Errorf("downloading %s: %w", name, err)
		}
	}
	if err := verifyRelease(path, sumsPath); err != nil {
		return "", err
	}
	return path, os.Chmod(path, 0o755) //nolint:gosec // G302: an executable
}

// verifyRelease checks the CLI at path against the release's SHA256SUMS.
func verifyRelease(path, sumsPath string) error {
	sums, err := os.ReadFile(sumsPath) //nolint:gosec // G304: the test's cache
	if err != nil {
		return err
	}
	var want string
	for line := range strings.Lines(string(sums)) {
		if sum, name, ok := strings.Cut(strings.TrimSpace(line), "  "); ok && name == releaseCLIAsset {
			want = sum
		}
	}
	if want == "" {
		return fmt.Errorf("SHA256SUMS lists no %s", releaseCLIAsset)
	}
	data, err := os.ReadFile(path) //nolint:gosec // G304: the test's cache
	if err != nil {
		return err
	}
	if got := sha256.Sum256(data); hex.EncodeToString(got[:]) != want {
		return fmt.Errorf("%s doesn't match the release's SHA256SUMS", releaseCLIAsset)
	}
	return nil
}

// releaseAssets returns the API URLs of a release's assets by name. The
// API, unlike the browser download links, serves assets of a private
// repository to a token.
func releaseAssets(ctx context.Context, version string) (map[string]string, error) {
	body, err := githubGet(ctx, "https://api.github.com/repos/"+ReleaseRepository+"/releases/tags/"+version, "application/vnd.github+json")
	if err != nil {
		return nil, err
	}
	var release struct {
		Assets []struct {
			Name string `json:"name"`
			URL  string `json:"url"`
		} `json:"assets"`
	}
	if err := json.Unmarshal(body, &release); err != nil {
		return nil, fmt.Errorf("parsing release %s: %w", version, err)
	}
	assets := map[string]string{}
	for _, a := range release.Assets {
		assets[a.Name] = a.URL
	}
	return assets, nil
}

// download writes an asset to dst.
func download(ctx context.Context, url, dst string) error {
	body, err := githubGet(ctx, url, "application/octet-stream")
	if err != nil {
		return err
	}
	return os.WriteFile(dst, body, 0o600)
}

// githubGet gets a GitHub API resource, authenticated when a token is set.
func githubGet(ctx context.Context, url, accept string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", accept)
	for _, name := range []string{"GH_TOKEN", "GITHUB_TOKEN"} {
		if token := os.Getenv(name); token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
			break
		}
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	switch {
	case resp.StatusCode == http.StatusNotFound:
		return nil, errNoRelease
	case resp.StatusCode != http.StatusOK:
		return nil, fmt.Errorf("GET %s: %s", url, resp.Status)
	}
	return body, nil
}
