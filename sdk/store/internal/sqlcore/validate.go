package sqlcore

import (
	"fmt"
	"net/url"
	"strings"

	"github.com/peterplaykkk/yzagmgr/sdk/router"
	"github.com/peterplaykkk/yzagmgr/sdk/store"
)

// 本文件是写库前的**形态校验与归一**（fail-closed，先于任何写库动作）。
// 归一结果只在 sqlcore 内部使用；哨兵错误（store.Err*）是核心包的公共契约。

// 臂名上限的唯一来源 = `router.ArmNameMaxLen`（当前 96；2026-09-25 随臂前缀
// `u-<user>-<session>-<provider>` 的长邮箱 slug 场景放宽，原 64）。镜像
// internal/proxy 的 arm.go（对方 lane 已同为 96）：臂名同时是请求侧 `model` 取值与
// 日志 target，**改一处必须同步另一处**——此处直接引用 router 常量，杜绝再分叉。

// validateArmName 校验臂名：首字符小写字母，其余小写字母/数字/-/_/.，≤ ArmNameMaxLen 字节。
func validateArmName(name string) error {
	if name == "" {
		return fmt.Errorf("%w：请求映射名不得为空", store.ErrConfig)
	}
	if len(name) > router.ArmNameMaxLen {
		return fmt.Errorf("%w：请求映射名过长（最多 %d 字节，实际 %d）", store.ErrConfig, router.ArmNameMaxLen, len(name))
	}
	if name[0] < 'a' || name[0] > 'z' {
		return fmt.Errorf("%w：请求映射名必须以小写字母起头（a-z），实际是 %q", store.ErrConfig, name)
	}
	for i := 0; i < len(name); i++ {
		c := name[i]
		ok := (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') || c == '-' || c == '_' || c == '.'
		if !ok {
			return fmt.Errorf("%w：请求映射名只允许小写字母/数字/-/_/.（实际含 %q）", store.ErrConfig, string(c))
		}
	}
	return nil
}

// maxCredRefLen 是凭据引用列的声明长度（MySQL 文本主键上限 191 见 Dialect.keyCap）。
const maxCredRefLen = 255

// validateRef 校验凭据引用的形态：非空、≤255 字节、无空白与控制字符。
//
// 与守护进程不同，SDK **不要求** `yzagmgr-managed-token://<uid>` 方案前缀：
// 嵌入方（如 TUI 会话服务）常按自己的命名登记引用（"openai-prod" 之类）。形态宽松
// 但不接受空白——引用会进 SQL 参数与错误串，含空白只会让人读不出问题。
func validateRef(what, ref string) error {
	if ref == "" {
		return fmt.Errorf("%w：%s：不得为空", store.ErrConfig, what)
	}
	if len(ref) > maxCredRefLen {
		return fmt.Errorf("%w：%s：过长（最多 %d 字节，实际 %d）", store.ErrConfig, what, maxCredRefLen, len(ref))
	}
	for i := 0; i < len(ref); i++ {
		if c := ref[i]; c <= ' ' || c == 0x7f {
			return fmt.Errorf("%w：%s：不得含空白或控制字符（位置 %d）", store.ErrConfig, what, i)
		}
	}
	return nil
}

// normalizeEndpoint 归一并校验上游端点：绝对 http/https URL、有主机名、无内嵌凭据、
// 去尾斜杠（引擎按 `<endpoint> + rest` 原样拼接，留尾斜杠会拼出 `//`）。
//
// 与守护进程 normalizeUpstream 的差异（**刻意**，不是漏做）：SDK 允许回环/本机
// 端点。守护进程拒回环的理由是「代理绝不向自己转发」（UC-007 A13）；而 SDK 是嵌入
// 形态，上游常常就是同机另一个进程（测试桩、本地网关），这条禁令在这里没有依据。
func normalizeEndpoint(raw string) (string, error) {
	s := strings.TrimSpace(raw)
	if s == "" {
		return "", fmt.Errorf("%w：上游端点不得为空", store.ErrConfig)
	}
	u, err := url.Parse(s)
	if err != nil {
		return "", fmt.Errorf("%w：上游端点不是合法 URL：%q", store.ErrConfig, raw)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return "", fmt.Errorf("%w：上游端点的 scheme 必须是 http 或 https，实际是 %q", store.ErrConfig, u.Scheme)
	}
	if u.User != nil {
		return "", fmt.Errorf("%w：上游端点不得内嵌凭据（凭据只经令牌库注入）", store.ErrConfig)
	}
	if u.Hostname() == "" {
		return "", fmt.Errorf("%w：上游端点缺少主机名：%q", store.ErrConfig, raw)
	}
	return strings.TrimRight(s, "/"), nil
}

// normalizeTablePrefix 校验表名前缀（会进 DDL 文本，必须是最保守的标识符）。
func normalizeTablePrefix(raw string) (string, error) {
	p := strings.TrimSpace(raw)
	if p == "" {
		return store.DefaultTablePrefix, nil
	}
	for i := 0; i < len(p); i++ {
		c := p[i]
		ok := c == '_' || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9' && i > 0)
		if !ok {
			return "", fmt.Errorf("%w：表名前缀只允许字母/数字/下划线且不以数字起头，实际是 %q", store.ErrConfig, raw)
		}
	}
	return p, nil
}

// normalizeRoute 归一并校验一条臂配置（写库前调用，不改任何状态）。
func normalizeRoute(rt router.Route) (router.Route, error) {
	name := strings.TrimSpace(rt.Name)
	if err := validateArmName(name); err != nil {
		return router.Route{}, err
	}
	ep, err := normalizeEndpoint(rt.Endpoint)
	if err != nil {
		return router.Route{}, err
	}
	ref := strings.TrimSpace(rt.CredRef)
	if err := validateRef("凭据引用", ref); err != nil {
		return router.Route{}, err
	}
	return router.Route{Name: name, Endpoint: ep, CredRef: ref}, nil
}

// normalizeRemap 归一并校验一条模型映射。
func normalizeRemap(arm string, rm router.Remap) (router.Remap, error) {
	alias := strings.TrimSpace(rm.Alias)
	target := strings.TrimSpace(rm.Target)
	if err := validateRef("映射 alias", alias); err != nil {
		return router.Remap{}, err
	}
	if err := validateRef("映射 target", target); err != nil {
		return router.Remap{}, err
	}
	if alias == target {
		return router.Remap{}, fmt.Errorf("%w：模型映射是自映射（%q → %q），映射会静默失效，拒绝登记", store.ErrConfig, alias, target)
	}
	if alias == arm {
		return router.Remap{}, fmt.Errorf("%w：alias %q 与本臂同名（请求侧选臂恒命中臂名，映射永远不可达），拒绝登记", store.ErrConfig, alias)
	}
	return router.Remap{Alias: alias, Target: target}, nil
}

// normalizeEffortTable 归一并校验一张思考深度表：`to`/force 必须在 store.EffortLadder 内
// （trim+小写归一），`from` 允许空串（= 请求缺 effort 字段时的缺省项）。
// 返回的 ok=false 表示「空表」（items 与 force 都空）= 未配置，调用方按清除处理。
func normalizeEffortTable(table router.EffortTable) (router.EffortTable, bool, error) {
	out := router.EffortTable{ForceEffort: strings.TrimSpace(table.ForceEffort)}
	if out.ForceEffort != "" {
		tier, ok := normalizeTier(out.ForceEffort)
		if !ok {
			return router.EffortTable{}, false, fmt.Errorf("%w：forceEffort %q 不是上游白名单档（%s）",
				store.ErrConfig, out.ForceEffort, strings.Join(store.EffortLadder, "|"))
		}
		out.ForceEffort = tier
	}
	seen := make(map[string]bool, len(table.Items))
	for _, it := range table.Items {
		from := strings.TrimSpace(it.From)
		if seen[from] {
			return router.EffortTable{}, false, fmt.Errorf("%w：深度表里 from %q 重复（同 from 只能有一条）", store.ErrConfig, from)
		}
		seen[from] = true
		to, ok := normalizeTier(it.To)
		if !ok {
			return router.EffortTable{}, false, fmt.Errorf("%w：深度映射目标 %q 不是上游白名单档（%s）",
				store.ErrConfig, it.To, strings.Join(store.EffortLadder, "|"))
		}
		out.Items = append(out.Items, router.EffortEntry{From: from, To: to})
	}
	return out, len(out.Items) > 0 || out.ForceEffort != "", nil
}

// normalizeTier 归一到上游白名单档（trim+小写）。
func normalizeTier(v string) (string, bool) {
	low := strings.ToLower(strings.TrimSpace(v))
	for _, l := range store.EffortLadder {
		if low == l {
			return low, true
		}
	}
	return low, false
}
