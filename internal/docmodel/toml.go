// TOML 定位层：字节保真地定位与改写 config.toml 里的段与键。
//
// WHY 不用第三方 TOML 库做「解析成 map 再序列化」：那会重排键序、丢掉注释与空白，
// 破坏 acceptance-recipe §C/§E 的 sha256 逐字节断言。本层只记录字节区间，
// 改动以「区间 → 新字节」表达，渲染时按区间拼接，未被触碰的字节原样通过。
//
// 消费方（acceptance-recipe §A 行 3 codex / 行 5 grokbuild）的写入都落在
// 「顶层键 + 单个 [section] 段」上，所以本层把「只动一段」「只动一行的一段的值」
// 做成显式 API（SetSectionOnlyKey / SetKey），而不是让调用方自己算区间。
//
// 同一段要连续写多个键（codex 一次写四个），因此插入用**插入点**（tomlSite）收拢：
// 新键先攒在插入点里，改键只重建这一段字节。没有它，第二次调用会「找不到段再建
// 一次段」，产出重复段（非法 TOML）或覆盖掉上一次的插入。
package docmodel

import (
	"bytes"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// 解析与写入的错误。调用方按 errors.Is 分支（例如「键不存在 → 改走插入」）。
var (
	ErrTOMLParse            = errors.New("TOML 解析失败")
	ErrTOMLDuplicateSection = errors.New("TOML 段重复定义")
	ErrTOMLDuplicateKey     = errors.New("TOML 段内键重复定义")
	ErrTOMLNoSuchSection    = errors.New("TOML 段不存在")
	ErrTOMLNoSuchKey        = errors.New("TOML 键不存在")
	ErrTOMLValueInvalid     = errors.New("TOML 值不合法")
	ErrTOMLArrayOfTables    = errors.New("TOML 段是数组表（[[...]]），无法按键寻址")
	ErrTOMLEditConflict     = errors.New("TOML 编辑区间冲突")
)

// TOMLKind 是值的形态。
//
// 它存在的理由不是「多一个字段」：grokbuild 的 max_completion_tokens 必须写成
// 裸整数，写成字符串会让整个 [model."<id>"] override 被静默丢弃。调用方需要
// 一个可断言的形态判据，而不是靠猜。
type TOMLKind uint8

const (
	TOMLString TOMLKind = iota
	TOMLInteger
	TOMLFloat
	TOMLBool
	TOMLDateTime
	TOMLArray
	TOMLInlineTable
)

// String 返回形态名（错误信息与日志用）。
func (k TOMLKind) String() string {
	switch k {
	case TOMLString:
		return "string"
	case TOMLInteger:
		return "integer"
	case TOMLFloat:
		return "float"
	case TOMLBool:
		return "bool"
	case TOMLDateTime:
		return "datetime"
	case TOMLArray:
		return "array"
	case TOMLInlineTable:
		return "inline-table"
	}
	return "unknown"
}

// TOMLValue 是某个键在源文本中的值。
type TOMLValue struct {
	Kind  TOMLKind
	Start int // 值首字节（含引号等记号本身）
	End   int // 值末字节（右开；不含尾随空白与注释）
	src   []byte
}

// Raw 返回值的原始字节（未编辑时即源片段）。
func (v *TOMLValue) Raw() []byte {
	if v == nil {
		return nil
	}
	return v.src[v.Start:v.End]
}

// String 返回字符串值的**解码后**文本（去引号、展开转义）；非字符串返回 ok=false。
func (v *TOMLValue) String() (string, bool) {
	if v == nil || v.Kind != TOMLString {
		return "", false
	}
	s, err := decodeTOMLString(v.Raw())
	if err != nil {
		return string(v.Raw()), true
	}
	return s, true
}

// Int 返回整数值；非整数或溢出返回 ok=false。
func (v *TOMLValue) Int() (int64, bool) {
	if v == nil || v.Kind != TOMLInteger {
		return 0, false
	}
	n, err := strconv.ParseInt(strings.ReplaceAll(string(v.Raw()), "_", ""), 0, 64)
	if err != nil {
		return 0, false
	}
	return n, true
}

// Float 返回浮点值（含 inf/nan）；非浮点返回 ok=false。
func (v *TOMLValue) Float() (float64, bool) {
	if v == nil || (v.Kind != TOMLFloat && v.Kind != TOMLInteger) {
		return 0, false
	}
	f, err := strconv.ParseFloat(strings.ReplaceAll(string(v.Raw()), "_", ""), 64)
	if err != nil {
		return 0, false
	}
	return f, true
}

// Bool 返回布尔值；非布尔返回 ok=false。
func (v *TOMLValue) Bool() (bool, bool) {
	if v == nil || v.Kind != TOMLBool {
		return false, false
	}
	switch string(v.Raw()) {
	case "true":
		return true, true
	case "false":
		return false, true
	}
	return false, false
}

// IsEmpty 报告值区间是否为零长（理论上不会出现；供调用方做防御性判断）。
func (v *TOMLValue) IsEmpty() bool { return v == nil || v.Start == v.End }

// ── 文档 ────────────────────────────────────────────────────────────────────

// tomlKey 是一个键在源文本中的定位。
type tomlKey struct {
	Name     string // 规范化键名（点分路径，必要时带引号）
	KeyStart int    // 键记号首字节
	LineEnd  int    // 该键所在行的行内容末（不含 EOL）
	ValStart int
	ValEnd   int
	Kind     TOMLKind
}

// tomlSection 是一个段（根段没有段头）。
type tomlSection struct {
	Path        string // 规范化段名；根段为 ""
	HeaderStart int    // '[' 的偏移；根段为 -1
	HeaderEnd   int    // ']'（或 ']]'）之后
	Array       bool   // [[...]] 数组表：源序保留，但不按键寻址
	keys        []tomlKey
	byKey       map[string]int
}

func newTOMLSection(path string) *tomlSection {
	return &tomlSection{Path: path, HeaderStart: -1, byKey: map[string]int{}}
}

// key 按规范化名查找键。
func (s *tomlSection) key(name string) (*tomlKey, bool) {
	if s == nil {
		return nil, false
	}
	i, ok := s.byKey[name]
	if !ok {
		return nil, false
	}
	return &s.keys[i], true
}

// tomlSite 是「某个段的插入点」：同一段里新插入的键都落在这里，可以反复改写。
//
// 为什么需要它：写链会连续写同一段的多个键（codex 的 [model_providers.custom]
// 一次写四个），而刚插入的键不在解析树里。没有插入点，第二次调用就会「找不到段
// → 再建一次段」，产出重复段（非法 TOML）或把上一次的插入整段覆盖掉——两种都是
// 静默的数据事故。
type tomlSite struct {
	path    string
	editIdx int    // 该插入点占用的编辑下标
	header  bool   // 插入点自带段头（该段是新建的）
	prefix  []byte // 段头之前要补的换行（按文件既有排版决定）
	indent  []byte // 新键行的缩进，跟随该段最后一个键
	lead    bool   // true = 插入点在某一行的行尾之前，每行自带前导换行
	keys    []*tomlPendingKey
	repl    []byte // 当前待渲染字节（每次改动后重建）
}

// tomlPendingKey 是一个还没落盘的键。
type tomlPendingKey struct {
	name string
	raw  []byte
	kind TOMLKind
	off  int // 值在 site.repl 中的偏移（GetKey 靠它给出可读的 TOMLValue）
}

func (s *tomlSite) pendingKey(name string) *tomlPendingKey {
	for _, pk := range s.keys {
		if pk.name == name {
			return pk
		}
	}
	return nil
}

// TOMLDoc 是一份已解析的 TOML：源字节 + 段/键索引 + 待应用编辑。
type TOMLDoc struct {
	src    []byte
	root   *tomlSection
	sects  []*tomlSection // 源序（不含根段；含数组表）
	byPath map[string]*tomlSection
	arrays map[string]bool // 数组表段名集合（不可寻址）
	sites  map[string]*tomlSite
	order  []string // 插入点创建顺序（决定新建段的可见次序）
	edits  []edit
}

// ParseTOML 解析一份 TOML，记录段与键的字节区间。
//
// 严格性：段/键重复、非法值、未闭合的字符串或集合一律报错（不猜测）。
// 数组表 [[x]] 会被识别并保留，但标记为不可按键寻址——同一段名可以合法重复出现，
// 按名取值必然要猜第几个元素。
func ParseTOML(src []byte) (*TOMLDoc, error) {
	d := &TOMLDoc{
		src:    src,
		root:   newTOMLSection(""),
		byPath: map[string]*tomlSection{},
		arrays: map[string]bool{},
		sites:  map[string]*tomlSite{},
	}
	p := &tomlParser{src: src, doc: d, cur: d.root}
	for p.pos < len(src) {
		lineStart := p.pos
		contentEnd := lineContentEnd(src, lineStart)
		content := skipSpaces(src, lineStart, contentEnd)
		switch {
		case content >= contentEnd: // 空行
			p.pos = lineNext(src, contentEnd)
		case src[content] == '#': // 整行注释
			p.pos = lineNext(src, contentEnd)
		case src[content] == '[':
			if err := p.header(content, contentEnd); err != nil {
				return nil, err
			}
		default:
			if err := p.keyValue(content, contentEnd); err != nil {
				return nil, err
			}
		}
	}
	return d, nil
}

type tomlParser struct {
	src []byte
	pos int
	doc *TOMLDoc
	cur *tomlSection
}

// header 解析一个段头（[a] / [a.b] / [a."b c"] / [[a]]）。
func (p *tomlParser) header(content, contentEnd int) error {
	i := content + 1
	array := false
	if i < contentEnd && p.src[i] == '[' {
		array = true
		i++
	}
	segs, end, err := parseTOMLPath(p.src, i)
	if err != nil {
		return err
	}
	if end >= contentEnd || p.src[end] != ']' {
		return fmt.Errorf("%w: 偏移 %d 处的段名未以 ']' 结束", ErrTOMLParse, end)
	}
	end++
	if array {
		if end >= contentEnd || p.src[end] != ']' {
			return fmt.Errorf("%w: 偏移 %d 处的数组表未以 ']]' 结束", ErrTOMLParse, end)
		}
		end++
	}
	if rest := skipSpaces(p.src, end, contentEnd); rest < contentEnd && p.src[rest] != '#' {
		return fmt.Errorf("%w: 偏移 %d 处段头后有额外内容", ErrTOMLParse, rest)
	}
	path := canonTOMLPath(segs)
	sec := newTOMLSection(path)
	sec.Array = array
	sec.HeaderStart = content
	sec.HeaderEnd = end
	switch {
	case array:
		if _, dup := p.doc.byPath[path]; dup {
			return fmt.Errorf("%w: %s（表与数组表不能同名）", ErrTOMLDuplicateSection, path)
		}
		p.doc.arrays[path] = true
	case p.doc.arrays[path]:
		return fmt.Errorf("%w: %s（表与数组表不能同名）", ErrTOMLDuplicateSection, path)
	default:
		if _, dup := p.doc.byPath[path]; dup {
			return fmt.Errorf("%w: %s", ErrTOMLDuplicateSection, path)
		}
		p.doc.byPath[path] = sec
	}
	p.doc.sects = append(p.doc.sects, sec)
	p.cur = sec
	p.pos = lineNext(p.src, contentEnd)
	return nil
}

// keyValue 解析一行 key = value。值可以跨行（多行字符串、多行数组），
// 因此本方法负责把 p.pos 推进到该值最后一行的下一行。
func (p *tomlParser) keyValue(content, contentEnd int) error {
	segs, end, err := parseTOMLPath(p.src, content)
	if err != nil {
		return err
	}
	if end >= contentEnd || p.src[end] != '=' {
		return fmt.Errorf("%w: 偏移 %d 处期望 '='", ErrTOMLParse, end)
	}
	valStart := skipSpaces(p.src, end+1, contentEnd)
	if valStart >= contentEnd {
		return fmt.Errorf("%w: 偏移 %d 处的键缺少值", ErrTOMLParse, end)
	}
	valEnd, kind, err := scanTOMLValue(p.src, valStart)
	if err != nil {
		return err
	}
	// 值所在行的行尾只允许空白与注释。
	vLineEnd := lineContentEnd(p.src, valEnd)
	tail := skipSpaces(p.src, valEnd, vLineEnd)
	if tail < vLineEnd && p.src[tail] != '#' {
		return fmt.Errorf("%w: 偏移 %d 处值后有额外内容", ErrTOMLParse, tail)
	}
	k := tomlKey{
		Name:     canonTOMLPath(segs),
		KeyStart: content,
		LineEnd:  vLineEnd,
		ValStart: valStart,
		ValEnd:   valEnd,
		Kind:     kind,
	}
	sec := p.cur
	if sec.Array {
		// 数组表内部允许重复键（不同元素合法地重复同一批键），不查重。
		sec.keys = append(sec.keys, k)
	} else {
		if _, dup := sec.byKey[k.Name]; dup {
			where := sec.Path
			if where == "" {
				where = "<根>"
			}
			return fmt.Errorf("%w: %s.%s", ErrTOMLDuplicateKey, where, k.Name)
		}
		sec.byKey[k.Name] = len(sec.keys)
		sec.keys = append(sec.keys, k)
	}
	p.pos = lineNext(p.src, vLineEnd)
	return nil
}

// ── 查询 ────────────────────────────────────────────────────────────────────

// Src 返回原始源字节。
func (d *TOMLDoc) Src() []byte { return d.src }

// Dirty 报告是否有改动。
func (d *TOMLDoc) Dirty() bool { return len(d.edits) > 0 }

// GetKey 取段内键的值；段或键不存在（或段是数组表）返回 nil。
// section 用 TOML 键语法书写（见 SectionPath）；根段的 section 传空串。
//
// 待渲染的键（同一轮里刚 SetSectionOnlyKey 插进去的）也可见：返回值指向插入点的
// 待渲染字节，因此「写完立刻回读」不会看到过期的 nil。
func (d *TOMLDoc) GetKey(section, key string) *TOMLValue {
	secPath, err := canonTOMLPathText(section)
	if err != nil {
		return nil
	}
	segs, _, err := parseTOMLPath([]byte(key), 0)
	if err != nil || len(segs) == 0 {
		return nil
	}
	name := canonTOMLPath(segs)
	if s := d.sites[secPath]; s != nil {
		if pk := s.pendingKey(name); pk != nil {
			return &TOMLValue{Kind: pk.kind, Start: pk.off, End: pk.off + len(pk.raw), src: s.repl}
		}
	}
	sec, ok := d.findSection(secPath)
	if !ok {
		return nil
	}
	k, ok := sec.key(name)
	if !ok {
		return nil
	}
	return &TOMLValue{Kind: k.Kind, Start: k.ValStart, End: k.ValEnd, src: d.src}
}

// HasSection 报告段是否存在（含本轮新建、尚未渲染的段；数组表不算）。
func (d *TOMLDoc) HasSection(section string) bool {
	secPath, err := canonTOMLPathText(section)
	if err != nil {
		return false
	}
	if _, ok := d.sites[secPath]; ok {
		return true
	}
	_, ok := d.findSection(secPath)
	return ok
}

// Sections 返回全部普通段名（源序，其后是本轮新建的段）。
func (d *TOMLDoc) Sections() []string {
	out := make([]string, 0, len(d.sects)+len(d.order))
	for _, s := range d.sects {
		if s.Array {
			continue
		}
		out = append(out, s.Path)
	}
	for _, p := range d.order {
		if s := d.sites[p]; s != nil && s.header {
			out = append(out, p)
		}
	}
	return out
}

// ArrayOfTables 返回全部数组表段名（源序）。
func (d *TOMLDoc) ArrayOfTables() []string {
	out := make([]string, 0, len(d.arrays))
	for _, s := range d.sects {
		if s.Array {
			out = append(out, s.Path)
		}
	}
	return out
}

// Keys 返回段内全部键名（源序，其后是本轮新插入的键）。
func (d *TOMLDoc) Keys(section string) []string {
	secPath, err := canonTOMLPathText(section)
	if err != nil {
		return nil
	}
	var out []string
	if sec, ok := d.findSection(secPath); ok {
		for _, k := range sec.keys {
			out = append(out, k.Name)
		}
	}
	if s := d.sites[secPath]; s != nil {
		for _, pk := range s.keys {
			out = append(out, pk.name)
		}
	}
	return out
}

// findSection 按规范化段名查找可寻址的段；根段永远存在。
func (d *TOMLDoc) findSection(section string) (*tomlSection, bool) {
	path, err := canonTOMLPathText(section)
	if err != nil {
		return nil, false
	}
	if path == "" {
		return d.root, true
	}
	if d.arrays[path] {
		return nil, false
	}
	sec, ok := d.byPath[path]
	return sec, ok
}

// sectionFor 与 findSection 同义，但把「段不存在」「段是数组表」区分成不同错误。
func (d *TOMLDoc) sectionFor(section string) (*tomlSection, error) {
	path, err := canonTOMLPathText(section)
	if err != nil {
		return nil, err
	}
	if path == "" {
		return d.root, nil
	}
	if d.arrays[path] {
		return nil, fmt.Errorf("%w: %s", ErrTOMLArrayOfTables, path)
	}
	sec, ok := d.byPath[path]
	if !ok {
		return nil, fmt.Errorf("%w: %s", ErrTOMLNoSuchSection, path)
	}
	return sec, nil
}

// ── 变更 ────────────────────────────────────────────────────────────────────

// add 记录一次编辑。
//
// 同一**非空**区间的重复设置视为「后者胜」（调用方重试同一键时不应自锁）；
// 零长插入允许多次落在同一点上，按调用顺序渲染——「两个新建段都追加到文件末尾」
// 就是这种情形，把它当成后者胜会静默丢掉前一段。
func (d *TOMLDoc) add(start, end int, repl []byte) error {
	if start < 0 || end > len(d.src) || start > end {
		return fmt.Errorf("%w: [%d,%d) 越界", ErrTOMLEditConflict, start, end)
	}
	for i := range d.edits {
		e := &d.edits[i]
		if start < e.end && e.start < end {
			if start == e.start && end == e.end && start != end {
				e.repl = repl
				return nil
			}
			return fmt.Errorf("%w: [%d,%d) 与 [%d,%d) 重叠", ErrTOMLEditConflict, start, end, e.start, e.end)
		}
	}
	d.edits = append(d.edits, edit{start: start, end: end, repl: repl})
	return nil
}

// covered 报告某偏移是否已落在某次编辑的区间内。
func (d *TOMLDoc) covered(pos int) bool {
	for _, e := range d.edits {
		if pos > e.start && pos < e.end {
			return true
		}
	}
	return false
}

// newSite 建一个插入点（一条零长编辑 + 它的待渲染键表）。
func (d *TOMLDoc) newSite(secPath string, header, lead bool, prefix, indent []byte, pos int) (*tomlSite, error) {
	if d.covered(pos) {
		return nil, fmt.Errorf("%w: 段 %q 在偏移 %d 处无可用插入点", ErrTOMLEditConflict, secPath, pos)
	}
	if err := d.add(pos, pos, nil); err != nil {
		return nil, err
	}
	s := &tomlSite{path: secPath, editIdx: len(d.edits) - 1, header: header, prefix: prefix, indent: indent, lead: lead}
	d.sites[secPath] = s
	d.order = append(d.order, secPath)
	return s, nil
}

// appendSiteKey 在插入点追加一个键。
func (d *TOMLDoc) appendSiteKey(s *tomlSite, name string, rawValue []byte, kind TOMLKind) error {
	s.keys = append(s.keys, &tomlPendingKey{name: name, raw: tomlCloneBytes(rawValue), kind: kind})
	return d.rebuild(s)
}

// rebuild 重建插入点的待渲染字节。
//
// 两种排版由 lead 决定：插入点落在某行行尾之前时，每行必须自带前导换行；
// 落在行首之前（根段无键、插到首个段头之前）或文件末尾时，每行自带尾换行。
func (d *TOMLDoc) rebuild(s *tomlSite) error {
	eol := detectLineEnding(d.src)
	var b bytes.Buffer
	if s.header {
		b.Write(s.prefix)
		b.WriteString("[" + s.path + "]")
		b.WriteString(eol)
	}
	for _, pk := range s.keys {
		if s.lead {
			b.WriteString(eol)
		}
		b.Write(s.indent)
		b.WriteString(SectionSegment(pk.name))
		b.WriteString(" = ")
		pk.off = b.Len()
		b.Write(pk.raw)
		if !s.lead {
			b.WriteString(eol)
		}
	}
	s.repl = b.Bytes()
	d.edits[s.editIdx].repl = s.repl
	return nil
}

// dropSite 撤销一个插入点（它的键被删光了）：必须连编辑一起撤，
// 否则文件里会留下一个空段头或一行幽灵换行。
func (d *TOMLDoc) dropSite(secPath string) error {
	s := d.sites[secPath]
	if s == nil {
		return nil
	}
	d.edits = append(d.edits[:s.editIdx], d.edits[s.editIdx+1:]...)
	delete(d.sites, secPath)
	for i, p := range d.order {
		if p == secPath {
			d.order = append(d.order[:i], d.order[i+1:]...)
			break
		}
	}
	for _, other := range d.sites {
		if other.editIdx > s.editIdx {
			other.editIdx--
		}
	}
	return nil
}

// siteForParsedSection 为已存在的段建插入点，位置跟随该段既有排版。
func (d *TOMLDoc) siteForParsedSection(sec *tomlSection) (*tomlSite, error) {
	if len(sec.keys) > 0 {
		last := sec.keys[len(sec.keys)-1]
		indent := leadingIndentBytes(d.src, last.KeyStart)
		if pos := last.LineEnd; !d.covered(pos) {
			return d.newSite(sec.Path, false, true, nil, indent, pos)
		}
	}
	// 段内没有键（或最后一个键所在行同时被编辑）。
	switch {
	case sec.HeaderStart >= 0:
		// 落到段头行之后（仍在段头那一行内插入）。
		return d.newSite(sec.Path, false, true, nil, nil, lineContentEnd(d.src, sec.HeaderStart))
	case len(d.sects) > 0:
		// 根段且无键：插到第一个段头行之前，保住文件顶部的注释块。
		return d.newSite(sec.Path, false, false, nil, nil, lineStartAt(d.src, d.sects[0].HeaderStart))
	default:
		return d.newSite(sec.Path, false, false, nil, nil, len(d.src))
	}
}

// createSectionSite 在文件末尾新建段；也走插入点，后续键继续落在这里。
func (d *TOMLDoc) createSectionSite(secPath, name string, rawValue []byte, kind TOMLKind) error {
	eol := detectLineEnding(d.src)
	s, err := d.newSite(secPath, true, false, []byte(d.sectionPrefix(eol)), nil, len(d.src))
	if err != nil {
		return err
	}
	return d.appendSiteKey(s, name, rawValue, kind)
}

// SetKey 把段内已存在键的值替换为 rawValue。
//
// 只改值的字节区间：该行的 `key = ` 前缀、行尾注释、文件其余部分一律不动。
// 键或段不存在时报错（ErrTOMLNoSuchKey / ErrTOMLNoSuchSection）——
// 插入语义由 SetSectionOnlyKey 表达，两者分开是为了让「我以为它在改、
// 其实它插了一行」这种事故不可能发生。
func (d *TOMLDoc) SetKey(section, key string, rawValue []byte) error {
	kind, secPath, name, err := d.target(section, key, rawValue)
	if err != nil {
		return err
	}
	if s := d.sites[secPath]; s != nil {
		if pk := s.pendingKey(name); pk != nil {
			pk.raw, pk.kind = tomlCloneBytes(rawValue), kind
			return d.rebuild(s)
		}
		// 插入点里没有，不代表段里没有：已解析的同名键仍然只改值。
		if sec, ok := d.findSection(secPath); ok {
			if k, ok := sec.key(name); ok {
				return d.add(k.ValStart, k.ValEnd, rawValue)
			}
		}
		return fmt.Errorf("%w: %s", ErrTOMLNoSuchKey, tomlQualify(secPath, name))
	}
	sec, err := d.sectionFor(secPath)
	if err != nil {
		return err
	}
	k, ok := sec.key(name)
	if !ok {
		return fmt.Errorf("%w: %s", ErrTOMLNoSuchKey, tomlQualify(secPath, name))
	}
	return d.add(k.ValStart, k.ValEnd, rawValue)
}

// SetSectionOnlyKey 确保段内键等于 rawValue：键存在则只替换值，不存在则按既有
// 排版插入到该段末尾；段不存在则在文件末尾创建段并插入。
//
// 「SectionOnly」是承诺而非措辞：产生的编辑全部落在目标段/目标行的字节区间内，
// 不重排、不触碰其它段（codex 的 [model_providers.custom] 因此能与用户自建的
// [projects]/[tui] 共存）。同一段连续写多个键是常态：这些键由同一个插入点收拢
// （见 tomlSite），所以「新建段后继续加键」不会产出重复段。
func (d *TOMLDoc) SetSectionOnlyKey(section, key string, rawValue []byte) error {
	kind, secPath, name, err := d.target(section, key, rawValue)
	if err != nil {
		return err
	}
	if s := d.sites[secPath]; s != nil {
		if pk := s.pendingKey(name); pk != nil {
			pk.raw, pk.kind = tomlCloneBytes(rawValue), kind
			return d.rebuild(s)
		}
		// 插入点里没有，不代表段里没有：已解析的同名键仍然只改值，否则会插出重复键。
		if sec, ok := d.findSection(secPath); ok {
			if k, ok := sec.key(name); ok {
				return d.add(k.ValStart, k.ValEnd, rawValue)
			}
		}
		return d.appendSiteKey(s, name, rawValue, kind)
	}
	sec, err := d.sectionFor(secPath)
	switch {
	case err == nil:
		if k, ok := sec.key(name); ok {
			return d.add(k.ValStart, k.ValEnd, rawValue)
		}
		s, serr := d.siteForParsedSection(sec)
		if serr != nil {
			return serr
		}
		return d.appendSiteKey(s, name, rawValue, kind)
	case errors.Is(err, ErrTOMLNoSuchSection):
		return d.createSectionSite(secPath, name, rawValue, kind)
	default:
		return err
	}
}

// DeleteKey 删除段内键所在的整行；键不存在时无操作（幂等）。
// 对本轮刚插入、尚未渲染的键，删除即撤销那次插入。
func (d *TOMLDoc) DeleteKey(section, key string) error {
	secPath, err := canonTOMLPathText(section)
	if err != nil {
		return err
	}
	name, err := canonTOMLPathText(key)
	if err != nil {
		return err
	}
	if s := d.sites[secPath]; s != nil {
		for i, pk := range s.keys {
			if pk.name != name {
				continue
			}
			s.keys = append(s.keys[:i], s.keys[i+1:]...)
			if len(s.keys) == 0 {
				return d.dropSite(secPath)
			}
			return d.rebuild(s)
		}
	}
	sec, err := d.sectionFor(secPath)
	if err != nil {
		if errors.Is(err, ErrTOMLNoSuchSection) {
			return nil
		}
		return err
	}
	k, ok := sec.key(name)
	if !ok {
		return nil
	}
	return d.add(lineStartAt(d.src, k.KeyStart), lineEndAfter(d.src, k.LineEnd), nil)
}

// target 校验并规范化一次写操作的入参（值形态 + 段名 + 键名）。
func (d *TOMLDoc) target(section, key string, rawValue []byte) (TOMLKind, string, string, error) {
	if err := validateTOMLValue(rawValue); err != nil {
		return 0, "", "", err
	}
	kind, err := tomlValueKind(rawValue)
	if err != nil {
		return 0, "", "", err
	}
	secPath, err := canonTOMLPathText(section)
	if err != nil {
		return 0, "", "", err
	}
	name, err := canonTOMLPathText(key)
	if err != nil {
		return 0, "", "", err
	}
	return kind, secPath, name, nil
}

// tomlValueKind 给出原始值字节的形态。
func tomlValueKind(raw []byte) (TOMLKind, error) {
	_, kind, err := scanTOMLValue(raw, 0)
	return kind, err
}

// tomlQualify 把段名与键名拼成可读的定位串（根段不显示为空前缀）。
func tomlQualify(secPath, name string) string {
	if secPath == "" {
		return name
	}
	return secPath + "." + name
}

func tomlCloneBytes(b []byte) []byte { return append([]byte(nil), b...) }

// SetString 确保字符串键等于 value（upsert）。
func (d *TOMLDoc) SetString(section, key, value string) error {
	return d.SetSectionOnlyKey(section, key, TOMLQuote(value))
}

// SetInt 确保整数键等于 value（upsert）。
//
// 写成裸整数是本方法存在的全部理由：grokbuild 的 max_completion_tokens 若落成
// 字符串，整个 [model."<id>"] override 会被客户端静默丢弃。
func (d *TOMLDoc) SetInt(section, key string, value int64) error {
	return d.SetSectionOnlyKey(section, key, []byte(strconv.FormatInt(value, 10)))
}

// SetBool 确保布尔键等于 value（upsert）。
func (d *TOMLDoc) SetBool(section, key string, value bool) error {
	return d.SetSectionOnlyKey(section, key, []byte(strconv.FormatBool(value)))
}

// sectionPrefix 产出「在新段之前需要补的换行」，与文件现状一致：
// 已有段之间用空行分隔就补空行，否则只补一个换行。
func (d *TOMLDoc) sectionPrefix(eol string) string {
	if len(d.src) == 0 {
		return ""
	}
	blank := d.usesBlankLineBetweenSections()
	trailing := trailingLineEndings(d.src, eol)
	switch {
	case trailing == 0:
		if blank {
			return eol + eol
		}
		return eol
	case trailing == 1:
		if blank {
			return eol
		}
		return ""
	default:
		return ""
	}
}

// usesBlankLineBetweenSections 报告文件是否用空行分隔段（决定新建段的排版）。
func (d *TOMLDoc) usesBlankLineBetweenSections() bool {
	eol := detectLineEnding(d.src)
	for i, s := range d.sects {
		if i == 0 && s.HeaderStart == 0 {
			continue
		}
		start := lineStartAt(d.src, s.HeaderStart)
		if trailingLineEndings(d.src[:start], eol) >= 2 {
			return true
		}
	}
	if len(d.sects) == 0 {
		// 无段可参照：文件里出现空行就沿用空行风格。
		return bytes.Contains(d.src, []byte(eol+eol))
	}
	return false
}

// ── 渲染 ────────────────────────────────────────────────────────────────────

// Render 应用全部编辑，返回结果字节；未被编辑的字节逐字节保留。
func (d *TOMLDoc) Render() []byte {
	out := make([]byte, 0, len(d.src)+128)
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

// ── 键路径 ──────────────────────────────────────────────────────────────────

var tomlBareKeyRE = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)

// SectionSegment 把一个段名片段转成规范写法：裸标识符原样，否则加双引号。
func SectionSegment(name string) string {
	if tomlBareKeyRE.MatchString(name) {
		return name
	}
	return string(TOMLQuote(name))
}

// SectionPath 把若干段名片段拼成规范段名。
//
// 存在的理由是模型 id 里可能带点号（[model."grok-4.1-fast"]）：直接写
// "model.grok-4.1-fast" 会被解析成三段，匹配不到任何段。调用方应当用
// SectionPath("model", id) 构造段名。
func SectionPath(segs ...string) string {
	out := make([]string, len(segs))
	for i, s := range segs {
		out[i] = SectionSegment(s)
	}
	return strings.Join(out, ".")
}

// TOMLQuote 产出 TOML 基本字符串字面量（正确的转义形态）。
func TOMLQuote(s string) []byte {
	var b bytes.Buffer
	b.WriteByte('"')
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch c {
		case '"':
			b.WriteString(`\"`)
		case '\\':
			b.WriteString(`\\`)
		case '\n':
			b.WriteString(`\n`)
		case '\r':
			b.WriteString(`\r`)
		case '\t':
			b.WriteString(`\t`)
		case '\b':
			b.WriteString(`\b`)
		case '\f':
			b.WriteString(`\f`)
		default:
			if c < 0x20 || c == 0x7f {
				fmt.Fprintf(&b, `\u%04X`, c)
				continue
			}
			b.WriteByte(c)
		}
	}
	b.WriteByte('"')
	return b.Bytes()
}

