// Package cigate is the local CI gate: the one definition of what "CI passes"
// means for a forge project, runnable on a laptop and from a workflow.
//
// A scaffolded workflow is a thin wrapper that prepares the environment
// (checkout, toolchains, caches) and calls `task ci:checks`, `task test` and
// `task test:integration`. `task ci` runs all three locally, so "CI did not
// run" never has to mean "unverified". This package is what makes the first and
// last of those honest, with four rules:
//
//  1. Every check runs, even after one fails, so one pass shows every failure.
//     The verdict is a summary table, and the gate exits non-zero if any row
//     failed.
//  2. A check that could not run (a missing tool or prerequisite) is SKIPPED,
//     never PASS, and fails the gate unless CI_SKIP names it. A silent skip is
//     false confidence. Rows a project's own configuration or kind makes moot
//     are N/A, which is not a skip and does not fail the gate.
//  3. Tools run at the versions CI pins (tools.go), not whatever is on PATH.
//  4. Under GitHub Actions the table is appended to $GITHUB_STEP_SUMMARY.
package cigate

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// Status is one row's verdict.
type Status string

const (
	Pass Status = "PASS"
	Fail Status = "FAIL"
	// Skip: the check did not run. Fails the gate unless Allowed.
	Skip Status = "SKIPPED"
	// NA: this project has nothing for the check to look at.
	NA Status = "N/A"
)

// Lane is a group of checks a CI job runs together.
type Lane string

const (
	// LaneChecks is the static and build lane: the `checks` CI job.
	LaneChecks Lane = "checks"
	// LaneTest is the unit-test lane: `task test`, the `test` CI job.
	LaneTest Lane = "test"
	// LaneIntegration is `task test:integration`, the `integration-test` job.
	LaneIntegration Lane = "integration"
)

// SkipEnv names the checks a caller has decided not to run, comma-separated.
const SkipEnv = "CI_SKIP"

// Check is one row of the gate.
type Check struct {
	Name string
	Lane Lane

	// NotApplicable, when set, reports the row as N/A with this reason and
	// runs nothing.
	NotApplicable string
	// Done returns a non-empty note when the check's work is already done
	// (dependencies installed); the row is PASS and nothing runs.
	Done func() string
	// Prereq returns a non-empty reason when the check cannot run here. The
	// row is then SKIPPED, which fails the gate.
	Prereq func() string
	// Tools are pinned tools the command needs. Each is resolved to its
	// pinned version before the command runs, and its directory is put first
	// on the command's PATH.
	Tools []Tool

	Argv []string
	// Dir is project-relative; empty is the project root.
	Dir string
	Env []string

	// OnFail runs after the command failed, to undo what it left behind.
	OnFail func(out io.Writer)
}

// Result is the outcome of one Check.
type Result struct {
	Name     string
	Lane     Lane
	Status   Status
	Duration time.Duration
	Note     string
	// Allowed marks a SKIPPED row the caller asked for (CI_SKIP), which does
	// not fail the gate.
	Allowed bool
	// Tail is the end of a failed command's output, for the job summary.
	Tail string
}

// Report is every row of one gate run.
type Report struct {
	Lane    string
	Results []Result
}

// Failed reports whether the gate is red: any FAIL, or any SKIPPED the caller
// did not allow.
func (r Report) Failed() bool {
	for _, res := range r.Results {
		if res.Status == Fail || (res.Status == Skip && !res.Allowed) {
			return true
		}
	}
	return false
}

// Counts tallies the rows by outcome. Skipped counts only the SKIPPED rows
// that fail the gate; Allowed counts the ones CI_SKIP named.
func (r Report) Counts() (passed, failed, skipped, allowed, na int) {
	for _, res := range r.Results {
		switch {
		case res.Status == Pass:
			passed++
		case res.Status == Fail:
			failed++
		case res.Status == Skip && res.Allowed:
			allowed++
		case res.Status == Skip:
			skipped++
		case res.Status == NA:
			na++
		}
	}
	return
}

func (r Report) verdict() string {
	passed, failed, skipped, allowed, na := r.Counts()
	parts := []string{fmt.Sprintf("%d passed", passed)}
	if failed > 0 {
		parts = append(parts, fmt.Sprintf("%d FAILED", failed))
	}
	if skipped > 0 {
		parts = append(parts, fmt.Sprintf("%d SKIPPED (not allowed)", skipped))
	}
	if allowed > 0 {
		parts = append(parts, fmt.Sprintf("%d skipped by %s", allowed, SkipEnv))
	}
	if na > 0 {
		parts = append(parts, fmt.Sprintf("%d n/a", na))
	}
	return strings.Join(parts, ", ")
}

