package family

import "testing"

func TestFullBaseProviderRequiresExplicitModeAndExactHost(t *testing.T) {
	cases := []struct {
		name     string
		origin   string
		fullBase bool
		model    string
		want     FullBaseProvider
	}{
		{"deepseek exact", "https://api.deepseek.com/v1", true, "glm-model-name-does-not-matter", FullBaseProviderDeepSeek},
		{"mimo exact", "https://api.xiaomimimo.com/anthropic", true, "deepseek-model-name", FullBaseProviderMiMo},
		{"glm exact", "https://open.bigmodel.cn/api/paas/v4", true, "mimo-model-name", FullBaseProviderGLM},
		{"mode omitted", "https://api.deepseek.com/v1", false, "deepseek-chat", ""},
		{"http disallowed", "http://api.deepseek.com/v1", true, "deepseek-chat", ""},
		{"lookalike suffix", "https://api.deepseek.com.attacker.example/v1", true, "deepseek-chat", ""},
		{"lookalike userinfo host", "https://api.deepseek.com@attacker.example/v1", true, "deepseek-chat", ""},
		{"unknown host despite model name", "https://vendor.example/v1", true, "deepseek-chat", ""},
		{"invalid URL", "://api.deepseek.com", true, "deepseek-chat", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			in := GatewayInput{Origin: tc.origin, EndpointIsFullBase: tc.fullBase, Model: tc.model}
			if got := FullBaseProviderFor(in); got != tc.want {
				t.Fatalf("FullBaseProviderFor(%q, full=%v, model=%q)=%q, want %q", tc.origin, tc.fullBase, tc.model, got, tc.want)
			}
		})
	}
}

func TestResponsesProviderClassificationAndKnownProtocolExclusions(t *testing.T) {
	cases := []struct {
		name, endpoint string
		responses      FullBaseProvider
		unsupported    bool
	}{
		{"deepseek root is Responses", "https://api.deepseek.com", FullBaseProviderDeepSeek, false},
		{"http DeepSeek is not verified", "http://api.deepseek.com", "", false},
		{"malformed full-base URL is generic", "://api.deepseek.com", "", false},
		{"deepseek /v1 remains generic", "https://api.deepseek.com/v1", "", false},
		{"deepseek path is generic", "https://api.deepseek.com/custom", "", false},
		{"deepseek Anthropic path is unsupported for Responses", "https://api.deepseek.com/anthropic/v1", "", true},
		{"mimo chat base unsupported for Responses", "https://api.xiaomimimo.com/v1", "", true},
		{"mimo Anthropic base unsupported for Responses", "https://api.xiaomimimo.com/anthropic", "", true},
		{"glm historical Responses base", "https://open.bigmodel.cn/api/v1", FullBaseProviderGLM, false},
		{"glm chat base unsupported for Responses", "https://open.bigmodel.cn/api/paas/v4", "", true},
		{"glm Anthropic base unsupported for Responses", "https://open.bigmodel.cn/api/anthropic", "", true},
		{"unknown Responses base is generic", "https://provider.example/responses", "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			in := GatewayInput{Origin: tc.endpoint, EndpointIsFullBase: true}
			if got := FullBaseResponsesProviderFor(in); got != tc.responses {
				t.Errorf("Responses provider = %q, want %q", got, tc.responses)
			}
			reason := UnsupportedResponsesBaseReason(in)
			if (reason != "") != tc.unsupported {
				t.Errorf("unsupported Responses reason = %q, wantUnsupported=%v", reason, tc.unsupported)
			}
		})
	}
	legacy := GatewayInput{Origin: "https://api.xiaomimimo.com/v1", EndpointIsFullBase: false}
	if FullBaseResponsesProviderFor(legacy) != "" || UnsupportedResponsesBaseReason(legacy) != "" {
		t.Fatal("legacy endpoint=false must preserve its former generic suffix path")
	}
}
