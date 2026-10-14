// §4.6 配置测试（UC-039 的「测试」入口）：POST /tools/{id}/profiles/{pid}/test。
//
// 语义一句话：把该卡「启用时会写的配置」**真实写进一个 0700 临时根**，用对应的 CLI
// 在那个根里发一条最小请求，然后如实汇报结果。它回答的是配置内容本身回答不了的问题——
// 「这份配置 CLI 认不认、上游收不收」。
//
// 六条硬约束（UC-039 契约 + UC-097 边界）：
//  1. **永不写 live**：计划字节只落临时根，磁盘上的 live 配置在测试前后逐字节不变
//     （UC-097 A3 起核对面扩到认证文件/会话文件/工具用户会话目录）；
//     临时根无论成败都删（defer），不留残留目录。
//  2. **只测装了的工具**：先过 `/tools/{id}` 的安装探测来源（受管文件 / 可执行副本），
//     找不到可执行文件就 `not_supported` 并说明原因——界面上按钮据此是禁用的；
//     前置失败分类独立机器可读（not_installed / not_executable / no_recipe /
//     gui_family，UC-097 A6），**绝不**顺手要系统权限（「不建议在自动化中给系统权限」）。
//  3. **同工具串行**：同一工具上已经有一次测试在跑时，第二个请求得 `test_in_progress`
//     （不是排队、也不是并发跑两份满负载 CLI）。不同工具之间互不影响。
//  4. **有界、可停止**：单次 90s 上限，超时与停止（UC-097 A4 的 cancel 入口）都按
//     **进程组**回收（CLI 可能拉子进程；只 kill 直接子进程会留下孤儿继续持着管道与网络）。
//     停止的终态如实标 `cancelled`——不谎称回滚、也不谎称成功。
//  5. **响应与日志无凭据**：`output` 先过 [`maskCredentials`] 再截断（先脱敏后截断的顺序
//     是安全要求：先截断会把令牌截成「不再匹配令牌形态」的半截串而漏掩）。
//  6. **费用如实**（UC-097 A8）：未获得实际费用时给未知而非 0——`cost` 段显式给出
//     费用来源与未知项清单（与实际费用对不上账的字段，宁缺毋猜）。
package api

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"time"

	"github.com/peterplaykkk/yzagmgr/internal/apierr"
	"github.com/peterplaykkk/yzagmgr/internal/family"
)

// profileTestTimeout 是单次测试的墙钟上限（UC-039 契约：90s；UI 侧按 100s 处理）。
const profileTestTimeout = 90 * time.Second

// profileTestPrompt is the baseline last argument in each supported recipe;
// the saved-card route replaces it with a fresh answer challenge per run.
const profileTestPrompt = "只回复两个字：正常"

const profileTestAnswerChallengePrefix = "YZAGMGR_CHALLENGE_"

func newProfileTestAnswerChallenge() (expected, prompt string) {
	var nonce [16]byte
	// Go 1.25 crypto/rand.Read always fills the buffer or terminates the process;
	// an error return here is unreachable and cannot be reported to the API caller.
	rand.Read(nonce[:])
	expected = profileTestAnswerChallengePrefix + hex.EncodeToString(nonce[:])
	prompt = "Reply with exactly " + expected + " and nothing else."
	return expected, prompt
}

func profileTestRecipeWithPrompt(recipe profileTestRecipe, prompt string) profileTestRecipe {
	if len(recipe.args) == 0 {
		return recipe
	}
	cloned := recipe
	cloned.args = append([]string(nil), recipe.args...)
	cloned.args[len(cloned.args)-1] = prompt
	return cloned
}

func claudeProfileTestArgs(prompt string) []string {
	return []string{
		"-p", "--output-format", "json",
		"--tools", "",
		"--disallowedTools", "mcp__*",
		"--max-turns", "1",
		"--no-session-persistence",
		"--settings", `{"disableAllHooks":true}`,
		prompt,
	}
}

// profileTestOutputLimit 是 `output` 摘要的字符上限（UC-039 契约：≤400 字）。
const profileTestOutputLimit = 400

// profileTestCancelWait 是停止入口等待「运行落定终态」的上限。
//
// 正常路径远小于它：ctx 取消 → cmd.Cancel 按进程组 TERM → 宽限 300ms → KILL →
// `WaitDelay` 再给管道 2s ⇒ 约 2.5s 内必然落定。取宽一档只为兜住极端调度/桩实现；
// 到点仍未落定时如实回答「信号已发出、尚未落定」，不假装已终止。
const profileTestCancelWait = 10 * time.Second

// 受控运行的种类（同工具同时至多一个；§4.6 测试与 §4.8 受控验证共用同一把闸门）。
const (
	controlledRunTest   = "test"
	controlledRunVerify = "verify"
)

// 测试前置被拒的机器可读原因（封闭取值；web 据此渲染文案，不从中文文案猜语义）。
const (
	// profileTestReasonNotInstalled：本机没有该 CLI 的可执行文件。
	profileTestReasonNotInstalled = "not_installed"
	// profileTestReasonNotExecutable：可执行文件在场但没有执行权限（A6 独立分类；
	// 解释文案带「不建议在自动化中给系统权限」口径）。
	profileTestReasonNotExecutable = "not_executable"
	// profileTestReasonNoRecipe：该家族没有已实证的 CLI 测试配方（不猜命令行；
	// WorkBuddy 这类无命令行入口的单元同归此类）。
	profileTestReasonNoRecipe = "no_recipe"
	// profileTestReasonGuiFamily：图形应用（没有命令行测试入口，不是「缺配方」）。
	profileTestReasonGuiFamily = "gui_family"
)

// 费用面词表（A8 additive；`quote` 与 §4.8 estimate.quote 同一口径）。
const (
	profileTestCostQuoteUnknown = "unknown"
	// 未知项（封闭取值）：未知明示，不伪装免费。
	profileTestCostUnknownActualCost   = "actual_cost"
	profileTestCostUnknownActualTokens = "actual_tokens"
	profileTestCostUnknownUnitPrice    = "unit_price"
)

// profileTestGuiFamilies 是没有命令行测试入口的图形应用。
//
// 「GUI 不做 CLI 测试」是产品边界（A6 的 gui_family），与「配方未实证」（no_recipe）
// 是两条解释：前者不该引导用户去装 CLI，后者说明我们不猜命令行。
var profileTestGuiFamilies = map[family.ID]bool{
	family.ClaudeDesktop: true,
}

// profileTestDirPrefix 是临时隔离根的目录名前缀（`os.MkdirTemp` 的 pattern）。
// 它同时是「哪些目录属于本功能」的唯一判据——清理崩溃残留时**只**认这个前缀。
const profileTestDirPrefix = "yzagmgr-config-test-"

// profileTestStaleAfter 是「崩溃残留」的年龄阈值：超过它的临时根在下一次测试时被清掉。
//
// 取 1 小时（而不是「比 90s 大一点」）：正常一次测试最长 90s，任何超过它一个数量级的
// 目录都只可能是崩溃残留；阈值取得宽一点是为了绝不去碰「另一个还活着的实例手上的目录」。
const profileTestStaleAfter = time.Hour

// profileTestMaxCapture 是单条流的捕获上限。//
// 存在的理由是有界性：测试跑的是一个不受我们控制的 CLI，它的 stdout 可能被刷屏
// （`--mode json` 的流式事件流也可能很长）。到上限后**丢弃后续并标记**，而不是无限
// 增长把守护进程的内存吃掉；截断点会回退到最后一个换行（见 cappedStream.String）。
const profileTestMaxCapture = 256 << 10

// profileTestRecipe 是一族的测试配方。
//
// 只列**已实证**的四条 CLI（UC-039 的「测试配方」节）：其余家族（含两个桌面应用、
// 无 CLI 的 WorkBuddy）一律 not_supported 并给出原因，不猜命令行。
type profileTestRecipe struct {
	// cli 是可执行**名**（也是响应里的 `cli` 字段；真实路径另经 [`resolveProfileTestCLI`] 解析，
	// 因为 app 包启动的守护进程 PATH 往往不含 `~/.local/bin`）。
	cli string
	// args 是最小请求参数（可执行名之外的全部参数）。
	args []string
	// envVar / envRel 是该族 CLI 认的**配置根变量**与其在假 HOME 下的相对位置
	// （来源：acceptance-recipe §B.2「CLI 侧隔离 env」表；§4.6 的契约钉的是 HOME 隔离，
	// 这里再补上各族自己的根变量，让「CLI 真的去读临时根」而不是回落用户真实配置）。
	envVar string
	envRel string
	// extraArgs 是执行前按**运行时事实**（计划内容、本实例代理端口）追加的参数构造器
	// （nil = 该族没有）。追加位置固定在末位的最小请求（prompt）**之前**，最小请求因此
	// 恒在末位；Codex 在这里接收隔离执行的配置覆盖（[`codexProxyBaseOverrideArgs`]）。
	extraArgs func(s *Server, content map[string][]byte) []string
}

// profileTestRecipes 是支持集（恰四条 CLI）。
var profileTestRecipes = map[family.ID]profileTestRecipe{
	family.Claude: {
		cli:    "claude",
		args:   claudeProfileTestArgs(profileTestPrompt),
		envVar: "CLAUDE_CONFIG_DIR",
		envRel: ".claude",
	},
	family.Codex: {
		cli: "codex",
		// `--skip-git-repo-check`（与产品自家 verify 脚本同口径：scripts/verify/cli-roundtrip.sh
		// 的 codex exec 带此旗标）：测试的 cmd.Dir 是临时根
		// （非 git 仓库），隔离 CODEX_HOME 又没有 [projects] 信任条目，codex 0.156 的信任门
		// 会在**发请求前**以「Not inside a trusted directory and --skip-git-repo-check was not
		// specified」拒绝（发布门禁实证：reports/evidence-releasegate-20260928/codex-profiletest*.json）。
		// 这是对最小请求的忠实复现——被测对象是配置，不是宿主目录形态。
		args:   []string{"exec", "--skip-git-repo-check", "--json", profileTestPrompt},
		envVar: "CODEX_HOME",
		envRel: ".codex",
		// 网关档的 base_url 覆盖到本地代理（与物化启动命令同源，见 codexProxyBase）：
		// 直连需要 CODEX_HOME/auth.json 的凭据，而隔离测试根按 UC-019 没有凭据副本 ⇒
		// 只补旗标仍会 401（真机实证，reports/evidence-codexfix-20260928/）。
		extraArgs: codexProxyBaseOverrideArgs,
	},
	family.OpenCode: {
		cli:    "opencode",
		args:   []string{"run", "--format", "json", profileTestPrompt},
		envVar: "XDG_CONFIG_HOME",
		envRel: ".config",
	},
	family.Pi: {
		cli: "pi",
		// 2026-09-25 实测配方：`--mode json` 的事件流里 `message_end` 的 assistant 消息
		// 带 `model`/`provider`（响应里的这两个字段就是从那里解析的）。
		args:   []string{"-p", "--no-session", "--no-tools", "--mode", "json", profileTestPrompt},
		envVar: "PI_CODING_AGENT_DIR",
		envRel: ".pi/agent",
	},
}

