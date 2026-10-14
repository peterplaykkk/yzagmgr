// `GET /tools/{id}` 的 `companionKeys`：形状附属键的当前值。
//
// 契约 §4.1 的定义是「除决定键之外、由本产品管理的键的当前值」，且 `masked:true` 表示
// 该键含凭据（`value` 是掩码后的展示值）。键位清单的来源是 acceptance-recipe §A 与
// `doc/verification-baseline.md` §2/§3/§4（数值基线），不是凭印象列的：
// 每个条目都对应某个家族包里的写入常量。
//
// 纪律：
//   - **只读**：这一层绝不写盘（探针语义，UC-003 A2）；
//   - 凭据位用 `<已脱敏>` 替换真实值，绝不把令牌明文放进响应（UC-014 A11）；
//   - 键缺席时值给空串（契约里 `value` 恒出现），而不是省略条目——界面按名字逐个对齐。
package api

import (
	"strings"

	"github.com/peterplaykkk/yzagmgr/internal/config"
	"github.com/peterplaykkk/yzagmgr/internal/docmodel"
	"github.com/peterplaykkk/yzagmgr/internal/family"
)

// companionKey 是契约 §4.1 的形状附属键。
type companionKey struct {
	Name   string `json:"name"`
	Value  string `json:"value"`
	Masked bool   `json:"masked"`
	Order  int    `json:"order"`
}

// maskedValue 是凭据位的展示值（与 envcheck 的脱敏记号同形）。
const maskedValue = "<已脱敏>"

// maskedValueContent 是「配置内容端点」（§4.6 / UC-039 A4）的展示替身：用户口径是
// 「查看写入的全部内容，密钥 *** mask」——替身取 `***`，且掩码走细粒度模式
// （只掩凭据叶子、结构化容器继续下钻），见 diff.go 的 maskContent。
const maskedValueContent = "***"

// fileKind 是读取该文件要用的定位层。
type fileKind int

const (
	kindJSON fileKind = iota // JSON / JSON5（容错读取）
	kindTOML
	kindEnv // dotenv
)

// companionSpec 是一条附属键的定位描述。
type companionSpec struct {
	// name 是展示名：对外用它，也是界面「按名字对齐」的键。
	name string
	// file 给出承载该键的文件（绝对路径）。
	file func(env *config.Env) string
	kind fileKind
	// path 是键路径：JSON 用逐级键名，数组下标写成 `#0`；TOML 用 [section, key]；
	// dotenv 用 [键名]。
	path []string
	// masked 报告该键是否含凭据。
	masked bool
}

// providerPath 把 `yzagmgr-<uid>` 这类 provider 键嵌进键路径。
func providerPath(uid string, rest ...string) []string {
	out := make([]string, 0, len(rest)+1)
	out = append(out, managedProviderPrefix+uid)
	out = append(out, rest...)
	return out
}

// providerKeyPath 是 opencode 的键路径：`provider["yzagmgr-<uid>"].<rest...>`。
func providerKeyPath(uid string, rest ...string) []string {
	return append([]string{"provider"}, providerPath(uid, rest...)...)
}

