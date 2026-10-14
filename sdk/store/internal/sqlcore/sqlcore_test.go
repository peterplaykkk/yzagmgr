package sqlcore

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	// 本包测试自带的驱动注册：sqlcore 生产代码不 import 任何驱动（驱动名由各驱动
	// 子包注册），测试里要开真 SQLite 库就只能自己在测试二进制里注册。
	_ "modernc.org/sqlite"

	"github.com/peterplaykkk/yzagmgr/sdk/router"
	"github.com/peterplaykkk/yzagmgr/sdk/store"
)

// ── 测试脚手架 ─────────────────────────────────────────────────────────────

// testKey 是确定性的 32 字节测试密钥（只用于测试；真实部署从 Secret/环境注入）。
func testKey() []byte {
	b := make([]byte, store.KeyLen)
	for i := range b {
		b[i] = byte(i)
	}
	return b
}

// sqliteDSN 给出测试用的 SQLite DSN（临时目录 + 与守护进程同款的 pragma 组合）。
func sqliteDSN(t *testing.T, name string) string {
	t.Helper()
	return filepath.Join(t.TempDir(), name+".db") + "?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_txlock=immediate"
}

// openSQLite 打开一个测试用 Store（默认加密密钥）。
func openSQLite(t *testing.T, opts store.Options) store.Store {
	t.Helper()
	if opts.TokenKey == nil && !opts.AllowPlaintext {
		opts.TokenKey = testKey()
	}
	st, err := Open(DialectSQLite, sqliteDSN(t, "test"), opts)
	if err != nil {
		t.Fatalf("Open(sqlite) 失败：%v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st
}

// openSQLiteRaw 打开测试用 Store 并返回内部句柄（供密文列回读等白盒断言）。
func openSQLiteRaw(t *testing.T, opts store.Options) *sqlStore {
	t.Helper()
	if opts.TokenKey == nil && !opts.AllowPlaintext {
		opts.TokenKey = testKey()
	}
	st, err := Open(DialectSQLite, sqliteDSN(t, "test"), opts)
	if err != nil {
		t.Fatalf("Open(sqlite) 失败：%v", err)
	}
	s := st.(*sqlStore)
	t.Cleanup(func() { _ = s.Close() })
	return s
}

// rawTokenRow 直接回读令牌行（密文 + 方案），绕过 Store 的解密路径。
func rawTokenRow(t *testing.T, s *sqlStore, ref string) ([]byte, string) {
	t.Helper()
	var (
		blob   []byte
		scheme string
	)
	q := s.stmt(`SELECT cipher, cipher_scheme FROM %s WHERE cred_ref = ?`, s.t(tableTokens))
	if err := s.db.QueryRow(q, ref).Scan(&blob, &scheme); err != nil {
		t.Fatalf("回读令牌行 %q 失败：%v", ref, err)
	}
	return blob, scheme
}

// mustPutRoute 登记一条臂，失败即终止。
func mustPutRoute(t *testing.T, st store.Store, rt router.Route) {
	t.Helper()
	if err := st.PutRoute(context.Background(), rt); err != nil {
		t.Fatalf("PutRoute(%+v) 失败：%v", rt, err)
	}
}

const (
	refA = "yzagmgr-managed-token://alpha"
	refB = "beta"
)

// ── Migrate：幂等 ──────────────────────────────────────────────────────────

func TestMigrateIdempotent(t *testing.T) {
	s := openSQLiteRaw(t, store.Options{})
	ctx := context.Background()
	// 构造期已迁移一次；这里连跑两次必须都成功（IF NOT EXISTS + 版本行幂等）。
	for i := 0; i < 2; i++ {
		if err := s.Migrate(ctx); err != nil {
			t.Fatalf("第 %d 次 Migrate 失败：%v", i+2, err)
		}
	}
	var (
		version int
		rows    int
	)
	vq := s.stmt(`SELECT COUNT(*), COALESCE(MAX(version), 0) FROM %s`, s.t(tableSchema))
	if err := s.db.QueryRow(vq).Scan(&rows, &version); err != nil {
		t.Fatalf("读 schema 表失败：%v", err)
	}
	if rows != 1 || version != schemaVersion {
		t.Fatalf("schema 版本行应为 1 行 version=%d，实际 rows=%d version=%d", schemaVersion, rows, version)
	}
	// 六张表都在（含 schema 表）。
	for _, suffix := range []string{tableSchema, tableRoutes, tableModelRemaps, tableEffortTables, tableEffortEntries, tableTokens} {
		var n int
		q := s.stmt(`SELECT COUNT(*) FROM %s`, s.t(suffix))
		if err := s.db.QueryRow(q).Scan(&n); err != nil {
			t.Fatalf("表 %s 不可查：%v", s.t(suffix), err)
		}
	}
}

// TestMigrateSameSecondSameValueIdempotent 钉住「同秒同值二次 Migrate 幂等」：
// 冻结时钟让每次 Migrate 写出的 version/updated_at 与行内现值**逐字节相同**——
// 这正是 go-sql-driver/mysql 默认 changed-rows 语义下 RowsAffected=0、缺陷实现
// 误判首建撞 1062 的场景（真 MySQL 上的同款钉子在 sdk/store/mysql 集成格内，
// YZAGMGR_SDK_MYSQL_DSN 门控）。存在性判据实现不读变更行计数，必须幂等：
// 不报错、版本行不翻倍、updated_at 落冻结值（证明稳态 UPDATE 真跑过）。
func TestMigrateSameSecondSameValueIdempotent(t *testing.T) {
	s := openSQLiteRaw(t, store.Options{})
	ctx := context.Background()
	fixed := time.Unix(1759000000, 0)
	s.now = func() time.Time { return fixed }
	for i := 0; i < 3; i++ {
		if err := s.Migrate(ctx); err != nil {
			t.Fatalf("同秒同值第 %d 次 Migrate 失败：%v", i+1, err)
		}
	}
	var (
		rows, version int
		updatedAt     int64
	)
	q := s.stmt(`SELECT COUNT(*), COALESCE(MAX(version), 0), COALESCE(MAX(updated_at), 0) FROM %s`, s.t(tableSchema))
	if err := s.db.QueryRow(q).Scan(&rows, &version, &updatedAt); err != nil {
		t.Fatalf("读 schema 表失败：%v", err)
	}
	if rows != 1 {
		t.Fatalf("同值二次 Migrate 后版本行应恰 1 行（误走 INSERT 即撞主键），实际 %d 行", rows)
	}
	if version != schemaVersion {
		t.Fatalf("版本应为 %d，实际 %d", schemaVersion, version)
	}
	if updatedAt != fixed.Unix() {
		t.Fatalf("updated_at 应落冻结值 %d，实际 %d（稳态 UPDATE 未执行？）", fixed.Unix(), updatedAt)
	}
}

// ── 臂（routes）CRUD ───────────────────────────────────────────────────────

func TestRouteCRUD(t *testing.T) {
	st := openSQLite(t, store.Options{})
	ctx := context.Background()

	mustPutRoute(t, st, router.Route{Name: "alpha", Endpoint: "http://up.example.com/", CredRef: refA})
	mustPutRoute(t, st, router.Route{Name: "beta", Endpoint: "https://b.example.com", CredRef: refB})

	got, ok, err := st.GetRoute(ctx, "alpha")
	if err != nil || !ok {
		t.Fatalf("GetRoute(alpha) = %v, %v, %v", got, ok, err)
	}
	if got.Endpoint != "http://up.example.com" {
		t.Fatalf("端点应去掉尾斜杠，实际 %q", got.Endpoint)
	}
	if got.CredRef != refA {
		t.Fatalf("凭据引用应为 %q，实际 %q", refA, got.CredRef)
	}

	// upsert：同名再登记即覆盖。
	mustPutRoute(t, st, router.Route{Name: "alpha", Endpoint: "http://up2.example.com", CredRef: refB})
	got, _, _ = st.GetRoute(ctx, "alpha")
	if got.Endpoint != "http://up2.example.com" || got.CredRef != refB {
		t.Fatalf("同名 Put 应覆盖，实际 %+v", got)
	}

	list, err := st.ListRoutes(ctx)
	if err != nil {
		t.Fatalf("ListRoutes 失败：%v", err)
	}
	if len(list) != 2 || list[0].Name != "alpha" || list[1].Name != "beta" {
		t.Fatalf("ListRoutes 应按臂名排序且含两条，实际 %+v", list)
	}

	if _, ok, _ := st.GetRoute(ctx, "nope"); ok {
		t.Fatal("GetRoute(nope) 应 ok=false")
	}
	del, err := st.DeleteRoute(ctx, "alpha")
	if err != nil || !del {
		t.Fatalf("DeleteRoute(alpha) = %v, %v", del, err)
	}
	if del2, _ := st.DeleteRoute(ctx, "alpha"); del2 {
		t.Fatal("重复删除应返回 false")
	}
}

func TestRouteCascadeKeepsTokens(t *testing.T) {
	st := openSQLite(t, store.Options{})
	ctx := context.Background()
	mustPutRoute(t, st, router.Route{Name: "alpha", Endpoint: "http://up.example.com", CredRef: refA})
	if err := st.PutRemap(ctx, "alpha", router.Remap{Alias: "a-fast", Target: "m1"}); err != nil {
		t.Fatalf("PutRemap 失败：%v", err)
	}
	if err := st.PutEffort(ctx, "alpha", router.EffortTable{
		Items: []router.EffortEntry{{From: "high", To: "max"}}, ForceEffort: "max",
	}); err != nil {
		t.Fatalf("PutEffort 失败：%v", err)
	}
	if err := st.PutToken(ctx, refA, "sk-alpha"); err != nil {
		t.Fatalf("PutToken 失败：%v", err)
	}

	if _, err := st.DeleteRoute(ctx, "alpha"); err != nil {
		t.Fatalf("DeleteRoute 失败：%v", err)
	}
	// 子表连删。
	if remaps, _ := st.ListRemaps(ctx, "alpha"); len(remaps) != 0 {
		t.Fatalf("删臂应连删映射表，实际 %+v", remaps)
	}
	if _, ok, _ := st.GetEffort(ctx, "alpha"); ok {
		t.Fatal("删臂应连删深度表")
	}
	if _, _, ok := st.Snapshot().Lookup("a-fast"); ok {
		t.Fatal("删臂后 alias 不应再命中")
	}
	// 令牌按引用独立存在：不随臂删除。
	if refs, _ := st.ListTokenRefs(ctx); len(refs) != 1 || refs[0] != refA {
		t.Fatalf("删臂不得删令牌，实际 %+v", refs)
	}
	if tok, err := st.Token(refA); err != nil || tok != "sk-alpha" {
		t.Fatalf("删臂后令牌应仍可解析：%q, %v", tok, err)
	}
}

func TestRouteValidationNegative(t *testing.T) {
	st := openSQLite(t, store.Options{})
	ctx := context.Background()
	cases := []struct {
		name string
		rt   router.Route
		want string
	}{
		{"空名", router.Route{Endpoint: "http://up.example.com", CredRef: refA}, "不得为空"},
		{"大写起头", router.Route{Name: "Alpha", Endpoint: "http://up.example.com", CredRef: refA}, "小写字母起头"},
		{"数字起头", router.Route{Name: "1alpha", Endpoint: "http://up.example.com", CredRef: refA}, "小写字母起头"},
		{"非法字符", router.Route{Name: "al pha", Endpoint: "http://up.example.com", CredRef: refA}, "只允许"},
		{"超长名", router.Route{Name: strings.Repeat("a", 97), Endpoint: "http://up.example.com", CredRef: refA}, "过长"},
		{"空端点", router.Route{Name: "alpha", CredRef: refA}, "端点不得为空"},
		{"非 http", router.Route{Name: "alpha", Endpoint: "ftp://up.example.com", CredRef: refA}, "scheme"},
		{"缺主机", router.Route{Name: "alpha", Endpoint: "http:///path", CredRef: refA}, "主机名"},
		{"内嵌凭据", router.Route{Name: "alpha", Endpoint: "http://u:p@up.example.com", CredRef: refA}, "内嵌凭据"},
		{"空凭据引用", router.Route{Name: "alpha", Endpoint: "http://up.example.com"}, "凭据引用：不得为空"},
		{"凭据引用含空白", router.Route{Name: "alpha", Endpoint: "http://up.example.com", CredRef: "a b"}, "空白"},
		{"凭据引用过长", router.Route{Name: "alpha", Endpoint: "http://up.example.com", CredRef: strings.Repeat("r", 256)}, "过长"},
		{"端点不可解析", router.Route{Name: "alpha", Endpoint: "http://[bad", CredRef: refA}, "不是合法 URL"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := st.PutRoute(ctx, tc.rt)
			if err == nil {
				t.Fatalf("应被拒绝，实际成功")
			}
			if !errors.Is(err, store.ErrConfig) || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("错误应含 %q 且为 ErrConfig，实际 %v", tc.want, err)
			}
		})
	}
	// 全程零写入。
	if list, _ := st.ListRoutes(ctx); len(list) != 0 {
		t.Fatalf("负向用例不得留下任何一行，实际 %+v", list)
	}
}

