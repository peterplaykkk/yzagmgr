// Package apierr 的单元测试。
//
// 守的是四条已被用例固化的契约：
//   - UC-006 A4 / doc/api-v1.md §1.3：错误码是**封闭集合**，多一个少一个都要红；
//   - UC-014 A1：同一失败场景恒得同一码（码 → HTTP 状态映射稳定且在 4xx/5xx）；
//   - UC-014 A2：信封互斥——成功响应不含 error，失败响应不含 data；
//   - UC-014 A12：每个响应（含失败）都带可检索的 requestId。
package apierr

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// envelope 是测试侧对响应体的严格解读：用 map 判「键是否出现」，
// 而不是靠结构体零值——否则「缺 data」与「data:null」会分辨不出来。
type envelope struct {
	OK        bool                       `json:"ok"`
	Data      any                        `json:"data"`
	Error     *Error                     `json:"error"`
	RequestID string                     `json:"requestId"`
	Raw       map[string]json.RawMessage `json:"-"`
}

func decodeEnvelope(t *testing.T, body []byte) envelope {
	t.Helper()
	var env envelope
	if err := json.Unmarshal(body, &env); err != nil {
		t.Fatalf("响应体不是合法 JSON: %v（体：%s）", err, body)
	}
	if err := json.Unmarshal(body, &env.Raw); err != nil {
		t.Fatalf("响应体不是 JSON 对象: %v（体：%s）", err, body)
	}
	return env
}

// TestErrorCodesAreAClosedSet 守住 UC-006 A4 与 doc/api-v1.md §1.3 的表。
// 新增码必须同时改那张表与本用例——这正是「封闭」的含义。
func TestErrorCodesAreAClosedSet(t *testing.T) {
	// §1.3 表逐行展开后共 31 条（login_expired / cancelled 同行占两条；
	// 2026-09-25 UC-039 新增 test_in_progress、TUI wave-2 新增 quota_exceeded；
	// 2026-09-27 UC-094 B-5 新增 version_conflict / capability_conflict；
	// 2026-10-13 yzagmgr 网关化新增 rate_limited——design-b §5.4 三处同步的 Go 侧落点，
	// doc/api-v1.md §1.3 行与 web errors.ts 由 T7/T5 同批补）。
	const wantCount = 31
	if len(All) != wantCount {
		t.Fatalf("封闭集合长度 = %d，契约要求 %d；新增码必须同时改 doc/api-v1.md §1.3 与本用例",
			len(All), wantCount)
	}

	seen := make(map[Code]bool, len(All))
	for _, c := range All {
		if c == "" {
			t.Errorf("封闭集合里出现空码（空码必须是「非法」）")
		}
		if seen[c] {
			t.Errorf("码 %q 在 All 中重复登记", c)
		}
		seen[c] = true
		if !c.Valid() {
			t.Errorf("码 %q 在 All 中，Valid() 却为 false", c)
		}
	}

	// 伪造值不得被判为合法（Valid 必须是集合成员判定，不是「非空即合法」）。
	for _, bogus := range []Code{"", "not_a_code", "Invalid_Request", "invalid-request", "internal ", " Internal"} {
		if bogus.Valid() {
			t.Errorf("伪造码 %q 被判为合法", bogus)
		}
	}

	// 点名跨包依赖的稳定码：删表项不会让调用方编译失败，只会静默换语义，
	// 所以这里显式点名（改名会编译失败，删除不会）。
	for _, want := range []Code{
		CodeInvalidRequest, CodeUnauthorized, CodeForbiddenOrigin, CodeUnauthenticated,
		CodeNotFound, CodeConfigChanged, CodeConfigInvalid, CodePathConflict,
		CodePermissionDenied, CodePreviewExpired, CodePreviewInUse, CodeBackupUnreadable,
		CodeStorage, CodeIntegrityFailed, CodeGatewayUnreachable, CodeGatewayUnexpectedResponse,
		CodeLoginExpired, CodeCancelled, CodeChannelNotConfigured,
		CodeTestInProgress,
		CodeQuotaExceeded,
		CodeRateLimited,
		CodeVersionConflict,
		CodeCapabilityConflict,
		CodeScriptTimeout, CodeScriptLimitExceeded, CodeInvalidScriptShape, CodeInvalidURL,
		CodeBlockedTarget, CodeNotSupported, CodeInternal,
	} {
		if !seen[want] {
			t.Errorf("契约码 %q 不在 All 中", want)
		}
	}
}

