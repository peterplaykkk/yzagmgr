package api

import (
	"context"
	"encoding/json"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/peterplaykkk/yzagmgr/internal/usage"
)

// 本文件覆盖监控只读面（§14）的接线：裸 JSON、令牌必需、来源检查生效、
// 数据面臂路径与静态站的分派边界。

// monitorResult 是一次监控面响应（裸 JSON，不裹信封）。
type monitorResult struct {
	status int
	body   string
	header http.Header
}

// callMonitor 发一次监控面请求（withToken 决定是否带管理令牌）。
func (s *contractServer) callMonitor(t *testing.T, method, path string, withToken bool) monitorResult {
	t.Helper()
	req := httptest.NewRequest(method, path, nil)
	if withToken {
		req.Header.Set("X-Agmgr-Token", s.token)
	}
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)
	res := rec.Result()
	return monitorResult{status: res.StatusCode, body: rec.Body.String(), header: res.Header}
}

// seedMonitorArm 往用量库里写一条带臂标识的记录（时间取「刚刚」，落进 1h 窗口）。
func seedMonitorArm(t *testing.T, srv *contractServer, id, user, session string, out int64, durationMs int64) {
	t.Helper()
	if _, err := srv.Usage.Record(context.Background(), usage.Record{
		ID: id, At: time.Now().Add(-time.Minute), App: "claude", Prefix: "glm", Provider: "glm",
		User: user, Session: session,
		ModelRequested: "glm-5.3-flash", ModelUpstream: "glm-5.3-flash",
		Status: 200, DurationMs: durationMs, InputTokens: 100, OutputTokens: out, Streamed: true,
	}); err != nil {
		t.Fatalf("写入用量记录失败: %v", err)
	}
}

// TestMonitorOverviewIsBareJSON 覆盖形态选择：`/monitor/*` 的响应体**就是指标对象**
// （没有 /api/v1 的 ok/data 信封），且字典分组齐全。
func TestMonitorOverviewIsBareJSON(t *testing.T) {
	srv := newContractServer(t)
	seedMonitorArm(t, srv, "rq_mon1", "user-a", "sess-1", 120, 2000)

	res := srv.callMonitor(t, http.MethodGet, "/monitor/overview?window=1h", true)
	if res.status != http.StatusOK {
		t.Fatalf("状态 = %d：%s", res.status, res.body)
	}
	if ct := res.header.Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Fatalf("Content-Type = %q", ct)
	}
	if res.header.Get("X-Request-Id") == "" {
		t.Fatal("监控面同样要带 X-Request-Id（可检索）")
	}
	var payload map[string]json.RawMessage
	if err := json.Unmarshal([]byte(res.body), &payload); err != nil {
		t.Fatalf("响应不是 JSON 对象: %v（%s）", err, res.body)
	}
	if _, ok := payload["ok"]; ok {
		t.Fatalf("监控面不裹信封（不得出现 ok 字段）：%s", res.body)
	}
	for _, group := range []string{"records", "performance", "aggregate", "trend", "model_analysis", "session_axis"} {
		if _, ok := payload[group]; !ok {
			t.Fatalf("字典缺少分组 %s：%s", group, res.body)
		}
	}
	var window string
	if err := json.Unmarshal(payload["window"], &window); err != nil || window != "1h" {
		t.Fatalf("window 回显不符：%s", res.body)
	}
}

