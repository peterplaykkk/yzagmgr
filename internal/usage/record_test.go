package usage

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/peterplaykkk/yzagmgr/internal/apierr"
)

// TestConcurrentAccountingLosesNoRecords 对应 UC-008 A12：
// 并发上报必须不丢失、不重复——条数恒等于不同请求数，翻页遍历是严格一次覆盖。
func TestConcurrentAccountingLosesNoRecords(t *testing.T) {
	const (
		writers   = 32
		perWriter = 8
	)
	total := writers * perWriter

	s := newStore(t)
	ctx := context.Background()

	var (
		wg   sync.WaitGroup
		mu   sync.Mutex
		errs []error
	)
	for w := 0; w < writers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := 0; i < perWriter; i++ {
				id := fmt.Sprintf("rq_c%02d_%02d", w, i)
				if _, err := s.Record(ctx, rec(id, time.Duration(i)*time.Millisecond)); err != nil {
					mu.Lock()
					errs = append(errs, err)
					mu.Unlock()
					return
				}
			}
		}(w)
	}
	wg.Wait()
	if len(errs) > 0 {
		t.Fatalf("并发记账失败 %d 次，首个: %v", len(errs), errs[0])
	}
	if got := countRecords(t, s); got != int64(total) {
		t.Fatalf("并发上报 %d 条应有 %d 行，实际 %d（记账丢失或重复）", total, total, got)
	}

	// 翻页遍历必须是全集的严格一次覆盖。
	window := Window{From: T0.Add(-time.Hour), To: T0.Add(time.Hour)}
	seen := map[string]int{}
	cursor := ""
	pages := 0
	for {
		page, err := s.Requests(ctx, RequestsQuery{Window: window, Limit: 16, Cursor: cursor, Order: OrderDesc})
		if err != nil {
			t.Fatalf("翻页失败: %v", err)
		}
		if page.Total != int64(total) {
			t.Fatalf("每页 total 都应是 %d，实际 %d", total, page.Total)
		}
		for _, item := range page.Items {
			seen[item.ID]++
		}
		pages++
		if page.NextCursor == "" {
			break
		}
		cursor = page.NextCursor
		if pages > total {
			t.Fatal("翻页未收敛（游标可能死循环）")
		}
	}
	if len(seen) != total {
		t.Fatalf("翻页应覆盖 %d 个不同 id，实际 %d", total, len(seen))
	}
	for id, n := range seen {
		if n != 1 {
			t.Fatalf("id %s 在翻页中出现 %d 次（重复）", id, n)
		}
	}

	// 同一 id 的并发重放（代理重试）只允许留一行。
	var wg2 sync.WaitGroup
	replayErrs := make(chan error, 16)
	for i := 0; i < 16; i++ {
		wg2.Add(1)
		go func() {
			defer wg2.Done()
			if _, err := s.Record(ctx, rec("rq_replayed", 0)); err != nil {
				replayErrs <- err
			}
		}()
	}
	wg2.Wait()
	close(replayErrs)
	for err := range replayErrs {
		t.Fatalf("同一 id 并发重放不应报错: %v", err)
	}
	if got := countRecords(t, s); got != int64(total)+1 {
		t.Fatalf("同一 id 重放必须只留一行，期望 %d 行，实际 %d", total+1, got)
	}
}

