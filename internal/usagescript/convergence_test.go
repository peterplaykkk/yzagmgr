package usagescript

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"strings"
	"testing"

	"github.com/peterplaykkk/yzagmgr/internal/apierr"
)

// UC-016 收敛波（封闭性 / 掩码三条 + DNS 解析期防护）的复现夹具。
//
// 四个缺陷各有修前必红的用例（本文件先于实现落盘；各 Test 注释标了修前
// 观测到的红形态）：
//
//   - (a) 句柄出现在**请求头名**位置：修前头名原样出网、展示面未掩名；
//   - (b) 句柄出现在 **extract 键名**位置：修前原样回显进 extracted；
//   - (c) 位置封闭性只认「字面串」：修前保留句柄形态的变形（截断/去后缀）
//     可绕过逐位置判定出网；
//   - (d) DNS 解析期防护缺失：修前已批准域名解析到内网不会在拨号层被拦。

// ── (a) 句柄出现在请求头名位置 ─────────────────────────────────────────────

func TestUsageScriptCredentialInHeaderNameIsRejected(t *testing.T) {
	const secret = "sk-hdrname-SECRET-246810"
	ff := &fakeFetcher{}
	r := NewRunner(RunnerOptions{Fetcher: ff})
	// 修前红形态：头「值」是干净的 "v"，逐值判定放行 ⇒ 句柄随头名出网，
	// 且试运行响应原样回显头名。
	res, err := r.Test(`({ request: { method: "GET", url: "https://provider.example/usage",
	  headers: { "X-{{apiKey}}": "v" } },
	  extract: { usedPercent: 1 } })`, Vars{APIKey: secret}, testPolicy())
	if err != nil {
		t.Fatalf("试运行返回错误: %v", err)
	}
	se := mustScriptCode(t, res.Error, apierr.CodeInvalidScriptShape)
	if se.Stage != StageRequest {
		t.Fatalf("stage=%s，期望 %s（消息：%s）", se.Stage, StageRequest, se.Message)
	}
	if len(ff.calls) != 0 {
		t.Fatalf("头名携带句柄必须发送前拒绝，实际代发 %d 次", len(ff.calls))
	}
	assertNoPlaintext(t, res, secret, handlePrefix)
	// 展示面掩码作用于名：被拒请求的头名里不得再出现句柄。
	for k := range res.Request.Headers {
		if strings.Contains(k, handlePrefix) {
			t.Fatalf("展示面头名未脱敏: %q", k)
		}
	}

	// 头名变形同样在拒绝面：计算属性键 + 保留形态的截断句柄。
	ff2 := &fakeFetcher{}
	r2 := NewRunner(RunnerOptions{Fetcher: ff2})
	res2, err := r2.Test(`({ request: { method: "GET", url: "https://provider.example/usage",
	  headers: { ["X-" + "{{apiKey}}".slice(0, 32)]: "v" } },
	  extract: { usedPercent: 1 } })`, Vars{APIKey: secret}, testPolicy())
	if err != nil {
		t.Fatalf("试运行返回错误: %v", err)
	}
	se2 := mustScriptCode(t, res2.Error, apierr.CodeInvalidScriptShape)
	if se2.Stage != StageRequest {
		t.Fatalf("stage=%s，期望 %s（消息：%s）", se2.Stage, StageRequest, se2.Message)
	}
	if len(ff2.calls) != 0 {
		t.Fatalf("头名携带句柄形态必须发送前拒绝，实际代发 %d 次", len(ff2.calls))
	}
	assertNoPlaintext(t, res2, secret, handlePrefix)
}

// ── (b) 句柄出现在 extract 键名位置 ────────────────────────────────────────

