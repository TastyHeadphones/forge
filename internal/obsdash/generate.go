package obsdash

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/reliant-labs/forge/internal/checksums"
)

const (
	// DashboardsDir is the project-relative directory mounted as HyperDX's
	// DASHBOARD_PROVISIONER_DIR.
	DashboardsDir = "deploy/observability/dashboards"
	// SpecFile is the serialized Spec: the renderer-neutral model. It sits
	// beside the dashboards directory, not in it, because the provisioner
	// reads every *.json it finds there.
	SpecFile = "deploy/observability/dashboard-spec_gen.json"
	// SavedSearchesFile holds the HyperDX saved searches, one JSON object per
	// line. It lives in the dashboards directory (the one directory compose
	// mounts) under an extension the dashboard provisioner ignores; the
	// compose entrypoint upserts it, because the file provisioner has no
	// saved-search path.
	SavedSearchesFile = DashboardsDir + "/saved-searches_gen.ndjson"
)

// genSuffix marks the files forge owns and rewrites.
const genSuffix = "_gen.json"

// OverridePath is the file that, when present, replaces the generated
// dashboard d: forge neither writes nor refreshes its _gen twin. The file is
// the project's, and forge never touches it.
func OverridePath(project string, d Dashboard) string {
	return DashboardsDir + "/" + project + "-" + d.ID + ".json"
}

// Write generates the project's dashboards under projectDir through the
// checksummed writer (a hand-edited generated file is left alone, exactly like
// every other Tier-1 output). It returns the project-relative paths written
// or kept.
func Write(projectDir string, in Input, cs *checksums.FileChecksums) ([]string, error) {
	sp := Build(in)

	files, err := RenderHyperDX(sp)
	if err != nil {
		return nil, err
	}
	byFile := map[string]Dashboard{}
	for _, d := range sp.Dashboards {
		byFile[DashboardFile(sp.Project, d)] = d
	}

	var out []string
	emit := func(rel string, content []byte) error {
		if _, err := checksums.WriteGeneratedFile(projectDir, rel, content, cs, true); err != nil {
			return fmt.Errorf("write %s: %w", rel, err)
		}
		out = append(out, rel)
		return nil
	}

	for _, f := range files {
		d := byFile[f.File]
		rel := DashboardsDir + "/" + f.File
		if _, err := os.Stat(filepath.Join(projectDir, OverridePath(sp.Project, d))); err == nil {
			if err := retireGenerated(projectDir, rel, cs); err != nil {
				return nil, err
			}
			continue
		}
		if err := emit(rel, f.Content); err != nil {
			return nil, err
		}
	}

	// Dashboards the project no longer has (a removed service) leave a
	// pristine generated file behind; drop exactly the ones forge wrote.
	if err := retireStale(projectDir, sp, cs); err != nil {
		return nil, err
	}

	specJSON, err := json.MarshalIndent(sp, "", "  ")
	if err != nil {
		return nil, err
	}
	if err := emit(SpecFile, append(specJSON, '\n')); err != nil {
		return nil, err
	}

	alertDefaults, err := MarshalAlertDefaults(AlertInputOf(in))
	if err != nil {
		return nil, err
	}
	if err := emit(AlertDefaultsFile, alertDefaults); err != nil {
		return nil, err
	}

	searches, err := RenderHyperDXSavedSearches(sp)
	if err != nil {
		return nil, err
	}
	var nd bytes.Buffer
	for _, s := range searches {
		b, err := json.Marshal(s)
		if err != nil {
			return nil, err
		}
		nd.Write(b)
		nd.WriteByte('\n')
	}
	if err := emit(SavedSearchesFile, nd.Bytes()); err != nil {
		return nil, err
	}

	sort.Strings(out)
	return out, nil
}

// retireGenerated removes rel when it is a pristine forge render.
func retireGenerated(projectDir, rel string, cs *checksums.FileChecksums) error {
	abs := filepath.Join(projectDir, rel)
	onDisk, err := os.ReadFile(abs)
	if err != nil {
		return nil
	}
	if cs == nil || cs.Unstampable[rel] != checksums.BodyHash(onDisk) {
		return nil
	}
	if err := checksums.RemoveJournaled(abs); err != nil {
		return err
	}
	delete(cs.Unstampable, rel)
	return nil
}

func retireStale(projectDir string, sp Spec, cs *checksums.FileChecksums) error {
	want := map[string]bool{}
	for _, d := range sp.Dashboards {
		want[DashboardFile(sp.Project, d)] = true
	}
	entries, err := os.ReadDir(filepath.Join(projectDir, DashboardsDir))
	if err != nil {
		return nil
	}
	for _, e := range entries {
		n := e.Name()
		if e.IsDir() || !strings.HasSuffix(n, genSuffix) || want[n] {
			continue
		}
		if err := retireGenerated(projectDir, DashboardsDir+"/"+n, cs); err != nil {
			return err
		}
	}
	return nil
}
