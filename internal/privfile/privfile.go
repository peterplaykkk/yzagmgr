package privfile

import (
	"os"
	"path/filepath"

	"github.com/peterplaykkk/yzagmgr/internal/apierr"
)

// 本文件是「本产品的私有文件」纪律的唯一实现：0600 + 原子替换 + 拒绝符号链接。
//
// 为什么独立成包而不是散在各自的包：策略持久化等落盘点都要走同一套判据，
// 多份拷贝必然漂移——而凭据落盘的权限纪律不允许有第二种写法。
// 读路径分档：凭据文件严格（组/其他可读即拒绝），非凭据资产宽松（写仍按 0600）。
// （本包自 gateway 包迁出：登录链删除后 gateway 包整删，写盘纪律随 policy 等
// 存活面迁至 privfile。）

// WritePrivateFile 以指定权限原子写入私有文件。
//
// 先写同目录临时文件再 rename：中断只会留下半个临时文件，不会留下半个目标文件——
// 「令牌在盘上但读不出来」是最难排查的坏状态。
func WritePrivateFile(path string, data []byte, mode os.FileMode) error {
	dir := filepath.Dir(path)
	if err := EnsurePrivateDir(dir); err != nil {
		return err
	}
	if st, err := os.Lstat(path); err == nil && (!st.Mode().IsRegular() || st.Mode()&os.ModeSymlink != 0) {
		return apierr.New(apierr.CodePermissionDenied, "私有文件路径不是普通文件，拒绝写入")
	}
	tmp, err := os.CreateTemp(dir, ".yzagmgr-*.tmp")
	if err != nil {
		return apierr.Wrap(apierr.CodeStorage, err, "创建临时文件失败")
	}
	name := tmp.Name()
	discard := func(cause error) error {
		_ = tmp.Close()
		_ = os.Remove(name)
		return cause
	}
	if _, err := tmp.Write(data); err != nil {
		return discard(apierr.Wrap(apierr.CodeStorage, err, "写入文件失败"))
	}
	if err := tmp.Sync(); err != nil {
		return discard(apierr.Wrap(apierr.CodeStorage, err, "落盘失败"))
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(name)
		return apierr.Wrap(apierr.CodeStorage, err, "落盘失败")
	}
	if err := os.Chmod(name, mode); err != nil {
		_ = os.Remove(name)
		return apierr.Wrap(apierr.CodeStorage, err, "设置文件权限失败")
	}
	if err := os.Rename(name, path); err != nil {
		_ = os.Remove(name)
		return apierr.Wrap(apierr.CodeStorage, err, "替换文件失败")
	}
	return nil
}

// ReadPrivateFile 读取私有文件；不存在返回 (nil, nil)。
//
// strict 为真时拒绝组/其他可读的文件：这不只是洁癖——权限被放宽说明有人碰过它，
// 而凭据文件一旦被改过，继续使用等于沿用一份来源不明的令牌。
func ReadPrivateFile(path string, strict bool) ([]byte, error) {
	st, err := os.Lstat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, apierr.Wrap(apierr.CodeStorage, err, "读取文件失败")
	}
	if !st.Mode().IsRegular() || st.Mode()&os.ModeSymlink != 0 {
		return nil, apierr.New(apierr.CodePermissionDenied, "路径不是普通文件")
	}
	if strict && st.Mode().Perm()&0o077 != 0 {
		return nil, apierr.New(apierr.CodePermissionDenied, "文件权限过宽，拒绝读取")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, apierr.Wrap(apierr.CodeStorage, err, "读取文件失败")
	}
	return data, nil
}

// RemovePrivateFile 删除私有文件；不存在视为成功（登出必须幂等，UC-002 A3）。
func RemovePrivateFile(path string) error {
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return apierr.Wrap(apierr.CodeStorage, err, "删除文件失败")
	}
	return nil
}

// EnsurePrivateDir 建立私有目录（0700）。
//
// 只拒绝**目标自身**的符号链接，不逐级检查祖先：macOS 的系统临时目录
// （/var → /private/var）本身就是系统级链接，逐级拒绝会连测试的临时目录都用不了。
// 祖先的符号链接纪律由 config 的隔离总闸负责（UC-013）。
func EnsurePrivateDir(dir string) error {
	if dir == "" || dir == "." {
		return nil
	}
	if st, err := os.Lstat(dir); err == nil {
		if st.Mode()&os.ModeSymlink != 0 {
			return apierr.New(apierr.CodePermissionDenied, "私有目录是符号链接，拒绝写入")
		}
		if !st.IsDir() {
			return apierr.New(apierr.CodeStorage, "私有目录路径不是目录")
		}
		return nil
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return apierr.Wrap(apierr.CodeStorage, err, "建立私有目录失败")
	}
	return nil
}
