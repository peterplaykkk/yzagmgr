// §4.1 工具、§4.2 档位卡、§4.3 预览与应用、§5 备份保留。
//
// 本文件的每个 handler 都只做三件事：解析入参 → 调服务包 → 包信封。
// 档位判定、路径解析、事务语义全在 `internal/family`、`internal/tx`、`internal/backup` 里。
package api

import (
	"context"
	"net/http"
	"net/url"
	"os"
	"runtime"
	"strings"
	"time"

	"github.com/peterplaykkk/yzagmgr/internal/apierr"
	"github.com/peterplaykkk/yzagmgr/internal/backup"
	"github.com/peterplaykkk/yzagmgr/internal/family"
	"github.com/peterplaykkk/yzagmgr/internal/policy"
	"github.com/peterplaykkk/yzagmgr/internal/proxy"
	"github.com/peterplaykkk/yzagmgr/internal/tx"
)

// ── §4.1 工具 ───────────────────────────────────────────────────────────────

// pathInfo 是契约 §4.1 的 PathInfo。
//
// `paths` **只含文件**（UC-003 A10）：目录是 EISDIR 陷阱的来源，路径解析为空时报
// `path_conflict` 而不是给一个空列表。
type pathInfo struct {
	Path        string `json:"path"`
	Exists      bool   `json:"exists"`
	Managed     bool   `json:"managed"`
	CreatedByUs bool   `json:"createdByUs"`
	SizeBytes   int64  `json:"sizeBytes"`
	SHA256      string `json:"sha256"`
}

// toolView 是契约 §4.1 的工具形状。
//
// `externalManaged` / `externalSource` 是 additive 字段（2026-09-24，见 externalmanaged.go）：
// （原 externalManaged 响应位说明已随 externalmanaged 删除——design §2.2；`tier` 枚举与语义不动。）
//
// `installedState` 是 additive 字段（2026-09-27，UC-003 修订「状态拆分」）：安装三态
// 「已安装 / 未安装 / 未知」，与 `tier`（盘上配置形态）**两个维度**，谁都不顶替谁。
// 旧 `installed` 布尔与 `tier` 既有四值保留原语义（零回归）；`tier` 只**增补**
// `unknown`（探测失败/不可判，见 family.TierUnknown）。
type toolView struct {
	ID              string     `json:"id"`
	Name            string     `json:"name"`
	Installed       bool       `json:"installed"`
	InstalledState  string     `json:"installedState"`
	InstallCopies   int        `json:"installCopies"`
	Version         string     `json:"version"`
	Tier            string     `json:"tier"`
	DecideKey       string     `json:"decideKey"`
	Paths           []pathInfo `json:"paths"`
	ActiveProfileID string     `json:"activeProfileId"`
}

// installedState 的封闭取值（契约 §4.1 的 installedState 枚举）。
const (
	installStateInstalled    = "installed"
	installStateNotInstalled = "not_installed"
	installStateUnknown      = "unknown"
)

// toolDetail 是 GET /tools/{id} 的形状（工具形状 + 形状附属键）。
type toolDetail struct {
	toolView
	CompanionKeys []companionKey `json:"companionKeys"`
}

func (s *Server) handleTools(*http.Request) (any, error) {
	ids := s.Families.IDs() // 已注册的家族，顺序即 UI 展示顺序
	views := make([]toolView, 0, len(ids))
	for _, id := range ids {
		view, err := s.toolViewFor(id)
		if err != nil {
			// 单族探测失败**不会**走到这里：探测错误已在 toolState 本体被映射成
			// TierUnknown（UC-003 修订「状态拆分」，2026-09-27），列表因此不需要
			// 「降级视图」兜底。能留下的错误只剩「未注册单元」这类装配级故障——
			// ids 取自注册表本身，出现即意味着装配被绕过；如实拒绝整条请求，
			// 好过用伪造视图把不确定性藏起来（旧兜底分支已按新语义清理）。
			return nil, err
		}
		views = append(views, view)
	}
	return views, nil
}

func (s *Server) handleToolGet(r *http.Request) (any, error) {
	id, err := s.requireFamily(r.PathValue("id"))
	if err != nil {
		return nil, err
	}
	view, err := s.toolViewFor(id)
	if err != nil {
		return nil, err
	}
	return toolDetail{toolView: view, CompanionKeys: s.readCompanions(id)}, nil
}

// toolViewFor 组装单族的工具视图（盘上字节判定，不看内部记账）。
func (s *Server) toolViewFor(id family.ID) (toolView, error) {
	f, ok := s.Families.Lookup(id)
	if !ok {
		return toolView{}, errNotSupported(id)
	}
	tier, active, probe, err := s.toolState(id)
	if err != nil {
		return toolView{}, err
	}

	paths := f.Paths(s.Env)
	infos := make([]pathInfo, 0, len(paths))
	installed := false
	for _, p := range paths {
		info := s.pathInfoFor(id, p)
		if info.Exists {
			installed = true
		}
		infos = append(infos, info)
	}
	installSnapshot, installProbed := s.probe.snapshot(id)
	copies := installSnapshot.copies
	if len(copies) > 0 {
		installed = true
	}
	version := installSnapshot.version
	if version == "" {
		version = s.desktopVersionFor(id)
	}
	// 探测到版本就说明工具确实装着：`installed` 与 `version` 不能互相打脸
	// （两个桌面族没有 CLI 可执行、也未必有受管文件，但应用包在场就是装着）。
	if version != "" {
		installed = true
	}

	return toolView{
		ID:              string(id),
		Name:            f.DisplayName(),
		Installed:       installed,
		InstalledState:  s.installedStateForSnapshot(id, tier, installSnapshot, installProbed),
		InstallCopies:   len(copies),
		Version:         version,
		Tier:            string(tier),
		DecideKey:       probe.DecideKey,
		Paths:           infos,
		ActiveProfileID: active,
	}, nil
}

// installedStateFor 判定安装三态（UC-003 修订「状态拆分」/ UC-091 A3）。
//
// 判据只认**安装证据**，受管配置文件在场**不算**安装证据——「配置形态」与「安装状态」
// 是两个维度，谁都不顶替谁（旧 `installed` 布尔保留原口径以零回归，本字段是修正后的答案）：
//   - installed：可执行副本在场（copies>0）、版本可读、或桌面族应用包在场；
//   - unknown：探测失败（tier==unknown）且没有安装证据——不可判时不冒充「未安装」；
//   - not_installed：既无证据也无探测失败 = 明确缺少可执行工具（扫面覆盖了
//     PATH/常见 bin 目录；两个桌面族以应用包在场为准）。
func (s *Server) installedStateFor(id family.ID, tier family.Tier) string {
	state, probed := s.probe.snapshot(id)
	return s.installedStateForSnapshot(id, tier, state, probed)
}

func (s *Server) installedStateForSnapshot(
	id family.ID, tier family.Tier, state probeState, probed bool,
) string {
	if len(state.copies) > 0 || state.version != "" || s.desktopBundlePresent(id) {
		return installStateInstalled
	}
	if !probed || tier == family.TierUnknown {
		return installStateUnknown
	}
	return installStateNotInstalled
}

// desktopVersionFor 给两个桌面族（没有 CLI 可执行）读应用包版本。
func (s *Server) desktopVersionFor(id family.ID) string {
	appPath, ok := desktopBundlePath(id)
	if !ok {
		return ""
	}
	return desktopBundleVersion(appPath)
}

// desktopBundlePresent 报告桌面族的应用包是否在场（安装证据；版本读不到不影响）。
func (s *Server) desktopBundlePresent(id family.ID) bool {
	appPath, ok := desktopBundlePath(id)
	if !ok || runtime.GOOS != "darwin" {
		return false
	}
	st, err := os.Stat(appPath)
	return err == nil && st.IsDir()
}

// desktopBundlePath 给出桌面族的应用包路径（familyBinary 的桌面版对照）。
func desktopBundlePath(id family.ID) (string, bool) {
	switch id {
	case family.ClaudeDesktop:
		return "/Applications/Claude.app", true
	case family.WorkBuddy:
		return "/Applications/WorkBuddy.app", true
	}
	return "", false
}

// pathInfoFor 读一条受管路径的盘上状态。
func (s *Server) pathInfoFor(id family.ID, path string) pathInfo {
	info := pathInfo{Path: path, Managed: true, CreatedByUs: s.createdByUs(id, path)}
	st, err := statRegular(path)
	if err != nil {
		return info
	}
	info.Exists = true
	info.SizeBytes = st.Size()
	if blob, err := readRegularFile(path); err == nil {
		info.SHA256 = tx.HashBytes(blob)
	}
	return info
}

// createdByUs 报告该路径是否是「本产品接管时创建的」。
//
// 判据来自初始快照（`reason:"initial"`）：它记录的是**接管前**的字节，若那里没有这个
// 路径而现在有，那就是我们建的（恢复时将被删除，§4.3 的 delete 语义）。
func (s *Server) createdByUs(id family.ID, path string) bool {
	initial, err := s.Backups.Initial(string(id))
	if err != nil {
		return false
	}
	for _, file := range initial.Files {
		if file.Path == path {
			return !file.Existed
		}
	}
	return false
}

