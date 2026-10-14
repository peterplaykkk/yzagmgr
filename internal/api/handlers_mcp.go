// §17 MCP 配置（UC-111）：独立写链（WorkBuddy 式入口）。
//
// 本文件只做「请求体翻译 + 结果包信封」，注册表与物化目标字节的全部构造在
// `internal/mcp`。preview/apply 复用 §4.3 的载荷形状（previewPayload /
// applyPayload）与共享租约、tx.Apply 守卫。
package api

import (
	"net/http"

	"github.com/peterplaykkk/yzagmgr/internal/apierr"
	"github.com/peterplaykkk/yzagmgr/internal/family"
	"github.com/peterplaykkk/yzagmgr/internal/mcp"
)

// mcpServersPayload 是 GET /mcp/servers 的响应。
type mcpServersPayload struct {
	Schema int               `json:"schema"`
	Items  []mcp.NamedServer `json:"items"`
}

// handleMcpServersList 列出注册表（字典序）。
func (s *Server) handleMcpServersList(r *http.Request) (any, error) {
	items, err := s.MCP.ListServers()
	if err != nil {
		return nil, err
	}
	if items == nil {
		items = []mcp.NamedServer{}
	}
	return mcpServersPayload{Schema: mcp.RegistrySchema, Items: items}, nil
}

// handleMcpServerSave 注册/覆盖一条条目（PUT /mcp/servers/{name}，幂等 upsert）。
func (s *Server) handleMcpServerSave(r *http.Request) (any, string, error) {
	name := r.PathValue("name")
	var body mcp.Server
	if err := decode(r, &body); err != nil {
		return nil, "", err
	}
	item, err := s.MCP.PutServer(name, body)
	if err != nil {
		return nil, "", err
	}
	return item, "", nil
}

// handleMcpServerDelete 只删注册表条目（live 清理走 op=remove 的 preview/apply）。
func (s *Server) handleMcpServerDelete(r *http.Request) (any, string, error) {
	name := r.PathValue("name")
	if err := s.MCP.DeleteServer(name); err != nil {
		return nil, "", err
	}
	return map[string]any{"name": name, "removed": true}, "", nil
}

// handleMcpToolsStatus 报告逐工具物化目标与在位状态（UC-111 A9）。
func (s *Server) handleMcpToolsStatus(r *http.Request) (any, error) {
	items, err := s.MCP.Status()
	if err != nil {
		return nil, err
	}
	return map[string]any{"items": items}, nil
}

// mcpPreviewBody 是 POST /mcp/preview 的请求体。
type mcpPreviewBody struct {
	Tool  string   `json:"tool"`
	Op    string   `json:"op"`
	Names []string `json:"names"`
}

// handleMcpPreview 产出零副作用的文件计划（租约含意图快照与我方条目指纹）。
func (s *Server) handleMcpPreview(r *http.Request) (any, string, error) {
	var body mcpPreviewBody
	if err := decode(r, &body); err != nil {
		return nil, "", err
	}
	lease, err := s.MCP.Preview(mcp.Op{Tool: body.Tool, Mode: body.Op, Names: body.Names})
	if err != nil {
		return nil, "", err
	}
	return previewPayload{
		PreviewID: lease.ID,
		ExpiresAt: lease.ExpiresAt.Format(rfc3339Second),
		Operation: lease.Operation,
		Files:     toFilePlanDTOs(lease.Files, nil),
		Warning:   lease.Warning,
	}, "", nil
}

// handleMcpApply 执行一份已确认的租约（我方条目冲突守卫 + 意图重算在 Store 内）。
func (s *Server) handleMcpApply(r *http.Request) (any, string, error) {
	var body applyBody
	if err := decode(r, &body); err != nil {
		return nil, "", err
	}
	if body.PreviewID == "" {
		return nil, "", apierr.New(apierr.CodeInvalidRequest, "previewId 必填")
	}
	result, tool, err := s.MCP.ApplyPreview(body.PreviewID)
	backupID := primaryBackupID(result.BackupIDs)
	if err != nil {
		return nil, backupID, err
	}
	// payload 的 activation 按租约所属工具的盘上档位回读——与 §4.3 同一套读法。
	payload, err := s.applyPayloadFrom(result, family.ID(tool))
	return payload, backupID, err
}
