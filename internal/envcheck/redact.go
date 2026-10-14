package envcheck

import (
	"sort"
	"strings"
)

// 整行级脱敏（UC-015 修订版 A4「凭据值替换为掩码」/ A13「响应正文不含任何令牌
// 明文」的唯一实现）。
//
// 为什么必须整行级：行文本是「用户能不能看懂要删哪一行」的主要线索，但它整行带值。
// 旧实现只掩「本条目自身的值」——同一物理行上任何别的凭据（非决定键赋值、
// 行内注释里的令牌）都会原样进入 /env/conflicts 响应、/env/repair/preview 的
// deleteLines、UI 展示与「复制行原文」按钮；2026-09-27 补充审计在 HEAD 快照上
// 实测复现（判定不敏感的决定键条目，其 lineText 携带同行兄弟凭据的明文）。
// 修法：脱敏不再以「本条目的值」为界，而是**按行独立判定**：
//
//  1. 每个 KEY=VALUE 赋值的「值」：名敏感（IsSensitiveName）或值形如凭据
//     （LooksLikeSecret）→ 掩；本条目自身的值在决定键声明 Sensitive 时也掩。
//     相邻引号段按 shell 词法拼成同一个 token 再判定（`"短段"'长段'` 是一个
//     值，见 shellJoinedValue）。键名、引号与整行结构一律保留（A4 的
//     「保留键名与结构」）。
//  2. 行内注释与其它自由文本里的**前缀形态令牌**（sk-/tp-/bt-/ghp_… 见
//     secretValuePrefixes；前缀后跟 ≥minTokenTail 的记号）→ 掩。掩码从前缀
//     一路盖到**硬边界**（空白/引号，见 IsHardBoundary）：令牌后紧跟 `.`、
//     `。`、CJK、`*` 等字符时同样整段掩掉（2026-09-27 独立核验判红的残留：
//     旧实现按小边界集合判定，这些形态整段不脱敏）。
//  3. ≥minTokenRun 的连续 hex/base64 形状（无同字符连跑）→ 掩。
//
// 但同样**不得过掩**（A4 的可读性一面）：路径、域名、`true`/`1` 这类非秘密值、
// 无凭据段的 URL（https://old.example）、主机名（https://sk-gateway.…，前缀后的
// 记号太短）、长填充（AAAA…）都必须原样保留。令牌与注释文字紧贴时取「宁可多掩」
// 的选边：紧跟令牌的标点/CJK 一并进掩码（掩码只影响展示，不参与删行判定，见下）。
//
// 单源要求：同一函数供扫描的 LineText（scan.go 的 conflicts()）与修复预览的
// deleteLines（repair.go 的 planDeletions）共用——两处调用点传同一组参数
// （条目自身的值与敏感判定），行内其余内容的判定与调用点无关。删行判定用的是
// 文件字节与位置区间，不依赖这里，因此脱敏不会影响修复正确性（A5）。

const (
	// minTokenTail 是前缀形态令牌里「前缀之后还算记号」的最小长度：与 UC-003 A12
	// 的令牌形态（sk-[A-Za-z0-9_-]{16,}）及 api/diff.go 的同名常量同口径。
	// 取更短会把 sk-gateway 这类主机名、sk-1 这类短值误判成令牌。
	minTokenTail = 16
	// minTokenRun 是「连续 hex/base64 形状」判定为令牌的最小长度：40 是 SHA-1
	// 摘要的长度，远长于 rc 文件里正常出现的单词与路径段。
	minTokenRun = 40
	// tokenRunRepeatLimit 是长跑判定的同字符连跑豁免阈值：连续 8 个相同字符的
	// 长串是填充而不是令牌（与 LooksLikeSecret 里长度启发式的豁免同口径）。
	tokenRunRepeatLimit = 8
)

// redactLine 把一行 shell 文本脱敏成可以进响应体的形态。
//
// ownValue / ownSensitive 是本条目自身的信息（决定键的值与 sensitiveFor 的结论）；
// 行内其余内容**按行独立判定**——这正是修掉「同行兄弟凭据外泄」的关键：判定不
// 取决于触发这条响应的是哪个条目，因此同一物理行无论从哪个条目看都得到同一结果。
func redactLine(line, ownValue string, ownSensitive bool) string {
	if line == "" {
		return ""
	}
	out := maskAssignmentValues(line, ownValue, ownSensitive)
	out = maskTokenShapesLine(out)
	// 兼容口径：本条目自身的值可能在注释等自由文本里再次出现（旧实现是整行
	// 字符串替换）。长于前缀阈值的记号已由形态扫描覆盖，短记号由这一步兜住；
	// 自身值不敏感时不掩（非秘密值保留可读性）。
	if ownSensitive && len(ownValue) >= minRedactLen {
		out = strings.ReplaceAll(out, ownValue, MaskedValue)
	}
	return out
}

