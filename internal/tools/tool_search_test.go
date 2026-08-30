package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
)

// mockDeferredTool 模拟一个会被 defer 的工具
type mockDeferredTool struct {
	name string
	desc string
}

func (t *mockDeferredTool) Name() string        { return t.name }
func (t *mockDeferredTool) Description() string  { return t.desc }
func (t *mockDeferredTool) Category() ToolCategory { return CategoryCommand }
func (t *mockDeferredTool) ShouldDefer() bool    { return true }

func (t *mockDeferredTool) Schema() map[string]any {
	return map[string]any{
		"name":        t.name,
		"description": t.desc,
		"input_schema": map[string]any{
			"type":       "object",
			"properties": map[string]any{"arg1": map[string]any{"type": "string"}},
		},
	}
}
func (t *mockDeferredTool) Execute(ctx context.Context, args map[string]any) ToolResult {
	return ToolResult{Output: "ok"}
}

// mockNormalTool 模拟一个不会被 defer 的普通工具
type mockNormalTool struct {
	name string
}

func (t *mockNormalTool) Name() string        { return t.name }
func (t *mockNormalTool) Description() string  { return "normal tool" }
func (t *mockNormalTool) Category() ToolCategory { return CategoryRead }
func (t *mockNormalTool) Schema() map[string]any {
	return map[string]any{
		"name":        t.name,
		"description": "normal tool",
		"input_schema": map[string]any{"type": "object", "properties": map[string]any{}},
	}
}
func (t *mockNormalTool) Execute(ctx context.Context, args map[string]any) ToolResult {
	return ToolResult{Output: "ok"}
}

// mockMCPTool 模拟 MCPToolWrapper（没实现 DeferrableTool 接口）
type mockMCPTool struct {
	name string
	desc string
}

func (t *mockMCPTool) Name() string        { return t.name }
func (t *mockMCPTool) Description() string  { return t.desc }
func (t *mockMCPTool) Category() ToolCategory { return CategoryCommand }
func (t *mockMCPTool) Schema() map[string]any {
	return map[string]any{
		"name":        t.name,
		"description": t.desc,
		"input_schema": map[string]any{
			"type":       "object",
			"properties": map[string]any{"expr": map[string]any{"type": "string"}},
		},
	}
}
func (t *mockMCPTool) Execute(ctx context.Context, args map[string]any) ToolResult {
	return ToolResult{Output: "ok"}
}

func TestDeferredToolsNotInGetAllSchemas(t *testing.T) {
	reg := NewRegistry()
	reg.Register(&mockNormalTool{name: "ReadFile"})
	reg.Register(&mockDeferredTool{name: "mcp__grafana__query", desc: "Query Prometheus"})
	reg.Register(&mockDeferredTool{name: "mcp__grafana__search", desc: "Search dashboards"})

	schemas := reg.GetAllSchemas("anthropic")

	// 只应该有 ReadFile，deferred 的不应该出现
	if len(schemas) != 1 {
		t.Errorf("expected 1 schema (only ReadFile), got %d", len(schemas))
	}
	if schemas[0]["name"] != "ReadFile" {
		t.Errorf("expected ReadFile, got %s", schemas[0]["name"])
	}
}

