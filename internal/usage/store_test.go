package usage

import (
	"bytes"
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/peterplaykkk/yzagmgr/internal/apierr"
	"github.com/peterplaykkk/yzagmgr/internal/config"
)

// TestUsageDBLivesInDataRootWithPrivateMode 对应 UC-008 A10：
// 用量库落在 v2 数据根下，且库文件自身是 0600（不靠进程 umask 兜底）。
func TestUsageDBLivesInDataRootWithPrivateMode(t *testing.T) {
	home := t.TempDir()
	env := &config.Env{Home: home, Overrides: map[string]string{}, Isolated: true}
	if err := env.EnsureLayout(); err != nil {
		t.Fatalf("EnsureLayout 失败: %v", err)
	}

	path := env.UsageDBFile()
	if got, want := filepath.Dir(path), env.DataRoot(); got != want {
		t.Fatalf("用量库目录应为数据根 %s，实际 %s", want, got)
	}
	if !strings.HasPrefix(path, home) {
		t.Fatalf("用量库必须落在隔离根内，实际 %s（根 %s）", path, home)
	}

	s := newStoreAt(t, path, Options{Location: testLoc})
	mustRecord(t, s, rec("rq_perm01", 0))

	if st, err := os.Stat(path); err != nil {
		t.Fatalf("统计库文件失败: %v", err)
	} else if perm := st.Mode().Perm(); perm != 0o600 {
		t.Fatalf("库文件权限应为 0600，实际 %04o", perm)
	}
	if st, err := os.Stat(env.DataRoot()); err != nil {
		t.Fatalf("统计数据根失败: %v", err)
	} else if perm := st.Mode().Perm(); perm != 0o700 {
		t.Fatalf("数据根权限应为 0700，实际 %04o", perm)
	}
	// WAL 侧车文件若存在，同样不得放宽到组/其他用户可读。
	for _, suffix := range []string{"-wal", "-shm"} {
		st, err := os.Stat(path + suffix)
		if err != nil {
			continue
		}
		if perm := st.Mode().Perm(); perm != 0o600 {
			t.Fatalf("%s 权限应为 0600，实际 %04o", path+suffix, perm)
		}
	}
}

