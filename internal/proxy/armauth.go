package proxy

import (
	"context"
	"fmt"
	"net/http"
	"strings"

	"github.com/peterplaykkk/yzagmgr/internal/apierr"
)

// 本文件是臂请求的**上游凭据注入层**（F3 / design.md §1.3 / design-b §1.3）。
//
// 背景：agserv 生产形态给会话进程注入的是占位符 `ANTHROPIC_AUTH_TOKEN=PROXY_MANAGED`
// （注释逐字「The arm owns upstream authentication…this public marker is not a grant」），
// 透传到底 = 占位符抵达官方端点。因此臂车道在保留「真 key 一字不改透传」的同时，
// 对占位符/缺头做解析注入：
//
//	1. 任一凭据头有**真 key**（≠ PROXY_MANAGED）⇒ 透传：真 key 一字不改，
//	   含占位符的残留头值只剔除、绝不覆盖真 key；
//	2. 占位符或完全缺失 ⇒ 按 (provider, wire) 从注入的 ProviderSource 解析
//	   （providers.json credRef → vault → env 兜底，链在 internal/upstream）：
//	   - 解析成功 ⇒ anthropic 线 `x-api-key` + `Authorization: Bearer` 双写，
//	     chat/responses 线 `Authorization: Bearer` 并清除 `x-api-key` 占位；
//	     客户端原有的 anthropic-version 等头原样透传；
//	   - 解析失败（无文件、无 env、credRef 撤销/未接线）⇒ **不发往上游**，
//	     401 `unauthorized`，message 注明 provider；记账照记一条
//	     （status 401、error_code=unauthorized、token=0）；
//	3. **任何分支不把 PROXY_MANAGED 发出网**（出网前终检 + 专项 UT 钉住）。
//
// 数据面仍无管理令牌环节（C1 C4）；客户端凭据与正文不入日志。

// ProviderSource 是臂上游凭据/配置的注入面（IP2/IP3 同族接口；实现 =
// internal/upstream 的 providers.json/env 解析链，由集成阶段接线）。
//
// 三个方法各自独立生效（实现方并发安全；每请求现读，热轮换语义见 design-b §1.2）：
type ProviderSource interface {
	// UpstreamKey 解析该 provider 的出网真 key。返回 ("", nil) 表示**未配置**
	//（无 providers.json credRef、无 env）；返回错误表示解析失败——两者都走 401。
	// wire 入参与注入形态相关（实现可只按 provider 取键），保留它以便按线差异化。
	// 纪律：key 明文只经返回值传递，不入日志/错误文本。
	UpstreamKey(ctx context.Context, provider string, wire string) (string, error)
	// UpstreamBase 给出 providers.json 的**基址覆写**（键 = armWireGroup 字符串：
	// "anthropic"/"chat"/"responses"）。无覆写返回 ("", false) ⇒ 走内置注册表。
	UpstreamBase(provider string, wireGroup string) (string, bool)
	// ProviderEnabled 报告 provider 是否启用（providers.json `enabled:false` ⇒ false
	// ⇒ 臂解析阶段 404「已停用」）。未接线/无该条目 ⇒ true。
	ProviderEnabled(provider string) bool
}

// providerSource 取注入面（锁内读；nil = 未接线）。
func (s *Server) providerSource() ProviderSource {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.providers
}

// providerBase 是 armBase 的覆写读面（未接线/无覆写 ⇒ false）。
func (s *Server) providerBase(provider, wireGroup string) (string, bool) {
	src := s.providerSource()
	if src == nil {
		return "", false
	}
	return src.UpstreamBase(provider, wireGroup)
}

// providerEnabled 是臂解析阶段的停用位读面（未接线 ⇒ 启用）。
func (s *Server) providerEnabled(provider string) bool {
	src := s.providerSource()
	if src == nil {
		return true
	}
	return src.ProviderEnabled(provider)
}

