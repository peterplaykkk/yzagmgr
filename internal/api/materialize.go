// 配置物化（UC-019）：把「每张档位卡会写成什么」落到 `<Home>/.yzagmgr/materialize/<agent>/<profile>/`。
//
// 为什么放在 api 层：物化是**同一份 `planForProfile` 的第二次投递**——把它的目标字节写到
// 别处而不是 live。家族实现（`internal/family`）只回答「目标字节是什么」，不回答
// 「写到哪里」，因此这里不重复任何配置层判断；反过来，任何「为了物化再算一遍字节」的
// 写法都会让物化内容与 apply 内容悄悄分叉（A3 要的正是「看到的就是 apply 会写的」）。
//
// 四条纪律：
//  1. **零触碰 live**（A2）：物化不经 `internal/tx`（那是给 live 写的执行器：它会记账、
//     会做备份），只写物化根下的文件。live 路径在物化前后逐字节不变。
//  2. **根取 `Env.Home`**（A6）：`YZAGMGR_HOME` 一旦设定，物化根自动跟着隔离；
//     任何地方都不读 `os.UserHomeDir()`，否则隔离运行会写进真实 `~/.yzagmgr/materialize`。
//  3. **幂等**（A5）：目标字节与盘上相同就不触盘（mtime 不变）；卡片消失后其目录在
//     下次物化时被清掉，不留孤儿。
//  4. **失败逐项报出**（A9）：一张卡算不出来只登记这一项，其余单元照常完成。
//
// **物化目录可能承载凭据**（写文档/做展示时别写成「这里没有凭据」）：六个形态内嵌令牌的族
// （pi/opencode/openclaw/hermes/gemini/grokbuild）的托管条目带 `apiKey`，物化按 A3 逐字节
// 照搬，因此目录里就是真令牌。显式 endpointIsFullBase 的 Claude / Claude Desktop / Codex
// 自建直连卡也带卡片自己的令牌并按原样物化。Claude 的普通受管卡仍写占位符、
// Claude Desktop 的普通 Proxy 卡写自发密钥、Codex 的普通网关卡不含令牌。
// 权限纪律（根 0700 / 文件 0600）与「不进 Git、不分享」由此而来：可分享的是 `launchCommand`，
// 它只指向目录或本地代理，本身不含凭据。
package api

import (
	"bytes"
	"context"
	"encoding/json"
	"io/fs"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/peterplaykkk/yzagmgr/internal/apierr"
	"github.com/peterplaykkk/yzagmgr/internal/docmodel"
	"github.com/peterplaykkk/yzagmgr/internal/family"
	"github.com/peterplaykkk/yzagmgr/internal/family/claude"
	"github.com/peterplaykkk/yzagmgr/internal/family/claudedesktop"
	"github.com/peterplaykkk/yzagmgr/internal/family/codex"
)

// materializeDirName 是物化根所在的目录名；物化根 = `<Home>/.yzagmgr/materialize`
// （UC-019 A1/A6，R236：数据根 `.yzagmgr` 之下的固定 `materialize/` 子目录——与 data/logs
// 分居，论证见 doc/rename-uniqueness-runbook.md §3.2）。
const materializeDirName = ".yzagmgr"

// materializeLeafName 是物化根在数据根下的固定子目录名（与 materializeDirName 拼合即物化根）。
const materializeLeafName = "materialize"

// codexProviderCustom 是 codex 托管 provider 的段名（`[model_providers.custom]`）。
const codexProviderCustom = "custom"

// opencodeConfigDirName 是 opencode 物化布局里的配置根目录名。
//
// 它**不是**可选的排版选择：opencode 只读 `$XDG_CONFIG_HOME/opencode/opencode.json`，
// 而 `OPENCODE_CONFIG` 指文件并不替代配置根（2026-09-22 真机实测：设了它，opencode 仍会
// 读 `$XDG_CONFIG_HOME/opencode/*`）。所以目录里多一层 `opencode/`，命令才能只给
// `XDG_CONFIG_HOME`——见 materializedNames 与 launchSupportFor 的 opencode 分支。
const opencodeConfigDirName = "opencode"

// materializePayload 是 §4.4 的清单形状。
//
// `launchCommand` 在**每个 profile** 里：命令必须带该目录，放到 family 层拼不出来。
// `launchVerified` / `launchNote` 在 family 层：它们表达的是 A7 的「这个单元的启动口径
// 有没有经真机验证」，与具体某张卡无关；未验证的单元必须如实置 false，
// 界面据此显示警告而不是假装可用。
type materializePayload struct {
	Root            string               `json:"root"`
	Families        []materializeFamily  `json:"families"`
	PartialFailures []materializeFailure `json:"partialFailures"`
}

// materializeFamily 是一个受管单元的物化清单。
type materializeFamily struct {
	ToolID         string               `json:"toolId"`
	Name           string               `json:"name"`
	LaunchVerified bool                 `json:"launchVerified"`
	LaunchNote     string               `json:"launchNote"`
	Profiles       []materializeProfile `json:"profiles"`
}

// materializeProfile 是一张档位卡的物化结果。
type materializeProfile struct {
	ProfileID     string   `json:"profileId"`
	Name          string   `json:"name"`
	Kind          string   `json:"kind"`
	Dir           string   `json:"dir"`
	LaunchCommand string   `json:"launchCommand"`
	Files         []string `json:"files"`
	// ContainsCredentials 指示这张卡的物化副本里**是否含真实凭据**（UC-094 A11 的
	// 指示面）：只指示不含值。消费方（界面）据此在执行物化前**逐一展示副本路径与
	// 保护提醒**（只限本机、不可公开分享/同步；目录 0700、文件 0600）。
	// 判据见 materializedCardContainsCredentials 的注释；恒在场（true/false 都是断言）。
	ContainsCredentials bool `json:"containsCredentials"`
}

// materializeFailure 是一项算不出来的卡片（A9 的部分成功）。
//
// `byDesign` 把「按设计跳过」从「真失败」里分出来：消费方（界面）不能靠 `reason` 文案
// 判断——文案会被改写、被翻译，而两者要触发的是不同的呈现（WorkBuddy 的跳过在单元面板里
// 显示一句专门的说明，不计入任何「未完成」计数与首屏告警）。条目**仍留在
// `partialFailures` 里**（它必须可见），因此没有另开 `skipped[]`。
//
// **`omitempty` 是契约而不是省字节**：缺席与 `false` 对消费方都表示「计入未完成」
// （失败安全：宁可多报，不可静默丢弃），因此真失败条目省略它反而更紧——「键在场」就
// 只可能表示 true，按「键是否存在」判定的消费方不会被显式的 false 骗到；真失败条目的
// 字节形状也与加字段之前逐字节相同（没学过这个字段的消费方行为不变）。
type materializeFailure struct {
	ToolID    string `json:"toolId"`
	ProfileID string `json:"profileId"`
	Reason    string `json:"reason"`
	ByDesign  bool   `json:"byDesign,omitempty"`
}

