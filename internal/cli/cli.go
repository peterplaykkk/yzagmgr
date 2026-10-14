// Package cli 是 yzagmgr 二进制的命令行子命令入口：在无 GUI 的 Mac 与 Linux 上
// 经本地管理 API（REST，契约见 doc/api-v1.md）管理被管工具的配置文件。
//
// 四条纪律：
//
//  1. **纯客户端**：全部操作经 127.0.0.1:<YZAGMGR_API_PORT|17022> 的 REST 完成，
//     零 API 变更、不直接读写被管工具的配置文件、不改 internal/api 的路由表。
//  2. **令牌复用守护进程同一读取逻辑**：`config.FromProcess` + `config.Env.ReadAdminToken`
//     （`<data_root>/admin.token`，0600，守护进程每次启动重新生成），
//     不另发明读取路径。`health` 是契约免鉴权端点（§1.1），令牌缺失时照常可用；
//     其余命令一律要求令牌。
//  3. **不自动拉起守护进程**：管理 API 不可达时给出清晰错误与提示（退出码 4），
//     由用户自己决定何时启动（`yzagmgr serve` 或 yzagmgr.app）。
//  4. **无参数与 `yzagmgr serve` 与守护进程形态完全一致**（LaunchServices/托盘以
//     无参数方式拉起，绝不能破坏）——Parse 把它们归到 KindDaemon，由 main 走
//     原 run() 路径，一个字节的行为都不改。
//
// 端口/env 语义与守护进程一致（YZAGMGR_HOME / YZAGMGR_API_PORT），端口解析直接
// 复用 config.PortFromEnv。
package cli

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/peterplaykkk/yzagmgr/internal/config"
)

// 退出码（契约，doc/cli.md 与 `yzagmgr help` 同文）。
//
// 映射刻意把「本地前置不成立」与「API 业务失败」分开：脚本可以只看退出码就知道
// 该重试（4：起守护进程）、该查环境（3：令牌/2：参数），还是该看业务错误（1）。
const (
	ExitOK      = 0 // 成功
	ExitAPI     = 1 // API/业务失败（信封 ok:false 或响应不可解析）
	ExitUsage   = 2 // 用法错误（未知子命令、缺参、非法参数、非法 YZAGMGR_API_PORT）
	ExitToken   = 3 // 管理令牌缺失或不可读
	ExitUnreach = 4 // 管理 API 不可达（守护进程未运行）
)

// Kind 是子命令种类。
type Kind int

const (
	KindDaemon Kind = iota // 无参数 / serve：启动守护进程（行为与旧版一致）
	KindHelp
	KindHealth
	KindToolsList
	KindToolsShow
	KindPreview
	KindApply
	KindRestore
	KindBackupsList
	KindPolicyShow
	KindPolicyRefresh
	KindRoutesAdd
	KindRoutesList
	KindRoutesShow
	KindRoutesSet
	KindRoutesRemove
	KindRemapModelSet
	KindRemapModelList
	KindRemapEffortSet
	KindRemapEffortForce
	KindRemapEffortList
	KindProfilesList
	KindProfilesShow
	KindProfilesCreate
	KindProfilesUpdate
	KindProfilesDelete
	KindProfilesDuplicate
	KindMaterialize
)

// Cmd 是一次解析完成的命令。
type Cmd struct {
	Kind    Kind
	JSON    bool   // --json：结果以 REST 同形信封输出（{"ok":true,"data":...}）
	Tool    string // tools show / preview / apply 的工具 id
	Profile string // preview / apply 的档位卡 id（--profile）
	Backup  string // restore 的备份编号（--backup；空 = 恢复最近一次写入前的备份）

	// Name 是 routes/remap 家族的路由名（臂名）。
	Name string
	// Endpoint / CredFile 是 `routes add|set` 的 --endpoint / --cred-file
	// （凭据明文只经 0600 文件交付，不进 argv/日志）。
	Endpoint string
	CredFile string
	// Alias / Target 是 `remap model set` 的 --alias / --target。
	Alias  string
	Target string
	// Supports1m / Prefer1m 是 `remap model set` 的 --supports1m / --prefer1m
	// 能力位（UC-030 additive）：布尔存在旗标（给出 = true，重复幂等，同 --json）。
	// 请求体**仅在显式给出时**写键：缺席 = 不声明（z 的「缺席=false」语义，
	// false/缺席不伪造键）。
	Supports1m bool
	Prefer1m   bool
	// From / To 是 `remap effort set|force` 的 --from / --to；Set 位区分「旗标没给」
	// 与「给了空值」（--from '' = 缺省项登记，--to '' = 清除 force）。
	// `profiles create --from` 复用 From/FromSet（同旗标异语义，按子命令白名单收口）。
	From    string
	FromSet bool
	To      string
	ToSet   bool

	// CardID 是 profiles show|update|delete|duplicate 的档位卡 id（位置参数）。
	CardID string
	// CardName / Models / ModelID 是 profiles create|update 的 --name / --models /
	// --model-id（ModelsSet 区分「没给」与「给了空清单」：--models '' = 清空）。
	CardName  string
	Models    string
	ModelsSet bool
	ModelID   string
	// CredRef / MaxOutputTokens 是 profiles create|update 的 --cred-ref /
	// --max-output-tokens（UC-032 引用形凭据 / UC-040 能力位）。MaxOutputTokensSet
	// 区分「没给」与给了值；--cred-ref 与 --cred-file 互斥（与 REST 的 cred/credRef 同口径）。
	CredRef            string
	MaxOutputTokens    int
	MaxOutputTokensSet bool
	// UserID / PolicyVersion 是 ensure-gateway-profiles 的 --user / --policy-version。
	UserID        string
	PolicyVersion string
	// Refresh 是 materialize 的 --refresh（缺省 = 只读盘点）。
	Refresh bool
	// Tools 是 `delivery switch` 的 --tools（逗号分隔的工具 id；空 = 全部下发卡）。
	Tools    string
	ToolsSet bool
}

