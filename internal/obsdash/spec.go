// Package obsdash turns a forge project's own service metadata into
// observability dashboards.
//
// The package has two layers, and the split is the point:
//
//   - Spec (this file) is a backend-neutral description of WHAT to show:
//     dashboards made of panels, each panel a query intent (signal, aggregate,
//     predicates on OpenTelemetry attribute names). It names no ClickHouse
//     column, no HyperDX field and no chart library. Build derives it from the
//     project's services, rpcs, frontends and database. It is also serialized
//     (deploy/observability/dashboard-spec_gen.json) so a second renderer —
//     the Reliant console, drawing with uPlot — can consume exactly the same
//     model without parsing HyperDX JSON.
//
//   - RenderHyperDX (hyperdx.go) is one renderer over the Spec: it emits
//     HyperDX v2 dashboard JSON for the file provisioner (local dev, operators)
//     or for the HyperDX API (hosted).
//
// Everything is keyed on OpenTelemetry names (service.name,
// deployment.environment.name, span.name, ...), the contract forge's runtime
// and KCL already settle, so no panel depends on how a given backend stores
// them.
package obsdash

import (
	"fmt"
	"sort"
	"strings"

	"github.com/reliant-labs/forge/pkg/observe/metrics"
)

// SpecVersion identifies the serialized shape of Spec. Bump it on a breaking
// change so a consumer can refuse a spec it does not understand.
const SpecVersion = "forge.dashboards/v1"

// Signal is the telemetry table a panel reads.
type Signal string

// The signals a panel can read.
const (
	Traces  Signal = "traces"
	Metrics Signal = "metrics"
	Logs    Signal = "logs"
)

// PanelKind is how a panel is drawn.
type PanelKind string

const (
	// KindNumber is one headline value over the selected time range.
	KindNumber PanelKind = "number"
	// KindLine is a time series; with GroupBy, one line per group.
	KindLine PanelKind = "line"
	// KindTable is one row per GroupBy value, one column per Series.
	KindTable PanelKind = "table"
	// KindSearch is a log or span search (no aggregate): the rows themselves.
	KindSearch PanelKind = "search"
)

// Agg is an aggregate function.
type Agg string

// The aggregate functions a series can apply.
const (
	AggCount         Agg = "count"
	AggCountDistinct Agg = "count_distinct"
	AggSum           Agg = "sum"
	AggAvg           Agg = "avg"
	AggMax           Agg = "max"
	AggQuantile      Agg = "quantile"
	AggLast          Agg = "last"
)

// Unit is how a panel's values are formatted.
type Unit string

// The units a panel's values are formatted in.
const (
	UnitNone    Unit = ""
	UnitCount   Unit = "count"
	UnitMillis  Unit = "ms"
	UnitBytes   Unit = "bytes"
	UnitPercent Unit = "percent" // values are fractions in [0,1]
)

// Op is a predicate operator.
type Op string

// The predicate operators.
const (
	OpEq     Op = "eq"
	OpNeq    Op = "neq"
	OpIn     Op = "in"
	OpNotIn  Op = "not_in"
	OpPrefix Op = "prefix"
	OpMatch  Op = "match" // RE2
	OpExists Op = "exists"
	OpGTE    Op = "gte"
)

// Semantic field names. A field is one of:
//
//	service.name, deployment.environment.name        (every signal)
//	span.name, span.kind, span.status,
//	span.status_message, span.duration               (traces)
//	span.attr.<key>                                  (traces)
//	log.severity, log.body, log.attr.<key>           (logs)
//	metric.attr.<key>                                (metrics)
//	resource.attr.<key>                              (every signal)
//
// span.kind is "server" or "client"; span.status and log.severity compare
// against "error". span.duration is milliseconds.
const (
	FieldService = "service.name"
	FieldEnv     = "deployment.environment.name"
)

// Predicate is one condition. Values are literals; Field is a semantic field.
type Predicate struct {
	Field  string   `json:"field"`
	Op     Op       `json:"op"`
	Value  string   `json:"value,omitempty"`
	Values []string `json:"values,omitempty"`
}

