package siteimport_test

import (
	"bytes"
	"errors"
	"io"
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/rchouinard/pressfleet/control-plane/internal/siteimport"
)

func TestArchiveLimits(t *testing.T) {
	content := wpContent("")
	big := append(slices.Clone(content), file{name: "uploads/big.bin", body: strings.Repeat("x", 4096)})
	tests := []struct {
		name     string
		entries  int
		unpacked int64
		open     func(*testing.T) *os.File
		want     string
	}{
		{"tar.gz bytes", 1000, 1024, func(t *testing.T) *os.File { return tarFile(t, true, big...) }, "unpacks to more than"},
		{"zip bytes", 1000, 1024, func(t *testing.T) *os.File { return zipFile(t, big...) }, "unpacks to more than"},
		{"tar.gz entries", 2, 1 << 20, func(t *testing.T) *os.File { return tarFile(t, true, content...) }, "more than 2 entries"},
		{"zip entries", 2, 1 << 20, func(t *testing.T) *os.File { return zipFile(t, content...) }, "more than 2 entries"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := tt.open(t)
			defer siteimport.SetLimits(tt.entries, tt.unpacked)()
			_, err := siteimport.InspectArchive(f)
			if !errors.Is(err, siteimport.ErrInvalidArchive) || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("InspectArchive error = %v, want invalid archive containing %q", err, tt.want)
			}
		})
	}
}

func TestDumpLimits(t *testing.T) {
	dump := mysqldump + strings.Repeat("-- padding padding padding padding\n", 100)
	for name, data := range map[string][]byte{"sql": []byte(dump), "sql.gz": gzipped(t, dump)} {
		t.Run(name, func(t *testing.T) {
			defer siteimport.SetLimits(1000, 1024)()
			if _, err := siteimport.InspectDump(bytes.NewReader(data)); !errors.Is(err, siteimport.ErrInvalidDump) || !strings.Contains(err.Error(), "unpacked") {
				t.Fatalf("InspectDump error = %v, want invalid dump, too large", err)
			}
			if _, err := siteimport.SanitizeDump(io.Discard, bytes.NewReader(data)); !errors.Is(err, siteimport.ErrInvalidDump) {
				t.Fatalf("SanitizeDump error = %v, want ErrInvalidDump", err)
			}
		})
	}
}
