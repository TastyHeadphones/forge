package cli

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// The generated-file merge driver merges only as the forge the project pins.
// reliant's clone kept a driver naming a forge several pins old; a
// cherry-pick warned it was not the pinned forge, then merged with it anyway.

func pinTestRepo(t *testing.T) string {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
	repo := t.TempDir()
	if out, err := exec.Command("git", "init", "-q", repo).CombinedOutput(); err != nil {
		t.Fatalf("git init: %v\n%s", err, out)
	}
	writeTreeFileT(t, filepath.Join(repo, "svc", "forge.yaml"), "name: svc\nmodule_path: example.com/svc\n")
	for _, env := range []string{"FORGE_NO_GIT_HOOKS", "FORGE_NO_HOOKS", "FORGE_NO_MERGE_DRIVER", delegatedDriverEnv} {
		t.Setenv(env, "")
	}
	return repo
}

func setPinCheck(t *testing.T, skewed bool) {
	t.Helper()
	prev := mergeDriverPinCheck
	t.Cleanup(func() { mergeDriverPinCheck = prev })
	mergeDriverPinCheck = func(string) (string, string, bool) {
		return "v0.1.46-0.20261014193438-7410e351b23b", "v0.1.45-0.20261001000000-6f9811cb0000", skewed
	}
}

func gitConfig(t *testing.T, dir, key string) string {
	t.Helper()
	out, _ := exec.Command("git", "-C", dir, "config", "--get", key).Output()
	return strings.TrimSpace(string(out))
}

// The pinned forge repairs a driver another forge left, and records itself
// so a driver from another pin can hand it the merge.
func TestEnsureGeneratedMergeDriver_PinnedForgeRepairsAStaleDriver(t *testing.T) {
	repo := pinTestRepo(t)
	stale := filepath.Join(t.TempDir(), "old-forge")
	writeTreeFileT(t, stale, "#!/bin/sh\n")
	staleCmd := "'" + stale + "' merge-generated %O %A %B %L %P"
	if out, err := exec.Command("git", "-C", repo, "config", "merge.forge-generated.driver", staleCmd).CombinedOutput(); err != nil {
		t.Fatalf("seed: %v\n%s", err, out)
	}
	prevExe, prevID := mergeDriverExecutable, mergeDriverIdentity
	t.Cleanup(func() { mergeDriverExecutable, mergeDriverIdentity = prevExe, prevID })
	mergeDriverExecutable = func() (string, error) { return "/opt/forge/bin/forge", nil }
	mergeDriverIdentity = func() string { return "7410e351b23b" }

	setPinCheck(t, false)
	ensureGeneratedMergeDriver(filepath.Join(repo, "svc"))
	got := gitConfig(t, repo, "merge.forge-generated.driver")
	if !strings.HasPrefix(got, "'/opt/forge/bin/forge'") || !strings.HasSuffix(got, "merge-generated %O %A %B %L %P") {
		t.Errorf("the pinned forge did not take over a stale driver: %q", got)
	}
	if reg := gitConfig(t, repo, pinnedDriverSection+".7410e351b23b.command"); reg != got {
		t.Errorf("the pinned forge did not record itself: %q, want %q", reg, got)
	}

	// A forge that is not the pin leaves the pinned forge's driver alone.
	otherExe := filepath.Join(t.TempDir(), "other-forge")
	writeTreeFileT(t, otherExe, "#!/bin/sh\n")
	mergeDriverExecutable = func() (string, error) { return otherExe, nil }
	mergeDriverIdentity = func() string { return "6f9811cb0000" }
	setPinCheck(t, true)
	ensureGeneratedMergeDriver(filepath.Join(repo, "svc"))
	if again := gitConfig(t, repo, "merge.forge-generated.driver"); again != got {
		t.Errorf("a forge that is not the pin rewrote the driver: %q", again)
	}
	if reg := gitConfig(t, repo, pinnedDriverSection+".6f9811cb0000.command"); reg != "" {
		t.Errorf("a forge that is not the pin registered itself: %q", reg)
	}
}

func runMergeDriver(t *testing.T, repo, relPath, base, ours, theirs string) (merged string, err error) {
	t.Helper()
	dir := t.TempDir()
	paths := []string{filepath.Join(dir, "base"), filepath.Join(dir, "ours"), filepath.Join(dir, "theirs")}
	for i, body := range []string{base, ours, theirs} {
		if werr := os.WriteFile(paths[i], []byte(body), 0o644); werr != nil {
			t.Fatal(werr)
		}
	}
	t.Chdir(repo)
	cmd := newMergeGeneratedCmd()
	cmd.SetArgs([]string{paths[0], paths[1], paths[2], "7", relPath})
	cmd.SetOut(&strings.Builder{})
	cmd.SetErr(&strings.Builder{})
	err = cmd.Execute()
	out, rerr := os.ReadFile(paths[1])
	if rerr != nil {
		t.Fatal(rerr)
	}
	return string(out), err
}

// A driver that is not the pinned forge hands the merge to the pinned one.
func TestMergeGenerated_DelegatesToThePinnedForge(t *testing.T) {
	repo := pinTestRepo(t)
	setPinCheck(t, true)
	pinned := filepath.Join(t.TempDir(), "pinned-forge")
	writeTreeFileT(t, pinned, "#!/bin/sh\n[ \"$"+delegatedDriverEnv+"\" = 1 ] || exit 3\nprintf 'merged by the pinned forge\\n' > \"$3\"\n")
	if err := os.Chmod(pinned, 0o755); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command("git", "-C", repo, "config", pinnedDriverSection+".7410e351b23b.command",
		"'"+pinned+"' merge-generated %O %A %B %L %P").CombinedOutput(); err != nil {
		t.Fatalf("seed: %v\n%s", err, out)
	}
	merged, err := runMergeDriver(t, repo, "svc/x_gen.go", "a\n", "b\n", "c\n")
	if err != nil {
		t.Fatalf("delegated merge failed: %v", err)
	}
	if merged != "merged by the pinned forge\n" {
		t.Errorf("%%A = %q; the pinned forge did not merge", merged)
	}
}

// With no pinned forge known, a mismatched driver merges as git does without
// one: a normal conflict where the sides overlap, a clean merge where not —
// never its own rules (which would settle the fingerprint line below).
func TestMergeGenerated_WithoutThePinnedForgeMergesAsGitDoes(t *testing.T) {
	repo := pinTestRepo(t)
	setPinCheck(t, true)
	base := "// forge:hash=aaaa\nx := 1\n"
	merged, err := runMergeDriver(t, repo, "svc/x_gen.go", base, "// forge:hash=bbbb\nx := 1\n", "// forge:hash=cccc\nx := 1\n")
	if err == nil {
		t.Fatalf("a mismatched forge settled a conflict by its own rules:\n%s", merged)
	}
	if !strings.Contains(merged, "<<<<<<< ours") || !strings.Contains(merged, ">>>>>>> theirs") {
		t.Errorf("want git's conflict markers:\n%s", merged)
	}

	merged, err = runMergeDriver(t, repo, "svc/x_gen.go", "a\nb\nc\nd\ne\n", "A\nb\nc\nd\ne\n", "a\nb\nc\nd\nE\n")
	if err != nil || merged != "A\nb\nc\nd\nE\n" {
		t.Errorf("a non-overlapping change must merge cleanly: %q, %v", merged, err)
	}
}
