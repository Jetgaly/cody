package teams

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"cody/internal/tools"
)

// 团队共享任务板工具：TaskCreate / TaskGet / TaskList / TaskUpdate。
// 这四个工具都挂在同一个团队的 SharedTaskStore 上，队友之间共享同一份任务列表。

var validTaskStatuses = map[string]bool{
	"pending":     true,
	"in_progress": true,
	"completed":   true,
	"blocked":     true,
}

// stringSlice 把工具参数里的字符串数组安全地取出来。
func stringSlice(raw any) []string {
	arr, ok := raw.([]any)
	if !ok {
		return nil
	}
	var out []string
	for _, v := range arr {
		if s, ok := v.(string); ok {
			out = append(out, s)
		}
	}
	return out
}

// ── TaskCreate ─────────────────────────────────────────────────────

type TaskCreateTool struct {
	TeamMgr   *TeamManager
	TeamName  string
	AgentName string
}

func (t *TaskCreateTool) Name() string                 { return "TaskCreate" }
func (t *TaskCreateTool) Category() tools.ToolCategory { return tools.CategoryCommand }
func (t *TaskCreateTool) Description() string {
	return "Create a shared task in the team's task board. Supports dependency tracking with blocks/blocked_by fields."
}

func (t *TaskCreateTool) Schema() map[string]any {
	return map[string]any{
		"name":        t.Name(),
		"description": t.Description(),
		"input_schema": map[string]any{
			"type": "object",
			"properties": map[string]any{
				"title":       map[string]any{"type": "string", "description": "Short task title"},
				"description": map[string]any{"type": "string", "description": "Optional task details"},
				"assignee":    map[string]any{"type": "string", "description": "Teammate name to assign this task to"},
				"blocks":      map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "description": "IDs of tasks that this task blocks"},
				"blocked_by":  map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "description": "IDs of tasks that block this task"},
			},
			"required": []string{"title"},
		},
	}
}

func (t *TaskCreateTool) Execute(_ context.Context, args map[string]any) tools.ToolResult {
	title, _ := args["title"].(string)
	if title == "" {
		return tools.ToolResult{Output: "Error: 'title' is required", IsError: true}
	}
	store := t.TeamMgr.GetTaskStore(t.TeamName)
	if store == nil {
		return tools.ToolResult{Output: fmt.Sprintf("Task store not found for team '%s'", t.TeamName), IsError: true}
	}
	description, _ := args["description"].(string)
	assignee, _ := args["assignee"].(string)
	task := store.Create(title, description, assignee, stringSlice(args["blocks"]), stringSlice(args["blocked_by"]), t.AgentName)
	assigneeStr := task.Assignee
	if assigneeStr == "" {
		assigneeStr = "(unassigned)"
	}
	return tools.ToolResult{Output: fmt.Sprintf(
		"Task created:\n  ID: %s\n  Title: %s\n  Status: %s\n  Assignee: %s",
		task.ID, task.Title, task.Status, assigneeStr)}
}

// ── TaskGet ────────────────────────────────────────────────────────

type TaskGetTool struct {
	TeamMgr  *TeamManager
	TeamName string
}

func (t *TaskGetTool) Name() string                 { return "TaskGet" }
func (t *TaskGetTool) Category() tools.ToolCategory { return tools.CategoryRead }
func (t *TaskGetTool) Description() string {
	return "Get details of a shared task by ID, including dependency information."
}

func (t *TaskGetTool) Schema() map[string]any {
	return map[string]any{
		"name":        t.Name(),
		"description": t.Description(),
		"input_schema": map[string]any{
			"type": "object",
			"properties": map[string]any{
				"task_id": map[string]any{"type": "string", "description": "ID of the task to fetch"},
			},
			"required": []string{"task_id"},
		},
	}
}

