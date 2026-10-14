package router

import (
	"bytes"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"
)

// ── 字节保真的 JSON 外科手术（internal/docmodel 的最小移植子集）──────────────
//
// 为什么不用 encoding/json + map：那会丢掉键序与原始空白，把「客户端没碰过的字节」
// 一起重写。router 数据面的契约是「只改 model 与 output_config.effort 两处，其余
// 字节逐字节原样」（UC-028/029）；守护进程侧由 internal/docmodel 保证，SDK 侧由
// 本文件保证，两边必须给出**同一串出网字节**。
//
// UC-038 A8 要求 sdk/router 纯标准库（嵌入方的传递依赖 ≈ 标准库 + pgx），因此这里
// 不 import 同模块的 internal/docmodel，而是把 router 数据面用到的那一部分逐行移植：
// 解析接受度（含宽松数字字面量）、编辑区间守卫、插入的排版探测、渲染的逐字节保真
// 都以 internal/docmodel/json.go 为准。**改动本文件时必须同步核对那边**
// （json_test.go 用字节级用例钉住移植结果）。
//
// 唯一的刻意差异：解析带嵌套深度上限（maxJSONDepth）。SDK 的监听地址可能暴露给
// 容器网桥（UC-038 外部约束），超深文档直接判非法，而不是递归到栈上限。
const maxJSONDepth = 4096

// jsonKind 是 JSON 值种类（取值集合与 docmodel.Kind 同构）。
type jsonKind uint8

const (
	jObject jsonKind = iota
	jArray
	jString
	jNumber
	jBool
	jNull
)

// jsonMember 是对象的一个成员，携带键与值在源文本中的区间。
type jsonMember struct {
	key string
	val *jsonNode
	// start/end 是「成员整体」在源中的区间（键起，值止，不含尾随逗号）。
	start, end int
}

// jsonNode 是解析后的值，携带它在源文本中的字节区间。
type jsonNode struct {
	kind    jsonKind
	start   int
	end     int
	src     []byte
	members []jsonMember // jObject；保持源顺序
	str     string       // jString
	b       bool         // jBool
}

// jsonDoc 是一份已解析的文档：源字节 + 根节点 + 待应用编辑。
type jsonDoc struct {
	src   []byte
	root  *jsonNode
	edits []jsonEdit
}

type jsonEdit struct {
	start, end int
	repl       []byte
}

// ── 解析 ────────────────────────────────────────────────────────────────────

type jsonParser struct {
	src   []byte
	pos   int
	depth int
}

// parseJSONDoc 解析一份严格 JSON（顶层必须是对象或数组），记录全部字节区间。
func parseJSONDoc(src []byte) (*jsonDoc, error) {
	p := &jsonParser{src: src}
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
	if root.kind != jObject && root.kind != jArray {
		return nil, fmt.Errorf("顶层必须是对象或数组")
	}
	return &jsonDoc{src: src, root: root}, nil
}

func (p *jsonParser) ws() {
	for p.pos < len(p.src) {
		switch p.src[p.pos] {
		case ' ', '\t', '\n', '\r':
			p.pos++
		default:
			return
		}
	}
}

