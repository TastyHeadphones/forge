package cli

import (
	"bytes"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/reliant-labs/forge/internal/assets"
)

// vendoredImport is one annotation proto forge copies into a project the
// first time one of the project's protos imports it.
type vendoredImport struct {
	// importPath is the path a project proto imports: "forge/v1/forge.proto".
	importPath string
	// relPath is the project-relative copy buf resolves that import from.
	relPath string
	// protoPackage is the file's proto package: the name a Go plugin's
	// `exclude_types` drops it by.
	protoPackage string
	// write copies forge's embedded file into the copy's directory.
	write func(destDir string) error
}

// vendoredImports is every annotation proto forge vendors on demand: the two
// files internal/assets embeds. Both are buf COMPILE inputs only — their Go
// home is forge/pkg/forgepb and the protovalidate runtime respectively, so the
// Go buf template must never generate them (checkGoTemplateSkipsVendoredProtos).
func vendoredImports() []vendoredImport {
	return []vendoredImport{
		{importPath: assets.ForgeProtoImportPath, relPath: assets.ForgeProtoVendorRelPath,
			protoPackage: "forge.v1", write: assets.WriteForgeV1Proto},
		{importPath: assets.ValidateProtoImportPath, relPath: assets.ValidateProtoVendorRelPath,
			protoPackage: "buf.validate", write: assets.WriteValidateProto},
	}
}

// ensureVendoredProtos vendors each annotation proto a project proto imports
// — forge/v1/forge.proto, buf/validate/validate.proto — into proto/ WHEN, and
// only when, some proto imports it and the project has no copy yet.
//
// A project that uses `(forge.v1.config)` or `[(buf.validate.field)...]`
// needs the definitions on disk for buf to compile, and a project that does
// not stays clean. It runs before every buf invocation, so it is demand-
// driven rather than stamped at birth: `forge project new` writes forge.proto
// into a greenfield tree, but a project that adopted forge in place, or one
// whose only forge-annotated protos are config protos, never got one, and buf
// failed on the import until it was copied in by hand.
//
// An existing copy is never touched: a stale one is `forge lint
// --vendored-protos`' to report (and `forge project disown`'s to keep), so
// this cannot silently overwrite a deliberate customization.
func ensureVendoredProtos(projectDir string) error {
	var missing []vendoredImport
	for _, v := range vendoredImports() {
		if _, err := os.Stat(filepath.Join(projectDir, filepath.FromSlash(v.relPath))); errors.Is(err, fs.ErrNotExist) {
			missing = append(missing, v)
		}
	}
	if len(missing) == 0 {
		return nil
	}
	used, err := protosImporting(projectDir, missing)
	if err != nil {
		return err
	}
	for _, v := range missing {
		if !used[v.importPath] {
			continue
		}
		dest := filepath.Join(projectDir, filepath.FromSlash(v.relPath))
		if err := v.write(filepath.Dir(dest)); err != nil {
			return err
		}
	}
	return nil
}

// protosImporting reports which of the candidates' import paths some .proto
// under projectDir/proto imports. The vendored copies themselves are skipped,
// so a file's own text can never trigger its vendoring.
func protosImporting(projectDir string, candidates []vendoredImport) (map[string]bool, error) {
	root := filepath.Join(projectDir, "proto")
	vendored := map[string]bool{}
	for _, v := range vendoredImports() {
		vendored[v.relPath] = true
	}
	used := map[string]bool{}
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, werr error) error {
		if werr != nil {
			return werr
		}
		if d.IsDir() || !strings.HasSuffix(d.Name(), ".proto") {
			return nil
		}
		if rel, rerr := filepath.Rel(projectDir, path); rerr == nil && vendored[filepath.ToSlash(rel)] {
			return nil
		}
		b, rerr := os.ReadFile(path)
		if rerr != nil {
			return rerr
		}
		for _, v := range candidates {
			if bytes.Contains(b, []byte(`"`+v.importPath+`"`)) {
				used[v.importPath] = true
			}
		}
		if len(used) == len(candidates) {
			return fs.SkipAll
		}
		return nil
	})
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	return used, err
}
