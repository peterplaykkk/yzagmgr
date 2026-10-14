// 本文件是 providers.json 的**管理面写入面**（List/Replace），与 upstream.go 的
// 只读解析面共处一个实例：同一个 *Source 同时满足 api.ProviderRegistry（管理面
// GET/PUT /api/v1/providers）与 proxy.ProviderSource（数据面臂注入）——「两处同一
// 实例」是 IP1 接线的硬要求，否则写盘与热读会看两份状态。
//
// 纪律（design-b §1.2 / handlers_providers.go 语义要求）：
//   - 落盘一律 privfile.WritePrivateFile（0600 原子替换，失败不留半张表）；
//   - 回读按 name 升序、整表替换后与回读一致；DTO 无明文 key 字段（结构上漏不出去）；
//   - 写盘后使热读快照失效——下一次数据面请求现读新表（热轮换语义）。
package upstream

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"

	"github.com/peterplaykkk/yzagmgr/internal/apierr"
	"github.com/peterplaykkk/yzagmgr/internal/privfile"
)

// SetResolver 安装 credRef → 托管凭据库解析器（装配期一次：api.New 造好 tokenVault
// 后回填，早于任何监听/请求，见 deps.go 接线注释）。nil = 未接线。
func (s *Source) SetResolver(r CredRefResolver) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.resolver = r
}

// ProviderEntry 是注册表一个 provider 的管理面形状（design-b §1.2 providers.json
// 条目；api.ProviderEntry 是它的类型别名——接口方法签名与本类型同一，*Source 因此
// 同时满足 api.ProviderRegistry 与 proxy.ProviderSource，「两处同一实例」成立）。
type ProviderEntry struct {
	// Name 是 provider 词表值（glm/openai/anthropic——与 arm.go 闭集同源，IP6）。
	Name string `json:"name"`
	// Enabled false = 该 provider 全部臂请求按「已停用」404 拒绝（与「已移除」区分）。
	Enabled bool `json:"enabled"`
	// Bases 按线覆写上游基址（仅测试/代理/灰度需要；缺省 = 代码内置官方注册表）。
	Bases map[string]string `json:"bases,omitempty"`
	// CredRef 是托管凭据引用（如 yzagmgr-managed-token://<uid>）；**永远不是明文 key**。
	CredRef string `json:"credRef,omitempty"`
}

// entryForRead 把文件层条目转成管理面 DTO（enabled 缺席 = true）。
func entryForRead(name string, e providerEntry) ProviderEntry {
	enabled := true
	if e.Enabled != nil {
		enabled = *e.Enabled
	}
	return ProviderEntry{Name: name, Enabled: enabled, Bases: e.Bases, CredRef: e.CredRef}
}

// ListProviders 按 name 升序回读全部条目（满足 api.ProviderRegistry）。
//
// 文件不存在 = 出厂常态 ⇒ 空表；权限过宽/形状坏 = **显式报错**（管理面不能把
// 「文件坏了」静默呈现成「表是空的」——读面数据路径的 fail-safe 是拒用+Warn+落
// env，管理面必须让人看见坏文件本身）。
func (s *Source) ListProviders(context.Context) ([]ProviderEntry, error) {
	table, err := s.readTableStrict()
	if err != nil {
		return nil, err
	}
	out := make([]ProviderEntry, 0, len(table))
	for name, e := range table {
		out = append(out, entryForRead(name, e))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// ReplaceProviders 整表替换 providers.json（满足 api.ProviderRegistry）：
// 校验 → 0600 原子写 → 热读快照失效 → 回读新表（与写入一致）。
func (s *Source) ReplaceProviders(_ context.Context, entries []ProviderEntry) ([]ProviderEntry, error) {
	table := make(map[string]providerEntry, len(entries))
	for i, e := range entries {
		name := filepath.Base(e.Name) // 防御：name 是词表值，绝不允许是路径形态
		if e.Name == "" || name != e.Name || name == "." || name == string(filepath.Separator) {
			return nil, apierr.New(apierr.CodeInvalidRequest,
				"providers 第 %d 条 name 非法（要求 provider 词表值，非路径）: %q", i+1, e.Name)
		}
		if _, dup := table[e.Name]; dup {
			return nil, apierr.New(apierr.CodeInvalidRequest,
				"providers 条目重复: %q（整表替换须去重）", e.Name)
		}
		enabled := e.Enabled
		table[e.Name] = providerEntry{
			Enabled: &enabled,
			Bases:   e.Bases,
			CredRef: e.CredRef,
		}
	}
	document := struct {
		Providers map[string]providerEntry `json:"providers"`
	}{Providers: table}
	raw, err := json.MarshalIndent(&document, "", "  ")
	if err != nil {
		return nil, apierr.Wrap(apierr.CodeInternal, err, "编码 providers.json 失败")
	}
	raw = append(raw, '\n')
	// 0600 原子替换：privfile 在同目录写临时文件后 rename，失败不留半张表。
	if err := privfile.WritePrivateFile(s.path, raw, 0o600); err != nil {
		return nil, apierr.Wrap(apierr.CodeStorage, err, "写入 providers.json 失败")
	}
	// 热读快照失效：下一次 UpstreamKey/UpstreamBase/ProviderEnabled 现读新表。
	s.mu.Lock()
	s.snapshot, s.sig = nil, fileSig{}
	s.mu.Unlock()
	return s.ListProviders(context.Background())
}

// readTableStrict 是管理面的严格读：区分「文件不存在（空表）」与「文件坏了（报错）」。
func (s *Source) readTableStrict() (map[string]providerEntry, error) {
	fi, err := os.Stat(s.path)
	if err != nil {
		if os.IsNotExist(err) {
			return map[string]providerEntry{}, nil
		}
		return nil, apierr.Wrap(apierr.CodeStorage, err, "stat providers.json 失败")
	}
	if fi.Mode().Perm()&0o077 != 0 {
		return nil, apierr.New(apierr.CodeStorage,
			"providers.json 权限过宽（%o，要求 0600）：拒绝读取", fi.Mode().Perm())
	}
	raw, err := os.ReadFile(filepath.Clean(s.path))
	if err != nil {
		return nil, apierr.Wrap(apierr.CodeStorage, err, "读取 providers.json 失败")
	}
	var shape fileShape
	if err := json.Unmarshal(raw, &shape); err != nil || shape.Providers == nil {
		return nil, apierr.New(apierr.CodeStorage,
			"providers.json 形状非法（需要 {\"providers\":{…}}）：拒绝读取")
	}
	return shape.Providers, nil
}