// codexProxyBaseOverrideArgs 是 codex §4.6 的运行时参数补充（配方 extraArgs）：
// 关闭可选分析、启动更新检查并使用内存凭据存储；网关档再把 base_url 覆盖到本地代理，
// 凭据由代理转发时注入——与物化启动命令 [`codexLaunchCommand`] **同一事实源**。
func codexProxyBaseOverrideArgs(s *Server, content map[string][]byte) []string {
	args := []string{
		"-c", "analytics.enabled=false",
		"-c", "check_for_update_on_startup=false",
		"-c", "cli_auth_credentials_store=ephemeral",
	}
	base := codexProxyBase(s, content)
	if base != "" {
		args = append(args, "-c", "model_providers."+codexProviderCustom+".base_url="+base)
	}
	return args
}

// profileTestArgs 给出一次测试真正发出的命令行：配方静态参数 + 族级运行时补充，
// 补充插在末位最小请求（prompt）**之前**（codex 的 `-c` 因此落在 `exec` 与 prompt 之间，
// 与 verify 脚本「flags 在 prompt 前」的形态一致）。没有 extraArgs 的族逐字节原样返回。
func (s *Server) profileTestArgs(recipe profileTestRecipe, content map[string][]byte) []string {
	if recipe.extraArgs == nil || len(recipe.args) == 0 {
		return recipe.args
	}
	extra := recipe.extraArgs(s, content)
	if len(extra) == 0 {
		return recipe.args
	}
	out := make([]string, 0, len(recipe.args)+len(extra))
	out = append(out, recipe.args[:len(recipe.args)-1]...)
	out = append(out, extra...)
	out = append(out, recipe.args[len(recipe.args)-1])
	return out
}

// profileTestPayload 是契约 §4.6 的响应形状。
//
// `status` 取 ok|failed|cancelled（`cancelled` 是 UC-097 A4 additive：被停止入口终止）；
// `reason` 恒给一句**可读短文**（ok 时说清依据，failed/cancelled 时说清发生了什么）。
// `model` / `provider` 尽力解析（pi 从事件流解析；解析不到留空，不编）。
type profileTestPayload struct {
	Status     string `json:"status"`
	Reason     string `json:"reason"`
	ExitCode   int    `json:"exitCode"`
	DurationMs int64  `json:"durationMs"`
	Model      string `json:"model"`
	Provider   string `json:"provider"`
	CLI        string `json:"cli"`
	TimedOut   bool   `json:"timedOut"`
	Output     string `json:"output"`
	// Cancelled 报告这次测试是否被停止入口（POST .../test/cancel）终止（A4 additive）。
	// true 时 `status` 恒为 `cancelled`；「超时」与「被停止」是两条不同的如实陈述。
	Cancelled bool `json:"cancelled"`
	// Cost 是费用面（A8 additive）：未获得实际费用时给未知而非 0（见 [`profileTestCost`]）。
	Cost profileTestCost `json:"cost"`
}

// profileTestCost 是测试结果的费用面（UC-097 A8 additive，2026-09-27）。
//
// 两条纪律（与 §4.8 受控验证的 estimate、§8 用量 Summary 同一条）：
//   - **未获得实际费用时给未知而非 0**：金额 `null` + `quote:"unknown"`，绝不写 0 冒充免费；
//   - 未知要**明示**：`source` 一句可读来源/原因，`unknowns` 逐项列出不知道的东西（封闭词表），
//     单价可读时 `unitPrice` 给出结构化材料。
//
// 为什么 §4.6 的实际费用恒未知：把「这一次测试花了多少」对到用量记录上需要唯一标记
// （§4.8 的标记归因是那条通道的方法）；§4.6 的请求没有标记，归因不出「这条记录就是
// 本次测试」，因此不猜、不折。能拿到的是目标模型登记单价（有则 `unitPrice` + `currency`）。
type profileTestCost struct {
	// Quote：known | unknown（实际费用是否已知；本通道恒 unknown）。
	Quote string `json:"quote"`
	// Source 是**可读的来源/原因**：金额从哪来、或为什么未知（来源行文案）。
	Source string `json:"source"`
	// Currency 是币种（单价已登记时给出；未知时缺席——不伪造）。
	Currency string `json:"currency,omitempty"`
	// CostUSD 是本次测试的实际费用：恒 null = 未知（不写 0；明细见 unknowns 与 source）。
	CostUSD *float64 `json:"costUsd"`
	// UnitPrice 是目标模型的登记单价（可读费用材料；未知时 null）。
	UnitPrice *profileTestUnitPrice `json:"unitPrice"`
	// Unknowns 是显式未知项（封闭词表：actual_cost | actual_tokens | unit_price）。
	Unknowns []string `json:"unknowns"`
}

// profileTestUnitPrice 是目标模型的登记单价（每百万 token；单位随登记币种）。
type profileTestUnitPrice struct {
	InputPerMillion  float64 `json:"inputPerMillion"`
	OutputPerMillion float64 `json:"outputPerMillion"`
	Currency         string  `json:"currency"`
}

// profileTestExecRequest / Result 是进程执行面的边界（唯一的注入缝）。
//
// 之所以要有这道缝：单元测试**绝不跑子进程**（仓库纪律，见 deps.go 的 StartBackground 注释），
// 而本端点的核心正是跑子进程；测试用可替换的实现观察「请求了什么、临时根里有什么」。
type profileTestExecRequest struct {
	Root       string
	Exe        string
	Args       []string
	Env        []string
	Redactions []string
}

type profileTestExecResult struct {
	Stdout   string
	Stderr   string
	ExitCode int
	TimedOut bool
	Err      error
	// PID 是被执行进程的进程号（0 = 不可得，例如测试桩或进程未能启动）。
	// §4.8 的受控验证把「该工具实例/进程身份」记进证据，因此这里如实带上。
	PID int
}

type profileDraftPlanExecutor func(
	ctx context.Context,
	id family.ID,
	recipe profileTestRecipe,
	exe string,
	plans []family.FilePlan,
	mayDispatch func() bool,
	redactions []string,
) (profileTestPayload, profileTestExecResult, bool, error)

// handleProfileTest 是 `POST /api/v1/tools/{id}/profiles/{pid}/test`。
func (s *Server) handleProfileTest(r *http.Request) (any, string, error) {
	id, err := s.requireFamily(r.PathValue("id"))
	if err != nil {
		return nil, "", err
	}
	pid := r.PathValue("pid")
	recipe, ok := profileTestRecipes[id]
	if !ok {
		// 两类「没有配方」分开：GUI 应用是产品边界（不是缺配方），其余是没有已实证的
		// CLI 配方（不猜命令行）。机器可读的 reason 供 web 做双语文案（A6 additive）。
		if profileTestGuiFamilies[id] {
			return nil, "", apierr.New(apierr.CodeNotSupported,
				"%s 是图形应用（GUI）：没有命令行测试入口，不支持配置测试；请在应用内确认配置可用性", id).
				WithDetails(map[string]any{"toolId": string(id), "reason": profileTestReasonGuiFamily})
		}
		message := string(id) + " 没有已实证的 CLI 测试配方（不猜命令行）：不支持配置测试"
		if id == family.WorkBuddy {
			// WorkBuddy 不是「配方待补」：它没有命令行入口，配置走独立写链。
			message = string(id) + " 没有命令行入口（配置走独立写链 /workbuddy/*）：不支持配置测试"
		}
		return nil, "", apierr.New(apierr.CodeNotSupported, "%s", message).
			WithDetails(map[string]any{"toolId": string(id), "reason": profileTestReasonNoRecipe})
	}
	exe := s.resolveTestCLI(id, recipe.cli)
	if exe == "" {
		view, verr := s.toolViewFor(id)
		installed := verr == nil && view.Installed
		reason := "本机未安装 " + recipe.cli + "（PATH 与常见 bin 目录里都没有可执行文件）"
		if installed {
			// 受管文件在场但可执行找不到：这是「装了但命令不在可执行搜索面里」，
			// 措辞必须与「没装」分开，否则用户会去重装一个已经装好的工具。
			reason = "探测到 " + string(id) + " 的配置在场，但找不到 " + recipe.cli +
				" 可执行文件（PATH 与常见 bin 目录均未命中）"
		}
		return nil, "", apierr.New(apierr.CodeNotSupported, "%s", reason).
			WithDetails(map[string]any{"toolId": string(id), "cli": recipe.cli, "reason": profileTestReasonNotInstalled})
	}
	// A6 前置分类（2026-09-27 additive）：可执行文件在场但没有执行权限是**独立**的
	// 失败类——不是没装、不是缺配方；文案明示「不建议在自动化中给系统权限」并把
	// 手动修复路径说清（权限问题不该由自动化在系统层面解决）。
	if !profileTestExecutableFile(exe) {
		return nil, "", apierr.New(apierr.CodeNotSupported,
			"找到 %s 可执行文件（%s）但没有执行权限：本测试不会以系统权限运行，"+
				"也不建议你在自动化中给系统权限——请在文件管理器中手动赋予执行权限（chmod +x），或重装该工具",
			recipe.cli, exe).
			WithDetails(map[string]any{
				"toolId": string(id), "cli": recipe.cli, "path": exe,
				"reason": profileTestReasonNotExecutable,
			})
	}
	// 并发闸门放在**最前面**（在任何计算与磁盘操作之前）：第二个请求应当廉价地被拒。
	// 2026-09-27 起闸门同时登记「停止入口的把手」（cancel 端点据此找到在飞运行）。
	runCtx, cancelRun := context.WithCancel(r.Context())
	defer cancelRun()
	run := s.startControlledRun(id, controlledRunTest, pid, cancelRun)
	if run == nil {
		return nil, "", apierr.New(apierr.CodeTestInProgress,
			"%s 上已有一次配置测试在跑，等它结束后再试", id).
			WithDetails(map[string]any{"toolId": string(id)})
	}
	// 兜底释放（幂等）：早退路径（测试没能开始）也要让等待中的停止入口立即看到「已结束」。
	defer s.finishControlledRun(run)

	payload, runErr := s.runProfileTest(runCtx, id, pid, recipe, exe, run)
	if runErr != nil {
		// 「没能开始」（计划算不出来、临时根建不起来）不是「测出失败」：按契约错误码
		// 如实上报，操作日志也据此记码，而不是把它折成一条 status:failed 的成功响应。
		return nil, "", runErr
	}
	if payload.Status != profileTestStatusOK {
		// 响应按成功信封给出完整结果（契约把 `status:"failed"`/`"cancelled"` 放在载荷里），
		// 但操作日志沿用「非 ok 即 failed」的封闭结果集如实记 failed（cancelled 的区分在
		// 响应载荷里；错误码与失败测试同口径，见 doc §4.6「停止入口」段）。
		return payload, "", &loggedFailure{data: payload, code: apierr.CodeInternal}
	}
	return payload, "", nil
}

