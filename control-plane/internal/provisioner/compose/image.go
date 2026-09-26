package compose

import (
	"context"
	"fmt"
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
