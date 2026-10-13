package obsdash

import (
	"os"
	"os/exec"
	"strings"
	"testing"
)

// TestDefaultAlertSQLOnClickHouse runs every generated default against a real
// ClickHouse holding the clickhouseexporter's tables, with seeded series for
// each case the SQL must get right: cumulative counters with a restart, delta
// counters, an unsampled error ratio, low traffic, a stopped service and a
// young one. The goldens pin the text; this pins what it MEANS.
//
// Set FORGE_ALERT_CLICKHOUSE to the name of a running container with
// clickhouse-client and the exporter's tables (password "pw"), e.g. the one
// internal/templates' ClickStack gate starts.
func TestDefaultAlertSQLOnClickHouse(t *testing.T) {
	container := os.Getenv("FORGE_ALERT_CLICKHOUSE")
	if testing.Short() || container == "" {
		t.Skip("needs a ClickHouse container; set FORGE_ALERT_CLICKHOUSE=<container>")
	}
	run := func(sql string) string {
		t.Helper()
		cmd := exec.Command("docker", "exec", "-i", container, "clickhouse-client", "--password", "pw",
			"--joined_subquery_requires_alias=0", "--multiquery", "--format", "TSV")
		cmd.Stdin = strings.NewReader(sql)
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("clickhouse: %v\n%s\n--- sql ---\n%s", err, out, sql)
		}
		return strings.TrimSpace(string(out))
	}
	seed, err := os.ReadFile("testdata/clickhouse-seed.sql")
	if err != nil {
		t.Fatal(err)
	}
	run(string(seed))

	in := AlertInput{
		Project: "shop", FrontendService: "shop-frontend",
		Services: []AlertService{
			{"api", "a.v1.S"}, {"quiet", "q.v1.Q"}, {"reset", "r.v1.R"}, {"delta", "d.v1.D"},
			{"fast", "f.v1.F"}, {"stopped", "s.v1.T"}, {"healthy", "h.v1.H"}, {"sparse", "p.v1.P"}, {"young", "y.v1.Y"},
		},
	}
	rules, err := ResolveAlertDefaults(in, "dev", nil)
	if err != nil {
		t.Fatal(err)
	}
	fired := map[string][]string{}
	for _, r := range rules {
		out := run(r.Expr)
		if out != "" {
			for _, line := range strings.Split(out, "\n") {
				fired[r.ID] = append(fired[r.ID], strings.Split(line, "\t")[0])
			}
		}
	}
	want := map[string][]string{
		// api fails 20% of 200 calls, delta 10% of 60. quiet (3 calls in the window), reset (no errors) stay quiet.
		"error-rate": {"api", "delta"},
		// api's p95 sits in the (1s, 5s] bucket; fast's in the first.
		"p95-latency": {"api"},
		// stopped served traffic through most of the last day, then stopped. sparse (4 active hours) and young (2h) do not count as steady; healthy is serving.
		"service-silent":     {"stopped"},
		"frontend-js-errors": {"shop-frontend"},
	}
	for id, w := range want {
		if strings.Join(fired[id], ",") != strings.Join(w, ",") {
			t.Errorf("%s fired for %v, want %v", id, fired[id], w)
		}
	}

	// A different env sees none of dev's data.
	other, _ := ResolveAlertDefaults(in, "prod", nil)
	for _, r := range other {
		if r.ID == "service-silent" || r.ID == "frontend-js-errors" {
			continue
		}
		if out := run(r.Expr); strings.Contains(out, "api") && r.ID != "error-rate" {
			t.Errorf("%s in env prod saw dev's series: %s", r.ID, out)
		}
	}
}
