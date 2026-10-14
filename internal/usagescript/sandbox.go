package usagescript

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/dop251/goja"

	"github.com/peterplaykkk/yzagmgr/internal/apierr"
)

// ── 限额与沙箱 ──────────────────────────────────────────────────────────────

// SandboxOptions 覆盖限额；零值即契约默认（5s / 16 MiB / 256 KiB，UC-016 §10）。
type SandboxOptions struct {
	Timeout      time.Duration
	HeapLimit    int64
	CallDepth    int
	ScriptLimit  int
	ResultLimit  int
	PollInterval time.Duration
}

// Sandbox 是 goja 运行时的封装：每次执行建独立实例，带超时 / 堆 / 栈三重护栏。
type Sandbox struct {
	timeout      time.Duration
	heapLimit    int64
	callDepth    int
	scriptLimit  int
	resultLimit  int
	pollInterval time.Duration
}

// NewSandbox 构造沙箱（零值选项即契约默认）。
func NewSandbox(opts SandboxOptions) *Sandbox {
	s := &Sandbox{
		timeout:      opts.Timeout,
		heapLimit:    opts.HeapLimit,
		callDepth:    opts.CallDepth,
		scriptLimit:  opts.ScriptLimit,
		resultLimit:  opts.ResultLimit,
		pollInterval: opts.PollInterval,
	}
	if s.timeout <= 0 {
		s.timeout = DefaultTimeout
	}
	if s.heapLimit <= 0 {
		s.heapLimit = MaxHeapBytes
	}
	if s.callDepth <= 0 {
		s.callDepth = MaxCallDepth
	}
	if s.scriptLimit <= 0 {
		s.scriptLimit = MaxScriptBytes
	}
	if s.resultLimit <= 0 {
		s.resultLimit = MaxResultBytes
	}
	if s.pollInterval <= 0 {
		s.pollInterval = heapPollInterval
	}
	return s
}

// Plan 是脚本一次执行的结果。
//
// extract 的取值语义（本包定义；doc 只固定了形状 `extract:{...}`）：
//
//   - 以 `$.` 开头的字符串是**响应 JSON 的路径**（`$.data.quota.used_percent`，
//     支持点号与 `[n]` 下标）；其它标量按字面量处理；
//   - 额度字段：`name`（默认 credits）、`usedPercent`（必填，数字）、
//     `resetsAt`（RFC3339 字符串或 epoch 秒数）；其余字段原样回显给界面。
//
// 路径求值刻意放在宿主侧（Go）而不是 JS：响应是外部不可信数据，宿主侧的
// 求值不存在「用污染过的原型重新定义取值器」这条路。
type Plan struct {
	Request RequestSpec
	Extract map[string]any
	// bindings 是本次注入的台账（句柄 ↔ 秘密；秘密绝不进脚本），
	// 只用于宿主侧代入与脱敏，不进任何响应。
	bindings *Bindings
}

// redact 把句柄与真实秘密从错误面抹掉（A1：两者都不落日志、不进响应）。
func (p *Plan) redact(se *ScriptError) *ScriptError {
	if se == nil || p == nil {
		return se
	}
	se.Message = bounded(p.bindings.Mask(se.Message), MaxErrorMessageBytes)
	return se
}

// heapSentinel / timeoutSentinel 是 Interrupt 的判别值。
var (
	timeoutSentinel = errors.New("yzagmgr: usage script timeout")
	heapSentinel    = errors.New("yzagmgr: usage script heap limit")
)

// Run 在沙箱里执行脚本，得到它构造的请求与提取规则。
//
// 执行顺序：长度上限 → 模板注入（秘密槽位注入不透明引用，非秘密槽位注入真实值）
// → 编译 → 受限执行 → 返回值序列化（有界）→ 形状校验。任何一步失败都返回
// 有界、可诊断的 *ScriptError（A7/A9），且错误消息一律过 Bindings.Mask。
func (s *Sandbox) Run(script string, vars Vars) (*Plan, *ScriptError) {
	if len(script) > s.scriptLimit {
		return nil, scriptErr(apierr.CodeScriptLimitExceeded, StageCompile,
			fmt.Sprintf("脚本长度 %d 字节超过上限 %d 字节", len(script), s.scriptLimit), 0)
	}
	src, bindings, err := Substitute(script, vars)
	if err != nil {
		msg := err.Error()
		if bindings != nil {
			msg = bindings.Mask(msg)
		}
		// 随机源故障是宿主侧问题（internal），其余是脚本/模板问题（shape）。
		if errors.Is(err, errRandomSource) {
			return nil, scriptErr(apierr.CodeInternal, StageTemplate, msg, 0)
		}
		return nil, scriptErr(apierr.CodeInvalidScriptShape, StageTemplate, msg, 0)
	}

	prog, cerr := goja.Compile(sourceName, src, false)
	if cerr != nil {
		return nil, compileErr(cerr, bindings)
	}

	blob, serr := s.execute(prog, bindings)
	if serr != nil {
		return nil, serr
	}

	plan, serr := parsePlan(blob)
	if serr != nil {
		// 形状错误的消息只提字段名，仍统一过一遍脱敏（纵深防御）。
		serr.Message = bounded(bindings.Mask(serr.Message), MaxErrorMessageBytes)
		return nil, serr
	}
	plan.bindings = bindings
	return plan, nil
}

