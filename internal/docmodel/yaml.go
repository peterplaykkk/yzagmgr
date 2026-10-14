// YAML 多文档定位层（hermes 的 live 形态）。
//
// 契约来源：acceptance-recipe §L。**本层绝不制造第二个根节点**——在 flow 根上追加
// block 段落在 libyaml 眼里就是第二个隐式文档，Hermes 与我们随即都读不动
// （2026-09-20 现场事故：每轮启动刷新都失败）。因此这里没有 AppendDoc，
// 且所有写侧 API 都只改文档内部的字节区间。
//
// 写侧 API（按调用方的需要分层，全部只动目标那一段，其余字节逐字节保留）：
//
//	SetPathInDoc                替换**已存在**的叶子值（单行）
//	UpsertPathInDoc             已存在则替换、不存在则创建（父级按需创建）；
//	                            block 上下文收多行块值，flow 上下文只收单行值
//	DeletePathInDoc             删成员（含它的多行值整段）
//	DeleteFromSeqInDoc          按谓词删序列元素（hermes 移除托管条目）
//
// 读侧：ParseYAMLDocs（全部文档）/ ParseYAMLSet（可编辑）/ DetectFlowRoot。
// hermes 的三条写链配方：
//   - 新建/替换整段：把 custom_providers 列表的**全部条目文本**拼好后
//     `UpsertPathInDoc(0, []string{"custom_providers"}, text)`；既有条目用
//     `BlockValueFromNode(src, elem, "- ")` 折成相对缩进的块值再拼进来（用户字节不重排）。
//   - 删条目：`DeleteFromSeqInDoc(0, []string{"custom_providers"}, 按 name 前缀匹配)`，
//     顶层托管键用 `DeletePathInDoc`。
//   - flow 根（live 可能是 `{...}`）：同两个 API 直接可用，插入发生在集合内部，
//     文档数恒为 1；无法表达时返回 ErrRefuseSecondRoot，而不是硬写。
package docmodel

import (
	"bytes"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
)

// YAML 层的错误。Caller 按 errors.Is 分支。
var (
	ErrYAMLParse        = errors.New("YAML 解析失败")
	ErrYAMLUnsupported  = errors.New("YAML 形态不在支持范围")
	ErrYAMLValueInvalid = errors.New("YAML 值不合法")
	ErrYAMLEditConflict = errors.New("YAML 编辑区间冲突")
	ErrNoSuchDocument   = errors.New("YAML 文档不存在")
	ErrRefuseSecondRoot = errors.New("拒绝在文档尾部追加内容（会造出第二个根文档）")
)

// ── 行与文档切分 ────────────────────────────────────────────────────────────

// srcLine 是源中的一行（行尾换行不计入）。
type srcLine struct {
	start   int  // 行首偏移
	end     int  // 行内容末（不含 EOL；已剥 '\r'）
	content int  // 行内第一个非空白字节
	blank   bool // 空行或纯注释行
	indent  int  // content - start
}

func splitSrcLines(src []byte) []srcLine {
	var out []srcLine
	for i := 0; i < len(src); {
		start := i
		j := i
		for j < len(src) && src[j] != '\n' {
			j++
		}
		end := j
		if end > start && src[end-1] == '\r' {
			end--
		}
		c := start
		for c < end && (src[c] == ' ' || src[c] == '\t') {
			c++
		}
		out = append(out, srcLine{
			start:   start,
			end:     end,
			content: c,
			blank:   c >= end || src[c] == '#',
			indent:  c - start,
		})
		if j < len(src) {
			i = j + 1
		} else {
			i = j
		}
	}
	return out
}

// yamlDocRange 是一个文档占用的行区间 [first, last)。
type yamlDocRange struct{ first, last int }

// yamlMarker 报告该行是否为列 0 的文档标记（--- / ...）。
// 行内文档（`--- foo`）不在支持范围：它意味着标记行自带内容，本层不做重排。
func yamlMarker(src []byte, ln srcLine, mark string) (bool, error) {
	if ln.indent != 0 || ln.content >= ln.end {
		return false, nil
	}
	if ln.end-ln.start < len(mark) || string(src[ln.start:ln.start+len(mark)]) != mark {
		return false, nil
	}
	rest := skipSpaces(src, ln.start+len(mark), ln.end)
	if rest < ln.end && src[rest] != '#' {
		return false, fmt.Errorf("%w: 偏移 %d 处文档标记 %s 后带内容", ErrYAMLUnsupported, ln.start, mark)
	}
	return true, nil
}

// yamlSplitDocs 按 `---` / `...` 切分文档。只含标记/注释/空行的区间不产生文档条目。
func yamlSplitDocs(src []byte, lines []srcLine) ([]yamlDocRange, error) {
	var raw []yamlDocRange
	cur := 0
	for i := range lines {
		ln := lines[i]
		if ln.indent != 0 {
			continue
		}
		isStart, err := yamlMarker(src, ln, "---")
		if err != nil {
			return nil, err
		}
		isEnd, err := yamlMarker(src, ln, "...")
		if err != nil {
			return nil, err
		}
		if isStart || isEnd {
			raw = append(raw, yamlDocRange{first: cur, last: i})
			cur = i + 1
		}
	}
	raw = append(raw, yamlDocRange{first: cur, last: len(lines)})
	out := make([]yamlDocRange, 0, len(raw))
	for _, r := range raw {
		for i := r.first; i < r.last; i++ {
			if !lines[i].blank {
				out = append(out, r)
				break
			}
		}
	}
	return out, nil
}

// ── 文档集合（可编辑） ──────────────────────────────────────────────────────

// YAMLSet 是一份 YAML 源的全部文档 + 待应用编辑。
type YAMLSet struct {
	src    []byte
	docs   []*Node
	sites  map[string]*yamlSite // 父路径 → 插入点（本轮新建的键都落在那里）
	groups map[int]*yamlGroup   // 插入点位置 → 落在同一点上的站点组
	order  []string             // 插入点创建顺序
	edits  []edit
}

// yamlGroup 是**同一个插入位置**上的若干站点：它们共享一条编辑，渲染时按父级深度
// 从深到浅拼接。
//
// 为什么不能按调用顺序拼接：`agent:`（空值键）的子键与它所在映射的新键，插入点常常
// 是同一个位置（映射末尾）。顺序反了，子键就会被读成外层映射的兄弟——文件能解析，
// 语义却悄悄错了。
type yamlGroup struct {
	editIdx int
	sites   []*yamlSite
}

// ParseYAMLDocs 解析全部文档，返回各文档的根节点（含字节区间）。
// 与 json.go 的 Node 同形，因此 Get / MemberOf / AsRaw 等取值 API 一致。
func ParseYAMLDocs(src []byte) ([]*Node, error) {
	s, err := ParseYAMLSet(src)
	if err != nil {
		return nil, err
	}
	return s.docs, nil
}

// ParseYAMLSet 解析全部文档并返回可编辑的文档集合。
func ParseYAMLSet(src []byte) (*YAMLSet, error) {
	lines := splitSrcLines(src)
	ranges, err := yamlSplitDocs(src, lines)
	if err != nil {
		return nil, err
	}
	s := &YAMLSet{src: src, sites: map[string]*yamlSite{}, groups: map[int]*yamlGroup{}}
	for _, r := range ranges {
		p := &yamlParser{src: src, lines: lines}
		n, err := p.parseDoc(r)
		if err != nil {
			return nil, err
		}
		s.docs = append(s.docs, n)
	}
	return s, nil
}

// Doc 返回第 i 个文档的根节点；越界返回 nil。
func (s *YAMLSet) Doc(i int) *Node {
	if i < 0 || i >= len(s.docs) {
		return nil
	}
	return s.docs[i]
}

// Docs 返回全部文档根节点。
func (s *YAMLSet) Docs() []*Node { return s.docs }

// Src 返回原始源字节。
func (s *YAMLSet) Src() []byte { return s.src }

// Dirty 报告是否有改动。
func (s *YAMLSet) Dirty() bool { return len(s.edits) > 0 }

// DetectFlowRoot 报告源是否为「单个流式 `{...}` 根文档」。
//
// 判据：恰好一份文档、根是映射、且该文档首个字节就是 '{'。
// 多文档（即使第一份是 flow）返回 false —— 那是另一种形态，处理方式不同。
func DetectFlowRoot(src []byte) bool {
	s, err := ParseYAMLSet(src)
	if err != nil || len(s.docs) != 1 {
		return false
	}
	root := s.docs[0]
	if root.Kind != KObject || root.Start >= len(src) {
		return false
	}
	return src[root.Start] == '{'
}