func TestMCPToolIsDeferred(t *testing.T) {
	// mockMCPTool 没实现 DeferrableTool，模拟旧行为（不 defer）
	// 真正的 MCPToolWrapper 现在实现了 ShouldDefer() = true
	reg := NewRegistry()
	reg.Register(&mockNormalTool{name: "ReadFile"})
	reg.Register(&mockMCPTool{name: "mcp__grafana__query", desc: "Query Prometheus"})

	schemas := reg.GetAllSchemas("anthropic")
	// mockMCPTool 没实现 DeferrableTool，所以仍然全量传
	if len(schemas) != 2 {
		t.Errorf("expected 2 schemas (mockMCPTool has no ShouldDefer), got %d", len(schemas))
	}

	// 但如果用 mockDeferredTool 模拟真正的 MCPToolWrapper 行为
	reg2 := NewRegistry()
	reg2.Register(&mockNormalTool{name: "ReadFile"})
	reg2.Register(&mockDeferredTool{name: "mcp__grafana__query", desc: "Query Prometheus"})

	schemas2 := reg2.GetAllSchemas("anthropic")
	if len(schemas2) != 1 {
		t.Errorf("expected 1 schema (deferred MCP tool excluded), got %d", len(schemas2))
	}

	deferred := reg2.GetDeferredTools()
	if len(deferred) != 1 {
		t.Errorf("expected 1 deferred tool, got %d", len(deferred))
	}
}

func TestDiscoveredToolsIncludedInSchemas(t *testing.T) {
	reg := NewRegistry()
	reg.Register(&mockNormalTool{name: "ReadFile"})
	reg.Register(&mockDeferredTool{name: "mcp__grafana__query", desc: "Query Prometheus"})
	reg.Register(&mockDeferredTool{name: "mcp__grafana__search", desc: "Search dashboards"})

	// 发现之前：只有 ReadFile
	schemas := reg.GetAllSchemas("anthropic")
	if len(schemas) != 1 {
		t.Errorf("before discovery: expected 1 schema, got %d", len(schemas))
	}

	// 发现一个工具
	reg.MarkDiscovered("mcp__grafana__query")

	// 发现之后：ReadFile + 已发现的工具
	schemas = reg.GetAllSchemas("anthropic")
	if len(schemas) != 2 {
		t.Errorf("after discovery: expected 2 schemas, got %d", len(schemas))
	}

	// GetDeferredToolNames 应该只返回未发现的工具
	names := reg.GetDeferredToolNames()
	if len(names) != 1 || names[0] != "mcp__grafana__search" {
		t.Errorf("expected only mcp__grafana__search as deferred, got %v", names)
	}
}

func TestToolSearchMarksDiscovered(t *testing.T) {
	reg := NewRegistry()
	reg.Register(&mockDeferredTool{name: "mcp__grafana__query", desc: "Query Prometheus"})

	// 搜索之前：未发现
	if reg.IsDiscovered("mcp__grafana__query") {
		t.Error("should not be discovered before ToolSearch")
	}

	ts := &ToolSearchTool{Registry: reg, Protocol: "anthropic"}
	ts.Execute(context.Background(), map[string]any{"query": "select:mcp__grafana__query"})

	// 搜索之后：应当已被发现
	if !reg.IsDiscovered("mcp__grafana__query") {
		t.Error("should be discovered after ToolSearch")
	}

	// 现在 Schema 应当包含该工具
	schemas := reg.GetAllSchemas("anthropic")
	if len(schemas) != 1 {
		t.Errorf("discovered tool should be in schemas, got %d", len(schemas))
	}
}

func TestToolSearchSelect(t *testing.T) {
	reg := NewRegistry()
	reg.Register(&mockDeferredTool{name: "mcp__grafana__query", desc: "Query Prometheus"})
	reg.Register(&mockDeferredTool{name: "mcp__grafana__search", desc: "Search dashboards"})

	ts := &ToolSearchTool{Registry: reg, Protocol: "anthropic"}
	result := ts.Execute(context.Background(), map[string]any{
		"query": "select:mcp__grafana__query",
	})

	if result.IsError {
		t.Errorf("ToolSearch returned error: %s", result.Output)
	}
	if !contains(result.Output, "mcp__grafana__query") {
		t.Errorf("expected output to contain tool name, got: %s", result.Output)
	}
}

