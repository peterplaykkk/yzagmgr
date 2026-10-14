// 逐文件 diff：把「写前字节 → 目标字节」变成契约 §4.3 的 `diff:[{op,text}]`。
//
// 为什么自己写而不是引库：契约的 op 只有 add|del|ctx 三种，且这里只需要**给人看**的
// 行级差异；引一个 diff 依赖会为了同一件事多一条供应链，而仓库纪律是标准库优先。
//
// 有界性：diff 会随文件长度增长，而它要进 HTTP 响应。这里做三层收敛——
//   - 只对「去掉公共前后缀后的中段」算 LCS（配置文件通常只改动中段几行）；
//   - 中段过大时退化为「整段删 + 整段增」，不做 O(n·m) 的表格；
//   - 输出行数有上限，截断处显式给一条 ctx 说明，绝不静默截断。
//
// 安全：差异行来自**用户自己的 live 字节**（写前）与本产品的目标字节（写后），
// 其中若干家族的真实凭据就写在文件里（见下方 maskCredentials）。因此**从本文件出去的
// 每一行都先过掩码**——放在唯一的出口上，是「漏掩一行」这类事故的结构性防线。
// 掩码只发生在这一步：`family.FilePlan`（字节保真的计算产物）与 tx 的写盘路径都不经过它。
package api

import (
	"bytes"
	"strings"

	"github.com/peterplaykkk/yzagmgr/internal/envcheck"
	"github.com/peterplaykkk/yzagmgr/internal/proxy"
)

// diff 操作（契约 §4.3 的封闭取值）。
const (
	DiffAdd = "add"
	DiffDel = "del"
	DiffCtx = "ctx"
)

// diffLine 是契约 §4.3 的一行差异。
type diffLine struct {
	Op   string `json:"op"`
	Text string `json:"text"`
}

// maxDiffLines 是单文件 diff 的输出上限。
const maxDiffLines = 400

// maxDiffBlock 是走 LCS 的中段长度上限（两侧都超过就退化为整段替换）。
const maxDiffBlock = 240

// diffContext 是改动前后各保留的上下文行数。
const diffContext = 3

// buildDiff 计算两个字节串的行级差异。
func buildDiff(before, after []byte) []diffLine {
	if bytes.Equal(before, after) {
		return []diffLine{}
	}
	oldLines := splitLines(before)
	newLines := splitLines(after)

	// 公共前后缀：配置文件里绝大多数改动都在中段，先切掉两端能显著缩小问题规模。
	prefix := 0
	for prefix < len(oldLines) && prefix < len(newLines) && oldLines[prefix] == newLines[prefix] {
		prefix++
	}
	suffix := 0
	for suffix < len(oldLines)-prefix && suffix < len(newLines)-prefix &&
		oldLines[len(oldLines)-1-suffix] == newLines[len(newLines)-1-suffix] {
		suffix++
	}

	oldMid := oldLines[prefix : len(oldLines)-suffix]
	newMid := newLines[prefix : len(newLines)-suffix]

	out := make([]diffLine, 0, len(oldMid)+len(newMid)+2*diffContext)
	// 前文上下文：只保留改动前若干行（再多就对读者无益）。
	ctxStart := prefix - diffContext
	if ctxStart < 0 {
		ctxStart = 0
	}
	for _, line := range oldLines[ctxStart:prefix] {
		out = append(out, diffLine{Op: DiffCtx, Text: line})
	}

	if len(oldMid) <= maxDiffBlock && len(newMid) <= maxDiffBlock {
		out = append(out, lcsDiff(oldMid, newMid)...)
	} else {
		// 退化路径：整段替换。它仍然是**如实**的差异（这些行没了、这些行出现了），
		// 只是不给出更细的对应关系。
		for _, line := range oldMid {
			out = append(out, diffLine{Op: DiffDel, Text: line})
		}
		for _, line := range newMid {
			out = append(out, diffLine{Op: DiffAdd, Text: line})
		}
	}

	// 后文上下文。
	ctxEnd := len(oldLines) - suffix + diffContext
	if ctxEnd > len(oldLines) {
		ctxEnd = len(oldLines)
	}
	for _, line := range oldLines[len(oldLines)-suffix : ctxEnd] {
		out = append(out, diffLine{Op: DiffCtx, Text: line})
	}

	return redactDiffLines(capDiff(out))
}

