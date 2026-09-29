package compose_test

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/rchouinard/pressfleet/control-plane/internal/id"
	"github.com/rchouinard/pressfleet/control-plane/internal/provisioner/compose"
)

// A pinned build survives its tag moving to a new build, whichever image
// store the daemon uses, so a site can be moved back to it.
func TestPinImageDocker(t *testing.T) {
	if os.Getenv("PRESSFLEET_DOCKER_TESTS") != "1" {
		t.Skip("set PRESSFLEET_DOCKER_TESTS=1 to run tests against Docker")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	repo := "pfpin.local/" + strings.ToLower(id.New())
	tag := repo + ":php8.4-wp7.1.2"
	var created []string
	t.Cleanup(func() {
		for _, image := range append(created, tag) {
			_ = compose.RemoveImage(context.Background(), nil, image)
		}
	})
	p := newTestProvisioner(t, nil)
	build := func(n string) string {
		t.Helper()
		if err := compose.BuildImage(ctx, nil, tag, "FROM alpine:3\nRUN echo "+n+" > /build\n"); err != nil {
			t.Fatal(err)
		}
		pinned, err := p.PinImage(ctx, tag)
		if err != nil {
			t.Fatal(err)
		}
		imageID, err := compose.ImageID(ctx, nil, tag)
		if err != nil {
			t.Fatal(err)
		}
		created = append(created, compose.PinTag(repo, imageID))
		if !strings.HasPrefix(pinned, repo+"@sha256:") && pinned != imageID {
			t.Errorf("PinImage = %q, want %s@sha256:... or %s", pinned, repo, imageID)
		}
		return pinned
	}
	first := build("1")
	firstID, err := compose.ImageID(ctx, nil, first)
	if err != nil {
		t.Fatal(err)
	}
	second := build("2")
	if second == first {
		t.Fatalf("a new build has the same pin %s", first)
	}
	// The first build is still there under its pin.
	if id, err := compose.ImageID(ctx, nil, first); err != nil || id != firstID {
		t.Errorf("first build after the tag moved: %q, %v; want %s", id, err, firstID)
	}
	// Pinning again is idempotent.
	if again, err := p.PinImage(ctx, tag); err != nil || again != second {
		t.Errorf("PinImage again = %q, %v; want %s", again, err, second)
	}
	// Nothing to pull from a registry that doesn't exist.
	if err := p.PullImage(ctx, tag); err == nil {
		t.Error("PullImage from a registry that doesn't exist succeeded")
	}
}
