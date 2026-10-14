// Package tx 的单元测试：预览租约、指纹乐观并发、原子写、按文件逆序回滚。
//
// 这些断言直接对应用例里的验收契约：
//   - UC-003 A4 / UC-014 A5：外改 → config_changed 且一个字节都不写；
//   - UC-003 A6 / UC-014 A8：过期与非法是两个互不串的码；
//   - UC-003 A7：同档位 apply 幂等（unchanged，且不写盘）；
//   - UC-003 A8 / UC-014 A6：写失败 → config_invalid + 已写文件逆序恢复为写前字节；
//   - UC-005 A5：写盘走同目录临时文件 + rename（不残留临时文件）；
//   - UC-003 A10 / UC-014 A9：受管路径是目录 → path_conflict（不是 permission_denied）。
//
// 全部落盘都在 t.TempDir() 下，不触碰任何真实配置。
package tx

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/peterplaykkk/yzagmgr/internal/apierr"
	"github.com/peterplaykkk/yzagmgr/internal/family"
)

// ── 夹具 ────────────────────────────────────────────────────────────────────

type fakeClock struct{ at time.Time }

func newClock() *fakeClock { return &fakeClock{at: time.Date(2026, 9, 22, 18, 0, 0, 0, time.UTC)} }

func (c *fakeClock) now() time.Time          { return c.at }
func (c *fakeClock) advance(d time.Duration) { c.at = c.at.Add(d) }

// recordingBackuper 记录备份调用，并可按需注入失败（storage 分支）。
type recordingBackuper struct {
	calls  int
	toolID string
	reason string
	files  []family.FilePlan
	id     string
	err    error
}

func (b *recordingBackuper) Record(toolID, reason string, files []family.FilePlan) (string, error) {
	b.calls++
	b.toolID = toolID
	b.reason = reason
	b.files = files
	return b.id, b.err
}

// writeFile 造一个受管文件，返回绝对路径。
func writeFile(t *testing.T, dir, name, content string, mode os.FileMode) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		t.Fatalf("建目录失败: %v", err)
	}
	if err := os.WriteFile(p, []byte(content), mode); err != nil {
		t.Fatalf("写文件失败: %v", err)
	}
	return p
}

func readFile(t *testing.T, p string) string {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatalf("读 %s 失败: %v", p, err)
	}
	return string(b)
}

// planFor 造一个「盘上已有 = before，目标 = after」的计划。
func planFor(t *testing.T, path, before, after string) family.FilePlan {
	t.Helper()
	b, err := ReadForFingerprint(path)
	if err != nil {
		if apierr.From(err).Code != apierr.CodePathConflict {
			t.Fatalf("读写前字节失败: %v", err)
		}
		b = nil
	}
	return family.FilePlan{
		Path:    path,
		Existed: b != nil,
		Before:  b,
		After:   []byte(after),
	}
}

func errCode(t *testing.T, err error) apierr.Code {
	t.Helper()
	if err == nil {
		t.Fatalf("期望一个错误，实际 nil")
	}
	return apierr.From(err).Code
}

// tempResidue 报告目录下残留的本产品临时文件（UC-005 A4 / UC-013 A7）。
func tempResidue(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("读目录 %s 失败: %v", dir, err)
	}
	var out []string
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".yzagmgr-tmp-") {
			out = append(out, e.Name())
		}
	}
	return out
}

// ── 租约 ────────────────────────────────────────────────────────────────────

func TestCachePutFillsIDExpiryAndFingerprints(t *testing.T) {
	dir := t.TempDir()
	existing := writeFile(t, dir, "a.json", `{"a":1}`, 0o600)
	missing := filepath.Join(dir, "b.json")

	clock := newClock()
	cache := NewCache(90*time.Second, clock.now)
	lease := cache.Put(&Lease{
		Operation: "apply",
		ToolID:    "claude",
		Files: []family.FilePlan{
			planFor(t, existing, `{"a":1}`, `{"a":2}`),
			{Path: missing, Existed: false, Before: nil, After: []byte("new")},
		},
	})

	if !strings.HasPrefix(lease.ID, "pv_") || lease.ID == "" {
		t.Errorf("ID = %q，期望 pv_ 前缀的非空标识", lease.ID)
	}
	if !lease.CreatedAt.Equal(clock.now()) {
		t.Errorf("CreatedAt = %v，期望注入时钟的 %v", lease.CreatedAt, clock.now())
	}
	if got := lease.ExpiresAt.Sub(lease.CreatedAt); got != 90*time.Second {
		t.Errorf("有效期 = %v，期望 90s", got)
	}
	if got := lease.Fingerprints[existing]; got != HashBytes([]byte(`{"a":1}`)) {
		t.Errorf("既有文件指纹 = %q，期望写前字节的 sha256", got)
	}
	// 缺失文件记空字节指纹（§F-T7）：缺席是合法状态，不是「无指纹」。
	if got := lease.Fingerprints[missing]; got != EmptyHash {
		t.Errorf("缺失文件指纹 = %q，期望 EmptyHash %q", got, EmptyHash)
	}

	// 同一时刻的第二份租约必须拿到不同 ID（单次消费靠它区分）。
	other := cache.Put(&Lease{ToolID: "codex"})
	if other.ID == lease.ID {
		t.Errorf("同一时刻的两份租约 ID 相同: %q", lease.ID)
	}
	if other.Fingerprints == nil {
		t.Errorf("无文件的租约也要有非 nil 的指纹映射")
	}
	if got := cache.Len(); got != 2 {
		t.Errorf("在途租约数 = %d，期望 2", got)
	}
}

func TestCacheTakeIsSingleUse(t *testing.T) {
	cache := NewCache(time.Minute, newClock().now)
	lease := cache.Put(&Lease{ToolID: "claude"})

	got, err := cache.Take(lease.ID)
	if err != nil {
		t.Fatalf("首次 Take 失败: %v", err)
	}
	if got != lease {
		t.Errorf("Take 返回的不是同一份租约")
	}
	if cache.Len() != 0 {
		t.Errorf("取出后在途租约数 = %d，期望 0", cache.Len())
	}
	if _, err := cache.Take(lease.ID); err == nil {
		t.Errorf("同一 ID 第二次必须被拒绝（一次性语义）")
	}
}

// TestPreviewExpiryDistinctFromInvalidID 守住 UC-003 A6 / UC-014 A8：
// 未知 id → invalid_request；已过期 → preview_expired。两码不可互串。
func TestPreviewExpiryDistinctFromInvalidID(t *testing.T) {
	clock := newClock()
	cache := NewCache(60*time.Second, clock.now)

	// 未知 id。
	_, err := cache.Take("pv_nope")
	if code := errCode(t, err); code != apierr.CodeInvalidRequest {
		t.Fatalf("未知 id 的码 = %q，期望 invalid_request", code)
	}

	// 已过期（超过有效期）。
	expired := cache.Put(&Lease{ToolID: "claude"})
	clock.advance(61 * time.Second)
	_, err = cache.Take(expired.ID)
	if code := errCode(t, err); code != apierr.CodePreviewExpired {
		t.Fatalf("过期租约的码 = %q，期望 preview_expired", code)
	}

	// 两个码必须真的不同（否则界面无法区分「重新预览」与「非法 id」）。
	if apierr.CodePreviewExpired == apierr.CodeInvalidRequest {
		t.Fatalf("过期码与非法码相同，契约要求两者可区分")
	}
	// 过期后同一 ID 再取仍是 invalid（已从表中移除），而不是又变成 expired。
	if code := errCode(t, func() error { _, e := cache.Take(expired.ID); return e }()); code != apierr.CodeInvalidRequest {
		t.Errorf("取过一次的过期 ID 第二次 = %q，期望 invalid_request", code)
	}
	// 未知 id 永远不会被读成 expired（依赖表中存在与否，而不是时间）。
	clock.advance(24 * time.Hour)
	if code := errCode(t, func() error { _, e := cache.Take("pv_still_unknown"); return e }()); code != apierr.CodeInvalidRequest {
		t.Errorf("未知 id 的码 = %q，期望始终 invalid_request", code)
	}

	// 边界：恰好到 ExpiresAt 时仍有效（After 判定，半开区间）。
	edge := cache.Put(&Lease{ToolID: "codex"})
	clock.advance(60 * time.Second)
	if _, err := cache.Take(edge.ID); err != nil {
		t.Errorf("恰好到期的租约仍应在有效期内: %v", err)
	}
	// 再多一个纳秒即过期。
	tick := cache.Put(&Lease{ToolID: "codex"})
	clock.advance(60*time.Second + time.Nanosecond)
	if code := errCode(t, func() error { _, e := cache.Take(tick.ID); return e }()); code != apierr.CodePreviewExpired {
		t.Errorf("超过有效期一个纳秒的码 = %q，期望 preview_expired", code)
	}
}