// toolState 用「记账 × 盘上字节」联合判定档位与生效档位卡。
//
// 规则来自契约 §4.1 的最后一条：`tier` 与 `activeProfileId` **由盘上字节判定**；
// 与内部记账不一致时 `tier:"drift"` 并置 `activeProfileId:""`。
//
// 「不一致」以**有记账**为前提（2026-09-24 细化，契约 §4.1 的 tier 语义行）：
// 空记账不属于「不一致」——按盘上真值报档位。空记账分支三态：
//   - absent → absent（不变）；
//   - official → official（工具原生状态，不是漂移）；
//   - gateway → drift（未知外部写入，fail-safe 不放松——旧 v1 外部托管兼容面
//     已随 externalmanaged 删除，design §1.1/§2.2，不再有「可认领的外部网关形态」）。
//
// 探测本身**报错**时（2026-09-27 修订）：返回 `TierUnknown` 而不是把错误上抛、也不回落
// drift——「探测失败」不是盘上事实，unknown 是盘上判定词汇里为它准备的显式答案
// （family.TierUnknown；旧行为见 TestToolStateErrorPaths 的注释）。未注册单元仍报
// not_supported，那与探测无关。
func (s *Server) toolState(id family.ID) (family.Tier, string, family.ProbeResult, error) {
	f, ok := s.Families.Lookup(id)
	if !ok {
		return "", "", family.ProbeResult{}, errNotSupported(id)
	}
	probe, err := f.Probe(s.Env)
	if err != nil {
		// 探测失败：tier 与安装状态都不可判（unknown）。错误本身记进程日志，
		// 不进响应——响应只能承载契约词汇，不能把内部错误文本当档位事实。
		s.log.Warn("档位探测失败，按 unknown 报告", "tool", string(id), "err", err)
		probe.Notes = append(probe.Notes, "探测失败: "+err.Error())
		return family.TierUnknown, "", probe, nil
	}
	// unknown 是「不可判」，与记账无关：家族将来若显式报出它（例如决定文件在场但
	// 无法解析），按它原样透传，绝不落进下面任何「按盘上真值/漂移」的判定臂。
	if probe.Tier == family.TierUnknown {
		return family.TierUnknown, "", probe, nil
	}
	applied := s.profiles.snapshot(string(id)).Applied
	switch {
	case applied == "":
		switch probe.Tier {
		case family.TierAbsent, family.TierOfficial:
			// 空盘/原生官方形态：按盘上真值报，不是漂移。
			return probe.Tier, "", probe, nil
		case family.TierGateway:
			// 空记账 + 网关形态：未知外部写入，fail-safe 报漂移
			// （externalmanaged 认领面已删，design §2.2）。
			return family.TierDrift, "", probe, nil
		default:
			// probe 显式报出的异常态（如 workbuddy 文件不可解析，probe 报 drift）：
			// fail-safe 报漂移。unknown（不可判）在上面已透传，不会落到这一臂。
			return family.TierDrift, "", probe, nil
		}
	default:
		if wantsTier(f, applied) == probe.Tier {
			return probe.Tier, applied, probe, nil
		}
		// 自建**独立端点**卡（UC-032 独立 provider 形态）：卡上的端点不在 probe
		// 的受管词汇（回环）内，会被归入 official ⇒ wantsTier（gateway）
		// 不符 ⇒ 看起来是漂移；但盘上决定键指向卡上端点时，「生效项 = 该自建卡」就是盘上
		// 事实（字节是本产品经 §4.3 事务写的）。只认自己写的字节：卡被删/换、决定键消失
		// 或指向别处，照常报 drift。
		if s.selfBuiltEndpointAppliedOnDisk(id, applied, probe) {
			return family.TierGateway, applied, probe, nil
		}
		return family.TierDrift, "", probe, nil
	}
}

// selfBuiltEndpointAppliedOnDisk 报告「记账指向本工具的自建卡（独立端点）+ 盘上
// 决定键指向卡上的端点」：受管词汇认不出卡上的独立端点，而决定键指向它恰是
// 「该卡已按事务落盘」的证据；指向别处或卡无端点即 false。
// （原「下发卡同因同形」分支已随登录下发删除——design §2.2。）
func (s *Server) selfBuiltEndpointAppliedOnDisk(id family.ID, applied string, probe family.ProbeResult) bool {
	card, ok := findUserCard(s.profiles.snapshot(string(id)), applied)
	if !ok {
		return false
	}
	if !probe.Present || strings.TrimSpace(probe.DecideValue) == "" {
		return false
	}
	host := deliveryHostOf(card.Endpoint)
	if host == "" {
		return false
	}
	return strings.Contains(strings.ToLower(probe.DecideValue), host)
}

// deliveryHostOf 返回端点的主机名（小写）；不是合法 URL 时返回空串。
// （自 delivery.go 迁出：下发面整删后它是自建独立端点判定的唯一消费点。）
func deliveryHostOf(endpoint string) string {
	u, err := url.Parse(strings.TrimSpace(endpoint))
	if err != nil || u.Host == "" {
		return ""
	}
	return strings.ToLower(u.Hostname())
}

// wantsTier 报告某张档位卡「期望」的盘上档位。
func wantsTier(f family.Family, profileID string) family.Tier {
	if profileID == OfficialProfileID {
		if f.OfficialSeeded() {
			return family.TierOfficial
		}
		// additive 型家族没有官方种子：应用官方档的语义是「移除托管条目」，
		// 因此盘上回落成 official（用户自己的条目还在）或 absent（文件整体消失）都算成功。
		return family.TierAbsent
	}
	return family.TierGateway
}

// requireFamily 取家族实现；未注册（如 hermes 尚未实现）→ not_supported。
func (s *Server) requireFamily(raw string) (family.ID, error) {
	id := family.ID(strings.TrimSpace(raw))
	if id == "" {
		return "", apierr.New(apierr.CodeInvalidRequest, "工具 id 必填")
	}
	if _, ok := s.Families.Lookup(id); !ok {
		return "", errNotSupported(id)
	}
	return id, nil
}

// errNotSupported 统一「该工具/平台不支持此操作」的措辞（契约 §1.3 的 not_supported）。
func errNotSupported(id family.ID) error {
	return apierr.New(apierr.CodeNotSupported,
		"该受管单元尚未实现或不受支持: %s", id).WithDetails(map[string]any{"toolId": string(id)})
}

// proxyOnlyFamily 报告该族是否必须走独立入口（WorkBuddy：独立写链，不走 §4.3 通道）。
func proxyOnlyFamily(id family.ID) bool { return id == family.WorkBuddy }

// familyInstalled 报告该工具在盘上是否已安装（受管文件在场或有可执行副本）。
func (s *Server) familyInstalled(id family.ID, probe family.ProbeResult) bool {
	if probe.Present {
		return true
	}
	return len(s.probe.copies(id)) > 0
}

// handleInstallCopies 列出该工具的正式副本（UC-010 A11 的越界检测面）。
func (s *Server) handleInstallCopies(r *http.Request) (any, error) {
	id, err := s.requireFamily(r.PathValue("id"))
	if err != nil {
		return nil, err
	}
	copies := s.probe.copies(id)
	if copies == nil {
		copies = []installCopy{}
	}
	return copies, nil
}

// ── §4.2 档位卡 ─────────────────────────────────────────────────────────────

func (s *Server) handleProfilesList(r *http.Request) (any, error) {
	id, err := s.requireFamily(r.PathValue("id"))
	if err != nil {
		return nil, err
	}
	return s.profileList(id)
}

// profileList 组装卡片列表：受管卡恒首位、官方卡（有种子才有）次位、自建卡按记账顺序。
func (s *Server) profileList(id family.ID) (profileListPayload, error) {
	f, ok := s.Families.Lookup(id)
	if !ok {
		return profileListPayload{}, errNotSupported(id)
	}
	p, err := s.Policy.Load()
	if err != nil {
		return profileListPayload{}, err
	}
	model, ok := p.Default()
	if !ok {
		return profileListPayload{}, apierr.New(apierr.CodeConfigInvalid, "模型策略没有默认模型")
	}
	tier, active, _, err := s.toolState(id)
	if err != nil {
		return profileListPayload{}, err
	}
	_ = tier // 工具当前档位不在卡上回显（D1：卡的 tier 是卡自身档位形态）
	uid := s.currentUID()
	rec := s.profiles.snapshot(string(id))

	// card 组装派生卡与自建卡的公共形状。`tier` 是**卡自身**的档位形态（D1：
	// 官方卡=official、其余=gateway），工具的当前档位另走 toolState——web 按卡的
	// tier 决定「切官方/切受管」语义（ToolDetailPage 的 target.tier === 'official'），
	// 两处混用一个字段会把启用语义整个对调。
	//
	// UC-094 A6：不可编辑的卡带**机器可读的只读原因**（readOnlyReason）——文案与
	// 编辑/删除入口的拒绝措辞同源（同一个常量），界面据此明示「为什么不能改」，
	// 不再各写一句。
	card := func(pid, name, kind string, editable, deletable bool, modelID string) (Profile, error) {
		hasScript, scriptErr := s.hasUsageScript(string(id), pid)
		if scriptErr != nil {
			return Profile{}, scriptErr
		}
		settings := map[string]any{"modelId": modelID}
		editableKeys := []string{}
		if editable {
			editableKeys = append(editableKeys, editableSettingsKeys...)
		}
		cardTier := string(family.TierGateway)
		if kind == ProfileKindOfficial {
			cardTier = string(family.TierOfficial)
		}
		return Profile{
			ID:               pid,
			Name:             name,
			Kind:             kind,
			Active:           pid == active,
			Tier:             cardTier,
			ModelID:          modelID,
			Models:           []string{model.ID},
			Editable:         editable,
			Deletable:        deletable,
			HasUsageScript:   hasScript,
			Settings:         settings,
			EditableSettings: editableKeys,
			ReadOnlyReason:   readOnlyReasonFor(kind),
		}, nil
	}

	items := make([]Profile, 0, len(rec.User)+3)
	managedID := managedProfileIDFor(uid)
	managed, err := card(managedID, managedCardName, ProfileKindManaged, false, false, model.ID)
	if err != nil {
		return profileListPayload{}, err
	}
	items = append(items, managed)
	if f.OfficialSeeded() {
		official, err := card(OfficialProfileID, officialCardName, ProfileKindOfficial, false, false, model.ID)
		if err != nil {
			return profileListPayload{}, err
		}
		items = append(items, official)
	}
	for _, userCard := range orderedUsers(rec) {
		modelID := userCard.ModelID
		if strings.TrimSpace(modelID) == "" {
			if userCard.Models != nil {
				if len(userCard.Models) > 0 {
					// A card with an explicit model inventory applies its first item when
					// no modelId is stored. Read that effective choice back instead of
					// substituting the unrelated policy default.
					modelID = userCard.Models[0]
				}
			} else {
				modelID = model.ID
			}
		}
		view, err := card(userCard.ID, userCard.Name, ProfileKindUser, true, true, modelID)
		if err != nil {
			return profileListPayload{}, err
		}
		if len(userCard.Settings) > 0 {
			if v, ok := userCard.Settings["modelId"]; ok {
				view.Settings["modelId"] = v
			}
		}
		// UC-032 三要素逐字段回读（凭据只回引用不回明文，UC-018 A12）。
		view.Endpoint = userCard.Endpoint
		view.EndpointIsFullBase = userCard.EndpointIsFullBase
		view.CredRef = userCard.CredRef
		// UC-040：能力声明逐字段回读（0 = 未声明 ⇒ 键缺席，不伪造 0）。
		view.MaxOutputTokens = userCard.MaxOutputTokens
		if userCard.Models != nil {
			view.Models = append([]string(nil), userCard.Models...)
		}
		// UC-094 A5/A6/A7 additive 回读：修订指纹（基于版本保存的输入）、能力声明与
		// 确认记录（缺席 = 未声明/无确认）、复制来源提示（缺席 = 非复制来源）。
		view.Rev = cardRevOf(userCard)
		view.Capabilities = userCard.Capabilities
		view.CapabilityAcks = userCard.CapabilityAcks
		view.CopiedFrom = userCard.CopiedFrom
		items = append(items, view)
	}
	if _, ok := f.(family.RouterForm); ok {
		// router 形态卡（UC-024）：一族恰一张，只给实现 RouterForm 的家族出；
		// 端点读回 = 回环基址 + 对应前缀（cli-rewrite-router.sh expected_base 同式）。
		// 刻意排在自建卡**之后**：受管恒首位、官方次位、自建按记账顺序是既有契约
		// （TestProfileCardLifecycle 钉 items[2..]），router 卡作为形态入口殿后。
		routerCard, err := card(RouterProfileID, routerCardName, ProfileKindRouter, false, false, model.ID)
		if err != nil {
			return profileListPayload{}, err
		}
		routerCard.Endpoint = s.routerFormEndpointFor(id)
		items = append(items, routerCard)
	}
	return profileListPayload{Items: items, Total: len(items), ActiveProfileID: active}, nil
}

