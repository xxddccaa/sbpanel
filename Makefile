# sbpanel build tasks. `make help` lists them.

VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -s -w -X main.version=$(VERSION)
ARCHES  := amd64 arm64 armv7 386

.PHONY: help build test lint dist run install clean

help:
	@echo "make build    编译当前平台的 sbpanel"
	@echo "make test     运行测试"
	@echo "make lint     gofmt / go vet / 前端语法检查"
	@echo "make dist     交叉编译所有 Linux 架构并打包到 dist/（发布用）"
	@echo "make run      在 ./dev-data 用开发模式运行面板（127.0.0.1:8091）"
	@echo "make install  编译并安装成本机的 systemd 服务（需要 sudo）"

build:
	CGO_ENABLED=0 go build -trimpath -ldflags "$(LDFLAGS)" -o sbpanel .

test:
	go test ./...

lint:
	@unformatted="$$(gofmt -l .)"; if [ -n "$$unformatted" ]; then echo "需要 gofmt:"; echo "$$unformatted"; exit 1; fi
	go vet ./...
	@if command -v node >/dev/null 2>&1; then node --check web/app.js; fi

dist:
	rm -rf build dist
	mkdir -p dist
	@set -e; for arch in $(ARCHES); do \
		goarch=$$arch; goarm=; \
		if [ "$$arch" = armv7 ]; then goarch=arm; goarm=7; fi; \
		echo "build linux/$$arch"; \
		mkdir -p build/$$arch; \
		CGO_ENABLED=0 GOOS=linux GOARCH=$$goarch GOARM=$$goarm \
			go build -trimpath -ldflags "$(LDFLAGS)" -o build/$$arch/sbpanel .; \
		cp LICENSE README.md build/$$arch/; \
		tar -czf dist/sbpanel-linux-$$arch.tar.gz -C build/$$arch sbpanel LICENSE README.md; \
	done
	cd dist && sha256sum *.tar.gz > sha256sums.txt
	@cat dist/sha256sums.txt

run: build
	mkdir -p dev-data
	SBPANEL_LISTEN=127.0.0.1:8091 ./sbpanel serve --data ./dev-data

install: build
	sudo ./sbpanel install

clean:
	rm -rf sbpanel build dist