// canonTOMLPath 把已解析的段/键片段拼成规范化名字。
func canonTOMLPath(segs []string) string {
	out := make([]string, len(segs))
	for i, s := range segs {
		out[i] = SectionSegment(s)
	}
	return strings.Join(out, ".")
}

// canonTOMLPathText 把一段 TOML 键文本（如 `model_providers.custom` 或
// `model."grok 4"`）规范化；空串表示根段。
func canonTOMLPathText(text string) (string, error) {
	if strings.TrimSpace(text) == "" {
		return "", nil
	}
	segs, end, err := parseTOMLPath([]byte(text), 0)
	if err != nil {
		return "", err
	}
	if rest := skipSpaces([]byte(text), end, len(text)); rest != len(text) {
		return "", fmt.Errorf("%w: %q 偏移 %d 处有多余内容", ErrTOMLParse, text, rest)
	}
	return canonTOMLPath(segs), nil
}

// parseTOMLPath 从 src[pos] 解析点分键路径（段可以是裸键或引号键）。
func parseTOMLPath(src []byte, pos int) ([]string, int, error) {
	var segs []string
	for {
		pos = skipSpaces(src, pos, len(src))
		if pos >= len(src) {
			return nil, pos, fmt.Errorf("%w: 偏移 %d 处键路径意外结束", ErrTOMLParse, pos)
		}
		var seg string
		switch src[pos] {
		case '"':
			s, n, err := scanTOMLBasicString(src, pos)
			if err != nil {
				return nil, pos, err
			}
			seg, pos = s, n
		case '\'':
			s, n, err := scanTOMLLiteralString(src, pos)
			if err != nil {
				return nil, pos, err
			}
			seg, pos = s, n
		default:
			j := pos
			for j < len(src) && isTOMLBareKeyByte(src[j]) {
				j++
			}
			if j == pos {
				return nil, pos, fmt.Errorf("%w: 偏移 %d 处 %q 不是合法键", ErrTOMLParse, pos, src[pos])
			}
			seg, pos = string(src[pos:j]), j
		}
		segs = append(segs, seg)
		pos = skipSpaces(src, pos, len(src))
		if pos < len(src) && src[pos] == '.' {
			pos++
			continue
		}
		return segs, pos, nil
	}
}

