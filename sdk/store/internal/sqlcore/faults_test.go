package sqlcore

import (
	"context"
	"database/sql/driver"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/peterplaykkk/yzagmgr/sdk/router"
	"github.com/peterplaykkk/yzagmgr/sdk/store"
)

// 本文件用注入式驱动（faultdriver_test.go）逐条点亮存储层的**错误分支**：
// 「库在运行期坏掉」时每条路径都必须报可读错误、不静默吞行、不 panic。
// 断言的是「谁在哪个环节失败、错误文案指出什么」，不是「恰好返回非 nil」。

// errFault 是注入用的一枚哨兵错误。
var errFault = errors.New("注入故障")

// expectErr 断言错误非 nil 且文案含 want（want 为空只要求非 nil）。
func expectErr(t *testing.T, got error, want string) {
	t.Helper()
	if got == nil {
		t.Fatalf("应报错（期望含 %q），实际 nil", want)
	}
	if want != "" && !strings.Contains(got.Error(), want) {
		t.Fatalf("错误应含 %q，实际 %v", want, got)
	}
}

// badRow 造一行「扫不进目标」的数据。
//
// 实证结论（本机 Go 1.27）：database/sql 对 *string/*int 目标有大量兜底转换——
// 数值、[]byte、bool、乃至任何 fmt.Stringer（含 time.Time）都会被悄悄转成字符串，
// 于是「语义上异构的值」照样 Scan 成功。真正会报 unsupported Scan 的只有非法取值
// 类型；本行用它来表达「取值层给出非法值」这一防御分支：读路径必须报错，不能把
// 垃圾当数据、更不能静默少行。
func badRow(cols int) [][]driver.Value {
	row := make([]driver.Value, cols)
	for i := range row {
		row[i] = struct{ X int }{}
	}
	return [][]driver.Value{row}
}

// countRow 造一行「COUNT(*) = n」（requireArm 与 alias 冲突检查的输入）。
func countRow(n int64) [][]driver.Value { return [][]driver.Value{{n}} }

// ── 构造期（open）：先校验配置，再连库，最后迁移 + 载入视图 ────────────────

func TestConstructorFaults(t *testing.T) {
	newFault := func(t *testing.T, cfg *faultConfig) error {
		t.Helper()
		key := fmt.Sprintf("ctor-%d", atomic.AddInt64(&faultSeq, 1))
		faultRegistry.Store(key, cfg)
		t.Cleanup(func() { faultRegistry.Delete(key) })
		d := Dialect{name: "fault", driver: faultDriverName, bytesType: "BLOB", keyCap: 255, maxOpen: 1}
		_, err := Open(d, key, store.Options{TokenKey: testKey()})
		return err
	}

	t.Run("Ping 失败", func(t *testing.T) {
		expectErr(t, newFault(t, &faultConfig{pingErr: errFault}), "连接 fault 不可用")
	})
	t.Run("DDL 失败", func(t *testing.T) {
		expectErr(t, newFault(t, &faultConfig{execErr: errFault, execFailAt: 1}), "第 1 条 DDL 失败")
	})
	// Migrate 版本行按存在性判据分流（方案 A）：探测 SELECT 回一行 ⇒ 稳态 UPDATE、
	// 回空行 ⇒ 首建 INSERT。注入式驱动默认回空行集，rowsAt 可把探测 steer 到「在册」。
	t.Run("schema 版本行 UPDATE 失败", func(t *testing.T) {
		expectErr(t, newFault(t, &faultConfig{
			execErr: errFault, execErrOn: "UPDATE",
			rowsAt: map[int][][]driver.Value{1: countRow(1)}, // 探测（第 1 次查询）回在册
		}), "写 schema 版本行失败")
	})
	t.Run("schema 版本行存在性探测失败", func(t *testing.T) {
		// 探测 SELECT 本身失败（旧实现的 RowsAffected 失败分支已随方案 A 移除：
		// Migrate 不再读变更行计数，其余写路径的「读删除/删除结果失败」另有覆盖）。
		expectErr(t, newFault(t, &faultConfig{queryErr: errFault}), "探测 schema 版本行存在性失败")
	})
	t.Run("schema 版本行 INSERT 失败", func(t *testing.T) {
		expectErr(t, newFault(t, &faultConfig{
			execErr: errFault, execErrOn: "INSERT INTO", // 探测缺省回空行 ⇒ 走首建路径
		}), "初始化 schema 版本行失败")
	})
	t.Run("载入视图时查询失败", func(t *testing.T) {
		// 构造期第 1 次查询 = 版本行存在性探测（须成功），第 2 次 = 载入视图首查。
		expectErr(t, newFault(t, &faultConfig{queryErr: errFault, queryFailAt: 2}), "载入路由视图失败")
	})
	t.Run("载入视图时开事务失败", func(t *testing.T) {
		expectErr(t, newFault(t, &faultConfig{beginErr: errFault}), "开只读事务失败")
	})
	t.Run("驱动名未注册", func(t *testing.T) {
		_, err := Open(Dialect{name: "bogus", driver: "yzagmgr-no-such-driver", maxOpen: 1}, "x",
			store.Options{TokenKey: testKey()})
		expectErr(t, err, "打开 bogus 连接失败")
	})
}