// 测试状态（契约 §4.6 的封闭取值；`cancelled` 是 UC-097 A4 additive）。
const (
	profileTestStatusOK        = "ok"
	profileTestStatusFailed    = "failed"
	profileTestStatusCancelled = "cancelled"
)

// profileTestExecutableFile 报告解析出的路径是否可用作可执行面（A6 前置分类的判据）。
//
// 三条语义刻意区分：
//   - 文件存在但没有任何执行位（mode&0o111 == 0）⇒ false（not_executable）；
//   - 不存在（或 stat 失败）⇒ true（**不是**「没有执行权限」）——解析面在生产上只返回
//     存在路径（LookPath / 探测副本 / 目录扫描都过存在判定），测试缝里注入的假路径不
//     存在也不该被误分类成权限问题；「没装」由解析面返回空串表达；
//   - 目录 ⇒ true（由执行面如实报错，前置分类不替它下结论；解析面本来就跳过目录）。
func profileTestExecutableFile(path string) bool {
	st, err := os.Stat(path)
	if err != nil {
		return true
	}
	if st.IsDir() {
		return true
	}
	return st.Mode().Perm()&0o111 != 0
}

// runProfileTest 跑完一次测试：建临时根 → 写计划 → 调 CLI → 清理 → 汇总。
//
// 返回值语义刻意分两层：
//   - `error` 非 nil = 这次测试**没能开始**（计划算不出来、临时根建不起来），调用方按
//     契约错误码上报；
//   - `error` 为 nil = 测试**跑完了**，结论在 payload 的 status 里（ok / failed / cancelled）。
//
// 成败都清理临时根：它装着**带真凭据的配置副本**（形态内嵌令牌的族会把令牌原样写上），
// 留下来就是一处没有任何清理者的凭据落点。
//
// 终态由 [`Server.settleProfileTestOutcome`] 在锁内落定（停止请求与终态判定原子发生）。
func (s *Server) runProfileTest(
	ctx context.Context, id family.ID, pid string, recipe profileTestRecipe, exe string, run *controlledRun,
) (profileTestPayload, error) {
	expected, prompt := newProfileTestAnswerChallenge()
	recipe = profileTestRecipeWithPrompt(recipe, prompt)
	plans, err := s.plansForCard(ctx, id, pid)
	if err != nil {
		return profileTestPayload{}, err
	}
	payload, result, _, err := s.executeProfileTestPlans(ctx, id, recipe, exe, plans, func() bool {
		return s.markControlledRunDispatched(run)
	}, nil)
	if err != nil {
		return profileTestPayload{}, err
	}
	payload.Model = maskProfileTestOutput(payload.Model, []string{expected})
	payload.Provider = maskProfileTestOutput(payload.Provider, []string{expected})
	// The legacy result is a verification outcome, not a supplier transcript.
	// Keep the UI field useful while never returning raw provider text or the
	// per-run challenge in the response or operation snapshot.
	payload.Output = "原始输出已脱敏并隐藏"
	s.settleProfileTestOutcome(run, &payload, result, expected)
	// Process logs contain structured outcomes only. Output may contain user
	// content and credentials and is deliberately excluded.
	s.log.Info("配置测试结束", "tool", string(id), "profileId", pid, "cli", recipe.cli,
		"status", payload.Status, "exitCode", payload.ExitCode, "cancelled", payload.Cancelled,
		"timedOut", payload.TimedOut, "durationMs", payload.DurationMs)
	return payload, nil
}

// executeProfileTestPlans runs the existing CLI recipe against an already
// normalized plan. It owns the 0700 temp tree and cleanup; dispatch is the
// caller's atomic cancellation boundary.
func (s *Server) executeProfileTestPlans(
	ctx context.Context,
	id family.ID,
	recipe profileTestRecipe,
	exe string,
	plans []family.FilePlan,
	mayDispatch func() bool,
	redactions []string,
) (profileTestPayload, profileTestExecResult, bool, error) {
	return s.executeProfileTestPlansWithTempOps(
		ctx, id, recipe, exe, plans, mayDispatch, redactions, s.defaultProfileTestTempRootOps())
}

type profileTestTempRootOps struct {
	MkdirTemp          func(string, string) (string, error)
	Chmod              func(string, os.FileMode) error
	RemoveAll          func(string) error
	PreparePrivateDirs func(string, profileTestRecipe) error
	WriteConfigTree    func(string, []family.FilePlan) error
}

func (s *Server) defaultProfileTestTempRootOps() profileTestTempRootOps {
	return profileTestTempRootOps{
		MkdirTemp:          os.MkdirTemp,
		Chmod:              os.Chmod,
		RemoveAll:          os.RemoveAll,
		PreparePrivateDirs: prepareProfileTestPrivateDirs,
		WriteConfigTree:    s.writeTempConfigTree,
	}
}

// prepareProfileTestPrivateDirs creates every directory a supported CLI may use for config,
// state, cache, sockets, or temporary files before the planned config tree is written.
func prepareProfileTestPrivateDirs(root string, recipe profileTestRecipe) error {
	return prepareProfileTestPrivateDirsWithOps(root, recipe, os.MkdirAll, os.Chmod)
}

func prepareProfileTestPrivateDirsWithOps(
	root string,
	recipe profileTestRecipe,
	mkdirAll func(string, os.FileMode) error,
	chmod func(string, os.FileMode) error,
) error {
	if strings.TrimSpace(root) == "" {
		return errors.New("private root is empty")
	}
	dirs := []string{
		filepath.Join(root, ".config"), filepath.Join(root, ".config.d"),
		filepath.Join(root, ".local"), filepath.Join(root, ".local", "share"),
		filepath.Join(root, ".local", "share.d"), filepath.Join(root, ".local", "state"),
		filepath.Join(root, ".cache"), filepath.Join(root, ".runtime"), filepath.Join(root, ".tmp"),
		filepath.Join(root, "AppData"), filepath.Join(root, "AppData", "Roaming"),
		filepath.Join(root, "AppData", "Local"),
	}
	if strings.TrimSpace(recipe.envRel) != "" {
		familyRoot := filepath.Join(root, filepath.FromSlash(recipe.envRel))
		relative, err := filepath.Rel(root, familyRoot)
		if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
			return errors.New("family config root escapes private root")
		}
		dirs = append(dirs, familyRoot)
	}
	for _, dir := range dirs {
		if err := mkdirAll(dir, 0o700); err != nil {
			return err
		}
		if err := chmod(dir, 0o700); err != nil {
			return err
		}
	}
	return nil
}

func (s *Server) executeProfileTestPlansWithTempOps(
	ctx context.Context,
	id family.ID,
	recipe profileTestRecipe,
	exe string,
	plans []family.FilePlan,
	mayDispatch func() bool,
	redactions []string,
	ops profileTestTempRootOps,
) (profileTestPayload, profileTestExecResult, bool, error) {
	// 先扫一遍**崩溃残留**：临时根里装着带真凭据的配置副本，正常运行用后即删，
	// 但进程被强杀（关机/OOM/信号）时那一次 defer 不会执行。清理由「下一次测试」顺带做，
	// 而不是塞进启动路径——它是请求侧的事，且只在真的有人用这个功能时发生。
	s.sweepStaleProfileTestRoots()
	root, err := ops.MkdirTemp("", profileTestDirPrefix)
	if err != nil {
		return profileTestPayload{}, profileTestExecResult{}, false,
			apierr.Wrap(apierr.CodeStorage, err, "建立临时隔离根失败")
	}
	defer func() { _ = ops.RemoveAll(root) }()
	if merr := ops.Chmod(root, 0o700); merr != nil {
		return profileTestPayload{}, profileTestExecResult{}, false,
			apierr.Wrap(apierr.CodeStorage, merr, "设置临时根权限失败")
	}
	preparePrivateDirs := ops.PreparePrivateDirs
	if preparePrivateDirs == nil {
		preparePrivateDirs = prepareProfileTestPrivateDirs
	}
	if derr := preparePrivateDirs(root, recipe); derr != nil {
		return profileTestPayload{}, profileTestExecResult{}, false,
			apierr.Wrap(apierr.CodeStorage, derr, "建立 CLI 私有运行目录失败")
	}
	if werr := ops.WriteConfigTree(root, plans); werr != nil {
		return profileTestPayload{}, profileTestExecResult{}, false, werr
	}

	env := s.profileTestEnv(root, id, recipe, plans)
	outputRedactions := profileTestRedactionsWithEnvironment(redactions, env)
	exec0 := s.profileTestExecute
	if exec0 == nil {
		exec0 = execProfileTestCLI
	}
	payload := profileTestPayload{CLI: recipe.cli}
	if mayDispatch != nil && !mayDispatch() {
		return payload, profileTestExecResult{}, false, nil
	}
	started := time.Now()
	args := s.profileTestArgs(recipe, plannedContent(plans))
	result := exec0(ctx, profileTestExecRequest{
		Root: root, Exe: exe, Args: args, Env: env,
		Redactions: append([]string(nil), outputRedactions...),
	})
	payload.DurationMs = time.Since(started).Milliseconds()
	payload.ExitCode = result.ExitCode
	payload.TimedOut = result.TimedOut
	payload.Model, payload.Provider = parseModelFromCLIOutput(result.Stdout)
	payload.Model = maskProfileTestOutput(payload.Model, outputRedactions)
	payload.Provider = maskProfileTestOutput(payload.Provider, outputRedactions)
	result.Stdout = redactExactProfileTestValues(result.Stdout, outputRedactions)
	result.Stderr = redactExactProfileTestValues(result.Stderr, outputRedactions)
	payload.Output = summarizeProfileTestOutputWithRedactions(
		result.Stdout, result.Stderr, result.Err != nil || result.TimedOut || result.ExitCode != 0, outputRedactions)
	payload.Cost = s.profileTestCostFor(ctx, payload.Model)
	return payload, result, true, nil
}

