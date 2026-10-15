package pgtest_test

import (
	"database/sql"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/reliant-labs/forge/pkg/pgtest"
)

func uniqueKey(t *testing.T) string { return fmt.Sprintf("t%d", time.Now().UnixNano()) }

// The schema is built once per key however many callers there are, every caller
// gets its own writable clone, and one caller's writes never reach another's.
func TestNewFromTemplate_BuildsOnceAndClonesIsolated(t *testing.T) {
	if testing.Short() {
		t.Skip("boots real postgres; skipped under -short")
	}
	key := uniqueKey(t)
	var builds atomic.Int32
	build := func(db *sql.DB) error {
		builds.Add(1)
		_, err := db.Exec(`CREATE TABLE seeded (v int); INSERT INTO seeded VALUES (42)`)
		return err
	}

	const n = 6
	dbs := make([]*sql.DB, n)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			db, cleanup, err := pgtest.NewFromTemplate(key, build)
			if err != nil {
				t.Errorf("NewFromTemplate %d: %v", i, err)
				return
			}
			t.Cleanup(cleanup)
			dbs[i] = db
		}()
	}
	wg.Wait()
	if t.Failed() {
		return
	}
	if got := builds.Load(); got != 1 {
		t.Fatalf("schema built %d times, want exactly once", got)
	}
	for i, db := range dbs {
		var v int
		if err := db.QueryRow(`SELECT v FROM seeded`).Scan(&v); err != nil || v != 42 {
			t.Fatalf("clone %d lost the template's rows: v=%d err=%v", i, v, err)
		}
	}
	if _, err := dbs[0].Exec(`INSERT INTO seeded VALUES (7)`); err != nil {
		t.Fatal(err)
	}
	var count int
	if err := dbs[1].QueryRow(`SELECT count(*) FROM seeded`).Scan(&count); err != nil || count != 1 {
		t.Fatalf("a write in clone 0 reached clone 1: count=%d err=%v", count, err)
	}
}

// A build that fails leaves nothing behind: the error comes back, no template is
// published, and the next caller with the same key builds again.
func TestNewFromTemplate_FailedBuildIsNotPublished(t *testing.T) {
	if testing.Short() {
		t.Skip("boots real postgres; skipped under -short")
	}
	key := uniqueKey(t)
	boom := errors.New("boom")
	if _, _, err := pgtest.NewFromTemplate(key, func(*sql.DB) error { return boom }); !errors.Is(err, boom) {
		t.Fatalf("err = %v, want the build error", err)
	}
	db, cleanup, err := pgtest.NewFromTemplate(key, func(db *sql.DB) error {
		_, err := db.Exec(`CREATE TABLE ok (id int)`)
		return err
	})
	if err != nil {
		t.Fatalf("a failed build must not poison the key: %v", err)
	}
	defer cleanup()
	if _, err := db.Exec(`INSERT INTO ok VALUES (1)`); err != nil {
		t.Fatal(err)
	}
}

// The clone is dropped by its cleanup.
func TestNewFromTemplate_CleanupDropsTheClone(t *testing.T) {
	if testing.Short() {
		t.Skip("boots real postgres; skipped under -short")
	}
	key := uniqueKey(t)
	build := func(db *sql.DB) error { _, err := db.Exec(`CREATE TABLE x (id int)`); return err }
	db, cleanup, err := pgtest.NewFromTemplate(key, build)
	if err != nil {
		t.Fatal(err)
	}
	var name string
	if err := db.QueryRow(`SELECT current_database()`).Scan(&name); err != nil {
		t.Fatal(err)
	}
	cleanup()
	other, cleanup2, err := pgtest.NewFromTemplate(key, build)
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup2()
	var n int
	if err := other.QueryRow(`SELECT count(*) FROM pg_database WHERE datname = $1`, name).Scan(&n); err != nil || n != 0 {
		t.Fatalf("clone %s still exists after cleanup (n=%d, err=%v)", name, n, err)
	}
}
