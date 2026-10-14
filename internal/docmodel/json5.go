// JSON5 层：容错**读取** + 区间级**增删渲染**（openclaw / opencode / pi 三族共用，
// 全仓唯一实现）。
//
// # 为什么需要它
//
// 这三个家族的盘上文件都可能是 JSON5：openclaw 的 house style 本身就是 JSON5
// （根键 `models:` / `agents:` 是**裸标识符**、嵌套键带引号，acceptance-recipe §A 行 7），
// opencode 与 pi 的文件允许注释、单引号与尾随逗号（§A 行 6/9「JSON5 读」）。
// 严格 JSON 读者（jq / json.load）在这类文件上必然失败——**盘上形态合法，不是我们写坏了**
// （§A 行 7 明确：段内容由 JSON 序列化后回填、根键名由文本拼接产出）。
//
// 本文件曾与 `internal/family/openclaw/json5` 并存（两份实现、对 Infinity/NaN 与
// 十六进制字面量的处理已经不一致）。2026-09 收敛为一份：那边**可编辑**的能力
// （SetJSON / Delete / Render / house style 渲染）并入此处，那一边整包删除。
//
// # 两条写侧纪律
//
//  1. 「用户没碰过的字节一个都不能变」：改动只以「区间 → 新字节」表达，未编辑的字节
//     原样通过（`JSON5Doc.Render`）。既有键的记号形态（裸标识符 / 单引号 / 双引号）
//     永远原样保留；只有**新插入**才按 house style 决定形态（`BareRootKeys`）。
//  2. 写回的**值**一律经 [`CanonicalJSON`] 渲染（排序键 + 2 空格缩进）后按成员缩进回填，
//     因此写出去的文件必然是合法 JSON：这要求解析期就把 `Infinity`/`NaN` 拒掉、
//     把十六进制等非 JSON 字面量归一成十进制（否则 `CanonicalJSON` 会写出非法 JSON，
//     而客户端读回来的是另一个值——静默变质比报错危险得多）。
package docmodel

import (
	"bytes"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"
)

// ParseJSON5 容错解析 JSON5，返回根节点（只读路径）。
//
// 顶层必须是对象或数组（与 ParseJSON 的约束一致）：三个消费家族的根都是对象，
// 顶层标量只可能意味着文件已被别的工具写坏，此时应当 fail-closed 而不是猜。
// 需要「就地增删 + 渲染」时用 ParseJSON5Doc。
func ParseJSON5(src []byte) (*Node, error) {
	doc, err := ParseJSON5Doc(src)
	if err != nil {
		return nil, err
	}
	return doc.Root(), nil
}

// LocateRootSection 定位根对象里名为 name 的**成员值的**字节区间 [start, end)。
//
// 用途：openclaw 的写链把 `models:` / `agents:` 段的内容按 JSON 序列化后定向回填，
// 段外字节（用户的其它根键、注释、空白）一个都不动。
//
// fail-closed 的四种情况返回 ok=false：源不是根对象、该根键不存在、根键重复
// （JSON5 语义是 last-wins，按名替换会留下一个仍然生效的旧值 —— 这种事不能猜）、
// 以及源本身解析失败。
func LocateRootSection(src []byte, name string) (start, end int, ok bool) {
	root, err := ParseJSON5(src)
	if err != nil || root.Kind != KObject {
		return 0, 0, false
	}
	var found *Member
	for i := range root.Members {
		if root.Members[i].Key != name {
			continue
		}
		if found != nil {
			return 0, 0, false // 重复根键：歧义
		}
		found = &root.Members[i]
	}
	if found == nil {
		return 0, 0, false
	}
	return found.Val.Start, found.Val.End, true
}

// NodeToGoValue 把解析结果转成可交给 [`CanonicalJSON`] 的 Go 值。
// 与 [`Node.Interface`] 同义，保留旧的错误返回位置以便调用方统一走 error 通道。
func NodeToGoValue(n *Node) (any, error) { return n.Interface(), nil }

// ── 节点取值 ────────────────────────────────────────────────────────────────

// Raw 返回该节点在源文本中的原始字节（与 AsRaw 同义；两个名字服务两批调用方）。
func (n *Node) Raw() []byte { return n.AsRaw() }

// IsObject 报告节点是对象（nil 安全）。
func (n *Node) IsObject() bool { return n != nil && n.Kind == KObject }

// AsInt 返回整数字面量的值；非数字、非整数（含指数形态与超范围）返回 ok=false。
func (n *Node) AsInt() (int64, bool) {
	if n == nil || n.Kind != KNumber {
		return 0, false
	}
	v, err := strconv.ParseInt(n.Num, 10, 64)
	if err != nil {
		return 0, false
	}
	return v, true
}

