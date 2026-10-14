package backup

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/peterplaykkk/yzagmgr/internal/apierr"
	"github.com/peterplaykkk/yzagmgr/internal/config"
	"github.com/peterplaykkk/yzagmgr/internal/family"
	"github.com/peterplaykkk/yzagmgr/internal/tx"
)

// ── 夹具 ────────────────────────────────────────────────────────────────────

// testNow 是固定时钟，让「同秒多条记录」的排序与淘汰在测试里完全确定。
var testNow = time.Date(2026, 9, 22, 18, 0, 0, 0, time.FixedZone("CST", 8*3600))

// testEnv 建隔离环境：数据根落在 t.TempDir() 下，绝不碰用户真实数据根（UC-013 A1）。
func testEnv(t *testing.T) *config.Env {
	t.Helper()
	return &config.Env{Home: t.TempDir(), Overrides: map[string]string{}, Isolated: true}
}

func testClock() func() time.Time { return func() time.Time { return testNow } }

// fixedPaths 给出确定性白名单；whitelist 用切片变量以便测试中途改口径（模拟家族路径变化）。
func fixedPaths(paths ...string) FamilyPaths {
	return func(family.ID) ([]string, error) { return paths, nil }
}

func mustWriteFile(t *testing.T, path string, data []byte) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatalf("建目录失败 %s: %v", filepath.Dir(path), err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatalf("写文件失败 %s: %v", path, err)
	}
}

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

func assertBytes(t *testing.T, path string, want []byte) {
	t.Helper()
	if got := readOrNil(t, path); !bytes.Equal(got, want) {
		t.Fatalf("盘上字节不符: %s\n got=%q\nwant=%q", path, got, want)
	}
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

// toolDir 返回某工具的备份目录（测试要直接看盘上结构）。
func toolDir(env *config.Env, toolID string) string {
	return filepath.Join(env.BackupsDir(), toolID)
}

// applyThroughTx 走真实写链：构造计划（读盘上写前字节）→ 一次性租约 → tx.Apply。
func applyThroughTx(t *testing.T, reg *Registry, toolID string, targets map[string][]byte, reason string) (tx.Result, error) {
	t.Helper()
	plans, err := tx.BuildPlans(targets)
	if err != nil {
		t.Fatalf("构造写前计划失败: %v", err)
	}
	return runTx(t, reg, toolID, plans, "apply", reason)
}

// restoreThroughTx 走真实恢复链：PlanRestoreDetailed → 租约 → tx.Apply。
func restoreThroughTx(t *testing.T, reg *Registry, toolID, backupID string) tx.Result {
	t.Helper()
	plan, err := reg.PlanRestoreDetailed(toolID, backupID)
	if err != nil {
		t.Fatalf("展开恢复计划失败: %v", err)
	}
	if len(plan.Files) == 0 {
		return tx.Result{Status: tx.StatusUnchanged, Files: []tx.FileResult{}}
	}
	res, err := runTx(t, reg, toolID, plan.Files, "restore", ReasonRestore)
	if err != nil {
		t.Fatalf("恢复写盘失败: %v", err)
	}
	return res
}

// runTx 是 apply/restore 共用的执行器：租约一次性、指纹取写前状态（与产品链路一致）。
func runTx(t *testing.T, reg *Registry, toolID string, plans []family.FilePlan, operation, reason string) (tx.Result, error) {
	t.Helper()
	sorted := append([]family.FilePlan(nil), plans...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Path < sorted[j].Path })
	cache := tx.NewCache(time.Minute, nil)
	lease := cache.Put(&tx.Lease{Operation: operation, ToolID: toolID, Files: sorted})
	return tx.Apply(lease, tx.Options{WriteMode: 0o600, Backuper: reg, Reason: reason})
}

// treeDigest 给一棵目录树按「相对路径 → sha256」打指纹，用于「盘上零变化」断言。
func treeDigest(t *testing.T, root string) map[string]string {
	t.Helper()
	out := map[string]string{}
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		b, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		out[rel] = tx.HashBytes(b)
		return nil
	})
	if err != nil {
		t.Fatalf("遍历 %s 失败: %v", root, err)
	}
	return out
}

func assertDigestEqual(t *testing.T, before, after map[string]string) {
	t.Helper()
	if len(before) != len(after) {
		t.Fatalf("文件集合变了: before=%v after=%v", keys(before), keys(after))
	}
	for k, v := range before {
		if after[k] != v {
			t.Fatalf("文件 %s 的字节变了: before=%s after=%s", k, v, after[k])
		}
	}
}

