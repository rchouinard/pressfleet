package compose

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
)

// ConfigHashes returns the configuration hash of each of the project's
// services (`docker compose config --hash`). Up recreates a service when its
// hash differs from the one its container was created with, so comparing
// two renderings' hashes tells which services an Up would recreate. The
// hash doesn't cover the content of bind-mounted files and secrets; see
// FileMounts.
func (p Project) ConfigHashes(ctx context.Context) (map[string]string, error) {
	out, err := p.run(ctx, nil, "config", "--hash", "*")
	if err != nil {
		return nil, err
	}
	hashes := map[string]string{}
	sc := bufio.NewScanner(bytes.NewReader(out))
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		service, hash, ok := strings.Cut(line, " ")
		if !ok || hash == "" {
			return nil, fmt.Errorf("compose project %s: unexpected config hash line %q", p.Name, line)
		}
		hashes[service] = strings.TrimSpace(hash)
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("compose project %s: reading config hashes: %w", p.Name, err)
	}
	return hashes, nil
}

// FileMounts returns, for each of the project's services, the host paths of
// the files and directories it bind-mounts and of the file-backed secrets it
// uses. Paths are absolute, resolved against the project directory. Compose
// doesn't notice when only the content of such a file changes, so a caller
// that changed one recreates the services using it.
func (p Project) FileMounts(ctx context.Context) (map[string][]string, error) {
	out, err := p.run(ctx, nil, "config", "--format", "json")
	if err != nil {
		return nil, err
	}
	var cfg struct {
		Services map[string]struct {
			Volumes []struct {
				Type   string `json:"type"`
				Source string `json:"source"`
			} `json:"volumes"`
			Secrets []struct {
				Source string `json:"source"`
			} `json:"secrets"`
		} `json:"services"`
		Secrets map[string]struct {
			File string `json:"file"`
		} `json:"secrets"`
	}
	if err := json.Unmarshal(out, &cfg); err != nil {
		return nil, fmt.Errorf("compose project %s: parsing config: %w", p.Name, err)
	}
	mounts := map[string][]string{}
	for name, svc := range cfg.Services {
		var paths []string
		for _, v := range svc.Volumes {
			if v.Type == "bind" && v.Source != "" {
				paths = append(paths, v.Source)
			}
		}
		for _, s := range svc.Secrets {
			if f := cfg.Secrets[s.Source].File; f != "" {
				paths = append(paths, f)
			}
		}
		slices.Sort(paths)
		mounts[name] = slices.Compact(paths)
	}
	return mounts, nil
}

// PullOptions configures Pull.
type PullOptions struct {
	// Missing pulls only images that aren't on the host yet, so images that
	// exist only locally (development builds) don't fail the pull.
	Missing bool
}

// Pull pulls the images of the project's services.
func (p Project) Pull(ctx context.Context, opts PullOptions) error {
	args := []string{"pull", "--quiet"}
	if opts.Missing {
		args = append(args, "--policy", "missing")
	}
	_, err := p.run(ctx, nil, args...)
	return err
}

// ImageDigests returns the registry digests of a local image (its
// RepoDigests, e.g. ghcr.io/rchouinard/pressfleet/control-plane@sha256:...),
// which are empty for an image that was built locally rather than pulled.
// image is an image ID or reference.
func ImageDigests(ctx context.Context, docker ExecFunc, image string) ([]string, error) {
	if docker == nil {
		docker = DockerExec
	}
	out, err := docker(ctx, nil, "image", "inspect", "--format", "{{json .RepoDigests}}", "--", image)
	if err != nil {
		return nil, fmt.Errorf("inspecting image %s: %w", image, err)
	}
	var digests []string
	if err := json.Unmarshal(bytes.TrimSpace(out), &digests); err != nil {
		return nil, fmt.Errorf("parsing inspect output for image %s: %w", image, err)
	}
	return digests, nil
}

// ImageDigests is the package's ImageDigests with the project's docker CLI.
func (p Project) ImageDigests(ctx context.Context, image string) ([]string, error) {
	return ImageDigests(ctx, p.Docker, image)
}
