package usagescript

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/peterplaykkk/yzagmgr/internal/apierr"
)

// ── 夹具 ────────────────────────────────────────────────────────────────────

// fakeFetcher 是假代发器：记录调用、返回预设响应，**绝不碰网络**。
type fakeFetcher struct {
	calls []RequestSpec
	// guards 记录每次代发拿到的逐跳守卫（跳转测试用）。
	guards []func(rawURL string) error
	resp   Response
	err    error
}

func (f *fakeFetcher) Do(req RequestSpec, guard func(rawURL string) error) (Response, error) {
	f.calls = append(f.calls, req)
	f.guards = append(f.guards, guard)
	if f.err != nil {
		return Response{}, f.err
	}
	return f.resp, nil
}

// testPolicy 是套件用策略：把用例里出现的目标主机登记为已批准供应商；
// 凭据代入面（CredentialOrigins）只给 provider.example（凭据用例之外的脚本
// 不携带句柄，与该面无关）。
func testPolicy() Policy {
	return Policy{
		Targets:           []string{"https://provider.example", "https://probe.example"},
		CredentialOrigins: []string{"https://provider.example"},
	}
}

// okScript 是一个形状完整、不含路径提取的脚本（出网一次就能成功）。
const okScript = `({
  request: { method: "GET", url: "https://provider.example/usage" },
  extract: { name: "5h" }
})`

func mustNoErr(t *testing.T, res TestResult) TestResult {
	t.Helper()
	if res.Error != nil {
		t.Fatalf("试运行失败: code=%s stage=%s line=%d msg=%s",
			res.Error.Code, res.Error.Stage, res.Error.Line, res.Error.Message)
	}
	return res
}

func mustScriptCode(t *testing.T, se *ScriptError, want apierr.Code) *ScriptError {
	t.Helper()
	if se == nil {
		t.Fatalf("期望脚本错误码 %s，实际无错误", want)
	}
	if se.Code != want {
		t.Fatalf("脚本错误码=%s，期望 %s（stage=%s line=%d 消息：%s）", se.Code, want, se.Stage, se.Line, se.Message)
	}
	if !se.Code.Valid() {
		t.Fatalf("错误码 %q 不在封闭集合里", se.Code)
	}
	return se
}

// probe 跑一个探针表达式，返回它的字符串值（脚本必须成功）。
func probe(t *testing.T, r *Runner, expr string) string {
	t.Helper()
	script := `({
	  request: { method: "GET", url: "https://probe.example/x" },
	  extract: { name: String(` + expr + `), usedPercent: 1 }
	})`
	res, err := r.Test(script, Vars{}, testPolicy())
	if err != nil {
		t.Fatalf("试运行返回错误: %v", err)
	}
	if res.Error != nil {
		t.Fatalf("探针 %s 意外失败: %s/%s %s", expr, res.Error.Code, res.Error.Stage, res.Error.Message)
	}
	return fmt.Sprint(res.Extracted["name"])
}

// ── UC-016 A2 ───────────────────────────────────────────────────────────────

func TestUsageScriptTimeoutIsInterrupted(t *testing.T) {
	// 契约默认值是 5s；测试用注入的短超时跑同一套中断机制，避免套件白等 5 秒。
	if DefaultTimeout != 5*time.Second {
		t.Fatalf("契约要求默认超时 5s，实际 %v", DefaultTimeout)
	}
	ff := &fakeFetcher{}
	r := NewRunner(RunnerOptions{
		Sandbox: NewSandbox(SandboxOptions{Timeout: 300 * time.Millisecond}),
		Fetcher: ff,
	})

	script := `(function(){
	  while (true) { Math.sqrt(Math.random()); }
	})();
	({ request: { method: "GET", url: "https://provider.example/usage" }, extract: {} })`

	start := time.Now()
	res, err := r.Test(script, Vars{}, testPolicy())
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("试运行返回错误: %v", err)
	}
	ae := mustScriptCode(t, res.Error, apierr.CodeScriptTimeout)
	if ae.Stage != StageRun {
		t.Fatalf("stage=%s，期望 %s", ae.Stage, StageRun)
	}
	if elapsed > 2*time.Second {
		t.Fatalf("死循环没有及时被中断：耗时 %v（超时阈值 300ms）", elapsed)
	}
	if len(ff.calls) != 0 {
		t.Fatalf("被中断的脚本不该出网，实际代发 %d 次", len(ff.calls))
	}

	// 进程没有被挂起：紧接着还能正常跑通。
	res2, err := r.Test(okScript, Vars{}, testPolicy())
	if err != nil {
		t.Fatalf("试运行返回错误: %v", err)
	}
	mustNoErr(t, res2)
	if len(ff.calls) != 1 {
		t.Fatalf("中断之后宿主应恢复工作，实际代发 %d 次", len(ff.calls))
	}
}