// materializeCardRef 是物化视角下的一张档位卡（只有清单需要的三样）。
type materializeCardRef struct {
	ID                 string
	Name               string
	Kind               string
	EndpointIsFullBase bool
}

// materializeRoot 给出物化根。
//
// 守卫刻意收得紧：只认「绝对路径 + 路径恰为 `<Home>/.yzagmgr/materialize`」（末段与父段
// 都对上）。清理孤儿目录是按「根下的目录名」判定的，若 `Home` 被误配成空串或相对路径，
// `filepath.Join` 会把它变成一个**相对 cwd** 的路径，于是「清孤儿」就有了删别人东西的能力。宁可拒绝。
func (s *Server) materializeRoot() (string, error) {
	root := filepath.Join(s.Env.Home, materializeDirName, materializeLeafName)
	if !filepath.IsAbs(root) || filepath.Base(root) != materializeLeafName ||
		filepath.Base(filepath.Dir(root)) != materializeDirName {
		return "", apierr.New(apierr.CodeConfigInvalid,
			"物化根必须位于绝对路径下且为 %s/%s（Home=%q）",
			materializeDirName, materializeLeafName, s.Env.Home)
	}
	return root, nil
}

// handleMaterializeRun 是 `POST /api/v1/materialize`：重新物化全部单元的全部档位卡。
func (s *Server) handleMaterializeRun(r *http.Request) (any, string, error) {
	payload, err := s.materializeRun(r.Context())
	if err != nil {
		return nil, "", err
	}
	return payload, "", nil
}

// handleMaterializeList 是 `GET /api/v1/materialize`：列出物化目录与启动命令（只读）。
//
// **从未物化过时**（#27 的定稿）：`families[]` 照常列出，每张卡的 `dir` 仍给出预期路径、
// `files` 为空数组。`dir` 由「单元 + 卡 id + 根」确定性算出，因此提前给出不产生歧义；
// 界面据此显示「尚未物化 + 开始物化」，不必自己拼路径、也不必引入第二个状态源。
func (s *Server) handleMaterializeList(r *http.Request) (any, error) {
	return s.materializeInspect(r.Context())
}

// materializeRun 计算全部卡片的目标字节并写盘。
//
// 返回的清单与 GET 同形：POST 已经把这份清单算在手上了，让 `curl` 调用方一次拿到
// 「写完了什么 + 哪些没写成 + 启动命令是什么」，与「API 是一等公民」同口径。
func (s *Server) materializeRun(ctx context.Context) (materializePayload, error) {
	return s.materializePass(ctx, true)
}

// refreshMaterialized 是「物化目录跟着状态走」的唯一入口（与 syncProxy 同一条纪律：
// 一个入口、一件事做全，调用点散落是上一轮缺陷的根因）。
//
// 调用点＝**会改变「某张卡会写成什么」的路径**：生效档变化（apply / 状态栏 apply，家族
// 的网关档要在 live 字节上做增量合并，故 live 一变计划就变）、策略变化（受管卡的默认
// 模型、codex 的目录）、以及 POST /materialize 自身。
//
// 刻意**不**放进装配期 New 与代理启停：前者会为了取令牌在启动路径上同步等网络
// （「启动不依赖网络」是既有契约），后两者不改变任何计划输入；这三处漏掉的只是
// 「进程重启后目录内容与盘上状态的一致性」，而目录本身就是随时可重建的派生物。
//
// 失败只记日志：物化是派生物，它坏了不该让一次已经落盘的 apply 报失败。
func (s *Server) refreshMaterialized(ctx context.Context) {
	if _, err := s.materializeRun(ctx); err != nil {
		s.log.Warn("物化目录刷新失败（live 已生效，目录可能落后一轮）", "err", err)
	}
}

// materializeInspect 只读盘点：不写任何文件，`files` 取盘上事实（界面复制走的是它）。
//
// 仍然跑一遍计划计算（纯计算 + 只读 import）：`partialFailures` 必须在**第一次 POST
// 之前**就能显示——「策略缺失所以这张卡算不出来」正是用户在还没物化时最需要看到的原因。
// 计算过程不落任何字节（`planForProfile` 的零副作用是 UC-003 A2 的既有保证）。
func (s *Server) materializeInspect(ctx context.Context) (materializePayload, error) {
	return s.materializePass(ctx, false)
}

// materializePass 是两条路径的共用实现：write=true 写盘，false 只盘点。
func (s *Server) materializePass(ctx context.Context, write bool) (materializePayload, error) {
	root, err := s.materializeRoot()
	if err != nil {
		return materializePayload{}, err
	}
	// A4 的引用候选先盘点一次（每张卡都遍历一遍数据根纯属浪费；卡多时是几十次目录遍历）。
	referables := dataRootFiles(s.Env.DataRoot())

	out := materializePayload{
		Root:            root,
		Families:        make([]materializeFamily, 0, len(family.All)),
		PartialFailures: []materializeFailure{},
	}
	expected := map[string]map[string]bool{}
	for _, id := range s.Families.IDs() {
		if proxyOnlyFamily(id) {
			// WorkBuddy 有独立写链（/workbuddy/*），没有能指向物化目录的启动命令。
			// 明确跳过并登记原因：静默产出一个空的 `workbuddy/` 目录是最坏的一种「成功」。
			// `ByDesign` 是给消费方的机器标记：这不是失败（界面据此不把它计成未完成），
			// 但条目本身照旧留在清单里——跳过是事实，也要在界面上有一句可读的说明。
			out.PartialFailures = append(out.PartialFailures, materializeFailure{
				ToolID:   string(id),
				Reason:   "WorkBuddy 走独立入口（/workbuddy/*），物化不覆盖它（不产空目录）",
				ByDesign: true,
			})
			continue
		}
		entry, dirs, failures := s.materializeFamilyCards(ctx, id, root, referables, write)
		out.Families = append(out.Families, entry)
		out.PartialFailures = append(out.PartialFailures, failures...)
		expected[string(id)] = dirs
	}
	if write {
		if err := pruneMaterialized(root, expected); err != nil {
			// 清理失败不影响本次写入的结果，但必须响亮：孤儿目录会让「卡已删除」表现为假象。
			s.log.Warn("物化目录清理失败", "root", root, "err", err)
		}
	}
	return out, nil
}

// materializeFamilyCards 处理一个单元的全部档位卡。
//
// 返回：该单元的清单、期望存在的目录名集合（供清理判定）、逐项失败。
func (s *Server) materializeFamilyCards(
	ctx context.Context, id family.ID, root string, referables []string, write bool,
) (materializeFamily, map[string]bool, []materializeFailure) {
	f, ok := s.Families.Lookup(id)
	if !ok {
		return materializeFamily{ToolID: string(id), Profiles: []materializeProfile{}},
			map[string]bool{}, nil
	}
	support := launchSupportFor(id)
	entry := materializeFamily{
		ToolID:         string(id),
		Name:           f.DisplayName(),
		LaunchVerified: support.Verified,
		LaunchNote:     support.Note,
		Profiles:       []materializeProfile{},
	}
	dirs := map[string]bool{}
	var failures []materializeFailure

	for _, card := range s.materializeCards(id) {
		// 目录名就是卡 id（`<root>/<agent>/<profile>/`，A1）：同一单元的不同卡各自成目录，
		// 因此不需要在目录名上再拼单元名，也不会互相覆盖。
		dir := filepath.Join(root, string(id), card.ID)
		dirs[card.ID] = true
		view, failure := s.materializeOneCard(ctx, id, card, dir, support, referables, write)
		if failure != nil {
			failures = append(failures, *failure)
		}
		entry.Profiles = append(entry.Profiles, view)
	}
	return entry, dirs, failures
}