// TestCorruptUsageDBIsReportedNotCrashed 对应 UC-008 A11：
// 各种「不像本产品建的库」都必须在启动时判成 corrupt（不崩、不静默重建），
// 且都能经 Rebuild 恢复：原文件被隔离保留、新库为空。
func TestCorruptUsageDBIsReportedNotCrashed(t *testing.T) {
	const (
		foreignDB = `CREATE TABLE unrelated_notes (note TEXT)`
		partialDB = `CREATE TABLE request_logs (id TEXT PRIMARY KEY, at INTEGER NOT NULL)`
	)

	cases := []struct {
		name string
		// prepare 造出用例的初始盘上状态；返回的检查函数在 Rebuild 之后验证「历史未被销毁」。
		prepare func(t *testing.T, path string) func(t *testing.T, quarantined string)
		corrupt bool
		// detailHas 是空列表以外的任一子串出现在 Health().Detail 里即通过（原因文本允许随驱动措辞变化）。
		detailHas []string
	}{
		{
			name: "非 SQLite 文件",
			prepare: func(t *testing.T, path string) func(*testing.T, string) {
				t.Helper()
				data := []byte(strings.Repeat("这不是数据库-", 300))
				if err := os.WriteFile(path, data, 0o600); err != nil {
					t.Fatalf("写入垃圾字节失败: %v", err)
				}
				return func(t *testing.T, quarantined string) {
					t.Helper()
					got := readFile(t, quarantined)
					if len(got) != len(data) || string(got) != string(data) {
						t.Fatalf("垃圾文件应原样保留（%d 字节），实际 %d 字节", len(data), len(got))
					}
				}
			},
			corrupt:   true,
			detailHas: []string{"not a database", "疑似损坏"},
		},
		{
			name: "外来 schema 且无版本标记",
			prepare: func(t *testing.T, path string) func(*testing.T, string) {
				t.Helper()
				prepareRawDB(t, path, foreignDB)
				return func(t *testing.T, quarantined string) {
					t.Helper()
					requireTableStillThere(t, quarantined, "unrelated_notes")
				}
			},
			corrupt:   true,
			detailHas: []string{"无版本标记却已有"},
		},
		{
			name: "表结构缺列",
			prepare: func(t *testing.T, path string) func(*testing.T, string) {
				t.Helper()
				prepareRawDB(t, path, partialDB)
				setRawUserVersion(t, path, SchemaVersion)
				return func(t *testing.T, quarantined string) {
					t.Helper()
					requireTableStillThere(t, quarantined, "request_logs")
				}
			},
			corrupt:   true,
			detailHas: []string{"缺少列"},
		},
		{
			name: "schema 版本高于本程序",
			prepare: func(t *testing.T, path string) func(*testing.T, string) {
				t.Helper()
				prepareRawDB(t, path, foreignDB)
				setRawUserVersion(t, path, SchemaVersion+1)
				return func(t *testing.T, quarantined string) {
					t.Helper()
					requireTableStillThere(t, quarantined, "unrelated_notes")
				}
			},
			corrupt:   true,
			detailHas: []string{"高于本程序支持"},
		},
		{
			name: "库路径是目录",
			prepare: func(t *testing.T, path string) func(*testing.T, string) {
				t.Helper()
				if err := os.MkdirAll(path, 0o700); err != nil {
					t.Fatalf("建目录失败: %v", err)
				}
				return func(t *testing.T, quarantined string) {
					t.Helper()
					st, err := os.Stat(quarantined)
					if err != nil {
						t.Fatalf("隔离路径应存在: %v", err)
					}
					if !st.IsDir() {
						t.Fatalf("隔离路径应仍是原目录，实际 %v", st.Mode())
					}
				}
			},
			corrupt:   true,
			detailHas: []string{"创建用量库文件"},
		},
		{
			name: "页内容损坏（快速一致性检查能发现）",
			prepare: func(t *testing.T, path string) func(*testing.T, string) {
				t.Helper()
				// 先建一个「看起来正常」的库并写点数据，再把数据页抹掉。
				tmp := newStoreAt(t, path, Options{Location: testLoc})
				recs := make([]Record, 0, 200)
				for i := 0; i < 200; i++ {
					recs = append(recs, rec("rq_corrupt"+itoaPad(i), time.Duration(i)*time.Second))
				}
				mustRecord(t, tmp, recs...)
				if err := tmp.Close(); err != nil { // 关掉才会把 WAL 落回主库
					t.Fatalf("关闭库失败: %v", err)
				}
				f, err := os.OpenFile(path, os.O_RDWR, 0o600)
				if err != nil {
					t.Fatalf("打开库文件失败: %v", err)
				}
				defer func() { _ = f.Close() }()
				garbage := bytes.Repeat([]byte{0xff}, 4096)
				if _, err := f.WriteAt(garbage, 4096); err != nil {
					t.Fatalf("写坏数据页失败: %v", err)
				}
				return func(t *testing.T, quarantined string) {
					t.Helper()
					st, err := os.Stat(quarantined)
					if err != nil {
						t.Fatalf("隔离文件应存在: %v", err)
					}
					if st.Size() == 0 {
						t.Fatal("隔离文件不应为空")
					}
				}
			},
			corrupt:   true,
			detailHas: []string{"一致性检查"},
		},
		{
			name: "空文件视为全新库",
			prepare: func(t *testing.T, path string) func(*testing.T, string) {
				t.Helper()
				if err := os.WriteFile(path, nil, 0o600); err != nil {
					t.Fatalf("建空文件失败: %v", err)
				}
				return nil
			},
			corrupt: false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "usage.db")
			verify := tc.prepare(t, path)

			s, err := Open(context.Background(), path, Options{Location: testLoc})
			// 无论损坏与否都不得返回 nil Store：守护进程必须能继续启动（UC-008 A11）。
			if s == nil {
				t.Fatal("Open 必须返回非 nil Store（库损坏也不得让启动失败）")
			}
			t.Cleanup(func() { _ = s.Close() })

			health := s.Health()
			if !tc.corrupt {
				if err != nil {
					t.Fatalf("全新库不应报错，实际: %v", err)
				}
				if health.State != StateOK {
					t.Fatalf("状态应为 %s，实际 %s（%s）", StateOK, health.State, health.Detail)
				}
				mustRecord(t, s, rec("rq_fresh01", 0))
				if got := countRecords(t, s); got != 1 {
					t.Fatalf("全新库应能记账，实际条数 %d", got)
				}
				return
			}

			if err == nil {
				t.Fatal("损坏库必须给出错误（用于日志），不得静默通过")
			}
			if health.State != StateCorrupt {
				t.Fatalf("状态应为 %s，实际 %s", StateCorrupt, health.State)
			}
			if health.Detail == "" {
				t.Fatal("损坏原因必须写进 detail（界面要能明示）")
			}
			if !containsAny(health.Detail, tc.detailHas) {
				t.Fatalf("detail 应包含 %v 之一，实际 %q", tc.detailHas, health.Detail)
			}

			// 损坏期间：读写都必须报可判别的 storage 错误，而不是 panic 或空结果。
			_, err = s.Summary(context.Background(), SummaryQuery{Window: Window{From: T0, To: T0.Add(time.Hour)}})
			requireCode(t, "损坏库 Summary", err, apierr.CodeStorage)
			_, err = s.Record(context.Background(), rec("rq_corrupt01", 0))
			requireCode(t, "损坏库 Record", err, apierr.CodeStorage)

			// 重建：必须显式告知历史会丢，并把原文件整体隔离保留。
			result, err := s.Rebuild(context.Background())
			if err != nil {
				t.Fatalf("Rebuild 失败: %v", err)
			}
			if !result.HistoryDiscarded {
				t.Fatal("Rebuild 必须显式标记历史已丢")
			}
			if !strings.Contains(result.Message, "历史") || !strings.Contains(result.Message, "不会自动恢复") {
				t.Fatalf("Rebuild 的告知文本不足：%q", result.Message)
			}
			if result.QuarantinedPath == "" {
				t.Fatal("Rebuild 必须给出隔离文件路径")
			}
			if verify != nil {
				verify(t, result.QuarantinedPath)
			}
			if health := s.Health(); health.State != StateOK {
				t.Fatalf("重建后状态应为 %s，实际 %s（%s）", StateOK, health.State, health.Detail)
			}
			mustRecord(t, s, rec("rq_rebuilt01", 0))
			if got := countRecords(t, s); got != 1 {
				t.Fatalf("重建后应是空库并接受新记录，实际条数 %d", got)
			}
		})
	}

	// /health 的取值是契约的一部分（契约 §2），逐字钉住。
	for _, c := range []struct {
		got  State
		want string
	}{
		{StateOK, "ok"},
		{StateCorrupt, "corrupt"},
		{StateRebuilding, "rebuilding"},
	} {
		if string(c.got) != c.want {
			t.Fatalf("health 状态取值漂移：got %q want %q", c.got, c.want)
		}
	}
}

