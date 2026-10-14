// Package docmodel 提供「字节保真」的结构化文档模型。
//
// 为什么不能用 encoding/json + map[string]any：那会丢掉键序与原始空白，
// 而本产品的验收以 sha256 逐字节断言（acceptance-recipe §C/§E），
// 任何整文件重序列化都会破坏「用户没碰过的字节保持原样」这条硬约束。
//
// 本包的做法：解析时记录每个节点的**字节区间**，改动以「区间 → 新字节」的
// 编辑列表表达，渲染时从后往前拼接。未被编辑的字节原样通过。
package docmodel

import (
	"bytes"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"
)

// Kind 是 JSON 值种类。
type Kind uint8

const (
	KObject Kind = iota
	KArray
	KString
	KNumber
	KBool
	KNull
)

func (k Kind) String() string {
	switch k {
	case KObject:
		return "object"
	case KArray:
		return "array"
	case KString:
		return "string"
	case KNumber:
		return "number"
	case KBool:
		return "bool"
	case KNull:
		return "null"
	}
	return "unknown"
}

// Member 是对象的一个成员，携带键与值在源文本中的区间。
type Member struct {
	Key    string
	KeyRaw []byte // 含引号的原始键记号
	Val    *Node
	Start  int // 成员整体（键起）在源中的起点
	End    int // 成员整体在源中的终点（不含尾随逗号）
}

// Node 是解析后的值，携带它在源文本中的字节区间。
type Node struct {
	Kind    Kind
	Start   int
	End     int
	Members []Member // KObject；保持源顺序
	Elems   []*Node  // KArray
	Str     string   // KString
	Num     string   // KNumber 的原始字面量
	Bool    bool     // KBool
	src     []byte
}

// Doc 是一份已解析的文档：源字节 + 根节点 + 待应用编辑。
type Doc struct {
	src   []byte
	root  *Node
	edits []edit
}

type edit struct {
	start, end int
	repl       []byte
}

// ── 解析 ────────────────────────────────────────────────────────────────────

type parser struct {
	src []byte
	pos int
}

// ParseJSON 解析一份严格 JSON，记录全部字节区间。
func ParseJSON(src []byte) (*Doc, error) {
	p := &parser{src: src}
	p.ws()
	if p.pos >= len(p.src) {
		return nil, fmt.Errorf("空文档不是合法 JSON")
	}
	root, err := p.value()
	if err != nil {
		return nil, err
	}
	p.ws()
	if p.pos != len(p.src) {
		return nil, fmt.Errorf("JSON 尾部有未解析内容（偏移 %d）", p.pos)
	}
	if root.Kind != KObject && root.Kind != KArray {
		return nil, fmt.Errorf("顶层必须是对象或数组，实际是 %s", root.Kind)
	}
	return &Doc{src: src, root: root}, nil
}

// ParseJSONValue 解析顶层可以是任意 JSON 值的文档（用于单个值的处理）。
func ParseJSONValue(src []byte) (*Doc, error) {
	p := &parser{src: src}
	p.ws()
	v, err := p.value()
	if err != nil {
		return nil, err
	}
	p.ws()
	if p.pos != len(p.src) {
		return nil, fmt.Errorf("JSON 尾部有未解析内容（偏移 %d）", p.pos)
	}
	return &Doc{src: src, root: v}, nil
}

func (p *parser) ws() {
	for p.pos < len(p.src) {
		switch p.src[p.pos] {
		case ' ', '\t', '\n', '\r':
			p.pos++
		default:
			return
		}
	}
}

func (p *parser) value() (*Node, error) {
	p.ws()
	if p.pos >= len(p.src) {
		return nil, fmt.Errorf("期望一个值，但已到文件末尾")
	}
	switch c := p.src[p.pos]; {
	case c == '{':
		return p.object()
	case c == '[':
		return p.array()
	case c == '"':
		return p.str()
	case c == 't' || c == 'f':
		return p.boolean()
	case c == 'n':
		return p.null()
	case c == '-' || (c >= '0' && c <= '9'):
		return p.number()
	default:
		return nil, fmt.Errorf("偏移 %d 处出现非预期字符 %q", p.pos, c)
	}
}

