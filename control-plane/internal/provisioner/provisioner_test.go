package provisioner_test

import (
	"strings"
	"testing"

	"github.com/rchouinard/pressfleet/control-plane/internal/provisioner"
)

func validConfig() provisioner.SiteConfig {
	return provisioner.SiteConfig{
		ID:               "site1",
		ClientGroupID:    "group1",
		PreviewDomain:    "site1.preview.example.com",
		PHPVersion:       "8.4",
		WPCoreVersion:    "6.9.1",
		DBSecretRef:      "secret/db/site1",
		Resources:        provisioner.DefaultResourceLimits(),
		UID:              provisioner.MinSiteUID,
		WPSaltsSecretRef: "secret/salts/site1",
	}
}

func TestSiteConfigValidate(t *testing.T) {
	tests := []struct {
		name    string
		mutate  func(*provisioner.SiteConfig)
		wantErr []string
	}{
		{name: "valid", mutate: func(*provisioner.SiteConfig) {}},
		{
			name:    "missing required fields",
			mutate:  func(c *provisioner.SiteConfig) { c.ClientGroupID, c.PHPVersion = "", "" },
			wantErr: []string{"ClientGroupID is required", "PHPVersion is required"},
		},
		{
			name:    "missing salts",
			mutate:  func(c *provisioner.SiteConfig) { c.WPSaltsSecretRef = "" },
			wantErr: []string{"WPSaltsSecretRef is required"},
		},
		{
			name:    "UID unset",
			mutate:  func(c *provisioner.SiteConfig) { c.UID = 0 },
			wantErr: []string{"UID 0 is outside the site UID range"},
		},
		{
			name:    "UID below range",
			mutate:  func(c *provisioner.SiteConfig) { c.UID = provisioner.MinSiteUID - 1 },
			wantErr: []string{"outside the site UID range"},
		},
		{
			name:    "UID above range",
			mutate:  func(c *provisioner.SiteConfig) { c.UID = provisioner.MaxSiteUID + 1 },
			wantErr: []string{"outside the site UID range"},
		},
		{
			name:   "UID at top of range",
			mutate: func(c *provisioner.SiteConfig) { c.UID = provisioner.MaxSiteUID },
		},
		{
			name:    "limit below reservation",
			mutate:  func(c *provisioner.SiteConfig) { c.Resources.MemoryLimitMB = 64 },
			wantErr: []string{"MemoryLimitMB must be at least MemoryReservationMB"},
		},
		{
			name:    "zero resources",
			mutate:  func(c *provisioner.SiteConfig) { c.Resources = provisioner.ResourceLimits{} },
			wantErr: []string{"CPU is required", "MemoryReservationMB must be positive"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := validConfig()
			tt.mutate(&cfg)
			err := cfg.Validate()
			if len(tt.wantErr) == 0 {
				if err != nil {
					t.Fatalf("Validate() = %v, want nil", err)
				}
				return
			}
			if err == nil {
				t.Fatal("Validate() = nil, want error")
			}
			for _, want := range tt.wantErr {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("Validate() error %q does not mention %q", err, want)
				}
			}
		})
	}
}
