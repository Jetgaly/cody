//go:build !darwin && !linux

package sandbox

// 不支持沙箱的平台返回 nil，调用方需判空
func newPlatformSandbox() Sandbox {
	return nil
}