// TestRebuildQuarantinesHistoryAndStartsEmpty 覆盖「健康库重建」这条路径：
// 历史整体移出主库（仍可用 sqlite 打开取回），新库为空。
func TestRebuildQuarantinesHistoryAndStartsEmpty(t *testing.T) {
	path := filepath.Join(t.TempDir(), "usage.db")
	s := newStoreAt(t, path, Options{Location: testLoc})
	mustRecord(t, s, rec("rq_q1", 0), rec("rq_q2", time.Minute), rec("rq_q3", 2*time.Minute))
	if got := countRecords(t, s); got != 3 {
		t.Fatalf("重建前应有 3 条，实际 %d", got)
	}

	result, err := s.Rebuild(context.Background())
	if err != nil {
		t.Fatalf("Rebuild 失败: %v", err)
	}
	if got := countRecords(t, s); got != 0 {
		t.Fatalf("重建后主库应为空，实际 %d", got)
	}
	// 隔离文件里的历史仍可被手工取回（不是删除）。
	db, err := sql.Open(driverName, result.QuarantinedPath)
	if err != nil {
		t.Fatalf("打开隔离文件失败: %v", err)
	}
	defer func() { _ = db.Close() }()
	var n int64
	if err := db.QueryRow(`SELECT COUNT(*) FROM request_logs`).Scan(&n); err != nil {
		t.Fatalf("隔离文件读取失败: %v", err)
	}
	if n != 3 {
		t.Fatalf("隔离文件应保留 3 条历史，实际 %d", n)
	}
}

