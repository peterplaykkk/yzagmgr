package router

import (
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"testing/iotest"
)

func TestIsEventStream(t *testing.T) {
	tests := []struct {
		ct   string
		want bool
	}{
		{"text/event-stream", true},
		{"text/event-stream; charset=utf-8", true},
		{"TEXT/Event-Stream", true},
		{"application/json", false},
		{"", false},
	}
	for _, tc := range tests {
		if got := isEventStream(tc.ct); got != tc.want {
			t.Fatalf("isEventStream(%q) = %t", tc.ct, got)
		}
	}
}

// streamWriter 是带 Flush 的响应写入器（按需注入错误）。
type streamWriter struct {
	hdr     http.Header
	status  int
	buf     strings.Builder
	flushN  int
	flushEr error
	writeEr error
}

func (w *streamWriter) Header() http.Header {
	if w.hdr == nil {
		w.hdr = http.Header{}
	}
	return w.hdr
}
func (w *streamWriter) WriteHeader(status int) { w.status = status }
func (w *streamWriter) Write(b []byte) (int, error) {
	if w.writeEr != nil {
		return 0, w.writeEr
	}
	return w.buf.Write(b)
}
func (w *streamWriter) Flush() { w.flushN++ }

// flushErrWriter 用 FlushError 形态报告 Flush 失败（http.ResponseController 的约定）。
type flushErrWriter struct {
	streamWriter
	err error
}

func (w *flushErrWriter) FlushError() error { return w.err }

func TestCopyStream(t *testing.T) {
	t.Run("逐块透传并逐块 Flush", func(t *testing.T) {
		w := &streamWriter{}
		var seen []byte
		total, err := copyStream(w, strings.NewReader("hello world"), func(b []byte) { seen = append(seen, b...) })
		if err != nil || total != 11 {
			t.Fatalf("copyStream = (%d, %v)", total, err)
		}
		if w.buf.String() != "hello world" || string(seen) != "hello world" {
			t.Fatalf("写出 = %q，观察 = %q", w.buf.String(), seen)
		}
		if w.flushN != 1 {
			t.Fatalf("应逐块 Flush，实际 %d 次", w.flushN)
		}
	})

	t.Run("observe 为 nil 时不观察", func(t *testing.T) {
		w := &streamWriter{}
		if total, err := copyStream(w, strings.NewReader("x"), nil); err != nil || total != 1 {
			t.Fatalf("copyStream = (%d, %v)", total, err)
		}
	})

	t.Run("写出失败即中止", func(t *testing.T) {
		werr := errors.New("连接已断")
		w := &streamWriter{writeEr: werr}
		total, err := copyStream(w, strings.NewReader("payload"), nil)
		if !errors.Is(err, werr) || total != 0 {
			t.Fatalf("copyStream = (%d, %v)", total, err)
		}
	})

	t.Run("Flush 失败即中止", func(t *testing.T) {
		ferr := errors.New("flush 失败")
		w := &flushErrWriter{err: ferr}
		total, err := copyStream(w, strings.NewReader("payload"), nil)
		if !errors.Is(err, ferr) || total != int64(len("payload")) {
			t.Fatalf("copyStream = (%d, %v)", total, err)
		}
	})

	t.Run("Flusher 不受支持时继续（例如内嵌 recorder）", func(t *testing.T) {
		w := &recorder{}
		if total, err := copyStream(w, strings.NewReader("payload"), nil); err != nil || total != 7 {
			t.Fatalf("copyStream = (%d, %v)", total, err)
		}
	})

	t.Run("读失败即上报", func(t *testing.T) {
		rerr := errors.New("上游中断")
		total, err := copyStream(&streamWriter{}, iotest.ErrReader(rerr), nil)
		if !errors.Is(err, rerr) || total != 0 {
			t.Fatalf("copyStream = (%d, %v)", total, err)
		}
	})
}

