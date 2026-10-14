// Package hermes 实现 Hermes 受管单元（family.Family）。
//
// 落盘契约以 `.agents/skills/yzagmgr-config-layer/references/acceptance-recipe.md`
// §A 行 8 与 §L 为唯一事实源：
//
//   - 一个受管文件：`$HOME/.hermes/config.yaml`；
//   - 决定键 = `custom_providers[].base_url`，网关档取值 = `{origin}/v1`，另写 `api_key`；
//   - 逐模型 `models:<id>:{context_length, supports_vision}`，其中 `supports_vision`
//     **仅当目录声明 `supportsImages` 为真时写**（目录沉默时不写，让客户端的 `auto`
//     兜到对任何模型都可用的 `text` 路径，避免对纯文本模型谎报 native）；
//   - 根段 `agent.reasoning_effort = "max"`；
//   - 压缩**只能用比例**：`compression.threshold = 0.9`（0.9 × 1M = 900k）。
//     不写 `compression.threshold_tokens`：其语义是「取两者较小者先触发」，
//     单写绝对值会被上游默认比例 0.50 抢先；
//   - **不写逐模型 `max_tokens`**（会触发 unknown field 警告）⇒ 该族输出维度登记
//     「工具不支持」；
//   - 根段另写 `model.default`（上游模型名）/ `model.provider`（托管条目名）：
//     这不是形状附属——缺它 Hermes 会以空模型 id 发请求而 400；
//   - 托管条目身份沿用 v1 约定：条目名 `yzagmgr-<uid>`（与 opencode/openclaw/pi 同前缀）。
//
// 本族是 additive 型：官方档 = **托管条目与本产品写入的根段键被移除**，用户其余字节逐字
// 存活（`OfficialSeeded() = false`）。
//
// §L 的 live 形态纪律：写链**绝不追加第二份文档**——flow 根只在集合内部插入，
// 多文档只改第一份映射文档，其余文档逐字节保留。渲染后还会再解析一遍自检
// （文档数必须不变），不合格即拒绝写入。
package hermes

import (
	"bytes"
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"strings"

	"github.com/peterplaykkk/yzagmgr/internal/apierr"
	"github.com/peterplaykkk/yzagmgr/internal/config"
	"github.com/peterplaykkk/yzagmgr/internal/docmodel"
	"github.com/peterplaykkk/yzagmgr/internal/family"
	"github.com/peterplaykkk/yzagmgr/internal/tx"
)

const (
	// displayName 与 v1 的 TOOL_CATALOG 一致。
	displayName = "Hermes"

	// decideKeyName 是决定档位的键（契约里的写法，用于 ProbeResult.DecideKey）。
	decideKeyName = "custom_providers[].base_url"

	// 段名与键名（§A 行 8）。
	providersKey = "custom_providers"
	baseURLKey   = "base_url"
	apiKeyKey    = "api_key"
	modelsKey    = "models"
	nameKey      = "name"
	modelKey     = "model"

	agentSection       = "agent"
	reasoningEffortKey = "reasoning_effort"
	compressionSection = "compression"
	thresholdKey       = "threshold"
	modelSection       = "model"
	defaultKey         = "default"
	providerKey        = "provider"

	// 契约取值（§A 行 8）：
	gatewayBaseSuffix = "v1"
	reasoningEffort   = "max"
	// compressionThreshold 是压缩触发点（比例）。0.9 × 1M = 900k，与其它族的
	// 900k 同值；绝对值键（threshold_tokens）会被默认比例抢先，故不写。
	compressionThreshold = "0.9"

	// managedProviderPrefix 是托管条目名的前缀（v1 约定 `yzagmgr-<uid>`）。
	managedProviderPrefix = "yzagmgr-"
	// providerIDExtraKey 是 GatewayInput.Extra 里托管条目 uid 的键名。
	providerIDExtraKey = "providerId"
)

// hermesFamily 是 Hermes 的 family.Family 实现（无状态，值语义）。
type hermesFamily struct{}

// New 返回 Hermes 的受管单元实现。
func New() family.Family { return hermesFamily{} }

func (hermesFamily) ID() family.ID { return family.Hermes }

func (hermesFamily) DisplayName() string { return displayName }

// OfficialSeeded：additive 型家族没有官方种子，官方档 = 托管条目被移除。
func (hermesFamily) OfficialSeeded() bool { return false }