// Interface 把值转成 Go 原生结构（数字用 json.Number 保字面量），
// 供 [`CanonicalJSON`] 渲染与调用方做值比较。
func (n *Node) Interface() any {
	if n == nil {
		return nil
	}
	switch n.Kind {
	case KObject:
		m := make(map[string]any, len(n.Members))
		for i := range n.Members {
			m[n.Members[i].Key] = n.Members[i].Val.Interface()
		}
		return m
	case KArray:
		out := make([]any, 0, len(n.Elems))
		for _, e := range n.Elems {
			out = append(out, e.Interface())
		}
		return out
	case KString:
		return n.Str
	case KNumber:
		return json.Number(n.Num)
	case KBool:
		return n.Bool
	}
	return nil
}

// Canonical 返回该值的规范 JSON 渲染（排序键 + 2 空格缩进 + 尾换行），
// 供「值等价则不写」的判定：盘上形态与目标形态的语义等价由它比较，
// 因此用户自己的排版不会被无谓地改写。渲染失败返回 nil。
func (n *Node) Canonical() []byte {
	if n == nil {
		return nil
	}
	b, err := CanonicalJSON(n.Interface())
	if err != nil {
		return nil
	}
	return b
}

// ── 变更（可编辑文档） ──────────────────────────────────────────────────────

// JSON5Doc 是一份已解析的 JSON5：源字节 + 根节点 + 待应用编辑。
type JSON5Doc struct {
	src   []byte
	root  *Node
	edits []edit
	// BareRootKeys 决定**新增根成员**的键记号形态：true 时裸标识符（openclaw 的
	// house style），false 时带引号。既有的键记号永远原样保留。
	BareRootKeys bool
}

// ParseJSON5Doc 解析一份 JSON5 并返回可编辑文档。
func ParseJSON5Doc(src []byte) (*JSON5Doc, error) {
	p := &json5Parser{src: src}
	root, err := p.value()
	if err != nil {
		return nil, err
	}
	if err := p.ws(); err != nil {
		return nil, err
	}
	if p.pos != len(p.src) {
		return nil, fmt.Errorf("JSON5 尾部有未解析内容（偏移 %d）", p.pos)
	}
	if root.Kind != KObject && root.Kind != KArray {
		return nil, fmt.Errorf("JSON5 顶层必须是对象或数组，实际是 %s", root.Kind)
	}
	return &JSON5Doc{src: src, root: root, BareRootKeys: rootHasBareKeys(root)}, nil
}

// rootHasBareKeys 报告根对象的键记号是否已是裸标识符形态（openclaw house style）。
// 这是**插入新根键时**的排版依据：盘上既然裸着，我们新写的根键也跟着裸。
func rootHasBareKeys(root *Node) bool {
	if root == nil || root.Kind != KObject || len(root.Members) == 0 {
		return false
	}
	for i := range root.Members {
		raw := root.Members[i].KeyRaw
		if len(raw) > 0 && raw[0] != '"' && raw[0] != '\'' {
			return true
		}
	}
	return false
}

// Root 返回根节点。
func (d *JSON5Doc) Root() *Node { return d.root }

// Src 返回原始源字节。
func (d *JSON5Doc) Src() []byte { return d.src }

// Dirty 报告是否有改动。
func (d *JSON5Doc) Dirty() bool { return len(d.edits) > 0 }

// Render 应用全部编辑，返回结果字节。未被编辑的字节逐字节保留。
//
// 排序用 SliceStable：同一偏移上的多个零长度插入（例如根对象末尾连续插入两个根段）
// 必须按调用顺序落字节，否则结果不确定。
func (d *JSON5Doc) Render() []byte {
	out := make([]byte, 0, len(d.src)+64)
	prev := 0
	es := append([]edit(nil), d.edits...)
	sort.SliceStable(es, func(i, j int) bool { return es[i].start < es[j].start })
	for _, e := range es {
		out = append(out, d.src[prev:e.start]...)
		out = append(out, e.repl...)
		prev = e.end
	}
	out = append(out, d.src[prev:]...)
	return out
}

// add 记录一次编辑。零长插入允许同点多次（按调用顺序渲染）；**同一非空区间**的
// 二次写入报错而不是「后者胜」——JSON5 的插入多为整段重写（空对象插入即替换
// `{}` 整段），静默覆盖会丢掉先写进去的内容，响亮失败才是对的。
func (d *JSON5Doc) add(start, end int, repl []byte) error {
	if start < 0 || end > len(d.src) || start > end {
		return fmt.Errorf("编辑区间越界: [%d,%d)", start, end)
	}
	for _, e := range d.edits {
		if start < e.end && e.start < end {
			return fmt.Errorf("编辑区间冲突: [%d,%d) 与 [%d,%d) 重叠", start, end, e.start, e.end)
		}
	}
	d.edits = append(d.edits, edit{start: start, end: end, repl: repl})
	return nil
}