// execute prepares a fresh runtime, runs the program with all three guards and
// returns the bounded JSON serialization of its result.
func (s *Sandbox) execute(prog *goja.Program, bindings *Bindings) ([]byte, *ScriptError) {
	// 每次执行独立实例：被污染的原型链、被改写的内建都留在这一次里。
	vm := goja.New()
	vm.SetMaxCallStackSize(s.callDepth)

	// 堆看门狗：goja 没有内存上限旋钮，只能看进程堆增量。
	//
	// 诚实说明其边界：这是**启发式**——它拦得住「循环里不断分配」这类真实
	// DoS（本用例要防的就是这个），拦不住「一次分配 1 GiB」这种单次巨量分配
	// （下一轮解释器检查点才会被中断）；并发负载也可能让它误报成限额错误。
	// 确定性的护栏是超时、脚本长度上限与返回值上限，这一条是它们的补充。
	base := heapAlloc()
	stop := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		ticker := time.NewTicker(s.pollInterval)
		defer ticker.Stop()
		for {
			select {
			case <-stop:
				return
			case <-ticker.C:
				if heapAlloc()-base > s.heapLimit {
					vm.Interrupt(heapSentinel)
					return
				}
			}
		}
	}()

	timer := time.AfterFunc(s.timeout, func() { vm.Interrupt(timeoutSentinel) })
	val, err := vm.RunProgram(prog)
	timer.Stop()
	close(stop)
	wg.Wait()
	// 看门狗可能在 RunProgram 返回后的一瞬才打上中断标记；清掉它，
	// 免得下面的 JSON.stringify 被误中断。
	vm.ClearInterrupt()

	if err != nil {
		return nil, classifyRunError(err, bindings)
	}
	return s.marshalResult(vm, val, bindings)
}

// marshalResult 在沙箱内序列化返回值，并按上限拒绝超大结果（A10）。
func (s *Sandbox) marshalResult(vm *goja.Runtime, val goja.Value, bindings *Bindings) ([]byte, *ScriptError) {
	if val == nil || goja.IsUndefined(val) || goja.IsNull(val) {
		return nil, scriptErr(apierr.CodeInvalidScriptShape, StageShape,
			"脚本没有返回值：最后一个表达式必须是 {request:{method,url}, extract:{...}} 对象", 0)
	}
	jsonVal := vm.GlobalObject().Get("JSON")
	jsonObj, ok := jsonVal.(*goja.Object)
	if !ok {
		return nil, scriptErr(apierr.CodeInternal, StageShape, "JSON 全局对象缺失", 0)
	}
	stringify, ok := goja.AssertFunction(jsonObj.Get("stringify"))
	if !ok {
		return nil, scriptErr(apierr.CodeInternal, StageShape, "JSON.stringify 不可调用", 0)
	}
	out, err := stringify(goja.Undefined(), val)
	if err != nil {
		return nil, scriptErr(apierr.CodeInvalidScriptShape, StageShape,
			bindings.Mask("返回值无法序列化为 JSON: "+err.Error()), 0)
	}
	blob := []byte(out.String())
	if len(blob) > s.resultLimit {
		return nil, scriptErr(apierr.CodeScriptLimitExceeded, StageShape,
			fmt.Sprintf("脚本返回值 %d 字节超过上限 %d 字节", len(blob), s.resultLimit), 0)
	}
	return blob, nil
}

// classifyRunError 把 goja 的执行失败翻译成契约错误码。
//
// 码的选择：超时 → script_timeout；栈/堆越界 → script_limit_exceeded；
// JS 自身抛错（含语法以外的运行时错）→ invalid_script_shape（封闭集合里
// 没有更贴的「脚本本身不合法」码，具体是编译期还是运行期由 stage 区分）；
// 既不是 JS 异常也不是中断的（VM 级异常）→ internal。
func classifyRunError(err error, bindings *Bindings) *ScriptError {
	var interrupted *goja.InterruptedError
	if errors.As(err, &interrupted) {
		switch interrupted.Value() {
		case timeoutSentinel:
			return scriptErr(apierr.CodeScriptTimeout, StageRun,
				fmt.Sprintf("脚本执行超过 %s 被中断", DefaultTimeout), 0)
		case heapSentinel:
			return scriptErr(apierr.CodeScriptLimitExceeded, StageRun,
				"脚本堆占用超过上限被中断", 0)
		default:
			return scriptErr(apierr.CodeScriptTimeout, StageRun, "脚本执行被中断", 0)
		}
	}
	var overflow *goja.StackOverflowError
	if errors.As(err, &overflow) {
		return scriptErr(apierr.CodeScriptLimitExceeded, StageRun, "脚本调用深度超过上限", 0)
	}
	var ex *goja.Exception
	if errors.As(err, &ex) {
		msg := ex.Value().String()
		return scriptErr(apierr.CodeInvalidScriptShape, StageRun,
			bindings.Mask(msg), exceptionLine(ex))
	}
	return scriptErr(apierr.CodeInternal, StageRun, "脚本运行时异常（非 JS 异常）", 0)
}

// exceptionLine 取 JS 异常在脚本里的行号（拿不到返回 0）。
func exceptionLine(ex *goja.Exception) int {
	stack := ex.Stack()
	if len(stack) == 0 {
		return 0
	}
	return stack[0].Position().Line
}

// compileErr 把编译失败翻成有界、含行号的错误。
//
// goja 把 parser.Error 包进 CompilerSyntaxError 时**丢掉了 File 指针**
// （实测 File==nil，调 Position() 会 panic），行号只剩消息里的 "Line N:C"。
func compileErr(err error, bindings *Bindings) *ScriptError {
	msg := bindings.Mask(err.Error())
	return scriptErr(apierr.CodeInvalidScriptShape, StageCompile, msg, lineFromMessage(msg))
}

