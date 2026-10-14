package backup

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/peterplaykkk/yzagmgr/internal/apierr"
	"github.com/peterplaykkk/yzagmgr/internal/family"
	"github.com/peterplaykkk/yzagmgr/internal/tx"
)

// ── UC-004 A3：恢复写回原字节 ───────────────────────────────────────────────

// 测试: TestRestoreWritesBackOriginalBytes
func TestRestoreWritesBackOriginalBytes(t *testing.T) {
	env := testEnv(t)
	a := filepath.Join(env.Home, "claude", "settings.json")
	b := filepath.Join(env.Home, "claude", "claude.json")
	officialA := []byte("{\"tier\":\"official\",\"k\":1}\n")
	officialB := []byte("{\"tier\":\"official\",\"n\":\"二\"}\n")
	mustWriteFile(t, a, officialA)
	mustWriteFile(t, b, officialB)

	reg := NewRegistry(env, fixedPaths(a, b), testClock())
	res, err := applyThroughTx(t, reg, "claude", map[string][]byte{
		a: []byte("{\"tier\":\"gateway\",\"k\":1}\n"),
		b: []byte("{\"tier\":\"gateway\",\"n\":\"二\"}\n"),
	}, ReasonApply)
	if err != nil {
		t.Fatalf("apply 失败: %v", err)
	}
	rec, err := reg.Get("claude", res.BackupIDs[0])
	if err != nil {
		t.Fatalf("取备份失败: %v", err)
	}

	restoreThroughTx(t, reg, "claude", res.BackupIDs[0])

	assertBytes(t, a, officialA)
	assertBytes(t, b, officialB)
	// 恢复后每个文件的 sha256 与该备份记录里的字节逐字节相等。
	for _, f := range rec.Files {
		want := officialA
		if f.Path == b {
			want = officialB
		}
		if got := readOrNil(t, f.Path); !bytes.Equal(got, want) || tx.HashBytes(got) != f.SHA256 {
			t.Fatalf("恢复后 %s 与备份记录不符: sha=%s want=%s", f.Path, tx.HashBytes(got), f.SHA256)
		}
	}
}

// ── UC-004 A4：恢复移除接管时创建的路径 ─────────────────────────────────────

// 测试: TestRestoreRemovesPathsCreatedByTakeover
func TestRestoreRemovesPathsCreatedByTakeover(t *testing.T) {
	env := testEnv(t)
	a := filepath.Join(env.Home, "pi", "models.json")
	b := filepath.Join(env.Home, "pi", "settings.json")
	officialA := []byte("[]\n")
	mustWriteFile(t, a, officialA)

	reg := NewRegistry(env, fixedPaths(a, b), testClock())
	if _, err := applyThroughTx(t, reg, "pi", map[string][]byte{
		a: []byte("[{\"id\":\"deepseek-flash\"}]\n"),
		b: []byte("{\"autoCompactWindow\":900000}\n"),
	}, ReasonApply); err != nil {
		t.Fatalf("apply 失败: %v", err)
	}
	if got := readOrNil(t, b); got == nil {
		t.Fatal("接管应创建 settings.json")
	}

	initial, err := reg.Initial("pi")
	if err != nil {
		t.Fatalf("取初始快照失败: %v", err)
	}
	plan, err := reg.PlanRestoreDetailed("pi", initial.BackupID)
	if err != nil {
		t.Fatalf("展开恢复计划失败: %v", err)
	}
	var deletePlan bool
	for _, f := range plan.Files {
		if f.Path == b {
			deletePlan = f.Delete && f.Existed
		}
	}
	if !deletePlan {
		t.Fatalf("接管时创建的路径应展开为删除计划: %+v", plan.Files)
	}
	if plan.Warnings[b] != WarningCreatedByTakeover {
		t.Fatalf("缺少「接管时创建」提示: %v", plan.Warnings)
	}
	for _, s := range plan.Skipped {
		if s.Path == b {
			t.Fatalf("接管时创建的路径不应被跳过: %+v", s)
		}
	}

	restoreThroughTx(t, reg, "pi", initial.BackupID)
	if _, err := os.Stat(b); !os.IsNotExist(err) {
		t.Fatalf("恢复后接管时创建的路径应被删除: %v", err)
	}
	assertBytes(t, a, officialA)
}

