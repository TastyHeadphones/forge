package deploy

import (
	"bytes"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"sigs.k8s.io/yaml"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"

	"github.com/reliant-labs/forge/pkg/deploy/v1alpha1"
)

func agentSpec() ObservabilityAgentSpec {
	return ObservabilityAgentSpec{
		Endpoint:       "https://otlp.example.com",
		Protocol:       "http/protobuf",
		HeadersSecret:  &ObservabilitySecretKey{Name: "otlp-auth", Key: "headers"},
		Signals:        ObservabilitySignals{Traces: true, Metrics: true, Logs: true},
		SamplingRatio:  1,
		ErrorsAlways:   true,
		Image:          "otel/opentelemetry-collector-contrib:0.162.0@sha256:39923a8e431bd1f57be82411999d389fcfe40857492e4365456d97a4c1f74be6",
		MemoryLimitMiB: 256,
	}
}

var agentCtx = ObservabilityContext{Namespace: "forge-observability", PartOf: "demo", Env: "staging"}

// The node agent renders with no cluster: this is the k3d-free test of the
// DaemonSet, goldened so every change to it is a reviewed diff.
func TestRenderObservabilityAgentGolden(t *testing.T) {
	cases := map[string]ObservabilityAgentSpec{
		"observability_agent": agentSpec(),
		"observability_agent_grpc_sampled": func() ObservabilityAgentSpec {
			s := agentSpec()
			s.Protocol, s.Endpoint = "grpc", "https://otlp.example.com:443"
			s.SamplingRatio = 0.1
			s.Signals.Profiles = true
			s.HeadersSecret = nil
			return s
		}(),
	}
	for name, spec := range cases {
		t.Run(name, func(t *testing.T) {
			objs, err := RenderObservabilityAgent(spec, agentCtx)
			if err != nil {
				t.Fatal(err)
			}
			var buf bytes.Buffer
			for _, o := range objs {
				b, err := yaml.Marshal(o.Object)
				if err != nil {
					t.Fatal(err)
				}
				buf.WriteString("---\n")
				buf.Write(b)
			}
			path := filepath.Join("testdata", name+".golden.yaml")
			if *updateGolden {
				if err := os.WriteFile(path, buf.Bytes(), 0o644); err != nil {
					t.Fatal(err)
				}
				return
			}
			want, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("%v (regenerate with -update)", err)
			}
			if !bytes.Equal(want, buf.Bytes()) {
				t.Errorf("%s differs from the render (-update to accept):\n--- got\n%s", path, buf.String())
			}
		})
	}
}

func renderedDaemonSet(t *testing.T, s ObservabilityAgentSpec) *appsv1.DaemonSet {
	t.Helper()
	objs, err := RenderObservabilityAgent(s, agentCtx)
	if err != nil {
		t.Fatal(err)
	}
	for _, o := range objs {
		if o.GetKind() == "DaemonSet" {
			var ds appsv1.DaemonSet
			if err := runtime.DefaultUnstructuredConverter.FromUnstructured(o.Object, &ds); err != nil {
				t.Fatal(err)
			}
			return &ds
		}
	}
	t.Fatal("no DaemonSet rendered")
	return nil
}

