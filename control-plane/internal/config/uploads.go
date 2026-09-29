package config

import (
	"errors"
	"fmt"
	"time"
)

// UploadsConfig configures the resumable upload API (#111), through which
// site imports and code deployments send large files (§5.7, §5.8).
type UploadsConfig struct {
	// MaxSizeMB is the largest file an upload may declare, in MiB.
	MaxSizeMB int `yaml:"max_size_mb"`
	// ExpiryHours is how long an upload that no job has taken may go
	// without a new chunk before it is removed as abandoned.
	ExpiryHours int `yaml:"expiry_hours"`
}

// Upload limits.
const (
	// DefaultUploadMaxSizeMB is 10 GiB, enough for the wp-content archive
	// of a large site.
	DefaultUploadMaxSizeMB = 10 * 1024
	// MaxUploadMaxSizeMB bounds uploads.max_size_mb at 1 TiB.
	MaxUploadMaxSizeMB = 1024 * 1024
	// DefaultUploadExpiryHours gives a client a day to resume an
	// interrupted upload, or to start the job that uses it.
	DefaultUploadExpiryHours = 24
	// MaxUploadExpiryHours bounds uploads.expiry_hours at 30 days.
	MaxUploadExpiryHours = 30 * 24
)

// DefaultUploads returns the default upload settings.
func DefaultUploads() UploadsConfig {
	return UploadsConfig{MaxSizeMB: DefaultUploadMaxSizeMB, ExpiryHours: DefaultUploadExpiryHours}
}

// MaxSizeBytes returns MaxSizeMB in bytes.
func (u UploadsConfig) MaxSizeBytes() int64 { return int64(u.MaxSizeMB) << 20 }

// Expiry returns ExpiryHours as a duration.
func (u UploadsConfig) Expiry() time.Duration { return time.Duration(u.ExpiryHours) * time.Hour }

// Validate reports settings out of range.
func (u UploadsConfig) Validate() error {
	var errs []error
	if u.MaxSizeMB < 1 || u.MaxSizeMB > MaxUploadMaxSizeMB {
		errs = append(errs, fmt.Errorf("uploads.max_size_mb must be between 1 and %d, got %d", MaxUploadMaxSizeMB, u.MaxSizeMB))
	}
	if u.ExpiryHours < 1 || u.ExpiryHours > MaxUploadExpiryHours {
		errs = append(errs, fmt.Errorf("uploads.expiry_hours must be between 1 and %d, got %d", MaxUploadExpiryHours, u.ExpiryHours))
	}
	return errors.Join(errs...)
}
