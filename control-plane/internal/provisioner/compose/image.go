package compose

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
)

// EnsureImage makes an image available to the local Docker daemon, pulling
// it only if it isn't there yet, so locally built images (make images) are
// used as they are. It reports whether it pulled. A nil docker means
// DockerExec.
func EnsureImage(ctx context.Context, docker ExecFunc, image string) (pulled bool, err error) {
	if docker == nil {
		docker = DockerExec
	}
	if _, err := docker(ctx, nil, "image", "inspect", "--format", "{{.Id}}", "--", image); err == nil {
		return false, nil
	} else if !stderrContains(err, "no such image") {
		return false, fmt.Errorf("inspecting image %s: %w", image, err)
	}
	if _, err := docker(ctx, nil, "image", "pull", "--quiet", "--", image); err != nil {
		return false, fmt.Errorf("pulling image %s: %w", image, err)
	}
	return true, nil
}

// ErrImageFileMissing means an image lacks a file its user needs.
var ErrImageFileMissing = errors.New("the image doesn't have the file")

// EnsureImageFile checks that a local image has a file. If it doesn't, the tag
// may be an old build left on the host, which EnsureImage is happy with, so it
// pulls the tag once and checks again. It reports whether the pull fixed it. An
// image that has the file is never pulled, so locally built images (make images)
// are used as they are. Without the file it returns an error that wraps
// ErrImageFileMissing, and says why the pull didn't help if it failed. A nil
// docker means DockerExec.
func EnsureImageFile(ctx context.Context, docker ExecFunc, image, path string) (refreshed bool, err error) {
	if docker == nil {
		docker = DockerExec
	}
	has := func() error {
		_, err := RunContainer(ctx, docker, RunOptions{Image: image, Entrypoint: "ls", Command: []string{path}})
		return err
	}
	if has() == nil {
		return false, nil
	}
	if _, err := docker(ctx, nil, "image", "pull", "--quiet", "--", image); err != nil {
		return false, fmt.Errorf("%w %s: %s, and pulling it failed: %w", ErrImageFileMissing, path, image, err)
	}
	if err := has(); err != nil {
		return false, fmt.Errorf("%w %s: %s, even after pulling it: %w", ErrImageFileMissing, path, image, err)
	}
	return true, nil
}

// BuildImage builds an image tagged tag from a Dockerfile without a build
// context (`docker build -`), such as one that adds a layer to an existing
// image. Tests use it to derive site images. It adds BuildFlags from the
// environment, as `make image` does. A nil docker means DockerExec.
func BuildImage(ctx context.Context, docker ExecFunc, tag, dockerfile string) error {
	if docker == nil {
		docker = DockerExec
	}
	args := append([]string{"build", "--quiet"}, BuildFlags(os.Getenv)...)
	args = append(args, "--tag", tag, "-")
	if _, err := docker(ctx, strings.NewReader(dockerfile), args...); err != nil {
		return fmt.Errorf("building image %s: %w", tag, err)
	}
	return nil
}

// proxyVars are the proxy variables BuildFlags passes on, which Docker
// predefines as build args: RUN steps see them, but the image and its
// history don't keep them.
var proxyVars = []string{"HTTP_PROXY", "http_proxy", "HTTPS_PROXY", "https_proxy", "ALL_PROXY", "all_proxy", "FTP_PROXY", "ftp_proxy"}

// BuildFlags returns the extra `docker build` options for builds behind a
// TLS-intercepting proxy, by the rules of images/build-flags.sh, which the
// Makefile uses (docs/testing.md): PRESSFLEET_BUILD_EXTRA_CA as the extra_ca
// build secret, PRESSFLEET_BUILD_NETWORK as --network, and the proxy
// variables that are set as build args. A proxy on a loopback address is
// left out unless the network is host, since a build container can't reach
// the host's loopback otherwise; NO_PROXY goes only with a proxy. With none
// of them set it returns nothing.
func BuildFlags(getenv func(string) string) []string {
	var flags []string
	if ca := getenv("PRESSFLEET_BUILD_EXTRA_CA"); ca != "" {
		flags = append(flags, "--secret", "id=extra_ca,src="+ca)
	}
	network := getenv("PRESSFLEET_BUILD_NETWORK")
	if network != "" {
		flags = append(flags, "--network", network)
	}
	proxied := false
	for _, v := range proxyVars {
		value := getenv(v)
		if value == "" || (network != "host" && loopbackProxy(value)) {
			continue
		}
		// Without a value, docker takes the build arg from its environment.
		flags = append(flags, "--build-arg", v)
		proxied = true
	}
	if proxied {
		for _, v := range []string{"NO_PROXY", "no_proxy"} {
			if getenv(v) != "" {
				flags = append(flags, "--build-arg", v)
			}
		}
	}
	return flags
}

// loopbackProxy reports whether a proxy URL (with or without a scheme)
// names a loopback host.
func loopbackProxy(proxy string) bool {
	if _, rest, ok := strings.Cut(proxy, "://"); ok {
		proxy = rest
	}
	return proxy == "localhost" || strings.HasPrefix(proxy, "localhost:") || strings.HasPrefix(proxy, "localhost/") ||
		strings.HasPrefix(proxy, "127.") || strings.HasPrefix(proxy, "[::1]")
}

// TagImage points tag at image. A nil docker means DockerExec.
func TagImage(ctx context.Context, docker ExecFunc, image, tag string) error {
	if docker == nil {
		docker = DockerExec
	}
	if _, err := docker(ctx, nil, "image", "tag", image, tag); err != nil {
		return fmt.Errorf("tagging image %s as %s: %w", image, tag, err)
	}
	return nil
}

// RemoveImage removes an image tag; a missing image is not an error. A nil
// docker means DockerExec.
func RemoveImage(ctx context.Context, docker ExecFunc, image string) error {
	if docker == nil {
		docker = DockerExec
	}
	if _, err := docker(ctx, nil, "image", "rm", "--", image); err != nil && !stderrContains(err, "no such image") {
		return fmt.Errorf("removing image %s: %w", image, err)
	}
	return nil
}

// ImageID returns the ID of a local image. A nil docker means DockerExec.
func ImageID(ctx context.Context, docker ExecFunc, image string) (string, error) {
	if docker == nil {
		docker = DockerExec
	}
	out, err := docker(ctx, nil, "image", "inspect", "--format", "{{.Id}}", "--", image)
	if err != nil {
		return "", fmt.Errorf("inspecting image %s: %w", image, err)
	}
	return strings.TrimSpace(string(out)), nil
}
