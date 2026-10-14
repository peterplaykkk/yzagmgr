// dotenv（`.env`）定位层（gemini 的 live 形态）。
//
// 职责是「只改这一行的值，其它字节一个都不动」：gemini 的决定键是
// `GOOGLE_GEMINI_BASE_URL`（acceptance-recipe §A 行 4），而用户常在同一文件里
// 攒着注释、空行、`export ` 前缀与单/双引号混用的历史值。
//
// 字节契约：非 UTF-8 与 CRLF 一律按字节处理——本层不做编码往返、不做换行归一化，
// 写入时沿用文件既有的换行风格（见 detectLineEnding）。
//
// 查询语义与 json.go 的 Doc 一致：Get / Raw / Keys 看到的是**解析时**的源状态，
// 编辑只影响 Render 的产物（同一轮内不做读改写回环）。
package docmodel

import (
	"bytes"
	"errors"
	"fmt"
	"sort"
	"strings"
)

// dotenv 层的错误。
var (
	ErrDotenvParse        = errors.New("dotenv 解析失败")
	ErrDotenvValueInvalid = errors.New("dotenv 值不合法")
	ErrDotenvEditConflict = errors.New("dotenv 编辑区间冲突")
)

// EnvLineKind 是行种类。Opaque 指「既不是注释也不是 KEY=VALUE」的行：
// 逐字节保留，但不可按键寻址（例如 `FOO` 单独一行、或语法古怪的历史行）。
type EnvLineKind uint8

const (
	EnvBlank EnvLineKind = iota
	EnvComment
	EnvAssignment
	EnvOpaque
)

// EnvLine 是 .env 中的一行及其字节区间。
type EnvLine struct {
	Kind         EnvLineKind
	Key          string
	Export       bool
	Start        int // 整行首字节
	End          int // 整行内容末（不含 EOL）
	ValStart     int // 值（含引号）首字节
	ValEnd       int // 值（含引号）末字节（不含尾随空白与注释）
	ContentStart int // 去引号后的内容首字节
	ContentEnd   int // 去引号后的内容末字节
	Quote        byte
}

// EnvDoc 是一份 .env：源字节 + 行索引 + 待应用编辑。
type EnvDoc struct {
	src   []byte
	lines []EnvLine
	byKey map[string][]int // 同名键保留全部出现位置（dotenv 语义是 last-wins）
	edits []edit
}

// ParseDotenv 解析 `KEY=VALUE` 行，记录每行的字节区间。
//
// 保留全部既存形态（注释、空行、引号、`export ` 前缀、缩进）；
// 引号未闭合是唯一的硬错误——那说明这行不是我们以为的那个键，
// 静默当成值会写坏用户的文件。
func ParseDotenv(src []byte) (*EnvDoc, error) {
	d := &EnvDoc{src: src, byKey: map[string][]int{}}
	for _, ln := range splitSrcLines(src) {
		el, err := parseEnvLine(src, ln)
		if err != nil {
			return nil, err
		}
		if el.Kind == EnvAssignment {
			d.byKey[el.Key] = append(d.byKey[el.Key], len(d.lines))
		}
		d.lines = append(d.lines, el)
	}
	return d, nil
}

func parseEnvLine(src []byte, ln srcLine) (EnvLine, error) {
	out := EnvLine{Kind: EnvBlank, Start: ln.start, End: ln.end}
	if ln.content >= ln.end {
		return out, nil
	}
	if src[ln.content] == '#' {
		out.Kind = EnvComment
		return out, nil
	}
	i := ln.content
	if hasEnvExportPrefix(src, i, ln.end) {
		out.Export = true
		i = skipSpaces(src, i+len("export"), ln.end)
	}
	keyStart := i
	for i < ln.end && isEnvKeyByte(src[i], i == keyStart) {
		i++
	}
	if i == keyStart {
		out.Kind = EnvOpaque
		return out, nil
	}
	keyEnd := i
	j := skipSpaces(src, i, ln.end)
	if j >= ln.end || src[j] != '=' {
		out.Kind = EnvOpaque
		return out, nil
	}
	out.Kind = EnvAssignment
	out.Key = string(src[keyStart:keyEnd])
	v := skipSpaces(src, j+1, ln.end)
	out.ValStart, out.ValEnd = v, v
	out.ContentStart, out.ContentEnd = v, v
	if v >= ln.end {
		return out, nil // 空值 `KEY=`
	}
	switch q := src[v]; q {
	case '"', '\'':
		end, err := scanEnvQuoted(src, v, ln.end, q)
		if err != nil {
			return EnvLine{}, err
		}
		out.Quote = q
		out.ValStart, out.ValEnd = v, end
		out.ContentStart, out.ContentEnd = v+1, end-1
	default:
		end := ln.end
		for k := v; k < ln.end; k++ {
			if src[k] == '#' && (k == v || src[k-1] == ' ' || src[k-1] == '\t') {
				end = k
				break
			}
		}
		for end > v && (src[end-1] == ' ' || src[end-1] == '\t') {
			end--
		}
		out.ValStart, out.ValEnd = v, end
		out.ContentStart, out.ContentEnd = v, end
	}
	return out, nil
}

