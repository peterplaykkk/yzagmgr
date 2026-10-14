package usage

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/peterplaykkk/yzagmgr/internal/apierr"
)

// 本文件覆盖 token 限额的执行面存储（D32 / UC-041 A2 的读数与写入）。
//
// 期望取自契约（doc/api-v1.md §15）与用例，不取自实现：
//   - 无记录 / NULL / 0 ⇒ 不限（永不拒绝）；
//   - 恰好用满 ⇒ 已满（`>=`，不是 `>`）；
//   - 写是整值覆盖、可幂等重放；
//   - 路由器已用量 = request_logs 按 user_slug 的全时求和（不落第二本账）。

// quotaPtr 造一个非 nil 的 int64（表里显式写值的用例用）。
func quotaPtr(v int64) *int64 { return &v }

// viewJSON 把读数渲染成契约形状的 JSON 文本。
// 幂等断言用它而不是结构体比较：指针字段直接比较会退化成地址比较。
func viewJSON(t *testing.T, v QuotaView) string {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("序列化 QuotaView 失败: %v", err)
	}
	return string(raw)
}

// quotaOf 读一次限额读数并断言无错。
func quotaOf(t *testing.T, s *Store, slug string) Quota {
	t.Helper()
	q, err := s.QuotaFor(context.Background(), slug)
	if err != nil {
		t.Fatalf("QuotaFor(%s) 失败: %v", slug, err)
	}
	return q
}

// setQuota 写一次限额并断言无错（baseline 为 nil = 本次不动重置基线）。
func setQuota(t *testing.T, s *Store, slug string, total int64, external *int64, baseline *int64) Quota {
	t.Helper()
	q, err := s.SetQuota(context.Background(), slug, total, external, baseline)
	if err != nil {
		t.Fatalf("SetQuota(%s) 失败: %v", slug, err)
	}
	return q
}

// TestQuotaWithoutRowIsUnlimited 覆盖「无记录 ⇒ 不限」，且已用量照实报告。
func TestQuotaWithoutRowIsUnlimited(t *testing.T) {
	s := newStore(t)
	mustRecord(t, s,
		armRec("rq_q01", "user-a", "sess-1", 0, func(r *Record) { r.InputTokens, r.OutputTokens = 70, 30 }),
		armRec("rq_q02", "user-a", "sess-1", time.Minute, func(r *Record) { r.InputTokens, r.OutputTokens = 5, 5 }),
		// 别的用户与九前缀车道（无臂标识）不得计入 user-a。
		armRec("rq_q03", "user-b", "sess-2", time.Minute, func(r *Record) { r.InputTokens, r.OutputTokens = 900, 900 }),
		rec("rq_q04", 2*time.Minute, func(r *Record) { r.InputTokens, r.OutputTokens = 800, 800 }),
	)

	q := quotaOf(t, s, "user-a")
	if q.QuotaTotal != nil {
		t.Fatalf("无记录时 quota 应为 NULL（不限），实际 %v", *q.QuotaTotal)
	}
	if q.RouterUsed != 110 || q.ExternalUsed != 0 || q.UsedTotal() != 110 {
		t.Fatalf("已用量口径不符：%+v", q)
	}
	if q.Enforced() || q.Exhausted() {
		t.Fatalf("无记录不得判成受限或已满：%+v", q)
	}
	if q.Remaining() != nil {
		t.Fatalf("不限时剩余额度必须是 nil（null），实际 %d", *q.Remaining())
	}
	view := q.View()
	if view.Quota != nil || view.Remaining != nil {
		t.Fatalf("不限时 view 的 quota/remaining 都应是 null：%+v", view)
	}
	if view.UsedTotal != 110 || view.UsedRouter != 110 {
		t.Fatalf("view 已用量不符：%+v", view)
	}
}

