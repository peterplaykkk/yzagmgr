// §4.6 停止入口（`POST /tools/{id}/profiles/{pid}/test/cancel`，UC-097 A4）的断言。
//
// 四条纪律的测试面（与 doc/api-v1.md §4.6「停止入口」段逐条对应）：
//  1. **幂等**：没有在飞测试（含连续第二次停止）⇒ 200 空操作（`stopped:false`），不报错；
//  2. **竞态语义明确**：终态判定与停止请求在同一把锁里发生——停止先于终态落定 ⇒ 终态
//     cancelled（停止优先于同时到达的超时/退出）；落定先发生 ⇒ 空操作、原结果不动；
//     **客户端断开不冒充用户停止**（不置停止位，仍走 failed）；
//  3. **只停配置测试**：在飞的是受控验证（§4.8 共用同一把闸门）⇒ 如实报告 kind=verify，
//     不假装停过、也不把它折成错误；
//  4. **归属**：路径 pid 与在跑运行的发起卡不符 ⇒ 不停止任何东西、如实说明在跑的是谁。
//
// 真实子进程的进程组回收（「停止后无孤儿进程」）另见 profiletest_cancel_unix_test.go
// （桩 CLI 子进程）；本文件只跑注入的桩，不 spawn 任何进程。
package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/peterplaykkk/yzagmgr/internal/family"
)

// cancelData 是停止入口的响应载荷（形状与 doc/api-v1.md §4.6 逐字对应）。
type cancelData struct {
	ToolID  string              `json:"toolId"`
	Active  bool                `json:"active"`
	Stopped bool                `json:"stopped"`
	Kind    string              `json:"kind"`
	Result  *profileTestPayload `json:"result"`
	Reason  string              `json:"reason"`
}

// cancelTestRun 发一次停止请求并解出载荷（要求成功信封；pid 默认受管卡）。
func cancelTestRun(t *testing.T, srv *contractServer, tool string) (envelope, cancelData) {
	t.Helper()
	return cancelTestRunAs(t, srv, tool, managedCardID)
}

// cancelTestRunAs 同上，但指定路径里的配置卡 id（归属断言用）。
func cancelTestRunAs(t *testing.T, srv *contractServer, tool, pid string) (envelope, cancelData) {
	t.Helper()
	res := srv.callRaw(t, http.MethodPost,
		"/api/v1/tools/"+tool+"/profiles/"+pid+"/test/cancel", "", true)
	if !res.ok {
		t.Fatalf("停止端点失败: %s", res.body)
	}
	var data cancelData
	if err := json.Unmarshal(res.data, &data); err != nil {
		t.Fatalf("停止载荷不是预期结构: %v（%s）", err, string(res.data))
	}
	return res, data
}

// waitFor 轮询等待条件成立（上限 10s——门禁机器上可能同时跑着别的负载，
// 短上限会把「慢」误报成「坏」；超时才 t.Fatal）。
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("等待「%s」超时", what)
}

// cancelRequestedOf 读某工具在飞运行的停止请求位（白盒：断言竞态窗口里的顺序）。
func cancelRequestedOf(srv *contractServer, id family.ID) bool {
	srv.profileTestMu.Lock()
	defer srv.profileTestMu.Unlock()
	run := srv.profileTestRuns[id]
	return run != nil && run.cancelReq
}

