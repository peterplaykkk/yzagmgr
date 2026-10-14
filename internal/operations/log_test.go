package operations

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/peterplaykkk/yzagmgr/internal/apierr"
	"github.com/peterplaykkk/yzagmgr/internal/backup"
	"github.com/peterplaykkk/yzagmgr/internal/config"
	"github.com/peterplaykkk/yzagmgr/internal/family"
	"github.com/peterplaykkk/yzagmgr/internal/tx"
)

// ── 夹具 ────────────────────────────────────────────────────────────────────

// testNow 是固定时钟起点，让时间窗与排序断言完全确定。
var testNow = time.Date(2026, 9, 22, 18, 0, 0, 0, time.FixedZone("CST", 8*3600))

// testEnv 建隔离环境：数据根落在 t.TempDir() 下，绝不碰用户真实数据根（UC-013 A1）。
func testEnv(t *testing.T) *config.Env {
	t.Helper()
	return &config.Env{Home: t.TempDir(), Overrides: map[string]string{}, Isolated: true}
}

// newLogWithClock 建日志并返回可推进的时钟指针（测试里改 *at 即可换时间）。
func newLogWithClock(t *testing.T, env *config.Env, at *time.Time) *Log {
	t.Helper()
	return NewLog(env, func() time.Time { return *at })
}

func assertCode(t *testing.T, err error, want apierr.Code) {
	t.Helper()
	if err == nil {
		t.Fatalf("期望错误 %s，实际没有错误", want)
	}
	if got := apierr.From(err).Code; got != want {
		t.Fatalf("错误码不符: got=%s want=%s (err=%v)", got, want, err)
	}
}

// readOrNil 读文件；不存在返回 nil（测试里「文件根本没建」是有意义的断言）。
func readOrNil(t *testing.T, path string) []byte {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		t.Fatalf("读文件失败 %s: %v", path, err)
	}
	return b
}

// rawLines 返回日志文件里的原始行（测试要断言「盘上真的没有令牌」这类字节级事实）。
func rawLines(t *testing.T, env *config.Env) []string {
	t.Helper()
	b, err := os.ReadFile(env.OperationsFile())
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		t.Fatalf("读日志失败: %v", err)
	}
	out := []string{}
	for _, line := range strings.Split(string(b), "\n") {
		if line != "" {
			out = append(out, line)
		}
	}
	return out
}

// mutation 是一对「真实变更 + 其操作记录」的共用上下文：
// 备份登记表与操作日志挂在同一个隔离数据根上，与产品装配方式一致。
type mutation struct {
	env *config.Env
	reg *backup.Registry
	log *Log
}

func newMutation(t *testing.T, at *time.Time) *mutation {
	t.Helper()
	env := testEnv(t)
	if err := env.EnsureLayout(); err != nil {
		t.Fatalf("建数据根失败: %v", err)
	}
	reg := backup.NewRegistry(env, func(family.ID) ([]string, error) { return nil, nil }, func() time.Time { return *at })
	return &mutation{env: env, reg: reg, log: NewLog(env, func() time.Time { return *at })}
}

// apply 走真实写链（tx.Apply + 备份登记），返回事务结果。
func (m *mutation) apply(t *testing.T, toolID, path string, target []byte) (tx.Result, error) {
	t.Helper()
	plans, err := tx.BuildPlans(map[string][]byte{path: target})
	if err != nil {
		t.Fatalf("构造计划失败: %v", err)
	}
	lease := tx.NewCache(time.Minute, nil).Put(&tx.Lease{Operation: "apply", ToolID: toolID, Files: plans})
	return tx.Apply(lease, tx.Options{WriteMode: 0o600, Backuper: m.reg, Reason: backup.ReasonApply})
}

// record 按 API 层的用法记一条操作日志。
func (m *mutation) record(t *testing.T, op Operation) Operation {
	t.Helper()
	got, err := m.log.Append(op)
	if err != nil {
		t.Fatalf("追加操作日志失败: %v", err)
	}
	return got
}

