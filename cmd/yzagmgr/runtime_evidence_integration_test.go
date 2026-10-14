// §4.8 的**隔离守护进程**集成用例（UC-092 A11 的负路径在真装配链路里成立）。
//
// 与 internal/api 契约夹具的分工：本用例装配的是 **cmd 的真实装配路径**（assemble：
// 真实策略仓库/用量库/代理对象/环境扫描器 + 隔离数据根 + 一次性管理令牌），经真实
// HTTP（回环监听）走一遍「应用受管卡 → 读取运行时证据 → 受控验证预览 → 受控验证执行」。
// 它证明的是两件事：
//   - 新账本/新端点/新闸门在真实守护进程里接得上（不是只在测试夹具里成立）；
//   - **在无法确定硬限的环境里，受控验证拒绝发起**（0 次模型调用、不写 live、不跑任何
//     子进程、不发任何网络请求）——这正是硬边界「额度不足不调用、无法确定不主动发起」
//     的默认世界（未登录守护进程）。
//
// 受控运行的**成功路径**（真跑一次 CLI）由 internal/api 的执行缝覆盖：仓库纪律是
// 「单元测试绝不跑子进程」，本用例因此刻意只走拒绝路径——把「不发起调用」本身钉住。
package main

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/peterplaykkk/yzagmgr/internal/config"
	"github.com/peterplaykkk/yzagmgr/internal/family"
)

// seedPolicyForRuntimeEvidence 是给隔离守护进程的种子策略（形状 = GET /policy 的响应）。
const seedPolicyForRuntimeEvidence = `{
  "version": 7,
  "defaultModel": "deepseek-flash",
  "models": [
    {"id": "deepseek-flash", "name": "DeepSeek Flash", "supportsImages": true,
     "maxInputTokens": 1000000, "maxOutputTokens": 384000}
  ],
  "catalogs": {"codex": {"models": [{"id": "deepseek-flash"}]}}
}`