// materializeOneCard 处理一张卡：算 → （写）→ 组装清单项。
//
// 失败时**保持目录原状**：删掉上一轮的内容等于把「这轮算不出来」变成「什么都没了」，
// 而物化目录的价值恰恰是「可随时重建 + 手边留着一份」。失败项在清单里如实登记原因。
func (s *Server) materializeOneCard(
	ctx context.Context, id family.ID, card materializeCardRef, dir string,
	support launchSupport, referables []string, write bool,
) (materializeProfile, *materializeFailure) {
	view := materializeProfile{
		ProfileID:           card.ID,
		Name:                card.Name,
		Kind:                card.Kind,
		Dir:                 dir,
		Files:               []string{},
		ContainsCredentials: materializedCardContainsCredentials(id, card.Kind, card.EndpointIsFullBase),
	}
	plans, err := s.planForProfile(ctx, id, card.ID, "")
	if err == nil {
		content := plannedContent(plans)
		if write {
			err = s.materializeWriteCard(dir, referables, content, materializedNames(id, plans))
		}
		if err == nil {
			view.ContainsCredentials = materializedContainsCredentialsForContent(id, card.Kind, card.EndpointIsFullBase, content, false)
			view.LaunchCommand = materializedLaunchCommand(s, id, card, dir, content, false, support)
		}
	}
	if err != nil {
		// 命令与文件列表按**盘上事实**给出：目录里那份可能还是上一轮写的，但它确实存在，
		// 界面据此仍能复制出一条指向真实位置的命令。
		content, files, unreadable := materializedDirContent(dir)
		view.ContainsCredentials = materializedContainsCredentialsForContent(id, card.Kind, card.EndpointIsFullBase, content, unreadable)
		view.LaunchCommand = materializedLaunchCommand(s, id, card, dir, content, unreadable, support)
		view.Files = files
		// Reason 进的是成功信封的 partialFailures[].reason，但承载的是同一类错误消息
		// （planForProfile 的解析失败记号回显是可达路径）——与信封同口径掩码（UC-014 A11）。
		return view, &materializeFailure{ToolID: string(id), ProfileID: card.ID, Reason: maskedErrorText(err)}
	}
	view.Files = materializedFileNames(dir)
	return view, nil
}

// plannedContent 从计划里取出「要写出的文件」。
//
// 两条排除，都会在目录里留下假东西，因此必须显式判掉：
//   - `Delete` 的项是「该卡会让这个文件消失」——写出它反而是伪造；
//   - live 上缺席且目标字节为空（`Existed=false` 且 `After` 为空）的项是**无事可做**：
//     家族用这种形态表达「这个文件在我这一档里不存在」，写出一个 0 字节文件会让
//     「live 上没有」变成「目录里有一个空文件」。
func plannedContent(plans []family.FilePlan) map[string][]byte {
	content := map[string][]byte{}
	for _, plan := range plans {
		if plan.Delete || (len(plan.After) == 0 && !plan.Existed) {
			continue
		}
		content[plan.Path] = plan.After
	}
	return content
}

// materializeCards 枚举某单元的全部档位卡：受管卡恒在、官方卡按家族能力、自建卡按记账顺序。
//
// 与 `profileList` 的差异只在**错误处理**：这里必须容错（策略缺失不能让整个单元消失，
// A9），因此不在这里取模型、不报错，把「算不出来」留给 `planForProfile` 逐项回答。
func (s *Server) materializeCards(id family.ID) []materializeCardRef {
	f, ok := s.Families.Lookup(id)
	if !ok {
		return nil
	}
	out := []materializeCardRef{{
		ID:   managedProfileIDFor(s.currentUID()),
		Name: managedCardName,
		Kind: ProfileKindManaged,
	}}
	if f.OfficialSeeded() {
		// 无官方形态的家族（additive 四家）没有官方卡，因此不产 `official/` 目录（A8）。
		out = append(out, materializeCardRef{
			ID:   OfficialProfileID,
			Name: officialCardName,
			Kind: ProfileKindOfficial,
		})
	}
	for _, card := range orderedUsers(s.profiles.snapshot(string(id))) {
		name := strings.TrimSpace(card.Name)
		if name == "" {
			name = card.ID
		}
		out = append(out, materializeCardRef{
			ID: card.ID, Name: name, Kind: ProfileKindUser,
			EndpointIsFullBase: card.EndpointIsFullBase && strings.TrimSpace(card.Endpoint) != "",
		})
	}
	if _, ok := f.(family.RouterForm); ok {
		// router 形态卡（UC-024）：与 profileList 同序（自建卡之后殿后）。
		out = append(out, materializeCardRef{
			ID:   RouterProfileID,
			Name: routerCardName,
			Kind: ProfileKindRouter,
		})
	}
	return out
}

// materializedCardContainsCredentials 报告这张卡的物化副本里是否含**真实凭据**
// （UC-094 A11 的指示面；只指示不含值）。
//
// 判据来自写链而不是猜测（与文件头「物化目录可能承载凭据」一段同源）：
//   - **六个形态内嵌令牌的族**（pi/opencode/openclaw/hermes/gemini/grokbuild）的
//     网关档把**解析后的真实令牌**逐字节写进配置（托管卡 = 本机推理令牌；自建卡 =
//     卡自有凭据，指向自定义端点时同样是真实秘密）——物化按 A3 照搬 ⇒ 目录里就是真令牌；
//   - endpointIsFullBase 自建直连卡（claude/claude-desktop/codex 等）把该卡自己的凭据写进
//     完整 provider 配置；物化同样照搬，指示位为 true；
//   - claude 盘上恒为占位符（UC-003 A15 的装配层闸门 `tokenForFamily`）、
//     claude-desktop 普通 Proxy 卡写自发派生密钥（ProxyKey，不是用户真实秘密）、codex 普通网关卡的
//     `auth.json` 不进计划且 `config.toml` 不含令牌 ⇒ 这些普通形态为 false；
//   - router 形态：凭据位恒为占位符（各族 RouterEntry 的契约）⇒ false；
//   - 官方档：PlanOfficial 移除托管条目（additive 四家）或回官方种子 ⇒ false。
//
// 这个判据与装配层边界（tokenForFamily 的「哪些族拿真令牌」）配对，并由
// TestMaterializeCredentialIndicatorMatchesReality 以 TokenSeed 合成令牌**逐族钉住**
// （判 false 的族，哨兵字节不得出现在目录里；判 true 的卡，目录里必须出现其真实凭据）。
func materializedCardContainsCredentials(id family.ID, kind string, endpointIsFullBase bool) bool {
	switch kind {
	case ProfileKindOfficial, ProfileKindRouter:
		return false
	}
	if endpointIsFullBase {
		return true
	}
	switch id {
	case family.Pi, family.OpenCode, family.OpenClaw, family.Hermes, family.Gemini, family.Grokbuild:
		return true
	default:
		return false
	}
}

