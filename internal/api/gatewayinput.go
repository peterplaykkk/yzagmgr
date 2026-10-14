// `family.GatewayInput` 的装配：把「策略 + 契约常量 + 会话令牌」翻译成家族写链的输入。
//
// 为什么必须在这里做对：家族单测只验「给什么写什么」，不验「给的对不对」。
// 数值写错会静默发出错误的窗口/压缩/输出配置——真机上的表现是「压缩在错误的位置触发」，
// 在低层测试里完全看不出来。因此本文件的每个数字都带来源行号。
//
// 数值来源：`doc/verification-baseline.md` §1/§2（它又从 v1 `enterprise/profiles.rs`
// 的常量块与 acceptance-recipe §A 抽出；冲突时以 §A 为准）。
package api

import (
	"context"
	"strconv"
	"strings"

	"github.com/peterplaykkk/yzagmgr/internal/apierr"
	"github.com/peterplaykkk/yzagmgr/internal/family"
	"github.com/peterplaykkk/yzagmgr/internal/family/claudedesktop"
	"github.com/peterplaykkk/yzagmgr/internal/family/pi"
	"github.com/peterplaykkk/yzagmgr/internal/policy"
)

// 契约数值基线（verification-baseline §1）。
const (
	// managedContextWindow 是上游模型的上下文窗口（baseline §1，DEEPSEEK_FLASH_CONTEXT_WINDOW）。
	managedContextWindow = 1_000_000
	// managedCompactWindow 是 1M 档的压缩触发点（baseline §1，MANAGED_COMPACT_WINDOW）。
	managedCompactWindow = 900_000
	// managedMaxOutputCeiling 是**除 pi 外**各族的写入上限（baseline §1，MANAGED_MAX_OUTPUT_CEILING）。
	managedMaxOutputCeiling = 128_000
	// managedPiCompactWindow 是 pi 的触发点（baseline §1，MANAGED_PI_COMPACT_WINDOW；
	// 派生自 墙 1_048_576 − 384_000 − 24_576）。
	managedPiCompactWindow = 640_000
	// managedPiMaxOutput 是上游模型的官方最大输出；**只有 pi 对齐它**
	// （baseline §1，DEEPSEEK_FLASH_MAX_OUTPUT）。
	managedPiMaxOutput = 384_000
	// managedTierModel 是客户端可见固定档名（baseline §1，MANAGED_CLAUDE_TIER_MODEL）。
	// 刻意不是 `claude-opus-4-8`：改名是为避开客户端启动态的档位钉。
	managedTierModel = "claude-fable-5-1"
	// managedTokenScheme 是令牌引用串的协议前缀（baseline §1，MANAGED_TOKEN_SCHEME）。
	managedTokenScheme = "yzagmgr-managed-token://"
	// managedProviderPrefix 是托管 provider id 的前缀（openclaw/opencode/pi 的 live 配置用）。
	managedProviderPrefix = "yzagmgr-"
	// compactWindowAndMaxOutputMustFit 是编译期不变量的表述载体：见下方断言。
	gatewayDeclaredTokenWall = 1_048_576
)

// 编译期不变量（baseline §1）：压缩触发点与输出上限之和不得越过网关预检墙。
// 越墙的后果是请求被判 400，而这在设计期就该被挡住——写成常量除零，越墙即编译失败。
const (
	_ = 1 / (gatewayDeclaredTokenWall - managedCompactWindow - managedMaxOutputCeiling)
	_ = 1 / (gatewayDeclaredTokenWall - managedPiCompactWindow - managedPiMaxOutput)
)

// familyWindowSpec 是「一个家族的窗口/压缩/输出三件套」。
//
// 三件套必须成组给：窗口是写进客户端的上下文窗口，触发点是自动压缩发生的绝对 token 数。
// 二者是两个不同的量，不得共用一个数（baseline §2 开头明写）。
type familyWindowSpec struct {
	ContextWindow int
	CompactWindow int
	MaxOutput     int
}

// windowSpecFor 给出该家族的窗口三件套。
//
// pi 与其余九族的差别是**实质性的**：pi 吃上游官方 384K 输出，触发点随之压到 640K；
// 其余九族统一走 128K 天花板 + 900K 触发点。任何「顺手统一」都会破坏 pi 的声明一致性。
func windowSpecFor(id family.ID) familyWindowSpec {
	if id == family.Pi {
		return familyWindowSpec{
			ContextWindow: pi.ContextWindowDefault, // 1_000_000
			CompactWindow: pi.CompactWindowDefault, // 640_000
			MaxOutput:     pi.OfficialMaxOutput,    // 384_000
		}
	}
	return familyWindowSpec{
		ContextWindow: managedContextWindow,
		CompactWindow: managedCompactWindow,
		MaxOutput:     managedMaxOutputCeiling,
	}
}

