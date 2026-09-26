package sitedbtest_test

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"github.com/rchouinard/pressfleet/control-plane/internal/sitedb"
	"github.com/rchouinard/pressfleet/control-plane/internal/sitedb/sitedbtest"
)

func TestFakeDumpImportRoundTrip(t *testing.T) {
	ctx := context.Background()
	f := sitedbtest.New()
	dump := filepath.Join(t.TempDir(), "dump.sql")

	ref, err := f.CreateDatabase(ctx, "a")
	if err != nil || ref != sitedbtest.SecretRef("a") {
		t.Fatalf("CreateDatabase = %q, %v", ref, err)
	}
	f.SetData("a", []byte("original"))
	// Create is idempotent and keeps existing data.
	if _, err := f.CreateDatabase(ctx, "a"); err != nil {
		t.Fatal(err)
	}
	if d, _ := f.Data("a"); string(d) != "original" {
		t.Fatalf("second CreateDatabase clobbered data: %q", d)
	}

	if err := f.Dump(ctx, "a", dump); err != nil {
		t.Fatalf("Dump: %v", err)
	}
	f.SetData("a", []byte("modified"))
	if err := f.Import(ctx, "a", dump); err != nil {
		t.Fatalf("Import: %v", err)
	}
	if d, _ := f.Data("a"); string(d) != "original" {
		t.Errorf("data after import = %q, want original", d)
	}

	if err := f.DropDatabase(ctx, "a"); err != nil {
		t.Fatal(err)
	}
	if err := f.DropDatabase(ctx, "a"); err != nil {
		t.Fatalf("second DropDatabase: %v", err)
	}
	if err := f.Dump(ctx, "a", dump); !errors.Is(err, sitedb.ErrDatabaseNotFound) {
		t.Errorf("Dump after drop err = %v, want ErrDatabaseNotFound", err)
	}
	if err := f.Import(ctx, "a", dump); !errors.Is(err, sitedb.ErrDatabaseNotFound) {
		t.Errorf("Import after drop err = %v, want ErrDatabaseNotFound", err)
	}
}

func TestFakeFaultInjection(t *testing.T) {
	injected := errors.New("injected")
	f := sitedbtest.New()
	f.Err = func(op, _ string) error {
		if op == "CreateDatabase" {
			return injected
		}
		return nil
	}
	if _, err := f.CreateDatabase(context.Background(), "a"); !errors.Is(err, injected) {
		t.Fatalf("err = %v, want injected", err)
	}
	if _, ok := f.Data("a"); ok {
		t.Error("failed create left a database behind")
	}
}