// ── UC-016 A3 ───────────────────────────────────────────────────────────────

func TestUsageScriptMemoryAndStackLimits(t *testing.T) {
	ff := &fakeFetcher{}
	r := NewRunner(RunnerOptions{Fetcher: ff})

	// 1) 栈：深递归（用契约默认调用深度）。堆护栏也把 goroutine 栈计进增量，
	//    所以默认配置下可能先被堆护栏拦下——两者都是 script_limit_exceeded。
	deep := `(function(){ function f(){ return f() + 1; } return f(); })();`
	res, err := r.Test(deep, Vars{}, testPolicy())
	if err != nil {
		t.Fatalf("试运行返回错误: %v", err)
	}
	ae := mustScriptCode(t, res.Error, apierr.CodeScriptLimitExceeded)
	if ae.Stage != StageRun {
		t.Fatalf("stage=%s，期望 %s", ae.Stage, StageRun)
	}

	// 1b) 把堆上限放到 1 GiB，同样的递归就必须由**调用深度**那道护栏拦下
	//     （否则「256 KiB 栈」这条就只是纸面数字）。
	deepOnly := NewRunner(RunnerOptions{
		Sandbox: NewSandbox(SandboxOptions{HeapLimit: 1 << 30}),
		Fetcher: ff,
	})
	resDeep, err := deepOnly.Test(deep, Vars{}, testPolicy())
	if err != nil {
		t.Fatalf("试运行返回错误: %v", err)
	}
	aeDeep := mustScriptCode(t, resDeep.Error, apierr.CodeScriptLimitExceeded)
	if !strings.Contains(aeDeep.Message, "调用深度") {
		t.Fatalf("大堆上限下应由调用深度护栏拦下，实际消息：%s", aeDeep.Message)
	}

	// 2) 堆：循环分配（goja 没有内存上限旋钮，靠堆增量看门狗中断）。
	r2 := NewRunner(RunnerOptions{
		Sandbox: NewSandbox(SandboxOptions{
			HeapLimit:    2 << 20,
			Timeout:      3 * time.Second,
			PollInterval: time.Millisecond,
		}),
		Fetcher: ff,
	})
	hog := `(function(){ var a = []; for (;;) { a.push(new Array(1024).fill(0)); } })()`
	start := time.Now()
	res2, err := r2.Test(hog, Vars{}, testPolicy())
	if err != nil {
		t.Fatalf("试运行返回错误: %v", err)
	}
	aeHog := mustScriptCode(t, res2.Error, apierr.CodeScriptLimitExceeded)
	if !strings.Contains(aeHog.Message, "堆") {
		t.Fatalf("循环分配应由堆护栏拦下，实际消息：%s", aeHog.Message)
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("堆看门狗没有及时生效：耗时 %v（超时阈值 3s，说明是被超时兜底而不是堆护栏拦下的）", elapsed)
	}

	// 3) 守护进程毫发无伤：超限之后照常跑通正常脚本。
	res3, err := r.Test(okScript, Vars{}, testPolicy())
	if err != nil {
		t.Fatalf("试运行返回错误: %v", err)
	}
	mustNoErr(t, res3)
	if len(ff.calls) != 1 {
		t.Fatalf("超限用例不该出网，实际代发 %d 次", len(ff.calls))
	}
}

// ── UC-016 A4 ───────────────────────────────────────────────────────────────