// TestUsageRecordsCarryNoPayloadOrToken 对应 UC-008 A9：
// 记录里没有承载正文与令牌的字段，也没有列可以承载它们；
// 试图把这些内容塞进字符串字段的上报必须被拒绝，且落库内容不含其片段。
func TestUsageRecordsCarryNoPayloadOrToken(t *testing.T) {
	const (
		token      = "Bearer sk-live-9f8e7d6c5b4a3210"
		plainToken = "sk-live-9f8e7d6c5b4a3210"
		body       = `{"messages":[{"role":"user","content":"请把这句话原样返回"}]}`
		// hexToken / codeToken 是「纯小写无分隔长串」：字符集规则放行，只能靠无分隔长串规则拦下。
		hexToken  = "9f8e7d6c5b4a32100fedcba987654321"
		codeToken = "abcdef0123456789abcdef0123456789"
	)
	s := newStore(t)
	ctx := context.Background()

	// 1) 形状护栏：令牌/正文往任何字符串字段塞都不接受。
	rejects := []struct {
		name  string
		build func() Record
	}{
		{"app 塞令牌", func() Record {
			return rec("rq_a9_1", 0, func(r *Record) { r.App = token })
		}},
		{"prefix 塞令牌", func() Record {
			return rec("rq_a9_2", 0, func(r *Record) { r.Prefix = token })
		}},
		{"provider 塞令牌", func() Record {
			return rec("rq_a9_3", 0, func(r *Record) { r.Provider = token })
		}},
		{"provider 塞纯小写长串（令牌形态，需靠无分隔长串规则拦下）", func() Record {
			return rec("rq_a9_3b", 0, func(r *Record) { r.Provider = hexToken })
		}},
		{"errorCode 塞纯小写长串", func() Record {
			return rec("rq_a9_3c", 0, func(r *Record) { r.ErrorCode = codeToken })
		}},
		{"modelRequested 塞正文", func() Record {
			return rec("rq_a9_4", 0, func(r *Record) { r.ModelRequested = body })
		}},
		{"modelUpstream 塞令牌", func() Record {
			return rec("rq_a9_5", 0, func(r *Record) { r.ModelUpstream = token })
		}},
		{"errorCode 塞上游报错正文", func() Record {
			return rec("rq_a9_6", 0, func(r *Record) { r.ErrorCode = `upstream said {"error":"invalid api key"}` })
		}},
		{"id 塞令牌", func() Record {
			return rec(plainToken, 0)
		}},
	}
	for _, tc := range rejects {
		t.Run(tc.name, func(t *testing.T) {
			_, err := s.Record(ctx, tc.build())
			requireCode(t, tc.name, err, apierr.CodeInvalidRequest)
		})
	}
	if got := countRecords(t, s); got != 0 {
		t.Fatalf("被拒的上报不得落库，实际 %d 行", got)
	}

	// 2) 正常上报之后，库文件（含 WAL）里不得出现令牌或正文片段。
	mustRecord(t, s, rec("rq_a9_ok", 0))
	for _, path := range []string{s.Path(), s.Path() + "-wal"} {
		data, err := os.ReadFile(path)
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			t.Fatalf("读取 %s 失败: %v", path, err)
		}
		for _, needle := range []string{plainToken, "Bearer", "messages", "请把这句话原样返回"} {
			if bytes.Contains(data, []byte(needle)) {
				t.Fatalf("%s 中出现了 %q：记账不得携带令牌或正文", path, needle)
			}
		}
	}

	// 3) 表结构里没有可承载正文/令牌的列（含头、cookie、正文、令牌）。
	cols := tableColumnsOf(t, s, "request_logs")
	for _, forbidden := range []string{
		"body", "request_body", "response", "response_body", "headers", "authorization",
		"cookie", "token", "api_key", "content", "prompt", "messages", "payload",
	} {
		if cols[forbidden] {
			t.Fatalf("用量表不得有 %s 列（UC-008 A9）", forbidden)
		}
	}

	// 4) 库层不变量：CHECK/STRICT 挡住绕开 Go 校验的写入。
	if _, err := s.db.ExecContext(ctx,
		`INSERT INTO request_logs (id, at, input_tokens) VALUES ('rq_raw', 1, -5)`); err == nil {
		t.Fatal("库层 CHECK 必须拦住负 token（第三方写入也不放行）")
	}
	if _, err := s.db.ExecContext(ctx,
		`INSERT INTO request_logs (id, at) VALUES ('rq_raw2', 'not-an-int')`); err == nil {
		t.Fatal("STRICT 表必须拦住非整数 at（正文塞不进时间列）")
	}
}

