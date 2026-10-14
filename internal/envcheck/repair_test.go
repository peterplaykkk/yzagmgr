package envcheck

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/peterplaykkk/yzagmgr/internal/apierr"
)

// ── 测试辅助 ────────────────────────────────────────────────────────────────

func canonical(t *testing.T, path string) string {
	t.Helper()
	real, err := filepath.EvalSymlinks(path)
	if err != nil {
		t.Fatalf("解析真实路径失败 %s: %v", path, err)
	}
	return filepath.Clean(real)
}

func readFile(t *testing.T, path string) []byte {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("读文件失败 %s: %v", path, err)
	}
	return b
}

func assertFileBytes(t *testing.T, path string, want []byte) {
	t.Helper()
	got := readFile(t, path)
	if !bytes.Equal(got, want) {
		t.Fatalf("文件字节不符 %s\n got=%q\nwant=%q", path, got, want)
	}
}

func mustPreview(t *testing.T, r *Repairer, rep Report, ids []string) RepairPreview {
	t.Helper()
	prev, err := r.Preview(rep, ids)
	if err != nil {
		t.Fatalf("预览失败: %v", err)
	}
	if prev.PreviewID == "" {
		t.Fatalf("预览 id 为空")
	}
	return prev
}

func mustApply(t *testing.T, r *Repairer, previewID string) ApplyResult {
	t.Helper()
	res, err := r.Apply(previewID)
	if err != nil {
		t.Fatalf("应用失败: %v", err)
	}
	return res
}

func mustCode(t *testing.T, err error, want apierr.Code) *apierr.Error {
	t.Helper()
	if err == nil {
		t.Fatalf("期望错误码 %s，实际无错误", want)
	}
	ae := apierr.From(err)
	if ae.Code != want {
		t.Fatalf("错误码=%s，期望 %s（消息：%s）", ae.Code, want, ae.Message)
	}
	return ae
}

func listSnapshots(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		t.Fatalf("读快照目录失败: %v", err)
	}
	var names []string
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".json") {
			names = append(names, e.Name())
		}
	}
	return names
}

func loadSnapshot(t *testing.T, dir, name string) snapshotFile {
	t.Helper()
	var s snapshotFile
	if err := json.Unmarshal(readFile(t, filepath.Join(dir, name)), &s); err != nil {
		t.Fatalf("解析快照失败: %v", err)
	}
	return s
}

// ── UC-015 A5 ───────────────────────────────────────────────────────────────

