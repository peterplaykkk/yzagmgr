package usagescript

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"unicode/utf8"
)

// 模板变量注入（UC-016 修订版 A1）。
//
// 注入分两类，**秘密与脚本彻底隔离**：
//
//   - 非秘密（{{baseUrl}} / {{userId}}）：注入真实值。它们本来就要出现在
//     请求 URL / 提取结果里（供应商 host、网关用户 id），不是凭据；
//   - 秘密（{{apiKey}} / {{accessToken}}）：**绝不注入真值**，只注入一个本次
//     运行随机生成、与运行绑定的**不透明引用**（句柄）。脚本可以拿句柄去
//     拼 header、做比较、写进 request 描述，但拿不到、也推不出真实密钥；
//     真实密钥只在宿主侧中介层（mediate）对「已批准供应商 + 批准位置 + 只读
//     请求」代入（A15 边界）。
//
// 为什么还要按词法状态转义：非秘密值（baseUrl / userId 由部署与身份决定，不
// 受本包控制）仍可能含引号、换行、`${`——直接 `strings.ReplaceAll` 会改变脚本
// 语义。这里按脚本的词法状态选择转义方式：
//
//   - 普通位置：注入 JSON 字符串字面量（`"..."`，完整转义）；
//   - 单引号串内：只转义 `\` `'` 与换行；
//   - 双引号串内：只转义 `\` `"` 与换行；
//   - 模板串内：转义 `\` 与反引号，并把 `${` 打断（否则值里的 `${}` 会被求值）。
//
// 句柄本身是 `[a-z0-9-]` 字符集，任何位置注入都不会改变脚本语义，但仍统一走
// 同一转义通道（同一代码路径，不给「哪类值走哪条路」留分叉）。
//
// 已知边界：正则字面量里的占位符会被当作普通位置处理（JS 的 `/` 二义性需要
// 完整解析器才能消歧）；注释里的占位符照常注入。两者都不改变「秘密不落脚本」
// 的性质，只是可能让脚本自身语法出错——那种情况会以 compile 阶段的行号报出。

// Vars 是模板变量取值。APIKey / AccessToken 是**真实秘密**：它们不注入脚本，
// 只进运行绑定的宿主侧代入面（Bindings）。
type Vars struct {
	APIKey      string
	BaseURL     string
	AccessToken string
	UserID      string
}

// Placeholders 是模板变量名（契约至少这四个）。
var Placeholders = []string{"apiKey", "baseUrl", "accessToken", "userId"}

// secretSlots 是走不透明引用的槽位：注入句柄，不注入真值。
var secretSlots = []string{"apiKey", "accessToken"}

func isSecretSlot(name string) bool {
	for _, s := range secretSlots {
		if s == name {
			return true
		}
	}
	return false
}

func (v Vars) lookup(name string) (string, bool) {
	switch name {
	case "apiKey":
		return v.APIKey, true
	case "baseUrl":
		return v.BaseURL, true
	case "accessToken":
		return v.AccessToken, true
	case "userId":
		return v.UserID, true
	default:
		return "", false
	}
}

// handlePrefix 是不透明引用的前缀：给日志与诊断一个可判别的稳定形态。
const handlePrefix = "yzagmgr-ref-"

// handleShapePattern 是句柄**形态**：`yzagmgr-ref-` + ≥20 位小写 hex。
//
// 位置封闭性（mediate 的非批准位置全扫）用形态而不是「本次运行的句柄字面
// 串」：字面串判定会漏掉仍保留形态的变形（截断、去槽位后缀、拼接前缀……），
// 形态扫描把这些一并抓住。句柄本体是 `yzagmgr-ref-<32 位 hex>-<槽位>`，形态阈值
// 取 20 位 hex，能覆盖「截断但保留足够信息量」的写法。
//
// 已知限制（并在收敛回报的 deviations 中登记）：把句柄**拆散重排**的变形
// （反转、逐字符插空、大小写改写、base64/编码）会脱离该形态，形态扫描不可
// 探测。这类变形不构成凭据外传：秘密永不随行（代入只发生在批准头值，且只
// 代入本次运行的完整句柄），变形串换不回秘密；目标也仍受批准面约束（https +
// 非内网 + 已批准供应商 origin）。
var handleShapePattern = regexp.MustCompile(regexp.QuoteMeta(handlePrefix) + `[0-9a-f]{20,}`)