// Series is one aggregate shown by a panel.
type Series struct {
	Label string `json:"label"`
	Agg   Agg    `json:"agg"`
	// Quantile is the level for AggQuantile, e.g. 0.95.
	Quantile float64 `json:"quantile,omitempty"`
	// Field is the aggregated value: a semantic field for traces and logs
	// (span.duration, span.attr.lcp), the metric NAME for metrics. Empty for
	// AggCount.
	Field string `json:"field,omitempty"`
	// MetricType is "sum" or "gauge" (metrics only).
	MetricType string `json:"metricType,omitempty"`
	// Where narrows this series alone (e.g. only the failed requests).
	Where []Predicate `json:"where,omitempty"`
}

// Panel is one tile.
type Panel struct {
	ID          string    `json:"id"`
	Title       string    `json:"title"`
	Description string    `json:"description,omitempty"`
	Kind        PanelKind `json:"kind"`
	Signal      Signal    `json:"signal"`
	Series      []Series  `json:"series,omitempty"`
	// Ratio makes a two-series line panel plot Series[0]/Series[1]
	// (an error rate is failed requests over all requests).
	Ratio bool `json:"ratio,omitempty"`
	// Where narrows every series of the panel.
	Where   []Predicate `json:"where,omitempty"`
	GroupBy []string    `json:"groupBy,omitempty"`
	// Columns are the fields a KindSearch panel shows.
	Columns []string `json:"columns,omitempty"`
	Unit    Unit     `json:"unit,omitempty"`
	// Width is in grid columns out of 24; Height in rows. Zero takes the
	// default for the kind.
	Width  int `json:"width,omitempty"`
	Height int `json:"height,omitempty"`
}

// Filter is a dashboard-level dropdown that narrows every panel.
type Filter struct {
	ID    string `json:"id"`
	Label string `json:"label"`
	Field string `json:"field"`
	// Signal is the telemetry the dropdown reads its values from. Empty is
	// Traces, which every dashboard but the metrics ones draws.
	Signal Signal `json:"signal,omitempty"`
}

// Dashboard is an ordered list of panels under a shared set of filters.
type Dashboard struct {
	ID          string   `json:"id"`
	Title       string   `json:"title"`
	Description string   `json:"description,omitempty"`
	Filters     []Filter `json:"filters,omitempty"`
	Panels      []Panel  `json:"panels"`
}

// SavedSearch is a named log search a service owner returns to.
type SavedSearch struct {
	ID      string      `json:"id"`
	Title   string      `json:"title"`
	Signal  Signal      `json:"signal"`
	Where   []Predicate `json:"where"`
	Columns []string    `json:"columns,omitempty"`
}

// AlertStatus says whether an alert definition is live.
type AlertStatus string

// AlertPending marks an alert that is recorded but not live. HyperDX 2.40 validates an alert declared in a provisioned
// dashboard file and then never evaluates it (hyperdx PR #3233 is open), so a
// definition is recorded here and NOT embedded in the dashboard JSON. Whoever
// owns delivery (the hosted control plane, via the HyperDX alerts API)
// creates it.
const AlertPending AlertStatus = "pending"

// Alert is an alert definition over one panel's query.
type Alert struct {
	ID    string `json:"id"`
	Title string `json:"title"`
	// Panel is "<dashboard id>/<panel id>": the query the alert evaluates.
	Panel string `json:"panel"`
	// Interval is the evaluation window: 1m, 5m, 15m, 30m, 1h.
	Interval      string      `json:"interval"`
	ThresholdType string      `json:"thresholdType"` // "above" | "below"
	Threshold     float64     `json:"threshold"`
	Status        AlertStatus `json:"status"`
	Note          string      `json:"note,omitempty"`
}

// Spec is everything the generator derives for one project.
type Spec struct {
	Version       string        `json:"version"`
	Project       string        `json:"project"`
	Dashboards    []Dashboard   `json:"dashboards"`
	SavedSearches []SavedSearch `json:"savedSearches"`
	Alerts        []Alert       `json:"alerts"`
}

// Method is one rpc of a service.
type Method struct {
	Name string
}

// Service is one Connect/gRPC service.
type Service struct {
	// Name is the short service name used in file names and titles
	// ("orders").
	Name string
	// Package is the proto package ("orders.v1"); ProtoService the proto
	// service name ("OrdersService"). Together they give the procedure
	// "orders.v1.OrdersService/GetOrder" the RPC interceptors name spans by.
	Package      string
	ProtoService string
	Methods      []Method
}

