package api

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/peterplaykkk/yzagmgr/internal/apierr"
)

// mcpPresence / mcpToolStatus / mcpToolsPayload 是 /mcp/tools 载荷的命名形态
// （匿名结构体在测试里复用两次，命名避免形态漂移）。
type mcpPresence struct {
	Name    string `json:"name"`
	Present bool   `json:"present"`
}

type mcpToolStatus struct {
	Tool    string        `json:"tool"`
	Path    string        `json:"path"`
	Exists  bool          `json:"exists"`
	Servers []mcpPresence `json:"servers"`
}

type mcpToolsPayload struct {
	Items []mcpToolStatus `json:"items"`
}

func findTool(t *testing.T, items []mcpToolStatus, want string) mcpToolStatus {
	t.Helper()
	for _, it := range items {
		if it.Tool == want {
			return it
		}
	}
	t.Fatalf("状态里没有工具 %s", want)
	panic("unreachable")
}

// registryBytes 读注册表当前字节（缺席记 nil）。
func registryBytes(t *testing.T, srv *contractServer) []byte {
	t.Helper()
	raw, err := os.ReadFile(srv.env.MCPServersFile())
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		t.Fatalf("读注册表失败: %v", err)
	}
	return raw
}

// TestMcpRegistryRejectsInvalidServer（UC-111 A2）：未知字段/非法形状一律
// invalid_request，且注册表零变化（先种一条合法条目做基线）。
func TestMcpRegistryRejectsInvalidServer(t *testing.T) {
	srv := newContractServer(t)

	ok := srv.callRaw(t, "PUT", "/api/v1/mcp/servers/seed-ok",
		`{"type":"http","url":"https://seed.invalid/mcp"}`, true)
	ok.assertEnvelope(t, "PUT", "/api/v1/mcp/servers/seed-ok")
	if !ok.ok {
		t.Fatalf("种入合法条目失败: %s", ok.body)
	}
	baseline := registryBytes(t, srv)

	cases := []struct {
		what string
		path string
		body string
	}{
		{"未知字段", "/api/v1/mcp/servers/x1", `{"type":"http","url":"https://a.invalid/m","oops":1}`},
		{"非法 type", "/api/v1/mcp/servers/x2", `{"type":"ftp","url":"https://a.invalid/m"}`},
		{"缺 url", "/api/v1/mcp/servers/x3", `{"type":"http"}`},
		{"url 非 http(s)", "/api/v1/mcp/servers/x4", `{"type":"http","url":"ftp://a.invalid/m"}`},
		{"stdio 缺 command", "/api/v1/mcp/servers/x5", `{"type":"stdio"}`},
		{"tools 含未支持工具", "/api/v1/mcp/servers/x6", `{"type":"http","url":"https://a.invalid/m","tools":["codex"]}`},
		{"非法 name（大写）", "/api/v1/mcp/servers/BadName", `{"type":"http","url":"https://a.invalid/m"}`},
		{"非法 name（符号起头）", "/api/v1/mcp/servers/-lead", `{"type":"http","url":"https://a.invalid/m"}`},
	}
	for _, tc := range cases {
		res := srv.callRaw(t, "PUT", tc.path, tc.body, true)
		res.assertEnvelope(t, "PUT", tc.path)
		if res.ok || res.code != apierr.CodeInvalidRequest {
			t.Fatalf("%s 应得 invalid_request，实际 ok=%v code=%s（%s）", tc.what, res.ok, res.code, res.body)
		}
	}

	if got := registryBytes(t, srv); string(got) != string(baseline) {
		t.Fatalf("失败的 PUT 不得改动注册表:\n got=%s\nwant=%s", got, baseline)
	}
	list := srv.callRaw(t, "GET", "/api/v1/mcp/servers", "", true)
	list.assertEnvelope(t, "GET", "/api/v1/mcp/servers")
	var payload struct {
		Schema int `json:"schema"`
		Items  []struct {
			Name string `json:"name"`
		} `json:"items"`
	}
	if err := json.Unmarshal(list.data, &payload); err != nil {
		t.Fatalf("列表载荷非法: %v（%s）", err, list.body)
	}
	if payload.Schema != 1 || len(payload.Items) != 1 || payload.Items[0].Name != "seed-ok" {
		t.Fatalf("失败 PUT 后列表应恰含 seed-ok: %+v", payload)
	}
}

