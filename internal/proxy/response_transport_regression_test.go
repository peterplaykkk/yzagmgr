package proxy

import (
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
	"testing"
	"time"

	"github.com/peterplaykkk/yzagmgr/internal/apierr"
)

type responseErrorRoundTripper func(*http.Request) (*http.Response, error)

func (f responseErrorRoundTripper) RoundTrip(r *http.Request) (*http.Response, error) {
	return f(r)
}

type relayBodyFailure struct {
	data   []byte
	cancel context.CancelFunc
}

func (b *relayBodyFailure) Read(p []byte) (int, error) {
	if len(b.data) > 0 {
		n := copy(p, b.data)
		b.data = b.data[n:]
		return n, nil
	}
	if b.cancel != nil {
		b.cancel()
		b.cancel = nil
	}
	return 0, errors.New("synthetic upstream body interruption")
}

func (*relayBodyFailure) Close() error { return nil }

type relayCaptureWriter struct {
	header http.Header
	status int
	body   bytes.Buffer
	flush  int
}

func (w *relayCaptureWriter) Header() http.Header {
	if w.header == nil {
		w.header = make(http.Header)
	}
	return w.header
}

func (w *relayCaptureWriter) WriteHeader(status int)      { w.status = status }
func (w *relayCaptureWriter) Write(p []byte) (int, error) { return w.body.Write(p) }
func (w *relayCaptureWriter) Flush()                      { w.flush++ }

func proxySyntheticEncodedBody(t *testing.T, encoding, raw string) []byte {
	t.Helper()
	var out bytes.Buffer
	switch encoding {
	case "":
		return []byte(raw)
	case "gzip":
		zw := gzip.NewWriter(&out)
		if _, err := io.WriteString(zw, raw); err != nil {
			t.Fatalf("write gzip fixture: %v", err)
		}
		if err := zw.Close(); err != nil {
			t.Fatalf("close gzip fixture: %v", err)
		}
	case "deflate":
		zw := zlib.NewWriter(&out)
		if _, err := io.WriteString(zw, raw); err != nil {
			t.Fatalf("write deflate fixture: %v", err)
		}
		if err := zw.Close(); err != nil {
			t.Fatalf("close deflate fixture: %v", err)
		}
	case "br":
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

func TestRouterPlanePassesIdentityGzipDeflateAndBrotliVerbatim(t *testing.T) {
	const raw = `{"type":"message","content":[]}`
	for _, tc := range []struct{ name, encoding string }{
		{"identity", ""}, {"gzip", "gzip"}, {"deflate", "deflate"}, {"brotli", "br"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			encoded := proxySyntheticEncodedBody(t, tc.encoding, raw)
			h, _ := newRouterHarness(t)
			h.gw.on(func(w http.ResponseWriter, _ *http.Request, _ []byte) {
				w.Header().Set("Content-Type", "application/json")
				w.Header().Set("Content-Length", strconv.Itoa(len(encoded)))
				w.Header().Set("X-Upstream-Trace", "synthetic-trace")
				if tc.encoding != "" {
					w.Header().Set("Content-Encoding", tc.encoding)
				}
				w.WriteHeader(http.StatusOK)
				_, _ = w.Write(encoded)
			})
			resp := h.raw(t, http.MethodPost, "/router/v1/messages?beta=true", `{"model":"mimo"}`,
				map[string]string{"Content-Type": "application/json", "Accept-Encoding": "gzip, deflate, br"})
			body, err := io.ReadAll(resp.Body)
			if err != nil {
				t.Fatalf("read proxied response: %v", err)
			}
			if resp.StatusCode != http.StatusOK || resp.Header.Get("Content-Encoding") != tc.encoding ||
				resp.Header.Get("Content-Length") != strconv.Itoa(len(encoded)) ||
				resp.Header.Get("Content-Type") != "application/json" || resp.Header.Get("X-Upstream-Trace") != "synthetic-trace" ||
				!bytes.Equal(body, encoded) {
				t.Fatalf("%s response changed in proxy: status=%d headers=%v body=%x want=%x", tc.name, resp.StatusCode, resp.Header, body, encoded)
			}
			upstream := h.gw.last()
			if upstream.RawQuery != "beta=true" || upstream.Header.Get("Accept-Encoding") != "gzip, deflate, br" {
				t.Fatalf("request query/encoding negotiation changed: %+v", upstream)
			}
			if h.gw.count() != 1 {
				t.Fatalf("one request must produce one upstream attempt, got %d", h.gw.count())
			}
			if rec := h.rec.one(t); rec.Status != http.StatusOK || rec.ErrorCode != "" {
				t.Fatalf("complete encoded response record = %+v", rec)
			}
		})
	}
}

func TestRouterPlanePassesCompressedNon2xxResponseWithoutRetry(t *testing.T) {
	const raw = `{"error":{"type":"rate_limit_error"}}`
	encoded := proxySyntheticEncodedBody(t, "gzip", raw)
	h, _ := newRouterHarness(t)
	h.gw.on(func(w http.ResponseWriter, _ *http.Request, _ []byte) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Content-Encoding", "gzip")
		w.Header().Set("Content-Length", strconv.Itoa(len(encoded)))
		w.Header().Set("Retry-After", "17")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write(encoded)
	})
	resp := h.raw(t, http.MethodPost, "/router/v1/messages", `{"model":"mimo"}`,
		map[string]string{"Accept-Encoding": "gzip"})
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read compressed 429: %v", err)
	}
	if resp.StatusCode != http.StatusTooManyRequests || resp.Header.Get("Content-Encoding") != "gzip" ||
		resp.Header.Get("Retry-After") != "17" || !bytes.Equal(body, encoded) {
		t.Fatalf("compressed upstream error changed: status=%d headers=%v body=%x", resp.StatusCode, resp.Header, body)
	}
	if h.gw.count() != 1 {
		t.Fatalf("upstream error must not be retried, attempts=%d", h.gw.count())
	}
	if rec := h.rec.one(t); rec.Status != http.StatusTooManyRequests || rec.ErrorCode != string(apierr.AccountingUpstreamRateLimit) {
		t.Fatalf("compressed non-2xx record = %+v", rec)
	}
}

