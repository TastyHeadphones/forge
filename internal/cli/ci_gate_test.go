package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/reliant-labs/forge/internal/cigate"
)

func TestGateLanes(t *testing.T) {
	for _, c := range []struct {
		args []string
		want []cigate.Lane
	}{
		{nil, []cigate.Lane{cigate.LaneChecks, cigate.LaneTest, cigate.LaneIntegration}},
		{[]string{"all"}, []cigate.Lane{cigate.LaneChecks, cigate.LaneTest, cigate.LaneIntegration}},
		{[]string{"checks"}, []cigate.Lane{cigate.LaneChecks}},
		{[]string{"test"}, []cigate.Lane{cigate.LaneTest}},
		{[]string{"integration"}, []cigate.Lane{cigate.LaneIntegration}},
	} {
		got, _, err := gateLanes(c.args)
		if err != nil || len(got) != len(c.want) {
			t.Errorf("gateLanes(%v) = %v, %v; want %v", c.args, got, err, c.want)
			continue
		}
		for i := range got {
			if got[i] != c.want[i] {
				t.Errorf("gateLanes(%v)[%d] = %s, want %s", c.args, i, got[i], c.want[i])
			}
		}
	}
	if _, _, err := gateLanes([]string{"lint"}); err == nil || !strings.Contains(err.Error(), "checks, test, integration, all") {
		t.Errorf("an unknown lane must name the valid ones, got %v", err)
	}
}

// The job summary: a table appended (not truncated) to $GITHUB_STEP_SUMMARY, so
// the gate's output sits beside whatever earlier steps wrote.
func TestAppendStepSummaryAppends(t *testing.T) {
	path := filepath.Join(t.TempDir(), "summary.md")
	if err := os.WriteFile(path, []byte("earlier step\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	rep := cigate.Report{Lane: "checks", Results: []cigate.Result{
		{Name: "go-build", Status: cigate.Pass},
		{Name: "vuln-go", Status: cigate.Skip, Note: "govulncheck is not on PATH"},
	}}
	if err := appendStepSummary(path, rep); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(path)
	got := string(b)
	for _, want := range []string{"earlier step", "### CI gate — checks", "| `go-build` | ✅ PASS", "| `vuln-go` | ⏭️ SKIPPED", "❌ **FAIL**"} {
		if !strings.Contains(got, want) {
			t.Errorf("summary missing %q:\n%s", want, got)
		}
	}
}

// A skipped check fails the gate, so `forge ci gate` is wired into `ci` as a
// verb that exists and rejects stray arguments rather than ignoring them.
func TestCIGateIsRegisteredAndStrictAboutArgs(t *testing.T) {
	root := NewRootCmd()
	cmd, _, err := root.Find([]string{"ci", "gate"})
	if err != nil || cmd == nil || cmd.Name() != "gate" {
		t.Fatalf("forge ci gate is not registered: %v", err)
	}
	var out bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&out)
	root.SetArgs([]string{"ci", "gate", "checks", "extra"})
	if err := root.Execute(); err == nil {
		t.Error("a second positional argument should be rejected")
	}
	if (gateFailure{}).Error() == "" {
		t.Error("gateFailure must say something: main prints it")
	}
}
