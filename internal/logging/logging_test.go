// Package logging 的单元测试：落盘位置、上限、轮转与并发安全。
//
// 守的是 UC-006 A12（数据根下的有界日志）与 UC-014 A14 的前提（日志本身先得存在且有内容）。
// 全部用例用 `t.Setenv(YZAGMGR_HOME, t.TempDir())` 构造隔离根，**绝不**触碰真实 ~/.yzagmgr。
package logging

import (
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"unicode/utf8"

	"github.com/peterplaykkk/yzagmgr/internal/config"
)

// newEnv 造隔离环境：YZAGMGR_HOME 指向临时根，并建出数据根骨架（与启动路径同一顺序）。
func newEnv(t *testing.T) *config.Env {
	t.Helper()
	root := t.TempDir()
	t.Setenv(config.EnvHomeVar, root)
	env, err := config.FromProcess()
	if err != nil {
		t.Fatalf("构造隔离环境失败: %v", err)
	}
	if !env.Isolated {
		t.Fatalf("YZAGMGR_HOME=%q 时 Isolated 应为真（否则测试会写到真实 home）", root)
	}
	if err := env.EnsureLayout(); err != nil {
		t.Fatalf("建立数据根失败: %v", err)
	}
	return env
}

// newLogger 把 slog 接到写入器上（与 cmd/yzagmgr 的接法同形）。
func newLogger(w *Writer) *slog.Logger {
	return slog.New(slog.NewTextHandler(w, &slog.HandlerOptions{Level: slog.LevelInfo}))
}

// logFiles 返回日志目录下的文件（名 → 大小，按名排序）。
func logFiles(t *testing.T, env *config.Env) map[string]int64 {
	t.Helper()
	entries, err := os.ReadDir(filepath.Dir(env.LogFile()))
	if err != nil {
		t.Fatalf("读日志目录失败: %v", err)
	}
	out := map[string]int64{}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		st, err := e.Info()
		if err != nil {
			t.Fatalf("取 %s 信息失败: %v", e.Name(), err)
		}
		out[e.Name()] = st.Size()
	}
	return out
}

// readAll 把日志目录下所有文件（含轮转）按名顺序拼起来，便于「记录还在不在」的断言。
func readAll(t *testing.T, env *config.Env) string {
	t.Helper()
	files := logFiles(t, env)
	names := make([]string, 0, len(files))
	for name := range files {
		names = append(names, name)
	}
	// .1 排在 .2 之前（字典序会反过来），但本函数只用于「有没有这条记录」，顺序无所谓。
	sort.Strings(names)
	var b strings.Builder
	for _, name := range names {
		raw, err := os.ReadFile(filepath.Join(filepath.Dir(env.LogFile()), name))
		if err != nil {
			t.Fatalf("读 %s 失败: %v", name, err)
		}
		b.Write(raw)
	}
	return b.String()
}

// TestWriterPutsRecordsUnderDataRoot：记录落在 env.LogFile()，文件 0600、目录 0700。
func TestWriterPutsRecordsUnderDataRoot(t *testing.T) {
	env := newEnv(t)
	w, err := Open(env, Options{})
	if err != nil {
		t.Fatalf("Open 失败: %v", err)
	}
	defer func() { _ = w.Close() }()

	newLogger(w).Info("落盘探针", "k", "v")

	raw, err := os.ReadFile(env.LogFile())
	if err != nil {
		t.Fatalf("日志文件不存在（路径必须来自 env.LogFile()）: %v", err)
	}
	for _, want := range []string{"落盘探针", "k=v"} {
		if !strings.Contains(string(raw), want) {
			t.Errorf("日志里缺 %q: %s", want, raw)
		}
	}

	st, err := os.Stat(env.LogFile())
	if err != nil {
		t.Fatalf("取文件信息失败: %v", err)
	}
	if perm := st.Mode().Perm(); perm != 0o600 {
		t.Errorf("日志文件权限 = %o，期望 0600", perm)
	}
	dirSt, err := os.Stat(filepath.Dir(env.LogFile()))
	if err != nil {
		t.Fatalf("取目录信息失败: %v", err)
	}
	if perm := dirSt.Mode().Perm(); perm&0o077 != 0 {
		t.Errorf("日志目录权限 = %o，对其他用户可见", perm)
	}
}

