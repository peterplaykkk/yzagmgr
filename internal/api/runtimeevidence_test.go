// §4.8 运行时证据与受控验证（UC-092 A9/A10/A11）的夹具与用例。
//
// 三条测试纪律：
//  1. **绝不跑子进程**：受控运行走 §4.6 的同一道执行缝（profileTestExecute），
//     夹具用它模拟「CLI 真的把请求发到了本地代理」——桩负责在用量库里写一条同形记录。
//  2. **绝不触网**：契约夹具的上游传输默认立即失败；额度面走注入缝，本文件零网络请求。
//  3. 负向/失效/五场景矩阵逐条对应冻结文本的判据，原因词表逐字断言（机器可读）。
package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/peterplaykkk/yzagmgr/internal/apierr"
	"github.com/peterplaykkk/yzagmgr/internal/family"
	"github.com/peterplaykkk/yzagmgr/internal/usage"
)

// runtimeVerifyRecordSeq 给观测记录生成唯一 id（同一夹具内多次写入不冲突）。
var runtimeVerifyRecordSeq int32

// mustObjectRaw 取载荷里的子对象。
func mustObjectRaw(t *testing.T, item map[string]json.RawMessage, key string) map[string]json.RawMessage {
	t.Helper()
	raw, ok := item[key]
	if !ok {
		t.Fatalf("载荷缺键 %s: %s", key, mustMarshal(t, item))
	}
	var out map[string]json.RawMessage
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("键 %s 不是对象: %s", key, raw)
	}
	return out
}

// rawInt 取载荷里的整数值。
func rawInt(t *testing.T, item map[string]json.RawMessage, key string) int {
	t.Helper()
	raw, ok := item[key]
	if !ok {
		t.Fatalf("载荷缺键 %s: %s", key, mustMarshal(t, item))
	}
	var n int
	if err := json.Unmarshal(raw, &n); err != nil {
		t.Fatalf("键 %s 不是整数: %s", key, raw)
	}
	return n
}

// rawBool 取载荷里的布尔值。
func rawBool(t *testing.T, item map[string]json.RawMessage, key string) bool {
	t.Helper()
	raw, ok := item[key]
	if !ok {
		t.Fatalf("载荷缺键 %s: %s", key, mustMarshal(t, item))
	}
	var v bool
	if err := json.Unmarshal(raw, &v); err != nil {
		t.Fatalf("键 %s 不是布尔: %s", key, raw)
	}
	return v
}

// mustMarshal 把载荷折成字符串（断言失败时打印用）。
func mustMarshal(t *testing.T, item map[string]json.RawMessage) string {
	t.Helper()
	blob, err := json.Marshal(item)
	if err != nil {
		t.Fatalf("编码载荷失败: %v", err)
	}
	return string(blob)
}

// runtimeVerifyFixture 装配一套受控验证夹具：隔离数据根 + 已应用的受管档 + 定价 + 充足额度。
func runtimeVerifyFixture(t *testing.T) *contractServer {
	t.Helper()
	srv := newContractServer(t)
	applyCard(t, srv, family.Claude, managedCardID)
	if _, err := srv.Usage.ReplacePricing(context.Background(), usage.PricingTable{Entries: []usage.PricingEntry{
		{Model: "deepseek-flash", InputPerMillion: 0.14, OutputPerMillion: 0.28, Currency: "USD"},
	}}); err != nil {
		t.Fatalf("登记定价失败: %v", err)
	}
	srv.runtimeVerifyQuotaRead = func(context.Context) runtimeVerifyQuota {
		return runtimeVerifyQuota{State: runtimeQuotaSufficient, Source: "gateway", Detail: "测试夹具"}
	}
	return srv
}

// runtimeVerifyPreview 走一次验证预览并解析负载。
func runtimeVerifyPreview(t *testing.T, srv *contractServer, tool, body string) map[string]json.RawMessage {
	t.Helper()
	res := srv.callRaw(t, http.MethodPost, "/api/v1/tools/"+tool+"/runtime-evidence/verify/preview", body, true)
	res.assertEnvelope(t, http.MethodPost, "/runtime-evidence/verify/preview")
	if !res.ok {
		t.Fatalf("验证预览失败: %s", res.body)
	}
	return mustObject(t, res)
}

// runtimeVerifyApplyRaw 走一次验证执行并返回原始信封（失败语义在载荷里）。
func runtimeVerifyApplyRaw(t *testing.T, srv *contractServer, tool, previewID string) envelope {
	t.Helper()
	res := srv.callRaw(t, http.MethodPost, "/api/v1/tools/"+tool+"/runtime-evidence/verify/apply",
		`{"previewId":"`+previewID+`"}`, true)
	res.assertEnvelope(t, http.MethodPost, "/runtime-evidence/verify/apply")
	return res
}

// runtimeVerifyApply 走一次验证执行并解析负载（信封必须是成功——拒绝也在载荷里表达）。
func runtimeVerifyApply(t *testing.T, srv *contractServer, tool, previewID string) map[string]json.RawMessage {
	t.Helper()
	res := runtimeVerifyApplyRaw(t, srv, tool, previewID)
	if !res.ok {
		t.Fatalf("验证执行的响应不是成功信封（拒绝应在载荷里表达）: %s", res.body)
	}
	return mustObject(t, res)
}

// runtimeEvidenceGet 读运行时证据面并解析负载。
func runtimeEvidenceGet(t *testing.T, srv *contractServer, tool string) map[string]json.RawMessage {
	t.Helper()
	res := srv.callRaw(t, http.MethodGet, "/api/v1/tools/"+tool+"/runtime-evidence", "", true)
	res.assertEnvelope(t, http.MethodGet, "/runtime-evidence")
	if !res.ok {
		t.Fatalf("读取运行时证据失败: %s", res.body)
	}
	return mustObject(t, res)
}

// runtimeVerifyObservedStub 是一条「被观测到」的执行桩：读受控工作区里的配置（真实请求
// 就会带那份配置里的档名），并写一条与本地代理同形的用量记录。
//
// 返回值：调用计数（可由用例断言「拒绝 = 0 次调用」）与最后一次的工作区快照。
type runtimeVerifyObservedStub struct {
	calls    int32
	roots    []string
	lastRoot string
	// app / prefix / upstream / status / dropMarker 让每个场景摆出不同的观测世界。
	app       string
	prefix    string
	upstream  string
	status    int
	markerful bool
	// fail / timeout 模拟执行失败与超时。
	fail    bool
	timeout bool
	// pid 是桩返回的进程号（证据实例要素）。
	pid int
	// ruleVersion 是桩写观测记录时携带的生效规则版本（UC-096 A6；空 = 不携带）。
	ruleVersion string
}

func (s *runtimeVerifyObservedStub) exec(t *testing.T, srv *contractServer) func(context.Context, profileTestExecRequest) profileTestExecResult {
	t.Helper()
	if s.app == "" {
		s.app = "claude"
	}
	if s.prefix == "" {
		s.prefix = "claude"
	}
	if s.upstream == "" {
		s.upstream = "deepseek-flash"
	}
	return func(ctx context.Context, req profileTestExecRequest) profileTestExecResult {
		atomic.AddInt32(&s.calls, 1)
		s.lastRoot = req.Root
		s.roots = append(s.roots, req.Root)
		if s.fail {
			return profileTestExecResult{ExitCode: 1, Err: fmt.Errorf("桩：CLI 失败"), PID: 0}
		}
		if s.timeout {
			return profileTestExecResult{ExitCode: -1, TimedOut: true, PID: 0}
		}
		blob, err := os.ReadFile(filepath.Join(req.Root, ".claude", "settings.json"))
		if err != nil {
			t.Errorf("受控工作区里没有 .claude/settings.json: %v", err)
			return profileTestExecResult{ExitCode: 1, Err: err}
		}
		var doc struct {
			Env map[string]string `json:"env"`
		}
		if err := json.Unmarshal(blob, &doc); err != nil {
			t.Errorf("受控工作区配置不是 JSON: %v", err)
			return profileTestExecResult{ExitCode: 1, Err: err}
		}
		model := doc.Env["ANTHROPIC_MODEL"]
		if !s.markerful {
			model = strings.SplitN(model, "-agv-", 2)[0] + "[1m]"
		}
		rec := usage.Record{
			ID:             fmt.Sprintf("rq_verify%024d", atomic.AddInt32(&runtimeVerifyRecordSeq, 1)),
			At:             time.Now(),
			App:            s.app,
			Prefix:         s.prefix,
			Provider:       "glm",
			ModelRequested: model,
			ModelUpstream:  s.upstream,
			Status:         s.status,
			DurationMs:     12,
			RuleVersion:    s.ruleVersion,
		}
		if rec.Status == 0 {
			rec.Status = 200
		}
		if _, err := srv.Usage.Record(ctx, rec); err != nil {
			t.Errorf("写入观测记录失败: %v", err)
		}
		return profileTestExecResult{ExitCode: 0, PID: s.pid}
	}
}