// companionsFor 返回该族的附属键（顺序即 order）。
//
// hermes 未实现 → 返回空（注册表缺失时调用方按 not_supported 处理，不在这里伪造）。
func companionsFor(id family.ID, uid string) []companionSpec {
	switch id {
	case family.Claude:
		return []companionSpec{
			{name: "env.ANTHROPIC_AUTH_TOKEN", file: (*config.Env).ClaudeSettings, kind: kindJSON,
				path: []string{"env", "ANTHROPIC_AUTH_TOKEN"}, masked: true},
			{name: "env.ANTHROPIC_MODEL", file: (*config.Env).ClaudeSettings, kind: kindJSON,
				path: []string{"env", "ANTHROPIC_MODEL"}},
			{name: "env.CLAUDE_CODE_AUTO_COMPACT_WINDOW", file: (*config.Env).ClaudeSettings, kind: kindJSON,
				path: []string{"env", "CLAUDE_CODE_AUTO_COMPACT_WINDOW"}},
			{name: "env.CLAUDE_CODE_MAX_OUTPUT_TOKENS", file: (*config.Env).ClaudeSettings, kind: kindJSON,
				path: []string{"env", "CLAUDE_CODE_MAX_OUTPUT_TOKENS"}},
			{name: "env.CLAUDE_CODE_MODEL_CAPABILITIES", file: (*config.Env).ClaudeSettings, kind: kindJSON,
				path: []string{"env", "CLAUDE_CODE_MODEL_CAPABILITIES"}},
		}
	case family.ClaudeDesktop:
		return []companionSpec{
			{name: "inferenceGatewayURL", file: claudeDesktopProfileFile, kind: kindJSON,
				path: []string{"inferenceGatewayURL"}},
			{name: "inferenceGatewayKey", file: claudeDesktopProfileFile, kind: kindJSON,
				path: []string{"inferenceGatewayKey"}, masked: true},
			{name: "inferenceModels", file: claudeDesktopProfileFile, kind: kindJSON,
				path: []string{"inferenceModels"}},
			{name: "modelPrefer1mContext", file: claudeDesktopProfileFile, kind: kindJSON,
				path: []string{"modelPrefer1mContext"}},
		}
	case family.Codex:
		return []companionSpec{
			{name: "model_context_window", file: (*config.Env).CodexConfig, kind: kindTOML,
				path: []string{"", "model_context_window"}},
			{name: "model_auto_compact_token_limit", file: (*config.Env).CodexConfig, kind: kindTOML,
				path: []string{"", "model_auto_compact_token_limit"}},
			{name: "model_catalog_json", file: (*config.Env).CodexConfig, kind: kindTOML,
				path: []string{"", "model_catalog_json"}},
			{name: "model_reasoning_effort", file: (*config.Env).CodexConfig, kind: kindTOML,
				path: []string{"", "model_reasoning_effort"}},
			{name: "model_providers.custom.base_url", file: (*config.Env).CodexConfig, kind: kindTOML,
				path: []string{"model_providers.custom", "base_url"}},
			{name: "model_providers.custom.wire_api", file: (*config.Env).CodexConfig, kind: kindTOML,
				path: []string{"model_providers.custom", "wire_api"}},
		}
	case family.Gemini:
		return []companionSpec{
			{name: "GEMINI_API_KEY", file: (*config.Env).GeminiEnv, kind: kindEnv,
				path: []string{"GEMINI_API_KEY"}, masked: true},
			{name: "GEMINI_MODEL", file: (*config.Env).GeminiEnv, kind: kindEnv,
				path: []string{"GEMINI_MODEL"}},
			{name: "security.auth.selectedType", file: (*config.Env).GeminiSettings, kind: kindJSON,
				path: []string{"security", "auth", "selectedType"}},
		}
	case family.Grokbuild:
		return []companionSpec{
			{name: "models.default_reasoning_effort", file: (*config.Env).GrokConfig, kind: kindTOML,
				path: []string{"models", "default_reasoning_effort"}},
			{name: "model.*.context_window", file: (*config.Env).GrokConfig, kind: kindTOML,
				path: []string{"@model.", "context_window"}},
			{name: "model.*.auto_compact_threshold_percent", file: (*config.Env).GrokConfig, kind: kindTOML,
				path: []string{"@model.", "auto_compact_threshold_percent"}},
			{name: "model.*.max_completion_tokens", file: (*config.Env).GrokConfig, kind: kindTOML,
				path: []string{"@model.", "max_completion_tokens"}},
		}
	case family.OpenCode:
		return []companionSpec{
			{name: "provider.options.apiKey", file: (*config.Env).OpenCodeConfig, kind: kindJSON,
				path: providerKeyPath(uid, "options", "apiKey"), masked: true},
			{name: "provider.limit.context", file: (*config.Env).OpenCodeConfig, kind: kindJSON,
				path: providerKeyPath(uid, "models", "@rootModel", "limit", "context")},
			{name: "provider.limit.output", file: (*config.Env).OpenCodeConfig, kind: kindJSON,
				path: providerKeyPath(uid, "models", "@rootModel", "limit", "output")},
		}
	case family.OpenClaw:
		return []companionSpec{
			{name: "models.providers.apiKey", file: (*config.Env).OpenClawConfig, kind: kindJSON,
				path: []string{"models", "providers", managedProviderPrefix + uid, "apiKey"}, masked: true},
			{name: "models.providers.contextWindow", file: (*config.Env).OpenClawConfig, kind: kindJSON,
				path: []string{"models", "providers", managedProviderPrefix + uid, "models", "#0", "contextWindow"}},
			{name: "models.providers.maxTokens", file: (*config.Env).OpenClawConfig, kind: kindJSON,
				path: []string{"models", "providers", managedProviderPrefix + uid, "models", "#0", "maxTokens"}},
			{name: "agents.defaults.thinkingDefault", file: (*config.Env).OpenClawConfig, kind: kindJSON,
				path: []string{"agents", "defaults", "thinkingDefault"}},
			{name: "agents.defaults.compaction.reserveTokens", file: (*config.Env).OpenClawConfig, kind: kindJSON,
				path: []string{"agents", "defaults", "compaction", "reserveTokens"}},
		}
	case family.Pi:
		return []companionSpec{
			{name: "providers.apiKey", file: (*config.Env).PiModels, kind: kindJSON,
				path: []string{"providers", managedProviderPrefix + uid, "apiKey"}, masked: true},
			{name: "providers.models.contextWindow", file: (*config.Env).PiModels, kind: kindJSON,
				path: []string{"providers", managedProviderPrefix + uid, "models", "#0", "contextWindow"}},
			{name: "providers.models.maxTokens", file: (*config.Env).PiModels, kind: kindJSON,
				path: []string{"providers", managedProviderPrefix + uid, "models", "#0", "maxTokens"}},
			{name: "compaction.reserveTokens", file: (*config.Env).PiSettings, kind: kindJSON,
				path: []string{"compaction", "reserveTokens"}},
			{name: "defaultThinkingLevel", file: (*config.Env).PiSettings, kind: kindJSON,
				path: []string{"defaultThinkingLevel"}},
		}
	case family.WorkBuddy:
		return []companionSpec{
			{name: "entries[].apiKey", file: (*config.Env).WorkbuddyModels, kind: kindJSON,
				path: []string{"@managedEntry", "apiKey"}, masked: true},
			{name: "entries[].maxInputTokens", file: (*config.Env).WorkbuddyModels, kind: kindJSON,
				path: []string{"@managedEntry", "maxInputTokens"}},
			{name: "entries[].maxOutputTokens", file: (*config.Env).WorkbuddyModels, kind: kindJSON,
				path: []string{"@managedEntry", "maxOutputTokens"}},
			{name: "settings.autoCompactWindow", file: (*config.Env).WorkbuddySettings, kind: kindJSON,
				path: []string{"autoCompactWindow"}},
			{name: "settings.autoCompactEnabled", file: (*config.Env).WorkbuddySettings, kind: kindJSON,
				path: []string{"autoCompactEnabled"}},
		}
	}
	return nil
}

