// v8 上游注册表配置面：`GET/PUT /api/v1/providers`——design.md §1.3 配置三层 /
// design-b §6.1-10（providers.json 读写）。
//
// 密钥纪律：响应只回 credRef（凭据**引用**，0600 库里的键不是明文）与 bases（URL，
// 非密钥）；DTO 里没有承载 key 明文的字段——结构上漏不出去，不是靠过滤漏出去。
// PUT 为整表替换（回读一致），落盘必须走 WritePrivateFile 纪律（0600）；
// 写面由 ProviderRegistry 的实现方（internal/upstream 的写入面，T2/集成）执行，
// 本文件只做 HTTP 形状与参数校验。
//
// 接线（IP1，已落）：Deps.Providers 与 proxy.Options.Providers 注入**同一个**
// *upstream.Source 实例（cmd/yzagmgr assemble 构造），credRef 解析器由 api.New 在
// tokenVault 造好后回填（deps.go 的 SetResolver 接线）。
package api

import (
	"context"
	"net/http"

	"github.com/peterplaykkk/yzagmgr/internal/apierr"
	"github.com/peterplaykkk/yzagmgr/internal/upstream"
)

// ProviderEntry 是注册表一个 provider 的管理面形状——本体定义在 internal/upstream
// （providers.json 条目的属地包），这里是类型别名：接口方法签名用同一类型，
// *upstream.Source 才能不改方法集地满足 ProviderRegistry（IP1「两处同一实例」）。
type ProviderEntry = upstream.ProviderEntry

// ProviderRegistry 是注册表读写的最小依赖面（internal/upstream 的管理面实现满足它；
// Deps.Providers 的字段类型）。
//
// 语义要求：
//   - ListProviders 按 name 升序回读全部条目（不含任何明文密钥）；
//   - ReplaceProviders 整表替换并回读一致；写盘 0600（WritePrivateFile 纪律）；
//     失败不得留下半张表（单事务/原子写）。
type ProviderRegistry interface {
	ListProviders(ctx context.Context) ([]ProviderEntry, error)
	ReplaceProviders(ctx context.Context, entries []ProviderEntry) ([]ProviderEntry, error)
}

// providerTable 是 GET/PUT 的信封形状（与 PricingTable 同风格）。
type providerTable struct {
	Entries []ProviderEntry `json:"entries"`
}

// requireProviders 取注册表；未装配时以 storage 明确报出（不静默空表——
// 「没接线」与「表是空的」是两件事）。
func (s *Server) requireProviders() (ProviderRegistry, error) {
	if s.Providers == nil {
		return nil, apierr.New(apierr.CodeStorage,
			"上游注册表未装配（启动时未注入），见进程日志")
	}
	return s.Providers, nil
}

func (s *Server) handleProvidersGet(r *http.Request) (any, error) {
	reg, err := s.requireProviders()
	if err != nil {
		return nil, err
	}
	entries, err := reg.ListProviders(r.Context())
	if err != nil {
		return nil, err
	}
	if entries == nil {
		entries = []ProviderEntry{}
	}
	return providerTable{Entries: entries}, nil
}

// handleProvidersSave 是整表替换（回读一致；第二返回值 = 操作日志 target，恒空）。
func (s *Server) handleProvidersSave(r *http.Request) (any, string, error) {
	reg, err := s.requireProviders()
	if err != nil {
		return nil, "", err
	}
	var body providerTable
	if err := decode(r, &body); err != nil {
		return nil, "", err
	}
	for i, e := range body.Entries {
		if e.Name == "" {
			return nil, "", apierr.New(apierr.CodeInvalidRequest,
				"providers 第 %d 条缺 name", i+1)
		}
	}
	entries, err := reg.ReplaceProviders(r.Context(), body.Entries)
	if err != nil {
		return nil, "", err
	}
	if entries == nil {
		entries = []ProviderEntry{}
	}
	return providerTable{Entries: entries}, "", nil
}
