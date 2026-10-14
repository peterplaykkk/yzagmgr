// UC-048（上限/维护）与 UC-049（系统信息）的 HTTP 面契约：
// 冻结形状真的出现在响应 JSON 里、PUT 校验不落库、PUT 触发维护、system/info 只读。
//
// 为什么在 API 层再钉一遍：服务包层的断言只看得到 DTO 值；「字段有没有被序列化出去」
// 与「端点有没有真触发维护」只有穿过信封与真实存根才能确认。
package api

import (
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/peterplaykkk/yzagmgr/internal/usage"
)

// seedBulkUsage 经真实写路径播 n 条台账（at 按秒递增），让隔离库真实超限。
func seedBulkUsage(t *testing.T, srv *contractServer, n int) {
	t.Helper()
	base := time.Now().Add(-time.Hour)
	for i := 0; i < n; i++ {
		seedAdditiveRecord(t, srv, "claude", "yzagmgr-local", "m1", 200, 10, 100, 50, base.Add(time.Duration(i)*time.Second))
	}
}

// usageTableRows 直查某表的行数（经服务包读数，独立于 HTTP 面）。
func usageTableRows(t *testing.T, srv *contractServer, table string) int64 {
	t.Helper()
	tables, err := srv.Usage.TableRows(t.Context())
	if err != nil {
		t.Fatalf("TableRows 失败: %v", err)
	}
	for _, tc := range tables {
		if tc.Name == table {
			return tc.Rows
		}
	}
	t.Fatalf("表 %s 不在读数清单里：%+v", table, tables)
	return 0
}

// TestUsageLimitsAPI 覆盖 UC-048 A2/A5 的 HTTP 面：
// 默认值、PUT 生效并触发维护、非法值 400 且不落库、回读一致。
func TestUsageLimitsAPI(t *testing.T) {
	srv := newContractServer(t)

	t.Run("默认值：20 MiB、适用、无清理记录", func(t *testing.T) {
		res := srv.callRaw(t, http.MethodGet, "/api/v1/usage/limits", "", true)
		if !res.ok {
			t.Fatalf("GET /usage/limits 失败: %s", res.body)
		}
		requireRawKeys(t, res.data, "maxBytes", "applicable", "dbBytes", "usedPct")
		var limits usage.Limits
		if err := json.Unmarshal(res.data, &limits); err != nil {
			t.Fatalf("limits 不是预期结构: %v（%s）", err, res.data)
		}
		if limits.MaxBytes != usage.DefaultMaxBytes || !limits.Applicable {
			t.Fatalf("默认限额不符：%+v", limits)
		}
		if limits.LastRunAt != "" || limits.LastDeletedRows != nil {
			t.Fatalf("从未清理过时不得出现清理记录：%+v", limits)
		}
	})

	// 播足量台账让库**内容**真实超限（~5200 条 ≈ 1.4 MB > 1 MiB 上限；
	// 判据是内容而不是 WAL 帧数——WAL 里的重复帧会被维护先收掉，不算超限）。
	seedBulkUsage(t, srv, 5200)
	rowsBefore := usageTableRows(t, srv, "request_logs")

	t.Run("PUT 生效并立即触发维护（超限 ⇒ 真有删除）", func(t *testing.T) {
		res := srv.callRaw(t, http.MethodPut, "/api/v1/usage/limits", `{"maxBytes":1048576}`, true)
		if !res.ok {
			t.Fatalf("PUT /usage/limits 失败: %s", res.body)
		}
		var limits usage.Limits
		if err := json.Unmarshal(res.data, &limits); err != nil {
			t.Fatalf("PUT 回读不是 limits 形状: %v（%s）", err, res.data)
		}
		if limits.MaxBytes != 1048576 {
			t.Fatalf("PUT 后 maxBytes = %d，期望 1048576", limits.MaxBytes)
		}
		if limits.LastRunAt == "" || limits.LastDeletedRows == nil || *limits.LastDeletedRows <= 0 {
			t.Fatalf("保存后必须触发一次真实清理：%+v", limits)
		}
		if limits.LastVacuumed == nil {
			t.Fatalf("压实与否必须如实回读：%+v", limits)
		}
		if limits.DBBytes <= 0 || limits.UsedPct < 0 {
			t.Fatalf("尺寸与占用必须真实：%+v", limits)
		}
		if got := usageTableRows(t, srv, "request_logs"); got >= rowsBefore {
			t.Fatalf("清理应真的删掉旧行：%d → %d", rowsBefore, got)
		}
		if got := usageTableRows(t, srv, "pricing"); got != 0 {
			t.Fatalf("pricing 不得被清理碰过：%d 行", got)
		}
	})

	t.Run("GET 回读一致（限额随库走）", func(t *testing.T) {
		res := srv.callRaw(t, http.MethodGet, "/api/v1/usage/limits", "", true)
		if !res.ok {
			t.Fatalf("GET 失败: %s", res.body)
		}
		var limits usage.Limits
		if err := json.Unmarshal(res.data, &limits); err != nil {
			t.Fatalf("解析失败: %v", err)
		}
		if limits.MaxBytes != 1048576 || limits.LastRunAt == "" {
			t.Fatalf("回读应保持 PUT 的值与清理记录：%+v", limits)
		}
	})

	t.Run("非法值一律 400 且不落库", func(t *testing.T) {
		for _, tc := range []struct {
			name string
			body string
		}{
			{"缺字段", `{}`},
			{"零", `{"maxBytes":0}`},
			{"负数", `{"maxBytes":-1}`},
			{"低于下限", `{"maxBytes":1048575}`},
			{"高于上限", `{"maxBytes":10737418241}`},
			{"类型错", `{"maxBytes":"1MiB"}`},
		} {
			t.Run(tc.name, func(t *testing.T) {
				res := srv.callRaw(t, http.MethodPut, "/api/v1/usage/limits", tc.body, true)
				res.assertEnvelope(t, http.MethodPut, "/api/v1/usage/limits")
				if res.ok {
					t.Fatalf("非法值必须 400：%s", res.body)
				}
			})
		}
		// 落库未被污染。
		res := srv.callRaw(t, http.MethodGet, "/api/v1/usage/limits", "", true)
		var limits usage.Limits
		if err := json.Unmarshal(res.data, &limits); err != nil {
			t.Fatalf("解析失败: %v", err)
		}
		if limits.MaxBytes != 1048576 {
			t.Fatalf("非法 PUT 不得改到落库值：%+v", limits)
		}
	})

	t.Run("手动维护返回冻结形状（未超限时 0 删除）", func(t *testing.T) {
		res := srv.callRaw(t, http.MethodPost, "/api/v1/usage/maintenance", "", true)
		if !res.ok {
			t.Fatalf("POST /usage/maintenance 失败: %s", res.body)
		}
		requireRawKeys(t, res.data, "dbBytes", "deletedRows", "vacuumed", "lastRunAt")
		var result usage.MaintainResult
		if err := json.Unmarshal(res.data, &result); err != nil {
			t.Fatalf("维护结果不是预期结构: %v（%s）", err, res.data)
		}
		if result.DeletedRows != 0 || result.Vacuumed {
			t.Fatalf("刚清理过、仍在水位线下 ⇒ 空维护：%+v", result)
		}
		if result.LastRunAt == "" {
			t.Fatalf("维护响应必须带本次运行时刻：%+v", result)
		}
	})
}