func keys(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// ── UC-004 A1：初始快照 ─────────────────────────────────────────────────────

// 测试: TestInitialSnapshotCapturesPreTakeoverBytes
func TestInitialSnapshotCapturesPreTakeoverBytes(t *testing.T) {
	env := testEnv(t)
	cfg := filepath.Join(env.Home, "claude", "settings.json")
	official := []byte("{\"tier\":\"official\"}\n")
	gateway := []byte("{\"tier\":\"gateway\"}\n")
	mustWriteFile(t, cfg, official)

	reg := NewRegistry(env, fixedPaths(cfg), testClock())
	res, err := applyThroughTx(t, reg, "claude", map[string][]byte{cfg: gateway}, ReasonApply)
	if err != nil {
		t.Fatalf("apply 失败: %v", err)
	}
	if len(res.BackupIDs) == 0 {
		t.Fatal("apply 未产生备份编号")
	}
	if got := readOrNil(t, cfg); !bytes.Equal(got, gateway) {
		t.Fatalf("apply 后盘上字节不符: %q", got)
	}

	initial, err := reg.Initial("claude")
	if err != nil {
		t.Fatalf("取初始快照失败: %v", err)
	}
	if !initial.Initial || initial.Reason != ReasonInitial {
		t.Fatalf("初始快照标记不符: initial=%v reason=%s", initial.Initial, initial.Reason)
	}
	if len(initial.Files) != 1 || initial.Files[0].Path != cfg {
		t.Fatalf("初始快照的文件集合不符: %+v", initial.Files)
	}
	if initial.Files[0].SHA256 != tx.HashBytes(official) {
		t.Fatalf("初始快照记的不是接管前字节: %s != %s",
			initial.Files[0].SHA256, tx.HashBytes(official))
	}

	// 快照可经恢复还原到接管前的字节。
	restoreThroughTx(t, reg, "claude", initial.BackupID)
	assertBytes(t, cfg, official)

	// 初始快照只落一份：第二次 apply 不会再造一条。
	res2, err := applyThroughTx(t, reg, "claude", map[string][]byte{cfg: gateway}, ReasonApply)
	if err != nil {
		t.Fatalf("第二次 apply 失败: %v", err)
	}
	if len(res2.BackupIDs) != 1 {
		t.Fatalf("第二次 apply 的备份编号数不符: %v", res2.BackupIDs)
	}
	page, err := reg.List("claude", ListOptions{})
	if err != nil {
		t.Fatalf("列举失败: %v", err)
	}
	initials := 0
	for _, b := range page.Items {
		if b.Initial {
			initials++
		}
	}
	if initials != 1 {
		t.Fatalf("初始快照条数不符: %d", initials)
	}
}

// ── UC-004 A2：写前必留备份 ─────────────────────────────────────────────────

// 测试: TestEveryApplyLeavesABackupCoveringAllWrittenFiles
func TestEveryApplyLeavesABackupCoveringAllWrittenFiles(t *testing.T) {
	env := testEnv(t)
	a := filepath.Join(env.Home, "gemini", ".gemini", ".env")
	b := filepath.Join(env.Home, "gemini", ".gemini", "settings.json")
	c := filepath.Join(env.Home, "gemini", ".gemini", "created.json")
	mustWriteFile(t, a, []byte("KEY=official\n"))
	mustWriteFile(t, b, []byte("{\"official\":true}\n"))

	reg := NewRegistry(env, fixedPaths(a, b, c), testClock())
	targets := map[string][]byte{
		a: []byte("KEY=gateway\n"),
		b: []byte("{\"gateway\":true}\n"),
		c: []byte("{\"created\":true}\n"),
	}
	res, err := applyThroughTx(t, reg, "gemini", targets, ReasonApply)
	if err != nil {
		t.Fatalf("apply 失败: %v", err)
	}
	if len(res.BackupIDs) != 1 {
		t.Fatalf("apply 应恰产生一条备份记录: %v", res.BackupIDs)
	}

	rec, err := reg.Get("gemini", res.BackupIDs[0])
	if err != nil {
		t.Fatalf("取备份失败: %v", err)
	}
	if rec.Reason != ReasonApply || rec.Initial {
		t.Fatalf("备份原因标记不符: reason=%s initial=%v", rec.Reason, rec.Initial)
	}
	got := map[string]BackupFile{}
	for _, f := range rec.Files {
		got[f.Path] = f
	}
	if len(got) != len(targets) {
		t.Fatalf("备份未覆盖全部被写路径: got=%v want=%v", keysOf(got), keysOfPaths(targets))
	}
	// 覆盖且逐条记的是写前状态：a/b 有字节，c 是被接管新建的。
	if f := got[a]; !f.Existed || f.SHA256 != tx.HashBytes([]byte("KEY=official\n")) {
		t.Fatalf("a 的写前记录不符: %+v", f)
	}
	if f := got[b]; !f.Existed || f.SHA256 != tx.HashBytes([]byte("{\"official\":true}\n")) {
		t.Fatalf("b 的写前记录不符: %+v", f)
	}
	if f := got[c]; f.Existed || f.SHA256 != tx.EmptyHash || f.SizeBytes != 0 {
		t.Fatalf("c 应记为接管时新建: %+v", f)
	}

	// 第二次 apply 再产生一条（幂等路径也记备份，与 v1 一致）。
	res2, err := applyThroughTx(t, reg, "gemini", targets, ReasonApply)
	if err != nil {
		t.Fatalf("第二次 apply 失败: %v", err)
	}
	if len(res2.BackupIDs) != 1 || res2.BackupIDs[0] == res.BackupIDs[0] {
		t.Fatalf("第二次 apply 的备份编号不符: %v", res2.BackupIDs)
	}
}

func keysOf(m map[string]BackupFile) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func keysOfPaths(m map[string][]byte) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// TestRecordExplicitInitialIsSingle：显式记初始快照时，本次记录就是那份快照，且不可重复。
func TestRecordExplicitInitialIsSingle(t *testing.T) {
	env := testEnv(t)
	cfg := filepath.Join(env.Home, "openclaw", "openclaw.json")
	mustWriteFile(t, cfg, []byte("{\"official\":true}\n"))
	reg := NewRegistry(env, fixedPaths(cfg), testClock())
	files, err := tx.BuildPlans(map[string][]byte{cfg: []byte("{\"gateway\":true}\n")})
	if err != nil {
		t.Fatalf("构造计划失败: %v", err)
	}

	id, err := reg.Record("openclaw", ReasonInitial, files)
	if err != nil {
		t.Fatalf("记初始快照失败: %v", err)
	}
	rec, err := reg.Get("openclaw", id)
	if err != nil {
		t.Fatalf("取备份失败: %v", err)
	}
	if !rec.Initial || rec.Reason != ReasonInitial {
		t.Fatalf("显式初始快照的标记不符: %+v", rec)
	}
	page, err := reg.List("openclaw", ListOptions{})
	if err != nil {
		t.Fatalf("列举失败: %v", err)
	}
	if page.Total != 1 {
		t.Fatalf("显式记初始快照应只产生一条记录: %d", page.Total)
	}
	if _, err := reg.Record("openclaw", ReasonInitial, files); err == nil {
		t.Fatal("重复记初始快照应被拒")
	} else {
		assertCode(t, err, apierr.CodeInvalidRequest)
	}
}

// ── UC-004 A6：有界轮转 ─────────────────────────────────────────────────────

// 测试: TestBackupRotationIsBounded
func TestBackupRotationIsBounded(t *testing.T) {
	env := testEnv(t)
	cfg := filepath.Join(env.Home, "codex", "config.toml")
	mustWriteFile(t, cfg, []byte("tier = \"official\"\n"))

	reg := NewRegistry(env, fixedPaths(cfg), testClock())
	if s, err := reg.Settings(); err != nil || s.Keep != DefaultKeep {
		t.Fatalf("默认保留上限不符: %+v err=%v", s, err)
	}
	if _, err := reg.SetKeep(0); err == nil {
		t.Fatal("keep=0 应被拒")
	} else {
		assertCode(t, err, apierr.CodeInvalidRequest)
	}
	if _, err := reg.SetKeep(MaxKeep + 1); err == nil {
		t.Fatal("keep 超过上限应被拒")
	}
	if s, err := reg.SetKeep(3); err != nil || s.Keep != 3 {
		t.Fatalf("设置保留上限失败: %+v err=%v", s, err)
	}

	// 五次 apply（内容各异，每次一条普通备份），外加首次写入的初始快照。
	var ids []string
	for i := 0; i < 5; i++ {
		target := []byte("tier = \"gateway-" + string(rune('a'+i)) + "\"\n")
		res, err := applyThroughTx(t, reg, "codex", map[string][]byte{cfg: target}, ReasonApply)
		if err != nil {
			t.Fatalf("第 %d 次 apply 失败: %v", i+1, err)
		}
		ids = append(ids, res.BackupIDs[0])
	}

	page, err := reg.List("codex", ListOptions{})
	if err != nil {
		t.Fatalf("列举失败: %v", err)
	}
	var (
		normal  []string
		initial []Backup
	)
	for _, b := range page.Items {
		if b.Initial {
			initial = append(initial, b)
			continue
		}
		normal = append(normal, b.BackupID)
	}
	if len(initial) != 1 {
		t.Fatalf("初始快照应恒为 1 条: %d", len(initial))
	}
	if len(normal) != 3 {
		t.Fatalf("普通备份应保留 3 条: %v", normal)
	}
	want := []string{ids[4], ids[3], ids[2]} // 新 → 旧
	for i := range want {
		if normal[i] != want[i] {
			t.Fatalf("保留的不是最新三条: got=%v want=%v", normal, want)
		}
	}
	// 被淘汰的载荷从盘上消失；保留的与初始快照的载荷都在。
	dir := toolDir(env, "codex")
	for _, evicted := range []string{ids[0], ids[1]} {
		if _, err := os.Stat(payloadPath(dir, evicted)); !os.IsNotExist(err) {
			t.Fatalf("被淘汰的载荷仍在盘上: %s (err=%v)", evicted, err)
		}
	}
	for _, kept := range []string{ids[2], ids[3], ids[4], initial[0].BackupID} {
		if _, err := os.Stat(payloadPath(dir, kept)); err != nil {
			t.Fatalf("保留的载荷缺失: %s (%v)", kept, err)
		}
	}
	// 初始快照可经恢复取回（永不淘汰的那一条）。
	restoreThroughTx(t, reg, "codex", initial[0].BackupID)
	assertBytes(t, cfg, []byte("tier = \"official\"\n"))
}

// TestKeepChangeRotatesExistingBackups：调小上限后立即对既有备份生效（不必等到下次 apply）。
func TestKeepChangeRotatesExistingBackups(t *testing.T) {
	env := testEnv(t)
	cfg := filepath.Join(env.Home, "pi", "models.json")
	mustWriteFile(t, cfg, []byte("[]\n"))
	reg := NewRegistry(env, fixedPaths(cfg), testClock())

	for i := 0; i < 4; i++ {
		if _, err := applyThroughTx(t, reg, "pi",
			map[string][]byte{cfg: []byte("[\"m" + string(rune('a'+i)) + "\"]\n")}, ReasonApply); err != nil {
			t.Fatalf("apply 失败: %v", err)
		}
	}
	if _, err := reg.SetKeep(2); err != nil {
		t.Fatalf("设置保留上限失败: %v", err)
	}
	page, err := reg.List("pi", ListOptions{})
	if err != nil {
		t.Fatalf("列举失败: %v", err)
	}
	normal := 0
	for _, b := range page.Items {
		if !b.Initial {
			normal++
		}
	}
	if normal != 2 {
		t.Fatalf("调小上限后应立即只剩 2 条普通备份: %d", normal)
	}
	dir := toolDir(env, "pi")
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("读目录失败: %v", err)
	}
	baks := 0
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), payloadSuffix) {
			baks++
		}
	}
	if baks != 3 { // 2 条普通 + 1 条初始
		t.Fatalf("盘上载荷条数不符: %d", baks)
	}
}

