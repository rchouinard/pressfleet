package deploy_test

import (
	"context"
	"strings"
	"testing"

	"github.com/rchouinard/pressfleet/control-plane/internal/deploy"
	"github.com/rchouinard/pressfleet/control-plane/internal/provisioner"
	"github.com/rchouinard/pressfleet/control-plane/internal/store"
	"github.com/rchouinard/pressfleet/control-plane/internal/store/storetest"
)

// runAll runs the pool until no job is runnable and returns the job.
func (e *jobEnv) runAll(t *testing.T, jobID string) store.Job {
	t.Helper()
	ctx := context.Background()
	for range 100 {
		ran, err := e.pool.RunNext(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if !ran {
			break
		}
	}
	j, err := store.NewJobs(e.db).Get(ctx, jobID)
	if err != nil {
		t.Fatal(err)
	}
	return j
}

// failTask makes site task operation op fail with fail's result; other
// operations run as usual.
func (e *jobEnv) failTask(op string, fail func() (*provisioner.TaskResult, error)) {
	run := e.prov.RunTaskFunc
	e.prov.RunTaskFunc = func(siteID string, task provisioner.Task) (*provisioner.TaskResult, error) {
		if task.Command[len(deploy.TaskCommand)] == op {
			return fail()
		}
		return run(siteID, task)
	}
}

func TestDeployJobFailures(t *testing.T) {
	pkg := func(t *testing.T) []byte { return zipOf(t, member{name: "a.php", body: "<?php"}) }
	tests := []struct {
		name string
		// setup runs after the job is queued.
		setup      func(t *testing.T, e *jobEnv, uploadID string)
		wantState  store.JobState
		wantError  string
		permanent  bool // gives up on the first attempt
		wantPlugin bool // the package ends up deployed
	}{
		{
			name: "upload released while the job waited",
			setup: func(t *testing.T, e *jobEnv, uploadID string) {
				if _, err := e.db.ExecContext(context.Background(), "UPDATE uploads SET status = ?, job_id = NULL WHERE id = ?",
					string(store.UploadComplete), uploadID); err != nil {
					t.Fatal(err)
				}
			},
			wantState: store.JobFailed, wantError: "the package's upload", permanent: true,
		},
		{
			name: "site gone from the runtime",
			setup: func(_ *testing.T, e *jobEnv, _ string) {
				e.failTask("deploy-unpack", func() (*provisioner.TaskResult, error) { return nil, provisioner.ErrSiteNotFound })
			},
			wantState: store.JobFailed, wantError: provisioner.ErrSiteNotFound.Error(), permanent: true,
		},
		{
			name: "task output isn't JSON",
			setup: func(_ *testing.T, e *jobEnv, _ string) {
				e.failTask("deploy-unpack", func() (*provisioner.TaskResult, error) { return &provisioner.TaskResult{Stdout: "oops"}, nil })
			},
			wantState: store.JobFailed, wantError: "reading the site task's result",
		},
		{
			// Removing the previous version is the last step; once it
			// has run out of attempts the deployment stands, with a
			// warning.
			name: "cleanup keeps failing",
			setup: func(_ *testing.T, e *jobEnv, _ string) {
				e.failTask("deploy-cleanup", func() (*provisioner.TaskResult, error) {
					return nil, &provisioner.ExecError{ExitCode: 1, Stderr: "Error: disk full"}
				})
			},
			wantState: store.JobSucceeded, wantPlugin: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			storetest.Run(t, func(t *testing.T, db *store.DB) {
				e := newJobEnv(t, db)
				uploadID := e.upload(t, pkg(t))
				job, err := e.svc.Deploy(context.Background(), e.site.ID,
					deploy.Request{UploadID: uploadID, Kind: deploy.KindPlugin, Name: "a", UserID: testUser})
				if err != nil {
					t.Fatal(err)
				}
				tt.setup(t, e, uploadID)
				j := e.runAll(t, job.ID)
				if j.State != tt.wantState || !strings.Contains(j.Error, tt.wantError) {
					t.Errorf("job = %s %q, want %s containing %q", j.State, j.Error, tt.wantState, tt.wantError)
				}
				d, err := store.NewJobs(db).Detail(context.Background(), job.ID)
				if err != nil {
					t.Fatal(err)
				}
				retried := false
				for _, s := range d.Steps {
					if s.Attempts > 1 {
						retried = true
					}
				}
				if tt.permanent && retried {
					t.Errorf("a permanent failure was retried: %s", stepStates(d))
				}
				if _, ok := e.files(t, deploy.KindPlugin)["a/a.php"]; ok != tt.wantPlugin {
					t.Errorf("plugin deployed = %v, want %v", ok, tt.wantPlugin)
				}
				e.checkClean(t)
			})
		})
	}
}

func stepStates(d store.JobDetail) string {
	var b strings.Builder
	for _, s := range d.Steps {
		b.WriteString(s.Name + "=" + string(s.State) + " ")
	}
	return b.String()
}
