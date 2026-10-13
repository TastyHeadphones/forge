package generator

import (
	"bytes"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/reliant-labs/forge/internal/checksums"
	"github.com/reliant-labs/forge/internal/config"
	"github.com/reliant-labs/forge/internal/naming"
)

// ormOutput is one file the ORM projection is about to write into
// internal/db, rendered but not yet written.
type ormOutput struct {
	file   string // base name, e.g. "task_orm_gen.go"
	entity string // "" for the store file, which serves every entity
	src    []byte
}

// ormNameCollision is one package-level name the ORM would declare that a
// file the project owns in internal/db already declares.
type ormNameCollision struct {
	name   string
	at     string // "internal/db/models.go:119"
	entity string
	table  string
	file   string // the ORM file that declares it
}

// checkORMNameCollisions refuses an ORM projection that would redeclare a name
// the project already declares in internal/db, before anything is written.
//
// internal/db is ONE Go package. forge projects each entity into it — the
// struct (`Task`), its delegates (`CreateTask`, `ListTask`, …), its
// `TaskStore` and the aggregate `Store`/`NewStore` — and a project adopting
// forge usually already has a data layer there. reliant's `type Task =
// core.Task` alias is the measured case: the projection wrote task_orm_gen.go
// beside it, the validate build failed on `Task redeclared in this block`,
// and generate reverted everything it wrote, naming neither the declaration
// nor the way out.
//
// The ORM's package and names stay fixed (one convention every generated
// consumer and owned file relies on); what this adds is the answer. Each
// collision names the project's declaration and the entity behind it, and the
// error names the three ways to resolve it: keep that table off forge's ORM
// (`// forge:custom` on its CRUD-named rpcs, the per-entity opt-out — the
// incremental path off a hand-written or sqlc data layer), hand the name to
// forge, or opt the whole package out with `//forge:no-orm`.
//
// A file the ORM itself owns is not the project's: this run's outputs, stale
// ORM files the sweep reclaims, and the retired forge-written types.go.
// Internal test files (package db) share the namespace and are read too.
func checkORMNameCollisions(dbDir string, entities []config.PlanEntity, outputs []ormOutput) error {
	declaredBy := map[string]ormOutput{} // name → the ORM output declaring it
	for _, out := range outputs {
		for _, name := range packageLevelNames(out.src) {
			if _, seen := declaredBy[name]; !seen {
				declaredBy[name] = out
			}
		}
	}
	tables := map[string]string{}
	for _, ent := range entities {
		table := ent.TableName
		if table == "" {
			table = naming.Pluralize(naming.ToSnakeCase(ent.Name))
		}
		tables[ent.Name] = table
	}

	entries, err := os.ReadDir(dbDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("read %s: %w", dbDir, err)
	}
	var collisions []ormNameCollision
	fset := token.NewFileSet()
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") {
			continue
		}
		path := filepath.Join(dbDir, name)
		body, rerr := os.ReadFile(path)
		if rerr != nil || ormOwnsFile(name, body) {
			continue
		}
		file, perr := parser.ParseFile(fset, path, body, parser.SkipObjectResolution)
		if perr != nil || strings.HasSuffix(file.Name.Name, "_test") {
			continue // a broken file is the validate build's to report; an external test package shares no names
		}
		for _, d := range packageLevelDecls(file) {
			out, hit := declaredBy[d.Name]
			if !hit {
				continue
			}
			collisions = append(collisions, ormNameCollision{
				name:   d.Name,
				at:     fmt.Sprintf("internal/db/%s:%d", name, fset.Position(d.Pos()).Line),
				entity: out.entity,
				table:  tables[out.entity],
				file:   out.file,
			})
		}
	}
	if len(collisions) == 0 {
		return nil
	}
	sort.Slice(collisions, func(i, j int) bool { return collisions[i].at < collisions[j].at })
	return ormCollisionError(collisions)
}