// ── 载入视图（load）：四条 SELECT 的查询/遍历/解析错误各自可辨 ──────────────

func TestViewLoadFaults(t *testing.T) {
	ctx := context.Background()
	t.Run("routes 查询失败", func(t *testing.T) {
		s := newFaultStore(t, &faultConfig{queryErr: errFault, queryFailAt: 1})
		expectErr(t, s.Refresh(ctx), "读请求映射失败")
	})
	t.Run("remaps 查询失败", func(t *testing.T) {
		s := newFaultStore(t, &faultConfig{queryErr: errFault, queryFailAt: 2})
		expectErr(t, s.Refresh(ctx), "读模型映射失败")
	})
	t.Run("effort 表头查询失败", func(t *testing.T) {
		s := newFaultStore(t, &faultConfig{queryErr: errFault, queryFailAt: 3})
		expectErr(t, s.Refresh(ctx), "读思考深度表头失败")
	})
	t.Run("effort 条目查询失败", func(t *testing.T) {
		s := newFaultStore(t, &faultConfig{queryErr: errFault, queryFailAt: 4})
		expectErr(t, s.Refresh(ctx), "读思考深度条目失败")
	})
	t.Run("routes 行解析失败", func(t *testing.T) {
		s := newFaultStore(t, &faultConfig{rowsAt: map[int][][]driver.Value{1: badRow(3)}})
		expectErr(t, s.Refresh(ctx), "解析请求映射行失败")
	})
	t.Run("remaps 行解析失败", func(t *testing.T) {
		s := newFaultStore(t, &faultConfig{rowsAt: map[int][][]driver.Value{2: badRow(3)}})
		expectErr(t, s.Refresh(ctx), "解析模型映射行失败")
	})
	t.Run("effort 表头解析失败", func(t *testing.T) {
		s := newFaultStore(t, &faultConfig{rowsAt: map[int][][]driver.Value{3: badRow(2)}})
		expectErr(t, s.Refresh(ctx), "解析思考深度表头失败")
	})
	t.Run("effort 条目解析失败", func(t *testing.T) {
		s := newFaultStore(t, &faultConfig{rowsAt: map[int][][]driver.Value{4: badRow(3)}})
		expectErr(t, s.Refresh(ctx), "解析思考深度条目失败")
	})
	// 遍历/关闭报错要按「第几次 Query」定位：第 1 条失败时后面几条根本走不到。
	t.Run("遍历报错（routes）", func(t *testing.T) {
		s := newFaultStore(t, &faultConfig{rowsErr: errFault, rowsErrAt: 1})
		expectErr(t, s.Refresh(ctx), "遍历结果集失败")
	})
	t.Run("遍历报错（remaps）", func(t *testing.T) {
		s := newFaultStore(t, &faultConfig{rowsErr: errFault, rowsErrAt: 2})
		expectErr(t, s.Refresh(ctx), "遍历结果集失败")
	})
	t.Run("遍历报错（effort 表头）", func(t *testing.T) {
		s := newFaultStore(t, &faultConfig{rowsErr: errFault, rowsErrAt: 3})
		expectErr(t, s.Refresh(ctx), "遍历结果集失败")
	})
	t.Run("遍历报错（effort 条目）", func(t *testing.T) {
		s := newFaultStore(t, &faultConfig{rowsErr: errFault, rowsErrAt: 4})
		expectErr(t, s.Refresh(ctx), "遍历结果集失败")
	})
	t.Run("关闭结果集报错", func(t *testing.T) {
		s := newFaultStore(t, &faultConfig{rowsCloseErr: errFault, rowsCloseAt: 1})
		expectErr(t, s.Refresh(ctx), "注入故障")
	})
}