func (p *parser) object() (*Node, error) {
	start := p.pos
	p.pos++ // '{'
	n := &Node{Kind: KObject, Start: start, src: p.src}
	p.ws()
	if p.pos < len(p.src) && p.src[p.pos] == '}' {
		p.pos++
		n.End = p.pos
		return n, nil
	}
	for {
		p.ws()
		if p.pos >= len(p.src) || p.src[p.pos] != '"' {
			return nil, fmt.Errorf("偏移 %d 处期望对象的键（带引号字符串）", p.pos)
		}
		keyStart := p.pos
		keyNode, err := p.str()
		if err != nil {
			return nil, err
		}
		keyRaw := p.src[keyStart:p.pos]
		p.ws()
		if p.pos >= len(p.src) || p.src[p.pos] != ':' {
			return nil, fmt.Errorf("偏移 %d 处期望 ':'", p.pos)
		}
		p.pos++
		val, err := p.value()
		if err != nil {
			return nil, err
		}
		n.Members = append(n.Members, Member{
			Key:    keyNode.Str,
			KeyRaw: keyRaw,
			Val:    val,
			Start:  keyStart,
			End:    val.End,
		})
		p.ws()
		if p.pos >= len(p.src) {
			return nil, fmt.Errorf("对象未闭合")
		}
		if p.src[p.pos] == ',' {
			p.pos++
			continue
		}
		if p.src[p.pos] == '}' {
			p.pos++
			n.End = p.pos
			return n, nil
		}
		return nil, fmt.Errorf("偏移 %d 处期望 ',' 或 '}'", p.pos)
	}
}

func (p *parser) array() (*Node, error) {
	start := p.pos
	p.pos++ // '['
	n := &Node{Kind: KArray, Start: start, src: p.src}
	p.ws()
	if p.pos < len(p.src) && p.src[p.pos] == ']' {
		p.pos++
		n.End = p.pos
		return n, nil
	}
	for {
		v, err := p.value()
		if err != nil {
			return nil, err
		}
		n.Elems = append(n.Elems, v)
		p.ws()
		if p.pos >= len(p.src) {
			return nil, fmt.Errorf("数组未闭合")
		}
		if p.src[p.pos] == ',' {
			p.pos++
			continue
		}
		if p.src[p.pos] == ']' {
			p.pos++
			n.End = p.pos
			return n, nil
		}
		return nil, fmt.Errorf("偏移 %d 处期望 ',' 或 ']'", p.pos)
	}
}

func (p *parser) str() (*Node, error) {
	start := p.pos
	p.pos++ // '"'
	var sb strings.Builder
	for p.pos < len(p.src) {
		c := p.src[p.pos]
		switch c {
		case '"':
			p.pos++
			return &Node{Kind: KString, Start: start, End: p.pos, Str: sb.String(), src: p.src}, nil
		case '\\':
			p.pos++
			if p.pos >= len(p.src) {
				return nil, fmt.Errorf("字符串转义未结束")
			}
			switch e := p.src[p.pos]; e {
			case '"', '\\', '/':
				sb.WriteByte(e)
			case 'b':
				sb.WriteByte('\b')
			case 'f':
				sb.WriteByte('\f')
			case 'n':
				sb.WriteByte('\n')
			case 'r':
				sb.WriteByte('\r')
			case 't':
				sb.WriteByte('\t')
			case 'u':
				if p.pos+4 >= len(p.src) {
					return nil, fmt.Errorf("\\u 转义不完整")
				}
				hexs := string(p.src[p.pos+1 : p.pos+5])
				v, err := strconv.ParseUint(hexs, 16, 32)
				if err != nil {
					return nil, fmt.Errorf("\\u 转义非法: %q", hexs)
				}
				sb.WriteRune(rune(v))
				p.pos += 4
			default:
				return nil, fmt.Errorf("未知转义 \\%c", e)
			}
			p.pos++
		default:
			sb.WriteByte(c)
			p.pos++
		}
	}
	return nil, fmt.Errorf("字符串未闭合")
}