func TestCacheDefaultTTLAndClock(t *testing.T) {
	clock := newClock()
	for _, ttl := range []time.Duration{0, -time.Second} {
		cache := NewCache(ttl, clock.now)
		l := cache.Put(&Lease{})
		if got := l.ExpiresAt.Sub(l.CreatedAt); got != 180*time.Second {
			t.Errorf("ttl=%v 时有效期 = %v，期望默认 180s", ttl, got)
		}
	}

	// clock 为 nil 时退回 time.Now：有效期末必须落在「现在 + ttl」附近。
	wall := NewCache(time.Minute, nil).Put(&Lease{})
	if d := wall.ExpiresAt.Sub(time.Now()); d > time.Minute || d < time.Minute-time.Second {
		t.Errorf("真实时钟下的剩余有效期 = %v，期望约 60s", d)
	}
}

func TestHashBytesAndEmptyHash(t *testing.T) {
	// 空字节的 sha256 是公开已知值，独立于本实现（防止有人改成别的常量）。
	sum := sha256.Sum256(nil)
	if EmptyHash != hex.EncodeToString(sum[:]) {
		t.Errorf("EmptyHash = %q，与空字节 sha256 %q 不符", EmptyHash, hex.EncodeToString(sum[:]))
	}
	if got := HashBytes(nil); got != EmptyHash {
		t.Errorf("HashBytes(nil) = %q，期望 EmptyHash", got)
	}
	if got := HashBytes([]byte{}); got != EmptyHash {
		t.Errorf("HashBytes(空切片) = %q，期望 EmptyHash", got)
	}
	// 已知向量："abc" → ba7816bf...
	const abcHash = "ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad"
	if got := HashBytes([]byte("abc")); got != abcHash {
		t.Errorf("HashBytes(abc) = %q，期望 %q", got, abcHash)
	}
	if HashBytes([]byte("abc")) == HashBytes([]byte("abd")) {
		t.Errorf("不同字节产生了相同指纹")
	}
}

func TestReadForFingerprint(t *testing.T) {
	dir := t.TempDir()

	t.Run("文件不存在返回空字节且不报错", func(t *testing.T) {
		b, err := ReadForFingerprint(filepath.Join(dir, "missing.json"))
		if err != nil {
			t.Fatalf("缺席路径不得报错（UC-003 A11）: %v", err)
		}
		if b != nil {
			t.Errorf("缺席路径返回 %q，期望 nil", b)
		}
	})

	t.Run("普通文件返回内容", func(t *testing.T) {
		p := writeFile(t, dir, "a.json", `{"a":1}`, 0o600)
		b, err := ReadForFingerprint(p)
		if err != nil {
			t.Fatalf("读文件失败: %v", err)
		}
		if string(b) != `{"a":1}` {
			t.Errorf("内容 = %q", b)
		}
	})

	t.Run("空文件是空字节而不是错误", func(t *testing.T) {
		p := writeFile(t, dir, "empty.json", "", 0o600)
		b, err := ReadForFingerprint(p)
		if err != nil {
			t.Fatalf("空文件不得报错: %v", err)
		}
		if HashBytes(b) != EmptyHash {
			t.Errorf("空文件的指纹 = %q，期望 EmptyHash", HashBytes(b))
		}
	})

	// UC-003 A10 / UC-014 A9：目录被当文件读必须是 path_conflict，
	// 不能是 permission_denied（v1 的 EISDIR 历史事故就是错在这里）。
	t.Run("路径是目录返回 path_conflict", func(t *testing.T) {
		sub := filepath.Join(dir, "configdir")
		if err := os.Mkdir(sub, 0o700); err != nil {
			t.Fatalf("建目录失败: %v", err)
		}
		_, err := ReadForFingerprint(sub)
		if code := errCode(t, err); code != apierr.CodePathConflict {
			t.Fatalf("目录路径的码 = %q，期望 path_conflict（不是 permission_denied）", code)
		}
		if code := apierr.From(err).Code; code == apierr.CodePermissionDenied {
			t.Errorf("不得报 permission_denied：会把「路径形状错」误诊成「权限问题」")
		}
	})

	t.Run("符号链接跟随到目标", func(t *testing.T) {
		target := writeFile(t, dir, "real.json", "real", 0o600)
		link := filepath.Join(dir, "link.json")
		if err := os.Symlink(target, link); err != nil {
			t.Fatalf("建符号链接失败: %v", err)
		}
		b, err := ReadForFingerprint(link)
		if err != nil {
			t.Fatalf("链接文件读取失败: %v", err)
		}
		if string(b) != "real" {
			t.Errorf("跟随链接读到 %q，期望 real", b)
		}

		dirLink := filepath.Join(dir, "dirlink")
		if err := os.Symlink(filepath.Join(dir, "configdir"), dirLink); err != nil {
			t.Fatalf("建目录链接失败: %v", err)
		}
		_, err = ReadForFingerprint(dirLink)
		if code := errCode(t, err); code != apierr.CodePathConflict {
			t.Errorf("指向目录的链接 = %q，期望 path_conflict", code)
		}
	})

	// 非「不存在」的 IO 错误必须向上抛（只有缺席才是合法状态）。
	t.Run("非法路径的 IO 错误被上报", func(t *testing.T) {
		_, err := ReadForFingerprint(dir + "/bad\x00name")
		if err == nil {
			t.Fatalf("含 NUL 的路径必须报错")
		}
		if code := apierr.From(err).Code; code != apierr.CodePermissionDenied {
			t.Errorf("IO 错误的码 = %q，期望 permission_denied（真实 IO 错误的上报口径）", code)
		}
	})

	t.Run("能 Stat 但不能读的路径被上报", func(t *testing.T) {
		// unix socket：Stat 成功（既不是目录也不是普通文件），打开读取必然失败。
		// 用系统临时目录的短路径：sun_path 有 104 字节上限，t.TempDir() 的长路径会 bind 失败。
		sockDir, err := os.MkdirTemp("", "yzagmgr-sock")
		if err != nil {
			t.Fatalf("建临时目录失败: %v", err)
		}
		defer func() { _ = os.RemoveAll(sockDir) }()

		sock := filepath.Join(sockDir, "s")
		ln, err := net.Listen("unix", sock)
		if err != nil {
			t.Fatalf("建 unix socket 失败: %v", err)
		}
		defer func() { _ = ln.Close() }()

		_, err = ReadForFingerprint(sock)
		if code := errCode(t, err); code != apierr.CodePermissionDenied {
			t.Errorf("socket 路径的码 = %q，期望 permission_denied", code)
		}
	})
}

// ── Apply：指纹与幂等 ───────────────────────────────────────────────────────