// ── 写路径：事务/提交/刷新失败 ─────────────────────────────────────────────

func TestWritePathFaults(t *testing.T) {
	ctx := context.Background()
	rt := router.Route{Name: "alpha", Endpoint: "http://up.example.com", CredRef: refA}

	t.Run("开事务失败", func(t *testing.T) {
		s := newFaultStore(t, &faultConfig{beginErr: errFault})
		expectErr(t, s.PutRoute(ctx, rt), "开事务失败")
	})
	t.Run("提交失败", func(t *testing.T) {
		s := newFaultStore(t, &faultConfig{commitErr: errFault})
		expectErr(t, s.PutRoute(ctx, rt), "提交事务失败")
	})
	t.Run("提交成功但视图重载失败", func(t *testing.T) {
		// 写事务的语句走 Exec（不受 queryErrOn 影响），重载走 Query ⇒ 命中「已提交」分支。
		s := newFaultStore(t, &faultConfig{queryErr: errFault, queryErrOn: "routes"})
		expectErr(t, s.PutRoute(ctx, rt), "写入已提交")
	})
	t.Run("删臂清理子表失败", func(t *testing.T) {
		s := newFaultStore(t, &faultConfig{execErr: errFault, execFailAt: 1})
		expectErr(t, s.PutRoute(ctx, rt), "清理 alpha 的 model_remaps 失败")
	})
	t.Run("删臂主表失败", func(t *testing.T) {
		s := newFaultStore(t, &faultConfig{execErr: errFault, execFailAt: 4})
		expectErr(t, s.PutRoute(ctx, rt), "删除请求映射 \"alpha\" 失败")
	})
	t.Run("写入臂失败", func(t *testing.T) {
		s := newFaultStore(t, &faultConfig{execErr: errFault, execFailAt: 5})
		expectErr(t, s.PutRoute(ctx, rt), "写入请求映射 \"alpha\" 失败")
	})
}

// ── 读路径：每条 SELECT 的查询/扫描/遍历/关闭失败 ──────────────────────────