func TestRouteLoopbackAllowed(t *testing.T) {
	// 与守护进程刻意的差异：SDK 嵌入形态允许回环端点（测试桩/同机上游）。
	st := openSQLite(t, store.Options{})
	ctx := context.Background()
	mustPutRoute(t, st, router.Route{Name: "local", Endpoint: "http://127.0.0.1:18080", CredRef: refA})
	if got, ok, _ := st.GetRoute(ctx, "local"); !ok || got.Endpoint != "http://127.0.0.1:18080" {
		t.Fatalf("回环端点应被接受，实际 %+v ok=%v", got, ok)
	}
}

// ── 模型映射（model_remaps）CRUD ───────────────────────────────────────────

func TestRemapCRUDAndNegatives(t *testing.T) {
	st := openSQLite(t, store.Options{})
	ctx := context.Background()
	mustPutRoute(t, st, router.Route{Name: "alpha", Endpoint: "http://up.example.com", CredRef: refA})

	// 臂不在册 ⇒ ErrArmNotFound（映射从属于臂）。
	if err := st.PutRemap(ctx, "ghost", router.Remap{Alias: "g", Target: "t"}); !errors.Is(err, store.ErrArmNotFound) {
		t.Fatalf("臂不在册应报 ErrArmNotFound，实际 %v", err)
	}
	negatives := []struct {
		name string
		rm   router.Remap
		want string
	}{
		{"空 alias", router.Remap{Target: "t"}, "alias：不得为空"},
		{"空 target", router.Remap{Alias: "a"}, "target：不得为空"},
		{"自映射", router.Remap{Alias: "same", Target: "same"}, "自映射"},
		{"alias 撞本臂名", router.Remap{Alias: "alpha", Target: "t"}, "与本臂同名"},
		{"alias 撞他臂名", router.Remap{Alias: "beta", Target: "t"}, "在册请求映射名"},
	}
	mustPutRoute(t, st, router.Route{Name: "beta", Endpoint: "http://b.example.com", CredRef: refB})
	for _, tc := range negatives {
		t.Run(tc.name, func(t *testing.T) {
			err := st.PutRemap(ctx, "alpha", tc.rm)
			if err == nil {
				t.Fatal("应被拒绝，实际成功")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("错误应含 %q，实际 %v", tc.want, err)
			}
		})
	}

	if err := st.PutRemap(ctx, "alpha", router.Remap{Alias: "fast", Target: "m1"}); err != nil {
		t.Fatalf("PutRemap 失败：%v", err)
	}
	if err := st.PutRemap(ctx, "alpha", router.Remap{Alias: "cheap", Target: "m2"}); err != nil {
		t.Fatalf("PutRemap 失败：%v", err)
	}
	// upsert：同 alias 覆盖 target。
	if err := st.PutRemap(ctx, "alpha", router.Remap{Alias: "fast", Target: "m1-v2"}); err != nil {
		t.Fatalf("PutRemap 覆盖失败：%v", err)
	}
	list, err := st.ListRemaps(ctx, "alpha")
	if err != nil {
		t.Fatalf("ListRemaps 失败：%v", err)
	}
	want := []router.Remap{{Alias: "cheap", Target: "m2"}, {Alias: "fast", Target: "m1-v2"}}
	if fmt.Sprint(list) != fmt.Sprint(want) {
		t.Fatalf("ListRemaps 应为 %+v，实际 %+v", want, list)
	}
	del, err := st.DeleteRemap(ctx, "alpha", "cheap")
	if err != nil || !del {
		t.Fatalf("DeleteRemap = %v, %v", del, err)
	}
	if del2, _ := st.DeleteRemap(ctx, "alpha", "cheap"); del2 {
		t.Fatal("重复删除应返回 false")
	}
}