// TestUsageStoreReopenKeepsRecords 覆盖「关掉再打开」：记账是落盘的，不是内存态。
func TestUsageStoreReopenKeepsRecords(t *testing.T) {
	path := filepath.Join(t.TempDir(), "usage.db")
	s := newStoreAt(t, path, Options{Location: testLoc})
	mustRecord(t, s, rec("rq_r1", 0), rec("rq_r2", time.Minute))
	if err := s.Close(); err != nil {
		t.Fatalf("Close 失败: %v", err)
	}

	again := newStoreAt(t, path, Options{Location: testLoc})
	if got := countRecords(t, again); got != 2 {
		t.Fatalf("重开后应有 2 条，实际 %d", got)
	}
	sum, err := again.Summary(context.Background(), SummaryQuery{Window: Window{From: T0.Add(-time.Hour), To: T0.Add(time.Hour)}})
	if err != nil {
		t.Fatalf("重开后 Summary 失败: %v", err)
	}
	if sum.Requests != 2 || sum.InputTokens != 200 || sum.OutputTokens != 100 {
		t.Fatalf("重开后汇总不符：%+v", sum)
	}
}

// TestUsageStoreUsesWALAndBusyTimeout 钉住并发纪律本身：
// WAL + busy_timeout 是「并发写不丢不重」的前提，不能被后续改动悄悄去掉。
func TestUsageStoreUsesWALAndBusyTimeout(t *testing.T) {
	s := newStore(t)

	var mode string
	if err := s.db.QueryRowContext(context.Background(), `PRAGMA journal_mode`).Scan(&mode); err != nil {
		t.Fatalf("读取 journal_mode 失败: %v", err)
	}
	if !strings.EqualFold(mode, "wal") {
		t.Fatalf("应使用 WAL，实际 %q", mode)
	}
	var timeout int
	if err := s.db.QueryRowContext(context.Background(), `PRAGMA busy_timeout`).Scan(&timeout); err != nil {
		t.Fatalf("读取 busy_timeout 失败: %v", err)
	}
	if timeout != 5000 {
		t.Fatalf("busy_timeout 应为 5000ms，实际 %d", timeout)
	}
}

// TestConcurrentStoresShareOneLedgerNoBusy 覆盖跨句柄（两条连接）并发写：
// 单进程单连接之外，本机 CLI/第二次打开也必须靠 busy_timeout 与 WAL 正常落账。
func TestConcurrentStoresShareOneLedgerNoBusy(t *testing.T) {
	path := filepath.Join(t.TempDir(), "usage.db")
	s1 := newStoreAt(t, path, Options{Location: testLoc})
	s2 := newStoreAt(t, path, Options{Location: testLoc})

	const perStore = 24
	var wg sync.WaitGroup
	errs := make(chan error, 2*perStore)
	for i := 0; i < perStore; i++ {
		for si, s := range []*Store{s1, s2} {
			wg.Add(1)
			go func(i, si int, s *Store) {
				defer wg.Done()
				id := "rq_x" + string(rune('a'+si)) + itoaPad(i)
				if _, err := s.Record(context.Background(), rec(id, time.Duration(i)*time.Second)); err != nil {
					errs <- err
				}
			}(i, si, s)
		}
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatalf("跨句柄并发写失败（不应出现 busy 类错误）: %v", err)
	}
	if got := countRecords(t, s1); got != 2*perStore {
		t.Fatalf("应有 %d 条，实际 %d", 2*perStore, got)
	}
}