// ── 凭据掩码（响应呈现层，UC-003 A12） ──────────────────────────────────────

// maskCredentials 把一行差异文本里承载凭据的**值**换成展示替身（`<已脱敏>`）。
//
// 为什么必须做：`diff[].text` 是 UI 用来回答「这次会改哪一行」的主要线索，而它整行带值。
// 用户 live 配置里常常就有真密钥（`ANTHROPIC_AUTH_TOKEN`），本产品自己也会把真令牌写进
// 若干家族的 live 文件（gemini 的 `GEMINI_API_KEY`、opencode/openclaw/pi 的 `apiKey`、
// grokbuild/hermes 的 `api_key`、ClaudeDesktop 物化形态的 `inferenceGatewayApiKey`、
// WorkBuddy 受管条目的 `apiKey`）。原样输出等于把令牌送进浏览器、前端日志与任何抓包。
//
// 掩的是**值**，不是整行：键名、分隔符、引号、缩进、`op` 全都保留，所以
// 「哪一行变了、变成什么形状、而且这一行承载凭据」依然可判读。落盘与备份都不经过这里
// （它们用的是 `family.FilePlan` 的字节），因此脱敏不改变任何写入结果。
func maskCredentials(text string) string {
	return maskTokenShapes(maskSettings(text))
}

// maskContent 是「配置内容端点」（§4.6 / UC-039 A4）的脱敏：替身 `***`、细粒度下钻。
//
// 与预览 diff 的 maskCredentials 只差两处，两处都是用户可见面的明确要求
// （「查看写入的所有内容，密钥 *** mask」）：
//   - 替身是 `***`（预览差异行仍是 `<已脱敏>`，契约与既有断言不动）；
//   - 结构化容器**不**因「内部含令牌形态」而整段掩（pi 的 `models.json` 曾被整段吞成
//     `<已脱敏>`，等于把用户要点开看的内容藏掉了）——普通容器继续下钻，凭据在叶子处掩；
//     键名本身敏感的容器（credentials/password 一类）照掩不误（值掩到首个结构分隔符，
//     余下部分继续重扫，内部凭据叶子仍会被掩），安全底线不降。
//
// 末尾的 maskTokenShapesWith 兜底照旧：解析漏掉的令牌形态仍会被掩。
func maskContent(text string) string {
	return maskTokenShapesWith(maskSettingsWith(text, maskedValueContent, true), maskedValueContent)
}

// maskSettings 从左到右扫一遍，把每一个「键 + 值」里承载凭据的值换掉。
//
// 为什么要**扫**而不是只看行首那一对：配置文件并不担保「一行一个键」——
// 紧凑 JSON（`{"env":{"ANTHROPIC_AUTH_TOKEN":"sk-…"}}`）、YAML flow 映射
// （`- {base_url: …, api_key: sk-…}`）、TOML inline table 都会把多个键塞进同一行，
// 而凭据可能不在第一个。
//
// 两条硬约束：
//   - **绝不丢字**：掩码只做替换，输出必须与输入逐字对应（差一行被吞掉，用户就会
//     按一个不存在的形态判断改动）。所以每条路径都把消费掉的原文写出去，
//     「回到值内部继续扫」之前也先把前缀写出来。
//   - **有界**：行长度没有上限（紧凑 JSON 可以整份在一行），扫描必须线性——只在
//     「可能是一个键起点」的位置（行首、`{`/`[`/`,`/`;` 之后）尝试解析，其余位置
//     逐字节复制；解析成功就跳过它消费掉的那一段。结构化值的内部会**回到值内部**
//     继续扫（嵌套被逐层看穿），而位置严格单调前进，不会回环。
func maskSettings(text string) string { return maskSettingsWith(text, maskedValue, false) }

