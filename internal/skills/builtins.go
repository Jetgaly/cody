package skills

// LoadBuiltins 返回编译进二进制的内嵌技能。
// 目前为空——所有技能在运行时从磁盘加载
// （用户级 ~/.cody/skills/ 或项目级 .cody/skills/）。
func LoadBuiltins() []*Skill {
	return nil
}
