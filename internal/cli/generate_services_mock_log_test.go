package cli

import (
	"strings"
	"testing"

	"github.com/reliant-labs/forge/internal/codegen"
)

// TestMockOutcomeLine_NamesTheWrittenFile: generate's progress line named
// internal/handlers/mocks/<svc>_mock.go long after the file it wrote
// became <svc>_mock_gen.go. Both now read codegen.MockRelPath.
func TestMockOutcomeLine_NamesTheWrittenFile(t *testing.T) {
	svc := codegen.ServiceDef{Name: "ToolCallService"}
	for _, written := range []bool{true, false} {
		line := mockOutcomeLine(svc, written)
		if !strings.Contains(line, "internal/handlers/mocks/tool_call_mock_gen.go") {
			t.Errorf("written=%v: %q does not name the file GenerateMock writes", written, line)
		}
	}
}