// ── 思考深度表（effort）CRUD ───────────────────────────────────────────────

func TestEffortCRUD(t *testing.T) {
	st := openSQLite(t, store.Options{})
	ctx := context.Background()
	mustPutRoute(t, st, router.Route{Name: "alpha", Endpoint: "http://up.example.com", CredRef: refA})

	if _, ok, _ := st.GetEffort(ctx, "alpha"); ok {
		t.Fatal("未配置时 GetEffort 应 ok=false")
	}
	if err := st.PutEffort(ctx, "ghost", router.EffortTable{}); !errors.Is(err, store.ErrArmNotFound) {
		t.Fatalf("臂不在册应报 ErrArmNotFound，实际 %v", err)
	}

	// 归一：档位大小写/空白被归一（承载 glm 的 {xhigh→max, max→max} 形态）。
	in := router.EffortTable{
		Items: []router.EffortEntry{
			{From: " XHIGH ", To: "Max"},
			{From: "", To: "low"},
			{From: "max", To: "max"},
		},
		ForceEffort: " HIGH ",
	}
	if err := st.PutEffort(ctx, "alpha", in); err != nil {
		t.Fatalf("PutEffort 失败：%v", err)
	}
	got, ok, err := st.GetEffort(ctx, "alpha")
	if err != nil || !ok {
		t.Fatalf("GetEffort = %+v, %v, %v", got, ok, err)
	}
	// 条目按登记顺序回读（seq 保序），force 归一为小写。
	wantItems := []router.EffortEntry{{From: "XHIGH", To: "max"}, {From: "", To: "low"}, {From: "max", To: "max"}}
	if fmt.Sprint(got.Items) != fmt.Sprint(wantItems) || got.ForceEffort != "high" {
		t.Fatalf("归一/保序不符：%+v", got)
	}
	// 顺序进视图：Snapshot().Effort 必须与库内一致。
	vt := st.Snapshot().Effort("alpha")
	if fmt.Sprint(vt.Items) != fmt.Sprint(wantItems) || vt.ForceEffort != "high" {
		t.Fatalf("视图深度表不符：%+v", vt)
	}

	// 整表替换：再 Put 一次只留新表。
	if err := st.PutEffort(ctx, "alpha", router.EffortTable{ForceEffort: "max"}); err != nil {
		t.Fatalf("PutEffort 替换失败：%v", err)
	}
	got, _, _ = st.GetEffort(ctx, "alpha")
	if len(got.Items) != 0 || got.ForceEffort != "max" {
		t.Fatalf("整表替换不符：%+v", got)
	}

	// 空表 = 清除配置（与 Snapshot 零值口径一致）。
	if err := st.PutEffort(ctx, "alpha", router.EffortTable{}); err != nil {
		t.Fatalf("PutEffort(空表) 失败：%v", err)
	}
	if _, ok, _ := st.GetEffort(ctx, "alpha"); ok {
		t.Fatal("空表应等价于未配置")
	}
	if t2 := st.Snapshot().Effort("alpha"); len(t2.Items) != 0 || t2.ForceEffort != "" {
		t.Fatalf("空表应回零值，实际 %+v", t2)
	}

	// 再建一次，然后 DeleteEffort。
	if err := st.PutEffort(ctx, "alpha", router.EffortTable{ForceEffort: "low"}); err != nil {
		t.Fatalf("PutEffort 失败：%v", err)
	}
	existed, err := st.DeleteEffort(ctx, "alpha")
	if err != nil || !existed {
		t.Fatalf("DeleteEffort = %v, %v", existed, err)
	}
	if existed2, _ := st.DeleteEffort(ctx, "alpha"); existed2 {
		t.Fatal("重复删除应返回 false")
	}
}