// TestRecordRejectsInvalidShapes 是写入口校验的表驱动用例：
// 非法形状必须当下给 invalid_request，并保证库里一行不留。
func TestRecordRejectsInvalidShapes(t *testing.T) {
	cases := []struct {
		name  string
		build func() Record
	}{
		{"status 超上界", func() Record { return rec("rq_v01", 0, func(r *Record) { r.Status = 600 }) }},
		{"status 为负", func() Record { return rec("rq_v02", 0, func(r *Record) { r.Status = -1 }) }},
		{"输入 token 为负", func() Record { return rec("rq_v03", 0, func(r *Record) { r.InputTokens = -1 }) }},
		{"输出 token 为负", func() Record { return rec("rq_v04", 0, func(r *Record) { r.OutputTokens = -3 }) }},
		{"耗时为负", func() Record { return rec("rq_v05", 0, func(r *Record) { r.DurationMs = -1 }) }},
		{"id 含斜杠", func() Record { return rec("rq/v06", 0) }},
		{"id 超长", func() Record { return rec(strings.Repeat("x", 65), 0) }},
		{"app 大小写混排", func() Record { return rec("rq_v07", 0, func(r *Record) { r.App = "Claude" }) }},
		{"app 含空格", func() Record { return rec("rq_v08", 0, func(r *Record) { r.App = "claude desktop" }) }},
		{"prefix 路径穿越", func() Record { return rec("rq_v09", 0, func(r *Record) { r.Prefix = "../../etc" }) }},
		{"provider 无分隔长串", func() Record {
			return rec("rq_v10", 0, func(r *Record) { r.Provider = strings.Repeat("a1b2c3d4", 4) })
		}},
		{"模型名含换行", func() Record { return rec("rq_v11", 0, func(r *Record) { r.ModelRequested = "deepseek\nflash" }) }},
		{"模型名含引号", func() Record { return rec("rq_v12", 0, func(r *Record) { r.ModelUpstream = `deep"seek` }) }},
		{"模型名超长", func() Record {
			return rec("rq_v13", 0, func(r *Record) { r.ModelUpstream = strings.Repeat("m", 201) })
		}},
		{"errorCode 是整句错误", func() Record {
			return rec("rq_v14", 0, func(r *Record) { r.ErrorCode = "HTTP 502 Bad Gateway" })
		}},
		{"事件时间超出 Unix 纳秒范围", func() Record {
			return rec("rq_v15", 0, func(r *Record) {
				r.At = time.Date(9999, 1, 1, 0, 0, 0, 0, time.UTC)
			})
		}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := newStore(t)
			_, err := s.Record(context.Background(), tc.build())
			requireCode(t, tc.name, err, apierr.CodeInvalidRequest)
			if got := countRecords(t, s); got != 0 {
				t.Fatalf("非法上报不得落库，实际 %d 行", got)
			}
		})
	}
}

// TestRecordAcceptsEmptyScopesAndClosedCode 覆盖「记账优先」的一面：
// 缺标识、空错误码、0 状态这些边界值必须能落库——少一个装饰字段不该丢掉记账。
func TestRecordAcceptsEmptyScopesAndClosedCode(t *testing.T) {
	s := newStore(t)
	got := mustRecord(t, s, Record{
		ID:        "rq_edge01",
		At:        T0,
		Status:    0,
		ErrorCode: "gateway_unreachable",
	})[0]

	if got.App != "" || got.Prefix != "" || got.Provider != "" || got.ModelRequested != "" {
		t.Fatalf("空标识应原样落库（界面显示未知），实际 %+v", got)
	}
	if got.Status != 0 || got.ErrorCode != "gateway_unreachable" {
		t.Fatalf("边界值落库不符：%+v", got)
	}
	// 空标识也要能被汇总到（按空 key 成组，而不是被丢掉）。
	bd, err := s.Breakdown(context.Background(), BreakdownQuery{
		Window: Window{From: T0.Add(-time.Minute), To: T0.Add(time.Minute)}, By: ByApp,
	})
	if err != nil {
		t.Fatalf("Breakdown 失败: %v", err)
	}
	if len(bd.Items) != 1 || bd.Items[0].Key != "" || bd.Items[0].Requests != 1 {
		t.Fatalf("空 app 也应成组出现：%+v", bd.Items)
	}
}