// TestIsolatedDaemonRuntimeEvidenceRefusesToSpendWithoutDeterminableLimits 走完整链路：
// 应用受管卡 → 读取面未确认 → 预览给出全字段（含「不会发起」的阻塞原因）→ 执行被拒绝且
// 0 次调用 → live 字节前后一致 → 尝试留痕可核。
func TestIsolatedDaemonRuntimeEvidenceRefusesToSpendWithoutDeterminableLimits(t *testing.T) {
	root := t.TempDir()
	t.Setenv(config.EnvHomeVar, root)
	t.Setenv("YZAGMGR_DB_DSN", "")
	// 关掉周期维护（本用例不需要后台清理，也不该有）。
	t.Setenv(usageMaintenanceEnv, "off")

	env, err := config.FromProcess()
	if err != nil {
		t.Fatalf("构造隔离环境失败: %v", err)
	}
	if !env.Isolated {
		t.Fatalf("YZAGMGR_HOME=%q 时 Isolated 应为真（否则用例会写到真实 home）", root)
	}
	if err := env.EnsureLayout(); err != nil {
		t.Fatalf("建立数据根失败: %v", err)
	}
	// 种子策略：写进文件并让守护进程的种子读取面读到它（与真实验收同一入口）。
	seedPath := filepath.Join(root, "seed-policy.json")
	if err := os.WriteFile(seedPath, []byte(seedPolicyForRuntimeEvidence), 0o600); err != nil {
		t.Fatalf("写种子策略失败: %v", err)
	}
	t.Setenv("YZAGMGR_POLICY_FILE", seedPath)
	seeds := readSeeds(env)
	if err := seeds.apply(env); err != nil {
		t.Fatalf("应用种子策略失败: %v", err)
	}

	token, err := config.NewAdminToken()
	if err != nil {
		t.Fatalf("生成管理令牌失败: %v", err)
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	// 版本探测白名单 = 空切片 ⇒ 一个子进程都不起（单元测试纪律）。
	server, cleanup, err := assemble(env, token, 17022, 17021, seeds, []family.ID{}, logger)
	if err != nil {
		t.Fatalf("装配隔离守护进程失败: %v", err)
	}
	defer cleanup()
	// 真实 HTTP 面（回环监听；数据面刻意不启动——本用例只走管理面）。
	httpServer := httptest.NewServer(server)
	defer httpServer.Close()

	call := func(method, path, body string) (int, string) {
		t.Helper()
		var reader io.Reader
		if body != "" {
			reader = strings.NewReader(body)
		}
		req, err := http.NewRequest(method, httpServer.URL+path, reader)
		if err != nil {
			t.Fatalf("构造请求失败: %v", err)
		}
		req.Header.Set("X-Agmgr-Token", token)
		if body != "" {
			req.Header.Set("Content-Type", "application/json")
		}
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("%s %s 请求失败: %v", method, path, err)
		}
		defer func() { _ = res.Body.Close() }()
		payload, err := io.ReadAll(res.Body)
		if err != nil {
			t.Fatalf("读响应失败: %v", err)
		}
		return res.StatusCode, string(payload)
	}

	// 0) 应用受管卡（走真实预览 → 应用事务）。
	status, body := call(http.MethodPost, "/api/v1/tools/claude/preview", `{"profileId":"yzagmgr-local"}`)
	if status != http.StatusOK || !strings.Contains(body, `"previewId"`) {
		t.Fatalf("预览失败: %d %s", status, body)
	}
	previewID := jsonField(t, body, "previewId")
	status, body = call(http.MethodPost, "/api/v1/tools/claude/apply", `{"previewId":"`+previewID+`"}`)
	if status != http.StatusOK || !strings.Contains(body, `"applied"`) {
		t.Fatalf("应用失败: %d %s", status, body)
	}
	settings := env.ClaudeSettings()
	liveBefore, err := os.ReadFile(settings)
	if err != nil {
		t.Fatalf("读 live 失败: %v", err)
	}
	if !strings.Contains(string(liveBefore), "http://127.0.0.1:17021/claude") {
		t.Fatalf("受管档没有落成本地 route 形态: %s", liveBefore)
	}

	// 1) 读取面：从未验证 ⇒ 未确认 + 机器可读原因；能力面如实报可用。
	status, body = call(http.MethodGet, "/api/v1/tools/claude/runtime-evidence", "")
	if status != http.StatusOK || !strings.Contains(body, `"state":"unconfirmed"`) ||
		!strings.Contains(body, `"reason":"never_verified"`) {
		t.Fatalf("读取面未确认判定不符: %d %s", status, body)
	}
	if !strings.Contains(body, `"supported":true`) {
		t.Fatalf("能力面应报支持（claude）：%s", body)
	}

	// 2) 预览：全字段可读；本地限额无记录（无记录=不限，F2/UC-041）但**无定价 ⇒ 报价未知**，
	//    阻塞发起（额度未知态保留给「用量库读不出」的场景）。
	status, body = call(http.MethodPost, "/api/v1/tools/claude/runtime-evidence/verify/preview", "")
	if status != http.StatusOK {
		t.Fatalf("验证预览失败: %d %s", status, body)
	}
	for _, want := range []string{
		`"willCallModel":true`, `"quote":"unknown"`, `"hardCapUsd":0.5`,
		`"reason":"quote_unknown"`, `"marker":"agv-`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("预览载荷缺 %s: %s", want, body)
		}
	}
	verifyPreviewID := jsonField(t, body, "previewId")

	// 3) 执行：被闸门拒绝 ⇒ 0 次调用、未确认、原因机器可读；live 一个字节都不动。
	status, body = call(http.MethodPost, "/api/v1/tools/claude/runtime-evidence/verify/apply",
		`{"previewId":"`+verifyPreviewID+`"}`)
	if status != http.StatusOK {
		t.Fatalf("验证执行的响应应为成功信封（拒绝在载荷里表达）: %d %s", status, body)
	}
	for _, want := range []string{
		`"status":"unconfirmed"`, `"reason":"quote_unknown"`, `"modelCalls":0`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("执行载荷缺 %s: %s", want, body)
		}
	}
	liveAfter, err := os.ReadFile(settings)
	if err != nil {
		t.Fatalf("复读 live 失败: %v", err)
	}
	if string(liveBefore) != string(liveAfter) {
		t.Errorf("被拒绝的受控验证改动了 live 字节")
	}

	// 4) 尝试留痕可核（追加式；重启存活——证据记账在隔离数据根下）。
	status, body = call(http.MethodGet, "/api/v1/tools/claude/runtime-evidence", "")
	if status != http.StatusOK || !strings.Contains(body, `"attemptCount":1`) ||
		!strings.Contains(body, `"modelCalls":0`) {
		t.Fatalf("尝试留痕不可核: %d %s", status, body)
	}
	evidenceLedger := filepath.Join(env.ManagedDir(), "runtime-evidence.json")
	if _, err := os.Stat(evidenceLedger); err != nil {
		t.Fatalf("证据记账未落盘: %v", err)
	}

	// 5) 凭据纪律：全流程响应不含管理令牌与（种子）推理令牌。
	for _, secret := range []string{token, seeds.token} {
		if secret == "" {
			continue
		}
		if strings.Contains(body, secret) {
			t.Errorf("响应泄漏令牌明文")
		}
	}
}

// jsonField 从 JSON 文本里取一个字符串字段（只用于本文件的固定形状）。
func jsonField(t *testing.T, body, key string) string {
	t.Helper()
	marker := `"` + key + `":"`
	idx := strings.Index(body, marker)
	if idx < 0 {
		t.Fatalf("JSON 里没有字段 %s: %s", key, body)
	}
	rest := body[idx+len(marker):]
	end := strings.IndexByte(rest, '"')
	if end < 0 {
		t.Fatalf("字段 %s 不是字符串: %s", key, body)
	}
	return rest[:end]
}