// TestQuotaDecisionRules 表驱动覆盖判定规则：0/NULL 不限、未达放行、恰好用满拒绝、超出拒绝。
func TestQuotaDecisionRules(t *testing.T) {
	cases := []struct {
		name       string
		quotaTotal *int64 // nil = 不写行（NULL）
		writeRow   bool
		tokens     int64 // 路由器已用量（单条记录）
		external   *int64
		baseline   int64 // 重置基线（D34）
		wantEnf    bool
		wantExh    bool
		wantRemain *int64
	}{
		{name: "无记录", writeRow: false, tokens: 500, wantEnf: false, wantExh: false},
		{name: "NULL=不限", writeRow: true, quotaTotal: nil, tokens: 500, wantEnf: false, wantExh: false},
		{name: "0=不限", writeRow: true, quotaTotal: quotaPtr(0), tokens: 500, wantEnf: false, wantExh: false},
		{name: "未达限额", writeRow: true, quotaTotal: quotaPtr(1000), tokens: 400, wantEnf: true, wantExh: false, wantRemain: quotaPtr(600)},
		{name: "恰好用满", writeRow: true, quotaTotal: quotaPtr(1000), tokens: 1000, wantEnf: true, wantExh: true, wantRemain: quotaPtr(0)},
		{name: "超出限额", writeRow: true, quotaTotal: quotaPtr(1000), tokens: 1200, wantEnf: true, wantExh: true, wantRemain: quotaPtr(0)},
		{name: "外部折算计入判定", writeRow: true, quotaTotal: quotaPtr(1000), tokens: 400, external: quotaPtr(600), wantEnf: true, wantExh: true, wantRemain: quotaPtr(0)},
		{name: "外部折算未满", writeRow: true, quotaTotal: quotaPtr(1000), tokens: 400, external: quotaPtr(599), wantEnf: true, wantExh: false, wantRemain: quotaPtr(1)},
		// D34 重置：判定与剩余额度都按 used_eff = max(0, 已用合计 − 基线)。
		{name: "基线=已用（重置后放行）", writeRow: true, quotaTotal: quotaPtr(1000), tokens: 1000, baseline: 1000, wantEnf: true, wantExh: false, wantRemain: quotaPtr(1000)},
		{name: "基线小于已用：按差额判", writeRow: true, quotaTotal: quotaPtr(1000), tokens: 900, baseline: 400, wantEnf: true, wantExh: false, wantRemain: quotaPtr(500)},
		{name: "基线小于已用且差额超限：仍拒绝", writeRow: true, quotaTotal: quotaPtr(1000), tokens: 900, external: quotaPtr(600), baseline: 400, wantEnf: true, wantExh: true, wantRemain: quotaPtr(0)},
		{name: "基线高于已用：生效用量归 0（不出现负用量）", writeRow: true, quotaTotal: quotaPtr(1000), tokens: 300, baseline: 5000, wantEnf: true, wantExh: false, wantRemain: quotaPtr(1000)},
		{name: "重置后外部折算再涨：重新逼近限额", writeRow: true, quotaTotal: quotaPtr(1000), tokens: 1000, baseline: 1000, external: quotaPtr(999), wantEnf: true, wantExh: false, wantRemain: quotaPtr(1)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := newStore(t)
			ctx := context.Background()
			if _, err := s.Record(ctx, armRec("rq_r01", "user-a", "sess-1", 0, func(r *Record) {
				r.InputTokens, r.OutputTokens = tc.tokens, 0
			})); err != nil {
				t.Fatalf("写入用量记录失败: %v", err)
			}
			if tc.writeRow {
				// 直插一行：本用例要能造出 NULL 限额（PUT 写不出 NULL，0 才是它的「不限」写法）。
				if _, err := s.db.ExecContext(ctx,
					s.bind(`INSERT INTO user_quotas (user_slug, quota_total, external_used, used_baseline, updated_at) VALUES (?, ?, ?, ?, ?)`),
					"user-a", nullIfNil(tc.quotaTotal), externalOrZero(tc.external), tc.baseline,
					s.nsArg(T0.UnixNano())); err != nil {
					t.Fatalf("预置限额行失败: %v", err)
				}
			}
			q := quotaOf(t, s, "user-a")
			if q.Enforced() != tc.wantEnf || q.Exhausted() != tc.wantExh {
				t.Fatalf("判定不符：Enforced=%v Exhausted=%v，期望 %v/%v（%+v）",
					q.Enforced(), q.Exhausted(), tc.wantEnf, tc.wantExh, q)
			}
			got := q.Remaining()
			switch {
			case tc.wantRemain == nil && got != nil:
				t.Fatalf("剩余额度应为 nil（不限），实际 %d", *got)
			case tc.wantRemain != nil && got == nil:
				t.Fatalf("剩余额度应为 %d，实际 nil", *tc.wantRemain)
			case tc.wantRemain != nil && *got != *tc.wantRemain:
				t.Fatalf("剩余额度 = %d，期望 %d", *got, *tc.wantRemain)
			}
		})
	}
}

func nullIfNil(v *int64) any {
	if v == nil {
		return nil
	}
	return *v
}

func externalOrZero(v *int64) int64 {
	if v == nil {
		return 0
	}
	return *v
}

