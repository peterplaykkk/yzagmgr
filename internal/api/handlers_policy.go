// §3 策略读取与刷新（GET /policy、POST /policy/refresh）。
//
// 原 handlers 随 handlers_session.go（登录/账户面）删除，但这两条端点是 design §1.1
// 的 2 REWORK 保留项：策略源已切换为本地策略文件（design §2.3），零出网。
package api

import (
	"encoding/json"
	"net/http"

	"github.com/peterplaykkk/yzagmgr/internal/policy"
)

func (s *Server) handlePolicy(*http.Request) (any, error) {
	p, err := s.Policy.Load()
	if err != nil {
		return nil, err
	}
	return policyPayload(p), nil
}

func (s *Server) handlePolicyRefresh(r *http.Request) (any, string, error) {
	p, err := s.Policy.Refresh(r.Context())
	if err != nil {
		return nil, "", err
	}
	// 策略同样决定物化内容（受管卡的默认模型、codex 的托管目录），因此一并刷新（UC-019）。
	// （原 syncProxy——策略目录/模型映射喂代理——已随 C 路删除，design §2.2/E2。）
	s.refreshMaterialized(r.Context())
	return policyPayload(p), "", nil
}

// policyPayload 是 §3 的策略响应：`{version, defaultModel, models:[{id,name,supportsImages}], catalogs}`。
//
// 直接透传服务包的类型：`catalogs` 的值是**原始字节**（`json.RawMessage`），
// 在这里再解码一遍就会破坏「目录载荷逐字节等同」（UC-007 A6）。
// 策略尚未就绪时给空形状而不是 null——契约的字段恒出现。
func policyPayload(p *policy.Policy) any {
	if p == nil {
		return emptyPolicy()
	}
	return p
}

// emptyPolicy 给出「尚未就绪」的空形状（字段恒出现，绝不回 null）。
func emptyPolicy() *policy.Policy {
	return &policy.Policy{Models: []policy.Model{}, Catalogs: map[string]json.RawMessage{}}
}
