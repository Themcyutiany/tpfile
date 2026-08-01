GO      ?= go
BIN     := tpfile
LDFLAGS := -s -w

.PHONY: all build test vet linux windows release pack clean install install-user

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

# 打包发布文件：Windows 用 zip，Linux 用 tar.gz（均包含对应平台的安装脚本）
pack: release
	cd dist && cp ../scripts/install.bat . && cp ../scripts/install.ps1 . && \
	zip -q tpfile-windows-amd64.zip tpfile-windows-amd64.exe install.bat install.ps1 && \
	zip -q tpfile-windows-arm64.zip tpfile-windows-arm64.exe install.bat install.ps1 && \
	rm -f install.bat install.ps1
	cd dist && cp ../scripts/install.sh . && \
	tar -czf tpfile-linux-amd64.tar.gz tpfile-linux-amd64 install.sh && \
	tar -czf tpfile-linux-arm64.tar.gz tpfile-linux-arm64 install.sh && \
	rm -f install.sh
	rm -f dist/tpfile-windows-amd64.exe dist/tpfile-windows-arm64.exe dist/tpfile-linux-amd64 dist/tpfile-linux-arm64

clean:
	rm -rf dist $(BIN)