func lineFromMessage(msg string) int {
	i := strings.Index(msg, "Line ")
	if i < 0 {
		return 0
	}
	j := i + len("Line ")
	start := j
	for j < len(msg) && msg[j] >= '0' && msg[j] <= '9' {
		j++
	}
	if j == start {
		return 0
	}
	n, err := strconv.Atoi(msg[start:j])
	if err != nil {
		return 0
	}
	return n
}

// ── 形状校验 ────────────────────────────────────────────────────────────────

// allowedMethods 是允许脚本使用的 HTTP 方法（封闭集合：CONNECT/TRACE 之类
// 的攻击面方法一律拒绝）。
var allowedMethods = map[string]bool{
	"GET": true, "POST": true, "PUT": true, "PATCH": true, "DELETE": true, "HEAD": true, "OPTIONS": true,
}

func parsePlan(blob []byte) (*Plan, *ScriptError) {
	dec := json.NewDecoder(bytes.NewReader(blob))
	dec.UseNumber()
	var obj map[string]any
	if err := dec.Decode(&obj); err != nil {
		return nil, scriptErr(apierr.CodeInvalidScriptShape, StageShape,
			"脚本返回值不是 JSON 对象", 0)
	}

	reqRaw, ok := obj["request"].(map[string]any)
	if !ok {
		return nil, scriptErr(apierr.CodeInvalidScriptShape, StageShape,
			"缺少 request 对象（形状：{request:{method,url,headers?,body?}, extract:{...}}）", 0)
	}
	method, _ := reqRaw["method"].(string)
	method = strings.ToUpper(strings.TrimSpace(method))
	if method == "" {
		return nil, scriptErr(apierr.CodeInvalidScriptShape, StageShape, "request.method 缺失", 0)
	}
	if !allowedMethods[method] {
		return nil, scriptErr(apierr.CodeInvalidScriptShape, StageShape,
			fmt.Sprintf("不支持的 HTTP 方法 %q", method), 0)
	}
	rawURL, _ := reqRaw["url"].(string)
	if strings.TrimSpace(rawURL) == "" {
		return nil, scriptErr(apierr.CodeInvalidScriptShape, StageShape, "request.url 缺失", 0)
	}

	headers := map[string]string{}
	if h, present := reqRaw["headers"]; present && h != nil {
		hm, ok := h.(map[string]any)
		if !ok {
			return nil, scriptErr(apierr.CodeInvalidScriptShape, StageShape,
				"request.headers 必须是字符串映射", 0)
		}
		if len(hm) > MaxHeaders {
			return nil, scriptErr(apierr.CodeScriptLimitExceeded, StageShape,
				fmt.Sprintf("请求头数量 %d 超过上限 %d", len(hm), MaxHeaders), 0)
		}
		for k, v := range hm {
			sv, ok := v.(string)
			if !ok {
				return nil, scriptErr(apierr.CodeInvalidScriptShape, StageShape,
					fmt.Sprintf("请求头 %q 的值必须是字符串", k), 0)
			}
			if !validHeaderName(k) {
				return nil, scriptErr(apierr.CodeInvalidScriptShape, StageShape,
					fmt.Sprintf("请求头名不合法: %q", k), 0)
			}
			if strings.ContainsAny(sv, "\r\n") {
				// 换行进请求头就是头注入：宿主代发前必须挡住。
				return nil, scriptErr(apierr.CodeInvalidScriptShape, StageShape,
					fmt.Sprintf("请求头 %q 的值含换行", k), 0)
			}
			headers[k] = sv
		}
	}

	body := ""
	if b, present := reqRaw["body"]; present && b != nil {
		sb, ok := b.(string)
		if !ok {
			return nil, scriptErr(apierr.CodeInvalidScriptShape, StageShape, "request.body 必须是字符串", 0)
		}
		if len(sb) > MaxBodyBytes {
			return nil, scriptErr(apierr.CodeScriptLimitExceeded, StageShape,
				fmt.Sprintf("请求体 %d 字节超过上限 %d 字节", len(sb), MaxBodyBytes), 0)
		}
		body = sb
	}

	extract, ok := obj["extract"].(map[string]any)
	if !ok {
		return nil, scriptErr(apierr.CodeInvalidScriptShape, StageShape,
			"缺少 extract 对象（至少给出 usedPercent）", 0)
	}
	if len(extract) > MaxExtractFields {
		return nil, scriptErr(apierr.CodeScriptLimitExceeded, StageShape,
			fmt.Sprintf("extract 字段数 %d 超过上限 %d", len(extract), MaxExtractFields), 0)
	}

	return &Plan{
		Request: RequestSpec{Method: method, URL: strings.TrimSpace(rawURL), Headers: headers, Body: body},
		Extract: extract,
	}, nil
}

func validHeaderName(name string) bool {
	if name == "" || len(name) > 128 {
		return false
	}
	for i := 0; i < len(name); i++ {
		c := name[i]
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9':
		case c == '-', c == '_', c == '.', c == ':', c == '+', c == '*', c == '!', c == '#',
			c == '$', c == '%', c == '&', c == '\'', c == '^', c == '`', c == '|', c == '~':
		default:
			return false
		}
	}
	return true
}

// ── 提取 ────────────────────────────────────────────────────────────────────

