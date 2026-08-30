package teams

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"sync"
)

// SharedTask 是团队共享任务板上的一条任务，带依赖关系（Blocks / BlockedBy）
// 和归属（Assignee）。
type SharedTask struct {
	ID          string   `json:"id"`
	Title       string   `json:"title"`
	Description string   `json:"description"`
	Status      string   `json:"status"` // pending | in_progress | completed | blocked
	Assignee    string   `json:"assignee"`
	Blocks      []string `json:"blocks"`
	BlockedBy   []string `json:"blocked_by"`
	CreatedBy   string   `json:"created_by"`
}

// storeData 是 tasks.json 的整体结构：下一个可用 id + 任务列表。
type storeData struct {
	NextID int          `json:"next_id"`
	Tasks  []SharedTask `json:"tasks"`
}

// SharedTaskStore 以 JSON 文件（tasks.json）落盘，供同一团队的所有成员读写。
// 每次读操作前先重新加载文件，保证跨进程的队友能拿到最新数据。
type SharedTaskStore struct {
	mu     sync.Mutex
	path   string
	nextID int
	tasks  []SharedTask
}

// NewSharedTaskStore 打开（或初始化）指定路径的共享任务库。
func NewSharedTaskStore(path string) *SharedTaskStore {
	s := &SharedTaskStore{path: path, nextID: 1}
	s.load()
	return s
}

// load 从磁盘重新读取任务列表；文件不存在时保持空。调用方需持有锁。
func (s *SharedTaskStore) load() {
	data, err := os.ReadFile(s.path)
	if err != nil {
		return
	}
	var sd storeData
	if err := json.Unmarshal(data, &sd); err != nil {
		return
	}
	s.tasks = sd.Tasks
	if sd.NextID > 0 {
		s.nextID = sd.NextID
	}
}

// save 把当前任务列表写回磁盘。调用方需持有锁。
func (s *SharedTaskStore) save() {
	if err := os.MkdirAll(filepath.Dir(s.path), 0o755); err != nil {
		return
	}
	sd := storeData{NextID: s.nextID, Tasks: s.tasks}
	if s.tasks == nil {
		sd.Tasks = []SharedTask{}
	}
	out, err := json.MarshalIndent(sd, "", "  ")
	if err != nil {
		return
	}
	_ = os.WriteFile(s.path, out, 0o644)
}

// Create 创建一条共享任务，返回新任务。
func (s *SharedTaskStore) Create(title, description, assignee string, blocks, blockedBy []string, createdBy string) SharedTask {
	s.mu.Lock()
	defer s.mu.Unlock()
	if blocks == nil {
		blocks = []string{}
	}
	if blockedBy == nil {
		blockedBy = []string{}
	}
	task := SharedTask{
		ID:          strconv.Itoa(s.nextID),
		Title:       title,
		Description: description,
		Status:      "pending",
		Assignee:    assignee,
		Blocks:      blocks,
		BlockedBy:   blockedBy,
		CreatedBy:   createdBy,
	}
	s.nextID++
	s.tasks = append(s.tasks, task)
	s.save()
	return task
}

// Get 按 id 获取任务；读前先 reload 拿最新。找不到返回 nil。
func (s *SharedTaskStore) Get(id string) *SharedTask {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.load()
	for i := range s.tasks {
		if s.tasks[i].ID == id {
			t := s.tasks[i]
			return &t
		}
	}
	return nil
}

// ListTasks 列出任务，可选按状态、归属人过滤。
func (s *SharedTaskStore) ListTasks(status, assignee string) []SharedTask {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.load()
	var result []SharedTask
	for _, t := range s.tasks {
		if status != "" && t.Status != status {
			continue
		}
		if assignee != "" && t.Assignee != assignee {
			continue
		}
		result = append(result, t)
	}
	return result
}

// TaskUpdate 描述一次更新；nil 指针表示对应字段不改。
type TaskUpdate struct {
	Status       *string
	Assignee     *string
	Description  *string
	AddBlocks    []string
	AddBlockedBy []string
}

// Update 按 TaskUpdate 修改任务；AddBlocks / AddBlockedBy 追加依赖（去重）。
// 任务不存在返回 nil。
func (s *SharedTaskStore) Update(id string, upd TaskUpdate) *SharedTask {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.load()
	for i := range s.tasks {
		if s.tasks[i].ID != id {
			continue
		}
		t := &s.tasks[i]
		if upd.Status != nil {
			t.Status = *upd.Status
		}
		if upd.Assignee != nil {
			t.Assignee = *upd.Assignee
		}
		if upd.Description != nil {
			t.Description = *upd.Description
		}
		t.Blocks = appendUnique(t.Blocks, upd.AddBlocks)
		t.BlockedBy = appendUnique(t.BlockedBy, upd.AddBlockedBy)
		s.save()
		updated := *t
		return &updated
	}
	return nil
}

// InitEmpty 清空任务库并落盘，用于新建团队时初始化。
func (s *SharedTaskStore) InitEmpty() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.tasks = []SharedTask{}
	s.nextID = 1
	s.save()
}

// appendUnique 把 add 里尚不存在的元素追加到 base，返回新切片。
func appendUnique(base, add []string) []string {
	for _, v := range add {
		found := false
		for _, e := range base {
			if e == v {
				found = true
				break
			}
		}
		if !found {
			base = append(base, v)
		}
	}
	if base == nil {
		base = []string{}
	}
	return base
}