func isTOMLBareKeyByte(c byte) bool {
	return c == '-' || c == '_' || (c >= '0' && c <= '9') || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z')
}

// ── 值扫描 ──────────────────────────────────────────────────────────────────

var (
	tomlIntegerRE = regexp.MustCompile(`^[+-]?(0x[0-9A-Fa-f](_?[0-9A-Fa-f])*|0o[0-7](_?[0-7])*|0b[01](_?[01])*|(0|[1-9](_?[0-9])*))$`)
	tomlFloatRE   = regexp.MustCompile(`^[+-]?(inf|nan)$|^[+-]?(0|[1-9](_?[0-9])*)(\.[0-9](_?[0-9])*)?([eE][+-]?[0-9](_?[0-9])*)?$`)
	tomlDateOnly  = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}$`)
	tomlDateRE    = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}([Tt ]\d{2}:\d{2}:\d{2}(\.\d+)?([Zz]|[+-]\d{2}:\d{2})?)?$`)
	tomlTimeRE    = regexp.MustCompile(`^\d{2}:\d{2}:\d{2}(\.\d+)?$`)
)

// scanTOMLValue 扫描一个 TOML 值，返回其结束偏移（不含尾随空白与注释）与形态。
func scanTOMLValue(src []byte, pos int) (int, TOMLKind, error) {
	if pos >= len(src) {
		return 0, 0, fmt.Errorf("%w: 偏移 %d 处缺少值", ErrTOMLParse, pos)
	}
	switch src[pos] {
	case '"', '\'':
		end, err := skipTOMLString(src, pos)
		return end, TOMLString, err
	case '[', '{':
		end, err := scanTOMLBalanced(src, pos)
		kind := TOMLArray
		if src[pos] == '{' {
			kind = TOMLInlineTable
		}
		return end, kind, err
	default:
		return scanTOMLAtom(src, pos)
	}
}