// ── UC-004 A5：只按记录展开且过白名单 ───────────────────────────────────────

// 测试: TestRestoreOnlyExpandsRecordedPathsWithinWhitelist
func TestRestoreOnlyExpandsRecordedPathsWithinWhitelist(t *testing.T) {
	env := testEnv(t)
	a := filepath.Join(env.Home, "gemini", ".gemini", ".env")
	b := filepath.Join(env.Home, "gemini", ".gemini", "settings.json")
	c := filepath.Join(env.Home, "gemini", ".gemini", "extra.json")

	officialA := []byte("KEY=official\n")
	officialB := []byte("{\"official\":true}\n")
	gatewayA := []byte("KEY=gateway\n")
	gatewayB := []byte("{\"gateway\":true}\n")
	untouched := []byte("{\"user\":\"mine\"}\n")

	mustWriteFile(t, a, officialA)
	mustWriteFile(t, b, officialB)
	whitelist := []string{a, b}
	reg := NewRegistry(env, func(family.ID) ([]string, error) { return whitelist, nil }, testClock())

	// 家族首次接管时 b 还是受管文件；之后 c 才被纳入、b 因版本变化退出受管集合。
	if _, err := applyThroughTx(t, reg, "gemini", map[string][]byte{a: gatewayA, b: gatewayB}, ReasonApply); err != nil {
		t.Fatalf("apply 失败: %v", err)
	}
	mustWriteFile(t, c, untouched)
	whitelist = []string{a, c}

	plan, err := reg.PlanRestoreDetailed("gemini", "")
	if err == nil {
		t.Fatalf("空备份编号应被拒，实际展开出 %d 个文件", len(plan.Files))
	}
	assertCode(t, err, apierr.CodeInvalidRequest)

	page, err := reg.List("gemini", ListOptions{})
	if err != nil {
		t.Fatalf("列举失败: %v", err)
	}
	applyBackup := ""
	for _, item := range page.Items {
		if !item.Initial {
			applyBackup = item.BackupID
			break
		}
	}
	if applyBackup == "" {
		t.Fatal("未找到 apply 的备份记录")
	}

	plan, err = reg.PlanRestoreDetailed("gemini", applyBackup)
	if err != nil {
		t.Fatalf("展开恢复计划失败: %v", err)
	}
	if len(plan.Files) != 1 || plan.Files[0].Path != a {
		t.Fatalf("计划应只含白名单内的已记录路径: %+v", plan.Files)
	}
	foundSkip := false
	for _, s := range plan.Skipped {
		if s.Path == b && s.Reason == SkipNotManaged {
			foundSkip = true
		}
		if s.Path == c {
			t.Fatalf("未在备份记录里的路径不应出现在计划里: %+v", s)
		}
	}
	if !foundSkip {
		t.Fatalf("非受管路径应跳过并记 warning: %+v", plan.Skipped)
	}

	// 执行计划：只有 a 被写回，b/c 逐字节不动。
	sorted := plan.Files
	if _, err := runTx(t, reg, "gemini", sorted, "restore", ReasonRestore); err != nil {
		t.Fatalf("恢复写盘失败: %v", err)
	}
	assertBytes(t, a, officialA)
	assertBytes(t, b, gatewayB)
	assertBytes(t, c, untouched)
}

// ── UC-004 A8：恢复幂等 ─────────────────────────────────────────────────────