func (p *parser) number() (*Node, error) {
	start := p.pos
	for p.pos < len(p.src) {
		c := p.src[p.pos]
		if (c >= '0' && c <= '9') || c == '-' || c == '+' || c == '.' || c == 'e' || c == 'E' {
			p.pos++
			continue
		}
		break
	}
	lit := string(p.src[start:p.pos])
	if _, err := strconv.ParseFloat(lit, 64); err != nil {
		return nil, fmt.Errorf("非法数字字面量 %q", lit)
	}
	return &Node{Kind: KNumber, Start: start, End: p.pos, Num: lit, src: p.src}, nil
}

func (p *parser) boolean() (*Node, error) {
	start := p.pos
	if strings.HasPrefix(string(p.src[p.pos:]), "true") {
		p.pos += 4
		return &Node{Kind: KBool, Start: start, End: p.pos, Bool: true, src: p.src}, nil
	}
	if strings.HasPrefix(string(p.src[p.pos:]), "false") {
		p.pos += 5
		return &Node{Kind: KBool, Start: start, End: p.pos, Bool: false, src: p.src}, nil
	}
	return nil, fmt.Errorf("偏移 %d 处非法字面量", p.pos)
}

func (p *parser) null() (*Node, error) {
	start := p.pos
	if !strings.HasPrefix(string(p.src[p.pos:]), "null") {
		return nil, fmt.Errorf("偏移 %d 处非法字面量", p.pos)
	}
	p.pos += 4
	return &Node{Kind: KNull, Start: start, End: p.pos, src: p.src}, nil
}

// ── 查询 ────────────────────────────────────────────────────────────────────

// Root 返回根节点。
func (d *Doc) Root() *Node { return d.root }

// Src 返回原始源字节。
func (d *Doc) Src() []byte { return d.src }

// Get 按键路径取值；不存在返回 nil。
func (n *Node) Get(path ...string) *Node {
	cur := n
	for _, key := range path {
		if cur == nil || cur.Kind != KObject {
			return nil
		}
		var next *Node
		for i := range cur.Members {
			if cur.Members[i].Key == key {
				next = cur.Members[i].Val
				break
			}
		}
		if next == nil {
			return nil
		}
		cur = next
	}
	return cur
}

// MemberOf 返回指定键的成员（含区间）。
func (n *Node) MemberOf(key string) *Member {
	if n == nil || n.Kind != KObject {
		return nil
	}
	for i := range n.Members {
		if n.Members[i].Key == key {
			return &n.Members[i]
		}
	}
	return nil
}

// At 按索引取数组元素。
func (n *Node) At(i int) *Node {
	if n == nil || n.Kind != KArray || i < 0 || i >= len(n.Elems) {
		return nil
	}
	return n.Elems[i]
}

// Len 返回数组长度或对象成员数。
func (n *Node) Len() int {
	if n == nil {
		return 0
	}
	switch n.Kind {
	case KArray:
		return len(n.Elems)
	case KObject:
		return len(n.Members)
	}
	return 0
}

// AsString 返回字符串值；非字符串返回 ok=false。
func (n *Node) AsString() (string, bool) {
	if n == nil || n.Kind != KString {
		return "", false
	}
	return n.Str, true
}

// AsBool 返回布尔值。
func (n *Node) AsBool() (bool, bool) {
	if n == nil || n.Kind != KBool {
		return false, false
	}
	return n.Bool, true
}

// AsRaw 返回该节点的原始字节（未经修改时即源文本片段）。
func (n *Node) AsRaw() []byte {
	if n == nil {
		return nil
	}
	return n.src[n.Start:n.End]
}

// ── 变更 ────────────────────────────────────────────────────────────────────