// ── UC-004 A7：可列举可定位 ─────────────────────────────────────────────────

// 测试: TestBackupsAreListableAndAddressable
func TestBackupsAreListableAndAddressable(t *testing.T) {
	env := testEnv(t)
	cfg := filepath.Join(env.Home, "hermes", "config.yaml")
	mustWriteFile(t, cfg, []byte("tier: official\n"))

	reg := NewRegistry(env, fixedPaths(cfg), testClock())
	var states [][]byte
	for i := 0; i < 3; i++ {
		states = append(states, []byte("tier: gateway-"+string(rune('a'+i))+"\n"))
		if _, err := applyThroughTx(t, reg, "hermes", map[string][]byte{cfg: states[i]}, ReasonApply); err != nil {
			t.Fatalf("第 %d 次 apply 失败: %v", i+1, err)
		}
	}

	first, err := reg.List("hermes", ListOptions{})
	if err != nil {
		t.Fatalf("列举失败: %v", err)
	}
	second, err := reg.List("hermes", ListOptions{})
	if err != nil {
		t.Fatalf("二次列举失败: %v", err)
	}
	if first.Total != 4 { // 3 条 apply + 1 条 initial
		t.Fatalf("总数不符: %d", first.Total)
	}
	if len(first.Items) != len(second.Items) {
		t.Fatalf("两次列举条数不同: %d vs %d", len(first.Items), len(second.Items))
	}
	for i := range first.Items {
		if first.Items[i].BackupID != second.Items[i].BackupID {
			t.Fatalf("备份编号不稳定: %s vs %s", first.Items[i].BackupID, second.Items[i].BackupID)
		}
	}

	// 每条都能按编号取回摘要与字节；最新那条普通备份记的是上一次的盘上状态。
	for _, item := range first.Items {
		if !safeBackupID(item.BackupID) {
			t.Fatalf("备份编号形态不符: %q", item.BackupID)
		}
		got, err := reg.Get("hermes", item.BackupID)
		if err != nil {
			t.Fatalf("按编号取备份失败 %s: %v", item.BackupID, err)
		}
		if got.BackupID != item.BackupID || !got.At.Equal(item.At) {
			t.Fatalf("取回的记录与列举不符: %+v vs %+v", got, item)
		}
		payload, err := reg.ReadPayload("hermes", item.BackupID)
		if err != nil {
			t.Fatalf("取字节失败 %s: %v", item.BackupID, err)
		}
		if len(payload.Backup.Files) != 1 {
			t.Fatalf("载荷文件数不符: %+v", payload.Backup.Files)
		}
		sum := payload.Backup.Files[0]
		if sum.Existed && tx.HashBytes(payload.Bytes[sum.Path]) != sum.SHA256 {
			t.Fatalf("载荷字节与摘要不符: %s", item.BackupID)
		}
	}
	newest := first.Items[0]
	payload, err := reg.ReadPayload("hermes", newest.BackupID)
	if err != nil {
		t.Fatalf("取字节失败: %v", err)
	}
	if !bytes.Equal(payload.Bytes[cfg], states[1]) {
		t.Fatalf("最新备份应记上一次的盘上状态: got=%q want=%q", payload.Bytes[cfg], states[1])
	}

	// 契约形状：Backup 逐字取 §4.3 的键集，at 是 RFC3339。
	raw, err := json.Marshal(newest)
	if err != nil {
		t.Fatalf("序列化失败: %v", err)
	}
	var shape map[string]json.RawMessage
	if err := json.Unmarshal(raw, &shape); err != nil {
		t.Fatalf("反序列化失败: %v", err)
	}
	assertKeySet(t, "Backup", shape, []string{"backupId", "at", "reason", "initial", "files"})
	var at string
	if err := json.Unmarshal(shape["at"], &at); err != nil {
		t.Fatalf("at 不是字符串: %v", err)
	}
	if _, err := time.Parse(time.RFC3339, at); err != nil {
		t.Fatalf("at 不是 RFC3339: %q (%v)", at, err)
	}
	var fileShape []map[string]json.RawMessage
	if err := json.Unmarshal(shape["files"], &fileShape); err != nil {
		t.Fatalf("files 不是数组: %v", err)
	}
	if len(fileShape) != 1 {
		t.Fatalf("files 条数不符: %d", len(fileShape))
	}
	assertKeySet(t, "Backup.files[]", fileShape[0], []string{"path", "existed", "sha256", "sizeBytes"})
}