// TestMonitorEndpointsRequireToken 覆盖「不经 /api/v1 不等于免鉴权」：
// 缺令牌 401；带对令牌 200；跨站来源 403。
func TestMonitorEndpointsRequireToken(t *testing.T) {
	srv := newContractServer(t)
	seedMonitorArm(t, srv, "rq_mon2", "user-a", "sess-1", 60, 1000)

	paths := []string{
		"/monitor/overview?window=1h",
		"/monitor/trends?metric=rpm&window=1h&bucket=minute",
		"/monitor/session-cost?user=user-a&session=sess-1",
	}
	for _, path := range paths {
		res := srv.callMonitor(t, http.MethodGet, path, false)
		if res.status != http.StatusUnauthorized {
			t.Fatalf("%s 缺令牌应 401，实际 %d（%s）", path, res.status, res.body)
		}
		if !strings.Contains(res.body, `"code":"unauthorized"`) {
			t.Fatalf("%s 的 401 应是裸错误对象：%s", path, res.body)
		}
		res = srv.callMonitor(t, http.MethodGet, path, true)
		if res.status != http.StatusOK {
			t.Fatalf("%s 带令牌应 200，实际 %d（%s）", path, res.status, res.body)
		}
	}

	// 跨站来源：与 /api/v1 同一套守卫（带 Origin 且非本机管理页 ⇒ forbidden_origin）。
	req := httptest.NewRequest(http.MethodGet, "/monitor/overview", nil)
	req.Header.Set("X-Agmgr-Token", srv.token)
	req.Header.Set("Origin", "https://evil.example")
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), "forbidden_origin") {
		t.Fatalf("跨站来源应 403 forbidden_origin，实际 %d（%s）", rec.Code, rec.Body.String())
	}
	// 本机管理页来源放行。
	req = httptest.NewRequest(http.MethodGet, "/monitor/overview", nil)
	req.Header.Set("X-Agmgr-Token", srv.token)
	req.Header.Set("Origin", "http://127.0.0.1:17022")
	rec = httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("本机来源应放行，实际 %d（%s）", rec.Code, rec.Body.String())
	}
}

// TestMonitorTrendsAndSessionCostReadouts 覆盖另两个端点的接线与口径字段。
func TestMonitorTrendsAndSessionCostReadouts(t *testing.T) {
	srv := newContractServer(t)
	seedMonitorArm(t, srv, "rq_mon3", "user-a", "sess-1", 100, 1000)
	seedMonitorArm(t, srv, "rq_mon4", "user-a", "sess-1", 50, 1000)

	res := srv.callMonitor(t, http.MethodGet,
		"/monitor/trends?metric=rpm&window=1h&bucket=minute&user=user-a", true)
	if res.status != http.StatusOK {
		t.Fatalf("trends 状态 = %d：%s", res.status, res.body)
	}
	var trends struct {
		Metric string `json:"metric"`
		Bucket string `json:"bucket"`
		Points []struct {
			TS    string  `json:"ts"`
			Value float64 `json:"value"`
		} `json:"points"`
		Missing bool `json:"missing"`
	}
	if err := json.Unmarshal([]byte(res.body), &trends); err != nil {
		t.Fatalf("trends 不是预期形状: %v（%s）", err, res.body)
	}
	if trends.Metric != "rpm" || trends.Bucket != "minute" || len(trends.Points) != 60 {
		t.Fatalf("trends 形状不符：%+v", trends)
	}
	// 两条记录落在同一分钟 ⇒ 该分钟 rpm = 2，其余为 0。
	var peak float64
	for _, p := range trends.Points {
		if p.Value > peak {
			peak = p.Value
		}
	}
	if peak != 2 {
		t.Fatalf("rpm 峰值 = %v，期望 2", peak)
	}

	res = srv.callMonitor(t, http.MethodGet, "/monitor/session-cost?user=user-a&session=sess-1", true)
	if res.status != http.StatusOK {
		t.Fatalf("session-cost 状态 = %d：%s", res.status, res.body)
	}
	var cost struct {
		MaxTokS      *float64 `json:"max_tok_s"`
		AvgTokS      *float64 `json:"avg_tok_s"`
		OutputTokens *int64   `json:"output_tokens"`
		Turns        *int64   `json:"turns"`
		Window       string   `json:"window"`
		Provider     string   `json:"provider"`
		Model        string   `json:"model"`
		FetchedAt    string   `json:"fetched_at"`
	}
	if err := json.Unmarshal([]byte(res.body), &cost); err != nil {
		t.Fatalf("session-cost 不是预期形状: %v（%s）", err, res.body)
	}
	if cost.Turns == nil || *cost.Turns != 2 || cost.OutputTokens == nil || *cost.OutputTokens != 150 {
		t.Fatalf("会话速率计数不符：%+v", cost)
	}
	if cost.MaxTokS == nil || *cost.MaxTokS != 100 || cost.AvgTokS == nil || *cost.AvgTokS != 75 {
		t.Fatalf("会话速率口径不符：%+v", cost)
	}
	if cost.Window != "all" || cost.Provider != "glm" || cost.FetchedAt == "" {
		t.Fatalf("会话速率的窗口/提供方/取数时刻不符：%+v", cost)
	}

	// 无计量：200 且数值为 null（调用方读成 no_data，不报错）。
	res = srv.callMonitor(t, http.MethodGet, "/monitor/session-cost?user=nobody&session=nosuch", true)
	if res.status != http.StatusOK {
		t.Fatalf("无计量应 200，实际 %d（%s）", res.status, res.body)
	}
	var empty map[string]json.RawMessage
	if err := json.Unmarshal([]byte(res.body), &empty); err != nil {
		t.Fatalf("无计量响应不是 JSON: %v", err)
	}
	for _, key := range []string{"max_tok_s", "avg_tok_s", "output_tokens", "turns"} {
		if string(empty[key]) != "null" {
			t.Fatalf("无计量时 %s 应为 null，实际 %s", key, empty[key])
		}
	}
}

