package teams

import "sync"

// NameRegistry 是进程内的全局名称注册表，维护 成员名 → agentID 的映射。
// SendMessage 用它把用户/LLM 给的收件人名字解析成投递用的标识。
type NameRegistry struct {
	mu    sync.Mutex
	names map[string]string // name -> agentID
}

var (
	nameRegistryOnce     sync.Once
	nameRegistryInstance *NameRegistry
)

// GetNameRegistry 返回全局单例注册表。
func GetNameRegistry() *NameRegistry {
	nameRegistryOnce.Do(func() {
		nameRegistryInstance = &NameRegistry{names: make(map[string]string)}
	})
	return nameRegistryInstance
}

// Register 登记一个 名字 → agentID 映射。
func (r *NameRegistry) Register(name, agentID string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.names[name] = agentID
}

// Resolve 把名字或 ID 解析成 agentID：先按名字查；传入的本身就是 ID 则原样返回；
// 都查不到返回空字符串。
func (r *NameRegistry) Resolve(nameOrID string) string {
	r.mu.Lock()
	defer r.mu.Unlock()
	if id, ok := r.names[nameOrID]; ok {
		return id
	}
	for _, id := range r.names {
		if id == nameOrID {
			return nameOrID
		}
	}
	return ""
}

// Unregister 移除一个名字的映射。
func (r *NameRegistry) Unregister(name string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.names, name)
}

// Clear 清空全部映射，主要用于测试隔离。
func (r *NameRegistry) Clear() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.names = make(map[string]string)
}
