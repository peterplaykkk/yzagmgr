// Package upstream 是臂上游的**凭据与配置三层链**（design.md §1.3 /
// design-b §1.2）：`<data_root>/providers.json`（0600，热轮换）+ env 兜底
// `YZAGMGR_UPSTREAM_{GLM,OPENAI,ANTHROPIC}_KEY`（每请求现读）+ 可选的托管凭据库
// credRef 解析（vault 命中优先于 env，撤销/读失败回落 env）。
//
// 优先级（降序，每层缺省即落到下一层）：
//
//  1. 请求自带真 key —— 不经本包（proxy/armauth.go 的透传分支最高）；
//  2. providers.json 条目：
//     - `credRef` 非空 ⇒ 经注入的 CredRefResolver 解析（vault 命中即返回；
//     撤销/为空/读错 ⇒ 落到 env 层）；
//     - `bases` 按线覆写上游基址、`enabled:false` 停用该 provider（404 语义在
//     proxy 侧执行）；
//  3. env 兜底（部署注入，容器 `--env-file` 0600；每请求 os.Getenv 现读，不缓存）。
//
// 密钥纪律：key 明文只经返回值传递，不入日志、错误文本、API 响应；管理面写入
// （GET/PUT /api/v1/providers 的 List/Replace）在本包 registry.go，落盘走
// privfile.WritePrivateFile（0600 原子替换）纪律。
//
// 文件层的三条 fail-safe（本包自定，已在 records 登记）：
//   - 权限宽于 0600（perm&0077 != 0）⇒ 拒用该文件（Warn）并落到 env 层；
//   - JSON 形状坏/缺 `providers` 键 ⇒ 拒用该文件（Warn）并落到 env 层——
//     「文件坏了」不等于「没有凭据」，env 仍可服务；
//   - 文件不存在 ⇒ 静默走 env（出厂常态）。
package upstream

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

// CredRefResolver 按凭据引用解析托管令牌明文（结构类型：proxy.ArmTokenSource
// 的方法集与它逐字一致，直接注入即满足）。
//
// 纪律与 proxy.ArmTokenSource 相同：不落盘、不记明文；返回 ("", nil) 表示该引用
// 当前无令牌（视为未命中，落 env 层）。
type CredRefResolver interface {
	InferenceTokenFor(ref string) (string, error)
}

// providerEntry 是 providers.json 里一个 provider 的条目（design-b §1.2 形状）。
type providerEntry struct {
	// Enabled 缺省 true（JSON bool 零值是 false，用指针区分「缺席」与显式 false）。
	Enabled *bool             `json:"enabled"`
	Bases   map[string]string `json:"bases"`
	CredRef string            `json:"credRef"`
}

// fileShape 是 providers.json 的整体形状：`{"providers": {...}}`。
type fileShape struct {
	Providers map[string]providerEntry `json:"providers"`
}

// Options 是 Source 的注入点。
type Options struct {
	// Resolver 解析 credRef → 托管令牌明文；nil = 未接线（credRef 层视为缺省，
	// 直接落 env 层——rotated/未接线语义一致）。
	Resolver CredRefResolver
	// Logger 结构化日志；nil = slog.Default()（文件层的 Warn 必须留痕）。
	Logger *slog.Logger
	// Getenv 读 env 兜底；nil = os.Getenv（每请求现读，测试注入替身）。
	Getenv func(string) string
}

// Source 是三层链的只读解析面（并发安全；文件层按 mtime+size 热读缓存）。
type Source struct {
	path     string
	resolver CredRefResolver
	log      *slog.Logger
	getenv   func(string) string

	mu       sync.Mutex
	snapshot map[string]providerEntry // 上一次成功装载的文件层；nil = 无文件/拒用
	sig      fileSig                  // 文件指纹（mtime+size），用于热读判断
	warned   string                   // 上一次 Warn 的主题（去重，避免每请求刷屏）
}

type fileSig struct {
	mtime int64
	size  int64
	ok    bool
}

// New 构造解析链。path 是 `<data_root>/providers.json`（可不存在）。
func New(path string, opts Options) *Source {
	logger := opts.Logger
	if logger == nil {
		logger = slog.Default()
	}
	getenv := opts.Getenv
	if getenv == nil {
		getenv = os.Getenv
	}
	return &Source{
		path:     path,
		resolver: opts.Resolver,
		log:      logger,
		getenv:   getenv,
	}
}