// scanTOMLAtom 扫描数字/日期/时间/布尔这类原子记号。
func scanTOMLAtom(src []byte, pos int) (int, TOMLKind, error) {
	i := pos
	for i < len(src) && !isTOMLAtomDelim(src[i]) {
		i++
	}
	tok := string(src[pos:i])
	switch tok {
	case "true", "false":
		return i, TOMLBool, nil
	}
	if tomlDateOnly.MatchString(tok) && i < len(src) && src[i] == ' ' {
		// TOML 允许 offset date-time 用空格分隔日期与时间。
		j := i + 1
		for j < len(src) && !isTOMLAtomDelim(src[j]) {
			j++
		}
		if cand := tok + " " + string(src[i+1:j]); tomlDateRE.MatchString(cand) {
			return j, TOMLDateTime, nil
		}
	}
	switch {
	case tomlIntegerRE.MatchString(tok):
		return i, TOMLInteger, nil
	case tomlFloatRE.MatchString(tok):
		return i, TOMLFloat, nil
	case tomlDateRE.MatchString(tok) || tomlTimeRE.MatchString(tok):
		return i, TOMLDateTime, nil
	}
	return pos, 0, fmt.Errorf("%w: 偏移 %d 处的记号 %q 不是合法 TOML 值", ErrTOMLValueInvalid, pos, tok)
}

