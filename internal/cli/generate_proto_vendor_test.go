package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/reliant-labs/forge/internal/assets"
	"github.com/reliant-labs/forge/internal/templates"
)

// writeProto writes a project proto at rel (slash-separated, project-relative).
func writeProto(t *testing.T, dir, rel, body string) {
	t.Helper()
	p := filepath.Join(dir, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

const configProtoImportingForge = `syntax = "proto3";
package config.v1;
import "forge/v1/forge.proto";
message ReliantConfig {
  option (forge.v1.binary_config) = {binary: "reliant"};
}
`

func exists(t *testing.T, dir, rel string) bool {
	t.Helper()
	_, err := os.Stat(filepath.Join(dir, filepath.FromSlash(rel)))
	return err == nil
}

// The adopted-project case: a project whose only forge-annotated proto is a
// config proto, and which never got forge.proto from `forge project new`.
// buf failed on the import until the file was copied in by hand; generate now
// vendors it, with the go_package that points at forge/pkg/forgepb.
func TestEnsureVendoredProtos_VendorsForgeProtoForAConfigProto(t *testing.T) {
	dir := t.TempDir()
	writeProto(t, dir, "proto/config/v1/config.proto", configProtoImportingForge)

	if err := ensureVendoredProtos(dir); err != nil {
		t.Fatalf("ensureVendoredProtos: %v", err)
	}
	got, err := os.ReadFile(filepath.Join(dir, filepath.FromSlash(assets.ForgeProtoVendorRelPath)))
	if err != nil {
		t.Fatalf("forge.proto was not vendored: %v", err)
	}
	if !strings.Contains(string(got), `option go_package = "github.com/reliant-labs/forge/pkg/forgepb;forgepb";`) {
		t.Error("vendored forge.proto must point go_package at forge/pkg/forgepb")
	}
	if exists(t, dir, assets.ValidateProtoVendorRelPath) {
		t.Error("validate.proto vendored though nothing imports it")
	}
}

// Each file is vendored on its own import, and an existing copy is never
// rewritten: a stale or customized one is `forge lint --vendored-protos`'
// (and `forge project disown`'s) to deal with.
func TestEnsureVendoredProtos_OnDemandAndNeverOverwrites(t *testing.T) {
	dir := t.TempDir()
	writeProto(t, dir, "proto/services/a/v1/a.proto", `syntax = "proto3";
package services.a.v1;
import "buf/validate/validate.proto";
`)
	if err := ensureVendoredProtos(dir); err != nil {
		t.Fatal(err)
	}
	if !exists(t, dir, assets.ValidateProtoVendorRelPath) || exists(t, dir, assets.ForgeProtoVendorRelPath) {
		t.Fatal("want validate.proto vendored for its import, and forge.proto not")
	}

	writeProto(t, dir, "proto/config/v1/config.proto", configProtoImportingForge)
	custom := "// customized on purpose\n"
	writeProto(t, dir, assets.ValidateProtoVendorRelPath, custom)
	if err := ensureVendoredProtos(dir); err != nil {
		t.Fatal(err)
	}
	if !exists(t, dir, assets.ForgeProtoVendorRelPath) {
		t.Error("forge.proto not vendored once a proto imports it")
	}
	if got, _ := os.ReadFile(filepath.Join(dir, filepath.FromSlash(assets.ValidateProtoVendorRelPath))); string(got) != custom {
		t.Error("an existing vendored copy was overwritten")
	}
}

func TestEnsureVendoredProtos_NoImportNoProtoTree(t *testing.T) {
	dir := t.TempDir()
	if err := ensureVendoredProtos(dir); err != nil {
		t.Fatalf("no proto tree: %v", err)
	}
	writeProto(t, dir, "proto/api/v1/api.proto", "syntax = \"proto3\";\npackage api.v1;\n")
	if err := ensureVendoredProtos(dir); err != nil {
		t.Fatal(err)
	}
	if exists(t, dir, "proto/forge") || exists(t, dir, "proto/buf") {
		t.Error("vendored an annotation proto nothing imports")
	}
}

// vendoredProject is a project holding both vendored protos.
func vendoredProject(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if err := assets.WriteForgeV1Proto(filepath.Join(dir, "proto", "forge", "v1")); err != nil {
		t.Fatal(err)
	}
	if err := assets.WriteValidateProto(filepath.Join(dir, "proto", "buf", "validate")); err != nil {
		t.Fatal(err)
	}
	return dir
}

// Every Go template forge itself writes keeps the vendored protos out of Go
// generation: the scaffold's buf.gen.yaml, the default generate writes when
// a project has none, and the `--buf-plugins=remote` rewrite (which used to
// drop the exclusion and generate gen/forge/v1/forge.pb.go — a second
// registration of the descriptor forgepb already registers).
func TestForgeWrittenGoTemplatesSkipVendoredProtos(t *testing.T) {
	scaffold, err := templates.ProjectTemplates().Get("buf.gen.yaml")
	if err != nil {
		t.Fatal(err)
	}
	written := map[string][]byte{"scaffold template": scaffold}

	def := t.TempDir()
	if err := writeDefaultBufGenYaml(def); err != nil {
		t.Fatal(err)
	}
	written["writeDefaultBufGenYaml"], _ = os.ReadFile(filepath.Join(def, "buf.gen.yaml"))

	remote := t.TempDir()
	if err := os.WriteFile(filepath.Join(remote, "buf.gen.yaml"), scaffold, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := rewriteBufGenYamlToRemote(remote); err != nil {
		t.Fatal(err)
	}
	written["rewriteBufGenYamlToRemote"], _ = os.ReadFile(filepath.Join(remote, "buf.gen.yaml"))

	dir := vendoredProject(t)
	for name, body := range written {
		if len(body) == 0 {
			t.Fatalf("%s: empty template", name)
		}
		if err := checkGoTemplateSkipsVendoredProtos(dir, "buf.gen.yaml", body); err != nil {
			t.Errorf("%s generates Go for a vendored proto:\n%v", name, err)
		}
	}
}

func TestCheckGoTemplateSkipsVendoredProtos(t *testing.T) {
	const goPlugins = `plugins:
  - local: [go, tool, protoc-gen-go]
    out: gen
  - local: [go, tool, protoc-gen-connect-go]
    out: gen
`
	cases := []struct {
		name, body string
		onlyForge  bool // vendor forge.proto alone
		refused    []string
	}{
		{name: "no inputs targets the workspace", body: "version: v2\n" + goPlugins,
			refused: []string{"proto/forge/v1/forge.proto and proto/buf/validate/validate.proto"}},
		{name: "proto/ without exclusions", body: "version: v2\ninputs:\n  - directory: proto\n" + goPlugins,
			refused: []string{"proto/forge/v1/forge.proto"}},
		{name: "forge excluded, validate not", body: "version: v2\ninputs:\n  - directory: proto\n    exclude_paths: [proto/forge/v1]\n" + goPlugins,
			refused: []string{"generates Go for proto/buf/validate/validate.proto:"}},
		{name: "forge excluded, forge alone vendored", onlyForge: true,
			body: "version: v2\ninputs:\n  - directory: proto\n    exclude_paths: [proto/forge/v1]\n" + goPlugins},
		{name: "both excluded", body: "version: v2\ninputs:\n  - directory: proto\n    exclude_paths: [proto/forge, proto/buf/validate]\n" + goPlugins},
		{name: "input below the vendored dirs", body: "version: v2\ninputs:\n  - directory: proto/services\n" + goPlugins},
		{name: "exclude_types on the Go plugin", body: `version: v2
plugins:
  - local: protoc-gen-go
    out: gen
    exclude_types: [forge.v1, buf]
`},
		{name: "remote Go plugin, pinned", body: `version: v2
plugins:
  - remote: buf.build/protocolbuffers/go:v1.36.0
    out: gen
`, refused: []string{"proto/forge/v1/forge.proto"}},
		{name: "connect-go alone emits nothing for them", body: `version: v2
plugins:
  - local: protoc-gen-connect-go
    out: gen
`},
		{name: "a TypeScript template is not a Go template", body: `version: v2
plugins:
  - local: ./node_modules/.bin/protoc-gen-es
    out: web/src/gen
    include_imports: true
`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := vendoredProject(t)
			if tc.onlyForge {
				if err := os.RemoveAll(filepath.Join(dir, "proto", "buf")); err != nil {
					t.Fatal(err)
				}
			}
			err := checkGoTemplateSkipsVendoredProtos(dir, "buf.gen.yaml", []byte(tc.body))
			if len(tc.refused) == 0 {
				if err != nil {
					t.Fatalf("refused a template that generates no vendored proto:\n%v", err)
				}
				return
			}
			if err == nil {
				t.Fatal("a Go template targeting a vendored proto was not refused")
			}
			for _, want := range append(tc.refused, "already registered", "exclude_paths:", "- proto/forge/v1") {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("refusal missing %q:\n%v", want, err)
				}
			}
		})
	}

	// forge's own repository: proto/forge/v1/forge.proto is the SOURCE of
	// forgepb, and its buf.gen.yaml generates it on purpose.
	own := vendoredProject(t)
	if err := os.WriteFile(filepath.Join(own, "go.mod"), []byte("module github.com/reliant-labs/forge\n\ngo 1.24\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(filepath.Join(own, "proto", "buf")); err != nil {
		t.Fatal(err)
	}
	if err := checkGoTemplateSkipsVendoredProtos(own, "buf.gen.yaml", []byte("version: v2\nplugins:\n  - local: protoc-gen-go\n    out: .\n    opt: [module=github.com/reliant-labs/forge]\n")); err != nil {
		t.Errorf("forge's own repo, which owns forgepb, was refused: %v", err)
	}

	// Nothing vendored, nothing to guard.
	if err := checkGoTemplateSkipsVendoredProtos(t.TempDir(), "buf.gen.yaml", []byte("version: v2\n"+goPlugins)); err != nil {
		t.Errorf("a project with no vendored protos was refused: %v", err)
	}
}