// maskSettingsWith 是 maskSettings 的参数化形态：
//   - replacement 是凭据值的展示替身（预览 `<已脱敏>` / 内容端点 `***`）；
//   - fineGrained=true（内容端点）时，结构化容器不再因「值里含令牌形态」而整段掩，
//     而是继续下钻、在凭据叶子处掩；键名敏感的容器照掩（见 maskContent 的注释）。
func maskSettingsWith(text, replacement string, fineGrained bool) string {
	var b strings.Builder
	b.Grow(len(text) + 16)
	// 已写出字节的流式深度：>0 表示处在 `{}`/`[]` 里（JSON、TOML inline、YAML flow）。
	// 它只由**写出去的**文本推出——掩码不改结构，所以前缀就是当前所处的上下文。
	st := flowState{}
	write := func(s string) {
		st.observe(s)
		b.WriteString(s)
	}
	candidate := true // 行首可能是一个键的起点
	for i := 0; i < len(text); {
		switch c := text[i]; {
		case c == ' ' || c == '\t':
			b.WriteByte(c) // 行内空白不影响结构状态，直接写（缩进的键仍要能被判定）
			i++
		case c == '\n' || c == '\r':
			// **行首**也是一个键的起点，而这里的「行」不只是文档第一行：本函数吃的是整段
			// 文本（§4.6 的配置内容端点按整份文件调用），因此每个换行之后都是新的行首。
			// 位置必须在 `case candidate` **之前**：否则换行会先落进候选分支、被当成一个键的
			// 起点去解析（换行后面往往是缩进，解析必然失败），而失败分支会把候选位清掉。
			// 症状是一整类**静默的漏掩**：缩进漂亮的 JSON 整份不脱敏（`"apiKey": "真令牌"`
			// 原样进响应），而紧凑 JSON 照掩（键紧跟在 `{`/`,` 之后，候选位本来为真）。
			// 2026-09-25 由 §4.6 的内容端点实测发现（差异行都是一行文本，照不出这个缺口）。
			b.WriteByte(c)
			candidate = true
			i++
		case candidate:
			key, start, end, judge, ok := splitSetting(text, i, st.depth > 0)
			if !ok {
				write(text[i : i+1])
				i++
				candidate = false
				continue
			}
			// raw 是掩码区间原文；judge 是判定用值（相邻引号拼接形态下 = 各段内容按序
			// 拼接，其余形态与 raw 逐字相同——与 envcheck 的 shell 拼接口径同源）。
			raw := text[start:end]
			concat := len(raw) > 0 && isQuoteByte(raw[0])
			cred := isCredentialValue(key, judge)
			// 细粒度模式：普通结构化容器的「值里含令牌形态」不再触发整段掩——让出到下面的
			// 结构化分支继续下钻，凭据在叶子处掩。键名敏感（IsSensitiveName）的容器保持
			// 既有掩码路径（掩到首个结构分隔符，余下继续重扫，内部凭据叶子仍会被掩）。
			// 拼接形态不走这条豁免：整段字面量不以下钻形式重扫（区间含引号，重扫会绕回），
			// 豁免会把内部的令牌形态整个放行——宁可整段掩。
			if cred && fineGrained && !concat {
				if trimmed := strings.TrimSpace(judge); len(trimmed) > 0 &&
					(trimmed[0] == '{' || trimmed[0] == '[') && !envcheck.IsSensitiveName(key) {
					cred = false
				}
			}
			switch {
			case cred:
				write(text[i:start])
				write(replacement)
				i = end
				candidate = false
			case judge == "":
				// 空值（`"key":` 收尾、YAML 的父键行）：没有内容可掩，也没有内部可扫。
				write(text[i:end])
				i = end
				candidate = false
			case raw[0] == '{' || raw[0] == '[':
				// 结构化值：先原样写出前缀，再回到值内部继续扫（里面的键才是承载凭据的）。
				// 深度有上限（maxMaskNesting）：再深的嵌套按整段处理，掩码成本因此被
				// 常数封顶，不会随「一份压成一行的深层文档」变得超线性。代价是深层
				// （>32 层）的键名判据不再生效——那一层仍有末尾的令牌形态扫描兜底。
				if st.depth < maxMaskNesting {
					write(text[i:start])
					i = start
					candidate = true
					continue
				}
				write(text[i:end])
				i = end
				candidate = false
			default:
				write(text[i:end])
				i = end
				candidate = false
			}
		default:
			write(text[i : i+1])
			switch c {
			case ',', ';', '{', '[':
				candidate = true
			}
			i++
		}
	}
	return b.String()
}