// settleProfileTestOutcome 在锁内落定测试终态：读停止请求、按语义回写 status/reason、
// 存终态快照并关闭 done（幂等：只关一次）。
//
// 「停止优先」的规则说明（与超时/自然完成的并发语义，测试钉住）：
//   - cancelRequested = true 且终态尚未落定 ⇒ `cancelled`（用户显式停止优先于同时到达的
//     超时/退出；进程组在两种情形下都已按同一路径回收，区别只在如实陈述是哪一件事）；
//   - 落定先发生（cancel 端点随后到）⇒ 空操作「没有在飞运行」，结果保持 ok/failed；
//   - 客户端断开（r.Context 取消）不置 cancelRequested ⇒ 走既有 failed 语义
//     （不把「调用方中止」谎称成「用户点了停止」）。
func (s *Server) settleProfileTestOutcome(run *controlledRun, payload *profileTestPayload, result profileTestExecResult, expected string) {
	s.profileTestMu.Lock()
	defer s.profileTestMu.Unlock()
	switch {
	case run.cancelReq:
		payload.Status = profileTestStatusCancelled
		payload.Cancelled = true
		payload.Reason = "已按停止要求终止（按进程组回收；本次测试未完成，结果不作为通过或失败依据）"
	case !result.TimedOut && result.Err == nil && result.ExitCode == 0 &&
		validProfileTestAnswerFor(result.Stdout, payload.CLI, expected):
		payload.Status = profileTestStatusOK
		payload.Reason = "CLI 正常返回并取得本次挑战的有效回答"
	case result.TimedOut:
		payload.Status = profileTestStatusFailed
		payload.Reason = "超时未结束（上限 " + itoa(int(profileTestTimeout/time.Second)) + "s，已按进程组回收）"
	case result.Err == nil && result.ExitCode == 0:
		payload.Status = profileTestStatusFailed
		payload.Reason = "CLI 已退出，但没有返回本次挑战要求的有效回答"
	default:
		payload.Status = profileTestStatusFailed
		if result.ExitCode != 0 {
			payload.Reason = "CLI 退出码 " + itoa(result.ExitCode)
		} else {
			payload.Reason = "CLI 执行未成功完成"
		}
	}
	snapshot := *payload
	s.finishControlledRunLocked(run, &snapshot)
}

// profileTestCostFor 组装费用面（A8）：只读用量定价表（本地库，绝不发起网络请求）。
//
// 实际费用恒未知（没有唯一标记就归因不出「这条记录就是本次测试」，见 payload 注释）；
// 能拿到的是目标模型登记单价（有则 `unitPrice` + `currency`，`source` 行如实说明来源）。
func (s *Server) profileTestCostFor(ctx context.Context, model string) profileTestCost {
	cost := profileTestCost{
		Quote:  profileTestCostQuoteUnknown,
		Source: "实际费用未知（测试不关联用量记录；不写 0 冒充免费）：实际费用以用量页为准",
		Unknowns: []string{
			profileTestCostUnknownActualCost,
			profileTestCostUnknownActualTokens,
			profileTestCostUnknownUnitPrice,
		},
	}
	if s.Usage == nil || strings.TrimSpace(model) == "" {
		return cost
	}
	table, err := s.Usage.Pricing(ctx)
	if err != nil {
		return cost
	}
	for _, entry := range table.Entries {
		if strings.TrimSpace(entry.Model) != strings.TrimSpace(model) {
			continue
		}
		if entry.InputPerMillion <= 0 || entry.OutputPerMillion <= 0 {
			// 单价非正按「未知」处理而不是「免费」（与 §4.8 runtimeVerifyQuote 同口径）：
			// 0 元会被读成「这次测试不花钱」，而事实是这个模型没有可用的计费维度。
			return cost
		}
		currency := strings.TrimSpace(entry.Currency)
		if currency == "" {
			currency = "USD"
		}
		cost.Currency = currency
		cost.UnitPrice = &profileTestUnitPrice{
			InputPerMillion:  entry.InputPerMillion,
			OutputPerMillion: entry.OutputPerMillion,
			Currency:         currency,
		}
		cost.Source = "实际费用未知（测试不关联用量记录）：目标模型单价已登记（来源：用量定价表）；" +
			"实际费用以用量页为准"
		cost.Unknowns = []string{profileTestCostUnknownActualCost, profileTestCostUnknownActualTokens}
		return cost
	}
	return cost
}

// profileTestEnv 组装被测 CLI 的环境（§4.6 隔离测试入口：没有唯一标记，
// markedModel 恒空 ⇒ 永不注入模型键，行为与历史契约逐字一致）。
func (s *Server) profileTestEnv(root string, id family.ID, recipe profileTestRecipe, plans []family.FilePlan) []string {
	return s.profileTestEnvMarked(root, id, recipe, plans, "")
}

// ccHostManagedEnvKeys 是 **CC 宿主托管形态**的判定键（R225③）：守护进程从 Claude
// Code 会话拉起时会携带其一，此形态下 claude CLI **只认进程 env、忽略 workspace
// settings 的 env 段**（§4.8 的唯一标记就写在 settings 里 ⇒ 不注入即恒 marker_missing）。
var ccHostManagedEnvKeys = []string{"CLAUDE_CODE_HOST_AUTH_ENV_VAR", "CLAUDE_CODE_PROVIDER_MANAGED_BY_HOST"}

// ccHostManagedForm 报告守护进程环境是否处于 CC 宿主托管形态：判定键**在场且取值非空**
// （空串视为未设置——「携带」以有值为准，也让测试可以显式归一成正常形态）。
func ccHostManagedForm(env []string) bool {
	for _, kv := range env {
		i := strings.IndexByte(kv, '=')
		if i <= 0 {
			continue
		}
		for _, key := range ccHostManagedEnvKeys {
			if kv[:i] == key && strings.TrimSpace(kv[i+1:]) != "" {
				return true
			}
		}
	}
	return false
}

// profileTestEnvMarked 是共享的 env 构造（§4.6 与 §4.8 同一处）：在 profileTestEnv
// 的基础上接受一个**可选的带标记模型档位值**（§4.8 传 facts.MarkedModel，§4.6 传空）。
// 子进程只继承 CLI 必需的路径、locale、出站代理和公开 CA 信任设置。所有供应商认证、
// 路由、模型、加载器和 yzagmgr 根变量都留在守护进程边界外；HOME、XDG 与临时目录都指向
// 本次运行的私有树。
//
// claude 代理卡另有**占位认证注入**（见 [`Server.profileTestRouteInject`]）：守护进程常继承
// CC 宿主变量（`CLAUDE_CODE_HOST_AUTH_ENV_VAR` 等），CLI 只认**进程 env** 里的令牌位，
// settings.json 的占位符不再过门 ⇒ 缺占位时隔离测试与受控验证必然
// `Not logged in · Please run /login` exit 1（真机证据：reports/evidence-authfix-20260928/）。
// §4.6 配置测试与 §4.8 受控验证（runtimeevidence 的受控执行）共用本构造函数，注入在两条
// 链路一次覆盖。
//
// **CC 宿主形态的模型键注入（R225③）**：仅当 ①调用方给了带标记的档位值（§4.8 的
// facts.MarkedModel——与写进工作区 settings.json 的标记值同源同值）且 ②守护进程处于
// CC 宿主形态时，把 `ANTHROPIC_MODEL=<带标记档位值>` 追加进进程 env——该形态下 CLI
// 忽略 settings，不注入则设置内标记不可达（恒 marker_missing）；注入值本身带标记，
// 不遮标记反而把它带过门。**正常形态一律不注入**（settings 可达，保持现状零回归）；
// allowlist 会丢弃宿主带来的模型键，避免未标记的模型值胜出。
func (s *Server) profileTestEnvMarked(root string, id family.ID, recipe profileTestRecipe, plans []family.FilePlan, markedModel string) []string {
	return s.profileTestEnvMarkedFrom(root, id, recipe, plans, markedModel, os.Environ())
}