// procedure returns the full procedure name of m: the span name the
// otelconnect server interceptor uses.
func procedure(s Service, m Method) string {
	return s.Package + "." + s.ProtoService + "/" + m.Name
}

// Input is what Build reads from the project.
type Input struct {
	Project   string
	Services  []Service
	Frontends []string
	// HasDatabase adds the database panels: true once the project has a
	// migration, since the panels read spans the ORM's query hook emits.
	HasDatabase bool
	// Metrics, when set, is the project's metrics schema (forge.metrics/v1,
	// from `<bin> metrics schema --json`). Build adds the "Custom metrics"
	// dashboard for the metrics it declares. Nil adds nothing: generating
	// needs no built binary, so the schema is supplied by whoever has one.
	Metrics *metrics.Schema
}

// maxPerRPCRows bounds the per-rpc RED rows: past this a service gets the
// RED table only, because 3 tiles per rpc stops being readable (and
// HyperDX caps a dashboard at 500 tiles).
const maxPerRPCRows = 25

// FrontendServiceName is the service.name the scaffolded frontends report
// (src/lib/otel_gen.ts: '<project>-frontend').
func FrontendServiceName(project string) string { return project + "-frontend" }

// Build derives the project's Spec.
func Build(in Input) Spec {
	sp := Spec{Version: SpecVersion, Project: in.Project, SavedSearches: []SavedSearch{}, Alerts: []Alert{}}

	services := append([]Service(nil), in.Services...)
	sort.Slice(services, func(i, j int) bool { return services[i].Name < services[j].Name })

	overview := buildOverview(in)
	sp.Dashboards = append(sp.Dashboards, overview)
	sp.Alerts = append(sp.Alerts, buildAlerts(overview)...)

	for _, s := range services {
		if len(s.Methods) == 0 {
			continue
		}
		sp.Dashboards = append(sp.Dashboards, buildService(in, s))
		sp.SavedSearches = append(sp.SavedSearches, SavedSearch{
			ID:      "errors-" + s.Name,
			Title:   fmt.Sprintf("%s: error logs", s.Name),
			Signal:  Logs,
			Where:   append(serviceLogScope(s), errorLogs),
			Columns: logColumns,
		})
	}

	if in.Metrics != nil {
		if d, ok := BuildCustomMetrics(in.Project, *in.Metrics); ok {
			sp.Dashboards = append(sp.Dashboards, d)
		}
	}

	fes := append([]string(nil), in.Frontends...)
	sort.Strings(fes)
	if len(fes) > 0 {
		sp.Dashboards = append(sp.Dashboards, buildFrontend(in))
	}
	return sp
}

var (
	errorSpans = Predicate{Field: "span.status", Op: OpEq, Value: "error"}
	errorLogs  = Predicate{Field: "log.severity", Op: OpEq, Value: "error"}
	logColumns = []string{"timestamp", FieldService, "log.severity", "log.body"}

	// rpcServer selects the server side of an rpc: a server span named
	// "<package>.<Service>/<Method>". The name shape (no spaces, one slash)
	// keeps plain HTTP spans ("GET /x") out of an rpc dashboard.
	rpcServer = []Predicate{
		{Field: "span.kind", Op: OpEq, Value: "server"},
		{Field: "span.name", Op: OpMatch, Value: `^[A-Za-z0-9_.]+/[A-Za-z0-9_]+$`},
	}
)

func scoped(base []Predicate, extra ...Predicate) []Predicate {
	out := append([]Predicate(nil), base...)
	return append(out, extra...)
}

func commonFilters() []Filter {
	return []Filter{
		{ID: "service", Label: "service.name", Field: FieldService},
		{ID: "env", Label: "deployment.environment.name", Field: FieldEnv},
	}
}

func serviceLogScope(s Service) []Predicate {
	return []Predicate{{Field: "log.attr.procedure", Op: OpPrefix, Value: "/" + s.Package + "." + s.ProtoService + "/"}}
}

func latencySeries() []Series {
	return []Series{
		{Label: "p50", Agg: AggQuantile, Quantile: 0.5, Field: "span.duration"},
		{Label: "p95", Agg: AggQuantile, Quantile: 0.95, Field: "span.duration"},
		{Label: "p99", Agg: AggQuantile, Quantile: 0.99, Field: "span.duration"},
	}
}

