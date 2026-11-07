package cli

import (
	"github.com/rchouinard/pressfleet/control-plane/internal/skew"
)

// upgradeStep is what moving the control plane from one release to another
// is, under the upgrade policy of design doc §4.7. `pressfleet upgrade` on
// Compose and the Helm chart's pre-upgrade hook on Kubernetes apply the same
// policy, each with its own advice.
type upgradeStep int

const (
	// stepUnordered: a development build, which can't be ordered.
	stepUnordered upgradeStep = iota
	// stepSame: the same release, applied again.
	stepSame
	// stepUp: an upgrade the policy allows.
	stepUp
	// stepDown: a downgrade, which is refused.
	stepDown
	// stepTooOld: from a release older than skew.MinUpgradeFrom, which
	// needs an intermediate release first.
	stepTooOld
)

// upgradeOrder is the upgrade policy applied to two versions.
type upgradeOrder struct {
	Step upgradeStep
	// From and To are the versions, parsed; zero for stepUnordered.
	From, To skew.Version
	// Oldest is the oldest release this one upgrades from.
	Oldest skew.Version
}

// classifyUpgrade applies the upgrade policy to moving the control plane
// from one version to another.
func classifyUpgrade(from, to string) (upgradeOrder, error) {
	oldest, err := skew.Parse(skew.MinUpgradeFrom)
	if err != nil {
		return upgradeOrder{}, err
	}
	toV, toErr := skew.Parse(to)
	fromV, fromErr := skew.Parse(from)
	if toErr != nil || fromErr != nil {
		return upgradeOrder{Step: stepUnordered, Oldest: oldest}, nil
	}
	o := upgradeOrder{From: fromV, To: toV, Oldest: oldest}
	switch {
	case toV.Compare(fromV) < 0:
		o.Step = stepDown
	case fromV.Compare(oldest) < 0:
		o.Step = stepTooOld
	case toV.Compare(fromV) == 0:
		o.Step = stepSame
	default:
		o.Step = stepUp
	}
	return o, nil
}
