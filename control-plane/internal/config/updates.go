package config

import (
	"errors"
	"fmt"
	"time"
)

// UpdatesConfig is how the platform rolls out new site images (design doc
// §5.5): WordPress core releases (#57), and the platform image updates
// that build on the same rollout (#58). A rollout updates one site at a
// time and halts after MaxFailures failed site updates in a row.
type UpdatesConfig struct {
	// WindowStart is the UTC time of day, "HH:MM", the maintenance window
	// opens. Rollouts only start site updates inside it. Empty means no
	// window: they start whenever there is something to roll out.
	WindowStart string `yaml:"window_start"`
	// WindowHours is the window's length in hours, 1 to 24.
	WindowHours int `yaml:"window_hours"`
	// MaxFailures is how many site updates in a row may fail before a
	// rollout halts until an operator resumes it, 1 to 100.
	MaxFailures int `yaml:"max_failures"`
	// Core configures WordPress core rollouts.
	Core CoreUpdatesConfig `yaml:"core"`
}

// CoreUpdatesConfig is the install-wide part of the WordPress core rollout
// policy (decision #56). Minor and security releases always roll out; major
// releases wait for approval per site or client group.
type CoreUpdatesConfig struct {
	// MajorAutoApproveDays approves a major release automatically this
	// many days after its images were published, unless a site or client
	// group is held or decided otherwise; 0 never does.
	MajorAutoApproveDays int `yaml:"major_auto_approve_days"`
	// HoldWarningDays is how long a site may be held on an older major
	// branch before the platform warns about it, 1 to 3650.
	HoldWarningDays int `yaml:"hold_warning_days"`
}

// DefaultUpdates has no maintenance window (a 4-hour one once
// window_start is set), halts a rollout after 3 failures in a row, never
// approves major releases automatically, and warns about holds after 90
// days.
func DefaultUpdates() UpdatesConfig {
	return UpdatesConfig{WindowHours: 4, MaxFailures: 3, Core: CoreUpdatesConfig{HoldWarningDays: 90}}
}

// HasWindow reports whether a maintenance window is set.
func (u UpdatesConfig) HasWindow() bool { return u.WindowStart != "" }

// Start returns WindowStart as the time since midnight UTC. It is only
// meaningful once Validate has accepted the config.
func (u UpdatesConfig) Start() time.Duration {
	return BackupScheduleConfig{WindowStart: u.WindowStart}.Start()
}

// Window returns WindowHours as a duration.
func (u UpdatesConfig) Window() time.Duration {
	return time.Duration(u.WindowHours) * time.Hour
}

// Validate reports every problem with the update settings, or nil.
func (u UpdatesConfig) Validate() error {
	var errs []error
	if u.WindowStart != "" {
		if _, err := time.Parse("15:04", u.WindowStart); err != nil || len(u.WindowStart) != len("15:04") {
			errs = append(errs, fmt.Errorf("updates.window_start must be empty or a UTC time of day like 03:00, got %q", u.WindowStart))
		}
	}
	if u.WindowHours < 1 || u.WindowHours > 24 {
		errs = append(errs, fmt.Errorf("updates.window_hours must be from 1 to 24, got %d", u.WindowHours))
	}
	if u.MaxFailures < 1 || u.MaxFailures > 100 {
		errs = append(errs, fmt.Errorf("updates.max_failures must be from 1 to 100, got %d", u.MaxFailures))
	}
	if u.Core.MajorAutoApproveDays < 0 || u.Core.MajorAutoApproveDays > 3650 {
		errs = append(errs, fmt.Errorf("updates.core.major_auto_approve_days must be from 0 (off) to 3650, got %d", u.Core.MajorAutoApproveDays))
	}
	if u.Core.HoldWarningDays < 1 || u.Core.HoldWarningDays > 3650 {
		errs = append(errs, fmt.Errorf("updates.core.hold_warning_days must be from 1 to 3650, got %d", u.Core.HoldWarningDays))
	}
	return errors.Join(errs...)
}