// containsHandleShape 报告文本里是否出现句柄形态（空文本恒 false）。
func containsHandleShape(text string) bool {
	return text != "" && handleShapePattern.MatchString(text)
}

// Binding 是一条秘密槽位的绑定：句柄（注入脚本）+ 真实秘密（宿主侧代入）。
type Binding struct {
	Slot   string
	Handle string
	// Secret 是真实秘密；只出现在宿主侧代入与脱敏路径，绝不进脚本、响应与日志。
	Secret string
}

// Bindings 是一次执行的注入台账。
type Bindings struct {
	refs []Binding
	// values 是全部槽位实际注入脚本源码的字面值（秘密槽位 = 句柄）。
	values map[string]string
}

// newBindings 生成一次运行的注入台账：先给秘密槽位各生成一个随机句柄。
func newBindings(vars Vars) (*Bindings, error) {
	b := &Bindings{values: make(map[string]string, len(Placeholders))}
	for _, slot := range Placeholders {
		val, _ := vars.lookup(slot)
		if !isSecretSlot(slot) {
			b.values[slot] = val
			continue
		}
		handle, err := newHandle(slot)
		if err != nil {
			return nil, err
		}
		b.refs = append(b.refs, Binding{Slot: slot, Handle: handle, Secret: val})
		b.values[slot] = handle
	}
	return b, nil
}

// errRandomSource 标记句柄随机源不可用：这是宿主侧故障（internal），不是脚本
// 的问题（invalid_script_shape）——两者在 Run 里分别落码。
var errRandomSource = errors.New("凭据引用随机源不可用")

// newHandle 生成一个句柄：`yzagmgr-ref-<16 字节随机 hex>-<槽位>`。
//
// 随机源必须是 crypto/rand：句柄是「不可知」性质的唯一保证——可预测就等于把
// 秘密交给了可枚举的攻击者（句柄本身不是秘密，但「猜中另一个槽位的句柄」不
// 应该有任何捷径；何况句柄还要与**运行**绑定，两次运行的句柄必须互不相关）。
func newHandle(slot string) (string, error) {
	var buf [16]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return "", fmt.Errorf("%w: %v", errRandomSource, err)
	}
	return handlePrefix + hex.EncodeToString(buf[:]) + "-" + slot, nil
}

// Refs 返回秘密槽位的绑定（拷贝）。
func (b *Bindings) Refs() []Binding {
	if b == nil {
		return nil
	}
	out := make([]Binding, len(b.refs))
	copy(out, b.refs)
	return out
}

// Handles 返回全部句柄。
func (b *Bindings) Handles() []string {
	if b == nil {
		return nil
	}
	out := make([]string, 0, len(b.refs))
	for _, r := range b.refs {
		if r.Handle != "" {
			out = append(out, r.Handle)
		}
	}
	return out
}

// Secrets 返回全部非空真实秘密（供脱敏与代入面；不进任何响应）。
func (b *Bindings) Secrets() []string {
	if b == nil {
		return nil
	}
	out := make([]string, 0, len(b.refs))
	for _, r := range b.refs {
		if r.Secret != "" {
			out = append(out, r.Secret)
		}
	}
	return out
}

// handleFor 返回槽位的句柄（没有绑定时为空串）。
func (b *Bindings) handleFor(slot string) string {
	if b == nil {
		return ""
	}
	for _, r := range b.refs {
		if r.Slot == slot {
			return r.Handle
		}
	}
	return ""
}

// HasCredential 报告文本里是否出现句柄、真实秘密或**句柄形态**（A15 的位置
// 校验用）。
//
// 形态判定覆盖「保留 `yzagmgr-ref-<hex>` 结构的变形」（截断 / 去槽位后缀 /
// 拼接前缀……）：字面串判定会漏掉这些，而它们与句柄同属凭据派生材料。
//
// 这里与脱敏共用同一套长度闸门：太短的值盲替换会把无关键名切碎，而真实凭据
// 与句柄都远长于 MinSecretLenForRedact（形态有正则兜底，不受该闸门影响）。
func (b *Bindings) HasCredential(text string) bool {
	if text == "" {
		return false
	}
	if b != nil {
		for _, h := range b.Handles() {
			if len(h) >= MinSecretLenForRedact && strings.Contains(text, h) {
				return true
			}
		}
		for _, s := range b.Secrets() {
			if len(s) >= MinSecretLenForRedact && strings.Contains(text, s) {
				return true
			}
		}
	}
	return containsHandleShape(text)
}