// TestUsageQueriesRejectInvalidParams 是入参校验的表驱动用例：
// 非法参数必须给 invalid_request，不得静默当成默认值。
func TestUsageQueriesRejectInvalidParams(t *testing.T) {
	inWindow := Window{From: T0, To: T0.Add(time.Hour)}
	cases := []struct {
		name string
		run  func(s *Store) error
		code apierr.Code
	}{
		{"summary 缺 from/to", func(s *Store) error {
			_, err := s.Summary(context.Background(), SummaryQuery{Window: Window{To: T0}})
			return err
		}, apierr.CodeInvalidRequest},
		{"summary 时窗倒挂", func(s *Store) error {
			_, err := s.Summary(context.Background(), SummaryQuery{Window: Window{From: T0, To: T0.Add(-time.Second)}})
			return err
		}, apierr.CodeInvalidRequest},
		{"summary 时窗超出可表示范围", func(s *Store) error {
			_, err := s.Summary(context.Background(), SummaryQuery{
				Window: Window{From: T0, To: time.Date(9999, 1, 1, 0, 0, 0, 0, time.UTC)},
			})
			return err
		}, apierr.CodeInvalidRequest},
		{"requests 时窗早于可表示范围", func(s *Store) error {
			_, err := s.Requests(context.Background(), RequestsQuery{
				Window: Window{From: time.Date(1600, 1, 1, 0, 0, 0, 0, time.UTC)},
			})
			return err
		}, apierr.CodeInvalidRequest},
		{"summary app 形状非法", func(s *Store) error {
			_, err := s.Summary(context.Background(), SummaryQuery{Window: inWindow, App: "Claude Desktop"})
			return err
		}, apierr.CodeInvalidRequest},
		{"trends 未知 bucket", func(s *Store) error {
			_, err := s.Trends(context.Background(), TrendsQuery{Window: inWindow, Bucket: "week", By: ByAll})
			return err
		}, apierr.CodeInvalidRequest},
		{"trends 未知 by", func(s *Store) error {
			_, err := s.Trends(context.Background(), TrendsQuery{Window: inWindow, Bucket: BucketHour, By: "prefix"})
			return err
		}, apierr.CodeInvalidRequest},
		{"trends 桶数超上限", func(s *Store) error {
			_, err := s.Trends(context.Background(), TrendsQuery{
				Window: Window{From: T0, To: T0.Add(10 * 365 * 24 * time.Hour)},
				Bucket: BucketHour, By: ByAll,
			})
			return err
		}, apierr.CodeInvalidRequest},
		{"breakdown 拒绝 all", func(s *Store) error {
			_, err := s.Breakdown(context.Background(), BreakdownQuery{Window: inWindow, By: ByAll})
			return err
		}, apierr.CodeInvalidRequest},
		{"requests limit 越界", func(s *Store) error {
			_, err := s.Requests(context.Background(), RequestsQuery{Window: inWindow, Limit: MaxLimit + 1})
			return err
		}, apierr.CodeInvalidRequest},
		{"requests order 非法", func(s *Store) error {
			_, err := s.Requests(context.Background(), RequestsQuery{Window: inWindow, Order: "newest"})
			return err
		}, apierr.CodeInvalidRequest},
		{"requests cursor 非法", func(s *Store) error {
			_, err := s.Requests(context.Background(), RequestsQuery{Window: inWindow, Cursor: "not-a-cursor"})
			return err
		}, apierr.CodeInvalidRequest},
		{"requests model 形状非法", func(s *Store) error {
			_, err := s.Requests(context.Background(), RequestsQuery{Window: inWindow, Model: "deepseek flash\n"})
			return err
		}, apierr.CodeInvalidRequest},
		{"requests status 越界", func(s *Store) error {
			bad := 900
			_, err := s.Requests(context.Background(), RequestsQuery{Window: inWindow, Status: &bad})
			return err
		}, apierr.CodeInvalidRequest},
		{"requests 时窗倒挂", func(s *Store) error {
			_, err := s.Requests(context.Background(), RequestsQuery{Window: Window{From: T0, To: T0.Add(-time.Minute)}})
			return err
		}, apierr.CodeInvalidRequest},
		{"detail 未知 id", func(s *Store) error {
			_, err := s.RequestDetail(context.Background(), "rq_nothere")
			return err
		}, apierr.CodeNotFound},
		{"detail id 形状非法", func(s *Store) error {
			_, err := s.RequestDetail(context.Background(), "../../etc/passwd")
			return err
		}, apierr.CodeNotFound},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := newStore(t)
			requireCode(t, tc.name, tc.run(s), tc.code)
		})
	}
}