// Extract 按 extract 的路径规则在响应正文上求值。
//
// 取不到的字段不会伪造值：只要 usedPercent 取不到就报 invalid_script_shape，
// 并把缺失路径（含「响应不是合法 JSON」这类原因）写进消息，便于诊断（A9）。
func Extract(plan *Plan, body []byte) (map[string]any, *ScriptError) {
	if plan == nil {
		return nil, scriptErr(apierr.CodeInternal, StageExtract, "缺少脚本执行结果", 0)
	}
	var root any
	var parseErr error
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.UseNumber()
	if err := dec.Decode(&root); err != nil {
		parseErr = err
	}

	out := map[string]any{}
	var missing []string
	for _, key := range sortedKeys(plan.Extract) {
		v := plan.Extract[key]
		path, isPath := v.(string)
		if !isPath || !strings.HasPrefix(path, "$.") {
			out[key] = v
			continue
		}
		if parseErr != nil {
			missing = append(missing, fmt.Sprintf("%s（响应不是合法 JSON）", path))
			continue
		}
		got, ok := lookupPath(root, path)
		if !ok {
			missing = append(missing, path)
			continue
		}
		out[key] = got
	}
	if len(missing) > 0 {
		// 诊断字段（下划线开头）：界面可展示「哪些路径没取到」，额度取值只认
		// 非诊断字段。
		out["_missing"] = missing
		if _, ok := out["usedPercent"]; !ok {
			return out, scriptErr(apierr.CodeInvalidScriptShape, StageExtract,
				"提取路径取不到值: "+strings.Join(missing, "、"), 0)
		}
	}
	return out, nil
}

// ToQuotaWindow 把提取结果落成 QuotaWindow，来源恒标注为 script（A11）。
//
// 绝对量（used / limit / unit）是**可选**的：脚本只给出百分比时它们缺席，
// 本函数绝不据百分比反推绝对量（UC-012 A3、契约 §3）；反过来，脚本只给出
// used+limit 时百分比可以由绝对量算出（这是据实计算，不是编造）。
func ToQuotaWindow(extracted map[string]any) (*QuotaWindow, *ScriptError) {
	w := &QuotaWindow{Name: "credits", Source: SourceScript}
	if name, ok := extracted["name"].(string); ok && strings.TrimSpace(name) != "" {
		w.Name = strings.TrimSpace(name)
	}
	if raw, ok := extracted["used"]; ok && raw != nil {
		v, err := asFloat(raw)
		if err != nil {
			return nil, scriptErr(apierr.CodeInvalidScriptShape, StageExtract,
				"extract.used 不合法: "+err.Error(), 0)
		}
		if v < 0 {
			return nil, scriptErr(apierr.CodeInvalidScriptShape, StageExtract,
				"extract.used 不能是负数", 0)
		}
		w.Used = &v
	}
	if raw, ok := extracted["limit"]; ok && raw != nil {
		v, err := asFloat(raw)
		if err != nil {
			return nil, scriptErr(apierr.CodeInvalidScriptShape, StageExtract,
				"extract.limit 不合法: "+err.Error(), 0)
		}
		if v < 0 {
			return nil, scriptErr(apierr.CodeInvalidScriptShape, StageExtract,
				"extract.limit 不能是负数", 0)
		}
		w.Limit = &v
	}
	if unit, ok := extracted["unit"].(string); ok && strings.TrimSpace(unit) != "" {
		w.Unit = strings.TrimSpace(unit)
	}
	if raw, ok := extracted["resetsAt"]; ok && raw != nil {
		ts, err := asTime(raw)
		if err != nil {
			return nil, scriptErr(apierr.CodeInvalidScriptShape, StageExtract,
				"extract.resetsAt 不合法: "+err.Error(), 0)
		}
		w.ResetsAt = &ts
	}

	if raw, ok := extracted["usedPercent"]; ok && raw != nil {
		pct, err := asFloat(raw)
		if err != nil {
			return nil, scriptErr(apierr.CodeInvalidScriptShape, StageExtract,
				"extract.usedPercent 不合法: "+err.Error(), 0)
		}
		w.UsedPercent = pct
		return w, nil
	}
	if w.Used != nil && w.Limit != nil && *w.Limit > 0 {
		w.UsedPercent = *w.Used / *w.Limit * 100
		return w, nil
	}
	return nil, scriptErr(apierr.CodeInvalidScriptShape, StageExtract,
		"extract.usedPercent 缺失（要么给出百分比，要么同时给出 used 与正数 limit）", 0)
}

func sortedKeys(m map[string]any) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// lookupPath 在解码后的 JSON 上求 `$.a.b[2].c` 的值。
func lookupPath(root any, path string) (any, bool) {
	rest := strings.TrimPrefix(path, "$")
	rest = strings.TrimPrefix(rest, ".")
	if rest == "" {
		return root, true
	}
	cur := root
	for _, raw := range strings.Split(rest, ".") {
		if raw == "" {
			return nil, false
		}
		name := raw
		var indexes []int
		for {
			open := strings.IndexByte(name, '[')
			if open < 0 {
				break
			}
			closeIdx := strings.IndexByte(name[open:], ']')
			if closeIdx < 0 {
				return nil, false
			}
			n, err := strconv.Atoi(strings.TrimSpace(name[open+1 : open+closeIdx]))
			if err != nil || n < 0 {
				return nil, false
			}
			indexes = append(indexes, n)
			name = name[:open] + name[open+closeIdx+1:]
		}
		if name != "" {
			obj, ok := cur.(map[string]any)
			if !ok {
				return nil, false
			}
			v, ok := obj[name]
			if !ok {
				return nil, false
			}
			cur = v
		}
		for _, n := range indexes {
			arr, ok := cur.([]any)
			if !ok || n >= len(arr) {
				return nil, false
			}
			cur = arr[n]
		}
	}
	return cur, true
}