// routerFormEndpointFor 给出该族 router 形态决定键的期望值（端点读回，UC-024）：
// claude 的请求路径 = {base}/v1/messages ⇒ base=…/router；OpenAI 兼容三家 ⇒ …/router/v1；
// claude-desktop（UC-046）同样是 anthropic wire（`{base}/v1/messages`）⇒ 到 /router 为止。
func (s *Server) routerFormEndpointFor(id family.ID) string {
	base := s.localRouteBase() + "/router"
	switch id {
	case family.Claude, family.ClaudeDesktop:
	default:
		base += "/v1"
	}
	return base
}

// 三张派生卡的展示名（受管卡用托管 provider 的显示名，官方卡直白写「官方」，
// router 卡写「本地 router」）。
const (
	managedCardName  = "yzagmgr"
	officialCardName = "官方"
	routerCardName   = "请求映射"
)

func (s *Server) hasUsageScript(toolID, profileID string) (bool, error) {
	if s.Scripts == nil {
		return false, nil
	}
	_, ok, err := s.Scripts.Get(toolID, profileID)
	if err != nil {
		return false, apierr.Wrap(apierr.CodeStorage, err, "读取用量脚本失败")
	}
	return ok, nil
}

// profileCreateBody 是 §4.2 的建卡请求（UC-032 additive：endpoint/cred/credRef/models）。
//
// `cred` 是**一次性明文**（入托管凭据库，卡只落引用）；`credRef` 引用既有托管凭据。
// 两者都出现是 `invalid_request`（互斥）。指针用于分辨「没给」与「给了空串」：
// 空串一律是交付违约（routes set 同口径：cred 不得以空串交付）。
type profileCreateBody struct {
	Name               string   `json:"name"`
	From               string   `json:"from"`
	ModelID            *string  `json:"modelId"`
	Endpoint           string   `json:"endpoint"`
	EndpointIsFullBase *bool    `json:"endpointIsFullBase"`
	Cred               *string  `json:"cred"`
	CredRef            *string  `json:"credRef"`
	Models             []string `json:"models"`
	// MaxOutputTokens 是 UC-040 的能力声明。**缺席 = 不声明**；给了就必须落在
	// 1..1_000_000（0/负值/超上限一律 invalid_request：显式交付一个非法能力位是缺陷，
	// 不该被静默当成「没声明」）。
	MaxOutputTokens *int `json:"maxOutputTokens"`
	// Capabilities 是 UC-094 A7 的能力声明（可选；缺席 = 不声明）。
	Capabilities *userCardCapabilities `json:"capabilities"`
	// ConfirmCapabilities 是 UC-094 A7 的显式确认：对本次校验报告里 severity=confirm
	// 的检查码逐条确认（确认会落卡）。码不在当前阻塞集内 ⇒ invalid_request（防基于
	// 过期报告的假确认）。
	ConfirmCapabilities []string `json:"confirmCapabilities"`
}

// maxOutputTokensCeiling 是自建卡能力声明的上界（UC-040 冻结契约）。
//
// 它**不做**与上游的联网校验（本地无法可靠探测某端点接受什么）：越界只在本地挡，
// 真实可接受度由 §4.6 的测试端点如实暴露。
const maxOutputTokensCeiling = 1_000_000

// validateMaxOutputTokens 校验能力声明落在合法区间，错误文案**必须**指出范围
// （UC-040 A1：越界被拒且文案含范围）。
func validateMaxOutputTokens(v int) error {
	if v < 1 || v > maxOutputTokensCeiling {
		return apierr.New(apierr.CodeInvalidRequest,
			"maxOutputTokens 必须在 1..%d 之间（留空 = 不声明）: %d", maxOutputTokensCeiling, v)
	}
	return nil
}

// validateMaxOutputTokensPatch 是**改卡**口径：0 = 清除声明（否则用户建卡时声明过、
// 之后想回到「按所选模型策略条目解析」就没有出路）；1..ceiling = 覆盖；负值/越界拒。
func validateMaxOutputTokensPatch(v int) error {
	if v < 0 || v > maxOutputTokensCeiling {
		return apierr.New(apierr.CodeInvalidRequest,
			"maxOutputTokens 必须在 0..%d 之间（0 = 清除声明，留空 = 不改）: %d", maxOutputTokensCeiling, v)
	}
	return nil
}

// validateProfileEndpoint 校验自建卡端点形态（绝对 http(s) URL，不内嵌凭据）。
func validateProfileEndpoint(raw string) error {
	s := strings.TrimSpace(raw)
	u, err := url.Parse(s)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Hostname() == "" {
		return apierr.New(apierr.CodeInvalidRequest, "端点不是合法的 http(s) URL: %q", raw)
	}
	if u.User != nil {
		return apierr.New(apierr.CodeInvalidRequest, "端点不得内嵌凭据（凭据只以引用登记）")
	}
	return nil
}

// validateProfileFullBaseEndpoint narrows exact provider bases: query strings
// and fragments can carry credentials that would otherwise be returned in the
// editable Profile DTO. Legacy endpoint cards keep validateProfileEndpoint's
// historical acceptance rules when endpointIsFullBase is false.
func validateProfileFullBaseEndpoint(raw string) error {
	if err := validateProfileEndpoint(raw); err != nil {
		return err
	}
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u.Scheme != "https" || u.Hostname() == "" ||
		u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || u.RawFragment != "" ||
		strings.Contains(raw, "#") {
		return apierr.New(apierr.CodeInvalidRequest,
			"endpointIsFullBase 的 endpoint 必须是无凭据、query、fragment 的 HTTPS 基址")
	}
	return nil
}

func validateProfileFullBaseFamily(id family.ID) error {
	switch id {
	case family.Claude, family.ClaudeDesktop, family.OpenCode, family.OpenClaw,
		family.Hermes, family.Pi, family.Codex, family.Grokbuild:
		return nil
	default:
		return apierr.New(apierr.CodeInvalidRequest,
			"该工具不支持完整 provider API 基址").WithDetails(map[string]any{
			"field":  "endpointIsFullBase",
			"reason": "full_base_unsupported",
		})
	}
}

func validateProfileFullBaseSelection(id family.ID, endpoint string) error {
	if err := validateProfileFullBaseFamily(id); err != nil {
		return err
	}
	if strings.TrimSpace(endpoint) == "" {
		return apierr.New(apierr.CodeInvalidRequest,
			"endpointIsFullBase=true 时必须提供 endpoint").WithDetails(map[string]any{
			"field":  "endpointIsFullBase",
			"reason": "full_base_endpoint_missing",
		})
	}
	return validateProfileFullBaseEndpoint(endpoint)
}

// validateCredRefForm 校验托管凭据引用形态（yzagmgr-managed-token://<uid>，UC-018 A12）。
// **只验形态不验在册**：引用能否解析在启用链的凭据步判（UC-032 边界：保存与启用分离）。
func validateCredRefForm(ref string) error {
	uid, ok := strings.CutPrefix(ref, proxy.ArmTokenScheme)
	if !ok || uid == "" || strings.ContainsAny(uid, "/?# \t\r\n") {
		return apierr.New(apierr.CodeInvalidRequest,
			"凭据引用形态非法（应为 %s<uid>）", proxy.ArmTokenScheme)
	}
	return nil
}

