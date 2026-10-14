package proxy

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/peterplaykkk/yzagmgr/internal/apierr"
	"github.com/peterplaykkk/yzagmgr/internal/docmodel"
)

// ── 出网字节对照（UC-038）：引擎的 JSON 外科手术 vs internal/docmodel ─────────
//
// 引擎（sdk/router）因为 A8 的「纯标准库」约束不能 import internal/docmodel，于是
// 把字节保真的编辑逻辑移植了一份。移植会不会漂移是这次重构最大的风险，因此这里用
// 「重构前守护进程的做法」作为对照实现（docmodel 的 ReplaceRaw/SetJSON 序列），对同
// 一批请求体逐字节比对两边产出的出网字节：不等即失败。

// docmodelRewrite 是重构前 serveRouter 的改写序列（对照实现，仅供本测试使用）。
func docmodelRewrite(t *testing.T, body, modelTo, forceEffort string) string {
	t.Helper()
	doc, err := docmodel.ParseJSON([]byte(body))
	if err != nil {
		t.Fatalf("对照实现解析失败: %v", err)
	}
	if modelTo != "" {
		enc, err := encodeJSONString(modelTo)
		if err != nil {
			t.Fatalf("编码模型名: %v", err)
		}
		if err := doc.ReplaceRaw(enc, "model"); err != nil {
			t.Fatalf("改写模型名: %v", err)
		}
	}
	if forceEffort != "" {
		if err := doc.SetJSON(forceEffort, "output_config", "effort"); err != nil {
			oc := doc.Root().MemberOf("output_config")
			if oc == nil || oc.Val == nil || oc.Val.Kind != docmodel.KObject {
				if err := doc.SetJSON(map[string]string{"effort": forceEffort}, "output_config"); err != nil {
					t.Fatalf("补出 output_config: %v", err)
				}
			} else {
				t.Fatalf("对照实现出现编辑冲突: %v", err)
			}
		}
	}
	if !doc.Dirty() {
		return body
	}
	return string(doc.Render())
}

func TestRouterRewriteByteParityWithDocmodel(t *testing.T) {
	const (
		alias  = "claude-fable-5-mp"
		target = "mimo-v2.6-pro"
	)
	bodies := []string{
		`{"model":"claude-fable-5-mp","messages":[]}`,
		`{"model":"claude-fable-5-mp","stream":true}`,
		`{"messages":[],"model":"claude-fable-5-mp"}`,
		"{ \"model\" : \"claude-fable-5-mp\" }",
		"{\n  \"model\": \"claude-fable-5-mp\",\n  \"messages\": []\n}",
		"{\n\t\"model\":\t\"claude-fable-5-mp\"\n}",
		`{"model":"claude-fable-5-mp","output_config":{"effort":"low"},"messages":[]}`,
		`{"model":"claude-fable-5-mp","output_config":{},"messages":[]}`,
		`{"model":"claude-fable-5-mp","output_config":"legacy","messages":[]}`,
		`{"model":"claude-fable-5-mp","output_config":7,"messages":[]}`,
		`{"model":"claude-fable-5-mp","output_config":null,"messages":[]}`,
		"{\n  \"model\": \"claude-fable-5-mp\",\n  \"output_config\": {\n    \"effort\": \"low\"\n  }\n}",
		`{"model":"claude-fable-5-mp","text":"汉字 <&> \"引号\" \\ 反斜杠 é"}`,
		`{"model":"claude-fable-5-mp","empty":{},"arr":[{},[]],"tail":1}`,
		`{"model":"claude-fable-5-mp","n":10000000000000000001,"neg":-0.5e-3}`,
	}

	for _, forceEffort := range []string{"", "max"} {
		name := "只改写 model"
		if forceEffort != "" {
			name = "改写 model + force effort"
		}
		t.Run(name, func(t *testing.T) {
			for _, body := range bodies {
				// (a) 对照：docmodel 的编辑序列。
				want := docmodelRewrite(t, body, target, forceEffort)

				// (b) 引擎：经守护进程数据面把真实出网字节抓下来。
				h, _ := newRouterHarness(t)
				if _, err := h.srv.PutModelMap("mimo", alias, target); err != nil {
					t.Fatalf("PutModelMap: %v", err)
				}
				if forceEffort != "" {
					if _, err := h.srv.SetEffortForce("mimo", forceEffort); err != nil {
						t.Fatalf("SetEffortForce: %v", err)
					}
				}
				code, resp := postRouter(t, h, body)
				if code != http.StatusOK {
					t.Fatalf("HTTP %d（%s）：%s", code, body, resp)
				}
				req := h.gw.last()
				if got := string(req.Body); got != want {
					t.Fatalf("出网字节与 docmodel 不一致：\n got %q\nwant %q\n源 %q", got, want, body)
				}
				if req.Header.Get("Authorization") != "Bearer token-mimo" {
					t.Fatalf("凭据注入不对: %q", req.Header.Get("Authorization"))
				}
			}
		})
	}
}

// TestRouterErrorBodyParityWithDataPlane 守错误信封的字节形状：引擎写出的正文必须
// 与守护进程 writeDataPlaneError 对同一个 (状态码, 错误码, 文案) 写出的正文逐字节相同。
func TestRouterErrorBodyParityWithDataPlane(t *testing.T) {
	h, _ := newRouterHarness(t)
	code, body := postRouter(t, h, `{"model":"bogus<&>model"}`)
	if code != http.StatusBadRequest {
		t.Fatalf("HTTP %d", code)
	}
	msg := envelopeMessage(t, body)
	if !strings.Contains(msg, `bogus<&>model`) || !strings.Contains(msg, "未命中") {
		t.Fatalf("错误文案未点名未命中: %q", msg)
	}
	// 原始字节里 < > & 走 encoding/json 的默认转义（与守护进程同形）。
	if !strings.Contains(body, `bogus\u003c\u0026\u003emodel`) {
		t.Fatalf("错误正文的转义形态与守护进程不一致: %s", body)
	}
	w := httptest.NewRecorder()
	writeDataPlaneError(w, http.StatusBadRequest, apierr.CodeInvalidRequest, msg)
	if got := w.Body.String(); got != body {
		t.Fatalf("引擎与守护进程的错误正文不一致：\n引擎 %s\n守护进程 %s", body, got)
	}
}
