// Package upgradestate is the contract between the host CLI, which writes
// the upgrade record (<data_dir>/upgrade/upgrade.json), and the control
// plane, which reads it to hold the fleet-wide rollouts while an upgrade is
// unconfirmed (design doc §4.7, #247).
//
// The contract is versioned like the admin interface: [ContractVersion]
// changes only when a change is incompatible, and a status a reader doesn't
// know is treated as unconfirmed, so a control plane never starts rollouts
// on a status it can't interpret. The record's other fields belong to the
// CLI; this package reads only the ones the control plane needs.
//
// The record's statuses:
//
//	in-progress      the CLI is applying the upgrade, or was killed doing so
//	failed           the apply failed; the platform awaits a retry or a rollback
//	rolling-back     a rollback is running
//	rollback-failed  a rollback stopped; the platform's state is uncertain
//	succeeded        the CLI saw the platform healthy at the new version
//	rolled-back      the platform is back on the previous release
//
// The first four hold the rollouts, as does any status not listed; the last
// two release them. Underscores are accepted for hyphens (rolling_back).
package upgradestate

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// ContractVersion is the version of this contract.
const ContractVersion = 1

// RecordFile is the name of the upgrade record in the upgrade directory.
const RecordFile = "upgrade.json"

// Statuses of an upgrade in its record.
const (
	InProgress     = "in-progress"
	Failed         = "failed"
	RollingBack    = "rolling-back"
	RollbackFailed = "rollback-failed"
	Succeeded      = "succeeded"
	RolledBack     = "rolled-back"
)

// ErrUnreadable means the record exists but can't be read or understood.
var ErrUnreadable = errors.New("unreadable upgrade record")

// Record is the part of the upgrade record the control plane reads.
type Record struct {
	From   string `json:"from"`
	To     string `json:"to"`
	Status string `json:"status"`
}

// Hold says whether rollouts are held, and why.
type Hold struct {
	Held bool
	// Reason is a sentence for operators, set when Held.
	Reason string
	// Status is the record's status; empty if there is no record or it is
	// unreadable.
	Status string
	// Err is set when the record is unreadable, which holds the rollouts.
	Err error
}

// ReadRecord reads the upgrade record in dir (config.Layout.UpgradeDir). A
// missing record returns fs.ErrNotExist: installs that never upgraded have
// none.
func ReadRecord(dir string) (Record, error) {
	path := filepath.Join(dir, RecordFile)
	data, err := os.ReadFile(path) //nolint:gosec // G304: dir is the configured upgrade directory
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return Record{}, err
		}
		return Record{}, fmt.Errorf("%w: %w", ErrUnreadable, err)
	}
	var rec Record
	if err := json.Unmarshal(data, &rec); err != nil {
		return Record{}, fmt.Errorf("%w: %s: %w", ErrUnreadable, path, err)
	}
	return rec, nil
}

// Check reads the record in dir and decides whether rollouts are held.
// No record means no hold; a record that can't be read holds them, because
// the control plane can't tell that the upgrade is confirmed.
func Check(dir string) Hold {
	rec, err := ReadRecord(dir)
	if errors.Is(err, fs.ErrNotExist) {
		return Hold{}
	}
	if err != nil {
		return Hold{
			Held: true, Err: err,
			Reason: "the upgrade record can't be read, so the upgrade can't be confirmed: " + err.Error(),
		}
	}
	return Decide(rec)
}

// Decide decides whether rollouts are held for the record.
func Decide(rec Record) Hold {
	status := strings.ReplaceAll(strings.ToLower(strings.TrimSpace(rec.Status)), "_", "-")
	to := rec.To
	if to == "" {
		to = "a new release"
	}
	var reason string
	switch status {
	case Succeeded, RolledBack:
		return Hold{Status: status}
	case InProgress:
		reason = fmt.Sprintf("the upgrade to %s is in progress", to)
	case Failed:
		reason = fmt.Sprintf("the upgrade to %s failed and hasn't been retried or rolled back", to)
	case RollingBack:
		reason = fmt.Sprintf("the upgrade to %s is being rolled back", to)
	case RollbackFailed:
		reason = fmt.Sprintf("the rollback of the upgrade to %s failed", to)
	default:
		reason = fmt.Sprintf("the upgrade to %s has the unknown status %q", to, rec.Status)
	}
	return Hold{Held: true, Status: status, Reason: reason}
}