// add 记录一次编辑，并拒绝与既有编辑区间重叠的改动（重叠必然产生歧义）。
func (d *Doc) add(start, end int, repl []byte) error {
	// 区间守卫：start > end 会让 Render 把同一段源字节拼接两次（实测会产出
	// 内容重复的坏文件），因此这里必须响亮失败而不是生成可疑输出。
	if start < 0 || end > len(d.src) || start > end {
		return fmt.Errorf("非法编辑区间 [%d,%d)（源长度 %d）", start, end, len(d.src))
	}
	for _, e := range d.edits {
		if start < e.end && e.start < end {
			return fmt.Errorf("编辑区间冲突: [%d,%d) 与 [%d,%d) 重叠", start, end, e.start, e.end)
		}
	}
	d.edits = append(d.edits, edit{start: start, end: end, repl: repl})
	return nil
}

// SetJSON 把 path 处的值替换为 value（以 JSON 编码）。
// 路径不存在时插入，插入格式对齐该对象的既有排版风格。
func (d *Doc) SetJSON(value any, path ...string) error {
	enc, err := encodeCanonical(value)
	if err != nil {
		return err
	}
	return d.setRaw(enc, path...)
}

func (d *Doc) setRaw(enc []byte, path ...string) error {
	if len(path) == 0 {
		return fmt.Errorf("空路径")
	}
	parent := d.root.Get(path[:len(path)-1]...)
	if parent == nil {
		return fmt.Errorf("路径父级不存在: %v", path[:len(path)-1])
	}
	if parent.Kind != KObject {
		return fmt.Errorf("路径父级不是对象: %v", path[:len(path)-1])
	}
	key := path[len(path)-1]
	if m := parent.MemberOf(key); m != nil {
		return d.add(m.Val.Start, m.Val.End, enc)
	}
	return d.insertMember(parent, key, enc)
}

// insertMember 在对象末尾插入一个新成员，排版跟随对象现状。
func (d *Doc) insertMember(obj *Node, key string, enc []byte) error {
	keyBytes := encodeString(key)
	style := detectStyle(obj)
	var ins []byte
	if len(obj.Members) == 0 {
		// 空对象：在 { 与 } 之间插入。
		ins = append([]byte(nil), keyBytes...)
		ins = append(ins, ':', ' ')
		ins = append(ins, enc...)
		// 空对象的两个花括号可能相邻，也可能有空白；直接用整段替换最稳。
		body := ins
		if style.multiline {
			body = append(append([]byte{'\n'}, style.indent...), body...)
			body = append(body, '\n')
		}
		return d.add(obj.Start, obj.End, append(append([]byte{'{'}, body...), '}'))
	}
	last := obj.Members[len(obj.Members)-1]
	ins = append(ins, ',', '\n')
	ins = append(ins, style.indent...)
	ins = append(ins, keyBytes...)
	ins = append(ins, ':', ' ')
	ins = append(ins, enc...)
	return d.add(last.End, last.End, ins)
}

// Delete 删除 path 处的成员（含其分隔逗号），其余字节不动。
func (d *Doc) Delete(path ...string) error {
	if len(path) == 0 {
		return fmt.Errorf("空路径")
	}
	parent := d.root.Get(path[:len(path)-1]...)
	if parent == nil || parent.Kind != KObject {
		return fmt.Errorf("路径父级不存在或不是对象: %v", path[:len(path)-1])
	}
	key := path[len(path)-1]
	idx := -1
	for i := range parent.Members {
		if parent.Members[i].Key == key {
			idx = i
			break
		}
	}
	if idx < 0 {
		return nil // 不存在即无操作，幂等
	}
	m := parent.Members[idx]
	// 唯一成员：连容器内的空白一起清掉，结果就是空容器（不留空行、不留残壳）。
	if len(parent.Members) == 1 {
		return d.add(parent.Start+1, parent.End-1, nil)
	}
	if idx+1 < len(parent.Members) {
		next := parent.Members[idx+1]
		// 排版判定看「本成员结束到下一成员开始之间有没有换行」，而不是文件全局样式：
		// 单行容器里退到行首会把整份内容一起吃掉（实测 {"a":1,"b":2} 删 a 得空串）。
		if bytes.IndexByte(d.src[m.End:next.Start], '\n') >= 0 {
			// 多行：删掉整行（含行首缩进与该行换行），后一行不受影响。
			return d.add(lineStartAt(d.src, m.Start), lineEndAfter(d.src, m.End), nil)
		}
		// 单行：只删本成员到下一成员之前（含中间那个逗号与空白）。
		return d.add(m.Start, next.Start, nil)
	}
	// 末位成员：删掉它前面的逗号到闭合括号之前（多行保留括号前的换行以维持缩进）。
	start := commaBefore(d.src, m.Start, parent.Start)
	if start < 0 {
		start = m.Start
	}
	return d.add(start, trimBeforeClose(d.src, m.End, parent.End), nil)
}

