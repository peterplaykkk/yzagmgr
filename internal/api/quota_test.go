package api

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/peterplaykkk/yzagmgr/internal/usage"
)

// 本文件覆盖限额执行面的管理端点（§15 / D32 / UC-041）：`GET/PUT /quota/{slug}`。
//
// 三条守则：
//   - 形状是机器可判定的：裸 JSON、**恰好**五个字段、null 与 0 分得开；
//   - 写是整值覆盖且幂等，PUT 的响应就是写入后的读数（回读一致）；
//   - 「PUT 立即生效」必须走**真实链路**验证：改完限额后，同一条数据面臂请求
//     的判定随之改变（不是只断言响应体好看）。

// quotaPayload 是限额面响应的严格解读（字段名按契约逐字）。
type quotaPayload struct {
	Quota        *int64 `json:"quota"`
	UsedRouter   int64  `json:"used_router"`
	UsedExternal int64  `json:"used_external"`
	UsedTotal    int64  `json:"used_total"`
	UsedBaseline int64  `json:"used_baseline"`
	UsedEff      int64  `json:"used_eff"`
	Remaining    *int64 `json:"remaining"`
}

// callQuota 发一次限额面请求（裸 JSON；withToken 决定是否带管理令牌）。
func (s *contractServer) callQuota(t *testing.T, method, path, body string, withToken bool) monitorResult {
	t.Helper()
	var reader io.Reader
	if body != "" {
		reader = strings.NewReader(body)
	}
	req := httptest.NewRequest(method, path, reader)
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	if withToken {
		req.Header.Set("X-Agmgr-Token", s.token)
	}
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)
	res := rec.Result()
	return monitorResult{status: res.StatusCode, body: rec.Body.String(), header: res.Header}
}

// decodeQuota 解析限额面响应并断言形状（恰好五个字段）。
func decodeQuota(t *testing.T, res monitorResult) quotaPayload {
	t.Helper()
	var raw map[string]json.RawMessage
	if err := json.Unmarshal([]byte(res.body), &raw); err != nil {
		t.Fatalf("限额响应不是 JSON 对象: %v（%s）", err, res.body)
	}
	if len(raw) != 7 {
		t.Fatalf("限额响应必须恰好七个字段，实际 %d：%s", len(raw), res.body)
	}
	for _, key := range []string{"quota", "used_router", "used_external", "used_total", "used_baseline", "used_eff", "remaining"} {
		if _, ok := raw[key]; !ok {
			t.Fatalf("限额响应缺少 %s：%s", key, res.body)
		}
	}
	if _, ok := raw["ok"]; ok {
		t.Fatalf("限额面不裹信封（不得出现 ok 字段）：%s", res.body)
	}
	var payload quotaPayload
	if err := json.Unmarshal([]byte(res.body), &payload); err != nil {
		t.Fatalf("限额响应形状不符: %v（%s）", err, res.body)
	}
	return payload
}

// seedQuotaUsage 往用量库里写一条带臂标识的记录（token 由用例给足）。
func seedQuotaUsage(t *testing.T, srv *contractServer, id, user string, in, out int64) {
	t.Helper()
	if _, err := srv.Usage.Record(context.Background(), usage.Record{
		ID: id, At: time.Now().Add(-time.Minute), App: "codex", Prefix: "glm", Provider: "glm",
		User: user, Session: "9f2c1a4b7e0d3c55",
		ModelRequested: "glm-5.3-flash", ModelUpstream: "glm-5.3-flash",
		Status: 200, DurationMs: 1000, InputTokens: in, OutputTokens: out, Streamed: true,
	}); err != nil {
		t.Fatalf("写入用量记录失败: %v", err)
	}
}

// quotaArmPath 是判定用例里用的臂路径（user 段与限额 slug 必须一致）。
func quotaArmPath(user string) string {
	return "/u/" + user + "/9f2c1a4b7e0d3c55/glm/v1/messages"
}