// TestSetQuotaIsWholeValueAndIdempotent 覆盖写入语义：整值覆盖、重复推送同值幂等、
// 不带 external_used 的推送不动外部折算。
func TestSetQuotaIsWholeValueAndIdempotent(t *testing.T) {
	s := newStore(t)

	first := setQuota(t, s, "user-a", 100000, quotaPtr(250), quotaPtr(50))
	if first.QuotaTotal == nil || *first.QuotaTotal != 100000 || first.ExternalUsed != 250 || first.UsedBaseline != 50 {
		t.Fatalf("首次写入读数不符：%+v", first)
	}
	// 幂等：同值重放后读数逐字相同（agserv 重连后重推不得翻倍）。
	again := setQuota(t, s, "user-a", 100000, quotaPtr(250), quotaPtr(50))
	if viewJSON(t, again.View()) != viewJSON(t, first.View()) {
		t.Fatalf("同值重放不应改变读数：%s vs %s", viewJSON(t, again.View()), viewJSON(t, first.View()))
	}
	if n := scalarCount(t, s, `SELECT COUNT(*) FROM user_quotas`); n != 1 {
		t.Fatalf("同值重放应仍只有一行，实际 %d 行", n)
	}
	// 只改限额：外部折算保持原值。
	onlyTotal := setQuota(t, s, "user-a", 200000, nil, nil)
	if onlyTotal.ExternalUsed != 250 || *onlyTotal.QuotaTotal != 200000 {
		t.Fatalf("只改限额时应保留 external_used：%+v", onlyTotal)
	}
	// 基线（D34）：整值覆盖；省略时保持原值——省略绝不是「清零」。
	withBaseline := setQuota(t, s, "user-a", 200000, nil, quotaPtr(1234))
	if withBaseline.UsedBaseline != 1234 {
		t.Fatalf("基线应整值写入：%+v", withBaseline)
	}
	kept := setQuota(t, s, "user-a", 300000, quotaPtr(9), nil)
	if kept.UsedBaseline != 1234 || kept.ExternalUsed != 9 {
		t.Fatalf("省略基线时不得清零：%+v", kept)
	}
	// 覆盖为 0（不限）：读数是 0 而不是 NULL，判定立即失效。
	unlimited := setQuota(t, s, "user-a", 0, quotaPtr(0), quotaPtr(0))
	if unlimited.QuotaTotal == nil || *unlimited.QuotaTotal != 0 {
		t.Fatalf("0 应原样落库（不是 NULL）：%+v", unlimited)
	}
	if unlimited.Enforced() || unlimited.Exhausted() || unlimited.Remaining() != nil {
		t.Fatalf("0=不限，不得再判成受限：%+v", unlimited)
	}
	if raw, err := json.Marshal(unlimited.View()); err != nil {
		t.Fatalf("序列化失败: %v", err)
	} else {
		var m map[string]json.RawMessage
		if err := json.Unmarshal(raw, &m); err != nil {
			t.Fatalf("解析失败: %v", err)
		}
		if string(m["quota"]) != "0" {
			t.Fatalf("0 必须呈现为 0（不限），实际 %s", m["quota"])
		}
		if string(m["remaining"]) != "null" {
			t.Fatalf("不限时 remaining 必须是 null，实际 %s", m["remaining"])
		}
	}
}

// TestQuotaResetBaselineSemantics 是 D34 的主流程：超限 → admin 重置（抬基线）→ 恢复可用 →
// 继续计费后再次逼近限额；全程限额与历史不改写，重置前后都可由「已用合计 − 基线」复算。
func TestQuotaResetBaselineSemantics(t *testing.T) {
	s := newStore(t)
	track := func(id string, tokens int64) {
		t.Helper()
		if _, err := s.Record(context.Background(), armRec(id, "user-a", "sess-1", 0, func(r *Record) {
			r.InputTokens, r.OutputTokens = tokens, 0
		})); err != nil {
			t.Fatalf("写入用量记录失败: %v", err)
		}
	}

	track("rq_rs01", 600)
	track("rq_rs02", 400)
	setQuota(t, s, "user-a", 1000, nil, nil) // 限额 1000，已用 1000 ⇒ 已满
	if q := quotaOf(t, s, "user-a"); !q.Exhausted() {
		t.Fatalf("1000/1000 应判成已满：%+v", q)
	}

	// 重置：限额不变、基线抬到当前已用 ⇒ 放行恢复，剩余额度回到满额。
	reset := setQuota(t, s, "user-a", 1000, nil, quotaPtr(1000))
	if reset.UsedTotal() != 1000 || reset.UsedEffective() != 0 || reset.Exhausted() {
		t.Fatalf("重置后生效用量应归 0：%+v", reset)
	}
	if got := reset.Remaining(); got == nil || *got != 1000 {
		t.Fatalf("重置后剩余额度应为满额：%+v", reset)
	}

	// 继续计费：生效用量从 0 起算，差额判定照样生效。
	track("rq_rs03", 700)
	q := quotaOf(t, s, "user-a")
	if q.UsedBaseline != 1000 || q.UsedTotal() != 1700 || q.UsedEffective() != 700 {
		t.Fatalf("重置后应只按差额计：%+v", q)
	}
	if q.Exhausted() {
		t.Fatalf("700/1000 不该判成已满：%+v", q)
	}
	track("rq_rs04", 300)
	if q := quotaOf(t, s, "user-a"); !q.Exhausted() || *q.Remaining() != 0 {
		t.Fatalf("再涨到 1000/1000 应重新判满：%+v", q)
	}
	// 历史不被重置改写：记录条数只增不改（对账可复算）。
	if n := countRecords(t, s); n != 4 {
		t.Fatalf("重置不得改写历史，记录数 = %d", n)
	}
}

