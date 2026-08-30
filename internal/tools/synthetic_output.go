package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
)

// SyntheticOutputTool 让 Agent 以结构化数据交付最终结果。
// 非交互模式和 coordinator 模式下，调用方要的是能直接解析的 JSON，
// 而不是夹在自然语言里的一段文字。
type SyntheticOutputTool struct {
	// JSONSchema 可选。设置后会校验 output 是否符合调用方约定的结构。
	JSONSchema map[string]any
}

func (t *SyntheticOutputTool) Name() string           { return "SyntheticOutput" }
func (t *SyntheticOutputTool) Category() ToolCategory { return CategoryRead }

func (t *SyntheticOutputTool) Description() string {
	return "Return structured output in JSON format. Use this tool to return your final response " +
		"as structured data in non-interactive or coordinator mode sessions."
}

func (t *SyntheticOutputTool) Schema() map[string]any {
	return map[string]any{
		"name":        t.Name(),
		"description": t.Description(),
		"input_schema": map[string]any{
			"type": "object",
			"properties": map[string]any{
				"output": map[string]any{
					"description": "The structured result: an object, an array, or a plain string",
				},
			},
			"required": []string{"output"},
		},
	}
}

func (t *SyntheticOutputTool) Execute(ctx context.Context, args map[string]any) ToolResult {
	output, ok := args["output"]
	if !ok {
		return ToolResult{Output: "Error: output is required", IsError: true}
	}

	if err := t.validateSchema(output); err != "" {
		return ToolResult{
			Output:  fmt.Sprintf("Output does not match required schema: %s", err),
			IsError: true,
		}
	}

	// 字符串原样返回，不做二次 JSON 包装
	if s, isString := output.(string); isString {
		return ToolResult{Output: s}
	}

	encoded, err := json.MarshalIndent(output, "", "  ")
	if err != nil {
		return ToolResult{
			Output:  fmt.Sprintf("Error: output is not serializable: %s", err),
			IsError: true,
		}
	}
	return ToolResult{Output: string(encoded)}
}

// validateSchema 只覆盖顶层类型和必填字段，返回空字符串表示通过。
// 完整的 JSON Schema 校验没有必要，这里挡的是模型交付结构明显走样的情况。
func (t *SyntheticOutputTool) validateSchema(data any) string {
	if t.JSONSchema == nil {
		return ""
	}

	if expected, ok := t.JSONSchema["type"].(string); ok {
		switch expected {
		case "object":
			if _, isMap := data.(map[string]any); !isMap {
				return fmt.Sprintf("Expected object, got %T", data)
			}
		case "array":
			if _, isSlice := data.([]any); !isSlice {
				return fmt.Sprintf("Expected array, got %T", data)
			}
		case "string":
			if _, isString := data.(string); !isString {
				return fmt.Sprintf("Expected string, got %T", data)
			}
		}
	}

	required, hasRequired := t.JSONSchema["required"].([]any)
	obj, isObj := data.(map[string]any)
	if hasRequired && isObj {
		var missing []string
		for _, key := range required {
			name, _ := key.(string)
			if name == "" {
				continue
			}
			if _, present := obj[name]; !present {
				missing = append(missing, name)
			}
		}
		if len(missing) > 0 {
			return "Missing required fields: " + strings.Join(missing, ", ")
		}
	}

	return ""
}
