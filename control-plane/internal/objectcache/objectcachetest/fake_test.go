package objectcachetest_test

import (
	"context"
	"errors"
	"slices"
	"testing"

	"github.com/rchouinard/pressfleet/control-plane/internal/objectcache/objectcachetest"
)

func TestFake(t *testing.T) {
	ctx := context.Background()
	f := objectcachetest.New()
	for range 2 {
		ref, err := f.CreateUser(ctx, "a")
		if err != nil || ref != objectcachetest.SecretRef("a") {
			t.Fatalf("CreateUser = %q, %v", ref, err)
		}
	}
	if !f.HasUser("a") || !slices.Equal(f.Users(), []string{"a"}) {
		t.Errorf("users = %v", f.Users())
	}
	for range 2 {
		if err := f.DeleteUser(ctx, "a"); err != nil {
			t.Fatal(err)
		}
	}
	if f.HasUser("a") {
		t.Error("user left after DeleteUser")
	}

	injected := errors.New("down")
	f.Err = func(op, _ string) error {
		if op == "CreateUser" {
			return injected
		}
		return nil
	}
	if _, err := f.CreateUser(ctx, "b"); !errors.Is(err, injected) || f.HasUser("b") {
		t.Errorf("injected failure: err = %v, has user %v", err, f.HasUser("b"))
	}
	if n := len(f.Calls()); n != 5 {
		t.Errorf("calls = %d, want 5", n)
	}
}