// TestQuotaGetShapeAndPutApplies 覆盖 GET 形状与 PUT 的即时回读：
// 无记录 ⇒ quota/remaining 为 null 但已用量照实；写入后 GET 立刻反映新值。
func TestQuotaGetShapeAndPutApplies(t *testing.T) {
	srv := newContractServer(t)
	seedQuotaUsage(t, srv, "rq_qp01", "user-a", 700, 300)

	res := srv.callQuota(t, http.MethodGet, "/quota/user-a", "", true)
	if res.status != http.StatusOK {
		t.Fatalf("GET 状态 = %d：%s", res.status, res.body)
	}
	if ct := res.header.Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Fatalf("Content-Type = %q", ct)
	}
	if res.header.Get("X-Request-Id") == "" {
		t.Fatal("限额面同样要带 X-Request-Id（可检索）")
	}
	got := decodeQuota(t, res)
	if got.Quota != nil || got.Remaining != nil {
		t.Fatalf("无记录时 quota/remaining 必须是 null：%s", res.body)
	}
	if got.UsedRouter != 1000 || got.UsedExternal != 0 || got.UsedTotal != 1000 {
		t.Fatalf("已用量口径不符：%s", res.body)
	}

	// PUT 回读一致：响应就是写入后的读数。
	put := srv.callQuota(t, http.MethodPut, "/quota/user-a", `{"quota_total":1000}`, true)
	if put.status != http.StatusOK {
		t.Fatalf("PUT 状态 = %d：%s", put.status, put.body)
	}
	written := decodeQuota(t, put)
	if written.Quota == nil || *written.Quota != 1000 {
		t.Fatalf("PUT 响应里的 quota 不符：%s", put.body)
	}
	if written.Remaining == nil || *written.Remaining != 0 {
		t.Fatalf("恰好用满时 remaining 应为 0：%s", put.body)
	}
	// GET 与 PUT 的响应逐字相同（回读一致，不是两次独立算法）。
	again := srv.callQuota(t, http.MethodGet, "/quota/user-a", "", true)
	if again.body != put.body {
		t.Fatalf("GET 与 PUT 的读数不一致：\nGET  %s\nPUT  %s", again.body, put.body)
	}
}

// TestQuotaPutIsIdempotentAndExternalIsOptional 覆盖写语义：整值覆盖、可重放、
// 省略 external_used 时不动外部折算、0=不限（呈现为 0 而不是 null）。
func TestQuotaPutIsIdempotentAndExternalIsOptional(t *testing.T) {
	srv := newContractServer(t)
	seedQuotaUsage(t, srv, "rq_qp02", "user-a", 100, 100)

	first := srv.callQuota(t, http.MethodPut, "/quota/user-a", `{"quota_total":100000,"external_used":250}`, true)
	if first.status != http.StatusOK {
		t.Fatalf("首次 PUT 状态 = %d：%s", first.status, first.body)
	}
	if p := decodeQuota(t, first); p.UsedExternal != 250 || p.UsedTotal != 450 {
		t.Fatalf("外部折算未按整值写入：%s", first.body)
	}
	// 幂等：同值重放逐字相同。
	replay := srv.callQuota(t, http.MethodPut, "/quota/user-a", `{"quota_total":100000,"external_used":250}`, true)
	if replay.body != first.body {
		t.Fatalf("同值重放不得改变读数：\n%s\n%s", replay.body, first.body)
	}
	// 省略 external_used：保持原值。
	onlyTotal := srv.callQuota(t, http.MethodPut, "/quota/user-a", `{"quota_total":200000}`, true)
	if p := decodeQuota(t, onlyTotal); p.UsedExternal != 250 || *p.Quota != 200000 {
		t.Fatalf("省略 external_used 时应保留原值：%s", onlyTotal.body)
	}
	// 基线（D34）：整值覆盖；省略时保留（省略绝不是清零）。
	withBaseline := srv.callQuota(t, http.MethodPut, "/quota/user-a",
		`{"quota_total":200000,"used_baseline":150}`, true)
	// 已用毛值 200（路由器）+ 250（外部折算）= 450，扣掉基线 150 ⇒ 生效用量 300。
	if p := decodeQuota(t, withBaseline); p.UsedBaseline != 150 || p.UsedEff != 300 || p.UsedTotal != 450 {
		t.Fatalf("基线应整值写入并计入 used_eff：%s", withBaseline.body)
	}
	replayBaseline := srv.callQuota(t, http.MethodPut, "/quota/user-a",
		`{"quota_total":200000,"used_baseline":150}`, true)
	if replayBaseline.body != withBaseline.body {
		t.Fatalf("带基线的同值重放应逐字相同：\n%s\n%s", replayBaseline.body, withBaseline.body)
	}
	keptBaseline := srv.callQuota(t, http.MethodPut, "/quota/user-a", `{"quota_total":210000}`, true)
	if p := decodeQuota(t, keptBaseline); p.UsedBaseline != 150 {
		t.Fatalf("省略基线时不得清零：%s", keptBaseline.body)
	}
	// 0=不限：quota 呈现为 0（不是 null），remaining 为 null。
	unlimited := srv.callQuota(t, http.MethodPut, "/quota/user-a", `{"quota_total":0}`, true)
	raw := unlimited.body
	if !strings.Contains(raw, `"quota":0`) || !strings.Contains(raw, `"remaining":null`) {
		t.Fatalf("0=不限 的呈现不符：%s", raw)
	}
}