func redTableSeries() []Series {
	s := []Series{
		{Label: "requests", Agg: AggCount},
		{Label: "errors", Agg: AggCount, Where: []Predicate{errorSpans}},
	}
	return append(s, latencySeries()...)
}

func errorRatioSeries() []Series {
	return []Series{
		{Label: "errors", Agg: AggCount, Where: []Predicate{errorSpans}},
		{Label: "requests", Agg: AggCount},
	}
}

func buildOverview(in Input) Dashboard {
	d := Dashboard{
		ID:          "overview",
		Title:       in.Project + ": overview",
		Description: "Request rate, error rate and latency per service, top errors and recent error logs. Generated by forge from the project's services; narrow with the service.name and deployment.environment.name filters.",
		Filters:     commonFilters(),
	}
	d.Panels = []Panel{
		{ID: "requests", Title: "Requests", Kind: KindNumber, Signal: Traces, Where: rpcServer, Series: []Series{{Label: "requests", Agg: AggCount}}, Unit: UnitCount, Width: 6, Height: 4},
		{ID: "errors", Title: "Server errors", Kind: KindNumber, Signal: Traces, Where: scoped(rpcServer, errorSpans), Series: []Series{{Label: "errors", Agg: AggCount}}, Unit: UnitCount, Width: 6, Height: 4},
		{ID: "p95", Title: "p95 latency", Kind: KindNumber, Signal: Traces, Where: rpcServer, Series: []Series{{Label: "p95", Agg: AggQuantile, Quantile: 0.95, Field: "span.duration"}}, Unit: UnitMillis, Width: 6, Height: 4},
		{ID: "p99", Title: "p99 latency", Kind: KindNumber, Signal: Traces, Where: rpcServer, Series: []Series{{Label: "p99", Agg: AggQuantile, Quantile: 0.99, Field: "span.duration"}}, Unit: UnitMillis, Width: 6, Height: 4},
		{ID: "request-rate", Title: "Requests per service", Description: "Count per time bucket.", Kind: KindLine, Signal: Traces, Where: rpcServer, Series: []Series{{Label: "requests", Agg: AggCount}}, GroupBy: []string{FieldService}, Unit: UnitCount, Width: 12, Height: 7},
		{ID: "error-rate", Title: "Error rate per service", Description: "Server faults (span status Error) over all requests. Client errors such as NotFound are not counted.", Kind: KindLine, Signal: Traces, Where: rpcServer, Series: errorRatioSeries(), Ratio: true, GroupBy: []string{FieldService}, Unit: UnitPercent, Width: 12, Height: 7},
		{ID: "latency", Title: "Latency p50 / p95 / p99", Kind: KindLine, Signal: Traces, Where: rpcServer, Series: latencySeries(), Unit: UnitMillis, Width: 24, Height: 7},
		{ID: "red-by-service", Title: "Rate, errors and latency by service", Kind: KindTable, Signal: Traces, Where: rpcServer, Series: redTableSeries(), GroupBy: []string{FieldService}, Width: 24, Height: 8},
		{ID: "top-errors", Title: "Top errors", Kind: KindTable, Signal: Traces, Where: scoped(rpcServer, errorSpans), Series: []Series{{Label: "count", Agg: AggCount}}, GroupBy: []string{"span.name", "span.status_message"}, Width: 24, Height: 8},
	}
	if in.HasDatabase {
		d.Panels = append(d.Panels, dbPanels(24)...)
	}
	d.Panels = append(d.Panels,
		Panel{ID: "error-logs", Title: "Recent error logs", Kind: KindSearch, Signal: Logs, Where: []Predicate{errorLogs}, Columns: logColumns, Width: 24, Height: 9},
	)
	return d
}

