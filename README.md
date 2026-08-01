# tpfile — 多线程文件传输工具

`tpfile` 是一个轻量级、跨平台（Windows / Linux）的 TCP 文件传输工具：
服务端监听端口接收文件，客户端连接后把文件（或整个目录）拆成多个分块、通过
多个并行 TCP 连接传输，两端实时显示进度条与速度。纯 Go 标准库实现，零第三方依赖，
单个可执行文件即可运行。

## 特性

- **多线程并行传输**：默认 4 条并行连接（`-t` 可调），大文件显著提速
- **双端进度条**：客户端显示发送进度，服务端实时显示接收进度（百分比 / 速度 / 剩余时间）
- **断线重试**：单个分块失败自动重试（`-r` 可调），传输结果以服务端落盘确认（ACK）为准
- **IPv6 原生支持**：服务端默认双栈监听（IPv4 + IPv6），客户端支持 `[::1]:1090`、`::1:1090` 等写法
- **SOCKS5 代理**：`--proxy 127.0.0.1:7897` 可走代理出站，直连不通时非常有用
- **目录传输**：`-f` 传目录会递归发送，服务端自动重建目录结构
- **多文件**：`-f` 可重复使用，一次连接发送多个文件
- **安全细节**：文件名清洗防目录穿越；同名文件自动追加 `(1)`、`(2)` 等后缀
- **IPv4/IPv6 双栈监听**，端口默认 **1090**

## 快速开始

```bash
# 服务端（接收）：监听 1090 端口，文件保存到当前目录
tpfile -s

# 服务端：指定端口与保存目录
tpfile -s -p 9000 -d /data/incoming

# 客户端（发送）：连接并发送单个文件（-c 不带端口时默认 1090）
tpfile -c 192.168.1.5 -f movie.mp4

# 客户端：8 线程发送整个目录，走 SOCKS5 代理
tpfile -c 192.168.1.5:1090 -f ./photos -t 8 --proxy 127.0.0.1:7897
```

## Linux 安装（任意目录可用，无需 sudo）

tpfile 运行时**不需要 sudo**（监听端口、收发文件都不需要管理员权限）。推荐用下面的
用户级安装，全程不需要 sudo：

```bash
# 方式一：一键安装脚本（推荐，无需 sudo）
bash scripts/install.sh ./tpfile-linux-amd64
# 脚本会自动装到 ~/.local/bin，并把该目录加入 PATH（写入 ~/.bashrc 等）
# 按提示执行下面命令让 PATH 立即生效（或重新打开终端）：
source ~/.bashrc

# 方式二：手动安装（同样无需 sudo）
mkdir -p ~/.local/bin
install -m 755 tpfile-linux-amd64 ~/.local/bin/tpfile
echo 'export PATH="$HOME/.local/bin:$PATH"' >> ~/.bashrc
source ~/.bashrc

# 方式三：系统级安装（所有用户可用，只有这一步需要 sudo）
sudo bash scripts/install.sh --system ./tpfile-linux-amd64

# 安装后验证（任意目录均可执行，运行本身不需要 sudo）
tpfile --version
```

> 脚本参数：`--user` 强制用户级安装（无需 sudo）；`--system` 强制装到
> `/usr/local/bin`（需要 root）。不带参数时自动选择：普通用户走用户级，root 走系统级。

## Windows 安装（任意目录可用，无需管理员权限）

在 Windows 上想让任意目录都能直接输入 `tpfile`，运行安装脚本即可（无需管理员权限，
脚本会把程序装到用户目录并自动加入用户 PATH）：

```powershell
# 在 tpfile-windows-amd64.exe 所在目录打开 PowerShell，然后运行：
powershell -ExecutionPolicy Bypass -File install.ps1
# 或者直接双击 install.bat

# 安装完成后，重新打开一个终端（或新开窗口），任意目录输入：
tpfile --version
tpfile -s
```

> 说明：脚本把 `tpfile-windows-amd64.exe` 复制为
> `%LOCALAPPDATA%\tpfile\tpfile.exe`，并把该目录加入用户 PATH（只加一次，
> 保留原有 PATH 格式）。卸载时删除这两个东西即可：`%LOCALAPPDATA%\tpfile`
> 目录和用户 PATH 里对应的条目。


## 参数说明

| 参数 | 说明 |
| --- | --- |
| `-s, --serve` | 服务端模式：监听并接收文件 |
| `-c, --connect 地址` | 客户端模式：目标地址，如 `192.168.1.5:1090`、`[::1]:1090`；**不带端口时默认 1090**，可用 `-p` 覆盖 |
| `-p, --port 端口` | 端口，默认 `1090` |
| `-d, --dir 目录` | 服务端保存目录，默认当前目录 |
| `-f, --file 路径` | 要发送的文件或目录，可重复；目录会递归发送 |
| `-t, --threads N` | 每个文件的并行连接数，默认 `4` |
| `-r, --retries N` | 分块失败重试次数，默认 `3` |
| `--proxy 地址` | 出站走 SOCKS5 代理，如 `127.0.0.1:7897` |
| `-v, --verbose` | 显示详细日志 |
| `--version` | 显示版本 |
| `-h, --help` | 显示帮助 |

