// §4.6 配置内容端点的断言（UC-039 A4）：
//
//   - 内容 = 计划的**目标字节**逐文件（与 apply 会写的同一份，sha256 对得上）；
//   - 凭据位一律脱敏（响应正文里不得出现任何令牌明文，含用户 live 里那份与装配期令牌）；
//   - 纯读：调用前后 live 文件逐字节不变（sha256 对照），不物化、不建租约。
//
// 夹具全部写在隔离数据根（t.TempDir）下，不触碰任何真实工具配置。
package api

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/peterplaykkk/yzagmgr/internal/family"
	"github.com/peterplaykkk/yzagmgr/internal/tx"
)

// contentOf 读一张卡的配置详情（信封成功 + 结构解析）。
func contentOf(t *testing.T, srv *contractServer, tool, pid string) profileContentPayload {
	t.Helper()
	res := srv.callRaw(t, http.MethodGet,
		"/api/v1/tools/"+tool+"/profiles/"+pid+"/content", "", true)
	if !res.ok {
		t.Fatalf("读 %s/%s 的配置内容失败: %s", tool, pid, res.body)
	}
	var payload profileContentPayload
	if err := json.Unmarshal(res.data, &payload); err != nil {
		t.Fatalf("配置内容不是预期结构: %v（%s）", err, string(res.data))
	}
	return payload
}

// writeIsolatedLive 在隔离数据根下写一份 live 夹具（建立父目录，0600）。
func writeIsolatedLive(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatalf("建立 live 目录失败: %v", err)
	}
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("写入 live 夹具失败: %v", err)
	}
}

// fileSHA256 读文件并给出内容哈希（文件不存在 = 空串）。
func fileSHA256(t *testing.T, path string) string {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return ""
		}
		t.Fatalf("读取 %s 失败: %v", path, err)
	}
	return tx.HashBytes(raw)
}

