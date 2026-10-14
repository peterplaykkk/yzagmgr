package mcp

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"

	"github.com/peterplaykkk/yzagmgr/internal/apierr"
)

// registryDoc 是注册表文件形态（managed/mcp-servers.json，0600 原子写）。
type registryDoc struct {
	Schema  int               `json:"schema"`
	Servers map[string]Server `json:"servers"`
}

// loadRegistry 读注册表；文件缺席 = 空表（合法冷启动），schema 不符或任一条目
// 形状损坏 = config_invalid（UC-111 边界：损坏显式报错，不静默空表）。
//
// 调用方须已持 s.mu（读-改-写序列的一部分，或只读快照）。
func (s *Store) loadRegistry() (map[string]Server, error) {
	raw, err := os.ReadFile(s.registryPath)
	if err != nil {
		if os.IsNotExist(err) {
			return map[string]Server{}, nil
		}
		return nil, apierr.Wrap(apierr.CodeStorage, err, "读 MCP 注册表失败")
	}
	var doc registryDoc
	if err := json.Unmarshal(raw, &doc); err != nil {
		return nil, configInvalid(s.registryPath, err)
	}
	if doc.Schema != RegistrySchema {
		return nil, apierr.New(apierr.CodeConfigInvalid,
			"MCP 注册表 schema 不符: %d（期望 %d）: %s", doc.Schema, RegistrySchema, s.registryPath)
	}
	if doc.Servers == nil {
		doc.Servers = map[string]Server{}
	}
	for name, sv := range doc.Servers {
		if ValidateName(name) != nil {
			return nil, apierr.New(apierr.CodeConfigInvalid,
				"MCP 注册表条目名非法: %q: %s", name, s.registryPath)
		}
		if err := sv.Validate(); err != nil {
			return nil, apierr.Wrap(apierr.CodeConfigInvalid, err,
				"MCP 注册表条目损坏: %s", name)
		}
	}
	return doc.Servers, nil
}

// saveRegistry 原子写注册表（MkdirAll 0700 + CreateTemp 0600 + Rename）。
// 调用方须已持 registryMu。
func (s *Store) saveRegistry(servers map[string]Server) error {
	doc := registryDoc{Schema: RegistrySchema, Servers: servers}
	raw, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return apierr.Wrap(apierr.CodeInternal, err, "序列化 MCP 注册表失败")
	}
	raw = append(raw, '\n')
	dir := filepath.Dir(s.registryPath)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return apierr.Wrap(apierr.CodeStorage, err, "建 MCP 注册表目录失败")
	}
	tmp, err := os.CreateTemp(dir, ".mcp-servers-*.tmp")
	if err != nil {
		return apierr.Wrap(apierr.CodeStorage, err, "创建 MCP 注册表临时文件失败")
	}
	tmpName := tmp.Name()
	if _, err := tmp.Write(raw); err != nil {
		tmp.Close()
		os.Remove(tmpName)
		return apierr.Wrap(apierr.CodeStorage, err, "写 MCP 注册表临时文件失败")
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpName)
		return apierr.Wrap(apierr.CodeStorage, err, "关闭 MCP 注册表临时文件失败")
	}
	if err := os.Chmod(tmpName, 0o600); err != nil {
		os.Remove(tmpName)
		return apierr.Wrap(apierr.CodeStorage, err, "设置 MCP 注册表权限失败")
	}
	if err := os.Rename(tmpName, s.registryPath); err != nil {
		os.Remove(tmpName)
		return apierr.Wrap(apierr.CodeStorage, err, "落位 MCP 注册表失败")
	}
	return nil
}

// sortedNames 返回注册名的字典序快照。
func sortedNames(servers map[string]Server) []string {
	names := make([]string, 0, len(servers))
	for n := range servers {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}
