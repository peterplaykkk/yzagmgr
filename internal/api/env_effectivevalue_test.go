// §9 值字段（effectiveValue / expectedValue）令牌形态掩码的 HTTP 级回归钉
// （A3-fix 复核判红唯一项）。
//
// 复核实证（2026-09-27）：非敏感决定键 + 值为「引号拼接」或「含空白」时，
// POST /env/scan 与 GET /env/conflicts 的 items[].effectiveValue 原样回显令牌
// 明文（同一响应的兄弟字段 lineText 已掩）。本用例在真实信封与真实序列化字节上
// 判定：两条路径（含缓存二次读）的完整正文都不含明文，非秘密片段保留可读。
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

func TestEnvResponsesCarryNoEffectiveValueTokenResidue(t *testing.T) {
	// 复核夹具令牌：ghp_ 前缀 + 29 字节记号（分段写会用到 tail）。
	const tail = "V9QZ7XGHP0123456789abcdefghij"
	const tok = "ghp_" + tail
	srv := newContractServer(t)

	// 与既有回归钉同一纪律：扫描器只扫隔离根，绝不读真实 $HOME、真实 launchd
	// 或真实会话环境。
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
	// 实证A（反向拼接）用 ANTHROPIC_BASE_URL 复刻复核夹具；实证B 形态（含空白）
	// 换 ANTHROPIC_MODEL 承载——同名赋值在文件内只有最后一条生效，一个决定键
	// 装不下两个夹具（根因与键名无关，两者同为非敏感决定键）。
	lineA := `export ANTHROPIC_BASE_URL='ghp_'"` + tail + `"`
	lineB := `export ANTHROPIC_MODEL="` + tok + ` trailing text"`
	if err := os.WriteFile(rc, []byte(lineA+"\n"+lineB+"\n"), 0o644); err != nil {
		t.Fatalf("写夹具启动文件失败: %v", err)
	}

	type envConflict struct {
		ID             string `json:"id"`
		Name           string `json:"name"`
		EffectiveValue string `json:"effectiveValue"`
		ExpectedValue  string `json:"expectedValue"`
		Sensitive      bool   `json:"sensitive"`
		LineText       string `json:"lineText"`
	}
	wantValue := map[string]string{
		"ANTHROPIC_BASE_URL": envcheck.MaskedValue,
		"ANTHROPIC_MODEL":    envcheck.MaskedValue + " trailing text",
	}
	wantExpected := map[string]string{
		"ANTHROPIC_BASE_URL": "http://127.0.0.1:17021/claude",
		"ANTHROPIC_MODEL":    "from-widget",
	}
	scannedAt := ""
	assertList := func(label string, res envelope) {
		t.Helper()
		if !res.ok {
			t.Fatalf("%s 请求失败: %s", label, res.body)
		}
		// 整份序列化正文不得含任何形态的令牌明文（含不带前缀的记号段）。
		for _, s := range []string{tok, tail} {
			if strings.Contains(res.body, s) {
				t.Fatalf("%s 响应正文泄漏了令牌明文 %q: %s", label, s, res.body)
			}
		}
		var list struct {
			Items []envConflict `json:"items"`
			Total int           `json:"total"`
			// scannedAt 用于证明接下来的读取确实走的缓存（同一份载荷）。
			ScannedAt string `json:"scannedAt"`
		}
		if err := json.Unmarshal(res.data, &list); err != nil {
			t.Fatalf("解析 %s 列表失败: %v（%s）", label, err, res.body)
		}
		if list.Total != 2 || len(list.Items) != 2 {
			t.Fatalf("%s 期望 2 条冲突: %s", label, res.body)
		}
		for _, it := range list.Items {
			if it.Sensitive {
				t.Fatalf("%s 条目自身不该敏感（泄漏正是经由非敏感条目发生）: %+v", label, it)
			}
			if it.EffectiveValue != wantValue[it.Name] {
				t.Fatalf("%s 的 effectiveValue=%q，期望 %q", label, it.EffectiveValue, wantValue[it.Name])
			}
			if it.ExpectedValue != wantExpected[it.Name] {
				t.Fatalf("%s 的 expectedValue=%q，期望 %q", label, it.ExpectedValue, wantExpected[it.Name])
			}
			if !strings.Contains(it.LineText, envcheck.MaskedValue) {
				t.Fatalf("%s 的 lineText 未掩令牌: %q", label, it.LineText)
			}
		}
		// 非秘密片段（键名、尾部文案、非秘密期望值）必须保留可读。
		for _, keep := range []string{"ANTHROPIC_BASE_URL", "ANTHROPIC_MODEL", "trailing text", "from-widget"} {
			if !strings.Contains(res.body, keep) {
				t.Fatalf("%s 把可读片段 %q 抹掉了: %s", label, keep, res.body)
			}
		}
		if scannedAt == "" {
			scannedAt = list.ScannedAt
		} else if list.ScannedAt != scannedAt {
			t.Fatalf("%s 的 scannedAt=%q 与首读 %q 不一致（读取没有走同一份缓存）", label, list.ScannedAt, scannedAt)
		}
	}

	// ① POST /env/scan：强制重扫（不读缓存），夹具第一次经过完整序列化。
	scan := srv.callRaw(t, http.MethodPost, "/api/v1/env/scan", "", true)
	scan.assertEnvelope(t, http.MethodPost, "/api/v1/env/scan")
	assertList("/env/scan", scan)

	// ② GET /env/conflicts：读缓存（scan 已把此刻定为准），正文同样不得带明文。
	conflicts := srv.callRaw(t, http.MethodGet, "/api/v1/env/conflicts", "", true)
	conflicts.assertEnvelope(t, http.MethodGet, "/api/v1/env/conflicts")
	assertList("/env/conflicts", conflicts)

	// ③ GET /env/conflicts 二次读：仍走缓存，同一份载荷再序列化一次（缓存路径
	// 与强制重扫路径的载荷在场性一致）。
	again := srv.callRaw(t, http.MethodGet, "/api/v1/env/conflicts", "", true)
	again.assertEnvelope(t, http.MethodGet, "/api/v1/env/conflicts")
	assertList("/env/conflicts（缓存二次读）", again)
}
