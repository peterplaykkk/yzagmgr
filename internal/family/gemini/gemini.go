// Package gemini 实现 Gemini CLI 受管单元（family.Family）。
//
// 落盘契约以 `.agents/skills/yzagmgr-config-layer/references/acceptance-recipe.md`
// §A 行 4 为唯一事实源：
//
//   - 两个受管文件：`$HOME/.gemini/.env`（dotenv）与 `$HOME/.gemini/settings.json`；
//   - 决定档位的键 = `.env` 的 `GOOGLE_GEMINI_BASE_URL`，网关档取值 = `{origin}/v1beta`；
//   - 官方档 = 这三个 env 键**不存在**（官方种子 `gemini-official` 的
//     `settings_config = {"env":{},"config":{}}`：env 为空即三个键都不在，
//     config 为空即 settings.json 里不留本产品的选择）；
//   - 本族**没有**窗口/输出/图像的托管键（§A 行 4 登记为「不支持（未证实）」与
//     `unknown`）⇒ 不伪造任何此类键，也不写 effort 键。
//
// 本族是**确有真令牌落盘**的家族：`GEMINI_API_KEY` 写真实推理令牌（与 claude 的
// 本地 route 占位符形态不同）。令牌只经 GatewayInput.Token 传入并入盘，
// 不进错误详情、不进日志。
//
// 字节保真：`.env` 的读写经 `docmodel` 的 dotenv 定位层（只改命中行的值段/整行，
// 注释、空行、行序、CRLF 与用户自有键逐字节保留），settings.json 经
// `docmodel` 的 JSON 定位层（只改命中成员的区间）。
package gemini

import (
	"bytes"
	"fmt"
	"net/url"
	"strings"

	"github.com/peterplaykkk/yzagmgr/internal/apierr"
	"github.com/peterplaykkk/yzagmgr/internal/config"
	"github.com/peterplaykkk/yzagmgr/internal/docmodel"
	"github.com/peterplaykkk/yzagmgr/internal/family"
	"github.com/peterplaykkk/yzagmgr/internal/tx"
)

const (
	// displayName 与 v1 的 TOOL_CATALOG 一致。
	displayName = "Gemini CLI"

	// 三个托管 env 键：决定键 + 凭据 + 模型（§A 行 4 的「网关档值形态」）。
	decideEnvKey = "GOOGLE_GEMINI_BASE_URL"
	apiKeyEnvKey = "GEMINI_API_KEY"
	modelEnvKey  = "GEMINI_MODEL"

	// gatewayBaseSuffix 是网关档基址后缀：`{origin}/v1beta`。
	gatewayBaseSuffix = "v1beta"
)

// geminiFamily 是 Gemini CLI 的 family.Family 实现（无状态，值语义）。
type geminiFamily struct{}

// New 返回 Gemini CLI 的受管单元实现。
func New() family.Family { return geminiFamily{} }

func (geminiFamily) ID() family.ID { return family.Gemini }

func (geminiFamily) DisplayName() string { return displayName }

// OfficialSeeded：五家有官方种子的家族之一（`gemini-official` =
// `{"env":{},"config":{}}`），官方档可往返。
func (geminiFamily) OfficialSeeded() bool { return true }

// HasLocalLane：该族在代理前缀表内有独立前缀（本地 route 可达），
// 故代理侧需要为它建路由。
func (geminiFamily) HasLocalLane() bool { return true }

// Paths 只返回**文件**（绝不返回目录，§F-T2）：承载决定键的 .env 在前，settings.json 在后。
func (geminiFamily) Paths(env *config.Env) []string {
	return []string{env.GeminiEnv(), env.GeminiSettings()}
}

