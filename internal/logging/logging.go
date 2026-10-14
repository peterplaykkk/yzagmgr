// Package logging 是守护进程的本地日志落盘：stderr 之外再写一份**有界**文件。
//
// 为什么必须有它：macOS 上 `open`（LaunchServices）启动的进程没有终端，stderr 直接进黑洞。
// 2026-09-22 一次 `POST /api/v1/tray/action` 以 `unauthenticated` 失败（操作日志里有
// `op_1790080040767739000_3`），而 `data/logs/` 是空的 —— 事件只出现过一次，于是完全无从回溯。
// 日志"写在哪、写多大"因此成了可验收契约（UC-006 A12），不再是实现细节。
//
// 三条纪律：
//  1. **只能落在数据根下**：构造函数收 *config.Env 而不是裸路径，隔离运行（YZAGMGR_HOME）
//     因此自动生效，调用方没有机会发明一个数据根之外的路径；
//  2. **有硬上限**：单文件写满即轮转，只保留固定份数 —— 桌面应用会连续运行数周，日志不能
//     无界增长。单条记录本身超限时截断并附标记，也不让文件越界；
//  3. **逐条串行**：slog handler 会被任意多个 goroutine 调用，每条记录的写入在互斥锁内完成，
//     绝不出现两条记录交错成半个行。
//
// 刻意不做的事：不按日期命名（那是「按天归档」的场景，序号让「保留 n 份」是可枚举的事实）、
// 不压缩、不做异步缓冲（缓冲意味着崩溃时丢掉最后几条 —— 那正是最需要的那几条）。
package logging

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sync"
	"unicode/utf8"

	"github.com/peterplaykkk/yzagmgr/internal/config"
)

const (
	// DefaultMaxBytes 是单个日志文件的上限。
	//
	// 取 1 MiB 的依据：本进程的信息级日志只有启动、生命周期与拒绝几类（数据面逐请求是 debug），
	// 1 MiB 大致覆盖「最近几天到几周」。排查要的是「最近发生了什么」，不是无限历史，
	// 而受限的文件数让「日志自己把磁盘写满」这件事在结构上不可能发生。
	DefaultMaxBytes int64 = 1 << 20

	// DefaultKeep 是保留的轮转份数（**不含**当前文件）⇒ 盘上最多 3 个文件、约 3 MiB。
	DefaultKeep = 2

	// maxKeep 是份数上限（防呆）：轮转是链式 rename，份数过多只是浪费 inode。
	maxKeep = 8
)

// truncMarker 是超长记录的截断标记。自带换行 —— 截断后若不留换行，
// 下一条记录会黏在同一行上，读日志的人会把两条当成一条。
const truncMarker = "…[该条日志超过单文件上限，已截断]\n"

// errRecordTooLong 表示一条记录超过单文件上限、已截断写入。
//
// 它不是「写失败」：截断后的内容已经落盘。回非 nil 错误只是为了让 io.Writer 契约成立
// （n < len(p) 必须报错）；调用方**不要**据此重写，重写只会再截一次。
var errRecordTooLong = errors.New("日志记录超过单文件上限，已截断")

// Options 是轮转参数。零值即默认（见 DefaultMaxBytes / DefaultKeep）。
type Options struct {
	// MaxBytes 是单个文件的上限；<= 0 取 DefaultMaxBytes。
	MaxBytes int64
	// Keep 是保留的轮转份数；< 0 取 DefaultKeep，0 表示不留轮转文件（写满即删旧的）。
	Keep int
}

// DefaultOptions 是守护进程采用的参数（1 MiB × 2 份轮转）。
func DefaultOptions() Options { return Options{MaxBytes: DefaultMaxBytes, Keep: DefaultKeep} }