func assertKeySet(t *testing.T, what string, obj map[string]json.RawMessage, want []string) {
	t.Helper()
	var got []string
	for k := range obj {
		got = append(got, k)
	}
	sort.Strings(got)
	sorted := append([]string(nil), want...)
	sort.Strings(sorted)
	if strings.Join(got, ",") != strings.Join(sorted, ",") {
		t.Fatalf("%s 的键集不符: got=%v want=%v", what, got, sorted)
	}
}

// TestListCursorPagesWithoutDuplicatesOrGaps：游标翻页不重不漏，total 恒为匹配总数。
func TestListCursorPagesWithoutDuplicatesOrGaps(t *testing.T) {
	env := testEnv(t)
	cfg := filepath.Join(env.Home, "grokbuild", "config.toml")
	mustWriteFile(t, cfg, []byte("tier = \"official\"\n"))
	reg := NewRegistry(env, fixedPaths(cfg), testClock())
	for i := 0; i < 6; i++ {
		if _, err := applyThroughTx(t, reg, "grokbuild",
			map[string][]byte{cfg: []byte("tier = \"g" + string(rune('a'+i)) + "\"\n")}, ReasonApply); err != nil {
			t.Fatalf("apply 失败: %v", err)
		}
	}

	seen := map[string]int{}
	cursor := ""
	pages := 0
	for {
		page, err := reg.List("grokbuild", ListOptions{Limit: 2, Cursor: cursor})
		if err != nil {
			t.Fatalf("列举失败: %v", err)
		}
		if page.Total != 7 {
			t.Fatalf("total 应恒为匹配总数 7: %d", page.Total)
		}
		for _, item := range page.Items {
			seen[item.BackupID]++
		}
		pages++
		if page.NextCursor == "" {
			break
		}
		cursor = page.NextCursor
	}
	if pages != 4 { // 7 条按每页 2 条 = 4 页
		t.Fatalf("页数不符: %d", pages)
	}
	if len(seen) != 7 {
		t.Fatalf("翻页并集不等于全集: %v", seen)
	}
	for id, n := range seen {
		if n != 1 {
			t.Fatalf("翻页出现重复: %s x%d", id, n)
		}
	}
	if _, err := reg.List("grokbuild", ListOptions{Cursor: "c1:%%%"}); err == nil {
		t.Fatal("非法游标应被拒")
	} else {
		assertCode(t, err, apierr.CodeInvalidRequest)
	}
	if _, err := reg.List("grokbuild", ListOptions{Limit: MaxListLimit + 1}); err == nil {
		t.Fatal("limit 超上限应被拒")
	} else {
		assertCode(t, err, apierr.CodeInvalidRequest)
	}
}