func TestEffortValidationNegative(t *testing.T) {
	st := openSQLite(t, store.Options{})
	ctx := context.Background()
	mustPutRoute(t, st, router.Route{Name: "alpha", Endpoint: "http://up.example.com", CredRef: refA})
	cases := []struct {
		name  string
		table router.EffortTable
		want  string
	}{
		{"表外档", router.EffortTable{ForceEffort: "ultra"}, "白名单档"},
		{"表外目标", router.EffortTable{Items: []router.EffortEntry{{From: "high", To: "ultra"}}}, "白名单档"},
		{"from 重复", router.EffortTable{Items: []router.EffortEntry{{From: "high", To: "max"}, {From: "high", To: "low"}}}, "重复"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := st.PutEffort(ctx, "alpha", tc.table)
			if err == nil || !errors.Is(err, store.ErrConfig) || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("应报 ErrConfig 且含 %q，实际 %v", tc.want, err)
			}
		})
	}
}

// ── 令牌：加密、负向、回读 ─────────────────────────────────────────────────

func TestTokenEncryptedRoundTrip(t *testing.T) {
	s := openSQLiteRaw(t, store.Options{})
	ctx := context.Background()
	const plain = "sk-live-abc123-DO-NOT-LEAK"

	if err := s.PutToken(ctx, refA, plain); err != nil {
		t.Fatalf("PutToken 失败：%v", err)
	}
	// 明文只经解密路径回来。
	got, err := s.Token(refA)
	if err != nil || got != plain {
		t.Fatalf("Token(%s) = %q, %v", refA, got, err)
	}
	// 落库列是密文：不等于明文，且方案标为 aes-256-gcm。
	blob, scheme := rawTokenRow(t, s, refA)
	if scheme != store.SchemeAESGCM {
		t.Fatalf("cipher_scheme 应为 %s，实际 %s", store.SchemeAESGCM, scheme)
	}
	if bytes.Contains(blob, []byte(plain)) {
		t.Fatal("密文列里出现了明文子串")
	}
	if string(blob) == plain {
		t.Fatal("密文列不得等于明文")
	}
	if len(blob) != 12+len(plain)+16 {
		t.Fatalf("密文布局应为 nonce(12)+ct+tag(16)=%d，实际 %d", 12+len(plain)+16, len(blob))
	}

	// 同值两次落库：nonce 随机 ⇒ 密文不同（不可用密文相等反推「令牌没变」）。
	if err := s.PutToken(ctx, refA, plain); err != nil {
		t.Fatalf("PutToken 覆盖失败：%v", err)
	}
	blob2, _ := rawTokenRow(t, s, refA)
	if bytes.Equal(blob, blob2) {
		t.Fatal("同值两次落库的密文相同：nonce 不是随机的")
	}
	if got2, _ := s.Token(refA); got2 != plain {
		t.Fatalf("覆盖后解密应仍为明文，实际 %q", got2)
	}

	// ListTokenRefs 只回引用。
	if err := s.PutToken(ctx, refB, "b-token"); err != nil {
		t.Fatalf("PutToken 失败：%v", err)
	}
	// 排序断言与顺序无关：refA 的 scheme 改名（agmgr-→yzagmgr-）后按 cred_ref
	// 字典序排到了 refB 后面——契约只要求「只回引用且回全」，不钉顺序。
	refs, err := s.ListTokenRefs(ctx)
	gotRefs := append([]string(nil), refs...)
	wantRefs := []string{refA, refB}
	slices.Sort(gotRefs)
	slices.Sort(wantRefs)
	if err != nil || fmt.Sprint(gotRefs) != fmt.Sprint(wantRefs) {
		t.Fatalf("ListTokenRefs = %+v, %v", refs, err)
	}
	// 删除。
	del, err := s.DeleteToken(ctx, refB)
	if err != nil || !del {
		t.Fatalf("DeleteToken = %v, %v", del, err)
	}
	if del2, _ := s.DeleteToken(ctx, refB); del2 {
		t.Fatal("重复删除应返回 false")
	}
	if _, err := s.Token(refB); !errors.Is(err, store.ErrTokenNotFound) {
		t.Fatalf("已删引用应报 ErrTokenNotFound，实际 %v", err)
	}
}