// TestProfileContentIsPlannedBytesMaskedAndPureRead 守住 A4 的三条硬断言。
//
// 选 pi 作被测族是因为它的受管形态**把真令牌写进配置**（`apiKey`）：脱敏断言在这里
// 有真实对象（claude 的受管形态写占位符，用它做这条测试会变成空转）。
func TestProfileContentIsPlannedBytesMaskedAndPureRead(t *testing.T) {
	const userKey = "sk-ant-api03-USERLIVEKEY-0123456789abcdef"
	srv := newContractServer(t)

	// live 先行：用户自己的 pi 配置里已有一枚密钥 + 一个自建 provider 条目。
	// 详情页读的是「计划的目标字节」，它由 live 字节做加法得到 ⇒ 用户那枚密钥会**进入**
	// 目标字节（加法保真），因此这条断言有内容可判。
	modelsLive := srv.env.PiModels()
	settingsLive := srv.env.PiSettings()
	writeIsolatedLive(t, modelsLive, `{"providers":{"mine":{"baseUrl":"https://mine.example/v1","apiKey":"`+userKey+`"}}}`)
	writeIsolatedLive(t, settingsLive, `{"theme":"dark"}`)
	beforeModels := fileSHA256(t, modelsLive)
	beforeSettings := fileSHA256(t, settingsLive)

	res := srv.callRaw(t, http.MethodGet,
		"/api/v1/tools/pi/profiles/"+managedCardID+"/content", "", true)
	if !res.ok {
		t.Fatalf("读 pi 受管卡的内容失败: %s", res.body)
	}
	var payload profileContentPayload
	if err := json.Unmarshal(res.data, &payload); err != nil {
		t.Fatalf("配置内容不是预期结构: %v（%s）", err, string(res.data))
	}

	// 形状：自述两个旗标 + profileId 回显。
	if payload.ProfileID != managedCardID || !payload.Masked || payload.Writes {
		t.Fatalf("形状不符：profileId=%q masked=%v writes=%v（期望 %s/true/false）",
			payload.ProfileID, payload.Masked, payload.Writes, managedCardID)
	}
	if len(payload.Files) == 0 {
		t.Fatal("受管卡的内容里一个文件都没有（夹具或实现有问题）")
	}

	// 逐文件与**计划本身**对照：bytes/sha256 取真实字节、content 取脱敏文本。
	plans, err := srv.planForProfile(t.Context(), family.Pi, managedCardID, "")
	if err != nil {
		t.Fatalf("直接计算计划失败: %v", err)
	}
	byPath := map[string]profileContentFile{}
	for _, file := range payload.Files {
		byPath[file.Path] = file
	}
	checked := 0
	for _, plan := range plans {
		if plan.Delete {
			continue
		}
		file, ok := byPath[plan.Path]
		if !ok {
			t.Errorf("计划里的文件 %s 没出现在内容里", plan.Path)
			continue
		}
		checked++
		if file.Bytes != len(plan.After) {
			t.Errorf("%s 的 bytes = %d，期望 %d", plan.Path, file.Bytes, len(plan.After))
		}
		if file.SHA256 != tx.HashBytes(plan.After) {
			t.Errorf("%s 的 sha256 = %q，期望真实字节哈希 %q",
				plan.Path, file.SHA256, tx.HashBytes(plan.After))
		}
		if file.Content != maskContent(string(plan.After)) {
			t.Errorf("%s 的 content 与细粒度脱敏后的计划字节不一致", plan.Path)
		}
	}
	if checked != len(payload.Files) {
		t.Errorf("逐一对照到 %d 个文件，内容里却有 %d 个（多出来的那些是凭空造的？）",
			checked, len(payload.Files))
	}

	// 脱敏：装配期令牌（受管条目的 apiKey）与用户 live 里的密钥都不许出现在响应里。
	body := res.body
	if srv.TokenSeed == "" {
		t.Fatal("夹具没有令牌种子，脱敏断言没有对象")
	}
	if strings.Contains(body, srv.TokenSeed) {
		t.Errorf("响应带出了装配期令牌明文")
	}
	if strings.Contains(body, userKey) {
		t.Errorf("响应带出了 live 配置里的用户密钥明文")
	}
	// 「确实掩了」这一侧在**解出来的内容**上判：原始响应里 `<`/`>` 会被 JSON 编码器
	// 转义成 `<`，在正文里搜掩码值会永远搜不到（那样的断言是空转）。
	// 内容端点用 `***`（UC-039 A4 的用户口径），不是预览差异行的 `<已脱敏>`。
	masks := 0
	for _, file := range payload.Files {
		masks += strings.Count(file.Content, maskedValueContent)
	}
	if masks == 0 {
		t.Errorf("内容里连一个掩码值都没有 —— 脱敏断言退化成空转")
	}
	if strings.Contains(body, "<已脱敏>") || strings.Contains(body, "已脱敏") {
		t.Errorf("内容端点不应出现预览口径的 <已脱敏> 替身（用户口径是 ***）")
	}
	if match := secretShape.FindString(body); match != "" {
		t.Errorf("响应正文出现令牌形态 %q", match)
	}
	// 键名与形状保留（掩的是值不是整行/整对象）。
	if !strings.Contains(body, "apiKey") {
		t.Errorf("掩码把键名也吃掉了（响应里没有 apiKey 键名）")
	}

	// 纯读：live 两个文件逐字节不变。
	if after := fileSHA256(t, modelsLive); after != beforeModels {
		t.Errorf("content 改动了 live 的 models.json（前 %s 后 %s）", beforeModels, after)
	}
	if after := fileSHA256(t, settingsLive); after != beforeSettings {
		t.Errorf("content 改动了 live 的 settings.json（前 %s 后 %s）", beforeSettings, after)
	}
}

// TestProfileContentErrorsFollowClosedSet 守住三条既有语义在详情端点上的收口：
// 未知 pid / 未知工具 id / 无官方形态家族都要用**既有错误码**回答，而不是 500。
func TestProfileContentErrorsFollowClosedSet(t *testing.T) {
	srv := newContractServer(t)

	t.Run("未知 pid ⇒ invalid_request", func(t *testing.T) {
		res := srv.callRaw(t, http.MethodGet,
			"/api/v1/tools/claude/profiles/00000000-0000-4000-8000-000000000000/content", "", true)
		if res.ok || res.code != "invalid_request" {
			t.Fatalf("未知 pid 的响应 = ok:%v code:%q（期望 invalid_request）: %s",
				res.ok, res.code, res.body)
		}
	})

	t.Run("未知工具 id ⇒ not_supported", func(t *testing.T) {
		res := srv.callRaw(t, http.MethodGet,
			"/api/v1/tools/nope/profiles/"+managedCardID+"/content", "", true)
		if res.ok || res.code != "not_supported" {
			t.Fatalf("未知工具 id 的响应 = ok:%v code:%q（期望 not_supported）: %s",
				res.ok, res.code, res.body)
		}
	})

	t.Run("additive 家族的官方卡 ⇒ invalid_request（没有官方档形态）", func(t *testing.T) {
		res := srv.callRaw(t, http.MethodGet,
			"/api/v1/tools/pi/profiles/"+OfficialProfileID+"/content", "", true)
		if res.ok || res.code != "invalid_request" {
			t.Fatalf("pi 官方卡的响应 = ok:%v code:%q（期望 invalid_request）: %s",
				res.ok, res.code, res.body)
		}
	})

	t.Run("WorkBuddy ⇒ not_supported（独立写链，与预览同一条边界）", func(t *testing.T) {
		res := srv.callRaw(t, http.MethodGet,
			"/api/v1/tools/workbuddy/profiles/"+managedCardID+"/content", "", true)
		if res.ok || res.code != "not_supported" {
			t.Fatalf("workbuddy 的响应 = ok:%v code:%q（期望 not_supported）: %s",
				res.ok, res.code, res.body)
		}
	})
}

