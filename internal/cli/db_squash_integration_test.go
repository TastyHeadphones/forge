//go:build integration

// File: internal/cli/db_squash_integration_test.go
//
// `forge db squash` end to end: squash a history in squash's own ephemeral
// postgres container, then run the baseline and the next migration through
// migratekit in one run (assertSquashedReleaseApplies, in
// db_squash_dump_test.go).
//
// Tagged integration because it runs the real docker: the default build's
// host-tool tripwire (hermetic_host_test.go) refuses that, and rightly — a
// unit test does not start containers on a shared machine. The container is
// squash's own, named forge-db-squash-<nanos>, started with --rm and stopped
// by squash on every exit path.

package cli

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// squashHistory is the history testdata/squash/pg_dump_16.15_inserts.sql was
// dumped from.
var squashHistory = map[string]string{
	"20260101000001_init.up.sql": `CREATE TABLE accounts (id BIGSERIAL PRIMARY KEY, name TEXT NOT NULL, note TEXT);
COMMENT ON TABLE accounts IS 'customer accounts; SET x = 1';
CREATE TYPE plan_kind AS ENUM ('free', 'pro');
CREATE FUNCTION touch() RETURNS trigger LANGUAGE plpgsql AS $fn$
BEGIN
SET LOCAL search_path = public;
  NEW.name := NEW.name || $$x$$;
  NEW.note := coalesce(NEW.note, 'a
\restrict not-a-meta');
  RETURN NEW;
END
$fn$;
CREATE FUNCTION account_count() RETURNS bigint LANGUAGE sql AS $$
SELECT count(*) FROM accounts
$$;
CREATE TRIGGER accounts_touch BEFORE INSERT ON accounts FOR EACH ROW EXECUTE FUNCTION touch();
`,
	"20260101000002_plans.up.sql": `CREATE TABLE plans (id INT PRIMARY KEY, kind plan_kind NOT NULL, account_id BIGINT REFERENCES accounts (id));
INSERT INTO accounts (name, note) VALUES ('acme', E'line one\nSET search_path = evil;\n\\restrict nope\nend');
`,
}

func TestDBSquash_BaselineAndTheNextMigrationApplyInOneMigratekitRun(t *testing.T) {
	if testing.Short() {
		t.Skip("boots a postgres container for squash and a pgtest database")
	}
	if err := exec.Command("docker", "info").Run(); err != nil {
		t.Skipf("no docker daemon: %v", err)
	}
	history := t.TempDir()
	for name, body := range squashHistory {
		if err := os.WriteFile(filepath.Join(history, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	release := t.TempDir()
	if err := runDBSquash(context.Background(), squashOptions{
		FromDir:  history,
		Baseline: "20260102000000_baseline",
		OutDir:   release,
		Image:    "postgres:16-alpine",
		DBName:   "forge_squash",
		DBUser:   "postgres",
		DBPass:   "forge_squash",
	}); err != nil {
		t.Fatalf("squash: %v", err)
	}
	if err := os.WriteFile(filepath.Join(release, "20260103000000_after.up.sql"), []byte(afterSquash), 0o644); err != nil {
		t.Fatal(err)
	}
	assertSquashedReleaseApplies(t, os.DirFS(release))
}
