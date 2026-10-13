package cluster

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// A stateful FAKE CLUSTER behind a `kubectl` on PATH.
//
// The recorders elsewhere in this package prove WHAT forge sends. The chart
// prune is a statement about two applies in a row — an object present in
// release N and absent from N+1 is gone afterwards — and no recorder can say
// that, because the answer lives in what the first apply left behind. So this
// fake keeps objects: `apply` stores them, `get -l` filters them by label,
// `delete` removes them.
//
// It is the test binary itself. TestMain below turns the process into the fake
// when it is exec'd under the name `kubectl` with fakeClusterEnv set, and
// newFakeCluster symlinks the test binary to that name on PATH. No shell, no
// second toolchain, and the fake is ordinary Go that can be read and extended.
//
// What it models, on purpose: object identity (kind/namespace/name), labels and
// annotations with server-side-apply field ownership (a key forge applied and
// later stops applying is removed; a key another manager set is kept), the
// namespace defaulting `-n` gives a namespace-less document, and the cluster's
// own `kubernetes.io/metadata.name` label. What it does not model is the
// apiserver: admission, finalizers, garbage collection. Those are the
// apiserver's, and the prune's guards are written so it does not need them.

const fakeClusterEnv = "FORGE_FAKE_CLUSTER_STATE"

func TestMain(m *testing.M) {
	if state := os.Getenv(fakeClusterEnv); state != "" && filepath.Base(os.Args[0]) == "kubectl" {
		os.Exit(runFakeKubectl(state, os.Args[1:], os.Stdin, os.Stdout, os.Stderr))
	}
	os.Exit(m.Run())
}

// fakeClusterState is the on-disk cluster.
type fakeClusterState struct {
	Objects []map[string]any `json:"objects"`
	// Deletes is every `kubectl delete` the fake served, as "resource name [ns]".
	Deletes []string `json:"deletes"`
	// Applies counts the apply calls, so a test can tell "nothing applied".
	Applies int `json:"applies"`
}

// fakeClusterScoped are the kinds the fake treats as cluster-scoped: no
// namespace is defaulted onto them and a delete of one takes no -n.
var fakeClusterScoped = map[string]bool{
	"Namespace": true, "ClusterRole": true, "ClusterRoleBinding": true,
	"CustomResourceDefinition": true, "PersistentVolume": true,
	"ValidatingWebhookConfiguration": true, "MutatingWebhookConfiguration": true,
	"PriorityClass": true, "StorageClass": true,
}

// fakeAPIResources is what `api-resources` answers.
var fakeAPIResources = []string{
	"configmaps", "namespaces", "persistentvolumeclaims", "secrets", "serviceaccounts",
	"events", "pods",
	"deployments.apps", "statefulsets.apps",
	"networkpolicies.networking.k8s.io",
	"clusterroles.rbac.authorization.k8s.io", "clusterrolebindings.rbac.authorization.k8s.io",
	"roles.rbac.authorization.k8s.io", "rolebindings.rbac.authorization.k8s.io",
	"customresourcedefinitions.apiextensions.k8s.io",
}

func fakeKey(kind, ns, name string) string { return kind + "|" + ns + "|" + name }

func objKey(o map[string]any) string {
	md, _ := o["metadata"].(map[string]any)
	ns, _ := md["namespace"].(string)
	kind, _ := o["kind"].(string)
	name, _ := md["name"].(string)
	return fakeKey(kind, ns, name)
}

func loadFakeState(path string) fakeClusterState {
	var s fakeClusterState
	if b, err := os.ReadFile(path); err == nil {
		_ = json.Unmarshal(b, &s)
	}
	return s
}

func saveFakeState(path string, s fakeClusterState) {
	b, _ := json.MarshalIndent(s, "", " ")
	_ = os.WriteFile(path, b, 0o644)
}

func strMap(v any) map[string]any {
	m, _ := v.(map[string]any)
	if m == nil {
		m = map[string]any{}
	}
	return m
}