// flowState 从「已写出的文本」里数出流式深度与引号状态。
//
// 它服务于一个局部判断：**裸值到哪里为止**。块式写法（dotenv 一行一个键、YAML 块映射）
// 的裸值到行尾；流式写法里 `,`/`}`/`]` 是结构分隔符，值到那里为止。
// 判错的代价不对称，因此这里偏向保守：认不出流式就当块式（值吃到行尾）——
// 那是「多掩」，而反过来（把值截断在逗号处）会把凭据的尾巴留在响应里。
type flowState struct {
	depth int
	quote byte
}

func (f *flowState) observe(s string) {
	for i := 0; i < len(s); i++ {
		c := s[i]
		if f.quote != 0 {
			if c == f.quote {
				f.quote = 0
			}
			continue
		}
		switch c {
		case '"', '\'':
			f.quote = c
		case '{', '[':
			f.depth++
		case '}', ']':
			if f.depth > 0 {
				f.depth--
			}
		}
	}
}

// redactDiffLines 逐行掩码（add/del/ctx 一视同仁：上下文行同样是响应正文的一部分）。
func redactDiffLines(lines []diffLine) []diffLine {
	out := make([]diffLine, len(lines))
	for i, line := range lines {
		out[i] = diffLine{Op: line.Op, Text: maskCredentials(line.Text)}
	}
	return out
}

// isCredentialValue 报告一个「键 → 值」是否承载凭据。
//
// 判据与仓库既有口径同源，不另立一套：`envcheck` 的 `sensitiveFor` 是全仓唯一的
// 敏感判定实现（环境扫描与修复共用），这里复用它判定的两个分支：
//   - 值本身形如凭据（`sk-`/`ghp_`/`AKIA`… 等公认前缀，或长而无分隔的不透明串，
//     见 `envcheck.LooksLikeSecret`）→ 承载凭据，**不看键名**（键名可以任意：
//     `inferenceGatewayApiKey`、`api_key`、`CUSTOM_HEADER`…）；
//   - 键名形如凭据位（`*TOKEN*`/`*KEY*`/`*SECRET*`/`*PASSWORD*`/`*CREDENTIAL*`/
//     `*BEARER*`/`*COOKIE*`/`*SESSION*`，见 `envcheck.IsSensitiveName` 的标记表）
//     → 承载凭据，但**计数/布尔字面量除外**（见 isCountLiteral）。
//
// 取舍一（宁可多掩）：漏掩是安全事故（令牌进响应正文），多掩只是少了一行文本的可读性。
// 因此除了上面两条判据，还有第三层兜底（`maskTokenShapes`）：值里出现已知前缀的记号
// 一律整值掩掉，覆盖「键名看不出、值里带前缀」的写法（典型：
// `"ANTHROPIC_CUSTOM_HEADERS": "x-api-key: sk-…"`）。
//
// 取舍二（但计数不掩）：`IsSensitiveName` 是**子串**判据，而本产品自己写的一批
// **计数型**键名里就含 TOKEN（`maxInputTokens`/`maxOutputTokens`/`reserveTokens`/
// `CLAUDE_CODE_MAX_OUTPUT_TOKENS`、`…AUTO_COMPACT_WINDOW`）。对差异行来说，
// 「这次把 token 预算从多少改成了多少」恰恰是最需要看清的那种改动，掩掉它等于把
// 可判读性丢掉，而且会让 JSON 行变成非法形状。仓库对同一批键的既有口径也是「不掩」：
// `companionKeys` 把 `entries[].apiKey` 标 `masked:true`、把 `maxInputTokens` 标不掩。
//
// **两类刻意不掩**：
//   - 空值：掩成替身是**说谎**（会让人以为这里原来有个值是密钥）；
//   - 已知占位符（`PROXY_MANAGED`、`<已脱敏>`、`__YZAGMGR_MASKED_API_KEY__`…）：
//     它们不是凭据，而且正是「这一行从带真密钥变成走网关」的证据，掩掉会把差异读反。
func isCredentialValue(key, value string) bool {
	if value == "" || isPlaceholderValue(value) {
		return false
	}
	if envcheck.LooksLikeSecret(value) || maskTokenShapes(value) != value {
		return true
	}
	return envcheck.IsSensitiveName(key) && !isCountLiteral(value)
}