// claudeDesktopProfileFile 是 3P 档案文件（ClaudeDesktopFiles 的第三项，顺序即契约顺序）。
func claudeDesktopProfileFile(env *config.Env) string {
	files := env.ClaudeDesktopFiles()
	if len(files) >= 3 {
		return files[2]
	}
	return ""
}

// readCompanions 读盘取值。缺文件/缺键一律给空值（`value:""`），不报错：
// 「这个键现在没有」是正常状态，不是失败。
func (s *Server) readCompanions(id family.ID) []companionKey {
	specs := companionsFor(id, s.currentUID())
	out := make([]companionKey, 0, len(specs))
	for i, spec := range specs {
		key := companionKey{Name: spec.name, Masked: spec.masked, Order: i}
		if raw, ok := s.readCompanionValue(spec); ok {
			if spec.masked {
				key.Value = maskedValue
			} else {
				key.Value = raw
			}
		}
		out = append(out, key)
	}
	return out
}

// readCompanionValue 按 kind 分派到对应的定位层。
func (s *Server) readCompanionValue(spec companionSpec) (string, bool) {
	if spec.file == nil {
		return "", false
	}
	path := spec.file(s.Env)
	if path == "" {
		return "", false
	}
	blob, err := readRegularFile(path)
	if err != nil {
		return "", false
	}
	switch spec.kind {
	case kindEnv:
		doc, err := docmodel.ParseDotenv(blob)
		if err != nil {
			return "", false
		}
		return doc.Get(spec.path[0])
	case kindTOML:
		doc, err := docmodel.ParseTOML(blob)
		if err != nil {
			return "", false
		}
		section, key := spec.path[0], spec.path[1]
		if strings.HasPrefix(section, "@") {
			// 段名由模型 id 派生（grokbuild 的 `[model."<id>"]`）：caller 给完整表名
			// 前缀 `model.`，避免 `[models]` 被误认成模型表。
			resolved, ok := firstSectionWithPrefix(doc, strings.TrimPrefix(section, "@"))
			if !ok {
				return "", false
			}
			section = resolved
		}
		value := doc.GetKey(section, key)
		if value == nil || value.IsEmpty() {
			return "", false
		}
		text, ok := value.String()
		if !ok {
			return strings.TrimSpace(string(value.Raw())), true
		}
		return text, true
	default:
		doc, err := docmodel.ParseJSON5Doc(blob)
		if err != nil {
			return "", false
		}
		return readNodeValue(doc.Root(), spec.path)
	}
}