// SetJSON 把 path 处的值设为 v（以规范 JSON 渲染 + 按成员缩进回填）。
//
// 与 json.go 的 Doc.SetJSON 同形，差别只有一条：**缺席的中间对象会被新建**（且只在
// 缺席时）。既有值不是对象时**拒写**——用户的 `compaction` 写成字符串时替他丢掉是
// 最坏的默认（§A 行 9「compaction 非对象则拒写」同一条纪律）。
//
// 调用约束：一次 SetJSON 只应落在源文本里**已存在**的路径上，或一次建出缺的那一段；
// 不要连续两次往同一个**刚新建**的子树里写（节点树不随待应用编辑更新）。空对象只支持
// 一次插入，二次插入会以区间冲突响亮失败而不是产出重叠编辑。
func (d *JSON5Doc) SetJSON(v any, path ...string) error {
	if len(path) == 0 {
		return fmt.Errorf("空路径")
	}
	if d.root == nil || d.root.Kind != KObject {
		return fmt.Errorf("根不是对象，不可设置路径 %v", path)
	}
	cur := d.root
	depth := 0
	for ; depth < len(path)-1; depth++ {
		m := cur.MemberOf(path[depth])
		if m == nil {
			break
		}
		if m.Val.Kind != KObject {
			return fmt.Errorf("路径 %v 处的既有值不是对象（拒绝覆盖用户内容）", path[:depth+1])
		}
		cur = m.Val
	}
	if depth == len(path)-1 {
		return d.setMember(cur, path[len(path)-1], v)
	}
	// 缺席的尾部：一次插入整段新建的中间对象（避免「插入后节点树未更新」的二次定位问题）。
	nested := any(v)
	for i := len(path) - 1; i > depth; i-- {
		nested = map[string]any{path[i]: nested}
	}
	return d.setMember(cur, path[depth], nested)
}

// setMember 在对象 obj 内设置成员 key：存在即替换值，缺席即插入。
func (d *JSON5Doc) setMember(obj *Node, key string, v any) error {
	enc, err := CanonicalJSON(v)
	if err != nil {
		return fmt.Errorf("渲染 %q 的值失败: %w", key, err)
	}
	enc = bytes.TrimSuffix(enc, []byte("\n"))
	if m := obj.MemberOf(key); m != nil {
		return d.add(m.Val.Start, m.Val.End, json5Reindent(enc, json5LeadingIndent(d.src, m.Start, obj.Start)))
	}
	return d.insertMember(obj, key, enc)
}

// insertMember 在对象末尾插入一个新成员，排版跟随该对象现状：
//   - 多行对象：新成员自成一行，缩进对齐上一个成员；
//   - 内联对象（成员与 `{` 同行）：按 `, ` 追加在同一行内，不把用户的一行拆成两行；
//   - 空对象：整段重写，让新成员与闭合括号各自成行（因此**同一空对象只能插入一次**，
//     二次插入会因区间冲突响亮失败，而不是悄悄产出重叠编辑）。
func (d *JSON5Doc) insertMember(obj *Node, key string, enc []byte) error {
	keyBytes := d.keyToken(key, obj)
	if len(obj.Members) == 0 {
		outer := json5LeadingIndent(d.src, obj.Start, 0)
		inner := append(append([]byte(nil), outer...), ' ', ' ')
		body := make([]byte, 0, len(enc)+len(inner)*2+8)
		body = append(body, '{', '\n')
		body = append(body, inner...)
		body = append(body, keyBytes...)
		body = append(body, ':', ' ')
		body = append(body, json5Reindent(enc, inner)...)
		body = append(body, '\n')
		body = append(body, outer...)
		body = append(body, '}')
		return d.add(obj.Start, obj.End, body)
	}
	last := obj.Members[len(obj.Members)-1]
	inline := !bytes.Contains(d.src[obj.Start:last.Start], []byte("\n"))
	indent := []byte(nil)
	if !inline {
		indent = json5LeadingIndent(d.src, last.Start, obj.Start)
	}
	ins := make([]byte, 0, len(enc)+len(indent)+8)
	ins = append(ins, ',')
	if inline {
		ins = append(ins, ' ')
	} else {
		ins = append(ins, '\n')
		ins = append(ins, indent...)
	}
	ins = append(ins, keyBytes...)
	ins = append(ins, ':', ' ')
	ins = append(ins, json5Reindent(enc, indent)...)
	return d.add(last.End, last.End, ins)
}