// What the agent needs, and no more: hostPath on /var/log/pods READ-ONLY, no
// capabilities, a read-only root, and the headers from a Secret rather than
// the manifest.
func TestObservabilityAgentSecurityPosture(t *testing.T) {
	ds := renderedDaemonSet(t, agentSpec())
	pod := ds.Spec.Template.Spec
	c := pod.Containers[0]

	var podLogs *corev1.VolumeMount
	for i := range c.VolumeMounts {
		if c.VolumeMounts[i].MountPath == "/var/log/pods" {
			podLogs = &c.VolumeMounts[i]
		}
	}
	if podLogs == nil || !podLogs.ReadOnly {
		t.Errorf("/var/log/pods must be mounted read-only: %+v", podLogs)
	}
	paths := map[string]bool{}
	for _, v := range pod.Volumes {
		if v.HostPath != nil {
			paths[v.HostPath.Path] = true
		}
	}
	if len(paths) != 2 || !paths["/var/log/pods"] || !paths[agentStorageDir] {
		t.Errorf("hostPath volumes = %v, want exactly /var/log/pods and %s", paths, agentStorageDir)
	}
	sc := c.SecurityContext
	if sc == nil || sc.ReadOnlyRootFilesystem == nil || !*sc.ReadOnlyRootFilesystem ||
		sc.AllowPrivilegeEscalation == nil || *sc.AllowPrivilegeEscalation ||
		sc.Capabilities == nil || len(sc.Capabilities.Drop) != 1 || sc.Capabilities.Drop[0] != "ALL" {
		t.Errorf("agent container must drop ALL capabilities, forbid escalation and keep a read-only root: %+v", sc)
	}
	var headers *corev1.EnvVar
	for i := range c.Env {
		if c.Env[i].Name == agentHeadersEnv {
			headers = &c.Env[i]
		}
	}
	if headers == nil || headers.Value != "" || headers.ValueFrom == nil || headers.ValueFrom.SecretKeyRef == nil ||
		headers.ValueFrom.SecretKeyRef.Name != "otlp-auth" || headers.ValueFrom.SecretKeyRef.Key != "headers" {
		t.Errorf("destination headers must come from a secretKeyRef, never inline: %+v", headers)
	}
	if !strings.Contains(c.Image, "0.162.0@sha256:") {
		t.Errorf("the collector image must be pinned >= 0.162 by digest: %s", c.Image)
	}
}

// The agent is NOT a hosted workload: its kinds are outside the Restricted
// allowlist on purpose, and RenderWorkloads under that profile still refuses
// anything beyond it. The platform renders its own agent.
func TestObservabilityAgentIsOutsideTheRestrictedProfile(t *testing.T) {
	objs, err := RenderObservabilityAgent(agentSpec(), agentCtx)
	if err != nil {
		t.Fatal(err)
	}
	outside := map[string]bool{}
	for _, o := range objs {
		if !RestrictedKinds[o.GetKind()] {
			outside[o.GetKind()] = true
		}
	}
	for _, k := range []string{"DaemonSet", "ClusterRole", "ClusterRoleBinding", "Namespace", "ConfigMap"} {
		if !outside[k] {
			t.Errorf("%s is expected outside RestrictedKinds (the agent is never hosted-rendered)", k)
		}
	}
	for _, k := range []string{"DaemonSet", "ClusterRole", "ClusterRoleBinding"} {
		if RestrictedKinds[k] {
			t.Errorf("RestrictedKinds must never allow %s", k)
		}
	}
	// And the Restricted workload path still validates a plain service whose
	// env carries the OTEL contract, with no agent anywhere in the set.
	w := v1alpha1.Workload{}
	w.Name = "api"
	w.Spec = v1alpha1.WorkloadSpec{
		Kind: v1alpha1.KindService, Image: "ghcr.io/acme/api@sha256:" + strings.Repeat("a", 64),
		Ports: []v1alpha1.Port{{Name: "http", Port: 8080, Expose: true}},
		Env: []v1alpha1.EnvVar{
			{Name: "OTEL_SERVICE_NAME", Value: "api"},
			{Name: "OTEL_EXPORTER_OTLP_PROTOCOL", Value: "http/protobuf"},
			{Name: "OTEL_RESOURCE_ATTRIBUTES", Value: "deployment.environment.name=prod,service.namespace=demo"},
		},
	}
	got, err := RenderWorkloads([]v1alpha1.Workload{w}, v1alpha1.ProfileRestricted, Context{Namespace: "env-1"})
	if err != nil {
		t.Fatalf("a hosted service carrying the OTEL_* contract must still render under Restricted: %v", err)
	}
	for _, o := range got {
		if !RestrictedKinds[o.GetKind()] {
			t.Errorf("restricted render produced %s", o.GetKind())
		}
	}
}

// The service is node-local and the DaemonSet reaches every node.
func TestObservabilityAgentServiceIsNodeLocal(t *testing.T) {
	objs, err := RenderObservabilityAgent(agentSpec(), agentCtx)
	if err != nil {
		t.Fatal(err)
	}
	for _, o := range objs {
		if o.GetKind() != "Service" {
			continue
		}
		spec := o.Object["spec"].(map[string]any)
		if spec["internalTrafficPolicy"] != "Local" {
			t.Errorf("internalTrafficPolicy = %v, want Local", spec["internalTrafficPolicy"])
		}
		return
	}
	t.Fatal("no Service")
}

