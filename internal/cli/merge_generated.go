package cli

// A git merge driver for forge-generated files.
//
// WHY. Some generated files carry a line whose value is a fingerprint of
// OTHER files: the `forge:hash=` header every generated file starts with, the
// mock fixtures' SEED_FINGERPRINT (a digest of every migration), and
// gen/forge_descriptor.json's source_hash (a digest of every .proto). Any two
// branches that each change a migration, a proto, or a generated file's body
// change that ONE line differently, so git reports a conflict there even when
// the real changes are nowhere near each other. On 2026-10-07 these were 2 of
// the 4 real conflicts in control-plane's stale checkout, and every forge pin
// PR conflicted with every other open branch.
//
// The fingerprint lines are not content anyone resolves by hand: they are
// recomputed by the next `forge generate`, and forge's own freshness checks
// (and CI's verify-generated) fail until that happens. So the driver merges
// everything ELSE with git's normal three-way merge, and takes the fingerprint
// lines from the current side. A real conflict in the body is still a
// conflict; a merge that touched only fingerprints is clean, and the stale
// fingerprint is exactly what `forge generate` refreshes.
//
// Beyond fingerprints, the driver settles the two kinds of file every branch
// that adds a service changes (P3b friction F16, 33 parallel lanes):
//   - forge's committed state files (.forge/scaffolded.json, hashes.json,
//     disowned.json) merge key by key (checksums.MergeStateFile), so two
//     lanes' births and hashes never conflict;
//   - a forge render both sides changed whose line merge still conflicts —
//     the service mount list — is not content a person resolves either.
//     When both sides are forge's own renders, the driver keeps this side's
//     (still a verifiable render) and the next `forge generate` renders the
//     merged inputs over it. A side carrying a hand edit keeps the conflict,
//     so nothing human is dropped. Outputs that need no such fallback are
//     written one file per service instead (the observability spec, its
//     saved searches and alert inputs: obsdash.ServicePart), which merges
//     into the render itself.
//
// Wiring is per clone, like core.hooksPath, and needs nothing committed:
//   - every forge command run in the project (re)writes the driver's command
//     into .git/config and the attributes that route forge's files to it into
//     the clone's info/attributes (ensureGeneratedMergeDriver). A project
//     adopted in place, which never got forge's .gitattributes, merges
//     through the driver all the same. FORGE_NO_MERGE_DRIVER=1 opts out of
//     the driver alone; FORGE_NO_GIT_HOOKS=1 (or FORGE_NO_HOOKS) keeps forge
//     out of the clone's git configuration altogether, driver included.
//   - a new project's .gitattributes also names the driver, so the routing
//     is visible in the tree.
// A clone where forge has not run yet gets git's built-in merge, which is the
// old behaviour — never worse.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"github.com/spf13/cobra"

	"github.com/reliant-labs/forge/internal/checksums"
	"github.com/reliant-labs/forge/internal/cli/cmdutil"
)

// generatedMergeDriver is the driver's name in .gitattributes and .git/config.
const generatedMergeDriver = "forge-generated"

// volatileLine matches a generated line whose value is a fingerprint of other
// files. Group 1 is the line's KEY: two lines with the same key are the same
// fact, whichever value they carry.
var volatileLine = regexp.MustCompile(`^(\s*(?://|#|<!--|/?\*)?\s*forge:hash=|export const SEED_FINGERPRINT\s*=|export const SEED_CONFIG_FINGERPRINT\s*=|export const SEED_FINGERPRINT_FILES\s*=|\s*"source_hash"\s*:)`)

// volatilePlaceholder replaces a fingerprint's value while the rest merges.
const volatilePlaceholder = "__FORGE_MERGE_GENERATED_VOLATILE__"

// neutralize replaces every fingerprint line's value with the placeholder and
// returns the original lines by key.
func neutralize(text []byte) ([]byte, map[string]string) {
	lines := strings.SplitAfter(string(text), "\n")
	originals := map[string]string{}
	for i, line := range lines {
		m := volatileLine.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		key := m[1]
		if _, seen := originals[key]; !seen {
			originals[key] = line
		}
		nl := ""
		if strings.HasSuffix(line, "\n") {
			nl = "\n"
		}
		lines[i] = key + volatilePlaceholder + nl
	}
	return []byte(strings.Join(lines, "")), originals
}