func TestCopyHeaders(t *testing.T) {
	src := http.Header{
		"X-Keep":              {"v1", "v2"},
		"Connection":          {"keep-alive"},
		"Host":                {"example.test"},
		"Content-Length":      {"12"},
		"Proxy-Authorization": {"x"},
		"Transfer-Encoding":   {"chunked"},
	}

	dst := http.Header{}
	copyRequestHeaders(dst, src)
	if got := dst.Values("X-Keep"); len(got) != 2 || got[0] != "v1" || got[1] != "v2" {
		t.Fatalf("多值头未透传: %v", got)
	}
	for _, name := range []string{"Connection", "Host", "Content-Length", "Proxy-Authorization", "Transfer-Encoding"} {
		if dst.Get(name) != "" {
			t.Fatalf("%s 应被剔除", name)
		}
	}

	resp := http.Header{}
	copyResponseHeaders(resp, src)
	if resp.Get("Host") != "example.test" || resp.Get("Content-Length") != "12" {
		t.Fatalf("响应头应保留 Host/Content-Length: %v", resp)
	}
	if resp.Get("Connection") != "" || resp.Get("Transfer-Encoding") != "" {
		t.Fatalf("逐跳响应头应被剔除: %v", resp)
	}
}

func TestHopByHopDetection(t *testing.T) {
	if !isHopByHop("connection") || !isHopByHop("Upgrade") || !isHopByHop("TE") {
		t.Fatal("逐跳头识别缺失")
	}
	if isHopByHop("X-Trace") || isHopByHop("Authorization") {
		t.Fatal("非逐跳头被误判")
	}
}

// TestWriteErrorEnvelopeBytes 守错误信封的字节形状（与守护进程 writeDataPlaneError 同形）。
func TestWriteErrorEnvelopeBytes(t *testing.T) {
	w := &recorder{}
	writeError(w, newInvalidRequest("模型 %q 未命中", "a<b>&c"))
	if w.status != http.StatusBadRequest {
		t.Fatalf("状态码 = %d", w.status)
	}
	if got := w.Header().Get("Content-Type"); got != "application/json; charset=utf-8" {
		t.Fatalf("Content-Type = %q", got)
	}
	// encoding/json 默认转义 <>&（与守护进程同一 encode 路径）。
	want := "{\"error\":{\"code\":\"invalid_request\",\"message\":\"模型 \\\"a\\u003cb\\u003e\\u0026c\\\" 未命中\"}}"
	if got := w.body(); got != want {
		t.Fatalf("信封 = %s\n期望 %s", got, want)
	}
}

func TestErrorConstructors(t *testing.T) {
	tests := []struct {
		name   string
		err    *Error
		status int
		code   string
	}{
		{"invalid_request", newInvalidRequest("x"), http.StatusBadRequest, codeInvalidRequest},
		{"payload_too_large", newPayloadTooLarge("x"), http.StatusRequestEntityTooLarge, codeInvalidRequest},
		{"not_found", newNotFound("x"), http.StatusNotFound, codeNotFound},
		{"unauthenticated", newUnauthenticated("x"), http.StatusUnauthorized, codeUnauthenticated},
		{"gateway_unreachable", newGatewayUnreachable("x"), http.StatusBadGateway, codeGatewayUnreachable},
		{"internal", newInternal("x"), http.StatusInternalServerError, codeInternal},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if tc.err.Status != tc.status || tc.err.Code != tc.code || tc.err.Message != "x" {
				t.Fatalf("构造失败: %+v", tc.err)
			}
		})
	}
}

func TestAsErrorNormalization(t *testing.T) {
	want := newUnauthenticated("凭据库未接线: ref")
	if got := asError(want); got != want {
		t.Fatalf("已是 *Error 应原样返回: %+v", got)
	}
	if got := asError(io.ErrUnexpectedEOF); got == nil || got.Status != http.StatusInternalServerError {
		t.Fatalf("普通错误应归一成 500 internal: %+v", got)
	}
}