// ── UC-006 A7：每次变更写一条操作记录 ───────────────────────────────────────

// 测试: TestEveryMutationIsRecordedInOperationLog
func TestEveryMutationIsRecordedInOperationLog(t *testing.T) {
	at := testNow
	m := newMutation(t, &at)
	cfg := filepath.Join(m.env.Home, "claude", "settings.json")
	if err := os.MkdirAll(filepath.Dir(cfg), 0o700); err != nil {
		t.Fatalf("建目录失败: %v", err)
	}
	if err := os.WriteFile(cfg, []byte("{\"tier\":\"official\"}\n"), 0o600); err != nil {
		t.Fatalf("写文件失败: %v", err)
	}

	var want []Operation
	for i := 0; i < 3; i++ {
		res, err := m.apply(t, "claude", cfg, []byte("{\"tier\":\"gateway-"+string(rune('a'+i))+"\"}\n"))
		if err != nil {
			t.Fatalf("第 %d 次 apply 失败: %v", i+1, err)
		}
		if len(res.BackupIDs) != 1 {
			t.Fatalf("apply 未产生备份编号: %+v", res)
		}
		at = at.Add(time.Second)
		want = append(want, m.record(t, Operation{
			Action:    "tool.apply",
			Target:    "claude",
			Result:    ResultOK,
			BackupID:  res.BackupIDs[0],
			RequestID: "req_test_" + string(rune('a'+i)),
		}))
	}

	// 每次变更都有一条记录，可按 id 回读（比 JSON 字节：落盘再读回必须逐字节相同）。
	for _, w := range want {
		got, err := m.log.Get(w.ID)
		if err != nil {
			t.Fatalf("按 id 回读失败 %s: %v", w.ID, err)
		}
		gotJSON, _ := json.Marshal(got)
		wantJSON, _ := json.Marshal(w)
		if string(gotJSON) != string(wantJSON) {
			t.Fatalf("回读的记录不符:\n got=%s\nwant=%s", gotJSON, wantJSON)
		}
		if !strings.HasPrefix(got.ID, "op_") {
			t.Fatalf("操作 id 形态不符: %q", got.ID)
		}
		if _, err := time.Parse(time.RFC3339, got.At.Format(time.RFC3339)); err != nil {
			t.Fatalf("at 不是 RFC3339: %v", err)
		}
		// 备份编号必须真的指向一条存在的备份。
		if _, err := m.reg.Get("claude", got.BackupID); err != nil {
			t.Fatalf("操作记录关联的备份不存在: %s (%v)", got.BackupID, err)
		}
	}

	// 盘上每行恰是契约的八个键，且新 → 旧。
	lines := rawLines(t, m.env)
	if len(lines) != len(want) {
		t.Fatalf("日志行数不符: %d != %d", len(lines), len(want))
	}
	for i, line := range lines {
		var shape map[string]json.RawMessage
		if err := json.Unmarshal([]byte(line), &shape); err != nil {
			t.Fatalf("第 %d 行不是 JSON: %v", i+1, err)
		}
		got := make([]string, 0, len(shape))
		for k := range shape {
			got = append(got, k)
		}
		sort.Strings(got)
		wantKeys := []string{"action", "at", "backupId", "errorCode", "id", "requestId", "result", "target"}
		if strings.Join(got, ",") != strings.Join(wantKeys, ",") {
			t.Fatalf("操作记录的键集不符: got=%v want=%v", got, wantKeys)
		}
	}

	page, err := m.log.Query(Filter{})
	if err != nil {
		t.Fatalf("查询失败: %v", err)
	}
	if page.Total != len(want) || len(page.Items) != len(want) {
		t.Fatalf("查询条数不符: %+v", page)
	}
	for i := range page.Items {
		if page.Items[i].ID != want[len(want)-1-i].ID {
			t.Fatalf("日志应新 → 旧: got=%v", ids(page.Items))
		}
	}
	// 过滤：按 action / target / result 各自命中。
	for _, f := range []Filter{
		{Action: "tool.apply"},
		{Target: "claude"},
		{Result: ResultOK},
	} {
		p, err := m.log.Query(f)
		if err != nil {
			t.Fatalf("过滤查询失败 %+v: %v", f, err)
		}
		if p.Total != len(want) {
			t.Fatalf("过滤 %+v 命中数不符: %d", f, p.Total)
		}
	}
	if p, err := m.log.Query(Filter{Action: "tool.restore"}); err != nil || p.Total != 0 {
		t.Fatalf("无关过滤不应命中: total=%d err=%v", p.Total, err)
	}
	if _, err := m.log.Get("op_不存在"); err == nil {
		t.Fatal("非法操作 id 应被拒")
	} else {
		assertCode(t, err, apierr.CodeInvalidRequest)
	}
	if _, err := m.log.Get("op_1_1"); err == nil {
		t.Fatal("形态合法但不存在的操作 id 应报 not_found")
	} else {
		assertCode(t, err, apierr.CodeNotFound)
	}
}