// maskFieldValue 是 §9 DTO 值字段（effectiveValue / expectedValue）的脱敏入口。
//
// 2026-09-27 A3-fix 复核判红（唯一项）：这两个字段此前只吃 sensitive 布尔
// （maskIf），决定键名不敏感而值呈「引号拼接」或「含空白」时——如
// `export ANTHROPIC_BASE_URL='ghp_'"<tail>"` 或 `"ghp_<tail> trailing text"`
// ——effectiveValue 原样回显令牌明文，而同一响应的兄弟字段 lineText 走
// redactLine 已掩。修法：sensitive=false 时也过**令牌形态**掩码，且与整行脱敏
// 共用同一套面（同前缀表 / 同 IsHardBoundary / 同长跑规则，单源在 redact.go）：
//
//  1. 先按 shell 词法拼接（shellJoinedValue，单源）再判凭据位：`'ghp_'"<tail>"`
//     这类相邻引号段的拼接形态由此面处理，不得另写一套（拼接点在前缀后、拼接点
//     在记号中段的写法，都只有拼接成实际取值后才可判）。判出即整值掩，与整行
//     脱敏的取边一致（valueReplaceSpan 对不成对引号整体替换）。
//  2. 拼接值不是凭据位时过形态扫描（maskTokenShapesLine，即 MaskTokenShapes 的
//     实现核心）：把嵌在值里的令牌段（`ghp_<tail> trailing text`）掩掉，其余
//     片段原样保留。
//
// sensitive=true 时维持整值 MaskedValue 语义不变（一个值就是一枚凭据）。
// 非令牌形态的值一律原样保留（A4 可读性一面）：他人端点 URL、含空格的普通文案、
// 主机名（前缀后记号太短）——掩少了是外泄，掩多了是可用性回退。
func maskFieldValue(name, value string, sensitive bool) string {
	if sensitive {
		return MaskedValue
	}
	if value == "" {
		return value
	}
	// 与 maskAssignmentValues 同一判定序列：先按归一值判凭据位，判不出再按
	// shell 词法拼接后判（拼接形态的「归一值」取到的是带引号原文，判不出凭据位）。
	if assignmentValueIsCredential(name, value, value, false) {
		return MaskedValue
	}
	if joined := shellJoinedValue([]byte(value)); joined != "" && joined != value &&
		assignmentValueIsCredential(name, joined, value, false) {
		return MaskedValue
	}
	return maskTokenShapesLine(value)
}

// maskAssignmentValues 把每一个承凭据的 KEY=VALUE 赋值的「值」换成掩码。
//
// 只替换值本身：键名、两侧引号、空白与分隔结构都原样保留（A4）；掩与不掩的判定
// 逐赋值独立进行，与「哪条赋值触发了这次响应」无关。
func maskAssignmentValues(line, ownValue string, ownSensitive bool) string {
	body := []byte(line)
	spans := parseAssignmentSpans(body)
	if len(spans) == 0 {
		return line
	}
	out := body
	changed := false
	// 从后往前替换：后面的替换不会让前面的位置区间失效。
	for i := len(spans) - 1; i >= 0; i-- {
		s := spans[i]
		raw := body[s.eq+1 : s.tokEnd]
		norm := normalizeValue(raw)
		cred := norm != "" && assignmentValueIsCredential(s.name, norm, ownValue, ownSensitive)
		if !cred {
			// shell 拼接形态（`"短段"'长段'`：相邻引号段是同一个 token）：
			// 「剥一层配对引号」在拼接形态上取到的是带引号原文、判不出凭据位，
			// 须按 shell 词法的拼接值再判一次（2026-09-27 核验判红②：两段都
			// 残留）。简单引号值的两个取值相等，这里自动跳过、不重复判定。
			joined := shellJoinedValue(raw)
			cred = joined != "" && joined != norm &&
				assignmentValueIsCredential(s.name, joined, ownValue, ownSensitive)
		}
		if !cred {
			continue
		}
		lo, hi := valueReplaceSpan(body, s)
		if lo < 0 {
			continue
		}
		if !changed {
			out = append([]byte(nil), body...)
			changed = true
		}
		masked := make([]byte, 0, len(out)-(hi-lo)+len(MaskedValue))
		masked = append(masked, out[:lo]...)
		masked = append(masked, MaskedValue...)
		masked = append(masked, out[hi:]...)
		out = masked
	}
	if !changed {
		return line
	}
	return string(out)
}