// uid 是本机用户 id（受管 provider id 与卡片 id 都从它派生）。
//
// 必须是**稳定**值：写进 live 配置的 provider id 不能每次预览都变，
// 否则「再 apply 一次」不再是逐字节相同（破坏幂等，UC-003 A7）。
const anonymousUID = "local"

// currentUID 返回当前用户 id：登录链删除后无会话身份，恒为本机占位
// （design §2.3 gatewayinput 行：currentUID→本地）。
func (s *Server) currentUID() string { return anonymousUID }

// managedProfileIDFor 给出受管卡 id（= 托管 provider id 的同一形态）。
func managedProfileIDFor(uid string) string { return managedProviderPrefix + uid }

// tokenRefFor 给出令牌引用串（卡片记账用，不是明文令牌）。
func tokenRefFor(uid string) string { return managedTokenScheme + uid }

// desktopGatewayKey 派生 claude-desktop 车道的**本地网关键**（UC-045）。
//
// 与写侧同源：档案里的 `inferenceGatewayApiKey` 就是 `claudedesktop.ProxyKey(in)` 的输出，
// 而 Proxy 形态的输入由 [Server.gatewayInputFor] 装配（`TokenRef = tokenRefFor(uid)`）——
// 因此这里用**同一个函数、同一份输入**再算一次，绝不另抄一份哈希配方（两处各写一份必然
// 漂移，漂移的表现正是 UC-045 的缺陷形态：客户端带着档案里的键却换不到真令牌）。
//
// 键只随 uid 派生（ProxyKey 的种子是 TokenRef，缺省才退到 Model），因此「当前生效的
// desktop 档案」与「当前会话身份」一致时两处恒同值。
func (s *Server) desktopGatewayKey() string {
	return claudedesktop.ProxyKey(family.GatewayInput{TokenRef: tokenRefFor(s.currentUID())})
}

// localRouteBase 是本地代理的基址（家族各自拼自己的前缀）。
func (s *Server) localRouteBase() string {
	return "http://127.0.0.1:" + strconv.Itoa(s.ProxyPort)
}

// inferenceToken 取推理令牌：装配期种子（`YZAGMGR_TOKEN_FILE`）→ 托管凭据库 → 报错。
//
// 会话令牌路径（Session.EnsureToken）已随登录链删除（design §2.3：
// inferenceToken→托管凭据/直连 key）。种子只在装配期注入一次，运行期无法打开或关闭；
// 无种子时按托管凭据库（tokenVault）取 `yzagmgr-managed-token://<uid>` 引用下的令牌，
// 取不到即 unauthenticated——不编造、不出网。
func (s *Server) inferenceToken(ctx context.Context) (string, error) {
	_ = ctx // 源是本地种子与本地凭据库：没有要等待的网络调用。
	if s.TokenSeed != "" {
		return s.TokenSeed, nil
	}
	token, err := s.routerTokens.InferenceTokenFor(tokenRefFor(s.currentUID()))
	if err != nil {
		return "", err
	}
	if strings.TrimSpace(token) == "" {
		return "", apierr.New(apierr.CodeUnauthenticated, "当前没有可用的推理令牌（无托管凭据与装配期种子）")
	}
	return token, nil
}

// tokenForFamily 决定「该家族是否有资格拿到真令牌」。
//
// 分界线不是偏好而是安全边界（UC-003 A15）：盘上只允许出现占位符的家族**不拿**真令牌，
// 于是「家族实现里写错一行就把真令牌落盘」在装配层就已经不可能发生。
func (s *Server) tokenForFamily(ctx context.Context, id family.ID, desktopMode string) (string, error) {
	return s.tokenForFamilyMode(ctx, id, desktopMode, true)
}