func isTOMLAtomDelim(c byte) bool {
	switch c {
	case ' ', '\t', '\r', '\n', ',', ']', '}', '#':
		return true
	}
	return false
}

// scanTOMLBalanced 扫描一个平衡的数组或内联表。
func scanTOMLBalanced(src []byte, pos int) (int, error) {
	var stack []byte
	for i := pos; i < len(src); {
		switch c := src[i]; {
		case c == '"' || c == '\'':
			n, err := skipTOMLString(src, i)
			if err != nil {
				return 0, err
			}
			i = n
		case c == '#':
			for i < len(src) && src[i] != '\n' {
				i++
			}
		case c == '[':
			stack = append(stack, ']')
			i++
		case c == '{':
			stack = append(stack, '}')
			i++
		case c == ']' || c == '}':
			if len(stack) == 0 || stack[len(stack)-1] != c {
				return 0, fmt.Errorf("%w: 偏移 %d 处括号不匹配", ErrTOMLParse, i)
			}
			stack = stack[:len(stack)-1]
			i++
			if len(stack) == 0 {
				return i, nil
			}
		default:
			i++
		}
	}
	return 0, fmt.Errorf("%w: 偏移 %d 处的集合未闭合", ErrTOMLParse, pos)
}

// skipTOMLString 跳过一个字符串记号（含多行形态），返回其后的偏移。
func skipTOMLString(src []byte, pos int) (int, error) {
	if pos >= len(src) {
		return 0, fmt.Errorf("%w: 偏移 %d 处缺少字符串", ErrTOMLParse, pos)
	}
	long := pos+2 < len(src) && src[pos+1] == src[pos] && src[pos+2] == src[pos]
	if long {
		return skipTOMLLongString(src, pos)
	}
	q := src[pos]
	i := pos + 1
	for i < len(src) {
		switch c := src[i]; c {
		case '\\':
			if q != '"' {
				i++
				continue
			}
			i += 2
		case q:
			return i + 1, nil
		case '\n':
			return 0, fmt.Errorf("%w: 偏移 %d 处字符串跨行", ErrTOMLParse, pos)
		default:
			i++
		}
	}
	return 0, fmt.Errorf("%w: 偏移 %d 处字符串未闭合", ErrTOMLParse, pos)
}