func dbPanels(width int) []Panel {
	dbSpans := []Predicate{
		{Field: "span.kind", Op: OpEq, Value: "client"},
		{Field: "span.attr.db.system", Op: OpExists},
	}
	half := width / 2
	return []Panel{
		{ID: "db-latency", Title: "Database call latency", Description: "Client spans carrying db.system (the ORM's query hook).", Kind: KindLine, Signal: Traces, Where: dbSpans, Series: latencySeries(), Unit: UnitMillis, Width: half, Height: 7},
		{ID: "db-by-operation", Title: "Database calls by operation", Kind: KindTable, Signal: Traces, Where: dbSpans, Series: []Series{
			{Label: "calls", Agg: AggCount},
			{Label: "errors", Agg: AggCount, Where: []Predicate{errorSpans}},
			{Label: "p95", Agg: AggQuantile, Quantile: 0.95, Field: "span.duration"},
		}, GroupBy: []string{"span.attr.db.operation"}, Width: half, Height: 7},
	}
}

func buildService(in Input, s Service) Dashboard {
	prefix := Predicate{Field: "span.name", Op: OpPrefix, Value: s.Package + "." + s.ProtoService + "/"}
	base := scoped(rpcServer, prefix)
	d := Dashboard{
		ID:          "service-" + s.Name,
		Title:       fmt.Sprintf("%s: %s service", in.Project, s.Name),
		Description: fmt.Sprintf("RED per rpc of %s.%s (server spans named %s.%s/<Method>), Go runtime and logs. The Go runtime panels describe the PROCESS that serves this service: narrow with the service.name filter.", s.Package, s.ProtoService, s.Package, s.ProtoService),
		Filters:     commonFilters(),
	}
	d.Panels = []Panel{
		{ID: "requests", Title: "Requests", Kind: KindNumber, Signal: Traces, Where: base, Series: []Series{{Label: "requests", Agg: AggCount}}, Unit: UnitCount, Width: 8, Height: 4},
		{ID: "errors", Title: "Server errors", Kind: KindNumber, Signal: Traces, Where: scoped(base, errorSpans), Series: []Series{{Label: "errors", Agg: AggCount}}, Unit: UnitCount, Width: 8, Height: 4},
		{ID: "p95", Title: "p95 latency", Kind: KindNumber, Signal: Traces, Where: base, Series: []Series{{Label: "p95", Agg: AggQuantile, Quantile: 0.95, Field: "span.duration"}}, Unit: UnitMillis, Width: 8, Height: 4},
		{ID: "red-by-rpc", Title: "Rate, errors and latency by rpc", Kind: KindTable, Signal: Traces, Where: base, Series: redTableSeries(), GroupBy: []string{"span.name"}, Width: 24, Height: 8},
	}
	if len(s.Methods) <= maxPerRPCRows {
		methods := append([]Method(nil), s.Methods...)
		sort.Slice(methods, func(i, j int) bool { return methods[i].Name < methods[j].Name })
		for _, m := range methods {
			w := scoped(rpcServer, Predicate{Field: "span.name", Op: OpEq, Value: procedure(s, m)})
			d.Panels = append(d.Panels,
				Panel{ID: "rpc-" + m.Name + "-rate", Title: m.Name + ": requests", Kind: KindLine, Signal: Traces, Where: w, Series: []Series{{Label: "requests", Agg: AggCount}}, Unit: UnitCount, Width: 8, Height: 6},
				Panel{ID: "rpc-" + m.Name + "-errors", Title: m.Name + ": error rate", Kind: KindLine, Signal: Traces, Where: w, Series: errorRatioSeries(), Ratio: true, Unit: UnitPercent, Width: 8, Height: 6},
				Panel{ID: "rpc-" + m.Name + "-latency", Title: m.Name + ": latency", Kind: KindLine, Signal: Traces, Where: w, Series: latencySeries(), Unit: UnitMillis, Width: 8, Height: 6},
			)
		}
	}
	if in.HasDatabase {
		d.Panels = append(d.Panels, dbPanels(24)...)
	}
	d.Panels = append(d.Panels, runtimePanels()...)
	d.Panels = append(d.Panels,
		Panel{ID: "error-logs", Title: "Error logs", Kind: KindSearch, Signal: Logs, Where: append(serviceLogScope(s), errorLogs), Columns: logColumns, Width: 24, Height: 9},
		Panel{ID: "logs", Title: "Logs", Description: "Every record whose rpc procedure belongs to this service.", Kind: KindSearch, Signal: Logs, Where: serviceLogScope(s), Columns: logColumns, Width: 24, Height: 9},
	)
	return d
}

