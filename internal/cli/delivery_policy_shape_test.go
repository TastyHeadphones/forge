package cli

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/reliant-labs/forge/internal/kclplugin"
	"github.com/reliant-labs/forge/internal/kclrender"
	"github.com/reliant-labs/forge/pkg/release"
)

// renderDeliveryPolicy renders a one-workload env with the given
// `delivery_policy = ...` expression ("" for none) through the same seam
// `forge env shape` uses, and returns the decoded entities.
func renderDeliveryPolicy(t *testing.T, policy string) *KCLEntities {
	t.Helper()
	if testing.Short() {
		t.Skip("-short: renders KCL; runs in task test")
	}
	dir := t.TempDir()
	decl := ""
	if policy != "" {
		decl = "delivery_policy = " + policy
	}
	files := map[string]string{
		"kcl.mod": "[package]\nname = \"deliverypolicy\"\nedition = \"v0.11.0\"\nversion = \"0.0.1\"\n",
		"main.k": `import forge
import forge.workloads as fw

_k3d = forge.ClusterTarget {cluster = "k3d-demo", namespace = "demo-dev"}

output = forge.render(forge.Bundle {
    project = "demo"
    ` + decl + `
    workloads = [fw.Workload {name = "api", image = "ghcr.io/acme/api", runtime = forge.OnCluster {target = _k3d}}]
})
`,
	}
	for f, c := range files {
		if err := os.WriteFile(filepath.Join(dir, f), []byte(c), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	kclplugin.Register()
	out, err := kclrender.Run(dir, dir, nil)
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	e, err := parseKCLEntities(out)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	return e
}

// forge.DeliveryPolicy {} renders every control at the default Go records,
// field for field. The two halves hold the defaults separately (kcl/schema.k
// and release.DefaultDeliveryPolicy); this is what keeps them from drifting.
func TestDeliveryPolicyDefaultsAgreeWithKCL(t *testing.T) {
	e := renderDeliveryPolicy(t, "forge.DeliveryPolicy {}")
	if e.DeliveryPolicy == nil {
		t.Fatal("an env that declares a delivery_policy rendered none")
	}
	if want := release.DefaultDeliveryPolicy(); !reflect.DeepEqual(*e.DeliveryPolicy, want) {
		t.Errorf("KCL and Go disagree on the defaults:\n kcl %+v\n go  %+v", *e.DeliveryPolicy, want)
	}
	if got := shapeDeliveryPolicyOf(e); got != nil {
		t.Errorf("an all-default policy must not be recorded in the shape, got %+v", got)
	}
}

func TestDeliveryPolicyAbsentRendersNoPolicyAndRecordsNone(t *testing.T) {
	e := renderDeliveryPolicy(t, "")
	if e.DeliveryPolicy != nil {
		t.Errorf("an env with no delivery_policy rendered %+v", e.DeliveryPolicy)
	}
	if shapeDeliveryPolicyOf(e) != nil {
		t.Error("an env with no delivery_policy recorded one in its shape")
	}
}

// A hardened prod, end to end: KCL -> render contract -> shape field, sorted
// and complete.
func TestDeliveryPolicyReachesTheShape(t *testing.T) {
	e := renderDeliveryPolicy(t, `forge.DeliveryPolicy {
        trusted_builder = forge.TrustedBuilder {mode = "enforce", builders = ["github:acme/api", "cloudbuild:acme/delivery"]}
        admission_verify = forge.Control {mode = "enforce"}
        signed_promotions = forge.Control {mode = "enforce"}
        vuln_gate = forge.VulnPolicy {
            mode = "enforce"
            exemptions = [
                forge.VulnExemption {id = "GHSA-aaaa-bbbb-cccc", reason = "unreachable", expires = "2027-01-31"}
                forge.VulnExemption {id = "CVE-2026-0001", reason = "fixed next base image", expires = "2026-12-01"}
            ]
        }
        approvals = forge.Approvals {mode = "enforce"}
    }`)
	got := shapeDeliveryPolicyOf(e)
	if got == nil {
		t.Fatal("a non-default policy was not recorded")
	}
	want := release.DefaultDeliveryPolicy()
	want.TrustedBuilder = release.ShapeTrustedBuilder{Mode: release.DeliveryEnforce, Builders: []string{"cloudbuild:acme/delivery", "github:acme/api"}}
	want.AdmissionVerify.Mode = release.DeliveryEnforce
	want.SignedPromotions.Mode = release.DeliveryEnforce
	want.VulnGate.Mode = release.DeliveryEnforce
	want.VulnGate.Exemptions = []release.ShapeVulnExempt{
		{ID: "CVE-2026-0001", Reason: "fixed next base image", Expires: "2026-12-01"},
		{ID: "GHSA-aaaa-bbbb-cccc", Reason: "unreachable", Expires: "2027-01-31"},
	}
	want.Approvals.Mode = release.DeliveryEnforce
	if !reflect.DeepEqual(*got, want) {
		t.Errorf("shape policy:\n got  %+v\n want %+v", *got, want)
	}
	if err := (release.Shape{Kind: release.EnvSelfManaged, DeliveryPolicy: got}).Canonical().Validate(); err != nil {
		t.Errorf("a policy KCL accepted must satisfy the shape's validation: %v", err)
	}
}

func TestShapeDeliveryPolicyOf(t *testing.T) {
	def := release.DefaultDeliveryPolicy()
	off := def
	off.VulnGate.Mode = release.DeliveryOff
	for name, tc := range map[string]struct {
		in   *KCLEntities
		want *release.ShapeDeliveryPolicy
	}{
		"no entities": {nil, nil},
		"no policy":   {&KCLEntities{}, nil},
		"defaults":    {&KCLEntities{DeliveryPolicy: &def}, nil},
		"one value":   {&KCLEntities{DeliveryPolicy: &off}, &off},
	} {
		if got := shapeDeliveryPolicyOf(tc.in); !reflect.DeepEqual(got, tc.want) {
			t.Errorf("%s: got %+v want %+v", name, got, tc.want)
		}
	}
}