// RenderText writes the summary table.
func (r Report) RenderText(w io.Writer) {
	width := len("CHECK")
	for _, res := range r.Results {
		if len(res.Name) > width {
			width = len(res.Name)
		}
	}
	fmt.Fprintf(w, "\nCI gate summary (%s)\n", r.Lane)
	fmt.Fprintf(w, "  %-*s  %-8s  %8s  %s\n", width, "CHECK", "RESULT", "TIME", "NOTE")
	for _, res := range r.Results {
		fmt.Fprintf(w, "  %-*s  %-8s  %8s  %s\n", width, res.Name, res.Status, formatDuration(res), res.note())
	}
	verdict := "PASS"
	if r.Failed() {
		verdict = "FAIL"
	}
	fmt.Fprintf(w, "\n%s — %s\n", verdict, r.verdict())
}

// RenderMarkdown is the same table for $GITHUB_STEP_SUMMARY.
func (r Report) RenderMarkdown(w io.Writer) {
	icon := map[Status]string{Pass: "✅", Fail: "❌", Skip: "⏭️", NA: "➖"}
	fmt.Fprintf(w, "### CI gate — %s\n\n| Check | Result | Time | Note |\n|---|---|---|---|\n", r.Lane)
	for _, res := range r.Results {
		fmt.Fprintf(w, "| `%s` | %s %s | %s | %s |\n",
			res.Name, icon[res.Status], res.Status, formatDuration(res), strings.ReplaceAll(res.note(), "|", "\\|"))
	}
	verdict := "✅ **PASS**"
	if r.Failed() {
		verdict = "❌ **FAIL**"
	}
	fmt.Fprintf(w, "\n%s — %s\n", verdict, r.verdict())
	for _, res := range r.Results {
		if res.Status == Fail && res.Tail != "" {
			fmt.Fprintf(w, "\n<details><summary><code>%s</code> output (tail)</summary>\n\n```\n%s\n```\n\n</details>\n", res.Name, res.Tail)
		}
	}
	fmt.Fprintln(w)
}

func (res Result) note() string {
	if res.Status == Skip && res.Allowed {
		return res.Note + " (allowed by " + SkipEnv + ")"
	}
	return res.Note
}

func formatDuration(res Result) string {
	if res.Status == Skip || res.Status == NA || res.Duration == 0 {
		return "-"
	}
	return res.Duration.Round(100 * time.Millisecond).String()
}

// ParseSkips splits a CI_SKIP value into check names.
func ParseSkips(v string) []string {
	var out []string
	for _, n := range strings.FieldsFunc(v, func(r rune) bool { return r == ',' || r == ' ' || r == '\n' }) {
		if n = strings.TrimSpace(n); n != "" {
			out = append(out, n)
		}
	}
	return out
}

// UnknownSkips returns the names in skips that no check carries. A typo in
// CI_SKIP would otherwise quietly skip nothing, or — worse — be read as the
// caller having accounted for a check they did not.
func UnknownSkips(skips []string, all []Check) []string {
	known := map[string]bool{}
	for _, c := range all {
		known[c.Name] = true
	}
	var bad []string
	for _, s := range skips {
		if !known[s] {
			bad = append(bad, s)
		}
	}
	sort.Strings(bad)
	return bad
}

// Options configure a Run.
type Options struct {
	// Out receives the checks' own output and the summary. Defaults to
	// os.Stdout.
	Out io.Writer
	// Skip names the checks the caller chose not to run.
	Skip []string
	// Resolver pins tools. Nil means checks that declare Tools cannot run.
	Resolver *Resolver
	// Exec runs one command. Nil uses os/exec; tests substitute.
	Exec ExecFunc
	// Root is the project root.
	Root string
	// Groups wraps each check's output in GitHub Actions ::group:: lines.
	Groups bool
	// Env is the base environment; nil is os.Environ().
	Env []string
}

// ExecFunc runs argv in dir with env, writing combined output to out.
type ExecFunc func(ctx context.Context, dir string, env []string, argv []string, out io.Writer) error

// Run executes checks in order and returns the report. It never stops early:
// a failing check is a row, not an abort.
func Run(ctx context.Context, lane string, checks []Check, opt Options) Report {
	out := opt.Out
	if out == nil {
		out = os.Stdout
	}
	execFn := opt.Exec
	if execFn == nil {
		execFn = osExec
	}
	skip := map[string]bool{}
	for _, s := range opt.Skip {
		skip[s] = true
	}
	rep := Report{Lane: lane}
	for _, c := range checks {
		res := Result{Name: c.Name, Lane: c.Lane}
		switch {
		case c.NotApplicable != "":
			res.Status, res.Note = NA, c.NotApplicable
		case skip[c.Name]:
			res.Status, res.Note, res.Allowed = Skip, "not run", true
		case ctx.Err() != nil:
			res.Status, res.Note = Skip, "gate interrupted"
		default:
			res = runOne(ctx, c, opt, execFn, out)
		}
		rep.Results = append(rep.Results, res)
	}
	return rep
}