// resolveUpstreamKey 走 §1.2 解析链取真 key；取不到一律 401（apierr 文案注明
// provider，不含任何密钥材料）。
func (s *Server) resolveUpstreamKey(ctx context.Context, arm ArmRoute, wire Wire) (string, error) {
	src := s.providerSource()
	if src == nil {
		return "", errUpstreamCredential(arm.Provider, nil)
	}
	key, err := src.UpstreamKey(ctx, string(arm.Provider), string(wire))
	if err != nil {
		return "", errUpstreamCredential(arm.Provider, err)
	}
	key = strings.TrimSpace(key)
	if key == "" {
		return "", errUpstreamCredential(arm.Provider, nil)
	}
	// 配置链自身给出占位符 = 配置坏了：照样拦下（占位符绝不发出网）。
	if strings.Contains(key, Placeholder) {
		return "", errUpstreamCredential(arm.Provider,
			fmt.Errorf("解析结果仍是占位符 %s（配置无效）", Placeholder))
	}
	return key, nil
}

// errUpstreamCredential 构造 401 拒绝（码 = unauthorized，design-b §1.3；
// detail 只接受解析链的错误文本——链上错误不携带密钥明文，这是 internal/upstream 的纪律）。
func errUpstreamCredential(p ArmProvider, detail error) error {
	msg := fmt.Sprintf("上游凭据未配置（provider=%s）：本次请求未发往上游", p)
	if detail != nil {
		msg += "：" + detail.Error()
	}
	return apierr.New(apierr.CodeUnauthorized, "%s", msg)
}

// armAuth 返回臂请求的凭据处理函数（挂在 relayRequest.auth 上；relay 在**出网前**
// 调用它——返回错误即 401 未出网、finish 已记账）。
func (s *Server) armAuth(arm ArmRoute) func(*http.Request, Wire) error {
	return func(up *http.Request, wire Wire) error {
		// ① 扫描凭据头：区分「真 key」与「占位符残留」。
		real, headerPlaceholder := false, false
		for _, name := range authHeaderNames {
			for _, v := range up.Header.Values(name) {
				if v == "" {
					continue
				}
				if strings.Contains(v, Placeholder) {
					headerPlaceholder = true
					continue
				}
				real = true
			}
		}
		queryPlaceholder := strings.Contains(up.URL.RawQuery, Placeholder)

		// ② 占位符残留一律先剔除（真 key 所在的头值逐字保留）。
		if headerPlaceholder {
			for _, name := range authHeaderNames {
				vals := up.Header.Values(name)
				if len(vals) == 0 {
					continue
				}
				kept := make([]string, 0, len(vals))
				for _, v := range vals {
					if v != "" && !strings.Contains(v, Placeholder) {
						kept = append(kept, v)
					}
				}
				if len(kept) == 0 {
					up.Header.Del(name)
				} else {
					up.Header[name] = kept
				}
			}
		}

		// ③ 真 key 且无查询串占位符 ⇒ 透传完成（不需要解析源）。
		if real && !queryPlaceholder {
			return nil
		}

		// ④ 占位符或缺失 ⇒ 解析 provider key（失败 = 401 未出网）。
		key, err := s.resolveUpstreamKey(up.Context(), arm, wire)
		if err != nil {
			return err
		}
		if !real {
			if wire == WireAnthropic {
				// anthropic 线双写：官方 api.anthropic.com 与 bigmodel /api/anthropic
				// 两形态各取所需，上游任取其一（design-b §1.3）。
				up.Header.Set("x-api-key", key)
				up.Header.Set("Authorization", "Bearer "+key)
			} else {
				up.Header.Set("Authorization", "Bearer "+key)
				up.Header.Del("x-api-key")
			}
		}
		if queryPlaceholder {
			up.URL.RawQuery = replaceHeaderToken(up.URL.RawQuery, Placeholder, key)
		}

		// ⑤ 出网前终检：任何凭据头/查询串仍含占位符 ⇒ 401（防御位；正常路径不可达）。
		for _, name := range authHeaderNames {
			for _, v := range up.Header.Values(name) {
				if strings.Contains(v, Placeholder) {
					return errUpstreamCredential(arm.Provider,
						fmt.Errorf("凭据头 %s 仍含占位符（注入层内部不一致）", name))
				}
			}
		}
		if strings.Contains(up.URL.RawQuery, Placeholder) {
			return errUpstreamCredential(arm.Provider,
				fmt.Errorf("查询串仍含占位符（注入层内部不一致）"))
		}
		return nil
	}
}
