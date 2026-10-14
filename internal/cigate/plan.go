package cigate

import (
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/reliant-labs/forge/internal/config"
	"github.com/reliant-labs/forge/internal/templates"
)

// Forge owns the step list below. The scaffolded ci.yml renders the same
// toggles (templates.CIWorkflowData) and the same tool pins
// (templates.*Version), and a test holds the workflow's pins to these, so the
// gate and the workflow cannot describe two different CIs. A project adds
// checks (forge.yaml ci.gate.extra_checks) and skips checks (CI_SKIP); it does
// not copy this list into a script of its own.

// Pins. Version strings come from templates so the workflow and the gate read
// one constant.
var (
	golangciLint = Tool{
		Binary:      "golangci-lint",
		Module:      "github.com/golangci/golangci-lint/v2/cmd/golangci-lint",
		BuildModule: "github.com/golangci/golangci-lint/v2",
		Version:     templates.GolangciLintVersion,
		MatchGo:     true,
		VersionArgs: []string{"--version"},
		VersionRE:   regexp.MustCompile(`has version v?(\d+\.\d+\.\d+)`),
	}
	buf = Tool{
		Binary:      "buf",
		Module:      "github.com/bufbuild/buf/cmd/buf",
		BuildModule: "github.com/bufbuild/buf",
		Version:     "v" + templates.BufVersion,
		VersionArgs: []string{"--version"},
		VersionRE:   regexp.MustCompile(`^v?(\d+\.\d+\.\d+)`),
	}
	govulncheck = Tool{
		Binary:      "govulncheck",
		Module:      "golang.org/x/vuln/cmd/govulncheck",
		BuildModule: "golang.org/x/vuln",
		Version:     templates.GovulncheckVersion,
	}
	goLicenses = Tool{
		Binary:  "go-licenses",
		Module:  "github.com/google/go-licenses",
		Version: templates.GoLicensesVersion,
	}
)

// PlanInput is everything the plan is derived from.
type PlanInput struct {
	// Root is the project root.
	Root string
	// CI is the data the scaffolded ci.yml renders with: which lints, scanners
	// and jobs the project has. The gate reads the same struct so a toggle
	// means the same thing in both.
	CI templates.CIWorkflowData
	// Cfg supplies the project's extra checks and its derived features.
	Cfg *config.ProjectConfig
	// Forge is the argv that re-enters this forge binary.
	Forge []string
}

// Plan returns every check of every lane, in the order they run.
func Plan(in PlanInput) ([]Check, error) {
	p := planner{PlanInput: in}
	var all []Check
	all = append(all, p.checksLane()...)
	all = append(all, p.testLane()...)
	all = append(all, p.integrationLane()...)
	extras, err := p.extras(all)
	if err != nil {
		return nil, err
	}
	// Extras belong to the checks lane, ahead of the test lanes in the order
	// the table prints.
	n := len(p.checksLane())
	out := append([]Check(nil), all[:n]...)
	out = append(out, extras...)
	out = append(out, all[n:]...)
	return out, nil
}

// Select keeps the checks of the named lanes, in plan order, dropping a check
// whose name an earlier selected lane already ran (the frontend-deps row sits
// in both the checks and test lanes, so `task ci` installs once).
func Select(all []Check, lanes []Lane) []Check {
	want := map[Lane]bool{}
	for _, l := range lanes {
		want[l] = true
	}
	seen := map[string]bool{}
	var out []Check
	for _, c := range all {
		if !want[c.Lane] || seen[c.Name] {
			continue
		}
		seen[c.Name] = true
		out = append(out, c)
	}
	return out
}

type planner struct{ PlanInput }

func (p planner) forge(args ...string) []string {
	return append(append([]string(nil), p.Forge...), args...)
}

func (p planner) frontendDeps(lane Lane) []Check {
	if !p.CI.HasFrontends {
		return nil
	}
	var out []Check
	for _, fe := range p.CI.Frontends {
		fe := fe
		out = append(out, Check{
			Name: "deps-" + fe.Name,
			Lane: lane,
			// `npm ci` deletes node_modules, so the gate installs only what is
			// absent. CI installs first, in the shared setup, so this row is
			// instant there.
			Done: func() string {
				if dirExists(p.path(fe.Path, "node_modules")) {
					return "already installed"
				}
				return ""
			},
			Prereq: needBinary("npm"),
			Argv:   []string{"npm", "ci"},
			Dir:    fe.Path,
		})
	}
	return out
}

func (p planner) path(parts ...string) string {
	return filepath.Join(append([]string{p.Root}, parts...)...)
}