// countLiteralMaxDigits 是「还算计数」的位数上限：再长就不像计数、像凭据。
const countLiteralMaxDigits = 12

// isCountLiteral 报告值是否是「计数/布尔字面量」（可带引号、可带单位后缀）。
//
// 存在的理由与取舍写在 isCredentialValue 的「取舍二」里。这里补两点实现口径：
//   - 认的数字形态就是配置里真出现的那些：纯数字、千分位/小数/下划线分隔、
//     `450k`/`1M`/`42%` 这类单位后缀、以及 `true`/`false`/`null`；
//   - 超长纯数字**不**算计数（`countLiteralMaxDigits`）：计数不会到 1e12 量级，
//     而纯数字密钥会——这是「短数字凭据可能漏掩」这个已知代价的收口。
func isCountLiteral(value string) bool {
	v := strings.TrimSpace(value)
	if len(v) >= 2 && (v[0] == '"' || v[0] == '\'') && v[len(v)-1] == v[0] {
		v = strings.TrimSpace(v[1 : len(v)-1])
	}
	switch strings.ToLower(v) {
	case "true", "false", "null":
		return true
	}
	digits := 0
	for i := 0; i < len(v); i++ {
		switch c := v[i]; {
		case c >= '0' && c <= '9':
			digits++
		case c == '_' || c == '.':
			// 千分位/小数/下划线分隔：计数里常见，凭据里罕见。
		case (c == 'k' || c == 'K' || c == 'm' || c == 'M' || c == '%') && i == len(v)-1:
			// 单位后缀（`450k`、`1M`、`42%`）。
		default:
			return false
		}
	}
	return digits > 0 && digits <= countLiteralMaxDigits
}

// isPlaceholderValue 报告值是否是凭据位上的已知占位符（用各包常量比对，不写字面量）。
func isPlaceholderValue(value string) bool {
	switch strings.TrimSpace(value) {
	case proxy.Placeholder, maskedValue:
		return true
	}
	return false
}

// minTokenTail 是兜底扫描里「前缀之后还算得上令牌」的最小记号长度。
// 16 与 UC-003 A12 用的令牌形态（`sk-[A-Za-z0-9_-]{16,}`）同口径；取更短会把
// `sk-` 开头的普通标识误判成令牌。
const minTokenTail = 16

// maxMaskNesting 是掩码愿意看穿的嵌套层数上限（见 maskSettings 的「结构化值」分支）。
// 取值只需覆盖真实配置：最深的受管文件是 `{"env":{...}}`、`provider.<id>.options.apiKey`
// 这一档（≤6 层），32 层留了足够余量。
const maxMaskNesting = 32