// restore puts fingerprint lines back: ours where ours has the key, else
// theirs. A placeholder line whose key neither side has cannot happen (the
// placeholder only exists for keys a side had), but is kept verbatim if it does.
func restore(merged []byte, ours, theirs map[string]string) []byte {
	lines := strings.SplitAfter(string(merged), "\n")
	for i, line := range lines {
		j := strings.Index(line, volatilePlaceholder)
		if j < 0 {
			continue
		}
		key := line[:j]
		switch {
		case ours[key] != "":
			lines[i] = ours[key]
		case theirs[key] != "":
			lines[i] = theirs[key]
		}
	}
	return []byte(strings.Join(lines, ""))
}

// mergeGenerated three-way merges a generated file with its fingerprint lines
// taken out of the comparison. clean is false when the rest of the file
// conflicts; merged then carries git's conflict markers.
func mergeGenerated(ctx context.Context, base, ours, theirs []byte, markerSize int) (merged []byte, clean bool, err error) {
	nBase, _ := neutralize(base)
	nOurs, ourLines := neutralize(ours)
	nTheirs, theirLines := neutralize(theirs)

	dir, err := os.MkdirTemp("", "forge-merge-generated-")
	if err != nil {
		return nil, false, err
	}
	defer func() { _ = os.RemoveAll(dir) }()
	paths := map[string][]byte{"ours": nOurs, "base": nBase, "theirs": nTheirs}
	for name, body := range paths {
		if err := os.WriteFile(filepath.Join(dir, name), body, 0o600); err != nil {
			return nil, false, err
		}
	}
	if markerSize <= 0 {
		markerSize = 7
	}
	cmd := exec.CommandContext(ctx, "git", "merge-file", "-p", "--marker-size="+strconv.Itoa(markerSize),
		"-L", "ours", "-L", "base", "-L", "theirs",
		filepath.Join(dir, "ours"), filepath.Join(dir, "base"), filepath.Join(dir, "theirs"))
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, runErr := cmd.Output()
	var exitErr *exec.ExitError
	switch {
	case runErr == nil:
		clean = true
	case errors.As(runErr, &exitErr) && exitErr.ExitCode() > 0 && exitErr.ExitCode() < 128:
		// git merge-file exits with the number of conflicts.
		clean = false
	default:
		return nil, false, fmt.Errorf("git merge-file: %v: %s", runErr, strings.TrimSpace(stderr.String()))
	}
	merged = restore(out, ourLines, theirLines)
	if clean && checksums.Verify(ours) == checksums.Pristine && checksums.Verify(theirs) == checksums.Pristine {
		merged = recertify(merged)
	}
	return merged, clean, nil
}

// recertify rewrites the forge:hash marker to certify the merged body.
//
// Ours' marker certifies ours' body, not the merge, so kept as is the merged
// file verifies Modified and the next `forge generate` refuses it as a
// hand-edit. A clean merge of two PRISTINE renders holds no human edit, so it
// is certified as a render of some vintage, which generate overwrites with
// the current render. mergeGenerated only calls this when both sides verify:
// a hand-edited side keeps ours' marker, so its edit stays Modified and the
// stomp guard still asks before anything discards it.
func recertify(merged []byte) []byte {
	old, ok := checksums.ExtractMarker(merged)
	if !ok {
		return merged
	}
	key := "forge:hash="
	return bytes.Replace(merged, []byte(key+old), []byte(key+checksums.BodyHash(merged)), 1)
}

// mergeGeneratedPath merges one file git routed to the driver. relPath is
// git's %P (repository-relative), empty when an older driver command did not
// pass it. note, when set, says what the driver decided for the reader of
// the merge output.
func mergeGeneratedPath(ctx context.Context, relPath string, base, ours, theirs []byte, markerSize int) (merged []byte, clean bool, note string, err error) {
	if relPath != "" {
		if m, ok := checksums.MergeStateFile(relPath, base, ours, theirs); ok {
			return m, true, "", nil
		}
	}
	merged, clean, err = mergeGenerated(ctx, base, ours, theirs, markerSize)
	if err != nil {
		return nil, false, "", err
	}
	keep := "kept this side's render; run `forge generate` to render both sides' inputs into it"
	switch {
	case relPath != "" && !checksums.Stampable(relPath):
		// A comment-incapable output (JSON) is certified by
		// .forge/hashes.json, which keeps THIS side's hash where both sides
		// re-rendered (MergeStateFile). A merged body — clean or not —
		// would match no recorded hash, and generate would refuse it as a
		// hand edit. So when this side is forge's verified render, keep it.
		if unstampableRenderOfOurs(relPath, ours) {
			return ours, true, keep, nil
		}
		// The descriptor is rewritten whole on every generate and certified
		// by nothing: a clean merge stands, a conflict takes this side.
		if path.Base(relPath) == "forge_descriptor.json" && !clean {
			return ours, true, keep, nil
		}
	case !clean && checksums.Verify(ours) == checksums.Pristine && checksums.Verify(theirs) == checksums.Pristine:
		// Two pristine renders still conflicting: both are pure functions
		// of inputs that merged, so the conflict holds no human content.
		return ours, true, keep, nil
	}
	return merged, clean, "", nil
}

