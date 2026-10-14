package cli

import (
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"text/tabwriter"

	"bytes"
	"fmt"
)

// ── profiles / materialize（UC-019/024/031/032）────────────────────────────
// （ensure-gateway-profiles 子命令已随 §4.5/§4.1 路由删除——design §2.3。）
//
// 与其余子命令同一纪律：纯客户端，业务语义全在守护进程侧。唯一的本地动作仍是
// `--cred-file` 的读取（routes add 同一函数、同一 0600 交付通道）：凭据明文经请求体
// 交付守护进程托管凭据库，**输出与信封里只有引用**（UC-018 A12 / UC-032 A1）。

// splitModels 把 --models 的逗号串切成清单（保序去空白；全空 = 显式空清单）。
func splitModels(raw string) []string {
	parts := strings.Split(raw, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}

// execProfiles 执行 profiles list|show|create|update|delete|duplicate。
//
// show 是**组合视图**（REST 无单卡 GET）：取列表载荷后按 id 摘出该卡的原文对象，
// data 逐字节保留列表 JSON 里该元素的字节（不重编码）。
func execProfiles(d Deps, cmd Cmd, c *Client) int {
	base := "/api/v1/tools/" + url.PathEscape(cmd.Tool) + "/profiles"
	switch cmd.Kind {
	case KindProfilesList:
		data, err := c.Do(http.MethodGet, base, nil)
		if err != nil {
			return failTransport(d, cmd, err)
		}
		return emit(d, cmd, data, formatProfilesList)
	case KindProfilesShow:
		data, err := c.Do(http.MethodGet, base, nil)
		if err != nil {
			return failTransport(d, cmd, err)
		}
		item, err := extractCard(data, cmd.CardID)
		if err != nil {
			return failf(d, cmd, ExitAPI, "not_found", "档位卡不存在: %s", cmd.CardID)
		}
		return emit(d, cmd, item, formatProfile)
	case KindProfilesCreate:
		body := map[string]any{"name": cmd.CardName}
		if cmd.FromSet && cmd.From != "" {
			body["from"] = cmd.From
		}
		if cmd.Endpoint != "" {
			body["endpoint"] = cmd.Endpoint
		}
		if cmd.CredFile != "" {
			cred, err := readCredFile(cmd.CredFile)
			if err != nil {
				// 凭据交付失败按业务失败计（routes add 同口径，UC-023 A4）。
				return failf(d, cmd, ExitAPI, "invalid_request", "%v", err)
			}
			body["cred"] = cred
		}
		if cmd.CredRef != "" {
			body["credRef"] = cmd.CredRef
		}
		if cmd.ModelsSet {
			body["models"] = splitModels(cmd.Models)
		}
		if cmd.MaxOutputTokensSet {
			body["maxOutputTokens"] = cmd.MaxOutputTokens
		}
		data, err := c.Do(http.MethodPost, base, body)
		if err != nil {
			return failTransport(d, cmd, err)
		}
		return emit(d, cmd, data, formatProfile)
	case KindProfilesUpdate:
		body := map[string]any{}
		if cmd.CardName != "" {
			body["name"] = cmd.CardName
		}
		if cmd.ModelID != "" {
			body["modelId"] = cmd.ModelID
		}
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
		if cmd.CredRef != "" {
			body["credRef"] = cmd.CredRef
		}
		if cmd.ModelsSet {
			body["models"] = splitModels(cmd.Models)
		}
		if cmd.MaxOutputTokensSet {
			body["maxOutputTokens"] = cmd.MaxOutputTokens
		}
		data, err := c.Do(http.MethodPut, base+"/"+url.PathEscape(cmd.CardID), body)
		if err != nil {
			return failTransport(d, cmd, err)
		}
		return emit(d, cmd, data, formatProfile)
	case KindProfilesDelete:
		data, err := c.Do(http.MethodDelete, base+"/"+url.PathEscape(cmd.CardID), nil)
		if err != nil {
			return failTransport(d, cmd, err)
		}
		return emit(d, cmd, data, formatProfileRemoved)
	case KindProfilesDuplicate:
		data, err := c.Do(http.MethodPost, base+"/"+url.PathEscape(cmd.CardID)+"/duplicate", nil)
		if err != nil {
			return failTransport(d, cmd, err)
		}
		return emit(d, cmd, data, formatProfile)
	}
	return failf(d, cmd, ExitUsage, "usage", "未实现的 profiles 子命令（内部错误）")
}

func execMaterialize(d Deps, cmd Cmd, c *Client) int {
	method := http.MethodGet
	if cmd.Refresh {
		method = http.MethodPost
	}
	data, err := c.Do(method, "/api/v1/materialize", nil)
	if err != nil {
		return failTransport(d, cmd, err)
	}
	return emit(d, cmd, data, formatMaterialize)
}

// extractCard 从列表载荷摘出单张卡的原文对象（组合视图的字节保真实现）。
func extractCard(data json.RawMessage, pid string) (json.RawMessage, error) {
	var wire struct {
		Items []json.RawMessage `json:"items"`
	}
	if err := json.Unmarshal(data, &wire); err != nil {
		return nil, err
	}
	for _, item := range wire.Items {
		var head struct {
			ID string `json:"id"`
		}
		if err := json.Unmarshal(item, &head); err != nil {
			return nil, err
		}
		if head.ID == pid {
			return item, nil
		}
	}
	return nil, fmt.Errorf("档位卡不存在: %s", pid)
}

// ── 人类可读格式化（--json 走 REST 载荷原文透传，不经这些函数）───────────────

type profileWire struct {
	ID        string   `json:"id"`
	Name      string   `json:"name"`
	Kind      string   `json:"kind"`
	Active    bool     `json:"active"`
	Tier      string   `json:"tier"`
	ModelID   string   `json:"modelId"`
	Models    []string `json:"models"`
	Endpoint  string   `json:"endpoint"`
	CredRef   string   `json:"credRef"`
	Editable  bool     `json:"editable"`
	Deletable bool     `json:"deletable"`
}

type materializeWire struct {
	Root     string `json:"root"`
	Families []struct {
		ToolID   string `json:"toolId"`
		Name     string `json:"name"`
		Profiles []struct {
			ProfileID     string   `json:"profileId"`
			Name          string   `json:"name"`
			Kind          string   `json:"kind"`
			Dir           string   `json:"dir"`
			LaunchCommand string   `json:"launchCommand"`
			Files         []string `json:"files"`
		} `json:"profiles"`
	} `json:"families"`
	PartialFailures []struct {
		ToolID    string `json:"toolId"`
		ProfileID string `json:"profileId"`
		Reason    string `json:"reason"`
	} `json:"partialFailures"`
}

func formatProfile(data json.RawMessage) (string, error) {
	var p profileWire
	if err := json.Unmarshal(data, &p); err != nil {
		return "", err
	}
	buf := &bytes.Buffer{}
	fmt.Fprintf(buf, "id\t%s\nname\t%s\nkind\t%s\n", p.ID, p.Name, p.Kind)
	fmt.Fprintf(buf, "tier\t%s\nmodelId\t%s\nmodels\t%s\n", p.Tier, p.ModelID, strings.Join(p.Models, ","))
	if p.Endpoint != "" {
		fmt.Fprintf(buf, "endpoint\t%s\n", p.Endpoint)
	}
	if p.CredRef != "" {
		fmt.Fprintf(buf, "credRef\t%s\n", p.CredRef)
	}
	fmt.Fprintf(buf, "active\t%v\neditable\t%v\ndeletable\t%v\n", p.Active, p.Editable, p.Deletable)
	return buf.String(), nil
}

func formatProfilesList(data json.RawMessage) (string, error) {
	var wire struct {
		Items           []profileWire `json:"items"`
		Total           int           `json:"total"`
		ActiveProfileID string        `json:"activeProfileId"`
	}
	if err := json.Unmarshal(data, &wire); err != nil {
		return "", err
	}
	buf := &bytes.Buffer{}
	w := tabwriter.NewWriter(buf, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "ID\tNAME\tKIND\tTIER\tMODEL\tENDPOINT\tCRED-REF")
	for _, p := range wire.Items {
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\t%s\n",
			p.ID, p.Name, p.Kind, p.Tier, p.ModelID, p.Endpoint, p.CredRef)
	}
	_ = w.Flush()
	fmt.Fprintf(buf, "total\t%d\nactiveProfileId\t%s\n", wire.Total, wire.ActiveProfileID)
	return buf.String(), nil
}

func formatProfileRemoved(data json.RawMessage) (string, error) {
	return "removed\tok\n", nil
}

func formatMaterialize(data json.RawMessage) (string, error) {
	var wire materializeWire
	if err := json.Unmarshal(data, &wire); err != nil {
		return "", err
	}
	buf := &bytes.Buffer{}
	fmt.Fprintf(buf, "root\t%s\n", wire.Root)
	w := tabwriter.NewWriter(buf, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "TOOL\tPROFILE\tKIND\tDIR")
	for _, fam := range wire.Families {
		for _, p := range fam.Profiles {
			fmt.Fprintf(w, "%s\t%s\t%s\t%s\n", fam.ToolID, p.ProfileID, p.Kind, p.Dir)
		}
	}
	_ = w.Flush()
	for _, f := range wire.PartialFailures {
		fmt.Fprintf(buf, "partialFailure\t%s\t%s\t%s\n", f.ToolID, f.ProfileID, f.Reason)
	}
	return buf.String(), nil
}
