// §7 本地代理的管理面（数据面在 internal/proxy，不经 /api/v1）。
package api

import (
	"net/http"
	"strings"

	"github.com/peterplaykkk/yzagmgr/internal/proxy"
)

// proxyStatusPayload 是 GET /proxy/status 的响应（§7）。
//
// `port` 是**当前实际监听**的端口，`configuredPort` 是配置值：两者不等时
// `restartRequired:true`，界面必须显示「需重启代理后生效」并给重启入口。
type proxyStatusPayload struct {
	Running         bool               `json:"running"`
	ListenAddr      string             `json:"listenAddr"`
	Port            int                `json:"port"`
	ConfiguredPort  int                `json:"configuredPort"`
	RestartRequired bool               `json:"restartRequired"`
	Prefixes        []proxy.PrefixInfo `json:"prefixes"`
}

func (s *Server) proxyStatusPayload() proxyStatusPayload {
	status := s.Proxy.Status()
	configured := s.Proxy.Listen()
	return proxyStatusPayload{
		Running:         status.Running,
		ListenAddr:      status.ListenAddr,
		Port:            status.Port,
		ConfiguredPort:  configured.Port,
		RestartRequired: s.Proxy.RestartRequired(),
		Prefixes:        status.Prefixes,
	}
}

func (s *Server) handleProxyStatus(*http.Request) (any, error) {
	// syncProxy（策略目录/模型映射喂代理）已随 C 路删除——design §2.2/E2。
	return s.proxyStatusPayload(), nil
}

func (s *Server) handleProxyConfigSave(r *http.Request) (any, string, error) {
	var body struct {
		ListenAddr *string `json:"listenAddr"`
		Port       *int    `json:"port"`
	}
	if err := decodeOptional(r, &body); err != nil {
		return nil, "", err
	}
	listen := s.Proxy.Listen()
	if body.ListenAddr != nil {
		listen.Addr = strings.TrimSpace(*body.ListenAddr)
	}
	if body.Port != nil {
		listen.Port = *body.Port
	}
	if err := s.Proxy.SetListen(listen); err != nil {
		return nil, "", err
	}
	// 回读一致：响应取自代理自身的状态，不回显请求体。
	return s.proxyStatusPayload(), "", nil
}

func (s *Server) handleProxyStart(r *http.Request) (any, string, error) {
	if err := s.Proxy.Start(); err != nil {
		return nil, "", err
	}
	return map[string]any{"running": s.Proxy.Running()}, "", nil
}

func (s *Server) handleProxyStop(r *http.Request) (any, string, error) {
	if err := s.Proxy.Stop(r.Context()); err != nil {
		return nil, "", err
	}
	return map[string]any{"running": s.Proxy.Running()}, "", nil
}