// assignmentValueIsCredential 判定一个赋值的值是否要掩。
//
// 三条判据与 sensitiveFor 同源（名敏感 / 值形如凭据），另加本条目自身值这一条
// 覆盖「决定键显式声明 Sensitive 但名与值都看不出凭据位」的写法。值参数既可能
// 是 normalizeValue 的结果，也可能是 shellJoinedValue 的拼接结果（拼接形态）。
func assignmentValueIsCredential(name, norm, ownValue string, ownSensitive bool) bool {
	if ownSensitive && len(ownValue) >= minRedactLen && norm == ownValue {
		return true
	}
	if IsSensitiveName(name) {
		return true
	}
	return LooksLikeSecret(norm)
}

// shellJoinedValue 按 shell 词法把值字面量拼接成**实际取值**：相邻引号段直接
// 相连（`"a"'b'` 的取值是 `ab`），引号只作分段、内容原样；`\` 按转义吞一个
// 字符（双引号内是常见转义；单引号内 `\` 本是字面量，这里从简按转义处理——
// 判定是启发式，误差方向取「宁可多掩」）。
//
// 为什么需要：`"短段"'长段'` 是 shell 合法写法，两个引号段是**同一个 token**。
// normalizeValue（首尾同引号才剥一层）在拼接形态上取到的是带引号原文，
// LooksLikeSecret 判否 ⇒ 两段都残留（2026-09-27 核验判红②）。
func shellJoinedValue(raw []byte) string {
	var b strings.Builder
	b.Grow(len(raw))
	i := 0
	for i < len(raw) && (raw[i] == ' ' || raw[i] == '\t') {
		i++
	}
	end := len(raw)
	for end > i && (raw[end-1] == ' ' || raw[end-1] == '\t') {
		end--
	}
	for i < end {
		switch raw[i] {
		case '\'':
			i++
			for i < end && raw[i] != '\'' {
				b.WriteByte(raw[i])
				i++
			}
			i++ // 收尾引号；未闭合时越过末尾，循环自然结束
		case '"':
			i++
			for i < end && raw[i] != '"' {
				if raw[i] == '\\' && i+1 < end {
					i++ // 吞掉反斜杠，取被转义字符
				}
				b.WriteByte(raw[i])
				i++
			}
			i++ // 收尾引号
		case '\\':
			if i+1 < end {
				b.WriteByte(raw[i+1])
				i += 2
				continue
			}
			i++
		default:
			b.WriteByte(raw[i])
			i++
		}
	}
	return b.String()
}

// valueReplaceSpan 返回值在行内的「该掩的区间」：引号保留在掩码之外
// （`NAME='<已脱敏>'`），`NAME= "v"` 这类写法里的空白也保留。返回 (-1, -1)
// 表示没有可掩的核心（空值或只剩引号）。
//
// 特例（选边固定）：首尾引号**不成对**（拼接形态 `"短段"'长段'`）时不做剥离，
// 整个值字面量（含引号）一起换成掩码——核验判红②的修法要求「确保整体被掩」。
func valueReplaceSpan(body []byte, s assignmentSpan) (int, int) {
	raw := body[s.eq+1 : s.tokEnd]
	lo, hi := 0, len(raw)
	for lo < hi && (raw[lo] == ' ' || raw[lo] == '\t') {
		lo++
	}
	for hi > lo && (raw[hi-1] == ' ' || raw[hi-1] == '\t') {
		hi--
	}
	if hi-lo >= 2 {
		switch raw[lo] {
		case '"', '\'':
			if raw[hi-1] == raw[lo] {
				lo++
				hi--
			}
		}
	}
	if lo >= hi {
		return -1, -1
	}
	return s.eq + 1 + lo, s.eq + 1 + hi
}

// ── 行内形态扫描（注释与自由文本里的令牌） ──────────────────────────────────

// MaskTokenShapes 是令牌形态掩码对外的**单源入口**：把一段自由文本（错误消息、
// 细节字段等非 shell 行文本）里所有「前缀形态令牌 + 长 hex/base64 跑」替换为
// MaskedValue。判据与整行脱敏（redactLine）逐字同源——同一条前缀表
// （secretValuePrefixes）、同一条边界判据（IsHardBoundary）、同一套长跑规则，
// 调用方不得另抄一份（抄的那份迟早在 `.`/`。`/CJK/`*` 这类边界形态上漂移）。
//
// 调用方约定（2026-09-27，UC-014 A11 响应面收口）：api 层的失败信封装配
// （failEnvelope）用它兜住「任何族、任何错误分支」的 message/details——令牌
// 明文不许出现在响应正文；掩码只替换令牌形态的区间，路径、偏移、原因等
// 诊断文本原样保留。
func MaskTokenShapes(text string) string { return maskTokenShapesLine(text) }