// TestAdminQuotaUpdateTakesEffect 是 UC-041 A4 的冻结用例名（yzagmgr 侧执行面）：
// admin 上调限额后**无需重登**即刻生效——同一条数据面臂请求从 429 变为放行。
//
// 「放行」的判据是请求真的往上游走了（契约测试的上游故意不可达 ⇒ 502），
// 而 429 的判据是**上游零请求**：两个状态码区分得开，才说明判定真的换了边。
func TestAdminQuotaUpdateTakesEffect(t *testing.T) {
	srv := newContractServer(t)
	seedQuotaUsage(t, srv, "rq_aq01", "user-a", 600, 400) // 已用 1000

	// 限额 500 < 已用 1000：数据面必须前置拒绝。
	if res := srv.callQuota(t, http.MethodPut, "/quota/user-a", `{"quota_total":500}`, true); res.status != http.StatusOK {
		t.Fatalf("PUT 状态 = %d：%s", res.status, res.body)
	}
	blocked := srv.callQuota(t, http.MethodPost, quotaArmPath("user-a"), `{"model":"glm-5.3-flash"}`, false)
	if blocked.status != http.StatusTooManyRequests {
		t.Fatalf("超限时必须 429，实际 %d（%s）", blocked.status, blocked.body)
	}
	if !strings.Contains(blocked.body, `"code":"quota_exceeded"`) {
		t.Fatalf("429 体里应有 quota_exceeded：%s", blocked.body)
	}
	// 上游零请求：契约测试的上游是「一被调用就记录」的桩。
	if n := len(srv.upstream.recorded()); n != 0 {
		t.Fatalf("超限请求不得触达上游，实际上游收到 %d 个请求", n)
	}
	// 拒绝体带三个数值，供调用方直接显示。
	var payload struct {
		Quota     int64 `json:"quota"`
		UsedTotal int64 `json:"used_total"`
		Remaining int64 `json:"remaining"`
	}
	if err := json.Unmarshal([]byte(blocked.body), &payload); err != nil {
		t.Fatalf("429 体不是预期 JSON: %v（%s）", err, blocked.body)
	}
	if payload.Quota != 500 || payload.UsedTotal != 1000 || payload.Remaining != 0 {
		t.Fatalf("429 体的数值不符：%+v", payload)
	}

	// admin 上调（无需重登）：同一请求立刻不再被限额拒绝 —— 它走到上游去了（502）。
	if res := srv.callQuota(t, http.MethodPut, "/quota/user-a", `{"quota_total":5000}`, true); res.status != http.StatusOK {
		t.Fatalf("上调 PUT 状态 = %d：%s", res.status, res.body)
	}
	allowed := srv.callQuota(t, http.MethodPost, quotaArmPath("user-a"), `{"model":"glm-5.3-flash"}`, false)
	if allowed.status != http.StatusBadGateway {
		t.Fatalf("上调后应放行到上游（桩上游不可达 ⇒ 502），实际 %d（%s）", allowed.status, allowed.body)
	}
	if n := len(srv.upstream.recorded()); n != 1 {
		t.Fatalf("放行后上游应收到 1 个请求，实际 %d", n)
	}

	// 置不限：0 永不拒绝。
	if res := srv.callQuota(t, http.MethodPut, "/quota/user-a", `{"quota_total":0}`, true); res.status != http.StatusOK {
		t.Fatalf("置不限 PUT 状态 = %d：%s", res.status, res.body)
	}
	unlimited := srv.callQuota(t, http.MethodPost, quotaArmPath("user-a"), `{"model":"glm-5.3-flash"}`, false)
	if unlimited.status == http.StatusTooManyRequests {
		t.Fatalf("0=不限 不得拒绝，实际 %d（%s）", unlimited.status, unlimited.body)
	}
}