// Collection model: apps PUSH. The agent pulls only stdout (filelog) and
// kubelet stats; it scrapes nothing, and k8sattributes only enriches.
func TestAgentConfigCollectsByPushAndEnrichesOnly(t *testing.T) {
	cfg, err := agentSpec().AgentConfig("forge-observability")
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Receivers  map[string]any `json:"receivers"`
		Processors map[string]any `json:"processors"`
		Service    struct {
			Pipelines map[string]struct {
				Receivers  []string `json:"receivers"`
				Processors []string `json:"processors"`
				Exporters  []string `json:"exporters"`
			} `json:"pipelines"`
		} `json:"service"`
	}
	if err := yaml.Unmarshal([]byte(cfg), &doc); err != nil {
		t.Fatal(err)
	}
	for r := range doc.Receivers {
		switch r {
		case "otlp", "filelog", "kubeletstats":
		default:
			t.Errorf("receiver %q: the agent pulls only stdout and kubelet stats (never scrape tenants by default)", r)
		}
	}
	for _, banned := range []string{"tenant", "org", "token", "attributes/", "resource/"} {
		for p := range doc.Processors {
			if strings.HasPrefix(p, banned) {
				t.Errorf("processor %q looks like a tenant-identity rule; forge's agent only enriches", p)
			}
		}
	}
	if strings.Contains(cfg, "prometheus") {
		t.Error("the agent config scrapes Prometheus; scraping is a per-namespace opt-in, not a default")
	}
	logs := doc.Service.Pipelines["logs"]
	if len(logs.Receivers) != 1 || logs.Receivers[0] != "filelog" {
		t.Errorf("logs pipeline receivers = %v, want only filelog (one log path)", logs.Receivers)
	}
	if m := doc.Service.Pipelines["metrics/node"]; len(m.Receivers) != 1 || m.Receivers[0] != "kubeletstats" {
		t.Errorf("kubelet stats get their own pipeline, got %v", m.Receivers)
	}
	if !contains(doc.Service.Pipelines["traces"].Receivers, "otlp") || contains(doc.Service.Pipelines["logs"].Receivers, "otlp") {
		t.Error("traces come over OTLP; logs must NOT also come over OTLP (one shipping path)")
	}
	if !strings.Contains(cfg, "exclude:") || !strings.Contains(cfg, "/var/log/pods/forge-observability_*") {
		t.Error("the agent must not tail its own logs")
	}
	for _, p := range doc.Service.Pipelines {
		if !contains(p.Exporters, "otlphttp/destination") {
			t.Errorf("pipeline exports to %v, want the destination", p.Exporters)
		}
	}
	if strings.Contains(cfg, "Bearer") || strings.Contains(cfg, "authorization") {
		t.Error("the config must reference the headers by env, never inline them")
	}
	if !strings.Contains(cfg, "${env:"+agentHeadersEnv+"}") {
		t.Error("destination headers must be read from the environment")
	}
}

func TestAgentConfigSignalsAndSampling(t *testing.T) {
	s := agentSpec()
	s.Signals = ObservabilitySignals{Traces: true, Profiles: true}
	s.SamplingRatio = 0.25
	cfg, err := s.AgentConfig("forge-observability")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"traces:", "profiles:", "tail_sampling", "status_code", "sampling_percentage: 25"} {
		if !strings.Contains(cfg, want) {
			t.Errorf("config missing %q:\n%s", want, cfg)
		}
	}
	for _, off := range []string{"\n    logs:", "\n    metrics:"} {
		if strings.Contains(cfg, off) {
			t.Errorf("a disabled signal still has a pipeline (%q)", strings.TrimSpace(off))
		}
	}
	if args := s.CollectorArgs("/x"); len(args) != 2 || args[1] != "--feature-gates=service.profilesSupport" {
		t.Errorf("profiles need the feature gate: %v", args)
	}
	s.ErrorsAlways = false
	cfg, _ = s.AgentConfig("forge-observability")
	if strings.Contains(cfg, "status_code") {
		t.Error("errors_always = false still renders an error policy")
	}
}