// TestApplyRejectsExternalModification 守住 UC-003 A4 / UC-014 A5：
// 指纹不符 → config_changed，且**一个字节都不写**。
func TestApplyRejectsExternalModification(t *testing.T) {
	cases := []struct {
		name   string
		damage func(t *testing.T, p string)
		// wantCode：内容/存在性被改 → config_changed；
		// 「被换成目录」在指纹读取阶段就报 path_conflict（更具体的诊断，见返回说明）。
		wantCode apierr.Code
	}{
		{"内容被改", func(t *testing.T, p string) {
			if err := os.WriteFile(p, []byte(`{"a":999}`), 0o600); err != nil {
				t.Fatalf("外改失败: %v", err)
			}
		}, apierr.CodeConfigChanged},
		{"文件被删", func(t *testing.T, p string) {
			if err := os.Remove(p); err != nil {
				t.Fatalf("外删失败: %v", err)
			}
		}, apierr.CodeConfigChanged},
		{"文件被换成目录", func(t *testing.T, p string) {
			if err := os.Remove(p); err != nil {
				t.Fatalf("删除失败: %v", err)
			}
			if err := os.Mkdir(p, 0o700); err != nil {
				t.Fatalf("建目录失败: %v", err)
			}
		}, apierr.CodePathConflict},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			target := writeFile(t, dir, "target.json", `{"a":1}`, 0o600)
			other := writeFile(t, dir, "other.json", `{"b":1}`, 0o600)
			newFile := filepath.Join(dir, "new.json")

			lease := NewCache(time.Minute, newClock().now).Put(&Lease{
				Operation: "apply",
				ToolID:    "claude",
				Files: []family.FilePlan{
					planFor(t, target, `{"a":1}`, `{"a":2}`),
					planFor(t, other, `{"b":1}`, `{"b":2}`),
					{Path: newFile, Existed: false, Before: nil, After: []byte("created")},
				},
			})

			tc.damage(t, target)
			bk := &recordingBackuper{id: "bk_1"}
			res, err := Apply(lease, Options{Backuper: bk, Reason: "tool.apply"})

			if code := errCode(t, err); code != tc.wantCode {
				t.Fatalf("码 = %q，期望 %q", code, tc.wantCode)
			}
			// 一个字节都不写：未注入的路径也不得被创建或改动。
			if got := readFile(t, other); got != `{"b":1}` {
				t.Errorf("未受影响的文件被写了: %q", got)
			}
			if _, statErr := os.Stat(newFile); !os.IsNotExist(statErr) {
				t.Errorf("新文件被提前创建（应当整体拒绝）")
			}
			if bk.calls != 0 {
				t.Errorf("指纹校验失败时不得记备份，实际调用 %d 次", bk.calls)
			}
			if res.Files != nil {
				t.Errorf("被拒绝的事务不应带 per-file 结果: %+v", res.Files)
			}
			if tc.wantCode == apierr.CodeConfigChanged {
				// details 里要能定位是哪个文件被外改。
				var detailPath string
				if d := apierr.From(err).Details; d != nil {
					detailPath, _ = d["path"].(string)
				}
				if detailPath != target {
					t.Errorf("details.path = %q，期望受影响的 %q", detailPath, target)
				}
			}
		})
	}
}