// TestMcpAPIEndToEnd（UC-111 A10）：CRUD → preview → apply → 回读 → remove
// 经 REST 端到端；错误码族与信封同既有契约。
func TestMcpAPIEndToEnd(t *testing.T) {
	srv := newContractServer(t)

	// PUT 注册。
	save := srv.callRaw(t, "PUT", "/api/v1/mcp/servers/figma-desktop",
		`{"type":"http","url":"https://mcp.figma.com/mcp"}`, true)
	save.assertEnvelope(t, "PUT", "/api/v1/mcp/servers/figma-desktop")
	if !save.ok {
		t.Fatalf("PUT 失败: %s", save.body)
	}
	var saved struct {
		Name string `json:"name"`
		Type string `json:"type"`
		URL  string `json:"url"`
	}
	if err := json.Unmarshal(save.data, &saved); err != nil || saved.Name != "figma-desktop" ||
		saved.Type != "http" || saved.URL != "https://mcp.figma.com/mcp" {
		t.Fatalf("PUT 回读不符: %+v（%s）", saved, save.body)
	}

	// GET 状态：claude 目标在场、条目 missing。
	tools := srv.callRaw(t, "GET", "/api/v1/mcp/tools", "", true)
	tools.assertEnvelope(t, "GET", "/api/v1/mcp/tools")
	var status mcpToolsPayload
	if err := json.Unmarshal(tools.data, &status); err != nil {
		t.Fatalf("tools 载荷非法: %v（%s）", err, tools.body)
	}
	claudeTool, desktopTool := findTool(t, status.Items, "claude"), findTool(t, status.Items, "claude-desktop")
	if !strings.HasSuffix(claudeTool.Path, filepath.Join(".claude.json")) {
		t.Fatalf("claude 目标路径不符: %s", claudeTool.Path)
	}
	if !claudeTool.Exists {
		// 契约夹具的隔离 HOME 里 .claude.json 可能被其它用例建过；存在与否都合法，
		// 但条目状态必须是 missing（尚未物化）。
	}
	for _, p := range claudeTool.Servers {
		if p.Name == "figma-desktop" && p.Present {
			t.Fatalf("物化前应 missing")
		}
	}
	if desktopTool.Path == claudeTool.Path || desktopTool.Path == "" {
		t.Fatalf("claude-desktop 应有独立路径: %q", desktopTool.Path)
	}

	// preview → apply。
	prev := srv.callRaw(t, "POST", "/api/v1/mcp/preview", `{"tool":"claude","op":"apply"}`, true)
	prev.assertEnvelope(t, "POST", "/api/v1/mcp/preview")
	if !prev.ok {
		t.Fatalf("preview 失败: %s", prev.body)
	}
	var plan struct {
		PreviewID string `json:"previewId"`
		Operation string `json:"operation"`
		Files     []struct {
			Path string `json:"path"`
			Diff []any  `json:"diff"`
		} `json:"files"`
	}
	if err := json.Unmarshal(prev.data, &plan); err != nil || plan.PreviewID == "" {
		t.Fatalf("preview 载荷非法: %v（%s）", err, prev.body)
	}
	if len(plan.Files) != 1 || plan.Files[0].Path != claudeTool.Path {
		t.Fatalf("preview 应恰含 claude 目标一个文件: %+v", plan.Files)
	}
	// preview 零副作用（UC-111 A3 在 API 面的复核）。
	if _, err := os.Stat(claudeTool.Path); err == nil {
		t.Fatalf("preview 不得创建目标文件（隔离夹具起点应缺席）: %s", claudeTool.Path)
	}

	applied := srv.callRaw(t, "POST", "/api/v1/mcp/apply", `{"previewId":"`+plan.PreviewID+`"}`, true)
	applied.assertEnvelope(t, "POST", "/api/v1/mcp/apply")
	if !applied.ok {
		t.Fatalf("apply 失败: %s", applied.body)
	}
	var applyRes struct {
		Status    string `json:"status"`
		BackupIDs []any  `json:"backupIds"`
		Files     []struct {
			Path   string `json:"path"`
			Status string `json:"status"`
		} `json:"files"`
	}
	if err := json.Unmarshal(applied.data, &applyRes); err != nil {
		t.Fatalf("apply 载荷非法: %v（%s）", err, applied.body)
	}
	if applyRes.Status != "applied" || len(applyRes.BackupIDs) < 1 {
		t.Fatalf("apply 应 applied 且带备份: %+v", applyRes)
	}
	if len(applyRes.Files) != 1 || applyRes.Files[0].Status != "written" {
		t.Fatalf("逐文件状态不符: %+v", applyRes.Files)
	}
	raw, err := os.ReadFile(claudeTool.Path)
	if err != nil {
		t.Fatalf("物化后读目标失败: %v", err)
	}
	var live struct {
		MCP map[string]map[string]any `json:"mcpServers"`
	}
	if err := json.Unmarshal(raw, &live); err != nil || live.MCP["figma-desktop"]["url"] != "https://mcp.figma.com/mcp" {
		t.Fatalf("物化字节不符: %v（%s）", err, raw)
	}

	// 回读状态 present。
	tools2 := srv.callRaw(t, "GET", "/api/v1/mcp/tools", "", true)
	if err := json.Unmarshal(tools2.data, &status); err != nil {
		t.Fatalf("tools 回读载荷非法: %v", err)
	}
	claudeTool = findTool(t, status.Items, "claude")
	for _, p := range claudeTool.Servers {
		if p.Name == "figma-desktop" && !p.Present {
			t.Fatalf("apply 后应 present")
		}
	}

	// DELETE 只动注册表 → remove 端点仍可按 names 清理 live。
	del := srv.callRaw(t, "DELETE", "/api/v1/mcp/servers/figma-desktop", "", true)
	del.assertEnvelope(t, "DELETE", "/api/v1/mcp/servers/figma-desktop")
	if !del.ok {
		t.Fatalf("DELETE 失败: %s", del.body)
	}
	gone := srv.callRaw(t, "DELETE", "/api/v1/mcp/servers/figma-desktop", "", true)
	if gone.ok || gone.code != apierr.CodeNotFound {
		t.Fatalf("重复 DELETE 应 not_found: ok=%v code=%s", gone.ok, gone.code)
	}
	list := srv.callRaw(t, "GET", "/api/v1/mcp/servers", "", true)
	var afterList struct {
		Items []json.RawMessage `json:"items"`
	}
	if err := json.Unmarshal(list.data, &afterList); err != nil || len(afterList.Items) != 0 {
		t.Fatalf("DELETE 后列表应空: %v（%s）", err, list.body)
	}

	prevRm := srv.callRaw(t, "POST", "/api/v1/mcp/preview",
		`{"tool":"claude","op":"remove","names":["figma-desktop"]}`, true)
	prevRm.assertEnvelope(t, "POST", "/api/v1/mcp/preview")
	if !prevRm.ok {
		t.Fatalf("remove preview 失败: %s", prevRm.body)
	}
	var planRm struct {
		PreviewID string `json:"previewId"`
	}
	if err := json.Unmarshal(prevRm.data, &planRm); err != nil {
		t.Fatalf("remove preview 载荷非法: %v", err)
	}
	applyRm := srv.callRaw(t, "POST", "/api/v1/mcp/apply", `{"previewId":"`+planRm.PreviewID+`"}`, true)
	if !applyRm.ok {
		t.Fatalf("remove apply 失败: %s", applyRm.body)
	}
	raw, err = os.ReadFile(claudeTool.Path)
	if err != nil {
		t.Fatalf("remove 后读目标失败: %v", err)
	}
	var liveAfterRemove struct {
		MCP map[string]map[string]any `json:"mcpServers"`
	}
	if err := json.Unmarshal(raw, &liveAfterRemove); err != nil {
		t.Fatalf("remove 后文件非法: %v（%s）", err, raw)
	}
	if _, ok := liveAfterRemove.MCP["figma-desktop"]; ok {
		t.Fatalf("remove 后条目应消失: %s", raw)
	}

	// M1（审查缺口补齐）：第二目标 claude-desktop 的 API 面全链——preview/apply/
	// 回读，顺带覆盖 handleMcpApply 的 family.ID 映射与 toolState(claude-desktop)。
	// 注意时序：上面已 DELETE 注册表，先补回条目（注册表空 ⇒ 预览是无写入的 noop）。
	rePut := srv.callRaw(t, "PUT", "/api/v1/mcp/servers/figma-desktop",
		`{"type":"http","url":"https://mcp.figma.com/mcp"}`, true)
	if !rePut.ok {
		t.Fatalf("补回条目失败: %s", rePut.body)
	}
	deskPrev := srv.callRaw(t, "POST", "/api/v1/mcp/preview", `{"tool":"claude-desktop","op":"apply"}`, true)
	deskPrev.assertEnvelope(t, "POST", "/api/v1/mcp/preview")
	if !deskPrev.ok {
		t.Fatalf("claude-desktop preview 失败: %s", deskPrev.body)
	}
	var deskPlan struct {
		PreviewID string `json:"previewId"`
		Operation string `json:"operation"`
	}
	if err := json.Unmarshal(deskPrev.data, &deskPlan); err != nil {
		t.Fatalf("claude-desktop preview 载荷非法: %v", err)
	}
	deskApply := srv.callRaw(t, "POST", "/api/v1/mcp/apply", `{"previewId":"`+deskPlan.PreviewID+`"}`, true)
	if !deskApply.ok {
		t.Fatalf("claude-desktop apply 失败: %s", deskApply.body)
	}
	rawDesk, err := os.ReadFile(desktopTool.Path)
	if err != nil {
		t.Fatalf("读 claude-desktop 目标失败: %v", err)
	}
	var deskLive struct {
		MCP map[string]map[string]any `json:"mcpServers"`
	}
	if err := json.Unmarshal(rawDesk, &deskLive); err != nil || deskLive.MCP["figma-desktop"]["url"] != "https://mcp.figma.com/mcp" {
		t.Fatalf("claude-desktop 物化不符: %v（%s）", err, rawDesk)
	}

	// 错误码族：未知 previewId ⇒ invalid_request。
	bad := srv.callRaw(t, "POST", "/api/v1/mcp/apply", `{"previewId":"pv_nope"}`, true)
	bad.assertEnvelope(t, "POST", "/api/v1/mcp/apply")
	if bad.ok || bad.code != apierr.CodeInvalidRequest {
		t.Fatalf("未知 previewId 应 invalid_request: ok=%v code=%s", bad.ok, bad.code)
	}
}