// tokenForFamilyMode selects credentials with an explicit fetch policy.
// Draft preview must be read-only: it may use an already available token but
// cannot call EnsureToken, which can contact the gateway and persist a token.
func (s *Server) tokenForFamilyMode(ctx context.Context, id family.ID, desktopMode string, allowFetch bool) (string, error) {
	switch id {
	case family.Claude:
		// claude 的本地 route 形态：盘上是 TokenPlaceholder，真令牌由代理转发时注入。
		return "", nil
	case family.ClaudeDesktop:
		if desktopMode != string(claudedesktop.ModeDirect) {
			// Proxy 形态（默认）：凭据位写确定性自发密钥，不需要真令牌。
			return "", nil
		}
	}
	if !allowFetch {
		if s.TokenSeed != "" {
			return s.TokenSeed, nil
		}
		// 会话就绪令牌缝（InferenceTokenIfReady）已随登录链删除（design §2.2）：
		// 无种子即「没有已就绪的令牌」，草稿预览不联网获取凭据的语义不变。
		return "", apierr.New(apierr.CodeUnauthenticated,
			"当前没有已就绪的推理令牌；草稿预览不会联网获取凭据")
	}
	token, err := s.inferenceToken(ctx)
	if err != nil {
		return "", err
	}
	if strings.TrimSpace(token) == "" {
		return "", apierr.New(apierr.CodeUnauthenticated, "当前没有可用的推理令牌，无法生成网关档")
	}
	return token, nil
}

// gatewayInputFor 装配某工具在某档位卡下的家族输入。
//
// 策略缺失时的行为：`model` 为空会让各家族响亮拒绝（config_invalid），
// 这是刻意的——用猜的模型名写进 live 配置比拒绝更危险。
func (s *Server) gatewayInputFor(ctx context.Context, id family.ID, p *policy.Policy) (family.GatewayInput, error) {
	return s.gatewayInputForMode(ctx, id, p, true)
}

// gatewayInputForNoFetch builds the same family input as gatewayInputFor while
// refusing to fetch or persist a missing inference token. Use it from pure
// previews and draft planning; real apply/test paths may use gatewayInputFor.
func (s *Server) gatewayInputForNoFetch(ctx context.Context, id family.ID, p *policy.Policy) (family.GatewayInput, error) {
	return s.gatewayInputForMode(ctx, id, p, false)
}

func (s *Server) gatewayInputForMode(ctx context.Context, id family.ID, p *policy.Policy, allowTokenFetch bool) (family.GatewayInput, error) {
	return s.gatewayInputForModeWithToken(ctx, id, p, allowTokenFetch, "")
}

// gatewayInputForModeWithToken allows a user card's own resolved
// credential to supply the family token without fetching the account token.
// Claude and Claude Desktop Proxy deliberately never place a real token in
// their generated configuration, so those families still use the family gate.
func (s *Server) gatewayInputForModeWithToken(
	ctx context.Context, id family.ID, p *policy.Policy, allowTokenFetch bool, explicitToken string,
) (family.GatewayInput, error) {
	uid := s.currentUID()
	model, spec, err := modelForProfile(id, p)
	if err != nil {
		return family.GatewayInput{}, err
	}

	desktopMode := string(claudedesktop.ModeProxy)
	extra := map[string]string{
		// providerId 决定 openclaw/opencode/pi 写进 live 配置的托管 provider 键名。
		"providerId": managedProviderPrefix + uid,
	}
	if id == family.ClaudeDesktop {
		extra[claudedesktop.ModeExtraKey] = desktopMode
	}

	token := ""
	switch id {
	case family.Claude, family.ClaudeDesktop:
		// These families intentionally use a local route + placeholder/local key.
	default:
		if explicitToken != "" {
			token = explicitToken
		} else {
			token, err = s.tokenForFamilyMode(ctx, id, desktopMode, allowTokenFetch)
			if err != nil {
				return family.GatewayInput{}, err
			}
		}
	}

	windows := windowSpecFor(id)
	in := family.GatewayInput{
		// Origin 是本网关基址（旧中继网关基址的承接位，design §2.3 gatewayOrigin→删）：
		// 装配面取本地代理基址——受管档的网关形态写「指向本机网关」；自建卡显式端点随后
		// 在 planForUserProfileCard 里覆盖（handlers_tools 以卡上 Endpoint 改写 Origin）。
		Origin:         s.localRouteBase(),
		LocalRoute:     s.localRouteBase(),
		Token:          token,
		TokenRef:       tokenRefFor(uid),
		Model:          model,
		TierModel:      managedTierModel,
		CompactWindow:  windows.CompactWindow,
		ContextWindow:  windows.ContextWindow,
		MaxOutput:      windows.MaxOutput,
		SupportsImages: spec.SupportsImages,
		Extra:          extra,
	}
	// codex 的 `model_catalog_json` 指针指向的文件缺失会让 codex **启动即失败**
	// （真机实测 `Error: No such file or directory`），因此装配层能取到策略目录就取。
	if id == family.Codex {
		if raw, ok := p.Catalog("codex"); ok {
			in.Catalog = raw
		}
	}
	return in, nil
}