func ids(ops []Operation) []string {
	out := make([]string, 0, len(ops))
	for _, op := range ops {
		out = append(out, op.ID)
	}
	return out
}

// ── UC-014 A13：失败记为失败 ────────────────────────────────────────────────

// 测试: TestFailuresAreLoggedAsFailures
func TestFailuresAreLoggedAsFailures(t *testing.T) {
	at := testNow
	m := newMutation(t, &at)
	dir := filepath.Join(m.env.Home, "opencode")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatalf("建目录失败: %v", err)
	}
	cfg := filepath.Join(dir, "opencode.json")
	if err := os.WriteFile(cfg, []byte("{\"tier\":\"official\"}\n"), 0o600); err != nil {
		t.Fatalf("写文件失败: %v", err)
	}

	// 先走一次正常变更，之后才有「索引损坏」这个前提。
	if _, err := m.apply(t, "opencode", cfg, []byte("{\"tier\":\"gateway-1\"}\n")); err != nil {
		t.Fatalf("首次 apply 失败: %v", err)
	}
	at = at.Add(time.Second)
	m.record(t, Operation{Action: "tool.apply", Target: "opencode", Result: ResultOK})

	// 失败一：备份不可用（索引损坏）⇒ 无备份即不写盘，错误记为 storage。
	indexPath := filepath.Join(m.env.BackupsDir(), "opencode", "index.json")
	if err := os.WriteFile(indexPath, []byte("{ 坏索引"), 0o600); err != nil {
		t.Fatalf("破坏索引失败: %v", err)
	}
	before := readOrNil(t, cfg)
	_, err := m.apply(t, "opencode", cfg, []byte("{\"tier\":\"gateway-2\"}\n"))
	if err == nil {
		t.Fatal("备份不可用时不应继续写入")
	}
	storageCode := apierr.From(err).Code
	if storageCode != apierr.CodeStorage {
		t.Fatalf("备份不可用应按 storage 报出: %s (%v)", storageCode, err)
	}
	assertUnchangedFile(t, cfg, before)
	at = at.Add(time.Second)
	failedByStorage := m.record(t, Operation{
		Action:    "tool.apply",
		Target:    "opencode",
		Result:    ResultFailed,
		ErrorCode: string(storageCode),
		RequestID: "req_fail_storage",
	})
	// 修掉索引（本用例只关心失败语义，不关心备份历史）：否则后面每条失败都会停在这一步。
	if err := os.Remove(indexPath); err != nil {
		t.Fatalf("清除坏索引失败: %v", err)
	}

	// 失败二：目标路径的父级是普通文件 ⇒ permission_denied（与运行身份无关，root 下同样成立）。
	badPath := filepath.Join(cfg, "child.json")
	badPlan := []family.FilePlan{{Path: badPath, Existed: false, After: []byte("{}\n")}}
	lease := tx.NewCache(time.Minute, nil).Put(&tx.Lease{Operation: "apply", ToolID: "opencode", Files: badPlan})
	if _, err := tx.Apply(lease, tx.Options{WriteMode: 0o600, Backuper: m.reg, Reason: backup.ReasonApply}); err == nil {
		t.Fatal("不可达路径不应写入成功")
	} else if got := apierr.From(err).Code; got != apierr.CodePermissionDenied {
		t.Fatalf("不可达路径应按 permission_denied 报出: %s (%v)", got, err)
	} else {
		at = at.Add(time.Second)
		failedByPermission := m.record(t, Operation{
			Action:    "tool.apply",
			Target:    "opencode",
			Result:    ResultFailed,
			ErrorCode: string(got),
			RequestID: "req_fail_permission",
		})
		if failedByPermission.Result != ResultFailed {
			t.Fatalf("失败必须记为失败: %+v", failedByPermission)
		}
	}
	assertUnchangedFile(t, cfg, before)

	// 失败三：写入阶段失败（已留备份、按文件回滚）。前提是目录不可写；
	// 以 root 运行时该前提不成立，此时只记录事实并继续（不是跳过：本用例的核心断言在前两条）。
	roDir := filepath.Join(dir, "ro")
	if err := os.MkdirAll(roDir, 0o500); err != nil {
		t.Fatalf("建只读目录失败: %v", err)
	}
	if probeWritable(roDir) {
		t.Logf("当前身份可写只读目录（可能是 root），本分支无法构造写入阶段失败")
	} else {
		blocked := filepath.Join(roDir, "blocked.json")
		plans, err := tx.BuildPlans(map[string][]byte{blocked: []byte("{}\n")})
		if err != nil {
			t.Fatalf("构造计划失败: %v", err)
		}
		lease := tx.NewCache(time.Minute, nil).Put(&tx.Lease{Operation: "apply", ToolID: "opencode", Files: plans})
		res, err := tx.Apply(lease, tx.Options{WriteMode: 0o600, Backuper: m.reg, Reason: backup.ReasonApply})
		if err == nil {
			t.Fatal("不可写目录不应写入成功")
		}
		if got := apierr.From(err).Code; got != apierr.CodeConfigInvalid {
			t.Fatalf("写入失败应按 config_invalid 报出: %s (%v)", got, err)
		}
		if len(res.BackupIDs) != 1 {
			t.Fatalf("失败事务也应留下写前备份: %+v", res)
		}
		at = at.Add(time.Second)
		rolled := m.record(t, Operation{
			Action:    "tool.apply",
			Target:    "opencode",
			Result:    ResultFailed,
			ErrorCode: string(apierr.CodeConfigInvalid),
			BackupID:  res.BackupIDs[0],
			RequestID: "req_fail_rollback",
		})
		if rolled.Result != ResultFailed || rolled.BackupID == "" {
			t.Fatalf("回滚失败也须记为失败并带上备份编号: %+v", rolled)
		}
		if _, err := os.Stat(blocked); !os.IsNotExist(err) {
			t.Fatalf("失败事务不应留下目标文件: %v", err)
		}
	}

	// 盘上：成功一条、失败若干条；失败都带错误码，绝不记为成功。
	page, err := m.log.Query(Filter{})
	if err != nil {
		t.Fatalf("查询失败: %v", err)
	}
	failed := 0
	for _, item := range page.Items {
		if item.Result == ResultOK {
			if item.ErrorCode != "" {
				t.Fatalf("成功记录带错误码: %+v", item)
			}
			continue
		}
		if item.Result != ResultFailed {
			t.Fatalf("结果取值超出封闭集合: %+v", item)
		}
		if item.ErrorCode == "" {
			t.Fatalf("失败记录缺少错误码: %+v", item)
		}
		failed++
	}
	if failed < 2 {
		t.Fatalf("失败记录条数不符: %d (%+v)", failed, page.Items)
	}
	if got, err := m.log.Get(failedByStorage.ID); err != nil || got.Result != ResultFailed ||
		got.ErrorCode != string(storageCode) {
		t.Fatalf("持久化的失败记录不符: %+v err=%v", got, err)
	}
	if !strings.Contains(strings.Join(rawLines(t, m.env), "\n"), `"result":"failed"`) {
		t.Fatal("盘上没有失败记录")
	}

	// 反向护栏：把失败写成成功必须被拒，且零字节落盘。
	beforeLines := rawLines(t, m.env)
	if _, err := m.log.Append(Operation{
		Action:    "tool.apply",
		Target:    "opencode",
		Result:    ResultOK,
		ErrorCode: string(apierr.CodeConfigInvalid),
	}); err == nil {
		t.Fatal("成功的记录不得带错误码")
	} else {
		assertCode(t, err, apierr.CodeInvalidRequest)
	}
	if _, err := m.log.Append(Operation{Action: "tool.apply", Result: "已成功"}); err == nil {
		t.Fatal("非法 result 取值应被拒")
	} else {
		assertCode(t, err, apierr.CodeInvalidRequest)
	}
	if strings.Join(beforeLines, "\n") != strings.Join(rawLines(t, m.env), "\n") {
		t.Fatalf("被拒的追加不应改动日志")
	}
}

