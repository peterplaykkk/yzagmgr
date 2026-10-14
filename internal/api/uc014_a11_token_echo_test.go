// UC-014 A11 的 TOML 回显面回归（Wave A3 判红修复的对抗钉，2026-09-27）。
//
// 判红复现（HEAD 即存在）：TOML 族（codex/grokbuild）的 config_invalid 错误信封
// 回显令牌明文——`docmodel` 的 `记号的 %q 原文` 把用户 live 配置里的令牌带进
// error.message。修法 = api 的失败信封统一咽喉点（failEnvelope）对 message/details
// 做令牌形态掩码，掩码单源在 envcheck（MaskTokenShapes，与 §9 脱敏同判据）。
//
// 本文件的对抗夹具按「令牌形态嵌入不同语法位」构造（值 / 字符串 / 注释 / 段名），
// 对**整个序列化响应正文**扫描：明文子串与 `sk-` 形态都不得出现；同时钉住
// 「掩码而非抹除」——诊断信息（路径、偏移、原因）与恢复入口都仍在。
package api

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/peterplaykkk/yzagmgr/internal/apierr"
	"github.com/peterplaykkk/yzagmgr/internal/family"
	"github.com/peterplaykkk/yzagmgr/internal/tx"
)

// a11Token 是夹具用的令牌形态（前缀 + ≥16 的记号，与 UC-003 A12 的形态同口径）。
const a11Token = "sk-ant-api03-A11VALUE-0123456789abcdef"

// assertNoTokenPlaintext 扫描一段响应原文：夹具令牌与 `sk-` 形态都不得出现。
func assertNoTokenPlaintext(t *testing.T, what, body string) {
	t.Helper()
	if strings.Contains(body, a11Token) {
		t.Errorf("%s：响应正文带上了令牌明文", what)
	}
	if m := secretShape.FindString(body); m != "" {
		t.Errorf("%s：响应正文出现令牌形态 %q", what, m)
	}
}

