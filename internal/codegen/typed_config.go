package codegen

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// HasConfigProtos reports whether proto/config holds a .proto file — the
// input the config loader generates pkg/config from. A proto/config
// directory with no proto in it (a CLI scaffold leaves an empty
// proto/config/v1) is not one: generating the loader from forge's default
// fields there writes a pkg/config importing gen/config/v1, which nothing
// will ever generate.
func HasConfigProtos(projectDir string) bool {
	found := false
	_ = filepath.WalkDir(filepath.Join(projectDir, "proto", "config"), func(p string, d os.DirEntry, err error) error {
		if err != nil || found {
			return nil //nolint:nilerr // an unreadable tree holds no protos we can use
		}
		if !d.IsDir() && strings.HasSuffix(p, ".proto") {
			found = true
		}
		return nil
	})
	return found
}

// configTypeRE matches a package-level `type Config` declaration.
var configTypeRE = regexp.MustCompile(`(?m)^type\s+Config\b`)

// ProjectHasTypedConfig reports whether the project has the typed config
// a scaffolded component's `Deps.Config *config.Config` names: config
// protos the loader turns into pkg/config, or a pkg/config that already
// declares Config (a CLI's stub, a hand-written one).
//
// A project adopted in place often has neither — its configuration lives
// in its own package. A component scaffolded there takes no Config:
// importing <module>/pkg/config would not compile, and a placeholder
// package configuring nothing is an empty default every caller would have
// to construct and pass. The project adds the field when it has config to
// give.
func ProjectHasTypedConfig(projectDir string) bool {
	if HasConfigProtos(projectDir) {
		return true
	}
	dir := filepath.Join(projectDir, "pkg", "config")
	entries, err := os.ReadDir(dir)
	if err != nil {
		return false
	}
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		if data, rerr := os.ReadFile(filepath.Join(dir, name)); rerr == nil && configTypeRE.Match(data) {
			return true
		}
	}
	return false
}
