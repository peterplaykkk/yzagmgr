package proxy

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"sync"
)

// 本文件是 T2 清理后各测试族共享的替身与小工具（F3 注入面、日志捕获）。

// testUpstreamKey 是测试臂上游的固定真 key（断言注入结果用）。
const testUpstreamKey = "arm-upstream-key-9f2c"

// testProviderSource 是 ProviderSource 的测试替身（F3 注入面 / providers.json 三层链
// 的进程内形状）：按 provider 给 key/基址覆写/停用位，可注入解析错误。
type testProviderSource struct {
	mu       sync.Mutex
	keys     map[string]string // provider → key（缺席 = 未配置 ⇒ 401）
	bases    map[string]string // "provider|wireGroup" → 基址覆写
	disabled map[string]bool   // provider → 停用位
	keyErr   error             // 非 nil：UpstreamKey 一律返回该错误
	keyCalls int               // UpstreamKey 调用计数（断言「真 key 不触源」）
}

func newTestProviders() *testProviderSource {
	return &testProviderSource{
		keys: map[string]string{
			"glm":       testUpstreamKey,
			"openai":    testUpstreamKey,
			"anthropic": testUpstreamKey,
		},
		bases:    map[string]string{},
		disabled: map[string]bool{},
	}
}

func (p *testProviderSource) UpstreamKey(_ context.Context, provider string, _ string) (string, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.keyCalls++
	if p.keyErr != nil {
		return "", p.keyErr
	}
	return p.keys[provider], nil
}

func (p *testProviderSource) UpstreamBase(provider string, wireGroup string) (string, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	base, ok := p.bases[provider+"|"+wireGroup]
	return base, ok
}

func (p *testProviderSource) ProviderEnabled(provider string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return !p.disabled[provider]
}

func (p *testProviderSource) setKey(provider, key string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.keys[provider] = key
}

func (p *testProviderSource) setBase(provider, wireGroup, base string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.bases[provider+"|"+wireGroup] = base
}

func (p *testProviderSource) setDisabled(provider string, off bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.disabled[provider] = off
}

func (p *testProviderSource) setKeyErr(err error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.keyErr = err
}

func (p *testProviderSource) calls() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.keyCalls
}

// captureLogger 是把日志写进内存缓冲的日志器（退役 Warn / 暴露面 Warn 断言用）。
type captureLogger struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

// logger 返回绑定到本缓冲的 slog.Logger（info 起记，测试要看 Warn 与 Info 两类）。
func (c *captureLogger) logger() *slog.Logger {
	return slog.New(slog.NewTextHandler(&lockedWriter{c: c}, &slog.HandlerOptions{Level: slog.LevelInfo}))
}

// contains 报告日志文本是否包含某子串（并发安全）。
func (c *captureLogger) contains(s string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return strings.Contains(c.buf.String(), s)
}

// text 返回日志全文（失败信息展示用）。
func (c *captureLogger) text() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.buf.String()
}

// lockedWriter 把写入串到 captureLogger 的锁上（slog handler 并发写同一缓冲）。
type lockedWriter struct{ c *captureLogger }

func (w *lockedWriter) Write(p []byte) (int, error) {
	w.c.mu.Lock()
	defer w.c.mu.Unlock()
	return w.c.buf.Write(p)
}