// AppendToArray 在数组末尾追加一个 JSON 值。
func (d *Doc) AppendToArray(value any, path ...string) error {
	enc, err := encodeCanonical(value)
	if err != nil {
		return err
	}
	arr := d.root.Get(path...)
	if arr == nil {
		return fmt.Errorf("数组不存在: %v", path)
	}
	if arr.Kind != KArray {
		return fmt.Errorf("目标不是数组: %v", path)
	}
	style := detectStyle(arr)
	if len(arr.Elems) == 0 {
		body := enc
		if style.multiline {
			body = append(append([]byte{'\n'}, style.indent...), body...)
			body = append(body, '\n')
		}
		return d.add(arr.Start, arr.End, append(append([]byte{'['}, body...), ']'))
	}
	last := arr.Elems[len(arr.Elems)-1]
	ins := append([]byte{','}, '\n')
	ins = append(ins, style.indent...)
	ins = append(ins, enc...)
	return d.add(last.End, last.End, ins)
}

// ReplaceRaw 用给定字节替换某路径处的值（用于写入精确的目标字节，如占位符）。
func (d *Doc) ReplaceRaw(repl []byte, path ...string) error {
	n := d.root.Get(path...)
	if n == nil {
		return fmt.Errorf("路径不存在: %v", path)
	}
	return d.add(n.Start, n.End, repl)
}

// ReplaceByteRange 直接替换一个字节区间（供数组元素级外科手术使用）。
func (d *Doc) ReplaceByteRange(start, end int, repl []byte) error {
	bounds := []int{start, end, 0, len(d.src)}
	sort.Ints(bounds)
	if start < 0 || end > len(d.src) || start > end {
		return fmt.Errorf("区间 [%d,%d) 越界", start, end)
	}
	return d.add(start, end, repl)
}

// ── 渲染 ────────────────────────────────────────────────────────────────────

// Render 应用全部编辑，返回结果字节。未被编辑的字节逐字节保留。
func (d *Doc) Render() []byte {
	out := make([]byte, 0, len(d.src)+64)
	prev := 0
	es := append([]edit(nil), d.edits...)
	sort.Slice(es, func(i, j int) bool { return es[i].start < es[j].start })
	for _, e := range es {
		out = append(out, d.src[prev:e.start]...)
		out = append(out, e.repl...)
		prev = e.end
	}
	out = append(out, d.src[prev:]...)
	return out
}

// Dirty 报告是否有改动。
func (d *Doc) Dirty() bool { return len(d.edits) > 0 }

// ── 排版探测 ────────────────────────────────────────────────────────────────

type style struct {
	multiline bool
	indent    []byte
}