func TestReadPathQueryFaults(t *testing.T) {
	ctx := context.Background()

	t.Run("GetRoute 查询失败", func(t *testing.T) {
		s := newFaultStore(t, &faultConfig{queryErr: errFault})
		_, _, err := s.GetRoute(ctx, "alpha")
		expectErr(t, err, "读取请求映射 \"alpha\" 失败")
	})
	t.Run("ListRoutes 查询失败", func(t *testing.T) {
		s := newFaultStore(t, &faultConfig{queryErr: errFault})
		_, err := s.ListRoutes(ctx)
		expectErr(t, err, "列出请求映射失败")
	})
	t.Run("ListRemaps 查询失败", func(t *testing.T) {
		s := newFaultStore(t, &faultConfig{queryErr: errFault})
		_, err := s.ListRemaps(ctx, "alpha")
		expectErr(t, err, "列出映射失败")
	})
	t.Run("GetEffort 表头查询失败", func(t *testing.T) {
		s := newFaultStore(t, &faultConfig{queryErr: errFault})
		_, _, err := s.GetEffort(ctx, "alpha")
		expectErr(t, err, "读 alpha 的思考深度表头失败")
	})
	t.Run("GetEffort 条目查询失败", func(t *testing.T) {
		// 表头查询要回一行（否则走的是「未配置」而不是条目查询），第 2 次查询才失败。
		s := newFaultStore(t, &faultConfig{
			rowsAt:      map[int][][]driver.Value{1: {[]driver.Value{"max"}}},
			queryErr:    errFault,
			queryFailAt: 2,
		})
		_, _, err := s.GetEffort(ctx, "alpha")
		expectErr(t, err, "读 alpha 的思考深度条目失败")
	})
	t.Run("ListTokenRefs 查询失败", func(t *testing.T) {
		s := newFaultStore(t, &faultConfig{queryErr: errFault})
		_, err := s.ListTokenRefs(ctx)
		expectErr(t, err, "列出凭据引用失败")
	})
	t.Run("Token 查询失败", func(t *testing.T) {
		s := newFaultStore(t, &faultConfig{queryErr: errFault})
		_, err := s.Token(refA)
		expectErr(t, err, "读取令牌")
	})
	t.Run("requireArm 查询失败", func(t *testing.T) {
		s := newFaultStore(t, &faultConfig{queryErr: errFault})
		expectErr(t, s.PutRemap(ctx, "alpha", router.Remap{Alias: "a", Target: "t"}), "校验请求映射 \"alpha\" 失败")
	})
	t.Run("alias 冲突查询失败", func(t *testing.T) {
		// 第 1 次查询（臂在册校验）回 1 行，第 2 次查询（alias 撞名校验）失败。
		s := newFaultStore(t, &faultConfig{
			rowsAt:      map[int][][]driver.Value{1: countRow(1)},
			queryErr:    errFault,
			queryFailAt: 2,
		})
		expectErr(t, s.PutRemap(ctx, "alpha", router.Remap{Alias: "a", Target: "t"}), "校验 alias 与臂名冲突失败")
	})
	t.Run("DeleteEffort 查询失败", func(t *testing.T) {
		s := newFaultStore(t, &faultConfig{queryErr: errFault})
		_, err := s.DeleteEffort(ctx, "alpha")
		expectErr(t, err, "读 alpha 的思考深度表头失败")
	})
}

