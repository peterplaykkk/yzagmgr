// §4.6 配置测试端点的断言（UC-039 A6）：
//
//   - 前置：不在支持集里的家族、以及本机没装的工具，都要在**跑任何子进程之前**被拒
//     （not_supported + 可读原因），界面上按钮据此禁用；
//   - 行为：真的把计划写进 0700 临时根、以 HOME=<临时根> 调 CLI，成败都清理临时根；
//   - 诚实：退出码/超时/输出如实汇报（status:failed 不是契约错误，但操作日志记 failed）；
//   - 安全：响应与日志无令牌明文，live 配置逐字节不变。
//
// **单元测试绝不跑真 CLI**（仓库纪律）：执行面与「装没装」的判定面各有一道注入缝
// （`profileTestExecute` / `profileTestResolveCLI`），本文件全部用桩——真机跑格由
// 验收 lane 在部署实例上执行。
package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/peterplaykkk/yzagmgr/internal/family"
)

// fakeCLI 记录一次执行请求并给出预置结果（桩以闭包形式注入 Server）。
type fakeCLI struct {
	mu    sync.Mutex
	calls []profileTestExecRequest
	// respond 决定本次结果；nil = 成功且回一段 pi 风格的事件流。
	respond func(req profileTestExecRequest) profileTestExecResult
	// beforeReturn 在返回前跑（用于在「进程还在跑」的窗口里做观察或阻塞）。
	beforeReturn func(req profileTestExecRequest)
}

func (f *fakeCLI) exec(_ context.Context, req profileTestExecRequest) profileTestExecResult {
	f.mu.Lock()
	f.calls = append(f.calls, req)
	f.mu.Unlock()
	if f.beforeReturn != nil {
		f.beforeReturn(req)
	}
	if f.respond != nil {
		return f.respond(req)
	}
	if challenge, ok := challengeFromProfileTestRequest(req); ok {
		return profileTestExecResult{
			Stdout: profileTestSuccessOutput(filepath.Base(req.Exe), challenge), ExitCode: 0,
		}
	}
	return profileTestExecResult{
		Stdout: piEventStream("deepseek-flash", "yzagmgr-local", "正常"),
	}
}

func (f *fakeCLI) recorded() []profileTestExecRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]profileTestExecRequest, len(f.calls))
	copy(out, f.calls)
	return out
}

// piEventStream 造一段 pi `--mode json` 形态的事件流（结尾是最终权威的 message_end）。
func piEventStream(model, provider, reply string) string {
	lines := []string{
		`{"type":"session","version":3,"id":"sess-1"}`,
		`{"type":"agent_start"}`,
		`{"type":"message_start","message":{"role":"assistant","content":[]}}`,
		`{"type":"message_update","assistantMessageEvent":{"type":"text_delta","delta":"` + reply + `"}}`,
		`{"type":"message_end","message":{"role":"assistant","content":[{"type":"text","text":"` + reply +
			`"}],"provider":"` + provider + `","model":"` + model + `","responseModel":"` + model + `"}}`,
		`{"type":"agent_end","messages":[]}`,
	}
	return strings.Join(lines, "\n") + "\n"
}

// testableServer 造一个可注入桩的契约夹具（安装判定恒为「装了」，除非另行覆盖）。
//
// 解析桩按 **cli 名**返回路径（不是固定一个 pi 路径）：否则「另一个工具」的用例会拿到
// 同一个 exe，按 exe 做的分流（如本文件并发用例的闸门）会误伤。
func testableServer(t *testing.T) (*contractServer, *fakeCLI) {
	t.Helper()
	srv := newContractServer(t)
	fake := &fakeCLI{}
	srv.profileTestExecute = fake.exec
	srv.profileTestResolveCLI = func(_ family.ID, cli string) string { return "/fake/bin/" + cli }
	return srv, fake
}

// testPayloadOf 发一次测试请求并解出载荷（要求信封成功）。
func testPayloadOf(t *testing.T, srv *contractServer, tool, pid string) profileTestPayload {
	t.Helper()
	res := srv.callRaw(t, http.MethodPost,
		"/api/v1/tools/"+tool+"/profiles/"+pid+"/test", "", true)
	if !res.ok {
		t.Fatalf("测试端点失败: %s", res.body)
	}
	var payload profileTestPayload
	if err := json.Unmarshal(res.data, &payload); err != nil {
		t.Fatalf("测试载荷不是预期结构: %v（%s）", err, string(res.data))
	}
	return payload
}

// TestProfileTestUnsupportedFamiliesAreRefusedWithReason 守住「不支持的家族给出明确禁用原因
// 而不是隐藏」：GUI 应用与无 CLI 入口/无配方的单元都在查配方这一步就被拒，一个子进程都不起；
// 机器可读的 reason（A6 additive）区分 gui_family 与 no_recipe 两条解释。
func TestProfileTestUnsupportedFamiliesAreRefusedWithReason(t *testing.T) {
	srv, fake := testableServer(t)
	cases := []struct {
		tool       string
		wantReason string
	}{
		{"claude-desktop", profileTestReasonGuiFamily},
		{"gemini", profileTestReasonNoRecipe},
		{"hermes", profileTestReasonNoRecipe},
	}
	for _, tc := range cases {
		res := srv.callRaw(t, http.MethodPost,
			"/api/v1/tools/"+tc.tool+"/profiles/"+managedCardID+"/test", "", true)
		if res.ok || res.code != "not_supported" {
			t.Errorf("%s 的测试响应 = ok:%v code:%q（期望 not_supported）: %s",
				tc.tool, res.ok, res.code, res.body)
		}
		if !strings.Contains(res.message, tc.tool) {
			t.Errorf("%s 的拒绝文案没有点名该单元: %q", tc.tool, res.message)
		}
		if got := reasonOf(t, res); got != tc.wantReason {
			t.Errorf("%s 的前置分类 = %q（期望 %q）", tc.tool, got, tc.wantReason)
		}
	}
	if calls := fake.recorded(); len(calls) != 0 {
		t.Fatalf("不支持的家族仍然起过进程: %+v", calls)
	}
}

