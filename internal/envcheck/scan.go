package envcheck

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"time"

	"github.com/peterplaykkk/yzagmgr/internal/apierr"
)

// ── 可写面 / 只读面（扫描与修复的唯一同源判定） ──────────────────────────────

// Policy 是可写面与系统只读面的判定输入。
//
// 扫描（标记 repairable）与修复（写前校验）**必须**用同一份 Policy：
// 若两边各判一次，就会出现「界面说可修、实现却拒绝写」或更糟的反向情况。
type Policy struct {
	// Home 是 shell 启动文件的解析基准与默认可写根，**必填**。
	// 为空即拒绝构造：本包绝不在「不知道基准目录」的情况下扫描（测试纪律）。
	Home string
	// WritableRoots 是可写根（默认 [Home]）。不在任何根内的文件只报不修。
	WritableRoots []string
	// SystemFiles 是只读检查的系统文件清单（默认 DefaultSystemFiles()）。
	SystemFiles []string
}

// allowlist 是已规范化（符号链接解析后）的可写面判定。
type allowlist struct {
	roots  []string
	system []string
}

// allowlist 规范化 Policy；Home 为空即拒绝。
func (p Policy) allowlist() (*allowlist, error) {
	if strings.TrimSpace(p.Home) == "" {
		return nil, apierr.New(apierr.CodeInvalidRequest,
			"环境变量扫描缺少 Home 基准目录：拒绝在未指定基准时扫描（不得回退真实 $HOME）")
	}
	roots := p.WritableRoots
	if len(roots) == 0 {
		roots = []string{p.Home}
	}
	a := &allowlist{}
	for _, r := range roots {
		if r == "" {
			continue
		}
		c, err := canonicalPath(r)
		if err != nil {
			return nil, apierr.Wrap(apierr.CodeInvalidRequest, err, "可写根不可解析: %s", r)
		}
		a.roots = append(a.roots, c)
	}
	if len(a.roots) == 0 {
		return nil, apierr.New(apierr.CodeInvalidRequest, "可写根为空：拒绝构造扫描器")
	}
	// 系统文件清单只用于「明确拒绝写」的提示；前缀规则另有兜底。
	sys := p.SystemFiles
	if len(sys) == 0 {
		sys = DefaultSystemFiles()
	}
	for _, f := range sys {
		if f == "" {
			continue
		}
		c, err := canonicalPath(f)
		if err != nil {
			c = filepath.Clean(f)
		}
		a.system = append(a.system, c)
	}
	return a, nil
}

// systemPrefixes 是无论是否在清单里都只读的系统目录前缀。
// 注意不包含 /var（macOS 的 t.TempDir() 落在 /private/var/folders 下）。
var systemPrefixes = []string{"/etc/", "/private/etc/", "/usr/", "/bin/", "/sbin/", "/System/", "/Library/"}

// allowed 报告该路径是否落在可写面内（真实路径判定，含系统前缀否决）。
func (a *allowlist) allowed(path string) bool {
	real, err := canonicalPath(path)
	if err != nil {
		return false
	}
	for _, s := range systemPrefixes {
		if strings.HasPrefix(real, s) {
			return false
		}
	}
	for _, s := range a.system {
		if real == s {
			return false
		}
	}
	for _, r := range a.roots {
		if within(r, real) {
			return true
		}
	}
	return false
}

// remedy 给出不可写时给用户的手工处理指引（A11 的「说明需手工处理」）。
func (a *allowlist) remedy(path string) string {
	real, err := canonicalPath(path)
	if err != nil {
		real = path
	}
	for _, s := range systemPrefixes {
		if strings.HasPrefix(real, s) {
			return fmt.Sprintf("这是系统级只读文件，本应用不写它：请手工编辑 %s（需要管理员权限）", real)
		}
	}
	return fmt.Sprintf("目标不在可写根（%s）内：请手工编辑 %s", strings.Join(a.roots, ", "), real)
}