// maskTokenShapes 把文本里每个「已知凭据前缀 + 足够长的记号」换成展示替身。
//
// 前缀表复用 `envcheck` 的唯一真相源（见 `envcheck.SecretValuePrefixes`），
// 免得这里再抄一份迟早会漂移的表；掩码终点与 `envcheck.redact` 的整行脱敏
// **同口径**：命中后从前缀一路盖到**硬边界**（`envcheck.IsHardBoundary`：只有
// 空白与引号）为止——令牌后紧跟 `.`、`。`、CJK、`*` 等字符时同样整段掩掉
// （2026-09-27 口径对齐，A2-S2 登记残余：旧实现按小边界集合判定终点，同形
// 夹具在差异展示面 3/4 泄漏；与 UC-015 脱敏残留同一根因，现两侧共用
// `IsHardBoundary` 这一条判据）。记号本身的字符集不含 `.`/`@`/`/`/`:`，
// 且要求前缀后的记号 ≥minTokenTail：这样 `https://sk-gateway.example.com`
// 这类**主机名**（前缀后的记号太短）不会被误当成令牌掩掉。
func maskTokenShapes(text string) string { return maskTokenShapesWith(text, maskedValue) }

// maskTokenShapesWith 是 maskTokenShapes 的参数化形态（替身可变，判据不变）。
func maskTokenShapesWith(text, replacement string) string {
	for _, prefix := range envcheck.SecretValuePrefixes() {
		for offset := 0; offset < len(text); {
			i := strings.Index(text[offset:], prefix)
			if i < 0 {
				break
			}
			i += offset // 折算回整行的绝对位置
			tailStart := i + len(prefix)
			tailEnd := tailStart
			for tailEnd < len(text) && isTokenByte(text[tailEnd]) {
				tailEnd++
			}
			if tailEnd-tailStart >= minTokenTail {
				// 命中：终点延伸到硬边界（与 envcheck.redact 的 prefixTokenSpans 同口径）。
				// 掩掉的至多是紧跟令牌的标点/CJK，漏掩的会是凭据明文——两个方向代价不对称。
				end := tailEnd
				for end < len(text) && !envcheck.IsHardBoundary(text[end]) {
					end++
				}
				text = text[:i] + replacement + text[end:]
				offset = i + len(replacement)
				continue
			}
			offset = tailStart
		}
	}
	return text
}

// isTokenByte 报告该字节是否属于令牌记号（与各家的令牌字符集一致：无空白、无分隔号）。
func isTokenByte(c byte) bool {
	return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' ||
		c == '-' || c == '_' || c == '+' || c == '=' || c == '~'
}