// ── UC-004 A10：备份文件权限 ────────────────────────────────────────────────

// 测试: TestBackupFilesArePrivateMode0600
func TestBackupFilesArePrivateMode0600(t *testing.T) {
	env := testEnv(t)
	cfg := filepath.Join(env.Home, "opencode", "opencode.json")
	mustWriteFile(t, cfg, []byte("{\"tier\":\"official\"}\n"))
	reg := NewRegistry(env, fixedPaths(cfg), testClock())
	res, err := applyThroughTx(t, reg, "opencode", map[string][]byte{cfg: []byte("{\"tier\":\"gateway\"}\n")}, ReasonApply)
	if err != nil {
		t.Fatalf("apply 失败: %v", err)
	}
	if _, err := reg.SetKeep(7); err != nil {
		t.Fatalf("设置保留上限失败: %v", err)
	}

	dir := toolDir(env, "opencode")
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("读目录失败: %v", err)
	}
	var baks, indexes int
	for _, e := range entries {
		info, err := e.Info()
		if err != nil {
			t.Fatalf("取文件信息失败: %v", err)
		}
		if perm := info.Mode().Perm(); perm != 0o600 {
			t.Fatalf("备份目录内的文件权限应为 0600: %s = %o", e.Name(), perm)
		}
		switch {
		case e.Name() == indexFileName:
			indexes++
		case strings.HasSuffix(e.Name(), payloadSuffix):
			baks++
		}
	}
	if baks != 2 || indexes != 1 { // 初始快照 + 本次 apply + 索引
		t.Fatalf("备份目录内容不符: baks=%d indexes=%d (backupId=%s)", baks, indexes, res.BackupIDs[0])
	}

	// 父目录不可被其他用户列举：group/other 位必须全为 0。
	for _, d := range []string{env.BackupsDir(), dir} {
		info, err := os.Stat(d)
		if err != nil {
			t.Fatalf("取目录信息失败 %s: %v", d, err)
		}
		if perm := info.Mode().Perm(); perm&0o077 != 0 {
			t.Fatalf("目录 %s 对其他用户可见: %o", d, perm)
		}
	}
	// 保留上限文件同样 0600。
	info, err := os.Stat(filepath.Join(env.BackupsDir(), settingsName))
	if err != nil {
		t.Fatalf("取设置文件信息失败: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Fatalf("备份设置权限应为 0600: %o", perm)
	}
}