// withDefaults 收敛零值与越界值：非法参数**不**报错，而是退回合法值。
// 日志参数是「记录」不是「功能」，写错不该让守护进程起不来（对比端口：那是写错就静默失联的类别）。
func (o Options) withDefaults() Options {
	if o.MaxBytes <= 0 {
		o.MaxBytes = DefaultMaxBytes
	}
	if o.Keep < 0 {
		o.Keep = DefaultKeep
	}
	if o.Keep > maxKeep {
		o.Keep = maxKeep
	}
	return o
}

// Writer 是有界的追加式日志文件（io.Writer）。零值不可用，用 Open 构造。
type Writer struct {
	mu       sync.Mutex
	dir      string
	name     string
	maxBytes int64
	keep     int

	// file 为 nil 表示当前没有打开的文件（Close 之后，或轮转只做了一半）。
	// size 是当前文件的字节数；每次打开都从盘上重新取，不信内存里的记账（见 openLocked）。
	file   *os.File
	size   int64
	closed bool
}

// Open 打开数据根下的日志文件（追加语义）。
//
// 追加而非截断：上一次运行留下的记录正是排查要用的（崩溃重启后前后文要连起来）。
// 上限不因追加而失效 —— size 取自盘上真实大小，写满照常轮转。
func Open(env *config.Env, opts Options) (*Writer, error) {
	if env == nil {
		// 失败要响亮：没有 env 就没有「数据根」这个概念，路径无从推导。
		return nil, errors.New("logging: env 不能为 nil（日志路径只能从数据根推导）")
	}
	cfg := opts.withDefaults()
	path := env.LogFile()
	dir := filepath.Dir(path)
	if err := ensureDir0700(dir); err != nil {
		return nil, fmt.Errorf("建立日志目录 %s 失败: %w", dir, err)
	}
	w := &Writer{dir: dir, name: filepath.Base(path), maxBytes: cfg.MaxBytes, keep: cfg.Keep}
	if err := w.openLocked(); err != nil {
		return nil, err
	}
	return w, nil
}

// Write 写入一条记录（slog 每次 Handle 就是一次 Write）。
//
// 顺序刻意如此：先把「截断 → 轮转 → 打开」做扎实，再写 —— 于是任何一份文件
// （包括刚被轮转走的那份）都不超过 maxBytes。
// 轮转失败时这一条不落到文件（错误原样返回；同一 slog 记录在 stderr 那份里仍在），
// 下一次写入会先按盘上真实大小重新校准，因此上限不会因此失守超过一条记录。
func (w *Writer) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.closed {
		return 0, fs.ErrClosed
	}

	rec := p
	truncated := false
	if int64(len(rec)) > w.maxBytes {
		rec = truncateRecord(rec, w.maxBytes)
		truncated = true
	}
	// size > 0 才轮转：空文件上的一条超限记录由上面的截断兜住，不必先转出一个空文件。
	if w.size > 0 && w.size+int64(len(rec)) > w.maxBytes {
		if err := w.rotateLocked(); err != nil {
			return 0, err
		}
	}
	if w.file == nil {
		if err := w.openLocked(); err != nil {
			return 0, err
		}
	}
	n, err := w.file.Write(rec)
	w.size += int64(n)
	if err != nil {
		return n, err
	}
	if truncated {
		return n, errRecordTooLong
	}
	return n, nil
}

// Close 关闭当前文件。之后再 Write 返回 ErrClosed（不 panic、不静默丢）。
// 可重复调用：第二次是无操作。
func (w *Writer) Close() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.closed = true
	if w.file == nil {
		return nil
	}
	err := w.file.Close()
	w.file = nil
	return err
}

// ── 落盘 ────────────────────────────────────────────────────────────────────

// openLocked 打开（必要时创建）当前文件，并把 size 校准到盘上真实字节数。
//
// 校准是必需而不是保险：重启后我们接着上一进程的文件追加，而内存里的计数器从零开始；
// 不校准就等于每次重启都把「已写上限」重置一遍，长命应用照样能把盘写满。
func (w *Writer) openLocked() error {
	path := w.rotatedPath(0)
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("打开日志文件 %s 失败: %w", path, err)
	}
	st, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return fmt.Errorf("读取日志文件 %s 信息失败: %w", path, err)
	}
	w.size = st.Size()
	// 既有文件可能是用宽松权限建出来的：日志带请求关联编号与本地路径，
	// 一律收紧到 0600（与操作日志同一条权限纪律）。
	if perm := st.Mode().Perm(); perm&0o077 != 0 {
		_ = f.Chmod(perm &^ 0o077)
	}
	w.file = f
	return nil
}