func TestTokenNegativeValidation(t *testing.T) {
	st := openSQLite(t, store.Options{})
	ctx := context.Background()
	if err := st.PutToken(ctx, "", "t"); !errors.Is(err, store.ErrConfig) {
		t.Fatalf("空引用应报 ErrConfig，实际 %v", err)
	}
	if err := st.PutToken(ctx, "ref", "   "); !errors.Is(err, store.ErrConfig) {
		t.Fatalf("空令牌应报 ErrConfig，实际 %v", err)
	}
	if _, err := st.Token("missing"); !errors.Is(err, store.ErrTokenNotFound) {
		t.Fatalf("缺引用应报 ErrTokenNotFound，实际 %v", err)
	}
	if _, err := st.Token("  "); !errors.Is(err, store.ErrTokenNotFound) {
		t.Fatalf("空引用应报 ErrTokenNotFound，实际 %v", err)
	}
}

// TestOpenKeyDiscipline 是单包时代的 TestCipherKeyDiscipline 经驱动入口（Open）的同一套断言：
// 密钥纪律必须先于连库报出。**cipher 层**的同一套纪律另有 `sdk/store` 的
// TestCipherKeyDiscipline（拆包后两边各守一层：加密件本身 / 驱动入口）。
func TestOpenKeyDiscipline(t *testing.T) {
	dsn := sqliteDSN(t, "key")

	// 无密钥 + 未显式 AllowPlaintext ⇒ 构造即错（默认拒绝明文），错误可读且不含密钥。
	_, err := Open(DialectSQLite, dsn, store.Options{})
	if !errors.Is(err, store.ErrKeyRequired) {
		t.Fatalf("缺密钥应报 ErrKeyRequired，实际 %v", err)
	}
	if !strings.Contains(err.Error(), "AllowPlaintext") {
		t.Fatalf("错误应指出显式开关，实际 %v", err)
	}
	if strings.Contains(err.Error(), string(testKey())) {
		t.Fatal("错误串不得含密钥内容")
	}

	// 长度不对 ⇒ ErrKeyLength（只报长度）。
	for _, n := range []int{1, 16, 31, 33} {
		key := make([]byte, n)
		_, err := Open(DialectSQLite, dsn, store.Options{TokenKey: key})
		if !errors.Is(err, store.ErrKeyLength) {
			t.Fatalf("密钥 %d 字节应报 ErrKeyLength，实际 %v", n, err)
		}
		if !strings.Contains(err.Error(), fmt.Sprintf("%d 字节", n)) {
			t.Fatalf("错误应只报长度，实际 %v", err)
		}
	}

	// 密钥 + AllowPlaintext 同时给出 ⇒ 歧义配置，拒绝。
	if _, err := Open(DialectSQLite, dsn, store.Options{TokenKey: testKey(), AllowPlaintext: true}); !errors.Is(err, store.ErrConfig) {
		t.Fatalf("互斥配置应报 ErrConfig，实际 %v", err)
	}
}