func TestReadPathScanFaults(t *testing.T) {
	ctx := context.Background()

	t.Run("ListRoutes 行解析失败", func(t *testing.T) {
		s := newFaultStore(t, &faultConfig{rowsAt: map[int][][]driver.Value{1: badRow(3)}})
		_, err := s.ListRoutes(ctx)
		expectErr(t, err, "解析请求映射行失败")
	})
	t.Run("GetRoute 行解析失败", func(t *testing.T) {
		s := newFaultStore(t, &faultConfig{rowsAt: map[int][][]driver.Value{1: badRow(3)}})
		_, _, err := s.GetRoute(ctx, "alpha")
		expectErr(t, err, "读取请求映射 \"alpha\" 失败")
	})
	t.Run("ListRemaps 行解析失败", func(t *testing.T) {
		s := newFaultStore(t, &faultConfig{rowsAt: map[int][][]driver.Value{1: badRow(2)}})
		_, err := s.ListRemaps(ctx, "alpha")
		expectErr(t, err, "解析映射行失败")
	})
	t.Run("GetEffort 条目解析失败", func(t *testing.T) {
		// 表头查询得到一行、条目查询回类型不符的行 ⇒ 走条目解析错误分支。
		s := newFaultStore(t, &faultConfig{rowsAt: map[int][][]driver.Value{
			1: {[]driver.Value{"max"}}, 2: badRow(2),
		}})
		_, _, err := s.GetEffort(ctx, "alpha")
		expectErr(t, err, "解析思考深度条目失败")
	})
	t.Run("ListTokenRefs 行解析失败", func(t *testing.T) {
		s := newFaultStore(t, &faultConfig{rowsAt: map[int][][]driver.Value{1: badRow(1)}})
		_, err := s.ListTokenRefs(ctx)
		expectErr(t, err, "解析凭据引用失败")
	})
	t.Run("ListRoutes 遍历报错", func(t *testing.T) {
		s := newFaultStore(t, &faultConfig{rowsErr: errFault, rowsErrAt: 1})
		_, err := s.ListRoutes(ctx)
		expectErr(t, err, "遍历结果集失败")
	})
	t.Run("ListRoutes 关闭结果集报错", func(t *testing.T) {
		s := newFaultStore(t, &faultConfig{rowsCloseErr: errFault, rowsCloseAt: 1})
		_, err := s.ListRoutes(ctx)
		expectErr(t, err, "注入故障")
	})
	t.Run("ListRemaps 遍历报错", func(t *testing.T) {
		s := newFaultStore(t, &faultConfig{rowsErr: errFault, rowsErrAt: 1})
		_, err := s.ListRemaps(ctx, "alpha")
		expectErr(t, err, "遍历结果集失败")
	})
	t.Run("ListRemaps 关闭结果集报错", func(t *testing.T) {
		s := newFaultStore(t, &faultConfig{rowsCloseErr: errFault, rowsCloseAt: 1})
		_, err := s.ListRemaps(ctx, "alpha")
		expectErr(t, err, "注入故障")
	})
	t.Run("GetEffort 条目遍历报错", func(t *testing.T) {
		s := newFaultStore(t, &faultConfig{
			rowsAt:    map[int][][]driver.Value{1: {[]driver.Value{"max"}}},
			rowsErr:   errFault,
			rowsErrAt: 2,
		})
		_, _, err := s.GetEffort(ctx, "alpha")
		expectErr(t, err, "遍历结果集失败")
	})
	t.Run("GetEffort 条目关闭报错", func(t *testing.T) {
		s := newFaultStore(t, &faultConfig{
			rowsAt:       map[int][][]driver.Value{1: {[]driver.Value{"max"}}},
			rowsCloseErr: errFault,
			rowsCloseAt:  2,
		})
		_, _, err := s.GetEffort(ctx, "alpha")
		expectErr(t, err, "注入故障")
	})
	t.Run("ListTokenRefs 遍历报错", func(t *testing.T) {
		s := newFaultStore(t, &faultConfig{rowsErr: errFault, rowsErrAt: 1})
		_, err := s.ListTokenRefs(ctx)
		expectErr(t, err, "遍历结果集失败")
	})
	t.Run("ListTokenRefs 关闭结果集报错", func(t *testing.T) {
		s := newFaultStore(t, &faultConfig{rowsCloseErr: errFault, rowsCloseAt: 1})
		_, err := s.ListTokenRefs(ctx)
		expectErr(t, err, "注入故障")
	})
}

// ── 删除/剩余写路径的 Exec 与 RowsAffected 失败 ────────────────────────────