func (t *TaskGetTool) Execute(_ context.Context, args map[string]any) tools.ToolResult {
	taskID, _ := args["task_id"].(string)
	if taskID == "" {
		return tools.ToolResult{Output: "Error: 'task_id' is required", IsError: true}
	}
	store := t.TeamMgr.GetTaskStore(t.TeamName)
	if store == nil {
		return tools.ToolResult{Output: fmt.Sprintf("Task store not found for team '%s'", t.TeamName), IsError: true}
	}
	task := store.Get(taskID)
	if task == nil {
		return tools.ToolResult{Output: fmt.Sprintf("Task '%s' not found", taskID), IsError: true}
	}
	assignee := task.Assignee
	if assignee == "" {
		assignee = "(unassigned)"
	}
	createdBy := task.CreatedBy
	if createdBy == "" {
		createdBy = "(unknown)"
	}
	lines := []string{
		fmt.Sprintf("Task %s:", task.ID),
		fmt.Sprintf("  Title:      %s", task.Title),
		fmt.Sprintf("  Status:     %s", task.Status),
		fmt.Sprintf("  Assignee:   %s", assignee),
		fmt.Sprintf("  Created by: %s", createdBy),
	}
	if task.Description != "" {
		lines = append(lines, fmt.Sprintf("  Description: %s", task.Description))
	}
	if len(task.Blocks) > 0 {
		lines = append(lines, fmt.Sprintf("  Blocks:     %s", strings.Join(task.Blocks, ", ")))
	}
	if len(task.BlockedBy) > 0 {
		lines = append(lines, fmt.Sprintf("  Blocked by: %s", strings.Join(task.BlockedBy, ", ")))
	}
	return tools.ToolResult{Output: strings.Join(lines, "\n")}
}

// ── TaskList ───────────────────────────────────────────────────────

type TaskListTool struct {
	TeamMgr  *TeamManager
	TeamName string
}

func (t *TaskListTool) Name() string                 { return "TaskList" }
func (t *TaskListTool) Category() tools.ToolCategory { return tools.CategoryRead }
func (t *TaskListTool) Description() string {
	return "List all shared tasks in the team's task board. Optionally filter by status (pending/in_progress/completed/blocked) or assignee."
}

func (t *TaskListTool) Schema() map[string]any {
	return map[string]any{
		"name":        t.Name(),
		"description": t.Description(),
		"input_schema": map[string]any{
			"type": "object",
			"properties": map[string]any{
				"status":   map[string]any{"type": "string", "description": "Filter by status"},
				"assignee": map[string]any{"type": "string", "description": "Filter by assignee name"},
			},
			"required": []string{},
		},
	}
}

func (t *TaskListTool) Execute(_ context.Context, args map[string]any) tools.ToolResult {
	store := t.TeamMgr.GetTaskStore(t.TeamName)
	if store == nil {
		return tools.ToolResult{Output: fmt.Sprintf("Task store not found for team '%s'", t.TeamName), IsError: true}
	}
	status, _ := args["status"].(string)
	assignee, _ := args["assignee"].(string)
	tasks := store.ListTasks(status, assignee)
	if len(tasks) == 0 {
		var filters []string
		if status != "" {
			filters = append(filters, "status="+status)
		}
		if assignee != "" {
			filters = append(filters, "assignee="+assignee)
		}
		suffix := ""
		if len(filters) > 0 {
			suffix = fmt.Sprintf(" (filters: %s)", strings.Join(filters, ", "))
		}
		return tools.ToolResult{Output: "No tasks found" + suffix}
	}
	icons := map[string]string{"pending": "○", "in_progress": "◐", "completed": "●", "blocked": "✕"}
	lines := []string{fmt.Sprintf("Tasks (%d):", len(tasks))}
	for _, t := range tasks {
		icon := icons[t.Status]
		if icon == "" {
			icon = "?"
		}
		assigneeStr := ""
		if t.Assignee != "" {
			assigneeStr = fmt.Sprintf(" [%s]", t.Assignee)
		}
		deps := ""
		if len(t.BlockedBy) > 0 {
			deps = fmt.Sprintf(" (blocked by: %s)", strings.Join(t.BlockedBy, ", "))
		}
		lines = append(lines, fmt.Sprintf("  %s [%s] %s%s%s", icon, t.ID, t.Title, assigneeStr, deps))
	}
	return tools.ToolResult{Output: strings.Join(lines, "\n")}
}