// TestQuotaResetBaselineRestoresAccess 是 D34 的端到端主流程（走真实链路）：
// 超限被拒 → `PUT ... {used_baseline}` 抬基线（限额不变）→ 同一条臂请求恢复放行；
// 基线小于已用时按差额判；GET 里 used_total（毛）/used_eff（生效）/remaining 三者自洽。
func TestQuotaResetBaselineRestoresAccess(t *testing.T) {
	srv := newContractServer(t)
	seedQuotaUsage(t, srv, "rq_rb01", "user-a", 950, 50) // 已用 1000

	// 限额 500 ⇒ 超限，数据面 429（上游零请求）。
	if res := srv.callQuota(t, http.MethodPut, "/quota/user-a", `{"quota_total":500}`, true); res.status != http.StatusOK {
		t.Fatalf("PUT 状态 = %d：%s", res.status, res.body)
	}
	if res := srv.callQuota(t, http.MethodPost, quotaArmPath("user-a"), `{"model":"glm-5.3-flash"}`, false); res.status != http.StatusTooManyRequests {
		t.Fatalf("超限时必须 429，实际 %d（%s）", res.status, res.body)
	}
	if n := len(srv.upstream.recorded()); n != 0 {
		t.Fatalf("超限请求不得触达上游，实际 %d", n)
	}

	// admin 重置：限额**不变**（仍 500），基线抬到当前已用。
	reset := srv.callQuota(t, http.MethodPut, "/quota/user-a",
		`{"quota_total":500,"used_baseline":1000}`, true)
	if reset.status != http.StatusOK {
		t.Fatalf("重置 PUT 状态 = %d：%s", reset.status, reset.body)
	}
	p := decodeQuota(t, reset)
	if p.Quota == nil || *p.Quota != 500 || p.UsedTotal != 1000 || p.UsedBaseline != 1000 || p.UsedEff != 0 {
		t.Fatalf("重置读数不符：%s", reset.body)
	}
	if p.Remaining == nil || *p.Remaining != 500 {
		t.Fatalf("重置后剩余额度应为满额 500：%s", reset.body)
	}
	// 放行恢复：同一请求不再被限额拒绝（走到上游去了 ⇒ 502）。
	allowed := srv.callQuota(t, http.MethodPost, quotaArmPath("user-a"), `{"model":"glm-5.3-flash"}`, false)
	if allowed.status != http.StatusBadGateway {
		t.Fatalf("重置后应放行到上游（桩上游不可达 ⇒ 502），实际 %d（%s）", allowed.status, allowed.body)
	}

	// 基线小于已用：按差额判（已用 1000 − 基线 800 = 200 < 500 ⇒ 仍放行）。
	partial := srv.callQuota(t, http.MethodPut, "/quota/user-a",
		`{"quota_total":500,"used_baseline":800}`, true)
	if got := decodeQuota(t, partial); got.UsedEff != 200 {
		t.Fatalf("差额判定不符：%s", partial.body)
	}
	if res := srv.callQuota(t, http.MethodPost, quotaArmPath("user-a"), `{"model":"glm-5.3-flash"}`, false); res.status == http.StatusTooManyRequests {
		t.Fatalf("差额未超限不得拒绝，实际 %d（%s）", res.status, res.body)
	}
	// 差额超过限额：重新拒绝，且拒绝体的 used_total 是**生效用量**（200 → 调低限额到 100）。
	if res := srv.callQuota(t, http.MethodPut, "/quota/user-a",
		`{"quota_total":100,"used_baseline":800}`, true); res.status != http.StatusOK {
		t.Fatalf("PUT 状态 = %d：%s", res.status, res.body)
	}
	blocked := srv.callQuota(t, http.MethodPost, quotaArmPath("user-a"), `{"model":"glm-5.3-flash"}`, false)
	if blocked.status != http.StatusTooManyRequests {
		t.Fatalf("差额超限必须 429，实际 %d（%s）", blocked.status, blocked.body)
	}
	var body struct {
		Quota     int64 `json:"quota"`
		UsedTotal int64 `json:"used_total"`
		Remaining int64 `json:"remaining"`
	}
	if err := json.Unmarshal([]byte(blocked.body), &body); err != nil {
		t.Fatalf("429 体不是预期 JSON: %v（%s）", err, blocked.body)
	}
	if body.Quota != 100 || body.UsedTotal != 200 || body.Remaining != 0 {
		t.Fatalf("429 体应按生效用量报数（quota=100, used_total=used_eff=200, remaining=0）：%+v", body)
	}
}