// 测试: TestRestoreIsIdempotent
func TestRestoreIsIdempotent(t *testing.T) {
	env := testEnv(t)
	cfg := filepath.Join(env.Home, "claude", "settings.json")
	official := []byte("{\"tier\":\"official\"}\n")
	mustWriteFile(t, cfg, official)

	reg := NewRegistry(env, fixedPaths(cfg), testClock())
	if _, err := applyThroughTx(t, reg, "claude", map[string][]byte{cfg: []byte("{\"tier\":\"gateway\"}\n")}, ReasonApply); err != nil {
		t.Fatalf("apply 失败: %v", err)
	}
	initial, err := reg.Initial("claude")
	if err != nil {
		t.Fatalf("取初始快照失败: %v", err)
	}
	restoreThroughTx(t, reg, "claude", initial.BackupID)
	assertBytes(t, cfg, official)

	second := restoreThroughTx(t, reg, "claude", initial.BackupID)
	if second.Status != tx.StatusUnchanged {
		t.Fatalf("第二次恢复应为 unchanged: %s (%+v)", second.Status, second.Files)
	}
	for _, f := range second.Files {
		if f.Status != tx.FileUnchanged {
			t.Fatalf("第二次恢复不应触盘: %s = %s", f.Path, f.Status)
		}
	}
	assertBytes(t, cfg, official)

	// 已回到「接管前不存在」的路径在第二次恢复时记为跳过（目标状态已达成）。
	// 这里换一个工具：初始快照每个工具只落一份，而本段要的是一个含「接管时新建路径」的新快照。
	absent := filepath.Join(env.Home, "gemini", ".gemini", "created.json")
	otherCfg := filepath.Join(env.Home, "gemini", ".gemini", "settings.json")
	mustWriteFile(t, otherCfg, []byte("{\"official\":true}\n"))
	takeover := NewRegistry(env, fixedPaths(otherCfg, absent), testClock())
	if _, err := applyThroughTx(t, takeover, "gemini", map[string][]byte{
		otherCfg: []byte("{\"gateway\":true}\n"),
		absent:   []byte("{}\n"),
	}, ReasonApply); err != nil {
		t.Fatalf("apply 失败: %v", err)
	}
	init2, err := takeover.Initial("gemini")
	if err != nil {
		t.Fatalf("取初始快照失败: %v", err)
	}
	restoreThroughTx(t, takeover, "gemini", init2.BackupID)
	plan, err := takeover.PlanRestoreDetailed("gemini", init2.BackupID)
	if err != nil {
		t.Fatalf("展开恢复计划失败: %v", err)
	}
	found := false
	for _, s := range plan.Skipped {
		if s.Path == absent && s.Reason == SkipAlreadyAbsent {
			found = true
		}
	}
	if !found {
		t.Fatalf("已不在场的接管路径应记为跳过: %+v", plan.Skipped)
	}
}

// ── UC-004 A9：两条往返 ─────────────────────────────────────────────────────

// 测试: TestRestoreAndReapplyRoundTrip
func TestRestoreAndReapplyRoundTrip(t *testing.T) {
	env := testEnv(t)
	cfg := filepath.Join(env.Home, "codex", "config.toml")
	official := []byte("model = \"gpt-5\"\ntier = \"official\"\n")
	gateway := []byte("model = \"deepseek-flash\"\ntier = \"gateway\"\n")
	mustWriteFile(t, cfg, official)

	reg := NewRegistry(env, fixedPaths(cfg), testClock())
	if _, err := applyThroughTx(t, reg, "codex", map[string][]byte{cfg: gateway}, ReasonApply); err != nil {
		t.Fatalf("apply 失败: %v", err)
	}
	assertBytes(t, cfg, gateway)

	initial, err := reg.Initial("codex")
	if err != nil {
		t.Fatalf("取初始快照失败: %v", err)
	}
	restoreThroughTx(t, reg, "codex", initial.BackupID)
	assertBytes(t, cfg, official)

	// 再 apply 一次回到网关档：目标字节与第一次逐字节相同。
	reapply := restoreThroughTxAfterApply(t, reg, "codex", cfg, gateway)
	assertBytes(t, cfg, gateway)
	if reapply.Status != tx.StatusApplied {
		t.Fatalf("再次 apply 应为 applied: %s", reapply.Status)
	}
}

// restoreThroughTxAfterApply 在往返测试里做第三次 apply（回到网关档）。
func restoreThroughTxAfterApply(t *testing.T, reg *Registry, toolID, path string, target []byte) tx.Result {
	t.Helper()
	res, err := applyThroughTx(t, reg, toolID, map[string][]byte{path: target}, ReasonApply)
	if err != nil {
		t.Fatalf("再次 apply 失败: %v", err)
	}
	return res
}

// ── UC-093 A5（后半）：对 restore 产生的新快照再次恢复（双向恢复链） ─────────