// usageError 是用法错误（退出码 2）。
type usageError struct{ msg string }

func (e *usageError) Error() string { return e.msg }

func usageErrorf(format string, args ...any) error {
	return &usageError{msg: fmt.Sprintf(format, args...)}
}

// Parse 解析 os.Args[1:] 形态的参数。纯函数：不做任何 IO。
//
// 旗标（--json / --profile / --backup）允许出现在任何位置；`--profile=x` 与
// `--profile x` 两种形态都接受。以 `-psn_` 起头的参数是 LaunchServices 拉起时的
// 进程序列号残留（不是用户输入），忽略之——否则「无参数拉起守护进程」会被误判成
// 用法错误，直接破坏托盘/LaunchServices 形态。
func Parse(args []string) (Cmd, error) {
	var cmd Cmd
	rest := make([]string, 0, len(args))
	for _, a := range args {
		if strings.HasPrefix(a, "-psn_") {
			continue
		}
		rest = append(rest, a)
	}

	// help 恒为独立命令（在旗标解析之前判，`--help` 不是「未知旗标」）。
	if len(rest) > 0 && (rest[0] == "help" || rest[0] == "--help" || rest[0] == "-h") {
		if len(rest) > 1 {
			return Cmd{}, usageErrorf("help 不接受参数: %q", strings.Join(rest[1:], " "))
		}
		cmd.Kind = KindHelp
		return cmd, nil
	}

	positional := make([]string, 0, 3)
	for i := 0; i < len(rest); i++ {
		a := rest[i]
		switch {
		case a == "--json":
			cmd.JSON = true
		case a == "--refresh":
			cmd.Refresh = true
		case a == "--supports1m":
			cmd.Supports1m = true
		case a == "--prefer1m":
			cmd.Prefer1m = true
		case a == "--profile" || a == "--backup" || a == "--endpoint" || a == "--cred-file" ||
			a == "--cred-ref" || a == "--max-output-tokens" ||
			a == "--alias" || a == "--target" || a == "--from" || a == "--to" ||
			a == "--name" || a == "--models" || a == "--model-id" || a == "--user" || a == "--policy-version" ||
			a == "--tools":
			if i+1 >= len(rest) {
				return Cmd{}, usageErrorf("%s 缺少取值", a)
			}
			i++
			if err := setValueFlag(&cmd, a, rest[i]); err != nil {
				return Cmd{}, err
			}
		case strings.HasPrefix(a, "--profile="):
			cmd.Profile = strings.TrimPrefix(a, "--profile=")
		case strings.HasPrefix(a, "--backup="):
			cmd.Backup = strings.TrimPrefix(a, "--backup=")
		case strings.HasPrefix(a, "--endpoint="):
			cmd.Endpoint = strings.TrimPrefix(a, "--endpoint=")
		case strings.HasPrefix(a, "--cred-file="):
			cmd.CredFile = strings.TrimPrefix(a, "--cred-file=")
		case strings.HasPrefix(a, "--cred-ref="):
			cmd.CredRef = strings.TrimPrefix(a, "--cred-ref=")
		case strings.HasPrefix(a, "--max-output-tokens="):
			raw := strings.TrimPrefix(a, "--max-output-tokens=")
			n, err := strconv.Atoi(raw)
			if err != nil || n < 0 || n > 1_000_000 {
				return Cmd{}, usageErrorf("--max-output-tokens 需要 0..1000000 的整数（0 = 清除声明，仅 update）: %q", raw)
			}
			cmd.MaxOutputTokens, cmd.MaxOutputTokensSet = n, true
		case strings.HasPrefix(a, "--alias="):
			cmd.Alias = strings.TrimPrefix(a, "--alias=")
		case strings.HasPrefix(a, "--target="):
			cmd.Target = strings.TrimPrefix(a, "--target=")
		case strings.HasPrefix(a, "--from="):
			cmd.From = strings.TrimPrefix(a, "--from=")
			cmd.FromSet = true
		case strings.HasPrefix(a, "--to="):
			cmd.To = strings.TrimPrefix(a, "--to=")
			cmd.ToSet = true
		case strings.HasPrefix(a, "--name="):
			cmd.CardName = strings.TrimPrefix(a, "--name=")
		case strings.HasPrefix(a, "--models="):
			cmd.Models = strings.TrimPrefix(a, "--models=")
			cmd.ModelsSet = true
		case strings.HasPrefix(a, "--model-id="):
			cmd.ModelID = strings.TrimPrefix(a, "--model-id=")
		case strings.HasPrefix(a, "--user="):
			cmd.UserID = strings.TrimPrefix(a, "--user=")
		case strings.HasPrefix(a, "--policy-version="):
			cmd.PolicyVersion = strings.TrimPrefix(a, "--policy-version=")
		case strings.HasPrefix(a, "--tools="):
			cmd.Tools = strings.TrimPrefix(a, "--tools=")
			cmd.ToolsSet = true
		case strings.HasPrefix(a, "-"):
			return Cmd{}, usageErrorf("未知旗标 %q", a)
		default:
			positional = append(positional, a)
		}
	}

	if len(positional) == 0 {
		// 无子命令 = 守护进程形态（含 -psn_* 过滤后的空参数）。带旗标却没有
		// 子命令一定是用法错误——旗标不属于守护进程形态。
		if hasAnyFlag(cmd) {
			return Cmd{}, usageErrorf("旗标必须与子命令一起使用")
		}
		cmd.Kind = KindDaemon
		return cmd, nil
	}

	sub := positional[0]
	restPos := positional[1:]
	switch sub {
	case "serve":
		if len(restPos) > 0 || hasAnyFlag(cmd) {
			return Cmd{}, usageErrorf("serve 不接受参数与旗标（无参数的 yzagmgr 与 yzagmgr serve 等价）")
		}
		cmd.Kind = KindDaemon
	case "health":
		if err := exactArity(restPos, 0, "health"); err != nil {
			return Cmd{}, err
		}
		cmd.Kind = KindHealth
	case "tools":
		if len(restPos) == 0 {
			return Cmd{}, usageErrorf("tools 需要子命令：list | show <id>")
		}
		switch restPos[0] {
		case "list":
			if err := exactArity(restPos[1:], 0, "tools list"); err != nil {
				return Cmd{}, err
			}
			cmd.Kind = KindToolsList
		case "show":
			if err := exactArity(restPos[1:], 1, "tools show"); err != nil {
				return Cmd{}, err
			}
			cmd.Kind = KindToolsShow
			cmd.Tool = restPos[1]
		default:
			return Cmd{}, usageErrorf("未知子命令 %q（tools 需要 list | show <id>）", restPos[0])
		}
	case "preview", "apply":
		if err := exactArity(restPos, 1, sub); err != nil {
			return Cmd{}, err
		}
		cmd.Tool = restPos[0]
		if sub == "preview" {
			cmd.Kind = KindPreview
		} else {
			cmd.Kind = KindApply
		}
		if strings.TrimSpace(cmd.Profile) == "" {
			return Cmd{}, usageErrorf("%s 需要 --profile <pid>（档位卡 id，见 yzagmgr tools show）", sub)
		}
		if cmd.Backup != "" {
			return Cmd{}, usageErrorf("--backup 只用于 restore")
		}
	case "restore":
		if err := exactArity(restPos, 0, "restore"); err != nil {
			return Cmd{}, err
		}
		if cmd.Profile != "" {
			return Cmd{}, usageErrorf("--profile 只用于 preview / apply")
		}
		cmd.Kind = KindRestore
	case "backups":
		if len(restPos) == 0 {
			return Cmd{}, usageErrorf("backups 需要子命令：list")
		}
		if restPos[0] != "list" {
			return Cmd{}, usageErrorf("未知子命令 %q（backups 只有 list）", restPos[0])
		}
		if err := exactArity(restPos[1:], 0, "backups list"); err != nil {
			return Cmd{}, err
		}
		if cmd.Profile != "" || cmd.Backup != "" {
			return Cmd{}, usageErrorf("backups list 不接受 --profile / --backup")
		}
		cmd.Kind = KindBackupsList
	case "policy":
		if len(restPos) == 0 {
			return Cmd{}, usageErrorf("policy 需要子命令：show | refresh")
		}
		switch restPos[0] {
		case "show":
			cmd.Kind = KindPolicyShow
		case "refresh":
			cmd.Kind = KindPolicyRefresh
		default:
			return Cmd{}, usageErrorf("未知子命令 %q（policy 需要 show | refresh）", restPos[0])
		}
		if err := exactArity(restPos[1:], 0, "policy "+restPos[0]); err != nil {
			return Cmd{}, err
		}
		if cmd.Profile != "" || cmd.Backup != "" {
			return Cmd{}, usageErrorf("policy 不接受 --profile / --backup")
		}
	case "routes":
		if err := parseRoutes(&cmd, restPos); err != nil {
			return Cmd{}, err
		}
	case "remap":
		if err := parseRemap(&cmd, restPos); err != nil {
			return Cmd{}, err
		}
	case "profiles":
		if err := parseProfiles(&cmd, restPos); err != nil {
			return Cmd{}, err
		}
	case "materialize":
		if err := exactArity(restPos, 0, "materialize"); err != nil {
			return Cmd{}, err
		}
		cmd.Kind = KindMaterialize
		if hasAnyOf(cmd.Endpoint, cmd.CredFile, cmd.CredRef, cmd.Alias, cmd.Target, cmd.CardName, cmd.ModelID,
			cmd.From, cmd.To, cmd.Models, cmd.UserID, cmd.PolicyVersion) || cmd.MaxOutputTokensSet ||
			cmd.FromSet || cmd.ToSet || cmd.ModelsSet || cmd.Supports1m || cmd.Prefer1m {
			return Cmd{}, usageErrorf("materialize 不接受配置旗标（只接受 --refresh）")
		}
		if cmd.Profile != "" || cmd.Backup != "" {
			return Cmd{}, usageErrorf("materialize 不接受 --profile / --backup")
		}
	default:
		return Cmd{}, usageErrorf("未知子命令 %q", sub)
	}

	if strings.TrimSpace(cmd.Tool) == "" && (cmd.Kind == KindToolsShow || cmd.Kind == KindPreview || cmd.Kind == KindApply) {
		return Cmd{}, usageErrorf("工具 id 不得为空")
	}
	// 旗标白名单按子命令收口：routes/remap 的配置旗标出现在别的子命令上（或反之）
	// 一律用法错误——静默忽略比报错危险（用户以为改了配置）。
	switch cmd.Kind {
	case KindRoutesAdd, KindRoutesSet:
		// 只吃 --endpoint/--cred-file（parseRoutes 已拒 alias/target/from/to 与
		// supports1m/prefer1m 能力位）。
		if hasAnyOf(cmd.CardName, cmd.ModelID, cmd.UserID, cmd.PolicyVersion) || cmd.ModelsSet || cmd.Refresh ||
			cmd.CredRef != "" || cmd.MaxOutputTokensSet {
			return Cmd{}, usageErrorf("该子命令不接受 --name / --models / --model-id / --user / --policy-version / --refresh")
		}
	case KindRoutesList, KindRoutesShow, KindRoutesRemove,
		KindRemapModelList, KindRemapEffortList:
		if cmd.Endpoint != "" || cmd.CredFile != "" || cmd.CredRef != "" ||
			hasAnyOf(cmd.CardName, cmd.ModelID, cmd.UserID, cmd.PolicyVersion) || cmd.ModelsSet || cmd.Refresh ||
			cmd.MaxOutputTokensSet {
			return Cmd{}, usageErrorf("该子命令不接受 --endpoint / --cred-file / --name / --models / --model-id / --user / --policy-version / --refresh")
		}
	case KindRemapModelSet, KindRemapEffortSet, KindRemapEffortForce:
		// 只吃 alias/target/from/to（parseRemap 已拒 endpoint/cred-file）；
		// supports1m/prefer1m 仅 remap model set 吃（effort 侧已在 parseRemap 拒，
		// 不可在此拦——拦了会连 model set 一起拒）。
		if hasAnyOf(cmd.CardName, cmd.ModelID, cmd.UserID, cmd.PolicyVersion) || cmd.ModelsSet || cmd.Refresh ||
			cmd.CredRef != "" || cmd.MaxOutputTokensSet {
			return Cmd{}, usageErrorf("该子命令不接受 --name / --models / --model-id / --user / --policy-version / --refresh")
		}
	case KindProfilesList, KindProfilesShow, KindProfilesCreate,
		KindProfilesUpdate, KindProfilesDelete, KindProfilesDuplicate, KindMaterialize:
		// 旗标已在各自 parse 段按白名单收口（含 --profile/--backup 拒绝）。
		if cmd.Profile != "" || cmd.Backup != "" {
			return Cmd{}, usageErrorf("该子命令不接受 --profile / --backup")
		}
	case KindPreview, KindApply:
		// UC-043：preview / apply 接受 `--model-id`（router 形态下语义 = 选一个**选择器**
		// ——已登记请求映射名或别名名；其余形态沿用既有「策略内模型 / 卡清单」校验），
		// 取值合法性一律由守护进程判（与 REST 同一套校验与文案），CLI 只做形态收口。
		if hasAnyOf(cmd.Endpoint, cmd.CredFile, cmd.CredRef, cmd.Alias, cmd.Target, cmd.CardName,
			cmd.UserID, cmd.PolicyVersion) || cmd.ModelsSet || cmd.Refresh ||
			cmd.Supports1m || cmd.Prefer1m || cmd.MaxOutputTokensSet ||
			cmd.FromSet || cmd.ToSet || cmd.ToolsSet {
			return Cmd{}, usageErrorf("preview / apply 只接受 --profile 与 --model-id" +
				"（其余配置旗标：--endpoint / --cred-file / --cred-ref / --alias / --target / --from / --to" +
				" / --name / --models / --user / --policy-version / --refresh / --supports1m / --prefer1m / --tools）")
		}
	default:
		if cmd.Endpoint != "" || cmd.CredFile != "" || cmd.CredRef != "" || cmd.MaxOutputTokensSet ||
			cmd.Alias != "" || cmd.Target != "" ||
			cmd.FromSet || cmd.ToSet || cmd.CardName != "" || cmd.ModelsSet || cmd.ModelID != "" ||
			cmd.UserID != "" || cmd.PolicyVersion != "" || cmd.Refresh || cmd.Supports1m || cmd.Prefer1m {
			return Cmd{}, usageErrorf("该子命令不接受 --endpoint / --cred-file / --alias / --target / --from / --to" +
				" / --name / --models / --model-id / --user / --policy-version / --refresh" +
				" / --supports1m / --prefer1m")
		}
	}
	return cmd, nil
}

