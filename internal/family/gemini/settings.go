package gemini

import (
	"bytes"

	"github.com/peterplaykkk/yzagmgr/internal/apierr"
	"github.com/peterplaykkk/yzagmgr/internal/docmodel"
)

// Gemini CLI 用 `security.auth.selectedType` 选认证方式：网关档需要它指向
// 「用 .env 里的 API Key」，官方档则不留本产品的选择（键缺席）。
const (
	authRootKey = "security"
	authSubKey  = "auth"
	authTypeKey = "selectedType"
)

// gatewaySelectedType 是网关档落在 `security.auth.selectedType` 上的取值。
// 取值来源 = v1 的 `write_packycode_settings()`（`gemini_config.rs`），
// 盘上见证 = 本机 `~/.gemini/settings.json`（82 B，只有这一条链）。
const gatewaySelectedType = "gemini-api-key"

// setSelectedType 返回把 `security.auth.selectedType` 设为 value 后的目标字节。
//
//   - 文件缺席或只有空白：按**规范形态**新建（键排序 + 2 空格缩进 + 尾换行，与 v1 盘上
//     82 B 形态同形，docmodel.CanonicalJSON）；
//   - 文件在场：只动该键，其余字节逐字保留（用户的 mcpServers / theme 等原样存活）；
//   - 链上父级在场但不是对象：拒绝写入（config_invalid）——不猜、不覆盖用户的结构。
func setSelectedType(src []byte, value string) ([]byte, error) {
	if len(bytes.TrimSpace(src)) == 0 {
		return docmodel.CanonicalJSON(map[string]any{
			authRootKey: map[string]any{
				authSubKey: map[string]any{authTypeKey: value},
			},
		})
	}
	doc, root, err := parseSettingsObject(src)
	if err != nil {
		return nil, err
	}

	security := root.MemberOf(authRootKey)
	if security == nil {
		// security 整条缺席：一次插入补齐我们需要的两支。
		if err := doc.SetJSON(map[string]any{
			authSubKey: map[string]any{authTypeKey: value},
		}, authRootKey); err != nil {
			return nil, settingsEditFailed(err)
		}
		return doc.Render(), nil
	}
	if security.Val.Kind != docmodel.KObject {
		return nil, settingsNotObject(authRootKey)
	}
	auth := security.Val.MemberOf(authSubKey)
	if auth == nil {
		if err := doc.SetJSON(map[string]any{authTypeKey: value},
			authRootKey, authSubKey); err != nil {
			return nil, settingsEditFailed(err)
		}
		return doc.Render(), nil
	}
	if auth.Val.Kind != docmodel.KObject {
		return nil, settingsNotObject(authRootKey + "." + authSubKey)
	}
	if err := doc.SetJSON(value, authRootKey, authSubKey, authTypeKey); err != nil {
		return nil, settingsEditFailed(err)
	}
	return doc.Render(), nil
}

// clearSelectedType 移除托管键 `security.auth.selectedType`，返回目标字节，并报告
// 「删完之后整份文档没有任何成员 ⇒ 该文件应被视为不存在（由调用方发 Delete 计划）」。
//
// 只有空白字节的输入（`""` / `"\n"`）没有任何可删的托管内容，原样返回；
// 网关档会把它当成「缺席文件」写成规范形态，随后官方档按「文档已空」把它删掉——
// 这类文件本身不含用户内容，故不构成用户字节损失。
//
// 删除层级取**最外层可删的祖先**：只有当链上每一层在删掉这一支后都会变空时才向上走一层。
// 这样用户自己的兄弟键逐字节存活，也不会留下 `{"security": {}}` 这类空壳垃圾（UC-005 A4）。
func clearSelectedType(src []byte) ([]byte, bool, error) {
	if len(bytes.TrimSpace(src)) == 0 {
		// 文件缺席或只有空白：没有任何可删的托管内容，一个字节都不动。
		return src, false, nil
	}
	doc, root, err := parseSettingsObject(src)
	if err != nil {
		return nil, false, err
	}
	security := root.MemberOf(authRootKey)
	if security == nil || security.Val.Kind != docmodel.KObject {
		return src, false, nil
	}
	auth := security.Val.MemberOf(authSubKey)
	if auth == nil || auth.Val.Kind != docmodel.KObject {
		return src, false, nil
	}
	if auth.Val.MemberOf(authTypeKey) == nil {
		return src, false, nil
	}

	target := []string{authRootKey, authSubKey, authTypeKey}
	drop := false
	if auth.Val.Len() == 1 {
		target = []string{authRootKey, authSubKey}
		if security.Val.Len() == 1 {
			target = []string{authRootKey}
			// 根上只剩这一支 ⇒ 文档随即变空：此时「托管键消失」的精确等价物是文件消失。
			drop = root.Len() == 1
		}
	}
	if err := doc.Delete(target...); err != nil {
		return nil, false, settingsEditFailed(err)
	}
	return doc.Render(), drop, nil
}

// parseSettingsObject 解析 settings.json 并断言顶层是对象。
func parseSettingsObject(src []byte) (*docmodel.Doc, *docmodel.Node, error) {
	doc, err := docmodel.ParseJSON(src)
	if err != nil {
		// 拒绝写入的文案指向**真实存在**的恢复路径（UC-003 修订的非法配置恢复面）：
		// 备份恢复与带快照的重置预览；旧文案「请先修复或走全量重置」教手改、且指向一个
		// 不存在的操作，2026-09-27 清理。
		return nil, nil, apierr.New(apierr.CodeConfigInvalid,
			"Gemini settings.json 不是合法 JSON，已拒绝写入（可从备份恢复，或先做带快照的重置预览；不必手改原文）: %v", err).
			WithDetails(map[string]any{"reason": "invalid_json"})
	}
	root := doc.Root()
	if root.Kind != docmodel.KObject {
		return nil, nil, apierr.New(apierr.CodeConfigInvalid,
			"Gemini settings.json 顶层不是对象，已拒绝写入").
			WithDetails(map[string]any{"reason": "root_not_object"})
	}
	return doc, root, nil
}

// settingsNotObject 报告链上某个父级不是对象（用户结构与我们预期不符）。
func settingsNotObject(path string) error {
	return apierr.New(apierr.CodeConfigInvalid,
		"Gemini settings.json 的 %s 不是对象，已拒绝写入", path).
		WithDetails(map[string]any{"reason": "parent_not_object", "path": path})
}

// settingsEditFailed 把 docmodel 的编辑失败归一为 config_invalid。
func settingsEditFailed(err error) error {
	return apierr.Wrap(apierr.CodeConfigInvalid, err, "Gemini settings.json 编辑失败")
}