// detectStyle 推断一个容器对象的排版：是否多行、成员缩进多少。
func detectStyle(n *Node) style {
	if n == nil || len(n.src) == 0 {
		return style{}
	}
	switch n.Kind {
	case KObject:
		if len(n.Members) >= 1 {
			inner := n.src[n.Start:n.Members[0].Start]
			if bytes.IndexByte(inner, '\n') >= 0 {
				return style{multiline: true, indent: leadingWSBefore(n.src, n.Members[len(n.Members)-1].Start, n.Start)}
			}
		}
		if bytes.IndexByte(n.src[n.Start:n.End], '\n') >= 0 {
			return style{multiline: true, indent: []byte("  ")}
		}
	case KArray:
		if len(n.Elems) >= 1 {
			inner := n.src[n.Start:n.Elems[0].Start]
			if bytes.IndexByte(inner, '\n') >= 0 {
				return style{multiline: true, indent: leadingWSBefore(n.src, n.Elems[len(n.Elems)-1].Start, n.Start)}
			}
		}
		if bytes.IndexByte(n.src[n.Start:n.End], '\n') >= 0 {
			return style{multiline: true, indent: []byte("  ")}
		}
	}
	return style{}
}

// leadingWSBefore 返回 pos 所在行的行首空白。
func leadingWSBefore(src []byte, pos, floor int) []byte {
	i := pos
	for i > floor && src[i-1] != '\n' {
		i--
	}
	j := i
	for j < len(src) && (src[j] == ' ' || src[j] == '\t') {
		j++
	}
	if j == i {
		// 该行没有缩进，退到与嵌套深度一致的默认值
		return nil
	}
	return src[i:j]
}

// lineStartAt 返回 pos 所在行的行首（含行首缩进）。
func lineStartAt(src []byte, pos int) int {
	i := pos
	for i > 0 && src[i-1] != '\n' {
		i--
	}
	return i
}

// lineEndAfter 返回 pos 之后第一个换行的下一个位置；无换行则到文件末尾。
func lineEndAfter(src []byte, pos int) int {
	i := pos
	for i < len(src) && src[i] != '\n' {
		i++
	}
	if i < len(src) {
		i++ // 吃掉换行本身
	}
	return i
}

// commaBefore 从 pos 向前找逗号，遇到容器起点即放弃（返回 -1）。
func commaBefore(src []byte, pos, floor int) int {
	i := pos
	for i > floor {
		i--
		if src[i] == ',' {
			return i
		}
		if src[i] == '}' || src[i] == ']' || src[i] == '{' || src[i] == '[' {
			return -1
		}
	}
	return -1
}

// trimBeforeClose 返回「从 pos 起到闭合括号之前」应当删到哪里（配合末位成员删除）。
//
// 多行排版：停在闭合括号前那个换行处 —— 保留换行，闭合括号得以维持自己的行与缩进。
// 单行排版：停在闭合括号本身之前 —— **绝不能**退到括号所在行的行首，
// 那会把本行前面的内容一起吃掉（实测会产出内容重复的坏文件）。
func trimBeforeClose(src []byte, pos, floor int) int {
	brace := pos
	for brace < floor && src[brace] != '}' && src[brace] != ']' {
		if src[brace] != ' ' && src[brace] != '\t' && src[brace] != '\n' && src[brace] != '\r' {
			return pos // 后面还有非空白内容，保守不动
		}
		brace++
	}
	if brace >= floor {
		return pos
	}
	i := brace - 1
	for i >= pos && (src[i] == ' ' || src[i] == '\t' || src[i] == '\r') {
		i--
	}
	// 注意用 >=：pos 自身就可能正是那个换行（末位成员的值结束于换行处）。
	if i >= pos && src[i] == '\n' {
		return i
	}
	return brace
}

// DeleteElem 删除顶层数组的第 i 个元素，逗号与空白按排版处理，结果仍是合法 JSON。
// 供顶层为数组的受管文件（如 workbuddy 的 models.json）使用。
func (d *Doc) DeleteElem(i int) error {
	arr := d.root
	if arr == nil || arr.Kind != KArray {
		return fmt.Errorf("顶层不是数组，无法按序号删除")
	}
	if i < 0 || i >= len(arr.Elems) {
		return fmt.Errorf("元素序号 %d 越界（长度 %d）", i, len(arr.Elems))
	}
	el := arr.Elems[i]
	if len(arr.Elems) == 1 {
		return d.add(arr.Start+1, arr.End-1, nil)
	}
	if i+1 < len(arr.Elems) {
		next := arr.Elems[i+1]
		if bytes.IndexByte(d.src[el.End:next.Start], '\n') >= 0 {
			return d.add(lineStartAt(d.src, el.Start), lineEndAfter(d.src, el.End), nil)
		}
		return d.add(el.Start, next.Start, nil)
	}
	start := commaBefore(d.src, el.Start, arr.Start)
	if start < 0 {
		start = el.Start
	}
	return d.add(start, trimBeforeClose(d.src, el.End, arr.End), nil)
}