// keyToken 产出键记号：根对象的**新**键在 house style 下写裸标识符，其余一律带引号。
func (d *JSON5Doc) keyToken(key string, obj *Node) []byte {
	if d.BareRootKeys && obj == d.root && json5IsIdentifierKey(key) {
		return []byte(key)
	}
	return encodeString(key)
}

// Delete 删除 path 处的成员（连同它的分隔逗号）。不存在即无操作（幂等）。
// 父级缺席或不是对象时同样无操作——「没有要移除的托管条目」不是错误。
func (d *JSON5Doc) Delete(path ...string) error {
	if len(path) == 0 {
		return fmt.Errorf("空路径")
	}
	parent := d.root.Get(path[:len(path)-1]...)
	if parent == nil || parent.Kind != KObject {
		return nil
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
		return nil
	}
	m := parent.Members[idx]
	first := idx == 0
	last := idx == len(parent.Members)-1

	// 与 `{` 同行的成员（紧凑形态）：按成员边界删除，不按行删除。
	if !bytes.Contains(d.src[parent.Start:m.Start], []byte("\n")) {
		switch {
		case first && last:
			return d.add(parent.Start, parent.End, []byte("{}"))
		case last:
			comma := json5SeparatorComma(d.src, parent.Members[idx-1].End, m.Start)
			if comma < 0 {
				return fmt.Errorf("删除 %s 失败：找不到分隔逗号", key)
			}
			// JSON5 comments are trivia, and may contain structural-looking bytes
			// such as `}`. Locate the comma between adjacent parsed members instead
			// of scanning source backwards. If comments follow the comma, retain all
			// of that trivia while removing the separator and the member.
			var keep []byte
			if json5HasComment(d.src, comma+1, m.Start) {
				keep = append([]byte(nil), d.src[comma+1:m.Start]...)
			}
			return d.add(comma, m.End, keep)
		default:
			return d.add(m.Start, parent.Members[idx+1].Start, nil)
		}
	}
	// 多行形态：删掉本成员所在的**整行**，闭合括号留在自己的行上。
	//
	// 前导逗号的取舍是这里唯一容易错的一步：本成员行尾没有逗号时，它前面的那个逗号
	// 是**分隔符**（严格 JSON 的常态），必须一并吃掉，否则剩下的 `"a": 1,` 后面直接
	// 跟 `}` 会变成尾随逗号、把严格 JSON 文件改坏；本成员行尾自带逗号时（JSON5 的
	// house style，如 openclaw 的盘上形态），那个逗号属于本行，前一个逗号则是上一行
	// 自己的尾随逗号，必须留下——用户的字节一个都不动。
	//
	// 行尾换行的取舍与它配对：吃掉前导分隔逗号时**保留**换行（上一行要有自己的行尾），
	// 否则连同换行一起删（不留空行）。
	start := lineStartAt(d.src, m.Start)
	ateLeadingComma := false
	if !hasTrailingComma(d.src, m.End) {
		if idx > 0 {
			comma := json5SeparatorComma(d.src, parent.Members[idx-1].End, m.Start)
			if comma < 0 {
				return fmt.Errorf("删除 %s 失败：找不到分隔逗号", key)
			}
			if json5HasComment(d.src, comma+1, m.Start) {
				// A line-based removal would also erase comments placed between the
				// separator and this member. Keep that trivia byte-for-byte and remove
				// only the comma and parsed member span.
				keep := append([]byte(nil), d.src[comma+1:m.Start]...)
				return d.add(comma, m.End, keep)
			}
			start = comma
			ateLeadingComma = true
		}
	}
	end := lineEndAfter(d.src, m.End)
	if brace := firstBraceIn(d.src, m.End, end); brace >= 0 {
		end = brace // 闭合括号必须留在自己的行上
	} else if ateLeadingComma {
		end = trimLineBreak(d.src, end, m.End)
	}
	return d.add(start, end, nil)
}

// json5SeparatorComma returns the single separator comma between adjacent
// parsed members. The range between value.End and the next key may contain
// whitespace and JSON5 comments, so a raw backwards scan can mistake braces or
// commas inside a comment for syntax.
func json5SeparatorComma(src []byte, start, end int) int {
	if start < 0 || end > len(src) || start > end {
		return -1
	}
	parser := json5Parser{src: src[start:end]}
	if err := parser.ws(); err != nil || parser.pos >= len(parser.src) || parser.src[parser.pos] != ',' {
		return -1
	}
	comma := parser.pos
	parser.pos++
	if err := parser.ws(); err != nil || parser.pos != len(parser.src) {
		return -1
	}
	return start + comma
}

// json5HasComment checks a range known to contain only JSON5 inter-member
// trivia (whitespace, comments, and the separator comma).
func json5HasComment(src []byte, start, end int) bool {
	trivia := src[start:end]
	return bytes.Contains(trivia, []byte("//")) || bytes.Contains(trivia, []byte("/*"))
}

