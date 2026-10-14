package cli

import (
	"encoding/json"
	"net/http"
	"net/url"
	"os"
	"runtime"

	"bytes"
	"fmt"
	"strings"
	"text/tabwriter"

	"github.com/peterplaykkk/yzagmgr/internal/proxy"
)

// ── routes / remap 家族（UC-023 / UC-028 / UC-029）────────────────────────────
//
// 与其余子命令同一纪律：纯客户端，业务语义全在守护进程侧。唯一的本地动作是
// `--cred-file` 的读取——凭据明文只经 0600 文件交付（不进 argv/日志），CLI 读出
// 后经请求体交付守护进程托管凭据库，**输出与信封里只有引用**（UC-023 A2）。

// readCredFile 读取 0600 凭据文件里的令牌明文。
//
// 三条本地守卫（凭据纪律，全局规则：只走 0600 文件/环境注入）：
//   - 必须是普通文件（不跟随目录/链接乱读）；
//   - Unix 上组/其他位必须为 0（0600/0400 等价，0644 拒绝）——交付通道本身的
//     形态就是契约（UC-023：「凭据明文只经 0600 文件交付」）；
//   - 读出的明文**不进任何回显**：错误消息只提路径，绝不带内容。
func readCredFile(path string) (string, error) {
	if strings.TrimSpace(path) == "" {
		return "", fmt.Errorf("缺凭据文件：凭据明文只经 0600 文件交付（--cred-file <path>）")
	}
	st, err := os.Lstat(path)
	if err != nil {
		return "", fmt.Errorf("凭据文件不可读（%s）: %w", path, err)
	}
	if !st.Mode().IsRegular() {
		return "", fmt.Errorf("凭据文件不是普通文件: %s", path)
	}
	if runtime.GOOS != "windows" && st.Mode().Perm()&0o077 != 0 {
		return "", fmt.Errorf("凭据文件权限过宽（%s 要求组/其他不可读，当前 %04o）: %s",
			"0600", st.Mode().Perm(), path)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("凭据文件不可读（%s）: %w", path, err)
	}
	tok := strings.TrimSpace(string(b))
	if tok == "" {
		return "", fmt.Errorf("凭据文件内容为空: %s", path)
	}
	return tok, nil
}

// execRoutes 执行 routes add|list|show|set|remove。
func execRoutes(d Deps, cmd Cmd, c *Client) int {
	base := "/api/v1/routes"
	switch cmd.Kind {
	case KindRoutesList:
		data, err := c.Do(http.MethodGet, base, nil)
		if err != nil {
			return failTransport(d, cmd, err)
		}
		return emit(d, cmd, data, formatRoutesList)
	case KindRoutesShow:
		data, err := c.Do(http.MethodGet, base+"/"+url.PathEscape(cmd.Name), nil)
		if err != nil {
			return failTransport(d, cmd, err)
		}
		return emit(d, cmd, data, formatRoute)
	case KindRoutesAdd:
		cred, err := readCredFile(cmd.CredFile)
		if err != nil {
			// 本地前置失败按业务失败计（注册校验拒绝 = 退出码 1，UC-023 A4）。
			return failf(d, cmd, ExitAPI, "invalid_request", "%v", err)
		}
		data, err := c.Do(http.MethodPost, base, map[string]string{
			"name": cmd.Name, "endpoint": cmd.Endpoint, "cred": cred,
		})
		if err != nil {
			return failTransport(d, cmd, err)
		}
		return emit(d, cmd, data, formatRoute)
	case KindRoutesSet:
		body := map[string]string{}
		if cmd.Endpoint != "" {
			body["endpoint"] = cmd.Endpoint
		}
		if cmd.CredFile != "" {
			cred, err := readCredFile(cmd.CredFile)
			if err != nil {
				return failf(d, cmd, ExitAPI, "invalid_request", "%v", err)
			}
			body["cred"] = cred
		}
		data, err := c.Do(http.MethodPut, base+"/"+url.PathEscape(cmd.Name), body)
		if err != nil {
			return failTransport(d, cmd, err)
		}
		return emit(d, cmd, data, formatRoute)
	case KindRoutesRemove:
		data, err := c.Do(http.MethodDelete, base+"/"+url.PathEscape(cmd.Name), nil)
		if err != nil {
			return failTransport(d, cmd, err)
		}
		return emit(d, cmd, data, formatRouteRemoved)
	}
	return failf(d, cmd, ExitUsage, "usage", "未实现的 routes 子命令（内部错误）")
}