func TestUsageScriptHasNoHostCapabilities(t *testing.T) {
	dir := t.TempDir()
	sentinel := filepath.Join(dir, "escaped.txt")
	ff := &fakeFetcher{}
	r := NewRunner(RunnerOptions{Fetcher: ff})

	attempts := []struct {
		name   string
		script string
	}{
		{"require 起进程", `(function(){ return require("child_process").execSync("touch ` + sentinel + `"); })()`},
		{"require 读文件", `(function(){ return require("fs").readFileSync("/etc/passwd", "utf8"); })()`},
		{"process 取环境", `(function(){ return process.env; })()`},
		{"os 起进程", `(function(){ return os.system("touch ` + sentinel + `"); })()`},
		{"fetch 直连元数据", `(function(){ return fetch("http://169.254.169.254/latest/meta-data/"); })()`},
		{"XMLHttpRequest", `(function(){ var x = new XMLHttpRequest(); x.open("GET", "https://provider.example"); return x; })()`},
		{"全局 readFile", `(function(){ return readFile("/etc/passwd"); })()`},
		{"Deno 运行时", `(function(){ return Deno.readTextFileSync("/etc/passwd"); })()`},
	}
	for _, a := range attempts {
		t.Run(a.name, func(t *testing.T) {
			res, err := r.Test(a.script, Vars{}, testPolicy())
			if err != nil {
				t.Fatalf("试运行返回错误: %v", err)
			}
			if res.Error == nil {
				t.Fatalf("逃逸尝试必须被阻断：%+v", res)
			}
			if !strings.Contains(res.Error.Message, "not defined") &&
				!strings.Contains(res.Error.Message, "is not a function") {
				t.Fatalf("阻断原因应是「没有这个宿主能力」: %s", res.Error.Message)
			}
		})
	}

	// 文件系统：没有任何一次尝试落下哨兵文件。
	if _, err := os.Stat(sentinel); !os.IsNotExist(err) {
		t.Fatalf("脚本逃出沙箱写下了文件: %s", sentinel)
	}
	// 网络：宿主只代发脚本**显式返回**的 request；上面这些都没能构造出请求。
	if len(ff.calls) != 0 {
		t.Fatalf("宿主代发了 %d 次，但逃逸尝试不该产生请求", len(ff.calls))
	}
	// 对照组：唯一出网口是脚本返回的 request。
	ffOK := &fakeFetcher{}
	rOK := NewRunner(RunnerOptions{Fetcher: ffOK})
	res, err := rOK.Test(okScript, Vars{}, testPolicy())
	if err != nil {
		t.Fatalf("试运行返回错误: %v", err)
	}
	mustNoErr(t, res)
	if len(ffOK.calls) != 1 || ffOK.calls[0].URL != "https://provider.example/usage" {
		t.Fatalf("宿主代发面不符: %+v", ffOK.calls)
	}
}

// ── UC-016 A7 ───────────────────────────────────────────────────────────────

