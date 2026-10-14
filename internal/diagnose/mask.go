package diagnose

import "strings"

// 个人身份脱敏（UC-100 A6 的复制面口径，单源在本文件）。
//
// 冻结口径（对 doc/api-v1.md 的掩码段 additive）：**复制诊断**默认隐藏
//   1. 用户名（`/Users/<名>` 与 `/home/<名>` 里的个人目录段、以及独立的用户名记号）；
//   2. 绝对路径中的个人目录（家目录前缀 ⇒ `~`）；
//   3. 令牌（由 envcheck.MaskTokenShapes 另行统一处理——本文件不重复令牌判据）。
//
// 与 §1.2/§4.3 的**错误信封**掩码口径分界：错误信封只掩令牌形态（路径等诊断文本
// 保留，那是本地排障的线索）；诊断包是**要交出去的**复制物，因此更严——
// 个人路径与用户名一并掩掉。两处不共用一条判据是刻意的：界面展示与复制外带
// 的安全要求不同，混用会让其中一侧必然过严或过松。
//
// 为什么用户名单独一条且带长度门槛：`/Users/<名>` 段规则覆盖不了「值里裸出现的
// 用户名」（如 `https://<名>.example/...`）；但短名（< 3 字符）作为裸记号替换会把
// 正常文本切碎（用户名 `k` 会把一切含 k 的词打烂），故裸记号规则只对 ≥ 3 字符的
// 用户名生效——短名仍由家目录前缀与个人目录段两条规则兜住。

// MaskedSegment 是个人目录段/用户名的展示替身。
const MaskedSegment = "<已脱敏>"

// minStandaloneUsername 是「裸用户名」替换的最小长度（见包内注释）。
const minStandaloneUsername = 3

// UsernameFor 从家目录推出个人目录段里的用户名。
//
// 取家目录里**最后一个** `/Users/<名>` 或 `/home/<名>` 段（家目录可能被夹具或
// 容器形态套在更长的前缀下，如 `<tmp>/Users/<名>`）；推不出就返回空串——
// 不把无关目录名冒充成用户名（那会把正常字符串误掩）。
func UsernameFor(home string) string {
	home = strings.TrimRight(home, "/")
	name := ""
	for _, prefix := range []string{"/Users/", "/home/"} {
		if i := strings.LastIndex(home, prefix); i >= 0 {
			rest := home[i+len(prefix):]
			if j := strings.IndexByte(rest, '/'); j >= 0 {
				rest = rest[:j]
			}
			if rest != "" {
				name = rest
			}
		}
	}
	return name
}

// MaskPersonal 把一段文本里的个人痕迹掩掉（家目录 → `~`、个人目录段 → `<已脱敏>`、
// 裸用户名 → `<已脱敏>`）。幂等：对已掩文本再跑一遍不再变化。
func MaskPersonal(text, home, username string) string {
	if text == "" {
		return ""
	}
	out := maskHome(text, home)
	out = maskDirSegment(out, "/Users/")
	out = maskDirSegment(out, "/home/")
	if username != "" && len(username) >= minStandaloneUsername {
		out = maskStandalone(out, username)
	}
	return out
}

// maskHome 把家目录前缀替换为 `~`。
//
// 边界：家目录前一个字符不得是标识符字节（否则是更长路径段的一部分），
// 后一个字符必须是 `/` 或行尾（否则是更长的目录名）。
// macOS 的 `/var` 与 `/tmp` 是符号链接，canonicalPath 解析出的文本会带
// `/private` 前缀——两种形态都要掩，否则「个人路径不出现」会留一条尾巴。
func maskHome(text, home string) string {
	home = strings.TrimRight(home, "/")
	if home == "" || home == "/" {
		return text
	}
	text = maskHomePrefix(text, home)
	if !strings.HasPrefix(home, "/private/") {
		text = maskHomePrefix(text, "/private"+home)
	}
	return text
}

// maskHomePrefix 替换单一形态的家目录前缀。
func maskHomePrefix(text, home string) string {
	var b strings.Builder
	b.Grow(len(text))
	for {
		j := strings.Index(text, home)
		if j < 0 {
			b.WriteString(text)
			return b.String()
		}
		end := j + len(home)
		beforeOK := j == 0 || !isIdentByte(text[j-1])
		afterOK := end == len(text) || text[end] == '/'
		if beforeOK && afterOK {
			b.WriteString(text[:j])
			b.WriteString("~")
		} else {
			b.WriteString(text[:end])
		}
		text = text[end:]
	}
}

// maskDirSegment 把 `prefix + <段>` 形态里的个人目录段换成掩码替身。
//
// 段从前缀之后开始，止于路径/文本分隔符；空段与已掩替身原样保留（幂等）。
func maskDirSegment(text, prefix string) string {
	var b strings.Builder
	b.Grow(len(text))
	for {
		j := strings.Index(text, prefix)
		if j < 0 {
			b.WriteString(text)
			return b.String()
		}
		segStart := j + len(prefix)
		segEnd := segStart
		for segEnd < len(text) && !isSegmentDelimiter(text[segEnd]) {
			segEnd++
		}
		b.WriteString(text[:j])
		b.WriteString(prefix)
		seg := text[segStart:segEnd]
		if seg == "" || seg == MaskedSegment {
			b.WriteString(seg)
		} else {
			b.WriteString(MaskedSegment)
		}
		text = text[segEnd:]
	}
}

// maskStandalone 把作为**独立记号**出现的用户名换成掩码替身。
//
// 边界：前后都不得是标识符字节（用户名可能是更长记号的一部分，如 `anotheruser`
// 里含 `user`——那不是本人）。替换后继续扫描（同一段文本可能出现多次）。
func maskStandalone(text, username string) string {
	var b strings.Builder
	b.Grow(len(text))
	for {
		j := strings.Index(text, username)
		if j < 0 {
			b.WriteString(text)
			return b.String()
		}
		end := j + len(username)
		beforeOK := j == 0 || !isWordByte(text[j-1])
		afterOK := end == len(text) || !isWordByte(text[end])
		b.WriteString(text[:j])
		if beforeOK && afterOK {
			b.WriteString(MaskedSegment)
		} else {
			b.WriteString(username)
		}
		text = text[end:]
	}
}

// isWordByte 报告该字节是否属于「词记号」（裸用户名替换的边界字符集）。
//
// 比 isIdentByte 更窄：点号在词边界之外（URL 主机名 `synthuser123.example` 里的
// 用户名段是独立标签，必须掩；更长记号 `anotheruser` 由字母数字边界挡住），
// 因此这里只认字母数字与 `_` `-`。
func isWordByte(c byte) bool {
	switch {
	case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9':
		return true
	case c == '_' || c == '-':
		return true
	}
	return false
}

// isIdentByte 报告该字节是否属于标识符/路径段字符（含 `.` `-` `_` `~` `+`）。
func isIdentByte(c byte) bool {
	switch {
	case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9':
		return true
	case c == '_' || c == '-' || c == '.' || c == '~' || c == '+':
		return true
	}
	return false
}

// isSegmentDelimiter 报告该字节是否终止一个个人目录段。
//
// 取「宁可保守」的集合：路径分隔符、空白、引号与常见包裹符号。
// （段内因此允许用户名/目录名的真实字符集，含中文与点号。）
func isSegmentDelimiter(c byte) bool {
	switch c {
	case '/', ' ', '\t', '\n', '\r', '"', '\'', '`', '<', '>', '(', ')', '[', ']', '{', '}', ',', ';', ':', '=', '|':
		return true
	}
	return false
}