// SetPathInDoc 在 docIndex 指定的文档内按路径定向替换叶子值，
// 其余字节（其它文档、注释、空白）逐字节保留。
//
// 语义边界（fail-closed）：
//   - 路径中任一级不存在、或父级不是映射：返回 ErrRefuseSecondRoot。调用方
//     **不得**因此往文档尾部追加内容——flow 根上加段落就是 §L 的历史事故；
//     正确出路是改走「整文档数据无损归一化」再重试。
//   - 值为空（`key:` 后无内容）：插入时自动补一个空格，否则 `key:VALUE` 会被
//     读成单个标量而不是映射条目。
//   - docIndex 越界：ErrNoSuchDocument。
//   - rawValue 不是单行合法 YAML 值：ErrYAMLValueInvalid（本层不做重排/重缩进）。
func (s *YAMLSet) SetPathInDoc(docIndex int, path []string, rawValue []byte) error {
	if len(path) == 0 {
		return fmt.Errorf("%w: 空路径", ErrYAMLValueInvalid)
	}
	if err := validateYAMLRawValue(rawValue); err != nil {
		return err
	}
	if docIndex < 0 || docIndex >= len(s.docs) {
		return fmt.Errorf("%w: 索引 %d（共 %d 份文档）", ErrNoSuchDocument, docIndex, len(s.docs))
	}
	// 本轮新建（尚未渲染）的键也算「存在」：否则「先 Upsert 建、再 Set 改」会在
	// 第二次调用上误报路径不存在。
	if cur, _, hit, err := s.yamlLocate(docIndex, path); err == nil && hit && cur.pendKey != nil {
		val, verr := yamlPendingValue(rawValue, cur.site != nil && cur.site.flow)
		if verr != nil {
			return verr
		}
		if val.inline == nil {
			return fmt.Errorf("%w: 待渲染键只接受单行值", ErrYAMLValueInvalid)
		}
		cur.pendKey.val = val
		s.yamlSiteRebuild(cur.site)
		return nil
	}
	n := s.docs[docIndex].Get(path...)
	if n == nil {
		return fmt.Errorf("%w: 文档 %d 无路径 %v", ErrRefuseSecondRoot, docIndex, path)
	}
	if n.Kind == KNull {
		repl := rawValue
		if repl[0] != ' ' && repl[0] != '\t' {
			repl = append([]byte{' '}, repl...)
		}
		return s.add(n.Start, n.End, repl)
	}
	return s.add(n.Start, n.End, rawValue)
}

// add 记录一次编辑（同区间后者胜；部分重叠报错）。
func (s *YAMLSet) add(start, end int, repl []byte) error {
	if start < 0 || end > len(s.src) || start > end {
		return fmt.Errorf("%w: [%d,%d) 越界", ErrYAMLEditConflict, start, end)
	}
	for i := range s.edits {
		e := &s.edits[i]
		if start == e.start && end == e.end {
			e.repl = repl
			return nil
		}
		if start < e.end && e.start < end {
			return fmt.Errorf("%w: [%d,%d) 与 [%d,%d) 重叠", ErrYAMLEditConflict, start, end, e.start, e.end)
		}
	}
	s.edits = append(s.edits, edit{start: start, end: end, repl: repl})
	return nil
}

// Render 应用全部编辑；未被编辑的字节逐字节保留。
func (s *YAMLSet) Render() []byte {
	out := make([]byte, 0, len(s.src)+64)
	if len(s.edits) == 0 {
		return append(out, s.src...)
	}
	es := append([]edit(nil), s.edits...)
	sort.SliceStable(es, func(i, j int) bool { return es[i].start < es[j].start })
	prev := 0
	for _, e := range es {
		out = append(out, s.src[prev:e.start]...)
		out = append(out, e.repl...)
		prev = e.end
	}
	return append(out, s.src[prev:]...)
}

// ── 创建与删除（hermes 写链：新建整段 / 删条目 / flow 根定向插入） ────────────
//
// 与 SetPathInDoc 的分工：
//   - SetPathInDoc：路径**已存在**时替换一个叶子值（单行）；
//   - UpsertPathInDoc：已存在时替换，不存在时按文档既有缩进**创建**（父级按需创建），
//     block 上下文接受多行块值（缩进由本层产出，调用方只管块内相对缩进）；
//   - DeletePathInDoc：删成员（键值对整段，含它的多行值）；
//   - DeleteFromSeqInDoc：按谓词删序列元素（hermes 移除托管条目就是这条路）。
//
// 三条硬约束：
//  1. **任何路径都不产出第二个文档**。flow 根（`{...}`）只在集合内部插入；绝不在
//     文档尾部追加段落（§L 的历史事故：追加出的段落在 libyaml 眼里是第二个隐式文档，
//     Hermes 与我们随即都读不动）。
//  2. 无法表达时返回 ErrRefuseSecondRoot（父级不是映射、根不是映射……），而不是硬写
//     一个可能读不回来的文件。
//  3. 未触碰的字节逐字节保留：新建/删除只改自己那一段，其余原样通过。

// BlockValueFromNode 把既有节点的源文本折算成可直接喂给 UpsertPathInDoc 的块值。
//
// 场景：hermes 网关档要在 `custom_providers` 里**追加**托管条目，又不能让用户既有的
// 条目重排——于是调用方按 prefix（列表条目是 "- "）把每个既有元素拼回一段文本。
// 直接拼 `Node.AsRaw()` 是不行的：原文里续行带着**源文件的绝对缩进**，而我们要求
// 块值用**相对缩进**（首行从目标层的子级缩进开始，续行相对于首行的内容列）。
// 本函数就做这一次折算：按元素起始列去缩进，再按 prefix 重排列。
//
// src 传该文档的源字节（YAMLSet.Src() 即可）。
func BlockValueFromNode(src []byte, n *Node, prefix string) []byte {
	if n == nil {
		return nil
	}
	raw := n.AsRaw()
	start := n.Start
	if start > len(src) {
		start = len(src)
	}
	col := start - lineStartAt(src, start)
	var out []byte
	for i, ln := range bytes.Split(raw, []byte("\n")) {
		ln = bytes.TrimSuffix(ln, []byte("\r"))
		if i == 0 {
			out = append(out, prefix...)
			out = append(out, ln...)
			continue
		}
		// 续行：先按元素起始列去缩进，再补上 prefix 的宽度
		cut := 0
		for cut < len(ln) && cut < col && (ln[cut] == ' ' || ln[cut] == '\t') {
			cut++
		}
		out = append(out, '\n')
		out = append(out, bytes.Repeat([]byte(" "), len(prefix))...)
		out = append(out, ln[cut:]...)
	}
	return out
}

// ErrYAMLNotASequence 报告「按元素删序列」的目标不是序列。
var ErrYAMLNotASequence = errors.New("YAML 目标不是序列")

// ErrYAMLDuplicateKey 报告待渲染内容会让同一映射出现重复键。
//
// YAML 的重复键是 **last-wins**：解析不报错、前一个静默失效。产出这种文件等于
// 「写成功、配置错的」——比报错危险得多，所以本层两层设防：
//   - 追加新键时若同层已有该键，当场拒绝（yamlAppendPending）；
//   - 渲染前可用 Validate 把产物解析回来复核（写链应在落盘前调用一次）。
var ErrYAMLDuplicateKey = errors.New("YAML 同一映射出现重复键")

// yamlPendMap / yamlPendKey / yamlPendVal 是插入点里「本轮新建的键」的结构化表示。
//
// 用结构而不是成品文本，是因为调用方常常连续写同一批新键（先建 custom_providers，
// 再补它下面的子键）：成品文本没法再定位，第二次写入要么插重复键、要么报「不存在」。
type yamlPendMap struct {
	flow    bool // 渲染成流式形态（flow 根里新建的子树）
	entries []*yamlPendKey
}

type yamlPendKey struct {
	key string
	val yamlPendVal
}

// yamlPendVal 三选一：单行值 / 多行块（整体缩进） / 新建的子映射。
type yamlPendVal struct {
	inline []byte
	block  []byte
	child  *yamlPendMap
}

func (m *yamlPendMap) find(key string) *yamlPendKey {
	for _, e := range m.entries {
		if e.key == key {
			return e
		}
	}
	return nil
}

func (m *yamlPendMap) remove(key string) bool {
	for i, e := range m.entries {
		if e.key == key {
			m.entries = append(m.entries[:i], m.entries[i+1:]...)
			return true
		}
	}
	return false
}

// yamlSite 是一个插入点：某个父级下本轮新建的键都落在这里，可以反复改写、可以整体撤销。
type yamlSite struct {
	key      string
	path     []string
	docIndex int
	editIdx  int

	flow   bool
	pos    int    // 插入点在源中的位置
	prefix []byte // 插入点位于「末行没有换行」处时需要先补的换行
	eol    string

	indent []byte // block：新键的缩进（对齐同级）
	unit   []byte // block：每深一级的缩进单位（沿用文档现状）

	flowInline  bool   // flow：闭合括号与末位成员同行（用 `, ` 追加）
	entryIndent []byte // flow 多行：新条目行的缩进
	needHead    bool   // flow：第一个新条目前面要补分隔逗号

	root *yamlPendMap
	repl []byte
}

// ── 站点管理 ────────────────────────────────────────────────────────────────

func yamlSiteKey(docIndex int, path []string) string {
	return strconv.Itoa(docIndex) + "\x00" + strings.Join(path, "\x00")
}

func (s *YAMLSet) yamlRegisterSite(site *yamlSite) (*yamlSite, error) {
	group := s.groups[site.pos]
	if group == nil {
		if err := s.add(site.pos, site.pos, nil); err != nil {
			return nil, err
		}
		group = &yamlGroup{editIdx: len(s.edits) - 1}
		s.groups[site.pos] = group
	}
	site.editIdx = group.editIdx
	group.sites = append(group.sites, site)
	s.sites[site.key] = site
	s.order = append(s.order, site.key)
	s.yamlSiteRebuild(site)
	return site, nil
}

// yamlRebuildGroup 把同一插入位置上各站点的待渲染字节按父级深度从深到浅拼成一条编辑。
func (s *YAMLSet) yamlRebuildGroup(pos int) {
	group := s.groups[pos]
	if group == nil {
		return
	}
	ordered := append([]*yamlSite(nil), group.sites...)
	sort.SliceStable(ordered, func(i, j int) bool { return len(ordered[i].path) > len(ordered[j].path) })
	var combined []byte
	for _, g := range ordered {
		combined = append(combined, g.repl...)
	}
	s.edits[group.editIdx].repl = combined
}