// TestQuotaInputValidation 覆盖入参校验：非法 slug 与非负约束都在存储层被拒（invalid_request）。
func TestQuotaInputValidation(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	for _, bad := range []string{"", ".hidden", "a..b", "user name", "user/name", string(make([]byte, 200))} {
		if _, err := s.QuotaFor(ctx, bad); err == nil {
			t.Fatalf("非法 slug %q 必须被拒", bad)
		} else {
			requireCode(t, "QuotaFor("+bad+")", err, apierr.CodeInvalidRequest)
		}
		if _, err := s.SetQuota(ctx, bad, 100, nil, nil); err == nil {
			t.Fatalf("非法 slug %q 的写入必须被拒", bad)
		}
	}
	if _, err := s.SetQuota(ctx, "user-a", -1, nil, nil); err == nil {
		t.Fatal("负限额必须被拒")
	} else {
		requireCode(t, "SetQuota(-1)", err, apierr.CodeInvalidRequest)
	}
	if _, err := s.SetQuota(ctx, "user-a", 100, quotaPtr(-1), nil); err == nil {
		t.Fatal("负的外部折算必须被拒")
	} else {
		requireCode(t, "SetQuota(external=-1)", err, apierr.CodeInvalidRequest)
	}
	if _, err := s.SetQuota(ctx, "user-a", 100, nil, quotaPtr(-1)); err == nil {
		t.Fatal("负的重置基线必须被拒")
	} else {
		requireCode(t, "SetQuota(baseline=-1)", err, apierr.CodeInvalidRequest)
	}
	if n := scalarCount(t, s, `SELECT COUNT(*) FROM user_quotas`); n != 0 {
		t.Fatalf("被拒的写入不得留下行，实际 %d 行", n)
	}
	// 合法边界：128 字节的 slug、恰好 0 的折算都收。
	long128 := make([]byte, 128)
	for i := range long128 {
		long128[i] = 'a'
	}
	if _, err := s.SetQuota(ctx, string(long128), 1, quotaPtr(0), nil); err != nil {
		t.Fatalf("128 字节 slug 应被接受: %v", err)
	}
}

// TestQuotaConcurrentWritesKeepCountsConsistent 覆盖并发下的计数一致性：
// 并发记账与并发推送之后，「路由器累计 == 写入的 token 总和」，且限额行只有一行。
func TestQuotaConcurrentWritesKeepCountsConsistent(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()

	const (
		writers  = 4
		perWrite = 10
	)
	var wg sync.WaitGroup
	for w := 0; w < writers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := 0; i < perWrite; i++ {
				rec := armRec(fmt.Sprintf("rq_c_%d_%d", w, i), "user-a", "sess-1", time.Duration(i)*time.Second,
					func(r *Record) { r.InputTokens, r.OutputTokens = 2, 3 })
				if _, err := s.Record(ctx, rec); err != nil {
					t.Errorf("并发记账失败: %v", err)
					return
				}
			}
		}(w)
	}
	// 并发的限额推送（整值覆盖）：最终值必须是其中某一个，不能是累加值。
	var wg2 sync.WaitGroup
	for w := 0; w < writers; w++ {
		wg2.Add(1)
		go func(w int) {
			defer wg2.Done()
			if _, err := s.SetQuota(ctx, "user-a", 1000, quotaPtr(int64(100+w)), nil); err != nil {
				t.Errorf("并发推送失败: %v", err)
			}
		}(w)
	}
	wg.Wait()
	wg2.Wait()

	q := quotaOf(t, s, "user-a")
	want := int64(writers * perWrite * 5)
	if q.RouterUsed != want {
		t.Fatalf("并发后路由器累计 = %d，期望 %d（记账不得丢失或重复）", q.RouterUsed, want)
	}
	if q.ExternalUsed < 100 || q.ExternalUsed >= 100+writers {
		t.Fatalf("external_used = %d，应为某一次推送的整值（100..%d）", q.ExternalUsed, 100+writers-1)
	}
	if n := scalarCount(t, s, `SELECT COUNT(*) FROM user_quotas`); n != 1 {
		t.Fatalf("并发推送后应只有一行限额，实际 %d 行", n)
	}
	if n := countRecords(t, s); n != want/5 {
		t.Fatalf("并发记账后应有 %d 条记录，实际 %d", want/5, n)
	}
}

// TestQuotaViewJSONKeysMatchContract 钉住 §15 的七个字段名（多一个少一个都算漂移）。
func TestQuotaViewJSONKeysMatchContract(t *testing.T) {
	got := jsonKeys(t, QuotaView{})
	want := []string{"quota", "remaining", "used_baseline", "used_eff", "used_external", "used_router", "used_total"}
	if len(got) != len(want) {
		t.Fatalf("字段数不符：got %v want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("字段集合不符：got %v want %v", got, want)
		}
	}
}