// probeWritable 探测目录当前身份是否可写（用于「只读目录」这类依赖运行身份的前提）。
func probeWritable(dir string) bool {
	f, err := os.CreateTemp(dir, ".probe-*")
	if err != nil {
		return false
	}
	name := f.Name()
	_ = f.Close()
	_ = os.Remove(name)
	return true
}

func assertUnchangedFile(t *testing.T, path string, want []byte) {
	t.Helper()
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("读文件失败: %v", err)
	}
	if string(got) != string(want) {
		t.Fatalf("失败路径不应改动目标文件: got=%q want=%q", got, want)
	}
}

// ── 令牌与配置正文不入日志 ──────────────────────────────────────────────────

// TestOperationLogCarriesNoTokenOrConfigBody 对应 UC-008 A9 / UC-014 A11 在日志面上的要求：
// 令牌明文与配置正文没有可落地的字段——不是靠调用方自觉，而是 Append 的输入校验。
func TestOperationLogCarriesNoTokenOrConfigBody(t *testing.T) {
	at := testNow
	m := newMutation(t, &at)

	adminToken := strings.Repeat("ab12cd34", 8) // 32 字节管理令牌的 hex 形态
	apiKey := "sk-" + strings.Repeat("A1b2c3", 12)
	configBody := `{"apiKey":"sk-live-0001","model":"deepseek-flash"}`

	bad := []struct {
		name string
		op   Operation
	}{
		{"令牌进 errorCode", Operation{Action: "tool.apply", Result: ResultFailed, ErrorCode: adminToken}},
		{"令牌进 action", Operation{Action: adminToken, Result: ResultOK}},
		{"令牌进 target", Operation{Action: "tool.apply", Target: adminToken, Result: ResultOK}},
		{"令牌进 backupId", Operation{Action: "tool.apply", Result: ResultOK, BackupID: adminToken}},
		{"令牌进 requestId", Operation{Action: "tool.apply", Result: ResultOK, RequestID: apiKey}},
		{"配置正文进 target", Operation{Action: "tool.apply", Target: configBody, Result: ResultOK}},
		{"配置正文进 action", Operation{Action: configBody, Result: ResultOK}},
		{"带空白的自由文本", Operation{Action: "tool.apply", Target: "claude settings.json", Result: ResultOK}},
	}
	for _, tc := range bad {
		if _, err := m.log.Append(tc.op); err == nil {
			t.Fatalf("%s：应被拒绝", tc.name)
		} else {
			assertCode(t, err, apierr.CodeInvalidRequest)
		}
	}
	if lines := rawLines(t, m.env); len(lines) != 0 {
		t.Fatalf("被拒的记录不应落盘: %v", lines)
	}

	// 正常记录落盘后，盘上也不得出现令牌或配置正文的任何形态。
	at = at.Add(time.Second)
	ok := m.record(t, Operation{
		Action:    "tool.apply",
		Target:    "claude",
		Result:    ResultOK,
		BackupID:  "bk-1758541234567890123-1",
		RequestID: "req_01HZ3Q0000000000000000000A",
	})
	if ok.Result != ResultOK || ok.ErrorCode != "" {
		t.Fatalf("正常记录不符: %+v", ok)
	}
	raw, err := os.ReadFile(m.env.OperationsFile())
	if err != nil {
		t.Fatalf("读日志失败: %v", err)
	}
	for _, shape := range []struct {
		name string
		re   *regexp.Regexp
	}{
		{"十六进制令牌", regexp.MustCompile(`[0-9a-fA-F]{40,}`)},
		{"provider 密钥", regexp.MustCompile(`sk-[A-Za-z0-9_-]{16,}`)},
		{"配置正文", regexp.MustCompile(`apiKey`)},
	} {
		if loc := shape.re.FindIndex(raw); loc != nil {
			t.Fatalf("日志里出现了 %s 形态的内容: %q", shape.name, raw[loc[0]:loc[1]])
		}
	}
	if strings.Contains(string(raw), adminToken) || strings.Contains(string(raw), apiKey) {
		t.Fatalf("日志里出现了令牌明文: %s", raw)
	}
}

