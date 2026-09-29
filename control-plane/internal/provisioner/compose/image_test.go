package compose_test

import (
	"context"
	"errors"
	"io"
	"slices"
	"strings"
	"testing"

	"github.com/rchouinard/pressfleet/control-plane/internal/provisioner/compose"
)

func TestEnsureImage(t *testing.T) {
	noSuchImage := &compose.CommandError{Stderr: "Error response from daemon: No such image: x", Err: errors.New("exit status 1")}
	tests := []struct {
		name       string
		inspect    error
		pull       error
		wantPulled bool
		wantErr    bool
		wantCalls  []string
	}{
		{"present", nil, nil, false, false, []string{"image inspect"}},
		{"missing", noSuchImage, nil, true, false, []string{"image inspect", "image pull"}},
		{"pull fails", noSuchImage, errors.New("denied"), false, true, []string{"image inspect", "image pull"}},
		{"daemon down", errors.New("cannot connect"), nil, false, true, []string{"image inspect"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var calls []string
			docker := func(_ context.Context, _ io.Reader, args ...string) ([]byte, error) {
				calls = append(calls, strings.Join(args[:2], " "))
				if args[len(args)-1] != "registry.example/site-php:php8.4-wp7.1.2" {
					t.Errorf("image argument missing: %q", args)
				}
				if args[1] == "inspect" {
					return nil, tt.inspect
				}
				return nil, tt.pull
			}
			pulled, err := compose.EnsureImage(context.Background(), docker, "registry.example/site-php:php8.4-wp7.1.2")
			if pulled != tt.wantPulled || (err != nil) != tt.wantErr {
				t.Errorf("EnsureImage = %v, %v", pulled, err)
			}
			if !slices.Equal(calls, tt.wantCalls) {
				t.Errorf("calls = %q, want %q", calls, tt.wantCalls)
			}
		})
	}
}

func TestBuildAndRemoveImage(t *testing.T) {
	var calls []string
	var stdin string
	noSuchImage := &compose.CommandError{Stderr: "Error response from daemon: No such image: x", Err: errors.New("exit status 1")}
	docker := func(_ context.Context, in io.Reader, args ...string) ([]byte, error) {
		calls = append(calls, strings.Join(args, " "))
		if in != nil {
			b, _ := io.ReadAll(in)
			stdin = string(b)
		}
		if args[0] == "image" && args[1] == "rm" {
			return nil, noSuchImage
		}
		return nil, nil
	}
	if err := compose.BuildImage(context.Background(), docker, "e2e.local/x:1", "FROM scratch\n"); err != nil {
		t.Fatal(err)
	}
	if err := compose.TagImage(context.Background(), docker, "e2e.local/x:1", "e2e.local/y:1"); err != nil {
		t.Fatal(err)
	}
	if err := compose.RemoveImage(context.Background(), docker, "e2e.local/x:1"); err != nil {
		t.Errorf("removing a missing image: %v", err)
	}
	want := []string{"build --quiet --tag e2e.local/x:1 -", "image tag e2e.local/x:1 e2e.local/y:1", "image rm -- e2e.local/x:1"}
	if !slices.Equal(calls, want) || stdin != "FROM scratch\n" {
		t.Errorf("calls = %q (stdin %q), want %q", calls, stdin, want)
	}
}