// unstampableRenderOfOurs reports whether ours is forge's render of a
// comment-incapable output: its body hash is the one THIS side's
// .forge/hashes.json records for it. The project root is the nearest
// directory above relPath holding forge.yaml (the driver runs at the top of
// the work tree, and relPath is relative to it); this side's hashes come
// from HEAD, which is this side in a merge, a rebase and a cherry-pick alike.
func unstampableRenderOfOurs(relPath string, ours []byte) bool {
	rel := filepath.ToSlash(relPath)
	for dir := path.Dir(rel); ; dir = path.Dir(dir) {
		prefix := ""
		if dir != "." {
			prefix = dir + "/"
		}
		if _, err := os.Stat(filepath.FromSlash(prefix + "forge.yaml")); err == nil {
			data, err := exec.CommandContext(context.Background(), "git", "cat-file", "blob", "HEAD:"+prefix+checksums.HashesFile).Output()
			if err != nil {
				return false
			}
			var h struct {
				Files map[string]string `json:"files"`
			}
			if json.Unmarshal(data, &h) != nil {
				return false
			}
			recorded, ok := h.Files[strings.TrimPrefix(rel, prefix)]
			return ok && recorded == checksums.BodyHash(ours)
		}
		if dir == "." || dir == "/" {
			return false
		}
	}
}

// newMergeGeneratedCmd is the driver git invokes:
//
//	forge merge-generated %O %A %B %L %P
//
// It writes the result into %A, as git's driver contract requires, and exits
// non-zero on a conflict. %P (the path being merged) is optional, so a
// driver command an older forge wrote keeps working.
func newMergeGeneratedCmd() *cobra.Command {
	return &cobra.Command{
		// git runs this mid-merge: no hook activation, no other side effects.
		Annotations: map[string]string{skipHookActivationAnnotation: ""},
		Use:         "merge-generated <base> <ours> <theirs> [marker-size] [path]",
		Short:       "git merge driver for forge-generated files (configured by forge; not run by hand)",
		Hidden:      true,
		Args:        cobra.RangeArgs(3, 5),
		// A merge driver's stdout/stderr land in the middle of git's own
		// output; nothing here should print a usage block.
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			base, err := os.ReadFile(args[0])
			if err != nil {
				return err
			}
			ours, err := os.ReadFile(args[1])
			if err != nil {
				return err
			}
			theirs, err := os.ReadFile(args[2])
			if err != nil {
				return err
			}
			marker := 7
			if len(args) >= 4 {
				if n, perr := strconv.Atoi(args[3]); perr == nil {
					marker = n
				}
			}
			relPath := ""
			if len(args) == 5 {
				relPath = args[4]
			}
			// Only the forge the project pins merges by its rules; see
			// merge_driver_pin.go.
			if handled, clean, err := pinnedMerge(cmd.Context(), cmd.ErrOrStderr(), args, relPath, base, ours, theirs, marker); handled {
				if err != nil {
					return err
				}
				if !clean {
					return exitCodeError{code: 1, msg: "conflicts remain"}
				}
				return nil
			}
			merged, clean, note, err := mergeGeneratedPath(cmd.Context(), relPath, base, ours, theirs, marker)
			if err != nil {
				return err
			}
			if err := os.WriteFile(args[1], merged, 0o644); err != nil {
				return err
			}
			if note != "" {
				fmt.Fprintf(cmd.ErrOrStderr(), "forge merge-generated %s: %s\n", relPath, note)
			}
			if !clean {
				return exitCodeError{code: 1, msg: "conflicts outside forge's fingerprint lines remain"}
			}
			return nil
		},
	}
}