// HasLocalLane：该族在代理前缀表内有独立前缀（本地 route 可达）。
func (hermesFamily) HasLocalLane() bool { return true }

// Paths 只返回**文件**（绝不返回目录，§F-T2）。
func (hermesFamily) Paths(env *config.Env) []string {
	return []string{env.HermesConfig()}
}

// Probe 从盘上字节判定档位：决定键 = `custom_providers[].base_url`。
func (hermesFamily) Probe(env *config.Env) (family.ProbeResult, error) {
	path := env.HermesConfig()
	res := family.ProbeResult{Path: path}

	src, err := tx.ReadForFingerprint(path)
	if err != nil {
		return family.ProbeResult{}, err
	}
	if src == nil {
		// 文件缺席 = 未托管，不是故障（§F-T3）。
		res.Tier = family.TierAbsent
		return res, nil
	}
	res.Present = true

	docs, err := docmodel.ParseYAMLDocs(src)
	if err != nil {
		// 解析失败时读不出决定键：按「条目不存在」判官方档，但把原因写进 Notes。
		res.Tier = family.TierOfficial
		res.Notes = append(res.Notes, "config.yaml 解析失败，决定键按不存在处理: "+err.Error())
		return res, nil
	}
	if len(docs) == 0 {
		res.Tier = family.TierOfficial
		return res, nil
	}
	if len(docs) > 1 {
		res.Notes = append(res.Notes,
			fmt.Sprintf("live 是多文档（%d 份）：决定键取自第一份文档", len(docs)))
	}
	if docmodel.DetectFlowRoot(src) {
		res.Notes = append(res.Notes, "live 是 flow 根（JSON 形态）：写入只在集合内部进行，不追加第二份文档")
	}

	if managedBase := managedEntryBaseURL(docs[0]); managedBase != "" {
		res.Tier = family.TierGateway
		res.DecideKey = decideKeyName
		res.DecideValue = managedBase
		if note := foreignEndpointNote(managedBase); note != "" {
			res.Notes = append(res.Notes, note)
		}
		return res, nil
	}
	if otherName, otherBase := firstEntryBaseURL(docs[0]); otherBase != "" {
		res.Tier = family.TierGateway
		res.DecideKey = decideKeyName
		res.DecideValue = otherBase
		res.Notes = append(res.Notes,
			fmt.Sprintf("未找到本产品托管条目（name 前缀 %q），决定键取自首个带 base_url 的条目 %q",
				managedProviderPrefix, otherName))
		if note := foreignEndpointNote(otherBase); note != "" {
			res.Notes = append(res.Notes, note)
		}
		return res, nil
	}

	res.Tier = family.TierOfficial
	if seq := providersSeq(docs[0]); seq != nil && seq.Len() > 0 {
		res.Notes = append(res.Notes,
			"存在自定义 provider 条目但没有任何 base_url：按官方档处理，该文件可能残缺")
	}
	return res, nil
}