func TestUsageScriptCredentialInExtractKeyIsRejected(t *testing.T) {
	const secret = "sk-extkey-SECRET-135790"

	// 顶层键名。修前红形态：extract 只扫键的「值」⇒ 句柄作键名原样回显。
	ff := &fakeFetcher{}
	r := NewRunner(RunnerOptions{Fetcher: ff})
	res, err := r.Test(`({ request: { method: "GET", url: "https://provider.example/usage" },
	  extract: { "{{apiKey}}": 2 } })`, Vars{APIKey: secret}, testPolicy())
	if err != nil {
		t.Fatalf("试运行返回错误: %v", err)
	}
	se := mustScriptCode(t, res.Error, apierr.CodeInvalidScriptShape)
	if se.Stage != StageRequest {
		t.Fatalf("stage=%s，期望 %s（消息：%s）", se.Stage, StageRequest, se.Message)
	}
	if len(ff.calls) != 0 {
		t.Fatalf("extract 键名携带句柄必须发送前拒绝，实际代发 %d 次", len(ff.calls))
	}
	assertNoPlaintext(t, res, secret, handlePrefix)

	// 嵌套键名（提取结果的深嵌套面）。
	ff2 := &fakeFetcher{}
	r2 := NewRunner(RunnerOptions{Fetcher: ff2})
	res2, err := r2.Test(`({ request: { method: "GET", url: "https://provider.example/usage" },
	  extract: { usedPercent: 1, note: { "{{apiKey}}": "v" } } })`,
		Vars{APIKey: secret}, testPolicy())
	if err != nil {
		t.Fatalf("试运行返回错误: %v", err)
	}
	se2 := mustScriptCode(t, res2.Error, apierr.CodeInvalidScriptShape)
	if se2.Stage != StageRequest {
		t.Fatalf("stage=%s，期望 %s（消息：%s）", se2.Stage, StageRequest, se2.Message)
	}
	if len(ff2.calls) != 0 {
		t.Fatalf("嵌套键名携带句柄必须发送前拒绝，实际代发 %d 次", len(ff2.calls))
	}
	assertNoPlaintext(t, res2, secret, handlePrefix)
}

// ── (a)(b) 展示面掩码：作用于名与值 ────────────────────────────────────────

func TestUsageScriptMasksHandleInNames(t *testing.T) {
	b, err := newBindings(Vars{APIKey: "sk-mask-name-0011223344"})
	if err != nil {
		t.Fatalf("构造注入台账失败: %v", err)
	}
	handle := b.Handles()[0]

	req := RequestSpec{
		Method: "GET",
		URL:    "https://provider.example/usage?k=" + handle,
		Headers: map[string]string{
			"X-" + handle: "Bearer " + handle,
			"X-Ok":        "v",
		},
	}
	masked := maskRequest(b, req)
	if strings.Contains(masked.URL, handle) {
		t.Fatalf("URL 未脱敏: %q", masked.URL)
	}
	maskedName := false
	for k, v := range masked.Headers {
		if strings.Contains(k, handle) || strings.Contains(v, handle) {
			t.Fatalf("头名/头值未脱敏: %q=%q", k, v)
		}
		if strings.Contains(k, MaskedSecret) {
			maskedName = true
		}
	}
	if !maskedName {
		t.Fatalf("头名应出现脱敏替身: %v", masked.Headers)
	}
	if got := masked.Headers["X-Ok"]; got != "v" {
		t.Fatalf("干净头不得被改写: %q", got)
	}

	extracted := map[string]any{
		handle: "v",
		"note": map[string]any{"n-" + handle: handle},
	}
	me := maskExtracted(b, extracted)
	for k, v := range me {
		if strings.Contains(k, handle) {
			t.Fatalf("extracted 键名未脱敏: %q", k)
		}
		if s, ok := v.(string); ok && strings.Contains(s, handle) {
			t.Fatalf("extracted 值未脱敏: %q", s)
		}
		if nested, ok := v.(map[string]any); ok {
			for kk, vv := range nested {
				if strings.Contains(kk, handle) || strings.Contains(fmt.Sprint(vv), handle) {
					t.Fatalf("嵌套键名/值未脱敏: %q=%v", kk, vv)
				}
			}
		}
	}
}