// TestOpenRefusesNilEnv：没有数据根就没有路径可推导 —— 响亮失败而不是回落到某个默认位置。
func TestOpenRefusesNilEnv(t *testing.T) {
	if _, err := Open(nil, Options{}); err == nil {
		t.Fatalf("env 为 nil 必须报错（否则日志路径只能靠猜）")
	}
}

// TestWriterCreatesNothingOutsideDataRoot：写日志（含轮转）只在数据根内建文件。
//
// 这是隔离纪律的可判定形式：临时的 YZAGMGR_HOME 之外出现任何文件，就说明有路径越界。
func TestWriterCreatesNothingOutsideDataRoot(t *testing.T) {
	root := t.TempDir()
	t.Setenv(config.EnvHomeVar, root)
	env, err := config.FromProcess()
	if err != nil {
		t.Fatalf("构造隔离环境失败: %v", err)
	}
	if err := env.EnsureLayout(); err != nil {
		t.Fatalf("建立数据根失败: %v", err)
	}

	w, err := Open(env, Options{MaxBytes: 512, Keep: 2})
	if err != nil {
		t.Fatalf("Open 失败: %v", err)
	}
	logger := newLogger(w)
	for i := 0; i < 200; i++ {
		logger.Info("记录", "i", i, "pad", strings.Repeat("x", 80))
	}
	if err := w.Close(); err != nil {
		t.Fatalf("Close 失败: %v", err)
	}

	rootAbs, err := filepath.Abs(root)
	if err != nil {
		t.Fatalf("取绝对路径失败: %v", err)
	}
	dataAbs, err := filepath.Abs(env.DataRoot())
	if err != nil {
		t.Fatalf("取绝对路径失败: %v", err)
	}

	var files []string
	err = filepath.WalkDir(rootAbs, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		files = append(files, path)
		return nil
	})
	if err != nil {
		t.Fatalf("遍历隔离根失败: %v", err)
	}
	if len(files) == 0 {
		t.Fatalf("隔离根下没有任何文件：日志根本没落盘")
	}
	for _, f := range files {
		rel, err := filepath.Rel(dataAbs, f)
		if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			t.Errorf("日志写到了数据根之外: %s（数据根 %s）", f, dataAbs)
		}
	}

	// 反向：这次写入**只**该产出一个当前文件与两份轮转，别的什么都不许有。
	names := make([]string, 0, len(files))
	for _, f := range files {
		names = append(names, filepath.Base(f))
	}
	sort.Strings(names)
	want := []string{"yzagmgr.log", "yzagmgr.log.1", "yzagmgr.log.2"}
	if strings.Join(names, ",") != strings.Join(want, ",") {
		t.Errorf("数据根下的文件 = %v，期望恰好 %v", names, want)
	}
}

// TestRotationIsBounded：写满很远之后，文件数与总量都在上限内，且没有哪一份超上限；
// 记录按「最旧先丢」淘汰（既不是清空，也不是随机丢中间）。
func TestRotationIsBounded(t *testing.T) {
	const (
		maxBytes = 1024
		keep     = 2
		records  = 400 // 每条约 160 字节 ⇒ 约 64 KiB，远超前文的 3 KiB 界
	)
	env := newEnv(t)
	w, err := Open(env, Options{MaxBytes: maxBytes, Keep: keep})
	if err != nil {
		t.Fatalf("Open 失败: %v", err)
	}
	logger := newLogger(w)
	pad := strings.Repeat("x", 96)
	for i := 0; i < records; i++ {
		logger.Info("压力记录", "i", i, "pad", pad)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("Close 失败: %v", err)
	}

	files := logFiles(t, env)
	if len(files) != keep+1 {
		t.Fatalf("文件数 = %d（%v），期望 %d（1 份当前 + %d 份轮转）", len(files), files, keep+1, keep)
	}
	var total int64
	for _, size := range files {
		if size > maxBytes {
			t.Errorf("有文件大小 %d 超过上限 %d：%v", size, maxBytes, files)
		}
		total += size
	}
	if bound := int64(keep+1) * maxBytes; total > bound {
		t.Errorf("盘上总量 = %d，超过界 %d", total, bound)
	}

	// 最新一条必须还在（轮转不是清空），最旧的那些必须已被淘汰（否则「有界」是空的）。
	all := readAll(t, env)
	if !strings.Contains(all, "i="+strconv.Itoa(records-1)) {
		t.Errorf("最新记录 i=%d 不在日志里（轮转把新记录也丢了）", records-1)
	}
	seen := map[int]bool{}
	for _, m := range regexp.MustCompile(`i=(\d+)`).FindAllStringSubmatch(all, -1) {
		n, err := strconv.Atoi(m[1])
		if err != nil {
			t.Fatalf("解析 %q 失败: %v", m[1], err)
		}
		seen[n] = true
	}
	if seen[0] {
		t.Errorf("最旧的记录 i=0 还在：轮转没有真正淘汰旧记录（界是靠别的办法伪装的？）")
	}
	// 保留的必须是**连续的一个后缀**：中间断档意味着轮转链有洞（丢记录）。
	first := records
	for i := 0; i < records; i++ {
		if seen[i] {
			first = i
			break
		}
	}
	for i := first; i < records; i++ {
		if !seen[i] {
			t.Errorf("保留的记录不连续：i=%d 缺失（i=%d..%d 已在）", i, first, records-1)
			break
		}
	}
}