// ── 权限与并发 ──────────────────────────────────────────────────────────────

// TestOperationLogFileIsMode0600：日志文件 0600，且既有的宽松权限被收紧。
func TestOperationLogFileIsMode0600(t *testing.T) {
	at := testNow
	m := newMutation(t, &at)
	if err := os.MkdirAll(m.env.DataRoot(), 0o700); err != nil {
		t.Fatalf("建目录失败: %v", err)
	}
	if err := os.WriteFile(m.env.OperationsFile(), nil, 0o644); err != nil {
		t.Fatalf("预建日志失败: %v", err)
	}
	if err := os.Chmod(m.env.OperationsFile(), 0o644); err != nil {
		t.Fatalf("chmod 失败: %v", err)
	}
	m.record(t, Operation{Action: "tool.apply", Target: "claude", Result: ResultOK})

	info, err := os.Stat(m.env.OperationsFile())
	if err != nil {
		t.Fatalf("取文件信息失败: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Fatalf("日志文件权限应为 0600: %o", perm)
	}
	dirInfo, err := os.Stat(filepath.Dir(m.env.OperationsFile()))
	if err != nil {
		t.Fatalf("取目录信息失败: %v", err)
	}
	if perm := dirInfo.Mode().Perm(); perm&0o077 != 0 {
		t.Fatalf("数据根目录对其他用户可见: %o", perm)
	}
}

// TestConcurrentAppendsDoNotInterleave：并发追加不丢不重、不交错成半行。
func TestConcurrentAppendsDoNotInterleave(t *testing.T) {
	at := testNow
	m := newMutation(t, &at)
	const n = 64

	var wg sync.WaitGroup
	seen := make([]string, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			op, err := m.log.Append(Operation{
				Action:    "tool.apply",
				Target:    "claude",
				Result:    ResultOK,
				RequestID: "req_" + strings.Repeat("x", i%7) + string(rune('a'+i%26)),
			})
			if err != nil {
				return
			}
			seen[i] = op.ID
		}(i)
	}
	wg.Wait()

	lines := rawLines(t, m.env)
	if len(lines) != n {
		t.Fatalf("并发追加丢行: got=%d want=%d", len(lines), n)
	}
	unique := map[string]bool{}
	for _, line := range lines {
		var op Operation
		if err := json.Unmarshal([]byte(line), &op); err != nil {
			t.Fatalf("有半行或坏行: %q (%v)", line, err)
		}
		if op.ID == "" || op.Action != "tool.apply" {
			t.Fatalf("行内容不完整: %q", line)
		}
		unique[op.ID] = true
	}
	if len(unique) != n {
		t.Fatalf("并发追加出现重复或丢失: %d 个不同 id", len(unique))
	}
	for _, id := range seen {
		if id != "" && !unique[id] {
			t.Fatalf("返回的 id 未落盘: %s", id)
		}
	}
	page, err := m.log.Query(Filter{Limit: MaxLimit})
	if err != nil {
		t.Fatalf("查询失败: %v", err)
	}
	if page.Total != n || len(page.Items) != n {
		t.Fatalf("查询条数不符: total=%d items=%d", page.Total, len(page.Items))
	}
}