// TestHTTPStatusMappingCoversEveryCode 守住「每个码都有状态映射」，
// 并把调用方（前端/curl）依赖的具体映射固定下来。
func TestHTTPStatusMappingCoversEveryCode(t *testing.T) {
	for _, c := range All {
		st := c.HTTPStatus()
		if st < 400 || st > 599 {
			t.Errorf("码 %q 的 HTTP 状态 = %d，不在 4xx/5xx 区间", c, st)
		}
	}

	cases := []struct {
		code Code
		want int
	}{
		{CodeInvalidRequest, 400},
		{CodeUnauthorized, 401},
		{CodeUnauthenticated, 401},
		{CodeForbiddenOrigin, 403},
		{CodePermissionDenied, 403},
		{CodeNotFound, 404},
		{CodeQuotaExceeded, 429},
		// 吞吐速率：与 quota_exceeded 同 429、码不同（分钟自愈 vs 等重置）。
		{CodeRateLimited, 429},
		{CodePreviewExpired, 410},
		{CodePreviewInUse, 410},
		{CodeLoginExpired, 410},
		{CodeCancelled, 410},
		{CodeConfigChanged, 400},
		{CodeConfigInvalid, 400},
		{CodePathConflict, 400},
		{CodeGatewayUnreachable, 502},
		{CodeGatewayUnexpectedResponse, 502},
		{CodeStorage, 507},
		{CodeInternal, 500},
		// 状态冲突走 409：前端据此把它当成「稍后再试」（而不是「我传错了」）。
		{CodeTestInProgress, 409},
		// UC-094 B-5：版本冲突与能力闸门同类——请求合法、状态挡住了它。
		{CodeVersionConflict, 409},
		{CodeCapabilityConflict, 409},
	}
	for _, tc := range cases {
		if got := tc.code.HTTPStatus(); got != tc.want {
			t.Errorf("码 %q 的 HTTP 状态 = %d，契约要求 %d", tc.code, got, tc.want)
		}
	}

	// 未知码兜底为 500：宁可 500 也不能落到 200（否则失败被当成成功）。
	if got := Code("definitely_not_a_code").HTTPStatus(); got != http.StatusInternalServerError {
		t.Errorf("未知码的状态 = %d，契约要求 500", got)
	}
}

func TestErrorTextAndFormatting(t *testing.T) {
	e := New(CodeConfigChanged, "目标文件被外改: %s", "/tmp/x.json")
	if got, want := e.Error(), "config_changed: 目标文件被外改: /tmp/x.json"; got != want {
		t.Errorf("Error() = %q，期望 %q", got, want)
	}
	if e.Code != CodeConfigChanged {
		t.Errorf("Code = %q，期望 %q", e.Code, CodeConfigChanged)
	}

	// nil 接收者不得 panic（From/Wrap 链上可能拿到 nil）。
	var nilErr *Error
	if got := nilErr.Error(); got != "<nil>" {
		t.Errorf("nil *Error Error() = %q，期望 %q", got, "<nil>")
	}
}

func TestWrapKeepsCauseText(t *testing.T) {
	cause := errors.New("permission denied")
	cases := []struct {
		name string
		err  error
		fmt  string
		want string
	}{
		{"有原因有前缀", cause, "读取目标失败", "读取目标失败: permission denied"},
		{"有原因无前缀", cause, "", "permission denied"},
		{"无原因", nil, "记录备份失败", "记录备份失败"},
		{"无原因无前缀", nil, "", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := Wrap(CodeStorage, tc.err, "%s", tc.fmt)
			if got.Message != tc.want {
				t.Errorf("Wrap 的 message = %q，期望 %q", got.Message, tc.want)
			}
			if got.Code != CodeStorage {
				t.Errorf("Wrap 的 code = %q，期望 %q", got.Code, CodeStorage)
			}
		})
	}
}

func TestFromNormalizesErrors(t *testing.T) {
	if got := From(nil); got != nil {
		t.Errorf("From(nil) = %v，期望 nil", got)
	}

	orig := New(CodePathConflict, "受管路径是目录: /tmp/d")
	if got := From(orig); got != orig {
		t.Errorf("From(*Error) 必须原样返回同一指针（后续 WithDetails 要能改到原对象）")
	}

	// 被 fmt.Errorf 包过一层后仍然还原为同一个 *Error（不降级成 internal）。
	wrapped := fmt.Errorf("事务执行失败: %w", orig)
	if got := From(wrapped); got != orig {
		t.Errorf("From(wrapped *Error) = %v，期望同一 *Error %v", got, orig)
	}

	// 其余一律归为 internal，且不把内部类型暴露成契约码。
	plain := From(errors.New("sql: database is locked"))
	if plain.Code != CodeInternal {
		t.Errorf("普通 error 的码 = %q，期望 %q", plain.Code, CodeInternal)
	}
	if !strings.Contains(plain.Message, "database is locked") {
		t.Errorf("普通 error 的 message = %q，期望保留原因文本", plain.Message)
	}
	if !plain.Code.Valid() {
		t.Errorf("From 产出的码 %q 不在封闭集合里", plain.Code)
	}
}