// SubstituteRefs 把文本里的句柄替换为真实秘密（宿主侧代入）。
//
// 只应在 approvePlacement / mediate 通过之后调用：这里不做任何位置校验，
// 它是纯替换（保证「代入只发生在一个调用点上」，避免规则散落）。
func (b *Bindings) SubstituteRefs(text string) string {
	if b == nil || text == "" {
		return text
	}
	for _, r := range b.refs {
		if r.Handle == "" || r.Secret == "" {
			continue
		}
		text = strings.ReplaceAll(text, r.Handle, r.Secret)
	}
	return text
}

// Mask 把句柄与真实秘密从文本里抹掉（A1/A16：错误消息、日志、试运行响应与
// 响应正文一律只见脱敏替身）；**句柄形态**（形态判定命中的变形，如截断 /
// 去后缀）同样替换为替身——展示面/错误面不出现句柄派生材料。
func (b *Bindings) Mask(text string) string {
	if text == "" {
		return text
	}
	if b != nil {
		for _, s := range b.Secrets() {
			if len(s) < MinSecretLenForRedact {
				continue
			}
			text = strings.ReplaceAll(text, s, MaskedSecret)
		}
		for _, h := range b.Handles() {
			if len(h) < MinSecretLenForRedact {
				continue
			}
			text = strings.ReplaceAll(text, h, MaskedSecret)
		}
	}
	return handleShapePattern.ReplaceAllString(text, MaskedSecret)
}

// Substitute 把 {{name}} 注入脚本源码。
//
// 语义（UC-016 修订版 A1）：非秘密槽位注入真实值；秘密槽位注入**本次运行随机
// 生成的不透明引用**。返回替换后的源码与注入台账（Bindings：句柄 ↔ 秘密的
// 全部绑定，供宿主侧代入与脱敏）。即使中途报错也返回已生成的台账（调用方
// 仍需用它脱敏错误消息）。
func Substitute(script string, vars Vars) (string, *Bindings, error) {
	bindings, err := newBindings(vars)
	if err != nil {
		return "", nil, err
	}

	var out strings.Builder
	out.Grow(len(script) + 32)

	var (
		quote      byte // 0=普通位置，其余是当前字符串引号
		lineCmt    bool
		blockCmt   bool
		errUnknown error
	)

	// emit 在 i 处尝试注入；返回新位置、是否命中、以及错误。
	emit := func(i int) (int, bool, error) {
		name, end, ok := placeholderAt(script, i)
		if !ok {
			return i, false, nil
		}
		val, known := bindings.values[name]
		if !known {
			return i, true, fmt.Errorf("未知模板变量 {{%s}}（可用：%s）",
				name, strings.Join(Placeholders, "、"))
		}
		switch quote {
		case '\'':
			out.WriteString(escapeSingleQuoted(val))
		case '"':
			out.WriteString(escapeDoubleQuoted(val))
		case '`':
			out.WriteString(escapeTemplate(val))
		default:
			out.WriteString(jsonStringLiteral(val))
		}
		return end, true, nil
	}

	for i := 0; i < len(script); {
		if errUnknown != nil {
			break
		}
		c := script[i]

		if lineCmt {
			out.WriteByte(c)
			if c == '\n' {
				lineCmt = false
			}
			i++
			continue
		}
		if blockCmt {
			if c == '*' && i+1 < len(script) && script[i+1] == '/' {
				out.WriteString("*/")
				blockCmt = false
				i += 2
				continue
			}
			out.WriteByte(c)
			i++
			continue
		}

		if quote != 0 {
			if c == '\\' && i+1 < len(script) {
				out.WriteByte(c)
				out.WriteByte(script[i+1])
				i += 2
				continue
			}
			if c == quote {
				quote = 0
				out.WriteByte(c)
				i++
				continue
			}
			if next, ok, err := emit(i); ok {
				if err != nil {
					errUnknown = err
					break
				}
				i = next
				continue
			}
			out.WriteByte(c)
			i++
			continue
		}

		switch {
		case c == '/' && i+1 < len(script) && script[i+1] == '/':
			lineCmt = true
			out.WriteString("//")
			i += 2
			continue
		case c == '/' && i+1 < len(script) && script[i+1] == '*':
			blockCmt = true
			out.WriteString("/*")
			i += 2
			continue
		case c == '\'' || c == '"' || c == '`':
			quote = c
			out.WriteByte(c)
			i++
			continue
		}
		if next, ok, err := emit(i); ok {
			if err != nil {
				errUnknown = err
				break
			}
			i = next
			continue
		}
		out.WriteByte(c)
		i++
	}
	if errUnknown != nil {
		return "", bindings, errUnknown
	}
	return out.String(), bindings, nil
}