// exactArity 校验位置参数个数（name 用于错误措辞）。
func exactArity(got []string, want int, name string) error {
	if len(got) == want {
		return nil
	}
	return usageErrorf("%s 需要 %d 个参数，得到 %d 个: %q", name, want, len(got), strings.Join(got, " "))
}

// setValueFlag 把取值旗标写进 cmd（--from/--to/--models 记录「出现过」）。
func setValueFlag(cmd *Cmd, flag, value string) error {
	switch flag {
	case "--profile":
		cmd.Profile = value
	case "--backup":
		cmd.Backup = value
	case "--endpoint":
		cmd.Endpoint = value
	case "--cred-file":
		cmd.CredFile = value
	case "--cred-ref":
		cmd.CredRef = value
	case "--max-output-tokens":
		n, err := strconv.Atoi(value)
		if err != nil || n < 0 || n > 1_000_000 {
			return usageErrorf("--max-output-tokens 需要 0..1000000 的整数（0 = 清除声明，仅 update）: %q", value)
		}
		cmd.MaxOutputTokens, cmd.MaxOutputTokensSet = n, true
	case "--alias":
		cmd.Alias = value
	case "--target":
		cmd.Target = value
	case "--from":
		cmd.From, cmd.FromSet = value, true
	case "--to":
		cmd.To, cmd.ToSet = value, true
	case "--name":
		cmd.CardName = value
	case "--models":
		cmd.Models, cmd.ModelsSet = value, true
	case "--model-id":
		cmd.ModelID = value
	case "--user":
		cmd.UserID = value
	case "--policy-version":
		cmd.PolicyVersion = value
	case "--tools":
		cmd.Tools, cmd.ToolsSet = value, true
	default:
		return usageErrorf("未知旗标 %q", flag)
	}
	return nil
}