// materializedContainsCredentialsForContent prefers the bytes actually present in a
// credential-bearing family directory over current profile metadata. A failed refresh
// can leave an older direct profile on disk after its profile flag changed; that file
// may still contain a real credential and must never be reported credential-free.
func materializedContainsCredentialsForContent(id family.ID, kind string, endpointIsFullBase bool, content map[string][]byte, unreadable bool) bool {
	if kind == ProfileKindOfficial || kind == ProfileKindRouter {
		return false
	}
	if unreadable {
		return true
	}
	if kind == ProfileKindUser {
		var hasCredential, uncertain bool
		switch id {
		case family.Claude:
			hasCredential, uncertain = claudeMaterializedCredentialState(content)
		case family.ClaudeDesktop:
			hasCredential, uncertain = claudeDesktopMaterializedCredentialState(content)
		case family.Codex:
			hasCredential, uncertain = codexMaterializedCredentialState(content)
		default:
			return materializedCardContainsCredentials(id, kind, endpointIsFullBase)
		}
		return hasCredential || uncertain
	}
	return materializedCardContainsCredentials(id, kind, endpointIsFullBase)
}

// ── 落盘 ────────────────────────────────────────────────────────────────────

// materializeWriteCard 把一张卡的目标字节写进它的物化目录（A1 + A4）。
//
// 顺序刻意是「先全部改写、再落盘」：改写要用到**全部**计划文件的物化名（被引用的文件
// 可能就是另一个计划文件），边改边写会在文件顺序上引入依赖。目录按需建立：
// 一张什么都不写的卡不产出空目录（空目录会被读成「物化过了」）。
func (s *Server) materializeWriteCard(
	dir string, referables []string, content map[string][]byte, names map[string]string,
) error {
	rewritten := make(map[string][]byte, len(content))
	copies := map[string][]byte{}
	// 引用候选 = 本卡的全部计划路径 ∪ 数据根下的现有文件。计划路径必须在内：codex 的
	// `model_catalog_json` 指向的正是**同一张卡的另一个计划文件**，而那个文件在
	// 「还没有 apply 过」时盘上并不存在——只按盘上文件找候选会让这条改写静默不发生。
	refs := make([]string, 0, len(content)+len(referables))
	for planPath := range content {
		refs = append(refs, planPath)
	}
	refs = append(refs, referables...)
	sort.Strings(refs)
	for planPath, blob := range content {
		rewritten[planPath] = s.rewriteDataRootRefs(dir, refs, blob, content, names, copies)
	}
	expected := map[string]bool{}
	for planPath, blob := range rewritten {
		rel := names[planPath]
		expected[rel] = true
		if err := writeMaterializedFile(filepath.Join(dir, filepath.FromSlash(rel)), blob); err != nil {
			return err
		}
	}
	for target, blob := range copies {
		if rel, err := filepath.Rel(dir, target); err == nil {
			expected[filepath.ToSlash(rel)] = true
		}
		if err := writeMaterializedFile(target, blob); err != nil {
			return err
		}
	}
	return convergeMaterializedDir(dir, expected)
}

// convergeMaterializedDir 把目录收敛成「恰好是这张卡这一轮的物化文件集」。
//
// 为什么需要：目录是派生物而不是备份（UC-019 边界），残留的旧文件会被读成「这张卡会写它」
// ——例如 codex 官方档在 live 被清空后再物化，上一轮的 `config.toml` 若留在目录里，
// 目录说的就不是当前这一档的形态。因此多出来的文件删掉；全空的目录一并删掉
// （「目录存在」=「这张卡有东西可写」，与从未物化的空 `files` 区分开）。
func convergeMaterializedDir(dir string, expected map[string]bool) error {
	var firstErr error
	if _, err := os.Stat(dir); err != nil {
		return nil // 尚未落过盘；本轮也没有要写的文件
	}
	_ = filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		rel, relErr := filepath.Rel(dir, path)
		if relErr != nil {
			return nil
		}
		if expected[filepath.ToSlash(rel)] {
			return nil
		}
		if rmErr := os.Remove(path); rmErr != nil && firstErr == nil {
			firstErr = rmErr
		}
		return nil
	})
	// 自下而上清掉空目录（含目录本身）：`expected` 为空时整棵目录都不该存在。
	dirs := []string{}
	_ = filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err == nil && d.IsDir() && path != dir {
			dirs = append(dirs, path)
		}
		return nil
	})
	sort.Sort(sort.Reverse(sort.StringSlice(dirs)))
	for _, sub := range dirs {
		if entries, err := os.ReadDir(sub); err == nil && len(entries) == 0 {
			_ = os.Remove(sub)
		}
	}
	if entries, err := os.ReadDir(dir); err == nil && len(entries) == 0 {
		_ = os.Remove(dir)
	}
	return firstErr
}

// rewriteDataRootRefs 做 A4 的唯一一件事：把**指向本产品数据根**的绝对路径改写为物化
// 目录里的对应路径，并把被引用的文件一并放进目录。它买到的是**指针自洽**——目录内不再有
// 指向数据根的绝对指针，不悬空、不因数据根搬迁而静默失效（codex 的 `model_catalog_json`
// 悬空即启动失败，§A 行 3 真机实测），除这次替换外字节一个都不动。
//
// 它**不**买到「拷到别处就能跑」：凭据由本地代理转发时注入，目录里刻意不放凭据副本
// （UC-019 非目标），因此启动命令的可用前提是本机 v2 在跑。
//
// 候选集是「本卡的计划路径 ∪ 数据根下的现有文件」，按长度降序做**整串**替换：
// 不扫描、不用分隔符启发式——路径里可能有空格，启发式一旦截断就会留下一个「看起来改过
// 了其实没改」的指针，而那种指针在真机上的表现是启动级失败（codex 的
// `model_catalog_json` 悬空即 `Error: No such file or directory`，§A 行 3 真机实测）。
//
// 被引用的文件优先取**计划字节**（它就是 apply 会写的字节，且不依赖「apply 过没有」）；
// 只有它不在计划里时才回读数据根。两者都拿不到就不改写：把一个已经悬空的指针换个位置
// 没有任何好处，反而少了「它原本指向数据根」这条线索。
func (s *Server) rewriteDataRootRefs(
	dir string, refs []string, blob []byte, content map[string][]byte,
	names map[string]string, copies map[string][]byte,
) []byte {
	for _, ref := range orderedReferences(refs) {
		if !bytes.Contains(blob, []byte(ref)) {
			continue
		}
		target, ok := materializedTarget(ref, dir, names)
		if !ok {
			continue
		}
		if _, isPlan := content[ref]; !isPlan {
			raw, err := os.ReadFile(ref)
			if err != nil {
				// 目标不在场：原样保留并响亮记日志（改写成目录内的路径只会换个地方悬空）。
				s.log.Warn("物化引用目标不可读，指针保持原样", "ref", ref, "err", err)
				continue
			}
			copies[target] = raw
		}
		blob = bytes.ReplaceAll(blob, []byte(ref), []byte(target))
	}
	return blob
}

