// File: internal/cli/db_migrate_kit.go
//
// `forge db migrate` runs forge's migrator — forge/pkg/migratekit, the package
// a project's own `<binary> db migrate up` and a server's AutoMigrate run —
// over the project's migrations directory.
//
// It used to shell out to the golang-migrate CLI, which applies SQL and keeps
// one version number. Every check migratekit adds on top of golang-migrate was
// skipped on that path, and each one mattered:
//
//   - the run lock, which serializes a migration run against a server booting
//     with AUTO_MIGRATE and against another replica;
//   - the applied record and its refusals: a version applied from a different
//     file, and a version the schema passed without applying it. Worse, a
//     version the CLI applied got no record row, so a server's next
//     AutoMigrate refused it as MISSING;
//   - adoption of a database goose migrated (`-- forge:adopts`): the CLI ran
//     the baseline's SQL against goose's schema and failed on its first
//     CREATE TABLE, leaving the database dirty;
//   - the schema-ahead verdict for a binary older than its schema.
//
// The output and refusals follow the scaffolded `<binary> db migrate up`
// (internal/templates/project/cmd-tree-db.go.tmpl), so one database reports
// the same facts whichever command migrated it.

package cli

import (
	"fmt"
	"io"
	"os"
	"strconv"

	"github.com/reliant-labs/forge/pkg/migratekit"
	"github.com/reliant-labs/forge/pkg/migratekit/compat"
	"github.com/reliant-labs/forge/pkg/seedplan"
)

// projectMigrations lists the migrations in migDir, in version order, as
// migratekit reads them.
func projectMigrations(migDir string) ([]compat.Migration, error) {
	return compat.Scan(os.DirFS(migDir), ".")
}

// withProjectMigrator opens migratekit over migDir, runs fn and closes it on
// every path. A close error is a warning on w: the command's own outcome has
// already been decided, and turning a failed cleanup into a non-zero exit
// would fail a run whose schema change landed.
func withProjectMigrator(w io.Writer, dsn, migDir string, fn func(*migratekit.Migrator) error) error {
	m, err := migratekit.Open(migratekit.Options{FS: os.DirFS(migDir), DSN: dsn})
	if err != nil {
		return err
	}
	defer func() {
		if cerr := m.Close(); cerr != nil {
			_, _ = fmt.Fprintf(w, "warning: close migrator: %v\n", cerr)
		}
	}()
	return fn(m)
}

// runMigrateUp applies every pending migration in migDir.
//
// A schema that is ALREADY dirty is refused before anything runs, with forge's
// recovery path (dirtyMigrationMessage) rather than golang-migrate's "Dirty
// database version N. Fix and force version." Checking first is what keeps
// that advice off a migration that is simply broken: golang-migrate marks the
// schema dirty as it fails, and forcing past SQL that never ran is worse than
// the raw error.
func runMigrateUp(w io.Writer, dsn, migDir string) error {
	ms, err := projectMigrations(migDir)
	if err != nil {
		return err
	}
	if len(ms) == 0 {
		_, _ = fmt.Fprintf(w, "no migrations in %s — nothing to apply (write one with `forge db migration new <name>`)\n", migDir)
		return nil
	}
	return withProjectMigrator(w, dsn, migDir, func(m *migratekit.Migrator) error {
		state, err := m.State()
		if err != nil {
			return err
		}
		if state.Dirty {
			return fmt.Errorf("%s", dirtyMigrationMessage(&seedplan.MigrationBlock{
				Dirty:   true,
				Version: strconv.FormatUint(uint64(state.Version), 10),
			}, "up"))
		}
		res, err := m.Up()
		if err != nil {
			return fmt.Errorf("migrate up: %w", err)
		}
		reportMigrateUp(w, res)
		return nil
	})
}