// skipTOMLLongString 跳过 """...""" 或 ”'...”'。
func skipTOMLLongString(src []byte, pos int) (int, error) {
	q := src[pos]
	i := pos + 3
	for i < len(src) {
		if q == '"' && src[i] == '\\' {
			i += 2
			continue
		}
		if src[i] == q {
			run := 0
			for i+run < len(src) && src[i+run] == q {
				run++
			}
			if run >= 3 {
				return i + run, nil
			}
			i += run
			continue
		}
		i++
	}
	return 0, fmt.Errorf("%w: 偏移 %d 处多行字符串未闭合", ErrTOMLParse, pos)
}

// scanTOMLBasicString 扫描一个基本字符串并返回解码后的文本。
func scanTOMLBasicString(src []byte, pos int) (string, int, error) {
	end, err := skipTOMLString(src, pos)
	if err != nil {
		return "", pos, err
	}
	s, err := decodeTOMLString(src[pos:end])
	if err != nil {
		return "", pos, err
	}
	return s, end, nil
}

// scanTOMLLiteralString 扫描一个字面字符串并返回其文本（无反斜杠转义）。
func scanTOMLLiteralString(src []byte, pos int) (string, int, error) {
	end, err := skipTOMLString(src, pos)
	if err != nil {
		return "", pos, err
	}
	raw := src[pos:end]
	switch {
	case bytes.HasPrefix(raw, []byte("'''")):
		return string(raw[3 : len(raw)-3]), end, nil
	default:
		return string(raw[1 : len(raw)-1]), end, nil
	}
}

