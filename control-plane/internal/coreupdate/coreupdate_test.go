package coreupdate_test

import (
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/rchouinard/pressfleet/control-plane/internal/catalog"
	"github.com/rchouinard/pressfleet/control-plane/internal/coreupdate"
)

// The manifest after WordPress 7.2 came out: 7.1 is kept for sites held on
// it or awaiting approval and got a security release (7.1.4); 6.9 is kept
// too but no longer gets security fixes.
const manifest = `{
  "registry": "registry.example/pf",
  "php": [{"version": "8.4", "activeSupportEnds": "2026-12-31", "securitySupportEnds": "2028-12-31"}],
  "wordpress": {
    "latest": {"version": "7.2.1", "sha1": "761b8101538f0631a0bfc4fba7bc4abeea92f81c", "published": "2026-09-01"},
    "pinned": [
      {"version": "7.1.4", "sha1": "1111111111111111111111111111111111111111", "published": "2026-09-01"},
      {"version": "6.9.9", "sha1": "2222222222222222222222222222222222222222", "published": "2026-03-01", "insecure": true}
    ]
  }
}`

func day(s string) time.Time {
	t, err := time.Parse(time.DateOnly, s)
	if err != nil {
		panic(err)
	}
	return t
}

func mustCatalog(t *testing.T, m string) *catalog.Catalog {
	t.Helper()
	c, err := catalog.Parse([]byte(m))
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func warningCodes(p coreupdate.Plan) []string {
	var out []string
	for _, w := range p.Warnings {
		out = append(out, w.Code)
	}
	return out
}

func TestDecide(t *testing.T) {
	cat := mustCatalog(t, manifest)
	now := day("2026-09-10")
	const delay = 14 * 24 * time.Hour
	held := coreupdate.Policy{Hold: true, HoldSince: day("2026-08-01")}
	approve := func(b string) coreupdate.Policy { return coreupdate.Policy{ApprovedBranch: b} }
	tests := []struct {
		name        string
		current     string
		site, group coreupdate.Policy
		settings    coreupdate.Settings
		now         time.Time

		target   string
		kind     coreupdate.Kind
		source   coreupdate.Source
		pending  string
		reason   coreupdate.PendingReason
		autoAt   string
		warnings []string
	}{
		{
			// The acceptance criterion: a held site gets its branch's
			// security release but not the next major.
			name: "held site gets its branch's security release", current: "7.1.2", site: held,
			target: "7.1.4", kind: coreupdate.KindMinor, source: coreupdate.SourceSite,
			pending: "7.2.1", reason: coreupdate.PendingHeld,
		},
		{
			name: "held group", current: "7.1.2", group: held,
			target: "7.1.4", kind: coreupdate.KindMinor, source: coreupdate.SourceClientGroup,
			pending: "7.2.1", reason: coreupdate.PendingHeld,
		},
		{
			name: "held and up to date on its branch", current: "7.1.4", site: held,
			source: coreupdate.SourceSite, pending: "7.2.1", reason: coreupdate.PendingHeld,
		},
		{
			// The acceptance criterion: a major release reaches a site
			// only after approval.
			name: "major awaits approval; the minor goes ahead", current: "7.1.2",
			target: "7.1.4", kind: coreupdate.KindMinor,
			pending: "7.2.1", reason: coreupdate.PendingAwaitingApproval,
		},
		{
			name: "site approval", current: "7.1.2", site: approve("7.2"),
			target: "7.2.1", kind: coreupdate.KindMajor, source: coreupdate.SourceSite,
		},
		{
			name: "group approval", current: "7.1.4", group: approve("7.2"),
			target: "7.2.1", kind: coreupdate.KindMajor, source: coreupdate.SourceClientGroup,
		},
		{
			name: "site hold wins over group approval", current: "7.1.2", site: held, group: approve("7.2"),
			target: "7.1.4", kind: coreupdate.KindMinor, source: coreupdate.SourceSite,
			pending: "7.2.1", reason: coreupdate.PendingHeld,
		},
		{
			name: "site approval wins over group hold", current: "7.1.2", site: approve("7.2"), group: held,
			target: "7.2.1", kind: coreupdate.KindMajor, source: coreupdate.SourceSite,
		},
		{
			name: "approval of an older branch than the newest", current: "6.9.9", site: approve("7.1"),
			target: "7.1.4", kind: coreupdate.KindMajor, source: coreupdate.SourceSite,
			pending: "7.2.1", reason: coreupdate.PendingAwaitingApproval,
		},
		{
			name: "approval skips majors", current: "6.9.9", site: approve("7.2"),
			target: "7.2.1", kind: coreupdate.KindMajor, source: coreupdate.SourceSite,
		},
		{
			name: "approval of the current branch changes nothing", current: "7.1.2", site: approve("7.1"),
			target: "7.1.4", kind: coreupdate.KindMinor, source: coreupdate.SourceSite,
			pending: "7.2.1", reason: coreupdate.PendingAwaitingApproval,
		},
		{
			// The acceptance criterion: or after the configured delay.
			name: "delay not yet over", current: "7.1.4", settings: coreupdate.Settings{MajorAutoApproveAfter: delay},
			pending: "7.2.1", reason: coreupdate.PendingAwaitingApproval, autoAt: "2026-09-15",
		},
		{
			name: "delay over", current: "7.1.4", settings: coreupdate.Settings{MajorAutoApproveAfter: delay}, now: day("2026-09-15"),
			target: "7.2.1", kind: coreupdate.KindMajor, source: coreupdate.SourceAuto,
		},
		{
			name: "hold overrides the delay", current: "7.1.4", group: held,
			settings: coreupdate.Settings{MajorAutoApproveAfter: delay}, now: day("2026-10-01"),
			source: coreupdate.SourceClientGroup, pending: "7.2.1", reason: coreupdate.PendingHeld,
		},
		{
			name: "delay approves the newest branch that is due", current: "6.9.9",
			settings: coreupdate.Settings{MajorAutoApproveAfter: 30 * 24 * time.Hour}, now: day("2026-09-20"),
			// 7.1.4 and 7.2.1 were both published on 2026-09-01; neither is due.
			pending: "7.2.1", reason: coreupdate.PendingAwaitingApproval, autoAt: "2026-10-01",
			warnings: []string{coreupdate.WarnBranchInsecure},
		},
		{
			name: "up to date", current: "7.2.1",
		},
		{
			name: "insecure branch", current: "6.9.8",
			target: "6.9.9", kind: coreupdate.KindMinor,
			pending: "7.2.1", reason: coreupdate.PendingAwaitingApproval,
			warnings: []string{coreupdate.WarnBranchInsecure},
		},
		{
			name: "branch no longer built", current: "7.0.3",
			pending: "7.2.1", reason: coreupdate.PendingAwaitingApproval,
			warnings: []string{coreupdate.WarnBranchNotBuilt},
		},
		{
			name: "branch no longer built, but approved away", current: "7.0.3", site: approve("7.2"),
			target: "7.2.1", kind: coreupdate.KindMajor, source: coreupdate.SourceSite,
		},
		{
			name: "held long", current: "7.1.4", site: held, now: day("2026-11-01"),
			source: coreupdate.SourceSite, pending: "7.2.1", reason: coreupdate.PendingHeld,
			warnings: []string{coreupdate.WarnHeldLong},
		},
		{
			name: "held long, with a shorter warning", current: "7.1.4", site: held,
			settings: coreupdate.Settings{HoldWarningAfter: 30 * 24 * time.Hour},
			source:   coreupdate.SourceSite, pending: "7.2.1", reason: coreupdate.PendingHeld,
			warnings: []string{coreupdate.WarnHeldLong},
		},
		{
			name: "held on the newest branch is no concern", current: "7.2.1", site: held, now: day("2027-06-01"),
			source: coreupdate.SourceSite,
		},
		{
			name: "newer than the manifest", current: "7.3",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			at := now
			if !tt.now.IsZero() {
				at = tt.now
			}
			p, err := coreupdate.Decide(cat, tt.current, tt.site, tt.group, tt.settings, at)
			if err != nil {
				t.Fatal(err)
			}
			if p.Target != tt.target || p.TargetKind != tt.kind || p.Source != tt.source {
				t.Errorf("target %q (%q, source %q), want %q (%q, source %q)", p.Target, p.TargetKind, p.Source, tt.target, tt.kind, tt.source)
			}
			switch {
			case tt.pending == "" && p.Pending != nil:
				t.Errorf("pending %+v, want none", *p.Pending)
			case tt.pending != "" && p.Pending == nil:
				t.Errorf("no pending release, want %s", tt.pending)
			case tt.pending != "":
				if p.Pending.Version != tt.pending || p.Pending.Reason != tt.reason {
					t.Errorf("pending %+v, want %s (%s)", *p.Pending, tt.pending, tt.reason)
				}
				if got := p.Pending.AutoApproveAt; (tt.autoAt == "") != got.IsZero() || (tt.autoAt != "" && !got.Equal(day(tt.autoAt))) {
					t.Errorf("auto approval at %v, want %q", got, tt.autoAt)
				}
			}
			if got := warningCodes(p); !slices.Equal(got, tt.warnings) {
				t.Errorf("warnings %v, want %v", p.Warnings, tt.warnings)
			}
			if p.Current != tt.current {
				t.Errorf("current %q, want %q", p.Current, tt.current)
			}
		})
	}
}