// TestSQLiteSchemaMigratesV2ToV3 覆盖加表迁移：既有 v2 库（没有限额表）在打开时被升到 v3，
// 历史记录保留，限额表空着（= 全员「无记录 ⇒ 不限」）且立即可写。
func TestSQLiteSchemaMigratesV2ToV3(t *testing.T) {
	path := filepath.Join(t.TempDir(), "usage.db")
	// 先造一个当前版本的库，再把「限额表」「v5 才有的查看时间列」「v6 才有的测量状态列」
	// 与版本戳退回 v2 的样子。
	s := newStoreAt(t, path, Options{Location: testLoc, Clock: (&testClock{now: T0}).Now})
	mustRecord(t, s, armRec("rq_v2a", "user-a", "sess-1", 0, func(r *Record) {
		r.InputTokens, r.OutputTokens = 400, 100
	}))
	if err := s.Close(); err != nil {
		t.Fatalf("关闭库失败: %v", err)
	}
	prepareRawDB(t, path, `DROP TABLE user_quotas`,
		`ALTER TABLE request_logs DROP COLUMN last_viewed_at`,
		`ALTER TABLE request_logs DROP COLUMN duration_measured`,
		`ALTER TABLE request_logs DROP COLUMN rule_version`,
		`ALTER TABLE request_logs DROP COLUMN cached_tokens`,
		`ALTER TABLE pricing DROP COLUMN cache_read_per_million`,
		`DROP TABLE billing_ledger`,
		`DROP TABLE pricing_rules`,
		`DROP TABLE session_quotas`)
	setRawUserVersion(t, path, 2)

	migrated := newStoreAt(t, path, Options{Location: testLoc, Clock: (&testClock{now: T0}).Now})
	if got := migrated.Health().State; got != StateOK {
		t.Fatalf("v2 库应能被迁移到 v3 并可用，实际 %s（%s）", got, migrated.Health().Detail)
	}
	if n := scalarCount(t, migrated, `SELECT COUNT(*) FROM user_quotas`); n != 0 {
		t.Fatalf("升级后的限额表应为空（无记录 ⇒ 不限），实际 %d 行", n)
	}
	q := quotaOf(t, migrated, "user-a")
	if q.Enforced() || q.RouterUsed != 500 {
		t.Fatalf("升级后应是不限、且历史已用量照实：%+v", q)
	}
	setQuota(t, migrated, "user-a", 1000, quotaPtr(7), nil)
	if got := scalarCount(t, migrated, `SELECT COUNT(*) FROM user_quotas`); got != 1 {
		t.Fatalf("升级后应能写限额，实际 %d 行", got)
	}
	// 版本戳升到位：再打开一次不应重复迁移（幂等）。
	if err := migrated.Close(); err != nil {
		t.Fatalf("关闭库失败: %v", err)
	}
	again := newStoreAt(t, path, Options{Location: testLoc, Clock: (&testClock{now: T0}).Now})
	if got := quotaOf(t, again, "user-a"); got.QuotaTotal == nil || *got.QuotaTotal != 1000 || got.ExternalUsed != 7 {
		t.Fatalf("重开后限额应保留：%+v", got)
	}
}

// TestQuotaReportsStorageFailures 覆盖存储故障面：读写都报 storage（绝不静默返回「不限」——
// 那会把「读不到」说成「没有限额」，是限额面最危险的失败方向）。
func TestQuotaReportsStorageFailures(t *testing.T) {
	ctx := context.Background()

	t.Run("库不可用", func(t *testing.T) {
		s := newStore(t)
		// 直接把库置成不可用（等价于启动时打开失败的装配结果）。
		s.mu.Lock()
		_ = s.db.Close()
		s.db = nil
		s.state = StateCorrupt
		s.detail = "测试：库不可用"
		s.mu.Unlock()

		if _, err := s.QuotaFor(ctx, "user-a"); err == nil {
			t.Fatal("库不可用时读限额必须报错")
		} else {
			requireCode(t, "QuotaFor", err, apierr.CodeStorage)
		}
		if _, err := s.SetQuota(ctx, "user-a", 100, nil, nil); err == nil {
			t.Fatal("库不可用时写限额必须报错")
		} else {
			requireCode(t, "SetQuota", err, apierr.CodeStorage)
		}
	})

	t.Run("句柄已关闭", func(t *testing.T) {
		s := newStore(t)
		s.mu.Lock()
		_ = s.db.Close() // 状态仍 ok，但连接已不可用
		s.mu.Unlock()
		if _, err := s.QuotaFor(ctx, "user-a"); err == nil {
			t.Fatal("连接不可用时读限额必须报错")
		}
		if _, err := s.SetQuota(ctx, "user-a", 100, nil, nil); err == nil {
			t.Fatal("连接不可用时写限额必须报错")
		}
	})

	t.Run("限额表缺失", func(t *testing.T) {
		s := newStore(t)
		// 表被外部删除：写入失败必须如实报出，不得留下「以为写上了」。
		if _, err := s.db.ExecContext(ctx, `DROP TABLE user_quotas`); err != nil {
			t.Fatalf("删表失败: %v", err)
		}
		if _, err := s.SetQuota(ctx, "user-a", 100, nil, nil); err == nil {
			t.Fatal("限额表缺失时写入必须报错")
		} else {
			requireCode(t, "SetQuota", err, apierr.CodeStorage)
		}
	})

	t.Run("记账表缺失", func(t *testing.T) {
		s := newStore(t)
		// 只删记账表：写入成功但回读（求和）失败 —— 事务必须整体失败，
		// 不能让「写进去了但读不出来」以半个成功的样子返回。
		if _, err := s.db.ExecContext(ctx, `DROP TABLE request_logs`); err != nil {
			t.Fatalf("删表失败: %v", err)
		}
		if _, err := s.SetQuota(ctx, "user-a", 100, nil, nil); err == nil {
			t.Fatal("回读失败时写入必须整体报错")
		} else {
			requireCode(t, "SetQuota", err, apierr.CodeStorage)
		}
		if n := scalarCount(t, s, `SELECT COUNT(*) FROM user_quotas`); n != 0 {
			t.Fatalf("整体失败的事务不得留下半写状态，实际 %d 行", n)
		}
	})
}