func (p planner) checksLane() []Check {
	ci := p.CI
	var out []Check

	// 1. Frontend dependencies, so every npm row below has what it needs.
	out = append(out, p.frontendDeps(LaneChecks)...)

	// 2. verify-generated runs FIRST. It regenerates the tree and demands a
	// byte-identical result, so it can only attribute drift to
	// `forge generate` when it starts from the committed tree; everything
	// after it may write files. If it fails, the tree it leaves is reset so the
	// rows after it judge the COMMITTED code, not the regenerated one.
	if ci.VerifyGenerated {
		vg := Check{
			Name:   "verify-generated",
			Lane:   LaneChecks,
			Argv:   p.forge("ci", "verify-generated"),
			Prereq: p.cleanTree,
			OnFail: p.resetTree,
		}
		if ci.RunsBufGenerate {
			vg.Tools = []Tool{buf}
		}
		out = append(out, vg)
	}

	// 3. Lint.
	if ci.LintGolangci {
		out = append(out, Check{
			Name:  "golangci-lint",
			Lane:  LaneChecks,
			Tools: []Tool{golangciLint},
			Argv:  []string{"golangci-lint", "run", "--timeout=5m", "--allow-serial-runners"},
		})
	}
	if ci.LintBuf && ci.HasServices {
		out = append(out, Check{
			Name:  "buf-lint",
			Lane:  LaneChecks,
			Tools: []Tool{buf},
			Argv:  []string{"buf", "lint"},
		})
	}
	if ci.LintMigrationSafety {
		c := Check{
			Name: "migration-safety",
			Lane: LaneChecks,
			Argv: p.forge("ci", "migration-safety"),
		}
		if !dirExists(p.path("db", "migrations")) {
			c.NotApplicable = "no db/migrations/ directory"
		}
		out = append(out, c)
	}
	if ci.LintFrontend && ci.HasFrontends {
		for _, fe := range ci.Frontends {
			fe := fe
			needDeps := p.needsDeps(fe)
			out = append(out,
				Check{Name: "lint-" + fe.Name, Lane: LaneChecks, Prereq: needDeps, Argv: []string{"npm", "run", "lint"}, Dir: fe.Path},
				// tsc --noEmit is not covered by eslint; without a dedicated
				// row, type errors only surface at `next build` in a deploy.
				Check{Name: "typecheck-" + fe.Name, Lane: LaneChecks, Prereq: needDeps, Argv: []string{"npm", "run", "typecheck"}, Dir: fe.Path},
			)
			if ci.LintFrontendStyles {
				out = append(out, Check{Name: "css-health-" + fe.Name, Lane: LaneChecks, Prereq: needDeps, Argv: []string{"npm", "run", "lint:styles", "--if-present"}, Dir: fe.Path})
			}
		}
	}

	// 4. Build. -trimpath makes builds reproducible across machines;
	// -buildvcs=true stamps the commit into the binary. Both must match the
	// Dockerfile and `task build` so every binary hashes identically from the
	// same source.
	build := Check{
		Name: "go-build",
		Lane: LaneChecks,
		Argv: []string{"go", "build", "-trimpath", "-buildvcs=true", "./cmd/..."},
	}
	if !dirExists(p.path("cmd")) {
		build.NotApplicable = "no cmd/ directory"
	}
	out = append(out, build)
	if ci.HasFrontends {
		for _, fe := range ci.Frontends {
			fe := fe
			out = append(out, Check{
				Name:   "build-" + fe.Name,
				Lane:   LaneChecks,
				Prereq: p.needsDeps(fe),
				Argv:   []string{"npm", "run", "build"},
				Dir:    fe.Path,
				Env:    []string{"NODE_ENV=production"},
			})
		}
	}

	// 5. Vulnerabilities and licenses.
	if ci.VulnGo {
		out = append(out, Check{
			Name:  "vuln-go",
			Lane:  LaneChecks,
			Tools: []Tool{govulncheck},
			Argv:  p.forge("ci", "vuln-scan", "--go"),
		})
	}
	if ci.VulnNPM && ci.HasFrontends {
		out = append(out, Check{
			Name:   "vuln-npm",
			Lane:   LaneChecks,
			Prereq: needBinary("npm"),
			Argv:   p.forge("ci", "vuln-scan", "--npm"),
		})
	}
	if ci.LicenseCheck {
		out = append(out, Check{
			Name:  "license-check",
			Lane:  LaneChecks,
			Tools: []Tool{goLicenses},
			Argv:  licenseCheckArgv(p.Root),
		})
	}

	// 6. Deployability, last: it is the one that renders every env's KCL.
	if ci.HasKCL {
		out = append(out,
			// Asserts the render is something `kubectl apply` accepts, not
			// merely that the KCL evaluates.
			Check{Name: "validate-kcl", Lane: LaneChecks, Argv: p.forge("ci", "validate-kcl")},
			// Would an SRE let this reach production: probes, resource
			// requests/limits, credentials from Secrets, bound ServiceAccounts.
			Check{Name: "deployability", Lane: LaneChecks, Argv: p.forge("doctor", "--signal", "deploy")},
		)
	}
	return out
}

func (p planner) testLane() []Check {
	out := p.frontendDeps(LaneTest)
	return append(out, Check{
		Name: "unit-tests",
		Lane: LaneTest,
		// Task is the single definition of the suite: this row, the CI `test`
		// job and `task test:all` all run the same commands out of
		// Taskfile.yml.
		Prereq: needBinary("task"),
		Argv:   p.task("test"),
	})
}