// PlanGateway 计算网关档目标字节（§A 行 8 的键位 + 根段键）。
//
// 纯计算，不触盘写入。
func (hermesFamily) PlanGateway(env *config.Env, in family.GatewayInput) ([]family.FilePlan, error) {
	if in.EndpointIsFullBase {
		in = fullBaseInput(in)
	}
	var base string
	var err error
	if in.EndpointIsFullBase {
		base, err = fullAPIBase(in.Origin)
	} else {
		base, err = gatewayBase(in.Origin)
	}
	if err != nil {
		return nil, err
	}
	model := in.Model
	if strings.TrimSpace(model) == "" {
		return nil, apierr.New(apierr.CodeInvalidRequest,
			"hermes 网关档需要模型 id（策略 default）")
	}
	// 凭据与模型 id **原样落盘**（只判空、不做 trim）。
	token := in.Token
	if strings.TrimSpace(token) == "" {
		return nil, apierr.New(apierr.CodeInvalidRequest, "hermes 网关档需要推理令牌")
	}
	// context_length 是该族唯一的窗口键；0/负值写下去等于让客户端按默认估算。
	if in.ContextWindow <= 0 {
		return nil, apierr.New(apierr.CodeInvalidRequest,
			"hermes 网关档需要正整数 context_length（当前 %d）", in.ContextWindow)
	}
	providerName, err := managedProviderName(in)
	if err != nil {
		return nil, err
	}

	path := env.HermesConfig()
	before, err := tx.ReadForFingerprint(path)
	if err != nil {
		return nil, err
	}
	set, err := docmodel.ParseYAMLSet(before)
	if err != nil {
		return nil, apierr.Wrap(apierr.CodeConfigInvalid, err,
			"Hermes config.yaml 不是可解析的 YAML，已拒绝写入")
	}

	spec := entrySpec{
		name:           providerName,
		baseURL:        base,
		apiKey:         token,
		modelID:        model,
		contextLength:  in.ContextWindow,
		supportsVision: in.SupportsImages,
	}
	// 形态分派（§L 的三种 live 形态）：block / 多文档走块文本合并，
	// flow 根（JSON 形态是它的特例）必须在集合内部插入**单行**值。
	providersValue, err := providersValue(set, docmodel.DetectFlowRoot(before), spec)
	if err != nil {
		return nil, err
	}

	// 根段键：推理档、压缩比例、模型路由（缺 model.default 会让 Hermes 以空模型 id
	// 发请求而 400，故它属于「让工具真能用」的必要写入）。
	steps := []upsertStep{{path: []string{providersKey}, value: providersValue}}
	effort := reasoningEffortFor(in)
	if effort == "" {
		// An unknown explicit full base has no verified effort enum. Remove the
		// writer-owned gateway default instead of sending the forced "max" value.
		if err := set.DeletePathInDoc(0, []string{agentSection, reasoningEffortKey}); err != nil {
			return nil, upsertErr(err, agentSection+"."+reasoningEffortKey)
		}
	} else {
		steps = append(steps, upsertStep{
			path: []string{agentSection, reasoningEffortKey}, value: []byte(effort),
		})
	}
	steps = append(steps,
		upsertStep{path: []string{compressionSection, thresholdKey}, value: []byte(compressionThreshold)},
		upsertStep{path: []string{modelSection, defaultKey}, value: []byte(yamlScalar(model))},
		upsertStep{path: []string{modelSection, providerKey}, value: []byte(yamlScalar(providerName))},
	)
	after, err := applyUpsertSteps(set, before, steps)
	if err != nil {
		return nil, err
	}
	return tx.BuildPlans(map[string][]byte{path: after})
}

func fullBaseInput(in family.GatewayInput) family.GatewayInput {
	in.SupportsImages = false // A provider base does not establish model-specific vision support.
	if family.FullBaseProviderFor(in) != "" {
		in.ContextWindow = 1_000_000 // Vendor-verified current provider models.
	} else {
		in.ContextWindow = 128_000 // Conservative client default for an unknown endpoint.
	}
	return in
}

// reasoningEffortFor follows each verified chat endpoint: MiMo tops out at high;
// DeepSeek and GLM 5.3 accept max. Unknown endpoints receive no forced effort.
func reasoningEffortFor(in family.GatewayInput) string {
	if !in.EndpointIsFullBase {
		return reasoningEffort
	}
	switch family.FullBaseProviderFor(in) {
	case family.FullBaseProviderDeepSeek:
		return "max"
	case family.FullBaseProviderMiMo:
		return "high"
	case family.FullBaseProviderGLM:
		return "max"
	default:
		return ""
	}
}

// fullAPIBase validates and preserves a user profile's complete protocol base.
func fullAPIBase(raw string) (string, error) {
	base := strings.TrimSpace(raw)
	if base == "" {
		return "", apierr.New(apierr.CodeInvalidRequest, "自建 provider 需要完整 API 基址（origin）")
	}
	u, err := url.Parse(base)
	if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
		return "", apierr.New(apierr.CodeInvalidRequest,
			"完整 API 基址非法（需要 http(s)://host[:port]/path 形态）")
	}
	return strings.TrimRight(base, "/"), nil
}