func ormCollisionError(collisions []ormNameCollision) error {
	var b strings.Builder
	b.WriteString("forge's ORM would redeclare names your internal/db already declares (one Go package, so both cannot compile):\n")
	entities := map[string]string{}
	for _, c := range collisions {
		if c.entity == "" {
			fmt.Fprintf(&b, "  - %s, declared at %s — forge declares it in internal/db/%s for every entity\n", c.name, c.at, c.file)
			continue
		}
		fmt.Fprintf(&b, "  - %s, declared at %s — forge declares it in internal/db/%s for entity %s (table %s)\n", c.name, c.at, c.file, c.entity, c.table)
		entities[c.entity] = c.table
	}
	b.WriteString("Resolve each entity one of three ways:\n")
	if len(entities) > 0 {
		names := make([]string, 0, len(entities))
		for e := range entities {
			names = append(names, e)
		}
		sort.Strings(names)
		for _, e := range names {
			fmt.Fprintf(&b, "  - keep %s off forge's ORM while your code owns it: put `// forge:custom` above each of its CRUD-named rpcs "+
				"(Create%s, Get%s, Update%s, Delete%s, List%s) — with none of them CRUD, %s is plain schema and forge projects nothing for it;\n",
				entities[e], e, e, e, e, naming.Pluralize(e), entities[e])
		}
	} else {
		b.WriteString("  - keep the tables off forge's ORM while your code owns them: put `// forge:custom` above each of their CRUD-named rpcs;\n")
	}
	b.WriteString("  - hand the name to forge: delete or rename your declaration (sqlc output: a `rename:` in sqlc.yaml) and use the generated one;\n")
	b.WriteString("  - or opt the whole data layer out with `//forge:no-orm: <why>` in internal/db's package doc.")
	return fmt.Errorf("%s", b.String())
}

// ormOwnsFile reports whether an internal/db file is the ORM's own output
// rather than the project's: a current or retired ORM file name (this run
// rewrites or sweeps it), or the proto-era types.go when forge wrote it.
func ormOwnsFile(name string, body []byte) bool {
	switch {
	case strings.HasSuffix(name, "_orm_gen.go"), strings.HasSuffix(name, "_orm.go"),
		name == storeGenFile, name == "orm_shared.go", name == "orm_shared_gen.go":
		return true
	case name == "types.go":
		return forgeWroteFile(body)
	}
	return false
}

// forgeWroteFile reports whether body is forge output: a verifying forge:hash
// marker, or (for output older than the marker) forge's generated banner. A
// marked file someone has since edited is theirs.
func forgeWroteFile(body []byte) bool {
	switch checksums.Verify(body) {
	case checksums.Pristine:
		return true
	case checksums.NoMarker:
		return bytes.Contains(body, []byte("Code generated by forge"))
	}
	return false
}

// packageLevelNames returns the package-level identifiers src declares.
func packageLevelNames(src []byte) []string {
	file, err := parser.ParseFile(token.NewFileSet(), "", src, parser.SkipObjectResolution)
	if err != nil {
		return nil
	}
	var names []string
	for _, id := range packageLevelDecls(file) {
		names = append(names, id.Name)
	}
	return names
}

// packageLevelDecls returns the identifiers file declares at package scope:
// types, vars, consts and receiver-less funcs. Methods, `_` and init are not
// package-scope names.
func packageLevelDecls(file *ast.File) []*ast.Ident {
	var ids []*ast.Ident
	keep := func(id *ast.Ident) {
		if id != nil && id.Name != "_" {
			ids = append(ids, id)
		}
	}
	for _, decl := range file.Decls {
		switch d := decl.(type) {
		case *ast.FuncDecl:
			if d.Recv == nil && d.Name.Name != "init" {
				keep(d.Name)
			}
		case *ast.GenDecl:
			for _, spec := range d.Specs {
				switch s := spec.(type) {
				case *ast.TypeSpec:
					keep(s.Name)
				case *ast.ValueSpec:
					for _, n := range s.Names {
						keep(n)
					}
				}
			}
		}
	}
	return ids
}
