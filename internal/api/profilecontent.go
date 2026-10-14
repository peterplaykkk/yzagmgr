// §4.6 配置内容（UC-039 的「配置详情」面）：GET /tools/{id}/profiles/{pid}/content。
//
// 为什么它与物化（§4.4）共用一条计算路径：两者回答的是**同一个问题**——「这张卡启用时
// 会写出什么字节」。物化把它写到 `<Home>/.yzagmgr/materialize/<agent>/<card>/`，本端点把它原样交给
// 界面。任何「为了展示再算一遍」的写法都会让「看到的」与「apply 会写的」悄悄分叉，
// 因此这里只做三件事：调 [`planForProfile`] → 取计划里的目标字节 → 脱敏后包信封。
//
// 三条纪律：
//  1. **零副作用**（A4）：不触盘、不建租约、不动物化目录、不改 live（前后 sha 不变）。
//  2. **sha256 是真实字节的哈希**（不是脱敏后文本的哈希）：它与 apply 之后盘上文件的
//     sha256 相等，界面据此能把「详情页看到的」与「盘上那份」对起来。
//  3. **响应正文里不得出现凭据明文**：`content` 一律先过 [`maskContent`]（§4.6 的
//     细粒度口径：替身 `***`、结构化容器继续下钻、只在凭据叶子处掩；预览差异行仍走
//     §4.3 的 [`maskCredentials`]），键名与形状保留。
package api

import (
	"context"
	"net/http"
	"path/filepath"
	"strings"

	"github.com/peterplaykkk/yzagmgr/internal/apierr"
	"github.com/peterplaykkk/yzagmgr/internal/family"
	"github.com/peterplaykkk/yzagmgr/internal/tx"
)

// profileContentFile 是契约 §4.6 的逐文件条目。
//
// `bytes` / `sha256` 取自**目标字节**（未脱敏的真实字节），`content` 是脱敏后的正文：
// 三者刻意不同源，因为消费方要回答的是两个不同问题——「写出来的会是什么」（sha/bytes）
// 与「里面写了什么」（content，密钥位已替换）。
type profileContentFile struct {
	Path    string `json:"path"`
	Bytes   int    `json:"bytes"`
	SHA256  string `json:"sha256"`
	Content string `json:"content"`
}

// profileContentPayload 是 §4.6 的响应形状。
//
// `masked:true` / `writes:false` 是这个端点的**自述**，让消费方不必靠约定记忆
// 「这里的 content 是脱敏的」与「调它不会写盘」：
//   - `masked` 恒 true（凭据值已替换为 `<已脱敏>`）；
//   - `writes` 恒 false（纯读端点，永不写 live）。
type profileContentPayload struct {
	ProfileID string               `json:"profileId"`
	Masked    bool                 `json:"masked"`
	Writes    bool                 `json:"writes"`
	Files     []profileContentFile `json:"files"`
}

func (s *Server) handleProfileContent(r *http.Request) (any, error) {
	id, err := s.requireFamily(r.PathValue("id"))
	if err != nil {
		return nil, err
	}
	pid := r.PathValue("pid")
	plans, err := s.plansForCard(r.Context(), id, pid)
	if err != nil {
		return nil, err
	}
	return profileContentPayload{
		ProfileID: pid,
		Masked:    true,
		Writes:    false,
		Files:     contentFilesOf(plans),
	}, nil
}

// plansForCard 把一张卡翻译成文件计划（走 planForProfile）。
// （下发卡分支已随登录下发删除——design §2.2。）
//
// WorkBuddy 与预览端点同一条边界：它有独立写链（`/workbuddy/*`），不吃「卡片 → 计划」
// 这条路（物化也把它排除在外），因此显式拒绝而不是在这里顺手算出第四种语义。
func (s *Server) plansForCard(ctx context.Context, id family.ID, pid string) ([]family.FilePlan, error) {
	if proxyOnlyFamily(id) {
		return nil, apierr.New(apierr.CodeNotSupported,
			"WorkBuddy 走独立入口（/workbuddy/*），不支持工具通道的配置内容与测试")
	}
	return s.planForProfile(ctx, id, pid, "")
}

// contentFilesOf 把计划翻成逐文件条目。
//
// **只列会被写出的文件**（`Delete` 项剔除）：`Delete` 表达的是「这个文件将被移除」，
// 它不是内容，而响应形状里没有「这是删除」的表达位，列出来只会让界面显示一个空文件。
// 与 [`plannedContent`]（物化取字节的那一处）同一条判据，两处因此不会各说一套。
// 其余项照列，含目标字节为空的项（那如实表示「这一档在这个文件上不写任何字节」）。
func contentFilesOf(plans []family.FilePlan) []profileContentFile {
	out := make([]profileContentFile, 0, len(plans))
	for _, plan := range plans {
		if plan.Delete {
			continue
		}
		out = append(out, profileContentFile{
			Path:    plan.Path,
			Bytes:   len(plan.After),
			SHA256:  tx.HashBytes(plan.After),
			Content: maskContent(string(plan.After)),
		})
	}
	return out
}

// ── 临时隔离根（§4.6 测试端点复用） ─────────────────────────────────────────

// tempTreeNames 给出「临时根下的目标相对路径」：**保留 live 路径相对 $HOME 的结构**。
//
// 这与物化目录的扁平命名（[`materializedNames`]，按 basename 落一层）刻意不同，
// 因为两者的目标读者不同：物化目录是给人看、可整体搬走的**展示副本**，而临时根是给
// CLI 用的**假 $HOME**——CLI 按 `$HOME/.pi/agent/models.json` 这类固定相对位置找配置，
// 拍平了它一个文件都读不到（`HOME=<临时根>` 的隔离就毫无意义）。
//
// 于是规则只有一条：`<Home>/<rest>` ⇒ `<临时根>/<rest>`。受管路径不在 $HOME 下时
// 明确报错而不是就地拍平：那种路径没有「换个 HOME 还能被找到」的对应物，猜一个位置
// 会让 CLI 读到空配置而测试假装成功。
func tempTreeNames(home string, plans []family.FilePlan) (map[string]string, error) {
	names := make(map[string]string, len(plans))
	for _, plan := range plans {
		if plan.Delete {
			continue
		}
		rel, err := relativeUnderHome(home, plan.Path)
		if err != nil {
			return nil, err
		}
		names[plan.Path] = rel
	}
	return names, nil
}

// relativeUnderHome 求受管文件相对 $HOME 的路径（`<Home>/<rest>` ⇒ `<rest>`）。
func relativeUnderHome(home, path string) (string, error) {
	rel, err := filepath.Rel(home, path)
	if err != nil || rel == "." || rel == ".." ||
		strings.HasPrefix(rel, ".."+string(filepath.Separator)) || filepath.IsAbs(rel) {
		return "", apierr.New(apierr.CodeNotSupported,
			"受管文件不在主目录下（%s），无法在临时隔离根里复现它的位置", path)
	}
	return filepath.ToSlash(rel), nil
}