// PlanOfficial 计算回到官方档的目标字节：移除**托管条目**与本产品写入的根段键，
// 用户其余字节逐字存活（additive 型的「往返」等价物，UC-005 A2）。
//
// 整段只剩本产品内容时连同容器一起移除（`custom_providers` 键、`agent`/`compression`/
// `model` 段），这样「官方档 → 网关档 → 官方档」回到起点字节、不留空壳。
func (hermesFamily) PlanOfficial(env *config.Env) ([]family.FilePlan, error) {
	path := env.HermesConfig()
	before, err := tx.ReadForFingerprint(path)
	if err != nil {
		return nil, err
	}
	set, err := docmodel.ParseYAMLSet(before)
	if err != nil {
		return nil, apierr.Wrap(apierr.CodeConfigInvalid, err,
			"Hermes config.yaml 不是可解析的 YAML，已拒绝写入")
	}
	root := set.Doc(0)
	if root == nil || root.Kind != docmodel.KObject {
		// 没有可寻址的映射根 ⇒ 本产品不可能写过托管条目，原样返回（幂等）。
		return tx.BuildPlans(map[string][]byte{path: before})
	}

	// 1) 托管条目：全部元素都是我们的 ⇒ 连 `custom_providers` 键一起删；否则按元素删。
	if seq := providersSeq(root); seq != nil {
		ours := 0
		for _, elem := range seq.Elems {
			if isManagedEntry(elem) {
				ours++
			}
		}
		switch {
		case ours == 0:
		case ours == seq.Len():
			if err := set.DeletePathInDoc(0, []string{providersKey}); err != nil {
				return nil, upsertErr(err, providersKey)
			}
		default:
			if _, err := set.DeleteFromSeqInDoc(0, []string{providersKey}, isManagedEntry); err != nil {
				return nil, upsertErr(err, providersKey)
			}
		}
	}

	// 2) 根段键：段里只剩本产品的键时连段一起删，否则只删键。
	for _, spec := range []struct {
		section  string
		keys     []string
		ownsKeys map[string]bool
	}{
		{agentSection, []string{reasoningEffortKey}, map[string]bool{reasoningEffortKey: true}},
		{compressionSection, []string{thresholdKey}, map[string]bool{thresholdKey: true}},
		{modelSection, []string{defaultKey, providerKey},
			map[string]bool{defaultKey: true, providerKey: true}},
	} {
		node := root.MemberOf(spec.section)
		if node == nil || node.Val.Kind != docmodel.KObject {
			continue
		}
		present := 0
		for _, key := range spec.keys {
			if node.Val.MemberOf(key) != nil {
				present++
			}
		}
		if present == 0 {
			continue
		}
		if sectionIsEntirelyOurs(node.Val, spec.ownsKeys) {
			if err := set.DeletePathInDoc(0, []string{spec.section}); err != nil {
				return nil, upsertErr(err, spec.section)
			}
			continue
		}
		for _, key := range spec.keys {
			if node.Val.MemberOf(key) == nil {
				continue
			}
			if err := set.DeletePathInDoc(0, []string{spec.section, key}); err != nil {
				return nil, upsertErr(err, spec.section+"."+key)
			}
		}
	}

	after, err := verifyWriteShape(before, set.Render())
	if err != nil {
		return nil, err
	}
	plans, err := tx.BuildPlans(map[string][]byte{path: after})
	if err != nil {
		return nil, err
	}
	// 「托管条目/键被移除」在「整份文件本来只由本产品内容构成」时的精确等价物是
	// **文件消失**（family.go 的 Delete 语义：恢复时移除接管时创建的路径）；
	// 不这样做，一次官方↔网关往返就会留下一个空壳 config.yaml。
	if len(plans) == 1 && plans[0].Existed && isBlankOnly(after) {
		plans[0].Delete = true
		plans[0].After = nil
	}
	return plans, nil
}

// isBlankOnly 报告字节串是否只剩空白（注释算内容：它是用户字节，不得据此删文件）。
func isBlankOnly(b []byte) bool {
	for _, line := range bytes.Split(b, []byte("\n")) {
		if len(bytes.TrimSpace(line)) > 0 {
			return false
		}
	}
	return true
}

// ── 条目与块文本 ────────────────────────────────────────────────────────────

// entrySpec 是一条托管 provider 条目的全部取值。
type entrySpec struct {
	name           string
	baseURL        string
	apiKey         string
	modelID        string
	contextLength  int
	supportsVision bool
}