// TestUsageEmptyWindowReturnsZeroShape 覆盖「时间窗无数据返回零值结构」：
// 零值不等于零长度数组，也不等于「未知金额」——空窗口是「花了 0」。
func TestUsageEmptyWindowReturnsZeroShape(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	empty := Window{From: T0, To: T0.Add(3 * time.Hour)}

	sum, err := s.Summary(ctx, SummaryQuery{Window: empty})
	if err != nil {
		t.Fatalf("Summary 失败: %v", err)
	}
	if sum.Requests != 0 || sum.InputTokens != 0 || sum.OutputTokens != 0 {
		t.Fatalf("空窗口计数应为 0：%+v", sum)
	}
	if sum.Cost == nil || *sum.Cost != 0 {
		t.Fatalf("空窗口金额应是已知的 0（不是未知）：%s", fmtMoney(sum.Cost))
	}
	if sum.CostUnknown {
		t.Fatal("空窗口不应标记为金额未知")
	}
	requireRFC3339(t, "空窗口 summary.asOf", sum.AsOf)

	tr, err := s.Trends(ctx, TrendsQuery{Window: empty, Bucket: BucketHour, By: ByAll})
	if err != nil {
		t.Fatalf("Trends 失败: %v", err)
	}
	if tr.Bucket != BucketHour {
		t.Fatalf("bucket 应原样回显 hour，实际 %q", tr.Bucket)
	}
	requireRFC3339(t, "空窗口 trends.asOf", tr.AsOf)
	if len(tr.Series) != 1 || tr.Series[0].Key != string(ByAll) {
		t.Fatalf("by=all 应给一条序列（key=all）：%+v", tr.Series)
	}
	if len(tr.Series[0].Points) != 3 {
		t.Fatalf("3 小时窗口应有 3 个桶，实际 %d", len(tr.Series[0].Points))
	}
	for i, p := range tr.Series[0].Points {
		if p.Requests != 0 || p.Tokens != 0 {
			t.Fatalf("第 %d 个空桶应为零：%+v", i, p)
		}
	}

	bd, err := s.Breakdown(ctx, BreakdownQuery{Window: empty, By: ByApp})
	if err != nil {
		t.Fatalf("Breakdown 失败: %v", err)
	}
	if bd.Items == nil || len(bd.Items) != 0 {
		t.Fatalf("空窗口拆分应是空数组（非 nil），实际 %+v", bd.Items)
	}

	page, err := s.Requests(ctx, RequestsQuery{Window: empty})
	if err != nil {
		t.Fatalf("Requests 失败: %v", err)
	}
	if page.Items == nil || len(page.Items) != 0 {
		t.Fatalf("空窗口列表应是空数组（非 nil），实际 %+v", page.Items)
	}
	if page.Total != 0 || page.NextCursor != "" {
		t.Fatalf("空窗口列表 total=0 且无下一页，实际 total=%d cursor=%q", page.Total, page.NextCursor)
	}
}

// TestUsageClockRollbackKeepsRecordsVisible 覆盖「系统时间回拨」：
// 记录以事件时间排序，回拨产生的更早记录照常可见、可翻页、可查详情。
func TestUsageClockRollbackKeepsRecordsVisible(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	mustRecord(t, s,
		rec("rq_roll_new", 0),
		rec("rq_roll_old", -6*time.Hour), // 回拨后才写入的更早事件
		rec("rq_roll_mid", -2*time.Hour),
	)

	sum, err := s.Summary(ctx, SummaryQuery{Window: Window{From: T0.Add(-24 * time.Hour), To: T0.Add(time.Hour)}})
	if err != nil {
		t.Fatalf("Summary 失败: %v", err)
	}
	if sum.Requests != 3 {
		t.Fatalf("回拨记录不得消失，应有 3 条，实际 %d", sum.Requests)
	}

	var ids []string
	cursor := ""
	for {
		page, err := s.Requests(ctx, RequestsQuery{
			Window: Window{From: T0.Add(-24 * time.Hour), To: T0.Add(time.Hour)},
			Order:  OrderAsc, Limit: 1, Cursor: cursor,
		})
		if err != nil {
			t.Fatalf("Requests 失败: %v", err)
		}
		for _, it := range page.Items {
			ids = append(ids, it.ID)
		}
		if page.NextCursor == "" {
			break
		}
		cursor = page.NextCursor
	}
	want := []string{"rq_roll_old", "rq_roll_mid", "rq_roll_new"}
	if len(ids) != len(want) {
		t.Fatalf("升序翻页应得 %d 条，实际 %v", len(want), ids)
	}
	for i := range want {
		if ids[i] != want[i] {
			t.Fatalf("事件时间升序不符：got %v want %v", ids, want)
		}
	}
}