// hasAnyOf 报告任一字符串旗标非空（旗标白名单的批量判定）。
func hasAnyOf(values ...string) bool {
	for _, v := range values {
		if v != "" {
			return true
		}
	}
	return false
}

// hasAnyFlag 报告是否出现过任何旗标（守护进程形态的「旗标必须与子命令」判定）。
func hasAnyFlag(cmd Cmd) bool {
	return cmd.JSON || cmd.Profile != "" || cmd.Backup != "" ||
		cmd.Endpoint != "" || cmd.CredFile != "" || cmd.Alias != "" || cmd.Target != "" ||
		cmd.Supports1m || cmd.Prefer1m ||
		cmd.FromSet || cmd.ToSet || cmd.CardName != "" || cmd.ModelsSet || cmd.ModelID != "" ||
		cmd.UserID != "" || cmd.PolicyVersion != "" || cmd.Refresh || cmd.ToolsSet
}

// parseRoutes 解析 `routes add|list|show|set|remove`（UC-023）。
//
// 与 preview/apply 的「缺 --profile = 用法错误」不同：`routes add` 缺
// --endpoint/--cred-file **不是**用法错误——注册校验拒绝是业务拒绝（退出码 1，
// UC-023 A4 契约），由守护进程判（缺端点/缺凭据/重复臂名），CLI 不做本地判断。
func parseRoutes(cmd *Cmd, restPos []string) error {
	if len(restPos) == 0 {
		return usageErrorf("routes 需要子命令：add | list | show <name> | set <name> | remove <name>")
	}
	sub := restPos[0]
	args := restPos[1:]
	switch sub {
	case "add":
		if err := exactArity(args, 1, "routes add"); err != nil {
			return err
		}
		cmd.Kind = KindRoutesAdd
		cmd.Name = args[0]
		if cmd.Alias != "" || cmd.Target != "" || cmd.FromSet || cmd.ToSet || cmd.Supports1m || cmd.Prefer1m {
			return usageErrorf("routes add 只接受 --endpoint / --cred-file")
		}
	case "list":
		if err := exactArity(args, 0, "routes list"); err != nil {
			return err
		}
		cmd.Kind = KindRoutesList
		if err := noRouteFlags(cmd, "routes list"); err != nil {
			return err
		}
	case "show":
		if err := exactArity(args, 1, "routes show"); err != nil {
			return err
		}
		cmd.Kind = KindRoutesShow
		cmd.Name = args[0]
		if err := noRouteFlags(cmd, "routes show"); err != nil {
			return err
		}
	case "set":
		if err := exactArity(args, 1, "routes set"); err != nil {
			return err
		}
		cmd.Kind = KindRoutesSet
		cmd.Name = args[0]
		if cmd.Alias != "" || cmd.Target != "" || cmd.FromSet || cmd.ToSet || cmd.Supports1m || cmd.Prefer1m {
			return usageErrorf("routes set 只接受 --endpoint / --cred-file")
		}
	case "remove":
		if err := exactArity(args, 1, "routes remove"); err != nil {
			return err
		}
		cmd.Kind = KindRoutesRemove
		cmd.Name = args[0]
		if err := noRouteFlags(cmd, "routes remove"); err != nil {
			return err
		}
	default:
		return usageErrorf("未知子命令 %q（routes 需要 add | list | show <name> | set <name> | remove <name>）", sub)
	}
	if cmd.Kind == KindRoutesAdd || cmd.Kind == KindRoutesShow ||
		cmd.Kind == KindRoutesSet || cmd.Kind == KindRoutesRemove {
		if strings.TrimSpace(cmd.Name) == "" {
			return usageErrorf("路由名不得为空")
		}
	}
	if cmd.Profile != "" || cmd.Backup != "" {
		return usageErrorf("routes 不接受 --profile / --backup")
	}
	return nil
}

