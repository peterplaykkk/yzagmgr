package router

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"compress/zlib"
	"context"
	"encoding/base64"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// failingResponseWriter models a downstream disconnect after the proxy has
// already committed response headers. The router must record the interruption
// once and abort the HTTP response instead of returning a cleanly terminated
// response with an upstream success status.
type failingResponseWriter struct {
	header   http.Header
	status   int
	writeN   int
	writeErr error
}

type flushErrorResponseWriter struct {
	header   http.Header
	status   int
	body     bytes.Buffer
	flushN   int
	flushErr error
}

func (w *flushErrorResponseWriter) Header() http.Header {
	if w.header == nil {
		w.header = make(http.Header)
	}
	return w.header
}
func (w *flushErrorResponseWriter) WriteHeader(status int)      { w.status = status }
func (w *flushErrorResponseWriter) Write(b []byte) (int, error) { return w.body.Write(b) }
func (w *flushErrorResponseWriter) FlushError() error {
	w.flushN++
	return w.flushErr
}

func newResponseRouter(t *testing.T, endpoint string, client *http.Client) *routerHarness {
	t.Helper()
	snap := fakeSnapshot{routes: map[string]Route{
		"mimo": {Name: "mimo", Endpoint: endpoint, CredRef: "synthetic-mimo-ref"},
	}}
	tokens := fakeTokens{tokens: map[string]string{"synthetic-mimo-ref": "synthetic-mimo-token"}}
	return newRouterHarness(t, snap, tokens, func(opts *Options) { opts.Client = client })
}

func newResponseTestServer(t *testing.T, http2 bool, handler http.Handler) *httptest.Server {
	t.Helper()
	if !http2 {
		server := httptest.NewServer(handler)
		t.Cleanup(server.Close)
		return server
	}
	server := httptest.NewUnstartedServer(handler)
	server.EnableHTTP2 = true
	server.StartTLS()
	t.Cleanup(server.Close)
	return server
}

func syntheticEncodedBody(t *testing.T, encoding, raw string) []byte {
	t.Helper()
	var out bytes.Buffer
	switch encoding {
	case "":
		return []byte(raw)
	case "gzip":
		w := gzip.NewWriter(&out)
		if _, err := io.WriteString(w, raw); err != nil {
			t.Fatalf("write gzip fixture: %v", err)
		}
		if err := w.Close(); err != nil {
			t.Fatalf("close gzip fixture: %v", err)
		}
	case "deflate":
		w := zlib.NewWriter(&out)
		if _, err := io.WriteString(w, raw); err != nil {
			t.Fatalf("write deflate fixture: %v", err)
		}
		if err := w.Close(); err != nil {
			t.Fatalf("close deflate fixture: %v", err)
		}
	case "br":
		// Bun node:zlib brotliCompressSync vector for the same raw JSON payload.
		decoded, err := base64.StdEncoding.DecodeString("Cw+AeyJ0eXBlIjoibWVzc2FnZSIsImNvbnRlbnQiOltdfQM=")
		if err != nil {
			t.Fatalf("decode fixed brotli fixture: %v", err)
		}
		return decoded
	default:
		t.Fatalf("unsupported synthetic encoding %q", encoding)
	}
	return out.Bytes()
}

func decodedSyntheticBody(t *testing.T, encoding string, body []byte) []byte {
	t.Helper()
	switch encoding {
	case "":
		return body
	case "gzip":
		r, err := gzip.NewReader(bytes.NewReader(body))
		if err != nil {
			t.Fatalf("open gzip response: %v", err)
		}
		defer r.Close()
		decoded, err := io.ReadAll(r)
		if err != nil {
			t.Fatalf("read gzip response: %v", err)
		}
		return decoded
	case "deflate":
		r, err := zlib.NewReader(bytes.NewReader(body))
		if err != nil {
			t.Fatalf("open deflate response: %v", err)
		}
		defer r.Close()
		decoded, err := io.ReadAll(r)
		if err != nil {
			t.Fatalf("read deflate response: %v", err)
		}
		return decoded
	case "br":
		// Go stdlib has no Brotli decoder; the fixture bytes are fixed and the
		// pass-through equality below is the contract under test.
		return body
	default:
		t.Fatalf("unsupported synthetic encoding %q", encoding)
		return nil
	}
}

func (w *failingResponseWriter) Header() http.Header {
	if w.header == nil {
		w.header = make(http.Header)
	}
	return w.header
}

func (w *failingResponseWriter) WriteHeader(status int) { w.status = status }