// managedEntryBlock 把托管条目渲染成块文本（相对缩进，首行即 `- ` 列表项）。
//
// 「Go 值 → 块文本」的方案要点（yaml 层收的是 rawValue 字节，这一小段由本包自备）：
//   - 键序 = §A 的书写序 + v1 追加 `name`/`model` 的次序，与本机 v1 盘上形态一致；
//   - 标量走 yamlScalar：安全字符集内用裸标量（网关基址/令牌/模型 id/条目名都在集内），
//     其余一律双引号 + 转义——宁可加引号，也不产出会被读者截断或误判类型的标量；
//   - 数值/布尔按 YAML 原生类型裸写（`context_length: 1000000`、`supports_vision: true`）；
//   - 行尾不带换行：调用方在拼接时补（与 `BlockValueFromNode` 的产物一致）。
func managedEntryBlock(spec entrySpec) []byte {
	var b bytes.Buffer
	fmt.Fprintf(&b, "- %s: %s\n", baseURLKey, yamlScalar(spec.baseURL))
	fmt.Fprintf(&b, "  %s: %s\n", apiKeyKey, yamlScalar(spec.apiKey))
	fmt.Fprintf(&b, "  %s:\n", modelsKey)
	fmt.Fprintf(&b, "    %s:\n", yamlScalar(spec.modelID))
	fmt.Fprintf(&b, "      context_length: %d\n", spec.contextLength)
	// supports_vision 只在目录声明 supportsImages 时写：目录沉默时不写，
	// 让客户端的 auto 兜到 text 路径。
	if spec.supportsVision {
		b.WriteString("      supports_vision: true\n")
	}
	fmt.Fprintf(&b, "  %s: %s\n", nameKey, yamlScalar(spec.name))
	fmt.Fprintf(&b, "  %s: %s", modelKey, yamlScalar(spec.modelID))
	return b.Bytes()
}

// providersValue 生成 `custom_providers` 的目标值字节：形态随 live 走。
//
//   - block / 多文档：块文本（既有条目按原文折算缩进后逐条保留，托管条目置末位）；
//   - flow 根：**单行** flow 文本——flow 上下文只接受单行值，多行块值会被层拒绝；
//     既有条目同样逐条保留（原文里带换行的条目无法在单行里表达，此时显式拒绝）。
//
// 两种形态都不会追加第二份文档：块文本由层缩进进 `custom_providers:` 之下，
// flow 文本插进集合内部。
func providersValue(set *docmodel.YAMLSet, flow bool, spec entrySpec) ([]byte, error) {
	root := set.Doc(0)
	seq := (*docmodel.Node)(nil)
	if root != nil {
		if member := root.MemberOf(providersKey); member != nil && member.Val.Kind != docmodel.KArray {
			// 键在场但不是序列（用户写成了映射或标量）：覆盖它会毁掉用户结构，拒绝。
			return nil, apierr.New(apierr.CodeConfigInvalid,
				"Hermes config.yaml 的 %s 不是序列，已拒绝写入", providersKey).
				WithDetails(map[string]any{"reason": "providers_not_sequence", "path": providersKey})
		}
		seq = providersSeq(root)
	}
	if !flow {
		var out []byte
		if seq != nil {
			for _, elem := range seq.Elems {
				if isManagedEntry(elem) {
					continue
				}
				out = append(out, docmodel.BlockValueFromNode(set.Src(), elem, "- ")...)
				out = append(out, '\n')
			}
		}
		return append(out, managedEntryBlock(spec)...), nil
	}
	if seq != nil {
		for _, elem := range seq.Elems {
			if !isManagedEntry(elem) && bytes.IndexByte(elem.AsRaw(), '\n') >= 0 {
				return nil, apierr.New(apierr.CodeConfigInvalid,
					"Hermes live 是 flow 根，且既有 provider 条目跨多行：无法在不追加第二份文档的前提下写入，已拒绝").
					WithDetails(map[string]any{"reason": "flow_entry_multiline"})
			}
		}
	}
	var out []byte
	out = append(out, '[')
	if seq != nil {
		first := true
		for _, elem := range seq.Elems {
			if isManagedEntry(elem) {
				continue
			}
			if !first {
				out = append(out, ',', ' ')
			}
			first = false
			out = append(out, elem.AsRaw()...)
		}
		if !first {
			out = append(out, ',', ' ')
		}
	}
	out = append(out, managedEntryFlow(spec)...)
	return append(out, ']'), nil
}