// runtimeEvidenceBlockers 读出载荷里的 blockers 原因清单。
func runtimeEvidenceBlockers(t *testing.T, payload map[string]json.RawMessage) []string {
	t.Helper()
	var blockers []struct {
		Reason string `json:"reason"`
	}
	raw := payload["blockers"]
	if raw == nil {
		return nil
	}
	if err := json.Unmarshal(raw, &blockers); err != nil {
		t.Fatalf("blockers 不是预期结构: %v", err)
	}
	out := make([]string, 0, len(blockers))
	for _, b := range blockers {
		out = append(out, b.Reason)
	}
	return out
}

// ── 正常路径（A4 的「有证据」夹具 + A9 五要素 + A10 范围） ──────────────────

func TestRuntimeVerifyHappyPathEndToEnd(t *testing.T) {
	srv := runtimeVerifyFixture(t)
	stub := &runtimeVerifyObservedStub{markerful: true, pid: 4242,
		// UC-096 A6 additive：观测记录携带规则版本 ⇒ 证据面照抄（同一份记账，不重算）。
		ruleVersion: "rv1-0123456789abcdef"}
	srv.profileTestExecute = stub.exec(t, srv)

	plan := runtimeVerifyPreview(t, srv, "claude", "")
	if got := rawString(t, plan, "previewId"); got == "" {
		t.Fatalf("预览没有给出 previewId: %v", plan)
	}
	if rawString(t, plan, "marker") == "" {
		t.Fatalf("预览没有给出唯一标记: %v", plan)
	}
	if !rawBool(t, plan, "willCallModel") {
		t.Fatalf("willCallModel 期望 true")
	}
	if blockers := runtimeEvidenceBlockers(t, plan); len(blockers) != 0 {
		t.Fatalf("正常路径不应有 blockers: %v", blockers)
	}
	planJSON, _ := json.Marshal(plan)
	// （声明映射 requested/upstream 行已随 C 路映射删除——design §2.2。）
	for _, want := range []string{`"quote":"known"`, `"hardCapUsd":0.5`} {
		if !strings.Contains(string(planJSON), want) {
			t.Errorf("预览载荷缺 %s: %s", want, planJSON)
		}
	}

	payload := runtimeVerifyApply(t, srv, "claude", rawString(t, plan, "previewId"))
	if got := rawString(t, payload, "status"); got != "verified" {
		t.Fatalf("执行结果 status = %s，期望 verified（%s）", got, mustMarshal(t, payload))
	}
	if got := rawString(t, payload, "reason"); got != reasonVerified {
		t.Fatalf("执行结果 reason = %s，期望 %s", got, reasonVerified)
	}
	if got := rawInt(t, payload, "modelCalls"); got != 1 {
		t.Fatalf("modelCalls = %d，期望 1（一次调用）", got)
	}
	if got := atomic.LoadInt32(&stub.calls); got != 1 {
		t.Fatalf("执行桩被调用 %d 次，期望恰 1 次", got)
	}
	if stub.lastRoot == "" {
		t.Fatalf("执行桩没有收到受控工作区")
	}
	if _, err := os.Stat(stub.lastRoot); !os.IsNotExist(err) {
		t.Errorf("受控工作区在运行后必须删除（当前 stat err=%v）", err)
	}

	// A9 五要素逐项核对（从响应载荷里读原始 JSON）。
	ev := mustObjectRaw(t, payload, "evidence")
	inst := mustObjectRaw(t, ev, "instance")
	if rawString(t, inst, "kind") != "controlled-run" || rawInt(t, inst, "pid") != 4242 {
		t.Errorf("instance 要素不符: %s", mustMarshal(t, inst))
	}
	cfg := mustObjectRaw(t, ev, "configVersion")
	if rawString(t, cfg, "applyTx") == "" || rawString(t, cfg, "profileId") != managedCardID {
		t.Errorf("configVersion 要素不符: %s", mustMarshal(t, cfg))
	}
	decide := mustObjectRaw(t, cfg, "decideKey")
	if rawString(t, decide, "key") != "env.ANTHROPIC_BASE_URL" || rawString(t, decide, "digest") == "" {
		t.Errorf("脱敏决定键摘要不符: %s", mustMarshal(t, decide))
	}
	ws := mustObjectRaw(t, ev, "workspace")
	if !strings.Contains(rawString(t, ws, "configPath"), string(os.PathSeparator)+".claude"+string(os.PathSeparator)+"settings.json") {
		t.Errorf("workspace.configPath 不符: %s", mustMarshal(t, ws))
	}
	req := mustObjectRaw(t, ev, "request")
	if rawString(t, req, "recordId") == "" || rawString(t, req, "at") == "" || rawInt(t, req, "status") != 200 {
		t.Errorf("request 要素不符: %s", mustMarshal(t, req))
	}
	out := mustObjectRaw(t, ev, "outbound")
	if !strings.Contains(rawString(t, out, "modelRequested"), "-agv-") {
		t.Errorf("modelRequested 里必须带唯一标记: %s", mustMarshal(t, out))
	}
	if rawString(t, out, "modelUpstream") != "deepseek-flash" {
		t.Errorf("modelUpstream 必须是映射后的上游名: %s", mustMarshal(t, out))
	}
	if rawString(t, ev, "obtainedAt") == "" {
		t.Errorf("证据缺获得时间")
	}
	applies := mustObjectRaw(t, ev, "appliesTo")
	if rawString(t, applies, "toolId") != "claude" {
		t.Errorf("appliesTo 不适用实例: %s", mustMarshal(t, applies))
	}
	// UC-096 A6 additive：证据关联请求的生效规则版本从记账照抄（与请求明细同一口径）。
	if got := rawString(t, ev, "ruleVersion"); got != "rv1-0123456789abcdef" {
		t.Errorf("证据的 ruleVersion = %q，期望照抄观测记录的值", got)
	}

	// 读取面：服务端判定 = verified；证据非 null。
	got := runtimeEvidenceGet(t, srv, "claude")
	if rawString(t, got, "state") != "verified" || rawString(t, got, "reason") != reasonVerified {
		t.Fatalf("GET 判定 = %s/%s，期望 verified", rawString(t, got, "state"), rawString(t, got, "reason"))
	}
	if !strings.Contains(string(got["evidence"]), `"source":"controlled"`) {
		t.Errorf("GET 的 evidence 应是受控来源: %s", got["evidence"])
	}
	// 「不新增钱包扣款」的结构性证明：整个验证流程没有向网关发出任何请求。
	if records := srv.upstream.recorded(); len(records) != 0 {
		t.Errorf("受控验证不得触发任何网关请求（钱包/计费路径不新增）: %v", records)
	}
}

// ── 无证据夹具（A4 的另一半） ───────────────────────────────────────────────

func TestRuntimeEvidenceWithoutEvidenceIsUnconfirmed(t *testing.T) {
	srv := runtimeVerifyFixture(t)
	got := runtimeEvidenceGet(t, srv, "claude")
	if rawString(t, got, "state") != "unconfirmed" || rawString(t, got, "reason") != reasonNeverVerified {
		t.Fatalf("无证据时必须未确认 + never_verified: %s", mustMarshal(t, got))
	}
	if string(got["evidence"]) != "null" {
		t.Errorf("无证据时 evidence 必须是 null: %s", got["evidence"])
	}
	verify := mustObjectRaw(t, got, "verify")
	if !rawBool(t, verify, "supported") || !rawBool(t, verify, "willCallModel") {
		t.Fatalf("claude 应报告受控验证可用: %s", mustMarshal(t, verify))
	}
}

// ── 负向矩阵（纯函数逐判据 + 端到端一条） ───────────────────────────────────