func (s *Server) profileTestEnvMarkedFrom(root string, id family.ID, recipe profileTestRecipe, plans []family.FilePlan, markedModel string, parentEnv []string) []string {
	overrides := []string{
		"HOME=" + root,
		"PWD=" + root,
		"USERPROFILE=" + root,
		"APPDATA=" + filepath.Join(root, "AppData", "Roaming"),
		"LOCALAPPDATA=" + filepath.Join(root, "AppData", "Local"),
		"XDG_CONFIG_HOME=" + filepath.Join(root, ".config"),
		"XDG_CONFIG_DIRS=" + filepath.Join(root, ".config.d"),
		"XDG_DATA_HOME=" + filepath.Join(root, ".local", "share"),
		"XDG_DATA_DIRS=" + filepath.Join(root, ".local", "share.d"),
		"XDG_CACHE_HOME=" + filepath.Join(root, ".cache"),
		"XDG_STATE_HOME=" + filepath.Join(root, ".local", "state"),
		"XDG_RUNTIME_DIR=" + filepath.Join(root, ".runtime"),
		"TMPDIR=" + filepath.Join(root, ".tmp"),
		"TMP=" + filepath.Join(root, ".tmp"),
		"TEMP=" + filepath.Join(root, ".tmp"),
		// 非交互：不要 CLI 的彩色输出与「CI 未设置」分支。
		"NO_COLOR=1",
		"CI=1",
	}
	familyRoot := filepath.Join(root, filepath.FromSlash(recipe.envRel))
	familyKeyPresent := false
	for _, kv := range overrides {
		if i := strings.IndexByte(kv, '='); i > 0 && kv[:i] == recipe.envVar {
			familyKeyPresent = true
			break
		}
	}
	if !familyKeyPresent && recipe.envVar != "" {
		overrides = append(overrides, recipe.envVar+"="+familyRoot)
	}
	if runtime.GOOS == "windows" {
		volume := filepath.VolumeName(root)
		overrides = append(overrides,
			"HOMEDRIVE="+volume,
			"HOMEPATH="+strings.TrimPrefix(root, volume),
		)
	}
	switch id {
	case family.Claude:
		// Claude Code documents these controls for optional update/reporting/download activity.
		overrides = append(overrides,
			"CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC=1",
			"CLAUDE_CODE_DISABLE_OFFICIAL_MARKETPLACE_AUTOINSTALL=1",
			"CLAUDE_CODE_DISABLE_POLICY_SKILLS=1",
			"CLAUDE_CODE_DISABLE_CLAUDE_MDS=1",
			"CLAUDE_CODE_TMPDIR="+filepath.Join(root, ".tmp"),
		)
	case family.OpenCode:
		// The planned config contains the selected model, so model-catalog discovery is not
		// needed for the request; also disable optional updates, sharing and LSP downloads.
		overrides = append(overrides,
			"OPENCODE_DISABLE_AUTOUPDATE=1",
			"OPENCODE_AUTO_SHARE=false",
			"OPENCODE_DISABLE_LSP_DOWNLOAD=1",
			"OPENCODE_DISABLE_MODELS_FETCH=1",
		)
	case family.Pi:
		// Pi's documented offline mode suppresses startup updates/packages/telemetry but
		// leaves the provider request in the normal recipe enabled.
		overrides = append(overrides,
			"PI_OFFLINE=1",
			"PI_SKIP_VERSION_CHECK=1",
			"PI_TELEMETRY=0",
		)
	}
	overrides = append(overrides, s.profileTestRouteInject(id, plans)...)
	if markedModel != "" && id == family.Claude && ccHostManagedForm(parentEnv) {
		overrides = append(overrides, claudeModelKey+"="+markedModel)
	}
	return append(profileTestSafeInheritedEnv(parentEnv), overrides...)
}

var profileTestProxyEnvKeys = []string{"HTTP_PROXY", "HTTPS_PROXY", "ALL_PROXY", "NO_PROXY"}

var profileTestInheritedEnvKeys = []string{
	"PATH", "LANG", "LC_ALL", "LC_CTYPE",
	"SSL_CERT_FILE", "SSL_CERT_DIR", "CURL_CA_BUNDLE", "REQUESTS_CA_BUNDLE", "NODE_EXTRA_CA_CERTS",
}

var profileTestWindowsInheritedEnvKeys = []string{
	"SYSTEMROOT", "WINDIR", "SystemDrive", "OS", "PATHEXT", "COMSPEC",
}

func profileTestSafeInheritedEnv(parentEnv []string) []string {
	windows := runtime.GOOS == "windows"
	values := make(map[string]string, len(profileTestInheritedEnvKeys)+len(profileTestWindowsInheritedEnvKeys))
	proxyUpper := make(map[string]string, len(profileTestProxyEnvKeys))
	proxyLower := make(map[string]string, len(profileTestProxyEnvKeys))
	proxyUpperPresent := make(map[string]bool, len(profileTestProxyEnvKeys))
	proxyLowerPresent := make(map[string]bool, len(profileTestProxyEnvKeys))

	for _, entry := range parentEnv {
		i := strings.IndexByte(entry, '=')
		if i <= 0 {
			continue
		}
		name, value := entry[:i], entry[i+1:]
		if canonical, ok := profileTestProxyKey(name); ok {
			if name == canonical {
				proxyUpper[canonical], proxyUpperPresent[canonical] = value, true
			} else {
				proxyLower[canonical], proxyLowerPresent[canonical] = value, true
			}
			continue
		}
		if canonical, ok := profileTestAllowlistedKey(name, profileTestInheritedEnvKeys, windows); ok {
			values[canonical] = value
			continue
		}
		if windows {
			if canonical, ok := profileTestAllowlistedKey(name, profileTestWindowsInheritedEnvKeys, true); ok {
				values[canonical] = value
			}
		}
	}

	out := make([]string, 0, len(values)+len(profileTestProxyEnvKeys))
	for _, key := range profileTestInheritedEnvKeys {
		if value, ok := values[key]; ok {
			out = append(out, key+"="+value)
		}
	}
	if windows {
		for _, key := range profileTestWindowsInheritedEnvKeys {
			canonical := strings.ToUpper(key)
			if value, ok := values[canonical]; ok {
				out = append(out, canonical+"="+value)
			}
		}
	}
	for _, key := range profileTestProxyEnvKeys {
		switch {
		case proxyUpperPresent[key]:
			out = append(out, key+"="+proxyUpper[key])
		case proxyLowerPresent[key]:
			out = append(out, key+"="+proxyLower[key])
		}
	}
	return out
}

func profileTestAllowlistedKey(name string, keys []string, caseInsensitive bool) (string, bool) {
	for _, key := range keys {
		if name == key || caseInsensitive && strings.EqualFold(name, key) {
			return strings.ToUpper(key), true
		}
	}
	return "", false
}

func profileTestProxyKey(name string) (string, bool) {
	for _, key := range profileTestProxyEnvKeys {
		if strings.EqualFold(name, key) {
			return key, true
		}
	}
	return "", false
}

// profileTestRouteInject 给出隔离运行要**额外注入**的进程 env 段（仅 claude 代理卡非空）。
//
// 判定与取值都与物化启动命令同一事实源（[`claudeRouteEnv`] 读计划内容的 settings `env` 段、
// [`claudeRoutePairs`] 定序取值，与 [`claudeLaunchCommand`] 共用），不另造口径：
//   - **仅 claude 族**：登录预检先于 settings.json 读取是 claude 特有的门，其它族不受影响；
//   - **仅计划的 settings env 含指向本地代理的 `ANTHROPIC_BASE_URL`**（字面前缀
//     `localRouteBase()/`，与 §4.8 `runtimeRequestRoute` 同一条判据口径）才注入；
//     官方直连卡（`{"env":{}}`，无代理 base）不注入——官方档走用户自己的上游登录态。
//
// 注入两键、值逐字取自计划（⇒ 与物化命令同源同值，凭据位恒为占位符 `PROXY_MANAGED`）：
// `ANTHROPIC_AUTH_TOKEN` 让 CC 的登录预检放行、真凭据由本地代理转发时注入（真机缺陷本体：
// 守护进程继承的 CC 宿主变量会让 CLI 只认进程 env 的令牌位，缺它时隔离测试与受控验证
// 必然 `Not logged in` exit 1，证据见 reports/evidence-authfix-20260928/）；
// `ANTHROPIC_BASE_URL` 防守护进程环境里的残留同名变量把请求指去别处（进程 env 盖过
// settings.json，物化命令带它同理）。**刻意不注入 `ANTHROPIC_MODEL`**（[`claudeTestInjectRouteKeys`]）。
func (s *Server) profileTestRouteInject(id family.ID, plans []family.FilePlan) []string {
	if id != family.Claude {
		return nil
	}
	env := claudeRouteEnv(plannedContent(plans))
	if !strings.HasPrefix(strings.TrimSpace(env[claudeKeyBaseURL]), s.localRouteBase()+"/") {
		return nil
	}
	pairs := claudeRoutePairs(env, claudeTestInjectRouteKeys)
	out := make([]string, 0, len(pairs))
	for _, pair := range pairs {
		out = append(out, pair[0]+"="+pair[1])
	}
	return out
}

// writeTempConfigTree 把计划写进临时隔离根，做到与物化（§4.4）**同一件事**：
// 计划字节 + 指向本产品数据根的绝对指针改写。
//
// 三条与物化共用的机制（刻意不另写一套）：
//   - 取哪些文件用 [`plannedContent`]（跳过 Delete 与「live 缺席且目标为空」的项）；
//   - 指针改写用 [`rewriteDataRootRefs`]（把 `<数据根>/…` 改写成临时根里的对应路径，
//     被引用的文件一并拷进来，目录内不留悬空指针——codex 的 `model_catalog_json` 悬空
//     会让 CLI 启动即失败）；
//   - 目标位置用 [`tempTreeNames`]（相对 $HOME 的结构原样保留，理由见该函数注释）。
//
// 不写：live 路径、备份、记账、物化目录。这里出去的字节只落在 root 里。
func (s *Server) writeTempConfigTree(root string, plans []family.FilePlan) error {
	content := plannedContent(plans)
	if len(content) == 0 {
		// 逐文件全是 Delete/空：这份配置的语义是「移除」而不是「写入」，没有可测的内容。
		// 报 not_supported 而不是「测出失败」——它不是一个能被 CLI 回答的问题。
		return apierr.New(apierr.CodeNotSupported,
			"该配置没有任何要写出的文件（它是移除形态），没有可测试的内容")
	}
	names, err := tempTreeNames(s.Env.Home, plans)
	if err != nil {
		return err
	}
	refs := make([]string, 0, len(content))
	for planPath := range content {
		refs = append(refs, planPath)
	}
	refs = append(refs, dataRootFiles(s.Env.DataRoot())...)

	copies := map[string][]byte{}
	for planPath, blob := range content {
		rewritten := s.rewriteDataRootRefs(root, refs, blob, content, names, copies)
		if err := writeTempFile(filepath.Join(root, filepath.FromSlash(names[planPath])), rewritten); err != nil {
			return err
		}
	}
	for target, blob := range copies {
		if err := writeTempFile(target, blob); err != nil {
			return err
		}
	}
	return nil
}