func within(root, path string) bool {
	if root == path {
		return true
	}
	rel, err := filepath.Rel(root, path)
	if err != nil {
		return false
	}
	return rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// canonicalPath 解析符号链接得到真实绝对路径；不存在时退化为绝对清理路径。
//
// 为什么必须解析：启动文件常是符号链接（如 ~/.zshrc → dotfiles 仓库），
// 用户看的与我们要改的必须是同一个真实文件（UC-015 边界条）。
func canonicalPath(p string) (string, error) {
	if p == "" {
		return "", fmt.Errorf("空路径")
	}
	if real, err := filepath.EvalSymlinks(p); err == nil {
		return filepath.Clean(real), nil
	}
	abs, err := filepath.Abs(p)
	if err != nil {
		return "", err
	}
	return filepath.Clean(abs), nil
}

// ── 扫描输入 ────────────────────────────────────────────────────────────────

// LaunchdProbe 探测 launchd 会话环境（键 → 值）；缺失的键不要放进结果。
type LaunchdProbe func(names []string) map[string]string

// EnvSnapshot 取一份环境变量快照（键 → 值）。
type EnvSnapshot func() map[string]string

// Options 是构造 Scanner 的输入。
type Options struct {
	Policy Policy
	// ShellFiles 是待扫的启动文件清单。**显式给出时视为完整清单**：
	// 不再叠加默认系统文件，避免测试夹具误读真实 /etc。
	// 为空时用 DefaultShellFiles(Home)。
	ShellFiles []string
	// Expected 是本产品写入的决定键；只有命中它们的赋值才可能是冲突。
	Expected []DecisionKey
	// Launchd / ProcessEnv 为 nil 时用平台默认实现；测试必须注入假实现，
	// 以免读到真实会话环境（进程环境）或调用 launchctl。
	Launchd    LaunchdProbe
	ProcessEnv EnvSnapshot
	// MaxSourceDepth 覆盖默认 source 跟随深度上限。
	MaxSourceDepth int
	// Clock 注入时间源，便于确定性测试。
	Clock func() time.Time
}

// DefaultShellFiles 返回契约要求的用户级启动文件（顺序即契约顺序）。
func DefaultShellFiles(home string) []string {
	return []string{
		filepath.Join(home, ".bashrc"),
		filepath.Join(home, ".bash_profile"),
		filepath.Join(home, ".zshrc"),
		filepath.Join(home, ".zprofile"),
		filepath.Join(home, ".profile"),
	}
}

// DefaultSystemFiles 返回契约要求的系统级只读检查文件。
func DefaultSystemFiles() []string {
	return []string{"/etc/profile", "/etc/bashrc"}
}

// ── 扫描器 ──────────────────────────────────────────────────────────────────

type scanFile struct {
	path   string
	system bool
}

// Scanner 扫描 shell 启动文件、launchd 会话环境与当前进程环境。
//
// 它**不缓存**任何结论：每次 Scan 都重读盘与环境，界面切页签即可复检（A14）。
type Scanner struct {
	opts     Options
	allow    *allowlist
	expected map[string]DecisionKey
	files    []scanFile
}

// NewScanner 构造扫描器（规范化 Policy、补齐默认文件清单与默认探测器）。
func NewScanner(opts Options) (*Scanner, error) {
	allow, err := opts.Policy.allowlist()
	if err != nil {
		return nil, err
	}
	s := &Scanner{opts: opts, allow: allow, expected: map[string]DecisionKey{}}
	for _, k := range opts.Expected {
		if strings.TrimSpace(k.Name) == "" {
			continue
		}
		s.expected[k.Name] = k
	}
	if s.opts.MaxSourceDepth <= 0 {
		s.opts.MaxSourceDepth = DefaultMaxSourceDepth
	}
	if s.opts.Clock == nil {
		s.opts.Clock = time.Now
	}
	if s.opts.Launchd == nil {
		s.opts.Launchd = defaultLaunchdProbe
	}
	if s.opts.ProcessEnv == nil {
		s.opts.ProcessEnv = defaultProcessEnv
	}

	// 文件清单与**加载顺序**：系统级（/etc）先、用户级后，与登录 shell 一致；
	// 因此同名赋值里「用户级那条」才是最后生效的（冲突判定按加载顺序取最后一条）。
	// 显式注入 ShellFiles 时视为完整的用户级清单。
	sys := opts.Policy.SystemFiles
	if len(sys) == 0 && len(opts.ShellFiles) == 0 {
		sys = DefaultSystemFiles()
	}
	for _, f := range sys {
		if f != "" {
			s.files = append(s.files, scanFile{path: f, system: true})
		}
	}
	if len(opts.ShellFiles) > 0 {
		for _, f := range opts.ShellFiles {
			if f != "" {
				s.files = append(s.files, scanFile{path: f})
			}
		}
	} else {
		for _, f := range DefaultShellFiles(s.opts.Policy.Home) {
			s.files = append(s.files, scanFile{path: f})
		}
	}
	return s, nil
}

// Scan 执行一次全量扫描。
func (s *Scanner) Scan() (Report, error) {
	rep := Report{ScannedAt: s.opts.Clock()}
	visited := map[string]bool{}
	var chain []string
	for _, f := range s.files {
		s.walk(f.path, f.system, 0, visited, &chain, &rep)
	}
	s.collectSession(&rep)
	rep.Items = s.conflicts(rep.Findings)
	return rep, nil
}

// walk 递归扫一个启动文件（含 source 跟随）。
//
// 去重与防环是两件事，都要有：visited 防同一文件被多路重复扫（结果去重），
// chain 是**当前包含链**，命中即报环并跳过（A2）。
func (s *Scanner) walk(path string, system bool, depth int, visited map[string]bool, chain *[]string, rep *Report) {
	real, err := canonicalPath(path)
	if err != nil {
		rep.note("无法解析启动文件路径 %s: %v", path, err)
		return
	}
	for _, c := range *chain {
		if c == real {
			rep.note("source 环：%s", strings.Join(append(append([]string{}, (*chain)...), real), " → "))
			return
		}
	}
	if visited[real] {
		return
	}
	visited[real] = true

	// 只在文件**存在**时检查（/etc/profile 在部分平台不存在属正常）。
	st, err := os.Stat(real)
	if err != nil || !st.Mode().IsRegular() {
		return
	}
	if st.Size() > MaxFileBytes {
		rep.note("启动文件超过读取上限 %d 字节，已跳过：%s", MaxFileBytes, real)
		return
	}
	data, err := os.ReadFile(real)
	if err != nil {
		rep.note("读取启动文件失败 %s: %v", real, err)
		return
	}

	*chain = append(*chain, real)
	defer func() { *chain = (*chain)[:len(*chain)-1] }()

	for _, ln := range splitRawLines(data) {
		if target, ok := sourceTarget(ln.body); ok {
			if depth+1 > s.opts.MaxSourceDepth {
				rep.note("source 深度超过上限 %d：%s 里的 %q 未跟随", s.opts.MaxSourceDepth, real, target)
				continue
			}
			s.walk(resolveSourcePath(target, real, s.opts.Policy.Home), system, depth+1, visited, chain, rep)
			continue
		}
		for _, a := range parseAssignments(ln.body) {
			key, ok := s.expected[a.name]
			if !ok {
				continue
			}
			rep.Findings = append(rep.Findings, Assignment{
				Name:       a.name,
				Value:      a.value,
				Key:        key,
				Overrides:  a.value != key.Value,
				RawPath:    path,
				RealPath:   real,
				Line:       ln.no,
				LineText:   string(ln.body),
				Repairable: !system && s.allow.allowed(real),
				Kind:       SourceFile,
			})
		}
	}
}

// collectSession 采集 launchd 会话环境与当前进程环境。
//
// 这两类来源**不可能**在本应用内删除（改的是登录会话与启动本应用的终端），
// 因此一律 repairable=false，UI 只解释、不给删除按钮。
func (s *Scanner) collectSession(rep *Report) {
	names := s.expectedNames()
	if len(names) == 0 {
		return
	}
	if s.opts.Launchd != nil {
		vals := s.opts.Launchd(names)
		for _, n := range names {
			v, ok := vals[n]
			if !ok || v == "" {
				continue
			}
			rep.Findings = append(rep.Findings, s.sessionAssignment(n, v, SourceLaunchd, LaunchdSourcePrefix+n))
		}
	}
	env := s.opts.ProcessEnv()
	for _, n := range names {
		v, ok := env[n]
		if !ok {
			continue
		}
		rep.Findings = append(rep.Findings, s.sessionAssignment(n, v, SourceProcess, ProcessSourcePrefix+n))
	}
}

func (s *Scanner) sessionAssignment(name, value string, kind SourceKind, source string) Assignment {
	key := s.expected[name]
	return Assignment{
		Name:       name,
		Value:      value,
		Key:        key,
		Overrides:  value != key.Value,
		RawPath:    source,
		Line:       0,
		LineText:   "",
		Repairable: false,
		Kind:       kind,
	}
}

func (s *Scanner) expectedNames() []string {
	names := make([]string, 0, len(s.expected))
	for n := range s.expected {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

// conflicts 把赋值提升成契约 DTO。
//
// 判定（A3）：命中决定键 + 值不等于期望值 = 覆盖行。文件来源里只有**生效**
// 那条进列表（shell 后写覆盖先写）；launchd / 进程环境是彼此独立的通道，
// 各自成立，不参与文件内的「最后一条」比较。
func (s *Scanner) conflicts(findings []Assignment) []EnvConflict {
	lastFile := map[string]int{}
	for i, a := range findings {
		if a.Kind == SourceFile {
			lastFile[a.Name] = i
		}
	}

	items := []EnvConflict{}
	for i, a := range findings {
		if !a.Overrides {
			continue
		}
		if a.Kind == SourceFile && lastFile[a.Name] != i {
			continue // 被更晚的同名赋值覆盖：它不是当前生效值
		}
		key := s.expected[a.Name]
		sensitive := sensitiveFor(a.Name, a.Value, key)
		findings[i].Sensitive = sensitive

		source := a.RealPath
		if a.Kind != SourceFile {
			source = a.RawPath
		}
		items = append(items, EnvConflict{
			ID:   conflictID(a.Name, source, a.Line),
			Name: a.Name,
			// 值字段与行文本同一条脱敏纪律（redact.go 的单源实现）：sensitive=false
			// 的条目也不许原样回显令牌形态——引号拼接、含空白的值形态此前只吃
			// sensitive 布尔，令牌明文直进响应正文（A3-fix 复核判红唯一项）。
			EffectiveValue: maskFieldValue(a.Name, a.Value, sensitive),
			ExpectedValue:  maskFieldValue(a.Name, key.Value, sensitive),
			Source:         source,
			Line:           a.Line,
			// 行文本脱敏是**整行级**的（redact.go 的单源实现）：同行的兄弟凭据与
			// 注释里的令牌形态一并掩掉，与「哪条条目触发这次渲染」无关（A4/A13）。
			LineText:   boundedLineText(redactLine(a.LineText, a.Value, sensitive)),
			Sensitive:  sensitive,
			Repairable: a.Repairable,
		})
	}
	sort.SliceStable(items, func(i, j int) bool {
		if items[i].Name != items[j].Name {
			return items[i].Name < items[j].Name
		}
		if items[i].Source != items[j].Source {
			return items[i].Source < items[j].Source
		}
		return items[i].Line < items[j].Line
	})
	return items
}

// note 记一条诊断；有界（最多 64 条），诊断再多也不该把内存撑大。
func (r *Report) note(format string, args ...any) {
	const maxNotes = 64
	if len(r.Notes) >= maxNotes {
		return
	}
	r.Notes = append(r.Notes, fmt.Sprintf(format, args...))
}

// ── 默认探测器 ──────────────────────────────────────────────────────────────

// launchdProbeTimeout 是单次 launchctl 调用的上限：探测是只读且非关键的，
// 卡住不该拖慢整轮扫描。
const launchdProbeTimeout = 3 * time.Second

// defaultLaunchdProbe 按名字点名查 launchd 会话环境（launchctl 只能点名查）。
// 非 darwin 平台返回空：会话环境在那里没有对应物。
func defaultLaunchdProbe(names []string) map[string]string {
	if runtime.GOOS != "darwin" {
		return nil
	}
	out := map[string]string{}
	for _, n := range names {
		ctx, cancel := context.WithTimeout(context.Background(), launchdProbeTimeout)
		b, err := exec.CommandContext(ctx, "launchctl", "getenv", n).Output()
		cancel()
		if err != nil {
			continue
		}
		v := strings.TrimRight(string(b), "\r\n")
		if v == "" {
			continue
		}
		out[n] = v
	}
	return out
}

// defaultProcessEnv 取当前进程环境快照。
func defaultProcessEnv() map[string]string {
	out := map[string]string{}
	for _, kv := range os.Environ() {
		if i := strings.IndexByte(kv, '='); i > 0 {
			out[kv[:i]] = kv[i+1:]
		}
	}
	return out
}

// ── 行与赋值解析（全程按字节，不做编码往返） ────────────────────────────────

// rawLine 是一行原始字节：body 不含行尾，term 是行尾（"\n" / "\r\n"，末行可空）。
type rawLine struct {
	no   int
	body []byte
	term []byte
	// start / end 是该行在文件字节里的区间（含行尾）。
	start, end int
}

// splitRawLines 按字节切行。
//
// 只认 LF 与 CRLF：孤立的 CR 视为行内容（老式 CR-only 文件极少见，把它当行尾
// 反而会误删半行）。行尾字节保存在 term 里，删行时整段（含行尾）一起消失，
// 其余行一个字节都不动（A5）。
func splitRawLines(data []byte) []rawLine {
	var out []rawLine
	i, no := 0, 0
	for i < len(data) {
		j := bytes.IndexByte(data[i:], '\n')
		end := len(data)
		if j >= 0 {
			end = i + j + 1
		}
		body, term := data[i:end], []byte(nil)
		if j >= 0 {
			if n := len(body); n >= 2 && body[n-2] == '\r' {
				body, term = body[:n-2], data[end-2:end]
			} else {
				body, term = body[:n-1], data[end-1:end]
			}
		}
		no++
		out = append(out, rawLine{no: no, body: body, term: term, start: i, end: end})
		i = end
	}
	return out
}

// parsedAssignment 是行内解析出的赋值。
type parsedAssignment struct{ name, value string }

// assignmentSpan 是行内一个赋值 token 的字节位置与解析结果。
//
// parseAssignments（只关心名与值）与整行脱敏（redact.go 要「只改值、保留键名与
// 结构与引号」）共用这一份词法：两处各写一遍迟早漂移，而漂移的那一半就是漏掩
// （UC-015 修订版 A4/A13）。
type assignmentSpan struct {
	tokStart int    // token 首字节在行内的偏移
	eq       int    // 未被引号包住的 '=' 在行内的偏移
	tokEnd   int    // token 末字节（不含）在行内的偏移
	name     string // 合法标识符形态的键名
}

// assignPrefixes 是 shell 的赋值前缀（与 v1 同清单）。
var assignPrefixes = [][]byte{
	[]byte("export "), []byte("declare -x "), []byte("typeset -x "), []byte("typeset "),
}

// parseAssignmentSpans 解析一行 shell 里的赋值并保留字节位置。
//
// 识别口径与 parseAssignments 逐字一致：只认「合法标识符 + `=`」，
// `if [ "$X" = "1" ]` 这类比较不会被误判成赋值；`export A=1 B=2` 会全部解析出来。
func parseAssignmentSpans(body []byte) []assignmentSpan {
	start := 0
	for start < len(body) && (body[start] == ' ' || body[start] == '\t') {
		start++
	}
	if start >= len(body) || body[start] == '#' {
		return nil
	}
	prefixEnd := start
	for _, p := range assignPrefixes {
		if bytes.HasPrefix(body[start:], p) {
			prefixEnd = start + len(p)
			break
		}
	}

	var out []assignmentSpan
	for _, sp := range tokenSpansPos(body, prefixEnd) {
		tok := body[sp[0]:sp[1]]
		eq := findUnquotedEq(tok)
		if eq < 0 {
			continue
		}
		name := string(tok[:eq])
		if !isValidIdentifier(name) {
			continue
		}
		out = append(out, assignmentSpan{tokStart: sp[0], eq: sp[0] + eq, tokEnd: sp[1], name: name})
	}
	return out
}

// parseAssignments 解析一行 shell 里的赋值。
//
// 只认「合法标识符 + `=`」：`if [ "$X" = "1" ]` 这类比较不会被误判成赋值；
// `export A=1 B=2` 会全部解析出来。`#` 行与行内注释、`export NAME`（无 `=`）
// 不产出赋值。
func parseAssignments(body []byte) []parsedAssignment {
	spans := parseAssignmentSpans(body)
	out := make([]parsedAssignment, 0, len(spans))
	for _, s := range spans {
		out = append(out, parsedAssignment{name: s.name, value: normalizeValue(body[s.eq+1 : s.tokEnd])})
	}
	return out
}

// tokenSpansPos 按 shell 词法把字节切成 token 的**位置区间**（空白与 `;` 分隔，
// 引号内不切，`#` 起注释）。
//
// 为什么保留位置而不是切片：整行脱敏要在原行上「只替换值区间」，没有位置就只能
// 整串替换，而整串替换会破坏键名与引号结构。
func tokenSpansPos(body []byte, from int) [][2]int {
	var out [][2]int
	i := from
	for i < len(body) {
		for i < len(body) && isSeparator(body[i]) {
			i++
		}
		if i >= len(body) || body[i] == '#' {
			break
		}
		start := i
		var quote byte
		for i < len(body) {
			c := body[i]
			if quote != 0 {
				switch {
				case c == '\\' && quote == '"':
					i += 2
					continue
				case c == quote:
					quote = 0
				}
				i++
				continue
			}
			switch {
			case c == '\\':
				i += 2
				continue
			case c == '\'' || c == '"':
				quote = c
				i++
				continue
			case isSeparator(c):
				// 到 token 边界：跳出内层循环，末尾统一收口。
			default:
				i++
				continue
			}
			break
		}
		if start > len(body) {
			break
		}
		end := i
		if end > len(body) {
			end = len(body)
		}
		out = append(out, [2]int{start, end})
	}
	return out
}

// tokenSpans 是 tokenSpansPos 的切片形态（既有调用点的字节视图口径，行为不变）。
func tokenSpans(body []byte, from int) [][]byte {
	spans := tokenSpansPos(body, from)
	out := make([][]byte, 0, len(spans))
	for _, s := range spans {
		out = append(out, body[s[0]:s[1]])
	}
	return out
}

func isSeparator(c byte) bool {
	return c == ' ' || c == '\t' || c == ';'
}

func isValidIdentifier(name string) bool {
	if name == "" {
		return false
	}
	for i := 0; i < len(name); i++ {
		c := name[i]
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c == '_':
		case c >= '0' && c <= '9':
			if i == 0 {
				return false
			}
		default:
			return false
		}
	}
	return true
}

// findUnquotedEq 找引号外（且未被反斜杠转义）的第一个 `=`。
func findUnquotedEq(tok []byte) int {
	var quote byte
	for i := 0; i < len(tok); i++ {
		c := tok[i]
		if quote != 0 {
			if c == '\\' && quote == '"' {
				i++
				continue
			}
			if c == quote {
				quote = 0
			}
			continue
		}
		switch c {
		case '\\':
			i++
			continue
		case '\'', '"':
			quote = c
		case '=':
			return i
		}
	}
	return -1
}

// normalizeValue 去掉一层配对的引号（只影响比较与展示，不动文件字节）。
func normalizeValue(raw []byte) string {
	s := strings.TrimSpace(string(raw))
	if len(s) >= 2 {
		switch {
		case s[0] == '"' && s[len(s)-1] == '"', s[0] == '\'' && s[len(s)-1] == '\'':
			return s[1 : len(s)-1]
		}
	}
	return s
}

// sourceTarget 识别行首的 `source <path>` / `. <path>` 指令。
//
// 只在行首识别：本机 rc 里的 source 都独占一行；命中该指令的行不再解析赋值，
// 避免把 `source foo=bar` 这种畸形行当成赋值。
func sourceTarget(body []byte) (string, bool) {
	trimmed := bytes.TrimLeft(body, " \t")
	for _, p := range [][]byte{[]byte("source "), []byte(". ")} {
		if !bytes.HasPrefix(trimmed, p) {
			continue
		}
		toks := tokenSpans(trimmed, len(p))
		if len(toks) == 0 {
			return "", false
		}
		t := normalizeValue(toks[0])
		if t == "" {
			return "", false
		}
		return t, true
	}
	return "", false
}

// resolveSourcePath 把 source 目标解析成绝对路径：支持 `~`、`$HOME`、`${HOME}`，
// 相对路径按**当前文件的目录**解析（shell 的语义）。
func resolveSourcePath(target, currentFile, home string) string {
	expanded := strings.ReplaceAll(target, "${HOME}", home)
	expanded = strings.ReplaceAll(expanded, "$HOME", home)

	if strings.HasPrefix(expanded, "~") {
		rest := strings.TrimPrefix(expanded, "~")
		rest = strings.TrimPrefix(rest, "/")
		if rest == "" {
			return home
		}
		return filepath.Join(home, rest)
	}
	if filepath.IsAbs(expanded) {
		return filepath.Clean(expanded)
	}
	return filepath.Join(filepath.Dir(currentFile), expanded)
}