// TestErrorBodiesCarryNoTokenPlaintextTOML 是 A11 的主对抗回归：
// codex 与 grokbuild 各一族夹具，令牌嵌入不同语法位，preview 的响应正文
// 全序列化不含明文。
//
// 「值」位是 A3 复现的确切形态（也是唯一真会把记号回显进 message 的位），
// 另外三个位置（字符串、注释、段名）是同一回显面的邻位——它们或许不触发回显，
// 但正是「按回显点逐个修」会漏、按信封收口才罩得住的那类分支，因此一并钉住。
func TestErrorBodiesCarryNoTokenPlaintextTOML(t *testing.T) {
	positions := []struct {
		name     string
		live     string
		wantCode apierr.Code // 空串 = 允许成功（此时正文仍须干净）
		// wantMasked 为 true 时断言 message 里掩码替身在场（掩码确实生效，
		// 而不是「不再回显任何内容」把诊断价值一起丢掉）。
		wantMasked bool
		// wantOffset 为 false 的错误分支不报偏移（如「段重复定义」只报路径）——
		// 断言随之放宽，不做超出该分支事实的要求。
		wantOffset bool
	}{
		{
			name: "值位（裸记号）",
			live: "model = " + a11Token + "\n",
			// codex 的值为 model；grokbuild 的 fixtures 单独给（见下）。
			wantCode: apierr.CodeConfigInvalid, wantMasked: true, wantOffset: true,
		},
		{
			name:     "值位（记号后随句点）",
			live:     "model = " + a11Token + ".tail\n",
			wantCode: apierr.CodeConfigInvalid, wantMasked: true, wantOffset: true,
		},
		{
			name:     "值位（记号后随 CJK 句号）",
			live:     "model = " + a11Token + "。尾巴\n",
			wantCode: apierr.CodeConfigInvalid, wantMasked: true, wantOffset: true,
		},
		{
			name:     "值位（记号后随星号）",
			live:     "model = " + a11Token + "*\n",
			wantCode: apierr.CodeConfigInvalid, wantMasked: true, wantOffset: true,
		},
		{
			name:     "值位（引号拼接形态）",
			live:     "model = \"" + a11Token + "\"'extra'\n",
			wantCode: apierr.CodeConfigInvalid, wantOffset: true, // 值后有额外内容：错误按偏移给出，不含记号原文
		},
		{
			name:     "字符串位（引号内，合法字符串 + 后随语法错）",
			live:     "model = \"" + a11Token + "\"\nbroken = [1, 2\n",
			wantCode: apierr.CodeConfigInvalid, wantOffset: true,
		},
		{
			name:     "注释位（行注释 + 后随语法错）",
			live:     "# " + a11Token + "\nmodel = @@@\n",
			wantCode: apierr.CodeConfigInvalid, wantOffset: true,
		},
		{
			name:     "段名位（重复段头回显路径）",
			live:     "[model.\"" + a11Token + "\"]\n[model.\"" + a11Token + "\"]\n",
			wantCode: apierr.CodeConfigInvalid, wantMasked: true,
		},
	}

	for _, familyID := range []string{"codex", "grokbuild"} {
		for _, pos := range positions {
			t.Run(familyID+"/"+pos.name, func(t *testing.T) {
				srv := newContractServer(t)
				live := srv.env.CodexConfig()
				body := pos.live
				if familyID == "grokbuild" {
					live = srv.env.GrokConfig()
					body = grokFixtureFor(pos.name, pos.live)
				}
				writeLiveFile(t, live, []byte(body))

				res := srv.callRaw(t, http.MethodPost, "/api/v1/tools/"+familyID+"/preview",
					`{"profileId":"yzagmgr-local"}`, true)
				res.assertEnvelope(t, http.MethodPost, "/api/v1/tools/"+familyID+"/preview")

				// 全序列化扫描：明文子串与令牌形态都不得出现（成功/失败面一体适用）。
				assertNoTokenPlaintext(t, familyID+"/"+pos.name, res.body)

				if pos.wantCode != "" {
					if res.ok || res.code != pos.wantCode {
						t.Fatalf("期望 %s 被拒（%s），实际 ok=%v code=%q: %s",
							pos.wantCode, pos.name, res.ok, res.code, res.body)
					}
					// 证据留痕（-v 日志）：被拒文案的实际形态（掩码替身在场、诊断保留）。
					t.Logf("拒绝文案: %s", res.message)
					// 诊断价值：掩码是替换而不是整段抹除——原因、偏移与定位都在。
					if !strings.Contains(res.message, "不是可解析的 TOML") && !strings.Contains(res.message, "不是合法 TOML") {
						t.Errorf("拒绝文案丢了原因（不以掩码为由抹除诊断）: %s", res.message)
					}
					// 定位断言用路径后缀（`.codex/config.toml`）：掩码的长跑规则可能把
					// ≥40 的路径段一起掩掉（t.TempDir() 的目录名就带测试名），那是
					// 「宁可多掩」的既定取边，但文件后缀与偏移必须原样保留。
					if !strings.Contains(res.message, "config.toml") {
						t.Errorf("拒绝文案丢了文件定位: %s", res.message)
					}
					if pos.wantOffset && !strings.Contains(res.message, "偏移") {
						t.Errorf("拒绝文案丢了偏移（诊断价值不得被掩码吞掉）: %s", res.message)
					}
					if pos.wantMasked && !strings.Contains(res.message, "<已脱敏>") {
						t.Errorf("回显点的令牌形态未被掩码替身替换: %s", res.message)
					}
				}
			})
		}
	}
}

// TestErrorBodiesCarryNoTokenPlaintextJSONFamilies 把同一纪律钉到**非 TOML 族**上：
// JSON/JSON5 的解析错误同样会回显记号（如 opencode 的「非法数字字面量 %q」把粘在
// 数字后的令牌整段带出），这正是「按 TOML 回显点逐个修」会漏的那类分支——信封
// 收口按定义罩住全部族。
func TestErrorBodiesCarryNoTokenPlaintextJSONFamilies(t *testing.T) {
	cases := []struct {
		name       string
		toolID     string
		write      func(t *testing.T, srv *contractServer)
		wantMasked bool
	}{
		{
			name:   "opencode/值位（令牌粘在数字字面量后）",
			toolID: "opencode",
			write: func(t *testing.T, srv *contractServer) {
				writeLiveFile(t, srv.env.OpenCodeConfig(),
					[]byte(`{"provider": {"x": 123`+a11Token+`}}`))
			},
			wantMasked: true, // 「非法数字字面量 %q」把整段记号带出 ⇒ 必须被掩码替身替换
		},
		{
			name:   "gemini/字符串位（未闭合字符串）",
			toolID: "gemini",
			write: func(t *testing.T, srv *contractServer) {
				writeLiveFile(t, srv.env.GeminiSettings(), []byte(`{"x": "`+a11Token))
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := newContractServer(t)
			tc.write(t, srv)
			res := srv.callRaw(t, http.MethodPost, "/api/v1/tools/"+tc.toolID+"/preview",
				`{"profileId":"yzagmgr-local"}`, true)
			res.assertEnvelope(t, http.MethodPost, "/api/v1/tools/"+tc.toolID+"/preview")
			assertNoTokenPlaintext(t, tc.name, res.body)
			if res.ok || res.code != apierr.CodeConfigInvalid {
				t.Fatalf("期望 config_invalid，实际 ok=%v code=%q: %s", res.ok, res.code, res.body)
			}
			t.Logf("拒绝文案: %s", res.message)
			if tc.wantMasked && !strings.Contains(res.message, "<已脱敏>") {
				t.Errorf("回显点的令牌形态未被掩码替身替换: %s", res.message)
			}
		})
	}
}