// TestBackupDirPermissionsAreTightened：既有的宽松权限在写入时被收紧（而不是被沿用）。
func TestBackupDirPermissionsAreTightened(t *testing.T) {
	env := testEnv(t)
	cfg := filepath.Join(env.Home, "claude-desktop", "claude_desktop_config.json")
	mustWriteFile(t, cfg, []byte("{}\n"))
	if err := os.MkdirAll(toolDir(env, "claude-desktop"), 0o755); err != nil {
		t.Fatalf("预建目录失败: %v", err)
	}
	if err := os.Chmod(toolDir(env, "claude-desktop"), 0o755); err != nil {
		t.Fatalf("chmod 失败: %v", err)
	}
	reg := NewRegistry(env, fixedPaths(cfg), testClock())
	if _, err := applyThroughTx(t, reg, "claude-desktop", map[string][]byte{cfg: []byte("{\"a\":1}\n")}, ReasonApply); err != nil {
		t.Fatalf("apply 失败: %v", err)
	}
	info, err := os.Stat(toolDir(env, "claude-desktop"))
	if err != nil {
		t.Fatalf("取目录信息失败: %v", err)
	}
	if perm := info.Mode().Perm(); perm&0o077 != 0 {
		t.Fatalf("宽松目录权限未被收紧: %o", perm)
	}
}

// ── 护栏与失败路径 ──────────────────────────────────────────────────────────

// TestRecordRejectsUnsafeToolID：工具 id 会拼进目录名，'/'、'..'、大写必须在进 IO 前被拒。
func TestRecordRejectsUnsafeToolID(t *testing.T) {
	env := testEnv(t)
	reg := NewRegistry(env, fixedPaths(), testClock())
	cfg := filepath.Join(env.Home, "x.json")
	mustWriteFile(t, cfg, []byte("x"))
	files, err := tx.BuildPlans(map[string][]byte{cfg: []byte("y")})
	if err != nil {
		t.Fatalf("构造计划失败: %v", err)
	}
	for _, id := range []string{"", "..", "a/b", "../claude", "Claude", "claude/../..", strings.Repeat("a", 65)} {
		if _, err := reg.Record(id, ReasonApply, files); err == nil {
			t.Fatalf("工具 id %q 应被拒", id)
		} else {
			assertCode(t, err, apierr.CodeInvalidRequest)
		}
	}
	if _, err := os.Stat(env.BackupsDir()); !os.IsNotExist(err) {
		t.Fatalf("被拒的调用不应在盘上建出备份目录: %v", err)
	}
}

// TestRecordRejectsInconsistentFilePlan：existed=false 却带字节的计划自相矛盾（会让删除语义失真）。
func TestRecordRejectsInconsistentFilePlan(t *testing.T) {
	env := testEnv(t)
	reg := NewRegistry(env, fixedPaths(), testClock())
	bad := []family.FilePlan{{
		Path:    filepath.Join(env.Home, "a.json"),
		Existed: false,
		Before:  []byte("{\"k\":\"v\"}"),
		After:   []byte("{}"),
	}}
	if _, err := reg.Record("claude", ReasonApply, bad); err == nil {
		t.Fatal("自相矛盾的计划应被拒")
	} else {
		assertCode(t, err, apierr.CodeInvalidRequest)
	}
}

// TestUnknownBackupAndToolAreAddressable：未知编号 → not_found；非法编号 → invalid_request；
// 未知工具列举为空页而不是错误。
func TestUnknownBackupAndToolAreAddressable(t *testing.T) {
	env := testEnv(t)
	cfg := filepath.Join(env.Home, "claude", "settings.json")
	mustWriteFile(t, cfg, []byte("{}\n"))
	reg := NewRegistry(env, fixedPaths(cfg), testClock())
	res, err := applyThroughTx(t, reg, "claude", map[string][]byte{cfg: []byte("{\"a\":1}\n")}, ReasonApply)
	if err != nil {
		t.Fatalf("apply 失败: %v", err)
	}
	_ = res

	if _, err := reg.Get("claude", "bk-1-1"); err == nil {
		t.Fatal("未知备份编号应报错")
	} else {
		assertCode(t, err, apierr.CodeNotFound)
	}
	for _, bad := range []string{"", "../etc/passwd", "bk-../../x", "nope", "bk-1-1/../2"} {
		if _, err := reg.Get("claude", bad); err == nil {
			t.Fatalf("非法备份编号 %q 应被拒", bad)
		} else {
			assertCode(t, err, apierr.CodeInvalidRequest)
		}
	}
	if _, err := reg.Get("codex", "bk-1-1"); err == nil {
		t.Fatal("无备份的工具应报 not_found")
	} else {
		assertCode(t, err, apierr.CodeNotFound)
	}
	page, err := reg.List("codex", ListOptions{})
	if err != nil {
		t.Fatalf("未备份工具的列举不应报错: %v", err)
	}
	if page.Total != 0 || len(page.Items) != 0 || page.NextCursor != "" {
		t.Fatalf("未备份工具的列举应为空页: %+v", page)
	}
	if _, err := reg.Initial("codex"); err == nil {
		t.Fatal("无初始快照应报 not_found")
	} else {
		assertCode(t, err, apierr.CodeNotFound)
	}
}