// cardCredential 处理凭据交付（UC-032 / UC-023 同口径）：
//   - cred 与 credRef 只能给一个（给空串 = 交付违约）；
//   - cred 明文入托管凭据库 ⇒ 新引用（每次提交独立 uid，引用互异）；
//   - credRef 只登记引用形态，不解析（启用时才解析，坏引用可保存）。
//
// 返回落卡的引用（"" = 本次不提供/不改）。
func (s *Server) cardCredential(cred, credRef *string) (string, error) {
	if cred != nil && credRef != nil {
		return "", apierr.New(apierr.CodeInvalidRequest,
			"cred 与 credRef 只能给一个（明文入托管库，或引用既有托管凭据）")
	}
	if cred != nil {
		if *cred == "" {
			return "", apierr.New(apierr.CodeInvalidRequest, "cred 不得以空串交付（缺省 = 不提供）")
		}
		return s.routerTokens.Put(*cred)
	}
	if credRef != nil {
		if *credRef == "" {
			return "", apierr.New(apierr.CodeInvalidRequest, "credRef 不得以空串交付（缺省 = 不改）")
		}
		if err := validateCredRefForm(*credRef); err != nil {
			return "", err
		}
		return *credRef, nil
	}
	return "", nil
}

// normalizeModelInventory 规整模型清单（去空白项，保持给出顺序；全空 ⇒ 显式空清单）。
func normalizeModelInventory(models []string) []string {
	out := make([]string, 0, len(models))
	for _, m := range models {
		m = strings.TrimSpace(m)
		if m != "" {
			out = append(out, m)
		}
	}
	return out
}

func (s *Server) handleProfileCreate(r *http.Request) (any, string, error) {
	id, err := s.requireFamily(r.PathValue("id"))
	if err != nil {
		return nil, "", err
	}
	var body profileCreateBody
	if err := decode(r, &body); err != nil {
		return nil, "", err
	}
	normalized, err := s.normalizeProfileCreateDraft(id, body)
	if err != nil {
		return nil, "", err
	}
	spec := normalized.Spec
	// The shared normalizer is pure. Only this existing CRUD handler persists a
	// submitted raw credential; draft preview/test keep it transient.
	ref, err := s.cardCredential(body.Cred, body.CredRef)
	if err != nil {
		return nil, "", err
	}
	if ref != "" {
		spec.CredRef = ref
	}
	created, err := s.profiles.create(string(id), spec)
	if err != nil {
		return nil, "", err
	}
	view, err := s.profileViewFor(id, created.ID)
	if err != nil {
		return nil, "", err
	}
	view.CapabilityCheck = &normalized.CapabilityCheck
	return view, "", nil
}

// normalizeProfileCreateDraft converts the existing profile-create body to a
// pure candidate. It performs the same local validation/defaulting as CRUD but
// never stores credentials or profile rows.
func (s *Server) normalizeProfileCreateDraft(id family.ID, body profileCreateBody) (normalizedProfileDraft, error) {
	name := strings.TrimSpace(body.Name)
	if name == "" {
		return normalizedProfileDraft{}, apierr.New(apierr.CodeInvalidRequest, "档位卡名称必填").
			WithDetails(map[string]any{"field": "name"})
	}
	if err := validateCardCapabilities(body.Capabilities); err != nil {
		return normalizedProfileDraft{}, err
	}
	spec := userCardSpec{Name: name, Settings: map[string]any{}}
	var sourced copySource
	var err error
	if strings.TrimSpace(body.From) != "" {
		sourced, err = s.copySourceSpec(id, strings.TrimSpace(body.From))
		if err != nil {
			return normalizedProfileDraft{}, err
		}
		spec.ModelID = sourced.spec.ModelID
		for k, v := range sourced.spec.Settings {
			spec.Settings[k] = v
		}
		spec.Endpoint = sourced.spec.Endpoint
		spec.EndpointIsFullBase = sourced.spec.EndpointIsFullBase
		spec.CredRef = sourced.spec.CredRef
		spec.Models = appendStringSlice(sourced.spec.Models)
		spec.MaxOutputTokens = sourced.spec.MaxOutputTokens
		spec.Capabilities = sourced.spec.Capabilities
		spec.CopiedFrom = sourced.copiedFrom
	}
	if body.Endpoint != "" {
		if err := validateProfileEndpoint(body.Endpoint); err != nil {
			return normalizedProfileDraft{}, err
		}
		spec.Endpoint = strings.TrimSpace(body.Endpoint)
	}
	if body.EndpointIsFullBase != nil {
		spec.EndpointIsFullBase = *body.EndpointIsFullBase
	}
	if spec.EndpointIsFullBase {
		if err := validateProfileFullBaseSelection(id, spec.Endpoint); err != nil {
			return normalizedProfileDraft{}, err
		}
	}
	if err := validateDraftCredential(body.Cred, body.CredRef); err != nil {
		return normalizedProfileDraft{}, err
	}
	transientCredential := ""
	if body.Cred != nil {
		transientCredential = *body.Cred
		spec.CredRef = ""
	} else if body.CredRef != nil {
		spec.CredRef = *body.CredRef
	}
	if body.Models != nil {
		spec.Models = normalizeModelInventory(body.Models)
	}
	if body.ModelID != nil {
		modelID := strings.TrimSpace(*body.ModelID)
		if modelID == "" {
			return normalizedProfileDraft{}, apierr.New(apierr.CodeInvalidRequest,
				"modelId 不得为空；省略字段表示使用现有默认值")
		}
		if err := s.validateCardModel(modelID, spec.Models); err != nil {
			return normalizedProfileDraft{}, err
		}
		spec.ModelID = modelID
	}
	if body.MaxOutputTokens != nil {
		if err := validateMaxOutputTokens(*body.MaxOutputTokens); err != nil {
			return normalizedProfileDraft{}, err
		}
		spec.MaxOutputTokens = *body.MaxOutputTokens
	}
	if body.Capabilities != nil {
		spec.Capabilities = normalizeCardCapabilities(body.Capabilities)
	}
	report, acks, err := s.cardCapabilityPlanned(
		spec.Capabilities, effectiveCardModelID(spec.ModelID, spec.Models),
		body.ConfirmCapabilities, sourced.acks, time.Now())
	if err != nil {
		return normalizedProfileDraft{}, err
	}
	if err := checkCardNameUnique(s.profiles.snapshot(string(id)), spec.Name, ""); err != nil {
		return normalizedProfileDraft{}, err
	}
	spec.CapabilityAcks = acks
	card := userProfileDoc{
		Name: spec.Name, ModelID: spec.ModelID, Settings: copySettings(spec.Settings),
		Endpoint: spec.Endpoint, EndpointIsFullBase: spec.EndpointIsFullBase,
		CredRef: spec.CredRef, Models: appendStringSlice(spec.Models),
		MaxOutputTokens: spec.MaxOutputTokens, Capabilities: spec.Capabilities,
		CapabilityAcks: append([]capabilityAck(nil), spec.CapabilityAcks...), CopiedFrom: spec.CopiedFrom,
	}
	card.Rev = cardRevOf(card)
	return normalizedProfileDraft{Spec: spec, Card: card, TransientCredential: transientCredential, CapabilityCheck: report}, nil
}

// copySource 是复制通道解析出的来源内容（UC-094 A6）。
type copySource struct {
	spec userCardSpec
	// acks 是来源卡的既有能力确认（随复制带回；由闸门按 (code,model,capsHash) 复核，
	// 声明或模型被本次复制改写即失效——「确认与声明同生命周期」在复制面同样成立）。
	acks       []capabilityAck
	copiedFrom *copiedFromHint
}

// copySourceSpec 解析 `from` 指向的复制来源（UC-094 A6）。
//
// 允许的源：自建卡（派生新个人配置，不带来源提示——同 kind 无来源歧义）。
// 下发卡复制通道已随登录下发删除（design §2.2）；受管/官方/router 卡的内容由策略
// 或形态派生，复制它们没有意义（与既有口径一致，拒）。
func (s *Server) copySourceSpec(id family.ID, from string) (copySource, error) {
	rec := s.profiles.snapshot(string(id))
	if source, ok := findUserCard(rec, from); ok {
		spec := userCardSpec{
			ModelID: source.ModelID,
			// Settings 也在跟随面内（B-5 修复 F2）：**新建 map 拷出**——
			// 若直接共享来源 map，副本侧的后续修改会隔着别名反写来源卡；
			// nil 保持 nil（不伪造空对象）。
			Settings:           copySettings(source.Settings),
			Endpoint:           source.Endpoint,
			EndpointIsFullBase: source.EndpointIsFullBase,
			CredRef:            source.CredRef,
			Models:             source.Models,
			MaxOutputTokens:    source.MaxOutputTokens,
			Capabilities:       source.Capabilities,
		}
		return copySource{spec: spec, acks: append([]capabilityAck(nil), source.CapabilityAcks...)}, nil
	}
	return copySource{}, apierr.New(apierr.CodeInvalidRequest,
		"只能从个人配置复制新配置: %s", from)
}

func findUserCard(rec toolProfileDoc, pid string) (userProfileDoc, bool) {
	for _, c := range rec.User {
		if c.ID == pid {
			return c, true
		}
	}
	return userProfileDoc{}, false
}

// profileUpdateBody 是 §4.2 的改卡请求（UC-032 additive：endpoint/cred/credRef/models）。
// 指针 nil = 不改；endpoint/cred/credRef 的空串是交付违约；models 给 `[]` = 清空清单。
type profileUpdateBody struct {
	Name               *string        `json:"name"`
	ModelID            *string        `json:"modelId"`
	Endpoint           *string        `json:"endpoint"`
	EndpointIsFullBase *bool          `json:"endpointIsFullBase"`
	Cred               *string        `json:"cred"`
	CredRef            *string        `json:"credRef"`
	Models             *[]string      `json:"models"`
	Settings           map[string]any `json:"settings"`
	// MaxOutputTokens 同建卡口径（UC-040）：缺席 = 不改；给了就必须落在 1..1_000_000。
	MaxOutputTokens *int `json:"maxOutputTokens"`
	// BaseRev 是 UC-094 A5 的「保存所基于的版本」：缺席/空串 = 不带闸（legacy）；
	// 非空且与当前卡内容指纹不符 ⇒ version_conflict（不静默最后覆盖）。
	BaseRev *string `json:"baseRev"`
	// Capabilities 是 UC-094 A7 的能力声明：缺席 = 不改；`{}`（空对象）= 清除声明。
	Capabilities *userCardCapabilities `json:"capabilities"`
	// ConfirmCapabilities 同建卡口径（UC-094 A7）。
	ConfirmCapabilities []string `json:"confirmCapabilities"`
}