// TestRestoreChainUsesPreRestoreSnapshotAsNewBackup 守住 UC-093 A5 的后半段：
// 备份后文件被外部新增了自定义字段；恢复预览必须列出该文件（字段将被覆盖）；
// 取消（只预览）零变化；确认后文件等于所选旧备份，且**恢复前留的新快照**
// 含该自定义字段、是一等备份——可再次恢复，把文件带回恢复前状态（双向恢复链）。
func TestRestoreChainUsesPreRestoreSnapshotAsNewBackup(t *testing.T) {
	env := testEnv(t)
	a := filepath.Join(env.Home, "claude", "settings.json")
	b := filepath.Join(env.Home, "claude", "claude.json")
	official := []byte("{\"tier\":\"official\"}\n")
	gateway := []byte("{\"tier\":\"gateway\"}\n")
	// 备份之后由外部编辑器新增的自定义字段（恢复会把它覆盖掉）。
	custom := []byte("{\"tier\":\"gateway\",\"customField\":\"keep-me\"}\n")
	other := []byte("{\"other\":true}\n")
	mustWriteFile(t, a, official)
	mustWriteFile(t, b, other)

	reg := NewRegistry(env, fixedPaths(a, b), testClock())
	applyRes, err := applyThroughTx(t, reg, "claude", map[string][]byte{a: gateway, b: other}, ReasonApply)
	if err != nil {
		t.Fatalf("apply 失败: %v", err)
	}
	if len(applyRes.BackupIDs) != 1 {
		t.Fatalf("apply 应记录一份备份: %+v", applyRes.BackupIDs)
	}
	// 所选旧备份 = apply 的记账备份，它记录的是**写前**字节（official）。
	oldBackup := applyRes.BackupIDs[0]

	// 备份后文件被外部改写（新增自定义字段）。
	mustWriteFile(t, a, custom)

	// ── 取消路径：只预览不应用 ──
	plan, err := reg.PlanRestoreDetailed("claude", oldBackup)
	if err != nil {
		t.Fatalf("展开恢复计划失败: %v", err)
	}
	listed := false
	for _, f := range plan.Files {
		if f.Path != a {
			continue
		}
		listed = true
		if got := tx.HashBytes(f.Before); got != tx.HashBytes(custom) {
			t.Errorf("预览的写前字节应为当前字节（含自定义字段）: got=%s want=%s", got, tx.HashBytes(custom))
		}
		if !bytes.Equal(f.After, official) {
			t.Errorf("预览的目标字节应为所选旧备份的字节: %q", f.After)
		}
	}
	if !listed {
		t.Fatalf("恢复预览必须列出受管文件（自定义字段将被覆盖）: %+v", plan.Files)
	}
	assertBytes(t, a, custom) // 取消 ⇒ 自定义字段原样保留

	// ── 确认路径：恢复 ⇒ 文件等于所选旧备份；恢复前的新快照含自定义字段 ──
	res := restoreThroughTx(t, reg, "claude", oldBackup)
	assertBytes(t, a, official)
	if len(res.BackupIDs) != 1 {
		t.Fatalf("恢复应记录一份「恢复前快照」: %+v", res.BackupIDs)
	}
	pre := res.BackupIDs[0]
	rec, err := reg.Get("claude", pre)
	if err != nil {
		t.Fatalf("回读恢复前快照失败: %v", err)
	}
	if rec.Reason != ReasonRestore {
		t.Errorf("恢复前快照的 reason = %q，期望 %q", rec.Reason, ReasonRestore)
	}
	recorded := false
	for _, f := range rec.Files {
		if f.Path == a {
			recorded = true
			if f.SHA256 != tx.HashBytes(custom) {
				t.Errorf("恢复前新快照必须含自定义字段: sha=%s want=%s", f.SHA256, tx.HashBytes(custom))
			}
		}
	}
	if !recorded {
		t.Fatalf("恢复前快照必须记录受管文件: %+v", rec.Files)
	}

	// ── 再恢复：对 restore 产生的新快照再次恢复 ⇒ 自定义字段回来（双向恢复链）──
	restoreThroughTx(t, reg, "claude", pre)
	assertBytes(t, a, custom)
	// 其他文件不变。
	assertBytes(t, b, other)
}

// ── 边界：备份损坏/缺失 ⇒ backup_unreadable 且盘上零变化 ─────────────────────