// TestGeminiManagedProfileContentIncludesManagedEnvTriplet exercises the public
// content handler, not only Gemini's family renderer: the profile detail view
// must expose the same planned model/base/key fields that apply would write,
// while remaining a pure read and masking the assembled inference token.
func TestGeminiManagedProfileContentIncludesManagedEnvTriplet(t *testing.T) {
	srv := newContractServer(t)
	envPath := srv.env.GeminiEnv()
	settingsPath := srv.env.GeminiSettings()
	beforeEnv := fileSHA256(t, envPath)
	beforeSettings := fileSHA256(t, settingsPath)

	payload := contentOf(t, srv, "gemini", managedCardID)
	if !payload.Masked || payload.Writes {
		t.Fatalf("Gemini content flags must be masked/read-only, got masked=%v writes=%v", payload.Masked, payload.Writes)
	}
	var envFile *profileContentFile
	for i := range payload.Files {
		if payload.Files[i].Path == envPath {
			envFile = &payload.Files[i]
			break
		}
	}
	if envFile == nil {
		t.Fatalf("Gemini managed profile content omitted its .env plan")
	}
	plans, err := srv.planForProfile(t.Context(), family.Gemini, managedCardID, "")
	if err != nil {
		t.Fatalf("direct Gemini profile planning failed: %v", err)
	}
	var envPlan *family.FilePlan
	for i := range plans {
		if plans[i].Path == envPath {
			envPlan = &plans[i]
			break
		}
	}
	if envPlan == nil {
		t.Fatalf("direct Gemini plan omitted .env")
	}
	planText := string(envPlan.After)
	t.Logf("Gemini plan has key/model/base=%t/%t/%t; masked content has key/model/base=%t/%t/%t; prior live present=%t before-bytes=%d before-sha=%q after-get-live-sha=%q",
		strings.Contains(planText, "GEMINI_API_KEY="),
		strings.Contains(planText, "GEMINI_MODEL="),
		strings.Contains(planText, "GOOGLE_GEMINI_BASE_URL="),
		strings.Contains(envFile.Content, "GEMINI_API_KEY="),
		strings.Contains(envFile.Content, "GEMINI_MODEL="),
		strings.Contains(envFile.Content, "GOOGLE_GEMINI_BASE_URL="),
		beforeEnv != "", len(envPlan.Before), tx.HashBytes(envPlan.Before), fileSHA256(t, envPath))
	for _, want := range []string{
		"GEMINI_API_KEY=" + maskedValueContent,
		"GEMINI_MODEL=deepseek-flash",
		"GOOGLE_GEMINI_BASE_URL=http://127.0.0.1:17021/v1beta",
	} {
		if !strings.Contains(envFile.Content, want) {
			t.Errorf("Gemini managed profile content omitted %q", want)
		}
	}
	if strings.Contains(envFile.Content, srv.TokenSeed) {
		t.Fatal("Gemini profile content exposed the assembled inference token")
	}
	if after := fileSHA256(t, envPath); after != beforeEnv {
		t.Fatalf("Gemini content GET changed .env: before=%q after=%q", beforeEnv, after)
	}
	if after := fileSHA256(t, settingsPath); after != beforeSettings {
		t.Fatalf("Gemini content GET changed settings.json: before=%q after=%q", beforeSettings, after)
	}
}
