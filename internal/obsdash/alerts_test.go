package obsdash

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestAlertInputFollowsTheDashboardInput(t *testing.T) {
	in := AlertInputOf(fixture())
	if in.Project != "shop" || in.FrontendService != "shop-frontend" {
		t.Fatalf("input = %+v", in)
	}
	want := []AlertService{{"billing", "billing.v1.BillingService"}, {"orders", "orders.v1.OrdersService"}}
	if len(in.Services) != 2 || in.Services[0] != want[0] || in.Services[1] != want[1] {
		t.Errorf("services = %+v, want %+v (sorted, rpc-bearing only)", in.Services, want)
	}
	noRPC := fixture()
	noRPC.Services = append(noRPC.Services, Service{Name: "empty", Package: "empty.v1", ProtoService: "EmptyService"})
	noRPC.Frontends = nil
	got := AlertInputOf(noRPC)
	if len(got.Services) != 2 || got.FrontendService != "" {
		t.Errorf("a service with no rpc, or no frontend, leaked into the input: %+v", got)
	}
}

func TestGoldenDefaultAlertSQL(t *testing.T) {
	rules, err := ResolveAlertDefaults(AlertInputOf(fixture()), "dev", nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(rules) != 4 {
		t.Fatalf("got %d default rules, want error-rate, p95-latency, service-silent, frontend-js-errors", len(rules))
	}
	var b strings.Builder
	for _, r := range rules {
		b.WriteString("-- " + r.Alert + " (" + r.ID + ") severity=" + r.Labels["severity"] + " for=" + r.For + " interval=" + r.Interval + "\n")
		b.WriteString(r.Expr + "\n\n")
	}
	golden(t, "alert-defaults-dev.sql", []byte(b.String()))
	catalog, err := MarshalAlertDefaults(AlertInputOf(fixture()))
	if err != nil {
		t.Fatal(err)
	}
	golden(t, "alert-defaults.json", catalog)
}

func TestDefaultsRouteToNoPagerAndAreMetricsBased(t *testing.T) {
	rules, _ := ResolveAlertDefaults(AlertInputOf(fixture()), "dev", nil)
	for _, r := range rules {
		if r.Labels["severity"] == "" {
			t.Errorf("%s has no severity", r.Alert)
		}
		if _, paged := r.Labels["page"]; paged {
			t.Errorf("%s carries a paging label; defaults must never page", r.Alert)
		}
	}
	for _, id := range []string{"error-rate", "p95-latency", "service-silent"} {
		for _, r := range rules {
			if r.ID != id {
				continue
			}
			if strings.Contains(r.Expr, "otel_traces") {
				t.Errorf("%s reads sampled spans; RED must come from the unsampled rpc.server.* metrics", id)
			}
			if !strings.Contains(r.Expr, "otel_metrics_") {
				t.Errorf("%s does not read a metrics table", id)
			}
		}
	}
	for _, r := range rules {
		if r.ID == "frontend-js-errors" && !strings.Contains(r.Expr, "lower(StatusCode) = 'error'") {
			t.Error("the span-based default must count only ERROR spans, which the tail sampler always keeps")
		}
	}
}

func TestDefaultsScopeToTheEnvAndTheProjectsServices(t *testing.T) {
	rules, _ := ResolveAlertDefaults(AlertInputOf(fixture()), "stag'ing", nil)
	for _, r := range rules {
		if !strings.Contains(r.Expr, `'stag\'ing'`) {
			t.Errorf("%s does not scope to the (quoted) env: %s", r.ID, r.Expr)
		}
		if r.ID != "frontend-js-errors" && !strings.Contains(r.Expr, "'orders.v1.OrdersService'") {
			t.Errorf("%s does not scope to the project's rpc services", r.ID)
		}
	}
}

func TestSilentDefaultNeedsSteadyTraffic(t *testing.T) {
	rules, _ := ResolveAlertDefaults(AlertInputOf(fixture()), "dev", nil)
	for _, r := range rules {
		if r.ID != "service-silent" {
			continue
		}
		if !strings.Contains(r.Expr, "uniqExactIf(toStartOfHour(t)") || !strings.Contains(r.Expr, ">= 18") {
			t.Errorf("service-silent must require 18 active hours of the previous 24h inside the SQL: %s", r.Expr)
		}
		if r.For != "0s" {
			t.Errorf("service-silent for = %q: the silence window is in the SQL, so the hold time is 0s", r.For)
		}
	}
}

func TestTuningDisablesRetargetsAndRefusesTypos(t *testing.T) {
	in := AlertInputOf(fixture())
	rules, err := ResolveAlertDefaults(in, "dev", map[string]AlertTuning{
		"error-rate":  {Enabled: false},
		"p95-latency": {Enabled: true, Labels: map[string]string{"severity": "critical"}, For: "15m", Params: map[string]float64{"threshold_seconds": 2.5}},
	})
	if err != nil {
		t.Fatal(err)
	}
	by := map[string]AlertRule{}
	for _, r := range rules {
		by[r.ID] = r
	}
	if _, ok := by["error-rate"]; ok {
		t.Error("a disabled default is still resolved")
	}
	p95 := by["p95-latency"]
	if p95.Labels["severity"] != "critical" || p95.For != "15m" || !strings.Contains(p95.Expr, "value > 2.5") {
		t.Errorf("tuning not applied: %+v", p95)
	}
	for name, tuning := range map[string]map[string]AlertTuning{
		"unknown default": {"nope": {Enabled: true}},
		"unknown param":   {"error-rate": {Enabled: true, Params: map[string]float64{"thresold": 0.1}}},
		"zero threshold":  {"error-rate": {Enabled: true, Params: map[string]float64{"threshold": 0}}},
	} {
		if _, err := ResolveAlertDefaults(in, "dev", tuning); err == nil {
			t.Errorf("%s: want a refusal", name)
		}
	}
}

func TestDefaultsFollowTheProject(t *testing.T) {
	none := AlertInput{Project: "x"}
	if rules, _ := ResolveAlertDefaults(none, "dev", nil); len(rules) != 0 {
		t.Errorf("a project with no service and no frontend has %d defaults", len(rules))
	}
	feOnly := AlertInput{Project: "x", FrontendService: "x-frontend"}
	rules, _ := ResolveAlertDefaults(feOnly, "dev", nil)
	if len(rules) != 1 || rules[0].ID != "frontend-js-errors" {
		t.Errorf("a frontend-only project: %+v", rules)
	}
}

func TestAlertDefaultsFileRoundTrips(t *testing.T) {
	b, err := MarshalAlertDefaults(AlertInputOf(fixture()))
	if err != nil {
		t.Fatal(err)
	}
	got, err := ParseAlertDefaults(b)
	if err != nil {
		t.Fatal(err)
	}
	want, _ := json.Marshal(AlertInputOf(fixture()))
	have, _ := json.Marshal(got)
	if string(want) != string(have) {
		t.Errorf("round trip changed the input:\n%s\n%s", want, have)
	}
	if _, err := ParseAlertDefaults([]byte(`{"version":"forge.alert-defaults/v0"}`)); err == nil {
		t.Error("an unknown file version must be refused")
	}
}
