package proxy

import (
	"bufio"
	"bytes"
	"io"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
	"unicode/utf8"
)

// 流式断言的时序纪律：**判据必须因果，不能是墙钟阈值**。
//
// 整包全量跑时 CPU 会被其它测试争用，任何固定阈值（例如「首帧必须在 700ms 内到达」）
// 都会假红；而「上游被阻塞在 channel 上时首帧仍能到达」这件事与负载无关。下面的
// 两个常量只作**安全网**：它们的作用是让失败以「可判读的失败」而不是「挂住」的形式
// 出现，不作为任何结论的依据。
const (
	// streamCtxTimeout 是流式请求的整体超时。
	streamCtxTimeout = 20 * time.Second
	// frameSafetyTimeout 是「等某一帧/等上游续写」的安全超时。
	frameSafetyTimeout = 10 * time.Second
)

// ── UC-007 A8：SSE 逐块透传，不缓冲至结束 ───────────────────────────────────

func TestSSEIsStreamedNotBuffered(t *testing.T) {
	const tailFrame = 4096
	const tailCount = 256 // 约 1 MiB 的续写：响应总长足够长

	release := make(chan struct{})
	releaseOnce := sync.OnceFunc(func() { close(release) })
	continued := make(chan struct{})
	continuedOnce := sync.OnceFunc(func() { close(continued) })
	var upstreamContinued atomic.Bool

	h := newArmHarness(t, nil)
	// 清理顺序：cleanup 后进先出 ⇒ 放行先于夹具关闭假上游执行（否则断言失败时会挂住）。
	t.Cleanup(releaseOnce)
	h.gw.on(func(w http.ResponseWriter, r *http.Request, _ []byte) {
		flusher, _ := w.(http.Flusher)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: first\n\n")
		flusher.Flush()
		// 首帧之后**阻塞**：代理若把响应缓冲到结束，首帧就永远到不了客户端。
		select {
		case <-release:
		case <-r.Context().Done():
			return
		}
		upstreamContinued.Store(true)
		continuedOnce()
		pad := strings.Repeat("x", tailFrame)
		for i := 0; i < tailCount; i++ {
			_, _ = io.WriteString(w, "data: "+pad+"\n\n")
		}
		flusher.Flush()
	})

	started := time.Now()
	resp := h.rawWithTimeout(t, http.MethodPost, "/u/user-a/9f2c1a4b7e0d3c55/glm/v1/messages",
		`{"model":"m","stream":true}`, map[string]string{"Content-Type": "application/json"}, streamCtxTimeout)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("状态 = %d", resp.StatusCode)
	}
	reader := bufio.NewReader(resp.Body)

	type readResult struct {
		line string
		err  error
	}
	got := make(chan readResult, 1)
	go func() {
		line, err := reader.ReadString('\n')
		got <- readResult{line: line, err: err}
	}()

	select {
	case r := <-got:
		if r.err != nil {
			t.Fatalf("读首帧失败: %v", r.err)
		}
		if r.line != "data: first\n" {
			t.Fatalf("首帧内容 = %q", r.line)
		}
		// 因果判据：首帧到手时，上游必须还没被放行（还没开始续写）。
		// 若上游已续写，首帧就可能是跟在大响应之后一起来的 ⇒ 无法排除「缓冲到结束」。
		if upstreamContinued.Load() {
			t.Fatal("首帧到达时上游已经续写：首帧跟在大响应之后才到，响应被缓冲到了结束")
		}
	case <-time.After(frameSafetyTimeout):
		releaseOnce()
		t.Fatalf("首帧在上游续写之前没有到达（安全超时 %v，仅作安全网，不是判据）：代理把响应缓冲到了结束",
			frameSafetyTimeout)
	}
	// 耗时只作观测记录，不参与判定。
	t.Logf("首帧到达耗时 %v（观测值，非判据）", time.Since(started))

	releaseOnce()
	select {
	case <-continued:
	case <-time.After(frameSafetyTimeout):
		t.Fatal("放行后上游没有续写：连接可能已断")
	}

	rest, err := io.ReadAll(reader)
	if err != nil {
		t.Fatalf("读完整流失败: %v", err)
	}
	// 「首字节时间与响应总长无关」的另一半：那条长尾巴必须**完整**透传。
	// 判据是字节数而不是耗时，同样与负载无关。
	wantTail := tailCount * (len("data: ") + tailFrame + len("\n\n"))
	if len(rest) < wantTail {
		t.Fatalf("续写部分没有完整透传：收到 %d 字节，期望至少 %d 字节", len(rest), wantTail)
	}
	if !strings.Contains(string(rest), "data: ") {
		t.Fatal("续写的 SSE 帧没有透传")
	}
}

// ── UC-017 A8：逐帧透传 ────────────────────────────────────────────────────