// TestSchemaBuildFailureIsDiagnosable 覆盖「建库失败」这条防御路径：
// 报告必须指出是哪一条语句失败，且失败不得留下半成品库（下一个进程还能正常建库）。
func TestSchemaBuildFailureIsDiagnosable(t *testing.T) {
	original := schemaStatements
	broken := "CREATE TABLE broken (\n\tx TEXT,\n)"
	schemaStatements = append(append([]string{}, original...), broken)
	restored := false
	restore := func() {
		if !restored {
			schemaStatements = original
			restored = true
		}
	}
	t.Cleanup(restore)

	path := filepath.Join(t.TempDir(), "usage.db")
	s, err := Open(context.Background(), path, Options{Location: testLoc})
	if s == nil {
		t.Fatal("Open 必须返回非 nil Store（建库失败也不例外）")
	}
	if err == nil {
		t.Fatal("建库语句非法时必须报错")
	}
	if got := s.Health().State; got != StateCorrupt {
		t.Fatalf("状态应为 %s，实际 %s", StateCorrupt, got)
	}
	detail := s.Health().Detail
	if !strings.Contains(detail, "CREATE TABLE broken (") {
		t.Fatalf("detail 应指出失败的语句，实际 %q", detail)
	}
	if strings.Contains(detail, "x TEXT") {
		t.Fatalf("detail 只应给出语句首行（避免把整段 DDL 灌进日志），实际 %q", detail)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("Close 失败: %v", err)
	}

	// 失败被事务回滚：库仍是「全新库」形态，下一次启动能正常建起来。
	restore()
	again := newStoreAt(t, path, Options{Location: testLoc})
	mustRecord(t, again, rec("rq_after_fail", 0))
	if got := countRecords(t, again); got != 1 {
		t.Fatalf("建库失败后应仍可从零开始，实际 %d 行", got)
	}
}

// ── 测试脚手架 ──────────────────────────────────────────────────────────────

func containsAny(s string, subs []string) bool {
	for _, sub := range subs {
		if strings.Contains(s, sub) {
			return true
		}
	}
	return false
}

// requireTableStillThere 断言隔离文件仍是可读的 SQLite 库且原表还在：
// 「历史可丢」指的是主库不再展示，不是把字节销毁。
func requireTableStillThere(t *testing.T, quarantined, table string) {
	t.Helper()
	db, err := sql.Open(driverName, quarantined)
	if err != nil {
		t.Fatalf("打开隔离文件失败: %v", err)
	}
	defer func() { _ = db.Close() }()
	var n int64
	if err := db.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = ?`, table).Scan(&n); err != nil {
		t.Fatalf("读取隔离文件结构失败: %v", err)
	}
	if n != 1 {
		t.Fatalf("隔离文件里应保留原表 %s", table)
	}
}

// prepareRawDB 用驱动直接建库（模拟「外来写入者」造出的库）。
func prepareRawDB(t *testing.T, path string, stmts ...string) {
	t.Helper()
	db, err := sql.Open(driverName, path)
	if err != nil {
		t.Fatalf("打开原始库失败: %v", err)
	}
	defer func() { _ = db.Close() }()
	for _, stmt := range stmts {
		if _, err := db.Exec(stmt); err != nil {
			t.Fatalf("执行 %q 失败: %v", stmt, err)
		}
	}
}

func setRawUserVersion(t *testing.T, path string, v int) {
	t.Helper()
	db, err := sql.Open(driverName, path)
	if err != nil {
		t.Fatalf("打开原始库失败: %v", err)
	}
	defer func() { _ = db.Close() }()
	if _, err := db.Exec("PRAGMA user_version = " + itoa(v)); err != nil {
		t.Fatalf("写 user_version 失败: %v", err)
	}
}

func readFile(t *testing.T, path string) []byte {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("读取 %s 失败: %v", path, err)
	}
	return b
}