func TestRuntimeVerifyNegativeMatrixCorrelation(t *testing.T) {
	marker := "agv-0123456789ab"
	upstream := "deepseek-flash"
	base := runtimeObservedRecord{
		ID: "rq_1", App: "claude", Prefix: "claude",
		ModelRequested: "claude-fable-5-1-" + marker + "[1m]", ModelUpstream: upstream, Status: 200,
	}
	cases := []struct {
		name string
		recs []runtimeObservedRecord
		want string
		hit  bool
	}{
		{"一致即升级", []runtimeObservedRecord{base}, reasonVerified, true},
		{"另一工具的相同模型请求不升级", []runtimeObservedRecord{func() runtimeObservedRecord {
			r := base
			r.App, r.Prefix = "codex", "codex"
			r.ModelRequested = strings.ReplaceAll(r.ModelRequested, "claude-fable-5-1", "deepseek-flash")
			return r
		}()}, reasonNotSelf, false},
		{"旁路/错前缀不升级", []runtimeObservedRecord{func() runtimeObservedRecord {
			r := base
			r.Prefix = "openai"
			return r
		}()}, reasonPathUnobservable, false},
		{"旧进程/旧配置版本（无标记）不升级", []runtimeObservedRecord{func() runtimeObservedRecord {
			r := base
			r.ModelRequested = "claude-fable-5-1[1m]"
			return r
		}()}, reasonMarkerMissing, false},
		{"映射不符不升级", []runtimeObservedRecord{func() runtimeObservedRecord {
			r := base
			r.ModelUpstream = "other-model"
			return r
		}()}, reasonMappingMismatch, false},
		{"上游非 2xx 不升级", []runtimeObservedRecord{func() runtimeObservedRecord {
			r := base
			r.Status = 401
			return r
		}()}, reasonUpstreamRejected, false},
		{"没有记录 = 观测缺失", nil, reasonObservationMissing, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, reason, hit := correlateRuntimeObservation(tc.recs, []string{"claude"}, []string{"claude", "anthropic"}, marker, upstream)
			if hit != tc.hit || reason != tc.want {
				t.Fatalf("关联判定 = (%v, %s)，期望 (%v, %s)", hit, reason, tc.hit, tc.want)
			}
		})
	}

	t.Run("最接近的拒绝原因胜出", func(t *testing.T) {
		other := base
		other.App, other.Prefix = "codex", "codex"
		noMarker := base
		noMarker.ModelRequested = "claude-fable-5-1[1m]"
		_, reason, hit := correlateRuntimeObservation(
			[]runtimeObservedRecord{other, noMarker}, []string{"claude"}, []string{"claude", "anthropic"}, marker, upstream)
		if hit || reason != reasonMarkerMissing {
			t.Fatalf("应报最接近的判据 marker_missing，实际 (%v, %s)", hit, reason)
		}
	})
}

func TestRuntimeVerifyNegativeMatrixEndToEnd(t *testing.T) {
	// 端到端：桩写一条「同模型但不带本次唯一标记」的记录（旧进程/旧配置版本的形态）。
	srv := runtimeVerifyFixture(t)
	stub := &runtimeVerifyObservedStub{markerful: false, pid: 7}
	srv.profileTestExecute = stub.exec(t, srv)

	plan := runtimeVerifyPreview(t, srv, "claude", "")
	payload := runtimeVerifyApply(t, srv, "claude", rawString(t, plan, "previewId"))
	if got := rawString(t, payload, "status"); got != "unconfirmed" {
		t.Fatalf("无标记记录不得升级，status = %s", got)
	}
	if got := rawString(t, payload, "reason"); got != reasonMarkerMissing {
		t.Fatalf("原因 = %s，期望 marker_missing", got)
	}
	if got := rawInt(t, payload, "modelCalls"); got != 1 {
		t.Fatalf("modelCalls = %d，期望 1（执行过，不重发）", got)
	}
	if got := atomic.LoadInt32(&stub.calls); got != 1 {
		t.Fatalf("执行桩调用 %d 次，期望恰 1 次（不自动重发）", got)
	}
	got := runtimeEvidenceGet(t, srv, "claude")
	if rawString(t, got, "state") != "unconfirmed" || rawString(t, got, "reason") != reasonNeverVerified {
		t.Fatalf("未升级时读取面必须保持未确认: %s", mustMarshal(t, got))
	}
	verify := mustObjectRaw(t, got, "verify")
	if rawInt(t, verify, "attemptCount") != 1 {
		t.Fatalf("尝试计数应为 1: %s", mustMarshal(t, verify))
	}
	if !strings.Contains(string(verify["attempts"]), reasonMarkerMissing) {
		t.Fatalf("尝试历史应留存首次失败原因: %s", verify["attempts"])
	}
}

// ── 五场景调用计数与预算拒绝（A11 判据） ────────────────────────────────────

func TestRuntimeVerifyFiveScenarioCallCounts(t *testing.T) {
	t.Run("正常：1 次调用且升级", func(t *testing.T) {
		srv := runtimeVerifyFixture(t)
		stub := &runtimeVerifyObservedStub{markerful: true}
		srv.profileTestExecute = stub.exec(t, srv)
		plan := runtimeVerifyPreview(t, srv, "claude", "")
		payload := runtimeVerifyApply(t, srv, "claude", rawString(t, plan, "previewId"))
		if rawString(t, payload, "status") != "verified" || atomic.LoadInt32(&stub.calls) != 1 {
			t.Fatalf("正常场景不符: %s（调用 %d 次）", mustMarshal(t, payload), stub.calls)
		}
	})

	t.Run("缺额度：0 次调用", func(t *testing.T) {
		srv := runtimeVerifyFixture(t)
		srv.runtimeVerifyQuotaRead = func(context.Context) runtimeVerifyQuota {
			return runtimeVerifyQuota{State: runtimeQuotaExhausted, Source: "gateway", Detail: "窗口用尽"}
		}
		stub := &runtimeVerifyObservedStub{markerful: true}
		srv.profileTestExecute = stub.exec(t, srv)
		plan := runtimeVerifyPreview(t, srv, "claude", "")
		if blockers := runtimeEvidenceBlockers(t, plan); len(blockers) == 0 || blockers[0] != reasonQuotaExhausted {
			t.Fatalf("预览应给出 quota_exhausted 阻塞: %v", blockers)
		}
		payload := runtimeVerifyApply(t, srv, "claude", rawString(t, plan, "previewId"))
		if rawString(t, payload, "reason") != reasonQuotaExhausted || rawInt(t, payload, "modelCalls") != 0 {
			t.Fatalf("缺额度必须拒绝且 0 调用: %s", mustMarshal(t, payload))
		}
		if got := atomic.LoadInt32(&stub.calls); got != 0 {
			t.Fatalf("缺额度时执行桩被调用 %d 次，期望 0", got)
		}
	})

	t.Run("未知报价：0 次调用且明示未知", func(t *testing.T) {
		srv := runtimeVerifyFixture(t)
		// 清空定价表 ⇒ 报价未知。
		if _, err := srv.Usage.ReplacePricing(context.Background(), usage.PricingTable{}); err != nil {
			t.Fatalf("清空定价失败: %v", err)
		}
		stub := &runtimeVerifyObservedStub{markerful: true}
		srv.profileTestExecute = stub.exec(t, srv)
		plan := runtimeVerifyPreview(t, srv, "claude", "")
		planJSON := string(mustMarshal(t, plan))
		if !strings.Contains(planJSON, `"quote":"unknown"`) || !strings.Contains(planJSON, `"estimatedCostUsd":null`) {
			t.Fatalf("预览必须明示报价未知（金额 null，不伪装免费）: %s", planJSON)
		}
		payload := runtimeVerifyApply(t, srv, "claude", rawString(t, plan, "previewId"))
		if rawString(t, payload, "reason") != reasonQuoteUnknown || rawInt(t, payload, "modelCalls") != 0 {
			t.Fatalf("未知报价必须拒绝且 0 调用: %s", mustMarshal(t, payload))
		}
		if got := atomic.LoadInt32(&stub.calls); got != 0 {
			t.Fatalf("未知报价时执行桩被调用 %d 次，期望 0", got)
		}
	})

	t.Run("失败：1 次调用、保留原因、不重发", func(t *testing.T) {
		srv := runtimeVerifyFixture(t)
		stub := &runtimeVerifyObservedStub{markerful: true, fail: true}
		srv.profileTestExecute = stub.exec(t, srv)
		plan := runtimeVerifyPreview(t, srv, "claude", "")
		payload := runtimeVerifyApply(t, srv, "claude", rawString(t, plan, "previewId"))
		if rawString(t, payload, "reason") != reasonVerifyFailed || rawInt(t, payload, "modelCalls") != 1 {
			t.Fatalf("失败场景不符: %s", mustMarshal(t, payload))
		}
		if got := atomic.LoadInt32(&stub.calls); got != 1 {
			t.Fatalf("失败后不得自动重发（调用 %d 次，期望 1）", got)
		}
	})

	t.Run("响应丢失：1 次调用、不升级、不重发", func(t *testing.T) {
		srv := runtimeVerifyFixture(t)
		// 桩「执行成功」但不写观测记录（响应丢失/未观测）。
		srv.profileTestExecute = func(context.Context, profileTestExecRequest) profileTestExecResult {
			return profileTestExecResult{ExitCode: 0, PID: 9}
		}
		plan := runtimeVerifyPreview(t, srv, "claude", "")
		payload := runtimeVerifyApply(t, srv, "claude", rawString(t, plan, "previewId"))
		if rawString(t, payload, "reason") != reasonObservationMissing || rawInt(t, payload, "modelCalls") != 1 {
			t.Fatalf("响应丢失场景不符: %s", mustMarshal(t, payload))
		}
		got := runtimeEvidenceGet(t, srv, "claude")
		if rawString(t, got, "state") != "unconfirmed" {
			t.Fatalf("无观测记录必须保持未确认: %s", mustMarshal(t, got))
		}
	})

	t.Run("预算拒绝：上界越批准上限 ⇒ 0 次调用", func(t *testing.T) {
		srv := runtimeVerifyFixture(t)
		stub := &runtimeVerifyObservedStub{markerful: true}
		srv.profileTestExecute = stub.exec(t, srv)
		plan := runtimeVerifyPreview(t, srv, "claude", `{"maxCostUsd":0.000001}`)
		if blockers := runtimeEvidenceBlockers(t, plan); len(blockers) == 0 || blockers[0] != reasonBudgetExceeded {
			t.Fatalf("预览应给出 budget_exceeded 阻塞: %v", blockers)
		}
		payload := runtimeVerifyApply(t, srv, "claude", rawString(t, plan, "previewId"))
		if rawString(t, payload, "reason") != reasonBudgetExceeded || rawInt(t, payload, "modelCalls") != 0 {
			t.Fatalf("预算越限必须拒绝且 0 调用: %s", mustMarshal(t, payload))
		}
		if got := atomic.LoadInt32(&stub.calls); got != 0 {
			t.Fatalf("预算越限时执行桩被调用 %d 次，期望 0", got)
		}
	})

	t.Run("尝试计数可核（拒绝 0 次 + 执行 1 次累加）", func(t *testing.T) {
		srv := runtimeVerifyFixture(t)
		srv.runtimeVerifyQuotaRead = func(context.Context) runtimeVerifyQuota {
			return runtimeVerifyQuota{State: runtimeQuotaExhausted, Source: "gateway", Detail: "窗口用尽"}
		}
		plan := runtimeVerifyPreview(t, srv, "claude", "")
		_ = runtimeVerifyApply(t, srv, "claude", rawString(t, plan, "previewId"))
		got := runtimeEvidenceGet(t, srv, "claude")
		verify := mustObjectRaw(t, got, "verify")
		if rawInt(t, verify, "attemptCount") != 1 {
			t.Fatalf("拒绝也要留痕: %s", mustMarshal(t, verify))
		}
		if !strings.Contains(string(verify["attempts"]), `"modelCalls":0`) {
			t.Fatalf("拒绝尝试的 modelCalls 必须是 0: %s", verify["attempts"])
		}
	})
}