func TestEnvRepairDeletesOnlyTargetLines(t *testing.T) {
	f := newFixture(t)
	// 刻意混入 CRLF 与非 UTF-8 字节：全程按字节处理，不得做编码往返。
	original := "# caf\xff comment\r\n" +
		"export PATH=\"/usr/bin:$PATH\"\n" +
		"export ANTHROPIC_AUTH_TOKEN='sk-shell-token-abc'\n" +
		"export ANTHROPIC_MODEL=shell-model\r\n" +
		"export ANTHROPIC_BASE_URL=https://old.example\n" +
		"# 尾注（无换行）"
	rc := f.write(".zshrc", original)
	want := "# caf\xff comment\r\n" + "export PATH=\"/usr/bin:$PATH\"\n" + "# 尾注（无换行）"

	rep := f.mustScan([]string{rc}, keys("ANTHROPIC_AUTH_TOKEN", "ANTHROPIC_MODEL", "ANTHROPIC_BASE_URL"), nil)
	if len(rep.Items) != 3 {
		t.Fatalf("期望 3 条冲突：%s", dumpItems(rep.Items))
	}

	r := f.mustRepairer(nil)
	prev := mustPreview(t, r, rep, nil)
	realRC := canonical(t, rc)
	if len(prev.Files) != 1 || prev.Files[0].Path != realRC {
		t.Fatalf("预览文件=%+v，期望只有 %s", prev.Files, realRC)
	}
	if prev.Files[0].SHA256Before == "" || prev.Files[0].BytesBefore != len(original) {
		t.Fatalf("预览少了写前指纹/长度: %+v", prev.Files[0])
	}
	wantLines := []DeleteLine{
		{Line: 3, Text: "export ANTHROPIC_AUTH_TOKEN='" + MaskedValue + "'"},
		{Line: 4, Text: "export ANTHROPIC_MODEL=shell-model"},
		{Line: 5, Text: "export ANTHROPIC_BASE_URL=https://old.example"},
	}
	if len(prev.Files[0].DeleteLines) != len(wantLines) {
		t.Fatalf("待删行数=%d，期望 %d：%+v", len(prev.Files[0].DeleteLines), len(wantLines), prev.Files[0].DeleteLines)
	}
	for i, want := range wantLines {
		if prev.Files[0].DeleteLines[i] != want {
			t.Fatalf("待删行[%d]=%+v，期望 %+v", i, prev.Files[0].DeleteLines[i], want)
		}
	}

	// 预览本身不改盘。
	assertFileBytes(t, rc, []byte(original))

	res := mustApply(t, r, prev.PreviewID)
	if res.Status != StatusApplied {
		t.Fatalf("apply status=%s，期望 %s", res.Status, StatusApplied)
	}
	// 只有目标行消失，其余字节（含 CRLF、0xff、末行无换行）逐字节不变。
	assertFileBytes(t, rc, []byte(want))

	// 修完再扫：不再报同一批冲突（用户手工回退才会重新报）。
	rep2 := f.mustScan([]string{rc}, keys("ANTHROPIC_AUTH_TOKEN", "ANTHROPIC_MODEL", "ANTHROPIC_BASE_URL"), nil)
	if len(rep2.Items) != 0 {
		t.Fatalf("修完不该再报：%s", dumpItems(rep2.Items))
	}
}

// ── UC-015 A6 ───────────────────────────────────────────────────────────────

func TestEnvRepairSnapshotsBeforeWriting(t *testing.T) {
	f := newFixture(t)
	original := "export ANTHROPIC_MODEL=shell-model\nexport PATH=$PATH\n"
	rc := f.write(".zshrc", original)

	rep := f.mustScan([]string{rc}, keys("ANTHROPIC_MODEL"), nil)
	r := f.mustRepairer(nil)
	prev := mustPreview(t, r, rep, nil)
	res := mustApply(t, r, prev.PreviewID)

	if len(res.SnapshotIDs) != 1 {
		t.Fatalf("快照 id=%v，期望 1 个", res.SnapshotIDs)
	}
	names := listSnapshots(t, f.snapshotsDir())
	if len(names) != 1 {
		t.Fatalf("快照文件=%v，期望 1 个", names)
	}
	st, err := os.Stat(filepath.Join(f.snapshotsDir(), names[0]))
	if err != nil {
		t.Fatalf("stat 快照失败: %v", err)
	}
	if st.Mode().Perm() != 0o600 {
		t.Fatalf("快照权限=%v，期望 0600", st.Mode().Perm())
	}

	// 快照里存的是**改前**字节：此刻盘上已是改后字节，两者构成对照。
	snap := loadSnapshot(t, f.snapshotsDir(), names[0])
	if snap.ID != res.SnapshotIDs[0] {
		t.Fatalf("快照 id 与 apply 回报不一致: %q vs %q", snap.ID, res.SnapshotIDs[0])
	}
	if len(snap.Files) != 1 || snap.Files[0].Path != canonical(t, rc) || !snap.Files[0].Existed {
		t.Fatalf("快照文件条目不符: %+v", snap.Files)
	}
	content, err := base64.StdEncoding.DecodeString(snap.Files[0].Content)
	if err != nil {
		t.Fatalf("快照内容不是合法 base64: %v", err)
	}
	if !bytes.Equal(content, []byte(original)) {
		t.Fatalf("快照内容不是改前字节: got=%q want=%q", content, original)
	}
	assertFileBytes(t, rc, []byte("export PATH=$PATH\n"))
}