// ── 编码 ────────────────────────────────────────────────────────────────────

// encodeString 产出 JSON 字符串字面量（不转义 HTML，与 serde_json 行为一致）。
func encodeString(s string) []byte {
	var sb bytes.Buffer
	enc := json.NewEncoder(&sb)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(s); err != nil {
		return []byte(`""`)
	}
	return bytes.TrimRight(sb.Bytes(), "\n")
}

// encodeCanonical 把任意 Go 值编码为紧凑 JSON（键按字典序，保证确定性）。
func encodeCanonical(v any) ([]byte, error) {
	var sb bytes.Buffer
	enc := json.NewEncoder(&sb)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return bytes.TrimRight(sb.Bytes(), "\n"), nil
}

// ── 规范化整文件重写 ────────────────────────────────────────────────────────

// CanonicalJSON 把一个 Go 值序列化为「排序键 + 2 空格缩进 + 尾换行」的 JSON。
// 这是若干家族（claude / gemini / pi / opencode 写侧）的整文件规范形态。
func CanonicalJSON(v any) ([]byte, error) {
	var sb bytes.Buffer
	if err := writeCanonical(&sb, v, 0); err != nil {
		return nil, err
	}
	sb.WriteByte('\n')
	return sb.Bytes(), nil
}

func writeCanonical(sb *bytes.Buffer, v any, depth int) error {
	switch t := v.(type) {
	case nil:
		sb.WriteString("null")
	case bool:
		sb.WriteString(strconv.FormatBool(t))
	case string:
		sb.Write(encodeString(t))
	case int:
		sb.WriteString(strconv.Itoa(t))
	case int64:
		sb.WriteString(strconv.FormatInt(t, 10))
	case float64:
		sb.WriteString(strconv.FormatFloat(t, 'f', -1, 64))
	case json.Number:
		sb.WriteString(t.String())
	case []any:
		if len(t) == 0 {
			sb.WriteString("[]")
			return nil
		}
		sb.WriteString("[\n")
		for i, e := range t {
			sb.WriteString(strings.Repeat("  ", depth+1))
			if err := writeCanonical(sb, e, depth+1); err != nil {
				return err
			}
			if i < len(t)-1 {
				sb.WriteByte(',')
			}
			sb.WriteByte('\n')
		}
		sb.WriteString(strings.Repeat("  ", depth))
		sb.WriteByte(']')
	case map[string]any:
		keys := make([]string, 0, len(t))
		for k := range t {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		if len(keys) == 0 {
			sb.WriteString("{}")
			return nil
		}
		sb.WriteString("{\n")
		for i, k := range keys {
			sb.WriteString(strings.Repeat("  ", depth+1))
			sb.Write(encodeString(k))
			sb.WriteString(": ")
			if err := writeCanonical(sb, t[k], depth+1); err != nil {
				return err
			}
			if i < len(keys)-1 {
				sb.WriteByte(',')
			}
			sb.WriteByte('\n')
		}
		sb.WriteString(strings.Repeat("  ", depth))
		sb.WriteByte('}')
	default:
		// 兜底：先走一次标准编码再递归，保证未知类型不静默丢失
		b, err := json.Marshal(v)
		if err != nil {
			return fmt.Errorf("无法规范化类型 %T: %w", v, err)
		}
		var generic any
		dec := json.NewDecoder(bytes.NewReader(b))
		dec.UseNumber()
		if err := dec.Decode(&generic); err != nil {
			return err
		}
		return writeCanonical(sb, generic, depth)
	}
	return nil
}