// ── 过滤与分页 ──────────────────────────────────────────────────────────────

// TestQueryFiltersAndPaginates：过滤组合、闭开时间窗、游标翻页不重不漏、total 是匹配总数。
func TestQueryFiltersAndPaginates(t *testing.T) {
	at := testNow
	m := newMutation(t, &at)

	type seeded struct {
		op Operation
	}
	var all []seeded
	// 三组动作 / 目标 / 结果，各两条，分布在三个时刻上。
	for i := 0; i < 6; i++ {
		if i%2 == 0 {
			at = at.Add(time.Second)
		}
		op := Operation{
			Action: []string{"tool.apply", "tool.restore", "workbuddy.reset"}[i%3],
			Target: []string{"claude", "gemini", "workbuddy"}[i%3],
			Result: ResultFailed,
		}
		if i%2 == 0 {
			op.Result = ResultOK
		} else {
			op.ErrorCode = string(apierr.CodeConfigInvalid)
		}
		all = append(all, seeded{op: m.record(t, op)})
	}

	// 时间窗 [from, to)：起点含、终点不含。
	// 六条记录落在三个时刻上（各两条），故 [t1, t3) 命中 t1、t2 共四条。
	from := all[0].op.At
	to := all[len(all)-1].op.At
	page, err := m.log.Query(Filter{From: from, To: to})
	if err != nil {
		t.Fatalf("窗口查询失败: %v", err)
	}
	if page.Total != 4 {
		t.Fatalf("闭开窗口命中数不符: %d", page.Total)
	}
	for _, item := range page.Items {
		if !item.At.Before(to) {
			t.Fatalf("窗口应排除终点时刻: %v", item.At)
		}
		if item.At.Before(from) {
			t.Fatalf("窗口应包含起点时刻: %v", item.At)
		}
	}
	if p, err := m.log.Query(Filter{From: to}); err != nil || p.Total != 2 {
		t.Fatalf("起点=终点时刻应命中该时刻的两条: total=%d err=%v", p.Total, err)
	}
	if p, err := m.log.Query(Filter{From: to, To: from}); err == nil {
		t.Fatalf("起点晚于终点应被拒: %+v", p)
	} else {
		assertCode(t, err, apierr.CodeInvalidRequest)
	}

	// 组合过滤：action + result。
	combined, err := m.log.Query(Filter{Action: "tool.apply", Result: ResultOK})
	if err != nil {
		t.Fatalf("组合过滤失败: %v", err)
	}
	if combined.Total != 1 {
		t.Fatalf("组合过滤命中数不符: %d", combined.Total)
	}
	if combined.Items[0].ErrorCode != "" {
		t.Fatalf("成功记录不应带错误码: %+v", combined.Items[0])
	}
	// 失败过滤只出失败，且都带码。
	failed, err := m.log.Query(Filter{Result: ResultFailed})
	if err != nil {
		t.Fatalf("失败过滤出错: %v", err)
	}
	if failed.Total != 3 {
		t.Fatalf("失败命中数不符: %d", failed.Total)
	}
	for _, item := range failed.Items {
		if item.Result != ResultFailed || item.ErrorCode == "" {
			t.Fatalf("失败记录形状不符: %+v", item)
		}
	}
	// 形态合法但无人命中的过滤，返回空页而不是错误。
	if p, err := m.log.Query(Filter{Action: "nope"}); err != nil || p.Total != 0 || len(p.Items) != 0 {
		t.Fatalf("无关过滤应为空页: %+v err=%v", p, err)
	}
	// 形态非法的过滤一律 invalid_request。
	for _, f := range []Filter{
		{Action: "NOPE"},
		{Target: "bad target"},
		{Result: "okay"},
		{Limit: -1},
		{Limit: MaxLimit + 1},
	} {
		if _, err := m.log.Query(f); err == nil {
			t.Fatalf("非法过滤 %+v 应被拒", f)
		} else {
			assertCode(t, err, apierr.CodeInvalidRequest)
		}
	}

	// 游标翻页：并集 == 全集，无重复，total 恒定。
	seen := map[string]int{}
	cursor := ""
	pages := 0
	for {
		p, err := m.log.Query(Filter{Limit: 2, Cursor: cursor})
		if err != nil {
			t.Fatalf("翻页失败: %v", err)
		}
		if p.Total != len(all) {
			t.Fatalf("total 应恒为匹配总数: %d", p.Total)
		}
		for _, item := range p.Items {
			seen[item.ID]++
		}
		pages++
		if p.NextCursor == "" {
			break
		}
		cursor = p.NextCursor
	}
	if pages != 3 {
		t.Fatalf("页数不符: %d", pages)
	}
	if len(seen) != len(all) {
		t.Fatalf("翻页并集不等于全集: %v", seen)
	}
	for id, n := range seen {
		if n != 1 {
			t.Fatalf("翻页重复: %s x%d", id, n)
		}
	}
	if _, err := m.log.Query(Filter{Cursor: "c1:!!!"}); err == nil {
		t.Fatal("非法游标应被拒")
	} else {
		assertCode(t, err, apierr.CodeInvalidRequest)
	}
	// 过滤 + 翻页：只数匹配项。
	p, err := m.log.Query(Filter{Result: ResultFailed, Limit: 1})
	if err != nil {
		t.Fatalf("过滤翻页失败: %v", err)
	}
	if p.Total != 3 || len(p.Items) != 1 || p.NextCursor == "" {
		t.Fatalf("过滤翻页形状不符: %+v", p)
	}
}

// TestQueryReportsTornLines：崩溃留下的半行被跳过，但计数必须回给调用方（不静默吞掉审计行）。
func TestQueryReportsTornLines(t *testing.T) {
	at := testNow
	m := newMutation(t, &at)
	good := m.record(t, Operation{Action: "tool.apply", Target: "claude", Result: ResultOK})

	f, err := os.OpenFile(m.env.OperationsFile(), os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatalf("打开日志失败: %v", err)
	}
	if _, err := f.WriteString("{\"id\":\"op_1"); err != nil {
		t.Fatalf("写半行失败: %v", err)
	}
	if err := f.Close(); err != nil {
		t.Fatalf("关闭失败: %v", err)
	}

	page, err := m.log.Query(Filter{})
	if err != nil {
		t.Fatalf("查询不应因半行失败: %v", err)
	}
	if page.Total != 1 || len(page.Items) != 1 || page.Items[0].ID != good.ID {
		t.Fatalf("可解析的记录应照常返回: %+v", page)
	}
	if page.Skipped != 1 {
		t.Fatalf("损坏行计数不符: %d", page.Skipped)
	}
	if _, err := m.log.Get(good.ID); err != nil {
		t.Fatalf("半行不应影响按 id 回读: %v", err)
	}
}