// noRouteFlags 拒绝 routes list|show|remove 上的任何配置旗标。
func noRouteFlags(cmd *Cmd, name string) error {
	if cmd.Endpoint != "" || cmd.CredFile != "" || cmd.Alias != "" || cmd.Target != "" ||
		cmd.FromSet || cmd.ToSet || cmd.Supports1m || cmd.Prefer1m {
		return usageErrorf("%s 不接受配置旗标", name)
	}
	return nil
}

// parseRemap 解析 `remap model set|list` 与 `remap effort set|force|list`（UC-028/029）。
//
// --alias/--target/--from/--to 是**形态必填**（缺旗标 = 用法错误 2，同 preview 的
// --profile 口径）；取值合法性（自映射/链式/白名单档）是业务校验，由守护进程判
// （退出码 1）。「给了空值」与「没给」刻意区分：`--from ”` = 缺省项登记，
// `--to ”` = 清除 force（UC-029 边界）。
func parseRemap(cmd *Cmd, restPos []string) error {
	if len(restPos) < 2 {
		return usageErrorf("remap 需要：model set|list <arm> 或 effort set|force|list <arm>")
	}
	kind, args := restPos[0], restPos[1:]
	switch kind {
	case "model":
		sub, armArgs := args[0], args[1:]
		switch sub {
		case "set":
			if err := exactArity(armArgs, 1, "remap model set"); err != nil {
				return err
			}
			cmd.Kind = KindRemapModelSet
			cmd.Name = armArgs[0]
			if cmd.Alias == "" || cmd.Target == "" {
				return usageErrorf("remap model set 需要 --alias <alias> --target <target>")
			}
			if cmd.FromSet || cmd.ToSet {
				return usageErrorf("remap model set 不接受 --from / --to")
			}
		case "list":
			if err := exactArity(armArgs, 1, "remap model list"); err != nil {
				return err
			}
			cmd.Kind = KindRemapModelList
			cmd.Name = armArgs[0]
			if cmd.Alias != "" || cmd.Target != "" || cmd.FromSet || cmd.ToSet || cmd.Supports1m || cmd.Prefer1m {
				return usageErrorf("remap model list 不接受 --alias / --target / --from / --to / --supports1m / --prefer1m")
			}
		default:
			return usageErrorf("未知子命令 %q（remap model 需要 set | list）", sub)
		}
	case "effort":
		sub, armArgs := args[0], args[1:]
		switch sub {
		case "set":
			if err := exactArity(armArgs, 1, "remap effort set"); err != nil {
				return err
			}
			cmd.Kind = KindRemapEffortSet
			cmd.Name = armArgs[0]
			if !cmd.FromSet || !cmd.ToSet {
				return usageErrorf("remap effort set 需要 --from <src> --to <dst>（--from '' = 缺省项）")
			}
			if cmd.Alias != "" || cmd.Target != "" || cmd.Supports1m || cmd.Prefer1m {
				return usageErrorf("remap effort set 不接受 --alias / --target / --supports1m / --prefer1m")
			}
		case "force":
			if err := exactArity(armArgs, 1, "remap effort force"); err != nil {
				return err
			}
			cmd.Kind = KindRemapEffortForce
			cmd.Name = armArgs[0]
			if !cmd.ToSet {
				return usageErrorf("remap effort force 需要 --to <dst>（--to '' = 清除 force）")
			}
			if cmd.Alias != "" || cmd.Target != "" || cmd.FromSet || cmd.Supports1m || cmd.Prefer1m {
				return usageErrorf("remap effort force 不接受 --alias / --target / --from / --supports1m / --prefer1m")
			}
		case "list":
			if err := exactArity(armArgs, 1, "remap effort list"); err != nil {
				return err
			}
			cmd.Kind = KindRemapEffortList
			cmd.Name = armArgs[0]
			if cmd.Alias != "" || cmd.Target != "" || cmd.FromSet || cmd.ToSet || cmd.Supports1m || cmd.Prefer1m {
				return usageErrorf("remap effort list 不接受 --alias / --target / --from / --to / --supports1m / --prefer1m")
			}
		default:
			return usageErrorf("未知子命令 %q（remap effort 需要 set | force | list）", sub)
		}
	default:
		return usageErrorf("未知子命令 %q（remap 需要 model | effort）", kind)
	}
	if cmd.Profile != "" || cmd.Backup != "" || cmd.Endpoint != "" || cmd.CredFile != "" {
		return usageErrorf("remap 不接受 --profile / --backup / --endpoint / --cred-file")
	}
	return nil
}

