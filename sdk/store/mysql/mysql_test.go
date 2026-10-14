package mysql

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/peterplaykkk/yzagmgr/sdk/router"
	"github.com/peterplaykkk/yzagmgr/sdk/store"
)

// ── MySQL：接口/编译层 + 集成格 ────────────────────────────────────────────
//
// 本机没有 mysql server，因此：
//   - 接口/编译层测试（下方 TestMySQLInterfaceLevel）**真跑**：形态校验与互斥配置
//     都在离线可断言，不需要 server（方言参数与 MySQL 专属 DDL 的断言在
//     internal/sqlcore 的 dialect_test.go：方言表对驱动子包不可见，由本包选一个、
//     由 sqlcore 钉住形态）；
//   - 集成格登记为 **unknown**（TestMySQLIntegrationCell，置 YZAGMGR_SDK_MYSQL_DSN 才真跑），
//     **不折 pass**。解除条件：本机起任意 MySQL 8（docker 亦可）并把 DSN 导进
//     YZAGMGR_SDK_MYSQL_DSN，重跑 `go test ./sdk/store/... -run MySQL -v`。

// TestMySQLInterfaceLevel 离线可跑的 MySQL 面：构造期纪律（无 server 也必须确定性报出）。
func TestMySQLInterfaceLevel(t *testing.T) {
	// 1) 密钥纪律先于连库：没有 server 也能确定性报出配置错误。
	if _, err := Open("yzagmgr:secret@tcp(127.0.0.1:3306)/yzagmgr", store.Options{}); !errors.Is(err, store.ErrKeyRequired) {
		t.Fatalf("缺密钥应报 ErrKeyRequired（与 server 是否存在无关），实际 %v", err)
	}
	if _, err := Open("yzagmgr:secret@tcp(127.0.0.1:3306)/yzagmgr", store.Options{TokenKey: make([]byte, 8)}); !errors.Is(err, store.ErrKeyLength) {
		t.Fatalf("短密钥应报 ErrKeyLength，实际 %v", err)
	}
	if _, err := Open("", store.Options{TokenKey: testKey()}); !errors.Is(err, store.ErrConfig) {
		t.Fatalf("空 DSN 应报 ErrConfig，实际 %v", err)
	}
	if _, err := Open("yzagmgr:secret@tcp(127.0.0.1:3306)/yzagmgr", store.Options{TokenKey: testKey(), AllowPlaintext: true}); !errors.Is(err, store.ErrConfig) {
		t.Fatalf("互斥配置应报 ErrConfig，实际 %v", err)
	}

	// 2) 无 server：连接失败必须是可读错误（不是 panic、不是假成功）。
	_, err := Open("yzagmgr_sdk:secret@tcp(127.0.0.1:1)/yzagmgr?timeout=2s", store.Options{TokenKey: testKey()})
	if err == nil {
		t.Fatal("不可达的 MySQL 应报错")
	}
	if !strings.Contains(err.Error(), "mysql") {
		t.Fatalf("错误应指明方言，实际 %v", err)
	}
	t.Logf("MySQL 无 server 时的构造错误（原文）：%v", err)
}