// scanEnvQuoted 找到引号值的结束偏移（含闭合引号）；跨行即报错。
func scanEnvQuoted(src []byte, pos, lineEnd int, q byte) (int, error) {
	for i := pos + 1; i < lineEnd; i++ {
		c := src[i]
		if q == '"' && c == '\\' {
			i++
			continue
		}
		if c == q {
			return i + 1, nil
		}
	}
	return 0, fmt.Errorf("%w: 偏移 %d 处引号未闭合", ErrDotenvParse, pos)
}

func hasEnvExportPrefix(src []byte, i, end int) bool {
	if !bytes.HasPrefix(src[i:end], []byte("export")) {
		return false
	}
	j := i + len("export")
	return j < end && (src[j] == ' ' || src[j] == '\t')
}

// isEnvKeyByte 限定键字符：首字符是字母或下划线，其后可含数字、点、短横线。
// 更宽的字符集会把 `1FOO=bar` 之类写成键，而这类行更可能是用户的手写备注。
func isEnvKeyByte(c byte, first bool) bool {
	if c == '_' || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') {
		return true
	}
	if first {
		return false
	}
	return (c >= '0' && c <= '9') || c == '.' || c == '-'
}

// ── 查询 ────────────────────────────────────────────────────────────────────

// Src 返回原始源字节。
func (d *EnvDoc) Src() []byte { return d.src }

// Lines 返回全部行信息（源序）。
func (d *EnvDoc) Lines() []EnvLine { return d.lines }

// Dirty 报告是否有改动。
func (d *EnvDoc) Dirty() bool { return len(d.edits) > 0 }

// Keys 返回去重后的键名，顺序为首次出现次序。
func (d *EnvDoc) Keys() []string {
	seen := make(map[string]bool, len(d.byKey))
	out := make([]string, 0, len(d.byKey))
	for _, l := range d.lines {
		if l.Kind != EnvAssignment || seen[l.Key] {
			continue
		}
		seen[l.Key] = true
		out = append(out, l.Key)
	}
	return out
}

// Has 报告键是否存在。
func (d *EnvDoc) Has(key string) bool {
	_, ok := d.Raw(key)
	return ok
}

// Raw 返回键的值字节（引号内的原始字节，不解码转义）。
// 同名键按 dotenv 惯例取最后一处（last-wins）。
func (d *EnvDoc) Raw(key string) ([]byte, bool) {
	idxs := d.byKey[key]
	if len(idxs) == 0 {
		return nil, false
	}
	l := d.lines[idxs[len(idxs)-1]]
	return d.src[l.ContentStart:l.ContentEnd], true
}

// Get 返回键的值（字节保真的字符串形态；转义不解码）。
func (d *EnvDoc) Get(key string) (string, bool) {
	raw, ok := d.Raw(key)
	if !ok {
		return "", false
	}
	return string(raw), true
}

// ── 变更 ────────────────────────────────────────────────────────────────────

// add 记录一次编辑。
//
// 同一**非空**区间的重复设置视为「后者胜」；零长插入允许同点多次、按调用顺序
// 渲染——连续追加两个新键就是这种情形，把它当后者胜会静默丢掉先追加的那个。
func (d *EnvDoc) add(start, end int, repl []byte) error {
	if start < 0 || end > len(d.src) || start > end {
		return fmt.Errorf("%w: [%d,%d) 越界", ErrDotenvEditConflict, start, end)
	}
	for i := range d.edits {
		e := &d.edits[i]
		if start < e.end && e.start < end {
			if start == e.start && end == e.end && start != end {
				e.repl = repl
				return nil
			}
			return fmt.Errorf("%w: [%d,%d) 与 [%d,%d) 重叠", ErrDotenvEditConflict, start, end, e.start, e.end)
		}
	}
	d.edits = append(d.edits, edit{start: start, end: end, repl: repl})
	return nil
}

// Set 把键的值替换为 value，只改该行的值段。
//
// 引号形态跟随原行（单引号值继续用单引号，除非新值里含单引号）；
// 无引号的值在新值含空白、`#`、引号、反斜杠或 `=` 时改用双引号——
// 否则写出的 `KEY=a b` 会被读者截断成两段，是静默失效。
func (d *EnvDoc) Set(key, value string) error {
	quote := d.quoteStyle(key)
	raw, err := encodeEnvValue(value, quote)
	if err != nil {
		return err
	}
	return d.replaceOrAppend(key, raw)
}