// TestApplyIsIdempotentOnSameTier 守住 UC-003 A7：目标 == 写前 → unchanged 且不写盘。
func TestApplyIsIdempotentOnSameTier(t *testing.T) {
	dir := t.TempDir()
	same := writeFile(t, dir, "same.json", `{"a":1}`, 0o600)

	// 把目录改成不可写：只要实现真的「不写盘」，这次 Apply 就必须成功。
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatalf("改目录权限失败: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })

	lease := NewCache(time.Minute, newClock().now).Put(&Lease{
		ToolID: "claude",
		Files:  []family.FilePlan{planFor(t, same, `{"a":1}`, `{"a":1}`)},
	})
	bk := &recordingBackuper{id: "bk_1"}
	res, err := Apply(lease, Options{Backuper: bk, Reason: "tool.apply"})
	if err != nil {
		t.Fatalf("全 unchanged 的事务不得因目录只读而失败（说明它写了盘）: %v", err)
	}
	if res.Status != StatusUnchanged {
		t.Errorf("事务状态 = %q，期望 %q", res.Status, StatusUnchanged)
	}
	if len(res.Files) != 1 || res.Files[0].Status != FileUnchanged {
		t.Errorf("per-file 状态 = %+v，期望 unchanged", res.Files)
	}
	if got := readFile(t, same); got != `{"a":1}` {
		t.Errorf("文件被改了: %q", got)
	}
	// 与 v1 一致：unchanged 也记备份（用户回滚的依据是「应用前」的字节）。
	if bk.calls != 1 {
		t.Errorf("unchanged 事务的备份调用次数 = %d，期望 1", bk.calls)
	}

	// 混合：一个要改、一个不变 → 事务 applied，逐文件状态分明。
	dir2 := t.TempDir()
	changed := writeFile(t, dir2, "changed.json", "old", 0o600)
	untouched := writeFile(t, dir2, "untouched.json", "keep", 0o600)
	lease2 := NewCache(time.Minute, newClock().now).Put(&Lease{
		ToolID: "claude",
		Files: []family.FilePlan{
			planFor(t, changed, "old", "new"),
			planFor(t, untouched, "keep", "keep"),
		},
	})
	res2, err := Apply(lease2, Options{Backuper: &recordingBackuper{id: "bk_2"}})
	if err != nil {
		t.Fatalf("Apply 失败: %v", err)
	}
	if res2.Status != StatusApplied {
		t.Errorf("事务状态 = %q，期望 %q", res2.Status, StatusApplied)
	}
	if res2.Files[0].Status != FileWritten || res2.Files[1].Status != FileUnchanged {
		t.Errorf("逐文件状态 = %+v，期望 [written unchanged]", res2.Files)
	}
	if got := readFile(t, changed); got != "new" {
		t.Errorf("目标文件内容 = %q，期望 new", got)
	}
	if got := readFile(t, untouched); got != "keep" {
		t.Errorf("unchanged 文件被改: %q", got)
	}
}

// ── Apply：写入与权限 ───────────────────────────────────────────────────────

func TestApplyWritePreservesExistingModeAndCreatesMissing(t *testing.T) {
	dir := t.TempDir()
	existing := writeFile(t, dir, "keep-mode.json", "before", 0o640)
	newDir := filepath.Join(dir, "nested", "deep")
	fresh := filepath.Join(newDir, "fresh.json")

	lease := NewCache(time.Minute, newClock().now).Put(&Lease{
		ToolID: "claude",
		Files: []family.FilePlan{
			planFor(t, existing, "before", "after"),
			{Path: fresh, Existed: false, Before: nil, After: []byte("created")},
		},
	})
	res, err := Apply(lease, Options{WriteMode: 0o600, Backuper: &recordingBackuper{id: "bk_1"}})
	if err != nil {
		t.Fatalf("Apply 失败: %v", err)
	}
	if res.Status != StatusApplied {
		t.Errorf("状态 = %q，期望 applied", res.Status)
	}
	if len(res.BackupIDs) != 1 || res.BackupIDs[0] != "bk_1" {
		t.Errorf("backupIds = %v，期望 [bk_1]", res.BackupIDs)
	}

	// 既有文件沿用原权限（不能把用户刻意放宽的权限改回 0600）。
	st, err := os.Stat(existing)
	if err != nil {
		t.Fatalf("Stat 失败: %v", err)
	}
	if perm := st.Mode().Perm(); perm != 0o640 {
		t.Errorf("既有文件权限 = %#o，期望保持 0640", perm)
	}
	if got := readFile(t, existing); got != "after" {
		t.Errorf("既有文件内容 = %q，期望 after", got)
	}

	// 新文件用 WriteMode，父目录按 0700 建出。
	fst, err := os.Stat(fresh)
	if err != nil {
		t.Fatalf("新文件未落盘: %v", err)
	}
	if perm := fst.Mode().Perm(); perm != 0o600 {
		t.Errorf("新文件权限 = %#o，期望 0600", perm)
	}
	dst, err := os.Stat(newDir)
	if err != nil {
		t.Fatalf("父目录未建出: %v", err)
	}
	if perm := dst.Mode().Perm(); perm != 0o700 {
		t.Errorf("新建父目录权限 = %#o，期望 0700", perm)
	}
	if got := readFile(t, fresh); got != "created" {
		t.Errorf("新文件内容 = %q", got)
	}

	// WriteMode 为 0 时退回 0600 默认值。
	d2 := t.TempDir()
	defaulted := filepath.Join(d2, "d.json")
	lease2 := NewCache(time.Minute, newClock().now).Put(&Lease{
		ToolID: "codex",
		Files:  []family.FilePlan{{Path: defaulted, Existed: false, Before: nil, After: []byte("x")}},
	})
	if _, err := Apply(lease2, Options{}); err != nil {
		t.Fatalf("Apply 失败: %v", err)
	}
	st2, err := os.Stat(defaulted)
	if err != nil {
		t.Fatalf("Stat 失败: %v", err)
	}
	if perm := st2.Mode().Perm(); perm != 0o600 {
		t.Errorf("默认 WriteMode 下权限 = %#o，期望 0600", perm)
	}

	// 二进制字节也必须逐字节落盘（不是文本）。
	bin := filepath.Join(d2, "bin.dat")
	payload := []byte{0x00, 0x01, 0xff, 0xfe, '\n', 0x7f}
	lease3 := NewCache(time.Minute, newClock().now).Put(&Lease{
		ToolID: "codex",
		Files:  []family.FilePlan{{Path: bin, Existed: false, Before: nil, After: payload}},
	})
	if _, err := Apply(lease3, Options{}); err != nil {
		t.Fatalf("Apply 失败: %v", err)
	}
	got, err := os.ReadFile(bin)
	if err != nil {
		t.Fatalf("读回失败: %v", err)
	}
	if string(got) != string(payload) {
		t.Errorf("二进制内容 = %v，期望 %v", got, payload)
	}
}

func TestApplyDeleteSemantics(t *testing.T) {
	dir := t.TempDir()
	created := writeFile(t, dir, "taken-over.json", "managed", 0o600)
	absent := filepath.Join(dir, "never-existed.json")

	lease := NewCache(time.Minute, newClock().now).Put(&Lease{
		Operation: "restore",
		ToolID:    "claude",
		Files: []family.FilePlan{
			{Path: created, Existed: true, Delete: true, Before: []byte("managed"), After: nil},
			{Path: absent, Existed: false, Delete: true, Before: nil, After: nil},
		},
	})
	res, err := Apply(lease, Options{Backuper: &recordingBackuper{id: "bk_1"}})
	if err != nil {
		t.Fatalf("Apply 失败: %v", err)
	}
	if _, statErr := os.Stat(created); !os.IsNotExist(statErr) {
		t.Errorf("Existed=true 的 Delete 计划必须删掉文件")
	}
	if res.Files[0].Status != FileDeleted {
		t.Errorf("被删文件状态 = %q，期望 deleted", res.Files[0].Status)
	}
	// 从未存在过的路径没有东西可删：记 unchanged，不算改动。
	if res.Files[1].Status != FileUnchanged {
		t.Errorf("缺席路径的 Delete 状态 = %q，期望 unchanged", res.Files[1].Status)
	}
	if res.Status != StatusApplied {
		t.Errorf("事务状态 = %q，期望 applied（确实删掉了一个文件）", res.Status)
	}

	// 只有「无文件可删」的计划 → 事务 unchanged。
	lease2 := NewCache(time.Minute, newClock().now).Put(&Lease{
		Operation: "restore",
		ToolID:    "claude",
		Files:     []family.FilePlan{{Path: absent, Existed: false, Delete: true}},
	})
	res2, err := Apply(lease2, Options{Backuper: &recordingBackuper{id: "bk_2"}})
	if err != nil {
		t.Fatalf("Apply 失败: %v", err)
	}
	if res2.Status != StatusUnchanged || res2.Files[0].Status != FileUnchanged {
		t.Errorf("无文件可删时 = %q/%+v，期望 unchanged/unchanged", res2.Status, res2.Files)
	}

	// 幂等：对已被删掉的路径再删一次不得报错（目标已达成）。
	again := NewCache(time.Minute, newClock().now).Put(&Lease{
		Operation: "restore",
		ToolID:    "claude",
		Files:     []family.FilePlan{{Path: created, Existed: true, Delete: true}},
	})
	res3, err := Apply(again, Options{})
	if err != nil {
		t.Fatalf("重复删除不得报错: %v", err)
	}
	if res3.Files[0].Status != FileDeleted {
		t.Errorf("重复删除的状态 = %q，期望 deleted（目标状态已达成）", res3.Files[0].Status)
	}
}

// ── Apply：失败与回滚 ───────────────────────────────────────────────────────

// TestFailedApplyRollsBackEveryWrittenFile 守住 UC-003 A8 / UC-014 A6：
// 最后一条受管文件不可写 → 事务不提交、已写文件按**逆序**恢复为写前字节、
// 错误码 config_invalid、未注入路径的最终字节 == 应用前。
func TestFailedApplyRollsBackEveryWrittenFile(t *testing.T) {
	cases := []struct {
		name string
		// order 决定不可写目标在计划里的位置："last" / "middle"。
		order string
	}{
		{"末位失败", "last"},
		{"中途失败", "middle"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			a := writeFile(t, dir, "a.json", `{"a":1}`, 0o600)
			c := writeFile(t, dir, "c.json", `{"c":1}`, 0o600)
			b := filepath.Join(dir, "b.json") // 应用时要新建的文件

			// 不可写目标：父目录 0555（不能创建临时文件 → 写入必失败）。
			lockDir := filepath.Join(dir, "locked")
			if err := os.Mkdir(lockDir, 0o700); err != nil {
				t.Fatalf("建目录失败: %v", err)
			}
			locked := filepath.Join(lockDir, "d.json")
			if err := os.Chmod(lockDir, 0o500); err != nil {
				t.Fatalf("改权限失败: %v", err)
			}
			t.Cleanup(func() { _ = os.Chmod(lockDir, 0o700) })

			planA := planFor(t, a, `{"a":1}`, `{"a":2}`)
			planB := family.FilePlan{Path: b, Existed: false, Before: nil, After: []byte(`{"b":2}`)}
			planC := planFor(t, c, `{"c":1}`, `{"c":2}`)
			planD := family.FilePlan{Path: locked, Existed: false, Before: nil, After: []byte(`{"d":2}`)}

			var files []family.FilePlan
			if tc.order == "last" {
				files = []family.FilePlan{planA, planB, planC, planD}
			} else {
				files = []family.FilePlan{planA, planD, planB, planC}
			}
			lease := NewCache(time.Minute, newClock().now).Put(&Lease{
				Operation: "apply",
				ToolID:    "claude",
				Files:     files,
			})

			bk := &recordingBackuper{id: "bk_1"}
			res, err := Apply(lease, Options{Backuper: bk, Reason: "tool.apply"})

			if code := errCode(t, err); code != apierr.CodeConfigInvalid {
				t.Fatalf("码 = %q，期望 config_invalid", code)
			}
			if res.Status != "" {
				t.Errorf("失败事务的状态 = %q，期望空（不得冒充 applied/unchanged）", res.Status)
			}
			// 备份仍然记录了（回滚的依据在备份里）。
			if bk.calls != 1 {
				t.Errorf("备份调用次数 = %d，期望 1", bk.calls)
			}

			// 未注入的路径最终字节 == 应用前。
			if got := readFile(t, a); got != `{"a":1}` {
				t.Errorf("已写文件未被恢复: %q", got)
			}
			if got := readFile(t, c); got != `{"c":1}` {
				t.Errorf("已写文件未被恢复: %q", got)
			}
			if _, statErr := os.Stat(b); !os.IsNotExist(statErr) {
				t.Errorf("新建文件未被回滚删除（最终字节应等于应用前：不存在）")
			}

			// 失败侧 files[].status 只允许回滚语义下的取值（契约 §4.3 / UC-003 A8）。
			// **不含 FileWritten**：那会让同一路径既报 written 又报 restored，调用方无从判断终态。
			allowed := map[string]bool{
				FileRestored: true, FileRestoreFailed: true, FileUnchanged: true,
			}
			// 每个路径**恰好一条**（不再有「写入阶段 + 回滚阶段」两条互相矛盾的记录）。
			seen := map[string]int{}
			restoredSet := map[string]bool{}
			for _, f := range res.Files {
				if !allowed[f.Status] {
					t.Errorf("失败侧出现越界状态 %q（路径 %s），契约只允许 restored/restore_failed/unchanged", f.Status, f.Path)
				}
				if f.Status == FileRestoreFailed {
					t.Errorf("本场景的恢复应当全部成功，%s 报 restore_failed", f.Path)
				}
				seen[f.Path]++
				if f.Status == FileRestored {
					restoredSet[f.Path] = true
				}
			}
			for path, n := range seen {
				if n != 1 {
					t.Errorf("路径 %s 在失败结果里出现 %d 次，应为 1 次", path, n)
				}
			}
			// 恢复集合：本场景写过的文件都应被恢复。
			var wantRestored []string
			if tc.order == "last" {
				wantRestored = []string{a, b, c}
			} else {
				wantRestored = []string{a}
			}
			for _, path := range wantRestored {
				if !restoredSet[path] {
					t.Errorf("写过且应被恢复的路径 %s 未出现在 restored 集合里（实际 %+v）", path, res.Files)
				}
			}
			// 没写过的路径必须是 unchanged。
			if st := statusOf(res.Files, locked); st != FileUnchanged {
				t.Errorf("失败路径 %s 的终态 = %q，期望 %q（磁盘未变）", locked, st, FileUnchanged)
			}

			// **逆序恢复**：files[] 按 lease 顺序、无法反推恢复顺序，故读 details.recoveryOrder。
			var wantOrder []string
			if tc.order == "last" {
				wantOrder = []string{c, b, a} // 写序 a,b,c → 恢复序 c,b,a
			} else {
				wantOrder = []string{a} // 只有 a 写过
			}
			gotOrder := toStrings(apierr.From(err).Details["recoveryOrder"])
			if len(gotOrder) != len(wantOrder) {
				t.Fatalf("details.recoveryOrder = %v，期望 %v（必须按写入的逆序）", gotOrder, wantOrder)
			}
			for i := range wantOrder {
				if gotOrder[i] != wantOrder[i] {
					t.Errorf("恢复顺序[%d] = %q，期望 %q（必须按写入的逆序）", i, gotOrder[i], wantOrder[i])
				}
			}

			// details 携带可定位信息。
			d := apierr.From(err).Details
			if d == nil {
				t.Fatalf("失败必须带 details")
			}
			if d["restoredAll"] != true {
				t.Errorf("details.restoredAll = %v，期望 true", d["restoredAll"])
			}
			if d["failedPath"] != locked {
				t.Errorf("details.failedPath = %v，期望 %q", d["failedPath"], locked)
			}
			if d["toolId"] != "claude" || d["operation"] != "apply" {
				t.Errorf("details 缺少 toolId/operation: %v", d)
			}

			// 收尾不得残留临时文件（含不可写目录之外的所有目录）。
			for _, d2 := range []string{dir, lockDir} {
				if res := tempResidue(t, d2); len(res) != 0 {
					t.Errorf("目录 %s 残留临时文件 %v", d2, res)
				}
			}
		})
	}
}