func (p *jsonParser) value() (*jsonNode, error) {
	p.ws()
	if p.pos >= len(p.src) {
		return nil, fmt.Errorf("期望一个值，但已到内容末尾")
	}
	if p.depth >= maxJSONDepth {
		return nil, fmt.Errorf("JSON 嵌套过深（上限 %d 层）", maxJSONDepth)
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

func (p *jsonParser) object() (*jsonNode, error) {
	start := p.pos
	p.pos++ // '{'
	p.depth++
	defer func() { p.depth-- }()
	n := &jsonNode{kind: jObject, start: start, src: p.src}
	p.ws()
	if p.pos < len(p.src) && p.src[p.pos] == '}' {
		p.pos++
		n.end = p.pos
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
		p.ws()
		if p.pos >= len(p.src) || p.src[p.pos] != ':' {
			return nil, fmt.Errorf("偏移 %d 处期望 ':'", p.pos)
		}
		p.pos++
		val, err := p.value()
		if err != nil {
			return nil, err
		}
		n.members = append(n.members, jsonMember{
			key:   keyNode.str,
			val:   val,
			start: keyStart,
			end:   val.end,
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
			n.end = p.pos
			return n, nil
		}
		return nil, fmt.Errorf("偏移 %d 处期望 ',' 或 '}'", p.pos)
	}
}

func (p *jsonParser) array() (*jsonNode, error) {
	start := p.pos
	p.pos++ // '['
	p.depth++
	defer func() { p.depth-- }()
	n := &jsonNode{kind: jArray, start: start, src: p.src}
	p.ws()
	if p.pos < len(p.src) && p.src[p.pos] == ']' {
		p.pos++
		n.end = p.pos
		return n, nil
	}
	for {
		// 数组元素只需被正确跳过：router 数据面从不读写数组内部（只改 model 与
		// output_config.effort），因此这里刻意**不记录**元素，避免为长 messages
		// 数组付出按元素的记账开销。
		if _, err := p.value(); err != nil {
			return nil, err
		}
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
			n.end = p.pos
			return n, nil
		}
		return nil, fmt.Errorf("偏移 %d 处期望 ',' 或 ']'", p.pos)
	}
}

func (p *jsonParser) str() (*jsonNode, error) {
	start := p.pos
	p.pos++ // '"'
	var sb strings.Builder
	for p.pos < len(p.src) {
		c := p.src[p.pos]
		switch c {
		case '"':
			p.pos++
			return &jsonNode{kind: jString, start: start, end: p.pos, str: sb.String(), src: p.src}, nil
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

// number 的接受度与 docmodel 同形：先按宽松字符集扫描，再交给 ParseFloat 判定
// （`01`、`+1` 这类非严格 JSON 字面量在守进程上今天也是接受的）。
func (p *jsonParser) number() (*jsonNode, error) {
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
	return &jsonNode{kind: jNumber, start: start, end: p.pos, src: p.src}, nil
}

func (p *jsonParser) boolean() (*jsonNode, error) {
	start := p.pos
	if strings.HasPrefix(string(p.src[p.pos:]), "true") {
		p.pos += 4
		return &jsonNode{kind: jBool, start: start, end: p.pos, b: true, src: p.src}, nil
	}
	if strings.HasPrefix(string(p.src[p.pos:]), "false") {
		p.pos += 5
		return &jsonNode{kind: jBool, start: start, end: p.pos, src: p.src}, nil
	}
	return nil, fmt.Errorf("偏移 %d 处非法字面量", p.pos)
}

func (p *jsonParser) null() (*jsonNode, error) {
	start := p.pos
	if !strings.HasPrefix(string(p.src[p.pos:]), "null") {
		return nil, fmt.Errorf("偏移 %d 处非法字面量", p.pos)
	}
	p.pos += 4
	return &jsonNode{kind: jNull, start: start, end: p.pos, src: p.src}, nil
}

// ── 查询 ────────────────────────────────────────────────────────────────────

// member 返回指定键的成员；不存在返回 nil。
func (n *jsonNode) member(key string) *jsonMember {
	if n == nil || n.kind != jObject {
		return nil
	}
	for i := range n.members {
		if n.members[i].key == key {
			return &n.members[i]
		}
	}
	return nil
}

// get 按键路径取值；不存在返回 nil。
func (n *jsonNode) get(path ...string) *jsonNode {
	cur := n
	for _, key := range path {
		if cur == nil || cur.kind != jObject {
			return nil
		}
		m := cur.member(key)
		if m == nil {
			return nil
		}
		cur = m.val
	}
	return cur
}

// asString 返回字符串值；非字符串返回 ok=false。
func (n *jsonNode) asString() (string, bool) {
	if n == nil || n.kind != jString {
		return "", false
	}
	return n.str, true
}

// asBool 返回布尔值。
func (n *jsonNode) asBool() (bool, bool) {
	if n == nil || n.kind != jBool {
		return false, false
	}
	return n.b, true
}

// raw 返回该节点的原始字节（未经修改时即源文本片段）。
func (n *jsonNode) raw() []byte {
	if n == nil {
		return nil
	}
	return n.src[n.start:n.end]
}

// ── 变更 ────────────────────────────────────────────────────────────────────

// add 记录一次编辑，并拒绝与既有编辑区间重叠的改动（重叠必然产生歧义）。
func (d *jsonDoc) add(start, end int, repl []byte) error {
	if start < 0 || end > len(d.src) || start > end {
		return fmt.Errorf("非法编辑区间 [%d,%d)（源长度 %d）", start, end, len(d.src))
	}
	for _, e := range d.edits {
		if start < e.end && e.start < end {
			return fmt.Errorf("编辑区间冲突: [%d,%d) 与 [%d,%d) 重叠", start, end, e.start, e.end)
		}
	}
	d.edits = append(d.edits, jsonEdit{start: start, end: end, repl: repl})
	return nil
}

// setRaw 把 path 处的值替换为 enc（已编码的 JSON 字面量）。
// 路径不存在时在末尾插入，插入格式对齐该对象的既有排版风格。
func (d *jsonDoc) setRaw(enc []byte, path ...string) error {
	if len(path) == 0 {
		return fmt.Errorf("空路径")
	}
	parent := d.root.get(path[:len(path)-1]...)
	if parent == nil {
		return fmt.Errorf("路径父级不存在: %v", path[:len(path)-1])
	}
	if parent.kind != jObject {
		return fmt.Errorf("路径父级不是对象: %v", path[:len(path)-1])
	}
	key := path[len(path)-1]
	if m := parent.member(key); m != nil {
		return d.add(m.val.start, m.val.end, enc)
	}
	return d.insertMember(parent, key, enc)
}

// insertMember 在对象末尾插入一个新成员，排版跟随对象现状。
func (d *jsonDoc) insertMember(obj *jsonNode, key string, enc []byte) error {
	keyBytes := encodeJSONString(key)
	style := detectStyle(obj)
	var ins []byte
	if len(obj.members) == 0 {
		// 空对象：在 { 与 } 之间插入（两花括号可能相邻，也可能有空白 ⇒ 整段替换最稳）。
		ins = append([]byte(nil), keyBytes...)
		ins = append(ins, ':', ' ')
		ins = append(ins, enc...)
		body := ins
		if style.multiline {
			body = append(append([]byte{'\n'}, style.indent...), body...)
			body = append(body, '\n')
		}
		return d.add(obj.start, obj.end, append(append([]byte{'{'}, body...), '}'))
	}
	last := obj.members[len(obj.members)-1]
	ins = append(ins, ',', '\n')
	ins = append(ins, style.indent...)
	ins = append(ins, keyBytes...)
	ins = append(ins, ':', ' ')
	ins = append(ins, enc...)
	return d.add(last.end, last.end, ins)
}

// replaceRaw 用给定字节替换某路径处的值（写入精确的目标字节）。
func (d *jsonDoc) replaceRaw(repl []byte, path ...string) error {
	n := d.root.get(path...)
	if n == nil {
		return fmt.Errorf("路径不存在: %v", path)
	}
	return d.add(n.start, n.end, repl)
}

// ── 渲染 ────────────────────────────────────────────────────────────────────

// render 应用全部编辑，返回结果字节。未被编辑的字节逐字节保留。
func (d *jsonDoc) render() []byte {
	out := make([]byte, 0, len(d.src)+64)
	prev := 0
	es := append([]jsonEdit(nil), d.edits...)
	sort.Slice(es, func(i, j int) bool { return es[i].start < es[j].start })
	for _, e := range es {
		out = append(out, d.src[prev:e.start]...)
		out = append(out, e.repl...)
		prev = e.end
	}
	out = append(out, d.src[prev:]...)
	return out
}

// dirty 报告是否有改动。
func (d *jsonDoc) dirty() bool { return len(d.edits) > 0 }

// ── 排版探测 ────────────────────────────────────────────────────────────────

type jsonStyle struct {
	multiline bool
	indent    []byte
}

// detectStyle 推断一个对象容器的排版：是否多行、成员缩进多少。
// 数组不在 router 数据面的改写面上，故不做数组分支（docmodel 侧的数组分支供
// AppendToArray 使用）。
func detectStyle(n *jsonNode) jsonStyle {
	if n == nil || len(n.src) == 0 {
		return jsonStyle{}
	}
	if n.kind != jObject {
		return jsonStyle{}
	}
	if len(n.members) >= 1 {
		inner := n.src[n.start:n.members[0].start]
		if bytes.IndexByte(inner, '\n') >= 0 {
			return jsonStyle{multiline: true, indent: leadingWSBefore(n.src, n.members[len(n.members)-1].start, n.start)}
		}
	}
	if bytes.IndexByte(n.src[n.start:n.end], '\n') >= 0 {
		return jsonStyle{multiline: true, indent: []byte("  ")}
	}
	return jsonStyle{}
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
		return nil
	}
	return src[i:j]
}

// ── 编码 ────────────────────────────────────────────────────────────────────

// encodeJSONString 产出 JSON 字符串字面量。
//
// 必须显式关掉 HTML 转义：encoding/json 默认把 `<`/`>`/`&` 转成 < 等，那是给
// HTML 嵌入用的，写在请求体里会平白改变上游看到的字节（与 internal/proxy 同口径）。
//
// 不可失败：Encoder 对字符串只在底层 Writer 出错时报错，而这里的 Writer 是
// bytes.Buffer，故调用点没有错误分支（失败面为零，不是被忽略）。
func encodeJSONString(s string) []byte {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(s)
	return bytes.TrimRight(buf.Bytes(), "\n")
}

// encodeEffortObject 产出 `{"effort":"<to>"}`（键序与 internal/docmodel 的
// encodeCanonical(map[string]string) 同形：单键、紧凑、无 HTML 转义）。
func encodeEffortObject(to string) []byte {
	key := encodeJSONString("effort")
	val := encodeJSONString(to)
	out := make([]byte, 0, len(key)+len(val)+4)
	out = append(out, '{')
	out = append(out, key...)
	out = append(out, ':')
	out = append(out, val...)
	out = append(out, '}')
	return out
}