// ── 失效矩阵（A10：外改 / 再次应用 / 恢复清空；映射与 origin 两行随 C 路删） ──

func TestRuntimeEvidenceInvalidationMatrix(t *testing.T) {
	newVerified := func(t *testing.T) (*contractServer, *runtimeVerifyObservedStub) {
		t.Helper()
		srv := runtimeVerifyFixture(t)
		stub := &runtimeVerifyObservedStub{markerful: true}
		srv.profileTestExecute = stub.exec(t, srv)
		plan := runtimeVerifyPreview(t, srv, "claude", "")
		payload := runtimeVerifyApply(t, srv, "claude", rawString(t, plan, "previewId"))
		if rawString(t, payload, "status") != "verified" {
			t.Fatalf("夹具未达到已验证: %s", mustMarshal(t, payload))
		}
		return srv, stub
	}

	t.Run("外改：盘上指纹变化 ⇒ config_changed", func(t *testing.T) {
		srv, _ := newVerified(t)
		settings := srv.env.ClaudeSettings()
		blob, err := os.ReadFile(settings)
		if err != nil {
			t.Fatalf("读 live 失败: %v", err)
		}
		if err := os.WriteFile(settings, append(blob, []byte("\n")...), 0o600); err != nil {
			t.Fatalf("外改 live 失败: %v", err)
		}
		got := runtimeEvidenceGet(t, srv, "claude")
		if rawString(t, got, "state") != "unconfirmed" || rawString(t, got, "reason") != reasonConfigChanged {
			t.Fatalf("外改后必须降级为 config_changed: %s", mustMarshal(t, got))
		}
		if string(got["evidence"]) != "null" {
			t.Errorf("失效证据不得回流: %s", got["evidence"])
		}
		if !strings.Contains(string(got["reasonDetail"]), settings) {
			t.Errorf("reasonDetail 应列出变化路径: %s", got["reasonDetail"])
		}
		if rawString(t, got, "lastObtainedAt") == "" {
			t.Errorf("应给出上次证据获得时间: %s", mustMarshal(t, got))
		}
	})

	t.Run("再次应用：事务身份变化 ⇒ reapplied（幂等重放同样失效）", func(t *testing.T) {
		srv, _ := newVerified(t)
		applyCard(t, srv, family.Claude, managedCardID) // 字节相同（unchanged），但事务身份是新的
		got := runtimeEvidenceGet(t, srv, "claude")
		if rawString(t, got, "state") != "unconfirmed" || rawString(t, got, "reason") != reasonReapplied {
			t.Fatalf("再次应用后必须降级为 reapplied: %s", mustMarshal(t, got))
		}
	})

	// （「映射变化」与「端点 origin 变化」两行已注销：声明映射与九前缀出厂路由表
	// 随 C 路删除——design §2.2/§2.4，mapping_changed 失效判据不再有可变输入。）

	t.Run("恢复/重置清空生效记账 ⇒ config_changed", func(t *testing.T) {
		srv, _ := newVerified(t)
		preview := srv.callRaw(t, http.MethodPost, "/api/v1/tools/claude/reset/preview", "", true)
		if !preview.ok {
			t.Fatalf("重置预览失败: %s", preview.body)
		}
		pid := rawString(t, mustObject(t, preview), "previewId")
		reset := srv.callRaw(t, http.MethodPost, "/api/v1/tools/claude/reset/apply",
			`{"previewId":"`+pid+`"}`, true)
		if !reset.ok {
			t.Fatalf("重置失败: %s", reset.body)
		}
		got := runtimeEvidenceGet(t, srv, "claude")
		if rawString(t, got, "state") != "unconfirmed" || rawString(t, got, "reason") != reasonConfigChanged {
			t.Fatalf("重置后必须降级为 config_changed: %s", mustMarshal(t, got))
		}
	})
}

// ── 不支持与不可观测（A10 的「明确未确认及原因」） ──────────────────────────

func TestRuntimeVerifyUnsupportedToolAndNotObservable(t *testing.T) {
	t.Run("不支持的工具：预览 not_supported、读取面如实报", func(t *testing.T) {
		srv := runtimeVerifyFixture(t)
		res := srv.callRaw(t, http.MethodPost, "/api/v1/tools/codex/runtime-evidence/verify/preview", "", true)
		res.assertEnvelope(t, http.MethodPost, "/runtime-evidence/verify/preview")
		if res.ok || res.code != apierr.CodeNotSupported {
			t.Fatalf("codex 的受控验证预览应 not_supported: ok=%v code=%s body=%s", res.ok, res.code, res.body)
		}
		got := runtimeEvidenceGet(t, srv, "codex")
		if rawString(t, got, "state") != "unconfirmed" || rawString(t, got, "reason") != reasonUnsupportedTool {
			t.Fatalf("codex 读取面应报 unsupported_tool: %s", mustMarshal(t, got))
		}
		verify := mustObjectRaw(t, got, "verify")
		if rawBool(t, verify, "supported") {
			t.Fatalf("verify.supported 应为 false: %s", mustMarshal(t, verify))
		}
	})

	t.Run("不可观测形态：官方档 ⇒ not_observable", func(t *testing.T) {
		srv := runtimeVerifyFixture(t)
		// 应用官方档：决定键消失（不是本地车道形态）⇒ 受控验证拒绝（没有观测点）。
		applyCard(t, srv, family.Claude, OfficialProfileID)
		plan := runtimeVerifyPreview(t, srv, "claude", "")
		blockers := runtimeEvidenceBlockers(t, plan)
		if len(blockers) == 0 || blockers[0] != reasonNotObservable {
			t.Fatalf("官方档应报 not_observable 阻塞: %v", blockers)
		}
		payload := runtimeVerifyApply(t, srv, "claude", rawString(t, plan, "previewId"))
		if rawString(t, payload, "reason") != reasonNotObservable || rawInt(t, payload, "modelCalls") != 0 {
			t.Fatalf("不可观测形态必须拒绝且 0 调用: %s", mustMarshal(t, payload))
		}
	})

	t.Run("未应用任何档 ⇒ config_not_applied 且 0 调用；额度/报价阻塞同列", func(t *testing.T) {
		// 隔离世界的默认形态（未登录 = 额度未知、无定价 = 报价未知）：预检必须把
		// 「配置补好后仍会被拦下」的当前条件一并列出（全字段可读、阻塞如实呈现）。
		srv := newContractServer(t)
		srv.runtimeVerifyQuotaRead = func(context.Context) runtimeVerifyQuota {
			return runtimeVerifyQuota{State: runtimeQuotaUnknown, Source: "gateway", Detail: "未登录（测试）"}
		}
		plan := runtimeVerifyPreview(t, srv, "claude", "")
		blockers := runtimeEvidenceBlockers(t, plan)
		if len(blockers) == 0 || blockers[0] != reasonConfigNotApplied {
			t.Fatalf("未应用档位应报 config_not_applied: %v", blockers)
		}
		for _, want := range []string{reasonQuotaUnknown, reasonQuoteUnknown} {
			if !containsString(blockers, want) {
				t.Fatalf("预检应同列 %s（当前条件事实）: %v", want, blockers)
			}
		}
		payload := runtimeVerifyApply(t, srv, "claude", rawString(t, plan, "previewId"))
		if rawString(t, payload, "reason") != reasonConfigNotApplied || rawInt(t, payload, "modelCalls") != 0 {
			t.Fatalf("未应用必须拒绝且 0 调用（最具体原因优先）: %s", mustMarshal(t, payload))
		}
	})

	t.Run("工具未安装：not_supported + 0 调用", func(t *testing.T) {
		srv := runtimeVerifyFixture(t)
		srv.profileTestResolveCLI = func(family.ID, string) string { return "" }
		plan := runtimeVerifyPreview(t, srv, "claude", "")
		payload := runtimeVerifyApply(t, srv, "claude", rawString(t, plan, "previewId"))
		if rawString(t, payload, "reason") != reasonUnsupportedTool || rawInt(t, payload, "modelCalls") != 0 {
			t.Fatalf("未安装必须拒绝且 0 调用: %s", mustMarshal(t, payload))
		}
	})
}

