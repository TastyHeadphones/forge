package obsdash

import (
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

var update = flag.Bool("update", false, "rewrite the golden files")

func fixture() Input {
	return Input{
		Project: "shop",
		Services: []Service{
			{Name: "orders", Package: "orders.v1", ProtoService: "OrdersService", Methods: []Method{{Name: "CreateOrder"}, {Name: "GetOrder"}, {Name: "ListOrders"}}},
			{Name: "billing", Package: "billing.v1", ProtoService: "BillingService", Methods: []Method{{Name: "Charge"}}},
		},
		Frontends:   []string{"web"},
		HasDatabase: true,
	}
}

func golden(t *testing.T, name string, got []byte) {
	t.Helper()
	path := filepath.Join("testdata", name)
	if *update {
		if err := os.WriteFile(path, got, 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("%v (run with -update)", err)
	}
	if string(want) != string(got) {
		t.Errorf("%s differs from the golden file; run `go test ./internal/obsdash -update` and review the diff", name)
	}
}

func TestGoldenFixtureProject(t *testing.T) {
	sp := Build(fixture())
	files, err := RenderHyperDX(sp)
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range files {
		golden(t, f.File, f.Content)
	}
	b, _ := json.MarshalIndent(sp, "", "  ")
	golden(t, "dashboard-spec.json", append(b, '\n'))
}

func TestDashboardSetFollowsTheProject(t *testing.T) {
	sp := Build(fixture())
	var ids []string
	for _, d := range sp.Dashboards {
		ids = append(ids, d.ID)
	}
	if got, want := strings.Join(ids, ","), "overview,service-billing,service-orders,frontend"; got != want {
		t.Fatalf("dashboards = %s, want %s", got, want)
	}

	in := fixture()
	in.Frontends, in.HasDatabase = nil, false
	sp = Build(in)
	for _, d := range sp.Dashboards {
		if d.ID == "frontend" {
			t.Error("a project with no frontend got a frontend dashboard")
		}
		for _, p := range d.Panels {
			if strings.HasPrefix(p.ID, "db-") {
				t.Errorf("%s/%s: database panel without a database", d.ID, p.ID)
			}
		}
	}
}

func TestPerRPCPanelsUseTheProcedureTheInterceptorNamesSpans(t *testing.T) {
	sp := Build(fixture())
	var orders Dashboard
	for _, d := range sp.Dashboards {
		if d.ID == "service-orders" {
			orders = d
		}
	}
	files, _ := RenderHyperDX(sp)
	var raw string
	for _, f := range files {
		if f.File == "shop-service-orders_gen.json" {
			raw = string(f.Content)
		}
	}
	for _, m := range []string{"CreateOrder", "GetOrder", "ListOrders"} {
		// otelconnect names the server span "<package>.<Service>/<Method>"
		// (no leading slash).
		if !strings.Contains(raw, `SpanName = 'orders.v1.OrdersService/`+m+`'`) {
			t.Errorf("no tile selects the span %q", "orders.v1.OrdersService/"+m)
		}
		for _, suffix := range []string{"-rate", "-errors", "-latency"} {
			if _, ok := findPanel(orders, "rpc-"+m+suffix); !ok {
				t.Errorf("missing panel rpc-%s%s", m, suffix)
			}
		}
	}
	if strings.Contains(raw, "SpanName = '/") || strings.Contains(raw, "startsWith(SpanName, '/") {
		t.Error("span names carry no leading slash (otelconnect trims connect.Spec.Procedure); log `procedure` values do")
	}
	if !strings.Contains(raw, "LogAttributes['procedure'], '/orders.v1.OrdersService/'") {
		t.Error("the log scope must match the logging interceptor's procedure, which keeps the leading slash")
	}
}

func findPanel(d Dashboard, id string) (Panel, bool) {
	for _, p := range d.Panels {
		if p.ID == id {
			return p, true
		}
	}
	return Panel{}, false
}

func TestEveryDashboardFiltersOnServiceAndEnvironment(t *testing.T) {
	for _, d := range Build(fixture()).Dashboards {
		got := map[string]bool{}
		for _, f := range d.Filters {
			got[f.Field] = true
		}
		if !got[FieldService] || !got[FieldEnv] {
			t.Errorf("%s: filters %v lack service.name and deployment.environment.name", d.ID, d.Filters)
		}
	}
}

func TestAlertsAreRecordedAsPendingAndReferenceRealPanels(t *testing.T) {
	sp := Build(fixture())
	if len(sp.Alerts) != 3 {
		t.Fatalf("alerts = %d, want error-rate, p99 and no-data", len(sp.Alerts))
	}
	for _, a := range sp.Alerts {
		if a.Status != AlertPending {
			t.Errorf("%s: status %q, want pending", a.ID, a.Status)
		}
		if _, ok := panelByRef(sp, a.Panel); !ok {
			t.Errorf("%s references missing panel %q", a.ID, a.Panel)
		}
	}
	files, _ := RenderHyperDX(sp)
	for _, f := range files {
		if strings.Contains(string(f.Content), `"alert"`) {
			t.Errorf("%s embeds an alert; provisioned alerts never fire in %s and must not be faked", f.File, PinnedHyperDX)
		}
	}
}

func TestRenderingIsDeterministic(t *testing.T) {
	a, _ := RenderHyperDX(Build(fixture()))
	in := fixture()
	in.Services[0], in.Services[1] = in.Services[1], in.Services[0]
	b, _ := RenderHyperDX(Build(in))
	if len(a) != len(b) {
		t.Fatal("different file sets")
	}
	for i := range a {
		if a[i].File != b[i].File || string(a[i].Content) != string(b[i].Content) {
			t.Errorf("%s differs when services are declared in another order", a[i].File)
		}
	}
}

func TestQuotingCannotBreakOutOfASQLString(t *testing.T) {
	s, err := predicateSQL(Traces, Predicate{Field: "span.name", Op: OpEq, Value: `x' OR 1=1 --`})
	if err != nil {
		t.Fatal(err)
	}
	if s != `SpanName = 'x\' OR 1=1 --'` {
		t.Errorf("got %s", s)
	}
}

func TestUnknownFieldIsAnError(t *testing.T) {
	if _, err := fieldSQL(Metrics, "span.name"); err == nil {
		t.Error("a span field on the metrics signal must be rejected, not rendered")
	}
}
