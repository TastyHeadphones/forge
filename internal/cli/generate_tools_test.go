package cli

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestEnsureGenGoMod_BootstrapsFreshWorktree covers the headline case:
// a checkout has `go.work` declaring `use gen`, a root `go.mod` with a
// module directive, but `gen/go.mod` is missing on disk (the situation a
// fresh worktree carved from a never-generated checkout lands in).
func TestEnsureGenGoMod_BootstrapsFreshWorktree(t *testing.T) {
	dir := t.TempDir()
	writeForTest(t, filepath.Join(dir, "go.mod"), "module example.com/proj\n\ngo 1.26.2\n")
	writeForTest(t, filepath.Join(dir, "go.work"), "go 1.26.2\n\nuse (\n\t.\n\tgen\n)\n")

	if err := ensureGenGoMod(dir); err != nil {
		t.Fatalf("ensureGenGoMod: %v", err)
	}

	body, err := os.ReadFile(filepath.Join(dir, "gen", "go.mod"))
	if err != nil {
		t.Fatalf("expected gen/go.mod to be created: %v", err)
	}
	if !strings.Contains(string(body), "module example.com/proj/gen") {
		t.Errorf("rendered gen/go.mod missing expected module line; got:\n%s", body)
	}
	if !strings.Contains(string(body), "go 1.26.2") {
		t.Errorf("rendered gen/go.mod missing go directive; got:\n%s", body)
	}
}

// TestEnsureGenGoMod_NoopWhenPresent confirms an existing gen/go.mod is
// left untouched.
func TestEnsureGenGoMod_NoopWhenPresent(t *testing.T) {
	dir := t.TempDir()
	writeForTest(t, filepath.Join(dir, "go.mod"), "module example.com/proj\n\ngo 1.26.2\n")
	writeForTest(t, filepath.Join(dir, "go.work"), "go 1.26.2\n\nuse (\n\t.\n\tgen\n)\n")
	writeForTest(t, filepath.Join(dir, "gen", "go.mod"), "// hand-edited\n")

	if err := ensureGenGoMod(dir); err != nil {
		t.Fatalf("ensureGenGoMod: %v", err)
	}
	body, _ := os.ReadFile(filepath.Join(dir, "gen", "go.mod"))
	if string(body) != "// hand-edited\n" {
		t.Errorf("ensureGenGoMod clobbered an existing gen/go.mod; content=%q", body)
	}
}