// TestRestoreRefusesUnreadableBackupWithoutWritingDisk 覆盖 UC-004 的三条边界：
// 备份文件本身损坏或不可读 → backup_unreadable，不部分恢复，盘上零变化。
func TestRestoreRefusesUnreadableBackupWithoutWritingDisk(t *testing.T) {
	cases := []struct {
		name   string
		damage func(t *testing.T, dir string, entry string)
	}{
		{
			name: "载荷缺失",
			damage: func(t *testing.T, dir, entry string) {
				if err := os.Remove(payloadPath(dir, entry)); err != nil {
					t.Fatalf("删载荷失败: %v", err)
				}
			},
		},
		{
			name: "载荷非 JSON",
			damage: func(t *testing.T, dir, entry string) {
				if err := os.WriteFile(payloadPath(dir, entry), []byte("{ broken"), 0o600); err != nil {
					t.Fatalf("破坏载荷失败: %v", err)
				}
			},
		},
		{
			name: "载荷被截断",
			damage: func(t *testing.T, dir, entry string) {
				raw := readOrNil(t, payloadPath(dir, entry))
				if err := os.WriteFile(payloadPath(dir, entry), raw[:len(raw)/2], 0o600); err != nil {
					t.Fatalf("截断载荷失败: %v", err)
				}
			},
		},
		{
			name: "载荷字节被改写（sha256 对不上）",
			damage: func(t *testing.T, dir, entry string) {
				path := payloadPath(dir, entry)
				var env envelope
				if err := json.Unmarshal(readOrNil(t, path), &env); err != nil {
					t.Fatalf("解析载荷失败: %v", err)
				}
				data, err := base64.StdEncoding.DecodeString(env.Entries[0].Data)
				if err != nil {
					t.Fatalf("解码失败: %v", err)
				}
				data[0] ^= 0x01
				env.Entries[0].Data = base64.StdEncoding.EncodeToString(data)
				raw, err := json.Marshal(env)
				if err != nil {
					t.Fatalf("编码失败: %v", err)
				}
				if err := os.WriteFile(path, raw, 0o600); err != nil {
					t.Fatalf("写坏载荷失败: %v", err)
				}
			},
		},
		{
			name: "索引与载荷不一致",
			damage: func(t *testing.T, dir, entry string) {
				path := filepath.Join(dir, indexFileName)
				var doc indexDoc
				if err := json.Unmarshal(readOrNil(t, path), &doc); err != nil {
					t.Fatalf("解析索引失败: %v", err)
				}
				for i := range doc.Entries {
					if doc.Entries[i].BackupID != entry {
						continue
					}
					doc.Entries[i].Files[0].SHA256 = tx.HashBytes([]byte("别的字节"))
				}
				raw, err := json.Marshal(doc)
				if err != nil {
					t.Fatalf("编码失败: %v", err)
				}
				if err := os.WriteFile(path, raw, 0o600); err != nil {
					t.Fatalf("写坏索引失败: %v", err)
				}
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			env := testEnv(t)
			live := filepath.Join(env.Home, "hermes")
			cfg := filepath.Join(live, "config.yaml")
			mustWriteFile(t, cfg, []byte("tier: official\n"))

			reg := NewRegistry(env, fixedPaths(cfg), testClock())
			res, err := applyThroughTx(t, reg, "hermes", map[string][]byte{cfg: []byte("tier: gateway\n")}, ReasonApply)
			if err != nil {
				t.Fatalf("apply 失败: %v", err)
			}
			dir := toolDir(env, "hermes")
			before := treeDigest(t, live)
			beforePage, err := reg.List("hermes", ListOptions{})
			if err != nil {
				t.Fatalf("列举失败: %v", err)
			}

			tc.damage(t, dir, res.BackupIDs[0])

			files, err := reg.PlanRestore("hermes", res.BackupIDs[0])
			if err == nil {
				t.Fatalf("损坏的备份不应展开出计划: %+v", files)
			}
			assertCode(t, err, apierr.CodeBackupUnreadable)
			if len(files) != 0 {
				t.Fatalf("失败时不得返回部分计划: %+v", files)
			}
			assertDigestEqual(t, before, treeDigest(t, live))
			if after, err := reg.List("hermes", ListOptions{}); err != nil || after.Total != beforePage.Total {
				t.Fatalf("失败路径不应改写备份记录: before=%d after=%d err=%v", beforePage.Total, after.Total, err)
			}
		})
	}
}
