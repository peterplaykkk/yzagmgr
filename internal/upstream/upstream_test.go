package upstream

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// 本文件覆盖三层链解析（design-b §1.2 / U16）：providers.json credRef → vault →
// env 兜底、bases 覆写、enabled 位、权限/形状 fail-safe、热读。

// fakeResolver 是 CredRefResolver 的测试替身（vault 形状）。
type fakeResolver struct {
	mu  sync.Mutex
	tok map[string]string
	err map[string]error
}

func (f *fakeResolver) InferenceTokenFor(ref string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err, ok := f.err[ref]; ok {
		return "", err
	}
	return f.tok[ref], nil
}

func (f *fakeResolver) set(ref, tok string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.tok == nil {
		f.tok = map[string]string{}
	}
	f.tok[ref] = tok
}

func (f *fakeResolver) setErr(ref string, err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err == nil {
		f.err = map[string]error{}
	}
	f.err[ref] = err
}

// newTestEnv 造 (path, getenv) 夹具：env 表可预置，providers.json 按需写入。
func newTestEnv(t *testing.T, providersJSON string, env map[string]string) (string, func(string) string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "providers.json")
	if providersJSON != "" {
		if err := os.WriteFile(path, []byte(providersJSON), 0o600); err != nil {
			t.Fatalf("写 providers.json: %v", err)
		}
	}
	getenv := func(k string) string { return env[k] }
	return path, getenv
}

func quiet(t *testing.T) *slog.Logger {
	t.Helper()
	return slog.New(slog.NewTextHandler(io.Discard, nil)) // 不污染测试输出；Warn 由专门用例捕获
}

// TestUpstreamKeyResolutionChain 钉 U16 的 credRef 链：
// vault 命中 > env；撤销/读错回落 env；全无 ⇒ ("", nil)（proxy 侧映射 401 U4）。
func TestUpstreamKeyResolutionChain(t *testing.T) {
	ctx := context.Background()
	const json = `{"providers":{"glm":{"credRef":"yzagmgr-managed-token://g1"}}}`

	t.Run("vault 命中优先于 env", func(t *testing.T) {
		path, getenv := newTestEnv(t, json, map[string]string{
			"YZAGMGR_UPSTREAM_GLM_KEY": "env-key",
		})
		res := &fakeResolver{tok: map[string]string{"yzagmgr-managed-token://g1": "vault-key"}}
		src := New(path, Options{Resolver: res, Getenv: getenv, Logger: quiet(t)})
		got, err := src.UpstreamKey(ctx, "glm", "anthropic")
		if err != nil || got != "vault-key" {
			t.Fatalf("UpstreamKey = (%q, %v)，期望 vault-key", got, err)
		}
	})

	t.Run("撤销回落 env", func(t *testing.T) {
		path, getenv := newTestEnv(t, json, map[string]string{
			"YZAGMGR_UPSTREAM_GLM_KEY": "env-key",
		})
		res := &fakeResolver{}
		res.setErr("yzagmgr-managed-token://g1", errors.New("托管凭据已被撤销"))
		src := New(path, Options{Resolver: res, Getenv: getenv, Logger: quiet(t)})
		got, err := src.UpstreamKey(ctx, "glm", "anthropic")
		if err != nil || got != "env-key" {
			t.Fatalf("撤销后应回落 env：(%q, %v)", got, err)
		}
	})

	t.Run("vault 空令牌回落 env", func(t *testing.T) {
		path, getenv := newTestEnv(t, json, map[string]string{
			"YZAGMGR_UPSTREAM_GLM_KEY": "env-key",
		})
		res := &fakeResolver{tok: map[string]string{"yzagmgr-managed-token://g1": "  "}}
		src := New(path, Options{Resolver: res, Getenv: getenv, Logger: quiet(t)})
		got, err := src.UpstreamKey(ctx, "glm", "anthropic")
		if err != nil || got != "env-key" {
			t.Fatalf("空令牌应回落 env：(%q, %v)", got, err)
		}
	})

	t.Run("无文件走 env；无文件无 env ⇒ 未配置", func(t *testing.T) {
		path, getenv := newTestEnv(t, "", map[string]string{
			"YZAGMGR_UPSTREAM_OPENAI_KEY": "openai-env",
		})
		src := New(path, Options{Getenv: getenv, Logger: quiet(t)})
		if got, err := src.UpstreamKey(ctx, "openai", "chat"); err != nil || got != "openai-env" {
			t.Fatalf("env 兜底 = (%q, %v)", got, err)
		}
		if got, err := src.UpstreamKey(ctx, "glm", "anthropic"); err != nil || got != "" {
			t.Fatalf("全无配置应 (\"\", nil)：(%q, %v)", got, err)
		}
	})

	t.Run("credRef 层未接线（resolver nil）⇒ 直接 env", func(t *testing.T) {
		path, getenv := newTestEnv(t, json, map[string]string{
			"YZAGMGR_UPSTREAM_GLM_KEY": "env-key",
		})
		src := New(path, Options{Getenv: getenv, Logger: quiet(t)})
		got, err := src.UpstreamKey(ctx, "glm", "anthropic")
		if err != nil || got != "env-key" {
			t.Fatalf("resolver 未接线应直接 env：(%q, %v)", got, err)
		}
	})

	t.Run("provider 词形不对 ⇒ 不进 env 查找", func(t *testing.T) {
		path, getenv := newTestEnv(t, "", map[string]string{
			"YZAGMGR_UPSTREAM_BAD-PROVIDER_KEY": "nope",
		})
		src := New(path, Options{Getenv: getenv, Logger: quiet(t)})
		if got, err := src.UpstreamKey(ctx, "bad-provider", "chat"); err != nil || got != "" {
			t.Fatalf("非法 provider 不该命中 env：(%q, %v)", got, err)
		}
	})
}