// TestEnsureGenGoMod_NoGoWorkSkips confirms we don't synthesize a
// gen/go.mod for projects that aren't using a workspace at all (no
// go.work file means the missing gen/ is by design).
func TestEnsureGenGoMod_NoGoWorkSkips(t *testing.T) {
	dir := t.TempDir()
	writeForTest(t, filepath.Join(dir, "go.mod"), "module example.com/proj\n\ngo 1.26.2\n")

	if err := ensureGenGoMod(dir); err != nil {
		t.Fatalf("ensureGenGoMod: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "gen", "go.mod")); !os.IsNotExist(err) {
		t.Errorf("ensureGenGoMod created gen/go.mod despite no go.work declaring the gen workspace")
	}
}

// writeForTest is a small fixture helper that ensures the parent dir
// exists before writing. The package's other mustWrite helper
// (generate_cleanup_test.go) assumes the parent exists; the gen/go.mod
// test paths need an extra os.MkdirAll, so we keep a local variant
// rather than churn the existing helper.
func writeForTest(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("MkdirAll(%s): %v", filepath.Dir(path), err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatalf("WriteFile(%s): %v", path, err)
	}
}

const genModForgeRoot = "module example.com/proj\n\ngo 1.26.2\n\nrequire github.com/reliant-labs/forge v0.1.44\n"

func readGenMod(t *testing.T, dir string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(dir, "gen", "go.mod"))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// A project scaffolded before this fix has a gen/go.mod holding the retired
// forge/pkg module (the scaffold's tidy removed the template's forge require,
// the next tidy picked the retired module). generate must migrate it to the
// root module's pin.
func TestEnsureGenForgeRequire_MigratesTheRetiredPkgModule(t *testing.T) {
	dir := t.TempDir()
	writeForTest(t, filepath.Join(dir, "go.mod"), genModForgeRoot)
	writeForTest(t, filepath.Join(dir, "gen", "go.mod"),
		"module example.com/proj/gen\n\ngo 1.26.2\n\nrequire (\n\tconnectrpc.com/connect v1.19.0\n\tgithub.com/reliant-labs/forge/pkg v0.1.15\n)\n")

	changed, err := ensureGenForgeRequire(dir)
	if err != nil || !changed {
		t.Fatalf("ensureGenForgeRequire = %v, %v; want a rewrite", changed, err)
	}
	got := readGenMod(t, dir)
	if strings.Contains(got, "forge/pkg") {
		t.Errorf("the retired forge/pkg module is still required:\n%s", got)
	}
	if !strings.Contains(got, "github.com/reliant-labs/forge v0.1.44") {
		t.Errorf("gen/go.mod does not require forge at the root pin:\n%s", got)
	}
	if !strings.Contains(got, "connectrpc.com/connect v1.19.0") {
		t.Errorf("unrelated requires were lost:\n%s", got)
	}
	if again, _ := ensureGenForgeRequire(dir); again {
		t.Errorf("a second run rewrote an already-migrated gen/go.mod")
	}
}

// A gen/go.mod with no forge require at all (what a fresh scaffold ended up
// with) gets one — gen's *.pb.go blank-import forge/pkg/forgepb.
func TestEnsureGenForgeRequire_AddsAMissingRequire(t *testing.T) {
	dir := t.TempDir()
	writeForTest(t, filepath.Join(dir, "go.mod"), genModForgeRoot)
	writeForTest(t, filepath.Join(dir, "gen", "go.mod"), "module example.com/proj/gen\n\ngo 1.26.2\n")

	if changed, err := ensureGenForgeRequire(dir); err != nil || !changed {
		t.Fatalf("ensureGenForgeRequire = %v, %v; want a rewrite", changed, err)
	}
	if got := readGenMod(t, dir); !strings.Contains(got, "github.com/reliant-labs/forge v0.1.44") {
		t.Errorf("forge require not added:\n%s", got)
	}
}

// A deliberate pin in gen/go.mod, or a project with no gen module, is left alone.
func TestEnsureGenForgeRequire_LeavesAHealthyOrAbsentModuleAlone(t *testing.T) {
	dir := t.TempDir()
	writeForTest(t, filepath.Join(dir, "go.mod"), genModForgeRoot)
	if changed, err := ensureGenForgeRequire(dir); changed || err != nil {
		t.Fatalf("no gen module: got %v, %v", changed, err)
	}
	const mine = "module example.com/proj/gen\n\ngo 1.26.2\n\nrequire github.com/reliant-labs/forge v0.1.43\n"
	writeForTest(t, filepath.Join(dir, "gen", "go.mod"), mine)
	if changed, err := ensureGenForgeRequire(dir); changed || err != nil {
		t.Fatalf("healthy gen module: got %v, %v", changed, err)
	}
	if got := readGenMod(t, dir); got != mine {
		t.Errorf("a healthy gen/go.mod was modified:\n%s", got)
	}
}

// The scaffold's tidy ran on a gen/ with no packages, which deletes every
// require the template wrote — forge included.
func TestDirHasGoFiles(t *testing.T) {
	dir := t.TempDir()
	writeForTest(t, filepath.Join(dir, "go.mod"), "module x\n")
	if dirHasGoFiles(dir) {
		t.Errorf("a module with only go.mod has no Go files")
	}
	writeForTest(t, filepath.Join(dir, "services", "a", "v1", "a.pb.go"), "package v1\n")
	if !dirHasGoFiles(dir) {
		t.Errorf("a nested .go file was not found")
	}
}

// Root cause of the retired-module pin: `forge project new` runs `go mod tidy`
// in gen/ while it holds no Go files, and tidy deletes every require the
// template wrote — forge included. The next tidy, after codegen, then resolves
// forge/pkg/forgepb to the retired forge/pkg module. The scaffold's tidy must
// leave a package-less gen/ alone.
func TestRunGoModTidy_LeavesAPackagelessGenModuleAlone(t *testing.T) {
	if testing.Short() {
		t.Skip("-short: runs a real `go mod tidy` against the module proxy; runs in task test")
	}
	dir := t.TempDir()
	writeForTest(t, filepath.Join(dir, "go.mod"), "module example.com/proj\n\ngo 1.24\n")
	const gen = "module example.com/proj/gen\n\ngo 1.24\n\nrequire github.com/reliant-labs/forge v0.1.44\n"
	writeForTest(t, filepath.Join(dir, "gen", "go.mod"), gen)

	if err := runGoModTidy(context.Background(), dir); err != nil {
		t.Fatalf("runGoModTidy: %v", err)
	}
	if got := readGenMod(t, dir); got != gen {
		t.Errorf("tidy emptied a package-less gen/go.mod (the scaffold's forge require is the one that later gets replaced by the retired forge/pkg module):\n%s", got)
	}
}