// TestRecordRefusesToWriteOnCorruptIndex：索引损坏时宁可拦住写入（无备份即不写）。
func TestRecordRefusesToWriteOnCorruptIndex(t *testing.T) {
	env := testEnv(t)
	cfg := filepath.Join(env.Home, "claude", "settings.json")
	mustWriteFile(t, cfg, []byte("{}\n"))
	reg := NewRegistry(env, fixedPaths(cfg), testClock())
	if _, err := applyThroughTx(t, reg, "claude", map[string][]byte{cfg: []byte("{\"a\":1}\n")}, ReasonApply); err != nil {
		t.Fatalf("首次 apply 失败: %v", err)
	}
	indexPath := filepath.Join(toolDir(env, "claude"), indexFileName)
	if err := os.WriteFile(indexPath, []byte("{not json"), 0o600); err != nil {
		t.Fatalf("破坏索引失败: %v", err)
	}
	before := readOrNil(t, cfg)
	if _, err := applyThroughTx(t, reg, "claude", map[string][]byte{cfg: []byte("{\"b\":2}\n")}, ReasonApply); err == nil {
		t.Fatal("索引损坏时不应继续写入")
	} else {
		assertCode(t, err, apierr.CodeStorage)
	}
	assertBytes(t, cfg, before)
}

// TestOrphanPayloadsAreSwept：上次在「载荷已写、索引未写」之间中断会留下孤儿载荷，下次写入清掉它。
func TestOrphanPayloadsAreSwept(t *testing.T) {
	env := testEnv(t)
	cfg := filepath.Join(env.Home, "claude", "settings.json")
	mustWriteFile(t, cfg, []byte("{}\n"))
	reg := NewRegistry(env, fixedPaths(cfg), testClock())
	if _, err := applyThroughTx(t, reg, "claude", map[string][]byte{cfg: []byte("{\"a\":1}\n")}, ReasonApply); err != nil {
		t.Fatalf("apply 失败: %v", err)
	}
	dir := toolDir(env, "claude")
	orphan := payloadPath(dir, "bk-1-999")
	if err := os.WriteFile(orphan, []byte("leaked"), 0o600); err != nil {
		t.Fatalf("造孤儿载荷失败: %v", err)
	}
	// 目录里不属于本包命名的文件不动。
	keep := filepath.Join(dir, "notes.txt")
	if err := os.WriteFile(keep, []byte("mine"), 0o600); err != nil {
		t.Fatalf("写文件失败: %v", err)
	}
	if _, err := applyThroughTx(t, reg, "claude", map[string][]byte{cfg: []byte("{\"b\":2}\n")}, ReasonApply); err != nil {
		t.Fatalf("第二次 apply 失败: %v", err)
	}
	if _, err := os.Stat(orphan); !os.IsNotExist(err) {
		t.Fatalf("孤儿载荷未被清扫: %v", err)
	}
	if got := readOrNil(t, keep); string(got) != "mine" {
		t.Fatalf("非本包命名的文件被误删: %q", got)
	}
}

// TestSettingsCorruptionIsExplicit：设置损坏时读接口显式报错，但**不**阻断 apply（备份优先）。
func TestSettingsCorruptionIsExplicit(t *testing.T) {
	env := testEnv(t)
	cfg := filepath.Join(env.Home, "claude", "settings.json")
	mustWriteFile(t, cfg, []byte("{}\n"))
	if err := os.MkdirAll(env.BackupsDir(), 0o700); err != nil {
		t.Fatalf("建目录失败: %v", err)
	}
	if err := os.WriteFile(filepath.Join(env.BackupsDir(), settingsName), []byte("keep"), 0o600); err != nil {
		t.Fatalf("写设置失败: %v", err)
	}
	reg := NewRegistry(env, fixedPaths(cfg), testClock())
	if _, err := reg.Settings(); err == nil {
		t.Fatal("损坏的设置应显式报错")
	} else {
		assertCode(t, err, apierr.CodeStorage)
	}
	// 备份绝不因为设置文件坏了而跳过。
	if _, err := applyThroughTx(t, reg, "claude", map[string][]byte{cfg: []byte("{\"a\":1}\n")}, ReasonApply); err != nil {
		t.Fatalf("设置损坏不应阻断 apply: %v", err)
	}
	if page, err := reg.List("claude", ListOptions{}); err != nil || page.Total != 2 {
		t.Fatalf("设置损坏时仍应照常记录备份: total=%d err=%v", page.Total, err)
	}
}