// managedEntryFlow 把托管条目渲染成单行 flow 映射（键走 JSON 风格双引号，
// 与 yaml 层在 flow 上下文里新建键的写法一致；JSON 是 flow YAML 的子集）。
func managedEntryFlow(spec entrySpec) []byte {
	var b bytes.Buffer
	fmt.Fprintf(&b, "{%s: %s, %s: %s, %s: {%s: {%s: %d",
		jsonKey(baseURLKey), jsonScalar(spec.baseURL),
		jsonKey(apiKeyKey), jsonScalar(spec.apiKey),
		jsonKey(modelsKey), jsonKey(spec.modelID), jsonKey("context_length"), spec.contextLength)
	if spec.supportsVision {
		fmt.Fprintf(&b, ", %s: true", jsonKey("supports_vision"))
	}
	fmt.Fprintf(&b, "}}, %s: %s, %s: %s}",
		jsonKey(nameKey), jsonScalar(spec.name),
		jsonKey(modelKey), jsonScalar(spec.modelID))
	return b.Bytes()
}

// jsonKey 产出 flow 上下文里的键字面量（双引号 + 最小转义）。
func jsonKey(s string) string { return jsonScalar(s) }

// jsonScalar 产出 JSON/YAML flow 里的字符串字面量。
func jsonScalar(s string) string {
	var b strings.Builder
	b.WriteByte('"')
	for i := 0; i < len(s); i++ {
		switch c := s[i]; c {
		case '"':
			b.WriteString(`\"`)
		case '\\':
			b.WriteString(`\\`)
		case '\n':
			b.WriteString(`\n`)
		case '\r':
			b.WriteString(`\r`)
		case '\t':
			b.WriteString(`\t`)
		default:
			if c < 0x20 {
				fmt.Fprintf(&b, `\u%04X`, c)
				continue
			}
			b.WriteByte(c)
		}
	}
	b.WriteByte('"')
	return b.String()
}

// upsertStep 是一次「路径 → 值」的定向写入。
type upsertStep struct {
	path  []string
	value []byte
}

// applyUpsertSteps 逐步写入并返回最终字节。
//
// 为什么每个键单独走一遍「解析 → 写 → 渲染」：解析树**不随待应用编辑更新**，
// 同一轮里连写同一父级下的多个新键时，第二次写入看不到第一次刚建的段，会再建一个
// 同名段（盘上出现重复键，YAML 语义变成 last-wins、前一个键静默失效）。
// 逐键重新解析即可让每一步都落在最新的源上。
func applyUpsertSteps(set *docmodel.YAMLSet, before []byte, steps []upsertStep) ([]byte, error) {
	src := before
	cur := set
	for _, step := range steps {
		if err := cur.UpsertPathInDoc(0, step.path, step.value); err != nil {
			return nil, upsertErr(err, strings.Join(step.path, "."))
		}
		src = cur.Render()
		next, err := docmodel.ParseYAMLSet(src)
		if err != nil {
			return nil, apierr.New(apierr.CodeConfigInvalid,
				"Hermes 写链中间产物不可解析，已拒绝写入: %v", err).
				WithDetails(map[string]any{"reason": "render_unparseable"})
		}
		cur = next
	}
	return verifyWriteShape(before, src)
}

// managedRootKeys 是本产品会写入的根段键。
var managedRootKeys = []string{providersKey, agentSection, compressionSection, modelSection}

// verifyWriteShape 落地前的自检：产物必须可解析、文档数不得增加、且不得引入重复根键。
//
// 前两条守 §L（「追加第二份文档」的历史事故）；第三条守上面 applyUpsertSteps 注释里
// 那条 last-wins 陷阱——重复根键会让刚写进去的托管值静默失效，宁可拒绝写入。
func verifyWriteShape(before, after []byte) ([]byte, error) {
	if len(bytes.TrimSpace(after)) == 0 {
		return after, nil
	}
	beforeDocs, err := docmodel.ParseYAMLDocs(before)
	if err != nil {
		beforeDocs = nil
	}
	afterDocs, err := docmodel.ParseYAMLDocs(after)
	if err != nil {
		return nil, apierr.New(apierr.CodeConfigInvalid,
			"Hermes 写链产物不可解析，已拒绝写入: %v", err).
			WithDetails(map[string]any{"reason": "render_unparseable"})
	}
	if beforeDocs != nil && len(afterDocs) > len(beforeDocs) {
		return nil, apierr.New(apierr.CodeConfigInvalid,
			"Hermes 写链产物出现了第二份文档（%d → %d），已拒绝写入",
			len(beforeDocs), len(afterDocs)).
			WithDetails(map[string]any{"reason": "second_document"})
	}
	beforeCounts, afterCounts := map[string]int{}, map[string]int{}
	if len(beforeDocs) > 0 {
		beforeCounts = countManagedRootKeys(beforeDocs[0])
	}
	if len(afterDocs) > 0 {
		afterCounts = countManagedRootKeys(afterDocs[0])
	}
	for key, n := range afterCounts {
		if n > 1 && n > beforeCounts[key] {
			return nil, apierr.New(apierr.CodeConfigInvalid,
				"Hermes 写链产物引入了重复根键 %q（YAML last-wins 会让托管值静默失效），已拒绝写入", key).
				WithDetails(map[string]any{"reason": "duplicate_root_key", "key": key})
		}
	}
	return after, nil
}