// runtimePanels are the Go runtime metrics the OTel runtime instrumentation
// exports (go.opentelemetry.io/contrib/instrumentation/runtime), a subset of
// HyperDX's own Go Runtime template.
func runtimePanels() []Panel {
	return []Panel{
		{ID: "go-memory", Title: "Go memory used", Kind: KindLine, Signal: Metrics, Series: []Series{{Label: "used", Agg: AggSum, Field: "go.memory.used", MetricType: "sum"}, {Label: "gc goal", Agg: AggAvg, Field: "go.memory.gc.goal", MetricType: "sum"}}, Unit: UnitBytes, Width: 12, Height: 7},
		{ID: "go-goroutines", Title: "Goroutines", Kind: KindLine, Signal: Metrics, Series: []Series{{Label: "goroutines", Agg: AggSum, Field: "go.goroutine.count", MetricType: "sum"}}, Unit: UnitCount, Width: 12, Height: 7},
		{ID: "go-alloc", Title: "Heap allocation", Description: "Bytes allocated to the heap per time bucket: the rate the garbage collector has to keep up with.", Kind: KindLine, Signal: Metrics, Series: []Series{{Label: "allocated", Agg: AggSum, Field: "go.memory.allocated", MetricType: "sum"}}, Unit: UnitBytes, Width: 24, Height: 7},
	}
}

var (
	browserSpans = []Predicate{{Field: "resource.attr.rum.sessionId", Op: OpExists}}
	pageLoads    = Predicate{Field: "span.name", Op: OpIn, Values: []string{"documentLoad", "routeChange"}}
	ajax         = Predicate{Field: "span.attr.component", Op: OpIn, Values: []string{"fetch", "xml-http-request"}}
	notAjax      = Predicate{Field: "span.attr.component", Op: OpNotIn, Values: []string{"fetch", "xml-http-request"}}
	ajaxFailed   = Predicate{Field: "span.attr.http.status_code", Op: OpGTE, Value: "400"}
)