// TestPostgresLegacyMigrationCarriesLimits 是 PG 门控用例：一次性搬迁必须把限额表一并平移，
// 否则升级瞬间全员退回「无记录 ⇒ 不限」，超限用户在 agserv 下一次推送前是有额度的。
func TestPostgresLegacyMigrationCarriesLimits(t *testing.T) {
	legacy := filepath.Join(t.TempDir(), "usage.db")
	src := newStoreAt(t, legacy, Options{Location: testLoc, Clock: (&testClock{now: T0}).Now})
	mustRecord(t, src, armRec("rq_leg01", "user-a", "sess-1", 0, func(r *Record) {
		r.InputTokens, r.OutputTokens = 600, 400
	}))
	setQuota(t, src, "user-a", 1000, quotaPtr(250), quotaPtr(1)) // 限额 + 折算 + 基线三者都要平移
	setQuota(t, src, "user-b", 0, nil, nil)                      // 不限的一行也要搬（它是「显式置不限」，与「无记录」不同）
	// NULL 限额的行（PUT 写不出来，但库里可能有既有行/第三方写入）：搬迁必须保形。
	if _, err := src.db.ExecContext(context.Background(),
		src.bind(`INSERT INTO user_quotas (user_slug, quota_total, external_used, used_baseline, updated_at) VALUES (?, NULL, 5, 42, ?)`),
		"user-c", src.nsArg(T0.UnixNano())); err != nil {
		t.Fatalf("预置 NULL 限额失败: %v", err)
	}
	if err := src.Close(); err != nil {
		t.Fatalf("关闭源库失败: %v", err)
	}

	dst, err := Open(context.Background(), legacy, Options{
		Location: testLoc, Clock: (&testClock{now: T0}).Now, DSN: pgSchemaDSN(t),
	})
	if err != nil {
		t.Fatalf("Open(PG) 失败: %v", err)
	}
	t.Cleanup(func() { _ = dst.Close() })
	if got := dst.Health().State; got != StateOK {
		t.Fatalf("状态应为 %s，实际 %s（%s）", StateOK, got, dst.Health().Detail)
	}
	note := dst.MigrationNote()
	if !strings.Contains(note, "user_quotas 3 行") {
		t.Fatalf("搬迁说明应报出限额表行数：%q", note)
	}
	got := quotaOf(t, dst, "user-a")
	if got.QuotaTotal == nil || *got.QuotaTotal != 1000 || got.ExternalUsed != 250 || got.UsedBaseline != 1 {
		t.Fatalf("限额/折算/基线未随搬迁平移：%+v", got)
	}
	if got.RouterUsed != 1000 {
		t.Fatalf("记账也未平移或口径不符：%+v", got)
	}
	// 已用 = 路由器 1000 + 外部折算 250 = 1250 ≥ 限额 1000 ⇒ 已满（判定也要跟着搬过来）。
	if !got.Exhausted() {
		t.Fatalf("限额 1000、已用 1250 应判成已满：%+v", got)
	}
	if other := quotaOf(t, dst, "user-b"); other.QuotaTotal == nil || *other.QuotaTotal != 0 {
		t.Fatalf("显式置不限的行也要搬迁（与「无记录」不同）：%+v", other)
	}
	if nul := quotaOf(t, dst, "user-c"); nul.QuotaTotal != nil || nul.ExternalUsed != 5 || nul.UsedBaseline != 42 {
		t.Fatalf("NULL 限额与基线必须保形搬过来（是 NULL，不是 0）：%+v", nul)
	}
}

// TestPostgresLegacyMigrationWithoutLimits 是 PG 门控用例：源库有限额表但一行都没有
// （限额面尚未推送过任何用户）时，搬迁照样成立且如实报 0 行。
func TestPostgresLegacyMigrationWithoutLimits(t *testing.T) {
	legacy := filepath.Join(t.TempDir(), "usage.db")
	src := newStoreAt(t, legacy, Options{Location: testLoc, Clock: (&testClock{now: T0}).Now})
	mustRecord(t, src, armRec("rq_leg02", "user-a", "sess-1", 0))
	if err := src.Close(); err != nil {
		t.Fatalf("关闭源库失败: %v", err)
	}

	dst, err := Open(context.Background(), legacy, Options{
		Location: testLoc, Clock: (&testClock{now: T0}).Now, DSN: pgSchemaDSN(t),
	})
	if err != nil {
		t.Fatalf("Open(PG) 失败: %v", err)
	}
	t.Cleanup(func() { _ = dst.Close() })
	if got := dst.Health().State; got != StateOK {
		t.Fatalf("状态应为 %s，实际 %s（%s）", StateOK, got, dst.Health().Detail)
	}
	if note := dst.MigrationNote(); !strings.Contains(note, "user_quotas 0 行") {
		t.Fatalf("搬迁说明应如实报 0 行限额：%q", note)
	}
	if q := quotaOf(t, dst, "user-a"); q.Enforced() || q.RouterUsed != 150 {
		t.Fatalf("无限额记录 ⇒ 不限，已用量照实（rec 缺省 100+50=150）：%+v", q)
	}
}