func TestIsComparesByCodeOnly(t *testing.T) {
	if !errors.Is(New(CodeStorage, "磁盘满"), New(CodeStorage, "另一个消息")) {
		t.Errorf("同码不同消息必须 errors.Is 相等（码是契约、消息是给人看的）")
	}
	if errors.Is(New(CodeStorage, "磁盘满"), New(CodeInternal, "磁盘满")) {
		t.Errorf("不同码不得 errors.Is 相等")
	}
	if errors.Is(New(CodeStorage, "磁盘满"), errors.New("磁盘满")) {
		t.Errorf("与普通 error 比较必须为 false")
	}
}

func TestWithDetailsMergesWithoutLosingKeys(t *testing.T) {
	e := New(CodeConfigChanged, "外改")
	same := e.WithDetails(map[string]any{"path": "/tmp/a.json"})
	if same != e {
		t.Fatalf("WithDetails 必须支持链式调用（返回同一指针）")
	}
	e.WithDetails(map[string]any{"toolId": "claude"})

	if got := e.Details["path"]; got != "/tmp/a.json" {
		t.Errorf("details.path = %v，期望 /tmp/a.json", got)
	}
	if got := e.Details["toolId"]; got != "claude" {
		t.Errorf("details.toolId = %v，期望 claude", got)
	}
	if len(e.Details) != 2 {
		t.Errorf("details 键数 = %d，期望 2（合并而非替换）", len(e.Details))
	}

	// details 参与 JSON 序列化（前端按 key 读）。
	b, err := json.Marshal(e)
	if err != nil {
		t.Fatalf("Error 序列化失败: %v", err)
	}
	var back Error
	if err := json.Unmarshal(b, &back); err != nil {
		t.Fatalf("Error 反序列化失败: %v", err)
	}
	if back.Code != CodeConfigChanged || back.Details["path"] != "/tmp/a.json" {
		t.Errorf("Error 往返丢失字段: %s", b)
	}
}

// TestEnvelopeNeverMixesDataAndError 守住 UC-014 A2：
// 「成功不带 error、失败不带 data」必须是机器可判定的，不能靠调用方自觉。
func TestEnvelopeNeverMixesDataAndError(t *testing.T) {
	t.Run("成功信封不含 error", func(t *testing.T) {
		b, err := json.Marshal(OK(map[string]any{"tools": 10}))
		if err != nil {
			t.Fatalf("序列化失败: %v", err)
		}
		env := decodeEnvelope(t, b)
		if !env.OK {
			t.Errorf("ok = false，期望 true")
		}
		if _, has := env.Raw["error"]; has {
			t.Errorf("成功信封出现 error 键: %s", b)
		}
		if _, has := env.Raw["data"]; !has {
			t.Errorf("成功信封缺 data 键: %s", b)
		}
	})

	t.Run("失败信封不含 data", func(t *testing.T) {
		b, err := json.Marshal(Fail(New(CodeNotFound, "工具不存在: %s", "foo")))
		if err != nil {
			t.Fatalf("序列化失败: %v", err)
		}
		env := decodeEnvelope(t, b)
		if env.OK {
			t.Errorf("ok = true，期望 false")
		}
		if _, has := env.Raw["data"]; has {
			t.Errorf("失败信封出现 data 键: %s", b)
		}
		if env.Error == nil || env.Error.Code != CodeNotFound {
			t.Errorf("失败信封的 error = %+v，期望 code=not_found", env.Error)
		}
	})

	t.Run("手工构造的 OK 带 error 时丢弃 error", func(t *testing.T) {
		b, err := json.Marshal(Response{OK: true, Data: 1, Error: New(CodeInternal, "不该出现")})
		if err != nil {
			t.Fatalf("序列化失败: %v", err)
		}
		if _, has := decodeEnvelope(t, b).Raw["error"]; has {
			t.Errorf("ok=true 时 error 必须被丢弃: %s", b)
		}
	})

	t.Run("手工构造的失败带 data 时丢弃 data", func(t *testing.T) {
		b, err := json.Marshal(Response{OK: false, Data: "不该出现", Error: New(CodeStorage, "磁盘满")})
		if err != nil {
			t.Fatalf("序列化失败: %v", err)
		}
		if _, has := decodeEnvelope(t, b).Raw["data"]; has {
			t.Errorf("ok=false 时 data 必须被丢弃: %s", b)
		}
	})

	t.Run("Fail(nil) 降级为 internal 而非 null", func(t *testing.T) {
		b, err := json.Marshal(Fail(nil))
		if err != nil {
			t.Fatalf("序列化失败: %v", err)
		}
		env := decodeEnvelope(t, b)
		if env.Error == nil {
			t.Fatalf("Fail(nil) 的 error 不能是 null: %s", b)
		}
		if env.Error.Code != CodeInternal {
			t.Errorf("Fail(nil) 的码 = %q，期望 %q", env.Error.Code, CodeInternal)
		}
		if env.Error.Message == "" {
			t.Errorf("Fail(nil) 的 message 不得为空: %s", b)
		}
	})
}