func TestRouterPlaneTruncatedEncodedBodyMapsTransportError(t *testing.T) {
	full := proxySyntheticEncodedBody(t, "gzip", `{"type":"message","content":[]}`)
	partial := full[:len(full)-8]
	h, _ := newRouterHarness(t)
	h.gw.on(func(w http.ResponseWriter, _ *http.Request, _ []byte) {
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
	})
	resp := h.raw(t, http.MethodPost, "/router/v1/messages", `{"model":"mimo"}`,
		map[string]string{"Accept-Encoding": "gzip"})
	_, readErr := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK || readErr == nil {
		t.Fatalf("truncated response must preserve sent status but abort body: status=%d readErr=%v", resp.StatusCode, readErr)
	}
	if h.gw.count() != 1 {
		t.Fatalf("truncated response must not retry, attempts=%d", h.gw.count())
	}
	if rec := h.rec.one(t); rec.Status != http.StatusOK || rec.ErrorCode != string(apierr.CodeGatewayUnreachable) {
		t.Fatalf("truncated response error code must override apparent 200: %+v", rec)
	}
}

func TestRouterPlaneInboundCancellationErrorCodeOverridesStatus(t *testing.T) {
	started := make(chan struct{})
	h, _ := newRouterHarness(t)
	h.gw.on(func(w http.ResponseWriter, r *http.Request, _ []byte) {
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
	})
	resp := h.rawWithTimeout(t, http.MethodPost, "/router/v1/messages", `{"model":"mimo"}`,
		map[string]string{"Accept-Encoding": "gzip"}, 250*time.Millisecond)
	_, readErr := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK || readErr == nil {
		t.Fatalf("cancelled response must preserve sent status but abort body: status=%d readErr=%v", resp.StatusCode, readErr)
	}
	select {
	case <-started:
	default:
		t.Fatal("fake upstream did not start the held response")
	}
	if h.gw.count() != 1 {
		t.Fatalf("cancelled response must not retry, attempts=%d", h.gw.count())
	}
	if rec := h.rec.one(t); rec.Status != http.StatusOK || rec.ErrorCode != string(apierr.CodeCancelled) {
		t.Fatalf("cancelled response error code must override apparent 200: %+v", rec)
	}
}

func TestLegacyRelayBodyFailureAbortsAndRecordsOnce(t *testing.T) {
	for _, tc := range []struct {
		name       string
		cancelBody bool
		wantCode   apierr.Code
	}{
		{name: "upstream interruption", wantCode: apierr.CodeGatewayUnreachable},
		{name: "inbound cancellation", cancelBody: true, wantCode: apierr.CodeCancelled},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			client := &http.Client{Transport: responseErrorRoundTripper(func(req *http.Request) (*http.Response, error) {
				body := &relayBodyFailure{data: []byte(`{"partial":`)}
				if tc.cancelBody {
					body.cancel = cancel
				}
				return &http.Response{
					Status:        "200 OK",
					StatusCode:    http.StatusOK,
					Header:        http.Header{"Content-Type": []string{"application/json"}},
					Body:          body,
					ContentLength: -1,
					Request:       req,
				}, nil
			})}
			srv, err := NewServer(Options{HTTPClient: client, Logger: quietLogger()})
			if err != nil {
				t.Fatalf("NewServer: %v", err)
			}
			req := httptest.NewRequestWithContext(ctx, http.MethodPost, "/messages", strings.NewReader(`{"model":"synthetic"}`))
			w := &relayCaptureWriter{}
			var finishCalls int
			var gotStatus int
			var gotCode apierr.Code
			var recovered any
			func() {
				defer func() { recovered = recover() }()
				srv.relay(w, req, relayRequest{
					target: "https://provider.example/v1/messages",
					body:   []byte(`{"model":"synthetic"}`),
					finish: func(status int, code apierr.Code, _ TransformResult) {
						finishCalls++
						gotStatus, gotCode = status, code
					},
				})
			}()
			if recovered != http.ErrAbortHandler {
				t.Fatalf("body transfer failure must abort committed response; recovered=%v", recovered)
			}
			if w.status != http.StatusOK || gotStatus != http.StatusOK || gotCode != tc.wantCode {
				t.Fatalf("committed status/error classification = writer:%d record:%d/%s, want 200/%s", w.status, gotStatus, gotCode, tc.wantCode)
			}
			if got := w.body.String(); got != `{"partial":` || w.flush != 1 {
				t.Fatalf("partial response and flush count = %q/%d", got, w.flush)
			}
			if finishCalls != 1 {
				t.Fatalf("relay must record exactly once before abort; finish calls=%d", finishCalls)
			}
		})
	}
}