func TestUsageScriptShapeValidation(t *testing.T) {
	r := NewRunner(RunnerOptions{Fetcher: &fakeFetcher{resp: Response{Status: 200, Body: `{"n":7}`}}})
	cases := []struct {
		name      string
		script    string
		wantStage string
		wantLine  int
	}{
		{"缺 request", `({ extract: { usedPercent: 1 } })`, StageShape, 0},
		{"缺 extract", `({ request: { method: "GET", url: "https://provider.example" } })`, StageShape, 0},
		{"request 不是对象", `({ request: "x", extract: {} })`, StageShape, 0},
		{"缺 method", `({ request: { url: "https://provider.example" }, extract: {} })`, StageShape, 0},
		{"缺 url", `({ request: { method: "GET" }, extract: {} })`, StageShape, 0},
		{"方法不支持", `({ request: { method: "CONNECT", url: "https://provider.example" }, extract: {} })`, StageShape, 0},
		{"headers 不是映射", `({ request: { method: "GET", url: "https://provider.example", headers: [] }, extract: {} })`, StageShape, 0},
		{"header 值不是字符串", `({ request: { method: "GET", url: "https://provider.example", headers: { "X-A": 1 } }, extract: {} })`, StageShape, 0},
		{"body 不是字符串", `({ request: { method: "GET", url: "https://provider.example", body: 1 }, extract: {} })`, StageShape, 0},
		{"没有返回值", `var x = 1;`, StageShape, 0},
		{"语法错误带行号", "var a = 1;\nvar b = ;", StageCompile, 2},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			res, err := r.Test(c.script, Vars{}, testPolicy())
			if err != nil {
				t.Fatalf("试运行返回错误: %v", err)
			}
			ae := mustScriptCode(t, res.Error, apierr.CodeInvalidScriptShape)
			if ae.Stage != c.wantStage {
				t.Fatalf("stage=%s，期望 %s（消息：%s）", ae.Stage, c.wantStage, ae.Message)
			}
			if c.wantLine != 0 && ae.Line != c.wantLine {
				t.Fatalf("行号=%d，期望 %d（消息：%s）", ae.Line, c.wantLine, ae.Message)
			}
		})
	}

	// 合法形状：方法归一为大写、body 原样传递、路径提取生效。
	script := `({
	  request: { method: "post", url: "https://provider.example/usage", headers: { "X-A": "1" }, body: "{}" },
	  extract: { usedPercent: "$.n", name: "5h" }
	})`
	res, err := r.Test(script, Vars{}, testPolicy())
	if err != nil {
		t.Fatalf("试运行返回错误: %v", err)
	}
	res = mustNoErr(t, res)
	if res.Request.Method != "POST" {
		t.Fatalf("method=%q，期望归一为 POST", res.Request.Method)
	}
	if res.Request.Body != "{}" || res.Request.Headers["X-A"] != "1" {
		t.Fatalf("请求形状不符: %+v", res.Request)
	}
	if fmt.Sprint(res.Extracted["usedPercent"]) != "7" {
		t.Fatalf("extract.usedPercent=%v，期望 7", res.Extracted["usedPercent"])
	}
	if res.Response.Status != 200 {
		t.Fatalf("响应摘要 status=%d", res.Response.Status)
	}
}

// ── UC-016 A9 ───────────────────────────────────────────────────────────────

func TestUsageScriptErrorsAreBoundedAndDiagnosable(t *testing.T) {
	r := NewRunner(RunnerOptions{Fetcher: &fakeFetcher{}})

	// 1) 运行时错误：消息 + 行号 + stage。
	res, err := r.Test("var a = 1;\nundefined.boom();", Vars{}, testPolicy())
	if err != nil {
		t.Fatalf("试运行返回错误: %v", err)
	}
	ae := mustScriptCode(t, res.Error, apierr.CodeInvalidScriptShape)
	if ae.Stage != StageRun {
		t.Fatalf("stage=%s，期望 %s", ae.Stage, StageRun)
	}
	if ae.Line != 2 {
		t.Fatalf("行号=%d，期望 2（消息：%s）", ae.Line, ae.Message)
	}
	if !strings.Contains(ae.Message, "boom") {
		t.Fatalf("消息应带出异常原文: %q", ae.Message)
	}

	// 2) 超长消息被截断到上限。
	res2, err := r.Test(`(function(){ throw new Error("x".repeat(20000)); })()`, Vars{}, testPolicy())
	if err != nil {
		t.Fatalf("试运行返回错误: %v", err)
	}
	mustScriptCode(t, res2.Error, apierr.CodeInvalidScriptShape)
	if len(res2.Error.Message) > MaxErrorMessageBytes {
		t.Fatalf("消息长度=%d，超过上限 %d", len(res2.Error.Message), MaxErrorMessageBytes)
	}

	// 3) 每个错误面都必须是封闭集合里的码，且带 stage。
	for _, script := range []string{
		"var a = ;",
		`({ request: { method: "GET" }, extract: {} })`,
		`(function(){ throw new Error("boom"); })()`,
	} {
		res, err := r.Test(script, Vars{}, testPolicy())
		if err != nil {
			t.Fatalf("试运行返回错误: %v", err)
		}
		if res.Error == nil {
			t.Fatalf("脚本 %q 必须失败", script)
		}
		if !res.Error.Code.Valid() {
			t.Fatalf("错误码 %q 不在封闭集合里", res.Error.Code)
		}
		if res.Error.Stage == "" {
			t.Fatalf("错误面必须带 stage：%+v", res.Error)
		}
	}
}