// yamlSiteRebuild 重建插入点的待渲染字节。
func (s *YAMLSet) yamlSiteRebuild(site *yamlSite) {
	var b bytes.Buffer
	if site.flow {
		for i, e := range site.root.entries {
			if i == 0 {
				if site.needHead {
					b.WriteString(",")
				}
			} else {
				b.WriteString(",")
			}
			if site.flowInline {
				b.WriteByte(' ')
			} else {
				b.WriteString(site.eol)
				b.Write(site.entryIndent)
			}
			b.Write(yamlFlowKeyToken(e.key))
			b.WriteString(": ")
			b.WriteString(yamlRenderPendingFlow(e.val))
		}
	} else {
		b.Write(site.prefix)
		b.Write(yamlRenderPendingBlock(site.root, site.indent, site.unit, site.eol))
	}
	site.repl = b.Bytes()
	s.yamlRebuildGroup(site.pos)
}

// yamlDropSite 撤销一个插入点（它的键被删光了）：连编辑一起撤，
// 否则文件里会留下一行幽灵换行。
func (s *YAMLSet) yamlDropSite(key string) error {
	site := s.sites[key]
	if site == nil {
		return nil
	}
	group := s.groups[site.pos]
	for i, g := range group.sites {
		if g == site {
			group.sites = append(group.sites[:i], group.sites[i+1:]...)
			break
		}
	}
	delete(s.sites, key)
	if len(group.sites) == 0 {
		s.edits = append(s.edits[:group.editIdx], s.edits[group.editIdx+1:]...)
		delete(s.groups, site.pos)
		for _, other := range s.groups {
			if other.editIdx > group.editIdx {
				other.editIdx--
			}
		}
	} else {
		s.yamlRebuildGroup(site.pos)
	}
	for i, k := range s.order {
		if k == key {
			s.order = append(s.order[:i], s.order[i+1:]...)
			break
		}
	}
	for _, other := range s.sites {
		if other.editIdx > site.editIdx {
			other.editIdx--
		}
	}
	return nil
}

// ── 渲染 ────────────────────────────────────────────────────────────────────

// yamlRenderPendingBlock 渲染一个 block 插入点的全部待渲染键（每行以 eol 收尾）。
func yamlRenderPendingBlock(m *yamlPendMap, indent, unit []byte, eol string) []byte {
	var out []byte
	childIndent := append(append([]byte(nil), indent...), unit...)
	for _, e := range m.entries {
		out = append(out, indent...)
		out = append(out, yamlBlockKeyToken(e.key)...)
		out = append(out, ':')
		switch {
		case e.val.inline != nil:
			out = append(out, ' ')
			out = append(out, e.val.inline...)
			out = append(out, eol...)
		case e.val.block != nil:
			out = append(out, eol...)
			out = append(out, yamlIndentAllLines(e.val.block, childIndent, []byte(eol))...)
		case e.val.child != nil:
			if e.val.child.flow {
				out = append(out, ' ')
				out = append(out, yamlRenderPendingFlowMap(e.val.child)...)
				out = append(out, eol...)
			} else {
				out = append(out, eol...)
				out = append(out, yamlRenderPendingBlock(e.val.child, childIndent, unit, eol)...)
			}
		default:
			out = append(out, eol...)
		}
	}
	return out
}

// yamlRenderPendingFlow 渲染一个流式值（单行）。
func yamlRenderPendingFlow(v yamlPendVal) string {
	switch {
	case v.child != nil:
		return string(yamlRenderPendingFlowMap(v.child))
	case v.inline != nil:
		return string(v.inline)
	default:
		return "null" // 块值在流式上下文里不被接受（插入时已拒）
	}
}

func yamlRenderPendingFlowMap(m *yamlPendMap) []byte {
	out := []byte{'{'}
	for i, e := range m.entries {
		if i > 0 {
			out = append(out, ',', ' ')
		}
		out = append(out, yamlFlowKeyToken(e.key)...)
		out = append(out, ':', ' ')
		out = append(out, yamlRenderPendingFlow(e.val)...)
	}
	return append(out, '}')
}

// yamlBlockKeyToken 产出块上下文的键记号：安全纯量原样，否则双引号。
func yamlBlockKeyToken(key string) []byte {
	if key != "" && !strings.ContainsAny(key, ":#{}[],&*!|>'\"%@` \t") && key[0] != '-' && key[0] != '?' {
		return []byte(key)
	}
	return encodeString(key)
}

// yamlFlowKeyToken 产出流式上下文的键记号：一律双引号（保住 JSON 读者的兼容性）。
func yamlFlowKeyToken(key string) []byte { return encodeString(key) }

// yamlIndentAllLines 把 raw 的每一行前置 indent，并按 eol 重新连接（换行风格归一）。
func yamlIndentAllLines(raw, indent, eol []byte) []byte {
	var out []byte
	for i, ln := range bytes.Split(raw, []byte("\n")) {
		_ = i
		ln = bytes.TrimSuffix(ln, []byte("\r"))
		out = append(out, indent...)
		out = append(out, ln...)
		out = append(out, eol...)
	}
	return out
}

// yamlIndentAfterFirst 把 raw 的首行原样保留、其余行前置 indent（用于替换
// 「已经在自己的行上」的块值：首行原地落进既有缩进里）。
func yamlIndentAfterFirst(raw, indent []byte, eol string) []byte {
	lines := bytes.Split(raw, []byte("\n"))
	var out []byte
	out = append(out, bytes.TrimSuffix(lines[0], []byte("\r"))...)
	for _, ln := range lines[1:] {
		out = append(out, eol...)
		out = append(out, indent...)
		out = append(out, bytes.TrimSuffix(ln, []byte("\r"))...)
	}
	return out
}

// ── 缩进探测 ────────────────────────────────────────────────────────────────

// yamlLineIndentBytes 返回 pos 所在行的行首空白。
func yamlLineIndentBytes(src []byte, pos int) []byte {
	ls := lineStartAt(src, pos)
	return src[ls:skipSpaces(src, ls, len(src))]
}

// yamlIsOwnLine 报告 pos 之前（本行内）只有空白——即该节点不依傍上一级键。
func yamlIsOwnLine(src []byte, pos int) bool {
	ls := lineStartAt(src, pos)
	return skipSpaces(src, ls, len(src)) >= pos
}

// yamlNodeIsFlow 报告节点是流式集合（`{...}` / `[...]`）。
func yamlNodeIsFlow(src []byte, n *Node) bool {
	if n == nil || (n.Kind != KObject && n.Kind != KArray) || n.Start >= len(src) {
		return false
	}
	return src[n.Start] == '{' || src[n.Start] == '['
}

// yamlIndentUnit 从文档现状里探测缩进单位（「按该文档既有缩进创建」的唯一依据）。
// 找不到嵌套就更新的样本时退回两个空格。
func yamlIndentUnit(src []byte, docs []*Node) []byte {
	var found []byte
	var walk func(n *Node)
	walk = func(n *Node) {
		if n == nil || found != nil {
			return
		}
		switch n.Kind {
		case KObject:
			for i := range n.Members {
				m := &n.Members[i]
				if v := m.Val; v != nil && v.Kind != KNull && yamlIsOwnLine(src, v.Start) {
					parent := yamlLineIndentBytes(src, m.Start)
					child := yamlLineIndentBytes(src, v.Start)
					if len(child) > len(parent) && bytes.HasPrefix(child, parent) {
						found = bytes.Clone(child[len(parent):])
						return
					}
				}
				walk(m.Val)
			}
		case KArray:
			for _, e := range n.Elems {
				walk(e)
			}
		}
	}
	for _, d := range docs {
		walk(d)
		if found != nil {
			break
		}
	}
	if len(found) == 0 {
		return []byte("  ")
	}
	return found
}

// ── 插入点构造 ──────────────────────────────────────────────────────────────

// yamlSiteForObject 取（必要时创建）「在该映射下新增键」的插入点。
func (s *YAMLSet) yamlSiteForObject(docIndex int, path []string, node *Node) (*yamlSite, error) {
	key := yamlSiteKey(docIndex, path)
	if site := s.sites[key]; site != nil {
		return site, nil
	}
	eol := detectLineEnding(s.src)
	if yamlNodeIsFlow(s.src, node) {
		return s.yamlSiteForFlow(key, docIndex, node, eol, path)
	}
	unit := yamlIndentUnit(s.src, s.docs)
	if len(node.Members) == 0 {
		// 空映射只在流式形态下出现；到这里说明形态异常，保守拒写。
		return nil, fmt.Errorf("%w: 路径 %v 的映射为空且不是流式形态", ErrRefuseSecondRoot, path)
	}
	last := node.Members[len(node.Members)-1]
	pos := lineEndAfter(s.src, last.End)
	site := &yamlSite{
		key: key, docIndex: docIndex, flow: false, pos: pos, eol: eol,
		indent: yamlLineIndentBytes(s.src, last.Start), unit: unit, root: &yamlPendMap{},
	}
	if pos > 0 && s.src[pos-1] != '\n' {
		site.prefix = []byte(eol)
	}
	return s.yamlRegisterSite(site)
}

// yamlSiteForNull 取「在该空值（`key:` 之后无内容）下新增键」的插入点：
// 新键另起一行，插到父键那一行的换行之后（保住行尾注释）。
func (s *YAMLSet) yamlSiteForNull(docIndex int, path []string, node *Node) (*yamlSite, error) {
	key := yamlSiteKey(docIndex, path)
	if site := s.sites[key]; site != nil {
		return site, nil
	}
	eol := detectLineEnding(s.src)
	unit := yamlIndentUnit(s.src, s.docs)
	parentIndent := yamlLineIndentBytes(s.src, node.Start)
	pos := lineEndAfter(s.src, lineContentEnd(s.src, node.Start))
	site := &yamlSite{
		key: key, path: path, docIndex: docIndex, pos: pos, eol: eol,
		indent: append(append([]byte(nil), parentIndent...), unit...), unit: unit,
		root: &yamlPendMap{},
	}
	if pos > 0 && s.src[pos-1] != '\n' {
		site.prefix = []byte(eol)
	}
	return s.yamlRegisterSite(site)
}

