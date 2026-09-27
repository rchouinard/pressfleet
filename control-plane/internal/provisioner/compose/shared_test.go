package compose_test

import (
	"context"
	"encoding/json"
	"io"
	"slices"
	"strings"
	"testing"

	"github.com/rchouinard/pressfleet/control-plane/internal/config"
	"github.com/rchouinard/pressfleet/control-plane/internal/provisioner/compose"
)

const testRedis = "pressfleet-redis-1"

// sharedDocker fakes the docker calls around the platform services of one
// existing site: every shared container's networks, and the state-changing
// calls made.
type sharedDocker struct {
	t        *testing.T
	networks map[string]map[string]bool // container -> networks
	calls    []string
}

func (f *sharedDocker) exec(_ context.Context, _ io.Reader, args ...string) ([]byte, error) {
	switch {
	case args[0] == "compose" && slices.Contains(args, "ps"):
		var out []byte
		for _, svc := range []string{"php", "nginx"} {
			b, _ := json.Marshal(compose.Container{Name: "pf-site-" + svc + "-1", Service: svc, State: "running", Health: "healthy"})
			out = append(append(out, b...), '\n')
		}
		return out, nil
	case args[0] == "compose" && slices.Contains(args, "down"):
		f.calls = append(f.calls, "compose down")
		return nil, nil
	case args[0] == "container" && args[1] == "inspect":
		nets, ok := f.networks[args[len(args)-1]]
		if !ok {
			return nil, errNoSuchContainer
		}
		m := map[string]any{}
		for n := range nets {
			m[n] = map[string]any{}
		}
		return json.Marshal([]any{map[string]any{"NetworkSettings": map[string]any{"Networks": m}}})
	case args[0] == "network" && (args[1] == "connect" || args[1] == "disconnect"):
		f.calls = append(f.calls, strings.Join(args, " "))
		container, network := args[len(args)-1], args[len(args)-2]
		if args[1] == "connect" {
			f.networks[container][network] = true
		} else {
			delete(f.networks[container], network)
		}
		return nil, nil
	}
	f.t.Fatalf("unexpected docker call: %q", args)
	return nil, nil
}

// TestSharedServicesIncludeRedis checks that the object cache joins and
// leaves site networks alongside MariaDB, under its own alias.
func TestSharedServicesIncludeRedis(t *testing.T) {
	docker := &sharedDocker{t: t, networks: map[string]map[string]bool{testMariaDB: {}, testRedis: {}}}
	prov, err := compose.NewProvisioner(compose.ProvisionerConfig{
		Layout:           config.Layout{Root: t.TempDir()},
		Secrets:          memSecrets{},
		Images:           imageMap{},
		MariaDBContainer: testMariaDB,
		RedisContainer:   testRedis,
		Docker:           docker.exec,
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	network := compose.SiteProjectName(testSiteID)

	if err := prov.AttachSharedServices(ctx, testSiteID); err != nil {
		t.Fatal(err)
	}
	want := []string{
		"network connect --alias " + compose.MariaDBAlias + " " + network + " " + testMariaDB,
		"network connect --alias " + compose.RedisAlias + " " + network + " " + testRedis,
	}
	if !slices.Equal(docker.calls, want) {
		t.Errorf("attach calls:\n got %q\nwant %q", docker.calls, want)
	}

	docker.calls = nil
	if err := prov.DeleteSite(ctx, testSiteID); err != nil {
		t.Fatal(err)
	}
	want = []string{
		"network disconnect " + network + " " + testMariaDB,
		"network disconnect " + network + " " + testRedis,
		"compose down",
	}
	if !slices.Equal(docker.calls, want) {
		t.Errorf("delete calls:\n got %q\nwant %q", docker.calls, want)
	}
}
