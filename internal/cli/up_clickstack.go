package cli

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/reliant-labs/forge/internal/clickstack"
	"github.com/reliant-labs/forge/pkg/deploy"
)

// frontendOTLPEnv is the collector address a frontend dev server gets: the
// BASE URL only, from the env's typed observability declaration (the loopback
// port its collector publishes). The dev server's /_otel proxy forwards the
// browser's OTLP/HTTP to it; the wire protocol is not the proxy's to choose.
// Nil when the env declares no observability or has no local collector.
func frontendOTLPEnv(e *KCLEntities) map[string]string {
	if e == nil || e.Observability == nil || !e.Observability.Enabled || e.Observability.LocalEndpoint == "" {
		return nil
	}
	return map[string]string{clickstack.EnvOTLPEndpoint: e.Observability.LocalEndpoint}
}

// withFrontendTelemetryEnv gives a frontend dev server what its browser
// telemetry needs from `forge env up`, each unless already set:
//
//   - OTEL_EXPORTER_OTLP_ENDPOINT, this project's collector base URL (the port
//     compose actually published). The dev server's /_otel proxy forwards the
//     browser's OTLP/HTTP there; without it the proxy guesses 127.0.0.1:4318,
//     which after a port step-off is ANOTHER project's ClickStack.
//   - <NEXT_PUBLIC_|VITE_|EXPO_PUBLIC_>ENVIRONMENT, the forge env name, which
//     the browser reports as deployment.environment.name exactly as the
//     backend does. An explicit config.environment already arrives through the
//     frontend's env_vars and is kept.
func withFrontendTelemetryEnv(env []string, e *KCLEntities, fe FrontendEntity, envName string) []string {
	// Only the endpoint is injected, so only the endpoint can be a decision to
	// respect: a protocol exported for some other process is not one.
	env = withDefaultEnv(env, frontendOTLPEnv(e))
	return withDefaultEnv(env, map[string]string{frontendEnvironmentEnvVar(fe.Type): envName})
}

// nonEmptyEnvKeys is the set of keys env carries a NON-EMPTY value for. Empty
// counts as unset on purpose: the project-config projection emits one entry per
// config field, so an unset otlp_endpoint arrives as
// OTEL_EXPORTER_OTLP_ENDPOINT="" and must not shadow a default.
func nonEmptyEnvKeys(env []string) map[string]bool {
	have := make(map[string]bool, len(env))
	for _, kv := range env {
		if i := strings.IndexByte(kv, '='); i > 0 && kv[i+1:] != "" {
			have[kv[:i]] = true
		}
	}
	return have
}

// withDefaultEnv adds defaults to env as ONE UNIT: if env already carries a
// non-empty value for any key of defaults, nothing is added. A value the user
// exported, declared in KCL or pinned in config.k always wins, and wins whole.
//
// All-or-nothing is the point. An endpoint and the protocol that speaks to it
// are a pair; filling in only the missing half pairs a user's own endpoint
// with a protocol they never chose (their gRPC :4317 with our http/protobuf),
// and export then fails with nothing on screen saying why.
func withDefaultEnv(env []string, defaults map[string]string) []string {
	have := nonEmptyEnvKeys(env)
	for k := range defaults {
		if have[k] {
			return env
		}
	}
	for _, k := range sortedKeys(defaults) {
		env = withForcedEnv(env, k, defaults[k])
	}
	return env
}

func composeServiceName(w WorkloadEntity) string {
	if w.Runtime.Compose != nil && w.Runtime.Compose.Service != "" {
		return w.Runtime.Compose.Service
	}
	return w.Name
}

// clickstackRowLabel names a clickstack port for the env-up summary. The
// service publishes three, and ":8080" alone does not say which one is the UI.
func clickstackRowLabel(service string, targetPort int) (string, bool) {
	if service != clickstack.Service {
		return "", false
	}
	switch targetPort {
	case 8080:
		return "HyperDX UI", true
	case 4318:
		return "OTLP/HTTP", true
	case 4317:
		return "OTLP/gRPC", true
	}
	return "", false
}

