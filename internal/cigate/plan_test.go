package cigate

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/reliant-labs/forge/internal/config"
	"github.com/reliant-labs/forge/internal/templates"
)

func write(t *testing.T, root, rel, body string) {
	t.Helper()
	p := filepath.Join(root, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func names(cs []Check) []string {
	var out []string
	for _, c := range cs {
		out = append(out, c.Name)
	}
	return out
}

func find(t *testing.T, cs []Check, name string) Check {
	t.Helper()
	for _, c := range cs {
		if c.Name == name {
			return c
		}
	}
	t.Fatalf("no check %q in %v", name, names(cs))
	return Check{}
}

func serviceInput(t *testing.T) PlanInput {
	root := t.TempDir()
	write(t, root, "cmd/app/main.go", "package main\nfunc main(){}\n")
	write(t, root, "db/migrations/001_a.up.sql", "select 1;")
	return PlanInput{
		Root:  root,
		Forge: []string{"forge"},
		CI: templates.CIWorkflowData{
			HasServices: true, HasFrontends: true,
			Frontends:    []templates.FrontendCIConfig{{Name: "web", Path: "frontends/web"}},
			LintGolangci: true, LintBuf: true, LintMigrationSafety: true,
			LintFrontend: true, LintFrontendStyles: true,
			VulnGo: true, VulnNPM: true, LicenseCheck: true,
			HasKCL: true, VerifyGenerated: true, RunsBufGenerate: true, TestRace: true,
		},
	}
}

// The order is load-bearing: verify-generated runs first (after dependency
// installs) because it needs the committed tree, and the KCL checks run last.
func TestPlan_OrderAndLanes(t *testing.T) {
	all, err := Plan(serviceInput(t))
	if err != nil {
		t.Fatal(err)
	}
	got := strings.Join(names(all), " ")
	want := "deps-web verify-generated golangci-lint buf-lint migration-safety lint-web typecheck-web css-health-web go-build build-web vuln-go vuln-npm license-check validate-kcl deployability deps-web unit-tests integration-tests"
	if got != want {
		t.Errorf("plan order:\n got %s\nwant %s", got, want)
	}

	if checks := names(Select(all, []Lane{LaneChecks})); checks[len(checks)-1] != "deployability" || checks[1] != "verify-generated" {
		t.Errorf("checks lane = %v", checks)
	}
	if test := names(Select(all, []Lane{LaneTest})); strings.Join(test, " ") != "deps-web unit-tests" {
		t.Errorf("test lane = %v", test)
	}
}

// `task ci` runs checks then test: the frontend install appears in both lanes
// and must run once, not twice.
func TestSelect_DedupesSharedRows(t *testing.T) {
	all, _ := Plan(serviceInput(t))
	n := 0
	for _, c := range Select(all, []Lane{LaneChecks, LaneTest, LaneIntegration}) {
		if c.Name == "deps-web" {
			n++
		}
	}
	if n != 1 {
		t.Errorf("deps-web appears %d times in `task ci`, want 1", n)
	}
}

// The gate follows the same toggles the workflow renders from: a check the
// project turned off in forge.yaml is not a row at all (not a SKIPPED one).
func TestPlan_FollowsTheWorkflowToggles(t *testing.T) {
	in := serviceInput(t)
	in.CI.LintGolangci, in.CI.VulnGo, in.CI.LicenseCheck, in.CI.HasKCL, in.CI.HasFrontends, in.CI.Frontends = false, false, false, false, false, nil
	all, err := Plan(in)
	if err != nil {
		t.Fatal(err)
	}
	for _, gone := range []string{"golangci-lint", "vuln-go", "license-check", "validate-kcl", "deployability", "deps-web", "lint-web", "build-web", "vuln-npm"} {
		for _, n := range names(all) {
			if n == gone {
				t.Errorf("%s is planned though its toggle is off", gone)
			}
		}
	}
	find(t, all, "go-build")
}

// A project with nothing for a check to look at gets N/A, which is not a skip.
func TestPlan_NotApplicableRows(t *testing.T) {
	in := PlanInput{Root: t.TempDir(), Forge: []string{"forge"}, CI: templates.CIWorkflowData{LintMigrationSafety: true, VerifyGenerated: true}}
	all, _ := Plan(in)
	if c := find(t, all, "migration-safety"); c.NotApplicable == "" {
		t.Error("migration-safety without db/migrations should be N/A")
	}
	if c := find(t, all, "go-build"); c.NotApplicable == "" {
		t.Error("go-build without cmd/ should be N/A")
	}
	if c := find(t, all, "integration-tests"); c.NotApplicable == "" {
		t.Error("integration-tests without //go:build integration files should be N/A")
	}
}

func TestHasIntegrationTests(t *testing.T) {
	root := t.TempDir()
	write(t, root, "a/a_test.go", "package a\n")
	write(t, root, "node_modules/x/x_test.go", "//go:build integration\n\npackage x\n")
	if HasIntegrationTests(root) {
		t.Fatal("a plain test, and one inside node_modules, are not an integration lane")
	}
	write(t, root, "internal/db/db_test.go", "//go:build integration\n\npackage db\n")
	if !HasIntegrationTests(root) {
		t.Fatal("a //go:build integration test file was not found")
	}
	root2 := t.TempDir()
	write(t, root2, "x_test.go", "//go:build linux && integration && !race\n\npackage x\n")
	if !HasIntegrationTests(root2) {
		t.Error("a compound constraint naming integration was not found")
	}
}

// A missing dependency directory makes the npm rows SKIPPED with the command
// that fixes it, never a bare npm failure that reads like a code defect.
func TestPlan_FrontendRowsNeedTheirDeps(t *testing.T) {
	in := serviceInput(t)
	all, _ := Plan(in)
	lint := find(t, all, "lint-web")
	reason := lint.Prereq()
	if !strings.Contains(reason, "npm ci") || !strings.Contains(reason, "frontends/web") {
		t.Errorf("reason %q should name the fix", reason)
	}
	write(t, in.Root, "frontends/web/node_modules/.keep", "")
	if reason := lint.Prereq(); reason != "" {
		t.Errorf("with node_modules present the row should run, got %q", reason)
	}
	if note := find(t, all, "deps-web").Done(); note != "already installed" {
		t.Errorf("deps-web Done = %q", note)
	}
}

// license-check exempts the project's own module (#755): go-licenses checks
// every package it loads, so without --ignore a project with no LICENSE file
// logged "Failed to find license" for each of its own packages.
func TestPlan_LicenseCheckIgnoresTheProjectsOwnModule(t *testing.T) {
	in := serviceInput(t)
	write(t, in.Root, "go.mod", "module example.com/shop\n\ngo 1.24\n\nrequire github.com/google/uuid v1.6.0\n")
	all, err := Plan(in)
	if err != nil {
		t.Fatal(err)
	}
	got := strings.Join(find(t, all, "license-check").Argv, " ")
	if want := "go-licenses check ./... --disallowed_types=forbidden,restricted --ignore example.com/shop"; got != want {
		t.Errorf("license-check argv:\n got %s\nwant %s", got, want)
	}
}

// go-licenses matches --ignore as a plain string prefix, so the ignores must
// not reach past the project's own modules, and must reach all of them.
func TestLicenseIgnores(t *testing.T) {
	for _, tc := range []struct {
		name  string
		files map[string]string
		want  string
	}{
		{"no go.mod: nothing to exempt", nil, ""},
		{"own module", map[string]string{
			"go.mod": "module example.com/shop\n",
		}, "example.com/shop"},
		{"a dependency extends the module path: only the subtree", map[string]string{
			"go.mod": "module example.com/shop\n\nrequire (\n\texample.com/shop-client v1.0.0\n\texample.com/shop/api v0.1.0\n)\n",
		}, "example.com/shop/"},
		{"workspace: modules inside the tree, not a linked checkout outside it", map[string]string{
			"go.mod":       "module example.com/shop\n",
			"go.work":      "go 1.24\n\nuse (\n\t.\n\t./tools\n\t./api\n\t../forge\n)\n",
			"tools/go.mod": "module example.com/shoptools\n",
			"api/go.mod":   "module example.com/shop/api\n", // nested: covered by example.com/shop
		}, "example.com/shop example.com/shoptools"},
		{"workspace modules requiring each other are not collisions", map[string]string{
			"go.mod":       "module example.com/shop\n\nrequire example.com/shoptools v0.0.0\n",
			"go.work":      "go 1.24\n\nuse (\n\t.\n\t./tools\n)\n",
			"tools/go.mod": "module example.com/shoptools\n",
		}, "example.com/shop example.com/shoptools"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			for rel, body := range tc.files {
				write(t, root, rel, body)
			}
			if got := strings.Join(licenseIgnores(root), " "); got != tc.want {
				t.Errorf("licenseIgnores = %q, want %q", got, tc.want)
			}
		})
	}
}

// ci.race=false in forge.yaml reaches `task test` the way the workflow carries
// it, so the local gate and CI run the same go test flags.
func TestPlan_CarriesTheRaceToggleToTask(t *testing.T) {
	in := serviceInput(t)
	in.CI.TestRace = false
	all, _ := Plan(in)
	if got := strings.Join(find(t, all, "unit-tests").Argv, " "); got != "task test GOTESTRACE=" {
		t.Errorf("argv = %q", got)
	}
}

func TestPlan_ExtraChecks(t *testing.T) {
	in := serviceInput(t)
	in.Cfg = &config.ProjectConfig{}
	in.Cfg.CI.Gate.ExtraChecks = []config.CIGateCheck{
		{Name: "gen-llm-gateway", Command: []string{"go", "run", "./cmd/gen-llm-gateway", "-check"}, Requires: []string{"definitely-not-a-binary-xyz"}},
	}
	all, err := Plan(in)
	if err != nil {
		t.Fatal(err)
	}
	extra := find(t, all, "gen-llm-gateway")
	if extra.Lane != LaneChecks {
		t.Errorf("extra check lane = %s, want checks (so `task ci:checks` and the CI checks job run it)", extra.Lane)
	}
	if got := names(Select(all, []Lane{LaneChecks})); got[len(got)-1] != "gen-llm-gateway" {
		t.Errorf("extras run after forge's own checks, got %v", got)
	}
	if reason := extra.Prereq(); !strings.Contains(reason, "not on PATH") {
		t.Errorf("a missing required binary must SKIP, got reason %q", reason)
	}

	for name, bad := range map[string]config.CIGateCheck{
		"no name":           {Command: []string{"x"}},
		"no command":        {Name: "x"},
		"collides w/ forge": {Name: "golangci-lint", Command: []string{"x"}},
	} {
		in.Cfg.CI.Gate.ExtraChecks = []config.CIGateCheck{bad}
		if _, err := Plan(in); err == nil {
			t.Errorf("%s: want an error", name)
		}
	}
	in.Cfg.CI.Gate.ExtraChecks = []config.CIGateCheck{{Name: "dup", Command: []string{"x"}}, {Name: "dup", Command: []string{"y"}}}
	if _, err := Plan(in); err == nil {
		t.Error("duplicate extra names must be rejected: CI_SKIP could not tell them apart")
	}
}

// buf is pinned only where verify-generated regenerates with it.
func TestPlan_BufPinnedOnlyWhereGenerateRunsIt(t *testing.T) {
	in := serviceInput(t)
	in.CI.RunsBufGenerate = false
	all, _ := Plan(in)
	if len(find(t, all, "verify-generated").Tools) != 0 {
		t.Error("verify-generated pins buf though generate does not run it")
	}
	in.CI.RunsBufGenerate = true
	all, _ = Plan(in)
	if tools := find(t, all, "verify-generated").Tools; len(tools) != 1 || tools[0].Binary != "buf" {
		t.Errorf("verify-generated tools = %+v, want buf", tools)
	}
}