// TestPostgresRebuildClearsLimits 是 PG 门控用例：重建（隔离历史）后限额表**为空**
// ——与 SQLite 换库文件的语义对齐，且不让「旧限额 + 清零的历史」拼在一起。
func TestPostgresRebuildClearsLimits(t *testing.T) {
	s := newPGStore(t)
	mustRecord(t, s, armRec("rq_rb01", "user-a", "sess-1", 0, func(r *Record) {
		r.InputTokens, r.OutputTokens = 900, 100
	}))
	setQuota(t, s, "user-a", 500, quotaPtr(80), nil)
	if !quotaOf(t, s, "user-a").Exhausted() {
		t.Fatal("预置状态应为已满（900+100+80 ≥ 500）")
	}

	if _, err := s.Rebuild(context.Background()); err != nil {
		t.Fatalf("Rebuild 失败: %v", err)
	}
	if got := s.Health().State; got != StateOK {
		t.Fatalf("重建后状态应为 %s，实际 %s（%s）", StateOK, got, s.Health().Detail)
	}
	q := quotaOf(t, s, "user-a")
	if q.QuotaTotal != nil || q.Enforced() || q.RouterUsed != 0 || q.Exhausted() {
		t.Fatalf("重建后应无记录 ⇒ 不限、历史归零：%+v", q)
	}
	if n := scalarCount(t, s, `SELECT COUNT(*) FROM user_quotas`); n != 0 {
		t.Fatalf("重建后限额表应为空，实际 %d 行", n)
	}
	// 重建之后照常可写（agserv 重推的落点）。
	setQuota(t, s, "user-a", 700, nil, nil)
	if got := quotaOf(t, s, "user-a"); got.QuotaTotal == nil || *got.QuotaTotal != 700 {
		t.Fatalf("重建后应能重新写入限额：%+v", got)
	}
}

// TestPostgresMigrationRejectsPreExistingLimits 是 PG 门控用例：版本戳缺失的库里**已经有**
// 限额行（外部写入或人工残留）时，一次性搬迁不得把两边合在一起静默成功——
// 两种撞法都必须以「对账不过」响亮报出（corrupt），且整体回滚。
func TestPostgresMigrationRejectsPreExistingLimits(t *testing.T) {
	cases := []struct {
		name     string
		ghostKey string
		contains string
	}{
		{"同名 slug：插入冲突", "user-a", "写入限额表失败"},
		{"异名 slug：全表对账行数不符", "ghost", "user_quotas 行数不符"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dsn := pgSchemaDSN(t)
			admin, err := openPG(dsn)
			if err != nil {
				t.Fatalf("连接 PG 失败: %v", err)
			}
			// 手工造一个「无版本标记但已有我们的表与一行限额」的库。
			for _, stmt := range pgSchemaStatements {
				if _, err := admin.Exec(stmt); err != nil {
					t.Fatalf("预置 PG schema 失败（%s）: %v", firstLine(stmt), err)
				}
			}
			if _, err := admin.Exec(
				`INSERT INTO user_quotas (user_slug, quota_total, external_used, updated_at) VALUES ($1, 1, 0, now())`,
				tc.ghostKey); err != nil {
				t.Fatalf("预置限额行失败: %v", err)
			}
			_ = admin.Close()

			legacy := filepath.Join(t.TempDir(), "usage.db")
			src := newStoreAt(t, legacy, Options{Location: testLoc, Clock: (&testClock{now: T0}).Now})
			setQuota(t, src, "user-a", 1000, nil, nil)
			if err := src.Close(); err != nil {
				t.Fatalf("关闭源库失败: %v", err)
			}

			s, err := Open(context.Background(), legacy, Options{
				Location: testLoc, Clock: (&testClock{now: T0}).Now, DSN: dsn,
			})
			if s == nil {
				t.Fatal("Open 必须返回非 nil Store")
			}
			t.Cleanup(func() { _ = s.Close() })
			if err == nil || !strings.Contains(err.Error(), tc.contains) {
				t.Fatalf("应报「%s」，实际 %v", tc.contains, err)
			}
			if got := s.Health().State; got != StateCorrupt {
				t.Fatalf("状态应为 %s，实际 %s", StateCorrupt, got)
			}
			// 整体回滚：记账表不得留下半搬状态（失败后库里仍只有那行外来限额）。
			if n := countRecordsLean(t, dsn); n != 0 {
				t.Fatalf("搬迁失败时不得留下已搬的记录，实际 %d 行", n)
			}
		})
	}
}