func TestToolSearchKeyword(t *testing.T) {
	reg := NewRegistry()
	reg.Register(&mockDeferredTool{name: "mcp__grafana__query", desc: "Query Prometheus metrics"})
	reg.Register(&mockDeferredTool{name: "mcp__github__issues", desc: "List GitHub issues"})

	ts := &ToolSearchTool{Registry: reg, Protocol: "anthropic"}
	result := ts.Execute(context.Background(), map[string]any{
		"query": "prometheus",
	})

	if result.IsError {
		t.Errorf("ToolSearch returned error: %s", result.Output)
	}
	if !contains(result.Output, "mcp__grafana__query") {
		t.Errorf("expected to find grafana query tool, got: %s", result.Output)
	}
}

func TestToolSearchNoMatch(t *testing.T) {
	reg := NewRegistry()
	reg.Register(&mockDeferredTool{name: "mcp__grafana__query", desc: "Query Prometheus"})

	ts := &ToolSearchTool{Registry: reg, Protocol: "anthropic"}
	result := ts.Execute(context.Background(), map[string]any{
		"query": "nonexistent_xyz",
	})

	if result.IsError {
		t.Errorf("should not be error, got: %s", result.Output)
	}
	if !contains(result.Output, "No matching") {
		t.Errorf("expected 'No matching' message, got: %s", result.Output)
	}
}

// mockLargeDeferredTool 模拟一个带有大型 schema 的真实 MCP 工具
// （每个工具约 500+ 字符的 JSON，模仿真实的 Grafana/Playwright 工具）。
type mockLargeDeferredTool struct {
	name string
	desc string
}

func (t *mockLargeDeferredTool) Name() string           { return t.name }
func (t *mockLargeDeferredTool) Description() string     { return t.desc }
func (t *mockLargeDeferredTool) Category() ToolCategory  { return CategoryCommand }
func (t *mockLargeDeferredTool) ShouldDefer() bool       { return true }
func (t *mockLargeDeferredTool) Schema() map[string]any {
	return map[string]any{
		"name":        t.name,
		"description": t.desc,
		"input_schema": map[string]any{
			"type":     "object",
			"required": []string{"query", "datasource"},
			"properties": map[string]any{
				"query":       map[string]any{"type": "string", "description": "The query expression to execute against the datasource"},
				"datasource":  map[string]any{"type": "string", "description": "Name or UID of the target datasource to query"},
				"start_time":  map[string]any{"type": "string", "description": "Start of the time range in RFC3339 or relative format"},
				"end_time":    map[string]any{"type": "string", "description": "End of the time range in RFC3339 or relative format"},
				"step":        map[string]any{"type": "string", "description": "Query resolution step width in duration format"},
				"max_results": map[string]any{"type": "integer", "description": "Maximum number of results to return from the query"},
				"format":      map[string]any{"type": "string", "description": "Output format: table, timeseries, or json"},
				"labels":      map[string]any{"type": "object", "description": "Additional label matchers to filter results"},
			},
		},
	}
}

func (t *mockLargeDeferredTool) Execute(ctx context.Context, args map[string]any) ToolResult {
	return ToolResult{Output: "ok"}
}

func TestDeferredTokenSavings(t *testing.T) {
	reg := NewRegistry()

	// 注册 2 个带小型 schema 的普通（非 defer）工具。
	reg.Register(&mockNormalTool{name: "ReadFile"})
	reg.Register(&mockNormalTool{name: "WriteFile"})

	// 注册 50 个带真实大型 schema 的 deferred 工具。
	for i := 0; i < 50; i++ {
		reg.Register(&mockLargeDeferredTool{
			name: fmt.Sprintf("mcp__grafana__tool_%03d", i),
			desc: fmt.Sprintf("A realistic MCP tool that queries datasource %d with full parameter set", i),
		})
	}

	// 在隐藏 deferred 工具（默认状态）的情况下测量大小。
	schemasDeferred := reg.GetAllSchemas("anthropic")
	bytesDeferred, err := json.Marshal(schemasDeferred)
	if err != nil {
		t.Fatalf("json.Marshal deferred schemas: %v", err)
	}
	sizeDeferred := len(bytesDeferred)

	// 发现全部 50 个 deferred 工具。
	for i := 0; i < 50; i++ {
		reg.MarkDiscovered(fmt.Sprintf("mcp__grafana__tool_%03d", i))
	}

	// 在包含全部工具的情况下测量大小。
	schemasAll := reg.GetAllSchemas("anthropic")
	bytesAll, err := json.Marshal(schemasAll)
	if err != nil {
		t.Fatalf("json.Marshal all schemas: %v", err)
	}
	sizeAll := len(bytesAll)

	savings := 1 - float64(sizeDeferred)/float64(sizeAll)
	t.Logf("Deferred size: %d bytes, Full size: %d bytes, Savings: %.2f%%", sizeDeferred, sizeAll, savings*100)

	if savings < 0.90 {
		t.Errorf("expected >= 90%% token savings from deferral, got %.2f%%", savings*100)
	}
}

