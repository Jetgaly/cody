package llm

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"cody/internal/config"
)

// TestResolveContextWindow_FetchSuccess 覆盖第 2 层的正常工作：健康的
// /v1/models/{model} 端点返回 max_input_tokens，该值被缓存在 provider
// 配置上，并由 GetContextWindow 对外提供。
func TestResolveContextWindow_FetchSuccess(t *testing.T) {
	var gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(200)
		w.Write([]byte(`{"id":"claude-sonnet-4-6","type":"model","display_name":"x","max_input_tokens":555000,"max_tokens":8192}`))
	}))
	defer srv.Close()

	cfg := &config.ProviderConfig{
		Protocol: "anthropic", BaseURL: srv.URL, APIKey: "k", Model: "claude-sonnet-4-6",
	}
	ResolveContextWindow(context.Background(), cfg)

	if !strings.Contains(gotPath, "/v1/models/claude-sonnet-4-6") {
		t.Errorf("expected fetch to hit /v1/models/{model}, got path %q", gotPath)
	}
	if got := cfg.GetContextWindow(); got != 555000 {
		t.Fatalf("fetched window should be used: got %d, want 555000", got)
	}
}

// TestResolveContextWindow_FetchErrorDegrades 覆盖关键路径：当端点出错
// （此处为 500）时，fetch 必须静默失败——不 panic、不阻塞——且
// GetContextWindow 回退到映射表。
func TestResolveContextWindow_FetchErrorDegrades(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(500)
		w.Write([]byte(`{"error":"boom"}`))
	}))
	defer srv.Close()

	cfg := &config.ProviderConfig{
		Protocol: "anthropic", BaseURL: srv.URL, APIKey: "k", Model: "claude-sonnet-4-6",
	}
	ResolveContextWindow(context.Background(), cfg) // 不得 panic

	// 映射表为 claude 提供 200000；失败的 fetch 不得降低该值。
	if got := cfg.GetContextWindow(); got != 200000 {
		t.Fatalf("on fetch error should fall back to mapping table: got %d, want 200000", got)
	}
}

// TestResolveContextWindow_UnreachableDegrades 模拟一个已失效的端点
// （服务器已关闭）。带超时上限的 fetch 必须降级到映射表，既不挂起也不崩溃。
func TestResolveContextWindow_UnreachableDegrades(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	url := srv.URL
	srv.Close() // 立即关闭，使连接被拒绝

	cfg := &config.ProviderConfig{
		Protocol: "anthropic", BaseURL: url, APIKey: "k", Model: "gpt-4o",
	}
	ResolveContextWindow(context.Background(), cfg)

	if got := cfg.GetContextWindow(); got != 128000 {
		t.Fatalf("unreachable endpoint should fall back: got %d, want 128000", got)
	}
}

// TestResolveContextWindow_NonAnthropicSkipped 确认第 2 层只适用于
// Anthropic 协议的 provider：非 anthropic provider 永远不会被 fetch，
// 直接通过映射表解析。
func TestResolveContextWindow_NonAnthropicSkipped(t *testing.T) {
	called := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		w.Write([]byte(`{"max_input_tokens":999999}`))
	}))
	defer srv.Close()

	cfg := &config.ProviderConfig{
		Protocol: "openai-compat", BaseURL: srv.URL, APIKey: "k", Model: "gpt-4o",
	}
	ResolveContextWindow(context.Background(), cfg)

	if called {
		t.Error("non-anthropic provider must not trigger a /v1/models fetch")
	}
	if got := cfg.GetContextWindow(); got != 128000 {
		t.Fatalf("non-anthropic should use mapping table: got %d, want 128000", got)
	}
}

// TestResolveContextWindow_ConfigOverrideSkipsFetch 确认显式的配置窗口
// 会完全短路 fetch（不发起任何网络调用）。
func TestResolveContextWindow_ConfigOverrideSkipsFetch(t *testing.T) {
	called := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		w.Write([]byte(`{"max_input_tokens":999999}`))
	}))
	defer srv.Close()

	cfg := &config.ProviderConfig{
		Protocol: "anthropic", BaseURL: srv.URL, APIKey: "k",
		Model: "claude-sonnet-4-6", ContextWindow: 4096,
	}
	ResolveContextWindow(context.Background(), cfg)

	if called {
		t.Error("explicit config window must skip the fetch")
	}
	if got := cfg.GetContextWindow(); got != 4096 {
		t.Fatalf("config window should win: got %d, want 4096", got)
	}
}