// hasTrailingComma 报告 pos 之后（跳过同行空白）是否紧跟一个逗号。
func hasTrailingComma(src []byte, pos int) bool {
	for i := pos; i < len(src); i++ {
		switch src[i] {
		case ' ', '\t':
			continue
		case ',':
			return true
		}
		return false
	}
	return false
}

// firstBraceIn 返回 [from,to) 内第一个闭合括号的位置；没有则 -1。
func firstBraceIn(src []byte, from, to int) int {
	for i := from; i < to && i < len(src); i++ {
		if src[i] == '}' || src[i] == ']' {
			return i
		}
	}
	return -1
}

// trimLineBreak 去掉 end 处的行尾换行（含 CRLF 的 `\r`），使上一行继承该换行。
func trimLineBreak(src []byte, end, floor int) int {
	if end-1 >= floor && end <= len(src) && src[end-1] == '\n' {
		end--
		if end-1 >= floor && src[end-1] == '\r' {
			end--
		}
	}
	return end
}

// ── house style 渲染 ────────────────────────────────────────────────────────

// JSON5Section 是一个根段：键名与值。键名在 house style 下按裸标识符写出。
type JSON5Section struct {
	Key   string
	Value any
}

// RenderJSON5HouseStyle 渲染一份**全新**的 house style 文档（openclaw 新建文件时的形态）：
// 根键名由文本拼接产出（裸标识符），段内容用 CanonicalJSON 序列化后按 2 空格缩进回填，
// 段间以 `,\n` 分隔，尾随换行收尾。
//
// 只用于「没有用户字节要保留」的场合（文件缺席、或根对象是空对象）；增量写入一律走
// SetJSON 的字节保真编辑。
func RenderJSON5HouseStyle(sections []JSON5Section) ([]byte, error) {
	if len(sections) == 0 {
		return []byte("{\n}\n"), nil
	}
	out := []byte("{\n")
	for i, s := range sections {
		enc, err := CanonicalJSON(s.Value)
		if err != nil {
			return nil, fmt.Errorf("渲染根段 %q 失败: %w", s.Key, err)
		}
		enc = bytes.TrimSuffix(enc, []byte("\n"))
		key := encodeString(s.Key)
		if json5IsIdentifierKey(s.Key) {
			key = []byte(s.Key)
		}
		out = append(out, ' ', ' ')
		out = append(out, key...)
		out = append(out, ':', ' ')
		out = append(out, json5Reindent(enc, []byte("  "))...)
		if i < len(sections)-1 {
			out = append(out, ',')
		}
		out = append(out, '\n')
	}
	out = append(out, '}', '\n')
	return out, nil
}

// ── 排版原语 ────────────────────────────────────────────────────────────────

// json5Reindent 把规范渲染的多行值按 indent 回填：首行原地接在 `key: ` 之后，
// 其余行前置 indent。
func json5Reindent(enc []byte, indent []byte) []byte {
	if len(indent) == 0 || !bytes.Contains(enc, []byte("\n")) {
		return enc
	}
	lines := bytes.Split(enc, []byte("\n"))
	out := make([]byte, 0, len(enc)+len(indent)*len(lines))
	out = append(out, lines[0]...)
	for _, line := range lines[1:] {
		out = append(out, '\n')
		out = append(out, indent...)
		out = append(out, line...)
	}
	return out
}

