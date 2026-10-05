package cli

import (
	"errors"

	"github.com/rchouinard/pressfleet/control-plane/internal/config"
	"github.com/rchouinard/pressfleet/control-plane/internal/platform"
	"github.com/rchouinard/pressfleet/control-plane/internal/redact"
)

// platformRender is one rendering of the platform project, shared by init
// and upgrade so both produce the same project from the same configuration.
type platformRender struct {
	// cfg is the configuration, with sealed secrets.
	cfg config.Config
	// projectName is the Compose project name.
	projectName string
	// images are the service images.
	images platform.Images
	// skipPreflight starts the control plane even if its host checks fail.
	skipPreflight bool
	// configFile is the host path of the config file mounted into the
	// control plane; empty mounts none.
	configFile string
	// secretsDir is the rendered platform project whose credentials are
	// reused: the MariaDB root password, the object cache admin password, the
	// SFTP host key, and the public metrics credential. Missing ones are
	// generated.
	secretsDir string
}

// renderPlatform renders the platform project into dir. It returns the
// password of a public metrics credential it had to create, which the
// caller shows once; it is empty otherwise.
func renderPlatform(dir string, r platformRender) (redact.Secret, error) {
	password, err := mariaDBRootPassword(r.secretsDir)
	if err != nil {
		return "", err
	}
	redisPassword, err := redisAdminPassword(r.secretsDir)
	if err != nil {
		return "", err
	}
	hostKey, err := sshHostKey(r.secretsDir)
	if err != nil {
		return "", err
	}
	metricsPassword, metricsHash, err := metricsAccessHash(r.secretsDir, r.cfg.Metrics.PublicDomain)
	if err != nil {
		return "", err
	}
	cfg := r.cfg
	acme := cfg.ACME
	err = platform.Render(dir, platform.Config{
		ProjectName:          r.projectName,
		DataDir:              cfg.DataDir,
		Images:               r.images,
		MariaDBRootPassword:  password,
		RedisAdminPassword:   redisPassword,
		SSHHostKey:           hostKey,
		SFTPPort:             cfg.SFTP.Port,
		MasterKeyFile:        cfg.MasterKeyFile,
		ConfigFile:           r.configFile,
		SkipPreflight:        r.skipPreflight,
		PreviewBaseDomain:    cfg.PreviewBaseDomain,
		PublicIP:             cfg.PublicIP,
		SMTP:                 cfg.SMTP,
		Backup:               cfg.Backup,
		DashboardDomain:      cfg.DashboardDomain,
		MetricsRetentionDays: cfg.Metrics.RetentionDays,
		MetricsPublicDomain:  cfg.Metrics.PublicDomain,
		MetricsAccessHash:    metricsHash,
		ACME: platform.ACMEConfig{
			Email:          acme.Email,
			CAServer:       acme.Directory(),
			CACertificates: acme.CACertificates,
			DNSProvider:    acme.DNSProvider,
			DNSCredentials: acme.DNSCredentials,
			DNSResolvers:   acme.DNSResolvers,
		},
	})
	return metricsPassword, err
}

// platformImages returns the images released with this CLI, with the
// control plane image replaced by override if it is set (development only).
func (a *app) platformImages(override string) (platform.Images, error) {
	images, err := platform.DefaultImages(a.opts.Version)
	if err != nil {
		return platform.Images{}, err
	}
	if override != "" {
		images.ControlPlane = override
	}
	return images, nil
}

// metricsAccessHash returns the credential hash of the public metrics route
// (metrics.public_domain): the one already rendered into dir, so
// re-rendering keeps Grafana working, or a new one together with its
// password. Both are empty without a public domain.
func metricsAccessHash(dir, publicDomain string) (password redact.Secret, hash string, err error) {
	if publicDomain == "" {
		return "", "", nil
	}
	hash, err = platform.ReadMetricsAccessHash(dir)
	if err == nil {
		return "", hash, nil
	}
	if !errors.Is(err, platform.ErrNoMetricsAccess) {
		return "", "", err
	}
	return platform.NewMetricsAccess()
}