func TestRemainingWriteFaults(t *testing.T) {
	ctx := context.Background()

	t.Run("DeleteRoute 读删除结果失败", func(t *testing.T) {
		s := newFaultStore(t, &faultConfig{resultErr: errFault})
		_, err := s.DeleteRoute(ctx, "alpha")
		expectErr(t, err, "读删除结果失败")
	})
	t.Run("PutRemap 覆盖失败", func(t *testing.T) {
		s := newFaultStore(t, &faultConfig{
			rowsAt:  map[int][][]driver.Value{1: countRow(1), 2: countRow(0)}, // 臂在册、alias 不冲突
			execErr: errFault, execFailAt: 1,
		})
		expectErr(t, s.PutRemap(ctx, "alpha", router.Remap{Alias: "a", Target: "t"}), "覆盖映射 \"a\" 失败")
	})
	t.Run("PutRemap 写入失败", func(t *testing.T) {
		s := newFaultStore(t, &faultConfig{
			rowsAt:  map[int][][]driver.Value{1: countRow(1), 2: countRow(0)},
			execErr: errFault, execFailAt: 2,
		})
		expectErr(t, s.PutRemap(ctx, "alpha", router.Remap{Alias: "a", Target: "t"}), "写入映射 \"a\" 失败")
	})
	t.Run("DeleteRemap 失败", func(t *testing.T) {
		s := newFaultStore(t, &faultConfig{execErr: errFault})
		_, err := s.DeleteRemap(ctx, "alpha", "a")
		expectErr(t, err, "删除映射 \"a\" 失败")
	})
	t.Run("DeleteRemap 读删除结果失败", func(t *testing.T) {
		s := newFaultStore(t, &faultConfig{resultErr: errFault})
		_, err := s.DeleteRemap(ctx, "alpha", "a")
		expectErr(t, err, "读删除结果失败")
	})
	t.Run("PutEffort 清理表头失败", func(t *testing.T) {
		s := newFaultStore(t, &faultConfig{
			rowsAt:  map[int][][]driver.Value{1: countRow(1)}, // 臂在册
			execErr: errFault, execFailAt: 1,
		})
		expectErr(t, s.PutEffort(ctx, "alpha", router.EffortTable{ForceEffort: "max"}), "清理 alpha 的思考深度表头失败")
	})
	t.Run("PutEffort 清理条目失败", func(t *testing.T) {
		s := newFaultStore(t, &faultConfig{
			rowsAt:  map[int][][]driver.Value{1: countRow(1)}, // 臂在册
			execErr: errFault, execFailAt: 2,
		})
		expectErr(t, s.PutEffort(ctx, "alpha", router.EffortTable{ForceEffort: "max"}), "清理 alpha 的思考深度条目失败")
	})
	t.Run("PutEffort 写表头失败", func(t *testing.T) {
		s := newFaultStore(t, &faultConfig{
			rowsAt:  map[int][][]driver.Value{1: countRow(1)}, // 臂在册
			execErr: errFault, execFailAt: 3,
		})
		expectErr(t, s.PutEffort(ctx, "alpha", router.EffortTable{ForceEffort: "max"}), "写入 alpha 的思考深度表头失败")
	})
	t.Run("PutEffort 写条目失败", func(t *testing.T) {
		s := newFaultStore(t, &faultConfig{
			rowsAt:  map[int][][]driver.Value{1: countRow(1)},
			execErr: errFault, execFailAt: 4,
		})
		expectErr(t, s.PutEffort(ctx, "alpha", router.EffortTable{
			Items: []router.EffortEntry{{From: "high", To: "max"}},
		}), "写入 alpha 的思考深度条目 0 失败")
	})
	t.Run("DeleteEffort 清理表头失败", func(t *testing.T) {
		s := newFaultStore(t, &faultConfig{
			rowsAt:  map[int][][]driver.Value{1: countRow(1)}, // 该臂有深度表
			execErr: errFault, execFailAt: 1,
		})
		_, err := s.DeleteEffort(ctx, "alpha")
		expectErr(t, err, "清理 alpha 的思考深度表头失败")
	})
	t.Run("DeleteEffort 清理条目失败", func(t *testing.T) {
		s := newFaultStore(t, &faultConfig{
			rowsAt:  map[int][][]driver.Value{1: countRow(1)}, // 该臂有深度表
			execErr: errFault, execFailAt: 2,
		})
		_, err := s.DeleteEffort(ctx, "alpha")
		expectErr(t, err, "清理 alpha 的思考深度条目失败")
	})
	t.Run("PutToken 覆盖失败", func(t *testing.T) {
		s := newFaultStore(t, &faultConfig{execErr: errFault, execFailAt: 1})
		expectErr(t, s.PutToken(ctx, refA, "tok"), "覆盖令牌")
	})
	t.Run("PutToken 写入失败", func(t *testing.T) {
		s := newFaultStore(t, &faultConfig{execErr: errFault, execFailAt: 2})
		expectErr(t, s.PutToken(ctx, refA, "tok"), "写入令牌")
	})
	t.Run("DeleteToken 失败", func(t *testing.T) {
		s := newFaultStore(t, &faultConfig{execErr: errFault})
		_, err := s.DeleteToken(ctx, refA)
		expectErr(t, err, "删除令牌")
	})
	t.Run("DeleteToken 读删除结果失败", func(t *testing.T) {
		s := newFaultStore(t, &faultConfig{resultErr: errFault})
		_, err := s.DeleteToken(ctx, refA)
		expectErr(t, err, "读删除结果失败")
	})
	t.Run("Migrate 中途失败", func(t *testing.T) {
		s := newFaultStore(t, &faultConfig{execErr: errFault, execFailAt: 3})
		expectErr(t, s.Migrate(ctx), "第 3 条 DDL 失败")
	})
}