// grokFixtureFor 把 codex 形态的夹具翻译成 grokbuild 的语法位（键名不同、值位用 default）。
func grokFixtureFor(position, codexLive string) string {
	switch position {
	case "值位（裸记号）":
		return "default = " + a11Token + "\n"
	case "值位（记号后随句点）":
		return "default = " + a11Token + ".tail\n"
	case "值位（记号后随 CJK 句号）":
		return "default = " + a11Token + "。尾巴\n"
	case "值位（记号后随星号）":
		return "default = " + a11Token + "*\n"
	case "值位（引号拼接形态）":
		return "default = \"" + a11Token + "\"'extra'\n"
	case "字符串位（引号内，合法字符串 + 后随语法错）":
		return "default = \"" + a11Token + "\"\nbroken = [1, 2\n"
	case "注释位（行注释 + 后随语法错）":
		return "# " + a11Token + "\ndefault = @@@\n"
	case "段名位（重复段头回显路径）":
		return "[model.\"" + a11Token + "\"]\n[model.\"" + a11Token + "\"]\n"
	}
	return codexLive
}

// TestFailEnvelopeMasksMessageAndDetails 是信封掩码的单元钉：
// message 与嵌套 details（map/slice/struct）里的令牌形态都被替换；非令牌文本
// 逐字保留；原错误对象不被污染（进程日志/操作日志照旧拿未掩码对象）。
func TestFailEnvelopeMasksMessageAndDetails(t *testing.T) {
	err := apierr.New(apierr.CodeConfigInvalid, "文件 %s 的记号 %q 不合法（偏移 8）", "/tmp/x.toml", a11Token).
		WithDetails(map[string]any{
			"cause": "回显 " + a11Token + "。尾部",
			"nested": map[string]any{
				"token": a11Token,
				"path":  "/tmp/x.toml",
			},
			"list":  []any{a11Token, map[string]any{"inner": a11Token}},
			"files": []tx.FileResult{{Path: "/tmp/" + a11Token, Status: "written"}},
			// UC-093 A4 的需处理清单（[]string）：掩码后必须仍是「数组 + 路径保留」。
			"needsAttention": []string{"/tmp/" + a11Token},
			"count":          3,
		})

	resp := failEnvelope(err)
	raw, merr := json.Marshal(resp)
	if merr != nil {
		t.Fatalf("信封序列化失败: %v", merr)
	}
	assertNoTokenPlaintext(t, "failEnvelope 单元", string(raw))

	// 掩码而非抹除：路径、偏移、结构、计数都还在。
	if !strings.Contains(resp.Error.Message, "/tmp/x.toml") || !strings.Contains(resp.Error.Message, "偏移 8") {
		t.Errorf("message 的诊断文本被抹掉了: %s", resp.Error.Message)
	}
	if resp.Error.Details["count"] != 3 {
		t.Errorf("非字符串 details 被改动: %v", resp.Error.Details["count"])
	}
	if !strings.Contains(resp.Error.Details["nested"].(map[string]any)["path"].(string), "/tmp/x.toml") {
		t.Errorf("普通 details 文本被改动: %v", resp.Error.Details["nested"])
	}
	// 未导出的原对象保持不变（掩码是副本，不反向污染日志面）。
	if !strings.Contains(err.Message, a11Token) {
		t.Errorf("原错误的 message 被就地改动（掩码必须发生在响应副本上）")
	}
	if !strings.Contains(err.Details["cause"].(string), a11Token) {
		t.Errorf("原错误的 details 被就地改动（掩码必须发生在响应副本上）")
	}
	// UC-093 A4 的 needsAttention（[]string 形状）过同一道咽喉点：结构保留、令牌形态掩掉。
	attention, ok := resp.Error.Details["needsAttention"].([]string)
	if !ok || len(attention) != 1 {
		t.Fatalf("needsAttention 的形状被掩码步骤改变: %#v", resp.Error.Details["needsAttention"])
	}
	if strings.Contains(attention[0], a11Token) {
		t.Errorf("needsAttention 混着令牌明文: %q", attention[0])
	}
	if !strings.HasPrefix(attention[0], "/tmp/") {
		t.Errorf("needsAttention 的路径部分被抹掉: %q", attention[0])
	}
	if !strings.Contains(err.Details["needsAttention"].([]string)[0], a11Token) {
		t.Errorf("原错误的 needsAttention 被就地改动（掩码必须发生在响应副本上）")
	}
}