// TestOversizeRecordIsTruncatedKeepsCap：单条记录超上限时截断，文件不越界，且仍是一条完整的行。
func TestOversizeRecordIsTruncatedKeepsCap(t *testing.T) {
	const maxBytes = 256
	env := newEnv(t)
	w, err := Open(env, Options{MaxBytes: maxBytes, Keep: 2})
	if err != nil {
		t.Fatalf("Open 失败: %v", err)
	}
	long := "record " + strings.Repeat("中", 2048) // 单条远超上限（多字节字符 + 越界）
	n, err := w.Write([]byte(long))
	if err == nil {
		t.Errorf("截断写入必须回非 nil 错误（io.Writer 契约：n < len(p) 要报错）")
	}
	if n >= len(long) {
		t.Errorf("返回的字节数 = %d，不应等于输入长度 %d", n, len(long))
	}
	if _, err := w.Write([]byte("下一条\n")); err != nil {
		t.Fatalf("截断后的下一次写入失败: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("Close 失败: %v", err)
	}

	files := logFiles(t, env)
	for name, size := range files {
		if size > maxBytes {
			t.Errorf("%s 大小 %d 超过上限 %d", name, size, maxBytes)
		}
	}
	all := readAll(t, env)
	if !strings.Contains(all, "已截断") {
		t.Errorf("截断标记不在日志里（截断是静默的？）: %q", all)
	}
	// 行边界必须保住：每个文件都必须以换行结尾，且出现的行都不含半个多字节字符。
	for name := range files {
		raw, err := os.ReadFile(filepath.Join(filepath.Dir(env.LogFile()), name))
		if err != nil {
			t.Fatalf("读 %s 失败: %v", name, err)
		}
		if len(raw) > 0 && raw[len(raw)-1] != '\n' {
			t.Errorf("%s 没有以换行结尾：下一条记录会黏在同一行上", name)
		}
		if !utf8.ValidString(string(raw)) {
			t.Errorf("%s 含非 UTF-8 字节（多字节字符被砍成半个）", name)
		}
	}
}

// TestReopenAppendsAndKeepsBounds：重启后接着上一次的文件追加，且上限判定以盘上真实大小为准。
func TestReopenAppendsAndKeepsBounds(t *testing.T) {
	const maxBytes = 512
	env := newEnv(t)

	w1, err := Open(env, Options{MaxBytes: maxBytes, Keep: 2})
	if err != nil {
		t.Fatalf("Open 失败: %v", err)
	}
	if _, err := w1.Write([]byte(strings.Repeat("a", 400) + "\n")); err != nil {
		t.Fatalf("写入失败: %v", err)
	}
	if err := w1.Close(); err != nil {
		t.Fatalf("Close 失败: %v", err)
	}

	// 新进程：内存里从零开始，但盘上已有 400 字节 ⇒ 再来 300 字节必须先轮转。
	w2, err := Open(env, Options{MaxBytes: maxBytes, Keep: 2})
	if err != nil {
		t.Fatalf("重新 Open 失败: %v", err)
	}
	if _, err := w2.Write([]byte(strings.Repeat("b", 300) + "\n")); err != nil {
		t.Fatalf("重启后写入失败: %v", err)
	}
	if err := w2.Close(); err != nil {
		t.Fatalf("Close 失败: %v", err)
	}

	files := logFiles(t, env)
	if len(files) != 2 {
		t.Fatalf("文件数 = %d（%v），期望 2（当前 + 一份轮转）", len(files), files)
	}
	current, err := os.ReadFile(env.LogFile())
	if err != nil {
		t.Fatalf("读当前文件失败: %v", err)
	}
	if !strings.Contains(string(current), "bbb") {
		t.Errorf("当前文件应是重启后的记录: %q", current)
	}
	rotated, err := os.ReadFile(env.LogFile() + ".1")
	if err != nil {
		t.Fatalf("读 .1 失败: %v", err)
	}
	if !strings.Contains(string(rotated), "aaa") {
		t.Errorf(".1 应是重启前那份（追加语义：重启不清空历史）: %q", rotated)
	}
}

// TestKeepZeroDropsRotatedFiles：Keep=0 是合法取值（只留当前文件），不是「无界」。
func TestKeepZeroDropsRotatedFiles(t *testing.T) {
	const maxBytes = 128
	env := newEnv(t)
	w, err := Open(env, Options{MaxBytes: maxBytes, Keep: 0})
	if err != nil {
		t.Fatalf("Open 失败: %v", err)
	}
	for i := 0; i < 40; i++ {
		if _, err := w.Write([]byte(strings.Repeat("c", 100) + "\n")); err != nil {
			t.Fatalf("写入失败: %v", err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatalf("Close 失败: %v", err)
	}
	files := logFiles(t, env)
	if len(files) != 1 {
		t.Fatalf("文件数 = %d（%v），Keep=0 时只应留当前文件", len(files), files)
	}
	for _, size := range files {
		if size > maxBytes {
			t.Errorf("当前文件 %d 字节，超过上限 %d", size, maxBytes)
		}
	}
}

// TestConcurrentWritesStayWhole：并发写不得交错成半行，也不得丢记录。
//
// slog handler 会被任意多个 goroutine 调用（管理面、数据面、后台探测各一份），
// 因此「每条记录一次 Write、锁内完成」是这里要钉住的性质。
func TestConcurrentWritesStayWhole(t *testing.T) {
	const (
		writers   = 16
		perWriter = 32
		pad       = 64
	)
	env := newEnv(t)
	// 上限刻意放大：本次要逐行核对，不轮转就没有跨文件拼接的干扰。
	w, err := Open(env, Options{MaxBytes: 1 << 20, Keep: 2})
	if err != nil {
		t.Fatalf("Open 失败: %v", err)
	}
	defer func() { _ = w.Close() }()

	var wg sync.WaitGroup
	for g := 0; g < writers; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			logger := newLogger(w)
			for i := 0; i < perWriter; i++ {
				logger.Info("并发记录", "g", g, "i", i, "pad", strings.Repeat("y", pad))
			}
		}(g)
	}
	wg.Wait()

	raw, err := os.ReadFile(env.LogFile())
	if err != nil {
		t.Fatalf("读日志失败: %v", err)
	}
	body := strings.TrimSuffix(string(raw), "\n")
	lines := strings.Split(body, "\n")
	if len(lines) != writers*perWriter {
		t.Fatalf("记录数 = %d，期望 %d（丢了或重了）", len(lines), writers*perWriter)
	}
	tail := regexp.MustCompile(`g=\d+ i=\d+ pad=y{` + strconv.Itoa(pad) + `}$`)
	for _, line := range lines {
		if !strings.HasPrefix(line, "time=") || !strings.Contains(line, "level=INFO") || !tail.MatchString(line) {
			t.Fatalf("记录被截断或交错: %q", line)
		}
	}
}

// TestWriteAfterCloseIsRefused：关闭后写入返回错误而不是 panic 或静默丢。
func TestWriteAfterCloseIsRefused(t *testing.T) {
	env := newEnv(t)
	w, err := Open(env, Options{})
	if err != nil {
		t.Fatalf("Open 失败: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("首次 Close 失败: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Errorf("重复 Close 应当是幂等的: %v", err)
	}
	n, err := w.Write([]byte("关闭后的记录\n"))
	if err == nil {
		t.Errorf("关闭后写入必须报错")
	}
	if n != 0 {
		t.Errorf("关闭后写入的字节数 = %d，期望 0", n)
	}
}