// ensureGeneratedMergeDriver configures the clone to merge forge's files
// through the driver: the driver command in the repository config, and the
// attributes routing the project's generated and state files to it in the
// clone's info/attributes. Both are per clone and shared by every linked
// worktree, like core.hooksPath. Best-effort and idempotent; it says one line
// when it changes something.
//
// The attributes live in info/attributes, not .gitattributes, because they
// must reach every forge project: one adopted in place keeps its own
// .gitattributes (reliant had none), and forge does not rewrite a file the
// project owns. They are scoped to the project's directory, so a forge
// project inside a larger repository routes only its own files.
//
// The command names this binary by absolute path. A bare name resolves on
// PATH at merge time, which is either nothing (a pinned forge run by path)
// or a different forge. An ephemeral binary (`go run`, a test's build,
// anything under the temp directory) is never written, because it is gone by
// the next merge. The forge the project pins writes itself over any other
// driver, and records itself under its identity so a driver from another pin
// can hand it the merge; a forge that is NOT the pin only fills in a missing
// or unusable command (merge_driver_pin.go).
//
// FORGE_NO_GIT_HOOKS / FORGE_NO_HOOKS keep forge out of the clone's git
// configuration altogether (a test suite running forge against a developer's
// checkout sets it); FORGE_NO_MERGE_DRIVER skips only the driver.
func ensureGeneratedMergeDriver(root string) {
	if gitHooksOptOut() != "" || os.Getenv(mergeDriverOptOutEnv) != "" {
		return
	}
	// Any git work tree, including one where the project sits in a
	// subdirectory of the repository.
	if exec.CommandContext(context.Background(), "git", "-C", root, "rev-parse", "--is-inside-work-tree").Run() != nil {
		return
	}
	// The routes are harmless without a command (git merges a file whose
	// driver is undefined as text), so they are written even when this
	// binary cannot be the command.
	changed := writeMergeAttributes(root)
	key := "merge." + generatedMergeDriver + ".driver"
	cur, _ := exec.CommandContext(context.Background(), "git", "-C", root, "config", "--get", key).Output()
	current := strings.TrimSpace(string(cur))
	command, ok := mergeDriverCommand()
	// A forge that is not the project's pin never installs itself, and an
	// unusable command is replaced by whatever forge runs. The pinned forge
	// takes the driver over from any other (merge_driver_pin.go): that is
	// what repairs a driver a pin bump left behind.
	_, _, skewed := mergeDriverPinCheck(root)
	if ok && !skewed {
		registerPinnedDriver(root, command)
	}
	if ok && current != command && (!driverCommandUsable(current) || !skewed) {
		_ = exec.CommandContext(context.Background(), "git", "-C", root, "config", "--local",
			"merge."+generatedMergeDriver+".name", "forge-generated files: merge, keeping fingerprint lines for forge generate to refresh").Run()
		if exec.CommandContext(context.Background(), "git", "-C", root, "config", "--local", key, command).Run() == nil {
			changed = true
		}
	}
	if changed {
		fmt.Fprintln(os.Stderr,
			"forge: configured this clone to merge forge's generated and state files through `forge merge-generated` (.git/config, info/attributes). Disable with "+mergeDriverOptOutEnv+"=1.")
	}
}

// mergeDriverOptOutEnv skips wiring the merge driver into a clone, leaving
// git hook activation alone.
const mergeDriverOptOutEnv = "FORGE_NO_MERGE_DRIVER"

// driverCommandUsable reports whether a configured driver command already
// does the job: it passes %P (so it is a forge that merges state files) and
// its binary still exists.
func driverCommandUsable(command string) bool {
	if !strings.HasSuffix(command, "%P") {
		return false
	}
	bin := command
	if strings.HasPrefix(bin, "'") {
		end := strings.Index(bin[1:], "'")
		if end < 0 {
			return false
		}
		bin = bin[1 : end+1]
	} else if i := strings.IndexByte(bin, ' '); i >= 0 {
		bin = bin[:i]
	}
	info, err := os.Stat(bin)
	return err == nil && !info.IsDir()
}

// ephemeralExecutable reports whether exe will be gone by the next merge: a
// `go test` binary, a `go run` build, or anything under the temp directory.
func ephemeralExecutable(exe string) bool {
	if strings.HasSuffix(exe, ".test") || strings.Contains(exe, string(filepath.Separator)+"go-build") {
		return true
	}
	tmp, err := filepath.EvalSymlinks(os.TempDir())
	if err != nil {
		tmp = os.TempDir()
	}
	rel, err := filepath.Rel(tmp, exe)
	return err == nil && !strings.HasPrefix(rel, "..")
}

