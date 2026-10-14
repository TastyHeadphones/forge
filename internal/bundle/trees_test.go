package bundle

// THE V2 LAYOUT, THROUGH A REAL BUILD.
//
// pkg/release tests the tree planner on abstract objects. These tests run the
// whole pipeline — parse, classify, shape, pack, validate — on a render shaped
// like control-plane prod (rollout-log session C), and read the result back the
// way a reconciler does: from the layer, with nothing of forge's in the path.

import (
	"bytes"
	"fmt"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/reliant-labs/forge/pkg/release"
)

const (
	tHub    = "gke-prod"
	tDaemon = "gke-prod-daemon"
)

type tdoc struct {
	cluster, unit, kind, name string
	annotations               string // pre-rendered `    key: value` lines
	apiVersion                string
}

func (d tdoc) yaml() string {
	api := d.apiVersion
	if api == "" {
		api = "v1"
	}
	labels := "  labels:\n    forge.dev/workload: " + d.name + "\n"
	if d.unit != "" {
		labels += "    forge.dev/delivery-unit: " + d.unit + "\n"
	}
	ann := ""
	if d.annotations != "" {
		ann = "  annotations:\n" + d.annotations
	}
	return fmt.Sprintf("# cluster: %s\napiVersion: %s\nkind: %s\nmetadata:\n  name: %s\n  namespace: control-plane-prod\n%s%s",
		d.cluster, api, d.kind, d.name, labels, ann)
}

const (
	postAnn = "    forge.dev/deploy-phase: post-rollout\n"
	preAnn  = "    forge.dev/deploy-phase: pre-rollout\n"
)

// prodRender is session C's prod, trimmed: temporal-schema before rollout,
// migrate before the apps, temporal-namespace and idp-provision after, the
// daemon substrate on its own cluster, and a prune-guarded PVC in hub-base.
func prodRender() []tdoc {
	return []tdoc{
		{tDaemon, "daemon-substrate", "StorageClass", "workspace-ssd", "", "storage.k8s.io/v1"},
		{tDaemon, "daemon-substrate", "Deployment", "cnpg-controller-manager", "", "apps/v1"},
		{tDaemon, "daemon-substrate", "DaemonSet", "kata-prepull", "", "apps/v1"},
		{tHub, "hub-base", "Namespace", "control-plane-prod", "", ""},
		{tHub, "hub-base", "PersistentVolumeClaim", "nats-jetstream", "", ""},
		{tHub, "infra", "Job", "temporal-schema-1023848b73", "", "batch/v1"},
		{tHub, "infra", "Deployment", "temporal", "", "apps/v1"},
		{tHub, "infra", "Job", "temporal-namespace-040a6cfe06", postAnn, "batch/v1"},
		{tHub, "reliant", "Deployment", "reliant-api-server", "", "apps/v1"},
		{tHub, "control-plane", "Job", "control-plane-migrate-b2e57763c4", preAnn, "batch/v1"},
		{tHub, "control-plane", "Deployment", "admin-server", "", "apps/v1"},
		{tHub, "control-plane", "Job", "control-plane-idp-provision-aa11", postAnn, "batch/v1"},
	}
}

var prodUnits = []release.DeliveryUnit{
	{Name: "daemon-substrate", After: []string{}},
	{Name: "hub-base", After: []string{"daemon-substrate"}},
	{Name: "reliant", After: []string{"hub-base"}},
	{Name: "infra", After: []string{"hub-base"}},
	{Name: "control-plane", After: []string{"hub-base"}},
}

func prodInput(docs []tdoc) BuildInput {
	in := buildFixture()
	var parts []string
	for _, d := range docs {
		parts = append(parts, d.yaml())
	}
	in.Shape = ShapeInput{
		Kind: release.EnvSelfManaged, Clusters: []string{tDaemon, tHub},
		Manifests: strings.Join(parts, "---\n"),
	}
	in.Units = prodUnits
	return in
}