// writeTempFile 落一个临时根下的文件（目录 0700 / 文件 0600，与物化同权限纪律）。
func writeTempFile(path string, blob []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return apierr.Wrap(apierr.CodeStorage, err, "建立临时目录失败: %s", filepath.Dir(path))
	}
	if err := os.WriteFile(path, blob, 0o600); err != nil {
		return apierr.Wrap(apierr.CodeStorage, err, "写入临时文件失败: %s", path)
	}
	return nil
}

// resolveTestCLI 走可注入的解析面（测试摆「装/没装」两个世界用；生产恒为
// [`Server.resolveProfileTestCLI`]）。
func (s *Server) resolveTestCLI(id family.ID, cli string) string {
	if s.profileTestResolveCLI != nil {
		return s.profileTestResolveCLI(id, cli)
	}
	return s.resolveProfileTestCLI(id, cli)
}

// resolveProfileTestCLI 解析该族 CLI 的可执行路径。
//
// 顺序（每一步都有来源，不猜）：
//  1. `PATH`（`exec.LookPath`）——终端里跑测试时的常见情形；
//  2. `/tools/{id}` 的安装探测缓存（[`installProbe.copies`]，**契约要求的同一来源**）——
//     它扫的是常见用户级 bin，且标出了哪个是活动副本；
//  3. [`candidateBinDirs`] 直接落盘检查——app 包启动的守护进程 PATH 往往只有
//     `/usr/bin:/bin:/usr/sbin:/sbin`，此时 `~/.local/bin/pi` 这类副本只能靠目录扫描找到。
//
// 找不到返回空串（调用方按 not_supported 回答，且区分「没装」与「装了我们没找到」）。
func (s *Server) resolveProfileTestCLI(id family.ID, cli string) string {
	if cli == "" {
		return ""
	}
	if path, err := exec.LookPath(cli); err == nil {
		return path
	}
	if s.probe != nil {
		copies := s.probe.copies(id)
		for _, copyInfo := range copies {
			if copyInfo.Active && filepath.Base(copyInfo.Path) == cli {
				return copyInfo.Path
			}
		}
		for _, copyInfo := range copies {
			if filepath.Base(copyInfo.Path) == cli {
				return copyInfo.Path
			}
		}
	}
	for _, dir := range candidateBinDirs() {
		path := filepath.Join(dir, cli)
		if st, err := os.Stat(path); err == nil && !st.IsDir() {
			return path
		}
	}
	return ""
}

// ── 并发闸门 ────────────────────────────────────────────────────────────────

// sweepStaleProfileTestRoots 清掉崩溃残留的临时隔离根（见 profileTestStaleAfter）。
//
// 三条收窄，缺一不可（这是一个会删东西的函数）：
//   - 只看 `os.TempDir()`（与 `os.MkdirTemp` 同一个基目录）；
//   - 名字必须带本功能的某个前缀 [`staleTempRootPrefixes`]（别人的目录一个字都不碰）——
//     两个前缀 = §4.6 配置测试与 §4.8 受控验证的临时根（两者都装着带真凭据的配置副本，
//     共用同一套清理纪律）；
//   - 年龄必须超过 [`profileTestStaleAfter`]（正在跑的测试不可能那么久，因此不会误删）。
//
// 失败只记 warn：清理是尽力而为，它坏了不该让一次测试报失败。
func (s *Server) sweepStaleProfileTestRoots() {
	entries, err := os.ReadDir(os.TempDir())
	if err != nil {
		return
	}
	cutoff := time.Now().Add(-profileTestStaleAfter)
	for _, entry := range entries {
		if !entry.IsDir() || !hasAnyPrefix(entry.Name(), staleTempRootPrefixes) {
			continue
		}
		info, err := entry.Info()
		if err != nil || info.ModTime().After(cutoff) {
			continue
		}
		path := filepath.Join(os.TempDir(), entry.Name())
		if err := os.RemoveAll(path); err != nil {
			s.log.Warn("清理残留的临时隔离根失败", "path", path, "err", err)
			continue
		}
		s.log.Info("已清理残留的临时隔离根（上次运行被强杀？）", "path", path)
	}
}

// staleTempRootPrefixes 是「属于本产品的一次性临时根」的目录名前缀集合。
var staleTempRootPrefixes = []string{profileTestDirPrefix, runtimeVerifyDirPrefix}

// hasAnyPrefix 是前缀判定的小工具。
func hasAnyPrefix(name string, prefixes []string) bool {
	for _, prefix := range prefixes {
		if strings.HasPrefix(name, prefix) {
			return true
		}
	}
	return false
}

// ── 并发闸门与在飞运行表（停止入口的把手） ──────────────────────────────────

// controlledRun 是一次在飞受控运行（§4.6 配置测试 / §4.8 受控验证）的把手与终态槽。
//
// 生命周期（全部经 [`Server.profileTestMu`] 串行化）：
//  1. [`Server.startControlledRun`] 入表（同工具已在表 ⇒ 拒绝：test_in_progress）；
//  2. 运行期间 cancel 端点可置 cancelReq 并调 cancel（进程组回收经既有 ctx 通道）；
//  3. 终态由 [`Server.settleProfileTestOutcome`]（测试）或 [`Server.finishControlledRun`]
//     （没能开始/受控验证）在锁内落定：存快照、关 done、摘表。
//
// 不变式：done 至多关闭一次（finalized 位守）；表里的条目在终态落定时即摘除，
// 因此 cancel 端点在锁内要么看到「在飞运行」、要么看到「没有」——不存在半态。
type controlledRun struct {
	id   family.ID
	kind string
	// pid 是发起本次运行的配置卡 id（测试运行来自 `.../profiles/{pid}/test` 的路径参数；
	// 受控验证不按卡发起，为空串）。停止入口据此回答「在跑的是哪张卡」，错位调用不停止。
	pid string
	// cancel 终止本次运行（测试运行 = 派生 ctx 的 CancelFunc：触发既有 TERM→KILL
	// 进程组回收通道）。nil = 该运行没有停止入口（§4.8 受控验证本波不开放停止）。
	cancel context.CancelFunc
	// done 在终态落定时关闭（等待方据此拿到确定答案，而不是轮询）。
	done chan struct{}

	// 以下字段由 profileTestMu 保护。
	cancelReq bool
	// dispatchStarted is the linearization point for external CLI/API work. A
	// cancel accepted before it guarantees that no request is dispatched.
	dispatchStarted bool
	finalized       bool
	final           any
}

// startControlledRun 尝试占用某工具的受控运行位并登记把手；已被占用返回 nil
// （调用方按 test_in_progress 拒绝）。kind 记录运行种类、pid 记录发起卡（若有），
// 供停止入口如实报告「在飞的是什么、是谁的」。
func (s *Server) startControlledRun(id family.ID, kind, pid string, cancel context.CancelFunc) *controlledRun {
	s.profileTestMu.Lock()
	defer s.profileTestMu.Unlock()
	if s.profileTestRuns == nil {
		s.profileTestRuns = map[family.ID]*controlledRun{}
	}
	if _, busy := s.profileTestRuns[id]; busy {
		return nil
	}
	run := &controlledRun{id: id, kind: kind, pid: pid, cancel: cancel, done: make(chan struct{})}
	s.profileTestRuns[id] = run
	return run
}

// finishControlledRunLocked 在**持锁状态下**落定终态：存快照、关 done、摘表（幂等）。
//
// 摘表带身份判定（表里还是不是**这一个**运行）：调用方的兜底 defer 可能与下一次运行的
// start 交错，无脑 delete 会把新运行的条目误摘。
func (s *Server) markControlledRunDispatched(run *controlledRun) bool {
	s.profileTestMu.Lock()
	defer s.profileTestMu.Unlock()
	if run.cancelReq || run.finalized {
		return false
	}
	run.dispatchStarted = true
	return true
}

func (s *Server) controlledRunCancelRequested(run *controlledRun) bool {
	s.profileTestMu.Lock()
	defer s.profileTestMu.Unlock()
	return run.cancelReq
}

func (s *Server) finishControlledRunLocked(run *controlledRun, final any) {
	if run.finalized {
		return
	}
	run.final = final
	run.finalized = true
	close(run.done)
	if s.profileTestRuns[run.id] == run {
		delete(s.profileTestRuns, run.id)
	}
}

// finishControlledRun 落定一个**没有终态载荷**的运行（没能开始/受控验证路径），
// 并释放运行位。幂等：已由 settle 落定的运行不受影响。
func (s *Server) finishControlledRun(run *controlledRun) {
	s.profileTestMu.Lock()
	defer s.profileTestMu.Unlock()
	s.finishControlledRunLocked(run, nil)
}

// ── HTTP：POST /tools/{id}/profiles/{pid}/test/cancel（UC-097 A4 停止入口）─────

// profileTestCancelPayload 是停止入口的响应形状（A4 additive）。
//
// 语义四态（幂等）：
//   - 没有在飞测试 ⇒ `stopped:false`（200 空操作，重复停止无害），正文说明「没有在跑的测试」；
//   - 在飞的测试属于**另一张配置卡**（路径 pid 与运行登记不符）⇒ `stopped:false` +
//     `active:true`，如实说明在跑的是哪张卡（错位调用不停止任何东西）；
//   - 在飞的是本卡配置测试 ⇒ 终止它，`stopped:true`，`result` 给 `status:"cancelled"`
//     的终态载荷（与原测试请求拿到的终态同一份快照）；
//   - 在飞的是受控验证（§4.8）⇒ `active:true, stopped:false, kind:"verify"` 如实报告
//     （本入口只停止配置测试；受控验证的停止入口不在本波范围）。
type profileTestCancelPayload struct {
	ToolID string `json:"toolId"`
	// Active 是**调用时刻**该工具上是否有在飞的受控运行（test 或 verify）。
	Active bool `json:"active"`
	// Stopped 是**本次调用**是否真的停止了在飞配置测试（`false` = 没有可停的：
	// 无在飞运行 / 在跑的是别的卡 / 在跑的是受控验证——不谎报「已停止」）。
	Stopped bool `json:"stopped"`
	// Kind 是调用时刻在飞运行的种类（active=true 时在场）：test | verify。
	Kind string `json:"kind,omitempty"`
	// Result 是终止后的终态载荷（stopped=true 且运行已落定时在场；等待超时的极端
	// 情形为 null，此时 reason 说明信号已发出、终态待回读）。
	Result *profileTestPayload `json:"result"`
	// Reason 是一句可读说明（空操作 / 已终止 / 错位卡 / 只停配置测试 / 等待未落定）。
	Reason string `json:"reason"`
}