// TestMySQLIntegrationCell 是登记为 unknown 的集成格：无 DSN 时跳过（带原因与
// 解除条件），有 DSN 时跑真集成（迁移/CRUD/密文回读），**不折 pass**。
func TestMySQLIntegrationCell(t *testing.T) {
	dsn := strings.TrimSpace(os.Getenv("YZAGMGR_SDK_MYSQL_DSN"))
	if dsn == "" {
		t.Skip("集成格 unknown：本机无 mysql server（未设 YZAGMGR_SDK_MYSQL_DSN）。" +
			"解除条件：起任意 MySQL 8 并 export YZAGMGR_SDK_MYSQL_DSN='user:pass@tcp(host:3306)/db?charset=utf8mb4'，" +
			"重跑 go test ./sdk/store/... -run MySQL -v")
	}
	ctx := context.Background()
	const (
		ref    = "mysql-it-ref"
		plain  = "mysql-it-token-2b8d"
		prefix = "yzagmgr_sdk_mysqlit_"
	)
	st, err := Open(dsn, store.Options{TokenKey: testKey(), TablePrefix: prefix})
	if err != nil {
		t.Fatalf("Open(mysql) 失败：%v", err)
	}
	defer func() { _ = st.Close() }()
	if err := st.Migrate(ctx); err != nil {
		t.Fatalf("Migrate 失败：%v", err)
	}
	// 「同秒同值二次 Migrate 幂等」真 MySQL 钉子（YZAGMGR_SDK_MYSQL_DSN 门控内）：
	// Open 内首迁 + 上面显式二迁通常已同秒；这里连对跑 Migrate 并要求一对完整落在
	// 同一 unix 秒——同 version、同 updated_at 的「同值 UPDATE」在 go-sql-driver
	// 默认 changed-rows 语义下 RowsAffected=0，缺陷实现会误判首建再 INSERT 撞
	// Error 1062（修复前原文见 reports/evidence-mysqlfix-20260928/red-before.txt）。
	// 存在性判据实现必须全程幂等。
	if err := migrateSameSecondPair(ctx, st); err != nil {
		t.Fatalf("同秒同值二次 Migrate 幂等失败：%v", err)
	}
	mustPutRoute(t, st, router.Route{Name: "alpha", Endpoint: "http://up.example.com", CredRef: ref})
	if err := st.PutRemap(ctx, "alpha", router.Remap{Alias: "fast", Target: "m1"}); err != nil {
		t.Fatalf("PutRemap 失败：%v", err)
	}
	if err := st.PutEffort(ctx, "alpha", router.EffortTable{ForceEffort: "Max"}); err != nil {
		t.Fatalf("PutEffort 失败：%v", err)
	}
	if err := st.PutToken(ctx, ref, plain); err != nil {
		t.Fatalf("PutToken 失败：%v", err)
	}
	if arm, out, ok := st.Snapshot().Lookup("fast"); !ok || arm.Name != "alpha" || out != "m1" {
		t.Fatalf("alias 选臂不符：%+v %q %v", arm, out, ok)
	}
	if tok, err := st.Token(ref); err != nil || tok != plain {
		t.Fatalf("Token 不符：%q, %v", tok, err)
	}
	// 密文列回读（HEX() 是 MySQL 的十六进制转换）：**另开一条独立连接**直查，
	// 不经 Store 的解密路径（表名 = 测试前缀 + "tokens"）。
	db, err := sql.Open("mysql", dsn) // 驱动名由本包（包自身）空白 import 注册
	if err != nil {
		t.Fatalf("独立连接失败：%v", err)
	}
	defer func() { _ = db.Close() }()
	var scheme, hexCipher string
	q := "SELECT cipher_scheme, HEX(cipher) FROM " + prefix + "tokens WHERE cred_ref = ?"
	if err := db.QueryRowContext(ctx, q, ref).Scan(&scheme, &hexCipher); err != nil {
		t.Fatalf("回读密文列失败：%v", err)
	}
	blob, err := hex.DecodeString(hexCipher)
	if err != nil {
		t.Fatalf("HEX 回读非法：%v", err)
	}
	if scheme != store.SchemeAESGCM || bytes.Contains(blob, []byte(plain)) {
		t.Fatalf("列应为密文：scheme=%s blob=%q", scheme, blob)
	}
	// 幂等钉子回读：反复 Migrate 后版本行仍恰 1 行（缺陷实现误走 INSERT 会在
	// 这里表现为 1062 报错或行数翻倍）。
	var schemaRows int
	if err := db.QueryRowContext(ctx, "SELECT COUNT(*) FROM "+prefix+"schema").Scan(&schemaRows); err != nil {
		t.Fatalf("读版本行行数失败：%v", err)
	}
	if schemaRows != 1 {
		t.Fatalf("版本行应恰 1 行，实际 %d 行", schemaRows)
	}
	t.Logf("MySQL 集成：cipher_scheme=%s，明文 %d 字节未出现在列里", scheme, len(plain))
}

// ── 测试脚手架（测试内重复三份，不跨包共享）─────────────────────────────────

// testKey 是确定性的 32 字节测试密钥（只用于测试；真实部署从 Secret/环境注入）。
func testKey() []byte {
	b := make([]byte, store.KeyLen)
	for i := range b {
		b[i] = byte(i)
	}
	return b
}

// mustPutRoute 登记一条臂，失败即终止。
func mustPutRoute(t *testing.T, st store.Store, rt router.Route) {
	t.Helper()
	if err := st.PutRoute(context.Background(), rt); err != nil {
		t.Fatalf("PutRoute(%+v) 失败：%v", rt, err)
	}
}

// migrateSameSecondPair 连续跑两对 Migrate，并要求至少一对**完整落在同一 unix 秒**：
// 该对的第二次 Migrate 必然是「同 version、同 updated_at」的同值 UPDATE——缺陷实现
// （按 UPDATE 变更行计数判首建）在此撞 Error 1062，存在性判据实现必须幂等。
// 跨秒的对不满足取样条件，重试（100 次仍跨秒视为无法钉住场景，判失败而非放行）。
func migrateSameSecondPair(ctx context.Context, st store.Store) error {
	for i := 0; i < 100; i++ {
		t0 := time.Now().Unix()
		if err := st.Migrate(ctx); err != nil {
			return fmt.Errorf("第 %d 次取样首迁失败：%w", i+1, err)
		}
		if err := st.Migrate(ctx); err != nil {
			return fmt.Errorf("第 %d 次取样二迁失败（同秒同值场景）：%w", i+1, err)
		}
		if time.Now().Unix() == t0 {
			return nil
		}
	}
	return errors.New("100 次取样均跨秒，无法钉住同秒同值场景")
}
