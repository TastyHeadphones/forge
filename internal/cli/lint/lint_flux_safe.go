// File: internal/cli/lint/lint_flux_safe.go
//
// flux-safe — an object that is wrong ONLY under a reconciler.
//
// forge's direct apply is a one-shot; Flux re-applies the same bytes on every
// interval and prunes what a bundle stops carrying. So a Job with
// ttlSecondsAfterFinished is deleted by the TTL controller and created again by
// the next reconcile (a migration re-runs every period), and a Job whose name
// carries no hash of its spec fails every reconcile after its first change
// because a Job's spec is immutable. The rules, and the control-plane incidents
// behind them, are in pkg/release/fluxsafe.go; the bundle build refuses the same
// objects, so this is the same check run earlier, over every env.
//
// SEVERITY FOLLOWS WHO APPLIES THE ENV. An env a reconciler may apply (it
// declares neither a local or ephemeral lifecycle nor forge as its applier) gets
// an ERROR: its bundle would be refused at build. An env forge applies itself
// gets a WARNING: nothing re-applies it today, but it cannot move onto Flux
// until the object is fixed, and saying so now is cheaper than finding out in
// the migration. A render that fails here is skipped, as in the PDB lint: lint
// judges the project and `forge env render` owns render errors.
package lint

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"

	"gopkg.in/yaml.v3"

	"github.com/reliant-labs/forge/internal/cluster"
	"github.com/reliant-labs/forge/internal/config"
	"github.com/reliant-labs/forge/internal/kclrender"
	"github.com/reliant-labs/forge/internal/linter/finding"
	"github.com/reliant-labs/forge/pkg/release"
)

// fluxSafeFinding is one unsafe object in one env.
type fluxSafeFinding struct {
	env        string
	reconciled bool
	release.FluxFinding
}

func fluxSafeFindings(ctx context.Context, projectDir string, cfg *config.ProjectConfig) []fluxSafeFinding {
	kclDir := filepath.Join(projectDir, deployKCLDirFor(cfg))
	entries, err := os.ReadDir(kclDir)
	if err != nil {
		return nil
	}
	var out []fluxSafeFinding
	for _, e := range entries {
		envDir := filepath.Join(kclDir, e.Name())
		mainK := filepath.Join(envDir, "main.k")
		if !e.IsDir() {
			continue
		}
		if _, err := os.Stat(mainK); err != nil {
			continue
		}
		manifests, err := cluster.RenderManifests(ctx, mainK, "lint", "", e.Name(), nil, nil)
		if err != nil {
			continue
		}
		var docs []any
		for _, raw := range cluster.SplitManifestDocs(manifests) {
			var body any
			if yaml.Unmarshal([]byte(raw), &body) == nil && body != nil {
				docs = append(docs, body)
			}
		}
		reconciled := envReconciled(projectDir, envDir, e.Name())
		for _, f := range release.CheckFluxSafety(docs) {
			out = append(out, fluxSafeFinding{env: e.Name(), reconciled: reconciled, FluxFinding: f})
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].env < out[j].env })
	return out
}

// envReconciled reports whether a reconciler may apply env: it declares neither
// a local or ephemeral lifecycle nor forge as its applier. An env whose render
// cannot be read is judged reconciled, the stricter reading: the finding then
// fails rather than being waved through on a render problem.
func envReconciled(projectDir, envDir, env string) bool {
	raw, err := kclrender.Run(projectDir, envDir, []string{"env=" + strconv.Quote(env)})
	if err != nil {
		return true
	}
	var doc struct {
		Output struct {
			Lifecycle string `json:"lifecycle"`
			Applier   string `json:"applier"`
		} `json:"output"`
	}
	if json.Unmarshal(raw, &doc) != nil {
		return true
	}
	return doc.Output.Lifecycle == "" && doc.Output.Applier == ""
}

func fluxSafeSeverity(f fluxSafeFinding) finding.Severity {
	if f.reconciled {
		return finding.SeverityError
	}
	return finding.SeverityWarning
}

func runFluxSafeLint(ctx context.Context, projectDir string, cfg *config.ProjectConfig) error {
	errs := 0
	for _, f := range fluxSafeFindings(ctx, projectDir, cfg) {
		icon := "⚠️ "
		if fluxSafeSeverity(f) == finding.SeverityError {
			icon = "❌"
			errs++
		}
		fmt.Printf("%s [%s] env %s: %s\n", icon, f.Rule, f.env, f.FluxFinding)
	}
	if errs > 0 {
		return fmt.Errorf("%d object(s) are not safe under a reconciler", errs)
	}
	return nil
}

func collectFluxSafeJSON(rc *lintRunCtx) ([]lintJSONFinding, bool, error) {
	var out []lintJSONFinding
	gated := false
	for _, f := range fluxSafeFindings(rc.ctx, rc.cwd, rc.cfg) {
		sev := fluxSafeSeverity(f)
		gated = gated || sev == finding.SeverityError
		out = append(out, lintJSONFinding{
			File:     deployKCLDirFor(rc.cfg) + "/" + f.env,
			Severity: string(sev),
			Rule:     f.Rule,
			Message:  fmt.Sprintf("env %s: %s %s: %s", f.env, f.Kind, f.Name, f.Reason),
			FixHint:  f.Fix,
		})
	}
	return out, gated, nil
}