func TestDecidePlanDetails(t *testing.T) {
	cat := mustCatalog(t, manifest)
	since := day("2026-08-01")
	p, err := coreupdate.Decide(cat, "7.1.2", coreupdate.Policy{Hold: true, HoldSince: since, ApprovedBranch: "7.2"},
		coreupdate.Policy{}, coreupdate.Settings{}, day("2026-09-10"))
	if err != nil {
		t.Fatal(err)
	}
	if p.Branch != "7.1" || !p.Held || !p.HeldSince.Equal(since) || p.ApprovedBranch != "" {
		t.Errorf("plan = %+v; want branch 7.1, held since %v, and the approval ignored while held", p, since)
	}
}

func TestPlanAllowsBranch(t *testing.T) {
	cat := mustCatalog(t, manifest)
	at := day("2026-09-10")
	hold := coreupdate.Policy{Hold: true, HoldSince: day("2026-08-01")}
	approve := func(branch string) coreupdate.Policy { return coreupdate.Policy{ApprovedBranch: branch} }
	auto := coreupdate.Settings{MajorAutoApproveAfter: 5 * 24 * time.Hour}
	for _, tt := range []struct {
		name        string
		site, group coreupdate.Policy
		settings    coreupdate.Settings
		branch      string
		want        bool
	}{
		{name: "its own branch", branch: "7.1", want: true},
		{name: "its own branch, held", site: hold, branch: "7.1", want: true},
		{name: "an older branch", branch: "6.9", want: true},
		{name: "a newer branch, without approval", branch: "7.2"},
		{name: "a newer branch, approved", site: approve("7.2"), branch: "7.2", want: true},
		{name: "a newer branch, approved by its group", group: approve("7.2"), branch: "7.2", want: true},
		{name: "a newer branch, past the approval", site: approve("7.1"), branch: "7.2"},
		{name: "a newer branch, held", site: hold, branch: "7.2"},
		{name: "a newer branch, its group held", group: hold, branch: "7.2"},
		{name: "the site's hold beats its group's approval", site: hold, group: approve("7.2"), branch: "7.2"},
		{name: "the site's approval beats its group's hold", site: approve("7.2"), group: hold, branch: "7.2", want: true},
		{name: "approved automatically", settings: auto, branch: "7.2", want: true},
		{name: "not yet approved automatically", settings: coreupdate.Settings{MajorAutoApproveAfter: 30 * 24 * time.Hour}, branch: "7.2"},
		{name: "a hold beats automatic approval", site: hold, settings: auto, branch: "7.2"},
		{name: "an explicit approval beats automatic approval", site: approve("7.1"), settings: auto, branch: "7.2"},
		{name: "a malformed branch", site: approve("7.2"), branch: "seven"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			p, err := coreupdate.Decide(cat, "7.1.2", tt.site, tt.group, tt.settings, at)
			if err != nil {
				t.Fatal(err)
			}
			if got := p.AllowsBranch(tt.branch); got != tt.want {
				t.Errorf("AllowsBranch(%q) = %v, want %v (plan %+v)", tt.branch, got, tt.want, p)
			}
		})
	}
}

func TestDecideErrors(t *testing.T) {
	cat := mustCatalog(t, manifest)
	if _, err := coreupdate.Decide(cat, "latest", coreupdate.Policy{}, coreupdate.Policy{}, coreupdate.Settings{}, time.Now()); !errors.Is(err, catalog.ErrInvalidVersion) {
		t.Errorf("Decide(latest) error = %v, want ErrInvalidVersion", err)
	}
	bad := coreupdate.Policy{ApprovedBranch: "7.2.1"}
	if _, err := coreupdate.Decide(cat, "7.1.2", coreupdate.Policy{}, bad, coreupdate.Settings{}, time.Now()); !errors.Is(err, coreupdate.ErrInvalidPolicy) {
		t.Errorf("Decide with a bad group policy: error = %v, want ErrInvalidPolicy", err)
	}
	if err := bad.Validate(); !errors.Is(err, coreupdate.ErrInvalidPolicy) {
		t.Errorf("Validate = %v", err)
	}
}