// mergeOwned is server-side-apply's rule for a map field written by one
// manager: the result is the applied keys, plus every existing key some OTHER
// manager owns. A key this manager applied last time and omits now is removed.
func mergeOwned(existing, applied map[string]any, prevOwned []any) (merged map[string]any, owned []any) {
	merged = map[string]any{}
	was := map[string]bool{}
	for _, k := range prevOwned {
		was[fmt.Sprint(k)] = true
	}
	for k, v := range existing {
		if !was[k] {
			merged[k] = v
		}
	}
	for k, v := range applied {
		merged[k] = v
		owned = append(owned, k)
	}
	sort.Slice(owned, func(i, j int) bool { return fmt.Sprint(owned[i]) < fmt.Sprint(owned[j]) })
	return merged, owned
}

func runFakeKubectl(statePath string, args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	if len(args) >= 2 && args[0] == "--context" {
		args = args[2:]
	}
	if len(args) == 0 {
		return 0
	}
	state := loadFakeState(statePath)
	defer func() { saveFakeState(statePath, state) }()

	flag := func(name string) string {
		for i, a := range args {
			if a == name && i+1 < len(args) {
				return args[i+1]
			}
		}
		return ""
	}

	switch args[0] {
	case "apply":
		if os.Getenv("FORGE_FAKE_FAIL_APPLY") != "" {
			fmt.Fprintln(stderr, "error: the server is currently unable to handle the request")
			return 1
		}
		state.Applies++
		defaultNS := flag("-n")
		dec := yaml.NewDecoder(stdin)
		for {
			var doc map[string]any
			if err := dec.Decode(&doc); err != nil {
				if errors.Is(err, io.EOF) {
					break
				}
				fmt.Fprintln(stderr, "error: parse:", err)
				return 1
			}
			if doc == nil {
				continue
			}
			kind, _ := doc["kind"].(string)
			md := strMap(doc["metadata"])
			if ns, _ := md["namespace"].(string); ns == "" && !fakeClusterScoped[kind] {
				if defaultNS == "" {
					defaultNS = "default"
				}
				md["namespace"] = defaultNS
			}
			doc["metadata"] = md
			applyFakeObject(&state, doc)
			name, _ := md["name"].(string)
			fmt.Fprintf(stdout, "%s/%s serverside-applied\n", strings.ToLower(kind), name)
		}
	case "api-resources":
		for _, r := range fakeAPIResources {
			fmt.Fprintln(stdout, r)
		}
	case "get":
		sel := map[string]string{}
		for _, kv := range strings.Split(flag("-l"), ",") {
			if k, v, ok := strings.Cut(kv, "="); ok {
				sel[k] = v
			}
		}
		items := []map[string]any{}
		for _, o := range state.Objects {
			labels := strMap(strMap(o["metadata"])["labels"])
			match := len(sel) > 0
			for k, v := range sel {
				if labels[k] != v {
					match = false
				}
			}
			if match {
				items = append(items, o)
			}
		}
		b, _ := json.Marshal(map[string]any{"apiVersion": "v1", "kind": "List", "items": items})
		fmt.Fprintln(stdout, string(b))
	case "delete":
		res, name, ns := args[1], args[2], flag("-n")
		state.Deletes = append(state.Deletes, strings.TrimSpace(res+" "+name+" "+ns))
		want, _, _ := strings.Cut(res, ".")
		kept := state.Objects[:0]
		for _, o := range state.Objects {
			md := strMap(o["metadata"])
			kind, _ := o["kind"].(string)
			oNS, _ := md["namespace"].(string)
			if strings.ToLower(kind) == want && md["name"] == name && oNS == ns {
				continue
			}
			kept = append(kept, o)
		}
		state.Objects = kept
	}
	return 0
}