// ── UC-016 A10 ──────────────────────────────────────────────────────────────

func TestUsageScriptIOIsBounded(t *testing.T) {
	// 1) 响应摘要有上限。
	big := `{"pad":"` + strings.Repeat("A", 100_000) + `"}`
	ff := &fakeFetcher{resp: Response{Status: 200, Body: big}}
	r := NewRunner(RunnerOptions{Fetcher: ff, ResponseLimit: 4096, ExcerptLimit: 128})
	res, err := r.Test(okScript, Vars{}, testPolicy())
	if err != nil {
		t.Fatalf("试运行返回错误: %v", err)
	}
	mustNoErr(t, res)
	if len(res.Response.BodyExcerpt) > 128 {
		t.Fatalf("摘要长度=%d，超过上限 128", len(res.Response.BodyExcerpt))
	}

	// 2) 交给脚本的正文按上限截断：路径指向的数据落在截断点之后 → 取不到，
	//    且错误里说明「被截断」。
	body := `{"pad":"` + strings.Repeat("A", 2000) + `","far":7}`
	ffBig := &fakeFetcher{resp: Response{Status: 200, Body: body}}
	rCut := NewRunner(RunnerOptions{Fetcher: ffBig, ResponseLimit: 512, ExcerptLimit: 128})
	script := `({
	  request: { method: "GET", url: "https://provider.example/usage" },
	  extract: { usedPercent: "$.far" }
	})`
	resCut, err := rCut.Test(script, Vars{}, testPolicy())
	if err != nil {
		t.Fatalf("试运行返回错误: %v", err)
	}
	ae := mustScriptCode(t, resCut.Error, apierr.CodeInvalidScriptShape)
	if !strings.Contains(ae.Message, "截断") {
		t.Fatalf("截断导致的失败应说明原因: %q", ae.Message)
	}

	// 对照组：同样正文、不截断时提取成功。
	rFull := NewRunner(RunnerOptions{Fetcher: ffBig, ResponseLimit: 1 << 20})
	resFull, err := rFull.Test(script, Vars{}, testPolicy())
	if err != nil {
		t.Fatalf("试运行返回错误: %v", err)
	}
	resFull = mustNoErr(t, resFull)
	if fmt.Sprint(resFull.Extracted["usedPercent"]) != "7" {
		t.Fatalf("未截断时 usedPercent=%v，期望 7", resFull.Extracted["usedPercent"])
	}

	// 3) 脚本返回值有上限。
	rSmall := NewRunner(RunnerOptions{
		Sandbox: NewSandbox(SandboxOptions{ResultLimit: 1024}),
		Fetcher: ff,
	})
	huge := `(function(){ return { request: { method: "GET", url: "https://provider.example" },
	                        extract: { name: "x".repeat(4096) } }; })()`
	resBig, err := rSmall.Test(huge, Vars{}, testPolicy())
	if err != nil {
		t.Fatalf("试运行返回错误: %v", err)
	}
	mustScriptCode(t, resBig.Error, apierr.CodeScriptLimitExceeded)

	// 4) 摘要按 rune 边界截断：多字节正文不得产出坏字节。
	ffUTF8 := &fakeFetcher{resp: Response{Status: 200, Body: strings.Repeat("中", 200)}}
	rUTF8 := NewRunner(RunnerOptions{Fetcher: ffUTF8, ExcerptLimit: 10})
	resUTF8, err := rUTF8.Test(okScript, Vars{}, testPolicy())
	if err != nil {
		t.Fatalf("试运行返回错误: %v", err)
	}
	// 该脚本不提路径，所以 JSON 正文的截断不影响结果。
	if resUTF8.Error != nil {
		t.Fatalf("不该失败: %+v", resUTF8.Error)
	}
	if !utf8.ValidString(resUTF8.Response.BodyExcerpt) {
		t.Fatalf("摘要截断产生了坏字节: %q", resUTF8.Response.BodyExcerpt)
	}
}

// ── UC-016 A13 ──────────────────────────────────────────────────────────────