// TestResetApplyRejectsWorkbuddyWithoutSideEffects 钉 A3 核验 issue2：
// reset/apply 与 reset/preview 同一条 proxyOnlyFamily 守卫——WorkBuddy 类
// （走独立写链的）租约工具经工具通道的 reset/apply 被拒，且在**租约消费之前**
// 拒绝：盘上零写入、在途租约原样保留、对照工具的 live 逐字节不变。
func TestResetApplyRejectsWorkbuddyWithoutSideEffects(t *testing.T) {
	srv := newContractServer(t)

	// 对照工具的 live：全程逐字节不变。
	claudeLive := srv.env.ClaudeSettings()
	writeLiveFile(t, claudeLive, []byte("{\"env\":{}}\n"))
	// 一个「若守卫缺失就会被消费」的租约：工具=workbuddy，计划删除一个真实文件。
	victim := filepath.Join(srv.env.Home, "workbuddy-models.json")
	writeLiveFile(t, victim, []byte("[]\n"))
	lease := srv.Leases.Put(&tx.Lease{
		ToolID:    string(family.WorkBuddy),
		Operation: "reset",
		Files: []family.FilePlan{
			{Path: victim, Existed: true, Before: []byte("[]\n"), Delete: true},
		},
	})

	before := snapshotTree(t, srv.env.Home)
	leasesBefore := srv.Leases.Len()

	res := srv.callRaw(t, http.MethodPost, "/api/v1/tools/workbuddy/reset/apply",
		`{"previewId":"`+lease.ID+`"}`, true)
	res.assertEnvelope(t, http.MethodPost, "/api/v1/tools/workbuddy/reset/apply")
	if res.ok || res.code != apierr.CodeNotSupported {
		t.Fatalf("workbuddy 的 reset/apply 应报 not_supported，实际 ok=%v code=%q: %s", res.ok, res.code, res.body)
	}

	// 零副作用：租约没被消费（守卫在 Take 之前）、文件与对照 live 原样。
	if got := srv.Leases.Len(); got != leasesBefore {
		t.Errorf("被拒的 reset/apply 消费了在途租约（%d → %d）", leasesBefore, got)
	}
	if raw, err := os.ReadFile(victim); err != nil || string(raw) != "[]\n" {
		t.Errorf("被拒的 reset/apply 动了盘上文件: %v %q", err, raw)
	}
	assertTreeUnchangedIgnoring(t, "workbuddy reset/apply 被拒后的隔离根", before,
		snapshotTree(t, srv.env.Home), filepath.Join(srv.env.DataRoot(), "operations.jsonl"))

	// 反向：守卫不过宽——正常工具（claude）的 reset 预览与应用照常可达。
	resetRes := srv.callRaw(t, http.MethodPost, "/api/v1/tools/claude/reset/preview", "", true)
	if !resetRes.ok {
		t.Fatalf("claude 的 reset/preview 被误拒: %s", resetRes.body)
	}
	plan := decodePreview(t, resetRes)
	if len(plan.Files) != 1 || plan.Files[0].Path != claudeLive {
		t.Fatalf("claude 重置计划应含在场受管文件，实际: %s", resetRes.data)
	}
	applyRes := srv.callRaw(t, http.MethodPost, "/api/v1/tools/claude/reset/apply",
		`{"previewId":"`+plan.PreviewID+`"}`, true)
	if !applyRes.ok {
		t.Fatalf("claude 的 reset/apply 失败: %s", applyRes.body)
	}
	if _, err := os.Stat(claudeLive); err == nil {
		t.Errorf("claude 重置后受管文件仍在盘上")
	}
	if raw, err := os.ReadFile(victim); err != nil || string(raw) != "[]\n" {
		t.Errorf("对照工具的 live 被重置动作波及: %v %q", err, raw)
	}
}