// parseProfiles 解析 `profiles list|show|create|update|delete|duplicate`（UC-031/032）。
//
// 与 routes 同口径：缺 --name / 缺三要素**不是**用法错误——建卡校验是业务拒绝
// （退出码 1），由守护进程判；CLI 只做形态收口（旗标白名单与位置参数个数）。
func parseProfiles(cmd *Cmd, restPos []string) error {
	if len(restPos) == 0 {
		return usageErrorf("profiles 需要子命令：list <tool> | show <tool> <pid> | create <tool> | update <tool> <pid> | delete <tool> <pid> | duplicate <tool> <pid>")
	}
	sub := restPos[0]
	args := restPos[1:]
	if cmd.Profile != "" || cmd.Backup != "" {
		return usageErrorf("profiles 不接受 --profile / --backup")
	}
	// 旗标白名单按动作收口（--json 天然放行；其余配置旗标混用一律用法错误）。
	allowed := func(name string, ok func(*Cmd) bool) error {
		if !ok(cmd) {
			return usageErrorf("%s 不接受该配置旗标（逐项见 `yzagmgr help` 的 profiles 段）", name)
		}
		return nil
	}
	noConfig := func(c *Cmd) bool {
		return !hasAnyOf(c.Endpoint, c.CredFile, c.CredRef, c.Alias, c.Target, c.CardName, c.ModelID,
			c.From, c.To, c.Models, c.UserID, c.PolicyVersion) &&
			!c.FromSet && !c.ToSet && !c.ModelsSet && !c.Refresh && !c.Supports1m && !c.Prefer1m &&
			!c.MaxOutputTokensSet
	}
	switch sub {
	case "list":
		if err := exactArity(args, 1, "profiles list"); err != nil {
			return err
		}
		cmd.Kind = KindProfilesList
		cmd.Tool = args[0]
		return allowed("profiles list", noConfig)
	case "show":
		if err := exactArity(args, 2, "profiles show"); err != nil {
			return err
		}
		cmd.Kind = KindProfilesShow
		cmd.Tool, cmd.CardID = args[0], args[1]
		return allowed("profiles show", noConfig)
	case "create":
		if err := exactArity(args, 1, "profiles create"); err != nil {
			return err
		}
		cmd.Kind = KindProfilesCreate
		cmd.Tool = args[0]
		if cmd.CredFile != "" && cmd.CredRef != "" {
			return usageErrorf("--cred-file 与 --cred-ref 只能给一个（明文入托管库，或引用既有托管凭据）")
		}
		return allowed("profiles create", func(c *Cmd) bool {
			return c.ModelID == "" && c.UserID == "" && c.PolicyVersion == "" && !c.ToSet && !c.Refresh &&
				c.Alias == "" && c.Target == "" && !c.Supports1m && !c.Prefer1m
		})
	case "update":
		if err := exactArity(args, 2, "profiles update"); err != nil {
			return err
		}
		cmd.Kind = KindProfilesUpdate
		cmd.Tool, cmd.CardID = args[0], args[1]
		if cmd.CredFile != "" && cmd.CredRef != "" {
			return usageErrorf("--cred-file 与 --cred-ref 只能给一个（明文入托管库，或引用既有托管凭据）")
		}
		return allowed("profiles update", func(c *Cmd) bool {
			return !c.FromSet && c.UserID == "" && c.PolicyVersion == "" && !c.ToSet && !c.Refresh &&
				c.Alias == "" && c.Target == "" && !c.Supports1m && !c.Prefer1m
		})
	case "delete":
		if err := exactArity(args, 2, "profiles delete"); err != nil {
			return err
		}
		cmd.Kind = KindProfilesDelete
		cmd.Tool, cmd.CardID = args[0], args[1]
		return allowed("profiles delete", noConfig)
	case "duplicate":
		if err := exactArity(args, 2, "profiles duplicate"); err != nil {
			return err
		}
		cmd.Kind = KindProfilesDuplicate
		cmd.Tool, cmd.CardID = args[0], args[1]
		return allowed("profiles duplicate", noConfig)
	default:
		return usageErrorf("未知子命令 %q（profiles 需要 list | show | create | update | delete | duplicate）", sub)
	}
}