// orderedReferences 给出替换次序：长路径优先。
//
// 「长优先」不是洁癖：数据根下若存在互为前缀的两个路径（`…/a` 与 `…/a/b`），先替换短的
// 会把长的截成半截路径。
func orderedReferences(refs []string) []string {
	out := append([]string(nil), refs...)
	sort.Slice(out, func(i, j int) bool {
		if len(out[i]) != len(out[j]) {
			return len(out[i]) > len(out[j])
		}
		return out[i] < out[j]
	})
	return out
}

// materializedTarget 把「数据根下的绝对路径」翻译成物化目录里的绝对路径。
//
// 计划文件落在自己的物化名上（A1 的 basename 规则，codex 的目录文件因此与
// `model_catalog_json` 指到同一处，见 TestCodexMaterializedCatalogPointerResolves）；
// 其余被引用的文件按 basename 拷进目录根。
func materializedTarget(ref, dir string, names map[string]string) (string, bool) {
	if name, ok := names[ref]; ok {
		return filepath.Join(dir, filepath.FromSlash(name)), true
	}
	base := filepath.Base(ref)
	if base == "" || base == "." || base == "/" {
		return "", false
	}
	return filepath.Join(dir, base), true
}

// materializedNames 给出每个计划文件的物化相对名。
//
// 默认取 live 路径的 basename（A1 的例子：`~/.codex/config.toml` → `config.toml`）。
// 例外有两类，都是「扁平布局表达不了该单元的形态」：
//   - **同一张卡里两个受管文件同名**：claude-desktop 的
//     `Claude/claude_desktop_config.json` 与 `Claude-3p/claude_desktop_config.json` 都叫
//     `claude_desktop_config.json`。扁平布局下后者会覆盖前者，那既违反「写出**全部**受管文件」，
//     也让 A3 的逐字节比对无从判定；此时退回父目录名作前缀，保持两者的区分且可读。
//   - **opencode 的配置根是目录**（2026-09-22 真机实测）：它只读
//     `$XDG_CONFIG_HOME/opencode/opencode.json`，而 `OPENCODE_CONFIG` **不替代**配置根
//     （设了它，opencode 仍会去读 `$XDG_CONFIG_HOME/opencode/*`——那不是隔离运行）。
//     因此它的物化名要多一层 `opencode/`，命令才能用 `XDG_CONFIG_HOME=<dir>` 指目录。
func materializedNames(id family.ID, plans []family.FilePlan) map[string]string {
	paths := make([]string, 0, len(plans))
	for _, plan := range plans {
		if plan.Delete {
			continue
		}
		paths = append(paths, plan.Path)
	}
	byBase := map[string]int{}
	for _, p := range paths {
		byBase[filepath.Base(p)]++
	}
	out := make(map[string]string, len(paths))
	for _, p := range paths {
		base := filepath.Base(p)
		switch {
		case byBase[base] > 1:
			out[p] = filepath.ToSlash(filepath.Join(filepath.Base(filepath.Dir(p)), base))
		case id == family.OpenCode:
			out[p] = filepath.ToSlash(filepath.Join(opencodeConfigDirName, base))
		default:
			out[p] = base
		}
	}
	return out
}

// writeMaterializedFile 写一个物化文件；目标字节与盘上相同即**不触盘**（A5 的幂等：
// 重复物化不重写、mtime 不变）。
func writeMaterializedFile(path string, blob []byte) error {
	if old, err := os.ReadFile(path); err == nil && bytes.Equal(old, blob) {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return apierr.Wrap(apierr.CodeStorage, err, "建立物化目录失败: %s", filepath.Dir(path))
	}
	if err := os.WriteFile(path, blob, 0o600); err != nil {
		return apierr.Wrap(apierr.CodeStorage, err, "写入物化文件失败: %s", path)
	}
	return nil
}

// materializedDirContent 读回物化目录的内容、文件名与可读状态，供失败回退按盘上事实展示。
func materializedDirContent(dir string) (map[string][]byte, []string, bool) {
	out := map[string][]byte{}
	files := []string{}
	unreadable := false
	// WalkDir reports root/child traversal errors through the callback. Every
	// callback path records the error and returns nil, so there is no separate
	// traversal error after the walk.
	_ = filepath.WalkDir(dir, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			if path == dir && os.IsNotExist(err) {
				return nil
			}
			unreadable = true
			return nil
		}
		if entry == nil || entry.IsDir() {
			return nil
		}
		// WalkDir supplies every file below dir on the same volume, so Rel cannot
		// fail for this path pair.
		rel, _ := filepath.Rel(dir, path)
		files = append(files, filepath.ToSlash(rel))
		raw, err := os.ReadFile(path)
		if err != nil {
			unreadable = true
			return nil
		}
		out[path] = raw
		return nil
	})
	sort.Strings(files)
	return out, files, unreadable
}

// materializedFileNames 列出物化目录里的文件（相对路径，盘上事实；空目录与不存在的目录
// 都返回空切片——#27 定稿：从未物化时 `files` 是空数组、`dir` 仍给出预期路径）。
func materializedFileNames(dir string) []string {
	out := []string{}
	_ = filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		rel, relErr := filepath.Rel(dir, path)
		if relErr != nil {
			return nil
		}
		out = append(out, filepath.ToSlash(rel))
		return nil
	})
	sort.Strings(out)
	return out
}

// dataRootFiles 列出数据根下的全部常规文件（A4 的引用候选；物化根不在数据根内，互不干扰）。
func dataRootFiles(dataRoot string) []string {
	out := []string{}
	_ = filepath.WalkDir(dataRoot, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d == nil || d.IsDir() || !d.Type().IsRegular() {
			return nil
		}
		out = append(out, path)
		return nil
	})
	return out
}

// pruneMaterialized 把物化根收敛到「当前存在的卡片」：根下不再有对应单元的目录、
// 单元目录下不再有对应卡片的目录（A5：卡片被删除后其目录不留孤儿）。
//
// 只删**目录**，且只删 `expected` 里没有的名字。算不出来的卡片仍登记在自己的单元名下
// （`expected` 由枚举给出，与算成没算成无关），因此「这轮失败」不会把上一轮的内容删掉。
func pruneMaterialized(root string, expected map[string]map[string]bool) error {
	agents, err := os.ReadDir(root)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	var firstErr error
	fail := func(err error) {
		if err != nil && firstErr == nil {
			firstErr = err
		}
	}
	for _, agent := range agents {
		if !agent.IsDir() {
			continue
		}
		agentPath := filepath.Join(root, agent.Name())
		dirs, ok := expected[agent.Name()]
		if !ok {
			fail(os.RemoveAll(agentPath))
			continue
		}
		entries, readErr := os.ReadDir(agentPath)
		if readErr != nil {
			fail(readErr)
			continue
		}
		for _, entry := range entries {
			if !entry.IsDir() || dirs[entry.Name()] {
				continue
			}
			fail(os.RemoveAll(filepath.Join(agentPath, entry.Name())))
		}
	}
	return firstErr
}