// TestMonitorRejectsUnknownPathsAndMethods 覆盖这一族的封闭形状：未知端点 404、非 GET 405。
func TestMonitorRejectsUnknownPathsAndMethods(t *testing.T) {
	srv := newContractServer(t)
	res := srv.callMonitor(t, http.MethodGet, "/monitor/nope", true)
	if res.status != http.StatusNotFound || !strings.Contains(res.body, `"code":"not_found"`) {
		t.Fatalf("未知监控端点应 404 裸错误，实际 %d（%s）", res.status, res.body)
	}
	res = srv.callMonitor(t, http.MethodPost, "/monitor/overview", true)
	if res.status != http.StatusMethodNotAllowed {
		t.Fatalf("非 GET 应 405，实际 %d（%s）", res.status, res.body)
	}
	// 非法入参：错误码取自同一封闭集合（invalid_request）。
	res = srv.callMonitor(t, http.MethodGet, "/monitor/overview?window=3h", true)
	if res.status != http.StatusBadRequest || !strings.Contains(res.body, `"code":"invalid_request"`) {
		t.Fatalf("非法窗口应 400 invalid_request，实际 %d（%s）", res.status, res.body)
	}
	// 未知指标不回落默认序列。
	res = srv.callMonitor(t, http.MethodGet, "/monitor/trends?metric=nope&window=1h", true)
	if res.status != http.StatusBadRequest {
		t.Fatalf("未知指标应 400，实际 %d（%s）", res.status, res.body)
	}
}

// TestMonitorUnavailableReportsStorage 覆盖用量面不可用时的行为：
// 明确报 storage（503），**不回**空指标（空指标会被读成「没有流量」）。
func TestMonitorUnavailableReportsStorage(t *testing.T) {
	srv := newContractServer(t)
	srv.Monitor = nil // 用量库打开失败时的装配结果
	res := srv.callMonitor(t, http.MethodGet, "/monitor/overview", true)
	if res.status != http.StatusServiceUnavailable {
		t.Fatalf("状态 = %d，期望 503", res.status)
	}
	if !strings.Contains(res.body, `"code":"storage"`) {
		t.Fatalf("应报 storage：%s", res.body)
	}
}