// ── UC-015 A7 ───────────────────────────────────────────────────────────────

func TestEnvRepairRestoreIsByteExact(t *testing.T) {
	f := newFixture(t)
	original := "export ANTHROPIC_MODEL=shell-model\r\nexport LANG=\xfe\xff\r\nexport ANTHROPIC_BASE_URL=https://old\n"
	rc := f.write(".zshrc", original)
	before := readFile(t, rc)

	rep := f.mustScan([]string{rc}, keys("ANTHROPIC_MODEL", "ANTHROPIC_BASE_URL"), nil)
	r := f.mustRepairer(nil)
	res := mustApply(t, r, mustPreview(t, r, rep, nil).PreviewID)
	if len(res.SnapshotIDs) == 0 {
		t.Fatalf("apply 没有留快照：%+v", res)
	}
	if bytes.Equal(readFile(t, rc), before) {
		t.Fatalf("apply 之后文件应已改变")
	}

	restore, err := r.Restore(res.SnapshotIDs)
	if err != nil {
		t.Fatalf("恢复失败: %v", err)
	}
	if restore.Status != StatusRestored {
		t.Fatalf("restore status=%s，期望 %s", restore.Status, StatusRestored)
	}
	if len(restore.Files) != 1 || restore.Files[0].Status != FileRestored {
		t.Fatalf("restore files=%+v", restore.Files)
	}
	assertFileBytes(t, rc, before)
}

// ── UC-015 A8 ───────────────────────────────────────────────────────────────

func TestEnvRepairRestoreIsIdempotent(t *testing.T) {
	f := newFixture(t)
	original := "export ANTHROPIC_MODEL=shell-model\nexport PATH=/usr/bin\n"
	rc := f.write(".zshrc", original)

	rep := f.mustScan([]string{rc}, keys("ANTHROPIC_MODEL"), nil)
	r := f.mustRepairer(nil)
	res := mustApply(t, r, mustPreview(t, r, rep, nil).PreviewID)

	if _, err := r.Restore(res.SnapshotIDs); err != nil {
		t.Fatalf("首次恢复失败: %v", err)
	}
	first := readFile(t, rc)

	// 第二次：不报错、字节不变，且如实记为 unchanged（不是又一次「还原成功」）。
	second, err := r.Restore(res.SnapshotIDs)
	if err != nil {
		t.Fatalf("第二次恢复不应报错: %v", err)
	}
	if second.Status != StatusUnchanged {
		t.Fatalf("第二次 status=%s，期望 %s", second.Status, StatusUnchanged)
	}
	if len(second.Files) != 1 || second.Files[0].Status != FileUnchanged {
		t.Fatalf("第二次 files=%+v", second.Files)
	}
	if !bytes.Equal(readFile(t, rc), first) {
		t.Fatalf("第二次恢复改动了字节")
	}
	assertFileBytes(t, rc, []byte(original))
}

// ── UC-015 A9 ───────────────────────────────────────────────────────────────

func TestEnvRepairRejectsStalePreview(t *testing.T) {
	f := newFixture(t)
	rc := f.write(".zshrc", "export ANTHROPIC_MODEL=shell-model\n")
	rep := f.mustScan([]string{rc}, keys("ANTHROPIC_MODEL"), nil)
	r := f.mustRepairer(nil)
	prev := mustPreview(t, r, rep, nil)

	// 取预览之后、应用之前，文件被外部改动（连无关行一起改）。
	external := "export ANTHROPIC_MODEL=shell-model\nexport NEW_THING=1\n"
	f.writeAt(rc, external)

	_, err := r.Apply(prev.PreviewID)
	ae := mustCode(t, err, apierr.CodeConfigChanged)
	if !strings.Contains(ae.Message, canonical(t, rc)) {
		t.Fatalf("错误消息里应指名被外改的文件: %s", ae.Message)
	}
	// 不改盘：文件保持外改后的字节。
	assertFileBytes(t, rc, []byte(external))
	// 也不留快照（指纹校验在留快照之前，先判后做）。
	if names := listSnapshots(t, f.snapshotsDir()); len(names) != 0 {
		t.Fatalf("被拒绝的 apply 不该留快照: %v", names)
	}
}