func asFloat(v any) (float64, error) {
	var f float64
	switch t := v.(type) {
	case json.Number:
		got, err := t.Float64()
		if err != nil {
			return 0, errors.New("不是数字")
		}
		f = got
	case float64:
		f = t
	case int64:
		f = float64(t)
	case string:
		got, err := strconv.ParseFloat(strings.TrimSpace(t), 64)
		if err != nil {
			return 0, errors.New("不是数字")
		}
		f = got
	default:
		return 0, fmt.Errorf("类型 %T 不是数字", v)
	}
	if math.IsNaN(f) || math.IsInf(f, 0) {
		return 0, errors.New("不是有限数字")
	}
	return f, nil
}

func asTime(v any) (string, error) {
	switch t := v.(type) {
	case string:
		s := strings.TrimSpace(t)
		if s == "" {
			return "", errors.New("空时间")
		}
		if ts, err := time.Parse(time.RFC3339, s); err == nil {
			return ts.Format(time.RFC3339), nil
		}
		if n, err := strconv.ParseFloat(s, 64); err == nil {
			return epochToRFC3339(n)
		}
		return "", errors.New("不是 RFC3339 时间")
	case json.Number:
		n, err := t.Float64()
		if err != nil {
			return "", errors.New("不是 RFC3339 时间")
		}
		return epochToRFC3339(n)
	case float64:
		return epochToRFC3339(t)
	case int64:
		return epochToRFC3339(float64(t))
	default:
		return "", fmt.Errorf("类型 %T 不是时间", v)
	}
}

func epochToRFC3339(f float64) (string, error) {
	if math.IsNaN(f) || math.IsInf(f, 0) || f <= 0 || f > 32503680000 { // 32503680000 = 3000-01-01
		return "", errors.New("epoch 秒数不合理")
	}
	sec := int64(f)
	nsec := int64((f - float64(sec)) * 1e9)
	return time.Unix(sec, nsec).UTC().Format(time.RFC3339), nil
}

// ── 宿主代发 ────────────────────────────────────────────────────────────────

// Response 是宿主拿到的响应（正文已按上限截断）。
type Response struct {
	Status    int
	Body      string
	Truncated bool
}

// Fetcher 抽象「宿主代发」，便于测试注入（本包不自己决定出网策略）。
//
// guard 是**逐跳守卫**：代发器必须在每一个跳转目标上调用它，返回错误即拒绝
// 该跳（跳转逐跳重新校验，跳到未授权域即拒——UC-016 修订版 A15）。入口 URL
// 由调用方在 mediate 里校验；guard 为 nil 时实现可退到自己的默认策略。
type Fetcher interface {
	Do(req RequestSpec, guard func(rawURL string) error) (Response, error)
}

// Resolver 是「域名 → 地址」的解析抽象（解析期防护的可测缝）。
// net 包的默认解析器经 defaultResolver 适配（含 /etc/hosts 与系统解析语义）。
type Resolver interface {
	LookupNetIP(ctx context.Context, host string) ([]netip.Addr, error)
}

// defaultResolver 是系统解析器适配。
type defaultResolver struct{}

func (defaultResolver) LookupNetIP(ctx context.Context, host string) ([]netip.Addr, error) {
	return net.DefaultResolver.LookupNetIP(ctx, "ip", host)
}

// HTTPFetcher 是默认代发实现。
//
// 入口 URL 已由 Runner 经 mediate 校验（https + 非内网 + 已批准供应商 origin）；
// 本类型额外在**每一个跳转目标**上调用逐跳守卫——否则一个外站 302 就能把请求
// 引到元数据地址或未授权域（经典的 SSRF/外传绕过）。
//
// 拨号层还有一道**解析期防护**（UC-016 边界：「DNS 解析到内网或跳往未授权
// 公网域均拒绝」）：主机名在拨号前解析，解析结果命中内网 / 回环 / 链路本地 /
// 元数据等网段即拒——「已批准域名」不豁免这一层，否则一个把内网地址写进
// DNS 的被批准供应商就是从批准面内部打进来的 SSRF。
type HTTPFetcher struct {
	Client *http.Client
	// MaxBytes 是正文读取上限（默认 MaxResponseBytes）。
	MaxBytes int
	// URLGuard 是兜底的跳转守卫；Do 的 guard 参数为 nil 时用它，再为 nil 时
	// 用包默认策略 ValidateRequestURL（不含批准供应商判定）。
	URLGuard func(raw string) error
	// MaxRedirects 是允许的跳转次数（默认 3）。
	MaxRedirects int
	// Resolver 是解析期防护的解析器（nil = 系统解析器）。测试用 stub 注入。
	Resolver Resolver
	// DialContext 是底层拨号的注入缝（nil = 内建拨号）。解析校验通过之后才
	// 会走到它；测试用计数拨号做「0 出网」判据与放行对照。
	DialContext func(ctx context.Context, network, addr string) (net.Conn, error)

	// trOnce / trCached 缓存内建（Client 无传输层或标准 Transport 时）装好
	// 解析期防护的传输：同一 fetcher 的多次代发共用连接池。
	trOnce   sync.Once
	trCached http.RoundTripper
}