func layerFiles(t *testing.T, b Bundle) []string {
	t.Helper()
	extracted := fluxExtract(t, b, release.BundleManifestsLayer)
	var names []string
	for n := range extracted {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

// treeOf strips the NNN-kind-name.yaml: the directory a Kustomization reads.
func treeOf(file string) string { return file[:strings.LastIndex(file, "/")] }

// TestV2LayoutPlacesEveryObjectInItsPhase is the golden for the layout: which
// directory each control-plane prod object lands in.
func TestV2LayoutPlacesEveryObjectInItsPhase(t *testing.T) {
	b := mustBuild(t, prodInput(prodRender()))
	got := map[string]string{} // name → tree
	for _, f := range layerFiles(t, b) {
		base := f[strings.LastIndex(f, "/")+1:]
		got[strings.TrimSuffix(base[strings.Index(base, "-")+1:], ".yaml")] = treeOf(f)
	}
	want := map[string]string{
		"storageclass-workspace-ssd":           "manifests/gke-prod-daemon/daemon-substrate/pre",
		"deployment-cnpg-controller-manager":   "manifests/gke-prod-daemon/daemon-substrate/rollout",
		"daemonset-kata-prepull":               "manifests/gke-prod-daemon/daemon-substrate/rollout",
		"namespace-control-plane-prod":         "manifests/gke-prod/hub-base/pre",
		"persistentvolumeclaim-nats-jetstream": "manifests/gke-prod/hub-base/pre",
		"job-temporal-schema-1023848b73":       "manifests/gke-prod/infra/pre",
		"deployment-temporal":                  "manifests/gke-prod/infra/rollout",
		"job-temporal-namespace-040a6cfe06":    "manifests/gke-prod/infra/post",
		"deployment-reliant-api-server":        "manifests/gke-prod/reliant/rollout",
		"job-control-plane-migrate-b2e57763c4": "manifests/gke-prod/control-plane/pre",
		"deployment-admin-server":              "manifests/gke-prod/control-plane/rollout",
		"job-control-plane-idp-provision-aa11": "manifests/gke-prod/control-plane/post",
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("layout\n got %v\nwant %v", got, want)
	}
}

// The build runs the F19 gate on its own layer; this pins that the v2 layout
// is one the gate admits, and that the document describes exactly it.
func TestV2LayerPassesTheIngestGateAndMatchesTheDocument(t *testing.T) {
	b := mustBuild(t, prodInput(prodRender()))
	layer, _ := b.Layer(release.BundleManifestsLayer)
	summary, err := release.ValidateManifestsLayer(bytes.NewReader(layer))
	if err != nil {
		t.Fatalf("the ingest gate refuses the v2 layer: %v", err)
	}
	held := map[string]int{}
	for _, o := range summary.Objects {
		if o.Unit == "" || o.Phase == "" {
			t.Errorf("%s is not in the v2 grammar", o.Path)
		}
		held[treeOf(o.Path)]++
	}
	if len(b.Doc.Trees) != len(held) {
		t.Fatalf("document publishes %d trees, layer holds %d", len(b.Doc.Trees), len(held))
	}
	for _, tr := range b.Doc.Trees {
		if held[tr.Path] != tr.Documents {
			t.Errorf("%s: document says %d, layer holds %d", tr.Path, tr.Documents, held[tr.Path])
		}
	}
	// A real cluster's v1 path is not published as a Kustomization root: only
	// its trees are. The cluster entry is kept so Documents still adds up.
	for _, c := range b.Doc.ClusterPaths {
		var sum int
		for _, tr := range b.Doc.Trees {
			if tr.Cluster == c.Cluster {
				sum += tr.Documents
			}
		}
		if sum != c.Documents {
			t.Errorf("cluster %s: %d documents, trees hold %d", c.Cluster, c.Documents, sum)
		}
	}
}

func TestV2TreesCarryTheOrderSessionCNeeded(t *testing.T) {
	b := mustBuild(t, prodInput(prodRender()))
	by := map[string]release.BundleTree{}
	for _, tr := range b.Doc.Trees {
		by[tr.Path] = tr
	}
	// migrate before the apps; the Jobs gate their own trees.
	cp := by["manifests/gke-prod/control-plane/rollout"]
	if !reflect.DeepEqual(cp.DependsOn, []string{"manifests/gke-prod/control-plane/pre"}) {
		t.Errorf("control-plane/rollout dependsOn %v", cp.DependsOn)
	}
	if pre := by["manifests/gke-prod/control-plane/pre"]; len(pre.Health) != 1 || pre.Health[0].Name != "control-plane-migrate-b2e57763c4" {
		t.Errorf("control-plane/pre is judged on %v, want the migrate Job", pre.Health)
	}
	// the hub waits for the daemon cluster, through its base
	basePre := by["manifests/gke-prod/hub-base/pre"]
	if !reflect.DeepEqual(basePre.DependsOn, []string{"manifests/gke-prod-daemon/daemon-substrate/rollout"}) {
		t.Errorf("hub-base/pre dependsOn %v, want the daemon rollout", basePre.DependsOn)
	}
	if got := by["manifests/gke-prod/infra/pre"].DependsOn; !reflect.DeepEqual(got, []string{"manifests/gke-prod/hub-base/pre"}) {
		t.Errorf("infra/pre dependsOn %v, want hub-base", got)
	}
}

func TestV2BuildIsDeterministicAcrossRenderOrder(t *testing.T) {
	first := mustBuild(t, prodInput(prodRender()))
	docs := prodRender()
	// Reverse the declaration order of the units and shuffle nothing else: a
	// unit's place comes from `after`, not from where it was declared.
	in := prodInput(docs)
	rev := make([]release.DeliveryUnit, len(prodUnits))
	for i, u := range prodUnits {
		rev[len(prodUnits)-1-i] = u
	}
	in.Units = rev
	again := mustBuild(t, in)
	if first.Digest != again.Digest {
		t.Fatalf("declaring the units in another order changed the digest:\n %s\n %s", first.Digest, again.Digest)
	}
	for i := 0; i < 5; i++ {
		if again := mustBuild(t, prodInput(prodRender())); again.Digest != first.Digest {
			t.Fatalf("build %d changed the digest", i)
		}
	}
}

// v1 → v2: the same render, with and without units. An env that declares no
// units still gets v2 trees (everything in base), so the layout never depends
// on whether an author remembered to declare one.
func TestV1RenderBecomesV2WithoutDeclaringUnits(t *testing.T) {
	b := mustBuild(t, buildFixture())
	if len(b.Doc.Trees) == 0 {
		t.Fatal("a real cluster has no trees")
	}
	for _, tr := range b.Doc.Trees {
		if tr.Unit != release.DefaultDeliveryUnit {
			t.Errorf("%s: unit %q, want %q", tr.Path, tr.Unit, release.DefaultDeliveryUnit)
		}
	}
}

// The flat trees stay flat: nothing applies the unclustered tree as a
// cluster, and the hosted tree is records the control plane renders.
func TestFlatClustersStayFlat(t *testing.T) {
	in := buildFixture()
	in.Shape.Manifests += "---\napiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: host-only\n"
	b := mustBuild(t, in)
	for _, tr := range b.Doc.Trees {
		if release.FlatCluster(tr.Cluster) {
			t.Errorf("flat cluster %q has tree %s", tr.Cluster, tr.Path)
		}
	}
	for _, o := range b.Doc.Shape.Objects {
		if release.FlatCluster(o.Cluster) && o.Tree != "" {
			t.Errorf("%s on flat cluster has tree %q", o.Key(), o.Tree)
		}
		if !release.FlatCluster(o.Cluster) && o.Tree == "" {
			t.Errorf("%s has no tree", o.Key())
		}
	}
}

// A document shared by two clusters is in each cluster's tree — and in the same
// unit and phase on both.
func TestSharedObjectIsInEachClustersTree(t *testing.T) {
	b := mustBuild(t, buildFixture())
	var trees []string
	for _, f := range layerFiles(t, b) {
		if strings.Contains(f, "namespace-shop-prod") {
			trees = append(trees, treeOf(f))
		}
	}
	sort.Strings(trees)
	want := []string{"manifests/gke-daemon/base/pre", "manifests/gke-prod/base/pre"}
	if !reflect.DeepEqual(trees, want) {
		t.Errorf("shared Namespace is in %v, want %v", trees, want)
	}
}

func TestUnknownDeployPhaseRefusesTheBuild(t *testing.T) {
	docs := prodRender()
	docs[5].annotations = "    forge.dev/deploy-phase: pre-rolout\n"
	_, err := Build(t.Context(), prodInput(docs))
	if err == nil || !strings.Contains(err.Error(), "pre-rolout") {
		t.Fatalf("a typo'd phase must refuse the build, got %v", err)
	}
}

func TestUnitCycleRefusesTheBuild(t *testing.T) {
	in := prodInput(prodRender())
	in.Units = []release.DeliveryUnit{
		{Name: "infra", After: []string{"control-plane"}},
		{Name: "control-plane", After: []string{"infra"}},
		{Name: "reliant", After: []string{}}, {Name: "daemon-substrate", After: []string{}}, {Name: "hub-base", After: []string{}},
	}
	if _, err := Build(t.Context(), in); err == nil || !strings.Contains(err.Error(), "cycle") {
		t.Fatalf("a unit cycle must refuse the build, got %v", err)
	}
}

// MOVING AN OBJECT BETWEEN TREES IS REFUSED, not applied. An object that
// leaves a tree is pruned by the Kustomization it leaves and created by the one
// it joins, with nothing ordering the two.
func TestMovingAnObjectBetweenTreesIsAStopFinding(t *testing.T) {
	before := mustBuild(t, prodInput(prodRender()))

	docs := prodRender()
	docs[7].unit = "reliant" // temporal-namespace: infra/post → reliant/post
	after := mustBuild(t, prodInput(docs))

	plan, err := release.BuildPlan(release.PlanInput{
		Candidate: after.Doc.Shape, CandidateConfigDigest: after.Doc.ConfigDigest,
		Live: &before.Doc.Shape, Basis: release.PlanBasis{AppliedConfigDigest: before.Doc.ConfigDigest},
	})
	if err != nil {
		t.Fatal(err)
	}
	var moved []release.PlanFinding
	for _, f := range plan.Findings {
		if f.Code == release.FindingTreeMoved {
			moved = append(moved, f)
		}
	}
	if len(moved) != 1 {
		t.Fatalf("want exactly one tree_moved finding, got %d: %+v", len(moved), plan.Findings)
	}
	if moved[0].Class != release.ClassStop {
		t.Errorf("a tree move is %s, want stop", moved[0].Class)
	}
	if !strings.Contains(moved[0].Subject, "temporal-namespace") {
		t.Errorf("subject %q", moved[0].Subject)
	}
	if !strings.Contains(moved[0].Detail, "infra/post") || !strings.Contains(moved[0].Detail, "reliant/post") {
		t.Errorf("detail %q does not name both trees", moved[0].Detail)
	}
	if got := plan.Unacknowledged(nil); !reflect.DeepEqual(got, []string{release.FindingTreeMoved}) {
		t.Errorf("unacknowledged = %v: a bare --yes must not cover a tree move", got)
	}
	if got := plan.Unacknowledged([]string{release.FindingTreeMoved}); len(got) != 0 {
		t.Errorf("an explicit acknowledgement is refused: %v", got)
	}

	// The same render is NOT a move.
	same := mustBuild(t, prodInput(prodRender()))
	plan, err = release.BuildPlan(release.PlanInput{
		Candidate: same.Doc.Shape, CandidateConfigDigest: same.Doc.ConfigDigest,
		Live: &before.Doc.Shape, Basis: release.PlanBasis{AppliedConfigDigest: before.Doc.ConfigDigest},
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range plan.Findings {
		if f.Code == release.FindingTreeMoved {
			t.Errorf("an unchanged render reports a move: %+v", f)
		}
	}
}

// The v1 → v2 migration moves every object of a cluster out of its flat tree.
// It happens once, to every env, on the release that first records trees, so
// it is ONE warn per cluster: a stop per object would block every direct-apply
// env's next deploy over a layout it does not use.
func TestV1ToV2IsOneWarnPerCluster(t *testing.T) {
	b := mustBuild(t, prodInput(prodRender()))
	v1 := b.Doc.Shape
	v1.Objects = append([]release.ShapeObject(nil), v1.Objects...)
	for i := range v1.Objects {
		v1.Objects[i].Tree = ""
	}
	plan, err := release.BuildPlan(release.PlanInput{Candidate: b.Doc.Shape, Live: &v1})
	if err != nil {
		t.Fatal(err)
	}
	var subjects []string
	for _, f := range plan.Findings {
		if f.Code != release.FindingTreeMoved {
			continue
		}
		if f.Class != release.ClassWarn {
			t.Errorf("layout migration of %s is %s, want warn", f.Subject, f.Class)
		}
		subjects = append(subjects, f.Subject)
	}
	sort.Strings(subjects)
	if want := []string{tHub, tDaemon}; !reflect.DeepEqual(subjects, want) {
		t.Errorf("layout findings %v, want one per cluster %v", subjects, want)
	}
	if got := plan.Unacknowledged(nil); len(got) != 0 {
		t.Errorf("the layout migration blocks a deploy: %v", got)
	}
}