// splitSetting 从 `from` 位置起解析「键名 + 值字面量」，返回值在行内的字节区间。
//
// 支持的形态就是本产品真的会读写的几种（`internal/family` 的写链 + 用户自己的文件）：
//
//	JSON/JSON5   "key": "value"
//	TOML         key = "value"
//	dotenv       KEY=value（可带 `export ` 前缀）
//	YAML         key: value（可带 `- ` 列表前缀）
//	shell 拼接   "a"'b'（相邻引号段是同一个值；掩码区间 = 整段字面量）
//
// `flow` 由调用方给出当前是否处于流式上下文；本函数若自己吃掉了 `{`/`[` 前缀
// （`{"a": 1}`、`- {a: 1}` 这类），也按流式处理——它决定**裸值**到哪里为止：
// 流式到 `,`/`}`/`]`，块式到行尾。
//
// 返回三元组（区间 + 判定值）的口径：
//   - [start,end) 是**掩码区间**：单段形态只覆盖值的内容（引号留在原地，这样掩码后的行
//     仍看得出原本的引号与逗号）；相邻引号拼接形态是整段字面量（含首尾引号）；
//   - judge 是**判定用值**：单段与裸值等于区间原文；拼接形态是各段内容按序拼接
//     （shell 词法），供 isCredentialValue 判凭据位（`"tp-"'<尾巴>'` 这类拼接点
//     在词法上属于同一枚令牌）。
//
// 认不出来就 ok=false —— 认不出来**不等于**安全，调用方随后仍会跑令牌形态扫描。
func splitSetting(line string, from int, flow bool) (key string, start, end int, judge string, ok bool) {
	i := from
	// 前缀：YAML 列表项、dotenv 的 `export `、单行 JSON/JSON5 的开括号。
	for i < len(line) {
		before := i
		i = skipSpaceBytes(line, i)
		switch {
		case strings.HasPrefix(line[i:], "- "):
			i += 2
		case strings.HasPrefix(line[i:], "export "):
			i += len("export ")
		case line[i] == '{' || line[i] == '[':
			i++
			flow = true // 吃掉了开括号：从这里往右是流式上下文
		}
		if i == before {
			break
		}
	}
	if i >= len(line) || line[i] == '#' || line[i] == '/' {
		return "", 0, 0, "", false // 注释行/空行：没有「键 + 值」可拆
	}

	if line[i] == '"' || line[i] == '\'' {
		quote := line[i]
		close := strings.IndexByte(line[i+1:], quote)
		if close < 0 {
			return "", 0, 0, "", false
		}
		key = line[i+1 : i+1+close]
		i += close + 2
	} else {
		keyStart := i
		for i < len(line) && line[i] != '=' && line[i] != ':' &&
			line[i] != ' ' && line[i] != '\t' {
			i++
		}
		key = strings.TrimSpace(line[keyStart:i])
	}
	if key == "" {
		return "", 0, 0, "", false
	}

	i = skipSpaceBytes(line, i)
	if i >= len(line) || (line[i] != '=' && line[i] != ':') {
		return "", 0, 0, "", false
	}
	i = skipSpaceBytes(line, i+1)
	if i >= len(line) {
		return key, i, i, "", true // 空值：键名单独一行（YAML 的父键形态）
	}

	if line[i] == '"' || line[i] == '\'' || line[i] == '`' {
		close := closingQuote(line, i+1, line[i])
		if close < 0 {
			return "", 0, 0, "", false // 引号没闭合：整行不可信，交给兜底扫描
		}
		// 单段（绝大多数形态）：区间 = 值内容（引号留在区间外），判定值 = 区间原文。
		if close+1 >= len(line) || !isQuoteByte(line[close+1]) {
			return key, i + 1, close, line[i+1 : close], true
		}
		// 相邻引号拼接（shell 合法写法 `"短段"'长段'`，与 envcheck 的 shell 拼接口径同源）：
		// 紧贴的下一段仍是**同一个值**的字面量。简单剥一层引号在这类形态上取到的是带引号
		// 原文，判不出凭据位 ⇒ 两段都残留（UC-015 核验判红②的同形形态，差异展示面由
		// 本函数的回归钉守着）。这里的口径：
		//   - 掩码区间 = **整段字面量**（首段开引号 → 末段闭引号，含引号），与 §9 的
		//     「整体（含引号）替换为掩码」同形；
		//   - 判定值 = 各段**内容**按序拼接（shell 词法），于是拼接点落在前缀后
		//     （`"tp-"'<记号>'`）还是记号中段（`"tp-mimo-01J2"'<尾巴>'`）都不影响
		//     整枚令牌被判成凭据并整段掩掉；区间之外的字节一个不动。
		var joined strings.Builder
		joined.WriteString(line[i+1 : close])
		end := close + 1 // 字面量终点（末段闭引号之后）
		for end < len(line) && isQuoteByte(line[end]) {
			next := closingQuote(line, end+1, line[end])
			if next < 0 {
				break // 末段未闭合：就停在这里，已并入的部分照掩
			}
			joined.WriteString(line[end+1 : next])
			end = next + 1
		}
		return key, i, end, joined.String(), true
	}

	// 裸值：去掉注释与尾部空白；流式上下文里到 `,`/`}`/`]` 为止（那是结构分隔符）。
	value := line[i:]
	if !flow {
		if lineEnd := strings.IndexAny(value, "\r\n"); lineEnd >= 0 {
			value = value[:lineEnd]
		}
	}
	if k := strings.IndexByte(value, '#'); k > 0 {
		value = value[:k]
	}
	if flow {
		if k := strings.IndexAny(value, ",}]"); k >= 0 {
			value = value[:k]
		}
	}
	value = strings.TrimRight(value, " \t\r")
	// 行尾的 `,`/`;` 是**语法分隔符**，不是值的一部分：把它留在区间外，掩码后的行
	// 才仍然是有逗号的同一形状（否则 `"maxInputTokens": <已脱敏>` 会把逗号一起吃掉，
	// JSON 变成非法形状）；同时也让「计数 vs 凭据」的判定看到干净的字面量。
	if flow == false {
		value = strings.TrimRight(value, ",;")
	}
	// 裸值没有拼接段：判定值与区间原文逐字相同。
	return key, i, i + len(value), value, true
}