// UsageText 是 `yzagmgr help` 的全文（doc/cli.md 与之同文）。
func UsageText() string {
	return `yzagmgr —— yzagmgr 管理 CLI（经本地管理 API 管理被管工具的配置文件）

用法:
  yzagmgr                                启动守护进程（等价于 yzagmgr serve；LaunchServices/托盘形态）
  yzagmgr serve                          启动守护进程（管理 API + 本地代理 + 用量库）
  yzagmgr help                           显示本帮助

  yzagmgr health [--json]                健康状态（daemon / 代理 / usageDb）
  yzagmgr tools list [--json]            列出全部受管工具
  yzagmgr tools show <tool> [--json]     单个工具详情（含档位卡清单）
  yzagmgr preview <tool> --profile <pid> [--model-id <m>] [--json]   预览一次应用（纯计算，不落盘）
  yzagmgr apply <tool> --profile <pid> [--model-id <m>] [--json]     应用一张档位卡（含写前备份语义）
                                       --model-id：请求映射形态（--profile yzagmgr-router）下
                                       是**选择器名**（已登记请求映射名或别名名，REST 面见
                                       GET /routes/names）；其余形态是策略内/卡清单内模型
  yzagmgr restore [--backup <id>] [--json]          恢复备份（缺省 = 撤销最近一次写入）
  yzagmgr backups list [--json]          列出全部备份（跨工具，新 → 旧）
  yzagmgr policy show [--json]           显示当前模型策略
  yzagmgr policy refresh [--json]        从网关刷新模型策略

  yzagmgr routes add <name> --endpoint <url> --cred-file <0600文件> [--json]
                                       登记一条请求映射（多映射模型，UC-023）
                                       凭据明文只经 0600 文件交付；守护进程收进托管
                                       凭据库，配置只落引用（yzagmgr-managed-token://<uid>）
  yzagmgr routes list [--json]           列出全部请求映射（name/endpoint/credRef）
  yzagmgr routes show <name> [--json]    单条请求映射详情
  yzagmgr routes set <name> [--endpoint <url>] [--cred-file <0600文件>] [--json]
                                       改一条请求映射的端点与/或凭据（空值 = 保持原值）
  yzagmgr routes remove <name> [--json]  移除一条请求映射（连带其 remap 表）
  yzagmgr remap model set <arm> --alias <alias> --target <target>
                             [--supports1m] [--prefer1m] [--json]
                                       登记模型映射 alias→target（单层，UC-028）；
                                       能力位仅显式给出时随映射登记（UC-030，缺席 = 不声明）
  yzagmgr remap model list <arm> [--json] 列出该请求映射的模型映射
  yzagmgr remap effort set <arm> --from <src> --to <dst> [--json]
                                       登记 effort 映射（单跳，UC-029；--from '' = 缺省项）
  yzagmgr remap effort force <arm> --to <dst> [--json]
                                       force 形态：入参任意恒发 <dst>（--to '' = 清除）
  yzagmgr remap effort list <arm> [--json] 列出该请求映射的 effort 映射表与 forceEffort

  yzagmgr profiles list <tool> [--json]  列出全部配置（官方/网关下发/个人）
  yzagmgr profiles show <tool> <pid> [--json]
                                       单条配置详情（组合视图：自列表按 id 取原文）
  yzagmgr profiles create <tool> --name <n> [--from <pid>]
                             [--endpoint <url>] [--cred-file <0600文件> | --cred-ref <ref>]
                             [--models a,b,c] [--max-output-tokens <n>] [--json]
                                       新建个人配置（UC-032：端点/凭据引用/模型清单三要素；
                                       凭据明文只经 0600 文件交付，卡只落引用）
  yzagmgr profiles update <tool> <pid> [--name <n>] [--model-id <m>]
                             [--endpoint <url>] [--cred-file <0600文件> | --cred-ref <ref>]
                             [--models a,b,c] [--max-output-tokens <n>] [--json]
                                       改个人配置（--models '' = 清空模型清单）
  yzagmgr profiles delete <tool> <pid> [--json]      删除个人配置（非个人配置或当前生效被拒）
  yzagmgr profiles duplicate <tool> <pid> [--json]   复制个人配置（名称加后缀、不切当前生效项）
  yzagmgr materialize [--refresh] [--json]           配置物化（缺省 = 只读盘点；--refresh = 重新物化）

环境变量（与守护进程同语义）:
  YZAGMGR_HOME         隔离总闸：数据根与被管工具 $HOME 基准
  YZAGMGR_API_PORT     管理 API 端口（默认 17022）

管理令牌自动读自 <data_root>/admin.token（存在即复用、缺失才生成，0600，无需手填；
health 为免鉴权端点，令牌缺失时照常可用）。守护进程未运行时 CLI **不会**自动拉起
它——先运行 yzagmgr serve（或启动 yzagmgr.app）再执行命令。

退出码:
  0 成功 / 1 API 或业务失败 / 2 用法错误 / 3 管理令牌缺失 / 4 管理 API 不可达
`
}

// Deps 是 Exec/Main 的全部外部依赖（测试注入点；生产用 DefaultDeps）。
type Deps struct {
	Out, Err io.Writer
	// LoadEnv 解析运行环境（YZAGMGR_HOME 等）；默认 config.FromProcess。
	LoadEnv func() (*config.Env, error)
	// HTTP 是 REST 客户端；默认 30s 超时的 http.DefaultTransport 客户端。
	HTTP *http.Client
	// BaseURL 覆盖管理 API 基址（仅测试用）；默认 http://127.0.0.1:<YZAGMGR_API_PORT>。
	BaseURL string
}

// DefaultDeps 组装生产依赖。
func DefaultDeps() Deps {
	return Deps{
		Out:     os.Stdout,
		Err:     os.Stderr,
		LoadEnv: config.FromProcess,
		HTTP:    &http.Client{Timeout: 30 * time.Second},
	}
}

func (d Deps) withDefaults() Deps {
	def := DefaultDeps()
	if d.Out == nil {
		d.Out = def.Out
	}
	if d.Err == nil {
		d.Err = def.Err
	}
	if d.LoadEnv == nil {
		d.LoadEnv = def.LoadEnv
	}
	if d.HTTP == nil {
		d.HTTP = def.HTTP
	}
	return d
}

// Main 是 CLI 的总入口：解析参数，守护进程形态转交 daemon（= cmd/yzagmgr 的 run()，
// 行为与旧版无参数启动完全一致），其余执行子命令并返回退出码。
func Main(args []string, daemon func() error, d Deps) int {
	d = d.withDefaults()
	cmd, err := Parse(args)
	if err != nil {
		fmt.Fprintln(d.Err, err)
		fmt.Fprintln(d.Err, "运行 `yzagmgr help` 查看用法。")
		return ExitUsage
	}
	switch cmd.Kind {
	case KindDaemon:
		if daemon == nil {
			return failf(d, cmd, ExitAPI, "internal", "守护进程入口未提供")
		}
		if err := daemon(); err != nil {
			fmt.Fprintf(d.Err, "yzagmgr 启动失败: %v\n", err)
			return ExitAPI
		}
		return ExitOK
	case KindHelp:
		fmt.Fprint(d.Out, UsageText())
		return ExitOK
	}
	return Exec(cmd, d)
}