func TestDeferredEndToEndDiscovery(t *testing.T) {
	reg := NewRegistry()
	reg.Register(&mockNormalTool{name: "Bash"})
	reg.Register(&mockDeferredTool{name: "mcp__playwright__click", desc: "Click an element"})
	reg.Register(&mockDeferredTool{name: "mcp__playwright__fill", desc: "Fill a form field"})

	// 第 1 步：deferred 工具不应出现在 GetAllSchemas 中。
	schemas := reg.GetAllSchemas("anthropic")
	for _, s := range schemas {
		name := s["name"].(string)
		if name == "mcp__playwright__click" || name == "mcp__playwright__fill" {
			t.Errorf("deferred tool %q should not appear in GetAllSchemas before discovery", name)
		}
	}
	if len(schemas) != 1 {
		t.Errorf("expected 1 schema (Bash only), got %d", len(schemas))
	}

	// 第 2 步：GetDeferredToolNames 应返回这两个 deferred 工具的名称。
	deferredNames := reg.GetDeferredToolNames()
	nameSet := make(map[string]bool)
	for _, n := range deferredNames {
		nameSet[n] = true
	}
	if !nameSet["mcp__playwright__click"] || !nameSet["mcp__playwright__fill"] {
		t.Errorf("expected both deferred tools in GetDeferredToolNames, got %v", deferredNames)
	}

	// 第 3 步：发现一个工具。
	reg.MarkDiscovered("mcp__playwright__click")

	// 第 4 步：已发现的工具现在应出现在 GetAllSchemas 中。
	schemas = reg.GetAllSchemas("anthropic")
	foundClick := false
	foundFill := false
	for _, s := range schemas {
		switch s["name"].(string) {
		case "mcp__playwright__click":
			foundClick = true
		case "mcp__playwright__fill":
			foundFill = true
		}
	}
	if !foundClick {
		t.Error("mcp__playwright__click should appear in GetAllSchemas after MarkDiscovered")
	}
	if foundFill {
		t.Error("mcp__playwright__fill should NOT appear in GetAllSchemas (still deferred)")
	}
	if len(schemas) != 2 {
		t.Errorf("expected 2 schemas (Bash + click), got %d", len(schemas))
	}

	// 第 5 步：GetDeferredToolNames 应只返回未发现的工具。
	deferredNames = reg.GetDeferredToolNames()
	if len(deferredNames) != 1 {
		t.Errorf("expected 1 deferred tool remaining, got %d: %v", len(deferredNames), deferredNames)
	}
	if len(deferredNames) == 1 && deferredNames[0] != "mcp__playwright__fill" {
		t.Errorf("expected mcp__playwright__fill as only deferred tool, got %q", deferredNames[0])
	}
}

func contains(s, substr string) bool {
	return len(s) >= len(substr) && (s == substr || len(s) > 0 && containsHelper(s, substr))
}

func containsHelper(s, substr string) bool {
	for i := 0; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}