// Do 代发一次请求；guard 在每个跳转目标上被调用（nil 走兜底策略）。
func (h *HTTPFetcher) Do(req RequestSpec, guard func(raw string) error) (Response, error) {
	maxBytes := h.MaxBytes
	if maxBytes <= 0 {
		maxBytes = MaxResponseBytes
	}
	hop := guard
	if hop == nil {
		hop = h.URLGuard
	}
	if hop == nil {
		hop = func(raw string) error {
			if ae := ValidateRequestURL(raw); ae != nil {
				return ae
			}
			return nil
		}
	}
	maxRedirects := h.MaxRedirects
	if maxRedirects <= 0 {
		maxRedirects = 3
	}
	client := h.Client
	if client == nil {
		client = &http.Client{Timeout: 10 * time.Second}
	} else {
		cp := *client
		client = &cp
	}
	// 解析期防护：装进传输层的拨号缝。跳转的每一跳都走同一条拨号路径，
	// 因此每个跳转目标（域名）的解析结果都被校验。
	if client.Transport == nil {
		// 缺省传输走缓存（同一 fetcher 共用连接池）。
		h.trOnce.Do(func() { h.trCached = h.transportWithResolveGuard(nil) })
		client.Transport = h.trCached
	} else {
		client.Transport = h.transportWithResolveGuard(client.Transport)
	}
	client.CheckRedirect = func(next *http.Request, via []*http.Request) error {
		if len(via) >= maxRedirects {
			return fmt.Errorf("跳转次数超过 %d", maxRedirects)
		}
		if err := hop(next.URL.String()); err != nil {
			return err
		}
		// 跨 origin 跳转剥掉凭据头：Go 只自动剥 Authorization 等四个敏感头，
		// 批准位置里的自定义凭据头（X-Api-Key 等）不在其列——秘密只能发往它
		// 被批准（并被代入）的那一个 origin，跳转不得把它中继给别的 origin，
		// 哪怕那个 origin 也在目标白名单里。
		if len(via) > 0 && originOf(next.URL) != originOf(via[len(via)-1].URL) {
			stripCredentialHeaders(next.Header)
		}
		return nil
	}

	var body io.Reader
	if req.Body != "" {
		body = strings.NewReader(req.Body)
	}
	httpReq, err := http.NewRequest(req.Method, req.URL, body)
	if err != nil {
		// 不回显 err：url.Error 会带上整条 URL（路径/查询串里可能有凭据引用）。
		return Response{}, errors.New("构造请求失败（URL 非法）")
	}
	for k, v := range req.Headers {
		httpReq.Header.Set(k, v)
	}
	resp, err := client.Do(httpReq)
	if err != nil {
		return Response{}, err
	}
	defer func() { _ = resp.Body.Close() }()

	buf, err := io.ReadAll(io.LimitReader(resp.Body, int64(maxBytes)+1))
	if err != nil {
		return Response{}, fmt.Errorf("读取响应失败")
	}
	truncated := len(buf) > maxBytes
	if truncated {
		buf = buf[:maxBytes]
	}
	return Response{Status: resp.StatusCode, Body: string(buf), Truncated: truncated}, nil
}

// transportWithResolveGuard 给传输层装上解析期校验（UC-016 边界：DNS 解析到
// 内网也拒绝）。
//
//   - nil：以 http.DefaultTransport 为底克隆（代理 / TLS / 连接池设置与默认
//     语义一致），拨号换成带解析校验的内建实现——校验后直接连**已校验的
//     IP**，不再按域名二次解析（消除「第二次解析换地址」的 TOCTOU 机会）；
//   - *http.Transport：克隆后安装（不改变调用方对象）；它原有的拨号实现作为
//     底层保留（校验通过后把原名地址交给它），DialTLSContext 非 nil 时同样
//     穿一道校验（https 走 DialTLSContext、会绕过 DialContext）；
//   - 其它 RoundTripper（测试桩 / 自定义传输）：原样返回——它们不经过拨号层，
//     无法注入；生产装配（Client 缺省或标准 Transport）不走这条路。
func (h *HTTPFetcher) transportWithResolveGuard(base http.RoundTripper) http.RoundTripper {
	switch t := base.(type) {
	case nil:
		var cp *http.Transport
		if dt, ok := http.DefaultTransport.(*http.Transport); ok {
			cp = dt.Clone()
		} else {
			cp = &http.Transport{}
		}
		cp.DialContext = h.checkedDialContext()
		return cp
	case *http.Transport:
		cp := t.Clone()
		switch {
		case cp.DialTLSContext != nil:
			orig := cp.DialTLSContext
			cp.DialTLSContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
				if _, _, ae := h.resolveAndCheck(ctx, addr); ae != nil {
					return nil, ae
				}
				return orig(ctx, network, addr)
			}
		case cp.DialContext != nil:
			orig := cp.DialContext
			cp.DialContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
				if _, _, ae := h.resolveAndCheck(ctx, addr); ae != nil {
					return nil, ae
				}
				return orig(ctx, network, addr)
			}
		default:
			cp.DialContext = h.checkedDialContext()
		}
		return cp
	default:
		return base
	}
}

// checkedDialContext 返回带解析期校验的拨号函数。
//
// 解析校验通过后：DialContext 注入缝非 nil 时把**原名地址**交给它（测试缝）；
// 否则内建拨号直接连已校验的 IP。
func (h *HTTPFetcher) checkedDialContext() func(ctx context.Context, network, addr string) (net.Conn, error) {
	return func(ctx context.Context, network, addr string) (net.Conn, error) {
		port, ips, ae := h.resolveAndCheck(ctx, addr)
		if ae != nil {
			return nil, ae
		}
		if h.DialContext != nil {
			return h.DialContext(ctx, network, addr)
		}
		d := &net.Dialer{Timeout: 30 * time.Second, KeepAlive: 30 * time.Second}
		var lastErr error
		for _, ip := range ips {
			conn, err := d.DialContext(ctx, network, net.JoinHostPort(ip.String(), port))
			if err == nil {
				return conn, nil
			}
			lastErr = err
		}
		return nil, lastErr
	}
}