// ── TaskUpdate ─────────────────────────────────────────────────────

type TaskUpdateTool struct {
	TeamMgr  *TeamManager
	TeamName string
}

func (t *TaskUpdateTool) Name() string                 { return "TaskUpdate" }
func (t *TaskUpdateTool) Category() tools.ToolCategory { return tools.CategoryCommand }
func (t *TaskUpdateTool) Description() string {
	return "Update a shared task's status, assignee, description, or dependencies. Use add_blocks/add_blocked_by to add dependency relations."
}

func (t *TaskUpdateTool) Schema() map[string]any {
	return map[string]any{
		"name":        t.Name(),
		"description": t.Description(),
		"input_schema": map[string]any{
			"type": "object",
			"properties": map[string]any{
				"task_id":        map[string]any{"type": "string", "description": "ID of the task to update"},
				"status":         map[string]any{"type": "string", "description": "New status: pending/in_progress/completed/blocked"},
				"assignee":       map[string]any{"type": "string", "description": "Teammate name to assign"},
				"description":    map[string]any{"type": "string", "description": "New description"},
				"add_blocks":     map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "description": "Task IDs to add to the blocks list"},
				"add_blocked_by": map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "description": "Task IDs to add to the blocked_by list"},
			},
			"required": []string{"task_id"},
		},
	}
}

func (t *TaskUpdateTool) Execute(_ context.Context, args map[string]any) tools.ToolResult {
	taskID, _ := args["task_id"].(string)
	if taskID == "" {
		return tools.ToolResult{Output: "Error: 'task_id' is required", IsError: true}
	}
	store := t.TeamMgr.GetTaskStore(t.TeamName)
	if store == nil {
		return tools.ToolResult{Output: fmt.Sprintf("Task store not found for team '%s'", t.TeamName), IsError: true}
	}

	var upd TaskUpdate
	var changes []string
	if status, ok := args["status"].(string); ok && status != "" {
		if !validTaskStatuses[status] {
			valid := []string{"blocked", "completed", "in_progress", "pending"}
			sort.Strings(valid)
			return tools.ToolResult{Output: fmt.Sprintf("Invalid status '%s'. Must be one of: %s", status, strings.Join(valid, ", ")), IsError: true}
		}
		upd.Status = &status
		changes = append(changes, "status → "+status)
	}
	if assignee, ok := args["assignee"].(string); ok {
		a := assignee
		upd.Assignee = &a
		disp := a
		if disp == "" {
			disp = "(unassigned)"
		}
		changes = append(changes, "assignee → "+disp)
	}
	if description, ok := args["description"].(string); ok {
		d := description
		upd.Description = &d
		changes = append(changes, "description updated")
	}
	if addBlocks := stringSlice(args["add_blocks"]); len(addBlocks) > 0 {
		upd.AddBlocks = addBlocks
		changes = append(changes, "blocks += "+strings.Join(addBlocks, ", "))
	}
	if addBlockedBy := stringSlice(args["add_blocked_by"]); len(addBlockedBy) > 0 {
		upd.AddBlockedBy = addBlockedBy
		changes = append(changes, "blocked_by += "+strings.Join(addBlockedBy, ", "))
	}

	task := store.Update(taskID, upd)
	if task == nil {
		return tools.ToolResult{Output: fmt.Sprintf("Task '%s' not found", taskID), IsError: true}
	}
	changeStr := "no changes"
	if len(changes) > 0 {
		changeStr = strings.Join(changes, "; ")
	}
	return tools.ToolResult{Output: fmt.Sprintf("Task %s updated: %s", task.ID, changeStr)}
}
