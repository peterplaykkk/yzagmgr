// Package mcp 承载 MCP server 注册表与「多工具物化」独立写链（UC-111）。
//
// 边界（UC-111 非目标，勿扩）：本包**不进**十家族 Paths——注册表与物化是独立于
// 档位切换的第三条写链（WorkBuddy 式：Store + 共享租约 + tx.Apply）；目标恰
// `claude`（~/.claude.json 顶层 mcpServers）与 `claude-desktop`（Claude-3p 标记文件
// 顶层 mcpServers），两者都走 docmodel 字节保真增量合并，未在操作集内的条目与
// 其它顶层键逐字节存活。
package mcp

import (
	"net/url"
	"regexp"
	"strings"

	"github.com/peterplaykkk/yzagmgr/internal/apierr"
)

// RegistrySchema 是注册表文件的 schema 版本；读到不等于该值即显式报错
// （UC-111 边界：损坏不静默空表）。
const RegistrySchema = 1

// SupportedTools 是物化目标的封闭集合（顺序即契约顺序）。
var SupportedTools = []string{"claude", "claude-desktop"}

// namePattern 是注册名的闸门：小写起头的 slug 形态。
// 只收小写是**有意的**——操作日志的 target 字符集（operations.safeTarget）只认
// 小写/数字/._-，名字要能在操作面原样回读。已知边界：looksLikeSecret 仍会拒
// `sk-` 前缀长名与长 hex 名于操作日志（登记面的静默缺条目，UC-111 边界）。
var namePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,63}$`)

// Server 是一条 MCP server 配置（注册表存储与 PUT 请求体共用；name 在注册表
// 的 map 键上，不在结构体里）。
type Server struct {
	Type    string            `json:"type"`
	URL     string            `json:"url,omitempty"`
	Command string            `json:"command,omitempty"`
	Args    []string          `json:"args,omitempty"`
	Env     map[string]string `json:"env,omitempty"`
	Headers map[string]string `json:"headers,omitempty"`
	// Tools 限定该条目物化到哪些受支持工具；空 = 全部（UC-111 A8）。
	Tools []string `json:"tools,omitempty"`
}

// NamedServer 是读面载荷：name + 条目字段平铺。
type NamedServer struct {
	Name string `json:"name"`
	Server
}

// ValidateName 校验注册名（UC-111 A2），非法即 invalid_request。
func ValidateName(name string) error {
	if !namePattern.MatchString(name) {
		return apierr.New(apierr.CodeInvalidRequest,
			"MCP server 名非法: %q（要求 %s）", name, namePattern.String())
	}
	return nil
}

// SupportedTool 报告 tool 是否属于封闭集合。
func SupportedTool(tool string) bool {
	for _, t := range SupportedTools {
		if t == tool {
			return true
		}
	}
	return false
}

// Validate 校验条目形状（UC-111 A2），非法即 invalid_request 且逐字段点名。
func (sv Server) Validate() error {
	switch sv.Type {
	case "http", "sse":
		if strings.TrimSpace(sv.URL) == "" {
			return apierr.New(apierr.CodeInvalidRequest, "type=%s 的条目缺 url", sv.Type)
		}
		u, err := url.Parse(sv.URL)
		if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
			return apierr.New(apierr.CodeInvalidRequest,
				"url 不是合法的 http/https 地址: %q", sv.URL)
		}
		if len(sv.Command) > 0 || len(sv.Args) > 0 || len(sv.Env) > 0 {
			return apierr.New(apierr.CodeInvalidRequest,
				"type=%s 的条目不得携带 command/args/env", sv.Type)
		}
	case "stdio":
		if strings.TrimSpace(sv.Command) == "" {
			return apierr.New(apierr.CodeInvalidRequest, "type=stdio 的条目缺 command")
		}
		if strings.TrimSpace(sv.URL) != "" || len(sv.Headers) > 0 {
			return apierr.New(apierr.CodeInvalidRequest,
				"type=stdio 的条目不得携带 url/headers")
		}
	default:
		return apierr.New(apierr.CodeInvalidRequest,
			"type 取值非法: %q（允许 http|sse|stdio）", sv.Type)
	}
	for _, t := range sv.Tools {
		if !SupportedTool(t) {
			return apierr.New(apierr.CodeInvalidRequest,
				"tools 含未支持工具: %q（允许 %s）", t, strings.Join(SupportedTools, "|"))
		}
	}
	return nil
}

// covers 报告该条目是否物化到 tool（空 tools = 全部）。
func (sv Server) covers(tool string) bool {
	if len(sv.Tools) == 0 {
		return true
	}
	for _, t := range sv.Tools {
		if t == tool {
			return true
		}
	}
	return false
}

// value 产出条目在 live 文件里的渲染值（编码交给 docmodel：键排序、紧凑单行、
// 不转 HTML 转义）。
func (sv Server) value() map[string]any {
	v := map[string]any{"type": sv.Type}
	if sv.URL != "" {
		v["url"] = sv.URL
	}
	if sv.Command != "" {
		v["command"] = sv.Command
	}
	if len(sv.Args) > 0 {
		v["args"] = sv.Args
	}
	if len(sv.Env) > 0 {
		v["env"] = sv.Env
	}
	if len(sv.Headers) > 0 {
		v["headers"] = sv.Headers
	}
	// tools 是注册表侧的物化范围，不落 live 文件（live 只有 Claude 认识的连接键）。
	return v
}

// configInvalid 把目标文件/注册表的解析失败统一成 config_invalid（带路径上下文）。
func configInvalid(path string, err error) error {
	return apierr.Wrap(apierr.CodeConfigInvalid, err, "JSON 解析失败: %s", path)
}