func runOne(ctx context.Context, c Check, opt Options, execFn ExecFunc, out io.Writer) Result {
	res := Result{Name: c.Name, Lane: c.Lane}
	if c.Done != nil {
		if note := c.Done(); note != "" {
			res.Status, res.Note = Pass, note
			return res
		}
	}
	if c.Prereq != nil {
		if reason := c.Prereq(); reason != "" {
			res.Status, res.Note = Skip, reason
			fmt.Fprintf(out, "SKIP %s: %s\n", c.Name, reason)
			return res
		}
	}

	env := opt.Env
	if env == nil {
		env = os.Environ()
	}
	var dirs []string
	for _, t := range c.Tools {
		if opt.Resolver == nil {
			res.Status, res.Note = Skip, t.Binary+" is not available: no tool resolver"
			return res
		}
		dir, err := opt.Resolver.Ensure(ctx, t)
		if err != nil {
			res.Status, res.Note = Skip, fmt.Sprintf("cannot provide %s %s: %v", t.Binary, t.Version, err)
			fmt.Fprintf(out, "SKIP %s: %s\n", c.Name, res.Note)
			return res
		}
		if dir != "" {
			dirs = append(dirs, dir)
		}
	}
	env = withPathPrefix(env, dirs)
	env = append(append([]string(nil), env...), c.Env...)

	dir := opt.Root
	if c.Dir != "" {
		dir = joinDir(opt.Root, c.Dir)
	}

	if opt.Groups {
		fmt.Fprintf(out, "::group::%s\n", c.Name)
	} else {
		fmt.Fprintf(out, "\n── %s ── %s\n", c.Name, strings.Join(c.Argv, " "))
	}
	tail := &tailBuffer{max: 6000}
	start := time.Now()
	err := execFn(ctx, dir, env, c.Argv, io.MultiWriter(out, tail))
	res.Duration = time.Since(start)
	if opt.Groups {
		fmt.Fprintln(out, "::endgroup::")
	}
	if err != nil {
		res.Status, res.Note, res.Tail = Fail, failNote(err), tail.lastLines(25)
		if opt.Groups {
			fmt.Fprintf(out, "::error title=ci gate: %s::%s\n", c.Name, res.Note)
		}
		if c.OnFail != nil {
			c.OnFail(out)
		}
		return res
	}
	res.Status = Pass
	return res
}

func failNote(err error) string {
	var ee *exec.ExitError
	if asExit(err, &ee) {
		return fmt.Sprintf("exit status %d", ee.ExitCode())
	}
	return err.Error()
}

func osExec(ctx context.Context, dir string, env []string, argv []string, out io.Writer) error {
	if len(argv) == 0 {
		return fmt.Errorf("empty command")
	}
	// exec.Command resolves argv[0] against THIS process's PATH, not the PATH in
	// env, so a pinned tool's directory placed first on env's PATH would be
	// ignored and the copy at another version would run. Resolve against env.
	bin, err := lookPathIn(env, argv[0])
	if err != nil {
		return err
	}
	cmd := exec.CommandContext(ctx, bin, argv[1:]...)
	cmd.Dir = dir
	cmd.Env = env
	cmd.Stdout = out
	cmd.Stderr = out
	return cmd.Run()
}

// tailBuffer keeps the last max bytes written to it.
type tailBuffer struct {
	max int
	buf bytes.Buffer
}

func (t *tailBuffer) Write(p []byte) (int, error) {
	t.buf.Write(p)
	if over := t.buf.Len() - t.max; over > 0 {
		t.buf.Next(over)
	}
	return len(p), nil
}

func (t *tailBuffer) lastLines(n int) string {
	lines := strings.Split(strings.TrimRight(t.buf.String(), "\n"), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "\n")
}

// lookPathIn finds file on the PATH carried by env. A name with a path
// separator is used as is.
func lookPathIn(env []string, file string) (string, error) {
	if strings.ContainsRune(file, filepath.Separator) {
		return file, nil
	}
	path := ""
	for _, kv := range env {
		if strings.HasPrefix(kv, "PATH=") {
			path = strings.TrimPrefix(kv, "PATH=")
		}
	}
	for _, dir := range filepath.SplitList(path) {
		if dir == "" {
			dir = "."
		}
		cand := filepath.Join(dir, file)
		if fi, err := os.Stat(cand); err == nil && !fi.IsDir() && fi.Mode()&0o111 != 0 {
			return cand, nil
		}
		if exeSuffix() != "" {
			if fi, err := os.Stat(cand + exeSuffix()); err == nil && !fi.IsDir() {
				return cand + exeSuffix(), nil
			}
		}
	}
	return "", &exec.Error{Name: file, Err: exec.ErrNotFound}
}