// mergeDriverCommand is the driver command line for this binary: its
// absolute path plus the route that reaches merge-generated (`forge`, or
// `reliant forge` when forge is mounted as a subcommand). ok is false for an
// ephemeral binary, which must never become a repository's merge driver.
func mergeDriverCommand() (string, bool) {
	exe, err := mergeDriverExecutable()
	if err != nil {
		return "", false
	}
	if resolved, rerr := filepath.EvalSymlinks(exe); rerr == nil {
		exe = resolved
	}
	if ephemeralExecutable(exe) {
		return "", false
	}
	words := strings.Fields(cmdutil.Name())
	words[0] = "'" + strings.ReplaceAll(exe, "'", `'\''`) + "'"
	return strings.Join(words, " ") + " merge-generated %O %A %B %L %P", true
}

// mergeDriverExecutable is os.Executable, a seam for tests: an ephemeral
// binary is refused as a driver, so a test that exercises the wiring names
// the binary it means.
var mergeDriverExecutable = os.Executable

// mergeAttributesBegin and mergeAttributesEnd bracket forge's block in
// info/attributes, so it is replaced in place and nothing else is touched.
const (
	mergeAttributesBegin = "# >>> forge merge-generated (managed by forge; FORGE_NO_MERGE_DRIVER=1 stops it)"
	mergeAttributesEnd   = "# <<< forge merge-generated"
)

// generatedMergePatterns are the files routed to the driver, relative to
// the project root: forge's generated outputs and its committed state.
var generatedMergePatterns = []string{
	"*_gen.go", "*_gen.ts", "*_gen.test.ts", "*_gen.json", "*_gen.ndjson",
	"forge_descriptor.json",
	".forge/scaffolded.json", ".forge/hashes.json", ".forge/disowned.json",
}

// writeMergeAttributes writes forge's block into the clone's
// info/attributes, scoped to root's directory within the repository.
// Reports whether the file changed.
func writeMergeAttributes(root string) bool {
	attrPath, err := exec.CommandContext(context.Background(), "git", "-C", root, "rev-parse", "--path-format=absolute", "--git-path", "info/attributes").Output()
	if err != nil {
		return false
	}
	prefix, err := exec.CommandContext(context.Background(), "git", "-C", root, "rev-parse", "--show-prefix").Output()
	if err != nil {
		return false
	}
	block := mergeAttributesBlock(strings.TrimSpace(string(prefix)))
	file := strings.TrimSpace(string(attrPath))
	existing, _ := os.ReadFile(file)
	updated := replaceManagedBlock(string(existing), block)
	if updated == string(existing) {
		return false
	}
	if err := os.MkdirAll(filepath.Dir(file), 0o755); err != nil {
		return false
	}
	return os.WriteFile(file, []byte(updated), 0o644) == nil
}

// mergeAttributesBlock renders the block for a project at prefix (git's
// --show-prefix: "" at the repository root, else "dir/sub/").
func mergeAttributesBlock(prefix string) string {
	var b strings.Builder
	b.WriteString(mergeAttributesBegin + "\n")
	for _, p := range generatedMergePatterns {
		switch {
		case prefix == "":
			// A pattern with no slash matches at any depth; one with a
			// slash is anchored to the repository root.
			b.WriteString(p)
		case strings.Contains(p, "/"):
			b.WriteString(prefix + p)
		default:
			b.WriteString(prefix + "**/" + p)
		}
		b.WriteString(" merge=" + generatedMergeDriver + "\n")
	}
	b.WriteString(mergeAttributesEnd + "\n")
	return b.String()
}

// replaceManagedBlock swaps forge's block in content for block, appending
// it when there is none.
func replaceManagedBlock(content, block string) string {
	start := strings.Index(content, mergeAttributesBegin)
	if start >= 0 {
		if end := strings.Index(content[start:], mergeAttributesEnd); end >= 0 {
			end += start + len(mergeAttributesEnd)
			if end < len(content) && content[end] == '\n' {
				end++
			}
			return content[:start] + block + content[end:]
		}
	}
	if content != "" && !strings.HasSuffix(content, "\n") {
		content += "\n"
	}
	return content + block
}