// resolveAndCheck 解析拨号地址里的主机名并做解析期校验。
//
//   - 主机是 IP 字面量：不走解析——字面量的网段黑名单判定收在 URL 层
//     （ValidateRequestURL / HostIsBlocked 在入口与每个跳转目标上执行），
//     拨号层只补「解析期」这一环；既有字面量回环夹具（直连本层的测试）
//     因此零回归。
//   - 域名：用 Resolver（nil = 系统解析器）解析，**任一**结果命中内网 /
//     回环 / 链路本地 / 元数据等网段即拒（fail-closed；blocked_target，
//     消息含「解析到内网」，可诊断）。
//
// 返回校验通过的端口与地址列表（成功时非空）。解析失败是网络层问题
// （gateway_unreachable），不是「被拒」；解析结果为内网才是拒绝面。
func (h *HTTPFetcher) resolveAndCheck(ctx context.Context, addr string) (port string, ips []netip.Addr, ae *apierr.Error) {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return "", nil, apierr.New(apierr.CodeInvalidURL, "拨号地址不合法")
	}
	if ip, perr := netip.ParseAddr(host); perr == nil {
		return port, []netip.Addr{ip}, nil
	}
	resolver := h.Resolver
	if resolver == nil {
		resolver = defaultResolver{}
	}
	addrs, rerr := resolver.LookupNetIP(ctx, host)
	if rerr != nil {
		return "", nil, apierr.New(apierr.CodeGatewayUnreachable,
			"请求目标域名无法解析：%s", host)
	}
	if len(addrs) == 0 {
		return "", nil, apierr.New(apierr.CodeGatewayUnreachable,
			"请求目标域名没有解析结果：%s", host)
	}
	for _, ip := range addrs {
		if ipIsBlocked(ip) {
			return "", nil, apierr.New(apierr.CodeBlockedTarget,
				"请求目标解析到内网 / 回环 / 链路本地地址，已拒绝（DNS 解析期防护）：%s", host)
		}
	}
	return port, addrs, nil
}

// stripCredentialHeaders 删掉请求里的凭据位请求头（批准位置清单的同一集合）。
func stripCredentialHeaders(h http.Header) {
	for _, name := range credentialHeaderAllowList {
		h.Del(name)
	}
}

// ── 执行器 ──────────────────────────────────────────────────────────────────

// RunnerOptions 装配执行器；零值即生产默认（契约沙箱 + HTTP 代发）。
type RunnerOptions struct {
	Sandbox       *Sandbox
	Fetcher       Fetcher
	ResponseLimit int
	ExcerptLimit  int
}

// Runner 串起「沙箱 → 校验 → 代发 → 提取」。
type Runner struct {
	sandbox       *Sandbox
	fetcher       Fetcher
	responseLimit int
	excerptLimit  int
}

// NewRunner 构造执行器。
func NewRunner(opts RunnerOptions) *Runner {
	r := &Runner{
		sandbox:       opts.Sandbox,
		fetcher:       opts.Fetcher,
		responseLimit: opts.ResponseLimit,
		excerptLimit:  opts.ExcerptLimit,
	}
	if r.sandbox == nil {
		r.sandbox = NewSandbox(SandboxOptions{})
	}
	if r.fetcher == nil {
		r.fetcher = &HTTPFetcher{}
	}
	if r.responseLimit <= 0 {
		r.responseLimit = MaxResponseBytes
	}
	if r.excerptLimit <= 0 {
		r.excerptLimit = MaxExcerptBytes
	}
	return r
}

// Test 执行一次试运行（POST …/usage-script/test）。
//
// 零持久化：本方法不碰盘，不写脚本、不写用量库、不改配置（A8）。
func (r *Runner) Test(script string, vars Vars, policy Policy) (TestResult, error) {
	out := r.run(script, vars, policy)
	return out.testResult(policy), nil
}

// Usage 执行一次取数（POST …/usage），把提取结果落成额度窗口。
//
// 失败时 quota 恒为 null 且带 error：绝不把「取不到」写成 0 额度（A12 与
// UC-016 边界条「不缓存为 0」）。
func (r *Runner) Usage(script string, vars Vars, policy Policy) (UsageResult, error) {
	out := r.run(script, vars, policy)
	if out.err != nil {
		return UsageResult{Quota: nil, Source: SourceScript, Error: out.err}, nil
	}
	quota, serr := ToQuotaWindow(out.extracted)
	if serr != nil {
		return UsageResult{Quota: nil, Source: SourceScript, Error: serr}, nil
	}
	return UsageResult{Quota: quota, Source: SourceScript}, nil
}

// FetchQuotaFor 按档取数：没有脚本时**不出网**、不伪造 0 额度，直接给出
// `{quota:null, source:"script"}`（A12）。
func (r *Runner) FetchQuotaFor(store *Store, toolID, profileID string, vars Vars, policy Policy) (UsageResult, error) {
	if store == nil {
		return UsageResult{}, apierr.New(apierr.CodeInvalidRequest, "缺少脚本存储")
	}
	saved, ok, err := store.Get(toolID, profileID)
	if err != nil {
		return UsageResult{}, err
	}
	if !ok {
		return UsageResult{Quota: nil, Source: SourceScript}, nil
	}
	return r.Usage(saved.Script, vars, policy)
}

