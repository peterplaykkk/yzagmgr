package proxy

import (
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

// ── UC-007 A9：每个完成的请求写一条记账（C 路删除后 A 路臂是主入账面）───────

func TestEveryRequestIsAccounted(t *testing.T) {
	h := newArmHarness(t, nil)
	h.gw.on(func(w http.ResponseWriter, r *http.Request, _ []byte) {
		if strings.Contains(r.Header.Get("Accept"), "text/event-stream") {
			flusher, _ := w.(http.Flusher)
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = io.WriteString(w, "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"usage\":{\"input_tokens\":100,\"output_tokens\":1}}}\n\n")
			flusher.Flush()
			_, _ = io.WriteString(w, "event: message_delta\ndata: {\"type\":\"message_delta\",\"usage\":{\"output_tokens\":50}}\n\n")
			flusher.Flush()
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"msg_1","usage":{"input_tokens":100,"output_tokens":50}}`))
	})

	started := time.Now().UTC().Add(-time.Second)
	armPath := "/u/user-a/9f2c1a4b7e0d3c55/glm/v1/messages"
	status1, _, _ := armDo(t, h, armPath, `{"model":"m"}`, nil)
	if status1 != http.StatusOK {
		t.Fatalf("第一个请求状态 = %d", status1)
	}
	status2, _, _ := h.do(t, http.MethodPost, armPath, `{"model":"m","stream":true}`,
		map[string]string{"Content-Type": "application/json", "Accept": "text/event-stream"})
	if status2 != http.StatusOK {
		t.Fatalf("第二个请求状态 = %d", status2)
	}

	recs := h.rec.exactly(t, 2)
	if len(recs) != 2 {
		t.Fatalf("期望每请求一条记录（共 2 条），实际 %d 条：%+v", len(recs), recs)
	}
	first, second := recs[0], recs[1]

	for i, rec := range recs {
		if !strings.HasPrefix(rec.ID, "rq_") {
			t.Fatalf("第 %d 条记录 id = %q，期望 rq_ 前缀", i+1, rec.ID)
		}
		if rec.App != "claude" || rec.Prefix != "glm" || rec.Provider != "glm" {
			t.Fatalf("第 %d 条记录维度不对（A 路：app=claude、prefix/provider=臂提供方）：%+v", i+1, rec)
		}
		if rec.User != "user-a" || rec.Session != "9f2c1a4b7e0d3c55" {
			t.Fatalf("第 %d 条记录身份轴不对：%+v", i+1, rec)
		}
		if rec.At.Before(started) || rec.At.After(time.Now().UTC().Add(time.Second)) {
			t.Fatalf("第 %d 条记录时间越界：%v", i+1, rec.At)
		}
		if rec.DurationMs < 0 {
			t.Fatalf("第 %d 条记录耗时 = %d", i+1, rec.DurationMs)
		}
	}
	if first.InputTokens != 100 || first.OutputTokens != 50 {
		t.Fatalf("非流式 token = %d/%d，期望 100/50", first.InputTokens, first.OutputTokens)
	}
	if first.Streamed {
		t.Fatal("非流式响应被记成流式")
	}
	if second.InputTokens != 100 || second.OutputTokens != 50 {
		t.Fatalf("流式 token = %d/%d，期望 100/50（跨事件取最后非零值）", second.InputTokens, second.OutputTokens)
	}
	if !second.Streamed {
		t.Fatal("流式响应没有被标成流式")
	}
}

// ── UC-007 A10：失败不记为成功 ──────────────────────────────────────────────

func TestFailedRequestsAreNotCountedAsSuccess(t *testing.T) {
	h := newArmHarness(t, nil)
	fail := true
	h.gw.on(func(w http.ResponseWriter, _ *http.Request, _ []byte) {
		if fail {
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte(`{"error":{"message":"boom"}}`))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"usage":{"input_tokens":7,"output_tokens":9}}`))
	})

	armPath := "/u/user-a/9f2c1a4b7e0d3c55/glm/v1/messages"
	if status, _, _ := armDo(t, h, armPath, `{"model":"m"}`, nil); status != http.StatusInternalServerError {
		t.Fatalf("状态 = %d，期望 500", status)
	}
	fail = false
	if status, _, _ := armDo(t, h, armPath, `{"model":"m"}`, nil); status != http.StatusOK {
		t.Fatalf("状态 = %d，期望 200", status)
	}

	recs := h.rec.exactly(t, 2)
	if len(recs) != 2 {
		t.Fatalf("期望 2 条记录，实际 %d 条", len(recs))
	}
	if recs[0].Status != http.StatusInternalServerError {
		t.Fatalf("失败请求被记成状态 %d（必须记真实状态，不能记成成功）", recs[0].Status)
	}
	if recs[0].ErrorCode == "" {
		t.Fatal("失败请求的 errorCode 为空")
	}
	if recs[0].InputTokens != 0 || recs[0].OutputTokens != 0 {
		t.Fatalf("失败响应不该有 token 计数：%+v", recs[0])
	}
	if recs[1].Status != http.StatusOK || recs[1].ErrorCode != "" {
		t.Fatalf("成功请求的记账不对：%+v", recs[1])
	}
}