// handleProfileTestCancel 是 `POST /api/v1/tools/{id}/profiles/{pid}/test/cancel`。
//
// 纪律：真实终止在飞测试（沿既有 TERM→KILL 进程组回收通道，见 process_unix.go），
// 终态如实标 `cancelled`（**不谎称回滚、也不谎称成功**）；资源清理走与超时同一条路径
// （runProfileTest 的 defer 删临时根）。幂等与竞态语义见 payload 注释与
// [`Server.settleProfileTestOutcome`]。
func (s *Server) handleProfileTestCancel(r *http.Request) (any, string, error) {
	id, err := s.requireFamily(r.PathValue("id"))
	if err != nil {
		return nil, "", err
	}
	pid := r.PathValue("pid")
	if _, ok := profileTestRecipes[id]; !ok {
		// 没有配方 = 不可能有过配置测试在跑：与测试入口同一道前置，按 not_supported 如实回答
		// （不是「已经停好了」——那会把一个不存在的功能说成可用）。
		return nil, "", apierr.New(apierr.CodeNotSupported,
			"%s 没有可用的 CLI 测试配方：没有可停止的配置测试", id).
			WithDetails(map[string]any{"toolId": string(id)})
	}
	payload := profileTestCancelPayload{ToolID: string(id)}

	s.profileTestMu.Lock()
	run := s.profileTestRuns[id]
	switch {
	case run == nil:
		s.profileTestMu.Unlock()
		payload.Reason = "当前没有在跑的配置测试（重复停止是无害的空操作）"
		return payload, "", nil
	case run.kind == controlledRunTest && strings.HasPrefix(run.pid, "dfp-v1-"):
		// 草稿测试只由 /profiles/draft/test/cancel 的 body-bound fingerprint
		// 入口停止，旧的 path 参数入口不能匹配或取消草稿测试。
		s.profileTestMu.Unlock()
		payload.Active = true
		payload.Kind = controlledRunTest
		payload.Reason = "该工具上在跑的是一份未保存配置草稿；请使用草稿测试的停止入口"
		return payload, "", nil
	case run.kind == controlledRunTest && run.pid != "" && run.pid != pid:
		// 错位调用（路径卡 ≠ 在跑运行的发起卡）：不停止任何东西，如实说明在跑的是谁。
		runningPid := run.pid
		s.profileTestMu.Unlock()
		payload.Active = true
		payload.Kind = controlledRunTest
		payload.Reason = "该工具上在跑的是另一张配置卡（" + runningPid + "）的测试：" +
			"本卡没有在跑的测试（错位调用不实施停止）"
		return payload, "", nil
	case run.kind != controlledRunTest:
		// 在飞的是受控验证：本入口只停止配置测试，如实报告而不是假装停过、
		// 也不是把「无法停止」折成错误（它会在完成或超时后自行结束）。
		kind := run.kind
		s.profileTestMu.Unlock()
		payload.Active = true
		payload.Kind = kind
		payload.Reason = "该工具上在跑的是受控验证（§4.8）：本入口只停止配置测试；" +
			"受控验证会在完成或超时后自行结束"
		return payload, "", nil
	}
	run.cancelReq = true
	cancel := run.cancel
	done := run.done
	s.profileTestMu.Unlock()

	if cancel != nil {
		cancel() // 触发 exec 的 ctx 取消 → cmd.Cancel → 进程组 TERM→KILL（既有通道）
	}
	// 有界等待终态：正常 < 3s（kill 宽限 300ms + WaitDelay 2s）；到点如实回答「尚未落定」。
	select {
	case <-done:
	case <-time.After(profileTestCancelWait):
		payload.Active = true
		payload.Stopped = true
		payload.Kind = controlledRunTest
		payload.Reason = "停止信号已发出（按进程组回收中）：运行尚未落定终态，稍后回读测试结果确认"
		return payload, "", nil
	}
	s.profileTestMu.Lock()
	final, _ := run.final.(*profileTestPayload)
	s.profileTestMu.Unlock()

	payload.Active = true
	payload.Stopped = true
	payload.Kind = controlledRunTest
	payload.Result = final
	if final == nil {
		payload.Reason = "运行已结束：这次测试没能开始，没有结果可读"
	} else {
		payload.Reason = "已终止在飞的配置测试：终态 cancelled（进程组与临时根按超时同一路径回收，" +
			"不谎称回滚、也不谎称成功）"
	}
	return payload, "", nil
}

// ── 进程执行（生产实现；平台差异只在进程组回收，见同目录 process_* 文件） ──────

// execProfileTestCLI 在隔离根里跑一次 CLI，返回有界的输出与退出码。
//
// 编排细节与理由：
//   - **不给 stdin**（`cmd.Stdin` 留 nil ⇒ /dev/null）：有些 CLI 会等 stdin
//     （claude 会白等 3s），在测试里等 stdin 没有意义，只会把「最小请求」拖成超时；
//   - `cmd.Dir = Root`：工作目录也进临时根，免得 CLI 在真实项目目录里捡到 AGENTS.md
//     之类的上下文文件——那是「另一份输入」，会让结果无法归因到被测配置；
//   - `cmd.Cancel` + `WaitDelay`：超时时先按进程组回收，再给 Wait 一个有限窗口把管道收掉
//     （否则孙进程持着管道会让 Wait 一直不返回，`WaitDelay` 正是为此存在）；
//   - 失败（含超时）都把已捕获的输出带回去：诊断信息是端点的产出之一。
func execProfileTestCLI(ctx context.Context, req profileTestExecRequest) profileTestExecResult {
	// **90s 上限在这里落下**：调用方给的是请求上下文（本身没有截止时间），而「一次测试最长
	// 90s」是契约（UC-039）。用 WithTimeout 而不是自己计时，是因为 `CommandContext` 会在
	// 期限到达时调用 `cmd.Cancel`（我们的进程组回收），`WaitDelay` 再给管道一个有限窗口。
	ctx, cancel := context.WithTimeout(ctx, profileTestTimeout)
	defer cancel()

	redactions := profileTestRedactionsWithEnvironment(req.Redactions, req.Env)
	out := newProfileTestRedactingStream(profileTestMaxCapture, redactions)
	errOut := newProfileTestRedactingStream(profileTestMaxCapture, redactions)

	cmd := exec.CommandContext(ctx, req.Exe, req.Args...)
	cmd.Dir = req.Root
	cmd.Env = req.Env
	cmd.Stdout = out
	cmd.Stderr = errOut
	setProcessGroup(cmd)
	cmd.Cancel = func() error { return killProcessGroup(cmd) }
	cmd.WaitDelay = 2 * time.Second

	// Start + Wait（而不是 Run）只为一件事：把进程号带回去（§4.8 的实例身份要素）；
	// 两者语义与 Run 等价（Run 就是 Start + Wait）。
	if startErr := cmd.Start(); startErr != nil {
		// Start 失败：没有进程、也没有任何拷贝 goroutine，此刻读缓冲是安全的。
		res := profileTestExecResult{Stdout: out.String(), Stderr: errOut.String(), ExitCode: -1, Err: startErr}
		if ctx.Err() != nil {
			res.Err = ctx.Err()
			if errors.Is(ctx.Err(), context.DeadlineExceeded) {
				res.TimedOut = true
			}
		}
		return res
	}
	// 这里**只取 PID、不读缓冲**：Start 成功后 os/exec 的 stdout/stderr 拷贝 goroutine
	// 正在往 out/errOut 里写，此刻读 bytes.Buffer 是与写方无同步的并发访问（-race 下
	// 桩 CLI 只要写出任何字节就报 DATA RACE，C-2fix 首轮核验 issue1）；这份早读值本就会
	// 被 Wait 之后的重读覆盖，去掉零损失。捕获缓冲一律等 cmd.Wait() 返回后再读——Wait
	// 保证拷贝 goroutine 已收尾，那之后是唯一安全的读取点。
	res := profileTestExecResult{PID: cmd.Process.Pid}
	err := cmd.Wait()
	res.Stdout = out.String()
	res.Stderr = errOut.String()
	res.Err = err
	switch {
	case errors.Is(ctx.Err(), context.DeadlineExceeded):
		// 超时：按 timedOut 如实作答；信号路径下的退出码没有意义。
		res.TimedOut = true
		res.ExitCode = -1
		return res
	case ctx.Err() != nil:
		// 调用方中止（客户端断开、服务退出）：不是「超时」，但同样没跑完，按失败上报。
		res.Err = ctx.Err()
		res.ExitCode = -1
		return res
	}
	if err == nil {
		res.ExitCode = 0
		return res
	}
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		res.ExitCode = exit.ExitCode()
		return res
	}
	res.ExitCode = -1
	return res
}

// cappedStream 是有界输出捕获：写满上限后丢弃后续，并把截断点回退到最后一个换行。
//
// 为什么回退到换行：① 让 [`parseModelFromCLIOutput`] 看到的最后一行仍是**完整**的 JSON
// 事件（半行必然解析失败，白白丢掉模型名）；② 令牌不会被截成半截串——半截串不再匹配
// 令牌形态，掩码会漏，而「安全性质不能靠碰巧没截在令牌中间」。
type cappedStream struct {
	buf       bytes.Buffer
	limit     int
	truncated bool
}

