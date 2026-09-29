package compose

import (
	"context"
	"fmt"
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

// BuildImage builds an image tagged tag from a Dockerfile without a build
// context (`docker build -`), such as one that adds a layer to an existing
// image. Tests use it to derive site images. A nil docker means
// DockerExec.
func BuildImage(ctx context.Context, docker ExecFunc, tag, dockerfile string) error {
	if docker == nil {
		docker = DockerExec
	}
	if _, err := docker(ctx, strings.NewReader(dockerfile), "build", "--quiet", "--tag", tag, "-"); err != nil {
		return fmt.Errorf("building image %s: %w", tag, err)
	}
	return nil
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