// SetRaw 用给定字节替换键的值段（不做引号加工，原样落盘）。
func (d *EnvDoc) SetRaw(key string, rawValue []byte) error {
	if bytes.IndexByte(rawValue, '\n') >= 0 || bytes.IndexByte(rawValue, '\r') >= 0 {
		return fmt.Errorf("%w: 值含换行，会破坏行结构", ErrDotenvValueInvalid)
	}
	return d.replaceOrAppend(key, rawValue)
}

// Delete 删除该键的**全部**行；键不存在时无操作（幂等）。
//
// 「全部」不是顺手为之：dotenv 是 last-wins 语义，只删最后一行会让先前的旧值
// 重新生效——那正好是档位切换里最难查的一类残留。
func (d *EnvDoc) Delete(key string) error {
	for _, idx := range d.byKey[key] {
		l := d.lines[idx]
		if err := d.add(l.Start, lineEndAfter(d.src, l.End), nil); err != nil {
			return err
		}
	}
	return nil
}

// quoteStyle 返回该键当前行的引号形态（0 表示未加引号）。
func (d *EnvDoc) quoteStyle(key string) byte {
	idxs := d.byKey[key]
	if len(idxs) == 0 {
		return 0
	}
	return d.lines[idxs[len(idxs)-1]].Quote
}

func (d *EnvDoc) replaceOrAppend(key string, raw []byte) error {
	idxs := d.byKey[key]
	if len(idxs) == 0 {
		return d.appendAssignment(key, raw)
	}
	l := d.lines[idxs[len(idxs)-1]]
	return d.add(l.ValStart, l.ValEnd, raw)
}

// appendAssignment 在文件末尾追加一行；沿用既有换行风格，
// 文件不以换行结尾时先补一个（否则新键会黏在最后一行上）。
func (d *EnvDoc) appendAssignment(key string, raw []byte) error {
	eol := detectLineEnding(d.src)
	ins := key + "=" + string(raw) + eol
	// 只有第一次追加需要补「文件末尾缺的那个换行」；第二次再补就会多出空行。
	if len(d.src) > 0 && !bytes.HasSuffix(d.src, []byte("\n")) && !d.appendsAtEOF() {
		ins = eol + ins
	}
	return d.add(len(d.src), len(d.src), []byte(ins))
}

// appendsAtEOF 报告文件末尾已经排了一次追加（同一轮里连续写多个新键）。
func (d *EnvDoc) appendsAtEOF() bool {
	for _, e := range d.edits {
		if e.start == len(d.src) {
			return true
		}
	}
	return false
}

// Render 应用全部编辑；未被编辑的字节逐字节保留。
func (d *EnvDoc) Render() []byte {
	out := make([]byte, 0, len(d.src)+64)
	if len(d.edits) == 0 {
		return append(out, d.src...)
	}
	es := append([]edit(nil), d.edits...)
	sort.SliceStable(es, func(i, j int) bool { return es[i].start < es[j].start })
	prev := 0
	for _, e := range es {
		out = append(out, d.src[prev:e.start]...)
		out = append(out, e.repl...)
		prev = e.end
	}
	return append(out, d.src[prev:]...)
}

// encodeEnvValue 按目标引号形态产出值的字节。形态维护不住时（单引号里含单引号）
// 退化为双引号，绝不产出一个自己都读不回来的值。
func encodeEnvValue(value string, quote byte) ([]byte, error) {
	if strings.ContainsAny(value, "\n\r") {
		return nil, fmt.Errorf("%w: 值含换行，会破坏行结构", ErrDotenvValueInvalid)
	}
	switch quote {
	case '\'':
		if !strings.Contains(value, "'") {
			return []byte("'" + value + "'"), nil
		}
		return []byte(`"` + escapeEnvDouble(value) + `"`), nil
	case '"':
		return []byte(`"` + escapeEnvDouble(value) + `"`), nil
	}
	if !envValueNeedsQuotes(value) {
		return []byte(value), nil
	}
	if !strings.Contains(value, `"`) {
		return []byte(`"` + value + `"`), nil
	}
	return []byte(`"` + escapeEnvDouble(value) + `"`), nil
}

func envValueNeedsQuotes(value string) bool {
	if value == "" {
		return true
	}
	return strings.ContainsAny(value, " \t#\"'\\=")
}

func escapeEnvDouble(value string) string {
	var sb strings.Builder
	for i := 0; i < len(value); i++ {
		switch c := value[i]; c {
		case '"':
			sb.WriteString(`\"`)
		case '\\':
			sb.WriteString(`\\`)
		case '\t':
			sb.WriteString(`\t`)
		default:
			sb.WriteByte(c)
		}
	}
	return sb.String()
}