// yamlSiteForFlow 取「在流式集合里新增成员」的插入点。
//
// 两种排版：闭合括号与末位成员同行 → 在它前面用 `, ` 追加（不把用户的一行拆开）；
// 闭合括号自成一行 → 在末位成员之后另起一行（保住 JSON 风格的多行排版）。
func (s *YAMLSet) yamlSiteForFlow(key string, docIndex int, node *Node, eol string, path []string) (*yamlSite, error) {
	site := &yamlSite{key: key, path: path, docIndex: docIndex, flow: true, eol: eol, root: &yamlPendMap{flow: true}}
	unit := yamlIndentUnit(s.src, s.docs)
	if len(node.Members) == 0 {
		// 空流式集合：紧跟 `{` 之后另起一行
		site.pos = node.Start + 1
		indent := append(append([]byte(nil), yamlLineIndentBytes(s.src, node.Start)...), unit...)
		site.entryIndent = indent
		return s.yamlRegisterSite(site)
	}
	last := node.Members[len(node.Members)-1]
	close := node.End - 1
	site.needHead = !hasTrailingComma(s.src, last.End)
	if !bytes.Contains(s.src[node.Start:close], []byte("\n")) {
		pos := close
		for pos > node.Start && (s.src[pos-1] == ' ' || s.src[pos-1] == '\t') {
			pos--
		}
		site.pos = pos
		site.flowInline = true
		return s.yamlRegisterSite(site)
	}
	site.pos = last.End
	site.entryIndent = yamlLineIndentBytes(s.src, last.Start)
	return s.yamlRegisterSite(site)
}

// yamlNewRootSite 处理「文件里还没有文档」（空文件 / 只有注释）：按默认缩进创建第一份
// 文档的内容。注意这是**第一份**，不是第二份——空文件本来就没有根。
func (s *YAMLSet) yamlNewRootSite(docIndex int) (*yamlSite, error) {
	if site := s.sites[yamlSiteKey(docIndex, nil)]; site != nil {
		return site, nil // 幂等：同一轮的第二次调用必须落到同一个插入点
	}
	eol := detectLineEnding(s.src)
	site := &yamlSite{
		key: yamlSiteKey(docIndex, nil), path: nil, docIndex: docIndex, pos: len(s.src), eol: eol,
		unit: []byte("  "), root: &yamlPendMap{},
	}
	if len(s.src) > 0 && s.src[len(s.src)-1] != '\n' {
		site.prefix = []byte(eol)
	}
	return s.yamlRegisterSite(site)
}

// ── 定位 ────────────────────────────────────────────────────────────────────

// yamlCursor 是定位过程中的游标：停在源树节点上，或停在插入点的待渲染键上。
type yamlCursor struct {
	node    *Node   // 源节点
	parent  *Node   // 源节点的父（判断 flow/block 与删除范围用）
	member  *Member // 源节点的成员
	pends   *yamlPendMap
	pendKey *yamlPendKey
	pendMap *yamlPendMap
	site    *yamlSite
	path    []string
}

// yamlLocate 沿 path 定位：hit=true 时 cur 指向目标（源节点或待渲染键）；
// hit=false 时 cur 指向最后一个存在的父级、rest 是从该处起缺失的段。
func (s *YAMLSet) yamlLocate(docIndex int, path []string) (cur yamlCursor, rest []string, hit bool, err error) {
	if docIndex < 0 || docIndex >= len(s.docs) {
		return yamlCursor{}, nil, false, fmt.Errorf("%w: 索引 %d（共 %d 份文档）", ErrNoSuchDocument, docIndex, len(s.docs))
	}
	cur = yamlCursor{node: s.docs[docIndex]}
	for i := 0; i < len(path); {
		seg := path[i]
		if cur.pends != nil {
			cur, rest, hit := yamlLocatePendingFrom(cur.site, cur.path, path[i:])
			return cur, rest, hit, nil
		}
		if cur.node == nil {
			return cur, path[i:], false, fmt.Errorf("%w: 路径 %v 处没有可用的父级", ErrRefuseSecondRoot, path[:i])
		}
		switch cur.node.Kind {
		case KObject:
			if m := cur.node.MemberOf(seg); m != nil {
				cur = yamlCursor{node: m.Val, parent: cur.node, member: m, path: append(cur.path, seg)}
				i++
				continue
			}
		case KNull:
			// 空值等于空映射：可以在它下面创建键
		default:
			if i == 0 {
				return cur, path[i:], false, fmt.Errorf("%w: 文档 %d 的根不是映射（%s）", ErrRefuseSecondRoot, docIndex, cur.node.Kind)
			}
			return cur, path[i:], false, fmt.Errorf("%w: 路径 %v 处的值不是映射（%s）", ErrRefuseSecondRoot, path[:i], cur.node.Kind)
		}
		// 源里没有这一段：也许本轮已经在这个父级下建过键
		if site := s.sites[yamlSiteKey(docIndex, cur.path)]; site != nil {
			cur, rest, hit := yamlLocatePendingFrom(site, cur.path, path[i:])
			return cur, rest, hit, nil
		}
		return cur, path[i:], false, nil
	}
	return cur, nil, true, nil
}

// yamlLocatePendingFrom 只沿待渲染树走完 path（basePath 是已经走过的段）。
//
// 停在新建的子映射上时 pendKey 仍指向承载它的那个 entry：这样「删除整段」「整体改写」
// 都还知道该动谁。
func yamlLocatePendingFrom(site *yamlSite, basePath, path []string) (yamlCursor, []string, bool) {
	cur := yamlCursor{pends: site.root, site: site, path: append([]string(nil), basePath...)}
	for i := 0; i < len(path); {
		seg := path[i]
		pk := cur.pends.find(seg)
		if pk == nil {
			return cur, path[i:], false
		}
		next := yamlCursor{site: site, pendMap: cur.pends, pendKey: pk, path: append(cur.path, seg)}
		if pk.val.child != nil {
			next.pends = pk.val.child
			cur = next
			i++
			continue
		}
		return next, path[i+1:], i+1 == len(path)
	}
	return cur, nil, true
}

// ── 值形态 ──────────────────────────────────────────────────────────────────

// yamlCheckPathSegments 拒绝空路径段：空段没有任何 YAML 表达，必然是调用方写错了。
func yamlCheckPathSegments(path []string) error {
	for i, seg := range path {
		if seg == "" {
			return fmt.Errorf("%w: 路径第 %d 段为空", ErrYAMLValueInvalid, i+1)
		}
	}
	return nil
}

// yamlPendingValue 把调用方给的原始值定形：流式上下文只接受单行；block 上下文里
// 含换行的按多行块处理（并当场解析自检）。
func yamlPendingValue(raw []byte, flow bool) (yamlPendVal, error) {
	if len(raw) == 0 {
		return yamlPendVal{}, fmt.Errorf("%w: 空值", ErrYAMLValueInvalid)
	}
	if flow {
		if bytes.IndexByte(raw, '\n') >= 0 || bytes.IndexByte(raw, '\r') >= 0 {
			return yamlPendVal{}, fmt.Errorf("%w: 流式上下文只接受单行值", ErrYAMLValueInvalid)
		}
		if err := validateYAMLRawValue(raw); err != nil {
			return yamlPendVal{}, err
		}
		return yamlPendVal{inline: bytes.Clone(raw)}, nil
	}
	if bytes.IndexByte(raw, '\n') < 0 && bytes.IndexByte(raw, '\r') < 0 {
		inlineErr := validateYAMLRawValue(raw)
		if inlineErr == nil {
			return yamlPendVal{inline: bytes.Clone(raw)}, nil
		}
		// 单行但不是合法内联值：`- name: a`、`name: a` 这类本身就是**块**形态
		// （写成 `key: - name: a` 不是 YAML）。能当块解析就按块处理。
		if blockErr := yamlValidateBlockValue(raw); blockErr != nil {
			return yamlPendVal{}, fmt.Errorf("%w: 值既不是合法内联值（%v）也不是合法块值（%v）",
				ErrYAMLValueInvalid, inlineErr, blockErr)
		}
		return yamlPendVal{block: bytes.Clone(raw)}, nil
	}
	if err := yamlValidateBlockValue(raw); err != nil {
		return yamlPendVal{}, err
	}
	return yamlPendVal{block: bytes.Clone(raw)}, nil
}

// yamlValidateBlockValue 把多行块塞进一个合成父键里解析一遍：块本身写错
// （缩进不一致、引号未闭合）时当场拒绝，而不是产出坏文件。
func yamlValidateBlockValue(raw []byte) error {
	if len(raw) == 0 {
		return fmt.Errorf("%w: 空块值", ErrYAMLValueInvalid)
	}
	var b bytes.Buffer
	b.WriteString("__probe__:\n")
	b.Write(yamlIndentAllLines(raw, []byte("  "), []byte("\n")))
	if _, err := ParseYAMLSet(b.Bytes()); err != nil {
		return fmt.Errorf("%w: 块值不是合法 YAML: %v", ErrYAMLValueInvalid, err)
	}
	return nil
}

