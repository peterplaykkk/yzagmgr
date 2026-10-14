package mcp

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/peterplaykkk/yzagmgr/internal/apierr"
	"github.com/peterplaykkk/yzagmgr/internal/backup"
	"github.com/peterplaykkk/yzagmgr/internal/config"
	"github.com/peterplaykkk/yzagmgr/internal/family"
	"github.com/peterplaykkk/yzagmgr/internal/tx"
)

// mcpFixture 是隔离 HOME 下的 Store 夹具（不触用户真实配置）。
type mcpFixture struct {
	store  *Store
	env    *config.Env
	target string // 物化目标 = claude（~/.claude.json）
}

func newFixture(t *testing.T) *mcpFixture {
	t.Helper()
	env := &config.Env{Home: t.TempDir(), Overrides: map[string]string{}}
	if err := env.EnsureLayout(); err != nil {
		t.Fatalf("建立数据根失败: %v", err)
	}
	backups := backup.NewRegistry(env, func(family.ID) ([]string, error) { return nil, nil }, time.Now)
	return &mcpFixture{
		store:  NewStore(env, backups, Options{}),
		env:    env,
		target: env.ClaudeGlobalJSON(),
	}
}

func (f *mcpFixture) mustPut(t *testing.T, name string, sv Server) {
	t.Helper()
	if _, err := f.store.PutServer(name, sv); err != nil {
		t.Fatalf("注册 %s 失败: %v", name, err)
	}
}

func (f *mcpFixture) writeTarget(t *testing.T, content []byte) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(f.target), 0o700); err != nil {
		t.Fatalf("建目标目录失败: %v", err)
	}
	if err := os.WriteFile(f.target, content, 0o644); err != nil {
		t.Fatalf("写目标文件失败: %v", err)
	}
}

func (f *mcpFixture) readTarget(t *testing.T) []byte {
	t.Helper()
	raw, err := os.ReadFile(f.target)
	if err != nil {
		t.Fatalf("读目标文件失败: %v", err)
	}
	return raw
}

// mustApply 走完整 preview→apply 链并返回事务状态。
func (f *mcpFixture) mustApply(t *testing.T, op Op) string {
	t.Helper()
	lease, err := f.store.Preview(op)
	if err != nil {
		t.Fatalf("preview 失败: %v", err)
	}
	res, _, err := f.store.ApplyPreview(lease.ID)
	if err != nil {
		t.Fatalf("apply 失败: %v", err)
	}
	return res.Status
}

func wantCode(t *testing.T, err error, code apierr.Code) {
	t.Helper()
	if err == nil {
		t.Fatalf("期望错误 %s，实际无错误", code)
	}
	if got := apierr.From(err).Code; got != code {
		t.Fatalf("期望错误码 %s，实际 %s（%v）", code, got, err)
	}
}

func fileMeta(t *testing.T, path string) (sha string, mtime int64) {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return "", -1
		}
		t.Fatalf("读文件失败: %v", err)
	}
	st, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat 失败: %v", err)
	}
	return tx.HashBytes(raw), st.ModTime().UnixNano()
}

func containsBytes(haystack, needle []byte) bool { return bytes.Contains(haystack, needle) }

func httpServer(url string) Server {
	return Server{Type: "http", URL: url}
}

