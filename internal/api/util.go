// 小工具：文件读取、数字格式化、时间格式。
package api

import (
	"os"
	"runtime"
	"strconv"
	"strings"
	"time"
)

// rfc3339Second 是本产品对外的时间格式（秒精度，与契约示例一致）。
// 用固定字面量而不是 time.RFC3339（后者带 `Z07:00` 时区形态差异），避免同一份
// 响应里出现两种时间写法。
const rfc3339Second = "2006-01-02T15:04:05Z07:00"

// statRegular 取普通文件的状态；目录、符号链接都返回错误（EISDIR 陷阱，UC-003 A10）。
func statRegular(path string) (os.FileInfo, error) {
	st, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !st.Mode().IsRegular() {
		return nil, os.ErrInvalid
	}
	return st, nil
}

// readRegularFile 读一个普通文件；不存在、是目录、是符号链接或不可读都返回错误。
//
// 为什么不用 os.ReadFile 了事：受管路径里出现目录或链接是**要显式报出的异常**
// （EISDIR 陷阱、UC-003 A10），调用方需要能区分「没有」与「不是文件」。
func readRegularFile(path string) ([]byte, error) {
	if _, err := statRegular(path); err != nil {
		return nil, err
	}
	return os.ReadFile(path)
}

// parseIntStrict 解析十进制整数（只接受数字与可选负号）。
func parseIntStrict(s string) (int, error) {
	return strconv.Atoi(strings.TrimSpace(s))
}

// trimFloat 把浮点渲染成不带多余零的十进制串（键值回显用，例如 900000 而不是 9e+05）。
func trimFloat(v float64) string {
	return strconv.FormatFloat(v, 'f', -1, 64)
}

// formatTime 按本产品的统一格式渲染时间（零值给空串）。
func formatTime(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.Format(rfc3339Second)
}

// isWindows / isArm64 是本机平台判定（目录与任务的默认过滤条件）。
func isWindows() bool { return runtime.GOOS == "windows" }

func isArm64() bool { return runtime.GOARCH == "arm64" }
