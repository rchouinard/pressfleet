package restic

import (
	"cmp"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/rchouinard/pressfleet/control-plane/internal/backup"
	"github.com/rchouinard/pressfleet/control-plane/internal/backup/repository"
	"github.com/rchouinard/pressfleet/control-plane/internal/provisioner"
)

// restic exit codes (restic 0.17 and later) that backupResult tells apart.
const (
	exitIncomplete    = 3 // the snapshot was made, but some files couldn't be read
	exitLocked        = 11
	exitWrongPassword = 12
)

const (
	// maxWarnings bounds the warnings kept from one backup; the rest are
	// counted.
	maxWarnings = 10
	// maxMessage bounds restic's own words in an error.
	maxMessage = 1024
)

// message is one line of `restic backup --json` output.
type message struct {
	MessageType string `json:"message_type"`
	// summary
	SnapshotID          string    `json:"snapshot_id"`
	BackupStart         time.Time `json:"backup_start"`
	TotalFilesProcessed int64     `json:"total_files_processed"`
	TotalBytesProcessed int64     `json:"total_bytes_processed"`
	DataAddedPacked     int64     `json:"data_added_packed"`
	// error
	Error *struct {
		Message string `json:"message"`
	} `json:"error"`
	Item string `json:"item"`
	// exit_error
	Code    int    `json:"code"`
	Message string `json:"message"`
}

// output is what a backup task printed.
type output struct {
	summary  *message
	errors   []string // per-file errors
	dropped  int      // errors beyond maxWarnings
	exitMsg  string   // the exit_error message
	unparsed []string // lines that weren't JSON, such as a fatal error
}

// parseOutput reads the JSON lines restic printed on standard output and
// standard error.
func parseOutput(streams ...string) output {
	var o output
	for _, s := range streams {
		for line := range strings.Lines(s) {
			line = strings.TrimSpace(line)
			if line == "" {
				continue
			}
			var m message
			if !strings.HasPrefix(line, "{") || json.Unmarshal([]byte(line), &m) != nil {
				o.unparsed = append(o.unparsed, line)
				continue
			}
			switch m.MessageType {
			case "summary":
				o.summary = &m
			case "error":
				if m.Error == nil {
					continue
				}
				if len(o.errors) == maxWarnings {
					o.dropped++
					continue
				}
				o.errors = append(o.errors, m.Error.Message)
			case "exit_error":
				o.exitMsg = m.Message
			}
		}
	}
	return o
}

// warnings are the per-file errors of a backup that went ahead.
func (o output) warnings() []string {
	w := o.errors
	if o.dropped > 0 {
		w = append(w, fmt.Sprintf("and %d more", o.dropped))
	}
	return w
}

// detail explains a failed backup in restic's words.
func (o output) detail() string {
	parts := []string{}
	if o.exitMsg != "" {
		parts = append(parts, o.exitMsg)
	}
	parts = append(parts, o.unparsed...)
	parts = append(parts, o.warnings()...)
	msg := strings.Join(parts, "; ")
	if len(msg) > maxMessage {
		msg = msg[:maxMessage] + "..."
	}
	return msg
}

// backupResult turns a backup task's outcome into a snapshot. A backup that
// couldn't read some files (exit status 3) still made a snapshot, without
// those files: it succeeds with warnings, so one unreadable upload doesn't
// stop a site from being backed up at all.
func backupResult(res *provisioner.TaskResult, runErr error) (backup.Snapshot, error) {
	var (
		ee   *provisioner.ExecError
		code int
		o    output
	)
	switch {
	case runErr == nil:
		o = parseOutput(res.Stdout, res.Stderr)
	case errors.As(runErr, &ee):
		code = ee.ExitCode
		o = parseOutput(ee.Stdout, ee.Stderr)
	default:
		return backup.Snapshot{}, fmt.Errorf("running the backup task: %w", runErr)
	}
	if code != 0 && (code != exitIncomplete || o.summary == nil) {
		err := fmt.Errorf("%w (exit status %d): %s", ErrBackupFailed, code, o.detail())
		switch code {
		case exitLocked:
			err = fmt.Errorf("%w: %w", repository.ErrLocked, err)
		case exitWrongPassword:
			err = fmt.Errorf("%w: %w", repository.ErrWrongPassword, err)
		}
		return backup.Snapshot{}, err
	}
	if o.summary == nil || o.summary.SnapshotID == "" {
		return backup.Snapshot{}, fmt.Errorf("%w: restic printed no snapshot ID: %s", ErrBackupFailed, o.detail())
	}
	s := o.summary
	snap := backup.Snapshot{
		ID:         backup.SnapshotID(s.SnapshotID),
		Time:       s.BackupStart.UTC(),
		FileCount:  s.TotalFilesProcessed,
		SizeBytes:  s.TotalBytesProcessed,
		AddedBytes: s.DataAddedPacked,
	}
	if code == exitIncomplete {
		snap.Warnings = o.warnings()
		if len(snap.Warnings) == 0 {
			snap.Warnings = []string{cmp.Or(o.exitMsg, "some files could not be read")}
		}
	}
	return snap, nil
}