// TestMcpRegistryUpsertIdempotent（UC-111 A1）：同名重复 PUT 覆盖更新无重复键；
// 注册表 0600、schema 1；schema 被外改损坏时读侧显式 config_invalid 不静默空表。
func TestMcpRegistryUpsertIdempotent(t *testing.T) {
	f := newFixture(t)
	f.mustPut(t, "figma-desktop", httpServer("https://mcp.figma.com/mcp"))
	f.mustPut(t, "figma-desktop", httpServer("https://mcp.figma.com/mcp/v2"))

	items, err := f.store.ListServers()
	if err != nil {
		t.Fatalf("ListServers 失败: %v", err)
	}
	if len(items) != 1 {
		t.Fatalf("重复 PUT 后应恰 1 条，实际 %d", len(items))
	}
	if items[0].Name != "figma-desktop" || items[0].URL != "https://mcp.figma.com/mcp/v2" {
		t.Fatalf("upsert 未覆盖: %+v", items[0])
	}

	st, err := os.Stat(f.env.MCPServersFile())
	if err != nil {
		t.Fatalf("注册表文件缺失: %v", err)
	}
	if perm := st.Mode().Perm(); perm != 0o600 {
		t.Fatalf("注册表权限应 0600，实际 %o", perm)
	}
	raw, err := os.ReadFile(f.env.MCPServersFile())
	if err != nil {
		t.Fatalf("读注册表失败: %v", err)
	}
	var doc struct {
		Schema  int            `json:"schema"`
		Servers map[string]any `json:"servers"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("注册表不是合法 JSON: %v", err)
	}
	if doc.Schema != RegistrySchema {
		t.Fatalf("schema 应 %d，实际 %d", RegistrySchema, doc.Schema)
	}

	// 外改 schema ⇒ 读侧显式报错（UC-111 边界），随后恢复原字节继续。
	if err := os.WriteFile(f.env.MCPServersFile(), []byte(`{"schema":99,"servers":{}}`), 0o600); err != nil {
		t.Fatalf("注入坏 schema 失败: %v", err)
	}
	if _, err := f.store.ListServers(); err == nil || apierr.From(err).Code != apierr.CodeConfigInvalid {
		t.Fatalf("坏 schema 应报 config_invalid，实际 %v", err)
	}
}

// TestMcpPreviewNoSideEffect（UC-111 A3）：preview 前后目标文件与注册表的
// sha256 与 mtime 双不变。
func TestMcpPreviewNoSideEffect(t *testing.T) {
	f := newFixture(t)
	start := []byte("{\n  \"keep\": true\n}\n")
	f.writeTarget(t, start)
	f.mustPut(t, "figma-desktop", httpServer("https://mcp.figma.com/mcp"))

	tgtSHA, tgtMT := fileMeta(t, f.target)
	regSHA, regMT := fileMeta(t, f.env.MCPServersFile())

	lease, err := f.store.Preview(Op{Tool: "claude", Mode: "apply"})
	if err != nil {
		t.Fatalf("preview 失败: %v", err)
	}
	if lease.ID == "" {
		t.Fatalf("preview 未返回租约 ID")
	}

	gotSHA, gotMT := fileMeta(t, f.target)
	if gotSHA != tgtSHA || gotMT != tgtMT {
		t.Fatalf("preview 后目标文件被改动: sha %s→%s mtime %d→%d", tgtSHA, gotSHA, tgtMT, gotMT)
	}
	regSHA2, regMT2 := fileMeta(t, f.env.MCPServersFile())
	if regSHA2 != regSHA || regMT2 != regMT {
		t.Fatalf("preview 后注册表被改动: sha %s→%s mtime %d→%d", regSHA, regSHA2, regMT, regMT2)
	}
}

// TestMcpApplyMaterializesPreservingForeignKeys（UC-111 A4）：apply 后操作集内
// 条目在位；非操作集的既有 mcpServers 条目与其它顶层键逐字节保留；缺席文件按需创建。
func TestMcpApplyMaterializesPreservingForeignKeys(t *testing.T) {
	f := newFixture(t)
	// 起始：他键 + 他人 mcpServers 条目（2 空格缩进的规范形态）。
	otherRaw := `{
  "type": "stdio",
  "command": "npx",
  "args": ["-y", "other"]
}`
	start := []byte(`{
  "numStartups": 7,
  "mcpServers": {
    "other-server": ` + otherRaw + `
  },
  "projects": {}
}
`)
	f.writeTarget(t, start)
	f.mustPut(t, "figma-desktop", httpServer("https://mcp.figma.com/mcp"))
	f.mustPut(t, "desktop-only", Server{Type: "sse", URL: "https://example.invalid/sse", Tools: []string{"claude-desktop"}})

	if status := f.mustApply(t, Op{Tool: "claude", Mode: "apply"}); status != "applied" {
		t.Fatalf("apply 状态应 applied，实际 %s", status)
	}
	after := f.readTarget(t)

	var doc struct {
		NumStartups int                        `json:"numStartups"`
		MCP         map[string]json.RawMessage `json:"mcpServers"`
		Projects    map[string]any             `json:"projects"`
	}
	if err := json.Unmarshal(after, &doc); err != nil {
		t.Fatalf("apply 后目标文件不是合法 JSON: %v（%s）", err, after)
	}
	if doc.NumStartups != 7 {
		t.Fatalf("他键 numStartups 被改动: %d", doc.NumStartups)
	}
	if doc.Projects == nil {
		t.Fatalf("他键 projects 消失")
	}
	if _, ok := doc.MCP["other-server"]; !ok {
		t.Fatalf("非操作集条目 other-server 被删除")
	}
	if _, ok := doc.MCP["figma-desktop"]; !ok {
		t.Fatalf("操作集条目 figma-desktop 未物化")
	}
	if _, ok := doc.MCP["desktop-only"]; ok {
		t.Fatalf("tools 范围外条目 desktop-only 不应物化到 claude")
	}
	// 他人条目的原始字节原样存活（逐字节保真，而非等值重编码）。
	if !containsBytes(after, []byte(otherRaw)) {
		t.Fatalf("他人条目字节未保留:\n%s", after)
	}
	var figma map[string]any
	if err := json.Unmarshal(doc.MCP["figma-desktop"], &figma); err != nil {
		t.Fatalf("figma 条目非法: %v", err)
	}
	if figma["type"] != "http" || figma["url"] != "https://mcp.figma.com/mcp" {
		t.Fatalf("figma 条目内容不符: %+v", figma)
	}
	if _, ok := figma["tools"]; ok {
		t.Fatalf("tools 是注册表侧范围，不应落进 live 文件")
	}

	// 缺席文件：apply 创建（权限 0600）。
	f2 := newFixture(t)
	f2.mustPut(t, "figma-desktop", httpServer("https://mcp.figma.com/mcp"))
	if status := f2.mustApply(t, Op{Tool: "claude", Mode: "apply"}); status != "applied" {
		t.Fatalf("冷启动 apply 状态应 applied，实际 %s", status)
	}
	raw := f2.readTarget(t)
	var created struct {
		MCP map[string]map[string]string `json:"mcpServers"`
	}
	if err := json.Unmarshal(raw, &created); err != nil || created.MCP["figma-desktop"]["url"] != "https://mcp.figma.com/mcp" {
		t.Fatalf("冷启动创建失败: %v（%s）", err, raw)
	}
	if st, _ := os.Stat(f2.target); st.Mode().Perm() != 0o600 {
		t.Fatalf("新建文件权限应 0600，实际 %o", st.Mode().Perm())
	}

	// B2 回归：单行容器（本写链自造形态）上重复 apply 必须幂等——同名原地替换，
	// 绝不追加重复键（审查实测缺陷：三次 apply 出现三个同名旧值）。
	first, err := os.ReadFile(f2.target)
	if err != nil {
		t.Fatalf("读物化结果失败: %v", err)
	}
	if status := f2.mustApply(t, Op{Tool: "claude", Mode: "apply"}); status != "unchanged" {
		t.Fatalf("同内容重 apply 应 unchanged，实际 %s", status)
	}
	second, err := os.ReadFile(f2.target)
	if err != nil {
		t.Fatalf("重 apply 后读文件失败: %v", err)
	}
	if string(first) != string(second) {
		t.Fatalf("重 apply 字节必须不变:\n got=%s\nwant=%s", second, first)
	}
	var dup struct {
		MCP map[string][]map[string]any `json:"mcpServers"`
	}
	if err := json.Unmarshal(second, &dup); err == nil && len(dup.MCP["figma-desktop"]) > 1 {
		t.Fatalf("出现重复键: %s", second)
	}

	// B1 回归：既有容器为空对象 `{}` ⇒ 可正常物化（审查缺陷：Members[-1] panic）。
	f3 := newFixture(t)
	f3.mustPut(t, "figma-desktop", httpServer("https://mcp.figma.com/mcp"))
	f3.writeTarget(t, []byte("{\n  \"mcpServers\": {}\n}\n"))
	if status := f3.mustApply(t, Op{Tool: "claude", Mode: "apply"}); status != "applied" {
		t.Fatalf("空对象容器 apply 应 applied，实际 %s", status)
	}
	var emptyObj struct {
		MCP map[string]map[string]any `json:"mcpServers"`
	}
	if err := json.Unmarshal(f3.readTarget(t), &emptyObj); err != nil || emptyObj.MCP["figma-desktop"]["url"] != "https://mcp.figma.com/mcp" {
		t.Fatalf("空对象容器物化失败: %v（%s）", err, f3.readTarget(t))
	}

	// B1 回归：mcpServers 为数组/标量/null ⇒ 显式 config_invalid（审查缺陷：panic）。
	for _, shape := range []string{
		`{"mcpServers": []}`,
		`{"mcpServers": null}`,
		`{"mcpServers": "not-an-object"}`,
	} {
		f4 := newFixture(t)
		f4.mustPut(t, "figma-desktop", httpServer("https://mcp.figma.com/mcp"))
		f4.writeTarget(t, []byte(shape))
		_, err := f4.store.Preview(Op{Tool: "claude", Mode: "apply"})
		wantCode(t, err, apierr.CodeConfigInvalid)
	}

	// M1 回归：第二目标 claude-desktop 的写路径全链（审查缺口：双目标之一零覆盖）。
	f5 := newFixture(t)
	f5.mustPut(t, "figma-desktop", httpServer("https://mcp.figma.com/mcp"))
	desktopPath := f5.env.ClaudeDesktopLiveConfig()
	if status := f5.mustApply(t, Op{Tool: "claude-desktop", Mode: "apply"}); status != "applied" {
		t.Fatalf("claude-desktop apply 应 applied，实际 %s", status)
	}
	rawDesk, err := os.ReadFile(desktopPath)
	if err != nil {
		t.Fatalf("读 claude-desktop 目标失败: %v", err)
	}
	var deskDoc struct {
		MCP map[string]map[string]any `json:"mcpServers"`
	}
	if err := json.Unmarshal(rawDesk, &deskDoc); err != nil || deskDoc.MCP["figma-desktop"]["url"] != "https://mcp.figma.com/mcp" {
		t.Fatalf("claude-desktop 物化失败: %v（%s）", err, rawDesk)
	}
}

// TestMcpApplyConflictSemantics（UC-111 A5）：我方条目外改 ⇒ config_changed
// 零写入；仅他键外改 ⇒ 吸收他改并成功。
func TestMcpApplyConflictSemantics(t *testing.T) {
	f := newFixture(t)
	f.mustPut(t, "figma-desktop", httpServer("https://mcp.figma.com/mcp"))

	// 先物化一次，使我方条目在场。
	f.writeTarget(t, []byte("{\n  \"keep\": 1\n}\n"))
	f.mustApply(t, Op{Tool: "claude", Mode: "apply"})

	// 分支一：仅他键外改 ⇒ 吸收。
	lease, err := f.store.Preview(Op{Tool: "claude", Mode: "apply"})
	if err != nil {
		t.Fatalf("preview 失败: %v", err)
	}
	f.writeTarget(t, []byte("{\n  \"keep\": 2,\n  \"mcpServers\": {\n    \"figma-desktop\": {\n      \"type\": \"http\",\n      \"url\": \"https://mcp.figma.com/mcp\"\n    }\n  }\n}\n"))
	if _, _, err := f.store.ApplyPreview(lease.ID); err != nil {
		t.Fatalf("仅他键外改应被吸收，实际报错: %v", err)
	}
	var absorbed struct {
		Keep int                          `json:"keep"`
		MCP  map[string]map[string]string `json:"mcpServers"`
	}
	if err := json.Unmarshal(f.readTarget(t), &absorbed); err != nil {
		t.Fatalf("吸收后文件非法: %v", err)
	}
	if absorbed.Keep != 2 {
		t.Fatalf("他键外改被丢弃: keep=%d", absorbed.Keep)
	}
	if absorbed.MCP["figma-desktop"]["url"] != "https://mcp.figma.com/mcp" {
		t.Fatalf("我方条目未按意图落位: %+v", absorbed.MCP)
	}

	// 分支二：我方条目外改 ⇒ config_changed 且零写入。
	lease2, err := f.store.Preview(Op{Tool: "claude", Mode: "apply"})
	if err != nil {
		t.Fatalf("preview2 失败: %v", err)
	}
	tampered := []byte("{\n  \"keep\": 2,\n  \"mcpServers\": {\n    \"figma-desktop\": {\n      \"type\": \"http\",\n      \"url\": \"https://evil.example/tampered\"\n    }\n  }\n}\n")
	f.writeTarget(t, tampered)
	_, _, err = f.store.ApplyPreview(lease2.ID)
	wantCode(t, err, apierr.CodeConfigChanged)
	if got := f.readTarget(t); string(got) != string(tampered) {
		t.Fatalf("config_changed 后文件必须零写入:\n%s", got)
	}
}

// TestMcpRemoveRoundtrip（UC-111 A6）：在场起始文件 apply→remove 字节回起点。
func TestMcpRemoveRoundtrip(t *testing.T) {
	f := newFixture(t)
	start := []byte("{\n  \"projects\": {},\n  \"numStartups\": 3\n}\n")
	f.writeTarget(t, start)
	f.mustPut(t, "figma-desktop", httpServer("https://mcp.figma.com/mcp"))
	f.mustPut(t, "sse-one", Server{Type: "sse", URL: "https://example.invalid/sse"})

	f.mustApply(t, Op{Tool: "claude", Mode: "apply"})
	if got := f.readTarget(t); string(got) == string(start) {
		t.Fatalf("apply 后字节不应等于起点（物化未生效）")
	}
	f.mustApply(t, Op{Tool: "claude", Mode: "remove"})
	if got := f.readTarget(t); string(got) != string(start) {
		t.Fatalf("remove 后未回到起始字节:\n got=%s\nwant=%s", got, start)
	}

	// A6 边界（UC 明文）：起始缺席的文件经 apply 创建后 remove，留下 {} 空壳。
	f6 := newFixture(t)
	f6.mustPut(t, "figma-desktop", httpServer("https://mcp.figma.com/mcp"))
	f6.mustApply(t, Op{Tool: "claude", Mode: "apply"})
	f6.mustApply(t, Op{Tool: "claude", Mode: "remove"})
	if got := f6.readTarget(t); string(got) != "{}\n" {
		t.Fatalf("缺席起始的 remove 应留 {}\\n 空壳，实际 %q", got)
	}
}

// TestMcpPreviewUnsupportedTool（UC-111 A7）：未支持工具 ⇒ invalid_request。
func TestMcpPreviewUnsupportedTool(t *testing.T) {
	f := newFixture(t)
	f.mustPut(t, "figma-desktop", httpServer("https://mcp.figma.com/mcp"))
	_, err := f.store.Preview(Op{Tool: "codex", Mode: "apply"})
	wantCode(t, err, apierr.CodeInvalidRequest)
	if _, err := os.Stat(f.target); !os.IsNotExist(err) {
		t.Fatalf("被拒的 preview 不得创建目标文件")
	}
}

// TestMcpApplyHonorsToolsScope（UC-111 A8）：条目 tools 不含目标工具时整体跳过。
func TestMcpApplyHonorsToolsScope(t *testing.T) {
	f := newFixture(t)
	f.mustPut(t, "both-tools", httpServer("https://a.invalid/mcp"))
	f.mustPut(t, "desktop-only", Server{Type: "stdio", Command: "npx", Args: []string{"-y", "x"}, Tools: []string{"claude-desktop"}})

	f.mustApply(t, Op{Tool: "claude", Mode: "apply"})
	var doc struct {
		MCP map[string]json.RawMessage `json:"mcpServers"`
	}
	if err := json.Unmarshal(f.readTarget(t), &doc); err != nil {
		t.Fatalf("目标文件非法: %v", err)
	}
	if _, ok := doc.MCP["both-tools"]; !ok {
		t.Fatalf("无范围限制条目应物化")
	}
	if _, ok := doc.MCP["desktop-only"]; ok {
		t.Fatalf("tools=[claude-desktop] 条目不应物化到 claude")
	}

	status, err := f.store.Status()
	if err != nil {
		t.Fatalf("Status 失败: %v", err)
	}
	for _, st := range status {
		if st.Tool != "claude" {
			continue
		}
		for _, p := range st.Servers {
			if p.Name == "desktop-only" && p.Present {
				t.Fatalf("claude 侧 desktop-only 应 missing")
			}
			if p.Name == "both-tools" && !p.Present {
				t.Fatalf("claude 侧 both-tools 应 present")
			}
		}
	}

	// A8 扩展（审查 m3）：用户在 claude 文件里手写了 tools=[claude-desktop] 的同名
	// 条目——缺省 remove（=撤销本工具的 apply）按范围过滤不碰它；显式 names 是
	// 强制清理出口，照删。
	handwritten := []byte(`{
  "mcpServers": {
    "desktop-only": {
      "type": "stdio",
      "command": "hand-written"
    }
  }
}
`)
	f.writeTarget(t, handwritten)
	f.mustApply(t, Op{Tool: "claude", Mode: "remove"})
	if got := f.readTarget(t); string(got) != string(handwritten) {
		t.Fatalf("缺省 remove 不得触碰范围外手写条目:\n got=%s", got)
	}
	f.mustApply(t, Op{Tool: "claude", Mode: "remove", Names: []string{"desktop-only"}})
	var after struct {
		MCP map[string]json.RawMessage `json:"mcpServers"`
	}
	if err := json.Unmarshal(f.readTarget(t), &after); err != nil {
		t.Fatalf("强制 remove 后文件非法: %v", err)
	}
	if _, ok := after.MCP["desktop-only"]; ok {
		t.Fatalf("显式 names 的 remove 应删除目标条目: %s", f.readTarget(t))
	}
}

// TestMcpToolsStatusReport（UC-111 A9）：逐工具目标路径与 present/missing 读
// live 真实状态；目标文件非法 JSON 显式 config_invalid。
func TestMcpToolsStatusReport(t *testing.T) {
	f := newFixture(t)
	f.mustPut(t, "figma-desktop", httpServer("https://mcp.figma.com/mcp"))
	f.mustPut(t, "sse-one", Server{Type: "sse", URL: "https://example.invalid/sse"})
	f.writeTarget(t, []byte(`{"mcpServers":{"figma-desktop":{"type":"http","url":"https://mcp.figma.com/mcp"}}}`))

	status, err := f.store.Status()
	if err != nil {
		t.Fatalf("Status 失败: %v", err)
	}
	if len(status) != len(SupportedTools) {
		t.Fatalf("应返回 %d 件工具，实际 %d", len(SupportedTools), len(status))
	}
	byTool := map[string]ToolStatus{}
	for _, st := range status {
		byTool[st.Tool] = st
	}
	claude := byTool["claude"]
	if claude.Path != f.target {
		t.Fatalf("claude 目标路径不符: %s != %s", claude.Path, f.target)
	}
	if !claude.Exists {
		t.Fatalf("claude 目标应存在")
	}
	presence := map[string]bool{}
	for _, p := range claude.Servers {
		presence[p.Name] = p.Present
	}
	if !presence["figma-desktop"] || presence["sse-one"] {
		t.Fatalf("present 矩阵不符: %v", presence)
	}
	desktop := byTool["claude-desktop"]
	if desktop.Path == "" || desktop.Path == claude.Path {
		t.Fatalf("claude-desktop 应有独立目标路径: %q", desktop.Path)
	}
	if desktop.Exists {
		t.Fatalf("claude-desktop 目标不应存在（未物化过）")
	}

	// 目标文件非法 JSON ⇒ config_invalid（显式，不静默）；且 preview 读面同口径
	// （n5：空注册表时 preview 不得静默 unchanged，两面一致）。
	f.writeTarget(t, []byte("{not json"))
	if _, err := f.store.Status(); err == nil || apierr.From(err).Code != apierr.CodeConfigInvalid {
		t.Fatalf("非法 JSON 应报 config_invalid，实际 %v", err)
	}
	fEmpty := newFixture(t) // 零注册表
	fEmpty.writeTarget(t, []byte("{not json"))
	_, err = fEmpty.store.Preview(Op{Tool: "claude", Mode: "apply"})
	wantCode(t, err, apierr.CodeConfigInvalid)
}