// countManagedRootKeys 数出文档根上每个本产品段键的出现次数（含重复定义）。
func countManagedRootKeys(root *docmodel.Node) map[string]int {
	counts := map[string]int{}
	if root == nil || root.Kind != docmodel.KObject {
		return counts
	}
	for i := range root.Members {
		for _, key := range managedRootKeys {
			if root.Members[i].Key == key {
				counts[key]++
			}
		}
	}
	return counts
}

// providersSeq 取 `custom_providers` 序列节点；键不存在返回 nil。
func providersSeq(root *docmodel.Node) *docmodel.Node {
	node := root.MemberOf(providersKey)
	if node == nil || node.Val.Kind != docmodel.KArray {
		return nil
	}
	return node.Val
}

// isManagedEntry 报告一个条目是否属于本产品托管（按 v1 的条目名约定匹配）。
//
// 只按名字前缀识别（而不是「名字等于本轮派生的全名」）：PlanOfficial 没有任何输入，
// 拿不到 uid；前缀 `yzagmgr-` 就是四家 additive 家族共用的托管条目命名约定。
func isManagedEntry(node *docmodel.Node) bool {
	if node == nil || node.Kind != docmodel.KObject {
		return false
	}
	name, ok := node.Get(nameKey).AsString()
	if !ok {
		return false
	}
	return strings.HasPrefix(name, managedProviderPrefix)
}

// managedEntryBaseURL 取托管条目的 base_url（找不到托管条目时返回空串）。
func managedEntryBaseURL(root *docmodel.Node) string {
	seq := providersSeq(root)
	if seq == nil {
		return ""
	}
	for _, elem := range seq.Elems {
		if !isManagedEntry(elem) {
			continue
		}
		base, ok := elem.Get(baseURLKey).AsString()
		if ok && strings.TrimSpace(base) != "" {
			return base
		}
	}
	return ""
}

// firstEntryBaseURL 取首个带非空 base_url 的条目（用于「用户自建自定义 provider」形态）。
func firstEntryBaseURL(root *docmodel.Node) (string, string) {
	seq := providersSeq(root)
	if seq == nil {
		return "", ""
	}
	for _, elem := range seq.Elems {
		base, ok := elem.Get(baseURLKey).AsString()
		if !ok || strings.TrimSpace(base) == "" {
			continue
		}
		name, _ := elem.Get(nameKey).AsString()
		return name, base
	}
	return "", ""
}

// sectionIsEntirelyOurs 报告一个段落的成员是否全在本产品的键集内。
func sectionIsEntirelyOurs(node *docmodel.Node, ours map[string]bool) bool {
	if node == nil || node.Kind != docmodel.KObject || len(node.Members) == 0 {
		return false
	}
	for i := range node.Members {
		if !ours[node.Members[i].Key] {
			return false
		}
	}
	return true
}

// managedProviderName 派生托管条目名 `yzagmgr-<uid>`。
//
// uid 取自 `GatewayInput.Extra["providerId"]`；缺省由 TokenRef
// （`yzagmgr-managed-token://<uid>`）派生。两处都没有时响亮拒绝——条目名是官方档
// 识别托管条目的唯一依据，猜一个名字会让「移除托管条目」失准。
func managedProviderName(in family.GatewayInput) (string, error) {
	raw := strings.TrimSpace(in.Extra[providerIDExtraKey])
	if raw == "" {
		raw = strings.TrimSpace(uidFromTokenRef(in.TokenRef))
	}
	if raw == "" {
		return "", apierr.New(apierr.CodeInvalidRequest,
			"hermes 网关档需要托管条目 id（Extra[%q] 或 TokenRef 派生）", providerIDExtraKey)
	}
	if strings.HasPrefix(raw, managedProviderPrefix) {
		return raw, nil
	}
	return managedProviderPrefix + raw, nil
}

