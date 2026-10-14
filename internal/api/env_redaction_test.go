// §9 环境变量冲突（UC-015 修订版 A4/A13）：整行级脱敏在**真实 HTTP 响应正文**上的回归钉。
//
// 审计实证的泄漏形态（2026-09-27，HEAD 快照）：决定键与兄弟凭据同处一物理行时，
// 旧实现只掩「本条目自身的值」，同行凭据经**非敏感**条目的 lineText 原样进入
// /env/conflicts 与 /env/repair/preview 的响应正文（再直通 UI 展示与复制按钮）。
//
// 本用例不复用 DTO 级断言（那在 envcheck 包里已有），而是在真实信封、真实序列化
// 字节上判定：原始响应体不含任何夹具秘密明文，解码后的行文本/待删行文本是掩码形态，
// 且非秘密片段（URL、键名）保留可读。
package api

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/peterplaykkk/yzagmgr/internal/envcheck"
)

func TestEnvConflictResponseCarriesNoSiblingSecrets(t *testing.T) {
	const (
		sibling = "sk-live-AUDIT-REALKEY-0123456789abcdef" // 同行兄弟（非决定键赋值）
		mimo    = "tp-mimo-01J2ZQ8ZK7M3V6ABCDEFGHJKLM"     // 行内注释里的另一家前缀令牌
	)
	srv := newContractServer(t)

	// 契约夹具的扫描器不带决定键（真实装配在 cmd/yzagmgr 里注入）；这里换成带一个
	// 决定键的扫描器，其余注入（假 launchd、空进程环境、只扫隔离根的 .zshrc）
	// 与夹具纪律一致——绝不读真实 $HOME 或真实会话环境。
	scanner, err := envcheck.NewScanner(envcheck.Options{
		Policy:     envcheck.Policy{Home: srv.env.Home, WritableRoots: []string{srv.env.Home}},
		ShellFiles: []string{filepath.Join(srv.env.Home, ".zshrc")},
		Expected: []envcheck.DecisionKey{{
			Name: "ANTHROPIC_BASE_URL", Value: "http://127.0.0.1:17021/claude", ToolID: "claude",
		}},
		Launchd:    func([]string) map[string]string { return nil },
		ProcessEnv: func() map[string]string { return map[string]string{} },
	})
	if err != nil {
		t.Fatalf("构造环境扫描器失败: %v", err)
	}
	srv.EnvScanner = scanner

	rc := filepath.Join(srv.env.Home, ".zshrc")
	line := "export ANTHROPIC_BASE_URL=https://old.example OPENAI_API_KEY=" + sibling +
		" # 轮换前旧令牌 " + mimo
	if err := os.WriteFile(rc, []byte(line+"\n"), 0o644); err != nil {
		t.Fatalf("写夹具启动文件失败: %v", err)
	}
	wantLineText := "export ANTHROPIC_BASE_URL=https://old.example OPENAI_API_KEY=" +
		envcheck.MaskedValue + " # 轮换前旧令牌 " + envcheck.MaskedValue

	// ① /env/conflicts：兄弟凭据必须被整行级脱敏挡住，哪怕条目自身不敏感。
	conflicts := srv.callRaw(t, http.MethodGet, "/api/v1/env/conflicts", "", true)
	conflicts.assertEnvelope(t, http.MethodGet, "/api/v1/env/conflicts")
	if !conflicts.ok {
		t.Fatalf("读取冲突失败: %s", conflicts.body)
	}
	for _, s := range []string{sibling, mimo} {
		if strings.Contains(conflicts.body, s) {
			t.Fatalf("响应正文泄漏了令牌明文 %q: %s", s, conflicts.body)
		}
	}
	if !strings.Contains(conflicts.body, "https://old.example") {
		t.Fatalf("非秘密片段（URL）应保留可读: %s", conflicts.body)
	}
	var list struct {
		Items []struct {
			ID        string `json:"id"`
			Name      string `json:"name"`
			Sensitive bool   `json:"sensitive"`
			LineText  string `json:"lineText"`
		} `json:"items"`
	}
	if err := json.Unmarshal(conflicts.data, &list); err != nil {
		t.Fatalf("解析冲突列表失败: %v（%s）", err, conflicts.body)
	}
	if len(list.Items) != 1 {
		t.Fatalf("期望 1 条冲突: %s", conflicts.body)
	}
	it := list.Items[0]
	if it.Sensitive {
		t.Fatalf("条目自身不该敏感（泄漏正是经由非敏感条目发生）: %+v", it)
	}
	if it.LineText != wantLineText {
		t.Fatalf("lineText=%q，期望 %q", it.LineText, wantLineText)
	}

	// ② /env/repair/preview：deleteLines 与扫描共用同一脱敏函数（单源），同样干净。
	preview := srv.callRaw(t, http.MethodPost, "/api/v1/env/repair/preview",
		`{"conflictIds":["`+it.ID+`"]}`, true)
	preview.assertEnvelope(t, http.MethodPost, "/api/v1/env/repair/preview")
	if !preview.ok {
		t.Fatalf("预览失败: %s", preview.body)
	}
	for _, s := range []string{sibling, mimo} {
		if strings.Contains(preview.body, s) {
			t.Fatalf("预览响应正文泄漏了令牌明文 %q: %s", s, preview.body)
		}
	}
	var prev struct {
		Files []struct {
			DeleteLines []struct {
				Line int    `json:"line"`
				Text string `json:"text"`
			} `json:"deleteLines"`
		} `json:"files"`
	}
	if err := json.Unmarshal(preview.data, &prev); err != nil {
		t.Fatalf("解析预览失败: %v（%s）", err, preview.body)
	}
	if len(prev.Files) != 1 || len(prev.Files[0].DeleteLines) != 1 {
		t.Fatalf("预览形状不符: %s", preview.body)
	}
	if got := prev.Files[0].DeleteLines[0].Text; got != wantLineText {
		t.Fatalf("待删行文本=%q，期望 %q", got, wantLineText)
	}
	if prev.Files[0].DeleteLines[0].Line != 1 {
		t.Fatalf("待删行号=%d，期望 1", prev.Files[0].DeleteLines[0].Line)
	}
}