// TestUpstreamBasesAndEnabled 钉 bases 覆写与 enabled 位（U16 另两断言）。
func TestUpstreamBasesAndEnabled(t *testing.T) {
	const json = `{"providers":{` +
		`"glm":{"bases":{"anthropic":"https://proxy.example/glm"}},` +
		`"openai":{"enabled":false},` +
		`"anthropic":{}}}`
	path, getenv := newTestEnv(t, json, nil)
	src := New(path, Options{Getenv: getenv, Logger: quiet(t)})

	if base, ok := src.UpstreamBase("glm", "anthropic"); !ok || base != "https://proxy.example/glm" {
		t.Fatalf("bases 覆写 = (%q, %v)", base, ok)
	}
	if _, ok := src.UpstreamBase("glm", "chat"); ok {
		t.Fatal("未登记的线路不该有覆写")
	}
	if _, ok := src.UpstreamBase("unknown", "chat"); ok {
		t.Fatal("未知 provider 不该有覆写")
	}
	if src.ProviderEnabled("openai") {
		t.Fatal("enabled:false 应判停用")
	}
	if !src.ProviderEnabled("glm") || !src.ProviderEnabled("anthropic") {
		t.Fatal("缺席 enabled 应为启用（零值缺省 true）")
	}
	// 无文件：一切启用、无覆写。
	empty, _ := newTestEnv(t, "", nil)
	src2 := New(empty, Options{Logger: quiet(t)})
	if !src2.ProviderEnabled("glm") {
		t.Fatal("无文件时应启用")
	}
	if _, ok := src2.UpstreamBase("glm", "chat"); ok {
		t.Fatal("无文件时不该有覆写")
	}
}