// TestProfileTestUninstalledToolIsRefusedWithReason 守住「本机没装就点不动」：
// 解析不到可执行文件 ⇒ not_supported，且文案说清是「未安装」（机器可读 reason 同源）。
func TestProfileTestUninstalledToolIsRefusedWithReason(t *testing.T) {
	srv, fake := testableServer(t)
	srv.profileTestResolveCLI = func(family.ID, string) string { return "" }

	res := srv.callRaw(t, http.MethodPost,
		"/api/v1/tools/pi/profiles/"+managedCardID+"/test", "", true)
	if res.ok || res.code != "not_supported" {
		t.Fatalf("未安装时的响应 = ok:%v code:%q（期望 not_supported）: %s", res.ok, res.code, res.body)
	}
	if !strings.Contains(res.message, "未安装") {
		t.Errorf("未安装的文案没有说清原因: %q", res.message)
	}
	if got := reasonOf(t, res); got != profileTestReasonNotInstalled {
		t.Errorf("前置分类 = %q（期望 not_installed）", got)
	}
	if calls := fake.recorded(); len(calls) != 0 {
		t.Fatalf("未安装时仍然起过进程: %+v", calls)
	}
}

// TestProfileTestNotExecutableIsClassifiedBeforeAnyProcess 守 A6 的独立前置分类：
// 可执行文件在场但**没有执行权限** ⇒ not_executable（不是 not_installed、不是缺配方），
// 文案带「不建议在自动化中给系统权限」口径并给出该文件路径，且**一个子进程都不起**；
// chmod +x 后同一夹具恢复可测（分类不粘滞）。
func TestProfileTestNotExecutableIsClassifiedBeforeAnyProcess(t *testing.T) {
	srv, fake := testableServer(t)
	// 夹具目录名刻意取短前缀（不带测试名）：长连写段会被信封的凭据掩码当成令牌形态，
	// 让 details.path 在响应里被 `<已脱敏>` 掉——那测的是掩码，不是本用例的语义。
	dir, err := os.MkdirTemp("", "yzagmgr-c2-notexec-")
	if err != nil {
		t.Fatalf("建夹具目录失败: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	fixture := filepath.Join(dir, "pi")
	if err := os.WriteFile(fixture, []byte("#!/bin/sh\nexit 0\n"), 0o600); err != nil {
		t.Fatalf("造夹具失败: %v", err)
	}
	if err := os.Chmod(fixture, 0o644); err != nil { // 显式无执行位（不受 umask 影响）
		t.Fatalf("chmod 夹具失败: %v", err)
	}
	srv.profileTestResolveCLI = func(family.ID, string) string { return fixture }

	res := srv.callRaw(t, http.MethodPost,
		"/api/v1/tools/pi/profiles/"+managedCardID+"/test", "", true)
	if res.ok || res.code != "not_supported" {
		t.Fatalf("无执行权限的响应 = ok:%v code:%q（期望 not_supported）: %s", res.ok, res.code, res.body)
	}
	details := preflightDetails(t, res)
	if details.Reason != profileTestReasonNotExecutable {
		t.Errorf("前置分类 = %q（期望 not_executable）", details.Reason)
	}
	if details.Path != fixture {
		t.Errorf("details.path = %q（期望夹具路径 %q）", details.Path, fixture)
	}
	for _, want := range []string{"执行权限", "不建议你在自动化中给系统权限"} {
		if !strings.Contains(res.message, want) {
			t.Errorf("文案缺 %q: %q", want, res.message)
		}
	}
	if calls := fake.recorded(); len(calls) != 0 {
		t.Fatalf("无执行权限时仍然起过进程: %+v", calls)
	}

	// 正向对照：chmod +x 后同一路径恢复可测（同一次测试内证明分类判据就是执行位）。
	if err := os.Chmod(fixture, 0o755); err != nil {
		t.Fatalf("chmod +x 失败: %v", err)
	}
	payload := testPayloadOf(t, srv, "pi", managedCardID)
	if payload.Status != profileTestStatusOK {
		t.Fatalf("chmod +x 后测试未恢复: %+v", payload)
	}
	calls := fake.recorded()
	if len(calls) != 1 || calls[0].Exe != fixture {
		t.Fatalf("执行面记录 = %+v（期望恰一次、exe=夹具）", calls)
	}
}

// TestProfileTestWritesPlanIntoIsolatedRootAndReportsOk 守住 A6 的主流程：
// 计划真的写进临时根（含真令牌）、CLI 在假 HOME 下被调用、结果解析回填、临时根用后即删、
// live 配置逐字节不变、响应正文无令牌。
func TestProfileTestWritesPlanIntoIsolatedRootAndReportsOk(t *testing.T) {
	srv, fake := testableServer(t)

	// live 先落一份用户自己的 pi 配置（内容会被加法保真带进计划）。
	modelsLive := srv.env.PiModels()
	settingsLive := srv.env.PiSettings()
	writeIsolatedLive(t, modelsLive, `{"providers":{"mine":{"baseUrl":"https://mine.example/v1","apiKey":"sk-user-own-0123456789abcdef"}}}`)
	writeIsolatedLive(t, settingsLive, `{"theme":"dark"}`)
	beforeModels, beforeSettings := fileSHA256(t, modelsLive), fileSHA256(t, settingsLive)

	var (
		root       string
		rootMode   os.FileMode
		written    = map[string]string{}
		filesByRel = map[string]bool{}
	)
	fake.beforeReturn = func(req profileTestExecRequest) {
		root = req.Root
		if st, err := os.Stat(req.Root); err == nil {
			rootMode = st.Mode().Perm()
		}
		// 临时根里必须真的躺着这份配置（按 live 路径相对 $HOME 的结构）。
		_ = filepath.WalkDir(req.Root, func(path string, d os.DirEntry, err error) error {
			if err != nil || d.IsDir() {
				return nil
			}
			rel, relErr := filepath.Rel(req.Root, path)
			if relErr != nil {
				return nil
			}
			filesByRel[filepath.ToSlash(rel)] = true
			raw, readErr := os.ReadFile(path)
			if readErr == nil {
				written[filepath.ToSlash(rel)] = string(raw)
			}
			return nil
		})
	}

	payload := testPayloadOf(t, srv, "pi", managedCardID)

	if payload.Status != profileTestStatusOK {
		t.Fatalf("状态 = %q（期望 ok）：reason=%q output=%q",
			payload.Status, payload.Reason, payload.Output)
	}
	if payload.CLI != "pi" || payload.ExitCode != 0 || payload.TimedOut {
		t.Errorf("元数据 = cli:%q exit:%d timeout:%v（期望 pi/0/false）",
			payload.CLI, payload.ExitCode, payload.TimedOut)
	}
	if payload.Model != "deepseek-flash" || payload.Provider != "yzagmgr-local" {
		t.Errorf("模型解析 = %q/%q（期望 deepseek-flash/yzagmgr-local）",
			payload.Model, payload.Provider)
	}
	if payload.Output == "" {
		t.Errorf("output 为空（失败诊断与成功回显都靠它）")
	}

	calls := fake.recorded()
	if len(calls) != 1 {
		t.Fatalf("执行次数 = %d（期望恰 1 次）", len(calls))
	}
	call := calls[0]
	if call.Exe != "/fake/bin/pi" {
		t.Errorf("可执行 = %q（期望解析出来的路径）", call.Exe)
	}
	if challenge, ok := challengeFromProfileTestRequest(call); !ok {
		t.Errorf("参数末尾应是本次运行唯一挑战提示，而不是静态提示：%v", call.Args)
	} else if strings.Contains(payload.Output, challenge) {
		t.Errorf("结果摘要回显了本次挑战: %q", payload.Output)
	}
	if call.Root == "" || call.Root != root {
		t.Errorf("临时根不一致: req=%q 观察=%q", call.Root, root)
	}
	if rootMode != 0o700 {
		t.Errorf("临时根权限 = %o（期望 700）", rootMode)
	}
	envOf := func(key string) string {
		for _, kv := range call.Env {
			if strings.HasPrefix(kv, key+"=") {
				return strings.TrimPrefix(kv, key+"=")
			}
		}
		return ""
	}
	if got := envOf("HOME"); got != root {
		t.Errorf("HOME = %q（期望临时根 %q）", got, root)
	}
	if got := envOf("PI_CODING_AGENT_DIR"); got != filepath.Join(root, ".pi", "agent") {
		t.Errorf("PI_CODING_AGENT_DIR = %q（期望临时根下的 .pi/agent）", got)
	}
	// 环境里不得有重复键：否则「HOME 指向临时根」取决于 exec 对重复键的取舍规则，
	// 而不是取决于我们写的这一行。
	seenKey := map[string]bool{}
	for _, kv := range call.Env {
		i := strings.IndexByte(kv, '=')
		if i <= 0 {
			continue
		}
		if seenKey[kv[:i]] {
			t.Errorf("环境里出现重复键 %s（覆盖必须显式摘掉继承值）", kv[:i])
		}
		seenKey[kv[:i]] = true
	}

	// 写进临时根的字节 = 计划的目标字节（含真令牌——这正是「真实写入配置」的证据）。
	plans, err := srv.planForProfile(t.Context(), family.Pi, managedCardID, "")
	if err != nil {
		t.Fatalf("直接计算计划失败: %v", err)
	}
	planned := map[string]string{}
	for _, plan := range plans {
		if plan.Delete {
			continue
		}
		rel, relErr := relativeUnderHome(srv.env.Home, plan.Path)
		if relErr != nil {
			t.Fatalf("计划路径不在 HOME 下: %v", relErr)
		}
		planned[rel] = string(plan.After)
	}
	if len(planned) != len(written) {
		t.Fatalf("临时根里的文件数 = %d（计划 %d）：%v", len(written), len(planned), filesByRel)
	}
	for rel, want := range planned {
		if written[rel] != want {
			t.Errorf("临时根里的 %s 与计划字节不一致", rel)
		}
	}
	if !strings.Contains(written[filepath.ToSlash(filepath.Join(".pi", "agent", "models.json"))], srv.TokenSeed) {
		t.Errorf("临时根里的 models.json 没有真令牌 —— 测的就不是这份配置")
	}

	// 用后即删：响应返回后临时根必须不存在。
	if _, err := os.Stat(root); !os.IsNotExist(err) {
		t.Errorf("临时根没有清理干净: %v", err)
	}
	// live 逐字节不变。
	if after := fileSHA256(t, modelsLive); after != beforeModels {
		t.Errorf("live models.json 被改动（前 %s 后 %s）", beforeModels, after)
	}
	if after := fileSHA256(t, settingsLive); after != beforeSettings {
		t.Errorf("live settings.json 被改动（前 %s 后 %s）", beforeSettings, after)
	}
}

// TestProfileTestLeavesAuthAndSessionFilesUntouched 守 A3（C-2 补强，2026-09-27）：
// 测试前后**认证文件、yzagmgr 自身 session 文件与工具用户会话目录**逐字节一致。
//
// 与既有用例的分工：`TestProfileTestWritesPlanIntoIsolatedRootAndReportsOk` 覆盖
// 「live 配置（pi models.json/settings.json，含令牌）前后 sha 一致 + 临时根无残留」；
// 这里把核对面扩到 C-0 审计点名的两处：
//   - yzagmgr 自身的会话文件（`<数据根>/private/session.json`）——测试绝不能碰登录态；
//   - 工具用户会话目录（pi 的 `<HOME>/.pi/agent/sessions/**`）——测试绝不能动用户会话。
func TestProfileTestLeavesAuthAndSessionFilesUntouched(t *testing.T) {
	srv, _ := testableServer(t)

	// 认证文件面：pi 的 live models.json（含令牌）——与既有断言同源，这里一并快照。
	modelsLive := srv.env.PiModels()
	writeIsolatedLive(t, modelsLive, `{"providers":{"mine":{"baseUrl":"https://mine.example/v1","apiKey":"sk-user-own-0123456789abcdef"}}}`)
	// yzagmgr 自身 session 文件（形状合法；夹具构建后无人回读，纯字节断言）。
	sessionFile := srv.env.SessionFile()
	writeIsolatedLive(t, sessionFile, `{"schema":1,"origin":"https://gateway.invalid",`+
		`"access":"fixture-access-token-0123456789","accessExpiresAt":4102444800}`)
	// 工具用户会话目录（pi 的真实布局：`<agent dir>/sessions/<cwd 形态>/*.jsonl`）。
	toolSessions := filepath.Join(srv.env.Home, ".pi", "agent", "sessions", "--tmp--")
	if err := os.MkdirAll(toolSessions, 0o700); err != nil {
		t.Fatalf("建立工具会话目录失败: %v", err)
	}
	toolSessionFile := filepath.Join(toolSessions, "2026-09-27T00-00-00-fixture.jsonl")
	writeIsolatedLive(t, toolSessionFile, `{"session":"fixture","turns":3}`+"\n")

	watched := []string{modelsLive, sessionFile, toolSessionFile}
	before := map[string]string{}
	for _, path := range watched {
		before[path] = fileSHA256(t, path)
	}
	beforeEntries := dirEntryNames(t, toolSessions)

	payload := testPayloadOf(t, srv, "pi", managedCardID)
	if payload.Status != profileTestStatusOK {
		t.Fatalf("测试没有跑成: %+v", payload)
	}

	for _, path := range watched {
		if after := fileSHA256(t, path); after != before[path] {
			t.Errorf("%s 被改动（前 %s 后 %s）", path, before[path], after)
		}
	}
	if afterEntries := dirEntryNames(t, toolSessions); !equalStrings(beforeEntries, afterEntries) {
		t.Errorf("工具用户会话目录出现增删: 前 %v 后 %v", beforeEntries, afterEntries)
	}
	// 反向哨兵：夹具路径必须真的存在（否则上面的「一致」是两边都为空的假绿）。
	if before[sessionFile] == "" || before[toolSessionFile] == "" || before[modelsLive] == "" {
		t.Fatalf("夹具文件缺失，哈希断言退化成空对空: %v", before)
	}
}

// TestProfileTestCostFaceReportsUnknownNotZero 守 A8（C-2 additive，2026-09-27）：
// 「未获得实际费用时给未知而非 0」——金额恒 null（绝不写 0 冒充免费），未知项显式列出，
// 费用材料（目标模型登记单价）可读时如实给出其来源。
func TestProfileTestCostFaceReportsUnknownNotZero(t *testing.T) {
	srv, _ := testableServer(t) // 桩回 pi 事件流：model=deepseek-flash

	containsUnknown := func(list []string, want string) bool {
		for _, item := range list {
			if item == want {
				return true
			}
		}
		return false
	}

	// ① 定价表为空：来源行说明「为什么未知」、单价未知、实际费用未知（金额 null）。
	payload := testPayloadOf(t, srv, "pi", managedCardID)
	if payload.Cost.Quote != profileTestCostQuoteUnknown {
		t.Errorf("quote = %q（期望 unknown）", payload.Cost.Quote)
	}
	if payload.Cost.Source == "" {
		t.Errorf("source 为空（费用来源/为什么未知必须可读）")
	}
	if payload.Cost.CostUSD != nil {
		t.Errorf("未获得实际费用时金额必须为 null（写 0 会被读成免费）: %v", *payload.Cost.CostUSD)
	}
	if payload.Cost.UnitPrice != nil || payload.Cost.Currency != "" {
		t.Errorf("单价未登记时不得伪造单价/币种: %+v", payload.Cost)
	}
	for _, want := range []string{
		profileTestCostUnknownActualCost, profileTestCostUnknownActualTokens, profileTestCostUnknownUnitPrice,
	} {
		if !containsUnknown(payload.Cost.Unknowns, want) {
			t.Errorf("未知项清单缺 %q: %v", want, payload.Cost.Unknowns)
		}
	}

	// ② 登记目标模型单价：单价/币种可读、来源行更新；实际费用**仍**未知（金额 null）。
	save := srv.callRaw(t, http.MethodPut, "/api/v1/usage/pricing",
		`{"entries":[{"model":"deepseek-flash","inputPerMillion":0.5,"outputPerMillion":1.5,"currency":"CNY"}]}`, true)
	if !save.ok {
		t.Fatalf("登记定价失败: %s", save.body)
	}
	payload = testPayloadOf(t, srv, "pi", managedCardID)
	if payload.Cost.UnitPrice == nil || payload.Cost.Currency != "CNY" {
		t.Fatalf("单价已登记但单价/币种缺席: %+v", payload.Cost)
	}
	if payload.Cost.UnitPrice.InputPerMillion != 0.5 || payload.Cost.UnitPrice.OutputPerMillion != 1.5 ||
		payload.Cost.UnitPrice.Currency != "CNY" {
		t.Errorf("单价字段 = %+v（期望 0.5/1.5/CNY）", payload.Cost.UnitPrice)
	}
	if !strings.Contains(payload.Cost.Source, "单价已登记") {
		t.Errorf("来源行没有说明单价已登记: %q", payload.Cost.Source)
	}
	if payload.Cost.CostUSD != nil {
		t.Errorf("登记单价不等于获得实际费用：金额仍必须为 null")
	}
	if containsUnknown(payload.Cost.Unknowns, profileTestCostUnknownUnitPrice) {
		t.Errorf("单价已可读但仍列为未知项: %v", payload.Cost.Unknowns)
	}
	if !containsUnknown(payload.Cost.Unknowns, profileTestCostUnknownActualCost) {
		t.Errorf("实际费用仍未知但不在未知项清单: %v", payload.Cost.Unknowns)
	}

	// ③ 单价非正按「未知」而不是「免费」（与 §4.8 runtimeVerifyQuote 同口径）。
	save = srv.callRaw(t, http.MethodPut, "/api/v1/usage/pricing",
		`{"entries":[{"model":"deepseek-flash","inputPerMillion":0,"outputPerMillion":0,"currency":"CNY"}]}`, true)
	if !save.ok {
		t.Fatalf("登记零价失败: %s", save.body)
	}
	payload = testPayloadOf(t, srv, "pi", managedCardID)
	if payload.Cost.UnitPrice != nil || payload.Cost.Currency != "" {
		t.Errorf("非正单价被当成可读材料（0 会被读成免费）: %+v", payload.Cost)
	}
	if !containsUnknown(payload.Cost.Unknowns, profileTestCostUnknownUnitPrice) {
		t.Errorf("非正单价应回到未知项清单: %v", payload.Cost.Unknowns)
	}
}

// 非零退出与超时都走**成功信封 + status:failed**，且 reason/output 说清发生了什么；
// 两种情形都清理临时根。
func TestProfileTestFailurePathsAreReportedFaithfully(t *testing.T) {
	t.Run("非零退出", func(t *testing.T) {
		srv, fake := testableServer(t)
		var root string
		fake.respond = func(req profileTestExecRequest) profileTestExecResult {
			root = req.Root
			return profileTestExecResult{Stderr: "Error: 401 Invalid token\n", ExitCode: 1,
				Err: errors.New("exit status 1")}
		}
		payload := testPayloadOf(t, srv, "pi", managedCardID)
		if payload.Status != profileTestStatusFailed || payload.ExitCode != 1 || payload.TimedOut {
			t.Fatalf("结果的形状 = %+v（期望 failed/1/false）", payload)
		}
		if !strings.Contains(payload.Reason, "1") {
			t.Errorf("reason 没说清退出码: %q", payload.Reason)
		}
		if strings.Contains(payload.Output, "401") || strings.Contains(payload.Output, "Invalid token") {
			t.Errorf("legacy profile-test output must not expose raw provider diagnostics: %q", payload.Output)
		}
		if _, err := os.Stat(root); !os.IsNotExist(err) {
			t.Errorf("失败路径没有清理临时根: %v", err)
		}
	})

	t.Run("超时", func(t *testing.T) {
		srv, fake := testableServer(t)
		var root string
		fake.respond = func(req profileTestExecRequest) profileTestExecResult {
			root = req.Root
			return profileTestExecResult{ExitCode: -1, TimedOut: true, Stderr: "still running"}
		}
		payload := testPayloadOf(t, srv, "pi", managedCardID)
		if payload.Status != profileTestStatusFailed || !payload.TimedOut || payload.ExitCode != -1 {
			t.Fatalf("结果的形状 = %+v（期望 failed/true/-1）", payload)
		}
		if !strings.Contains(payload.Reason, "超时") {
			t.Errorf("reason 没有点明超时: %q", payload.Reason)
		}
		if _, err := os.Stat(root); !os.IsNotExist(err) {
			t.Errorf("超时路径没有清理临时根: %v", err)
		}
	})
}

// TestProfileTestSecondCallOnSameToolGetsTestInProgress 守住并发闸门：
// 同一工具上第二次请求得 test_in_progress（409），**另一个工具**照常能跑。
func TestProfileTestSecondCallOnSameToolGetsTestInProgress(t *testing.T) {
	srv, fake := testableServer(t)
	// 让 pi 的那次执行停在闸门上，制造「正在跑」的窗口；`close(release)` 之后同一桩
	// 不再阻塞，因此释放后的第三次调用无需换桩（避免跨 goroutine 改字段造成竞态）。
	release := make(chan struct{})
	fake.beforeReturn = func(req profileTestExecRequest) {
		if strings.HasSuffix(req.Exe, "pi") {
			<-release
		}
	}

	done := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		// 刻意不走 callRaw：那个帮助函数在解析失败时会 t.Fatalf，而**非测试 goroutine
		// 里的 Fatalf 只会结束该 goroutine**（本测试随后会卡在 <-done 上）。
		req := httptest.NewRequest(http.MethodPost,
			"/api/v1/tools/pi/profiles/"+managedCardID+"/test", nil)
		req.Header.Set("X-Agmgr-Token", srv.token)
		rec := httptest.NewRecorder()
		srv.ServeHTTP(rec, req)
		done <- rec
	}()
	// 等到确实进了执行面（否则第二次请求可能先到而测不到闸门）。
	deadline := time.Now().Add(5 * time.Second)
	for len(fake.recorded()) == 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if len(fake.recorded()) == 0 {
		close(release)
		t.Fatal("第一次测试没有进入执行面（夹具没起来）")
	}

	second := srv.callRaw(t, http.MethodPost,
		"/api/v1/tools/pi/profiles/"+managedCardID+"/test", "", true)
	if second.ok || second.code != "test_in_progress" {
		close(release)
		t.Fatalf("并发第二次的响应 = ok:%v code:%q（期望 test_in_progress）: %s",
			second.ok, second.code, second.body)
	}
	if second.status != http.StatusConflict {
		t.Errorf("test_in_progress 的 HTTP 状态 = %d（期望 409）", second.status)
	}
	// 另一个工具不受影响（闸门按工具隔离）。
	other := srv.callRaw(t, http.MethodPost,
		"/api/v1/tools/opencode/profiles/"+managedCardID+"/test", "", true)
	if !other.ok {
		close(release)
		t.Fatalf("另一个工具的测试被闸门误伤: %s", other.body)
	}

	close(release)
	select {
	case rec := <-done:
		if rec.Code != http.StatusOK {
			t.Errorf("第一次测试的状态码 = %d（期望 200）: %s", rec.Code, rec.Body.String())
		}
	case <-time.After(10 * time.Second):
		t.Fatal("第一次测试在释放后没有返回（闸门或清理卡住了）")
	}
	// 释放后同一工具可以再跑（闸门必须成对释放）。
	again := srv.callRaw(t, http.MethodPost,
		"/api/v1/tools/pi/profiles/"+managedCardID+"/test", "", true)
	if !again.ok {
		t.Fatalf("释放后同一工具仍被拒: %s", again.body)
	}
}

// TestProfileTestOutputIsBoundedAndMasked 守住输出摘要的两条硬约束：
// ≤400 字、且**先脱敏后截断**（令牌不得以半截串形态漏出去）。
func TestProfileTestOutputIsBoundedAndMasked(t *testing.T) {
	const token = "sk-ant-api03-CLIOUTPUTKEY-0123456789abcdef"
	srv, fake := testableServer(t)
	fake.respond = func(profileTestExecRequest) profileTestExecResult {
		// 长输出 + 一枚令牌：令牌之后还有大量内容，确保「截断」与「掩码」都发生。
		return profileTestExecResult{
			Stdout: "token=" + token + "\n" + strings.Repeat("x", 5000) + "\n",
		}
	}
	payload := testPayloadOf(t, srv, "pi", managedCardID)
	if n := len([]rune(payload.Output)); n > profileTestOutputLimit {
		t.Errorf("output 长度 = %d（上限 %d）", n, profileTestOutputLimit)
	}
	if strings.Contains(payload.Output, token) {
		t.Errorf("output 带出了令牌明文")
	}
	if match := secretShape.FindString(payload.Output); match != "" {
		t.Errorf("output 里出现令牌形态 %q（先截断后脱敏会漏半截串）", match)
	}
	if !strings.Contains(payload.Output, "已脱敏") {
		t.Errorf("output 没有掩码标记: %q", payload.Output)
	}
}

// TestResolveProfileTestCLI 直接钉解析面：不存在的命令名得到空串（调用方据此 not_supported），
// 系统命令能得到真实路径（PATH 一步就命中）。
func TestResolveProfileTestCLI(t *testing.T) {
	srv := newContractServer(t)
	if got := srv.resolveProfileTestCLI(family.Pi, "yzagmgr-no-such-cli-4f2a"); got != "" {
		t.Errorf("不存在的命令解析出 %q（期望空串）", got)
	}
	if got := srv.resolveProfileTestCLI(family.Claude, "sh"); got == "" {
		t.Errorf("系统命令 sh 没解析出路径（PATH 那一步失效了？）")
	}
}

// TestProfileTestCodexCommandSkipsGitRepoCheck 钉 codex 配方的命令行口径（回归：发布门禁
// 唯一红项，reports/evidence-releasegate-20260928/codex-profiletest*.json）：
//
//   - codex：`exec` 之后带 `--skip-git-repo-check`，最小请求仍在末位——测试的 cmd.Dir 是
//     临时根（非 git 仓库）+ 隔离 CODEX_HOME 无信任条目，codex 0.156 的信任门会在**发请求前**
//     以「Not inside a trusted directory and --skip-git-repo-check was not specified」拒绝；
//     产品自家 verify 脚本（scripts/verify/cli-roundtrip.sh）同口径。
//   - 其余三族（claude/opencode/pi）命令行**不含**该旗标（零回归：它们不是 codex，不猜旗标）。
func TestProfileTestCodexCommandSkipsGitRepoCheck(t *testing.T) {
	const flag = "--skip-git-repo-check"
	cases := []struct {
		name     family.ID
		wantSkip bool
	}{
		{family.Codex, true},
		{family.Claude, false},
		{family.OpenCode, false},
		{family.Pi, false},
	}
	// 支持集恰四条（配方集是封闭的）：多出的族要么补进表、要么在此显式登记。
	if len(profileTestRecipes) != len(cases) {
		t.Fatalf("profileTestRecipes 条数 = %d，本表登记 %d 条（配方集变了要同步本表）",
			len(profileTestRecipes), len(cases))
	}
	for _, tc := range cases {
		t.Run(string(tc.name), func(t *testing.T) {
			recipe, ok := profileTestRecipes[tc.name]
			if !ok {
				t.Fatalf("配方集里没有 %s", tc.name)
			}
			joined := strings.Join(recipe.args, " ")
			gotSkip := strings.Contains(joined, flag)
			if gotSkip != tc.wantSkip {
				t.Fatalf("%s 的 args = %q：含 %s = %v，期望 %v",
					tc.name, joined, flag, gotSkip, tc.wantSkip)
			}
			if !tc.wantSkip {
				return
			}
			// codex 侧再钉形状：子命令在首位、旗标紧随、最小请求仍在末位（prompt 不被旗标顶掉）。
			if len(recipe.args) < 3 || recipe.args[0] != "exec" ||
				recipe.args[1] != flag || recipe.args[len(recipe.args)-1] != profileTestPrompt {
				t.Errorf("codex args = %#v，期望 [exec %s <prompt>]", recipe.args, flag)
			}
		})
	}
}

// TestProfileTestCodexArgsRouteGatewayCardViaLocalProxy 钉 codex 配方的运行时参数补充
// （与物化启动命令 codexLaunchCommand 同源，见 codexProxyBase）：
//
//   - 网关档（计划内容含 model_provider = "custom"）⇒ 命令行插入
//     `-c model_providers.custom.base_url=<本实例本地代理>/openai/v1`，插在
//     `--skip-git-repo-check` 之后、最小请求 prompt 之前——直连必 401（隔离测试根按
//     UC-019 没有凭据副本），与 launchCommand「覆盖到本地代理」是同一口径；
//   - 官方卡（无 custom 段）⇒ 保留本地执行安全覆盖但不插代理 base_url；
//   - 其余三族配方没有 extraArgs ⇒ 命令行逐字节原样（零回归）。
func TestProfileTestCodexArgsRouteGatewayCardViaLocalProxy(t *testing.T) {
	srv := newContractServer(t)
	gatewayCard := map[string][]byte{
		"config.toml": []byte("model_provider = \"custom\"\n[model_providers.custom]\n" +
			"base_url = \"https://example.invalid/openai/v1\"\n"),
	}
	officialCard := map[string][]byte{"config.toml": []byte("model = \"gpt-5\"\n")}

	codexRecipe := profileTestRecipes[family.Codex]
	joined := func(args []string) string { return strings.Join(args, " ") }

	// 网关档：-c 覆盖在场、值 = 本实例本地代理 + OpenAI 车道前缀，且顺序正确。
	got := srv.profileTestArgs(codexRecipe, gatewayCard)
	wantBase := srv.localRouteBase() + "/openai/v1"
	wantOverride := "-c model_providers." + codexProviderCustom + ".base_url=" + wantBase
	if !strings.Contains(joined(got), wantOverride) {
		t.Errorf("网关档 codex args = %q，期望含 %q", joined(got), wantOverride)
	}
	if !strings.Contains(joined(got), "--skip-git-repo-check") {
		t.Errorf("网关档 codex args = %q，期望仍含 --skip-git-repo-check", joined(got))
	}
	for _, setting := range []string{
		"analytics.enabled=false",
		"check_for_update_on_startup=false",
		"cli_auth_credentials_store=ephemeral",
	} {
		if !strings.Contains(joined(got), "-c "+setting) {
			t.Errorf("网关档 codex args 缺少隔离设置 %q: %q", setting, joined(got))
		}
	}
	iFlag := indexOf(got, "--skip-git-repo-check")
	iC := indexOf(got, "-c")
	if iFlag < 0 || iC < 0 || !(iFlag < iC && iC < len(got)-1) {
		t.Errorf("网关档 codex args = %#v，期望顺序 [exec --skip-git-repo-check -c k=v <prompt>]", got)
	}
	if got[len(got)-1] != profileTestPrompt {
		t.Errorf("网关档 codex args 末位 = %q，最小请求必须恒在末位", got[len(got)-1])
	}

	// 官方卡：保留必要的本地安全设置，但不覆盖用户官方配置的 base_url。
	officialArgs := srv.profileTestArgs(codexRecipe, officialCard)
	wantOfficialArgs := append([]string(nil), codexRecipe.args[:len(codexRecipe.args)-1]...)
	wantOfficialArgs = append(wantOfficialArgs,
		"-c", "analytics.enabled=false",
		"-c", "check_for_update_on_startup=false",
		"-c", "cli_auth_credentials_store=ephemeral",
		codexRecipe.args[len(codexRecipe.args)-1],
	)
	if strings.Join(officialArgs, "\x00") != strings.Join(wantOfficialArgs, "\x00") {
		t.Errorf("官方卡 codex args = %#v，期望 %#v", officialArgs, wantOfficialArgs)
	}
	if strings.Contains(joined(officialArgs), "model_providers."+codexProviderCustom+".base_url=") {
		t.Errorf("官方卡不得覆盖 base_url: %q", joined(officialArgs))
	}
	// A recipe callback may legitimately have no runtime override; the static recipe remains intact.
	emptyExtraRecipe := codexRecipe
	emptyExtraRecipe.extraArgs = func(*Server, map[string][]byte) []string { return nil }
	if got := joined(srv.profileTestArgs(emptyExtraRecipe, officialCard)); got != joined(codexRecipe.args) {
		t.Errorf("empty runtime override should preserve static recipe: got=%q want=%q", got, joined(codexRecipe.args))
	}

	// 其余三族：没有 extraArgs，逐字节原样（含「不含 -c 旗标」的零回归）。
	for _, id := range []family.ID{family.Claude, family.OpenCode, family.Pi} {
		recipe := profileTestRecipes[id]
		got := srv.profileTestArgs(recipe, gatewayCard) // 内容里带 custom 段也不该影响它们
		if strings.Contains(joined(got), "-c ") || strings.Contains(joined(got), "--skip-git-repo-check") {
			t.Errorf("%s args = %q，不应含 codex 专属旗标", id, joined(got))
		}
		if strings.Join(got, "\x00") != strings.Join(recipe.args, "\x00") {
			t.Errorf("%s args = %#v，期望原样 %#v", id, got, recipe.args)
		}
	}
}

// indexOf 返回元素首次出现的下标（-1 = 缺席）。
func indexOf(args []string, want string) int {
	for i, a := range args {
		if a == want {
			return i
		}
	}
	return -1
}

// TestParseModelFromCLIOutput 钉 pi 事件流的解析口径（含回落与「不编」）。
func TestParseModelFromCLIOutput(t *testing.T) {
	cases := []struct {
		name         string
		stdout       string
		wantModel    string
		wantProvider string
	}{
		{
			name:         "pi：末条 assistant message_end 为准",
			stdout:       piEventStream("glm-5.3-flash", "yzagmgr-5", "正常"),
			wantModel:    "glm-5.3-flash",
			wantProvider: "yzagmgr-5",
		},
		{
			name: "pi：多条 assistant 消息取最后一条（重试/多轮）",
			stdout: piEventStream("first", "p1", "a") +
				piEventStream("second", "p2", "b"),
			wantModel:    "second",
			wantProvider: "p2",
		},
		{
			name:         "model 缺席时回落 responseModel",
			stdout:       `{"type":"message_end","message":{"role":"assistant","responseModel":"resp-model"}}`,
			wantModel:    "resp-model",
			wantProvider: "",
		},
		{
			name:      "非 JSON 输出不猜（留空）",
			stdout:    "all good, model is deepseek-flash\n",
			wantModel: "",
		},
		{
			name:      "工具角色消息不算（只有 assistant 才报模型）",
			stdout:    `{"type":"message_end","message":{"role":"toolResult","model":"nope"}}`,
			wantModel: "",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			model, provider := parseModelFromCLIOutput(tc.stdout)
			if model != tc.wantModel || provider != tc.wantProvider {
				t.Errorf("解析 = %q/%q，期望 %q/%q", model, provider, tc.wantModel, tc.wantProvider)
			}
		})
	}
}