// uidFromTokenRef 从 `yzagmgr-managed-token://<uid>` 形态里取末段。
func uidFromTokenRef(ref string) string {
	tail := ref
	if i := strings.LastIndex(tail, "://"); i >= 0 {
		tail = tail[i+len("://"):]
	}
	if i := strings.LastIndexAny(tail, "/:"); i >= 0 {
		tail = tail[i+1:]
	}
	return tail
}

// ── 渲染自检与错误归一 ──────────────────────────────────────────────────────

// upsertErr 把 yaml 层的拒绝语义翻成产品错误码：拒绝写入是 config_invalid，
// 其余解析层问题同样不该被当成成功。
func upsertErr(err error, what string) error {
	if errors.Is(err, docmodel.ErrRefuseSecondRoot) {
		return apierr.New(apierr.CodeConfigInvalid,
			"Hermes config.yaml 的 %s 无法在不追加第二份文档的前提下写入，已拒绝", what).
			WithDetails(map[string]any{"reason": "refused_second_root", "path": what})
	}
	return apierr.Wrap(apierr.CodeConfigInvalid, err, "Hermes config.yaml 写入 %s 失败", what)
}

// gatewayBase 返回网关档基址：`{origin}/v1`。
func gatewayBase(origin string) (string, error) {
	origin = strings.TrimSpace(origin)
	if origin == "" {
		return "", apierr.New(apierr.CodeInvalidRequest, "网关档需要网关基址（origin）")
	}
	u, err := url.Parse(origin)
	if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
		return "", apierr.New(apierr.CodeInvalidRequest,
			"网关基址非法（需要 http(s)://host[:port] 形态）")
	}
	return strings.TrimRight(origin, "/") + "/" + gatewayBaseSuffix, nil
}

// yamlPlainScalar 匹配「可以裸写的标量」：首字符字母或数字，其余字母数字与 `._/:-`。
var yamlPlainScalar = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._/:-]*$`)

// yamlScalar 把 Go 字符串渲染成 YAML 标量。
//
// 裸写条件（三条全满足才裸写）：非空、命中 yamlPlainScalar、且不是会被 YAML 解析成
// 布尔/空值的字样（true/false/yes/no/on/off/null/~/）、也不像纯数字。
// 其余一律双引号 + 最小转义——我们的值都在安全集内（盘上形态与 v1 一致），
// 但用户/策略给的怪值绝不能裸写进文件。
func yamlScalar(s string) string {
	if s != "" && yamlPlainScalar.MatchString(s) && !yamlScalarIsTypedLookalike(s) {
		return s
	}
	var b strings.Builder
	b.WriteByte('"')
	for i := 0; i < len(s); i++ {
		switch c := s[i]; c {
		case '"':
			b.WriteString(`\"`)
		case '\\':
			b.WriteString(`\\`)
		case '\n':
			b.WriteString(`\n`)
		case '\r':
			b.WriteString(`\r`)
		case '\t':
			b.WriteString(`\t`)
		default:
			if c < 0x20 {
				fmt.Fprintf(&b, `\x%02X`, c)
				continue
			}
			b.WriteByte(c)
		}
	}
	b.WriteByte('"')
	return b.String()
}

// yamlScalarIsTypedLookalike 报告字符串是否会被 YAML 读成非字符串（布尔/空/数字）。
func yamlScalarIsTypedLookalike(s string) bool {
	switch strings.ToLower(s) {
	case "true", "false", "yes", "no", "on", "off", "null", "~", "y", "n":
		return true
	}
	numeric := true
	for i := 0; i < len(s); i++ {
		c := s[i]
		if (c < '0' || c > '9') && c != '.' && c != '+' && c != '-' && c != '_' {
			numeric = false
			break
		}
	}
	return numeric
}

// foreignEndpointNote 在决定键指向非受管网关端点、也不是本地车道时给一条可读备注。
// 只是诊断线索：档位判定仍只认键值本身是否存在（§F-T5），备注不改变档位。
func foreignEndpointNote(raw string) string {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u.Host == "" {
		return ""
	}
	host := u.Hostname()
	switch {
	case host == "127.0.0.1", host == "localhost", host == "::1":
		return ""
	}
	return fmt.Sprintf("决定键指向的端点不是受管网关域: %s", host)
}