// ── (c) 句柄形态全扫（非批准位置命中即拒） ─────────────────────────────────

func TestUsageScriptHandleShapeIsScannedEverywhere(t *testing.T) {
	const secret = "sk-shape-SECRET-864209"
	cases := []struct {
		name   string
		script string
	}{
		{
			// 修前红形态：去掉槽位后缀后不再等于字面句柄，逐位置的字面串判定
			// 放行 ⇒ 出网 query 携带 `yzagmgr-ref-<hex>` 形态。
			"去后缀变形放查询串",
			`({ request: { method: "GET",
			    url: "https://provider.example/usage?k=" + "{{apiKey}}".replace("-apiKey", "") },
			    extract: { usedPercent: 1 } })`,
		},
		{
			// 修前红形态：截断到 32 字符（保留 yzagmgr-ref- + 20 位 hex）后放请求体。
			// （归档改名后前缀由 10 字符变 12 字符，形态阈值 20 位 hex ⇒ 截断点 30→32。）
			"截断变形放请求体",
			`({ request: { method: "GET", url: "https://provider.example/usage",
			    body: "t=" + "{{apiKey}}".slice(0, 32) },
			    extract: { usedPercent: 1 } })`,
		},
		{
			// 修前红形态：非批准头「值」里的形态。
			"截断变形放非批准头值",
			`({ request: { method: "GET", url: "https://provider.example/usage",
			    headers: { "X-T": "v" + "{{apiKey}}".slice(0, 32) } },
			    extract: { usedPercent: 1 } })`,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			ff := &fakeFetcher{}
			r := NewRunner(RunnerOptions{Fetcher: ff})
			res, err := r.Test(c.script, Vars{APIKey: secret}, testPolicy())
			if err != nil {
				t.Fatalf("试运行返回错误: %v", err)
			}
			se := mustScriptCode(t, res.Error, apierr.CodeInvalidScriptShape)
			if se.Stage != StageRequest {
				t.Fatalf("stage=%s，期望 %s（消息：%s）", se.Stage, StageRequest, se.Message)
			}
			if len(ff.calls) != 0 {
				t.Fatalf("句柄形态出现在非批准位置必须发送前拒绝，实际代发 %d 次", len(ff.calls))
			}
			assertNoPlaintext(t, res, secret, handlePrefix)
		})
	}

	// 已知限制（如实登记，防止边界悄然扩大）：把句柄**拆散重排**的变形
	//（反转、逐字符插空、大小写改写、编码）会脱离 `yzagmgr-ref-<hex>` 形态，
	// 形态扫描不可探测。无害性依据：秘密永不随行（代入只发生在批准头值，
	// 且只代入本次运行的完整句柄），这类变形串换不回秘密；目标仍受批准面
	//（https + 非内网 + 已批准 origin）约束。本子测固定该边界现状。
	t.Run("已知限制：反转变形脱离形态、扫描不可探测", func(t *testing.T) {
		ff := &fakeFetcher{}
		r := NewRunner(RunnerOptions{Fetcher: ff})
		res, err := r.Test(`({ request: { method: "GET",
		    url: "https://provider.example/usage?k=" + "{{apiKey}}".split("").reverse().join("") },
		    extract: { usedPercent: 1 } })`, Vars{APIKey: secret}, testPolicy())
		if err != nil {
			t.Fatalf("试运行返回错误: %v", err)
		}
		if res.Error != nil {
			t.Fatalf("当前设计下反转形态不可探测（已知限制登记）: %+v", res.Error)
		}
		if len(ff.calls) != 1 {
			t.Fatalf("已知限制：该请求会出网（实际代发 %d 次）", len(ff.calls))
		}
		// 秘密不随行：出网请求里既无真实秘密、也无本运行句柄的字面串。
		got := ff.calls[0].URL
		if strings.Contains(got, secret) || strings.Contains(got, handlePrefix) {
			t.Fatalf("反转变形不得携带秘密或句柄字面串: %q", got)
		}
	})
}