func buildFrontend(in Input) Dashboard {
	d := Dashboard{
		ID:          "frontend",
		Title:       in.Project + ": frontend",
		Description: fmt.Sprintf("Browser errors, page loads, web vitals and API calls from the HyperDX browser SDK (service.name %s). Panels follow HyperDX's Browser RUM template: a browser span carries the rum.sessionId resource attribute.", FrontendServiceName(in.Project)),
		Filters:     commonFilters(),
	}
	js := scoped(browserSpans, errorSpans, notAjax)
	loads := scoped(browserSpans, Predicate{Field: "span.name", Op: OpEq, Value: "documentLoad"})
	vital := func(id, title, key string, mantissa Unit) Panel {
		return Panel{ID: id, Title: title, Kind: KindNumber, Signal: Traces,
			Where:  scoped(browserSpans, Predicate{Field: "span.name", Op: OpEq, Value: "webvitals"}, Predicate{Field: "span.attr." + key, Op: OpExists}),
			Series: []Series{{Label: key + " p75", Agg: AggQuantile, Quantile: 0.75, Field: "span.attr." + key}}, Unit: mantissa, Width: 6, Height: 4}
	}
	d.Panels = []Panel{
		{ID: "js-errors", Title: "JS errors", Kind: KindNumber, Signal: Traces, Where: js, Series: []Series{{Label: "errors", Agg: AggCount}}, Unit: UnitCount, Width: 6, Height: 4},
		{ID: "sessions", Title: "Sessions", Kind: KindNumber, Signal: Traces, Where: browserSpans, Series: []Series{{Label: "sessions", Agg: AggCountDistinct, Field: "resource.attr.rum.sessionId"}}, Unit: UnitCount, Width: 6, Height: 4},
		{ID: "page-views", Title: "Page views", Kind: KindNumber, Signal: Traces, Where: scoped(browserSpans, pageLoads), Series: []Series{{Label: "views", Agg: AggCount}}, Unit: UnitCount, Width: 6, Height: 4},
		{ID: "api-failures", Title: "Failed API calls", Description: "fetch / XHR spans with an HTTP status of 400 or more.", Kind: KindNumber, Signal: Traces, Where: scoped(browserSpans, ajax, ajaxFailed), Series: []Series{{Label: "failures", Agg: AggCount}}, Unit: UnitCount, Width: 6, Height: 4},
		{ID: "errors-over-time", Title: "JS errors over time", Kind: KindLine, Signal: Traces, Where: js, Series: []Series{{Label: "errors", Agg: AggCount}}, Unit: UnitCount, Width: 24, Height: 6},
		{ID: "top-js-errors", Title: "Top JS errors", Kind: KindTable, Signal: Traces, Where: js, Series: []Series{{Label: "occurrences", Agg: AggCount}, {Label: "sessions", Agg: AggCountDistinct, Field: "resource.attr.rum.sessionId"}}, GroupBy: []string{"span.attr.error.message"}, Width: 24, Height: 8},
		vital("lcp", "LCP p75", "lcp", UnitMillis),
		vital("inp", "INP p75", "inp", UnitMillis),
		vital("cls", "CLS p75", "cls", UnitNone),
		vital("fcp", "FCP p75", "fcp", UnitMillis),
		vital("ttfb", "TTFB p75", "ttfb", UnitMillis),
		{ID: "page-load", Title: "Page load p50 / p90 / p99", Kind: KindLine, Signal: Traces, Where: loads, Series: []Series{
			{Label: "p50", Agg: AggQuantile, Quantile: 0.5, Field: "span.duration"},
			{Label: "p90", Agg: AggQuantile, Quantile: 0.9, Field: "span.duration"},
			{Label: "p99", Agg: AggQuantile, Quantile: 0.99, Field: "span.duration"},
		}, Unit: UnitMillis, Width: 24, Height: 7},
		{ID: "api-latency", Title: "API call latency (browser)", Description: "Browser-side fetch / XHR spans; the same calls appear server-side on the service dashboards, joined by traceparent.", Kind: KindLine, Signal: Traces, Where: scoped(browserSpans, ajax), Series: latencySeries(), Unit: UnitMillis, Width: 12, Height: 7},
		{ID: "api-calls", Title: "API calls by URL", Kind: KindTable, Signal: Traces, Where: scoped(browserSpans, ajax), Series: []Series{
			{Label: "calls", Agg: AggCount},
			{Label: "failures", Agg: AggCount, Where: []Predicate{ajaxFailed}},
			{Label: "p95", Agg: AggQuantile, Quantile: 0.95, Field: "span.duration"},
		}, GroupBy: []string{"span.attr.http.url"}, Width: 12, Height: 7},
		{ID: "browser-console", Title: "Browser console and error events", Description: "The HyperDX SDK ships console output and uncaught errors as spans (console.*, onerror, unhandledrejection), not as OTLP logs.", Kind: KindSearch, Signal: Traces, Where: scoped(browserSpans, Predicate{Field: "span.name", Op: OpIn, Values: []string{"console.error", "console.warn", "onerror", "unhandledrejection"}}), Columns: []string{"timestamp", FieldService, "span.name", "span.attr.error.message"}, Width: 24, Height: 9},
	}
	return d
}

func buildAlerts(overview Dashboard) []Alert {
	const note = "Not provisioned: HyperDX 2.40 never evaluates an alert declared in a provisioned file (hyperdx PR #3233). Create it through the HyperDX alerts API."
	ref := func(panel string) string { return overview.ID + "/" + panel }
	return []Alert{
		{ID: "error-rate", Title: "Error rate above 5%", Panel: ref("error-rate"), Interval: "5m", ThresholdType: "above", Threshold: 0.05, Status: AlertPending, Note: note},
		{ID: "p99-latency", Title: "p99 latency above 1s", Panel: ref("p99"), Interval: "5m", ThresholdType: "above", Threshold: 1000, Status: AlertPending, Note: note},
		{ID: "no-data", Title: "No requests received", Panel: ref("requests"), Interval: "15m", ThresholdType: "below", Threshold: 1, Status: AlertPending, Note: note},
	}
}

// panelByRef resolves "<dashboard id>/<panel id>".
func panelByRef(s Spec, ref string) (Panel, bool) {
	dash, panel, ok := strings.Cut(ref, "/")
	if !ok {
		return Panel{}, false
	}
	for _, d := range s.Dashboards {
		if d.ID != dash {
			continue
		}
		for _, p := range d.Panels {
			if p.ID == panel {
				return p, true
			}
		}
	}
	return Panel{}, false
}