// Exec 执行一个已解析的 CLI 命令（KindDaemon/KindHelp 由 Main 处理）。
func Exec(cmd Cmd, d Deps) int {
	d = d.withDefaults()

	port, err := config.PortFromEnv("YZAGMGR_API_PORT", config.DefaultAPIPort)
	if err != nil {
		return failf(d, cmd, ExitUsage, "invalid_request", "%v", err)
	}
	base := d.BaseURL
	if base == "" {
		base = fmt.Sprintf("http://127.0.0.1:%d", port)
	}

	// 令牌复用守护进程的读取逻辑（config.Env.ReadAdminToken）。health 是契约免鉴权
	// 端点（§1.1），令牌缺失照常执行；其余命令一律要求令牌在场且非空。
	token := ""
	env, err := d.LoadEnv()
	if err != nil {
		return failf(d, cmd, ExitAPI, "internal", "解析运行环境失败: %v", err)
	}
	tok, terr := env.ReadAdminToken()
	if terr == nil && strings.TrimSpace(tok) != "" {
		token = strings.TrimSpace(tok)
	} else if cmd.Kind != KindHealth {
		if terr != nil {
			return failf(d, cmd, ExitToken, "token_missing",
				"管理令牌不可读（%s）：守护进程可能未运行（令牌在启动时生成），或数据根不对（检查 YZAGMGR_HOME）。原始错误: %v",
				env.AdminTokenFile(), terr)
		}
		return failf(d, cmd, ExitToken, "token_missing", "管理令牌文件为空: %s", env.AdminTokenFile())
	}

	c := &Client{Base: base, Token: token, HTTP: d.HTTP}
	switch cmd.Kind {
	case KindHealth:
		return execHealth(d, cmd, c)
	case KindToolsList:
		return execToolsList(d, cmd, c)
	case KindToolsShow:
		return execToolsShow(d, cmd, c)
	case KindPreview, KindApply:
		return execPreviewApply(d, cmd, c)
	case KindRestore:
		return execRestore(d, cmd, c)
	case KindBackupsList:
		return execBackupsList(d, cmd, c)
	case KindPolicyShow, KindPolicyRefresh:
		return execPolicy(d, cmd, c)
	case KindRoutesAdd, KindRoutesList, KindRoutesShow, KindRoutesSet, KindRoutesRemove:
		return execRoutes(d, cmd, c)
	case KindRemapModelSet, KindRemapModelList, KindRemapEffortSet, KindRemapEffortForce, KindRemapEffortList:
		return execRemap(d, cmd, c)
	case KindProfilesList, KindProfilesShow, KindProfilesCreate, KindProfilesUpdate,
		KindProfilesDelete, KindProfilesDuplicate:
		return execProfiles(d, cmd, c)
	case KindMaterialize:
		return execMaterialize(d, cmd, c)
	}
	return failf(d, cmd, ExitUsage, "usage", "未实现的子命令（内部错误）")
}

// failf 输出一次失败并返回退出码：--json 形态输出 REST 同形失败信封（stdout），
// 人类形态输出 `yzagmgr: <code>: <message>`（stderr，code 与 REST 错误码同域，
// 便于脚本按码分支）。**两种形态都写 stderr 人类行**：本地前置失败（令牌缺失等）
// 即使在 --json 下也必须在 stderr 给出含关键词的明确错误（cli-parity TC-UC031-A3
// 的无令牌判据读的是 stderr）；stdout 信封保持逐字节契约不动。
// 用法错误（Parse 失败）不受 --json 影响（那时旗标未必解析成功），
// 恒为 stderr 文本 + 退出码 2。
func failf(d Deps, cmd Cmd, exit int, code, format string, args ...any) int {
	msg := fmt.Sprintf(format, args...)
	if cmd.JSON {
		envelope := map[string]any{
			"ok":    false,
			"error": map[string]any{"code": code, "message": msg},
		}
		blob, _ := json.Marshal(envelope)
		fmt.Fprintln(d.Out, string(blob))
	}
	fmt.Fprintf(d.Err, "yzagmgr: %s: %s\n", code, msg)
	return exit
}

// emit 输出一次成功结果：--json 形态是 REST 同形信封 `{"ok":true,"data":<载荷>}`
// （单发命令的 data 逐字节保留 REST 原文；组合命令的 data 是 CLI 组装的对象），
// 人类形态交给各命令的格式化函数。
//
// **信封手工拼接，绝不过 json.Marshal**：`json.Marshal` 会对 `json.RawMessage` 做
// compact，把载荷里的空白全部压平——「原文逐字节保留」就此被悄悄破坏，而且用规范
// 空白的载荷**测不出来**（与 cmd/yzagmgr 的 seedPolicyDocument 同一纪律、同一陷阱）。
// 拼接前先 json.Valid 把关：非法原文绝不外发。
func emit(d Deps, cmd Cmd, data json.RawMessage, human func(json.RawMessage) (string, error)) int {
	if cmd.JSON {
		if !json.Valid(data) {
			return failf(d, cmd, ExitAPI, "internal", "结果序列化失败: data 不是合法 JSON")
		}
		var buf bytes.Buffer
		buf.WriteString(`{"ok":true,"data":`)
		buf.Write(data)
		buf.WriteString("}\n")
		fmt.Fprint(d.Out, buf.String())
		return ExitOK
	}
	s, err := human(data)
	if err != nil {
		return failf(d, cmd, ExitAPI, "internal", "解析响应失败: %v", err)
	}
	fmt.Fprint(d.Out, s)
	return ExitOK
}