// TestCappedStreamKeepsWholeLines 钉有界捕获：写满上限后丢后续，且截断点回退到整行
// （半行既解析不出 JSON，也可能把令牌截成不再匹配形态的半截串）。
func TestCappedStreamKeepsWholeLines(t *testing.T) {
	stream := &cappedStream{limit: 10}
	_, _ = stream.Write([]byte("aaaa\n"))
	_, _ = stream.Write([]byte("bbbbbbbb\n")) // 只装得下前 5 字节：这一行被切断
	if got := stream.String(); got != "aaaa\n" {
		t.Errorf("截断结果 = %q（期望只剩第一整行，半行要被丢掉）", got)
	}
	// 上限刚好落在行尾时不丢最后一整行（丢的是「半行」，不是「最后一行」）。
	aligned := &cappedStream{limit: 10}
	_, _ = aligned.Write([]byte("aaaa\n"))
	_, _ = aligned.Write([]byte("bbbb\ncccc\n"))
	if got := aligned.String(); got != "aaaa\nbbbb\n" {
		t.Errorf("行对齐的截断结果 = %q（期望保留两整行）", got)
	}

	// 一整行就超过上限：宁可什么都不留。
	single := &cappedStream{limit: 3}
	_, _ = single.Write([]byte("abcdefgh"))
	if got := single.String(); got != "" {
		t.Errorf("超长单行的结果 = %q（期望空串）", got)
	}
}