// countRecordsLean 直连 PG 数 request_logs 行数（用独立连接，不受被测 Store 状态影响）。
func countRecordsLean(t *testing.T, dsn string) int64 {
	t.Helper()
	db, err := openPG(dsn)
	if err != nil {
		t.Fatalf("连接 PG 失败: %v", err)
	}
	defer func() { _ = db.Close() }()
	var n int64
	if err := db.QueryRow(`SELECT COUNT(*) FROM request_logs`).Scan(&n); err != nil {
		t.Fatalf("数记录失败: %v", err)
	}
	return n
}

// TestPostgresMigrationRejectsMalformedLegacyLimits 是 PG 门控用例：旧库限额表里的
// `updated_at` 不是可解析的时刻（外来写入/手工改过）⇒ 搬迁响亮失败，绝不给 PG 塞一个错时间。
func TestPostgresMigrationRejectsMalformedLegacyLimits(t *testing.T) {
	legacy := filepath.Join(t.TempDir(), "usage.db")
	src := newStoreAt(t, legacy, Options{Location: testLoc, Clock: (&testClock{now: T0}).Now})
	setQuota(t, src, "user-a", 1000, nil, nil)
	if err := src.Close(); err != nil {
		t.Fatalf("关闭源库失败: %v", err)
	}
	// 把限额表换成「形状对、内容坏」的版本：列齐（过自检）但时刻列不可解析。
	prepareRawDB(t, legacy,
		`DROP TABLE user_quotas`,
		`CREATE TABLE user_quotas (user_slug TEXT PRIMARY KEY, quota_total INTEGER,
			external_used INTEGER NOT NULL DEFAULT 0, used_baseline INTEGER NOT NULL DEFAULT 0,
			updated_at TEXT NOT NULL) STRICT`,
		`INSERT INTO user_quotas (user_slug, quota_total, external_used, used_baseline, updated_at)
			VALUES ('user-a', 1000, 0, 0, 'not-a-time')`)

	s, err := Open(context.Background(), legacy, Options{
		Location: testLoc, Clock: (&testClock{now: T0}).Now, DSN: pgSchemaDSN(t),
	})
	if s == nil {
		t.Fatal("Open 必须返回非 nil Store")
	}
	t.Cleanup(func() { _ = s.Close() })
	if err == nil || !strings.Contains(err.Error(), "updated_at 列不可用") {
		t.Fatalf("坏时刻必须被逮住，实际 %v", err)
	}
	if got := s.Health().State; got != StateCorrupt {
		t.Fatalf("状态应为 %s，实际 %s", StateCorrupt, got)
	}
}

// TestPostgresQuotaRoundTrip 是 PG 后端的门控用例：限额表在 PG 上建得出、写得进、
// 读得回（含 `SUM(input_tokens + output_tokens)` 在 bigint 列上的取值类型）。
func TestPostgresQuotaRoundTrip(t *testing.T) {
	s := newPGStore(t)
	ctx := context.Background()

	if cols := tableColumnsOf(t, s, "user_quotas"); len(cols) == 0 {
		t.Fatal("PG 侧必须建出 user_quotas 表")
	}
	mustRecord(t, s, armRec("rq_pgq1", "user-a", "sess-1", 0, func(r *Record) {
		r.InputTokens, r.OutputTokens = 700, 300
	}))

	// 大取值：bigint 往返不得被截断或降精度。
	big := int64(1<<40 + 7)
	q := setQuota(t, s, "user-a", big, quotaPtr(1<<33), quotaPtr(1000))
	if q.QuotaTotal == nil || *q.QuotaTotal != big || q.ExternalUsed != 1<<33 || q.UsedBaseline != 1000 {
		t.Fatalf("PG 往返后取值不符：%+v", q)
	}
	// 生效用量 = max(0, 1000(路由器) + 2^33(外部) − 1000(基线)) = 2^33；限额远大于它 ⇒ 不超限。
	if q.UsedEffective() != 1<<33 || q.Exhausted() {
		t.Fatalf("生效用量应扣掉基线（只剩外部折算）、且远未超限：%+v", q)
	}
	if q.RouterUsed != 1000 {
		t.Fatalf("PG 侧路由器累计 = %d，期望 1000", q.RouterUsed)
	}
	if !q.Enforced() || q.Exhausted() {
		t.Fatalf("判定不符：%+v", q)
	}
	// NULL 限额：PG 侧同样读成「不限」。
	if _, err := s.db.ExecContext(ctx,
		s.bind(`INSERT INTO user_quotas (user_slug, quota_total, external_used, updated_at) VALUES (?, NULL, 0, ?)`),
		"user-null", s.nsArg(T0.UnixNano())); err != nil {
		t.Fatalf("预置 NULL 限额失败: %v", err)
	}
	if got := quotaOf(t, s, "user-null"); got.QuotaTotal != nil || got.Enforced() {
		t.Fatalf("NULL 限额应读成不限：%+v", got)
	}
	// CHECK 与主键在 PG 侧同样生效。
	if _, err := s.db.ExecContext(ctx,
		s.bind(`INSERT INTO user_quotas (user_slug, quota_total, external_used, updated_at) VALUES (?, ?, 0, ?)`),
		"user-bad", -5, s.nsArg(T0.UnixNano())); err == nil {
		t.Fatal("PG 侧负限额必须被 CHECK 拦住（第三方写入也不放行）")
	}
}
