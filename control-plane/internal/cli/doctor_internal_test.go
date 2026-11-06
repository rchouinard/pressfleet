package cli

import (
	"context"
	"log/slog"
	"strings"
	"testing"

	"github.com/rchouinard/pressfleet/control-plane/internal/auth"
	"github.com/rchouinard/pressfleet/control-plane/internal/config"
	"github.com/rchouinard/pressfleet/control-plane/internal/doctor"
	"github.com/rchouinard/pressfleet/control-plane/internal/doctor/doctortest"
	"github.com/rchouinard/pressfleet/control-plane/internal/store"
	"github.com/rchouinard/pressfleet/control-plane/internal/store/storetest"
)

func findResult(t *testing.T, rep doctor.Report, id string) doctor.Result {
	t.Helper()
	for _, r := range rep.Results {
		if r.ID == id {
			return r
		}
	}
	t.Fatalf("no %s result in %+v", id, rep.Results)
	return doctor.Result{}
}

// TestPlatformDoctorWebAuthn checks that the API's doctor counts keys from
// a previous dashboard domain, and that the host's doctor, which never
// reads the state database, reports the check as unavailable.
func TestPlatformDoctorWebAuthn(t *testing.T) {
	storetest.Run(t, func(t *testing.T, db *store.DB) {
		ctx := context.Background()
		a := &app{
			cfg:  config.Config{DashboardDomain: "new.example.com", DataDir: "/data"},
			log:  slog.New(slog.DiscardHandler),
			opts: Options{HostProbe: doctortest.Healthy(), DockerProbe: &doctortest.Docker{}},
		}
		svc := auth.NewService(db, a.authOptions(nil))
		const id = "webauthn-relying-party"

		if r := findResult(t, a.platformDoctor(svc, nil)(ctx), id); r.Status != doctor.StatusPass {
			t.Errorf("nothing stale: %+v, want pass", r)
		}

		u, err := svc.CreateUser(ctx, auth.NewUser{Email: "a@example.com", Password: "correct horse battery staple", Role: store.RoleAdmin})
		if err != nil {
			t.Fatal(err)
		}
		for _, c := range []store.WebAuthnCredential{
			{UserID: u.ID, CredentialID: []byte("old"), PublicKey: []byte("key"), Algorithm: -7, RPID: "old.example.com", Name: "Old"},
			{UserID: u.ID, CredentialID: []byte("new"), PublicKey: []byte("key"), Algorithm: -7, RPID: "new.example.com", Name: "New"},
		} {
			if err := store.NewWebAuthnCredentials(db).Create(ctx, &c); err != nil {
				t.Fatal(err)
			}
		}
		r := findResult(t, a.platformDoctor(svc, nil)(ctx), id)
		if r.Status != doctor.StatusWarn || !strings.Contains(r.Message, "1 security key or passkey of 1 user") ||
			!strings.Contains(r.Message, "new.example.com") || r.Fix == "" {
			t.Errorf("one stale key: %+v", r)
		}

		host := doctor.Run(ctx, a.doctorOptions(), doctor.ScopeInstall, doctor.ScopeRuntime)
		if r := findResult(t, host, id); r.Status != doctor.StatusUnavailable {
			t.Errorf("host doctor: %+v, want unavailable", r)
		}
	})
}

// TestPlatformDoctorKubernetes checks that the API's doctor runs the
// cluster checks, but not the ones that create pods, and counts the
// sites' reservations from the state database.
func TestPlatformDoctorKubernetes(t *testing.T) {
	storetest.Run(t, func(t *testing.T, db *store.DB) {
		ctx := context.Background()
		cluster := doctortest.HealthyCluster()
		a := &app{
			cfg: config.Config{Driver: config.SiteDriverKubernetes, DataDir: "/data"},
			log: slog.New(slog.DiscardHandler),
			opts: Options{
				HostProbe: doctortest.Healthy(), DockerProbe: &doctortest.Docker{}, ClusterProbe: cluster,
			},
		}
		rep := a.platformDoctor(nil, siteReservations{db})(ctx)
		if r := findResult(t, rep, "k8s-version"); r.Status != doctor.StatusPass {
			t.Errorf("version: %+v", r)
		}
		for _, id := range []string{"k8s-cgroup", "k8s-networkpolicy"} {
			if r := findResult(t, rep, id); r.Status != doctor.StatusUnavailable {
				t.Errorf("%s: %+v, want unavailable in the API", id, r)
			}
		}
		if r := findResult(t, rep, "k8s-capacity"); r.Status != doctor.StatusPass || !strings.Contains(r.Message, "0 sites") {
			t.Errorf("capacity: %+v", r)
		}
		if cluster.PodProbes != 0 {
			t.Errorf("the API ran %d pod probes", cluster.PodProbes)
		}
	})
}