func (s *Server) handleProfileUpdate(r *http.Request) (any, string, error) {
	id, err := s.requireFamily(r.PathValue("id"))
	if err != nil {
		return nil, "", err
	}
	pid := r.PathValue("pid")
	// 保留既有错误优先级：档位卡不存在时，即使请求体不合法也先返回 not_found。
	if err := s.requireUserCard(id, pid); err != nil {
		return nil, "", err
	}
	var body profileUpdateBody
	if err := decode(r, &body); err != nil {
		return nil, "", err
	}
	normalized, err := s.normalizeProfileUpdateDraft(id, pid, body, false)
	if err != nil {
		return nil, "", err
	}
	patch := normalized.Patch
	// Existing CRUD is the only path that persists a submitted raw credential.
	ref, err := s.cardCredential(body.Cred, body.CredRef)
	if err != nil {
		return nil, "", err
	}
	if ref != "" {
		patch.CredRef = &ref
	}
	updated, err := s.profiles.update(string(id), pid, patch)
	if err != nil {
		return nil, "", err
	}
	view, err := s.profileViewFor(id, updated.ID)
	if err != nil {
		return nil, "", err
	}
	view.CapabilityCheck = &normalized.CapabilityCheck
	return view, "", nil
}

// normalizeProfileUpdateDraft applies an existing PUT payload without writing
// storage. Draft preview/test uses requireBaseRev=true; the CRUD handler passes
// false to preserve its legacy optional-baseRev behavior, then commits the
// returned patch through profileStore.update's atomic revision check.
func (s *Server) normalizeProfileUpdateDraft(
	id family.ID, pid string, body profileUpdateBody, requireBaseRev bool,
) (normalizedProfileDraft, error) {
	if err := s.requireUserCard(id, pid); err != nil {
		return normalizedProfileDraft{}, err
	}
	if s.profileDraftAfterRequireUserCard != nil {
		s.profileDraftAfterRequireUserCard()
	}
	for key := range body.Settings {
		if !isEditableSetting(key) {
			return normalizedProfileDraft{}, apierr.New(apierr.CodeInvalidRequest,
				"settings 不接受键 %s（可编辑键：%s）", key,
				strings.Join(editableSettingsKeys, ","))
		}
	}
	if body.Name != nil && strings.TrimSpace(*body.Name) == "" {
		return normalizedProfileDraft{}, apierr.New(apierr.CodeInvalidRequest, "档位卡名称不得为空").
			WithDetails(map[string]any{"field": "name"})
	}
	if body.Endpoint != nil && strings.TrimSpace(*body.Endpoint) != "" {
		if err := validateProfileEndpoint(*body.Endpoint); err != nil {
			return normalizedProfileDraft{}, err
		}
	}
	if body.MaxOutputTokens != nil {
		if err := validateMaxOutputTokensPatch(*body.MaxOutputTokens); err != nil {
			return normalizedProfileDraft{}, err
		}
	}
	if err := validateCardCapabilities(body.Capabilities); err != nil {
		return normalizedProfileDraft{}, err
	}
	if err := validateDraftCredential(body.Cred, body.CredRef); err != nil {
		return normalizedProfileDraft{}, err
	}
	current, ok := s.profiles.find(string(id), pid)
	if !ok {
		return normalizedProfileDraft{}, apierr.New(apierr.CodeNotFound, "档位卡不存在: %s", pid)
	}
	currentRev := cardRevOf(current)
	if requireBaseRev && (body.BaseRev == nil || strings.TrimSpace(*body.BaseRev) == "") {
		return normalizedProfileDraft{}, apierr.New(apierr.CodeInvalidRequest, "草稿修改必须带 baseRev")
	}
	if body.BaseRev != nil && *body.BaseRev != "" && *body.BaseRev != currentRev {
		return normalizedProfileDraft{}, versionConflictError(current, currentRev, *body.BaseRev)
	}
	patch := userCardPatch{
		Name: body.Name, ModelID: body.ModelID, Endpoint: body.Endpoint,
		EndpointIsFullBase: body.EndpointIsFullBase,
		Models:             body.Models, Settings: body.Settings,
		MaxOutputTokens: body.MaxOutputTokens, BaseRev: body.BaseRev,
	}
	transientCredential := ""
	if body.Cred != nil {
		transientCredential = *body.Cred
	} else if body.CredRef != nil {
		ref := *body.CredRef
		patch.CredRef = &ref
	}
	if body.ModelID != nil {
		inventory := current.Models
		if body.Models != nil {
			inventory = *body.Models
		}
		if err := s.validateCardModel(*body.ModelID, inventory); err != nil {
			return normalizedProfileDraft{}, err
		}
	}
	postCaps := current.Capabilities
	if body.Capabilities != nil {
		postCaps = normalizeCardCapabilities(body.Capabilities)
		patch.Capabilities = &postCaps
	}
	postModel := current.ModelID
	if body.ModelID != nil {
		postModel = *body.ModelID
	}
	postModels := current.Models
	if body.Models != nil {
		postModels = *body.Models
	}
	report, acks, err := s.cardCapabilityPlanned(
		postCaps, effectiveCardModelID(postModel, postModels),
		body.ConfirmCapabilities, current.CapabilityAcks, time.Now())
	if err != nil {
		return normalizedProfileDraft{}, err
	}
	patch.CapabilityAcks = &acks
	candidate := applyProfileDraftPatch(current, patch)
	if candidate.EndpointIsFullBase {
		if err := validateProfileFullBaseSelection(id, candidate.Endpoint); err != nil {
			return normalizedProfileDraft{}, err
		}
	}
	if transientCredential != "" {
		// Raw credentials are assessment-only. The real Web save path supplies a
		// previously created credRef and the CRUD endpoint owns vault persistence.
		candidate.CredRef = ""
	}
	if err := checkCardNameUnique(s.profiles.snapshot(string(id)), candidate.Name, pid); err != nil {
		return normalizedProfileDraft{}, err
	}
	candidate.Rev = cardRevOf(candidate)
	return normalizedProfileDraft{
		ProfileID: pid, BaseRev: currentRev, Card: candidate,
		TransientCredential: transientCredential, Patch: patch, CapabilityCheck: report,
	}, nil
}

// applyProfileDraftPatch mirrors profileStore.update's pure field-merge rules.
// The store remains the sole persistence owner; this copy is only an ephemeral
// candidate for preview/test and never mutates the supplied card.
func applyProfileDraftPatch(old userProfileDoc, patch userCardPatch) userProfileDoc {
	next := old
	if patch.Name != nil {
		next.Name = *patch.Name
	}
	if patch.ModelID != nil {
		next.ModelID = *patch.ModelID
	}
	if patch.Endpoint != nil && *patch.Endpoint != "" {
		next.Endpoint = *patch.Endpoint
	}
	if patch.EndpointIsFullBase != nil {
		next.EndpointIsFullBase = *patch.EndpointIsFullBase
	}
	if patch.CredRef != nil && *patch.CredRef != "" {
		next.CredRef = *patch.CredRef
	}
	if patch.Models != nil {
		next.Models = appendStringSlice(*patch.Models)
	}
	if patch.MaxOutputTokens != nil {
		next.MaxOutputTokens = *patch.MaxOutputTokens
	}
	if patch.Capabilities != nil {
		next.Capabilities = *patch.Capabilities
	}
	if patch.CapabilityAcks != nil {
		next.CapabilityAcks = append([]capabilityAck(nil), (*patch.CapabilityAcks)...)
	}
	if patch.Settings != nil {
		merged := copySettings(old.Settings)
		if merged == nil {
			merged = map[string]any{}
		}
		for key, value := range patch.Settings {
			merged[key] = value
		}
		next.Settings = merged
	}
	return next
}

// validateCardModel 校验卡上的选中模型：有独立清单 ⇒ 必须在清单内（UC-032），
// 无清单 ⇒ 沿用策略内校验（既有语义不动）。
func (s *Server) validateCardModel(modelID string, inventory []string) error {
	if inventory != nil {
		for _, m := range inventory {
			if m == modelID {
				return nil
			}
		}
		return apierr.New(apierr.CodeInvalidRequest, "模型不在档位卡清单内: %s", modelID)
	}
	p, err := s.Policy.Load()
	if err != nil {
		return err
	}
	if _, ok := p.Spec(modelID); !ok {
		return apierr.New(apierr.CodeInvalidRequest, "模型不在策略内: %s", modelID)
	}
	return nil
}

func (s *Server) handleProfileDelete(r *http.Request) (any, string, error) {
	id, err := s.requireFamily(r.PathValue("id"))
	if err != nil {
		return nil, "", err
	}
	pid := r.PathValue("pid")
	if err := s.requireUserCard(id, pid); err != nil {
		return nil, "", err
	}
	// 删除当前生效的自建卡必须先切到别的卡（UC-018 A11）：直接删会留下
	// 「盘上是网关档、卡片列表里没有生效项」的悬空状态。
	if s.profiles.snapshot(string(id)).Applied == pid {
		return nil, "", apierr.New(apierr.CodeInvalidRequest,
			"该卡当前生效，先切到别的卡再删除").WithDetails(map[string]any{"profileId": pid})
	}
	if err := s.profiles.remove(string(id), pid); err != nil {
		return nil, "", err
	}
	if s.Scripts != nil {
		if _, err := s.Scripts.Delete(string(id), pid); err != nil {
			return nil, "", apierr.Wrap(apierr.CodeStorage, err, "删除档位卡的用量脚本失败")
		}
	}
	return struct{}{}, "", nil
}

