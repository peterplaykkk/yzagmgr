package usage

import (
	"context"
	"errors"
	"fmt"
	"math"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/peterplaykkk/yzagmgr/internal/apierr"
)

// testLoc 是测试固定时区，使 DTO 的 at 渲染不受运行环境 TZ 影响。
var testLoc = time.FixedZone("UTC+8", 8*60*60)

// T0 是测试基准时刻（整秒，便于对 at 文本做精确断言）。
var T0 = time.Date(2026, 9, 22, 10, 0, 0, 0, testLoc)

// moneyEpsilon 是金额断言容差。
// 契约：金额是整数刻度累加后的单次除法，误差只来自 float64 的表示本身；
// 1e-9 覆盖真实量级下的表示误差，又不至于掩盖「算错了」。
const moneyEpsilon = 1e-9

// testClock 是可控时钟，供「记录缺省 At」与「隔离文件名戳」使用。
type testClock struct{ now time.Time }

func (c *testClock) Now() time.Time { return c.now }

// newStore 在临时目录建库（绝不碰用户真实数据根）。
// 时钟钉在 T0：记录的缺省 At 与聚合结果的 asOf 都取它，测试因此完全确定。
func newStore(t *testing.T) *Store {
	t.Helper()
	return newStoreAt(t, filepath.Join(t.TempDir(), "usage.db"),
		Options{Location: testLoc, Clock: (&testClock{now: T0}).Now})
}

// newStoreAt 在指定路径与选项下建库（重开、跨句柄、损坏场景用）。
func newStoreAt(t *testing.T, path string, opts Options) *Store {
	t.Helper()
	s, err := Open(context.Background(), path, opts)
	if err != nil {
		t.Fatalf("Open(%s) 失败: %v", path, err)
	}
	t.Cleanup(func() { _ = s.Close() })
	if got := s.Health().State; got != StateOK {
		t.Fatalf("库状态应为 %s，实际 %s（%s）", StateOK, got, s.Health().Detail)
	}
	return s
}

// rec 构造一条形状合法的记录；opts 用于按需改写字段。
func rec(id string, offset time.Duration, opts ...func(*Record)) Record {
	r := Record{
		ID:             id,
		At:             T0.Add(offset),
		App:            "claude",
		Prefix:         "claude",
		Provider:       "legacy",
		ModelRequested: "claude-fable-5-1",
		ModelUpstream:  "deepseek-flash",
		Status:         200,
		DurationMs:     1200,
		InputTokens:    100,
		OutputTokens:   50,
		Streamed:       true,
	}
	for _, o := range opts {
		o(&r)
	}
	return r
}

func mustRecord(t *testing.T, s *Store, recs ...Record) []RequestLog {
	t.Helper()
	out := make([]RequestLog, 0, len(recs))
	for _, r := range recs {
		got, err := s.Record(context.Background(), r)
		if err != nil {
			t.Fatalf("Record(%s) 失败: %v", r.ID, err)
		}
		out = append(out, got)
	}
	return out
}

func mustSetPricing(t *testing.T, s *Store, entries ...PricingEntry) PricingTable {
	t.Helper()
	got, err := s.ReplacePricing(context.Background(), PricingTable{Entries: entries})
	if err != nil {
		t.Fatalf("ReplacePricing 失败: %v", err)
	}
	return got
}

// priceDeepSeek / priceGPT 是测试用的两档已知单价（数值刻意好算）。
var (
	priceDeepSeek = PricingEntry{Model: "deepseek-flash", InputPerMillion: 10, OutputPerMillion: 30, Currency: "CNY"}
	priceGPT      = PricingEntry{Model: "gpt-5.5", InputPerMillion: 1.68, OutputPerMillion: 3.36, Currency: "CNY"}
)

// wantCost 独立地按「单价 × token / 1e6」算期望金额，不复用实现的整数刻度。
func wantCost(inputTokens, outputTokens int64, p PricingEntry) float64 {
	return (float64(inputTokens)*p.InputPerMillion + float64(outputTokens)*p.OutputPerMillion) / 1e6
}

// requireMoney 比较金额：nil 表示未知；非 nil 用契约容差（1e-9）比较。
func requireMoney(t *testing.T, ctx string, got, want *float64) {
	t.Helper()
	if (got == nil) != (want == nil) {
		t.Fatalf("%s: 金额未知标记不符：got=%s want=%s", ctx, fmtMoney(got), fmtMoney(want))
	}
	if got != nil && math.Abs(*got-*want) > moneyEpsilon {
		t.Fatalf("%s: 金额不符：got=%.12f want=%.12f（容差 %g）", ctx, *got, *want, moneyEpsilon)
	}
}