// TestProfileTestCancelWithNoRunIsIdempotentNoop 守住幂等与前置：
//   - 没有在飞测试 ⇒ 200 空操作（active/cancelled 皆 false，result null），重复无副作用；
//   - 不支持的家族/未知工具 ⇒ 与测试入口同一道前置（没有可停止的测试，如实回答 not_supported）。
func TestProfileTestCancelWithNoRunIsIdempotentNoop(t *testing.T) {
	srv, fake := testableServer(t)

	for i := 0; i < 2; i++ { // 连续两次：第二次仍是同一答案（幂等）
		_, data := cancelTestRun(t, srv, "pi")
		if data.ToolID != "pi" || data.Active || data.Stopped || data.Kind != "" || data.Result != nil {
			t.Fatalf("空操作载荷 = %+v（第 %d 次）", data, i+1)
		}
		if !strings.Contains(data.Reason, "没有在跑的配置测试") {
			t.Errorf("空操作理由未说清: %q", data.Reason)
		}
	}
	if calls := fake.recorded(); len(calls) != 0 {
		t.Fatalf("空操作起了进程: %+v", calls)
	}

	// 不支持配方家族：没有配方 = 不可能有过测试在跑，按 not_supported 如实回答。
	for _, tool := range []string{"gemini", "claude-desktop", "workbuddy"} {
		res := srv.callRaw(t, http.MethodPost,
			"/api/v1/tools/"+tool+"/profiles/"+managedCardID+"/test/cancel", "", true)
		if res.ok || res.code != "not_supported" {
			t.Errorf("%s 的停止响应 = ok:%v code:%q（期望 not_supported）", tool, res.ok, res.code)
		}
	}
	// 未注册工具 id。
	res := srv.callRaw(t, http.MethodPost, "/api/v1/tools/nope/profiles/"+managedCardID+"/test/cancel", "", true)
	if res.ok || res.code != "not_supported" {
		t.Errorf("未知工具 = ok:%v code:%q（期望 not_supported）", res.ok, res.code)
	}
}