// TestQuotaEndpointsRequireToken 覆盖「不经 /api/v1 不等于免鉴权」：
// 读与**写**都要令牌；跨站来源一律 403（限额是写操作，放松等于把别人的额度交出去）。
func TestQuotaEndpointsRequireToken(t *testing.T) {
	srv := newContractServer(t)

	for _, method := range []string{http.MethodGet, http.MethodPut} {
		path := "/quota/user-a"
		body := ""
		if method == http.MethodPut {
			body = `{"quota_total":1000}`
		}
		res := srv.callQuota(t, method, path, body, false)
		if res.status != http.StatusUnauthorized || !strings.Contains(res.body, `"code":"unauthorized"`) {
			t.Fatalf("%s %s 缺令牌应 401 裸错误，实际 %d（%s）", method, path, res.status, res.body)
		}
	}

	// 跨站来源：与 /api/v1、/monitor 同一套守卫。
	req := httptest.NewRequest(http.MethodPut, "/quota/user-a", strings.NewReader(`{"quota_total":1000}`))
	req.Header.Set("X-Agmgr-Token", srv.token)
	req.Header.Set("Origin", "https://evil.example")
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), "forbidden_origin") {
		t.Fatalf("跨站来源应 403 forbidden_origin，实际 %d（%s）", rec.Code, rec.Body.String())
	}
	// 本机管理页来源放行。
	req = httptest.NewRequest(http.MethodGet, "/quota/user-a", nil)
	req.Header.Set("X-Agmgr-Token", srv.token)
	req.Header.Set("Origin", "http://127.0.0.1:17022")
	rec = httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("本机来源应放行，实际 %d（%s）", rec.Code, rec.Body.String())
	}
}

// TestQuotaRejectsInvalidInputAndMethods 覆盖这一族的封闭形状：
// 路径形状不对 404、slug 形状非法与请求体非法 400、非 GET/PUT 405。
func TestQuotaRejectsInvalidInputAndMethods(t *testing.T) {
	srv := newContractServer(t)

	// 路径形状不对：缺 slug、多一段。
	for _, path := range []string{"/quota/", "/quota/user-a/extra"} {
		res := srv.callQuota(t, http.MethodGet, path, "", true)
		if res.status != http.StatusNotFound || !strings.Contains(res.body, `"code":"not_found"`) {
			t.Fatalf("%s 应 404 裸错误，实际 %d（%s）", path, res.status, res.body)
		}
	}
	// slug 形状非法：400（路径对了、值不对）。
	for _, slug := range []string{".hidden", "a..b", "User%20Name"} {
		res := srv.callQuota(t, http.MethodGet, "/quota/"+slug, "", true)
		if res.status != http.StatusBadRequest || !strings.Contains(res.body, `"code":"invalid_request"`) {
			t.Fatalf("slug %q 应 400 invalid_request，实际 %d（%s）", slug, res.status, res.body)
		}
	}
	// 请求体：缺 quota_total、负值、未知字段、非 JSON。
	for _, body := range []string{
		`{}`, `{"quota_total":-1}`, `{"quota_total":10,"nope":1}`, `not-json`,
		// D34 的两个可选字段同样是「给了就必须非负」。
		`{"quota_total":10,"used_baseline":-1}`, `{"quota_total":10,"external_used":-1}`,
	} {
		res := srv.callQuota(t, http.MethodPut, "/quota/user-a", body, true)
		if res.status != http.StatusBadRequest || !strings.Contains(res.body, `"code":"invalid_request"`) {
			t.Fatalf("请求体 %s 应 400 invalid_request，实际 %d（%s）", body, res.status, res.body)
		}
	}
	// 非法请求不得留下任何限额行（先读一次确认仍是「无记录 ⇒ 不限」）。
	res := srv.callQuota(t, http.MethodGet, "/quota/user-a", "", true)
	if p := decodeQuota(t, res); p.Quota != nil {
		t.Fatalf("被拒的写入不得落库：%s", res.body)
	}
	// 方法：只有 GET 与 PUT。
	for _, method := range []string{http.MethodPost, http.MethodDelete, http.MethodPatch} {
		res := srv.callQuota(t, method, "/quota/user-a", "", true)
		if res.status != http.StatusMethodNotAllowed {
			t.Fatalf("%s 应 405，实际 %d（%s）", method, res.status, res.body)
		}
	}
}

// TestQuotaUnavailableReportsStorage 覆盖用量面不可用：明确报 storage（503），
// **不**回「quota:null」——那会被读成「不限」，是把「读不到」说成「没有限额」。
func TestQuotaUnavailableReportsStorage(t *testing.T) {
	srv := newContractServer(t)
	srv.Usage = nil // 用量库打开失败时的装配结果

	for _, tc := range []struct{ method, body string }{
		{http.MethodGet, ""},
		{http.MethodPut, `{"quota_total":1000}`},
	} {
		res := srv.callQuota(t, tc.method, "/quota/user-a", tc.body, true)
		if res.status != http.StatusServiceUnavailable {
			t.Fatalf("%s 状态 = %d，期望 503（%s）", tc.method, res.status, res.body)
		}
		if !strings.Contains(res.body, `"code":"storage"`) {
			t.Fatalf("%s 应报 storage：%s", tc.method, res.body)
		}
	}
}
