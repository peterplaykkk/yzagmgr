package usagescript

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"time"

	"github.com/peterplaykkk/yzagmgr/internal/apierr"
)

// 脚本持久化（UC-016 A14）：`<data root>/usage-scripts/<tool>__<profile>.json`，0600。
//
// 为什么存 JSON 而不是裸 .js：导出要自证身份（属于哪把档、什么时候写的），
// 导入才能校验归属；文件本身就是「随该档导出/导入」的产物（A14）。
// 调用方用 config.Env.UsageScriptsDir() 作为 dir。

const storeFormat = "yzagmgr2-usage-script/1"

// storeKeyRe 限定工具 id 与档位 id 的字符集：它们会拼进文件名，
// 放行 `/`、`..`、前导 `.` 就等于放行目录穿越。
var storeKeyRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)

// Store 是脚本存储。
type Store struct {
	dir string
	now func() time.Time
}

// NewStore 构造存储；now 为 nil 时用 time.Now。
func NewStore(dir string, now func() time.Time) *Store {
	if now == nil {
		now = time.Now
	}
	return &Store{dir: dir, now: now}
}

// Dir 返回存储目录。
func (s *Store) Dir() string { return s.dir }

// storedScript 是磁盘上的脚本文件（自包含，可导出）。
type storedScript struct {
	Format    string `json:"format"`
	ToolID    string `json:"toolId"`
	ProfileID string `json:"profileId"`
	Script    string `json:"script"`
	UpdatedAt string `json:"updatedAt"`
}

// ScriptRef 是导入结果里的归属。
type ScriptRef struct {
	ToolID    string
	ProfileID string
}

func (s *Store) path(toolID, profileID string) (string, error) {
	if !storeKeyRe.MatchString(toolID) {
		return "", apierr.New(apierr.CodeInvalidRequest, "工具 id 不合法: %q", toolID)
	}
	if !storeKeyRe.MatchString(profileID) {
		return "", apierr.New(apierr.CodeInvalidRequest, "档位 id 不合法: %q", profileID)
	}
	return filepath.Join(s.dir, toolID+"__"+profileID+".json"), nil
}

// Get 读一把档的脚本；没有脚本时 ok=false 且不报错（A12：缺失不是错误）。
func (s *Store) Get(toolID, profileID string) (Script, bool, error) {
	p, err := s.path(toolID, profileID)
	if err != nil {
		return Script{}, false, err
	}
	blob, err := os.ReadFile(p)
	if err != nil {
		if os.IsNotExist(err) {
			return Script{}, false, nil
		}
		return Script{}, false, apierr.Wrap(apierr.CodeStorage, err, "读取用量脚本失败: %s", p)
	}
	var st storedScript
	if err := json.Unmarshal(blob, &st); err != nil {
		return Script{}, false, apierr.Wrap(apierr.CodeConfigInvalid, err, "用量脚本文件损坏: %s", p)
	}
	return Script{Script: st.Script, UpdatedAt: st.UpdatedAt}, true, nil
}

// Put 写入脚本（0600），返回可回读的形状（PUT 契约要求「回读一致」）。
func (s *Store) Put(toolID, profileID, script string) (Script, error) {
	p, err := s.path(toolID, profileID)
	if err != nil {
		return Script{}, err
	}
	if len(script) > MaxScriptBytes {
		return Script{}, apierr.New(apierr.CodeScriptLimitExceeded,
			"脚本超过 %d 字节上限", MaxScriptBytes)
	}
	st := storedScript{
		Format:    storeFormat,
		ToolID:    toolID,
		ProfileID: profileID,
		Script:    script,
		UpdatedAt: s.now().Format(time.RFC3339),
	}
	blob, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return Script{}, apierr.Wrap(apierr.CodeStorage, err, "序列化脚本失败")
	}
	if err := writeFile0600(p, blob); err != nil {
		return Script{}, err
	}
	return Script{Script: script, UpdatedAt: st.UpdatedAt}, nil
}

// Delete 删除脚本；返回是否真的删掉了（幂等：不存在也算成功）。
func (s *Store) Delete(toolID, profileID string) (bool, error) {
	p, err := s.path(toolID, profileID)
	if err != nil {
		return false, err
	}
	if err := os.Remove(p); err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}
		return false, apierr.Wrap(apierr.CodeStorage, err, "删除用量脚本失败: %s", p)
	}
	return true, nil
}

// Export 返回该档脚本文件的原始字节（自包含，可直接随档导出）。
func (s *Store) Export(toolID, profileID string) ([]byte, error) {
	p, err := s.path(toolID, profileID)
	if err != nil {
		return nil, err
	}
	blob, err := os.ReadFile(p)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, apierr.New(apierr.CodeNotFound, "该档还没有用量脚本: %s/%s", toolID, profileID)
		}
		return nil, apierr.Wrap(apierr.CodeStorage, err, "导出用量脚本失败: %s", p)
	}
	return blob, nil
}

// Import 导入一份导出字节，按文件里的归属写回（校验格式与归属合法性）。
func (s *Store) Import(data []byte) (ScriptRef, error) {
	var st storedScript
	if err := json.Unmarshal(data, &st); err != nil {
		return ScriptRef{}, apierr.Wrap(apierr.CodeInvalidRequest, err, "导入内容不是合法 JSON")
	}
	if st.Format != storeFormat {
		return ScriptRef{}, apierr.New(apierr.CodeInvalidRequest,
			"导入内容的格式标识不支持: %q（期望 %q）", st.Format, storeFormat)
	}
	if _, err := s.Put(st.ToolID, st.ProfileID, st.Script); err != nil {
		return ScriptRef{}, err
	}
	return ScriptRef{ToolID: st.ToolID, ProfileID: st.ProfileID}, nil
}

// writeFile0600 同目录临时文件 + rename，并把权限钉成 0600。
//
// 权限必须显式 Chmod：umask 会让 0600 也变成 0400，而 rename 之后的文件
// 一旦可被同机其他用户读到，脚本里可能带的凭据就外泄了。
func writeFile0600(path string, blob []byte) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return apierr.Wrap(apierr.CodeStorage, err, "创建脚本目录失败: %s", dir)
	}
	tmp, err := os.CreateTemp(dir, ".yzagmgr-script-*")
	if err != nil {
		return apierr.Wrap(apierr.CodeStorage, err, "创建临时文件失败: %s", dir)
	}
	tmpName := tmp.Name()
	cleanup := func() { _ = os.Remove(tmpName) }
	if _, err := tmp.Write(blob); err != nil {
		_ = tmp.Close()
		cleanup()
		return apierr.Wrap(apierr.CodeStorage, err, "写入脚本失败")
	}
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		cleanup()
		return apierr.Wrap(apierr.CodeStorage, err, "设置脚本权限失败")
	}
	if err := tmp.Close(); err != nil {
		cleanup()
		return apierr.Wrap(apierr.CodeStorage, err, "关闭临时文件失败")
	}
	if err := os.Rename(tmpName, path); err != nil {
		cleanup()
		return apierr.Wrap(apierr.CodeStorage, err, "落定脚本失败: %s", path)
	}
	// 既有文件可能带着更宽的权限，重新钉一遍。
	if err := os.Chmod(path, 0o600); err != nil {
		return apierr.Wrap(apierr.CodeStorage, err, "设置脚本权限失败: %s", path)
	}
	return nil
}