// 核验判红残留的 HTTP 级回归钉（2026-09-27 独立核验，§1 两个夹具）：
// 前缀令牌后紧跟 CJK（`已废弃`）或全角句号（`。`）时，旧实现在 /env/conflicts、
// /env/scan、/env/repair/preview 的**完整响应正文**里原样带出令牌明文
// （令牌记号 29 字节：≥minTokenTail 会被前缀识别，<minTokenRun 逃过长跑扫描）。
func TestEnvResponsesCarryNoGluedTokenResidue(t *testing.T) {
	// 核验夹具令牌（tp- 前缀 + 29 字节记号，与 §1 两行注释夹具逐字同形）。
	const tok = "tp-mimo-01J2ZQ8ZK7M3V6ADVVVVVVVV"
	srv := newContractServer(t)

	// 与既有回归钉同一纪律：扫描器只扫隔离根，决定键有两个（每行各一），
	// 绝不读真实 $HOME、真实 launchd 或真实会话环境。
	scanner, err := envcheck.NewScanner(envcheck.Options{
		Policy:     envcheck.Policy{Home: srv.env.Home, WritableRoots: []string{srv.env.Home}},
		ShellFiles: []string{filepath.Join(srv.env.Home, ".zshrc")},
		Expected: []envcheck.DecisionKey{
			{Name: "ANTHROPIC_BASE_URL", Value: "http://127.0.0.1:17021/claude", ToolID: "claude"},
			{Name: "ANTHROPIC_MODEL", Value: "from-widget", ToolID: "claude"},
		},
		Launchd:    func([]string) map[string]string { return nil },
		ProcessEnv: func() map[string]string { return map[string]string{} },
	})
	if err != nil {
		t.Fatalf("构造环境扫描器失败: %v", err)
	}
	srv.EnvScanner = scanner

	rc := filepath.Join(srv.env.Home, ".zshrc")
	lineA := "export ANTHROPIC_BASE_URL=https://old.example # 旧令牌" + tok + "已废弃"
	lineB := "export ANTHROPIC_MODEL=from-shell # 轮换 " + tok + "。"
	if err := os.WriteFile(rc, []byte(lineA+"\n"+lineB+"\n"), 0o644); err != nil {
		t.Fatalf("写夹具启动文件失败: %v", err)
	}
	want := map[string]string{
		"ANTHROPIC_BASE_URL": "export ANTHROPIC_BASE_URL=https://old.example # 旧令牌" + envcheck.MaskedValue,
		"ANTHROPIC_MODEL":    "export ANTHROPIC_MODEL=from-shell # 轮换 " + envcheck.MaskedValue,
	}

	// envConflictItems 是 §9 列表条目在响应里的最小形状。
	type envConflictItems struct {
		Items []struct {
			ID        string `json:"id"`
			Name      string `json:"name"`
			Sensitive bool   `json:"sensitive"`
			LineText  string `json:"lineText"`
		} `json:"items"`
	}
	assertList := func(label string, res envelope) map[string]string {
		t.Helper()
		if !res.ok {
			t.Fatalf("%s 请求失败: %s", label, res.body)
		}
		if strings.Contains(res.body, tok) {
			t.Fatalf("%s 响应正文泄漏了令牌明文: %s", label, res.body)
		}
		var list envConflictItems
		if err := json.Unmarshal(res.data, &list); err != nil {
			t.Fatalf("解析 %s 列表失败: %v（%s）", label, err, res.body)
		}
		if len(list.Items) != 2 {
			t.Fatalf("%s 期望 2 条冲突: %s", label, res.body)
		}
		ids := map[string]string{}
		for _, it := range list.Items {
			if it.Sensitive {
				t.Fatalf("%s 条目自身不该敏感（泄漏正是经由非敏感条目发生）: %+v", label, it)
			}
			if got := want[it.Name]; it.LineText != got {
				t.Fatalf("%s 的 lineText=%q，期望 %q", it.Name, it.LineText, got)
			}
			ids[it.Name] = it.ID
		}
		// 非秘密片段（注释正文、URL、非秘密值）必须保留可读。
		for _, keep := range []string{"旧令牌", "轮换", "https://old.example", "from-shell"} {
			if !strings.Contains(res.body, keep) {
				t.Fatalf("%s 把可读片段 %q 抹掉了: %s", label, keep, res.body)
			}
		}
		return ids
	}

	// ① POST /env/scan：强制重扫（不读缓存），夹具第一次经过完整序列化。
	scan := srv.callRaw(t, http.MethodPost, "/api/v1/env/scan", "", true)
	scan.assertEnvelope(t, http.MethodPost, "/api/v1/env/scan")
	ids := assertList("/env/scan", scan)

	// ② GET /env/conflicts：读同一份缓存，正文同样不得带明文。
	conflicts := srv.callRaw(t, http.MethodGet, "/api/v1/env/conflicts", "", true)
	conflicts.assertEnvelope(t, http.MethodGet, "/api/v1/env/conflicts")
	assertList("/env/conflicts", conflicts)

	// ③ POST /env/repair/preview：deleteLines 与扫描共用同一脱敏（单源），
	// 两行的待删行文本都是掩码形态，完整正文无明文。
	preview := srv.callRaw(t, http.MethodPost, "/api/v1/env/repair/preview",
		`{"conflictIds":["`+ids["ANTHROPIC_BASE_URL"]+`","`+ids["ANTHROPIC_MODEL"]+`"]}`, true)
	preview.assertEnvelope(t, http.MethodPost, "/api/v1/env/repair/preview")
	if !preview.ok {
		t.Fatalf("预览失败: %s", preview.body)
	}
	if strings.Contains(preview.body, tok) {
		t.Fatalf("预览响应正文泄漏了令牌明文: %s", preview.body)
	}
	var prev struct {
		Files []struct {
			DeleteLines []struct {
				Line int    `json:"line"`
				Text string `json:"text"`
			} `json:"deleteLines"`
		} `json:"files"`
	}
	if err := json.Unmarshal(preview.data, &prev); err != nil {
		t.Fatalf("解析预览失败: %v（%s）", err, preview.body)
	}
	if len(prev.Files) != 1 || len(prev.Files[0].DeleteLines) != 2 {
		t.Fatalf("预览形状不符: %s", preview.body)
	}
	byLine := map[int]string{}
	for _, dl := range prev.Files[0].DeleteLines {
		byLine[dl.Line] = dl.Text
	}
	wantByLine := map[int]string{1: want["ANTHROPIC_BASE_URL"], 2: want["ANTHROPIC_MODEL"]}
	for line, got := range wantByLine {
		if byLine[line] != got {
			t.Fatalf("待删行[%d]=%q，期望 %q", line, byLine[line], got)
		}
	}
}