func (p planner) integrationLane() []Check {
	c := Check{
		Name:   "integration-tests",
		Lane:   LaneIntegration,
		Prereq: needBinary("task"),
		Argv:   p.task("test:integration"),
	}
	if !HasIntegrationTests(p.Root) {
		c.NotApplicable = "no //go:build integration test files"
	}
	return []Check{c}
}

// task builds a `task <name>` argv, carrying forge.yaml's ci.test.race through
// to the Taskfile the way the workflow does.
func (p planner) task(name string) []string {
	argv := []string{"task", name}
	if !p.CI.TestRace {
		argv = append(argv, "GOTESTRACE=")
	}
	return argv
}

func (p planner) needsDeps(fe templates.FrontendCIConfig) func() string {
	return func() string {
		if dirExists(p.path(fe.Path, "node_modules")) {
			return ""
		}
		return fmt.Sprintf("%s/node_modules is missing — run `npm ci` in %s (the deps-%s row does)", fe.Path, fe.Path, fe.Name)
	}
}

// extras turns forge.yaml's ci.gate.extra_checks into checks.
func (p planner) extras(owned []Check) ([]Check, error) {
	if p.Cfg == nil {
		return nil, nil
	}
	taken := map[string]bool{}
	for _, c := range owned {
		taken[c.Name] = true
	}
	var out []Check
	for i, e := range p.Cfg.CI.Gate.ExtraChecks {
		where := fmt.Sprintf("ci.gate.extra_checks[%d]", i)
		switch {
		case strings.TrimSpace(e.Name) == "":
			return nil, fmt.Errorf("%s: name is required (it is the row in the summary and what CI_SKIP matches)", where)
		case len(e.Command) == 0:
			return nil, fmt.Errorf("%s (%s): command is required", where, e.Name)
		case taken[e.Name]:
			return nil, fmt.Errorf("%s: %q is already a check name — pick another (forge's own checks: `forge ci gate --list`)", where, e.Name)
		}
		taken[e.Name] = true
		e := e
		out = append(out, Check{
			Name:   e.Name,
			Lane:   LaneChecks,
			Argv:   e.Command,
			Dir:    e.Dir,
			Prereq: needBinaries(e.Requires),
		})
	}
	return out, nil
}

func needBinary(bin string) func() string { return needBinaries([]string{bin}) }

func needBinaries(bins []string) func() string {
	return func() string {
		for _, b := range bins {
			if _, err := exec.LookPath(b); err != nil {
				return b + " is not on PATH"
			}
		}
		return ""
	}
}

func dirExists(p string) bool {
	fi, err := os.Stat(p)
	return err == nil && fi.IsDir()
}

var integrationTag = regexp.MustCompile(`(?m)^//go:build\s.*\bintegration\b`)

// HasIntegrationTests reports whether any Go test file under root carries the
// `integration` build tag. The scaffolded workflow asks the same question in
// its `changes` job (git grep for the same constraint) to decide whether the
// integration-test job runs at all.
func HasIntegrationTests(root string) bool {
	found := false
	_ = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil || found {
			return filepath.SkipDir
		}
		if d.IsDir() {
			switch d.Name() {
			case ".git", "node_modules", ".next", "vendor", ".forge":
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, "_test.go") {
			return nil
		}
		// The constraint must precede the package clause, so the head of the
		// file is enough.
		f, err := os.Open(path)
		if err != nil {
			return nil
		}
		defer func() { _ = f.Close() }()
		buf := make([]byte, 2048)
		n, _ := f.Read(buf)
		if integrationTag.Match(buf[:n]) {
			found = true
		}
		return nil
	})
	return found
}

// cleanTree is verify-generated's prerequisite: it regenerates the tree and
// attributes every change to `forge generate`, which is only true when it
// starts from the committed tree.
func (p planner) cleanTree() string {
	if _, err := exec.LookPath("git"); err != nil {
		return "git is not on PATH"
	}
	cmd := exec.Command("git", "status", "--porcelain")
	cmd.Dir = p.Root
	out, err := cmd.Output()
	if err != nil {
		return "not a git work tree, so drift cannot be attributed to `forge generate`"
	}
	if n := len(strings.Fields(strings.ReplaceAll(string(out), "\n", " "))); n > 0 {
		lines := strings.Count(strings.TrimSpace(string(out)), "\n") + 1
		return fmt.Sprintf("working tree has %d uncommitted path(s): verify-generated attributes every change to `forge generate`, so it needs the committed tree — commit or stash, or name it in %s", lines, SkipEnv)
	}
	return ""
}

// resetTree restores the committed tree after verify-generated failed, so the
// rows after it judge the committed code and not the regenerated copy. Safe
// only because cleanTree proved the tree was clean before the row ran.
func (p planner) resetTree(out io.Writer) {
	for _, argv := range [][]string{{"git", "checkout", "--", "."}, {"git", "clean", "-fd"}} {
		cmd := exec.Command(argv[0], argv[1:]...)
		cmd.Dir = p.Root
		if b, err := cmd.CombinedOutput(); err != nil {
			fmt.Fprintf(out, "could not reset the tree after verify-generated (%s): %v\n%s", strings.Join(argv, " "), err, b)
			return
		}
	}
	fmt.Fprintln(out, "reset the working tree to the committed state after verify-generated drift")
}
