package repository

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"os/exec"
	"slices"
	"strings"

	"github.com/rchouinard/pressfleet/control-plane/internal/redact"
)

// errRepositoryMissing means restic found no repository at the location.
var errRepositoryMissing = errors.New("repository does not exist")

// restic exit codes (restic 0.17 and later) that callers branch on.
const (
	exitRepositoryMissing = 10
	exitLocked            = 11
	exitWrongPassword     = 12
)

// maxStderr bounds how much of restic's error output goes into an error.
const maxStderr = 2048

// inheritedEnv are the only variables restic inherits from the control
// plane's environment, which may hold the master key.
var inheritedEnv = []string{"PATH", "HOME", "TMPDIR", "SSL_CERT_FILE", "SSL_CERT_DIR"}

// run runs restic against rec with its password and returns its standard
// output.
func (m *Manager) run(ctx context.Context, rec Repository, password redact.Secret, args ...string) ([]byte, error) {
	var stdout bytes.Buffer
	if err := m.runTo(ctx, rec, password, &stdout, args...); err != nil {
		return nil, err
	}
	return stdout.Bytes(), nil
}

// runTo runs restic against rec with its password, writing its standard
// output to stdout.
func (m *Manager) runTo(ctx context.Context, rec Repository, password redact.Secret, stdout io.Writer, args ...string) error {
	env := map[string]string{"RESTIC_REPOSITORY": rec.Location}
	secretEnv := map[string]redact.Secret{"RESTIC_PASSWORD": password}
	if rec.Backend == BackendS3 {
		if m.s3.AccessKeyID == "" {
			return ErrS3NotConfigured
		}
		maps.Copy(env, s3Env(m.s3))
		secretEnv["AWS_ACCESS_KEY_ID"] = m.s3.AccessKeyID
		secretEnv["AWS_SECRET_ACCESS_KEY"] = m.s3.SecretAccessKey
	}
	return m.exec(ctx, env, secretEnv, stdout, args...)
}

// exec runs restic with the given environment and nothing else from the
// control plane's but inheritedEnv. Secrets reach the child only through
// its environment. Group-level commands run without a local cache: they
// are rare, and the cache would need a home of its own.
func (m *Manager) exec(ctx context.Context, env map[string]string, secretEnv map[string]redact.Secret, stdout io.Writer, args ...string) error {
	cmd := exec.CommandContext(ctx, m.restic, slices.Concat([]string{"--no-cache"}, args)...) //nolint:gosec // G204: the operator-configured restic binary with our own arguments
	for _, k := range inheritedEnv {
		if v, ok := os.LookupEnv(k); ok {
			cmd.Env = append(cmd.Env, k+"="+v)
		}
	}
	for k, v := range env {
		cmd.Env = append(cmd.Env, k+"="+v)
	}
	for k, v := range secretEnv {
		cmd.Env = append(cmd.Env, k+"="+v.Reveal())
	}
	var stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = stdout, &stderr
	err := cmd.Run()
	if err == nil {
		return nil
	}
	sub := "restic"
	if len(args) > 0 {
		sub += " " + args[0]
	}
	msg := strings.TrimSpace(stderr.String())
	if len(msg) > maxStderr {
		// Both ends: restic says what it was doing first and ends with
		// its fatal error.
		msg = msg[:maxStderr/2] + "\n...\n" + msg[len(msg)-maxStderr/2:]
	}
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) {
		return fmt.Errorf("running %s: %w", sub, err)
	}
	var sentinel error
	switch exitErr.ExitCode() {
	case exitRepositoryMissing:
		sentinel = errRepositoryMissing
	case exitLocked:
		sentinel = ErrLocked
	case exitWrongPassword:
		sentinel = ErrWrongPassword
	}
	if sentinel != nil {
		return fmt.Errorf("%s: %w: %s", sub, sentinel, msg)
	}
	return fmt.Errorf("%s: %w: %s", sub, err, msg)
}