// ── 密文列的坏数据（真 SQLite，第三方写脏行）────────────────────────────────

func TestCipherCorruptRows(t *testing.T) {
	insert := func(s *sqlStore, ref string, blob []byte, scheme string) {
		table := s.t(tableTokens)
		if _, err := s.db.Exec(s.stmt(`INSERT INTO %s (cred_ref, cipher, cipher_scheme, updated_at) VALUES (?, ?, ?, ?)`, table),
			ref, blob, scheme, 0); err != nil {
			t.Fatalf("写脏行失败：%v", err)
		}
	}

	t.Run("密文过短", func(t *testing.T) {
		s := openSQLiteRaw(t, store.Options{})
		insert(s, "short", []byte("nope"), store.SchemeAESGCM)
		_, err := s.Token("short")
		expectErr(t, err, "密文长度非法")
		if !errors.Is(err, store.ErrCipher) {
			t.Fatalf("应为 ErrCipher，实际 %v", err)
		}
	})
	t.Run("方案未知", func(t *testing.T) {
		s := openSQLiteRaw(t, store.Options{})
		insert(s, "weird", []byte("tok"), "rot13")
		_, err := s.Token("weird")
		expectErr(t, err, "存储方案未知")
	})
	t.Run("明文模式实例读密文行", func(t *testing.T) {
		s := openSQLiteRaw(t, store.Options{AllowPlaintext: true})
		insert(s, "enc", []byte("tok"), store.SchemeAESGCM)
		_, err := s.Token("enc")
		expectErr(t, err, "未提供 TokenKey")
		if !errors.Is(err, store.ErrKeyRequired) {
			t.Fatalf("应为 ErrKeyRequired，实际 %v", err)
		}
	})
}

// ── 零值句柄与脏视图（不 panic、不误命中）──────────────────────────────────

func TestZeroValueHandles(t *testing.T) {
	var s sqlStore // 零值句柄：视图未载入
	if _, _, ok := s.Lookup("x"); ok {
		t.Fatal("零值句柄不应命中")
	}
	if t0 := s.Effort("x"); t0.ForceEffort != "" || len(t0.Items) != 0 {
		t.Fatalf("零值句柄应回零值，实际 %+v", t0)
	}
}

func TestDirtyAliasRowDoesNotHit(t *testing.T) {
	// 第三方直接写入「臂不存在」的映射行：视图兜底为不命中（臂名恒胜、孤儿 alias 不可达）。
	s := openSQLiteRaw(t, store.Options{})
	ctx := context.Background()
	if _, err := s.db.Exec(s.stmt(`INSERT INTO %s (arm, alias, target, updated_at) VALUES (?, ?, ?, ?)`,
		s.t(tableModelRemaps)), "ghost-arm", "ghost-alias", "m", 0); err != nil {
		t.Fatalf("写脏行失败：%v", err)
	}
	if err := s.Refresh(ctx); err != nil {
		t.Fatalf("Refresh 失败：%v", err)
	}
	if _, _, ok := s.Snapshot().Lookup("ghost-alias"); ok {
		t.Fatal("孤儿 alias（臂不在册）不应命中")
	}
	// 管理面仍旧如实回读该行（记录现状，不静默丢）。
	remaps, err := s.ListRemaps(ctx, "ghost-arm")
	if err != nil || len(remaps) != 1 {
		t.Fatalf("ListRemaps 应如实回读脏行：%+v %v", remaps, err)
	}
}