func (s *Server) handleProfileDuplicate(r *http.Request) (any, string, error) {
	id, err := s.requireFamily(r.PathValue("id"))
	if err != nil {
		return nil, "", err
	}
	pid := r.PathValue("pid")
	// UC-094 A6：复制通道的源既可以是自建卡、也可以是**下发卡**（「复制成个人」）。
	// 官方/受管/router 仍拒（内容由策略或形态派生，复制没有意义）。
	if err := s.requireCopySource(id, pid); err != nil {
		return nil, "", err
	}
	rec := s.profiles.snapshot(string(id))
	source, isUser := findUserCard(rec, pid)
	sourced, err := s.copySourceSpec(id, pid)
	if err != nil {
		return nil, "", err
	}
	settings := map[string]any{}
	if isUser {
		for k, v := range source.Settings {
			settings[k] = v
		}
	}
	// 复制 = 全量跟随（UC-018 A9 / UC-032 A2）：名称加后缀、三要素跟随（引用同一
	// 托管凭据，不搬明文）、不切换当前生效项。
	//
	// UC-094 A6：从下发卡复制出来的副本带**来源提示**（copiedFrom，见 copySourceSpec），
	// 名称用来源卡名派生；副本是新的个人配置，其后的修改只改副本、不回写上游。
	name := duplicateName(source.Name)
	if !isUser {
		name = duplicateName(sourced.copiedFrom.Name)
	}
	name, err = s.uniqueCardName(id, name)
	if err != nil {
		return nil, "", err
	}
	// UC-094 A7：复制同样过能力闸门（来源带回来的既有确认按 (code,model,capsHash) 复核；
	// 一致时仍有效，不阻断，也无须重新确认——用户确认过的是同一份声明与同一个模型）。
	report, acks, err := s.cardCapabilityPlanned(
		sourced.spec.Capabilities, effectiveCardModelID(sourced.spec.ModelID, sourced.spec.Models),
		nil, sourced.acks, time.Now())
	if err != nil {
		return nil, "", err
	}
	created, err := s.profiles.create(string(id), userCardSpec{
		Name:               name,
		ModelID:            sourced.spec.ModelID,
		Endpoint:           sourced.spec.Endpoint,
		EndpointIsFullBase: sourced.spec.EndpointIsFullBase,
		CredRef:            sourced.spec.CredRef,
		Models:             sourced.spec.Models,
		Settings:           settings,
		MaxOutputTokens:    sourced.spec.MaxOutputTokens,
		Capabilities:       sourced.spec.Capabilities,
		CapabilityAcks:     acks,
		CopiedFrom:         sourced.copiedFrom,
	})
	if err != nil {
		return nil, "", err
	}
	view, err := s.profileViewFor(id, created.ID)
	if err != nil {
		return nil, "", err
	}
	view.CapabilityCheck = &report
	return view, "", nil
}

// uniqueCardName 把候选名在**同工具自建卡**范围内调成不重名（UC-094 A2 的重名规则
// 是服务端权威：复制这类自动命名不该因为「副本已存在」失败，用户要的是副本本身）。
//
// 规则：候选名可用就直接用；否则依次追加 ` 2`/` 3`/…（有界，超界回退到 uuid 短后缀）。
func (s *Server) uniqueCardName(id family.ID, candidate string) (string, error) {
	rec := s.profiles.snapshot(string(id))
	taken := map[string]bool{}
	for _, c := range rec.User {
		taken[c.Name] = true
	}
	if !taken[candidate] {
		return candidate, nil
	}
	for i := 2; i <= 99; i++ {
		next := candidate + " " + itoa(i)
		if !taken[next] {
			return next, nil
		}
	}
	return "", apierr.New(apierr.CodeInvalidRequest,
		"同工具重名副本过多（%q 的编号副本已到上限）", candidate)
}

// duplicateName 给复制出来的卡加后缀（不切换当前档位，UC-018 A9）。
func duplicateName(name string) string {
	trimmed := strings.TrimSpace(name)
	if trimmed == "" {
		trimmed = "档位卡"
	}
	return trimmed + " 副本"
}

type profileOrderBody struct {
	Order []string `json:"order"`
}

func (s *Server) handleProfileOrder(r *http.Request) (any, string, error) {
	id, err := s.requireFamily(r.PathValue("id"))
	if err != nil {
		return nil, "", err
	}
	var body profileOrderBody
	if err := decode(r, &body); err != nil {
		return nil, "", err
	}
	if err := s.profiles.setOrder(string(id), body.Order); err != nil {
		return nil, "", err
	}
	// 回读一致：受管卡恒首位由 profileList 保证（它不从记账里取受管卡）。
	payload, err := s.profileList(id)
	return payload, "", err
}

// 只读原因（UC-094 A6：官方与下发配置的编辑限制**明示**）。
//
// 单一来源：回读面（`Profile.readOnlyReason`）与编辑/删除入口的拒绝措辞从同一组
// 常量拼出——界面说的与接口拒的是同一句话，不会两处各写一份而漂移。
const (
	readOnlyReasonOfficial = "官方卡由官方登录维护，不可编辑或删除"
	readOnlyReasonRouter   = "router 卡是形态派生卡，不可编辑或删除"
	readOnlyReasonManaged  = "网关下发配置由策略派生，不可编辑或删除"
)

// readOnlyReasonFor 给出某 kind 的只读原因；可编辑的 kind（user）返回空串（键缺席）。
func readOnlyReasonFor(kind string) string {
	switch kind {
	case ProfileKindOfficial:
		return readOnlyReasonOfficial
	case ProfileKindRouter:
		return readOnlyReasonRouter
	case ProfileKindManaged:
		return readOnlyReasonManaged
	}
	return ""
}

// requireUserCard 校验 pid 指向一张**可编辑的自建卡**：派生卡（官方/受管/router）
// 一律显式拒绝，其余必须是自己的自建卡。（下发卡判定已随登录下发删除——design §2.2。）
func (s *Server) requireUserCard(id family.ID, pid string) error {
	if pid == OfficialProfileID {
		return apierr.New(apierr.CodeInvalidRequest, "%s", readOnlyReasonOfficial)
	}
	if pid == RouterProfileID {
		return apierr.New(apierr.CodeInvalidRequest, "%s", readOnlyReasonRouter)
	}
	if pid == managedProfileIDFor(s.currentUID()) {
		return apierr.New(apierr.CodeInvalidRequest, "%s", readOnlyReasonManaged)
	}
	if _, ok := s.profiles.find(string(id), pid); !ok {
		return apierr.New(apierr.CodeNotFound, "档位卡不存在: %s", pid)
	}
	return nil
}

// requireCopySource 校验 pid 指向一张**可复制成新个人配置的卡**（UC-094 A6）：
// 自建卡可复制；官方/受管/router 派生卡的内容由策略或形态派生，复制没有意义。
// （下发卡复制通道已随登录下发删除——design §2.2。）
func (s *Server) requireCopySource(id family.ID, pid string) error {
	if pid == OfficialProfileID {
		return apierr.New(apierr.CodeInvalidRequest, "%s", readOnlyReasonOfficial)
	}
	if pid == RouterProfileID {
		return apierr.New(apierr.CodeInvalidRequest, "%s", readOnlyReasonRouter)
	}
	if pid == managedProfileIDFor(s.currentUID()) {
		return apierr.New(apierr.CodeInvalidRequest, "%s", readOnlyReasonManaged)
	}
	if _, ok := s.profiles.find(string(id), pid); !ok {
		return apierr.New(apierr.CodeNotFound, "档位卡不存在: %s", pid)
	}
	return nil
}

func isEditableSetting(key string) bool {
	for _, k := range editableSettingsKeys {
		if k == key {
			return true
		}
	}
	return false
}

// profileViewFor 返回单张卡的契约形状（用于创建/改/复制的响应体）。
func (s *Server) profileViewFor(id family.ID, pid string) (Profile, error) {
	list, err := s.profileList(id)
	if err != nil {
		return Profile{}, err
	}
	for _, item := range list.Items {
		if item.ID == pid {
			return item, nil
		}
	}
	return Profile{}, apierr.New(apierr.CodeNotFound, "档位卡不存在: %s", pid)
}

// ── §4.3 预览与应用 ─────────────────────────────────────────────────────────

// filePlanDTO 是契约 §4.3 的 FilePlan。
type filePlanDTO struct {
	Path         string     `json:"path"`
	Existed      bool       `json:"existed"`
	Delete       bool       `json:"delete"`
	SHA256Before string     `json:"sha256Before"`
	BytesBefore  int        `json:"bytesBefore"`
	Diff         []diffLine `json:"diff"`
	Warning      string     `json:"warning,omitempty"`
}

// previewPayload 是 §4.3 的预览响应。
type previewPayload struct {
	PreviewID string        `json:"previewId"`
	ExpiresAt string        `json:"expiresAt"`
	Operation string        `json:"operation"`
	Files     []filePlanDTO `json:"files"`
	Warning   string        `json:"warning,omitempty"`
}

func toFilePlanDTOs(plans []family.FilePlan, warnings map[string]string) []filePlanDTO {
	out := make([]filePlanDTO, 0, len(plans))
	for _, plan := range plans {
		dto := filePlanDTO{
			Path:         plan.Path,
			Existed:      plan.Existed,
			Delete:       plan.Delete,
			SHA256Before: tx.HashBytes(plan.Before),
			BytesBefore:  len(plan.Before),
			Diff:         buildDiff(plan.Before, plan.After),
		}
		if warnings != nil {
			dto.Warning = warnings[plan.Path]
		}
		if dto.Warning == "" && plan.Delete {
			// 「接管时创建，恢复后将被移除」是删除语义的固定提示（§4.3 的 warning 例）。
			dto.Warning = backup.WarningCreatedByTakeover
		}
		out = append(out, dto)
	}
	return out
}

type previewBody struct {
	ProfileID string `json:"profileId"`
	ModelID   string `json:"modelId"`
}