// Validate 是**写盘前自检**：把产物解析回来复核，确认
//
//  1. 产物本身能被本层重新解析（写链自检的第一层）；
//  2. 本轮新增的每个键在产物里**恰好出现一次**——重复键在 YAML 里是 last-wins，
//     会静默丢掉先写的那一份（「写成功、配置却是错的」）。
//
// 只检查**本轮新增**的路径：用户文件里原本就有的重复键不在此列——§L 明确把
// 重复顶层键的愈合留给调用方，本层不替用户改他的文件。
func (s *YAMLSet) Validate() error {
	if len(s.sites) == 0 {
		return nil // 只有替换类编辑：键的个数没变，重复与否取决于用户原文件
	}
	out := s.Render()
	docs, err := ParseYAMLDocs(out)
	if err != nil {
		return fmt.Errorf("%w: 产物不可解析: %v", ErrYAMLValueInvalid, err)
	}
	counts := map[string]int{}
	for i, d := range docs {
		yamlCountPaths(d, i, nil, counts)
	}
	for _, key := range s.order {
		site := s.sites[key]
		if site == nil {
			continue
		}
		for _, p := range yamlPendingPaths(site) {
			full := yamlPathID(site.docIndex, p)
			switch n := counts[full]; {
			case n == 0:
				return fmt.Errorf("%w: 新增路径 %v 未出现在产物里", ErrYAMLValueInvalid, p)
			case n > 1:
				return fmt.Errorf("%w: 路径 %v 在产物里出现 %d 次", ErrYAMLDuplicateKey, p, n)
			}
		}
	}
	return nil
}

// yamlCountPaths 统计一份文档里每个映射键的完整路径出现次数。
// 序列元素内部的键不参与统计：路径模型不含下标，逐个统计会把不同元素的同名键误报成重复。
func yamlCountPaths(n *Node, docIdx int, prefix []string, counts map[string]int) {
	if n == nil || n.Kind != KObject {
		return
	}
	for i := range n.Members {
		m := &n.Members[i]
		p := append(append([]string(nil), prefix...), m.Key)
		counts[yamlPathID(docIdx, p)]++
		yamlCountPaths(m.Val, docIdx, p, counts)
	}
}

// yamlPendingPaths 递归收集一个插入点里待渲染键的完整路径。
func yamlPendingPaths(site *yamlSite) [][]string {
	var out [][]string
	var walk func(m *yamlPendMap, prefix []string)
	walk = func(m *yamlPendMap, prefix []string) {
		for _, e := range m.entries {
			p := append(append([]string(nil), prefix...), e.key)
			out = append(out, p)
			if e.val.child != nil {
				walk(e.val.child, p)
			}
		}
	}
	walk(site.root, append([]string(nil), site.path...))
	return out
}

// yamlPathID 把「文档下标 + 路径」压成可比的字符串键。
func yamlPathID(docIndex int, path []string) string {
	return strconv.Itoa(docIndex) + "\x00" + strings.Join(path, "\x00")
}

// ── 创建 ────────────────────────────────────────────────────────────────────

// UpsertPathInDoc 确保 path 处的值等于 rawValue：已存在则替换，不存在则创建。
//
// 创建行为（acceptance-recipe §A 行 8 / §L）：
//   - 父级缺席时一并创建（`custom_providers` 整段、`providers.custom.models.<id>` 深链都可）；
//   - 新键的缩进沿用文档现状（从既有嵌套里探测缩进单位，探测不到才用两个空格）；
//   - block 上下文接受**多行块值**：块内相对缩进由调用方写对，本层负责把它整体
//     缩进到目标层级（因此 `custom_providers` 能写成一串 `- name: …` 的列表）；
//   - flow 上下文（`{...}` 里的键）只接受单行值，键按 JSON 风格双引号写出，
//     插入发生在集合内部——**绝不产出第二个文档**。
//
// 拒绝语义（fail-closed，不用硬写换取成功）：父级不是映射（例如 `compression` 被
// 用户写成了字符串）、根不是映射、需要落在一个块文本内部时，返回 ErrRefuseSecondRoot。
func (s *YAMLSet) UpsertPathInDoc(docIndex int, path []string, rawValue []byte) error {
	if len(path) == 0 {
		return fmt.Errorf("%w: 空路径", ErrYAMLValueInvalid)
	}
	if docIndex != 0 {
		return fmt.Errorf("%w: 索引 %d（共 %d 份文档）", ErrNoSuchDocument, docIndex, len(s.docs))
	}
	if err := yamlCheckPathSegments(path); err != nil {
		return err
	}
	if len(s.docs) == 0 {
		// 空文件没有解析树可走：只能沿「本轮已建的插入点」走。
		//
		// 这里必须复用插入点而不是每轮新建：同一父段下的两个 sibling 各自建一次段头，
		// 就是输出两份同名父段、后一份把前一份的值静默吃掉（YAML 的 last-wins）。
		site := s.sites[yamlSiteKey(docIndex, nil)]
		if site == nil {
			// 先按 block 定形：值不合法就不建站点（失败调用必须零改动）。
			if _, err := yamlPendingValue(rawValue, false); err != nil {
				return err
			}
			var err error
			site, err = s.yamlNewRootSite(docIndex)
			if err != nil {
				return err
			}
		}
		cur, rest, hit := yamlLocatePendingFrom(site, nil, path)
		if hit {
			return s.yamlApplyValue(cur, rawValue)
		}
		if len(rest) == 0 {
			return fmt.Errorf("%w: 文档 %d 无路径 %v", ErrRefuseSecondRoot, docIndex, path)
		}
		return s.yamlAppendPending(cur, rest, rawValue)
	}
	cur, rest, hit, err := s.yamlLocate(docIndex, path)
	if err != nil {
		return err
	}
	if hit {
		return s.yamlApplyValue(cur, rawValue)
	}
	if len(rest) == 0 {
		return fmt.Errorf("%w: 文档 %d 无路径 %v", ErrRefuseSecondRoot, docIndex, path)
	}
	if cur.pends != nil {
		return s.yamlAppendPending(cur, rest, rawValue)
	}
	flow := cur.node != nil && yamlNodeIsFlow(s.src, cur.node)
	if _, err := yamlPendingValue(rawValue, flow); err != nil {
		return err
	}
	switch {
	case cur.node != nil && cur.node.Kind == KObject:
		site, err := s.yamlSiteForObject(docIndex, cur.path, cur.node)
		if err != nil {
			return err
		}
		return s.yamlAppendPending(yamlCursor{pends: site.root, site: site}, rest, rawValue)
	case cur.node != nil && cur.node.Kind == KNull:
		site, err := s.yamlSiteForNull(docIndex, cur.path, cur.node)
		if err != nil {
			return err
		}
		return s.yamlAppendPending(yamlCursor{pends: site.root, site: site}, rest, rawValue)
	default:
		return fmt.Errorf("%w: 路径 %v 处无法创建键", ErrRefuseSecondRoot, path)
	}
}

// yamlAppendPending 把缺失的尾部（rest）整段挂到插入点上：末段的值是 rawValue，
// 中间段是新键的子映射（flow 上下文里子映射也渲染成流式）。
func (s *YAMLSet) yamlAppendPending(cur yamlCursor, rest []string, rawValue []byte) error {
	site := cur.site
	if site == nil || cur.pends == nil {
		return fmt.Errorf("%w: 插入点缺失", ErrRefuseSecondRoot)
	}
	val, err := yamlPendingValue(rawValue, cur.pends.flow)
	if err != nil {
		return err
	}
	if cur.pends.find(rest[0]) != nil {
		// 定位说「缺失」、树里却已经有了：这是内部不一致，绝不能靠追加掩盖
		// （追加的正是重复键，YAML 会静默丢掉先写进去的值）。
		return fmt.Errorf("%w: 路径 %v 的首段 %q 已在本轮新建", ErrYAMLDuplicateKey, rest, rest[0])
	}
	tail := yamlPendVal{child: &yamlPendMap{flow: cur.pends.flow, entries: []*yamlPendKey{{key: rest[len(rest)-1], val: val}}}}
	for i := len(rest) - 2; i >= 0; i-- {
		if rest[i] == "" {
			return fmt.Errorf("%w: 路径段为空", ErrYAMLValueInvalid)
		}
		tail = yamlPendVal{child: &yamlPendMap{flow: cur.pends.flow, entries: []*yamlPendKey{{key: rest[i], val: tail}}}}
	}
	cur.pends.entries = append(cur.pends.entries, tail.child.entries[0])
	s.yamlSiteRebuild(site)
	return nil
}

// yamlApplyValue 处理「路径已存在」：替换值，不改动其它字节。
func (s *YAMLSet) yamlApplyValue(cur yamlCursor, rawValue []byte) error {
	flow := cur.site != nil && cur.site.flow
	if cur.node != nil {
		flow = yamlNodeIsFlow(s.src, cur.parent)
	}
	val, err := yamlPendingValue(rawValue, flow)
	if err != nil {
		return err
	}
	if cur.pendKey != nil {
		cur.pendKey.val = val
		s.yamlSiteRebuild(cur.site)
		return nil
	}
	n := cur.node
	if n == nil {
		return fmt.Errorf("%w: 路径不存在", ErrRefuseSecondRoot)
	}
	eol := detectLineEnding(s.src)
	unit := yamlIndentUnit(s.src, s.docs)
	if n.Kind == KNull {
		if val.block != nil {
			// 块值另起一行：插到父键那一行的换行之后，避免把行尾注释拽到子行上
			pos := lineEndAfter(s.src, lineContentEnd(s.src, n.Start))
			indent := append(append([]byte(nil), yamlLineIndentBytes(s.src, n.Start)...), unit...)
			return s.add(pos, pos, yamlIndentAllLines(val.block, indent, []byte(eol)))
		}
		repl := val.inline
		if repl[0] != ' ' && repl[0] != '\t' {
			repl = append([]byte{' '}, repl...)
		}
		return s.add(n.Start, n.End, repl)
	}
	if val.inline != nil {
		return s.add(n.Start, n.End, val.inline)
	}
	if flow {
		return fmt.Errorf("%w: 流式上下文只接受单行值", ErrYAMLValueInvalid)
	}
	if yamlIsOwnLine(s.src, n.Start) {
		indent := yamlLineIndentBytes(s.src, n.Start)
		return s.add(n.Start, n.End, yamlIndentAfterFirst(val.block, indent, eol))
	}
	// 与键同行：`key: - x` 不是合法 YAML，块必须另起一行（连同键后空白一起替换）
	start := n.Start
	for start > 0 && (s.src[start-1] == ' ' || s.src[start-1] == '\t') {
		start--
	}
	indent := append(append([]byte(nil), yamlLineIndentBytes(s.src, n.Start)...), unit...)
	body := append([]byte(eol), yamlIndentAllLines(val.block, indent, []byte(eol))...)
	return s.add(start, n.End, body)
}