func TestPlaintextModeIsExplicitAndAudited(t *testing.T) {
	ctx := context.Background()
	var logs bytes.Buffer
	dsn := sqliteDSN(t, "plain")

	st, err := Open(DialectSQLite, dsn, store.Options{
		AllowPlaintext: true,
		Logger:         slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelWarn})),
	})
	if err != nil {
		t.Fatalf("显式 AllowPlaintext 应可构造，实际 %v", err)
	}
	raw := st.(*sqlStore)
	if err := st.PutToken(ctx, refA, "plain-token"); err != nil {
		t.Fatalf("PutToken 失败：%v", err)
	}
	if blob, scheme := rawTokenRow(t, raw, refA); scheme != store.SchemePlaintext || string(blob) != "plain-token" {
		t.Fatalf("明文模式应落明文并标 scheme=plaintext，实际 scheme=%s blob=%q", scheme, blob)
	}
	if got, err := st.Token(refA); err != nil || got != "plain-token" {
		t.Fatalf("明文模式应可读：%q, %v", got, err)
	}
	if !strings.Contains(logs.String(), "明文") {
		t.Fatalf("明文模式必须留警示日志，实际日志：%q", logs.String())
	}
	if err := st.Close(); err != nil {
		t.Fatalf("Close 失败：%v", err)
	}

	// 同库换一个「未显式开启明文」的 Store ⇒ 读写两侧都拒绝明文行。
	strict, err := Open(DialectSQLite, dsn, store.Options{TokenKey: testKey()})
	if err != nil {
		t.Fatalf("Open(DialectSQLite, 加密) 失败：%v", err)
	}
	defer func() { _ = strict.Close() }()
	if _, err := strict.Token(refA); !errors.Is(err, store.ErrPlaintextRefused) {
		t.Fatalf("未开明文开关时读明文行应报 ErrPlaintextRefused，实际 %v", err)
	}
	// 加密路径覆盖同一引用后，新值以密文落库（明文行被换掉）。
	if err := strict.PutToken(ctx, refA, "plain-token"); err != nil {
		t.Fatalf("PutToken 失败：%v", err)
	}
	if blob, scheme := rawTokenRow(t, strict.(*sqlStore), refA); scheme != store.SchemeAESGCM || bytes.Contains(blob, []byte("plain-token")) {
		t.Fatalf("覆盖后应为密文，实际 scheme=%s blob=%q", scheme, blob)
	}
}

func TestOpenRefusesWrongKey(t *testing.T) {
	// 换密钥读旧密文 ⇒ 解密失败（ErrCipher），不是「返回垃圾」也不是 panic。
	ctx := context.Background()
	dsn := sqliteDSN(t, "rotate")
	first, err := Open(DialectSQLite, dsn, store.Options{TokenKey: testKey()})
	if err != nil {
		t.Fatalf("Open(sqlite) 失败：%v", err)
	}
	if err := first.PutToken(ctx, refA, "t"); err != nil {
		t.Fatalf("PutToken 失败：%v", err)
	}
	_ = first.Close()

	other := make([]byte, store.KeyLen)
	for i := range other {
		other[i] = 0xAA
	}
	second, err := Open(DialectSQLite, dsn, store.Options{TokenKey: other})
	if err != nil {
		t.Fatalf("Open(DialectSQLite, 新密钥) 失败：%v", err)
	}
	defer func() { _ = second.Close() }()
	_, err = second.Token(refA)
	if !errors.Is(err, store.ErrCipher) {
		t.Fatalf("换密钥读旧密文应报 ErrCipher，实际 %v", err)
	}
	if strings.Contains(err.Error(), "t") && !strings.Contains(err.Error(), refA) {
		t.Fatalf("错误串不应含明文令牌，实际 %v", err)
	}
}

// ── 视图（Snapshot）语义 ───────────────────────────────────────────────────