// decodeTOMLString 解码四种 TOML 字符串形态。
func decodeTOMLString(raw []byte) (string, error) {
	if len(raw) == 0 {
		return "", fmt.Errorf("%w: 空字符串记号", ErrTOMLParse)
	}
	q := raw[0]
	multiline := len(raw) >= 6 && raw[1] == q && raw[2] == q
	body := raw
	switch {
	case multiline:
		body = raw[3 : len(raw)-3]
	case len(raw) >= 2:
		body = raw[1 : len(raw)-1]
	default:
		return "", fmt.Errorf("%w: 字符串记号不完整", ErrTOMLParse)
	}
	if q == '\'' {
		return string(body), nil
	}
	var sb strings.Builder
	for i := 0; i < len(body); {
		if body[i] != '\\' {
			sb.WriteByte(body[i])
			i++
			continue
		}
		if multiline && i+1 < len(body) && (body[i+1] == '\n' || body[i+1] == '\r') {
			// 行尾反斜杠：吃掉换行与其后的空白。
			i++
			for i < len(body) && (body[i] == '\n' || body[i] == '\r' || body[i] == ' ' || body[i] == '\t') {
				i++
			}
			continue
		}
		r, n, err := decodeTOMLEscape(body, i)
		if err != nil {
			return "", err
		}
		sb.WriteRune(r)
		i = n
	}
	return sb.String(), nil
}