// TestSystemInfoAPI 覆盖 UC-049 A3/A4 的数据面：
// 三块内容在场、数值与真值一致、诚实态（null）在场、只读零副作用。
func TestSystemInfoAPI(t *testing.T) {
	srv := newContractServer(t)
	base := time.Now().Add(-30 * time.Minute).Truncate(time.Second)
	seedAdditiveRecord(t, srv, "claude", "yzagmgr-local", "m1", 200, 120, 100, 50, base)
	seedAdditiveRecord(t, srv, "codex", "yzagmgr-local", "m2", 500, 0, 10, 0, base.Add(10*time.Minute))
	seedAdditiveRecord(t, srv, "claude", "yzagmgr-local", "m1", 200, 300, 1, 1, base.Add(20*time.Minute))

	// 让 queryStats 的 summary 有样本（另两个聚合操作保持无样本 ⇒ null）。
	if res := srv.callRaw(t, http.MethodGet, "/api/v1/usage/summary?"+usageWindowQuery(nil), "", true); !res.ok {
		t.Fatalf("预热 summary 失败: %s", res.body)
	}

	rowsBefore := usageTableRows(t, srv, "request_logs")
	metaBefore := usageTableRows(t, srv, "meta")

	res := srv.callRaw(t, http.MethodGet, "/api/v1/system/info", "", true)
	if !res.ok {
		t.Fatalf("GET /system/info 失败: %s", res.body)
	}
	raw := requireRawKeys(t, res.data, "storage", "tables", "records", "queryStats", "pragma")

	var info systemInfo
	if err := json.Unmarshal(res.data, &info); err != nil {
		t.Fatalf("system/info 不是预期结构: %v（%s）", err, res.data)
	}

	t.Run("数据占用块", func(t *testing.T) {
		if info.Storage.DataRootBytes <= 0 {
			t.Fatalf("数据根占用应为正：%+v", info.Storage)
		}
		if info.Storage.UsageDBBytes != info.Storage.DBBytes+info.Storage.WALBytes {
			t.Fatalf("usageDbBytes 必须 = dbBytes + walBytes：%+v", info.Storage)
		}
		if info.Storage.UsageDBBytes <= 0 {
			t.Fatalf("用量库占用应为正：%+v", info.Storage)
		}
		if info.Storage.Limits.MaxBytes != usage.DefaultMaxBytes || !info.Storage.Limits.Applicable {
			t.Fatalf("限额状态不符：%+v", info.Storage.Limits)
		}
		if info.Storage.Limits.UsedPct <= 0 || info.Storage.Limits.UsedPct >= 100 {
			t.Fatalf("usedPct 应为一个小的正数：%+v", info.Storage.Limits)
		}
		if info.Storage.BackupsBytes < 0 {
			t.Fatalf("备份目录占用不得为负：%+v", info.Storage)
		}
	})

	t.Run("数据表行数与记录跨度逐值对照", func(t *testing.T) {
		want := []string{"request_logs", "pricing", "user_quotas", "meta"}
		if len(info.Tables) != len(want) {
			t.Fatalf("表清单 = %+v，期望 %v", info.Tables, want)
		}
		for i, tc := range info.Tables {
			if tc.Name != want[i] {
				t.Fatalf("第 %d 张表 = %q，期望 %q", i, tc.Name, want[i])
			}
		}
		if info.Tables[0].Rows != 3 {
			t.Fatalf("request_logs 行数 = %d，期望 3", info.Tables[0].Rows)
		}
		if info.Records.OldestAt == nil || info.Records.NewestAt == nil {
			t.Fatalf("有记录时跨度不得为 null：%+v", info.Records)
		}
		if got := *info.Records.OldestAt; got != base.Format(time.RFC3339) {
			t.Fatalf("最旧 at = %s，期望 %s", got, base.Format(time.RFC3339))
		}
		if got := *info.Records.NewestAt; got != base.Add(20*time.Minute).Format(time.RFC3339) {
			t.Fatalf("最新 at = %s，期望 %s", got, base.Add(20*time.Minute).Format(time.RFC3339))
		}
	})

	t.Run("查询延时块：有样本的 op 有值、无样本的 op 是 null", func(t *testing.T) {
		var qs map[string]json.RawMessage
		if err := json.Unmarshal(raw["queryStats"], &qs); err != nil {
			t.Fatalf("queryStats 不是对象: %v", err)
		}
		for _, op := range []string{"summary", "trends", "breakdown", "requests", "detail", "pricing", "maintenance"} {
			if _, ok := qs[op]; !ok {
				t.Fatalf("queryStats 缺少 op %q：%s", op, raw["queryStats"])
			}
		}
		if string(qs["summary"]) == "null" {
			t.Fatalf("summary 调过，必须有样本：%s", raw["queryStats"])
		}
		if string(qs["trends"]) != "null" || string(qs["maintenance"]) != "null" {
			t.Fatalf("未调过的 op 必须 null（不得用 0 冒充）：%s", raw["queryStats"])
		}
		if info.QueryStats.Summary == nil || info.QueryStats.Summary.Count != 1 {
			t.Fatalf("summary 样本不符：%+v", info.QueryStats.Summary)
		}
		if info.QueryStats.Summary.LastMs < 0 || info.QueryStats.Summary.AvgMs < 0 || info.QueryStats.Summary.P95Ms < 0 {
			t.Fatalf("耗时不得为负：%+v", info.QueryStats.Summary)
		}
	})

	t.Run("PRAGMA 与诚实态说明", func(t *testing.T) {
		if info.Pragma == nil {
			t.Fatal("SQLite 后端必须给出 pragma 块")
		}
		if info.Pragma.JournalMode != "wal" || info.Pragma.PageSize <= 0 || info.Pragma.PageCount <= 0 {
			t.Fatalf("pragma 取值不符：%+v", info.Pragma)
		}
		if info.Notes == "" {
			t.Fatal("notes 必须说明埋点为进程内样本（重启归零）")
		}
	})

	t.Run("只读：不触发维护、不写任何行", func(t *testing.T) {
		if got := usageTableRows(t, srv, "request_logs"); got != rowsBefore {
			t.Fatalf("system/info 不得删行：%d → %d", rowsBefore, got)
		}
		if got := usageTableRows(t, srv, "meta"); got != metaBefore {
			t.Fatalf("system/info 不得写库：meta %d → %d", metaBefore, got)
		}
		limits := srv.callRaw(t, http.MethodGet, "/api/v1/usage/limits", "", true)
		var l usage.Limits
		if err := json.Unmarshal(limits.data, &l); err != nil {
			t.Fatalf("解析失败: %v", err)
		}
		if l.LastRunAt != "" {
			t.Fatalf("system/info 不得留下清理记录：%+v", l)
		}
	})
}

// TestUsageLimitsRequireToken 守住新端点不落在免鉴权白名单外（UC-006 A8 的 additive 检查）。
func TestUsageLimitsRequireToken(t *testing.T) {
	srv := newContractServer(t)
	for _, ep := range []struct {
		method, path, body string
	}{
		{http.MethodGet, "/api/v1/usage/limits", ""},
		{http.MethodPut, "/api/v1/usage/limits", `{"maxBytes":1048576}`},
		{http.MethodPost, "/api/v1/usage/maintenance", ""},
		{http.MethodGet, "/api/v1/system/info", ""},
	} {
		res := srv.callRaw(t, ep.method, ep.path, ep.body, false)
		if res.status != http.StatusUnauthorized {
			t.Fatalf("%s %s 缺令牌必须 401，实际 %d（%s）", ep.method, ep.path, res.status, res.body)
		}
	}
}
