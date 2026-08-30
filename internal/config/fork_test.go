package config

import "testing"

// enable_fork 默认开着，而且配置里写 false 必须真的能关掉。
// 用普通 bool 存的话「没写」和「写了 false」是同一个零值，后者就永远关不掉。
func TestForkEnabledDefaultsOn(t *testing.T) {
	cfg := &AppConfig{}
	if !cfg.ForkEnabled() {
		t.Fatal("配置里没写 enable_fork 时，fork 应该是开着的")
	}
}

func TestForkEnabledExplicitFalse(t *testing.T) {
	off := false
	cfg := &AppConfig{EnableFork: &off}
	if cfg.ForkEnabled() {
		t.Fatal("配置里写了 enable_fork: false，fork 应该关掉")
	}

	on := true
	cfg.EnableFork = &on
	if !cfg.ForkEnabled() {
		t.Fatal("配置里写了 enable_fork: true，fork 应该开着")
	}
}

// 后加载的配置里显式写了 enable_fork 才覆盖，没写就沿用前一层的值。
func TestMergeConfigFork(t *testing.T) {
	off := false

	base := &AppConfig{EnableFork: &off}
	merged := mergeConfig(base, &AppConfig{})
	if merged.ForkEnabled() {
		t.Fatal("后一层没写 enable_fork 时不该覆盖前一层的 false")
	}

	base2 := &AppConfig{}
	merged2 := mergeConfig(base2, &AppConfig{EnableFork: &off})
	if merged2.ForkEnabled() {
		t.Fatal("后一层写了 enable_fork: false 时应该覆盖掉默认值")
	}
}