// TestWriteJSONStatusHeadersAndBody 守住传输层：状态码取自错误码映射、
// 头齐全（含 X-Request-Id）、body 可被解析回信封（UC-014 A12）。
func TestWriteJSONStatusAndHeaders(t *testing.T) {
	const rid = "req_unit_test"
	cases := []struct {
		name       string
		resp       Response
		wantStatus int
		wantOK     bool
		wantCode   Code // 仅失败信封有效
	}{
		{"成功恒 200", OK(map[string]any{"n": 1}), 200, true, ""},
		{"非法参数 400", Fail(New(CodeInvalidRequest, "limit 非法")), 400, false, CodeInvalidRequest},
		{"缺令牌 401", Fail(New(CodeUnauthorized, "缺少管理令牌")), 401, false, CodeUnauthorized},
		{"跨站 403", Fail(New(CodeForbiddenOrigin, "来源不允许")), 403, false, CodeForbiddenOrigin},
		{"不存在 404", Fail(New(CodeNotFound, "没有这个工具")), 404, false, CodeNotFound},
		{"预览过期 410", Fail(New(CodePreviewExpired, "预览已过期")), 410, false, CodePreviewExpired},
		{"存储 507", Fail(New(CodeStorage, "磁盘满")), 507, false, CodeStorage},
		{"网关不可达 502", Fail(New(CodeGatewayUnreachable, "连不上")), 502, false, CodeGatewayUnreachable},
		{"内部 500", Fail(New(CodeInternal, "未归类")), 500, false, CodeInternal},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			tc.resp.WriteJSON(rec, rid)

			res := rec.Result()
			if res.StatusCode != tc.wantStatus {
				t.Errorf("状态码 = %d，期望 %d", res.StatusCode, tc.wantStatus)
			}
			if ct := res.Header.Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
				t.Errorf("Content-Type = %q，期望 application/json 前缀", ct)
			}
			if got := res.Header.Get("X-Request-Id"); got != rid {
				t.Errorf("X-Request-Id 头 = %q，期望 %q", got, rid)
			}

			env := decodeEnvelope(t, rec.Body.Bytes())
			if env.OK != tc.wantOK {
				t.Errorf("ok = %v，期望 %v", env.OK, tc.wantOK)
			}
			if env.RequestID != rid {
				t.Errorf("body.requestId = %q，期望 %q（失败响应要能和操作日志对上）", env.RequestID, rid)
			}
			if tc.wantOK {
				if _, has := env.Raw["error"]; has {
					t.Errorf("成功响应不得含 error: %s", rec.Body.Bytes())
				}
			} else {
				if _, has := env.Raw["data"]; has {
					t.Errorf("失败响应不得含 data: %s", rec.Body.Bytes())
				}
				if env.Error == nil || env.Error.Code != tc.wantCode {
					t.Errorf("error = %+v，期望 code=%q", env.Error, tc.wantCode)
				}
			}
		})
	}
}

// TestWriteJSONSerializationFailureIs500 覆盖信封序列化失败分支：
// 不得把不可序列化的 data 静默丢成 200。
func TestWriteJSONSerializationFailureIs500(t *testing.T) {
	rec := httptest.NewRecorder()
	OK(make(chan int)).WriteJSON(rec, "req_x")

	if rec.Code != http.StatusInternalServerError {
		t.Errorf("不可序列化的 data 的状态码 = %d，期望 500", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), string(CodeInternal)) {
		t.Errorf("兜底响应应带 internal 码，实际体 = %s", rec.Body.String())
	}
}