func TestUsageScriptSandboxEscapeAttemptsAreBlocked(t *testing.T) {
	ff := &fakeFetcher{resp: Response{Status: 200, Body: `{"n":1}`}}
	r := NewRunner(RunnerOptions{Fetcher: ff})

	// 1) 宿主能力的探针一律为 undefined / 未定义。
	probes := []struct {
		expr string
		want string
	}{
		{`typeof process`, "undefined"},
		{`typeof require`, "undefined"},
		{`typeof fetch`, "undefined"},
		{`typeof XMLHttpRequest`, "undefined"},
		{`typeof Deno`, "undefined"},
		{`typeof Bun`, "undefined"},
		{`typeof window`, "undefined"},
		{`typeof global`, "undefined"},
		{`typeof module`, "undefined"},
		{`typeof exports`, "undefined"},
		{`typeof os`, "undefined"},
		{`typeof readFile`, "undefined"},
		{`Function("return typeof process")()`, "undefined"},
		{`({}).constructor.constructor("return typeof process")()`, "undefined"},
		{`(function(){}).constructor("return typeof require")()`, "undefined"},
		{`typeof (1).constructor.constructor("return this")().process`, "undefined"},
		{`typeof (1).constructor.constructor("return this")().require`, "undefined"},
	}
	for _, p := range probes {
		t.Run(p.expr, func(t *testing.T) {
			if got := probe(t, r, p.expr); got != p.want {
				t.Fatalf("%s → %q，期望 %q", p.expr, got, p.want)
			}
		})
	}

	// 2) 全局对象上没有危险名字。
	names := probe(t, r, `Object.getOwnPropertyNames(this).join(",")`)
	for _, dangerous := range []string{"process", "require", "fetch", "global", "window", "module", "os", "child_process"} {
		for _, got := range strings.Split(names, ",") {
			if got == dangerous {
				t.Fatalf("全局对象泄漏了宿主名字 %q：%s", dangerous, names)
			}
		}
	}

	// 3) 直接引用未定义名字 → 被阻断（ReferenceError，不是真的能力）。
	//    上面的探针本身是合法脚本，会正常出网一次；这里只断言**没有新增**代发。
	callsBefore := len(ff.calls)
	for _, expr := range []string{`require("fs")`, `process.exit(0)`, `fetch("https://x")`, `os.system("id")`} {
		res, err := r.Test(`(function(){ return `+expr+`; })()`, Vars{}, testPolicy())
		if err != nil {
			t.Fatalf("试运行返回错误: %v", err)
		}
		if res.Error == nil {
			t.Fatalf("%s 必须被阻断", expr)
		}
		if !strings.Contains(res.Error.Message, "not defined") {
			t.Fatalf("%s 的阻断原因=%q，期望 ReferenceError", expr, res.Error.Message)
		}
	}
	// 阻断之后宿主没有新增任何代发。
	if len(ff.calls) != callsBefore {
		t.Fatalf("逃逸尝试不该产生代发，实际新增 %d 次", len(ff.calls)-callsBefore)
	}

	// 4) 原型污染：污染源与被污染者必须是不同的运行时实例。
	res, err := r.Test(`Object.prototype.polluted = "yes";
	  ({ request: { method: "GET", url: "https://provider.example" }, extract: { name: "polluted" } })`, Vars{}, testPolicy())
	if err != nil {
		t.Fatalf("试运行返回错误: %v", err)
	}
	mustNoErr(t, res)
	if got := probe(t, r, `typeof ({}).polluted`); got != "undefined" {
		t.Fatalf("上一次执行的原型污染泄漏到了下一次：%q", got)
	}

	// 5) 污染不能伪造额度：extract 里没有 usedPercent 时，哪怕原型上被塞了
	//    usedPercent，也绝不能变成一份「42% 额度」。
	plan, serr := r.sandbox.Run(`Object.prototype.usedPercent = 42;
	  ({ request: { method: "GET", url: "https://provider.example" }, extract: { name: "polluted" } })`, Vars{})
	if serr != nil {
		t.Fatalf("脚本应能执行: %+v", serr)
	}
	extracted, serr := Extract(plan, []byte(`{"n":1}`))
	if serr != nil {
		t.Fatalf("提取不该失败: %+v", serr)
	}
	if _, ok := extracted["usedPercent"]; ok {
		t.Fatalf("被污染的原型伪造出了 usedPercent：%+v", extracted)
	}
	if _, serr := ToQuotaWindow(extracted); serr == nil {
		t.Fatalf("没有 usedPercent 时必须报错，绝不能给出额度")
	}
}