// TestRecordCarriesBytesOnlyInPayload：索引只存摘要，载荷才存字节；
// 且载荷里的字节是 BASE64（与 v1 信封同构，§F-T10），不是明文 JSON。
func TestRecordCarriesBytesOnlyInPayload(t *testing.T) {
	env := testEnv(t)
	cfg := filepath.Join(env.Home, "workbuddy", "models.json")
	secret := []byte(`[{"id":"deepseek-flash","apiKey":"sk-FIXTURE-NOT-A-REAL-KEY-0001"}]` + "\n")
	mustWriteFile(t, cfg, secret)

	reg := NewRegistry(env, fixedPaths(cfg), testClock())
	res, err := applyThroughTx(t, reg, "workbuddy", map[string][]byte{cfg: []byte("[]\n")}, ReasonApply)
	if err != nil {
		t.Fatalf("apply 失败: %v", err)
	}
	dir := toolDir(env, "workbuddy")
	indexRaw := readOrNil(t, filepath.Join(dir, indexFileName))
	if bytes.Contains(indexRaw, secret) || bytes.Contains(indexRaw, []byte("apiKey")) {
		t.Fatalf("索引里出现了配置正文: %s", indexRaw)
	}
	payloadRaw := readOrNil(t, payloadPath(dir, res.BackupIDs[0]))
	if bytes.Contains(payloadRaw, secret) {
		t.Fatalf("载荷应是 BASE64 而不是明文: %s", payloadRaw)
	}
	// 备份内容含明文密钥是设计如此，但只能经 ReadPayload 取回（不外传）。
	payload, err := reg.ReadPayload("workbuddy", res.BackupIDs[0])
	if err != nil {
		t.Fatalf("取字节失败: %v", err)
	}
	if !bytes.Equal(payload.Bytes[cfg], secret) {
		t.Fatalf("备份字节不等于写前字节: got=%q want=%q", payload.Bytes[cfg], secret)
	}
	found := false
	for _, line := range strings.Split(string(payloadRaw), "\n") {
		if !strings.Contains(line, `"data"`) {
			continue
		}
		if !strings.Contains(line, base64.StdEncoding.EncodeToString(secret)) {
			t.Fatalf("载荷的 data 不是原始字节的 BASE64: %s", line)
		}
		found = true
	}
	if !found {
		t.Fatal("载荷里没有找到字节记录")
	}
}

// TestPlanRestoreNeedsWhitelist：没有白名单就不可能安全展开恢复计划（宁可报 internal 也不放行）。
func TestPlanRestoreNeedsWhitelist(t *testing.T) {
	env := testEnv(t)
	cfg := filepath.Join(env.Home, "claude", "settings.json")
	mustWriteFile(t, cfg, []byte("{}\n"))
	reg := NewRegistry(env, fixedPaths(cfg), testClock())
	res, err := applyThroughTx(t, reg, "claude", map[string][]byte{cfg: []byte("{\"a\":1}\n")}, ReasonApply)
	if err != nil {
		t.Fatalf("apply 失败: %v", err)
	}
	blind := NewRegistry(env, nil, testClock())
	if _, err := blind.PlanRestore("claude", res.BackupIDs[0]); err == nil {
		t.Fatal("未配置白名单时应拒绝展开恢复计划")
	} else {
		assertCode(t, err, apierr.CodeInternal)
	}
}

// TestFamilyPathsFromRegistry：适配器把家族注册表变成白名单，未知工具报 not_found。
func TestFamilyPathsFromRegistry(t *testing.T) {
	env := testEnv(t)
	cfg := filepath.Join(env.Home, "claude", "settings.json")
	reg := family.NewRegistry(stubFamily{id: family.Claude, paths: []string{cfg}})
	paths := FamilyPathsFrom(reg, env)
	got, err := paths(family.Claude)
	if err != nil || len(got) != 1 || got[0] != cfg {
		t.Fatalf("白名单适配不符: %v err=%v", got, err)
	}
	if _, err := paths(family.Codex); err == nil {
		t.Fatal("未注册家族应报错")
	} else {
		assertCode(t, err, apierr.CodeNotFound)
	}
}

// stubFamily 是只实现 Paths 的最小家族桩（其余方法在本包测试里用不到）。
type stubFamily struct {
	id    family.ID
	paths []string
}

func (s stubFamily) ID() family.ID              { return s.id }
func (s stubFamily) DisplayName() string        { return string(s.id) }
func (s stubFamily) Paths(*config.Env) []string { return s.paths }
func (s stubFamily) Probe(*config.Env) (family.ProbeResult, error) {
	return family.ProbeResult{}, errors.New("未实现")
}
func (s stubFamily) PlanGateway(*config.Env, family.GatewayInput) ([]family.FilePlan, error) {
	return nil, errors.New("未实现")
}
func (s stubFamily) PlanOfficial(*config.Env) ([]family.FilePlan, error) {
	return nil, errors.New("未实现")
}
func (s stubFamily) OfficialSeeded() bool { return true }
func (s stubFamily) HasLocalLane() bool   { return false }