func (w *failingResponseWriter) Write([]byte) (int, error) {
	w.writeN++
	return 0, w.writeErr
}

func TestEngineAbortsAfterCompressedResponseWriteFailure(t *testing.T) {
	const payload = `{"type":"message","content":[]}`
	var compressed bytes.Buffer
	zw := gzip.NewWriter(&compressed)
	if _, err := io.WriteString(zw, payload); err != nil {
		t.Fatalf("compress fixture: %v", err)
	}
	if err := zw.Close(); err != nil {
		t.Fatalf("finish gzip fixture: %v", err)
	}

	up := newUpstream(t)
	up.on(func(w http.ResponseWriter, _ *http.Request, _ []byte) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Content-Encoding", "gzip")
		w.Header().Set("Content-Length", strconv.Itoa(compressed.Len()))
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(compressed.Bytes())
	})
	rec := newRecords()
	h := NewHandler(Options{
		Snapshot:  standardSnapshot(up),
		Tokens:    standardTokens(),
		Client:    up.srv.Client(),
		OnRequest: rec.add,
	})
	writeErr := errors.New("downstream disconnected")
	w := &failingResponseWriter{writeErr: writeErr}
	req := httptest.NewRequest(http.MethodPost, PathPrefix+"/v1/messages?beta=true", strings.NewReader(`{"model":"mimo"}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept-Encoding", "gzip")

	var recovered any
	func() {
		defer func() { recovered = recover() }()
		h.ServeHTTP(w, req)
	}()
	if recovered != http.ErrAbortHandler {
		t.Fatalf("response writer failure must abort the committed HTTP stream with ErrAbortHandler; recovered=%v", recovered)
	}
	if w.status != http.StatusOK || w.Header().Get("Content-Encoding") != "gzip" ||
		w.Header().Get("Content-Length") != strconv.Itoa(compressed.Len()) || w.writeN != 1 {
		t.Fatalf("partial response metadata/write count = status:%d headers:%v writes:%d", w.status, w.Header(), w.writeN)
	}
	if got := up.last(t).RawQuery; got != "beta=true" {
		t.Fatalf("router did not preserve the request query: %q", got)
	}
	if got := len(rec.all()); got != 1 {
		t.Fatalf("interrupted response must be recorded exactly once, got %d", got)
	}
	if record := rec.one(t); record.Status != http.StatusOK || record.ResponseBodyError != ResponseBodyErrorGatewayUnreachable {
		t.Fatalf("record must retain the sent status and mark the downstream interruption: %+v", record)
	}
	if got := up.count(); got != 1 {
		t.Fatalf("interrupted request must not be retried, upstream received %d attempts", got)
	}
}

func TestEngineTransmitsIdentityGzipDeflateAndBrotliVerbatim(t *testing.T) {
	const raw = `{"type":"message","content":[]}`
	for _, tc := range []struct {
		name     string
		encoding string
	}{
		{"identity", ""},
		{"gzip", "gzip"},
		{"deflate", "deflate"},
		{"brotli", "br"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			encoded := syntheticEncodedBody(t, tc.encoding, raw)
			up := newUpstream(t)
			up.on(func(w http.ResponseWriter, _ *http.Request, _ []byte) {
				w.Header().Set("Content-Type", "application/json")
				w.Header().Set("Content-Length", strconv.Itoa(len(encoded)))
				w.Header().Set("X-Upstream-Trace", "synthetic-trace")
				if tc.encoding != "" {
					w.Header().Set("Content-Encoding", tc.encoding)
				}
				w.WriteHeader(http.StatusOK)
				_, _ = w.Write(encoded)
			})
			h := newResponseRouter(t, up.srv.URL, up.srv.Client())
			req, err := http.NewRequestWithContext(t.Context(), http.MethodPost,
				h.base+PathPrefix+"/v1/messages?beta=true", strings.NewReader(`{"model":"mimo"}`))
			if err != nil {
				t.Fatalf("build request: %v", err)
			}
			req.Header.Set("Content-Type", "application/json")
			// Explicit negotiation prevents Go's client-side transparent decoder so
			// this assertion observes the bytes on the HTTP wire.
			req.Header.Set("Accept-Encoding", "gzip, deflate, br")
			resp, err := h.cli.Do(req)
			if err != nil {
				t.Fatalf("router request: %v", err)
			}
			defer resp.Body.Close()
			got, err := io.ReadAll(resp.Body)
			if err != nil {
				t.Fatalf("read router response: %v", err)
			}
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("status=%d, want 200", resp.StatusCode)
			}
			if resp.Header.Get("Content-Encoding") != tc.encoding ||
				resp.Header.Get("Content-Length") != strconv.Itoa(len(encoded)) ||
				resp.Header.Get("Content-Type") != "application/json" ||
				resp.Header.Get("X-Upstream-Trace") != "synthetic-trace" {
				t.Fatalf("response headers changed or became inconsistent: %v", resp.Header)
			}
			if !bytes.Equal(got, encoded) {
				t.Fatalf("response body was not byte-preserving: got %x, want %x", got, encoded)
			}
			if tc.encoding != "br" && string(decodedSyntheticBody(t, tc.encoding, got)) != raw {
				t.Fatalf("decoded %s body differs from fixture", tc.encoding)
			}
			if reqOnUpstream := up.last(t); reqOnUpstream.RawQuery != "beta=true" ||
				reqOnUpstream.Header.Get("Accept-Encoding") != "gzip, deflate, br" {
				t.Fatalf("request query/encoding negotiation changed upstream: %+v", reqOnUpstream)
			}
			if got := up.count(); got != 1 {
				t.Fatalf("one router request must yield one upstream attempt, got %d", got)
			}
			record := h.rec.one(t)
			if record.Status != http.StatusOK || record.ResponseBodyError != "" {
				t.Fatalf("normal encoded response was marked interrupted: %+v", record)
			}
		})
	}
}

func TestEnginePassesCompressedUpstreamErrorWithoutRetry(t *testing.T) {
	const raw = `{"error":{"type":"rate_limit_error"}}`
	encoded := syntheticEncodedBody(t, "gzip", raw)
	up := newUpstream(t)
	up.on(func(w http.ResponseWriter, _ *http.Request, _ []byte) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Content-Encoding", "gzip")
		w.Header().Set("Content-Length", strconv.Itoa(len(encoded)))
		w.Header().Set("Retry-After", "17")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write(encoded)
	})
	h := newResponseRouter(t, up.srv.URL, up.srv.Client())
	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost,
		h.base+PathPrefix+"/v1/messages", strings.NewReader(`{"model":"mimo"}`))
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	req.Header.Set("Accept-Encoding", "gzip")
	resp, err := h.cli.Do(req)
	if err != nil {
		t.Fatalf("router request: %v", err)
	}
	body, readErr := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if readErr != nil {
		t.Fatalf("read compressed upstream error: %v", readErr)
	}
	if resp.StatusCode != http.StatusTooManyRequests || resp.Header.Get("Content-Encoding") != "gzip" ||
		resp.Header.Get("Content-Length") != strconv.Itoa(len(encoded)) || resp.Header.Get("Retry-After") != "17" ||
		!bytes.Equal(body, encoded) {
		t.Fatalf("compressed 429 was not passed through exactly: status=%d headers=%v body=%x", resp.StatusCode, resp.Header, body)
	}
	if decoded := string(decodedSyntheticBody(t, "gzip", body)); decoded != raw {
		t.Fatalf("decompressed error body = %q, want %q", decoded, raw)
	}
	if up.count() != 1 {
		t.Fatalf("a non-2xx upstream response must not be retried; attempts=%d", up.count())
	}
	if rec := h.rec.one(t); rec.Status != http.StatusTooManyRequests || rec.ResponseBodyError != "" {
		t.Fatalf("complete upstream error was misclassified: %+v", rec)
	}
}

func TestEngineCompressedSSEStreamsFirstFrameBeforeRelease(t *testing.T) {
	firstSent := make(chan struct{})
	release := make(chan struct{})
	var attempts atomic.Int32
	up := newResponseTestServer(t, false, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempts.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Content-Encoding", "gzip")
		w.WriteHeader(http.StatusOK)
		zw := gzip.NewWriter(w)
		if _, err := io.WriteString(zw, "data: {\"usage\":{\"output_tokens\":0}}\n\n"); err != nil {
			return
		}
		if err := zw.Flush(); err != nil {
			return
		}
		if flusher, ok := w.(http.Flusher); ok {
			flusher.Flush()
		}
		close(firstSent)
		select {
		case <-release:
		case <-r.Context().Done():
			_ = zw.Close()
			return
		}
		_, _ = io.WriteString(zw, "data: [DONE]\n\n")
		_ = zw.Close()
	}))
	t.Cleanup(func() {
		select {
		case <-release:
		default:
			close(release)
		}
	})
	h := newResponseRouter(t, up.URL, up.Client())
	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost,
		h.base+PathPrefix+"/v1/messages", strings.NewReader(`{"model":"mimo","stream":true}`))
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept-Encoding", "gzip")
	resp, err := (&http.Client{Timeout: 5 * time.Second}).Do(req)
	if err != nil {
		t.Fatalf("stream request: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK || resp.Header.Get("Content-Encoding") != "gzip" || resp.Header.Get("Content-Length") != "" {
		t.Fatalf("compressed SSE headers/status changed: %d %v", resp.StatusCode, resp.Header)
	}
	reader, err := gzip.NewReader(resp.Body)
	if err != nil {
		t.Fatalf("open gzip SSE: %v", err)
	}
	line, err := bufio.NewReader(reader).ReadString('\n')
	if err != nil || !strings.Contains(line, "output_tokens") {
		t.Fatalf("first compressed SSE frame not readable before release: %q err=%v", line, err)
	}
	select {
	case <-firstSent:
	default:
		t.Fatal("upstream first frame was not sent")
	}
	close(release)
	rest, err := io.ReadAll(reader)
	if err != nil {
		t.Fatalf("read remaining compressed SSE: %v", err)
	}
	if !strings.Contains(string(rest), "[DONE]") || attempts.Load() != 1 {
		t.Fatalf("SSE tail or attempt count invalid: tail=%q attempts=%d", rest, attempts.Load())
	}
	if rec := h.rec.one(t); !rec.Streamed || rec.ResponseBodyError != "" {
		t.Fatalf("complete compressed SSE was misclassified: %+v", rec)
	}
}

func TestEngineH1AndH2TruncatedGzipResponsesAbortAndRecordTransportError(t *testing.T) {
	const raw = `{"type":"message","content":[]}`
	full := syntheticEncodedBody(t, "gzip", raw)
	partial := full[:len(full)-8] // Drop the gzip trailer while keeping HTTP Content-Length truthful.
	for _, tc := range []struct {
		name string
		h2   bool
		want int
	}{
		{"http1", false, 1},
		{"http2", true, 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			attempts := atomic.Int32{}
			protocol := make(chan int, 1)
			up := newResponseTestServer(t, tc.h2, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				attempts.Add(1)
				protocol <- r.ProtoMajor
				w.Header().Set("Content-Type", "application/json")
				w.Header().Set("Content-Encoding", "gzip")
				w.Header().Set("Content-Length", strconv.Itoa(len(full)))
				w.WriteHeader(http.StatusOK)
				if flusher, ok := w.(http.Flusher); ok {
					flusher.Flush()
				}
				_, _ = w.Write(partial)
				if flusher, ok := w.(http.Flusher); ok {
					flusher.Flush()
				}
			}))
			h := newResponseRouter(t, up.URL, up.Client())
			req, err := http.NewRequestWithContext(t.Context(), http.MethodPost,
				h.base+PathPrefix+"/v1/messages", strings.NewReader(`{"model":"mimo"}`))
			if err != nil {
				t.Fatalf("build request: %v", err)
			}
			req.Header.Set("Accept-Encoding", "gzip")
			resp, err := h.cli.Do(req)
			if err != nil {
				t.Fatalf("receive committed response headers: %v", err)
			}
			body, readErr := io.ReadAll(resp.Body)
			_ = resp.Body.Close()
			if resp.StatusCode != http.StatusOK || resp.Header.Get("Content-Encoding") != "gzip" ||
				resp.Header.Get("Content-Length") != strconv.Itoa(len(full)) {
				t.Fatalf("already-sent status/headers changed: status=%d headers=%v", resp.StatusCode, resp.Header)
			}
			if readErr == nil {
				t.Fatalf("truncated upstream entity must abort the downstream response; body=%x", body)
			}
			if got := <-protocol; got != tc.want {
				t.Fatalf("upstream protocol major=%d, want HTTP/%d", got, tc.want)
			}
			if got := attempts.Load(); got != 1 {
				t.Fatalf("truncated POST must not be retried; attempts=%d", got)
			}
			if rec := h.rec.one(t); rec.Status != http.StatusOK ||
				rec.ResponseBodyError != ResponseBodyErrorGatewayUnreachable {
				t.Fatalf("truncated upstream body record = %+v", rec)
			}
		})
	}
}

func TestEngineOutboundBodyTimeoutIsGatewayUnreachable(t *testing.T) {
	started := make(chan struct{})
	var attempts atomic.Int32
	up := newResponseTestServer(t, false, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempts.Add(1)
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Content-Encoding", "gzip")
		zw := gzip.NewWriter(w)
		w.WriteHeader(http.StatusOK)
		if _, err := io.WriteString(zw, `{"partial":`); err != nil {
			return
		}
		if err := zw.Flush(); err != nil {
			return
		}
		if flusher, ok := w.(http.Flusher); ok {
			flusher.Flush()
		}
		close(started)
		<-r.Context().Done()
	}))
	outbound := up.Client()
	outbound.Timeout = 250 * time.Millisecond
	h := newResponseRouter(t, up.URL, outbound)
	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost,
		h.base+PathPrefix+"/v1/messages", strings.NewReader(`{"model":"mimo"}`))
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	req.Header.Set("Accept-Encoding", "gzip")
	resp, err := h.cli.Do(req)
	if err != nil {
		t.Fatalf("read already-committed response headers: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("committed upstream status changed: %d", resp.StatusCode)
	}
	_, readErr := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if readErr == nil {
		t.Fatal("outbound client timeout must not finish the partial response cleanly")
	}
	select {
	case <-started:
	default:
		t.Fatal("upstream response handler did not start")
	}
	if got := attempts.Load(); got != 1 {
		t.Fatalf("outbound timeout must not retry, attempts=%d", got)
	}
	if rec := h.rec.one(t); rec.Status != http.StatusOK ||
		rec.ResponseBodyError != ResponseBodyErrorGatewayUnreachable {
		t.Fatalf("live inbound request with outbound body timeout = %+v", rec)
	}
}

func TestEngineInboundCancellationIsRecordedCancelled(t *testing.T) {
	started := make(chan struct{})
	var attempts atomic.Int32
	up := newResponseTestServer(t, false, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempts.Add(1)
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Content-Encoding", "gzip")
		zw := gzip.NewWriter(w)
		w.WriteHeader(http.StatusOK)
		if _, err := io.WriteString(zw, `{"partial":`); err != nil {
			return
		}
		if err := zw.Flush(); err != nil {
			return
		}
		if flusher, ok := w.(http.Flusher); ok {
			flusher.Flush()
		}
		close(started)
		<-r.Context().Done()
	}))
	h := newResponseRouter(t, up.URL, up.Client())
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		h.base+PathPrefix+"/v1/messages", strings.NewReader(`{"model":"mimo"}`))
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	req.Header.Set("Accept-Encoding", "gzip")
	resp, err := h.cli.Do(req)
	if err != nil {
		t.Fatalf("receive response headers before cancellation: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("already-sent status changed: %d", resp.StatusCode)
	}
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		_ = resp.Body.Close()
		t.Fatal("upstream did not reach the streaming response before cancellation")
	}
	cancel()
	_, readErr := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if readErr == nil {
		t.Fatal("inbound cancellation must abort the partial response")
	}
	if attempts.Load() != 1 {
		t.Fatalf("cancelled POST must not be retried, attempts=%d", attempts.Load())
	}
	if rec := h.rec.one(t); rec.Status != http.StatusOK || rec.ResponseBodyError != ResponseBodyErrorCancelled {
		t.Fatalf("cancelled inbound request record = %+v", rec)
	}
}

func TestEngineFlushFailureAbortsAfterSingleRecord(t *testing.T) {
	up := newUpstream(t)
	up.on(func(w http.ResponseWriter, _ *http.Request, _ []byte) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true}`))
	})
	rec := newRecords()
	h := NewHandler(Options{
		Snapshot:  standardSnapshot(up),
		Tokens:    standardTokens(),
		Client:    up.srv.Client(),
		OnRequest: rec.add,
	})
	flushErr := errors.New("downstream flush failed")
	w := &flushErrorResponseWriter{flushErr: flushErr}
	req := httptest.NewRequest(http.MethodPost, PathPrefix+"/v1/messages", strings.NewReader(`{"model":"mimo"}`))
	var recovered any
	func() {
		defer func() { recovered = recover() }()
		h.ServeHTTP(w, req)
	}()
	if recovered != http.ErrAbortHandler {
		t.Fatalf("flush failure must abort the committed HTTP stream; recovered=%v", recovered)
	}
	if w.status != http.StatusOK || w.flushN != 1 || up.count() != 1 {
		t.Fatalf("flush-abort status/flush/upstream counts=%d/%d/%d", w.status, w.flushN, up.count())
	}
	if rec := rec.one(t); rec.Status != http.StatusOK || rec.ResponseBodyError != ResponseBodyErrorGatewayUnreachable {
		t.Fatalf("flush-failure record = %+v", rec)
	}
}