// ── UC-015 A10 ──────────────────────────────────────────────────────────────

func TestEnvRepairNeverWritesOutsideAllowlist(t *testing.T) {
	f := newFixture(t)

	// 契约清单：/etc/profile 与 /etc/bashrc 属「只读检查」。
	if sys := DefaultSystemFiles(); len(sys) != 2 || sys[0] != "/etc/profile" || sys[1] != "/etc/bashrc" {
		t.Fatalf("系统只读清单=%v", sys)
	}

	sysFile := f.writeSystem("profile", "export GEMINI_API_KEY=from-system\n")
	outside := f.writeAt(filepath.Join(f.root, "outside", "rc.sh"), "export XAI_API_KEY=from-outside\n")
	rc := f.write(".zshrc", "source "+outside+"\nexport ANTHROPIC_MODEL=from-user-rc\n")

	rep := f.mustScan([]string{rc}, keys("GEMINI_API_KEY", "XAI_API_KEY", "ANTHROPIC_MODEL"), nil)
	if len(rep.Items) != 3 {
		t.Fatalf("期望 3 条（系统文件 / HOME 外文件 / 用户 rc）：%s", dumpItems(rep.Items))
	}
	sysItem := mustFind(t, rep, "GEMINI_API_KEY", canonical(t, sysFile))
	outItem := mustFind(t, rep, "XAI_API_KEY", canonical(t, outside))
	userItem := mustFind(t, rep, "ANTHROPIC_MODEL", canonical(t, rc))
	if sysItem.Repairable || outItem.Repairable {
		t.Fatalf("白名单外的来源不得标可修: sys=%v out=%v", sysItem.Repairable, outItem.Repairable)
	}
	if !userItem.Repairable {
		t.Fatalf("隔离 HOME 内的启动文件应可修")
	}

	r := f.mustRepairer(nil)
	for _, id := range []string{sysItem.ID, outItem.ID} {
		_, err := r.Preview(rep, []string{id})
		ae := mustCode(t, err, apierr.CodePermissionDenied)
		if !strings.Contains(ae.Message, "手工") {
			t.Fatalf("拒绝消息应说明需手工处理: %s", ae.Message)
		}
	}
	// 一个字节都没动（含白名单外的两个文件）。
	assertFileBytes(t, sysFile, []byte("export GEMINI_API_KEY=from-system\n"))
	assertFileBytes(t, outside, []byte("export XAI_API_KEY=from-outside\n"))
	assertFileBytes(t, rc, []byte("source "+outside+"\nexport ANTHROPIC_MODEL=from-user-rc\n"))
	if names := listSnapshots(t, f.snapshotsDir()); len(names) != 0 {
		t.Fatalf("拒绝路径不该产生快照: %v", names)
	}

	// 白名单判定本身（字符串级，不读盘）：/etc 与 HOME 外一律为假。
	if r.allow.allowed("/etc/profile") {
		t.Fatalf("/etc/profile 必须判不可写")
	}
	if r.allow.allowed(canonical(t, outside)) {
		t.Fatalf("HOME 之外的路径必须判不可写")
	}
	if !r.allow.allowed(canonical(t, rc)) {
		t.Fatalf("隔离 HOME 内的启动文件必须判可写")
	}
}

// ── UC-015 A11 ──────────────────────────────────────────────────────────────