// ── 删除 ────────────────────────────────────────────────────────────────────

// DeletePathInDoc 删除 path 处的成员（键值对整行/整段，含它的多行值）。
//
// 幂等：路径不存在（含「父级不是映射」这种本来就没有的形态）时无操作、不报错——
// 「没有要移除的托管条目」不是错误（UC-005 A2 的官方档语义）。
// 只删目标成员自己的行：相邻的空行与注释是用户的，一律不动。
// 序列元素不在本方法的职责内，用 DeleteFromSeqInDoc。
func (s *YAMLSet) DeletePathInDoc(docIndex int, path []string) error {
	if len(path) == 0 {
		return fmt.Errorf("%w: 空路径", ErrYAMLValueInvalid)
	}
	if len(s.docs) == 0 {
		return nil
	}
	cur, _, hit, err := s.yamlLocate(docIndex, path)
	if err != nil {
		if errors.Is(err, ErrRefuseSecondRoot) {
			return nil
		}
		return err
	}
	if !hit {
		return nil
	}
	if cur.pendKey != nil {
		cur.pendMap.remove(cur.pendKey.key)
		if len(cur.site.root.entries) == 0 {
			return s.yamlDropSite(cur.site.key)
		}
		s.yamlSiteRebuild(cur.site)
		return nil
	}
	if cur.member == nil || cur.parent == nil {
		return fmt.Errorf("%w: 根节点不可删除", ErrRefuseSecondRoot)
	}
	if yamlNodeIsFlow(s.src, cur.parent) {
		start, end := yamlFlowDeleteRange(s.src, cur.parent.Start, cur.member.Start, cur.member.End)
		return s.add(start, end, nil)
	}
	return s.add(lineStartAt(s.src, cur.member.Start), lineEndAfter(s.src, cur.member.End), nil)
}

// DeleteFromSeqInDoc 删除 path 处序列里**匹配谓词**的元素，返回删除个数。
//
// 幂等：路径不存在、不是序列、或没有元素匹配时返回 0 而不报错；
// 路径存在但值不是序列时返回 ErrYAMLNotASequence（那是调用方的路径写错了）。
// 全部元素都被删掉时整段重写成 `[]`——不留「键后面空无一物」这种会被读成 null 的形态。
func (s *YAMLSet) DeleteFromSeqInDoc(docIndex int, path []string, match func(*Node) bool) (int, error) {
	if len(path) == 0 {
		return 0, fmt.Errorf("%w: 空路径", ErrYAMLValueInvalid)
	}
	if match == nil {
		return 0, fmt.Errorf("%w: 缺少匹配谓词", ErrYAMLValueInvalid)
	}
	if len(s.docs) == 0 {
		return 0, nil
	}
	cur, _, hit, err := s.yamlLocate(docIndex, path)
	if err != nil {
		if errors.Is(err, ErrRefuseSecondRoot) {
			return 0, nil
		}
		return 0, err
	}
	if !hit {
		return 0, nil
	}
	if cur.pendKey != nil {
		return 0, fmt.Errorf("%w: 路径 %v 的序列来自本轮新建的块文本，无法按元素寻址",
			ErrYAMLNotASequence, path)
	}
	seq := cur.node
	if seq == nil || seq.Kind != KArray {
		return 0, fmt.Errorf("%w: 路径 %v 的值不是序列", ErrYAMLNotASequence, path)
	}
	var matched []*Node
	for _, e := range seq.Elems {
		if match(e) {
			matched = append(matched, e)
		}
	}
	if len(matched) == 0 {
		return 0, nil
	}
	if len(matched) == len(seq.Elems) {
		if err := s.add(seq.Start, seq.End, []byte("[]")); err != nil {
			return 0, err
		}
		return len(matched), nil
	}
	flow := yamlNodeIsFlow(s.src, seq)
	for _, e := range matched {
		var start, end int
		if flow {
			start, end = yamlFlowDeleteRange(s.src, seq.Start, e.Start, e.End)
		} else {
			start, end = yamlSeqItemLineStart(s.src, seq, e), lineEndAfter(s.src, e.End)
		}
		if err := s.add(start, end, nil); err != nil {
			return 0, err
		}
	}
	return len(matched), nil
}

// yamlFlowDeleteRange 返回删掉流式集合里一个成员应覆盖的区间：优先连它的分隔逗号
// 一起删（否则会留下尾随逗号或双逗号），只剩一个成员时按成员边界删。
func yamlFlowDeleteRange(src []byte, containerStart, start, end int) (int, int) {
	// 先找前导逗号：连它一起删能让剩下的成员自然衔接（`{"a": 1, "c": 3}`）。
	for j := start; j > containerStart; {
		j--
		switch src[j] {
		case ',':
			return j, end
		case '{', '[', '}', ']':
			j = containerStart // 到容器边界，转去看向后的逗号
		}
		if j <= containerStart {
			break
		}
	}
	// 首位成员：吃掉它自己的分隔逗号
	i := end
	for i < len(src) && (src[i] == ' ' || src[i] == '\t') {
		i++
	}
	if i < len(src) && src[i] == ',' {
		return start, i + 1
	}
	return start, end
}

// yamlSeqItemLineStart 返回块序列元素的破折号所在行首。
//
// 元素的起点不一定在破折号那一行：`-` 之后换行再写内容时（
// `-` 换行 `  name: x`），按元素起点回推会漏掉破折号行，留下一个孤零零的 `-`。
func yamlSeqItemLineStart(src []byte, seq *Node, e *Node) int {
	seqLineStart := lineStartAt(src, seq.Start)
	dashIndent := seq.Start - seqLineStart
	for pos := e.Start; ; {
		ls := lineStartAt(src, pos)
		c := skipSpaces(src, ls, len(src))
		if c == ls+dashIndent && c < len(src) && src[c] == '-' {
			return ls
		}
		if ls <= seqLineStart {
			break
		}
		pos = ls - 1
	}
	return lineStartAt(src, e.Start)
}

// ── 解析器 ──────────────────────────────────────────────────────────────────

type yamlParser struct {
	src   []byte
	lines []srcLine
	i     int // 当前行
	limit int // 文档末行（右开）
}

func (p *yamlParser) parseDoc(r yamlDocRange) (*Node, error) {
	p.i, p.limit = r.first, r.last
	n, err := p.parseBlock(0)
	if err != nil {
		return nil, err
	}
	p.skipBlank()
	if p.i < p.limit {
		return nil, fmt.Errorf("%w: 偏移 %d 处文档尾部有无法归属的内容（如多行纯量）",
			ErrYAMLParse, p.lines[p.i].start)
	}
	if n == nil {
		pos := len(p.src)
		if r.first < len(p.lines) {
			pos = p.lines[r.first].start
		}
		return &Node{Kind: KNull, Start: pos, End: pos, src: p.src}, nil
	}
	return n, nil
}

func (p *yamlParser) skipBlank() {
	for p.i < p.limit && p.lines[p.i].blank {
		p.i++
	}
}

// parseBlock 解析一个块级节点；缩进浅于 minIndent 时返回 (nil, nil) 表示「此处没有值」。
func (p *yamlParser) parseBlock(minIndent int) (*Node, error) {
	p.skipBlank()
	if p.i >= p.limit {
		return nil, nil
	}
	ln := p.lines[p.i]
	if ln.content >= ln.end || ln.indent < minIndent {
		return nil, nil
	}
	if yamlIsSeqEntry(p.src, ln) {
		return p.parseSeq(ln.indent)
	}
	_, _, _, ok, err := p.probeEntry(ln, ln.content)
	if err != nil {
		return nil, err
	}
	if ok {
		return p.parseMapping(ln.content)
	}
	return p.parseInlineValue(ln, ln.content, ln.indent-1)
}

// parseSeq 解析一个块级序列（所有 `- ` 条目的缩进都等于 seqIndent）。
func (p *yamlParser) parseSeq(seqIndent int) (*Node, error) {
	n := &Node{Kind: KArray, Start: p.lines[p.i].content, src: p.src}
	for {
		p.skipBlank()
		if p.i >= p.limit {
			break
		}
		ln := p.lines[p.i]
		if ln.indent != seqIndent || !yamlIsSeqEntry(p.src, ln) {
			break
		}
		after := skipSpaces(p.src, ln.content+1, ln.end)
		if after >= ln.end || p.src[after] == '#' {
			// 条目内容在下一行的更深缩进处。
			p.i++
			child, err := p.parseBlock(seqIndent + 1)
			if err != nil {
				return nil, err
			}
			if child == nil {
				child = &Node{Kind: KNull, Start: ln.content + 1, End: ln.content + 1, src: p.src}
			}
			n.Elems = append(n.Elems, child)
			n.End = child.End
			continue
		}
		_, _, _, ok, err := p.probeEntry(ln, after)
		if err != nil {
			return nil, err
		}
		var item *Node
		if ok {
			// `- name: x` 形态：内联映射，后续条目与首个键同列。
			item, err = p.parseMapping(after)
			if err != nil {
				return nil, err
			}
		} else {
			item, err = p.parseInlineValue(ln, after, seqIndent)
			if err != nil {
				return nil, err
			}
		}
		n.Elems = append(n.Elems, item)
		n.End = item.End
	}
	if len(n.Elems) == 0 {
		return nil, nil
	}
	return n, nil
}