// ── UC-017 A10：记账含请求前/出网两个模型名（臂无映射，[1m] 剥离即两名）─────

func TestAccountingRecordsBothModelNames(t *testing.T) {
	h := newArmHarness(t, nil)
	h.gw.on(func(w http.ResponseWriter, _ *http.Request, _ []byte) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"usage":{"input_tokens":1,"output_tokens":2}}`))
	})

	const clientName = "claude-fable-5-1[1m]"
	if status, _, _ := armDo(t, h, "/u/user-a/9f2c1a4b7e0d3c55/glm/v1/messages",
		`{"model":"`+clientName+`"}`, nil); status != http.StatusOK {
		t.Fatalf("状态 = %d", status)
	}

	// 出网体保留客户端原名（臂零改写），上游名由记账侧剥 [1m] 后缀。
	up := h.gw.last()
	if !strings.Contains(string(up.Body), clientName) {
		t.Fatalf("臂请求体必须原样出网：%s", up.Body)
	}
	rec := h.rec.one(t)
	if rec.ModelRequested != clientName {
		t.Fatalf("ModelRequested = %q，期望客户端原样档名 %q", rec.ModelRequested, clientName)
	}
	if rec.ModelUpstream != "claude-fable-5-1" {
		t.Fatalf("ModelUpstream = %q，期望剥掉 [1m] 的上游名", rec.ModelUpstream)
	}
	if rec.ModelRequested == rec.ModelUpstream {
		t.Fatal("两个名字相同：这条记录无法用于对账")
	}
}

// ── UC-017 A11：上游非 2xx 状态与错误体透传 ─────────────────────────────────

func TestUpstreamErrorsPassThroughWithStatus(t *testing.T) {
	const errBody = `{"error":{"type":"rate_limit_error","message":"slow down"}}`
	h := newArmHarness(t, nil)
	h.gw.on(func(w http.ResponseWriter, _ *http.Request, _ []byte) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("x-upstream-request-id", "req_abc")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(errBody))
	})

	status, hdr, body := armDo(t, h, "/u/user-a/9f2c1a4b7e0d3c55/glm/v1/messages", `{"model":"m"}`, nil)
	if status != http.StatusTooManyRequests {
		t.Fatalf("状态 = %d，期望 429（不得吞成 200）", status)
	}
	if string(body) != errBody {
		t.Fatalf("错误体被改写：\n得到 %s\n期望 %s", body, errBody)
	}
	if ct := hdr.Get("Content-Type"); ct != "application/json" {
		t.Fatalf("Content-Type = %q", ct)
	}
	if got := hdr.Get("x-upstream-request-id"); got != "req_abc" {
		t.Fatalf("上游自定义响应头丢失：%q", got)
	}
	rec := h.rec.one(t)
	if rec.Status != http.StatusTooManyRequests {
		t.Fatalf("记账状态 = %d，期望 429", rec.Status)
	}
	if rec.ErrorCode == "" {
		t.Fatal("失败请求的 errorCode 为空")
	}
}

// ── 缓存 token 嗅探（design-b §2.3 U9：三形态 → cached_tokens）──────────────

func TestCachedTokensSniffedThreeForms(t *testing.T) {
	cases := []struct {
		name       string
		path       string
		response   string
		wantCached int
		wantIn     int
	}{
		{
			name: "anthropic：cache_read + cache_creation 合计",
			path: "/u/user-a/sess-1/glm/v1/messages",
			response: `{"usage":{"input_tokens":10,"output_tokens":5,` +
				`"cache_read_input_tokens":100,"cache_creation_input_tokens":50}}`,
			wantCached: 150, wantIn: 10,
		},
		{
			name:     "chat：prompt_tokens_details.cached_tokens 归一为未命中输入",
			path:     "/u/user-a/sess-1/glm/chat/completions",
			response: `{"usage":{"prompt_tokens":120,"completion_tokens":3,"prompt_tokens_details":{"cached_tokens":100}}}`,
			// OpenAI 形态 prompt_tokens **含** cached 明细：嗅探归一 in−=cached
			// （design §1.5 公式前提「in=未命中输入」、doc/glm-pricing.md 归一条款；
			// 原「不扣」断言已按编排器裁定改写——不归一会多收 cached×input_price）。
			wantCached: 100, wantIn: 20,
		},
		{
			name:       "responses：input_tokens_details.cached_tokens 归一为未命中输入",
			path:       "/u/user-a/sess-1/glm/responses",
			response:   `{"usage":{"input_tokens":100,"output_tokens":2,"input_tokens_details":{"cached_tokens":80}}}`,
			wantCached: 80, wantIn: 20,
		},
		{
			name:     "chat：cached > in 时钳到 0（畸形上游防御）",
			path:     "/u/user-a/sess-1/glm/chat/completions",
			response: `{"usage":{"prompt_tokens":5,"completion_tokens":1,"prompt_tokens_details":{"cached_tokens":10}}}`,
			wantCached: 10, wantIn: 0,
		},
		{
			name:       "缺席 = 0",
			path:       "/u/user-a/sess-1/glm/v1/messages",
			response:   `{"usage":{"input_tokens":7,"output_tokens":9}}`,
			wantCached: 0, wantIn: 7,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newArmHarness(t, nil)
			h.gw.on(func(w http.ResponseWriter, _ *http.Request, _ []byte) {
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(tc.response))
			})
			if status, _, body := armDo(t, h, tc.path, `{"model":"m"}`, nil); status != http.StatusOK {
				t.Fatalf("状态 = %d（%s）", status, body)
			}
			rec := h.rec.one(t)
			if rec.CachedTokens != tc.wantCached {
				t.Fatalf("CachedTokens = %d，期望 %d", rec.CachedTokens, tc.wantCached)
			}
			if rec.InputTokens != tc.wantIn {
				t.Fatalf("InputTokens = %d，期望 %d（OpenAI 形态=归一后未命中输入，Anthropic=上游原值）", rec.InputTokens, tc.wantIn)
			}
		})
	}

	// 流式（SSE）形态：Anthropic cache 字段在 message_start 里，同样要嗅到。
	t.Run("SSE：message_start 携带 cache 字段", func(t *testing.T) {
		h := newArmHarness(t, nil)
		h.gw.on(func(w http.ResponseWriter, r *http.Request, _ []byte) {
			flusher, _ := w.(http.Flusher)
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = io.WriteString(w,
				"event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"usage\":{\"input_tokens\":10,\"output_tokens\":1,\"cache_read_input_tokens\":64}}}\n\n")
			flusher.Flush()
			_, _ = io.WriteString(w, "event: message_delta\ndata: {\"type\":\"message_delta\",\"usage\":{\"output_tokens\":9}}\n\n")
			flusher.Flush()
		})
		armDo(t, h, "/u/user-a/sess-1/glm/v1/messages", `{"model":"m","stream":true}`,
			map[string]string{"Accept": "text/event-stream"})
		rec := h.rec.one(t)
		if rec.CachedTokens != 64 || rec.InputTokens != 10 || rec.OutputTokens != 9 {
			t.Fatalf("SSE 嗅探 = in/out/cached %d/%d/%d，期望 10/9/64",
				rec.InputTokens, rec.OutputTokens, rec.CachedTokens)
		}
	})
}
