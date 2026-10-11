// Package clickstack names the facts about the local ClickStack compose
// service that `forge env up`, the doctor checks and the integration gate all
// have to agree on, so none of them learns a name from another's source.
package clickstack

const (
	// Service is the compose service (and the workload name in dev/main.k).
	Service = "clickstack"

	// EnvOTLPHTTPPort is the collector's published OTLP/HTTP port. The port
	// variables are declared once in deploy/kcl/<env>/main.k, handed to
	// compose through OnCompose.env, and read back by `forge env up` to build
	// the endpoint host processes export to.
	EnvOTLPHTTPPort = "CLICKSTACK_OTLP_HTTP_PORT"
	// EnvOTLPGRPCPort is the collector's published OTLP/gRPC port.
	EnvOTLPGRPCPort = "CLICKSTACK_OTLP_GRPC_PORT"
	// EnvUIPort is the published HyperDX UI port.
	EnvUIPort = "CLICKSTACK_UI_PORT"

	// EnvOTLPPrefix prefixes the standard OTel exporter variables a process
	// is handed.
	EnvOTLPPrefix = "OTEL_EXPORTER_OTLP_"
	// EnvOTLPEndpoint is the standard OTLP base endpoint variable.
	EnvOTLPEndpoint = EnvOTLPPrefix + "ENDPOINT"
	// EnvOTLPProtocol is the standard OTLP protocol variable.
	EnvOTLPProtocol = EnvOTLPPrefix + "PROTOCOL"

	// DashboardsDir is the project-relative directory mounted as HyperDX's
	// DASHBOARD_PROVISIONER_DIR. A HyperDX v2 dashboard JSON dropped here is
	// loaded without a restart.
	DashboardsDir = "deploy/observability/dashboards"

	// ForwarderService is the compose service that stands in for ClickStack
	// when the env declares a destination: a plain collector that forwards
	// OTLP and the host logs there instead of storing them.
	ForwarderService = "otel-forwarder"
	// ForwarderConfigDir is the project-relative directory `forge env up`
	// renders the forwarder's pipeline file into (gitignored under .forge).
	ForwarderConfigDir = ".forge/observability"
	// ForwarderConfigFile is that file's name.
	ForwarderConfigFile = "forwarder.yaml"

	// CollectorConfig is the project-relative collector pipeline file.
	CollectorConfig = "deploy/observability/otel-collector.yaml"
)