func TestSSEStreamsFrameByFrame(t *testing.T) {
	release := make(chan struct{})
	releaseOnce := sync.OnceFunc(func() { close(release) })
	continued := make(chan struct{})
	continuedOnce := sync.OnceFunc(func() { close(continued) })
	var upstreamContinued atomic.Bool

	h := newArmHarness(t, nil)
	// 清理顺序：cleanup 后进先出 ⇒ 这里登记的放行先于夹具关闭假上游执行。
	// 少了它，断言失败时会把「测试挂住」变成唯一的失败现象（假上游的 handler 卡在
	// <-release 上，httptest 的 Close 会一直等它）。
	t.Cleanup(releaseOnce)
	h.gw.on(func(w http.ResponseWriter, r *http.Request, _ []byte) {
		flusher, _ := w.(http.Flusher)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {\"n\":1}\n\n")
		flusher.Flush()
		select {
		case <-release: // 停在两帧之间，直到测试确认首帧已到达
		case <-r.Context().Done():
			return
		}
		upstreamContinued.Store(true)
		continuedOnce()
		_, _ = io.WriteString(w, "data: {\"n\":2}\n\n")
		flusher.Flush()
	})

	resp := h.rawWithTimeout(t, http.MethodPost, "/u/user-a/9f2c1a4b7e0d3c55/glm/v1/messages",
		`{"model":"m","stream":true}`, map[string]string{"Content-Type": "application/json"}, streamCtxTimeout)
	reader := bufio.NewReader(resp.Body)
	type readResult struct {
		line string
		err  error
	}
	got := make(chan readResult, 1)
	go func() {
		line, err := reader.ReadString('\n')
		got <- readResult{line: line, err: err}
	}()

	select {
	case r := <-got:
		if r.err != nil {
			t.Fatalf("读首帧失败: %v", r.err)
		}
		if r.line != "data: {\"n\":1}\n" {
			t.Fatalf("首帧 = %q", r.line)
		}
		if upstreamContinued.Load() {
			t.Fatal("首帧到达时上游已经续写：响应被缓冲到了结束")
		}
	case <-time.After(frameSafetyTimeout):
		releaseOnce()
		t.Fatalf("首帧在上游完成之前没有到达（安全超时 %v）：代理把 SSE 缓冲到了响应结束", frameSafetyTimeout)
	}
	releaseOnce()
	select {
	case <-continued:
	case <-time.After(frameSafetyTimeout):
		t.Fatal("放行后上游没有续写：连接可能已断")
	}

	rest, err := io.ReadAll(reader)
	if err != nil {
		t.Fatalf("读尾帧失败: %v", err)
	}
	if !strings.Contains(string(rest), `data: {"n":2}`) {
		t.Fatalf("第二帧没有透传：%q", rest)
	}
}

// ── UC-017 A9：UTF-8 安全分块 ───────────────────────────────────────────────

func TestStreamChunkingIsUTF8Safe(t *testing.T) {
	payload := "data: {\"text\":\"中文🙂测试 end\"}\n\ndata: {\"text\":\"尾帧\"}\n\n"
	emojiAt := strings.Index(payload, "🙂")
	if emojiAt < 0 {
		t.Fatal("夹具里必须有 4 字节字符")
	}
	// 在 4 字节字符的**中间**切开：任何「先解码再编码」的中间层都会在这里坏掉。
	split := emojiAt + 2

	// 两半之间用 channel 而不是 sleep 拉开：让「分两次 flush」这件事确定发生，
	// 且不引入任何耗时常量。
	releaseSecond := make(chan struct{})
	releaseOnce := sync.OnceFunc(func() { close(releaseSecond) })

	h := newArmHarness(t, nil)
	t.Cleanup(releaseOnce)
	h.gw.on(func(w http.ResponseWriter, r *http.Request, _ []byte) {
		flusher, _ := w.(http.Flusher)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(payload[:split]))
		flusher.Flush()
		select {
		case <-releaseSecond:
		case <-r.Context().Done():
			return
		}
		_, _ = w.Write([]byte(payload[split:]))
		flusher.Flush()
	})

	resp := h.rawWithTimeout(t, http.MethodPost, "/u/user-a/9f2c1a4b7e0d3c55/glm/v1/messages",
		`{"model":"m","stream":true}`, map[string]string{"Content-Type": "application/json"}, streamCtxTimeout)
	reader := bufio.NewReader(resp.Body)

	// 读到「前半段」这 split 个字节为止：多字节字符此刻是被切断的状态。
	type readResult struct {
		buf []byte
		err error
	}
	got := make(chan readResult, 1)
	go func() {
		b := make([]byte, split)
		_, err := io.ReadFull(reader, b)
		got <- readResult{buf: b, err: err}
	}()
	select {
	case r := <-got:
		if r.err != nil {
			t.Fatalf("读前半段失败: %v", r.err)
		}
		if !bytes.Equal(r.buf, []byte(payload[:split])) {
			t.Fatalf("前半段被改写：%q", r.buf)
		}
		if utf8.Valid(r.buf) {
			t.Fatalf("夹具没有真的把多字节字符切断：%q", r.buf)
		}
	case <-time.After(frameSafetyTimeout):
		releaseOnce()
		t.Fatalf("前半段没有到达（安全超时 %v）", frameSafetyTimeout)
	}

	releaseOnce()
	rest, err := io.ReadAll(reader)
	if err != nil {
		t.Fatalf("读后半段失败: %v", err)
	}
	gotFull := append(append([]byte(nil), []byte(payload[:split])...), rest...)

	if !bytes.Equal(gotFull, []byte(payload)) {
		t.Fatalf("跨 chunk 边界的字节被破坏：\n得到 %q\n期望 %q", gotFull, payload)
	}
	if !utf8.Valid(gotFull) {
		t.Fatalf("透传结果不是合法 UTF-8：%q", gotFull)
	}
	if !strings.Contains(string(gotFull), "中文🙂测试") {
		t.Fatalf("多字节字符没有完整到达：%q", gotFull)
	}
}
