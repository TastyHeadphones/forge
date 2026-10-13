package cli

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/reliant-labs/forge/internal/checksums"
	"github.com/reliant-labs/forge/internal/obsdash"
)

// Two branches that each add a service change the same forge files: the
// birth ledger, the JSON outputs' hashes, the aggregated dashboard spec,
// alert defaults and saved searches, and a stamped aggregate like the
// service mount list. Before the driver understood them, every pair of
// such branches conflicted in all of them (P3b friction F16). These tests
// run a real `git merge` / `git rebase` with this test binary as the
// driver (TestMain dispatches `merge-generated` to the production
// command), over files rendered by forge's own writers.

// renderServices writes what a generate over these services writes to the
// shared files, through forge's own writers.
func renderServices(t *testing.T, dir string, services ...string) {
	t.Helper()
	checksums.ResetScaffoldLedgerCache()
	cs, err := checksums.Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	in := obsdash.Input{Project: "app"}
	var mounts strings.Builder
	for _, s := range services {
		in.Services = append(in.Services, obsdash.Service{
			Name: s, Package: "app.v1", ProtoService: strings.ToUpper(s[:1]) + s[1:] + "Service",
			Methods: []obsdash.Method{{Name: "Ping"}},
		})
		mounts.WriteString("\t\"" + s + "\",\n")
		checksums.RecordScaffold(dir, "internal/handlers/"+s+"/handlers_scaffold_ping_test.go")
	}
	if _, err := obsdash.Write(dir, in, cs); err != nil {
		t.Fatal(err)
	}
	body := "package app\n\n// Mounts lists the mounted services.\nvar Mounts = []string{\n" + mounts.String() + "}\n"
	if _, err := checksums.WriteGeneratedFile(dir, "internal/app/mounts_gen.go", []byte(body), cs, true); err != nil {
		t.Fatal(err)
	}
	if err := checksums.Save(dir, cs); err != nil {
		t.Fatal(err)
	}
	checksums.ResetScaffoldLedgerCache()
}

func gitT(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-c", "user.name=t", "-c", "user.email=t@t", "-c", "commit.gpgsign=false"}, args...)...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return string(out)
}

// twoServiceBranches builds base (system), branch "question" (+question)
// and branch "run" (+run), with the driver wired the way forge wires a
// clone, and leaves "question" checked out.
func twoServiceBranches(t *testing.T) string {
	t.Helper()
	if testing.Short() {
		t.Skip("-short: runs real git merges that exec the test binary as the driver (~10s); runs in task test")
	}
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
	dir := t.TempDir()
	gitT(t, dir, "init", "-q", "-b", "main")
	writeTreeFileT(t, filepath.Join(dir, "forge.yaml"), "name: app\nmodule_path: example.com/app\n")
	writeTreeFileT(t, filepath.Join(dir, "go.mod"), "module example.com/app\n\ngo 1.24\n")
	exe, err := filepath.Abs(os.Args[0])
	if err != nil {
		t.Fatal(err)
	}
	gitT(t, dir, "config", "merge.forge-generated.driver", "'"+exe+"' merge-generated %O %A %B %L %P")
	if !writeMergeAttributes(dir) {
		t.Fatal("precondition: the attributes block was written")
	}

	renderServices(t, dir, "system")
	gitT(t, dir, "add", "-A")
	gitT(t, dir, "commit", "-qm", "base")
	gitT(t, dir, "checkout", "-qb", "run")
	renderServices(t, dir, "run", "system")
	gitT(t, dir, "add", "-A")
	gitT(t, dir, "commit", "-qm", "run")
	gitT(t, dir, "checkout", "-q", "main")
	gitT(t, dir, "checkout", "-qb", "question")
	renderServices(t, dir, "question", "system")
	gitT(t, dir, "add", "-A")
	gitT(t, dir, "commit", "-qm", "question")
	return dir
}

// assertConvergesToBothServices checks a merged tree carries no conflict,
// and that the next generate renders both services over it without
// refusing a single file as a hand edit.
func assertConvergesToBothServices(t *testing.T, dir string) {
	t.Helper()
	for _, rel := range []string{".forge/hashes.json", ".forge/scaffolded.json", obsdash.SpecFile, obsdash.AlertDefaultsFile, obsdash.SavedSearchesFile, "internal/app/mounts_gen.go"} {
		data, err := os.ReadFile(filepath.Join(dir, rel))
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(data), "<<<<<<<") {
			t.Errorf("%s carries conflict markers:\n%s", rel, data)
		}
	}
	ledger, _ := os.ReadFile(filepath.Join(dir, checksums.ScaffoldedFile))
	for _, s := range []string{"question", "run"} {
		if !strings.Contains(string(ledger), "internal/handlers/"+s+"/") {
			t.Errorf("the merged ledger lost %s's birth:\n%s", s, ledger)
		}
	}

	// "generate": render the merged inputs. Every shared file must be one
	// forge still certifies, so it is rewritten rather than refused.
	cs, err := checksums.Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, rel := range []string{obsdash.SpecFile, obsdash.AlertDefaultsFile, obsdash.SavedSearchesFile} {
		data, _ := os.ReadFile(filepath.Join(dir, rel))
		if cs.Unstampable[rel] != checksums.BodyHash(data) {
			t.Errorf("%s is not certified by the merged hashes.json: generate would refuse it as a hand edit", rel)
		}
	}
	if mounts, _ := os.ReadFile(filepath.Join(dir, "internal/app/mounts_gen.go")); checksums.Verify(mounts) != checksums.Pristine {
		t.Errorf("mounts_gen.go must stay a pristine render:\n%s", mounts)
	}
	renderServices(t, dir, "question", "run", "system")
	fresh := t.TempDir()
	writeTreeFileT(t, filepath.Join(fresh, "go.mod"), "module example.com/app\n\ngo 1.24\n")
	renderServices(t, fresh, "question", "run", "system")
	for _, rel := range []string{obsdash.SpecFile, obsdash.AlertDefaultsFile, obsdash.SavedSearchesFile, "internal/app/mounts_gen.go"} {
		got, _ := os.ReadFile(filepath.Join(dir, rel))
		want, _ := os.ReadFile(filepath.Join(fresh, rel))
		if string(got) != string(want) {
			t.Errorf("%s after regenerating the merge differs from a fresh render of both services", rel)
		}
	}
}

func TestMergeGenerated_TwoServiceBranchesMergeClean(t *testing.T) {
	dir := twoServiceBranches(t)

	// The baseline: git's own merge, without the driver, conflicts.
	attrs := strings.TrimSpace(gitT(t, dir, "rev-parse", "--path-format=absolute", "--git-path", "info/attributes"))
	if err := os.Rename(attrs, attrs+".off"); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("git", "-c", "user.name=t", "-c", "user.email=t@t", "merge", "--no-edit", "run")
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err == nil || !strings.Contains(string(out), "CONFLICT") {
		t.Fatalf("precondition: without the driver the two branches conflict, got err=%v\n%s", err, out)
	}
	gitT(t, dir, "merge", "--abort")
	if err := os.Rename(attrs+".off", attrs); err != nil {
		t.Fatal(err)
	}

	gitT(t, dir, "merge", "--no-edit", "run")
	assertConvergesToBothServices(t, dir)
}

func TestMergeGenerated_TwoServiceBranchesRebaseClean(t *testing.T) {
	dir := twoServiceBranches(t)
	gitT(t, dir, "rebase", "run")
	assertConvergesToBothServices(t, dir)
}
