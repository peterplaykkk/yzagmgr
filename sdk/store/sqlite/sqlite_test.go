package sqlite

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/peterplaykkk/yzagmgr/sdk/router"
	"github.com/peterplaykkk/yzagmgr/sdk/store"
)

// ── 驱动包自身的公共面（Open）─────────────────────────────────────────────
//
// SQL 语义与方言形态由 internal/sqlcore 的测试钉住（驱动子包看不到 sqlcore 的
// 内部类型，也不该重复断言）；本文件只钉**这个包的公共契约**：Open 的形态校验、
// 真库端到端可用、以及与 DSN 无关的确定性错误。

// sqliteDSN 给出测试用的 SQLite DSN（临时目录 + 与守护进程同款的 pragma 组合）。
func sqliteDSN(t *testing.T) string {
	t.Helper()
	return filepath.Join(t.TempDir(), "test.db") + "?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_txlock=immediate"
}

// testKey 是确定性的 32 字节测试密钥（只用于测试；真实部署从 Secret/环境注入）。
func testKey() []byte {
	b := make([]byte, store.KeyLen)
	for i := range b {
		b[i] = byte(i)
	}
	return b
}

func TestOpenConfigDiscipline(t *testing.T) {
	dsn := sqliteDSN(t)
	// 配置校验先于连库：形态错误与「有没有库」无关，必须确定性报出。
	if _, err := Open(dsn, store.Options{}); !errors.Is(err, store.ErrKeyRequired) {
		t.Fatalf("缺密钥应报 ErrKeyRequired，实际 %v", err)
	}
	for _, n := range []int{1, 16, 31, 33} {
		if _, err := Open(dsn, store.Options{TokenKey: make([]byte, n)}); !errors.Is(err, store.ErrKeyLength) {
			t.Fatalf("密钥 %d 字节应报 ErrKeyLength，实际 %v", n, err)
		}
	}
	if _, err := Open(dsn, store.Options{TokenKey: testKey(), AllowPlaintext: true}); !errors.Is(err, store.ErrConfig) {
		t.Fatalf("互斥配置应报 ErrConfig，实际 %v", err)
	}
	if _, err := Open("", store.Options{TokenKey: testKey()}); !errors.Is(err, store.ErrConfig) {
		t.Fatalf("空 DSN 应报 ErrConfig，实际 %v", err)
	}
	// 不可达路径：连接/建表失败且错误可读（不是 panic），并指明方言。
	_, err := Open("/nonexistent-dir-yzagmgr/x.db", store.Options{TokenKey: testKey()})
	if err == nil {
		t.Fatal("不可达的 SQLite 路径应报错")
	}
	if !strings.Contains(err.Error(), "sqlite") {
		t.Fatalf("错误应指明方言，实际 %v", err)
	}
}

func TestOpenEndToEnd(t *testing.T) {
	ctx := context.Background()
	const (
		ref   = "yzagmgr-sqlite-it://alpha"
		plain = "sqlite-it-token-9c1f"
	)
	st, err := Open(sqliteDSN(t), store.Options{TokenKey: testKey()})
	if err != nil {
		t.Fatalf("Open(sqlite) 失败：%v", err)
	}
	defer func() { _ = st.Close() }()

	// 打开即迁移：重复 Migrate 必须仍成功（幂等）。
	if err := st.Migrate(ctx); err != nil {
		t.Fatalf("重复 Migrate 失败：%v", err)
	}
	if err := st.PutRoute(ctx, router.Route{Name: "alpha", Endpoint: "http://up.example.com/", CredRef: ref}); err != nil {
		t.Fatalf("PutRoute 失败：%v", err)
	}
	if err := st.PutRemap(ctx, "alpha", router.Remap{Alias: "fast", Target: "m-upstream"}); err != nil {
		t.Fatalf("PutRemap 失败：%v", err)
	}
	if err := st.PutEffort(ctx, "alpha", router.EffortTable{Items: []router.EffortEntry{{From: "high", To: "max"}}}); err != nil {
		t.Fatalf("PutEffort 失败：%v", err)
	}
	if err := st.PutToken(ctx, ref, plain); err != nil {
		t.Fatalf("PutToken 失败：%v", err)
	}

	// 端点归一（去尾斜杠）+ 视图语义 + 令牌解密。
	got, ok, err := st.GetRoute(ctx, "alpha")
	if err != nil || !ok || got.Endpoint != "http://up.example.com" {
		t.Fatalf("GetRoute = %+v, %v, %v", got, ok, err)
	}
	if arm, out, ok := st.Snapshot().Lookup("fast"); !ok || arm.Name != "alpha" || out != "m-upstream" {
		t.Fatalf("alias 选臂不符：%+v %q %v", arm, out, ok)
	}
	if eff := st.Snapshot().Effort("alpha"); len(eff.Items) != 1 || eff.Items[0].To != "max" {
		t.Fatalf("深度表视图不符：%+v", eff)
	}
	if tok, err := st.Token(ref); err != nil || tok != plain {
		t.Fatalf("Token = %q, %v", tok, err)
	}
	// 关闭幂等：重复 Close 返回 nil，之后读写报 ErrClosed。
	if err := st.Close(); err != nil {
		t.Fatalf("Close 失败：%v", err)
	}
	if err := st.Close(); err != nil {
		t.Fatalf("重复 Close 应返回 nil，实际 %v", err)
	}
	if _, err := st.ListRoutes(ctx); !errors.Is(err, store.ErrClosed) {
		t.Fatalf("关闭后 ListRoutes 应报 ErrClosed，实际 %v", err)
	}
}