// Probe 从盘上字节判定档位：只看 .env 的决定键，不看任何内部记账（§F-T5）。
func (geminiFamily) Probe(env *config.Env) (family.ProbeResult, error) {
	envPath := env.GeminiEnv()
	res := family.ProbeResult{DecideKey: decideEnvKey, Path: envPath}

	src, err := tx.ReadForFingerprint(envPath)
	if err != nil {
		return family.ProbeResult{}, err
	}
	if src == nil {
		// 文件缺席 = 未托管，不是故障（§F-T3）。
		res.Tier = family.TierAbsent
		if settings, err := tx.ReadForFingerprint(env.GeminiSettings()); err != nil {
			return family.ProbeResult{}, err
		} else if settings != nil {
			res.Notes = append(res.Notes,
				"settings.json 在场但承载决定键的 .env 缺席：按未托管处理")
		}
		return res, nil
	}

	res.Present = true
	doc, err := docmodel.ParseDotenv(src)
	if err != nil {
		// 解析失败不改变判定口径：读不出决定键就按「键不存在」处理，
		// 但把原因写进 Notes，让上层能报出「该文件需要人工修复」。
		res.Tier = family.TierOfficial
		res.Notes = append(res.Notes, ".env 解析失败，决定键按不存在处理: "+err.Error())
		return res, nil
	}
	value, _ := doc.Get(decideEnvKey)
	res.DecideValue = value
	if n := countAssignments(doc, decideEnvKey); n > 1 {
		res.Notes = append(res.Notes,
			fmt.Sprintf("决定键在 .env 里有 %d 处定义，生效的是最后一条", n))
	}
	if strings.TrimSpace(value) == "" {
		res.Tier = family.TierOfficial
		return res, nil
	}
	res.Tier = family.TierGateway
	if note := foreignEndpointNote(value); note != "" {
		res.Notes = append(res.Notes, note)
	}
	return res, nil
}

// PlanGateway 计算网关档目标字节：`.env` 的三个键 + settings.json 的
// `security.auth.selectedType = "gemini-api-key"`（v1 的 API Key 认证形态）。
//
// 纯计算，不触盘写入。
func (geminiFamily) PlanGateway(env *config.Env, in family.GatewayInput) ([]family.FilePlan, error) {
	base, err := gatewayBase(in.Origin)
	if err != nil {
		return nil, err
	}
	model := in.Model
	if strings.TrimSpace(model) == "" {
		return nil, apierr.New(apierr.CodeInvalidRequest,
			"gemini 网关档需要模型 id（策略 default）")
	}
	// 凭据与模型 id **原样落盘**（只判空、不做 trim）：改写凭据字节是静默事故，
	// 而含换行的值会被 dotenv 定位层显式拒绝（ErrDotenvValueInvalid）。
	token := in.Token
	if strings.TrimSpace(token) == "" {
		return nil, apierr.New(apierr.CodeInvalidRequest,
			"gemini 网关档需要推理令牌（本族真实落盘 GEMINI_API_KEY）")
	}

	envPath, settingsPath := env.GeminiEnv(), env.GeminiSettings()
	envBefore, err := tx.ReadForFingerprint(envPath)
	if err != nil {
		return nil, err
	}
	settingsBefore, err := tx.ReadForFingerprint(settingsPath)
	if err != nil {
		return nil, err
	}

	envAfter, err := writeManagedEnvKeys(envBefore, []struct{ key, value string }{
		{apiKeyEnvKey, token},
		{modelEnvKey, model},
		{decideEnvKey, base},
	})
	if err != nil {
		return nil, err
	}

	settingsAfter, err := setSelectedType(settingsBefore, gatewaySelectedType)
	if err != nil {
		return nil, err
	}
	return tx.BuildPlans(map[string][]byte{
		envPath:      envAfter,
		settingsPath: settingsAfter,
	})
}

// writeManagedEnvKeys 写入三个托管 env 键并返回目标字节。
//
// 键序 = 字母序（与 v1 盘上 `serialize_env_file` 的形态一致）；已存在的键只会改值段——
// 位置、行尾注释与同行其它字节都不动。
func writeManagedEnvKeys(src []byte, pairs []struct{ key, value string }) ([]byte, error) {
	doc, err := docmodel.ParseDotenv(src)
	if err != nil {
		return nil, apierr.Wrap(apierr.CodeConfigInvalid, err,
			"Gemini .env 不是可解析的 dotenv，已拒绝写入")
	}
	for _, kv := range pairs {
		if err := doc.Set(kv.key, kv.value); err != nil {
			return nil, apierr.Wrap(apierr.CodeConfigInvalid, err,
				"Gemini .env 写入失败: %s", kv.key)
		}
	}
	return doc.Render(), nil
}

