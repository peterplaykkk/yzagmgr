package usage

import (
	"context"
	"testing"
	"time"
)

// TestRecordRuleVersionRoundTrip 钉 v7（UC-096 A6）的 rule_version 列：
// 写入与明细回读逐字一致；空串是合法值（不适用/未知，不伪填）；非空时形状受校验
// （指纹是我们的机器码，不接受自由文本——防「什么字符串都能塞」的通道）。
func TestRecordRuleVersionRoundTrip(t *testing.T) {
	path := t.TempDir() + "/usage.db"
	s := newStoreAt(t, path, Options{Location: testLoc, Clock: (&testClock{now: T0}).Now})
	ctx := context.Background()

	mustRecord(t, s,
		rec("rq_rv1", 0, func(r *Record) { r.RuleVersion = "rv1-0123456789abcdef" }),
		rec("rq_rv0", time.Minute, func(r *Record) { r.RuleVersion = "" }),
	)
	got, err := s.RequestDetail(ctx, "rq_rv1")
	if err != nil {
		t.Fatalf("RequestDetail 失败: %v", err)
	}
	if got.RuleVersion != "rv1-0123456789abcdef" {
		t.Fatalf("规则版本回读 = %q", got.RuleVersion)
	}
	empty, err := s.RequestDetail(ctx, "rq_rv0")
	if err != nil {
		t.Fatalf("RequestDetail 失败: %v", err)
	}
	if empty.RuleVersion != "" {
		t.Fatalf("未携带的规则版本应为空串（不伪填），实际 %q", empty.RuleVersion)
	}
	// 列表同样携带（列表与明细同一 DTO）。
	page, err := s.Requests(ctx, RequestsQuery{})
	if err != nil {
		t.Fatalf("Requests 失败: %v", err)
	}
	found := false
	for _, it := range page.Items {
		if it.ID == "rq_rv1" {
			found = true
			if it.RuleVersion != "rv1-0123456789abcdef" {
				t.Errorf("列表规则版本 = %q", it.RuleVersion)
			}
		}
	}
	if !found {
		t.Fatalf("列表里找不到 rq_rv1")
	}

	// 形状非法即拒绝（自由文本 / 长串进不来）。
	for _, bad := range []string{"自由文本", "rv1-zzzz", "0123456789abcdef", "rv1-0123"} {
		if _, err := s.Record(ctx, Record{App: "claude", Prefix: "claude", Provider: "legacy", RuleVersion: bad}); err == nil {
			t.Errorf("ruleVersion %q 应被拒绝", bad)
		}
	}
}