// TestProfileTestCancelStopsInFlightRunAndReportsTerminalPayload 守主路径：
// 停止请求真实终止在飞测试（经 ctx 取消通道），终态如实标 cancelled（不谎称成功），
// 原测试请求与停止响应拿到**同一份**终态快照；随后重复停止是空操作。
func TestProfileTestCancelStopsInFlightRunAndReportsTerminalPayload(t *testing.T) {
	srv, _ := testableServer(t)
	entered := make(chan struct{})
	srv.profileTestExecute = func(ctx context.Context, _ profileTestExecRequest) profileTestExecResult {
		close(entered)
		<-ctx.Done() // 模拟「进程被取消后退出」：真实路径是 TERM→KILL 进程组
		return profileTestExecResult{ExitCode: -1, Err: ctx.Err(), Stderr: "stopped\n"}
	}

	done := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		req := httptest.NewRequest(http.MethodPost,
			"/api/v1/tools/pi/profiles/"+managedCardID+"/test", nil)
		req.Header.Set("X-Agmgr-Token", srv.token)
		rec := httptest.NewRecorder()
		srv.ServeHTTP(rec, req)
		done <- rec
	}()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("测试没有进入执行面（夹具没起来）")
	}

	_, data := cancelTestRun(t, srv, "pi")
	if !data.Active || !data.Stopped || data.Kind != controlledRunTest {
		t.Fatalf("停止载荷 = %+v（期望 active/cancelled/test）", data)
	}
	if data.Result == nil || data.Result.Status != profileTestStatusCancelled || !data.Result.Cancelled {
		t.Fatalf("终态载荷 = %+v（期望 status=cancelled + cancelled=true）", data.Result)
	}
	// 终态是 cancelled 而不是成功/回滚：文案须说「已按停止要求终止」，不得出现
	// 「正常返回」这类成功陈述。
	if !strings.Contains(data.Result.Reason, "停止") ||
		strings.Contains(data.Result.Reason, "正常返回") || strings.Contains(data.Result.Reason, "回滚") {
		t.Errorf("停止的终态文案不得谎称成功/回滚: %q", data.Result.Reason)
	}

	select {
	case rec := <-done:
		if rec.Code != http.StatusOK {
			t.Fatalf("原测试请求状态码 = %d（期望 200）: %s", rec.Code, rec.Body.String())
		}
		var parsed struct {
			OK   bool                `json:"ok"`
			Data *profileTestPayload `json:"data"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &parsed); err != nil || !parsed.OK || parsed.Data == nil {
			t.Fatalf("原测试请求信封不可解析: %v（%s）", err, rec.Body.String())
		}
		if parsed.Data.Status != profileTestStatusCancelled || !parsed.Data.Cancelled {
			t.Fatalf("原测试请求终态 = %+v（期望 cancelled）", parsed.Data)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("停止后原测试请求没有返回")
	}

	// 重复停止：运行已结束 ⇒ 空操作（幂等），且没有在飞运行时不再起任何东西。
	_, again := cancelTestRun(t, srv, "pi")
	if again.Active || again.Stopped {
		t.Fatalf("重复停止 = %+v（期望空操作）", again)
	}

	// 操作日志：停止入口属变更类（profile.test.cancel），成功的停止记 ok；
	// 被停止的测试本身沿用「非 ok 即 failed」的既有封闭口径如实记 failed。
	res := srv.callRaw(t, http.MethodGet, "/api/v1/operations?action=profile.test.cancel&limit=50", "", true)
	if !res.ok {
		t.Fatalf("读取操作日志失败: %s", res.body)
	}
	var page struct {
		Items []struct {
			Action string `json:"action"`
			Target string `json:"target"`
			Result string `json:"result"`
		} `json:"items"`
	}
	if err := json.Unmarshal(res.data, &page); err != nil {
		t.Fatalf("操作日志页不是预期结构: %v（%s）", err, string(res.data))
	}
	cancelLogged := false
	for _, item := range page.Items {
		if item.Action == "profile.test.cancel" && item.Target == "pi" {
			cancelLogged = true
			if item.Result != "ok" {
				t.Errorf("停止入口的操作日志 = %+v（期望 ok）", item)
			}
		}
	}
	if !cancelLogged {
		t.Fatalf("操作日志里没有本次停止的条目（共 %d 条）", len(page.Items))
	}
	testRes := srv.callRaw(t, http.MethodGet, "/api/v1/operations?action=profile.test&limit=50", "", true)
	if !testRes.ok {
		t.Fatalf("读取测试操作日志失败: %s", testRes.body)
	}
	if err := json.Unmarshal(testRes.data, &page); err != nil {
		t.Fatalf("操作日志页不是预期结构: %v", err)
	}
	stoppedLogged := false
	for _, item := range page.Items {
		if item.Action == "profile.test" && item.Target == "pi" {
			stoppedLogged = true
			if item.Result != "failed" {
				t.Errorf("被停止的测试在操作日志应记 failed（封闭结果集只有 ok/failed）: %+v", item)
			}
		}
	}
	if !stoppedLogged {
		t.Fatalf("操作日志里没有被停止测试的条目")
	}
}

// TestProfileTestCancelRacesWithCompletionAndTimeout 守竞态语义（逐条钉住）：
//   - 自然完成先落定 ⇒ 停止是空操作、原结果（ok）不动；
//   - 停止请求先于终态落定 ⇒ 停止优先（即便执行面同时报超时也不折成 failed/超时）；
//   - 客户端断开不置停止位 ⇒ 仍走 failed，不冒充「用户点了停止」。
func TestProfileTestCancelRacesWithCompletionAndTimeout(t *testing.T) {
	t.Run("自然完成先落定：停止是空操作", func(t *testing.T) {
		srv, _ := testableServer(t)
		payload := testPayloadOf(t, srv, "pi", managedCardID)
		if payload.Status != profileTestStatusOK {
			t.Fatalf("前置测试未成功: %+v", payload)
		}
		_, data := cancelTestRun(t, srv, "pi")
		if data.Active || data.Stopped {
			t.Fatalf("已结束的运行仍被当作在飞: %+v", data)
		}
		// 原结果不被改写：再跑一次仍是 ok（覆盖同工具连跑，闸门成对释放）。
		if p := testPayloadOf(t, srv, "pi", managedCardID); p.Status != profileTestStatusOK {
			t.Fatalf("空操作影响了后续测试: %+v", p)
		}
	})

	t.Run("停止先落定：停止优先于同时到达的超时", func(t *testing.T) {
		srv, _ := testableServer(t)
		entered := make(chan struct{})
		release := make(chan struct{})
		srv.profileTestExecute = func(context.Context, profileTestExecRequest) profileTestExecResult {
			close(entered)
			<-release // 模拟「CLI 卡住直到测试放行」；不放行时连超时都要等它
			return profileTestExecResult{ExitCode: -1, TimedOut: true, Stderr: "timeout\n"}
		}
		done := make(chan *httptest.ResponseRecorder, 1)
		go func() {
			req := httptest.NewRequest(http.MethodPost,
				"/api/v1/tools/pi/profiles/"+managedCardID+"/test", nil)
			req.Header.Set("X-Agmgr-Token", srv.token)
			rec := httptest.NewRecorder()
			srv.ServeHTTP(rec, req)
			done <- rec
		}()
		select {
		case <-entered:
		case <-time.After(5 * time.Second):
			t.Fatal("测试没有进入执行面")
		}

		cancelDone := make(chan cancelData, 1)
		go func() {
			_, data := cancelTestRun(t, srv, "pi")
			cancelDone <- data
		}()
		// 等停止请求确实先打上标记（白盒），再放行执行面——构造「停止先于终态落定」。
		waitFor(t, "停止请求位落定", func() bool { return cancelRequestedOf(srv, family.Pi) })
		close(release)

		select {
		case data := <-cancelDone:
			if !data.Stopped || data.Result == nil || data.Result.Status != profileTestStatusCancelled {
				t.Fatalf("停止载荷 = %+v（期望 cancelled 终态）", data)
			}
		case <-time.After(10 * time.Second):
			t.Fatal("停止请求没有返回")
		}
		rec := <-done
		var parsed struct {
			OK   bool                `json:"ok"`
			Data *profileTestPayload `json:"data"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &parsed); err != nil || parsed.Data == nil {
			t.Fatalf("信封不可解析: %v（%s）", err, rec.Body.String())
		}
		// 停止优先：即便执行面自报超时，终态也是 cancelled 而不是 failed/超时。
		if parsed.Data.Status != profileTestStatusCancelled || !parsed.Data.Cancelled {
			t.Fatalf("终态 = %+v（停止必须先于同时到达的超时落定）", parsed.Data)
		}
	})

	t.Run("客户端断开不冒充用户停止", func(t *testing.T) {
		srv, _ := testableServer(t)
		entered := make(chan struct{})
		srv.profileTestExecute = func(ctx context.Context, _ profileTestExecRequest) profileTestExecResult {
			close(entered)
			<-ctx.Done() // 请求 ctx 一断（客户端断开）执行面即退出
			return profileTestExecResult{ExitCode: -1, Err: ctx.Err()}
		}
		ctx, disconnect := context.WithCancel(context.Background())
		rec := httptest.NewRecorder()
		done := make(chan struct{})
		go func() {
			req := httptest.NewRequest(http.MethodPost,
				"/api/v1/tools/pi/profiles/"+managedCardID+"/test", nil).WithContext(ctx)
			req.Header.Set("X-Agmgr-Token", srv.token)
			srv.ServeHTTP(rec, req)
			close(done)
		}()
		select {
		case <-entered:
		case <-time.After(5 * time.Second):
			t.Fatal("测试没有进入执行面")
		}
		disconnect() // 客户端断开：取消的是**请求上下文**，不是停止入口
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			t.Fatal("断开后请求没有返回")
		}
		var parsed struct {
			OK   bool                `json:"ok"`
			Data *profileTestPayload `json:"data"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &parsed); err != nil || parsed.Data == nil {
			t.Fatalf("信封不可解析: %v（%s）", err, rec.Body.String())
		}
		if parsed.Data.Status != profileTestStatusFailed || parsed.Data.Cancelled {
			t.Fatalf("断开后的终态 = %+v（期望 failed 且 cancelled=false：不冒充用户停止）", parsed.Data)
		}
		if cancelRequestedOf(srv, family.Pi) {
			t.Errorf("客户端断开被误标成用户停止请求")
		}
	})
}

// TestProfileTestCancelReportsVerifyInFlightTruthfully 守「只停配置测试」的边界：
// 该工具上在飞的是受控验证（§4.8，与 §4.6 共用同一把闸门）⇒ 如实报告 kind=verify，
// 不终止它、不假装停过、也不折成错误。
func TestProfileTestCancelReportsVerifyInFlightTruthfully(t *testing.T) {
	srv, _ := testableServer(t)
	verifyRun := srv.startControlledRun(family.Pi, controlledRunVerify, "", nil)
	if verifyRun == nil {
		t.Fatal("夹具未能占用受控运行位")
	}
	defer srv.finishControlledRun(verifyRun)

	_, data := cancelTestRun(t, srv, "pi")
	if !data.Active || data.Stopped {
		t.Fatalf("受控验证在飞时 = %+v（期望 active=true, cancelled=false）", data)
	}
	if data.Kind != controlledRunVerify {
		t.Errorf("kind = %q（期望 verify）", data.Kind)
	}
	if !strings.Contains(data.Reason, "受控验证") || !strings.Contains(data.Reason, "只停止配置测试") {
		t.Errorf("边界文案未说清: %q", data.Reason)
	}
	// 受控验证没有被终止：运行仍在表里（停止入口不动它）。
	srv.profileTestMu.Lock()
	inFlight := srv.profileTestRuns[family.Pi]
	srv.profileTestMu.Unlock()
	if inFlight != verifyRun {
		t.Fatalf("受控验证被停止入口动过（%p → %p）", verifyRun, inFlight)
	}
}

// TestProfileTestCancelRejectsMismatchedCardOwnership 守归属：路径里的配置卡与在跑运行的
// 发起卡不符 ⇒ 不停止任何东西、`stopped:false` 且如实说明在跑的是哪张卡（错位调用不撒谎）。
func TestProfileTestCancelRejectsMismatchedCardOwnership(t *testing.T) {
	srv, _ := testableServer(t)
	entered := make(chan struct{})
	release := make(chan struct{})
	challenge := ""
	srv.profileTestExecute = func(_ context.Context, req profileTestExecRequest) profileTestExecResult {
		var ok bool
		challenge, ok = challengeFromProfileTestRequest(req)
		if !ok {
			t.Errorf("profile test did not pass a per-run challenge prompt: %q", req.Args)
		}
		close(entered)
		<-release
		return profileTestExecResult{Stdout: profileTestSuccessOutput("pi", challenge), ExitCode: 0}
	}
	done := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		req := httptest.NewRequest(http.MethodPost,
			"/api/v1/tools/pi/profiles/"+managedCardID+"/test", nil)
		req.Header.Set("X-Agmgr-Token", srv.token)
		rec := httptest.NewRecorder()
		srv.ServeHTTP(rec, req)
		done <- rec
	}()
	select {
	case <-entered:
	case <-time.After(10 * time.Second):
		t.Fatal("测试没有进入执行面")
	}

	// 另一张卡的停止请求：不实施停止。
	_, data := cancelTestRunAs(t, srv, "pi", "some-other-card")
	if !data.Active || data.Stopped || data.Kind != controlledRunTest {
		t.Fatalf("错位卡的停止载荷 = %+v（期望 active=true, stopped=false, kind=test）", data)
	}
	if !strings.Contains(data.Reason, managedCardID) || !strings.Contains(data.Reason, "另一张配置卡") {
		t.Errorf("归属文案未说清在跑的是谁: %q", data.Reason)
	}
	if cancelRequestedOf(srv, family.Pi) {
		t.Fatalf("错位调用置了停止位（不该停止任何东西）")
	}
	// 运行未受影响：放行后按正常路径收场（ok），不是 cancelled。
	close(release)
	rec := <-done
	var parsed struct {
		OK   bool                `json:"ok"`
		Data *profileTestPayload `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &parsed); err != nil || parsed.Data == nil {
		t.Fatalf("信封不可解析: %v（%s）", err, rec.Body.String())
	}
	if parsed.Data.Status != profileTestStatusOK || parsed.Data.Cancelled {
		t.Fatalf("错位停止影响了运行结果: %+v", parsed.Data)
	}
	if parsed.Data.Output != "原始输出已脱敏并隐藏" {
		t.Errorf("legacy test result should expose only the safe output summary: %q", parsed.Data.Output)
	}
	if challenge != "" && strings.Contains(rec.Body.String(), challenge) {
		t.Errorf("profile-test response leaked its challenge: %s", rec.Body.String())
	}
	if logs := srv.callRaw(t, http.MethodGet, "/api/v1/operations?limit=100", "", true); challenge != "" && strings.Contains(logs.body, challenge) {
		t.Errorf("operation log leaked its challenge: %s", logs.body)
	}
	// 事后按正确卡停止：已结束 ⇒ 空操作（幂等）。
	_, after := cancelTestRun(t, srv, "pi")
	if after.Active || after.Stopped {
		t.Fatalf("运行结束后的停止 = %+v（期望空操作）", after)
	}
}