## 使用示例

```bash
# 局域网传输（Linux 接收，Windows 发送）
# 接收端（Linux）
./tpfile-linux-amd64 -s -d ~/incoming
# 发送端（Windows）
tpfile-windows-amd64.exe -c 192.168.1.100 -f D:\movies\big.mp4 -t 8

# IPv6 传输
tpfile -s
tpfile -c [::1]:1090 -f a.bin
tpfile -c ::1:1090 -f a.bin          # 也支持无方括号写法

# 直连不通时走代理（127.0.0.1:7897 常见于 Clash 等）
tpfile -c 服务器地址:1090 -f a.bin --proxy 127.0.0.1:7897

# 一次发送多个文件 + 整个目录
tpfile -c 192.168.1.5:1090 -f a.txt -f b.zip -f ./docs

# 服务端端口与保存目录
tpfile -s -p 9000 -d D:\incoming
```

## 工作原理

1. 客户端把文件按线程数切块（小于 256 KiB 的块自动合并，小文件用较少连接）。
2. 每个分块用一条独立 TCP 连接发送：首部为 JSON（传输 ID、文件名、偏移、长度），
   随后是分块数据。
3. 服务端用同一文件句柄按偏移并发写入（`WriteAt`），分块可乱序到达，
   同时按实际写入字节实时统计接收进度。
4. 服务端落盘成功后回复 `ok` 确认；客户端收到全部确认才算传输完成，因此
   进度与"完成"信息是真实的服务端落盘结果，重试也完全幂等。
5. 服务端监听 `[::]` 双栈地址，Windows / Linux 均可同时接受 IPv4 与 IPv6 连接。

## 从源码构建与测试

需要 Go 1.26+（纯标准库，无第三方依赖）：

```bash
go build .                  # 构建当前平台
go test ./...               # 运行全部测试
go vet ./...                # 静态检查

# 或使用 Makefile
make build                  # 当前平台
make test
make release                # 交叉编译 Linux + Windows 共 4 个二进制到 dist/
make install                # Linux 安装（普通用户无需 sudo）
make install-user            # 强制用户级安装（无需 sudo）
```

## 文件结构

```
tpfile/
├── main.go               # 命令行入口与参数解析
├── client.go             # 客户端：切块、并行发送、重试
├── server.go             # 服务端：接收、并发落盘、进度渲染
├── proto.go              # 分块协议（头部 + ACK）
├── proxy.go              # SOCKS5 代理客户端
├── progress.go           # 进度条渲染
├── util.go               # 地址解析、路径清洗、切块计划等
├── *_test.go             # 单元测试与端到端集成测试
├── Makefile
├── scripts/install.sh    # Linux 安装脚本（用户级免 sudo / 系统级）
├── scripts/install.ps1   # Windows 安装脚本（免管理员，自动加入 PATH）
├── scripts/install.bat   # Windows 安装脚本的便捷入口（可双击）
└── .github/workflows/    # CI：测试 + 交叉编译 + Release
```

## 校验文件哈希值

以下是所有发布文件的 SHA-256 校验值（与 `sha256sums.txt` 内容一致，可用于校验下载完整性）：

```
148f514fd8d63ea5773b81c74df07e0df21e7378a9986f8cf1bfe7d71500e25e  install.bat
fbc83c8077287f35fb186a3a1b5a06812b001e59a798696520598c1a46c8f779  install.ps1
1a80fe4f8a9520747867038a41d27dcd5f4ae30348bdb3c5a060913f5b56d001  install.sh
92274bff56dc034bac1321007bbb59ef27b0b87edb7875ef2d3d53234fc3cc2e  tpfile-github.zip
a9645c9331f7431da7d031e4c9f525d81a985d664467510aad405f38b6ff52aa  tpfile-linux-amd64
7cb8efccc20990c09c2321c6d6cbaea84f6bfaa0bc78813e9b00025311ec1879  tpfile-linux-amd64.tar.gz
83438b63845f6ab51ed2b39666fe2d63756968bb3481f42890a5b0abee4c5db8  tpfile-linux-arm64
ba9d09c14695907074bb3ff2ca55b8788ac1c51ccc9d079e06ab213066169f53  tpfile-linux-arm64.tar.gz
4350a71b5d135da5fa2314ad373d44e06a7545342746ceb9b83b598865afbc08  tpfile-windows-amd64.exe
722b02204e1cc34a7dff7db161e0caa8422d4b0feb03820f0d4a69e6c3fb40f2  tpfile-windows-amd64.zip
dfaf80fa66475b5eb44055fc0a8df4fcffa2bef8d67b88c24f6afb90e9f233e9  tpfile-windows-arm64.exe
55042f7d0e60ad3ec333c468257c7a20981b0acf79446e920af0d82e128a1ab4  tpfile-windows-arm64.zip
```

## 版本

v1.0.0
