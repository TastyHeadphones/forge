package lint

import (
	"testing"

	"github.com/reliant-labs/forge/internal/linter/finding"
	"github.com/reliant-labs/forge/pkg/release"
)

// An env a reconciler may apply is refused; one forge applies is only warned.
// The same object, the same rule, a different consequence.
func TestFluxSafeSeverityFollowsWhoAppliesTheEnv(t *testing.T) {
	f := release.FluxFinding{Rule: release.RuleJobTTL, Kind: "Job", Name: "temporal-schema-1023848b73"}
	if got := fluxSafeSeverity(fluxSafeFinding{reconciled: true, FluxFinding: f}); got != finding.SeverityError {
		t.Errorf("reconciled env: %s, want error", got)
	}
	if got := fluxSafeSeverity(fluxSafeFinding{reconciled: false, FluxFinding: f}); got != finding.SeverityWarning {
		t.Errorf("forge-applied env: %s, want warning", got)
	}
}

func TestFluxSafeLintIsRegisteredAsAGate(t *testing.T) {
	var found bool
	for _, s := range lintPipeline() {
		if s.name == "flux-safe lint" {
			found = true
			if !s.gates {
				t.Error("flux-safe lint does not gate: a TTL'd Job would pass `forge lint`")
			}
		}
	}
	if !found {
		t.Error("flux-safe lint is not in the sweep")
	}
}