func TestSnapshotLookupSemantics(t *testing.T) {
	st := openSQLite(t, store.Options{})
	ctx := context.Background()
	mustPutRoute(t, st, router.Route{Name: "alpha", Endpoint: "http://a.example.com", CredRef: refA})
	mustPutRoute(t, st, router.Route{Name: "beta", Endpoint: "http://b.example.com", CredRef: refB})
	if err := st.PutRemap(ctx, "alpha", router.Remap{Alias: "fast", Target: "m-upstream"}); err != nil {
		t.Fatalf("PutRemap 失败：%v", err)
	}
	// 二跳样本：target 自身也是（另一臂的）alias —— 引擎契约是**单层**，不得再改写。
	if err := st.PutRemap(ctx, "alpha", router.Remap{Alias: "chain", Target: "fast"}); err != nil {
		t.Fatalf("PutRemap 失败：%v", err)
	}
	snap := st.Snapshot()

	// 臂名直选：出站模型不变。
	arm, out, ok := snap.Lookup("alpha")
	if !ok || arm.Name != "alpha" || out != "alpha" {
		t.Fatalf("臂名直选应不改写：%+v %q %v", arm, out, ok)
	}
	// alias 单层改写。
	arm, out, ok = snap.Lookup("fast")
	if !ok || arm.Name != "alpha" || out != "m-upstream" {
		t.Fatalf("alias 应单层改写为 target：%+v %q %v", arm, out, ok)
	}
	// 单层：链式 alias 只改写一次（fast 不再展开）。
	if _, out, ok = snap.Lookup("chain"); !ok || out != "fast" {
		t.Fatalf("单层改写不得二跳，实际 out=%q ok=%v", out, ok)
	}
	if arm, out, ok = snap.Lookup("beta"); !ok || arm.Name != "beta" || out != "beta" {
		t.Fatalf("另一臂直选：%+v %q %v", arm, out, ok)
	}
	// 未命中：ok=false（引擎据回 4xx，不落兜底）。
	if _, _, ok := snap.Lookup("nope"); ok {
		t.Fatal("未命中应 ok=false")
	}
	if _, _, ok := snap.Lookup(""); ok {
		t.Fatal("空 model 应 ok=false")
	}

	// 写入后视图自动跟新（无需重建 Snapshot 句柄）。
	if err := st.PutRemap(ctx, "beta", router.Remap{Alias: "fast2", Target: "m2"}); err != nil {
		t.Fatalf("PutRemap 失败：%v", err)
	}
	if arm, out, ok := snap.Lookup("fast2"); !ok || arm.Name != "beta" || out != "m2" {
		t.Fatalf("写入后视图应立即可见：%+v %q %v", arm, out, ok)
	}
	// 删臂后视图收缩。
	if _, err := st.DeleteRoute(ctx, "beta"); err != nil {
		t.Fatalf("DeleteRoute 失败：%v", err)
	}
	if _, _, ok := snap.Lookup("fast2"); ok {
		t.Fatal("删臂后 alias 不应命中")
	}
}

func TestSnapshotEffortReturnsCopy(t *testing.T) {
	st := openSQLite(t, store.Options{})
	ctx := context.Background()
	mustPutRoute(t, st, router.Route{Name: "alpha", Endpoint: "http://a.example.com", CredRef: refA})
	if err := st.PutEffort(ctx, "alpha", router.EffortTable{Items: []router.EffortEntry{{From: "high", To: "max"}}}); err != nil {
		t.Fatalf("PutEffort 失败：%v", err)
	}
	snap := st.Snapshot()
	got := snap.Effort("alpha")
	got.Items[0].To = "tampered"
	got.ForceEffort = "tampered"
	again := snap.Effort("alpha")
	if again.Items[0].To != "max" || again.ForceEffort != "" {
		t.Fatalf("Effort 的返回值必须是副本，实际被外部改动污染：%+v", again)
	}
	if t0 := snap.Effort("ghost"); len(t0.Items) != 0 || t0.ForceEffort != "" {
		t.Fatalf("未配置臂应回零值，实际 %+v", t0)
	}
}

func TestRefreshPicksUpExternalWrites(t *testing.T) {
	// 多写入方：另一实例改了同一个库，本实例 Refresh 后对齐。
	ctx := context.Background()
	dir := t.TempDir()
	dsn := filepath.Join(dir, "shared.db") + "?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)"

	one, err := Open(DialectSQLite, dsn, store.Options{TokenKey: testKey()})
	if err != nil {
		t.Fatalf("open 失败：%v", err)
	}
	defer func() { _ = one.Close() }()
	two, err := Open(DialectSQLite, dsn, store.Options{TokenKey: testKey()})
	if err != nil {
		t.Fatalf("open 失败：%v", err)
	}
	defer func() { _ = two.Close() }()

	if err := two.PutRoute(ctx, router.Route{Name: "ext", Endpoint: "http://ext.example.com", CredRef: refA}); err != nil {
		t.Fatalf("PutRoute 失败：%v", err)
	}
	if _, _, ok := one.Snapshot().Lookup("ext"); ok {
		t.Fatal("未 Refresh 前不应看到外部写入")
	}
	if err := one.Refresh(ctx); err != nil {
		t.Fatalf("Refresh 失败：%v", err)
	}
	if _, _, ok := one.Snapshot().Lookup("ext"); !ok {
		t.Fatal("Refresh 后应看到外部写入")
	}
}

// ── 并发与生命周期 ─────────────────────────────────────────────────────────

func TestConcurrentReadsDuringWrites(t *testing.T) {
	// 引擎每个请求都会 Lookup/Effort；写是配置期动作。两者并发必须无竞态（-race 复核）。
	st := openSQLite(t, store.Options{})
	ctx := context.Background()
	mustPutRoute(t, st, router.Route{Name: "alpha", Endpoint: "http://a.example.com", CredRef: refA})
	if err := st.PutToken(ctx, refA, "seed"); err != nil {
		t.Fatalf("PutToken 失败：%v", err)
	}
	snap := st.Snapshot()

	var wg sync.WaitGroup
	stop := make(chan struct{})
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				if _, _, ok := snap.Lookup("alpha"); !ok {
					t.Error("并发读期间臂必须恒在册")
					return
				}
				_ = snap.Effort("alpha")
				if _, err := st.Token(refA); err != nil {
					t.Errorf("并发读令牌失败：%v", err)
					return
				}
			}
		}()
	}
	for i := 0; i < 6; i++ {
		n := fmt.Sprintf("arm%d", i)
		mustPutRoute(t, st, router.Route{Name: n, Endpoint: "http://a.example.com", CredRef: refA})
		if err := st.PutRemap(ctx, n, router.Remap{Alias: "alias" + n, Target: "m"}); err != nil {
			t.Fatalf("PutRemap 失败：%v", err)
		}
		if err := st.PutToken(ctx, refA, fmt.Sprintf("tok-%d", i)); err != nil {
			t.Fatalf("PutToken 失败：%v", err)
		}
		if _, err := st.DeleteRoute(ctx, n); err != nil {
			t.Fatalf("DeleteRoute 失败：%v", err)
		}
	}
	close(stop)
	wg.Wait()
}

