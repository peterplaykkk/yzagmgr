// remap model set 的可选能力位（UC-030 additive：`--supports1m` / `--prefer1m`）。
//
// 为什么存储在 api 层而不是 internal/proxy：proxy.ModelMapEntry 是数据面契约形状
// （alias→target，「勿改字段名」），能力位只被**写链派生**（claude-desktop 多条目）与
// 管理面回读消费，不进请求路径——api 层保管它们，proxy 的 router 面保持稳定。
//
// 语义（claude-desktop-five-route.sh 冻结面）：能力位是**可选声明**——缺省 = 不声明；
// 回读行（items）只在声明时携带 supports1m/prefer1m 字段（false/缺席不伪造，与
// 档案条目同一口径）。持久化在 managed/router.json 的 modelCaps 段（additive，老文件
// 缺段 = 全部不声明）。
package api

// modelCaps 是一条 alias 的能力位声明（wire 形状与档案条目键同名）。
type modelCaps struct {
	Supports1m bool `json:"supports1m,omitempty"`
	Prefer1m   bool `json:"prefer1m,omitempty"`
}

// declared 报告是否有任何能力位被声明。
func (c modelCaps) declared() bool { return c.Supports1m || c.Prefer1m }

// setModelCaps 登记/替换一条 alias 的能力位（false = 不声明）。
func (s *Server) setModelCaps(arm, alias string, c modelCaps) {
	s.routeCapsMu.Lock()
	defer s.routeCapsMu.Unlock()
	if s.routeCaps == nil {
		s.routeCaps = map[string]map[string]modelCaps{}
	}
	if s.routeCaps[arm] == nil {
		s.routeCaps[arm] = map[string]modelCaps{}
	}
	s.routeCaps[arm][alias] = c
}

// modelCapsFor 取一条 alias 的能力位（缺省 = 全不声明）。
func (s *Server) modelCapsFor(arm, alias string) modelCaps {
	s.routeCapsMu.Lock()
	defer s.routeCapsMu.Unlock()
	return s.routeCaps[arm][alias]
}

// dropModelCaps 摘除一条 alias 的能力位（remap model delete 时）。
func (s *Server) dropModelCaps(arm, alias string) {
	s.routeCapsMu.Lock()
	defer s.routeCapsMu.Unlock()
	delete(s.routeCaps[arm], alias)
}

// dropArmCaps 摘除整臂的能力位（routes remove 时：remap 表从属于臂）。
func (s *Server) dropArmCaps(arm string) {
	s.routeCapsMu.Lock()
	defer s.routeCapsMu.Unlock()
	delete(s.routeCaps, arm)
}

// modelCapsSnapshot 导出当前能力位（持久化用；arm → alias → caps）。
func (s *Server) modelCapsSnapshot() map[string]map[string]modelCaps {
	s.routeCapsMu.Lock()
	defer s.routeCapsMu.Unlock()
	out := make(map[string]map[string]modelCaps, len(s.routeCaps))
	for arm, m := range s.routeCaps {
		cp := make(map[string]modelCaps, len(m))
		for a, c := range m {
			cp[a] = c
		}
		out[arm] = cp
	}
	return out
}

// loadModelCaps 装载持久化的能力位（启动时；坏值按缺省，不阻塞启动）。
func (s *Server) loadModelCaps(caps map[string]map[string]modelCaps) {
	s.routeCapsMu.Lock()
	defer s.routeCapsMu.Unlock()
	if s.routeCaps == nil {
		s.routeCaps = map[string]map[string]modelCaps{}
	}
	for arm, m := range caps {
		if s.routeCaps[arm] == nil {
			s.routeCaps[arm] = map[string]modelCaps{}
		}
		for a, c := range m {
			s.routeCaps[arm][a] = c
		}
	}
}