// UpstreamKey 解析该 provider 的出网真 key（满足 proxy.ProviderSource）。
//
// wire 入参保留给按线差异化的实现（本实现只按 provider 取键——三层链里没有线路
// 维度），签名与 proxy.ProviderSource 逐字一致。
func (s *Source) UpstreamKey(ctx context.Context, provider string, wire string) (string, error) {
	_ = wire
	// ② providers.json credRef 层（vault 命中即返回；撤销/为空/读错落 env）。
	if entry, ok := s.entry(provider); ok && entry.CredRef != "" && s.resolver != nil {
		tok, err := s.resolver.InferenceTokenFor(entry.CredRef)
		if err == nil && strings.TrimSpace(tok) != "" {
			return tok, nil
		}
		// 落层留痕：撤销是用户动作（回落 env 是设计语义），读错是故障——都值得 Debug。
		s.log.Debug("providers.json credRef 层未命中，落到 env 兜底",
			"provider", provider, "err", errText(err))
	}
	// ③ env 兜底（每请求现读；改 env 文件 + 重启生效，design-b §1.2）。
	if key := s.envKey(provider); key != "" {
		return key, nil
	}
	return "", nil // 未配置（proxy 侧映射为 401 未出网）
}

// UpstreamBase 给出 providers.json 的基址覆写（满足 proxy.ProviderSource）。
// 无文件/无条目/无该线覆写 ⇒ ("", false) ⇒ 调用方走内置注册表。
func (s *Source) UpstreamBase(provider string, wireGroup string) (string, bool) {
	entry, ok := s.entry(provider)
	if !ok {
		return "", false
	}
	base := strings.TrimSpace(entry.Bases[wireGroup])
	if base == "" {
		return "", false
	}
	return base, true
}

// ProviderEnabled 报告 provider 是否启用（满足 proxy.ProviderSource）。
// 无文件/无条目/显式 true ⇒ true；显式 `enabled:false` ⇒ false。
func (s *Source) ProviderEnabled(provider string) bool {
	entry, ok := s.entry(provider)
	if !ok {
		return true
	}
	return entry.Enabled == nil || *entry.Enabled
}

// envKey 读 env 兜底键名 `YZAGMGR_UPSTREAM_<PROVIDER>_KEY`（provider 先大写）。
func (s *Source) envKey(provider string) string {
	if provider == "" {
		return ""
	}
	for _, r := range provider {
		ok := (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '_'
		if !ok {
			return "" // 形状不对的 provider 不进 env 查找（不猜键名）
		}
	}
	return strings.TrimSpace(s.getenv("YZAGMGR_UPSTREAM_" + strings.ToUpper(provider) + "_KEY"))
}

// entry 取 provider 的文件层条目（热读：stat 指纹未变则复用上次解析结果）。
func (s *Source) entry(provider string) (providerEntry, bool) {
	table := s.loadTable()
	if table == nil {
		return providerEntry{}, false
	}
	e, ok := table[provider]
	return e, ok
}

// loadTable 热读 providers.json。文件不存在 = 正常出厂态（静默 nil）；
// 权限/形状问题各 Warn 一次（按主题去重，修复前不刷屏）。全程持 s.mu（Warn 路径
// 与快照写入互斥，避免并发 Warn 竞态）。
func (s *Source) loadTable() map[string]providerEntry {
	s.mu.Lock()
	defer s.mu.Unlock()

	fi, err := os.Stat(s.path)
	if err != nil {
		if !os.IsNotExist(err) {
			s.warnOnceLocked("stat", "providers.json 不可读，文件层暂缺（env 兜底仍生效）",
				"path", s.path, "err", err)
		}
		s.snapshot, s.sig = nil, fileSig{}
		return nil
	}
	sig := fileSig{mtime: fi.ModTime().UnixNano(), size: fi.Size(), ok: true}
	if sig == s.sig && s.snapshot != nil {
		return s.snapshot
	}
	// 权限守卫：必须私有（0600 或更严；0644 也拒——里面可能有 credRef/覆写）。
	if fi.Mode().Perm()&0o077 != 0 {
		s.warnOnceLocked("perm",
			fmt.Sprintf("providers.json 权限过宽（%o，要求 0600）：拒用文件层，env 兜底仍生效", fi.Mode().Perm()),
			"path", s.path)
		s.snapshot, s.sig = nil, fileSig{}
		return nil
	}
	raw, err := os.ReadFile(filepath.Clean(s.path))
	if err != nil {
		s.warnOnceLocked("read", "providers.json 读取失败：拒用文件层，env 兜底仍生效",
			"path", s.path, "err", err)
		s.snapshot, s.sig = nil, fileSig{}
		return nil
	}
	var shape fileShape
	if err := json.Unmarshal(raw, &shape); err != nil || shape.Providers == nil {
		s.warnOnceLocked("shape", "providers.json 形状非法（需要 {\"providers\":{…}}）：拒用文件层，env 兜底仍生效",
			"path", s.path, "err", errText(err))
		s.snapshot, s.sig = nil, fileSig{}
		return nil
	}
	s.snapshot, s.sig, s.warned = shape.Providers, sig, ""
	return shape.Providers
}

// warnOnceLocked 按主题去重的 Warn（文件持续坏着时不该每请求刷一条）；
// 调用方必须已持 s.mu。
func (s *Source) warnOnceLocked(topic, msg string, kv ...any) {
	if s.warned == topic {
		return
	}
	s.warned = topic
	s.log.Warn(msg, kv...)
}

// errText 把可能为 nil 的错误转成日志文本。
func errText(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}