func TestForwarderConfigTailsHostLogsOnly(t *testing.T) {
	cfg, err := agentSpec().ForwarderConfig()
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"/var/log/forge/*/*.log", "frontend_*.log", "deployment.environment.name", "${env:FORGE_OTLP_HEADERS}"} {
		if !strings.Contains(cfg, want) {
			t.Errorf("forwarder config missing %q", want)
		}
	}
	for _, banned := range []string{"k8sattributes", "kubeletstats", "file_storage"} {
		if strings.Contains(cfg, banned) {
			t.Errorf("the local forwarder has no Kubernetes or host dir to use %q", banned)
		}
	}
}

func TestObservabilityAgentSpecValidation(t *testing.T) {
	for name, mut := range map[string]func(*ObservabilityAgentSpec){
		"bad endpoint": func(s *ObservabilityAgentSpec) { s.Endpoint = "otlp.example.com" },
		"bad protocol": func(s *ObservabilityAgentSpec) { s.Protocol = "thrift" },
		"bad ratio":    func(s *ObservabilityAgentSpec) { s.SamplingRatio = 1.5 },
		"empty secret": func(s *ObservabilityAgentSpec) { s.HeadersSecret = &ObservabilitySecretKey{Name: "x"} },
	} {
		t.Run(name, func(t *testing.T) {
			s := agentSpec()
			mut(&s)
			if _, err := RenderObservabilityAgent(s, agentCtx); err == nil {
				t.Fatal("expected a refusal")
			}
		})
	}
}

func contains(l []string, s string) bool {
	for _, x := range l {
		if x == s {
			return true
		}
	}
	return false
}

// The kata attribution finding: on a pod-cache miss k8sattributes takes the
// namespace for its label extraction from the client's own k8s.namespace.name
// and never overwrites an attribute the client set. So client-supplied
// identity is deleted BEFORE k8sattributes on every pipeline a client can
// reach, and NOT on the pipelines the node feeds itself (their k8s.*
// attributes come from the node and are what the enrichment trusts).
func TestAgentStripsClientIdentityBeforeEnrichment(t *testing.T) {
	s := agentSpec()
	s.Signals.Profiles = true
	cfg, err := s.AgentConfig("forge-observability")
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Processors map[string]any `json:"processors"`
		Service    struct {
			Pipelines map[string]struct {
				Receivers  []string `json:"receivers"`
				Processors []string `json:"processors"`
			} `json:"pipelines"`
		} `json:"service"`
	}
	if err := yaml.Unmarshal([]byte(cfg), &doc); err != nil {
		t.Fatal(err)
	}
	const strip, enrich = "transform/strip_client_identity", "k8sattributes"
	if _, ok := doc.Processors[strip]; !ok {
		t.Fatal("no client-identity strip processor")
	}
	for name, p := range doc.Service.Pipelines {
		fromClient := contains(p.Receivers, "otlp")
		si, ei := indexOf(p.Processors, strip), indexOf(p.Processors, enrich)
		switch {
		case fromClient && (si < 0 || ei < 0 || si > ei):
			t.Errorf("pipeline %s takes OTLP from clients but does not strip identity before k8sattributes: %v", name, p.Processors)
		case !fromClient && si >= 0:
			t.Errorf("pipeline %s is node-derived; stripping would delete the trustworthy k8s.* attributes: %v", name, p.Processors)
		}
	}
	for _, key := range []string{"k8s.namespace.name", "k8s.pod.uid", "reliant.org.id", "tenant.id", "org.id"} {
		if !regexp.MustCompile(ClientIdentityKeyPattern).MatchString(key) {
			t.Errorf("%s is not stripped", key)
		}
	}
	for _, key := range []string{"service.name", "service.version", "deployment.environment.name"} {
		if regexp.MustCompile(ClientIdentityKeyPattern).MatchString(key) {
			t.Errorf("%s belongs to the app and must be kept", key)
		}
	}
}

func indexOf(l []string, s string) int {
	for i, x := range l {
		if x == s {
			return i
		}
	}
	return -1
}