func TestClosedStore(t *testing.T) {
	st := openSQLite(t, store.Options{})
	ctx := context.Background()
	if err := st.Close(); err != nil {
		t.Fatalf("Close 失败：%v", err)
	}
	if err := st.Close(); err != nil {
		t.Fatalf("重复 Close 应返回 nil，实际 %v", err)
	}
	if _, err := st.ListRoutes(ctx); !errors.Is(err, store.ErrClosed) {
		t.Fatalf("关闭后 ListRoutes 应报 ErrClosed，实际 %v", err)
	}
	if err := st.PutRoute(ctx, router.Route{Name: "a", Endpoint: "http://a.example.com", CredRef: refA}); !errors.Is(err, store.ErrClosed) {
		t.Fatalf("关闭后 PutRoute 应报 ErrClosed，实际 %v", err)
	}
	if _, err := st.Token(refA); !errors.Is(err, store.ErrClosed) {
		t.Fatalf("关闭后 Token 应报 ErrClosed，实际 %v", err)
	}
	if err := st.Refresh(ctx); !errors.Is(err, store.ErrClosed) {
		t.Fatalf("关闭后 Refresh 应报 ErrClosed，实际 %v", err)
	}
	if err := st.Migrate(ctx); !errors.Is(err, store.ErrClosed) {
		t.Fatalf("关闭后 Migrate 应报 ErrClosed，实际 %v", err)
	}
	if _, _, err := st.GetRoute(ctx, "a"); !errors.Is(err, store.ErrClosed) {
		t.Fatalf("关闭后 GetRoute 应报 ErrClosed，实际 %v", err)
	}
	if _, err := st.ListRemaps(ctx, "a"); !errors.Is(err, store.ErrClosed) {
		t.Fatalf("关闭后 ListRemaps 应报 ErrClosed，实际 %v", err)
	}
	if _, _, err := st.GetEffort(ctx, "a"); !errors.Is(err, store.ErrClosed) {
		t.Fatalf("关闭后 GetEffort 应报 ErrClosed，实际 %v", err)
	}
	if _, err := st.ListTokenRefs(ctx); !errors.Is(err, store.ErrClosed) {
		t.Fatalf("关闭后 ListTokenRefs 应报 ErrClosed，实际 %v", err)
	}
	if _, _, ok := st.Snapshot().Lookup("x"); ok {
		t.Fatal("关闭后不应命中任何臂")
	}
}

func TestTablePrefix(t *testing.T) {
	st, err := Open(DialectSQLite, sqliteDSN(t, "prefix"), store.Options{TokenKey: testKey(), TablePrefix: "tenant_1_"})
	if err != nil {
		t.Fatalf("自定义前缀应可构造，实际 %v", err)
	}
	defer func() { _ = st.Close() }()
	raw := st.(*sqlStore)
	for _, suffix := range []string{tableSchema, tableRoutes, tableModelRemaps, tableEffortTables, tableEffortEntries, tableTokens} {
		var n int
		if err := raw.db.QueryRow(fmt.Sprintf("SELECT COUNT(*) FROM %s", raw.t(suffix))).Scan(&n); err != nil {
			t.Fatalf("表 %s 不存在：%v", raw.t(suffix), err)
		}
	}
	// 非法前缀（含非标识符字符/数字起头）一律拒绝，防 SQL 注入进 DDL 文本。
	for _, bad := range []string{"a-b", "a b", "1a", "a;drop table x", "前缀"} {
		if _, err := Open(DialectSQLite, sqliteDSN(t, "bad"), store.Options{TokenKey: testKey(), TablePrefix: bad}); !errors.Is(err, store.ErrConfig) {
			t.Fatalf("非法前缀 %q 应报 ErrConfig，实际 %v", bad, err)
		}
	}
}

func TestOpenRejectsBadDSN(t *testing.T) {
	// 配置校验先于连库：形态错误与「有没有 server」无关，必须确定性报出。
	if _, err := Open(DialectMySQL, "", store.Options{TokenKey: testKey()}); !errors.Is(err, store.ErrConfig) {
		t.Fatalf("空 DSN 应报 ErrConfig，实际 %v", err)
	}
	// SQLite DSN 指向不存在的目录 ⇒ 连接/建表失败且错误可读（不是 panic）。
	_, err := Open(DialectSQLite, "/nonexistent-dir-yzagmgr/x.db", store.Options{TokenKey: testKey()})
	if err == nil {
		t.Fatal("不可达的 SQLite 路径应报错")
	}
	if !strings.Contains(err.Error(), "sqlite") {
		t.Fatalf("错误应指明方言，实际 %v", err)
	}
}
