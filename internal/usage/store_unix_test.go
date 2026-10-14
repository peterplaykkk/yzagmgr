//go:build unix

package usage

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"

	"github.com/peterplaykkk/yzagmgr/internal/config"
)

// TestUsageDBPrivateModeIsNotAnUmaskAccident 钉住 A10 的强度：
// 0600 是主动设上去的，不是进程 umask 的副产品。
//
// 顺带覆盖「库文件已存在且权限过宽」的收紧路径——那才是 chmod 真正要解决的问题。
// umask 是进程级状态，因此本测试刻意不与其他测试并行（包内测试默认串行）。
func TestUsageDBPrivateModeIsNotAnUmaskAccident(t *testing.T) {
	old := syscall.Umask(0)
	defer syscall.Umask(old)

	home := t.TempDir()
	env := &config.Env{Home: home, Overrides: map[string]string{}, Isolated: true}
	if err := env.EnsureLayout(); err != nil {
		t.Fatalf("EnsureLayout 失败: %v", err)
	}

	// 预置一个权限过宽的库文件（模拟历史遗留或被人手改过）。
	path := env.UsageDBFile()
	if err := os.WriteFile(path, nil, 0o644); err != nil {
		t.Fatalf("预置库文件失败: %v", err)
	}
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatalf("放宽库文件权限失败: %v", err)
	}

	s := newStoreAt(t, path, Options{Location: testLoc})
	mustRecord(t, s, rec("rq_umask01", 0))

	for _, p := range []string{path, path + "-wal", path + "-shm"} {
		st, err := os.Stat(p)
		if err != nil {
			continue // 侧车文件可能尚未建立
		}
		if perm := st.Mode().Perm(); perm != 0o600 {
			t.Fatalf("umask=0 下 %s 仍须收紧到 0600，实际 %04o", filepath.Base(p), perm)
		}
	}
}