// 每次执行独立实例：脚本改写的内建不会影响下一次（与原型污染同一纪律，
// 单独断言以便读日志时一眼看出是哪条护栏）。
func TestUsageScriptUsesFreshRuntimePerRun(t *testing.T) {
	ff := &fakeFetcher{resp: Response{Status: 200, Body: `{}`}}
	r := NewRunner(RunnerOptions{Fetcher: ff})

	mutate := `Array.prototype.push = function(){ throw new Error("mutated"); };
	  ({ request: { method: "GET", url: "https://provider.example" }, extract: { name: "x" } })`
	res, err := r.Test(mutate, Vars{}, testPolicy())
	if err != nil {
		t.Fatalf("试运行返回错误: %v", err)
	}
	mustNoErr(t, res)

	after := `(function(){ var a = []; a.push(1); return { request: { method: "GET", url: "https://provider.example" },
	                       extract: { name: "ok" } }; })()`
	res2, err := r.Test(after, Vars{}, testPolicy())
	if err != nil {
		t.Fatalf("试运行返回错误: %v", err)
	}
	res2 = mustNoErr(t, res2)
	if fmt.Sprint(res2.Extracted["name"]) != "ok" {
		t.Fatalf("新运行时里 Array.prototype.push 应已恢复：%+v", res2.Extracted)
	}
}

// QuotaWindow 的绝对量是可选面：只报百分比时 used/limit/unit 必须**缺席**
// （不得显示 0），报绝对量时照实带出；百分比可以由绝对量算出，但绝不反过来
// 由百分比编造绝对量（契约 §3 与 UC-012 A3）。
func TestQuotaWindowOptionalAbsolutes(t *testing.T) {
	ff := &fakeFetcher{resp: Response{Status: 200, Body: `{"d":{"pct":42.5,"used":42500,"limit":100000,"unit":"credits"}}`}}
	r := NewRunner(RunnerOptions{Fetcher: ff})

	keys := func(res UsageResult) map[string]any {
		t.Helper()
		if res.Error != nil {
			t.Fatalf("取数失败: %+v", res.Error)
		}
		blob, err := json.Marshal(res.Quota)
		if err != nil {
			t.Fatalf("序列化额度失败: %v", err)
		}
		var m map[string]any
		if err := json.Unmarshal(blob, &m); err != nil {
			t.Fatalf("反序列化额度失败: %v", err)
		}
		return m
	}
	run := func(extract string) UsageResult {
		t.Helper()
		script := `({
		  request: { method: "GET", url: "https://provider.example/usage" },
		  extract: { ` + extract + ` }
		})`
		res, err := r.Usage(script, Vars{}, testPolicy())
		if err != nil {
			t.Fatalf("取数返回错误: %v", err)
		}
		return res
	}

	// 1) 只报百分比 → 三个绝对量字段缺席（不是 0）。
	onlyPct := keys(run(`usedPercent: "$.d.pct", name: "5h"`))
	for _, absent := range []string{"used", "limit", "unit"} {
		if v, ok := onlyPct[absent]; ok {
			t.Fatalf("只报百分比时 %s 必须缺席，实际 %v", absent, v)
		}
	}
	if onlyPct["usedPercent"] != 42.5 {
		t.Fatalf("usedPercent=%v，期望 42.5", onlyPct["usedPercent"])
	}

	// 2) 报绝对量 → 照实带出，且值不被改写。
	withAbs := keys(run(`usedPercent: "$.d.pct", used: "$.d.used", limit: "$.d.limit", unit: "$.d.unit"`))
	for k, want := range map[string]any{"used": 42500.0, "limit": 100000.0, "unit": "credits"} {
		if withAbs[k] != want {
			t.Fatalf("%s=%v，期望 %v（整份字段：%+v）", k, withAbs[k], want, withAbs)
		}
	}

	// 3) 只给绝对量：百分比由绝对量算出（据实计算，不是编造）。
	derived := run(`used: "$.d.used", limit: "$.d.limit"`)
	if derived.Quota == nil || derived.Quota.UsedPercent != 42.5 {
		t.Fatalf("由 used/limit 推出的百分比不符: %+v", derived.Quota)
	}

	// 4) 绝不给出「0 额度」或除零结果：limit 不是数字、limit=0、used 为负数
	//    都必须拒绝（而不是悄悄产出一份 0 或乱算的额度）。
	for name, extract := range map[string]string{
		"limit 不是数字": `used: "$.d.used", limit: "$.d.unit"`,
		"limit=0":    `used: 1, limit: 0`,
		"used 负数":    `used: -1, limit: 100, usedPercent: 1`,
		"两者都缺":       `name: "5h"`,
	} {
		res := run(extract)
		if res.Quota != nil || res.Error == nil {
			t.Fatalf("%s 时应拒绝: %+v", name, res)
		}
		if res.Error != nil && res.Error.Code != apierr.CodeInvalidScriptShape {
			t.Fatalf("%s 的错误码=%s，期望 invalid_script_shape", name, res.Error.Code)
		}
	}
}