// reportMigrateUp prints what an Up did, in the scaffolded binary's words.
func reportMigrateUp(w io.Writer, res migratekit.Result) {
	if a := res.Adopted; a != nil {
		_, _ = fmt.Fprintf(w, "ADOPTED a database %s migrated: %s recorded as applied without running its SQL "+
			"(%d %s versions verified; %s's own table left in place)\n", a.Tool, a.Name, a.Versions, a.Tool, a.Tool)
	}
	if a := res.Ahead; a != nil {
		_, _ = fmt.Fprintf(w, "SCHEMA AHEAD OF THESE MIGRATIONS: database is at version %d, the directory holds up to %d; "+
			"version(s) %v were declared backward-compatible — nothing applied\n", a.Version, a.Latest, a.Versions)
		return
	}
	if !res.Changed {
		_, _ = fmt.Fprintf(w, "no pending migrations (version=%d)\n", res.After.Version)
		return
	}
	if !res.Before.Applied {
		_, _ = fmt.Fprintf(w, "migrations applied: none -> %d\n", res.After.Version)
		return
	}
	_, _ = fmt.Fprintf(w, "migrations applied: %d -> %d\n", res.Before.Version, res.After.Version)
}

// runMigrateStatus reports the schema's version against the migrations in
// migDir. An unmigrated database is a normal state, not an error.
func runMigrateStatus(w io.Writer, dsn, migDir string) error {
	ms, err := projectMigrations(migDir)
	if err != nil {
		return err
	}
	return withProjectMigrator(w, dsn, migDir, func(m *migratekit.Migrator) error {
		state, err := m.State()
		if err != nil {
			return err
		}
		switch {
		case !state.Applied:
			_, _ = fmt.Fprintln(w, "version: none (no migrations have been applied)")
		case state.Dirty:
			_, _ = fmt.Fprintf(w, "version: %d, DIRTY (it failed part-way; `forge db migrate up` prints the recovery)\n", state.Version)
		default:
			_, _ = fmt.Fprintf(w, "version: %d\n", state.Version)
		}
		if len(ms) == 0 {
			_, _ = fmt.Fprintf(w, "latest:  none (%s holds no migrations)\n", migDir)
			return nil
		}
		pending := 0
		for _, mig := range ms {
			if !state.Applied || mig.Version > state.Version {
				pending++
			}
		}
		latest := ms[len(ms)-1].Version
		_, _ = fmt.Fprintf(w, "latest:  %d (%s)\npending: %d\n", latest, migDir, pending)
		if state.Applied && state.Version > latest {
			_, _ = fmt.Fprintf(w, "the schema is AHEAD of %s: `forge db migrate up` reports whether that is safe\n", migDir)
		}
		return nil
	})
}

// runMigrateVersion prints the schema's version alone — `N`, or `N (dirty)` —
// for a script to read. No migration applied is an error, as it was from the
// golang-migrate CLI, so a script never reads a version that does not exist.
func runMigrateVersion(w io.Writer, dsn, migDir string) error {
	return withProjectMigrator(w, dsn, migDir, func(m *migratekit.Migrator) error {
		state, err := m.State()
		if err != nil {
			return err
		}
		if !state.Applied {
			return fmt.Errorf("no migration has been applied")
		}
		if state.Dirty {
			_, _ = fmt.Fprintf(w, "%d (dirty)\n", state.Version)
			return nil
		}
		_, _ = fmt.Fprintf(w, "%d\n", state.Version)
		return nil
	})
}

// runMigrateForce records version as the schema's, clean, without running
// SQL. migratekit keeps its applied record in step (Migrator.Force), which is
// what lets the `force` → `up` recovery through: forced by golang-migrate
// alone, the version had no record row and the next Up refused it as MISSING.
// -1 is golang-migrate's "no version".
func runMigrateForce(w io.Writer, dsn, migDir, arg string) error {
	version, err := strconv.Atoi(arg)
	if err != nil || version < -1 {
		return fmt.Errorf("force: %q is not a migration version (an integer, or -1 for none)", arg)
	}
	return withProjectMigrator(w, dsn, migDir, func(m *migratekit.Migrator) error {
		if err := m.Force(version); err != nil {
			return err
		}
		_, _ = fmt.Fprintf(w, "forced version %d: the dirty flag is cleared and no SQL was run\n", version)
		return nil
	})
}
