package tools

import (
	"context"
	"os/exec"
	"strings"
	"fmt"
	"testing"
)

//go test -v -run TestBashExecuteRunsCommandAndReturnsOutput
func TestBashExecuteRequiresCommand(t *testing.T) {
	tool := &BashTool{}

	result := tool.Execute(context.Background(), map[string]any{})

	if !result.IsError {
		t.Fatal("expected missing command to return an error")
	}
	if !strings.Contains(result.Output, "command is required") {
		t.Fatalf("expected missing-command message, got: %s", result.Output)
	}
}

func TestBashExecuteRunsCommandAndReturnsOutput(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not available")
	}

	tool := &BashTool{WorkDir: t.TempDir()}
	result := tool.Execute(context.Background(), map[string]any{
		"command": "printf 'hello from bash\\n'",
	})
	fmt.Printf("BashTool.Execute output:\n%s", result.Output)
	if result.IsError {
		t.Fatalf("expected successful command, got error: %s", result.Output)
	}
	if !strings.Contains(result.Output, "hello from bash") {
		t.Fatalf("expected command output to be included, got: %s", result.Output)
	}
	if !strings.Contains(result.Output, "$ printf 'hello from bash\\n'") {
		t.Fatalf("expected echoed command in output, got: %s", result.Output)
	}
}

func TestBashExecuteReportsConditionFailureHint(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not available")
	}

	tool := &BashTool{}
	result := tool.Execute(context.Background(), map[string]any{
		"command": "test -f /tmp/definitely-not-there",
	})

	if result.IsError {
		t.Fatalf("expected non-zero exit to be treated as non-fatal, got error: %s", result.Output)
	}
	if !strings.Contains(result.Output, "condition is false") {
		t.Fatalf("expected condition-false hint, got: %s", result.Output)
	}
}