// ── (d) DNS 解析期防护 ─────────────────────────────────────────────────────

// stubResolver 是解析期防护的可测缝：不改系统 DNS，直接给出解析结果。
type stubResolver struct {
	ips   []netip.Addr
	err   error
	hosts []string
}

func (s *stubResolver) LookupNetIP(_ context.Context, host string) ([]netip.Addr, error) {
	s.hosts = append(s.hosts, host)
	if s.err != nil {
		return nil, s.err
	}
	return s.ips, nil
}

func TestUsageScriptDNSResolutionToInternalIsBlocked(t *testing.T) {
	// 1) 单元面：HTTPFetcher 直接代发——stub 解析说「目标域名 → 内网地址」。
	//    修前红形态：拨号层不看待解析结果，请求直接拨到夹具（dials>0）。
	for _, tc := range []struct {
		name string
		ip   string
	}{
		{"回环 127.0.0.1", "127.0.0.1"},
		{"私网 10.0.0.8", "10.0.0.8"},
		{"链路本地 / 元数据 169.254.169.254", "169.254.169.254"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			res := &stubResolver{ips: []netip.Addr{netip.MustParseAddr(tc.ip)}}
			var dials int
			h := &HTTPFetcher{
				Resolver: res,
				DialContext: func(context.Context, string, string) (net.Conn, error) {
					dials++
					return nil, errors.New("夹具不发起真实拨号")
				},
			}
			_, err := h.Do(RequestSpec{Method: "GET", URL: "https://provider.example/usage"}, nil)
			var ae *apierr.Error
			if !errors.As(err, &ae) || ae.Code != apierr.CodeBlockedTarget {
				t.Fatalf("解析到 %s 必须 blocked_target，实际：%v", tc.ip, err)
			}
			if !strings.Contains(ae.Message, "解析到内网") {
				t.Fatalf("拒绝消息应含「解析到内网」可诊断信息：%q", ae.Message)
			}
			if dials != 0 {
				t.Fatalf("解析到内网必须 0 拨号（0 出网），实际 %d", dials)
			}
			if len(res.hosts) != 1 || res.hosts[0] != "provider.example" {
				t.Fatalf("解析的应是目标域名，实际 %v", res.hosts)
			}
		})
	}

	// 2) 端到端（Runner）：域名在目标白名单里、解析到回环 ⇒ 「已批准」不豁免
	//    解析期防护：发送前拒绝、0 拨号。
	var dials int
	h := &HTTPFetcher{
		Resolver: &stubResolver{ips: []netip.Addr{netip.MustParseAddr("127.0.0.1")}},
		DialContext: func(context.Context, string, string) (net.Conn, error) {
			dials++
			return nil, errors.New("夹具不发起真实拨号")
		},
	}
	r := NewRunner(RunnerOptions{Fetcher: h})
	res, err := r.Test(okScript, Vars{}, testPolicy())
	if err != nil {
		t.Fatalf("试运行返回错误: %v", err)
	}
	se := mustScriptCode(t, res.Error, apierr.CodeBlockedTarget)
	if se.Stage != StageRequest {
		t.Fatalf("stage=%s，期望 %s（消息：%s）", se.Stage, StageRequest, se.Message)
	}
	if !strings.Contains(se.Message, "解析到内网") {
		t.Fatalf("拒绝消息应含「解析到内网」：%q", se.Message)
	}
	if dials != 0 {
		t.Fatalf("已批准域名解析到回环必须 0 出网，实际拨号 %d 次", dials)
	}

	// 3) 对照：正常解析（公网地址）⇒ 放行到拨号层（拨号恰好一次，错误不是
	//    解析期拒绝，而是底层拨号失败）。
	var dials2 int
	var dialAddr string
	h2 := &HTTPFetcher{
		Resolver: &stubResolver{ips: []netip.Addr{netip.MustParseAddr("203.0.113.7")}},
		DialContext: func(_ context.Context, _, addr string) (net.Conn, error) {
			dials2++
			dialAddr = addr
			return nil, errors.New("夹具拨号失败（放行到拨号层即达判据）")
		},
	}
	if _, err := h2.Do(RequestSpec{Method: "GET", URL: "https://provider.example/usage"}, nil); err == nil {
		t.Fatalf("夹具拨号必须报错")
	}
	var ae2 *apierr.Error
	if errors.As(err, &ae2) && ae2.Code == apierr.CodeBlockedTarget {
		t.Fatalf("正常解析不得被解析期拒绝：%v", err)
	}
	if dials2 != 1 || dialAddr != "provider.example:443" {
		t.Fatalf("正常解析应放行到拨号层（addr=%q dials=%d）", dialAddr, dials2)
	}

	// 4) IP 字面量不走解析（分工说明）：字面量的网段黑名单判定收在 URL 层
	//    （ValidateRequestURL / HostIsBlocked 在入口与每个跳转目标上执行），
	//    拨号层只补「解析期」这一环。既有字面量回环夹具（store_test 的
	//    httptest 直连）因此零回归——这里显式钉住该分工。
	stub := &stubResolver{ips: []netip.Addr{netip.MustParseAddr("10.0.0.1")}}
	var dials3 int
	h3 := &HTTPFetcher{
		Resolver: stub,
		DialContext: func(_ context.Context, _, addr string) (net.Conn, error) {
			dials3++
			return nil, errors.New("夹具拨号失败")
		},
	}
	if _, err := h3.Do(RequestSpec{Method: "GET", URL: "https://127.0.0.1:8443/usage"}, nil); err == nil {
		t.Fatalf("夹具拨号必须报错")
	}
	if dials3 != 1 {
		t.Fatalf("IP 字面量应直达拨号层（dials=%d）", dials3)
	}
	if len(stub.hosts) != 0 {
		t.Fatalf("IP 字面量不应咨询解析器：%v", stub.hosts)
	}

	// 5) 调用方自带 *http.Transport（含自定义拨号）：克隆后穿一道解析校验，
	//    校验通过再把**原名地址**交给原拨号实现；解析到内网同样被拒、0 拨号。
	var dials5 int
	var dialAddr5 string
	h5 := &HTTPFetcher{
		Client: &http.Client{Transport: &http.Transport{
			DialContext: func(_ context.Context, _, addr string) (net.Conn, error) {
				dials5++
				dialAddr5 = addr
				return nil, errors.New("夹具拨号失败")
			},
		}},
		Resolver: &stubResolver{ips: []netip.Addr{netip.MustParseAddr("10.1.2.3")}},
	}
	// 解析到内网 ⇒ 校验拒绝，原拨号实现一次都没被调。
	_, err5 := h5.Do(RequestSpec{Method: "GET", URL: "https://provider.example/usage"}, nil)
	var ae5 *apierr.Error
	if !errors.As(err5, &ae5) || ae5.Code != apierr.CodeBlockedTarget {
		t.Fatalf("自带传输也要走解析期校验（blocked_target），实际：%v", err5)
	}
	if dials5 != 0 {
		t.Fatalf("解析到内网必须 0 拨号，实际 %d", dials5)
	}
	// 正常解析 ⇒ 放行，交给原拨号实现（收到的是原名地址）。
	h5.Resolver = &stubResolver{ips: []netip.Addr{netip.MustParseAddr("203.0.113.9")}}
	if _, err := h5.Do(RequestSpec{Method: "GET", URL: "https://provider.example/usage"}, nil); err == nil {
		t.Fatalf("夹具拨号必须报错")
	}
	if dials5 != 1 || dialAddr5 != "provider.example:443" {
		t.Fatalf("自带传输放行后应把原名地址交给原拨号（addr=%q dials=%d）", dialAddr5, dials5)
	}
}