// modelForProfile 解析某族的默认上游模型与策略条目。
//
// `models` 集合与 `Spec` 都取自策略，`SupportsImages` 因此是**策略值而不是硬编码**。
func modelForProfile(id family.ID, p *policy.Policy) (string, policy.ModelSpec, error) {
	if p == nil {
		return "", policy.ModelSpec{}, apierr.New(apierr.CodeConfigInvalid,
			"模型策略尚未就绪，无法生成 %s 的网关档（先刷新策略）", id)
	}
	model, ok := p.Default()
	if !ok {
		return "", policy.ModelSpec{}, apierr.New(apierr.CodeConfigInvalid, "模型策略没有默认模型")
	}
	spec, _ := p.Spec(model.ID)
	return model.ID, spec, nil
}

// applySelectedModelCapabilities 把**所选模型**的策略条目接进家族输入（UC-042）。
//
// 为什么需要它：`GatewayInput` 的窗口三件套缺省来自 `windowSpecFor`，而那组常量是
// 「**默认模型**的能力按家族折算」的结果（pi=384000/1M 即 deepseek-flash 的官方值，
// 其余九族 128000/1M 是受管档口径）。自建卡换到别的模型后，族常量不再是该模型的能力
// 描述：pi 的 `maxTokensFor` 还会把未置权威位的值向上夹到 `OfficialMaxOutput`（384000），
// 于是自建卡选 `bd-glm-5.3-flash`（策略条目 maxOutputTokens 128000）在盘上写出 384000，
// 逐请求被上游 `400 max_completion_tokens range is [1, 131072]`（UC-040/UC-042 背景）。
//
// 零回归的边界（不得放宽）：所选模型 == 策略默认模型、或模型无策略条目时，**一个字段都
// 不动**——那时族常量正是该模型的能力折算值，强行覆盖会让九族的 128K 上限被默认模型的
// 384000 顶掉（另一个缺陷，不是本条的目的）。
//
// 优先级：卡声明（`Profile.maxOutputTokens`，调用方随后覆盖）> 所选模型策略条目 > 族常量。
func applySelectedModelCapabilities(in *family.GatewayInput, p *policy.Policy) {
	if in.EndpointIsFullBase {
		// A model slug in the managed policy does not prove that the selected direct
		// provider implements that model's limits or modalities. Full-base writers
		// derive reviewed facts from the exact provider endpoint instead; the caller
		// applies a card's explicit maxOutputTokens after this function.
		in.ContextWindow = 0
		in.CompactWindow = 0
		in.MaxOutput = 0
		in.MaxOutputExplicit = false
		in.SupportsImages = false
		return
	}
	model := strings.TrimSpace(in.Model)
	if model == "" || p == nil {
		return
	}
	if def, ok := p.Default(); ok && def.ID == model {
		return
	}
	spec, ok := p.Spec(model)
	if !ok {
		return
	}
	if spec.MaxInputTokens > 0 {
		in.ContextWindow = spec.MaxInputTokens
	}
	if spec.MaxOutputTokens > 0 {
		in.MaxOutput = spec.MaxOutputTokens
		// 权威位：该值出自所选模型的策略条目（与卡声明同族），家族逐字采用、不再夹取。
		in.MaxOutputExplicit = true
	}
}

// modelForCard 解析某张卡片实际使用的模型 id：自建卡以卡上的 modelId 为准（必须在策略内），
// 其余（受管/官方）恒为策略 default。
func modelForCard(p *policy.Policy, card *userProfileDoc) (string, error) {
	if card != nil && strings.TrimSpace(card.ModelID) != "" {
		if p == nil {
			return "", apierr.New(apierr.CodeConfigInvalid, "模型策略尚未就绪，无法解析档位卡模型")
		}
		if _, ok := p.Spec(card.ModelID); !ok {
			return "", apierr.New(apierr.CodeInvalidRequest,
				"档位卡指定的模型不在策略内: %s", card.ModelID)
		}
		return card.ModelID, nil
	}
	model, _, err := modelForProfile("", p)
	return model, err
}