// withoutDerivedOTLPIfShellSet lets a developer's own exported transport win
// over the one the declaration derives. Declared env beats the shell in
// general, but the endpoint and protocol are a pair the developer may have
// pointed elsewhere on purpose (their gRPC :4317), and filling in half of it
// is how export fails with nothing on screen. Only the derived pair is
// dropped: an endpoint the author wrote in KCL is not this env's local one.
func withoutDerivedOTLPIfShellSet(declared map[string]string, shell []string) map[string]string {
	ep := declared[clickstack.EnvOTLPEndpoint]
	if ep == "" || !strings.HasPrefix(ep, "http://127.0.0.1:") {
		return declared
	}
	shellSet := nonEmptyEnvKeys(shell)
	set := false
	for _, suffix := range []string{"ENDPOINT", "PROTOCOL"} {
		for _, sig := range []string{"", "TRACES_", "METRICS_", "LOGS_"} {
			if shellSet[clickstack.EnvOTLPPrefix+sig+suffix] {
				set = true
			}
		}
	}
	if !set {
		return declared
	}
	out := make(map[string]string, len(declared))
	for k, v := range declared {
		if k != clickstack.EnvOTLPEndpoint && k != clickstack.EnvOTLPProtocol {
			out[k] = v
		}
	}
	return out
}

// localCollectorService is the compose service that is this env's local
// collector: the ClickStack that stores telemetry, or the forwarder that sends
// it to the env's declared destination. "" when the env runs neither.
func localCollectorService(e *KCLEntities) string {
	if e == nil {
		return ""
	}
	for _, w := range e.WorkloadsOn(RuntimeCompose) {
		switch composeServiceName(w) {
		case clickstack.Service, clickstack.ForwarderService:
			return composeServiceName(w)
		}
	}
	return ""
}

// prepareLocalCollector gets the host ready for the local collector before
// compose starts it:
//
//   - .forge/logs exists. The collector bind-mounts it read-only to tail the
//     host processes' logs, and docker creates a missing bind source as ROOT,
//     after which forge could not write the very logs the collector is there
//     to read.
//   - for the forwarder, its pipeline file is rendered from the typed
//     declaration into .forge/observability/forwarder.yaml. The destination's
//     headers never touch it: the collector reads them from its environment.
func prepareLocalCollector(projectDir string, e *KCLEntities) error {
	svc := localCollectorService(e)
	if svc == "" {
		return nil
	}
	if err := os.MkdirAll(filepath.Join(projectDir, ".forge", "logs"), 0o755); err != nil {
		return err
	}
	if svc != clickstack.ForwarderService {
		return nil
	}
	cfg, err := forwarderConfig(e.Observability)
	if err != nil {
		return err
	}
	dir := filepath.Join(projectDir, clickstack.ForwarderConfigDir)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, clickstack.ForwarderConfigFile), []byte(cfg), 0o644)
}

// forwarderConfig renders the local forwarder's pipelines for o.
func forwarderConfig(o *ObservabilityEntity) (string, error) {
	if o == nil || o.Destination == nil {
		return "", fmt.Errorf("the local forwarder needs Bundle.observability.destination")
	}
	spec := deploy.ObservabilityAgentSpec{
		Endpoint:      o.Destination.Endpoint,
		Protocol:      o.Destination.Protocol,
		SamplingRatio: o.Sampling.TracesRatio,
		ErrorsAlways:  o.Sampling.ErrorsAlways,
		Signals:       deploy.ObservabilitySignals(o.Signals),
	}
	if h := o.Destination.HeadersSecret; h != nil {
		spec.HeadersSecret = &deploy.ObservabilitySecretKey{Name: h.Name, Key: h.Key}
	}
	return spec.ForwarderConfig()
}

// forwarderHeadersEnv is the compose-process env that hands the forwarder its
// destination headers: the value of the declared key in the env's secret
// store. nil when the destination declares none.
func forwarderHeadersEnv(e *KCLEntities, store map[string]string) map[string]string {
	if e == nil || e.Observability == nil || e.Observability.Destination == nil || e.Observability.Destination.HeadersSecret == nil {
		return nil
	}
	v, ok := store[e.Observability.Destination.HeadersSecret.Key]
	if !ok || v == "" {
		return nil
	}
	return map[string]string{deploy.ForwarderHeadersEnv: v}
}