func (c *cappedStream) Write(p []byte) (int, error) {
	n := len(p)
	if c.truncated {
		return n, nil
	}
	room := c.limit - c.buf.Len()
	if room <= 0 {
		c.truncated = true
		return n, nil
	}
	if len(p) > room {
		p = p[:room]
		c.truncated = true
	}
	c.buf.Write(p)
	return n, nil
}

// String 给出捕获到的文本；被截断时丢掉最后一行（可能是半行）。
func (c *cappedStream) String() string {
	text := c.buf.String()
	if !c.truncated {
		return text
	}
	if i := strings.LastIndexByte(text, '\n'); i >= 0 {
		return text[:i+1]
	}
	return "" // 一整行就超过上限：宁可什么都不留，也不留半行
}

// ── 输出摘要与解析 ──────────────────────────────────────────────────────────

// summarizeProfileTestOutput 给出有界、无凭据的输出摘要。
//
// 两条取舍：
//   - **先脱敏再截断**：反过来会把令牌截成半截串，半截串不再匹配令牌形态 ⇒ 漏掩；
//   - 失败时优先 stderr（诊断信息在那里），成功时优先 stdout（CLI 的结果在那里），
//     另一侧为空才回落——两条流都空则给空串（不编「无输出」之外的话）。
func summarizeProfileTestOutputWithRedactions(stdout, stderr string, failed bool, redactions []string) string {
	primary, secondary := stdout, stderr
	if failed {
		primary, secondary = stderr, stdout
	}
	text := strings.TrimSpace(maskProfileTestOutput(primary, redactions))
	if text == "" {
		text = strings.TrimSpace(maskProfileTestOutput(secondary, redactions))
	}
	runes := []rune(text)
	if len(runes) > profileTestOutputLimit {
		return string(runes[:profileTestOutputLimit-1]) + "…"
	}
	return text
}

func maskProfileTestOutput(text string, redactions []string) string {
	return maskCredentials(redactExactProfileTestValues(text, redactions))
}

func redactExactProfileTestValues(text string, redactions []string) string {
	for _, secret := range sortedProfileTestRedactions(redactions) {
		text = strings.ReplaceAll(text, secret, maskedValue)
	}
	return text
}

func sortedProfileTestRedactions(redactions []string) []string {
	secrets := make([]string, 0, len(redactions))
	seen := make(map[string]struct{}, len(redactions))
	for _, secret := range redactions {
		if secret == "" {
			continue
		}
		if _, ok := seen[secret]; ok {
			continue
		}
		seen[secret] = struct{}{}
		secrets = append(secrets, secret)
	}
	sort.Slice(secrets, func(i, j int) bool {
		if len(secrets[i]) != len(secrets[j]) {
			return len(secrets[i]) > len(secrets[j])
		}
		return secrets[i] < secrets[j]
	})
	return secrets
}

func profileTestRedactionsWithEnvironment(redactions, env []string) []string {
	combined := append([]string(nil), redactions...)
	for _, entry := range env {
		i := strings.IndexByte(entry, '=')
		if i <= 0 {
			continue
		}
		if _, ok := profileTestProxyKey(entry[:i]); !ok {
			continue
		}
		value := strings.TrimSpace(entry[i+1:])
		if value == "" || !strings.Contains(value, "@") {
			continue
		}
		// Treat the complete proxy value as sensitive whenever it contains an authority separator;
		// this also protects malformed forms whose authority cannot be parsed or normalized.
		combined = append(combined, value)
		rawUserinfo := profileTestRawProxyUserinfo(value)
		// Scrub the exact URL and raw userinfo even if invalid escapes make url.Parse fail.
		if rawUserinfo != "" {
			combined = append(combined, rawUserinfo)
			if username, password, hasPassword := strings.Cut(rawUserinfo, ":"); hasPassword {
				combined = append(combined, username, password)
			} else {
				combined = append(combined, rawUserinfo)
			}
		}
		parsed, err := url.Parse(value)
		if err == nil && parsed.User == nil && !strings.Contains(value, "://") {
			parsed, err = url.Parse("http://" + value)
		}
		if err == nil && parsed.User != nil {
			userinfo := parsed.User.String()
			combined = append(combined, userinfo, parsed.User.Username())
			if _, encodedPassword, hasPassword := strings.Cut(userinfo, ":"); hasPassword {
				combined = append(combined, encodedPassword)
			}
			if password, ok := parsed.User.Password(); ok {
				combined = append(combined, password)
			}
		}
	}
	return sortedProfileTestRedactions(combined)
}

func profileTestRawProxyUserinfo(value string) string {
	authority := value
	if schemeEnd := strings.Index(value, "://"); schemeEnd >= 0 {
		authority = value[schemeEnd+3:]
	} else {
		authority = strings.TrimPrefix(authority, "//")
	}
	if authorityEnd := strings.IndexAny(authority, "/?#"); authorityEnd >= 0 {
		authority = authority[:authorityEnd]
	}
	at := strings.LastIndexByte(authority, '@')
	if at <= 0 {
		return ""
	}
	return authority[:at]
}

// profileTestRedactingStream redacts exact ephemeral credentials as bytes are
// captured, before the bounded output cap can split a credential at its edge.
// It retains at most the longest credential length across writes so secrets
// spanning pipe chunks are never emitted partially.
type profileTestRedactingStream struct {
	captured cappedStream
	pending  []byte
	secrets  [][]byte
	maxLen   int
}

func newProfileTestRedactingStream(limit int, redactions []string) *profileTestRedactingStream {
	secrets := sortedProfileTestRedactions(redactions)
	stream := &profileTestRedactingStream{captured: cappedStream{limit: limit}}
	for _, secret := range secrets {
		value := []byte(secret)
		stream.secrets = append(stream.secrets, value)
		if len(value) > stream.maxLen {
			stream.maxLen = len(value)
		}
	}
	return stream
}

func (s *profileTestRedactingStream) Write(p []byte) (int, error) {
	n := len(p)
	if n == 0 {
		return 0, nil
	}
	s.pending = append(s.pending, p...)
	s.drain(false)
	return n, nil
}

func (s *profileTestRedactingStream) String() string {
	pending := s.pending[:cap(s.pending)]
	s.drain(true)
	text := s.captured.String()
	for i := range s.secrets {
		for j := range s.secrets[i] {
			s.secrets[i][j] = 0
		}
	}
	for i := range pending {
		pending[i] = 0
	}
	return text
}

func (s *profileTestRedactingStream) drain(final bool) {
	if len(s.pending) == 0 {
		return
	}
	limit := len(s.pending)
	if !final && s.maxLen > 1 {
		limit -= s.maxLen - 1
		if limit < 0 {
			limit = 0
		}
	}
	if final {
		if partial := s.partialSecretSuffixStart(s.pending); partial < limit {
			limit = partial
		}
	}
	var safe bytes.Buffer
	i := 0
	for i < limit {
		if size := s.secretAt(i); size > 0 {
			safe.WriteString(maskedValue)
			i += size
			continue
		}
		safe.WriteByte(s.pending[i])
		i++
	}
	consumed := i
	if final {
		if consumed < len(s.pending) {
			safe.WriteString(maskedValue)
			consumed = len(s.pending)
		}
	}
	if safe.Len() > 0 {
		_, _ = s.captured.Write(safe.Bytes())
	}
	if consumed >= len(s.pending) {
		s.pending = s.pending[:0]
	} else {
		s.pending = append(s.pending[:0], s.pending[consumed:]...)
	}
}

func (s *profileTestRedactingStream) secretAt(offset int) int {
	for _, secret := range s.secrets {
		if offset+len(secret) <= len(s.pending) && bytes.Equal(s.pending[offset:offset+len(secret)], secret) {
			return len(secret)
		}
	}
	return 0
}

func (s *profileTestRedactingStream) partialSecretSuffixStart(value []byte) int {
	start := len(value)
	for _, secret := range s.secrets {
		max := len(secret) - 1
		if max > len(value) {
			max = len(value)
		}
		for n := max; n > 0; n-- {
			if bytes.Equal(value[len(value)-n:], secret[:n]) {
				if candidate := len(value) - n; candidate < start {
					start = candidate
				}
				break
			}
		}
	}
	return start
}

// parseModelFromCLIOutput 从 CLI 的 stdout 里尽力解析本次请求实际用的模型与 provider。
//
// 唯一有实证的形态是 pi 的 `--mode json` 事件流（2026-09-25 实测）：每行一个 JSON 事件，
// 其中 `message_end` 的 `message` 是**最终权威消息**，assistant 消息带 `model`/`provider`
// （另有 `responseModel` 表示上游回的真实模型名，作为 `model` 缺席时的回落）。
// 其余 CLI 的输出形态未实证，因此这里只做「逐行 JSON + 认得这两个字段」的通用扫描：
// 认得出就用，认不出留空——**不编**（契约：其余尽力，解析不到留空）。
//
// 取**最后**一次命中的值：一次运行可能有多个 assistant 消息（多轮/重试），最后一条
// 才是这次跑完的结论。
func parseModelFromCLIOutput(stdout string) (model, provider string) {
	type cliMessage struct {
		Role          string `json:"role"`
		Model         string `json:"model"`
		Provider      string `json:"provider"`
		ResponseModel string `json:"responseModel"`
	}
	for _, line := range strings.Split(stdout, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "{") {
			continue
		}
		var event struct {
			Type     string      `json:"type"`
			Model    string      `json:"model"`
			Provider string      `json:"provider"`
			Message  *cliMessage `json:"message"`
		}
		if err := json.Unmarshal([]byte(line), &event); err != nil {
			continue
		}
		if event.Message != nil && event.Message.Role == "assistant" {
			name := strings.TrimSpace(event.Message.Model)
			if name == "" {
				name = strings.TrimSpace(event.Message.ResponseModel)
			}
			if name != "" {
				model = name
			}
			if p := strings.TrimSpace(event.Message.Provider); p != "" {
				provider = p
			}
			continue
		}
		// 顶层形态（部分 CLI 把 model/provider 放在结果对象顶层）。
		if event.Type == "" && strings.TrimSpace(event.Model) != "" {
			model = strings.TrimSpace(event.Model)
			if p := strings.TrimSpace(event.Provider); p != "" {
				provider = p
			}
		}
	}
	return model, provider
}