// applyFakeObject upserts doc with server-side-apply label/annotation
// ownership.
func applyFakeObject(state *fakeClusterState, doc map[string]any) {
	key := objKey(doc)
	md := strMap(doc["metadata"])
	appliedLabels, appliedAnns := strMap(md["labels"]), strMap(md["annotations"])
	for i, existing := range state.Objects {
		if objKey(existing) != key {
			continue
		}
		emd := strMap(existing["metadata"])
		managed := strMap(emd["_managed"])
		labels, ownedL := mergeOwned(strMap(emd["labels"]), appliedLabels, asSlice(managed["labels"]))
		anns, ownedA := mergeOwned(strMap(emd["annotations"]), appliedAnns, asSlice(managed["annotations"]))
		md["labels"], md["annotations"] = labels, anns
		md["_managed"] = map[string]any{"labels": ownedL, "annotations": ownedA}
		pruneEmpty(md)
		state.Objects[i] = doc
		return
	}
	_, ownedL := mergeOwned(nil, appliedLabels, nil)
	_, ownedA := mergeOwned(nil, appliedAnns, nil)
	md["_managed"] = map[string]any{"labels": ownedL, "annotations": ownedA}
	if kind, _ := doc["kind"].(string); kind == "Namespace" {
		labels := strMap(md["labels"])
		labels["kubernetes.io/metadata.name"] = md["name"]
		md["labels"] = labels
	}
	state.Objects = append(state.Objects, doc)
}

func asSlice(v any) []any { s, _ := v.([]any); return s }

func pruneEmpty(md map[string]any) {
	for _, k := range []string{"labels", "annotations"} {
		if m, ok := md[k].(map[string]any); ok && len(m) == 0 {
			delete(md, k)
		}
	}
}

// fakeCluster is the test's handle on the fake.
type fakeCluster struct {
	t    *testing.T
	path string
}

// newFakeCluster installs the fake `kubectl` on PATH and returns a handle. The
// fake kubectl is this very test binary, symlinked under the right name.
func newFakeCluster(t *testing.T) *fakeCluster {
	t.Helper()
	requirePOSIXFake(t, "kubectl")
	self, err := os.Executable()
	if err != nil {
		t.Fatalf("locate test binary: %v", err)
	}
	dir := t.TempDir()
	if err := os.Symlink(self, filepath.Join(dir, "kubectl")); err != nil {
		t.Fatalf("symlink fake kubectl: %v", err)
	}
	path := filepath.Join(dir, "cluster.json")
	t.Setenv(fakeClusterEnv, path)
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return &fakeCluster{t: t, path: path}
}

func (c *fakeCluster) state() fakeClusterState { return loadFakeState(c.path) }

// find returns the live object, or nil.
func (c *fakeCluster) find(kind, ns, name string) map[string]any {
	for _, o := range c.state().Objects {
		if objKey(o) == fakeKey(kind, ns, name) {
			return o
		}
	}
	return nil
}

func (c *fakeCluster) has(kind, ns, name string) bool { return c.find(kind, ns, name) != nil }

// seed puts an object straight into the cluster, as if another actor created it.
func (c *fakeCluster) seed(doc map[string]any) {
	c.t.Helper()
	s := c.state()
	s.Objects = append(s.Objects, doc)
	saveFakeState(c.path, s)
}

// setForeignLabel adds a label to a live object as a DIFFERENT field manager
// would: forge does not own it.
func (c *fakeCluster) setForeignLabel(kind, ns, name, key, value string) {
	c.t.Helper()
	s := c.state()
	for _, o := range s.Objects {
		if objKey(o) == fakeKey(kind, ns, name) {
			md := strMap(o["metadata"])
			labels := strMap(md["labels"])
			labels[key] = value
			md["labels"] = labels
			saveFakeState(c.path, s)
			return
		}
	}
	c.t.Fatalf("no live %s %s/%s to label", kind, ns, name)
}

func (c *fakeCluster) labels(kind, ns, name string) map[string]any {
	o := c.find(kind, ns, name)
	if o == nil {
		c.t.Fatalf("no live %s %s/%s", kind, ns, name)
	}
	return strMap(strMap(o["metadata"])["labels"])
}

func (c *fakeCluster) deletes() []string { return c.state().Deletes }