// parseMapping 解析一个块级映射：首个键在**绝对偏移** keyCol 处，后续键必须与它同列。
//
// keyCol 用绝对偏移而不是缩进，是因为它有两个来源：块级映射（= 行内容起点）与
// `- name: x` 这种序列内的内联映射（= 破折号之后的列）。两者只有绝对偏移能统一，
// 缩进值统一不了——把缩进当偏移用会去索引行首空白，probeEntry 必然落空。
func (p *yamlParser) parseMapping(keyCol int) (*Node, error) {
	first := true
	baseIndent := keyCol - p.lines[p.i].start
	m := &Node{Kind: KObject, Start: keyCol, src: p.src}
	for {
		if !first {
			p.skipBlank()
			if p.i >= p.limit {
				break
			}
			ln := p.lines[p.i]
			if ln.content >= ln.end || ln.indent != baseIndent || yamlIsSeqEntry(p.src, ln) {
				break
			}
		}
		ln := p.lines[p.i]
		col := ln.content
		if first {
			col = keyCol
		}
		key, keyRaw, colon, ok, err := p.probeEntry(ln, col)
		if err != nil {
			return nil, err
		}
		if !ok {
			if first {
				return nil, fmt.Errorf("%w: 偏移 %d 处不是映射条目", ErrYAMLParse, ln.start+col)
			}
			break
		}
		valPos := skipSpaces(p.src, colon+1, ln.end)
		var val *Node
		if valPos >= ln.end || p.src[valPos] == '#' {
			// 值在下方；没有则记为空值。
			p.i++
			child, err := p.parseChildBlock(baseIndent)
			if err != nil {
				return nil, err
			}
			if child == nil {
				val = &Node{Kind: KNull, Start: colon + 1, End: colon + 1, src: p.src}
			} else {
				val = child
			}
		} else {
			v, err := p.parseInlineValue(ln, valPos, baseIndent)
			if err != nil {
				return nil, err
			}
			val = v
		}
		m.Members = append(m.Members, Member{
			Key:    key,
			KeyRaw: keyRaw,
			Val:    val,
			Start:  col,
			End:    val.End,
		})
		m.End = val.End
		first = false
	}
	if len(m.Members) == 0 {
		return nil, nil
	}
	return m, nil
}

// probeEntry 判断 (ln, col) 处是否是一个 `key:` 映射条目；返回键文本、键记号、
// 冒号位置。不推进解析状态。
func (p *yamlParser) probeEntry(ln srcLine, col int) (string, []byte, int, bool, error) {
	if col >= ln.end {
		return "", nil, 0, false, nil
	}
	switch c := p.src[col]; c {
	case '"', '\'':
		n, end, err := p.scanQuoted(col)
		if err != nil {
			return "", nil, 0, false, err
		}
		if j := skipSpaces(p.src, end, ln.end); j < ln.end && p.src[j] == ':' {
			return n.Str, p.src[col:end], j, true, nil
		}
		return "", nil, 0, false, nil
	case '{', '[':
		// 流式集合：本行不是块级映射条目（根为 flow 的文件就走这条路）。
		return "", nil, 0, false, nil
	case '?', '&', '*', '!', '%', '@', '`':
		return "", nil, 0, false, fmt.Errorf("%w: 偏移 %d 处 %q 形态不在支持范围", ErrYAMLUnsupported, col, c)
	}
	for j := col; j < ln.end; j++ {
		if p.src[j] == '#' && (j == col || p.src[j-1] == ' ' || p.src[j-1] == '\t') {
			return "", nil, 0, false, nil // 注释先出现，本行没有键
		}
		if p.src[j] != ':' {
			continue
		}
		if j+1 < ln.end && p.src[j+1] != ' ' && p.src[j+1] != '\t' {
			continue // `http://x` 这类冒号不是映射分隔符
		}
		key := strings.TrimRight(string(p.src[col:j]), " \t")
		if key == "" {
			return "", nil, 0, false, nil
		}
		return key, p.src[col:j], j, true, nil
	}
	return "", nil, 0, false, nil
}

// parseChildBlock 解析键下方缩进的子块。
//
// 序列允许与键同缩进（`a:` 换行后直接 `- x` 是 YAML 的常见手写形态），
// 映射必须更深一层——两者混同会让 `a:` + `- x` 这种文件被判成坏文件，
// 而它其实完全合法。
func (p *yamlParser) parseChildBlock(baseIndent int) (*Node, error) {
	p.skipBlank()
	if p.i < p.limit {
		ln := p.lines[p.i]
		if ln.content < ln.end && ln.indent == baseIndent && yamlIsSeqEntry(p.src, ln) {
			return p.parseSeq(baseIndent)
		}
	}
	return p.parseBlock(baseIndent + 1)
}

// parseInlineValue 解析与键同行的值：流式集合、引号字符串、块标量或纯量。
func (p *yamlParser) parseInlineValue(ln srcLine, pos, parentIndent int) (*Node, error) {
	switch c := p.src[pos]; c {
	case '{', '[':
		n, end, err := p.flow(pos)
		if err != nil {
			return nil, err
		}
		if rest := skipSpaces(p.src, end, ln.end); rest < ln.end && p.src[rest] != '#' {
			return nil, fmt.Errorf("%w: 偏移 %d 处流式集合后有额外内容", ErrYAMLParse, rest)
		}
		p.advancePast(end)
		return n, nil
	case '"', '\'':
		n, end, err := p.scanQuoted(pos)
		if err != nil {
			return nil, err
		}
		if rest := skipSpaces(p.src, end, ln.end); rest < ln.end && p.src[rest] != '#' {
			return nil, fmt.Errorf("%w: 偏移 %d 处字符串后有额外内容", ErrYAMLParse, rest)
		}
		p.i++
		return n, nil
	case '|', '>':
		return p.parseBlockScalar(ln, pos, parentIndent)
	case '&', '*', '!', '@', '`':
		return nil, fmt.Errorf("%w: 偏移 %d 处 %q 形态不在支持范围", ErrYAMLUnsupported, pos, c)
	}
	end := plainScalarEnd(p.src, ln, pos)
	if end <= pos {
		return nil, fmt.Errorf("%w: 偏移 %d 处缺少值", ErrYAMLParse, pos)
	}
	p.i++
	return &Node{Kind: KString, Start: pos, End: end, Str: string(p.src[pos:end]), src: p.src}, nil
}

// parseBlockScalar 解析 `|` / `>` 块标量：节点的区间覆盖指示符与其内容行，
// 这样调用方替换整个节点时不会留下悬空的内容行。
func (p *yamlParser) parseBlockScalar(ln srcLine, pos, parentIndent int) (*Node, error) {
	j := pos + 1
	for j < ln.end {
		c := p.src[j]
		if c == '+' || c == '-' || (c >= '1' && c <= '9') {
			j++
			continue
		}
		break
	}
	if rest := skipSpaces(p.src, j, ln.end); rest < ln.end && p.src[rest] != '#' {
		return nil, fmt.Errorf("%w: 偏移 %d 处块标量指示符后有额外内容", ErrYAMLParse, rest)
	}
	p.i++
	end := pos
	for p.i < p.limit {
		l := p.lines[p.i]
		if l.content >= l.end { // 空行属于块内容
			p.i++
			continue
		}
		if l.indent <= parentIndent {
			break
		}
		end = l.end
		p.i++
	}
	if end <= pos {
		end = ln.end
	}
	return &Node{Kind: KString, Start: pos, End: end, Str: string(p.src[pos:end]), src: p.src}, nil
}

// advancePast 把行游标推进到 end 所在行之后。
func (p *yamlParser) advancePast(end int) {
	for p.i < p.limit && p.lines[p.i].end < end {
		p.i++
	}
	p.i++
}

// ── 流式集合 ────────────────────────────────────────────────────────────────

// flow 从 pos 起解析一个流式节点，返回节点与其结束偏移。
func (p *yamlParser) flow(pos int) (*Node, int, error) {
	i := p.skipFlowSpace(pos)
	if i >= len(p.src) {
		return nil, i, fmt.Errorf("%w: 偏移 %d 处缺少流式节点", ErrYAMLParse, pos)
	}
	switch c := p.src[i]; c {
	case '{':
		return p.flowMapping(i)
	case '[':
		return p.flowSequence(i)
	case '"', '\'':
		return p.scanQuoted(i)
	case '&', '*', '!', '@', '`':
		return nil, i, fmt.Errorf("%w: 偏移 %d 处 %q 形态不在支持范围", ErrYAMLUnsupported, i, c)
	}
	end := p.flowPlain(i, false)
	if end <= i {
		return nil, i, fmt.Errorf("%w: 偏移 %d 处流式纯量为空", ErrYAMLParse, i)
	}
	return &Node{Kind: KString, Start: i, End: end, Str: string(p.src[i:end]), src: p.src}, end, nil
}