func (s *Server) handleToolPreview(r *http.Request) (any, string, error) {
	id, err := s.requireFamily(r.PathValue("id"))
	if err != nil {
		return nil, "", err
	}
	if proxyOnlyFamily(id) {
		return nil, "", apierr.New(apierr.CodeNotSupported,
			"WorkBuddy 走独立入口（/workbuddy/preview），不支持工具通道的预览")
	}
	var body previewBody
	if err := decode(r, &body); err != nil {
		return nil, "", err
	}
	if strings.TrimSpace(body.ProfileID) == "" {
		return nil, "", apierr.New(apierr.CodeInvalidRequest, "profileId 必填")
	}
	plans, err := s.planForProfile(r.Context(), id, body.ProfileID, body.ModelID)
	if err != nil {
		// 非法配置（如目标文件不可解析）被拒时，错误面同时给出恢复入口
		// （备份恢复 / 重置预览，UC-003 修订的恢复面接线）。
		return nil, "", s.withRecoveryHint(id, err)
	}
	lease := s.Leases.Put(&tx.Lease{
		ToolID:    string(id),
		Operation: "apply",
		Files:     plans,
		Meta:      map[string]string{"profileId": body.ProfileID},
	})
	return previewPayload{
		PreviewID: lease.ID,
		ExpiresAt: lease.ExpiresAt.Format(rfc3339Second),
		Operation: "apply",
		Files:     toFilePlanDTOs(plans, nil),
	}, "", nil
}

// planForProfile 把一张档位卡翻译成文件计划（纯计算，不触盘）。
func (s *Server) planForProfile(ctx context.Context, id family.ID, profileID, modelOverride string) ([]family.FilePlan, error) {
	return s.planForProfileMode(ctx, id, profileID, modelOverride, true)
}

func (s *Server) planForProfileNoFetch(ctx context.Context, id family.ID, profileID, modelOverride string) ([]family.FilePlan, error) {
	return s.planForProfileMode(ctx, id, profileID, modelOverride, false)
}

func (s *Server) planForProfileMode(
	ctx context.Context, id family.ID, profileID, modelOverride string, allowTokenFetch bool,
) ([]family.FilePlan, error) {
	p, err := s.Policy.Load()
	if err != nil {
		return nil, err
	}
	return s.planForProfileWithPolicy(ctx, id, profileID, modelOverride, allowTokenFetch, p)
}

func (s *Server) planForProfileWithPolicy(
	ctx context.Context, id family.ID, profileID, modelOverride string, allowTokenFetch bool, p *policy.Policy,
) ([]family.FilePlan, error) {
	f, ok := s.Families.Lookup(id)
	if !ok {
		return nil, errNotSupported(id)
	}
	// 家族路径解析为空 → path_conflict（UC-003 A9）：没有可写目标的计划是伪成功。
	paths := f.Paths(s.Env)
	if len(paths) == 0 {
		return nil, apierr.New(apierr.CodePathConflict,
			"该受管单元在当前环境下没有任何受管文件路径: %s", id)
	}
	uid := s.currentUID()
	switch {
	case profileID == OfficialProfileID:
		if !f.OfficialSeeded() {
			return nil, apierr.New(apierr.CodeInvalidRequest,
				"%s 没有官方档形态（additive 家族）", id)
		}
		return f.PlanOfficial(s.Env)
	case profileID == RouterProfileID:
		// router 形态（UC-024）：家族渲染经 family.RouterForm 接进同一条
		// Plan* + FilePlan + tx 写链；{回环} = GatewayInput.LocalRoute。
		rf, ok := f.(family.RouterForm)
		if !ok {
			return nil, apierr.New(apierr.CodeNotSupported,
				"%s 不支持请求映射形态（无 RouterForm 渲染入口）", id)
		}
		in, err := s.gatewayInputForMode(ctx, id, p, allowTokenFetch)
		if err != nil {
			return nil, err
		}
		if id == family.ClaudeDesktop {
			// claude-desktop 的请求映射形态（UC-046）：条目 = 别名集，没有单一模型槽位，
			// 因此 UC-043 的选择器解析/校验在这里**不适用**（另见 planDesktopRouter）。
			return s.planDesktopRouter(in, rf, strings.TrimSpace(modelOverride))
		}
		// 计划期模型解析（UC-043 四态 + UC-044 空表放宽）：求出该写进工具配置的名字——
		// 显式 modelId（必须 ∈ 选择器）> 策略默认可路由 > 枚举首个 > **空表 ⇒ 策略默认名**
		// （空表 = 未配置 ⇒ 数据面直连网关透传、不改写 model，默认名照旧生效，不再拒绝）。
		//
		// R159 守卫的边界因此收窄到**非空表**：登记了映射就是钳制（数据面按 UC-023 未命中 ⇒
		// 显式拒绝、不落兜底），那时绝不写出一个注定 400 的配置；空表下该判据不成立（数据面
		// 已改透传，见 UC-044）。非 router 卡的 modelId 校验（策略内模型 / 卡清单）零改动。
		model, err := routerCardModel(s.Proxy.RouterSelectors(), strings.TrimSpace(modelOverride), in.Model)
		if err != nil {
			return nil, err
		}
		in.Model = model
		return rf.PlanRouter(s.Env, in)
	case profileID == managedProfileIDFor(uid):
		in, err := s.gatewayInputForMode(ctx, id, p, allowTokenFetch)
		if err != nil {
			return nil, err
		}
		// claude-desktop 受管卡：路由模型完整在册时按多条目派生（UC-030）。
		// 自建卡走 default 分支（单条目选模型语义，不受路由模型影响）。
		if id == family.ClaudeDesktop {
			if err := s.attachDesktopModelEntries(&in); err != nil {
				return nil, err
			}
		}
		return f.PlanGateway(s.Env, in)
	default:
		rec := s.profiles.snapshot(string(id))
		card, ok := findUserCard(rec, profileID)
		if !ok {
			// 未知 pid 一律 invalid_request（与「过期预览」是两个码，不得互串）。
			return nil, apierr.New(apierr.CodeInvalidRequest, "未知档位卡: %s", profileID)
		}
		return s.planForUserProfileCard(ctx, id, f, p, profileID, modelOverride, card, "", allowTokenFetch)
	}
}

// planForUserProfileCard is the shared pure plan path for a saved user card and
// an unsaved draft candidate. transientCredential is used only by draft
// assessment; it is never written to the token vault or profile store.
func (s *Server) planForUserProfileCard(
	ctx context.Context,
	id family.ID,
	f family.Family,
	p *policy.Policy,
	profileID string,
	modelOverride string,
	card userProfileDoc,
	transientCredential string,
	allowTokenFetch bool,
) ([]family.FilePlan, error) {
	label := profileID
	if label == "" {
		label = "草稿"
	}
	// ── UC-032 启用链：先过「无模型可选」与「凭据步」，都拒绝在任何写盘语义前 ──
	if card.Models != nil && len(card.Models) == 0 {
		return nil, apierr.New(apierr.CodeInvalidRequest,
			"档位卡 %s 的模型清单为空（无模型可选），先补模型清单再启用", label)
	}
	credToken := transientCredential
	if credToken == "" && card.CredRef != "" {
		tok, err := s.resolveCredRefToken(card.CredRef)
		if err != nil {
			return nil, err
		}
		if tok == "" {
			// 凭据引用无法解析 ⇒ 启用时显式报缺凭据（UC-032 边界 / UC-023 同口径）；
			// 已撤销的引用在上一步就换成「已撤销」的明确文案（UC-094 A10）。
			return nil, apierr.New(apierr.CodeConfigInvalid,
				"档位卡 %s 的凭据引用无法解析（缺凭据）: %s", label, card.CredRef)
		}
		credToken = tok
	}
	model := card.ModelID
	if strings.TrimSpace(modelOverride) != "" {
		model = modelOverride
	}
	customEndpoint := strings.TrimSpace(card.Endpoint) != ""
	if card.EndpointIsFullBase {
		if err := validateProfileFullBaseSelection(id, card.Endpoint); err != nil {
			return nil, err
		}
	}
	fullBaseEndpoint := card.EndpointIsFullBase
	if fullBaseEndpoint && strings.TrimSpace(credToken) == "" {
		return nil, apierr.New(apierr.CodeInvalidRequest,
			"档位卡 %s 指向自定义端点时必须登记凭据（缺凭据）", label).
			WithDetails(map[string]any{
				"field":  "credRef",
				"reason": "custom_endpoint_credential_required",
			})
	}
	var in family.GatewayInput
	var err error
	if allowTokenFetch && !fullBaseEndpoint {
		in, err = s.gatewayInputFor(ctx, id, p)
	} else {
		in, err = s.gatewayInputForModeWithToken(ctx, id, p, false, credToken)
	}
	if err != nil {
		return nil, err
	}
	if customEndpoint && !fullBaseEndpoint && strings.TrimSpace(credToken) == "" && strings.TrimSpace(in.Token) != "" {
		return nil, apierr.New(apierr.CodeInvalidRequest,
			"档位卡 %s 指向自定义端点时必须登记凭据（缺凭据）", label).
			WithDetails(map[string]any{
				"field":  "credRef",
				"reason": "custom_endpoint_credential_required",
			})
	}
	// Every user-supplied endpoint remains an Origin override. Only cards that
	// explicitly froze endpointIsFullBase switch to the exact API base shape;
	// legacy cards continue through their family's old suffix behavior.
	if customEndpoint {
		in.Origin = strings.TrimSpace(card.Endpoint)
	}
	if fullBaseEndpoint {
		in.EndpointIsFullBase = true
		in.Token = credToken
	}
	// Legacy endpoint cards retain the prior credential/token behavior. The
	// explicit full-base direct mode above always uses the card's own credential.
	if !fullBaseEndpoint && credToken != "" && in.Token != "" {
		in.Token = credToken
	}
	if strings.TrimSpace(model) != "" {
		if err := s.validateCardModel(model, card.Models); err != nil {
			return nil, err
		}
		in.Model = model
	} else if card.Models != nil && len(card.Models) > 0 {
		// 有独立清单而未显式选中：清单首个即缺省选中（不回落策略 default——
		// 那是别家的模型名，写进自建 provider 会在请求时才炸）。
		in.Model = card.Models[0]
	}
	// UC-042：能力位（上下文窗口 / 最大输出）按**所选模型**的策略条目解析。
	applySelectedModelCapabilities(&in, p)
	// UC-040：用户声明的最大输出逐字采用。
	if card.MaxOutputTokens > 0 {
		in.MaxOutput = card.MaxOutputTokens
		in.MaxOutputExplicit = true
	}
	return f.PlanGateway(s.Env, in)
}