// 契约 DTO 的字段集合是冻结面：多一个字段就是违约（doc/api-v1.md §3/§10）。
func TestUsageScriptDTOFieldSetsMatchContract(t *testing.T) {
	used, limit := 42500.0, 100000.0
	resets := "2030-01-01T00:00:00Z"
	cases := []struct {
		name  string
		value any
		want  []string
	}{
		{"Script", Script{Script: "s", UpdatedAt: "t"}, []string{"script", "updatedAt"}},
		{"TestResult", TestResult{
			Request:   RequestSpec{Headers: map[string]string{}},
			Extracted: map[string]any{}}, []string{"request", "response", "extracted"}},
		{"TestResult.error", TestResult{
			Request: RequestSpec{Headers: map[string]string{}}, Extracted: map[string]any{},
			Error: &ScriptError{}}, []string{"request", "response", "extracted", "error"}},
		{"RequestSpec.min", RequestSpec{Method: "GET", URL: "https://e.com", Headers: map[string]string{}},
			[]string{"method", "url", "headers"}},
		{"RequestSpec.body", RequestSpec{Method: "GET", URL: "https://e.com",
			Headers: map[string]string{}, Body: "{}"}, []string{"method", "url", "headers", "body"}},
		{"ResponseSummary", ResponseSummary{}, []string{"status", "bodyExcerpt"}},
		{"QuotaWindow.pctOnly", QuotaWindow{UsedPercent: 1, Source: "script"},
			[]string{"name", "usedPercent", "resetsAt", "source"}},
		{"QuotaWindow.full", QuotaWindow{Name: "5h", UsedPercent: 1, Used: &used, Limit: &limit,
			Unit: "credits", ResetsAt: &resets, Source: "script"},
			[]string{"name", "usedPercent", "used", "limit", "unit", "resetsAt", "source"}},
		{"UsageResult.none", UsageResult{Source: SourceScript}, []string{"quota", "source"}},
		{"UsageResult.error", UsageResult{Source: SourceScript, Error: &ScriptError{}},
			[]string{"quota", "source", "error"}},
		{"ScriptError.min", ScriptError{Code: "script_timeout", Message: "m", Stage: "run"},
			[]string{"code", "message", "stage"}},
		{"ScriptError.line", ScriptError{Code: "script_timeout", Message: "m", Stage: "run", Line: 3},
			[]string{"code", "message", "stage", "line"}},
	}
	for _, c := range cases {
		blob, err := json.Marshal(c.value)
		if err != nil {
			t.Fatalf("%s 序列化失败: %v", c.name, err)
		}
		var got map[string]any
		if err := json.Unmarshal(blob, &got); err != nil {
			t.Fatalf("%s 反序列化失败: %v", c.name, err)
		}
		keys := make([]string, 0, len(got))
		for k := range got {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		want := append([]string(nil), c.want...)
		sort.Strings(want)
		if strings.Join(keys, ",") != strings.Join(want, ",") {
			t.Fatalf("%s 字段集合不符\n got=%v\nwant=%v", c.name, keys, want)
		}
	}
}