// placeholderAt 在 i 处识别 `{{name}}`；未命中时返回原位置。
func placeholderAt(s string, i int) (string, int, bool) {
	if i+3 >= len(s) || s[i] != '{' || s[i+1] != '{' {
		return "", i, false
	}
	j := i + 2
	start := j
	for j < len(s) && isNameByte(s[j]) {
		j++
	}
	if j == start || j+1 >= len(s) || s[j] != '}' || s[j+1] != '}' {
		return "", i, false
	}
	return s[start:j], j + 2, true
}

func isNameByte(c byte) bool {
	switch {
	case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9', c == '_':
		return true
	default:
		return false
	}
}

// jsonStringLiteral 产出完整转义的 JS 字符串字面量（含引号）。
func jsonStringLiteral(v string) string {
	var sb strings.Builder
	enc := json.NewEncoder(&sb)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		// 兜底：字符串永远可编码；真失败也绝不放行未转义的值。
		return `""`
	}
	return strings.TrimRight(sb.String(), "\n")
}

// escapeSingleQuoted 产出可放进单引号串里的内容（不含引号本身）。
func escapeSingleQuoted(v string) string {
	return escapeQuotedContent(v, '\'')
}

// escapeDoubleQuoted 产出可放进双引号串里的内容（不含引号本身）。
func escapeDoubleQuoted(v string) string {
	return escapeQuotedContent(v, '"')
}

func escapeQuotedContent(v string, quote byte) string {
	var sb strings.Builder
	sb.Grow(len(v) + 8)
	for i := 0; i < len(v); i++ {
		c := v[i]
		switch {
		case c == '\\':
			sb.WriteString(`\\`)
		case c == quote:
			sb.WriteByte('\\')
			sb.WriteByte(c)
		case c == '\n':
			sb.WriteString(`\n`)
		case c == '\r':
			sb.WriteString(`\r`)
		default:
			// U+2028 / U+2029 在 JS 里是行终止符，出现在字符串字面量的**原始
			// 位置**会把它截断，必须写成 \uXXXX 转义（ASCII 源码里按 rune 判定）。
			if r, size := utf8.DecodeRuneInString(v[i:]); r == '\u2028' || r == '\u2029' {
				fmt.Fprintf(&sb, `\u%04x`, r)
				i += size - 1
				continue
			}
			sb.WriteByte(c)
		}
	}
	return sb.String()
}

// escapeTemplate 产出可放进反引号模板串里的内容（不含反引号本身）。
func escapeTemplate(v string) string {
	var sb strings.Builder
	sb.Grow(len(v) + 8)
	for i := 0; i < len(v); i++ {
		c := v[i]
		switch {
		case c == '\\':
			sb.WriteString(`\\`)
		case c == '`':
			sb.WriteString("\\`")
		case c == '$' && i+1 < len(v) && v[i+1] == '{':
			// 打断 ${...}：值里出现 ${...} 时绝不能被当作插值求值。
			sb.WriteString(`\$`)
		default:
			sb.WriteByte(c)
		}
	}
	return sb.String()
}

// RedactSecrets 把给定值从任意文本里抹掉：错误消息、日志、诊断都不落凭据（A1）。
//
// 保留为包级工具：调用方没有 Bindings 时（如加载期诊断）也能做同一套脱敏。
// 依赖 Bindings 的路径请用 Bindings.Mask（句柄与秘密一并抹掉）。
func RedactSecrets(text string, secrets []string) string {
	for _, s := range secrets {
		if len(s) < MinSecretLenForRedact {
			continue
		}
		text = strings.ReplaceAll(text, s, MaskedSecret)
	}
	return text
}