// RedactAssignment 把一条「命中决定键的赋值」脱敏成工具维度可展示的
// （实际值, 期望值）形态（UC-100 A3 的「环境覆盖 → 实际请求端点 ≠ 盘上」并列素材）。
//
// 单源纪律：与 §9 冲突 DTO 的值字段**同一实现**（sensitiveFor 判定 ×
// maskFieldValue 掩码），调用方不得另抄一套——两处各判一次迟早出现
// 「冲突列表里掩了、工具维度里没掩」的外泄面。
// 返回的 sensitive 供调用方决定是否附加「该值已脱敏」的呈现标注。
func RedactAssignment(a Assignment) (observed, expected string, sensitive bool) {
	sensitive = sensitiveFor(a.Name, a.Value, a.Key)
	return maskFieldValue(a.Name, a.Value, sensitive),
		maskFieldValue(a.Name, a.Key.Value, sensitive),
		sensitive
}

// maskTokenShapesLine 把行内的前缀形态令牌与长 hex/base64 形状换成掩码。
//
// 键名区域被刻意排除：A4 要求保留键名与结构，而合法的变量名可以长过 40 字节
// （超长 CLAUDE_CODE_… 配置名），长跑规则必须给键名让路。
func maskTokenShapesLine(text string) string {
	spans := tokenShapeSpans(text)
	if len(spans) == 0 {
		return text
	}
	var b strings.Builder
	b.Grow(len(text))
	prev := 0
	for _, sp := range spans {
		b.WriteString(text[prev:sp.start])
		b.WriteString(MaskedValue)
		prev = sp.end
	}
	b.WriteString(text[prev:])
	return b.String()
}

// tokenShapeSpan 是行内一次形态脱敏的字节区间 [start, end)。
type tokenShapeSpan struct{ start, end int }

// tokenShapeSpans 收集行内所有需要按形态掩掉的区间：前缀形态 + 长跑，
// 已排序、已合并重叠、已剔除与键名区域相交者。
func tokenShapeSpans(text string) []tokenShapeSpan {
	var spans []tokenShapeSpan
	spans = append(spans, prefixTokenSpans(text)...)
	spans = append(spans, longRunSpans(text)...)
	if len(spans) == 0 {
		return nil
	}
	sort.Slice(spans, func(i, j int) bool {
		if spans[i].start != spans[j].start {
			return spans[i].start < spans[j].start
		}
		return spans[i].end > spans[j].end
	})
	merged := make([]tokenShapeSpan, 0, len(spans))
	for _, sp := range spans {
		if n := len(merged); n > 0 && sp.start <= merged[n-1].end {
			if sp.end > merged[n-1].end {
				merged[n-1].end = sp.end
			}
			continue
		}
		merged = append(merged, sp)
	}
	names := assignmentNameRanges(text)
	out := make([]tokenShapeSpan, 0, len(merged))
	for _, sp := range merged {
		if overlapsAny(sp, names) {
			continue
		}
		out = append(out, sp)
	}
	return out
}

// prefixTokenSpans 找行内「公认前缀 + 足够长记号」的令牌区间。
//
// 命中条件仍是「前缀 + ≥minTokenTail 的连续记号」（这样主机名
// https://sk-gateway.example.com 与短记号的普通文本不会被误掩）；掩码终点
// 则从前缀一路延伸到**硬边界**（空白/引号，见 IsHardBoundary）为止：令牌后
// 紧跟 `.`、`。`、CJK、`*` 等字符时同样整段掩掉。2026-09-27 独立核验在 HEAD
// 快照上实证：旧实现按小边界集合判定终点，这些形态整个令牌不脱敏、明文直进
// /env/conflicts、/env/scan 与修复预览的响应正文。修法取「宁可多掩」：紧跟
// 令牌的注释标点/CJK 一并进掩码（掩码只影响展示，不参与删行判定，见包注释）。
//
// 与 api/diff.go 的 maskTokenShapes 的关系（2026-09-27 起**同口径**）：前缀表
// （SecretValuePrefixes）与最小长度同源，掩码终点两侧共用 [IsHardBoundary]。
// 此前差异展示面保留旧严格边界、同形夹具泄漏（A2-S2 登记残余），本轮对齐后
// 两侧只剩既定差异：本函数另有长跑扫描与键名排除，diff 侧是逐行文本替换。
// 改任一侧时仍须同时评估另一侧（包依赖方向：envcheck 是底层，api 依赖 envcheck）。
func prefixTokenSpans(text string) []tokenShapeSpan {
	var out []tokenShapeSpan
	for _, prefix := range secretValuePrefixes {
		for offset := 0; offset < len(text); {
			i := strings.Index(text[offset:], prefix)
			if i < 0 {
				break
			}
			i += offset // 折算回整行的绝对位置
			tailStart := i + len(prefix)
			j := tailStart
			for j < len(text) && isTokenByte(text[j]) {
				j++
			}
			if j-tailStart >= minTokenTail {
				k := j
				for k < len(text) && !IsHardBoundary(text[k]) {
					k++
				}
				out = append(out, tokenShapeSpan{start: i, end: k})
				offset = k
				continue
			}
			offset = tailStart
		}
	}
	return out
}

