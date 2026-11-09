package boot

import (
	"context"
	"encoding/json"
	"testing"

	"reasonix/internal/tool"
)

type namedTool struct{ name string }

func (t namedTool) Name() string            { return t.name }
func (t namedTool) Description() string     { return t.name }
func (t namedTool) Schema() json.RawMessage { return json.RawMessage(`{"type":"object"}`) }
func (t namedTool) Execute(context.Context, json.RawMessage) (string, error) {
	return "", nil
}
func (t namedTool) ReadOnly() bool { return true }

func TestDirectMCPSurfaceOffersViewImageWhenRegistered(t *testing.T) {
	t.Setenv("REASONIX_DIRECT_MCP", "1")
	reg := tool.NewRegistry()
	for _, name := range []string{"bash", "read_file", "view_image", "edit_file", "write_file", "send_message", "web_search", "mcp__memory__find"} {
		reg.Add(namedTool{name: name})
	}
	applyUnifiedProviderToolSurface(reg)
	for _, name := range []string{"bash", "read_file", "view_image", "edit_file", "write_file", "send_message", "mcp__memory__find"} {
		if !reg.ProviderVisible(name) {
			t.Errorf("%s is not visible", name)
		}
	}
	if reg.ProviderVisible("web_search") {
		t.Fatal("web_search is on the direct surface")
	}
}

func TestDirectMCPSurfaceSkipsViewImageWhenAbsent(t *testing.T) {
	t.Setenv("REASONIX_DIRECT_MCP", "1")
	reg := tool.NewRegistry()
	for _, name := range []string{"bash", "read_file", "edit_file", "write_file", "send_message"} {
		reg.Add(namedTool{name: name})
	}
	applyUnifiedProviderToolSurface(reg)
	if reg.ProviderVisible("view_image") {
		t.Fatal("view_image offered without registration")
	}
}