func fmtMoney(v *float64) string {
	if v == nil {
		return "未知(null)"
	}
	return fmt.Sprintf("%.12f", *v)
}

// sampleCount 造一个已知的样本数期望值（与 latencySampleCount 的非 nil 取值比较）。
func sampleCount(v int64) *int64 { return &v }

// requireSampleCount 断言 latencySampleCount：nil = 未采集/无法判定（页面显示未知），
// 非 nil = 已知有效样本数（0 = 暂无样本）。要求与期望同形再比数值，
// 「把 null 当 0」与「把 0 当 null」都当场失败。
func requireSampleCount(t *testing.T, ctx string, got, want *int64) {
	t.Helper()
	if (got == nil) != (want == nil) {
		t.Fatalf("%s: 样本数未知标记不符：got=%s want=%s", ctx, fmtSampleCount(got), fmtSampleCount(want))
	}
	if got != nil && *got != *want {
		t.Fatalf("%s: 样本数不符：got=%d want=%d", ctx, *got, *want)
	}
}

func fmtSampleCount(v *int64) string {
	if v == nil {
		return "未知(null)"
	}
	return fmt.Sprintf("%d", *v)
}

// requireRFC3339 断言时间字段是可解析的 RFC3339（契约里 at/asOf 的形状）。
func requireRFC3339(t *testing.T, ctx, value string) {
	t.Helper()
	if _, err := time.Parse(time.RFC3339, value); err != nil {
		t.Fatalf("%s 必须是 RFC3339，实际 %q: %v", ctx, value, err)
	}
}

// requireCode 断言错误是 apierr 信封里的指定码。
func requireCode(t *testing.T, ctx string, err error, want apierr.Code) {
	t.Helper()
	if err == nil {
		t.Fatalf("%s: 期望错误码 %s，实际 nil", ctx, want)
	}
	var ae *apierr.Error
	if !errors.As(err, &ae) {
		t.Fatalf("%s: 期望 *apierr.Error，实际 %T: %v", ctx, err, err)
	}
	if ae.Code != want {
		t.Fatalf("%s: 期望错误码 %s，实际 %s（%s）", ctx, want, ae.Code, ae.Message)
	}
}

// countRecords 统计库中记录条数（直查，不经聚合查询）。
func countRecords(t *testing.T, s *Store) int64 {
	t.Helper()
	return scalarCount(t, s, `SELECT COUNT(*) FROM request_logs`)
}

func scalarCount(t *testing.T, s *Store, query string, args ...any) int64 {
	t.Helper()
	s.mu.RLock()
	defer s.mu.RUnlock()
	n, err := s.scalarInt(context.Background(), query, args...)
	if err != nil {
		t.Fatalf("查询失败（%s）: %v", query, err)
	}
	return n
}

// setLastViewed 直改一行的最后查看时间（UC-048 修订的 LRU 序键夹具：
// 直接伪造「显式查看」的落库值，不依赖 RequestDetail 的时钟推进）。
func setLastViewed(t *testing.T, s *Store, id string, at time.Time) {
	t.Helper()
	s.mu.RLock()
	defer s.mu.RUnlock()
	if _, err := s.db.ExecContext(context.Background(),
		s.bind(`UPDATE request_logs SET last_viewed_at = ? WHERE id = ?`),
		s.nsArg(at.UnixNano()), id); err != nil {
		t.Fatalf("设置 %s 的最后查看时间失败: %v", id, err)
	}
}

// lastViewedAt 直读一行的最后查看时间（NULL = 从未查看）。
func lastViewedAt(t *testing.T, s *Store, id string) *time.Time {
	t.Helper()
	s.mu.RLock()
	defer s.mu.RUnlock()
	var v any
	if err := s.db.QueryRowContext(context.Background(),
		s.bind(`SELECT last_viewed_at FROM request_logs WHERE id = ?`), id).Scan(&v); err != nil {
		t.Fatalf("读取 %s 的最后查看时间失败: %v", id, err)
	}
	if v == nil {
		return nil
	}
	ns, err := nsFromDriver(v)
	if err != nil {
		t.Fatalf("解析 %s 的最后查看时间失败: %v", id, err)
	}
	out := time.Unix(0, ns)
	return &out
}

func itoa(v int) string { return strconv.Itoa(v) }

// itoaPad 生成定宽十进制（构造批量 id 用）。
func itoaPad(v int) string { return fmt.Sprintf("%03d", v) }