type applyBody struct {
	PreviewID string `json:"previewId"`
}

// activationPayload 是 §4.3 的 activation 字段。
type activationPayload struct {
	Tier            string `json:"tier"`
	ActiveProfileID string `json:"activeProfileId"`
}

// applyPayload 是 §4.3 的 apply 响应。
type applyPayload struct {
	Status     string            `json:"status"`
	Files      []tx.FileResult   `json:"files"`
	BackupIDs  []string          `json:"backupIds"`
	Activation activationPayload `json:"activation"`
}

func (s *Server) handleToolApply(r *http.Request) (any, string, error) {
	id, err := s.requireFamily(r.PathValue("id"))
	if err != nil {
		return nil, "", err
	}
	if proxyOnlyFamily(id) {
		return nil, "", apierr.New(apierr.CodeNotSupported,
			"WorkBuddy 走独立入口（/workbuddy/apply），不支持工具通道的应用")
	}
	var body applyBody
	if err := decode(r, &body); err != nil {
		return nil, "", err
	}
	data, backupID, err := s.applyLease(r.Context(), id, body.PreviewID)
	if err != nil {
		// 同预览路径：写入侧的 config_invalid（回滚失败面）同样接上恢复入口。
		return nil, backupID, s.withRecoveryHint(id, err)
	}
	return data, backupID, nil
}

// applyLease 执行一份已确认的租约（apply 与 restore/apply 共用）。
func (s *Server) applyLease(ctx context.Context, id family.ID, previewID string) (any, string, error) {
	lease, err := s.Leases.Take(previewID)
	if err != nil {
		return nil, "", err
	}
	if lease.ToolID != "" && lease.ToolID != string(id) {
		return nil, "", apierr.New(apierr.CodeInvalidRequest,
			"预览属于 %s，不能用 %s 应用", lease.ToolID, id)
	}
	result, err := s.applyTransaction(lease, tx.Options{
		WriteMode: 0o600,
		Backuper:  s.Backups,
		Reason:    backupReasonFor(lease.Operation),
	})
	if err != nil {
		// 失败时逐文件状态在 error.details.files 里（tx 已按契约 §4.3 填好），
		// 这里不重复造一份——两处各造一套状态正是契约明令禁止的。
		return nil, primaryBackupID(result.BackupIDs), err
	}
	if pid := lease.Meta["profileId"]; pid != "" && result.Status != "" {
		// 事务身份（租约 ID）随应用一起记账：它是「再次应用」的判据本体（UC-092 A10）。
		if err := s.profiles.setApplied(string(id), pid, lease.ID, time.Now()); err != nil {
			s.log.Error("登记生效档位卡失败", "tool", string(id), "profileId", pid, "err", err)
		}
	}
	// D2：恢复/重置后记账必须收敛——恢复目标（初始快照或任意写前备份）与重置后的
	// 盘上态都不是任何一张卡，`Applied` 残留会让 toolState 判「有记账且不符」，把产品
	// 自己的写入报成 drift。清空后按盘上真值判定（UC-018 A7）；真漂移语义不动。
	// 空计划的重置不在此列：它一个文件都没动，清记账反而会抹掉「盘上仍是某张卡」的事实。
	resetTouchedDisk := lease.Operation == "reset" && len(lease.Files) > 0
	if (lease.Operation == "restore" || resetTouchedDisk) && result.Status != "" {
		if err := s.profiles.setApplied(string(id), "", "", time.Time{}); err != nil {
			s.log.Error("恢复/重置后清理生效档位记账失败", "tool", string(id), "err", err)
		}
	}
	// 网关档的目标字节要在 live 字节上增量合并，live 一变计划就变：物化目录也必须跟着刷新
	// （UC-019：目录永远反映「若激活它会写成什么」，不留在上一个 live 状态）。
	s.refreshMaterialized(ctx)
	payload, err := s.applyPayloadFrom(result, id)
	return payload, primaryBackupID(result.BackupIDs), err
}

// workbuddyToolID 是 WorkBuddy 的工具 id（它有独立写链，但同样在家族表里）。
const workbuddyToolID = family.WorkBuddy

// applyPayloadFrom 把事务结果包成 §4.3 的 apply 响应，并**重新读盘**给出 activation。
//
// activation 必须来自盘上字节（契约 §4.1/§4.3）：它是「应用之后实际生效的是什么」，
// 由记账回答就成了自证。
func (s *Server) applyPayloadFrom(result tx.Result, id family.ID) (applyPayload, error) {
	tier, active, _, err := s.toolState(id)
	if err != nil {
		return applyPayload{}, err
	}
	return applyPayload{
		Status:     result.Status,
		Files:      fileResultsOrEmpty(result.Files),
		BackupIDs:  backupList(result.BackupIDs),
		Activation: activationPayload{Tier: string(tier), ActiveProfileID: active},
	}, nil
}

// fileResultsOrEmpty 保证 `files` 恒为数组（契约形状恒成立，前端不判 null）。
func fileResultsOrEmpty(files []tx.FileResult) []tx.FileResult {
	if files == nil {
		return []tx.FileResult{}
	}
	return files
}

// backupList 同理保证 `backupIds` 恒为数组。
func backupList(ids []string) []string {
	if ids == nil {
		return []string{}
	}
	return ids
}

// backupReasonFor 把租约的 operation 映射成备份 reason。
func backupReasonFor(operation string) string {
	switch operation {
	case "restore":
		return backup.ReasonRestore
	case "reset":
		return backup.ReasonReset
	}
	return backup.ReasonApply
}

func primaryBackupID(ids []string) string {
	if len(ids) == 0 {
		return ""
	}
	return ids[0]
}

func (s *Server) handleToolRestorePreview(r *http.Request) (any, string, error) {
	id, err := s.requireFamily(r.PathValue("id"))
	if err != nil {
		return nil, "", err
	}
	var body struct {
		BackupID string `json:"backupId"`
	}
	if err := decode(r, &body); err != nil {
		return nil, "", err
	}
	if strings.TrimSpace(body.BackupID) == "" {
		return nil, "", apierr.New(apierr.CodeInvalidRequest, "backupId 必填")
	}
	plan, err := s.Backups.PlanRestoreDetailed(string(id), body.BackupID)
	if err != nil {
		return nil, "", err
	}
	lease := s.Leases.Put(&tx.Lease{
		ToolID:    string(id),
		Operation: "restore",
		Files:     plan.Files,
		Warning:   restoreWarning(plan),
		Meta:      map[string]string{"backupId": body.BackupID},
	})
	return previewPayload{
		PreviewID: lease.ID,
		ExpiresAt: lease.ExpiresAt.Format(rfc3339Second),
		Operation: "restore",
		Files:     toFilePlanDTOs(plan.Files, plan.Warnings),
		Warning:   lease.Warning,
	}, "", nil
}

// restoreWarning 把「跳过未受管路径」汇成一条顶层提示（逐文件提示已在 Files 上）。
func restoreWarning(plan backup.RestorePlan) string {
	if len(plan.Skipped) == 0 {
		return ""
	}
	reasons := make([]string, 0, len(plan.Skipped))
	for _, skipped := range plan.Skipped {
		reasons = append(reasons, skipped.Path+": "+skipped.Reason)
	}
	return "以下路径未纳入恢复计划 —— " + strings.Join(reasons, "；")
}

func (s *Server) handleToolRestoreApply(r *http.Request) (any, string, error) {
	id, err := s.requireFamily(r.PathValue("id"))
	if err != nil {
		return nil, "", err
	}
	var body applyBody
	if err := decode(r, &body); err != nil {
		return nil, "", err
	}
	data, backupID, err := s.applyLease(r.Context(), id, body.PreviewID)
	if err != nil {
		return nil, backupID, s.withRecoveryHint(id, err)
	}
	return data, backupID, nil
}

func (s *Server) handleToolBackups(r *http.Request) (any, error) {
	id, err := s.requireFamily(r.PathValue("id"))
	if err != nil {
		return nil, err
	}
	limit, cursor, err := PageParams(r)
	if err != nil {
		return nil, err
	}
	page, err := s.Backups.List(string(id), backup.ListOptions{Limit: limit, Cursor: cursor})
	if err != nil {
		return nil, err
	}
	return NewPage(page.Items, page.NextCursor, page.Total), nil
}

// handleToolProbe 重新读盘判定（只读重判，不改盘；返回同 GET /tools/{id}）。
func (s *Server) handleToolProbe(r *http.Request) (any, string, error) {
	id, err := s.requireFamily(r.PathValue("id"))
	if err != nil {
		return nil, "", err
	}
	// 同步重扫该族：这正是「probe」与普通读取的区别（普通读取只读缓存）。
	s.probe.refreshOne(id)
	view, err := s.toolViewFor(id)
	if err != nil {
		return nil, "", err
	}
	return toolDetail{toolView: view, CompanionKeys: s.readCompanions(id)}, "", nil
}

// ── §5 备份保留 ─────────────────────────────────────────────────────────────

func (s *Server) handleBackupSettings(*http.Request) (any, error) {
	settings, err := s.Backups.Settings()
	if err != nil {
		return nil, err
	}
	return settings, nil
}

func (s *Server) handleBackupSettingsSave(r *http.Request) (any, string, error) {
	var body struct {
		Keep int `json:"keep"`
	}
	if err := decode(r, &body); err != nil {
		return nil, "", err
	}
	settings, err := s.Backups.SetKeep(body.Keep)
	if err != nil {
		return nil, "", err
	}
	return settings, "", nil
}