func TestEnvRepairPermissionDeniedIsActionable(t *testing.T) {
	f := newFixture(t)
	good := f.write(".zshrc", "export ANTHROPIC_MODEL=shell-model\n")
	goodBytes := readFile(t, good)

	// 「目标不可写」的构造：父目录在扫描之后被换成同名普通文件（ENOTDIR）。
	// 之所以不用 chmod 0400：root 能无视权限位，而这条路径连 root 都写不进去，
	// 因此测试在任意 uid 下都判定同一个真实行为。
	sub := filepath.Join(f.home, "sub")
	target := f.writeAt(filepath.Join(sub, ".bashrc"), "export ANTHROPIC_BASE_URL=https://old\n")
	rep := f.mustScan([]string{good, target}, keys("ANTHROPIC_MODEL", "ANTHROPIC_BASE_URL"), nil)
	if len(rep.Items) != 2 {
		t.Fatalf("期望 2 条冲突：%s", dumpItems(rep.Items))
	}
	if err := os.RemoveAll(sub); err != nil {
		t.Fatalf("拆夹具目录失败: %v", err)
	}
	f.writeAt(sub, "blocker")

	r := f.mustRepairer(nil)
	_, err := r.Preview(rep, nil)
	ae := mustCode(t, err, apierr.CodePermissionDenied)
	if !strings.Contains(ae.Message, "手工") {
		t.Fatalf("消息应说明需手工处理: %s", ae.Message)
	}
	if got, _ := ae.Details["path"].(string); got == "" {
		t.Fatalf("拒绝里应带上是哪个目标: %+v", ae.Details)
	}

	// 不部分写入：另一个（本来就能写的）目标一个字节都没动。
	assertFileBytes(t, good, goodBytes)
	if names := listSnapshots(t, f.snapshotsDir()); len(names) != 0 {
		t.Fatalf("先判后做：拒绝路径不该留快照: %v", names)
	}
}

// ── 我自己的边界与负向用例 ──────────────────────────────────────────────────

// 预览是「看过再决定」：只看不应用时，盘上必须零变化（独立审查要求）。
func TestEnvRepairPreviewLeavesDiskUntouched(t *testing.T) {
	f := newFixture(t)
	original := "export ANTHROPIC_MODEL=shell-model\n"
	rc := f.write(".zshrc", original)

	rep := f.mustScan([]string{rc}, keys("ANTHROPIC_MODEL"), nil)
	r := f.mustRepairer(nil)
	prev := mustPreview(t, r, rep, nil)
	if len(prev.Files) != 1 {
		t.Fatalf("预览应给出 1 个文件: %+v", prev.Files)
	}
	assertFileBytes(t, rc, []byte(original))
	if names := listSnapshots(t, f.snapshotsDir()); len(names) != 0 {
		t.Fatalf("预览不该产生快照: %v", names)
	}
	if _, err := os.Stat(f.snapshotsDir()); !os.IsNotExist(err) {
		t.Fatalf("预览不该创建快照目录")
	}
}

func TestEnvRepairPreviewRejectsUnknownConflictID(t *testing.T) {
	f := newFixture(t)
	f.write(".zshrc", "export ANTHROPIC_MODEL=shell-model\n")
	rep := f.mustScan(f.homeFiles(), keys("ANTHROPIC_MODEL"), nil)
	r := f.mustRepairer(nil)
	_, err := r.Preview(rep, []string{"ec_deadbeefdeadbeef"})
	mustCode(t, err, apierr.CodeInvalidRequest)
}

// 会话来源（launchd / 进程环境）不可能在应用内删除：拒绝并给出可执行替代做法。
func TestEnvRepairRefusesSessionSourcesWithRemedy(t *testing.T) {
	f := newFixture(t)
	f.launchd["ANTHROPIC_BASE_URL"] = "https://from-launchd"
	f.process["ANTHROPIC_MODEL"] = "from-terminal"

	rep := f.mustScan(f.homeFiles(), keys("ANTHROPIC_BASE_URL", "ANTHROPIC_MODEL"), nil)
	r := f.mustRepairer(nil)
	for _, it := range rep.Items {
		_, err := r.Preview(rep, []string{it.ID})
		ae := mustCode(t, err, apierr.CodePermissionDenied)
		switch {
		case strings.HasPrefix(it.Source, LaunchdSourcePrefix):
			if !strings.Contains(ae.Message, "launchctl unsetenv "+it.Name) {
				t.Fatalf("launchd 来源应给出 launchctl unsetenv 指引: %s", ae.Message)
			}
		case strings.HasPrefix(it.Source, ProcessSourcePrefix):
			if !strings.Contains(ae.Message, "unset "+it.Name) {
				t.Fatalf("进程环境来源应给出 unset 指引: %s", ae.Message)
			}
		}
	}
}