// TestUpstreamFileFailsafes 钉文件层三条 fail-safe：权限过宽/形状坏 ⇒ 拒用文件层
// 落 env（各 Warn 一次）；文件不存在 ⇒ 静默。
func TestUpstreamFileFailsafes(t *testing.T) {
	ctx := context.Background()
	const json = `{"providers":{"glm":{"credRef":"yzagmgr-managed-token://g1"}}}`

	t.Run("权限过宽（0644）拒用并落 env", func(t *testing.T) {
		path, getenv := newTestEnv(t, json, map[string]string{
			"YZAGMGR_UPSTREAM_GLM_KEY": "env-key",
		})
		if err := os.Chmod(path, 0o644); err != nil {
			t.Fatalf("chmod: %v", err)
		}
		res := &fakeResolver{tok: map[string]string{"yzagmgr-managed-token://g1": "vault-key"}}
		var buf strings.Builder
		logger := slog.New(slog.NewTextHandler(&lockedBuf{b: &buf}, nil))
		src := New(path, Options{Resolver: res, Getenv: getenv, Logger: logger})
		got, err := src.UpstreamKey(ctx, "glm", "anthropic")
		if err != nil || got != "env-key" {
			t.Fatalf("权限过宽应拒文件层落 env：(%q, %v)", got, err)
		}
		if !strings.Contains(buf.String(), "权限过宽") {
			t.Fatalf("应留 Warn：%s", buf.String())
		}
	})

	t.Run("形状坏拒用并落 env", func(t *testing.T) {
		path, getenv := newTestEnv(t, `{"glm":{}}`, map[string]string{
			"YZAGMGR_UPSTREAM_GLM_KEY": "env-key",
		})
		var buf strings.Builder
		logger := slog.New(slog.NewTextHandler(&lockedBuf{b: &buf}, nil))
		src := New(path, Options{Getenv: getenv, Logger: logger})
		got, err := src.UpstreamKey(ctx, "glm", "anthropic")
		if err != nil || got != "env-key" {
			t.Fatalf("形状坏应拒文件层落 env：(%q, %v)", got, err)
		}
		if !strings.Contains(buf.String(), "形状非法") {
			t.Fatalf("应留 Warn：%s", buf.String())
		}
		// 停用位同样拒用（形状坏的文件不该让 provider 停摆）。
		if !src.ProviderEnabled("glm") {
			t.Fatal("形状坏时 enabled 位应视为缺席（启用）")
		}
	})

	t.Run("文件不存在静默走 env", func(t *testing.T) {
		path, getenv := newTestEnv(t, "", map[string]string{
			"YZAGMGR_UPSTREAM_GLM_KEY": "env-key",
		})
		var buf strings.Builder
		logger := slog.New(slog.NewTextHandler(&lockedBuf{b: &buf}, nil))
		src := New(path, Options{Getenv: getenv, Logger: logger})
		if got, _ := src.UpstreamKey(ctx, "glm", "anthropic"); got != "env-key" {
			t.Fatalf("env 兜底 = %q", got)
		}
		if buf.Len() != 0 {
			t.Fatalf("出厂态不该 Warn：%s", buf.String())
		}
	})
}

// TestUpstreamHotRead 钉热轮换：改文件（内容与 mtime 变化）后下一次解析即取新值，
// 不需要重启。
func TestUpstreamHotRead(t *testing.T) {
	path, getenv := newTestEnv(t, `{"providers":{"glm":{"enabled":false}}}`, nil)
	src := New(path, Options{Getenv: getenv, Logger: quiet(t)})
	if src.ProviderEnabled("glm") {
		t.Fatal("初始应停用")
	}
	next := `{"providers":{"glm":{"enabled":true,"bases":{"chat":"https://hot.example/v1"}}}}`
	if err := os.WriteFile(path, []byte(next), 0o600); err != nil {
		t.Fatalf("rewrite: %v", err)
	}
	// mtime 精度不足时用内容长度差触发 sig 变化（两串长度不同）。
	if !src.ProviderEnabled("glm") {
		t.Fatal("热读：改文件后应立即启用")
	}
	if base, ok := src.UpstreamBase("glm", "chat"); !ok || base != "https://hot.example/v1" {
		t.Fatalf("热读 bases = (%q, %v)", base, ok)
	}
}

// lockedBuf 是给 slog 用的极简串缓冲（测试侧并发写安全）。
type lockedBuf struct {
	mu sync.Mutex
	b  *strings.Builder
}

func (w *lockedBuf) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.b.Write(p)
}