// TestRecordIsIdempotentByIDLastWriteWins 覆盖同一 id 的两次上报：
// 只有一行，且以最后一次为准（代理可在流结束时补最终 token 数）。
func TestRecordIsIdempotentByIDLastWriteWins(t *testing.T) {
	s := newStore(t)
	mustRecord(t, s, rec("rq_dup", 0, func(r *Record) {
		r.InputTokens = 1
		r.OutputTokens = 1
		r.Status = 0
	}))
	mustRecord(t, s, rec("rq_dup", 30*time.Second, func(r *Record) {
		r.InputTokens = 1000
		r.OutputTokens = 200
		r.Streamed = false
	}))

	if got := countRecords(t, s); got != 1 {
		t.Fatalf("同一 id 只应有一行，实际 %d", got)
	}
	detail, err := s.RequestDetail(context.Background(), "rq_dup")
	if err != nil {
		t.Fatalf("RequestDetail 失败: %v", err)
	}
	if detail.InputTokens != 1000 || detail.OutputTokens != 200 || detail.Streamed {
		t.Fatalf("应以最后一次上报为准：%+v", detail)
	}
	if want := T0.Add(30 * time.Second).Format(time.RFC3339); detail.At != want {
		t.Fatalf("事件时间应更新为最后一次上报：got %s want %s", detail.At, want)
	}
}

// TestRecordUsesInjectedClockWhenAtZero 覆盖「At 缺省」：取注入时钟，落库保留纳秒。
func TestRecordUsesInjectedClockWhenAtZero(t *testing.T) {
	clk := &testClock{now: T0.Add(90 * time.Minute)}
	path := t.TempDir() + "/usage.db"
	s, err := Open(context.Background(), path, Options{Location: testLoc, Clock: clk.Now})
	if err != nil {
		t.Fatalf("Open 失败: %v", err)
	}
	defer func() { _ = s.Close() }()

	got, err := s.Record(context.Background(), Record{ID: "rq_clock01", App: "claude", Status: 200})
	if err != nil {
		t.Fatalf("Record 失败: %v", err)
	}
	if want := T0.Add(90 * time.Minute).Format(time.RFC3339); got.At != want {
		t.Fatalf("缺省事件时间应取注入时钟：got %s want %s", got.At, want)
	}
}

// TestRecordGeneratesOpaqueID 覆盖缺省 id：形状稳定且互不相同。
func TestRecordGeneratesOpaqueID(t *testing.T) {
	s := newStore(t)
	seen := map[string]bool{}
	for i := 0; i < 8; i++ {
		got := mustRecord(t, s, rec("", time.Duration(i)*time.Second))[0]
		if !strings.HasPrefix(got.ID, "rq_") {
			t.Fatalf("缺省 id 应有 rq_ 前缀，实际 %q", got.ID)
		}
		if !idRe.MatchString(got.ID) {
			t.Fatalf("缺省 id 形状必须自洽：%q", got.ID)
		}
		if seen[got.ID] {
			t.Fatalf("缺省 id 重复：%q", got.ID)
		}
		seen[got.ID] = true
	}
}

// tableColumnsOf 读取表的列集合（测试用）。
func tableColumnsOf(t *testing.T, s *Store, table string) map[string]bool {
	t.Helper()
	s.mu.RLock()
	defer s.mu.RUnlock()
	cols, err := s.tableColumns(context.Background(), table)
	if err != nil {
		t.Fatalf("读取表 %s 结构失败: %v", table, err)
	}
	return cols
}
