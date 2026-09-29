package compose

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
)

// PinImage implements provisioner.ImagePinner. It makes the image available
// (EnsureImage) and returns repository@<digest> for it, from the image's
// repository digests, or else its image ID: a locally built image has no
// repository digest under the classic image store. It also tags the build
// with PinTag, so the build stays when the tag moves to a newer one (the
// containerd image store deletes an image no name refers to), and a site
// can be moved back to it.
func (p *Provisioner) PinImage(ctx context.Context, image string) (string, error) {
	repo, err := imageRepository(image)
	if err != nil {
		return "", err
	}
	if _, err := EnsureImage(ctx, p.docker, image); err != nil {
		return "", err
	}
	out, err := p.docker(ctx, nil, "image", "inspect", "--format", "{{.Id}} {{json .RepoDigests}}", "--", image)
	if err != nil {
		return "", fmt.Errorf("inspecting image %s: %w", image, err)
	}
	id, digestsJSON, _ := strings.Cut(strings.TrimSpace(string(out)), " ")
	var digests []string
	if err := json.Unmarshal([]byte(digestsJSON), &digests); err != nil || !imageID.MatchString(id) {
		return "", fmt.Errorf("inspecting image %s: unexpected output %q", image, out)
	}
	pinned := id
	for _, d := range digests {
		if strings.HasPrefix(d, repo+"@sha256:") {
			pinned = d
			break
		}
	}
	if _, err := p.docker(ctx, nil, "image", "tag", id, PinTag(repo, id)); err != nil {
		return "", fmt.Errorf("tagging image %s: %w", image, err)
	}
	return pinned, nil
}

// PullImage implements provisioner.ImagePinner with docker image pull.
func (p *Provisioner) PullImage(ctx context.Context, image string) error {
	if _, err := imageRepository(image); err != nil {
		return err
	}
	if _, err := p.docker(ctx, nil, "image", "pull", "--quiet", "--", image); err != nil {
		return fmt.Errorf("pulling image %s: %w", image, err)
	}
	return nil
}

// PinTag is the tag PinImage keeps a build under: pin-<the first 16 hex
// digits of its image ID>, in the image's repository.
func PinTag(repository, imageID string) string {
	return repository + ":pin-" + strings.TrimPrefix(imageID, "sha256:")[:16]
}

var imageID = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)

// imageRepository returns the repository of an image reference with a tag:
// registry.example:5000/site-php for
// registry.example:5000/site-php:php8.4-wp7.1.2.
func imageRepository(image string) (string, error) {
	i := strings.LastIndexByte(image, ':')
	if validImage(image) != nil || strings.Contains(image, "@") || i <= strings.LastIndexByte(image, '/') {
		return "", fmt.Errorf("%q is not an image reference with a tag", image)
	}
	return image[:i], nil
}