func (p *yamlParser) flowMapping(pos int) (*Node, int, error) {
	n := &Node{Kind: KObject, Start: pos, src: p.src}
	i := pos + 1
	for {
		i = p.skipFlowSpace(i)
		if i >= len(p.src) {
			return nil, i, fmt.Errorf("%w: 偏移 %d 处流式映射未闭合", ErrYAMLParse, pos)
		}
		if p.src[i] == '}' {
			n.End = i + 1
			return n, i + 1, nil
		}
		keyStart := i
		var key string
		if p.src[i] == '"' || p.src[i] == '\'' {
			kn, end, err := p.scanQuoted(i)
			if err != nil {
				return nil, i, err
			}
			key, i = kn.Str, end
		} else {
			end := p.flowPlain(i, true)
			if end <= i {
				return nil, i, fmt.Errorf("%w: 偏移 %d 处流式映射缺少键", ErrYAMLParse, i)
			}
			key, i = string(p.src[i:end]), end
		}
		keyRaw := p.src[keyStart:i]
		i = p.skipFlowSpace(i)
		if i >= len(p.src) || p.src[i] != ':' {
			return nil, i, fmt.Errorf("%w: 偏移 %d 处流式映射期望 ':'", ErrYAMLParse, i)
		}
		i++
		val, end, err := p.flow(i)
		if err != nil {
			return nil, i, err
		}
		n.Members = append(n.Members, Member{Key: key, KeyRaw: keyRaw, Val: val, Start: keyStart, End: val.End})
		n.End = val.End
		i = p.skipFlowSpace(end)
		if i >= len(p.src) {
			return nil, i, fmt.Errorf("%w: 偏移 %d 处流式映射未闭合", ErrYAMLParse, pos)
		}
		switch p.src[i] {
		case ',':
			i++
		case '}':
			n.End = i + 1
			return n, i + 1, nil
		default:
			return nil, i, fmt.Errorf("%w: 偏移 %d 处期望 ',' 或 '}'", ErrYAMLParse, i)
		}
	}
}

func (p *yamlParser) flowSequence(pos int) (*Node, int, error) {
	n := &Node{Kind: KArray, Start: pos, src: p.src}
	i := pos + 1
	for {
		i = p.skipFlowSpace(i)
		if i >= len(p.src) {
			return nil, i, fmt.Errorf("%w: 偏移 %d 处流式序列未闭合", ErrYAMLParse, pos)
		}
		if p.src[i] == ']' {
			n.End = i + 1
			return n, i + 1, nil
		}
		elem, end, err := p.flow(i)
		if err != nil {
			return nil, i, err
		}
		n.Elems = append(n.Elems, elem)
		n.End = elem.End
		i = p.skipFlowSpace(end)
		if i >= len(p.src) {
			return nil, i, fmt.Errorf("%w: 偏移 %d 处流式序列未闭合", ErrYAMLParse, pos)
		}
		switch p.src[i] {
		case ',':
			i++
		case ']':
			n.End = i + 1
			return n, i + 1, nil
		default:
			return nil, i, fmt.Errorf("%w: 偏移 %d 处期望 ',' 或 ']'", ErrYAMLParse, i)
		}
	}
}

// skipFlowSpace 跳过流式上下文里的空白与注释。
func (p *yamlParser) skipFlowSpace(i int) int {
	for i < len(p.src) {
		switch p.src[i] {
		case ' ', '\t', '\n', '\r':
			i++
		case '#':
			for i < len(p.src) && p.src[i] != '\n' {
				i++
			}
		default:
			return i
		}
	}
	return i
}

// flowPlain 扫描一个流式纯量。stopAtColonSpace 为真时（键的用法）把
// 「冒号 + 空白/分隔符」也当成终结符。
func (p *yamlParser) flowPlain(i int, stopAtColonSpace bool) int {
	start := i
	for i < len(p.src) {
		c := p.src[i]
		if c == ',' || c == '}' || c == ']' || c == '\n' {
			break
		}
		if c == '#' && (i == start || p.src[i-1] == ' ' || p.src[i-1] == '\t') {
			break
		}
		if stopAtColonSpace && c == ':' {
			next := byte(0)
			if i+1 < len(p.src) {
				next = p.src[i+1]
			}
			if next == 0 || next == ' ' || next == '\t' || next == '\n' || next == ',' || next == '}' || next == ']' {
				break
			}
		}
		i++
	}
	end := i
	for end > start && (p.src[end-1] == ' ' || p.src[end-1] == '\t') {
		end--
	}
	return end
}

// ── 引号字符串与纯量 ────────────────────────────────────────────────────────

// scanQuoted 解析一个引号字符串（单行；YAML 的多行引号字符串不在支持范围）。
func (p *yamlParser) scanQuoted(pos int) (*Node, int, error) {
	q := p.src[pos]
	i := pos + 1
	var sb strings.Builder
	for i < len(p.src) {
		c := p.src[i]
		switch {
		case c == '\n':
			return nil, i, fmt.Errorf("%w: 偏移 %d 处引号字符串跨行", ErrYAMLUnsupported, pos)
		case q == '\'' && c == '\'':
			if i+1 < len(p.src) && p.src[i+1] == '\'' {
				sb.WriteByte('\'')
				i += 2
				continue
			}
			i++
			return &Node{Kind: KString, Start: pos, End: i, Str: sb.String(), src: p.src}, i, nil
		case q == '"' && c == '\\':
			r, n, err := decodeYAMLEscape(p.src, i)
			if err != nil {
				return nil, i, err
			}
			sb.WriteRune(r)
			i = n
		case c == q:
			i++
			return &Node{Kind: KString, Start: pos, End: i, Str: sb.String(), src: p.src}, i, nil
		default:
			sb.WriteByte(c)
			i++
		}
	}
	return nil, i, fmt.Errorf("%w: 偏移 %d 处引号字符串未闭合", ErrYAMLParse, pos)
}

func decodeYAMLEscape(src []byte, i int) (rune, int, error) {
	if i+1 >= len(src) {
		return 0, i, fmt.Errorf("%w: 转义未结束", ErrYAMLParse)
	}
	switch e := src[i+1]; e {
	case '0':
		return 0, i + 2, nil
	case 'a':
		return '\a', i + 2, nil
	case 'b':
		return '\b', i + 2, nil
	case 't':
		return '\t', i + 2, nil
	case 'n':
		return '\n', i + 2, nil
	case 'v':
		return '\v', i + 2, nil
	case 'f':
		return '\f', i + 2, nil
	case 'r':
		return '\r', i + 2, nil
	case 'e':
		return 0x1b, i + 2, nil
	case '"':
		return '"', i + 2, nil
	case '\\':
		return '\\', i + 2, nil
	case '/':
		return '/', i + 2, nil
	case 'x', 'u', 'U':
		width := 2
		switch e {
		case 'u':
			width = 4
		case 'U':
			width = 8
		}
		if i+2+width > len(src) {
			return 0, i, fmt.Errorf("%w: \\%c 转义不完整", ErrYAMLParse, e)
		}
		v, err := strconv.ParseUint(string(src[i+2:i+2+width]), 16, 32)
		if err != nil {
			return 0, i, fmt.Errorf("%w: \\%c 转义非法", ErrYAMLParse, e)
		}
		return rune(v), i + 2 + width, nil
	}
	return 0, i, fmt.Errorf("%w: 未知转义 \\%c", ErrYAMLParse, src[i+1])
}

// plainScalarEnd 返回纯量在行内的结束偏移（剥掉行尾空白与注释）。
func plainScalarEnd(src []byte, ln srcLine, pos int) int {
	end := ln.end
	for k := pos; k < ln.end; k++ {
		if src[k] == '#' && (k == pos || src[k-1] == ' ' || src[k-1] == '\t') {
			end = k
			break
		}
	}
	for end > pos && (src[end-1] == ' ' || src[end-1] == '\t') {
		end--
	}
	return end
}

func yamlIsSeqEntry(src []byte, ln srcLine) bool {
	if ln.content >= ln.end || src[ln.content] != '-' {
		return false
	}
	return ln.content+1 >= ln.end || src[ln.content+1] == ' ' || src[ln.content+1] == '\t'
}

// validateYAMLRawValue 校验待写入的值：单行、且本身是一个合法 YAML 值记号。
//
// 为什么要校验而不是照抄字节：把 `a, b` 或带换行的内容塞进块级位置会产出
// 解析不了的 YAML——本产品的硬约束是「写完的文件必须仍可被解析器读懂」。
func validateYAMLRawValue(raw []byte) error {
	if len(raw) == 0 {
		return fmt.Errorf("%w: 空值", ErrYAMLValueInvalid)
	}
	if bytes.IndexByte(raw, '\n') >= 0 || bytes.IndexByte(raw, '\r') >= 0 {
		return fmt.Errorf("%w: 值含换行；本层不做重排/重缩进", ErrYAMLValueInvalid)
	}
	if raw[0] == '"' || raw[0] == '\'' || raw[0] == '{' || raw[0] == '[' {
		// 引号字符串与流式集合：形态由流式解析器判定。
	} else {
		// 纯量：`key: x: y` / `key: - x` 这类形态在块级位置上会被读成别的东西，
		// 写下去就是坏文件，所以宁可在这里拒绝。
		if bytes.Contains(raw, []byte(": ")) || bytes.HasSuffix(raw, []byte(":")) {
			return fmt.Errorf("%w: 纯量含映射分隔符（%q）", ErrYAMLValueInvalid, raw)
		}
		if bytes.HasPrefix(raw, []byte("- ")) || bytes.HasPrefix(raw, []byte("? ")) {
			return fmt.Errorf("%w: 纯量以 %q 开头会被读成块结构指示符", ErrYAMLValueInvalid, raw[:2])
		}
	}
	p := &yamlParser{src: raw}
	_, end, err := p.flow(0)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrYAMLValueInvalid, err)
	}
	if rest := skipSpaces(raw, end, len(raw)); rest != len(raw) {
		return fmt.Errorf("%w: 值后有额外字节 %q", ErrYAMLValueInvalid, raw[rest:])
	}
	return nil
}