// ── §A7 启动命令 ────────────────────────────────────────────────────────────

// launchSupport 是一个受管单元的启动口径。
//
// `Verified` 只对**真的在本机跑通过一次真实请求**的单元为真（A7）。没有 CLI 可以跑的
// 单元（桌面应用、未安装的 CLI、无独立 CLI 的 WorkBuddy）一律 false 并在 `Note` 里写明
// 原因——界面把 false 显示成明确的警告，而不是假装可用。
//
// 所有命令的共同前提是**本机 v2 在运行**（凭据由本地代理注入，目录里没有凭据副本）；
// 目录可对照、可整体搬迁，但搬走之后命令不成立——这一条写进了 UC-019 的 A4 与边界段。
type launchSupport struct {
	Verified bool
	Note     string
	command  func(s *Server, dir string, content map[string][]byte) string
}

// noCommand 是「该单元没有可指向物化目录的启动入口」。
func noCommand(*Server, string, map[string][]byte) string { return "" }

// launchSupportFor 给出该单元的启动口径与命令构造器。
//
// 命令里的配置根变量逐条都有来源，不凭猜（A7：不得编造变量名）：
//   - claude / codex / pi / hermes 取 acceptance-recipe §B.2 的 CLI 侧隔离变量表
//     （该表是仓库内「各 CLI 认哪个变量」的事实源）；
//   - opencode 用 `XDG_CONFIG_HOME`（同表：opencode 那一行写的就是
//     `XDG_CONFIG_HOME` + `XDG_DATA_HOME`）。**不再用 `OPENCODE_CONFIG`**：2026-09-22
//     真机复核证明它不替代配置根（设了它，opencode 仍会读 `$XDG_CONFIG_HOME/opencode/*`），
//     因此那条命令不构成隔离运行；
//   - openclaw 用 `OPENCLAW_CONFIG_PATH`（v1 审计记录里实测存在的覆盖变量）；
//   - gemini / grokbuild 在 §B.2 里被登记为「待定 / 未证实」，因此**不编造变量名**：
//     命令留空并如实登记未验证。
func launchSupportFor(id family.ID) launchSupport {
	switch id {
	case family.Claude:
		return launchSupport{
			Verified: true,
			Note: "已在本机实测（claude 2.1.275）：隔离 CLAUDE_CONFIG_DIR 里没有登录态，" +
				"CLI 在读到 settings.json 之前就被登录门挡下（Not logged in · Please run /login，" +
				"一个请求都不发），因此命令显式给出 settings.json 里那三个 route 键——" +
				"进程 env 会盖过 settings.json（本机装过 v1 时尤其如此，故不能靠继承）。",
			command: claudeLaunchCommand,
		}
	case family.Codex:
		return launchSupport{
			Verified: true,
			Note: "已在本机实测（codex-cli 0.154.0）：只给 CODEX_HOME 时请求直发网关，而物化目录" +
				"按 UC-019 非目标不放凭据副本 ⇒ 网关 401 Invalid token；把 base_url 覆盖到本地代理后跑通" +
				"（凭据由代理转发时注入，与 claude 的 route 形态同口径）。需要本地代理在跑。",
			command: codexLaunchCommand,
		}
	case family.Pi:
		return launchSupport{
			Verified: true,
			Note: "已在本机实测（pi 0.85.1）：本单元的形态本就写网关直连 + 真令牌（§A 行 9 的口径，" +
				"物化按 A3 逐字节照搬），因此命令直接跑通；这类请求**不经过**本产品代理，" +
				"用量面不会记到它。",
			command: fixedRootCommand("PI_CODING_AGENT_DIR", "pi"),
		}
	case family.OpenCode:
		return launchSupport{
			Verified: true,
			Note: "已在本机实测（opencode 1.18.32，2026-09-22，隔离实例 + 隔离配置根）：命令前缀原样加上 " +
				"`run …` 后 rc=0 并拿到应答，日志 `llm.provider=yzagmgr-local llm.model=deepseek-flash`、" +
				"`stream providerID=yzagmgr-local` ⇒ 请求真的走托管 provider；配置探测路径只有 " +
				"`<dir>/opencode/{config.json,opencode.json,opencode.jsonc}`，读真实 `~/.config/opencode` 的行数为 0。" +
				"**`OPENCODE_CONFIG` 已作废**：它不替代配置根（设了它，opencode 仍会读 " +
				"`$XDG_CONFIG_HOME/opencode/*`），那条命令不构成隔离运行。" +
				"命令刻意只设配置根（data/cache 继承用户自己的那份）：本次实测连它们一起隔离（含冷缓存）" +
				"也能跑通，早前记的「隔离 data/cache 会去抓 provider 包而挂死」未复现——不隔离更省事，" +
				"也不再需要预热缓存。",
			command: fixedRootCommand("XDG_CONFIG_HOME", "opencode"),
		}
	case family.OpenClaw:
		return launchSupport{
			Verified: false,
			Note: "本机未安装 openclaw，未验证；命令用 OPENCLAW_CONFIG_PATH 指向物化文件" +
				"（该变量在 v1 的审计记录里实测存在）。",
			command: configFileCommand("OPENCLAW_CONFIG_PATH", "openclaw.json", "openclaw"),
		}
	case family.Hermes:
		return launchSupport{
			Verified: false,
			Note:     "本机未安装 hermes CLI，未验证；命令按 acceptance-recipe §B.2 的 HERMES_HOME 给出。",
			command:  fixedRootCommand("HERMES_HOME", "hermes"),
		}
	case family.ClaudeDesktop:
		return launchSupport{
			Verified: false,
			Note: "该单元是桌面应用，没有可指向物化目录的 CLI 启动入口（配置根固定为 " +
				"~/Library/Application Support，无 env 覆盖）；目录仅供对照与整体搬迁，命令留空。",
			command: noCommand,
		}
	case family.Gemini, family.Grokbuild:
		return launchSupport{
			Verified: false,
			Note: "该 CLI 本机未安装，且隔离变量在 acceptance-recipe §B.2 里登记为「待定/未证实」；" +
				"不编造变量名，命令留空（目录仍可整体搬走，或自行按该 CLI 文档指向它）。",
			command: noCommand,
		}
	default:
		return launchSupport{
			Verified: false,
			Note:     "该受管单元没有登记的启动口径。",
			command:  noCommand,
		}
	}
}

// fixedRootCommand 是「一个配置根变量 + 可执行名」的通用命令（pi / hermes 这类单变量对齐）。
func fixedRootCommand(envVar, exe string) func(*Server, string, map[string][]byte) string {
	return func(_ *Server, dir string, _ map[string][]byte) string {
		return envVar + "=" + shellValue(dir) + " " + exe
	}
}