// longRunSpans 找行内 ≥minTokenRun 的连续 hex/base64 形状区间（同字符连跑豁免）。
//
// 字符集取 hex ∪ base64 的字母数字与 `+`/`-`/`_`；**不含** `/` 与 `.`：否则
// 「https://old.example/seg/seg/…」这类多段路径会被连成一段误掩；也**不含** `=`
// （base64 尾部填充），它会把键名与值连成一段。含 `/` 的 base64 属罕见形态，
// 其中 ≥40 的连续段仍会被逐段评估。
func longRunSpans(text string) []tokenShapeSpan {
	var out []tokenShapeSpan
	i := 0
	for i < len(text) {
		if !isRunByte(text[i]) {
			i++
			continue
		}
		j := i
		for j < len(text) && isRunByte(text[j]) {
			j++
		}
		if j-i >= minTokenRun && !hasRepeatedRun(text[i:j], tokenRunRepeatLimit) {
			out = append(out, tokenShapeSpan{start: i, end: j})
		}
		i = j
	}
	return out
}

// isRunByte 报告该字节是否属于长跑判定的字符集（hex ∪ base64 的字母数字与 +-_）。
func isRunByte(c byte) bool {
	return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' ||
		c == '+' || c == '-' || c == '_'
}

// assignmentNameRanges 返回行内每个赋值的键名区间（形态扫描绝不改键名）。
//
// 用脱敏后的行重新解析即可：掩码替身（`<已脱敏>`）不是合法标识符字节序列，
// 不会参与赋值识别，位置也因此始终对应「当前这版文本」。
func assignmentNameRanges(text string) [][2]int {
	spans := parseAssignmentSpans([]byte(text))
	out := make([][2]int, 0, len(spans))
	for _, s := range spans {
		out = append(out, [2]int{s.tokStart, s.eq})
	}
	return out
}

func overlapsAny(sp tokenShapeSpan, ranges [][2]int) bool {
	for _, r := range ranges {
		if sp.start < r[1] && r[0] < sp.end {
			return true
		}
	}
	return false
}

// isTokenByte 报告该字节是否属于令牌记号（与各家令牌字符集一致：无空白、无分隔号）。
//
// 字符集与 api/diff.go 的同名函数保持逐字同口径（原因见 prefixTokenSpans 的注释）。
func isTokenByte(c byte) bool {
	return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' ||
		c == '-' || c == '_' || c == '+' || c == '=' || c == '~'
}

// IsHardBoundary 报告字节 c 是否终止「最长连续记号 run」：只有空白与引号。
//
// 2026-09-27 核验结论（UC-015 脱敏残留）后的口径：旧实现沿用小边界集合
// （逗号、冒号、斜杠等也算终点），结果 `.`、`。`、CJK、`*` 这些集合之外的
// 「非边界字符」让整个令牌不脱敏。新口径取「宁可多掩」——除空白/引号外一律
// 并入掩码：掩掉的至多是注释标点，漏掩的是凭据明文，两个方向代价不对称。
//
// 2026-09-27 起导出为 IsHardBoundary：§4.3 差异掩码（api/diff.go 的
// maskTokenShapes）与本节脱敏**共用同一条边界判据**，同形泄漏在差异展示面
// 一并收口（此前两侧分叉，差异面保留旧严格边界 ⇒ 同形夹具泄漏）。
func IsHardBoundary(c byte) bool {
	switch c {
	case ' ', '\t', '\r', '"', '\'', '`':
		return true
	}
	return false
}