// ── 并发闸门（与 §4.6 共用同一把） ─────────────────────────────────────────

func TestRuntimeVerifySharesBusyGate(t *testing.T) {
	srv := runtimeVerifyFixture(t)
	stub := &runtimeVerifyObservedStub{markerful: true}
	srv.profileTestExecute = stub.exec(t, srv)
	plan := runtimeVerifyPreview(t, srv, "claude", "")
	// 占住同一把闸门（在飞运行表）：用测试种类的把手（C-2 起 begin/endProfileTest
	// 由 controlledRun 表取代）。
	run := srv.startControlledRun(family.Claude, controlledRunTest, "", func() {})
	if run == nil {
		t.Fatalf("夹具未能占用受控运行位")
	}
	payload := runtimeVerifyApply(t, srv, "claude", rawString(t, plan, "previewId"))
	srv.finishControlledRun(run)
	if rawString(t, payload, "reason") != reasonInProgress || rawInt(t, payload, "modelCalls") != 0 {
		t.Fatalf("并发时必须拒绝且 0 调用: %s", mustMarshal(t, payload))
	}
	if got := atomic.LoadInt32(&stub.calls); got != 0 {
		t.Fatalf("并发拒绝时执行桩被调用 %d 次，期望 0", got)
	}
}

// ── 预览之后配置漂移（执行前复核） ─────────────────────────────────────────

func TestRuntimeVerifyRejectsDriftBetweenPreviewAndApply(t *testing.T) {
	srv := runtimeVerifyFixture(t)
	stub := &runtimeVerifyObservedStub{markerful: true}
	srv.profileTestExecute = stub.exec(t, srv)
	plan := runtimeVerifyPreview(t, srv, "claude", "")
	// 预览之后外改盘上配置 ⇒ 执行必须拒绝（0 调用）。
	settings := srv.env.ClaudeSettings()
	blob, err := os.ReadFile(settings)
	if err != nil {
		t.Fatalf("读 live 失败: %v", err)
	}
	if err := os.WriteFile(settings, append(blob, []byte("\n")...), 0o600); err != nil {
		t.Fatalf("外改 live 失败: %v", err)
	}
	payload := runtimeVerifyApply(t, srv, "claude", rawString(t, plan, "previewId"))
	if rawString(t, payload, "reason") != reasonConfigChanged || rawInt(t, payload, "modelCalls") != 0 {
		t.Fatalf("预览后漂移必须拒绝且 0 调用: %s", mustMarshal(t, payload))
	}
	if got := atomic.LoadInt32(&stub.calls); got != 0 {
		t.Fatalf("漂移拒绝时执行桩被调用 %d 次，期望 0", got)
	}
}

// ── 凭据纪律（界面与日志无凭据明文） ───────────────────────────────────────

func TestRuntimeEvidenceNoPlaintext(t *testing.T) {
	srv := runtimeVerifyFixture(t)
	stub := &runtimeVerifyObservedStub{markerful: true}
	srv.profileTestExecute = stub.exec(t, srv)
	settings := srv.env.ClaudeSettings()
	blob, err := os.ReadFile(settings)
	if err != nil {
		t.Fatalf("读 live 失败: %v", err)
	}
	secret := "sk-synthetic-secret-0123456789abcdef0123456789"
	mutated := strings.Replace(string(blob), `"ANTHROPIC_BASE_URL"`, `"ANTHROPIC_AUTH_TOKEN": "`+secret+`", "ANTHROPIC_BASE_URL"`, 1)
	if !strings.Contains(mutated, secret) {
		t.Fatalf("夹具注入合成秘密失败（live 里没有可替换的锚点）")
	}
	if err := os.WriteFile(settings, []byte(mutated), 0o600); err != nil {
		t.Fatalf("写合成秘密失败: %v", err)
	}

	bodies := []string{}
	preview := srv.callRaw(t, http.MethodPost, "/api/v1/tools/claude/runtime-evidence/verify/preview", "", true)
	bodies = append(bodies, preview.body)
	plan := mustObject(t, preview)
	apply := srv.callRaw(t, http.MethodPost, "/api/v1/tools/claude/runtime-evidence/verify/apply",
		`{"previewId":"`+rawString(t, plan, "previewId")+`"}`, true)
	bodies = append(bodies, apply.body)
	get := srv.callRaw(t, http.MethodGet, "/api/v1/tools/claude/runtime-evidence", "", true)
	bodies = append(bodies, get.body)
	// 全流程不得出现合成秘密、管理令牌与推理种子（单源掩码纪律）。
	for i, body := range bodies {
		for _, forbidden := range []string{secret, srv.token, "contract-test-inference-token"} {
			if strings.Contains(body, forbidden) {
				t.Errorf("响应 #%d 泄漏了 %q: %s", i, forbidden, body)
			}
		}
	}
	if got := atomic.LoadInt32(&stub.calls); got != 0 {
		t.Fatalf("外改后必须拒绝发起，执行桩被调用 %d 次", got)
	}
}

// ── 受控工作区：权限、标记、崩溃残留清理 ───────────────────────────────────

func TestRuntimeVerifyWorkspaceDiscipline(t *testing.T) {
	srv := runtimeVerifyFixture(t)
	var permsOK bool
	srv.profileTestExecute = func(ctx context.Context, req profileTestExecRequest) profileTestExecResult {
		rootInfo, err := os.Stat(req.Root)
		if err != nil {
			t.Errorf("受控工作区不存在: %v", err)
			return profileTestExecResult{ExitCode: 1}
		}
		settingsInfo, err := os.Stat(filepath.Join(req.Root, ".claude", "settings.json"))
		if err != nil {
			t.Errorf("工作区配置不存在: %v", err)
			return profileTestExecResult{ExitCode: 1}
		}
		permsOK = rootInfo.Mode().Perm() == 0o700 && settingsInfo.Mode().Perm() == 0o600
		return profileTestExecResult{ExitCode: 1, Err: fmt.Errorf("桩：故意失败")}
	}
	plan := runtimeVerifyPreview(t, srv, "claude", "")
	_ = runtimeVerifyApply(t, srv, "claude", rawString(t, plan, "previewId"))
	if !permsOK {
		t.Errorf("受控工作区权限不符（期望根 0700 / 配置 0600）")
	}

	// 崩溃残留清理：本功能的临时根前缀必须被纳管；无关目录一个字都不碰。
	tempRoot, err := os.MkdirTemp("", runtimeVerifyDirPrefix)
	if err != nil {
		t.Fatalf("建残留夹具失败: %v", err)
	}
	bystander, err := os.MkdirTemp("", "yzagmgr-other-feature-")
	if err != nil {
		t.Fatalf("建无关目录夹具失败: %v", err)
	}
	old := time.Now().Add(-2 * time.Hour)
	if err := os.Chtimes(tempRoot, old, old); err != nil {
		t.Fatalf("改残留夹具时间失败: %v", err)
	}
	if err := os.Chtimes(bystander, old, old); err != nil {
		t.Fatalf("改无关目录时间失败: %v", err)
	}
	srv.sweepStaleProfileTestRoots()
	if _, err := os.Stat(tempRoot); !os.IsNotExist(err) {
		t.Errorf("受控验证的崩溃残留未被清理")
	}
	if _, err := os.Stat(bystander); err != nil {
		t.Errorf("清理不得触碰无关目录: %v", err)
	}
	_ = os.RemoveAll(bystander)
}