// json5LeadingIndent 返回 pos 所在行的行首空白（以 floor 为下界）。
func json5LeadingIndent(src []byte, pos, floor int) []byte {
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

// json5IsIdentifierKey 报告 key 是否是裸标识符形态（ASCII，与 v1 的
// is_identifier_key 同域）。
func json5IsIdentifierKey(key string) bool {
	if key == "" || !isJSON5IdentStart(key[0]) {
		return false
	}
	for i := 1; i < len(key); i++ {
		if !isJSON5IdentPart(key[i]) {
			return false
		}
	}
	return true
}

// ── 解析器 ──────────────────────────────────────────────────────────────────

type json5Parser struct {
	src []byte
	pos int
}

// ws 跳过空白与两种注释；未闭合的块注释报错（静默吞掉文件尾部是事故温床）。
func (p *json5Parser) ws() error {
	for p.pos < len(p.src) {
		switch c := p.src[p.pos]; c {
		case ' ', '\t', '\n', '\r', '\v', '\f':
			p.pos++
		case 0xEF:
			// UTF-8 BOM：某些编辑器会加上，别让它把整份配置判死。
			if p.pos+2 < len(p.src) && p.src[p.pos+1] == 0xBB && p.src[p.pos+2] == 0xBF {
				p.pos += 3
				continue
			}
			return nil
		case '/':
			if p.pos+1 >= len(p.src) {
				return nil
			}
			switch p.src[p.pos+1] {
			case '/':
				for p.pos < len(p.src) && p.src[p.pos] != '\n' {
					p.pos++
				}
			case '*':
				p.pos += 2
				for {
					if p.pos+1 >= len(p.src) {
						return fmt.Errorf("偏移 %d 处块注释未闭合", p.pos)
					}
					if p.src[p.pos] == '*' && p.src[p.pos+1] == '/' {
						p.pos += 2
						break
					}
					p.pos++
				}
			default:
				return nil
			}
		default:
			return nil
		}
	}
	return nil
}

func (p *json5Parser) value() (*Node, error) {
	if err := p.ws(); err != nil {
		return nil, err
	}
	if p.pos >= len(p.src) {
		return nil, fmt.Errorf("期望一个值，但已到文件末尾")
	}
	switch c := p.src[p.pos]; {
	case c == '{':
		return p.object()
	case c == '[':
		return p.array()
	case c == '"' || c == '\'':
		return p.str()
	case c == 't' || c == 'f':
		return p.boolean()
	case c == 'n':
		return p.literal("null", &Node{Kind: KNull})
	case c == '-' || c == '+' || (c >= '0' && c <= '9') || c == '.':
		return p.number()
	case c == 'I' || c == 'N':
		// Infinity / NaN：无对应 JSON 表示，写回会静默变质（见包注释）。
		return nil, fmt.Errorf("偏移 %d 处的 %s 无对应 JSON 表示（拒绝写入）", p.pos, p.word())
	default:
		return nil, fmt.Errorf("偏移 %d 处出现非预期字符 %q", p.pos, c)
	}
}

func (p *json5Parser) object() (*Node, error) {
	start := p.pos
	p.pos++ // '{'
	n := &Node{Kind: KObject, Start: start, src: p.src}
	for {
		if err := p.ws(); err != nil {
			return nil, err
		}
		if p.pos >= len(p.src) {
			return nil, fmt.Errorf("对象未闭合")
		}
		if p.src[p.pos] == '}' {
			p.pos++
			n.End = p.pos
			return n, nil
		}
		keyStart := p.pos
		key, err := p.key()
		if err != nil {
			return nil, err
		}
		keyRaw := p.src[keyStart:p.pos]
		if err := p.ws(); err != nil {
			return nil, err
		}
		if p.pos >= len(p.src) || p.src[p.pos] != ':' {
			return nil, fmt.Errorf("偏移 %d 处期望 ':'", p.pos)
		}
		p.pos++
		val, err := p.value()
		if err != nil {
			return nil, err
		}
		n.Members = append(n.Members, Member{Key: key, KeyRaw: keyRaw, Val: val, Start: keyStart, End: val.End})
		if err := p.ws(); err != nil {
			return nil, err
		}
		if p.pos >= len(p.src) {
			return nil, fmt.Errorf("对象未闭合")
		}
		switch p.src[p.pos] {
		case ',':
			p.pos++ // 尾随逗号由下一轮的 '}' 分支兜住
		case '}':
			p.pos++
			n.End = p.pos
			return n, nil
		default:
			return nil, fmt.Errorf("偏移 %d 处期望 ',' 或 '}'", p.pos)
		}
	}
}

func (p *json5Parser) array() (*Node, error) {
	start := p.pos
	p.pos++ // '['
	n := &Node{Kind: KArray, Start: start, src: p.src}
	for {
		if err := p.ws(); err != nil {
			return nil, err
		}
		if p.pos >= len(p.src) {
			return nil, fmt.Errorf("数组未闭合")
		}
		if p.src[p.pos] == ']' {
			p.pos++
			n.End = p.pos
			return n, nil
		}
		v, err := p.value()
		if err != nil {
			return nil, err
		}
		n.Elems = append(n.Elems, v)
		if err := p.ws(); err != nil {
			return nil, err
		}
		if p.pos >= len(p.src) {
			return nil, fmt.Errorf("数组未闭合")
		}
		switch p.src[p.pos] {
		case ',':
			p.pos++
		case ']':
			p.pos++
			n.End = p.pos
			return n, nil
		default:
			return nil, fmt.Errorf("偏移 %d 处期望 ',' 或 ']'", p.pos)
		}
	}
}

// key 读取一个对象键：引号字符串或裸标识符（openclaw 的根键就是后者）。
func (p *json5Parser) key() (string, error) {
	switch c := p.src[p.pos]; {
	case c == '"' || c == '\'':
		n, err := p.str()
		if err != nil {
			return "", err
		}
		return n.Str, nil
	case isJSON5IdentStart(c):
		i := p.pos
		for i < len(p.src) && isJSON5IdentPart(p.src[i]) {
			i++
		}
		s := string(p.src[p.pos:i])
		p.pos = i
		return s, nil
	default:
		return "", fmt.Errorf("偏移 %d 处期望对象的键", p.pos)
	}
}

// word 读取 pos 处的标识符文本（仅用于错误信息）。
func (p *json5Parser) word() string {
	i := p.pos
	for i < len(p.src) && isJSON5IdentPart(p.src[i]) {
		i++
	}
	return string(p.src[p.pos:i])
}

func (p *json5Parser) str() (*Node, error) {
	start := p.pos
	q := p.src[p.pos]
	p.pos++
	var sb strings.Builder
	for p.pos < len(p.src) {
		c := p.src[p.pos]
		switch {
		case c == q:
			p.pos++
			return &Node{Kind: KString, Start: start, End: p.pos, Str: sb.String(), src: p.src}, nil
		case c == '\n' || c == '\r':
			return nil, fmt.Errorf("偏移 %d 处字符串出现裸换行", p.pos)
		case c == '\\':
			if err := p.escape(&sb); err != nil {
				return nil, err
			}
		default:
			sb.WriteByte(c)
			p.pos++
		}
	}
	return nil, fmt.Errorf("字符串未闭合")
}

// escape 处理字符串里的一个转义序列。
//
// 严格性：未定义的转义一律拒绝，而不是按 JSON5 规范的「去掉反斜杠按字面字符」处理。
// 理由与 Infinity/NaN 同源：本层的值会被 CanonicalJSON 写回，读错一个字节就会把
// 用户的路径/令牌悄悄改写；宁可让整个文件 fail-closed（报 config_invalid）也不猜。
func (p *json5Parser) escape(sb *strings.Builder) error {
	p.pos++ // '\\'
	if p.pos >= len(p.src) {
		return fmt.Errorf("字符串转义未结束")
	}
	switch c := p.src[p.pos]; c {
	case '\n':
		p.pos++ // 行尾续行：吃掉换行与下一行的前导空白
		for p.pos < len(p.src) && (p.src[p.pos] == ' ' || p.src[p.pos] == '\t') {
			p.pos++
		}
		return nil
	case '\r':
		p.pos++
		if p.pos < len(p.src) && p.src[p.pos] == '\n' {
			p.pos++
		}
		for p.pos < len(p.src) && (p.src[p.pos] == ' ' || p.src[p.pos] == '\t') {
			p.pos++
		}
		return nil
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
	case 'v':
		sb.WriteByte('\v')
	case '0':
		// JSON5 的 `\0` 之后不得紧跟十进制数字（会与历史上的八进制转义混淆）。
		if p.pos+1 < len(p.src) && p.src[p.pos+1] >= '0' && p.src[p.pos+1] <= '9' {
			return fmt.Errorf("偏移 %d 处不支持的 \\0 形态（后随数字）", p.pos)
		}
		sb.WriteByte(0)
	case '\'', '"', '\\', '/':
		sb.WriteByte(c)
	case 'x':
		if p.pos+2 >= len(p.src) {
			return fmt.Errorf("\\x 转义不完整")
		}
		v, err := strconv.ParseUint(string(p.src[p.pos+1:p.pos+3]), 16, 8)
		if err != nil {
			return fmt.Errorf("\\x 转义非法: %q", string(p.src[p.pos+1:p.pos+3]))
		}
		sb.WriteRune(rune(v))
		p.pos += 3
		return nil
	case 'u':
		r, consumed, err := p.unicodeEscape()
		if err != nil {
			return err
		}
		sb.WriteRune(r)
		p.pos += consumed
		return nil
	default:
		return fmt.Errorf("偏移 %d 处未知转义 \\%c", p.pos, c)
	}
	p.pos++
	return nil
}

// unicodeEscape 解析 `\uXXXX`（含代理对）。返回解码后的 rune 与消耗的字节数
// （含 `u` 本身）。孤立代理项没有 UTF-8 表示，一律拒绝。
func (p *json5Parser) unicodeEscape() (rune, int, error) {
	read := func(at int) (rune, error) {
		if at+4 > len(p.src) {
			return 0, fmt.Errorf("\\u 转义不完整")
		}
		v, err := strconv.ParseUint(string(p.src[at:at+4]), 16, 32)
		if err != nil {
			return 0, fmt.Errorf("\\u 转义非法: %q", string(p.src[at:at+4]))
		}
		return rune(v), nil
	}
	hi, err := read(p.pos + 1)
	if err != nil {
		return 0, 0, err
	}
	if hi >= 0xD800 && hi <= 0xDBFF {
		// 高代理项：必须紧跟一个低代理项。
		if p.pos+6+4 <= len(p.src) && p.src[p.pos+5] == '\\' && p.src[p.pos+6] == 'u' {
			lo, err := read(p.pos + 7)
			if err != nil {
				return 0, 0, err
			}
			if lo < 0xDC00 || lo > 0xDFFF {
				return 0, 0, fmt.Errorf("孤立的高代理项转义")
			}
			// 消耗量 = 第一段 `uXXXX`（5）＋ 第二段 `\uXXXX`（6）。
			return rune(0x10000 + (hi-0xD800)<<10 + (lo - 0xDC00)), 11, nil
		}
		return 0, 0, fmt.Errorf("孤立的高代理项转义")
	}
	if hi >= 0xDC00 && hi <= 0xDFFF {
		return 0, 0, fmt.Errorf("孤立的低代理项转义")
	}
	return hi, 5, nil
}

func (p *json5Parser) number() (*Node, error) {
	start := p.pos
	i := p.pos
	for i < len(p.src) && isJSON5NumberByte(p.src[i]) {
		i++
	}
	lit := string(p.src[start:i])
	norm, err := json5NormalizeNumber(lit)
	if err != nil {
		return nil, err
	}
	p.pos = i
	return &Node{Kind: KNumber, Start: start, End: i, Num: norm, src: p.src}, nil
}

// json5NormalizeNumber 把 JSON5 数字字面量归一为**合法 JSON** 字面量：
// 去掉前导 `+`、十六进制转十进制、补 `.5`/`5.` 的前后导点、去整数部前导零。
// 归一后必然能被 strconv.ParseFloat 接受，否则报错（不产出一个写回去就变质的字面量）。
func json5NormalizeNumber(lit string) (string, error) {
	s := lit
	sign := ""
	if strings.HasPrefix(s, "+") {
		s = s[1:]
	} else if strings.HasPrefix(s, "-") {
		sign = "-"
		s = s[1:]
	}
	if s == "" {
		return "", fmt.Errorf("偏移处非法数字字面量 %q", lit)
	}
	if strings.HasPrefix(s, "0x") || strings.HasPrefix(s, "0X") {
		if sign == "-" {
			return "", fmt.Errorf("十六进制字面量不支持负号: %q", lit)
		}
		v, err := strconv.ParseUint(s[2:], 16, 64)
		if err != nil {
			return "", fmt.Errorf("非法十六进制字面量 %q", lit)
		}
		return strconv.FormatUint(v, 10), nil
	}
	if strings.ContainsAny(s, "nN") { // Infinity / NaN
		return "", fmt.Errorf("偏移处非法数字字面量 %q", lit)
	}
	// 指数部分单独处理：尾数里补前后导点、去前导零，指数原样。
	mantissa, exponent := s, ""
	if i := strings.IndexAny(s, "eE"); i >= 0 {
		mantissa, exponent = s[:i], s[i:]
	}
	if strings.HasSuffix(mantissa, ".") {
		mantissa += "0"
	}
	if strings.HasPrefix(mantissa, ".") {
		mantissa = "0" + mantissa
	}
	intPart, fracPart := mantissa, ""
	if i := strings.IndexByte(mantissa, '.'); i >= 0 {
		intPart, fracPart = mantissa[:i], mantissa[i:]
	}
	j := 0
	for j < len(intPart)-1 && intPart[j] == '0' {
		j++ // 去掉整数部前导零，至少留一位
	}
	norm := sign + intPart[j:] + fracPart + exponent
	if _, err := strconv.ParseFloat(norm, 64); err != nil {
		return "", fmt.Errorf("偏移处非法数字字面量 %q", lit)
	}
	return norm, nil
}

func (p *json5Parser) boolean() (*Node, error) {
	if p.src[p.pos] == 't' {
		return p.literal("true", &Node{Kind: KBool, Bool: true})
	}
	return p.literal("false", &Node{Kind: KBool, Bool: false})
}

func (p *json5Parser) literal(word string, n *Node) (*Node, error) {
	start := p.pos
	if !strings.HasPrefix(string(p.src[p.pos:]), word) {
		return nil, fmt.Errorf("偏移 %d 处非法字面量", p.pos)
	}
	p.pos += len(word)
	n.Start, n.End, n.src = start, p.pos, p.src
	return n, nil
}

func isJSON5IdentStart(c byte) bool {
	return c == '_' || c == '$' || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z')
}

func isJSON5IdentPart(c byte) bool {
	return isJSON5IdentStart(c) || (c >= '0' && c <= '9')
}

func isJSON5NumberByte(c byte) bool {
	switch {
	case c >= '0' && c <= '9':
		return true
	case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z':
		return true
	case c == '+' || c == '-' || c == '.':
		return true
	}
	return false
}