// firstSectionWithPrefix 找第一个名字以 prefix 起头的 TOML 段。
func firstSectionWithPrefix(doc *docmodel.TOMLDoc, prefix string) (string, bool) {
	for _, section := range doc.Sections() {
		if strings.HasPrefix(section, prefix) {
			return section, true
		}
	}
	return "", false
}

// readNodeValue 按键路径取值。段以 `#` 起头表示数组下标；`@managedEntry` 表示
// 「WorkBuddy models.json 里当前受管条目的下标」；`@rootModel` 从根 `model` 指针中
// 解析当前 provider 下的模型名（OpenCode models 是按模型 id 建的对象）。
func readNodeValue(root *docmodel.Node, path []string) (string, bool) {
	node := root
	for i, seg := range path {
		switch {
		case strings.HasPrefix(seg, "#"):
			idx, err := parseIntStrict(strings.TrimPrefix(seg, "#"))
			if err != nil {
				return "", false
			}
			node = node.At(idx)
		case seg == "@managedEntry" && i == 0:
			idx, ok := managedEntryIndex(root)
			if !ok {
				return "", false
			}
			node = node.At(idx)
		case seg == "@rootModel":
			if i < 3 || path[0] != "provider" || path[i-1] != "models" {
				return "", false
			}
			providerID := path[i-2]
			modelRefNode := root.Get("model")
			if modelRefNode == nil {
				return "", false
			}
			modelRef, ok := modelRefNode.AsString()
			prefix := providerID + "/"
			if !ok || !strings.HasPrefix(modelRef, prefix) || len(modelRef) == len(prefix) {
				return "", false
			}
			node = node.Get(strings.TrimPrefix(modelRef, prefix))
		default:
			node = node.Get(seg)
		}
		if node == nil {
			return "", false
		}
	}
	if text, ok := node.AsString(); ok {
		return text, true
	}
	if value, err := docmodel.NodeToGoValue(node); err == nil {
		switch v := value.(type) {
		case float64:
			return trimFloat(v), true
		case bool:
			if v {
				return "true", true
			}
			return "false", true
		case nil:
			return "", false
		}
	}
	raw := strings.TrimSpace(string(node.AsRaw()))
	if raw == "" || raw == "null" {
		return "", false
	}
	return raw, true
}

// managedEntryIndex 在 WorkBuddy 的 models.json 数组里定位受管条目。
//
// 两条判据都与写链同源（`family/workbuddy` 的受管标记）：条目 id 等于
// `yzagmgr-<uid>`，或条目带 `isDefault` 且 url 指向网关。都命中不了就返回 false
// ——「没有受管条目」是合法状态（尚未建档），不该编一个下标出来。
func managedEntryIndex(root *docmodel.Node) (int, bool) {
	if root == nil {
		return 0, false
	}
	for i := 0; i < root.Len(); i++ {
		entry := root.At(i)
		if entry == nil {
			continue
		}
		id, _ := entry.Get("id").AsString()
		if strings.HasPrefix(id, managedProviderPrefix) {
			return i, true
		}
		if isDefault, ok := entry.Get("isDefault").AsBool(); ok && isDefault {
			return i, true
		}
	}
	return 0, false
}