// ── 标记注入（纯函数） ─────────────────────────────────────────────────────
//
// （声明映射子例已随 C 路模型映射删除——design §2.2；标记注入本身保留。）

func TestRuntimeVerifyMarkerInjection(t *testing.T) {
	if got := markModelName("claude-fable-5-1[1m]", "agv-abcdef123456"); got != "claude-fable-5-1-agv-abcdef123456[1m]" {
		t.Fatalf("markModelName = %q", got)
	}
	if got := markModelName("", "agv-x"); got != "" {
		t.Fatalf("空档名不得标记: %q", got)
	}
}

// ── 额度折算（纯函数） ─────────────────────────────────────────────────────

func TestRuntimeQuotaVerdictFromUsage(t *testing.T) {
	total := func(v int64) *int64 { return &v }
	cases := []struct {
		name string
		q    usage.Quota
		err  error
		want string
	}{
		{"未登记限额 ⇒ 充足（无记录不限）", usage.Quota{}, nil, runtimeQuotaSufficient},
		{"限额 100 用 50 ⇒ 充足", usage.Quota{QuotaTotal: total(100), RouterUsed: 50}, nil, runtimeQuotaSufficient},
		{"限额 100 用 100 ⇒ 用尽", usage.Quota{QuotaTotal: total(100), RouterUsed: 100}, nil, runtimeQuotaExhausted},
		{"限额 100 用 150 ⇒ 用尽", usage.Quota{QuotaTotal: total(100), RouterUsed: 150}, nil, runtimeQuotaExhausted},
		{"基线重置（D34）⇒ 生效用量 0 ⇒ 充足",
			usage.Quota{QuotaTotal: total(100), RouterUsed: 100, UsedBaseline: 100}, nil, runtimeQuotaSufficient},
		{"读数失败 ⇒ 未知（无法确定硬限）", usage.Quota{}, errors.New("storage exploded"), runtimeQuotaUnknown},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := quotaVerdictFromUsage(tc.q, tc.err).State; got != tc.want {
				t.Fatalf("判定 = %s，期望 %s", got, tc.want)
			}
		})
	}
}

// ── F1（B-3 阻断修复）：记账落盘失败 ⇒ 内存不得提交（apply/GET/重启三面同源） ──
//
// 落盘纪律：**先落盘、成功后才提交内存**。写失败（只读目录 / rename 失败 / 磁盘满）
// 时内存保持旧值，读取面继续以盘上真值为源；盘上不留半写文件（临时文件 + rename 的
// 失败路径必须清干净）。反向：正常落盘后三面（apply/GET/重启）逐字段同源。

// runtimeEvidenceLedgerPath 是证据记账的盘上位置（与 newRuntimeEvidenceStore 同源）。
func runtimeEvidenceLedgerPath(srv *contractServer) string {
	return filepath.Join(srv.env.ManagedDir(), "runtime-evidence.json")
}

// restartRuntimeEvidenceStore 用同一数据根重开证据记账（等价于守护进程重启后该账本的状态）。
func restartRuntimeEvidenceStore(t *testing.T, srv *contractServer) {
	t.Helper()
	reopened, err := newRuntimeEvidenceStore(srv.env)
	if err != nil {
		t.Fatalf("重启重开证据记账失败: %v", err)
	}
	srv.runtimeEvidence = reopened
}

// assertNoEvidenceTempResidue 断言记账目录里没有 saveLocked 的半写临时文件残渣。
func assertNoEvidenceTempResidue(t *testing.T, srv *contractServer) {
	t.Helper()
	entries, err := os.ReadDir(srv.env.ManagedDir())
	if err != nil {
		t.Fatalf("读取记账目录失败: %v", err)
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), ".runtime-evidence-") {
			t.Errorf("盘上残留半写临时文件 %s（失败路径必须把临时文件清掉）", entry.Name())
		}
	}
}

// readRuntimeEvidenceLedger 读盘上的记账文件（不存在即失败）。
func readRuntimeEvidenceLedger(t *testing.T, srv *contractServer) runtimeEvidenceDoc {
	t.Helper()
	var doc runtimeEvidenceDoc
	if err := json.Unmarshal(mustReadFile(t, runtimeEvidenceLedgerPath(srv)), &doc); err != nil {
		t.Fatalf("记账文件不是预期结构: %v", err)
	}
	return doc
}

// TestRuntimeEvidenceWriteFailureKeepsMemoryOnDiskTruth 覆盖只读目录注入（CreateTemp 失败）。
func TestRuntimeEvidenceWriteFailureKeepsMemoryOnDiskTruth(t *testing.T) {
	srv := runtimeVerifyFixture(t)
	stub := &runtimeVerifyObservedStub{markerful: true, pid: 71}
	srv.profileTestExecute = stub.exec(t, srv)
	plan := runtimeVerifyPreview(t, srv, "claude", "")

	managed := srv.env.ManagedDir()
	if err := os.Chmod(managed, 0o500); err != nil {
		t.Fatalf("置只读失败: %v", err)
	}
	restore := func() { _ = os.Chmod(managed, 0o700) }
	defer restore()

	payload := runtimeVerifyApply(t, srv, "claude", rawString(t, plan, "previewId"))
	if rawString(t, payload, "status") != "unconfirmed" || rawString(t, payload, "reason") != reasonWorkspaceFailed {
		t.Fatalf("证据写盘失败不得宣称已验证: %s", mustMarshal(t, payload))
	}
	if rawInt(t, payload, "modelCalls") != 1 {
		t.Fatalf("受控运行已发生 ⇒ 调用计数必须是 1: %s", mustMarshal(t, payload))
	}
	if got := atomic.LoadInt32(&stub.calls); got != 1 {
		t.Fatalf("执行桩应恰被调用 1 次（不重发），实际 %d", got)
	}

	// 同进程 GET：内存不得被写失败污染（F1 缺陷形态 = 这里谎报 verified）。
	got := runtimeEvidenceGet(t, srv, "claude")
	if rawString(t, got, "state") != "unconfirmed" || rawString(t, got, "reason") != reasonNeverVerified {
		t.Fatalf("写盘失败后同进程 GET 必须保持未确认（以盘上真值为源）: %s", mustMarshal(t, got))
	}
	if string(got["evidence"]) != "null" {
		t.Errorf("写盘失败时证据不得在读取面回流: %s", got["evidence"])
	}
	verify := mustObjectRaw(t, got, "verify")
	if rawInt(t, verify, "attemptCount") != 0 {
		t.Errorf("写盘失败的尝试不得留在内存（盘上不存在的幽灵尝试）: %s", mustMarshal(t, verify))
	}

	// 盘上无半写文件：临时残渣零、记账文件不存在（从未成功写过）。
	assertNoEvidenceTempResidue(t, srv)
	mustNotExist(t, runtimeEvidenceLedgerPath(srv), "写盘失败路径不得留下记账文件")

	// 重启后一致（读的只能是盘上真值）。
	restore()
	restartRuntimeEvidenceStore(t, srv)
	got = runtimeEvidenceGet(t, srv, "claude")
	if rawString(t, got, "state") != "unconfirmed" || rawString(t, got, "reason") != reasonNeverVerified {
		t.Fatalf("重启后读取面应与同进程一致: %s", mustMarshal(t, got))
	}
}

