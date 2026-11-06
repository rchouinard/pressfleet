package cli

import (
	"log/slog"
	"sync"

	"github.com/rchouinard/pressfleet/control-plane/internal/sites"
	"github.com/rchouinard/pressfleet/control-plane/internal/upgradestate"
)

// rolloutHold holds the core, image, and template rollouts while the
// upgrade record (dir is config.Layout.UpgradeDir) says the upgrade isn't
// confirmed (#247). It reads the record on every call, so the CLI marking
// the upgrade succeeded lifts the hold without a restart. The log gets one
// line when the hold starts, changes, or lifts, not one per read.
func rolloutHold(dir string, log *slog.Logger) sites.RolloutHold {
	var (
		mu   sync.Mutex
		last string
	)
	return func() (string, bool) {
		hold := upgradestate.Check(dir)
		state := "released"
		if hold.Held {
			state = hold.Status + "|" + hold.Reason
		}
		mu.Lock()
		changed := state != last
		last = state
		mu.Unlock()
		if changed {
			switch {
			case hold.Err != nil:
				log.Warn("rollouts held: the upgrade record can't be read", "error", hold.Err)
			case hold.Held:
				log.Warn("rollouts held until the upgrade is confirmed", "status", hold.Status, "reason", hold.Reason)
			default:
				log.Info("rollouts are not held by an upgrade", "status", hold.Status)
			}
		}
		return hold.Reason, hold.Held
	}
}