// 预览的待删行文本同样不得带令牌明文（A13 在修复面上的延伸）。
//
// 修订版加严：扫描与预览共用同一**整行级**脱敏函数（单源），因此同行兄弟凭据
// 与注释里的令牌形态在 deleteLines 里一并被掩；删行判定不受影响。
func TestEnvRepairPreviewRedactsSecretLines(t *testing.T) {
	const (
		secret  = "sk-ant-PREVIEW-SECRET-1234567890"
		sibling = "sk-live-AUDIT-REALKEY-0123456789abcdef"
		mimo    = "tp-mimo-01J2ZQ8ZK7M3V6ABCDEFGHJKLM"
	)
	f := newFixture(t)
	rc := f.write(".zshrc",
		"export ANTHROPIC_AUTH_TOKEN=\""+secret+"\"\n"+
			"export ANTHROPIC_BASE_URL=https://old.example OPENAI_API_KEY="+sibling+"\n"+
			"export ANTHROPIC_MODEL=from-shell # 注释里的令牌 "+mimo+"\n")
	rep := f.mustScan([]string{rc}, keys("ANTHROPIC_AUTH_TOKEN", "ANTHROPIC_BASE_URL", "ANTHROPIC_MODEL"), nil)
	if len(rep.Items) != 3 {
		t.Fatalf("期望 3 条冲突：%s", dumpItems(rep.Items))
	}

	r := f.mustRepairer(nil)
	prev := mustPreview(t, r, rep, nil)
	blob, err := json.Marshal(prev)
	if err != nil {
		t.Fatalf("序列化失败: %v", err)
	}
	for _, s := range []string{secret, sibling, mimo} {
		if bytes.Contains(blob, []byte(s)) {
			t.Fatalf("预览正文泄漏了令牌明文 %q: %s", s, blob)
		}
	}
	// 反序列化后（JSON 转义对任何合法解析器透明）行文本应带脱敏替身。
	var back RepairPreview
	if err := json.Unmarshal(blob, &back); err != nil {
		t.Fatalf("反序列化失败: %v", err)
	}
	if len(back.Files) != 1 || len(back.Files[0].DeleteLines) != 3 {
		t.Fatalf("预览形状不符: %+v", back.Files)
	}
	wantLines := []string{
		"export ANTHROPIC_AUTH_TOKEN=\"" + MaskedValue + "\"",
		"export ANTHROPIC_BASE_URL=https://old.example OPENAI_API_KEY=" + MaskedValue,
		"export ANTHROPIC_MODEL=from-shell # 注释里的令牌 " + MaskedValue,
	}
	for i, want := range wantLines {
		if got := back.Files[0].DeleteLines[i].Text; got != want {
			t.Fatalf("待删行[%d]=%q，期望 %q", i, got, want)
		}
	}
	// 脱敏只影响展示：删的仍是那三行。
	res := mustApply(t, r, prev.PreviewID)
	if res.Status != StatusApplied {
		t.Fatalf("apply status=%s", res.Status)
	}
	assertFileBytes(t, rc, []byte(""))
}

// 符号链接场景：改真实文件、链接保持可用。
func TestEnvRepairFollowsSymlinkToRealTarget(t *testing.T) {
	f := newFixture(t)
	real := f.writeAt(filepath.Join(f.home, "dotfiles", "zshrc"), "export ANTHROPIC_MODEL=shell-model\nexport PATH=/usr/bin\n")
	link := filepath.Join(f.home, ".zshrc")
	if err := os.Symlink(real, link); err != nil {
		t.Fatalf("建符号链接失败: %v", err)
	}

	rep := f.mustScan([]string{link}, keys("ANTHROPIC_MODEL"), nil)
	r := f.mustRepairer(nil)
	res := mustApply(t, r, mustPreview(t, r, rep, nil).PreviewID)
	if len(res.Files) != 1 || res.Files[0].Path != canonical(t, real) {
		t.Fatalf("改动面应是真实文件: %+v", res.Files)
	}
	assertFileBytes(t, real, []byte("export PATH=/usr/bin\n"))
	if fi, err := os.Lstat(link); err != nil || fi.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("符号链接应保持为链接: %v %v", fi, err)
	}
}