// TestRuntimeEvidenceRenameFailureDoesNotPoisonMemory 覆盖 rename 失败注入（记账路径被目录占位）。
func TestRuntimeEvidenceRenameFailureDoesNotPoisonMemory(t *testing.T) {
	srv := runtimeVerifyFixture(t)
	stub := &runtimeVerifyObservedStub{markerful: true, pid: 72}
	srv.profileTestExecute = stub.exec(t, srv)
	plan := runtimeVerifyPreview(t, srv, "claude", "")

	// 用目录占位记账路径：临时文件能建能写，最终 rename 必然失败。
	ledger := runtimeEvidenceLedgerPath(srv)
	if err := os.Mkdir(ledger, 0o700); err != nil {
		t.Fatalf("占位记账路径失败: %v", err)
	}

	payload := runtimeVerifyApply(t, srv, "claude", rawString(t, plan, "previewId"))
	if rawString(t, payload, "status") != "unconfirmed" || rawString(t, payload, "reason") != reasonWorkspaceFailed {
		t.Fatalf("rename 失败同样是写盘失败，不得宣称已验证: %s", mustMarshal(t, payload))
	}
	got := runtimeEvidenceGet(t, srv, "claude")
	if rawString(t, got, "state") != "unconfirmed" || rawString(t, got, "reason") != reasonNeverVerified {
		t.Fatalf("rename 失败后同进程 GET 必须保持未确认: %s", mustMarshal(t, got))
	}
	assertNoEvidenceTempResidue(t, srv)

	// 移除占位（磁盘恢复可用）⇒ 重启读盘上真值：与同进程判定一致。
	if err := os.RemoveAll(ledger); err != nil {
		t.Fatalf("移除占位失败: %v", err)
	}
	restartRuntimeEvidenceStore(t, srv)
	got = runtimeEvidenceGet(t, srv, "claude")
	if rawString(t, got, "state") != "unconfirmed" || rawString(t, got, "reason") != reasonNeverVerified {
		t.Fatalf("重启后应与同进程一致（盘上没有那份证据）: %s", mustMarshal(t, got))
	}

	// 反向：磁盘恢复后重新发起 ⇒ 升级成功且三面同源（apply/GET/重启）。
	plan2 := runtimeVerifyPreview(t, srv, "claude", "")
	payload = runtimeVerifyApply(t, srv, "claude", rawString(t, plan2, "previewId"))
	if rawString(t, payload, "status") != "verified" {
		t.Fatalf("磁盘恢复后重新发起应升级为已验证: %s", mustMarshal(t, payload))
	}
	got = runtimeEvidenceGet(t, srv, "claude")
	if rawString(t, got, "state") != "verified" {
		t.Fatalf("落盘成功后读取面应同源: %s", mustMarshal(t, got))
	}
	restartRuntimeEvidenceStore(t, srv)
	got = runtimeEvidenceGet(t, srv, "claude")
	if rawString(t, got, "state") != "verified" {
		t.Fatalf("重启后证据应存活（盘上真值）: %s", mustMarshal(t, got))
	}
}

// TestRuntimeEvidenceAttemptWriteFailureLeavesNoPhantomAttempt 覆盖 putAttempt 的同口径落盘纪律：
// 尝试写盘失败时内存不提交（不留盘上没有的幽灵尝试），恢复后记账恰有一条。
func TestRuntimeEvidenceAttemptWriteFailureLeavesNoPhantomAttempt(t *testing.T) {
	srv := runtimeVerifyFixture(t)
	srv.runtimeVerifyQuotaRead = func(context.Context) runtimeVerifyQuota {
		return runtimeVerifyQuota{State: runtimeQuotaExhausted, Source: "gateway", Detail: "窗口用尽"}
	}
	managed := srv.env.ManagedDir()
	if err := os.Chmod(managed, 0o500); err != nil {
		t.Fatalf("置只读失败: %v", err)
	}
	restore := func() { _ = os.Chmod(managed, 0o700) }
	defer restore()

	plan := runtimeVerifyPreview(t, srv, "claude", "")
	payload := runtimeVerifyApply(t, srv, "claude", rawString(t, plan, "previewId"))
	if rawString(t, payload, "reason") != reasonQuotaExhausted || rawInt(t, payload, "modelCalls") != 0 {
		t.Fatalf("缺额度必须拒绝且 0 调用: %s", mustMarshal(t, payload))
	}
	if rawInt(t, payload, "attemptCount") != 0 {
		t.Fatalf("记账写失败 ⇒ 内存不得留下盘上没有的幽灵尝试: %s", mustMarshal(t, payload))
	}
	got := runtimeEvidenceGet(t, srv, "claude")
	verify := mustObjectRaw(t, got, "verify")
	if rawInt(t, verify, "attemptCount") != 0 {
		t.Fatalf("写失败的尝试不得在读取面出现: %s", mustMarshal(t, verify))
	}

	// 反向：磁盘恢复后再拒绝一次 ⇒ 记账恰一条（不是两条），重启后仍是这条。
	restore()
	plan = runtimeVerifyPreview(t, srv, "claude", "")
	payload = runtimeVerifyApply(t, srv, "claude", rawString(t, plan, "previewId"))
	if rawInt(t, payload, "attemptCount") != 1 {
		t.Fatalf("恢复后落盘成功 ⇒ 记账应恰一条: %s", mustMarshal(t, payload))
	}
	var attempts []runtimeVerifyAttempt
	got = runtimeEvidenceGet(t, srv, "claude")
	verify = mustObjectRaw(t, got, "verify")
	if rawInt(t, verify, "attemptCount") != 1 {
		t.Fatalf("恢复后记账条数应为 1: %s", mustMarshal(t, verify))
	}
	if err := json.Unmarshal(verify["attempts"], &attempts); err != nil {
		t.Fatalf("尝试历史不是预期结构: %v", err)
	}
	if len(attempts) != 1 || attempts[0].Reason != reasonQuotaExhausted || attempts[0].ModelCalls != 0 {
		t.Errorf("恢复后尝试历史应恰为那条拒绝（无幽灵条目）: %+v", attempts)
	}
	restartRuntimeEvidenceStore(t, srv)
	got = runtimeEvidenceGet(t, srv, "claude")
	verify = mustObjectRaw(t, got, "verify")
	if rawInt(t, verify, "attemptCount") != 1 {
		t.Fatalf("重启后记账条数仍应为 1: %s", mustMarshal(t, verify))
	}
	doc := readRuntimeEvidenceLedger(t, srv)
	if rec := doc.Tools["claude"]; len(rec.Attempts) != 1 || rec.Attempts[0].Reason != reasonQuotaExhausted || rec.Attempts[0].ModelCalls != 0 {
		t.Errorf("盘上尝试历史不符: %+v", rec.Attempts)
	}
}

// TestRuntimeEvidenceThreeFacesShareOneSource 是 F1 的反向回归：正常落盘后
// apply 响应 / GET 读取面 / 重启后读取面**逐字段同源**（都对同一份盘上真值）。
func TestRuntimeEvidenceThreeFacesShareOneSource(t *testing.T) {
	srv := runtimeVerifyFixture(t)
	stub := &runtimeVerifyObservedStub{markerful: true, pid: 4242}
	srv.profileTestExecute = stub.exec(t, srv)

	plan := runtimeVerifyPreview(t, srv, "claude", "")
	payload := runtimeVerifyApply(t, srv, "claude", rawString(t, plan, "previewId"))
	if rawString(t, payload, "status") != "verified" {
		t.Fatalf("夹具未达到已验证: %s", mustMarshal(t, payload))
	}
	var applyEv runtimeEvidenceRecord
	if err := json.Unmarshal(payload["evidence"], &applyEv); err != nil {
		t.Fatalf("apply 证据不是预期结构: %v", err)
	}

	got := runtimeEvidenceGet(t, srv, "claude")
	if rawString(t, got, "state") != "verified" {
		t.Fatalf("落盘成功后 GET 应同源 verified: %s", mustMarshal(t, got))
	}
	var getEv runtimeEvidenceRecord
	if err := json.Unmarshal(got["evidence"], &getEv); err != nil {
		t.Fatalf("GET 证据不是预期结构: %v", err)
	}
	if !reflect.DeepEqual(applyEv, getEv) {
		t.Errorf("apply 与 GET 的证据必须逐字段同源:\napply=%+v\nget=%+v", applyEv, getEv)
	}

	// 盘上真值：与两个面逐字段一致（含尝试历史）。
	doc := readRuntimeEvidenceLedger(t, srv)
	rec := doc.Tools["claude"]
	if rec.Evidence == nil || !reflect.DeepEqual(*rec.Evidence, applyEv) {
		t.Errorf("盘上证据与响应不同源: %+v", rec.Evidence)
	}
	if len(rec.Attempts) != 1 || rec.Attempts[0].Reason != reasonVerified || rec.Attempts[0].ModelCalls != 1 {
		t.Errorf("盘上尝试历史不符: %+v", rec.Attempts)
	}

	// 重启后一致（证据存活、判定不变）。
	restartRuntimeEvidenceStore(t, srv)
	got = runtimeEvidenceGet(t, srv, "claude")
	if rawString(t, got, "state") != "verified" {
		t.Fatalf("重启后证据应存活: %s", mustMarshal(t, got))
	}
	var restartedEv runtimeEvidenceRecord
	if err := json.Unmarshal(got["evidence"], &restartedEv); err != nil {
		t.Fatalf("重启后证据不是预期结构: %v", err)
	}
	if !reflect.DeepEqual(applyEv, restartedEv) {
		t.Errorf("重启后的证据必须与 apply 同源:\napply=%+v\nrestarted=%+v", applyEv, restartedEv)
	}
	verify := mustObjectRaw(t, got, "verify")
	if rawInt(t, verify, "attemptCount") != 1 {
		t.Errorf("重启后尝试历史应存活: %s", mustMarshal(t, verify))
	}
}

// ── F2（B-3 数据面修复）：漂移早退也给出真实声明映射与估算，阻塞一次列全 ──────

