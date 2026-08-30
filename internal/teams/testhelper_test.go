package teams

import "testing"

// useTempHome 把用户主目录指到临时目录，让团队配置落在测试自己的沙箱里。
//
// 团队目录是 <home>/.cody/teams，不重定向的话跑一次测试就会在真实主目录里
// 留下一堆 squad、squad-2 这样的残留，下一次跑还会撞上「团队已存在」。
//
// os.UserHomeDir 在 Windows 上读 USERPROFILE，其余平台读 HOME，两个都设上就跨平台通用。
func useTempHome(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	t.Setenv("USERPROFILE", dir)
	return dir
}
