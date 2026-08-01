GO      ?= go
BIN     := tpfile
LDFLAGS := -s -w

.PHONY: all build test vet linux windows release clean install install-user

all: build

build:
	$(GO) build -trimpath -ldflags "$(LDFLAGS)" -o $(BIN) .

test:
	$(GO) test ./... -count=1

vet:
	$(GO) vet ./...

# 交叉编译 Linux / Windows 各两个架构
linux:
	GOOS=linux   GOARCH=amd64 CGO_ENABLED=0 $(GO) build -trimpath -ldflags "$(LDFLAGS)" -o dist/tpfile-linux-amd64 .
	GOOS=linux   GOARCH=arm64 CGO_ENABLED=0 $(GO) build -trimpath -ldflags "$(LDFLAGS)" -o dist/tpfile-linux-arm64 .

windows:
	GOOS=windows GOARCH=amd64 CGO_ENABLED=0 $(GO) build -trimpath -ldflags "$(LDFLAGS)" -o dist/tpfile-windows-amd64.exe .
	GOOS=windows GOARCH=arm64 CGO_ENABLED=0 $(GO) build -trimpath -ldflags "$(LDFLAGS)" -o dist/tpfile-windows-arm64.exe .

release: linux windows

# Linux 安装（普通用户默认装到 ~/.local/bin，无需 sudo；root 装到 /usr/local/bin）
install:
	bash scripts/install.sh

# 强制用户级安装（无需 sudo，装到 ~/.local/bin）
install-user:
	bash scripts/install.sh --user

clean:
	rm -rf dist $(BIN)