// TestFailedRollbackReportsPartialRestore 是 UC-093 A4 的**部分恢复夹具**：
// 回滚过程中恢复目标本身也不可写 ⇒ 终态必须是「恢复未完成」而不是成功：
//   - 该文件停在 restore_failed（磁盘上是**本次写入的新字节**，需人工处理）；
//   - details.restoredAll=false，details.needsAttention 恰列出该文件；
//   - 其余文件的终态如实（已恢复 / 未改变）；
//   - 顶层文案明确「回滚未完成」。
//
// 故障用**真实文件系统**构造：钩子在某文件写成功后把它的目录改成只读，回滚里的
// writeAtomic 因此真实失败——终态由真实 IO 产生，不是打桩伪造的。
func TestFailedRollbackReportsPartialRestore(t *testing.T) {
	base := t.TempDir()
	dirA := filepath.Join(base, "a")           // 回滚时恢复目标不可写的目录
	dirB := filepath.Join(base, "b")           // 正常目录
	dirLocked := filepath.Join(base, "locked") // 触发写入失败的目录

	a := writeFile(t, dirA, "a.json", `{"a":1}`, 0o600)
	b := writeFile(t, dirB, "b.json", `{"b":1}`, 0o600)
	if err := os.Mkdir(dirLocked, 0o700); err != nil {
		t.Fatalf("建目录失败: %v", err)
	}
	c := filepath.Join(dirLocked, "c.json")
	if err := os.Chmod(dirLocked, 0o500); err != nil {
		t.Fatalf("改权限失败: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(dirLocked, 0o700) })

	planA := planFor(t, a, `{"a":1}`, `{"a":2}`)
	planB := planFor(t, b, `{"b":1}`, `{"b":2}`)
	planC := family.FilePlan{Path: c, Existed: false, Before: nil, After: []byte(`{"c":2}`)}

	lease := NewCache(time.Minute, newClock().now).Put(&Lease{
		Operation: "apply",
		ToolID:    "claude",
		Files:     []family.FilePlan{planA, planB, planC},
	})

	// 受控触发：a 写成功后把它所在目录改成只读 ⇒ 逆序回滚先恢复 b（成功），
	// 再恢复 a 时 writeAtomic 真实失败（restore_failed）。
	triggered := false
	res, err := Apply(lease, Options{
		Backuper: &recordingBackuper{id: "bk_1"},
		faultAfterWrite: func(path string) {
			if path != a || triggered {
				return
			}
			triggered = true
			if cerr := os.Chmod(dirA, 0o500); cerr != nil {
				t.Errorf("夹具改权限失败: %v", cerr)
			}
		},
	})
	t.Cleanup(func() { _ = os.Chmod(dirA, 0o700) })
	if !triggered {
		t.Fatalf("夹具未被触发：a 写成功后钩子必须被调用")
	}

	// 事务终态：失败、config_invalid、文案明确「回滚未完成」（不显示成功）。
	if code := errCode(t, err); code != apierr.CodeConfigInvalid {
		t.Fatalf("码 = %q，期望 config_invalid", code)
	}
	if res.Status != "" {
		t.Errorf("失败事务的状态 = %q，期望空（不得冒充 applied/unchanged）", res.Status)
	}
	ae := apierr.From(err)
	if !strings.Contains(ae.Message, "回滚未完成") {
		t.Errorf("文案 = %q，期望明确「回滚未完成」", ae.Message)
	}

	// details：restoredAll=false + needsAttention 恰列出恢复失败的文件。
	d := ae.Details
	if d == nil {
		t.Fatalf("失败必须带 details")
	}
	if d["restoredAll"] != false {
		t.Errorf("details.restoredAll = %v，期望 false（存在恢复失败的文件）", d["restoredAll"])
	}
	if d["failedPath"] != c {
		t.Errorf("details.failedPath = %v，期望 %q", d["failedPath"], c)
	}
	if got := toStrings(d["needsAttention"]); len(got) != 1 || got[0] != a {
		t.Errorf("details.needsAttention = %v，期望 [%q]（需人工处理的对象清单）", got, a)
	}
	if got := toStrings(d["recoveryOrder"]); len(got) != 2 || got[0] != b || got[1] != a {
		t.Errorf("details.recoveryOrder = %v，期望 [%q %q]（逆序恢复）", got, b, a)
	}

	// 逐项终态（每个路径恰一条）。
	if st := statusOf(res.Files, a); st != FileRestoreFailed {
		t.Errorf("%s 的终态 = %q，期望 restore_failed", a, st)
	}
	if st := statusOf(res.Files, b); st != FileRestored {
		t.Errorf("%s 的终态 = %q，期望 restored", b, st)
	}
	if st := statusOf(res.Files, c); st != FileUnchanged {
		t.Errorf("%s 的终态 = %q，期望 unchanged（磁盘未变）", c, st)
	}

	// 磁盘真值：a 停在本次写入的新字节（这正是「需处理」的含义），
	// b 回到写前字节，c 根本没被创建。
	if got := readFile(t, a); got != `{"a":2}` {
		t.Errorf("a 的盘上字节 = %q，期望 %q（恢复失败 ⇒ 停在写入后的新字节）", got, `{"a":2}`)
	}
	if got := readFile(t, b); got != `{"b":1}` {
		t.Errorf("b 的盘上字节 = %q，期望 %q（已恢复写前字节）", got, `{"b":1}`)
	}
	if _, statErr := os.Stat(c); !os.IsNotExist(statErr) {
		t.Errorf("c 不应存在（写入失败即未创建）")
	}
}

// 全部恢复成功的场景必须保持既有的「已按文件回滚」文案与空 needsAttention
// （本夹具只改「回滚中再失败」一侧，成功一侧零回归）。
func TestSuccessfulRollbackKeepsMessageAndEmptyAttention(t *testing.T) {
	dir := t.TempDir()
	a := writeFile(t, dir, "a.json", `{"a":1}`, 0o600)
	lockDir := filepath.Join(dir, "locked")
	if err := os.Mkdir(lockDir, 0o700); err != nil {
		t.Fatalf("建目录失败: %v", err)
	}
	locked := filepath.Join(lockDir, "b.json")
	if err := os.Chmod(lockDir, 0o500); err != nil {
		t.Fatalf("改权限失败: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(lockDir, 0o700) })

	lease := NewCache(time.Minute, newClock().now).Put(&Lease{
		Operation: "apply",
		ToolID:    "claude",
		Files: []family.FilePlan{
			planFor(t, a, `{"a":1}`, `{"a":2}`),
			{Path: locked, Existed: false, Before: nil, After: []byte(`{"b":2}`)},
		},
	})
	_, err := Apply(lease, Options{})
	ae := apierr.From(err)
	if ae == nil || !strings.Contains(ae.Message, "已按文件回滚") {
		t.Fatalf("全部恢复成功时文案应保持「已按文件回滚」：%v", err)
	}
	if ae.Details["restoredAll"] != true {
		t.Errorf("details.restoredAll = %v，期望 true", ae.Details["restoredAll"])
	}
	if got := toStrings(ae.Details["needsAttention"]); len(got) != 0 {
		t.Errorf("details.needsAttention = %v，期望空数组（无恢复失败的文件）", got)
	}
}

// TestWritesAreAtomicViaRename 守住 UC-005 A5：同目录临时文件 + rename，
// 读取者要么看到完整旧文要么看到完整新文。
func TestWritesAreAtomicViaRename(t *testing.T) {
	dir := t.TempDir()
	target := writeFile(t, dir, "atomic.json", "OLD-CONTENT", 0o600)

	// 持有旧文件的 fd：rename 替换目录项后，这个 fd 仍指向旧 inode；
	// 若是「原地重写」，同一 fd 会读到新内容。
	old, err := os.Open(target)
	if err != nil {
		t.Fatalf("打开失败: %v", err)
	}
	defer func() { _ = old.Close() }()

	lease := NewCache(time.Minute, newClock().now).Put(&Lease{
		ToolID: "claude",
		Files:  []family.FilePlan{planFor(t, target, "OLD-CONTENT", "NEW-CONTENT-LONGER")},
	})
	if _, err := Apply(lease, Options{Backuper: &recordingBackuper{id: "bk_1"}}); err != nil {
		t.Fatalf("Apply 失败: %v", err)
	}

	if got := readFile(t, target); got != "NEW-CONTENT-LONGER" {
		t.Errorf("路径读到的内容 = %q，期望完整新内容", got)
	}
	if st, err := os.Stat(target); err != nil || st.Size() != int64(len("NEW-CONTENT-LONGER")) {
		t.Errorf("文件大小 = %v (err=%v)，期望完整长度", st, err)
	}
	if _, err := old.Seek(0, 0); err != nil {
		t.Fatalf("Seek 失败: %v", err)
	}
	buf := make([]byte, len("OLD-CONTENT"))
	n, _ := old.Read(buf)
	if string(buf[:n]) != "OLD-CONTENT" {
		t.Errorf("旧 fd 读到 %q，期望旧内容——说明是原地重写而不是 rename 替换", buf[:n])
	}
	if res := tempResidue(t, dir); len(res) != 0 {
		t.Errorf("写入后残留临时文件: %v", res)
	}
}

// ── Backup / BuildPlans ─────────────────────────────────────────────────────

func TestBackuperIsCalledOnceWithReasonAndTool(t *testing.T) {
	dir := t.TempDir()
	p := writeFile(t, dir, "a.json", "before", 0o600)

	lease := NewCache(time.Minute, newClock().now).Put(&Lease{
		Operation: "apply",
		ToolID:    "claude",
		Files:     []family.FilePlan{planFor(t, p, "before", "after")},
	})
	bk := &recordingBackuper{id: "bk_42"}
	res, err := Apply(lease, Options{Backuper: bk, Reason: "tool.apply"})
	if err != nil {
		t.Fatalf("Apply 失败: %v", err)
	}
	if bk.calls != 1 {
		t.Errorf("备份调用次数 = %d，期望恰好 1 次", bk.calls)
	}
	if bk.toolID != "claude" {
		t.Errorf("备份 toolID = %q，期望 claude", bk.toolID)
	}
	if bk.reason != "tool.apply" {
		t.Errorf("备份 reason = %q，期望 tool.apply", bk.reason)
	}
	// 备份拿到的必须是**写前**字节（用户回滚的依据）。
	if len(bk.files) != 1 || string(bk.files[0].Before) != "before" {
		t.Errorf("备份内容 = %+v，期望写前字节 before", bk.files)
	}
	if len(res.BackupIDs) != 1 || res.BackupIDs[0] != "bk_42" {
		t.Errorf("backupIds = %v，期望 [bk_42]", res.BackupIDs)
	}

	// 备份实现返回空编号时不产生空串条目。
	dir2 := t.TempDir()
	p2 := writeFile(t, dir2, "b.json", "x", 0o600)
	lease2 := NewCache(time.Minute, newClock().now).Put(&Lease{
		ToolID: "codex",
		Files:  []family.FilePlan{planFor(t, p2, "x", "y")},
	})
	res2, err := Apply(lease2, Options{Backuper: &recordingBackuper{id: ""}})
	if err != nil {
		t.Fatalf("Apply 失败: %v", err)
	}
	if len(res2.BackupIDs) != 0 {
		t.Errorf("空编号不该进入 backupIds: %v", res2.BackupIDs)
	}

	// Backuper 为 nil（内部只读路径）时跳过备份但不失败。
	dir3 := t.TempDir()
	p3 := writeFile(t, dir3, "c.json", "x", 0o600)
	lease3 := NewCache(time.Minute, newClock().now).Put(&Lease{
		ToolID: "codex",
		Files:  []family.FilePlan{planFor(t, p3, "x", "z")},
	})
	res3, err := Apply(lease3, Options{})
	if err != nil {
		t.Fatalf("无备份器时 Apply 失败: %v", err)
	}
	if res3.Status != StatusApplied || len(res3.BackupIDs) != 0 {
		t.Errorf("无备份器时 = %q/%v", res3.Status, res3.BackupIDs)
	}
}

func TestBackuperFailureAbortsWithStorage(t *testing.T) {
	dir := t.TempDir()
	p := writeFile(t, dir, "a.json", "before", 0o600)
	lease := NewCache(time.Minute, newClock().now).Put(&Lease{
		ToolID: "claude",
		Files:  []family.FilePlan{planFor(t, p, "before", "after")},
	})

	cause := errors.New("磁盘空间不足")
	_, err := Apply(lease, Options{Backuper: &recordingBackuper{err: cause}})
	if code := errCode(t, err); code != apierr.CodeStorage {
		t.Fatalf("备份失败的错误码 = %q，期望 storage", code)
	}
	if !strings.Contains(err.Error(), "磁盘空间不足") {
		t.Errorf("错误文本 = %q，期望保留底层原因", err.Error())
	}
	// 备份是所有写入的前提：失败即整体中止，一个字节都不写。
	if got := readFile(t, p); got != "before" {
		t.Errorf("备份失败后文件被写了: %q", got)
	}
}

func TestApplyEmptyLeaseIsUnchanged(t *testing.T) {
	bk := &recordingBackuper{id: "bk_1"}
	res, err := Apply(&Lease{ToolID: "claude"}, Options{Backuper: bk, Reason: "tool.apply"})
	if err != nil {
		t.Fatalf("空计划不得报错: %v", err)
	}
	if res.Status != StatusUnchanged {
		t.Errorf("状态 = %q，期望 unchanged", res.Status)
	}
	if len(res.Files) != 0 || len(res.BackupIDs) != 0 {
		t.Errorf("空计划不该有 per-file 结果或备份: %+v", res)
	}
	if bk.calls != 0 {
		t.Errorf("空计划不该记备份，实际 %d 次", bk.calls)
	}
}

func TestBuildPlansReadsBeforeAndSortsPaths(t *testing.T) {
	dir := t.TempDir()
	zz := writeFile(t, dir, "zz.json", "z-content", 0o600)
	aa := writeFile(t, dir, "aa.json", "a-content", 0o600)
	missing := filepath.Join(dir, "mm.json")

	plans, err := BuildPlans(map[string][]byte{
		zz:      []byte("z-next"),
		aa:      []byte("a-next"),
		missing: []byte("m-next"),
	})
	if err != nil {
		t.Fatalf("BuildPlans 失败: %v", err)
	}
	if len(plans) != 3 {
		t.Fatalf("计划数 = %d，期望 3", len(plans))
	}
	// 路径排序稳定（确定性：同一输入永远同一顺序，便于审计与哈希）。
	paths := []string{plans[0].Path, plans[1].Path, plans[2].Path}
	want := append([]string(nil), paths...)
	sort.Strings(want)
	for i := range want {
		if paths[i] != want[i] {
			t.Errorf("计划顺序 = %v，期望按路径字典序 %v", paths, want)
			break
		}
	}

	byPath := map[string]family.FilePlan{}
	for _, p := range plans {
		byPath[p.Path] = p
	}
	if got := byPath[aa]; got.Existed != true || string(got.Before) != "a-content" || string(got.After) != "a-next" {
		t.Errorf("既有文件计划 = %+v，期望 Existed/before/after 都正确", got)
	}
	if got := byPath[missing]; got.Existed != false || got.Before != nil || string(got.After) != "m-next" {
		t.Errorf("缺席文件计划 = %+v，期望 Existed=false 且 Before 为空", got)
	}
	if got := byPath[zz]; string(got.Before) != "z-content" || string(got.After) != "z-next" {
		t.Errorf("文件计划 = %+v", got)
	}

	// BuildPlans 本身不触盘写入。
	if got := readFile(t, aa); got != "a-content" {
		t.Errorf("BuildPlans 改了盘上的文件: %q", got)
	}
	if res := tempResidue(t, dir); len(res) != 0 {
		t.Errorf("BuildPlans 残留临时文件: %v", res)
	}

	// 空输入 → 空计划（非 nil），无错误。
	empty, err := BuildPlans(nil)
	if err != nil {
		t.Fatalf("空输入失败: %v", err)
	}
	if empty == nil || len(empty) != 0 {
		t.Errorf("空输入的计划 = %v，期望空切片", empty)
	}

	// 路径是目录 → path_conflict（家族路径只含文件，UC-003 A10）。
	sub := filepath.Join(dir, "adir")
	if err := os.Mkdir(sub, 0o700); err != nil {
		t.Fatalf("建目录失败: %v", err)
	}
	targets := map[string][]byte{sub: []byte("x")}
	_, err = BuildPlans(targets)
	if code := errCode(t, err); code != apierr.CodePathConflict {
		t.Errorf("目录目标的码 = %q，期望 path_conflict", code)
	}
}

// TestWriteAndDeleteFailuresAreTransactional 覆盖「写/删本身失败」的分支：
// 祖先不是目录、目标是非空目录、删除目标是非空目录，三者都必须走
// config_invalid + 回滚 + 无临时文件残留，而不是静默半成功。
//
// 这些场景手搓 lease 且不给指纹：走正常 Cache.Put 路径时，指纹校验会先把
// 「目标形状不对」拦成 path_conflict，这里要验的是**写/删阶段的失败处理**。
func TestWriteAndDeleteFailuresAreTransactional(t *testing.T) {
	t.Run("祖先路径不是目录导致写入失败", func(t *testing.T) {
		dir := t.TempDir()
		blocker := writeFile(t, dir, "blocker", "i am a file", 0o600)
		early := writeFile(t, dir, "early.json", "before", 0o600)
		impossible := filepath.Join(blocker, "child.json") // 父级是普通文件

		lease := &Lease{
			Operation:    "apply",
			ToolID:       "claude",
			Fingerprints: map[string]string{},
			Files: []family.FilePlan{
				{Path: early, Existed: true, Before: []byte("before"), After: []byte("after")},
				{Path: impossible, Existed: false, Before: nil, After: []byte("x")},
			},
		}
		res, err := Apply(lease, Options{})
		if code := errCode(t, err); code != apierr.CodeConfigInvalid {
			t.Fatalf("码 = %q，期望 config_invalid", code)
		}
		if got := readFile(t, early); got != "before" {
			t.Errorf("已写文件未被回滚: %q", got)
		}
		if res.Files[len(res.Files)-1].Path != impossible {
			t.Errorf("失败路径未记入结果: %+v", res.Files)
		}
		if got := readFile(t, blocker); got != "i am a file" {
			t.Errorf("占位文件被改动: %q", got)
		}
		if r := tempResidue(t, dir); len(r) != 0 {
			t.Errorf("残留临时文件: %v", r)
		}
	})

	t.Run("目标是非空目录导致 rename 失败", func(t *testing.T) {
		dir := t.TempDir()
		target := filepath.Join(dir, "adir")
		if err := os.Mkdir(target, 0o700); err != nil {
			t.Fatalf("建目录失败: %v", err)
		}
		if err := os.WriteFile(filepath.Join(target, "inner"), []byte("x"), 0o600); err != nil {
			t.Fatalf("写内层文件失败: %v", err)
		}

		lease := &Lease{
			Operation:    "apply",
			ToolID:       "claude",
			Fingerprints: map[string]string{},
			Files:        []family.FilePlan{{Path: target, Existed: true, Before: []byte("x"), After: []byte("new")}},
		}
		if _, err := Apply(lease, Options{}); errCode(t, err) != apierr.CodeConfigInvalid {
			t.Fatalf("码 = %q，期望 config_invalid", apierr.From(err).Code)
		}
		// 目录仍然完好（rename 未生效），且临时文件被清理。
		st, err := os.Stat(target)
		if err != nil || !st.IsDir() {
			t.Errorf("目标目录被破坏: %v", err)
		}
		if r := tempResidue(t, dir); len(r) != 0 {
			t.Errorf("rename 失败后残留临时文件: %v", r)
		}
	})

	t.Run("删除目标是非空目录导致删除失败", func(t *testing.T) {
		dir := t.TempDir()
		target := filepath.Join(dir, "bd")
		if err := os.Mkdir(target, 0o700); err != nil {
			t.Fatalf("建目录失败: %v", err)
		}
		if err := os.WriteFile(filepath.Join(target, "inner"), []byte("x"), 0o600); err != nil {
			t.Fatalf("写内层文件失败: %v", err)
		}

		lease := &Lease{
			Operation:    "restore",
			ToolID:       "claude",
			Fingerprints: map[string]string{},
			Files:        []family.FilePlan{{Path: target, Existed: true, Delete: true}},
		}
		res, err := Apply(lease, Options{})
		if code := errCode(t, err); code != apierr.CodeConfigInvalid {
			t.Fatalf("码 = %q，期望 config_invalid", code)
		}
		if st, statErr := os.Stat(target); statErr != nil || !st.IsDir() {
			t.Errorf("删除失败时目录不该消失: %v", statErr)
		}
		if len(res.Files) != 1 || res.Files[0].Status != FileUnchanged {
			t.Errorf("删除失败的文件状态 = %+v，期望 unchanged", res.Files)
		}
		if d := apierr.From(err).Details; d == nil || d["failedPath"] != target {
			t.Errorf("details 未定位失败路径: %v", d)
		}
	})
}

// statusOf 取某路径在失败结果里的终态；缺席返回空串。
func statusOf(files []FileResult, path string) string {
	for _, f := range files {
		if f.Path == path {
			return f.Status
		}
	}
	return ""
}

// toStrings 把 details 里的 []string（经 any 传递后可能是 []string 或 []any）归一。
func toStrings(v any) []string {
	switch t := v.(type) {
	case []string:
		return t
	case []any:
		out := make([]string, 0, len(t))
		for _, e := range t {
			if s, ok := e.(string); ok {
				out = append(out, s)
			}
		}
		return out
	}
	return nil
}

// ── 在飞写事务信号（UC-098 A3）──────────────────────────────────────────────

// TestCacheInFlightLeases 钉住信号口径：未消耗且**未过期**的租约才算在飞；
// 过期判据与 Take 的拒绝条件严格互补；Len（诊断口径）与 InFlight（信号口径）刻意不同。
func TestCacheInFlightLeases(t *testing.T) {
	clock := newClock()
	cache := NewCache(30*time.Second, clock.now)
	l1 := cache.Put(&Lease{Operation: "apply", ToolID: "claude"})
	clock.advance(time.Second)
	l2 := cache.Put(&Lease{Operation: "restore", ToolID: "pi"})
	clock.advance(time.Second)
	l3 := cache.Put(&Lease{Operation: "reset", ToolID: "codex"})

	got := cache.InFlight()
	if len(got) != 3 {
		t.Fatalf("在飞租约数 = %d，期望 3（%+v）", len(got), got)
	}
	// 排序确定性：按创建时刻（同一时刻按 ID）。
	if got[0].ID != l1.ID || got[1].ID != l2.ID || got[2].ID != l3.ID {
		t.Errorf("在飞快照顺序 = [%s %s %s]，期望创建顺序 [%s %s %s]",
			got[0].ID, got[1].ID, got[2].ID, l1.ID, l2.ID, l3.ID)
	}
	if got[0].Operation != "apply" || got[0].ToolID != "claude" || !got[0].CreatedAt.Equal(l1.CreatedAt) {
		t.Errorf("快照条目未如实携带租约身份: %+v", got[0])
	}

	// 边界必须与 Take 严格互补：Take 的过期判据是**严格** After(ExpiresAt)，因此
	// 恰在过期时刻的租约仍可被消费 ⇒ 仍算在飞；再晚一秒才算过期。
	clock.advance(28 * time.Second) // 现在恰为 l1.ExpiresAt
	if inFlight := cache.InFlight(); len(inFlight) != 3 {
		t.Errorf("恰在过期时刻的在飞租约数 = %d，期望 3（Take 此时仍接受，边界互补）", len(inFlight))
	}
	clock.advance(time.Second) // 现在晚于 l1.ExpiresAt
	got = cache.InFlight()
	if len(got) != 2 || got[0].ID != l2.ID || got[1].ID != l3.ID {
		t.Errorf("过期后的在飞快照 = %+v，期望 [%s %s]", got, l2.ID, l3.ID)
	}
	// 诊断口径（Len）把过期条目仍算在内：两个读数的差别是刻意的。
	if n := cache.Len(); n != 3 {
		t.Errorf("Len = %d，期望 3（诊断口径含过期未摘除的条目）", n)
	}
	// 信号口径的「不算在飞」与 Take 的拒绝必须是同一条边界。
	if _, err := cache.Take(l1.ID); errCode(t, err) != apierr.CodePreviewExpired {
		t.Fatalf("过期租约 Take 应 preview_expired（与 InFlight 的排除互补）")
	}
	if inFlight := cache.InFlight(); len(inFlight) != 2 {
		t.Errorf("Take 摘除过期租约后在飞数 = %d，期望 2", len(inFlight))
	}
}

// TestApplyInFlightCountsActiveAndQueued 钉住 Apply 的进程内计数：正卡的写事务与
// 等在目标路径锁上的写者同样计入（「写事务未结束」的诚实口径），退出全归零。
func TestApplyInFlightCountsActiveAndQueued(t *testing.T) {
	if got := ApplyInFlight(); got != 0 {
		t.Fatalf("前置：进程内写事务数 = %d，期望 0（并发测试不得泄漏计数）", got)
	}
	base := t.TempDir()
	a := writeFile(t, base, "a.json", `{"a":1}`, 0o600)
	lease := NewCache(time.Minute, newClock().now).Put(&Lease{
		Operation: "apply",
		ToolID:    "claude",
		Files:     []family.FilePlan{planFor(t, a, `{"a":1}`, `{"a":2}`)},
	})

	entered := make(chan struct{})
	release := make(chan struct{})
	firstDone := make(chan struct{})
	go func() {
		defer close(firstDone)
		_, _ = Apply(lease, Options{faultAfterWrite: func(string) {
			close(entered)
			<-release
		}})
	}()
	<-entered
	if got := ApplyInFlight(); got != 1 {
		t.Fatalf("卡在写途中的事务计数值 = %d，期望 1", got)
	}

	// 第二个写事务：同一目标路径 ⇒ 进 Apply 后等在路径锁上（还没写一个字节）。
	secondDone := make(chan struct{})
	go func() {
		defer close(secondDone)
		_, _ = Apply(lease, Options{})
	}()
	waitUntil(t, 2*time.Second, func() bool { return ApplyInFlight() == 2 },
		"排队中的第二个写事务没有被计入（等在目标路径锁上的写者也是「在飞」）")

	close(release)
	<-firstDone
	<-secondDone
	waitUntil(t, 2*time.Second, func() bool { return ApplyInFlight() == 0 },
		"写事务全部退出后计数没有归零（defer 兜底失效）")

	// 空计划不产生写、也不计入（它不是一次写事务）。
	if _, err := Apply(&Lease{}, Options{}); err != nil {
		t.Fatalf("空计划 Apply 失败: %v", err)
	}
	if got := ApplyInFlight(); got != 0 {
		t.Errorf("空计划执行后计数 = %d，期望 0", got)
	}
}

// waitUntil 轮询等待条件成立；超时用 t.Fatalf（本文件的用例都在测试 goroutine 上调用）。
func waitUntil(t *testing.T, timeout time.Duration, cond func() bool, msg string) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("%s（等待 %v 超时）", msg, timeout)
}