// configFileCommand 是「一个配置文件变量 + 物化目录内的文件名 + 可执行名」的通用命令。
//
// 只服务 openclaw：它的 `OPENCLAW_CONFIG_PATH` 是**文件级**变量（直接指文件），因此
// 扁平布局下无需中间层。opencode 曾经走这条路，2026-09-22 复核后改回目录级的
// `XDG_CONFIG_HOME`（它的配置根是目录，文件级变量不隔离配置根，见 opencodeConfigDirName）。
func configFileCommand(envVar, fileName, exe string) func(*Server, string, map[string][]byte) string {
	return func(_ *Server, dir string, _ map[string][]byte) string {
		return envVar + "=" + shellValue(filepath.Join(dir, fileName)) + " " + exe
	}
}

// claude 的两个 route 判定/凭据键（裸键名；`claudeModelKey` 在 modelmap.go）。
const (
	claudeKeyBaseURL   = "ANTHROPIC_BASE_URL"
	claudeKeyAuthToken = "ANTHROPIC_AUTH_TOKEN"
)

// claudeLaunchRouteKeys 是**物化启动命令**为 claude 显式给出的三个 route 键与发射顺序
// （UC-019：隔离配置目录不足以驱动真 CLI——真机实证 `Not logged in · Please run /login`，
// 故命令必须带全三键）。
var claudeLaunchRouteKeys = []string{claudeKeyBaseURL, claudeKeyAuthToken, claudeModelKey}

// claudeTestInjectRouteKeys 是 §4.6 配置测试 / §4.8 受控验证**进程 env 注入**的键子集：
// 只给「过登录门 + 指对上游」两键，**刻意不含 `ANTHROPIC_MODEL`**——§4.8 的唯一标记就写在
// 工作区 settings.json 的该键上，而进程 env 盖过 settings.json（launchNote 同一判据），
// 注入未标记的模型值会把标记遮掉、让观测关联失败；模型名留给 CLI 自己读 settings.json。
// **例外（R225③，不在本键表内）**：CC 宿主形态下 CLI 忽略 settings，由
// [`Server.profileTestEnvMarked`] 单独把**带标记**的档位值写进进程 env（带标记 = 不遮）。
var claudeTestInjectRouteKeys = []string{claudeKeyBaseURL, claudeKeyAuthToken}

// claudeRouteEnv 汇总内容里各文件 settings `env` 段的并集。
//
// 这是**物化启动命令与隔离测试 env 注入共用的同一事实源**：两处的判定与取值都从这里出，
// 不各读一份——否则改了写链而另一处没跟上，症状是命令/测试 env 把 CLI 指向别处（且不报错）。
// 非法 JSON 的条目跳过（与物化既有口径一致：读不出 env 就当没有 route 键）。
func claudeRouteEnv(content map[string][]byte) map[string]string {
	env := map[string]string{}
	for _, blob := range content {
		var doc struct {
			Env map[string]string `json:"env"`
		}
		if err := json.Unmarshal(blob, &doc); err != nil {
			continue
		}
		for k, v := range doc.Env {
			env[k] = v
		}
	}
	return env
}

// claudeRoutePairs 按 keys 的给定顺序挑出非空（TrimSpace 后）的键值对。
// 顺序与取值在两条消费面（物化命令、隔离 env 注入）逐字一致，杜绝两套口径漂移。
func claudeRoutePairs(env map[string]string, keys []string) [][2]string {
	pairs := make([][2]string, 0, len(keys))
	for _, key := range keys {
		if value := strings.TrimSpace(env[key]); value != "" {
			pairs = append(pairs, [2]string{key, value})
		}
	}
	return pairs
}

// claudeLaunchCommand 给出 claude 的启动命令。
//
// 三个 route 键的值**取自物化内容本身**（`settings.json` 的 `env` 段）而不是另抄常量：
// 「命令里写的」与「目录里写的」必须永远同源，否则改了写链而命令没跟上，症状是命令把 CLI
// 指向别处（而且不会有任何报错）。官方卡没有 `env` 段（`{"env":{}}`）⇒ 只给
// CLAUDE_CONFIG_DIR：那条路径走的是用户自己的上游登录态。
func materializedLaunchCommand(
	s *Server,
	id family.ID,
	card materializeCardRef,
	dir string,
	content map[string][]byte,
	unreadable bool,
	support launchSupport,
) string {
	if id == family.Claude {
		hasCredential, uncertain := claudeMaterializedCredentialState(content)
		if card.EndpointIsFullBase || hasCredential || uncertain || unreadable {
			return claudeFullBaseLaunchCommand(dir, content)
		}
	}
	return support.command(s, dir, content)
}

// claudeMaterializedCredentialState checks settings.json files in the supplied
// materialized content. A malformed settings file is uncertain: callers must choose
// the credential-safe launch path and report that credentials may be present.
func claudeMaterializedCredentialState(content map[string][]byte) (hasCredential, uncertain bool) {
	foundSettings := false
	for path, blob := range content {
		if filepath.Base(path) != "settings.json" {
			continue
		}
		foundSettings = true
		var doc struct {
			Env map[string]string `json:"env"`
		}
		if err := json.Unmarshal(blob, &doc); err != nil {
			uncertain = true
			continue
		}
		token := strings.TrimSpace(doc.Env[claudeKeyAuthToken])
		if token != "" && token != claude.TokenPlaceholder {
			hasCredential = true
		}
	}
	if !foundSettings {
		return false, false
	}
	return hasCredential, uncertain
}

func codexMaterializedCredentialState(content map[string][]byte) (hasCredential, uncertain bool) {
	foundConfig := false
	for path, blob := range content {
		if filepath.Base(path) != "config.toml" {
			continue
		}
		foundConfig = true
		doc, err := docmodel.ParseTOML(blob)
		if err != nil {
			uncertain = true
			continue
		}
		value := doc.GetKey("model_providers.custom", codex.ExperimentalBearerTokenKey)
		if value == nil {
			continue
		}
		token, ok := value.String()
		if !ok {
			uncertain = true
			continue
		}
		if strings.TrimSpace(token) != "" {
			hasCredential = true
		}
	}
	if !foundConfig {
		return false, false
	}
	return hasCredential, uncertain
}

func claudeDesktopMaterializedCredentialState(content map[string][]byte) (hasCredential, uncertain bool) {
	foundJSON := false
	for path, blob := range content {
		if strings.ToLower(filepath.Ext(path)) != ".json" {
			continue
		}
		foundJSON = true
		var profile map[string]any
		if err := json.Unmarshal(blob, &profile); err != nil {
			uncertain = true
			continue
		}
		baseURL, hasBaseURL := profile["inferenceGatewayBaseUrl"]
		baseURLText := ""
		if hasBaseURL {
			var ok bool
			baseURLText, ok = baseURL.(string)
			if !ok {
				uncertain = true
			} else if strings.TrimSpace(baseURLText) != "" && claudeDesktopURLMayContainCredential(baseURLText) {
				hasCredential = true
			}
		}
		value, hasKey := profile["inferenceGatewayApiKey"]
		if !hasKey {
			continue
		}
		token, ok := value.(string)
		if !ok {
			uncertain = true
			continue
		}
		token = strings.TrimSpace(token)
		if token == "" {
			continue
		}
		if !hasBaseURL || strings.TrimSpace(baseURLText) == "" {
			uncertain = true
			continue
		}
		if !claudeDesktopProxyCredential(baseURLText, token) {
			hasCredential = true
		}
	}
	if !foundJSON {
		return false, false
	}
	return hasCredential, uncertain
}