// TestDataPlaneArmPathsAreDispatchedToProxy 覆盖分派边界：臂路径必须落到数据面
// （否则会被静态站吞成 200 HTML —— 那会让「配错了」看起来像「配对了」）。
func TestDataPlaneArmPathsAreDispatchedToProxy(t *testing.T) {
	srv := newContractServer(t)
	// 形状合法但上游不可达：数据面回 502 JSON（不是 SPA 的 index.html）。
	res := srv.callMonitor(t, http.MethodPost, "/u/user-a/sess-1/glm/v1/messages", false)
	if res.status != http.StatusBadGateway {
		t.Fatalf("臂请求应落到数据面（上游不可达 502），实际 %d（%s）", res.status, res.body)
	}
	if strings.Contains(res.body, "<html") {
		t.Fatalf("不得回落到静态站：%s", res.body)
	}
	// 形状非法：数据面自己回答 404 JSON（不是 SPA）。
	res = srv.callMonitor(t, http.MethodPost, "/u/only-one-segment", false)
	if res.status != http.StatusNotFound || !strings.Contains(res.body, `"code":"not_found"`) {
		t.Fatalf("形状非法的臂路径应 404 JSON，实际 %d（%s）", res.status, res.body)
	}
	// 单段臂形态（`u-...`）同样归数据面。
	res = srv.callMonitor(t, http.MethodPost, "/u-user-a-sess-1-glm/v1/messages", false)
	if res.status != http.StatusBadGateway {
		t.Fatalf("单段臂请求应落到数据面（上游不可达 502），实际 %d（%s）", res.status, res.body)
	}
	// 规范形态挂在 /router 下：必须同样归数据面（否则会被静态站吞成 200 HTML）。
	res = srv.callMonitor(t, http.MethodPost, "/router/u-user-a-9f2c1a4b7e0d3c55-glm/v1/messages", false)
	if res.status != http.StatusBadGateway {
		t.Fatalf("/router 规范臂请求应落到数据面，实际 %d（%s）", res.status, res.body)
	}
	if strings.Contains(res.body, "<html") {
		t.Fatalf("不得回落到静态站：%s", res.body)
	}
	// 九前缀仍在原地（对照组）。
	res = srv.callMonitor(t, http.MethodGet, "/nope/v1/models", false)
	if res.status != http.StatusNotFound {
		t.Fatalf("未知前缀仍应 404，实际 %d", res.status)
	}
}

// TestWriteMonitorBodySurvivesUnencodablePayload 覆盖序列化失败这条防御分支：
// 响应体绝不能写出半个 JSON，失败时退化成一行固定错误对象。
func TestWriteMonitorBodySurvivesUnencodablePayload(t *testing.T) {
	rec := httptest.NewRecorder()
	writeMonitorBody(rec, http.StatusOK, map[string]any{"value": math.NaN()})
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("序列化失败应改用 500，实际 %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "序列化失败") {
		t.Fatalf("应给出可判读的失败文本：%s", rec.Body.String())
	}
	var parsed map[string]json.RawMessage
	if err := json.Unmarshal(rec.Body.Bytes(), &parsed); err != nil {
		t.Fatalf("失败体也必须是合法 JSON：%v（%s）", err, rec.Body.String())
	}
}

// TestRequestIDOfFallsBackWhenHeaderMissing 覆盖「响应头里没有 requestId」的兜底：
// 现生成一个可检索的 id，而不是空串。
func TestRequestIDOfFallsBackWhenHeaderMissing(t *testing.T) {
	rec := httptest.NewRecorder()
	if got := requestIDOf(rec); !strings.HasPrefix(got, "req_") {
		t.Fatalf("兜底应现生成 requestId，实际 %q", got)
	}
	rec.Header().Set("X-Request-Id", "req_fixed")
	if got := requestIDOf(rec); got != "req_fixed" {
		t.Fatalf("已有 requestId 时应原样返回，实际 %q", got)
	}
}