// 已手工修复（预览时算不出待删行）→ 不改盘、不留快照、apply 报 unchanged。
func TestEnvRepairIsNoOpWhenAlreadyClean(t *testing.T) {
	f := newFixture(t)
	rc := f.write(".zshrc", "export ANTHROPIC_MODEL=shell-model\n")
	rep := f.mustScan([]string{rc}, keys("ANTHROPIC_MODEL"), nil)

	// 用户在看预览之前自己改回期望值。
	clean := "export ANTHROPIC_MODEL=claude-fable-5-1\n"
	f.writeAt(rc, clean)

	r := f.mustRepairer(nil)
	prev := mustPreview(t, r, rep, nil)
	if len(prev.Files) != 0 {
		t.Fatalf("已无待删行时预览不该列文件: %+v", prev.Files)
	}
	res := mustApply(t, r, prev.PreviewID)
	if res.Status != StatusUnchanged {
		t.Fatalf("status=%s，期望 %s", res.Status, StatusUnchanged)
	}
	if len(res.SnapshotIDs) != 0 {
		t.Fatalf("无需改动时不该留快照: %v", res.SnapshotIDs)
	}
	assertFileBytes(t, rc, []byte(clean))
}

// 预览租约过期后不得再落盘。
func TestEnvRepairPreviewExpires(t *testing.T) {
	f := newFixture(t)
	original := "export ANTHROPIC_MODEL=shell-model\n"
	rc := f.write(".zshrc", original)
	rep := f.mustScan([]string{rc}, keys("ANTHROPIC_MODEL"), nil)

	now := time.Date(2026, 9, 22, 10, 0, 0, 0, time.UTC)
	r := f.mustRepairer(func(o *RepairOptions) {
		o.PreviewTTL = 10 * time.Second
		o.Clock = func() time.Time { return now }
	})
	prev := mustPreview(t, r, rep, nil)

	now = now.Add(11 * time.Second)
	_, err := r.Apply(prev.PreviewID)
	mustCode(t, err, apierr.CodePreviewExpired)
	assertFileBytes(t, rc, []byte(original))

	// 同一个 id 再取一次：已消费的租约是 invalid_request，与过期码不互串。
	_, err = r.Apply(prev.PreviewID)
	mustCode(t, err, apierr.CodeInvalidRequest)
}

// 同一个变量既出现在系统级只读文件、又出现在用户级启动文件里时：
// 系统级**先**加载（登录 shell 的顺序），所以生效的是用户级那条，修复也只碰
// 用户级那份——不能因为 /etc 里还有一份同名赋值就把整批修复拒掉。
func TestEnvRepairScopesToRepairableInstances(t *testing.T) {
	f := newFixture(t)
	sysFile := f.writeSystem("profile", "export GEMINI_API_KEY=from-system\n")
	rc := f.write(".zshrc", "export GEMINI_API_KEY=from-user\n")

	rep := f.mustScan([]string{rc}, keys("GEMINI_API_KEY"), nil)
	if len(rep.Items) != 1 {
		t.Fatalf("期望只报生效的一条：%s", dumpItems(rep.Items))
	}
	it := rep.Items[0]
	if it.Source != canonical(t, rc) || !it.Repairable {
		t.Fatalf("用户级赋值应最后生效且可修：%+v", it)
	}

	r := f.mustRepairer(nil)
	prev := mustPreview(t, r, rep, nil)
	if len(prev.Files) != 1 || prev.Files[0].Path != canonical(t, rc) {
		t.Fatalf("只应规划用户级那份：%+v", prev.Files)
	}
	mustApply(t, r, prev.PreviewID)
	assertFileBytes(t, sysFile, []byte("export GEMINI_API_KEY=from-system\n"))
	assertFileBytes(t, rc, []byte(""))
}