// rotateLocked 把当前文件挪到 `.1`，既有轮转文件依次后移，最旧的一份删除。
//
// 用序号而不是时间戳命名：序号让「保留 n 份」成为可枚举的事实（.1..n），
// 时间戳要让清理逻辑去解析目录与文件名，反而更容易漏掉一份。
func (w *Writer) rotateLocked() error {
	if w.file != nil {
		if err := w.file.Close(); err != nil {
			return fmt.Errorf("关闭日志文件失败: %w", err)
		}
		w.file = nil
	}
	w.size = 0

	if w.keep == 0 {
		// 不留轮转文件：旧的直接删，下一次 Write 会重建当前文件。
		return removeIfExists(w.rotatedPath(0))
	}
	if err := removeIfExists(w.rotatedPath(w.keep)); err != nil {
		return err
	}
	for i := w.keep - 1; i >= 1; i-- {
		if err := renameIfExists(w.rotatedPath(i), w.rotatedPath(i+1)); err != nil {
			return err
		}
	}
	return renameIfExists(w.rotatedPath(0), w.rotatedPath(1))
}

// rotatedPath 给出第 i 份文件的路径（0 = 当前文件，1..keep = 轮转文件）。
func (w *Writer) rotatedPath(i int) string {
	path := filepath.Join(w.dir, w.name)
	if i == 0 {
		return path
	}
	return fmt.Sprintf("%s.%d", path, i)
}

// ── 辅助 ────────────────────────────────────────────────────────────────────

// truncateRecord 把一条超限记录截到 maxBytes 以内，并保留结尾换行。
//
// 截断而不是「照写」：上限是硬上限。日志的价值在于「有痕迹」，而一条 1 MiB 的记录
// （真出现的话也只可能来自被回显的错误正文）不值得让「有界」变成「通常有界」。
func truncateRecord(rec []byte, maxBytes int64) []byte {
	limit := int(maxBytes)
	marker := truncMarker
	if len(marker) > limit {
		// 上限小到连标记都放不下（只可能出现在测试里）：退化成只保一个换行。
		marker = "\n"
	}
	keep := limit - len(marker)
	if keep > len(rec) {
		keep = len(rec)
	}
	out := make([]byte, 0, limit)
	out = append(out, rec[:keep]...)
	// 别把多字节字符砍成半个：从尾部至多回退 3 字节到合法 UTF-8 边界
	// （UTF-8 的续字节最多 3 个；循环有界，不在 1 MiB 的记录上做二次扫描）。
	for i := 0; i < utf8.UTFMax-1 && len(out) > 0; i++ {
		if utf8.Valid(out) {
			break
		}
		out = out[:len(out)-1]
	}
	return append(out, marker...)
}

// ensureDir0700 建目录并把权限收紧到 0700（既有目录的宽松权限也一并收紧）。
func ensureDir0700(dir string) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	st, err := os.Stat(dir)
	if err != nil {
		return err
	}
	if perm := st.Mode().Perm(); perm&0o077 != 0 {
		return os.Chmod(dir, perm&^0o077)
	}
	return nil
}

// removeIfExists 删文件；不存在不算错 —— 轮转链上可能有空洞（比如用户手工删过 .1）。
func removeIfExists(path string) error {
	if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("删除 %s 失败: %w", path, err)
	}
	return nil
}

// renameIfExists 改名；源不存在不算错（同上）。
func renameIfExists(src, dst string) error {
	if err := os.Rename(src, dst); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("轮转 %s → %s 失败: %w", src, dst, err)
	}
	return nil
}