// driftClaudeSettings 把 claude 的盘上配置外改一个字节（制造 config_changed 漂移）。
func driftClaudeSettings(t *testing.T, srv *contractServer) {
	t.Helper()
	settings := srv.env.ClaudeSettings()
	blob := mustReadFile(t, settings)
	if err := os.WriteFile(settings, append(blob, []byte("\n")...), 0o600); err != nil {
		t.Fatalf("外改 live 失败: %v", err)
	}
}

// TestRuntimeVerifyDriftPreviewKeepsDataFace：漂移态预检的估算行 = 真实口径
// （与新鲜态逐字段同源），且绝不出现「输出 ≤ 0 tokens」。
// （声明映射行已随 C 路模型映射删除——design §2.2。）
func TestRuntimeVerifyDriftPreviewKeepsDataFace(t *testing.T) {
	srv := runtimeVerifyFixture(t)
	// 新鲜态快照（同一夹具，未漂移）。
	fresh := runtimeVerifyPreview(t, srv, "claude", "")
	freshEstimate := mustObjectRaw(t, fresh, "estimate")

	driftClaudeSettings(t, srv)
	drift := runtimeVerifyPreview(t, srv, "claude", "")
	blockers := runtimeEvidenceBlockers(t, drift)
	if len(blockers) == 0 || blockers[0] != reasonConfigChanged {
		t.Fatalf("漂移态第一阻塞应为 config_changed: %v", blockers)
	}
	// 阻塞清单 = 全量：额度充足 + 报价已知 ⇒ 恰一条（漂移本身）。
	if len(blockers) != 1 {
		t.Fatalf("漂移态阻塞清单应恰为 [config_changed]（额度充足/报价已知）: %v", blockers)
	}

	// 估算行 = 真实口径：与新鲜态逐字段相等、输出上界非零且进 basis。
	driftEstimate := mustObjectRaw(t, drift, "estimate")
	for _, key := range []string{"quote", "currency", "estimatedCostUsd", "upperBoundUsd", "inputTokensBound", "outputTokensBound", "basis"} {
		if string(driftEstimate[key]) != string(freshEstimate[key]) {
			t.Errorf("漂移态估算字段 %s 应与新鲜态同源: drift=%s fresh=%s",
				key, driftEstimate[key], freshEstimate[key])
		}
	}
	if rawString(t, driftEstimate, "quote") != "known" {
		t.Fatalf("夹具报价已知，漂移态不得把报价呈现为未知: %s", mustMarshal(t, driftEstimate))
	}
	bound := rawInt(t, driftEstimate, "outputTokensBound")
	if bound <= 0 {
		t.Fatalf("漂移态输出上界不得回零值: %s", mustMarshal(t, driftEstimate))
	}
	if basis := rawString(t, driftEstimate, "basis"); !strings.Contains(basis, itoa(bound)) {
		t.Errorf("估算口径应含真实输出上界 %d: %s", bound, basis)
	}
}

// TestRuntimeVerifyDriftPreviewListsGateBlockers：漂移态把额度/报价阻塞与漂移一并列出
// （一次列全、最具体原因排第一）；报价未知时金额显式 null，但输出上界仍按真实口径。
func TestRuntimeVerifyDriftPreviewListsGateBlockers(t *testing.T) {
	srv := runtimeVerifyFixture(t)
	srv.runtimeVerifyQuotaRead = func(context.Context) runtimeVerifyQuota {
		return runtimeVerifyQuota{State: runtimeQuotaUnknown, Source: "gateway", Detail: "未登录（测试）"}
	}
	if _, err := srv.Usage.ReplacePricing(context.Background(), usage.PricingTable{}); err != nil {
		t.Fatalf("清空定价失败: %v", err)
	}
	driftClaudeSettings(t, srv)
	drift := runtimeVerifyPreview(t, srv, "claude", "")
	blockers := runtimeEvidenceBlockers(t, drift)
	want := []string{reasonConfigChanged, reasonQuotaUnknown, reasonQuoteUnknown}
	if !reflect.DeepEqual(blockers, want) {
		t.Fatalf("漂移态阻塞清单应为 %v（一次列全、最具体原因排第一）: %v", want, blockers)
	}

	// 估算行：报价未知 ⇒ 两个金额显式 null（不是 0），quote=unknown。
	estimate := mustObjectRaw(t, drift, "estimate")
	if rawString(t, estimate, "quote") != "unknown" {
		t.Errorf("报价未知时 quote 必须为 unknown: %s", mustMarshal(t, estimate))
	}
	if string(estimate["estimatedCostUsd"]) != "null" || string(estimate["upperBoundUsd"]) != "null" {
		t.Errorf("报价未知时金额必须显式 null（不伪装免费，也不假报 0）: %s", mustMarshal(t, estimate))
	}
	// 但输出上界仍按真实口径（目标模型可定）：非零、进 basis、不出现「输出 ≤ 0 tokens」。
	bound := rawInt(t, estimate, "outputTokensBound")
	if bound <= 0 {
		t.Fatalf("目标模型可定时输出上界不得回零值: %s", mustMarshal(t, estimate))
	}
	basis := rawString(t, estimate, "basis")
	if !strings.Contains(basis, itoa(bound)) || strings.Contains(basis, "输出 ≤ 0 tokens") {
		t.Errorf("估算口径应含真实上界 %d 且不得出现 0 上界: %s", bound, basis)
	}
}

// ── O1（B-3 口径修复）：闸门拒绝在操作日志里同类同码（quota_exceeded） ────────

// TestRuntimeVerifyFailureCodeMapping 是码表断言：全部闸门前置拒绝（未发起任何上游
// 请求）同类同码；执行侧失败折 internal（与 §4.6 同口径）。
func TestRuntimeVerifyFailureCodeMapping(t *testing.T) {
	cases := []struct {
		reason string
		want   apierr.Code
	}{
		{reasonQuotaExhausted, apierr.CodeQuotaExceeded},
		{reasonQuotaUnknown, apierr.CodeQuotaExceeded},
		{reasonQuoteUnknown, apierr.CodeQuotaExceeded},
		{reasonBudgetExceeded, apierr.CodeQuotaExceeded},
		{reasonVerifyFailed, apierr.CodeInternal},
		{reasonVerifyTimeout, apierr.CodeInternal},
		{reasonObservationMissing, apierr.CodeInternal},
		{reasonUpstreamRejected, apierr.CodeInternal},
		{reasonMappingMismatch, apierr.CodeInternal},
	}
	for _, tc := range cases {
		if got := runtimeVerifyFailureCode(tc.reason); got != tc.want {
			t.Errorf("runtimeVerifyFailureCode(%s) = %s，期望 %s", tc.reason, got, tc.want)
		}
	}
}

// TestRuntimeVerifyGateRefusalLogsQuotaExceeded 走真实链路核对操作日志：报价未知的
// 闸门拒绝在日志里记 quota_exceeded（与额度类拒绝同类同码，不再落 internal）。
func TestRuntimeVerifyGateRefusalLogsQuotaExceeded(t *testing.T) {
	srv := runtimeVerifyFixture(t)
	if _, err := srv.Usage.ReplacePricing(context.Background(), usage.PricingTable{}); err != nil {
		t.Fatalf("清空定价失败: %v", err)
	}
	plan := runtimeVerifyPreview(t, srv, "claude", "")
	payload := runtimeVerifyApply(t, srv, "claude", rawString(t, plan, "previewId"))
	if rawString(t, payload, "reason") != reasonQuoteUnknown || rawInt(t, payload, "modelCalls") != 0 {
		t.Fatalf("未知报价必须拒绝且 0 调用: %s", mustMarshal(t, payload))
	}

	res := srv.callRaw(t, http.MethodGet,
		"/api/v1/operations?action=runtime-evidence.verify.apply&result=failed&limit=50", "", true)
	res.assertEnvelope(t, http.MethodGet, "/operations")
	if !res.ok {
		t.Fatalf("读取操作日志失败: %s", res.body)
	}
	var page struct {
		Items []struct {
			Action    string `json:"action"`
			Target    string `json:"target"`
			Result    string `json:"result"`
			ErrorCode string `json:"errorCode"`
		} `json:"items"`
	}
	if err := json.Unmarshal(res.data, &page); err != nil {
		t.Fatalf("操作日志页不是预期结构: %v（%s）", err, string(res.data))
	}
	matched := false
	for _, item := range page.Items {
		if item.Action != "runtime-evidence.verify.apply" || item.Target != "claude" {
			continue
		}
		matched = true
		if item.Result != "failed" || item.ErrorCode != string(apierr.CodeQuotaExceeded) {
			t.Errorf("闸门拒绝在操作日志里应记 %s: %+v", apierr.CodeQuotaExceeded, item)
		}
	}
	if !matched {
		t.Fatalf("操作日志里没有本次 apply 的条目（共 %d 条）", len(page.Items))
	}
}