// TestTempTreeNamesRejectsPathsOutsideHome 钉「拍平即失效」的判据：受管路径不在 $HOME 下时
// 明确报错（换个 HOME 后 CLI 按固定相对位置找配置，猜一个位置会让它读到空配置）。
func TestTempTreeNamesRejectsPathsOutsideHome(t *testing.T) {
	home := t.TempDir()
	outside := t.TempDir()
	names, err := tempTreeNames(home, []family.FilePlan{
		{Path: filepath.Join(home, ".pi", "agent", "models.json")},
	})
	if err != nil || names[filepath.Join(home, ".pi", "agent", "models.json")] != ".pi/agent/models.json" {
		t.Fatalf("HOME 下的路径没有按相对结构命名: %v / %v", names, err)
	}
	if _, err := tempTreeNames(home, []family.FilePlan{
		{Path: filepath.Join(outside, "config.toml")},
	}); err == nil {
		t.Errorf("HOME 之外的路径没有被拒（会静默拍平成一个 CLI 读不到的位置）")
	}
}

// TestProfileTestSweepsStaleRootsFromCrashedRuns 守住崩溃残留的清理：
// 临时根里放着带真凭据的配置副本，进程被强杀时 defer 不会执行，因此下一次测试要顺带收掉
// **够老**的那些；正在跑的那份（刚建的）与别人的目录（前缀不符）一个都不能碰。
//
// C-2 补断言（sweeper 面）：受控验证的临时根前缀（§4.8 的 `yzagmgr-runtime-verify-`）
// 共用同一套清理纪律——老的要收、新的要留，两个前缀各验一遍。
func TestProfileTestSweepsStaleRootsFromCrashedRuns(t *testing.T) {
	base := os.TempDir()
	mk := func(pattern string) string {
		dir, err := os.MkdirTemp(base, pattern)
		if err != nil {
			t.Fatalf("建目录失败: %v", err)
		}
		t.Cleanup(func() { _ = os.RemoveAll(dir) })
		return dir
	}
	old := time.Now().Add(-3 * time.Hour)
	age := func(dir string) {
		if err := os.Chtimes(dir, old, old); err != nil {
			t.Fatalf("改 mtime 失败: %v", err)
		}
	}
	stale := mk(profileTestDirPrefix)
	age(stale)
	staleVerify := mk(runtimeVerifyDirPrefix)
	age(staleVerify)
	fresh := mk(profileTestDirPrefix)
	freshVerify := mk(runtimeVerifyDirPrefix)
	foreign := mk("yzagmgr-not-ours-")

	srv, _ := testableServer(t)
	if got := testPayloadOf(t, srv, "pi", managedCardID); got.Status != profileTestStatusOK {
		t.Fatalf("测试没有跑成: %+v", got)
	}

	for name, dir := range map[string]string{
		"配置测试残留": stale, "受控验证残留": staleVerify,
	} {
		if _, err := os.Stat(dir); !os.IsNotExist(err) {
			t.Errorf("%s 没有被清理: %v", name, err)
		}
	}
	for name, dir := range map[string]string{
		"刚建的配置测试根": fresh, "刚建的受控验证根": freshVerify,
	} {
		if _, err := os.Stat(dir); err != nil {
			t.Errorf("%s 被误删（年龄判据失效）: %v", name, err)
		}
	}
	if _, err := os.Stat(foreign); err != nil {
		t.Errorf("不是本功能前缀的目录被误删: %v", err)
	}
}

// ── 小工具 ──────────────────────────────────────────────────────────────────

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// preflightDetails 解出失败信封的 error.details（A6 前置分类断言用）。
func preflightDetails(t *testing.T, res envelope) struct {
	Reason string `json:"reason"`
	Path   string `json:"path"`
} {
	t.Helper()
	var parsed struct {
		Reason string `json:"reason"`
		Path   string `json:"path"`
	}
	if len(res.details) == 0 {
		t.Fatalf("失败信封没有 details（前置分类必须机器可读）: %s", res.body)
	}
	if err := json.Unmarshal(res.details, &parsed); err != nil {
		t.Fatalf("details 不是预期结构: %v（%s）", err, string(res.details))
	}
	return parsed
}

// reasonOf 解出失败信封的 error.details.reason。
func reasonOf(t *testing.T, res envelope) string {
	t.Helper()
	return preflightDetails(t, res).Reason
}

// dirEntryNames 列出目录条目名（排序；不存在 = 空）。
func dirEntryNames(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		t.Fatalf("读取目录 %s 失败: %v", dir, err)
	}
	out := make([]string, 0, len(entries))
	for _, e := range entries {
		out = append(out, e.Name())
	}
	return out
}