type runOutcome struct {
	request   RequestSpec
	response  ResponseSummary
	extracted map[string]any
	err       *ScriptError
}

// testResult 归一化试运行响应：headers 与 extracted 恒为对象（失败时也不给
// null），界面不必为「null 还是空对象」写两套分支。
//
// request 是**脱敏后**的请求（mediate 之后、秘密代入之前的显示面：句柄与秘密
// 都已换成脱敏替身）；credentialScope 是本次运行的凭据使用范围记录（A16 与
// 边界条「试运行前展示……凭据使用范围」）。
func (o runOutcome) testResult(policy Policy) TestResult {
	req := o.request
	if req.Headers == nil {
		req.Headers = map[string]string{}
	}
	extracted := o.extracted
	if extracted == nil {
		extracted = map[string]any{}
	}
	return TestResult{
		Request:         req,
		Response:        o.response,
		Extracted:       extracted,
		Error:           o.err,
		CredentialScope: policy.Scope(),
	}
}

// run 串起一次执行：沙箱 → 中介（批准目标/位置/只读 + 代入）→ 代发 → 脱敏 →
// 提取。任何拒绝都发生在发送前（上游 0 请求）。
func (r *Runner) run(script string, vars Vars, policy Policy) runOutcome {
	plan, serr := r.sandbox.Run(script, vars)
	if serr != nil {
		return runOutcome{err: serr}
	}

	// 脚本真正用到 {{baseUrl}} 时，先校验模板取值本身（A5：https，无例外）。
	if strings.Contains(script, "{{baseUrl}}") {
		if ae := ValidateBaseURL(vars.BaseURL); ae != nil {
			return runOutcome{request: maskRequest(plan.bindings, plan.Request), err: plan.redact(scriptErrFromAPIErr(ae))}
		}
	}

	outReq, guard, serr := mediate(plan, policy)
	// 展示面恒为脱敏请求：被拒时也不把句柄/秘密回显给调用方。
	out := runOutcome{request: maskRequest(plan.bindings, outReq)}
	if serr != nil {
		out.err = plan.redact(serr)
		return out
	}

	resp, err := r.fetcher.Do(outReq, guard)
	if err != nil {
		out.err = plan.redact(classifyFetchError(err))
		return out
	}
	body, truncated := truncateUTF8(resp.Body, r.responseLimit)
	// A16：响应正文先整体脱敏，再交给摘要与提取——供应商把秘密回显回来时，
	// 页面 / 复制 / 日志都不会见到明文。
	body = plan.bindings.Mask(body)
	excerpt, _ := truncateUTF8(body, r.excerptLimit)
	out.response = ResponseSummary{Status: resp.Status, BodyExcerpt: excerpt}

	extracted, serr := Extract(plan, []byte(body))
	out.extracted = maskExtracted(plan.bindings, extracted)
	if serr != nil {
		if truncated {
			serr.Message = fmt.Sprintf("%s（响应正文超过 %d 字节上限，已截断）",
				serr.Message, r.responseLimit)
		}
		out.err = plan.redact(serr)
	}
	return out
}

// scriptErrFromAPIErr 把校验错误（apierr 码）转成脚本错误面。
func scriptErrFromAPIErr(ae *apierr.Error) *ScriptError {
	if ae == nil {
		return nil
	}
	return scriptErr(ae.Code, StageRequest, ae.Message, 0)
}

// classifyFetchError 把代发失败翻成错误面。
//
// 守卫（未授权跳转等）与协议校验返回的是 *apierr.Error：保留它的错误码与
// 消息（可诊断），其余（连接失败、超时……）统一落 gateway_unreachable。
func classifyFetchError(err error) *ScriptError {
	var ae *apierr.Error
	if errors.As(err, &ae) {
		return scriptErr(ae.Code, StageRequest, ae.Message, 0)
	}
	return scriptErr(apierr.CodeGatewayUnreachable, StageRequest, fetchErrMessage(err), 0)
}

// fetchErrMessage 只保留底层原因：url.Error 的文本含**整条 URL**，
// 而 URL 的查询串里可能有凭据引用或被代入的秘密（A1）。
func fetchErrMessage(err error) string {
	var ue *url.Error
	if errors.As(err, &ue) && ue.Err != nil {
		err = ue.Err
	}
	var ae *apierr.Error
	if errors.As(err, &ae) {
		return ae.Message
	}
	return bounded(err.Error(), MaxErrorMessageBytes)
}

// ── 小节 ────────────────────────────────────────────────────────────────────

func scriptErr(code apierr.Code, stage, msg string, line int) *ScriptError {
	return &ScriptError{
		Code:    code,
		Message: bounded(msg, MaxErrorMessageBytes),
		Stage:   stage,
		Line:    line,
	}
}

// bounded 把文本截到上限（按 rune 边界，避免产出坏字节）。
func bounded(s string, limit int) string {
	out, _ := truncateUTF8(s, limit)
	return out
}

// truncateUTF8 按字节上限截断并退回最后一个完整 rune 边界。
func truncateUTF8(s string, limit int) (string, bool) {
	if len(s) <= limit {
		return s, false
	}
	cut := limit
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut], true
}

func heapAlloc() int64 {
	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)
	return int64(ms.HeapAlloc)
}