// PlanOfficial 计算回到官方档的目标字节：三个托管 env 键全部消失，
// settings.json 里的托管选择也消失（官方种子的 `config` 为空 ⇒ 不留本产品的键）。
//
// 当托管键是文件里的**全部**内容时，「键消失」的精确等价物是**文件消失**
// （family.go 的 Delete 语义：恢复时移除接管时创建的路径）。不这样做，一次
// 官方↔网关往返就会留下一个空壳文件，UC-005 A1 的 sha256 逐字节往返断言随即失败。
func (geminiFamily) PlanOfficial(env *config.Env) ([]family.FilePlan, error) {
	envPath, settingsPath := env.GeminiEnv(), env.GeminiSettings()
	envBefore, err := tx.ReadForFingerprint(envPath)
	if err != nil {
		return nil, err
	}
	settingsBefore, err := tx.ReadForFingerprint(settingsPath)
	if err != nil {
		return nil, err
	}

	envAfter, dropEnv, err := removeManagedEnvKeys(envBefore)
	if err != nil {
		return nil, err
	}
	settingsAfter, dropSettings, err := clearSelectedType(settingsBefore)
	if err != nil {
		return nil, err
	}

	plans, err := tx.BuildPlans(map[string][]byte{
		envPath:      envAfter,
		settingsPath: settingsAfter,
	})
	if err != nil {
		return nil, err
	}
	for i := range plans {
		switch {
		case plans[i].Path == envPath && dropEnv && plans[i].Existed:
			plans[i].Delete = true
			plans[i].After = nil
		case plans[i].Path == settingsPath && dropSettings && plans[i].Existed:
			plans[i].Delete = true
			plans[i].After = nil
		}
	}
	return plans, nil
}

// removeManagedEnvKeys 删除三个托管 env 键的全部定义行，并报告
// 「删完之后只剩空白 ⇒ 该文件应被视为不存在」。
func removeManagedEnvKeys(src []byte) ([]byte, bool, error) {
	if src == nil {
		return nil, false, nil
	}
	doc, err := docmodel.ParseDotenv(src)
	if err != nil {
		return nil, false, apierr.Wrap(apierr.CodeConfigInvalid, err,
			"Gemini .env 不是可解析的 dotenv，已拒绝写入")
	}
	for _, key := range []string{apiKeyEnvKey, modelEnvKey, decideEnvKey} {
		if err := doc.Delete(key); err != nil {
			return nil, false, apierr.Wrap(apierr.CodeConfigInvalid, err,
				"Gemini .env 移除失败: %s", key)
		}
	}
	if !doc.Dirty() {
		// 键本就不在：一个字节都不动（幂等，且不把用户文件重写一遍）。
		return src, false, nil
	}
	out := doc.Render()
	return out, isBlankOnly(out), nil
}

// countAssignments 数出 `KEY=VALUE` 行里该键的出现次数。
func countAssignments(doc *docmodel.EnvDoc, key string) int {
	n := 0
	for _, line := range doc.Lines() {
		if line.Kind == docmodel.EnvAssignment && line.Key == key {
			n++
		}
	}
	return n
}

// gatewayBase 返回网关档的 .env 基址：`{origin}/v1beta`。
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
		// 本地车道（本地 route 形态）属正常形态。
		return ""
	}
	return "决定键指向的端点不是受管网关域: " + host
}

// isBlankOnly 报告字节串是否只剩空白。注释算内容：它是用户字节，
// 不得据「删完只剩注释」把用户的文件删掉。
func isBlankOnly(b []byte) bool {
	for _, line := range bytes.Split(b, []byte("\n")) {
		if len(bytes.TrimSpace(line)) > 0 {
			return false
		}
	}
	return true
}