// decodeTOMLEscape 解码 body[i]（'\\'）处的一个转义。
func decodeTOMLEscape(body []byte, i int) (rune, int, error) {
	if i+1 >= len(body) {
		return 0, i, fmt.Errorf("%w: 转义未结束", ErrTOMLParse)
	}
	switch e := body[i+1]; e {
	case 'b':
		return '\b', i + 2, nil
	case 't':
		return '\t', i + 2, nil
	case 'n':
		return '\n', i + 2, nil
	case 'f':
		return '\f', i + 2, nil
	case 'r':
		return '\r', i + 2, nil
	case '"':
		return '"', i + 2, nil
	case '\\':
		return '\\', i + 2, nil
	case 'u', 'U':
		width := 4
		if e == 'U' {
			width = 8
		}
		if i+2+width > len(body) {
			return 0, i, fmt.Errorf("%w: \\%c 转义不完整", ErrTOMLParse, e)
		}
		v, err := strconv.ParseUint(string(body[i+2:i+2+width]), 16, 32)
		if err != nil {
			return 0, i, fmt.Errorf("%w: \\%c 转义非法", ErrTOMLParse, e)
		}
		return rune(v), i + 2 + width, nil
	}
	return 0, i, fmt.Errorf("%w: 未知转义 \\%c", ErrTOMLParse, body[i+1])
}

// validateTOMLValue 校验一段原始字节是合法 TOML 值（调用方自行决定形态，
// 但形态必须是 TOML 认得的，否则写下去的文件会解析不了）。
func validateTOMLValue(raw []byte) error {
	if len(raw) == 0 {
		return fmt.Errorf("%w: 空值", ErrTOMLValueInvalid)
	}
	end, _, err := scanTOMLValue(raw, 0)
	if err != nil {
		return err
	}
	if rest := skipSpaces(raw, end, len(raw)); rest != len(raw) {
		return fmt.Errorf("%w: 值后有额外字节 %q", ErrTOMLValueInvalid, raw[rest:])
	}
	return nil
}

// ── 行与排版 ────────────────────────────────────────────────────────────────

// lineContentEnd 返回 pos 所在行的内容末（不含 '\r'/'\n'）；无换行则到文件末。
func lineContentEnd(src []byte, pos int) int {
	i := pos
	for i < len(src) && src[i] != '\n' && src[i] != '\r' {
		i++
	}
	return i
}

// lineNext 返回 pos（行内容末）之后的下一行行首。
func lineNext(src []byte, contentEnd int) int {
	if contentEnd >= len(src) {
		return len(src)
	}
	if src[contentEnd] == '\n' {
		return contentEnd + 1
	}
	if contentEnd+1 < len(src) && src[contentEnd+1] == '\n' {
		return contentEnd + 2
	}
	return contentEnd + 1
}

// skipSpaces 跳过空格与制表符。
func skipSpaces(src []byte, i, end int) int {
	for i < end && (src[i] == ' ' || src[i] == '\t') {
		i++
	}
	return i
}

// leadingIndentBytes 返回 pos 所在行的行首空白（用于让插入的行跟随该行缩进）。
func leadingIndentBytes(src []byte, pos int) []byte {
	start := lineStartAt(src, pos)
	j := start
	for j < len(src) && (src[j] == ' ' || src[j] == '\t') {
		j++
	}
	if j >= pos {
		return nil
	}
	return src[start:j]
}

// detectLineEnding 沿用源文件的换行风格。
func detectLineEnding(src []byte) string {
	if bytes.Contains(src, []byte("\r\n")) {
		return "\r\n"
	}
	return "\n"
}

// trailingLineEndings 数出 src 末尾连续的换行个数（按给定风格）。
func trailingLineEndings(src []byte, eol string) int {
	n := 0
	for bytes.HasSuffix(src, []byte(eol)) {
		src = src[:len(src)-len(eol)]
		n++
	}
	return n
}
