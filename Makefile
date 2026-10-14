# yzagmgr 门禁入口（Makefile）
#
# 目标：
#   make build      构建 bin/yzagmgr（纯 Go，CGO_ENABLED=0）
#   make test       go test ./... -count=1
#   make fmt        gofmt -w 全部 Go 文件
#   make vet        go vet ./...
#   make gates      fmt-check + vet + build + test（web/ 就绪时含 web 检查），
#                   末尾输出机器可读摘要并写 reports/gates-<时间戳>/summary.json
#   make e2e        浏览器 E2E（真 daemon + 真 Chromium；自建 web/.e2e-bin 与 web/dist）
#   make clean      清理 bin/
#
# 纪律：
#   - 上述目标必须在本机真实可跑；Go 侧一律 CGO_ENABLED=0（保持可交叉编译，不引 cgo）。
#   - 守护进程（cmd/yzagmgr）由并行任务交付：未交付时 build-go 明确报告 pending，
#     不以假绿替代（gates 把它记为 pending 而不是 pass）。
#   - 本机 /bin/bash 恒为 3.2（SIP），脚本一律经 PATH 上的 bash 5.x 运行（见 BASH_BIN）。
#   - GNU Make 3.81（macOS 自带）：不要用 .ONESHELL / $(file …)；每条 recipe 行独立 shell。
#   - e2e 刻意**不进 gates**：低层门禁（P3）与浏览器 E2E（P4/P6）分层，gates 保持无浏览器
#     依赖；e2e 自建 web/.e2e-bin/yzagmgr 与 web/dist，不需要先 make build。

BASH_BIN := $(shell command -v bash 2>/dev/null || echo /bin/bash)
NPM ?= npm
GO ?= go
GO_FIND := find . -type f -name '*.go' -not -path './bin/*' -not -path './web/*' -not -path './reports/*'

.PHONY: all help build build-go test e2e fmt fmt-check vet gates clean

all: help

help:
	@echo "yzagmgr make 目标："
	@echo "  build       构建 bin/yzagmgr（CGO_ENABLED=0）"
	@echo "  test        go test ./... -count=1"
	@echo "  fmt         gofmt -w 全部 Go 文件"
	@echo "  vet         go vet ./..."
	@echo "  gates       低层门禁全跑 + reports/gates-<ts>/summary.json"
	@echo "  e2e         浏览器 E2E（真 daemon + 真 Chromium；JUnit → web/test-results/pw-junit.xml）"
	@echo "  clean       清理 bin/"

build: build-go

# Go 守护进程：先编译全部现有包（现在就有意义），cmd/yzagmgr 交付后再产出 bin/yzagmgr。
# 正式制品：设 YZAGMGR_RELEASE_VERSION=<semver 不带 -dev> 注入 main.version——二进制内版本
# 必须与发布版本（zip 名/登记版本）一致，否则客户端按「当前版本」判「可更新」会错位。
# 同一构建同时注入 main.commit（git rev-parse --short HEAD）与 main.buildTime
# （ISO8601 UTC 构建时刻）：/api/v1/version 的 commit/buildTime 字段为此前恒 unknown
# （第 1 轮评估缺陷 #2），发布件必须能自证构建来源。本地 dev 构建不注入，允许 unknown。
build-go:
	@mkdir -p bin
	CGO_ENABLED=0 $(GO) build ./...
	@if [ -d cmd/yzagmgr ]; then \
	  build_commit="$$(git rev-parse --short HEAD 2>/dev/null || echo unknown)"; \
	  build_time="$$(date -u +%Y-%m-%dT%H:%M:%SZ)"; \
	  if [ -n "$$YZAGMGR_RELEASE_VERSION" ]; then \
	    CGO_ENABLED=0 $(GO) build -ldflags "-X main.version=$$YZAGMGR_RELEASE_VERSION -X main.commit=$$build_commit -X main.buildTime=$$build_time" -o bin/yzagmgr ./cmd/yzagmgr && echo "build-go: bin/yzagmgr 已构建（CGO_ENABLED=0，纯 Go；version=$$YZAGMGR_RELEASE_VERSION commit=$$build_commit buildTime=$$build_time）"; \
	  else \
	    CGO_ENABLED=0 $(GO) build -o bin/yzagmgr ./cmd/yzagmgr && echo "build-go: bin/yzagmgr 已构建（CGO_ENABLED=0，纯 Go；version=默认 -dev，commit/buildTime 允许 unknown）"; \
	  fi; \
	else \
	  echo "build-go: pending —— cmd/yzagmgr 尚未交付（Go 守护进程由并行任务实现）；已用 CGO_ENABLED=0 go build ./... 编译全部现有包"; \
	fi

# （build-tray / tray-app 目标已随桌面面删除。）

test:
	$(GO) test ./... -count=1

# 浏览器 E2E（P4/P6 层）：真 daemon + 真 Chromium，验的是「渲染后才成立的事实」
# （可见性、几何、真实 Tab 序）——jsdom 量不出这些。自建 web/.e2e-bin/yzagmgr 与 web/dist。
e2e:
	cd web && $(NPM) run test:e2e

fmt:
	@files="$$($(GO_FIND))"; \
	if [ -z "$$files" ]; then echo "fmt: 无 Go 文件"; else gofmt -w $$files; echo "fmt: gofmt -w 完成（$$(echo $$files | wc -w | tr -d ' ') 个文件）"; fi

fmt-check:
	@files="$$($(GO_FIND))"; \
	if [ -z "$$files" ]; then echo "fmt-check: 无 Go 文件"; exit 0; fi; \
	out="$$(gofmt -l $$files)"; \
	if [ -n "$$out" ]; then echo "fmt-check: FAIL 未格式化：$$out" >&2; exit 1; fi; \
	echo "fmt-check: 干净"

vet:
	$(GO) vet ./...

gates:
	$(BASH_BIN) scripts/verify/gates.sh

clean:
	rm -rf bin