// 扫描之后用户自己删了启动文件：预览按「已无可改」处理（空预览 + apply
// unchanged），不制造误导性的 permission_denied，也不把文件重新造出来。
func TestEnvRepairToleratesTargetRemovedAfterScan(t *testing.T) {
	f := newFixture(t)
	rc := f.write(".zshrc", "export ANTHROPIC_MODEL=shell-model\n")
	rep := f.mustScan([]string{rc}, keys("ANTHROPIC_MODEL"), nil)

	if err := os.Remove(rc); err != nil {
		t.Fatalf("删夹具文件失败: %v", err)
	}

	r := f.mustRepairer(nil)
	prev := mustPreview(t, r, rep, nil)
	if len(prev.Files) != 0 {
		t.Fatalf("目标已不存在时不该规划任何文件: %+v", prev.Files)
	}
	res := mustApply(t, r, prev.PreviewID)
	if res.Status != StatusUnchanged {
		t.Fatalf("status=%s，期望 %s", res.Status, StatusUnchanged)
	}
	if _, err := os.Stat(rc); !os.IsNotExist(err) {
		t.Fatalf("已删除的目标不得被重新造出来")
	}
}

// 契约 DTO 的字段集合是冻结面：多一个字段就是违约（doc/api-v1.md §9，
// 「任何未在此列出的字段不得出现在响应中」）。这条测试把字段集合钉死。
func TestEnvDTOFieldSetsMatchContract(t *testing.T) {
	cases := []struct {
		name  string
		value any
		want  []string
	}{
		{"EnvConflict", EnvConflict{}, []string{
			"id", "name", "effectiveValue", "expectedValue", "source",
			"line", "lineText", "sensitive", "repairable"}},
		{"ConflictList", ConflictList{Items: []EnvConflict{}}, []string{"items", "total", "scannedAt"}},
		{"RepairPreview", RepairPreview{Files: []PreviewFile{}}, []string{"previewId", "expiresAt", "files"}},
		{"PreviewFile", PreviewFile{DeleteLines: []DeleteLine{}}, []string{
			"path", "sha256Before", "bytesBefore", "deleteLines"}},
		{"DeleteLine", DeleteLine{}, []string{"line", "text"}},
		{"ApplyResult", ApplyResult{Files: []FileStatus{}, SnapshotIDs: []string{}}, []string{
			"status", "files", "snapshotIds"}},
		{"RestoreResult", RestoreResult{Files: []FileStatus{}}, []string{"status", "files"}},
		{"FileStatus", FileStatus{}, []string{"path", "status"}},
	}
	for _, c := range cases {
		blob, err := json.Marshal(c.value)
		if err != nil {
			t.Fatalf("%s 序列化失败: %v", c.name, err)
		}
		var got map[string]any
		if err := json.Unmarshal(blob, &got); err != nil {
			t.Fatalf("%s 反序列化失败: %v", c.name, err)
		}
		keys := make([]string, 0, len(got))
		for k := range got {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		want := append([]string(nil), c.want...)
		sort.Strings(want)
		if strings.Join(keys, ",") != strings.Join(want, ",") {
			t.Fatalf("%s 字段集合不符\n got=%v\nwant=%v", c.name, keys, want)
		}
	}
	// sensitive 标记必须与字段集合同样可判定：敏感条目里不能出现明文。
	blob, err := json.Marshal(EnvConflict{EffectiveValue: MaskedValue, ExpectedValue: MaskedValue, Sensitive: true})
	if err != nil {
		t.Fatalf("序列化失败: %v", err)
	}
	if !strings.Contains(string(blob), `"sensitive":true`) {
		t.Fatalf("sensitive 标记缺失: %s", blob)
	}
}