// execRemap 执行 remap model set|list 与 remap effort set|force|list。
func execRemap(d Deps, cmd Cmd, c *Client) int {
	arm := url.PathEscape(cmd.Name)
	switch cmd.Kind {
	case KindRemapModelSet:
		// 能力位（UC-030 additive）仅在显式给出时写键：缺席 = 不声明
		// （z 的「缺席=false」语义，false/缺席不伪造键）。
		body := map[string]any{"alias": cmd.Alias, "target": cmd.Target}
		if cmd.Supports1m {
			body["supports1m"] = true
		}
		if cmd.Prefer1m {
			body["prefer1m"] = true
		}
		data, err := c.Do(http.MethodPut, "/api/v1/remap/model/"+arm, body)
		if err != nil {
			return failTransport(d, cmd, err)
		}
		return emit(d, cmd, data, formatModelMapEntry)
	case KindRemapModelList:
		data, err := c.Do(http.MethodGet, "/api/v1/remap/model/"+arm, nil)
		if err != nil {
			return failTransport(d, cmd, err)
		}
		return emit(d, cmd, data, formatModelMapList)
	case KindRemapEffortSet:
		data, err := c.Do(http.MethodPut, "/api/v1/remap/effort/"+arm,
			map[string]string{"from": cmd.From, "to": cmd.To})
		if err != nil {
			return failTransport(d, cmd, err)
		}
		return emit(d, cmd, data, formatEffortEntry)
	case KindRemapEffortForce:
		data, err := c.Do(http.MethodPut, "/api/v1/remap/effort/"+arm+"/force",
			map[string]string{"forceEffort": cmd.To})
		if err != nil {
			return failTransport(d, cmd, err)
		}
		return emit(d, cmd, data, formatEffortForce)
	case KindRemapEffortList:
		data, err := c.Do(http.MethodGet, "/api/v1/remap/effort/"+arm, nil)
		if err != nil {
			return failTransport(d, cmd, err)
		}
		return emit(d, cmd, data, formatEffortList)
	}
	return failf(d, cmd, ExitUsage, "usage", "未实现的 remap 子命令（内部错误）")
}

// ── 人类可读格式化（--json 走 REST 载荷原文透传，不经这些函数）───────────────

type routeWire = proxy.ArmEntry

type modelMapListWire struct {
	Items []proxy.ModelMapEntry `json:"items"`
}

type effortListWire struct {
	Items       []proxy.EffortEntry `json:"items"`
	ForceEffort string              `json:"forceEffort"`
}

func formatRoute(data json.RawMessage) (string, error) {
	var a routeWire
	if err := json.Unmarshal(data, &a); err != nil {
		return "", err
	}
	return fmt.Sprintf("name\t%s\nendpoint\t%s\ncredRef\t%s\n", a.Name, a.Endpoint, a.CredRef), nil
}

func formatRoutesList(data json.RawMessage) (string, error) {
	var wire struct {
		Items []routeWire `json:"items"`
	}
	if err := json.Unmarshal(data, &wire); err != nil {
		return "", err
	}
	var b strings.Builder
	w := tabwriter.NewWriter(&b, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "NAME\tENDPOINT\tCRED-REF")
	for _, a := range wire.Items {
		fmt.Fprintf(w, "%s\t%s\t%s\n", a.Name, a.Endpoint, a.CredRef)
	}
	_ = w.Flush()
	return b.String(), nil
}

func formatRouteRemoved(data json.RawMessage) (string, error) {
	var wire struct {
		Name string `json:"name"`
	}
	if err := json.Unmarshal(data, &wire); err != nil {
		return "", err
	}
	return fmt.Sprintf("removed\t%s\n", wire.Name), nil
}

func formatModelMapEntry(data json.RawMessage) (string, error) {
	var e proxy.ModelMapEntry
	if err := json.Unmarshal(data, &e); err != nil {
		return "", err
	}
	return fmt.Sprintf("alias\t%s\ntarget\t%s\n", e.Alias, e.Target), nil
}

func formatModelMapList(data json.RawMessage) (string, error) {
	var wire modelMapListWire
	if err := json.Unmarshal(data, &wire); err != nil {
		return "", err
	}
	var b strings.Builder
	w := tabwriter.NewWriter(&b, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "ALIAS\tTARGET")
	for _, e := range wire.Items {
		fmt.Fprintf(w, "%s\t%s\n", e.Alias, e.Target)
	}
	_ = w.Flush()
	return b.String(), nil
}

func formatEffortEntry(data json.RawMessage) (string, error) {
	var e proxy.EffortEntry
	if err := json.Unmarshal(data, &e); err != nil {
		return "", err
	}
	return fmt.Sprintf("from\t%s\nto\t%s\n", e.From, e.To), nil
}

func formatEffortForce(data json.RawMessage) (string, error) {
	var wire struct {
		ForceEffort string `json:"forceEffort"`
	}
	if err := json.Unmarshal(data, &wire); err != nil {
		return "", err
	}
	if wire.ForceEffort == "" {
		return "forceEffort\t（已清除）\n", nil
	}
	return fmt.Sprintf("forceEffort\t%s\n", wire.ForceEffort), nil
}

func formatEffortList(data json.RawMessage) (string, error) {
	var wire effortListWire
	if err := json.Unmarshal(data, &wire); err != nil {
		return "", err
	}
	buf := &bytes.Buffer{}
	w := tabwriter.NewWriter(buf, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "FROM\tTO")
	for _, e := range wire.Items {
		fmt.Fprintf(w, "%s\t%s\n", e.From, e.To)
	}
	force := wire.ForceEffort
	if force == "" {
		force = "—"
	}
	fmt.Fprintf(w, "forceEffort\t%s\n", force)
	_ = w.Flush()
	return buf.String(), nil
}