func claudeDesktopURLMayContainCredential(raw string) bool {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return true
	}
	return u.User != nil || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" ||
		u.RawFragment != "" || strings.Contains(raw, "#")
}

func claudeDesktopProxyCredential(baseURL, token string) bool {
	if !claudeDesktopProxyKey(token) {
		return false
	}
	u, err := url.Parse(strings.TrimSpace(baseURL))
	if err != nil || u.Scheme != "http" || u.Hostname() == "" || u.Port() == "" ||
		u.User != nil || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || u.RawFragment != "" ||
		strings.Contains(baseURL, "#") || u.Opaque != "" {
		return false
	}
	host := strings.ToLower(u.Hostname())
	loopback := host == "localhost"
	if ip := net.ParseIP(host); ip != nil && ip.IsLoopback() {
		loopback = true
	}
	if !loopback {
		return false
	}
	if u.EscapedPath() != u.Path {
		return false
	}
	return u.Path == claudedesktop.LocalRouteSuffix || u.Path == claudedesktop.RouterSuffix
}

func claudeDesktopProxyKey(token string) bool {
	if !strings.HasPrefix(token, "ccs-") {
		return false
	}
	uuid := strings.TrimPrefix(token, "ccs-")
	if len(uuid) != 36 {
		return false
	}
	for i, r := range uuid {
		switch i {
		case 8, 13, 18, 23:
			if r != '-' {
				return false
			}
		default:
			if !((r >= '0' && r <= '9') || (r >= 'a' && r <= 'f')) {
				return false
			}
		}
	}
	return uuid[14] == '4' && (uuid[19] == '8' || uuid[19] == '9' || uuid[19] == 'a' || uuid[19] == 'b')
}

// claudeFullBaseLaunchCommand points Claude Code at its materialized settings
// directory and file, and clears inherited provider variables. The endpoint
// and token stay inside settings.json instead of being copied into the command.
func claudeFullBaseLaunchCommand(dir string, content map[string][]byte) string {
	keys := map[string]bool{}
	for key := range claudeRouteEnv(content) {
		keys[key] = true
	}
	for _, key := range claudeLaunchRouteKeys {
		keys[key] = true
	}
	// These environment variables can select or authenticate through Bedrock,
	// Vertex, or another Anthropic endpoint ahead of the materialized settings.
	for _, key := range []string{
		"ANTHROPIC_API_KEY",
		"ANTHROPIC_BEDROCK_BASE_URL",
		"ANTHROPIC_VERTEX_BASE_URL",
		"CLAUDE_CODE_USE_BEDROCK",
		"CLAUDE_CODE_USE_VERTEX",
		"CLAUDE_CODE_SKIP_BEDROCK_AUTH",
		"CLAUDE_CODE_SKIP_VERTEX_AUTH",
	} {
		keys[key] = true
	}
	orderedKeys := make([]string, 0, len(keys))
	for key := range keys {
		orderedKeys = append(orderedKeys, key)
	}
	sort.Strings(orderedKeys)
	parts := []string{"env"}
	for _, key := range orderedKeys {
		parts = append(parts, "-u", shellValue(key))
	}
	settingsPath := filepath.Join(dir, "settings.json")
	parts = append(parts,
		"CLAUDE_CONFIG_DIR="+shellValue(dir),
		"claude", "--settings", shellValue(settingsPath),
	)
	return strings.Join(parts, " ")
}

func claudeLaunchCommand(_ *Server, dir string, content map[string][]byte) string {
	parts := []string{"CLAUDE_CONFIG_DIR=" + shellValue(dir)}
	for _, pair := range claudeRoutePairs(claudeRouteEnv(content), claudeLaunchRouteKeys) {
		parts = append(parts, pair[0]+"="+shellValue(pair[1]))
	}
	parts = append(parts, "claude")
	return strings.Join(parts, " ")
}

// codexProxyBase 给出 codex **网关档**把 base_url 指到本地代理时的 OpenAI 兼容基址。
// 普通网关卡的 `[model_providers.custom].requires_openai_auth=true` 由本地代理注入网关
// 凭据；显式 full-base 直连卡写 `false` 并带自己的 provider credential，必须保留原 endpoint。
// 官方卡没有 `[model_providers.custom]` 段，也返回空串。
//
// **物化启动命令（[`codexLaunchCommand`]）与 §4.6 配置测试（codexProxyBaseOverrideArgs）
// 共用本函数**：两处的覆盖判定与取值必须永远同源，否则改了一处另一处没跟上，症状是
// 命令/测试把 codex 指向别处（而且不会有任何报错）。
func codexProxyBase(s *Server, content map[string][]byte) string {
	for _, blob := range content {
		if !bytes.Contains(blob, []byte("model_provider = \""+codexProviderCustom+"\"")) {
			continue
		}
		if doc, err := docmodel.ParseTOML(blob); err == nil {
			if requiresAuth, ok := doc.GetKey("model_providers."+codexProviderCustom, "requires_openai_auth").Bool(); ok && !requiresAuth {
				return "" // direct provider owns its endpoint and credential
			}
		}
		return s.localRouteBase() + codex.OpenAICompatSuffix
	}
	return ""
}

// codexLaunchCommand 给出 codex 的启动命令。
//
// 网关档的 config.toml 需要本地代理注入网关令牌，所以命令用 codex 自己的 `-c` 覆盖到
// 本地代理。用户 full-base provider 卡带自己的 endpoint 与凭据，不加覆盖；官方卡没有
// `[model_providers.custom]` 段，也不加覆盖。
func codexLaunchCommand(s *Server, dir string, content map[string][]byte) string {
	command := "CODEX_HOME=" + shellValue(dir) + " codex"
	base := codexProxyBase(s, content)
	if base == "" {
		return command
	}
	return command + " -c model_providers." + codexProviderCustom + ".base_url=" + shellValue(base)
}

// shellValue 仅让窄白名单字符不加引号，其余值使用 POSIX 单引号和标准单引号转义。
// 这样路径和配置值都不会被 shell 当成命令替换、控制符或额外参数。
func shellValue(value string) string {
	if value != "" {
		safe := true
		for _, r := range value {
			if !((r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') ||
				r == '_' || strings.ContainsRune("./:=+-", r)) {
				safe = false
				break
			}
		}
		if safe {
			return value
		}
	}
	return "'" + strings.ReplaceAll(value, "'", `'\''`) + "'"
}