// skipSpaceBytes 跳过空格与制表符。
func skipSpaceBytes(s string, i int) int {
	for i < len(s) && (s[i] == ' ' || s[i] == '\t') {
		i++
	}
	return i
}

// isQuoteByte 报告该字节是否引号字符（三种引号同一口径；拼接段判定用）。
func isQuoteByte(c byte) bool { return c == '"' || c == '\'' || c == '`' }

// closingQuote 从 from 起找闭合引号 quote 的位置；没找到返回 -1。
// 双引号内按 JSON 转义跳过 `\` 及其后一个字符（与 splitSetting 的取值口径一致）。
func closingQuote(line string, from int, quote byte) int {
	for j := from; j < len(line); j++ {
		if quote == '"' && line[j] == '\\' {
			j++ // JSON 转义：跳过被转义的字符
			continue
		}
		if line[j] == quote {
			return j
		}
	}
	return -1
}

// capDiff 截断过长 diff，并在末尾显式给出截断说明（ctx 行）。
func capDiff(lines []diffLine) []diffLine {
	if len(lines) <= maxDiffLines {
		return lines
	}
	out := make([]diffLine, 0, maxDiffLines+1)
	out = append(out, lines[:maxDiffLines]...)
	out = append(out, diffLine{Op: DiffCtx,
		Text: "（差异过长，已截断；完整内容见备份与目标文件）"})
	return out
}

// splitLines 按行切分；行尾换行不进入 text（它不属于"内容"）。
func splitLines(b []byte) []string {
	if len(b) == 0 {
		return nil
	}
	text := string(b)
	if strings.HasSuffix(text, "\n") {
		text = text[:len(text)-1]
	}
	return strings.Split(text, "\n")
}

// lcsDiff 是两段文本的最长公共子序列差异（n·m 有界，调用方保证规模）。
func lcsDiff(oldLines, newLines []string) []diffLine {
	n, m := len(oldLines), len(newLines)
	// table[i][j] = oldLines[i:] 与 newLines[j:] 的 LCS 长度。
	table := make([][]int, n+1)
	for i := range table {
		table[i] = make([]int, m+1)
	}
	for i := n - 1; i >= 0; i-- {
		for j := m - 1; j >= 0; j-- {
			if oldLines[i] == newLines[j] {
				table[i][j] = table[i+1][j+1] + 1
			} else if table[i+1][j] >= table[i][j+1] {
				table[i][j] = table[i+1][j]
			} else {
				table[i][j] = table[i][j+1]
			}
		}
	}

	out := make([]diffLine, 0, n+m)
	i, j := 0, 0
	for i < n && j < m {
		switch {
		case oldLines[i] == newLines[j]:
			out = append(out, diffLine{Op: DiffCtx, Text: oldLines[i]})
			i++
			j++
		case table[i+1][j] >= table[i][j+1]:
			out = append(out, diffLine{Op: DiffDel, Text: oldLines[i]})
			i++
		default:
			out = append(out, diffLine{Op: DiffAdd, Text: newLines[j]})
			j++
		}
	}
	for ; i < n; i++ {
		out = append(out, diffLine{Op: DiffDel, Text: oldLines[i]})
	}
	for ; j < m; j++ {
		out = append(out, diffLine{Op: DiffAdd, Text: newLines[j]})
	}
	return out
}
