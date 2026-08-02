# tpfile — 多线程文件传输工具

`tpfile` 是一个轻量级、跨平台（Windows / Linux）的 TCP 文件传输工具：
服务端监听端口接收文件，客户端连接后把文件（或整个目录）拆成多个分块、通过
多个并行 TCP 连接传输，两端实时显示进度条与速度。纯 Go 标准库实现，零第三方依赖，
单个可执行文件即可运行。

## ⚡ Windows 一键安装（推荐）

在 **PowerShell** 窗口中复制粘贴下面这一行命令，按回车即可自动安装最新版：

```powershell
irm https://raw.githubusercontent.com/Themcyutiany/tpfile/main/install.ps1 | iex
```

- 无需管理员权限，自动下载当前 CPU 架构（amd64 / arm64）的最新版本
- 安装到 `%LOCALAPPDATA%\tpfile`，自动加入用户 PATH
- 装完**重新打开终端**，任意目录输入 `tpfile` 即可使用

> Windows 用户看到这里就够了；Linux 安装见下一节，手动安装见下文对应章节。

## 🐧 Linux 一键安装（推荐）

在终端中复制粘贴下面这一行命令，回车即可自动安装最新版（**无需 sudo**）：

```bash
curl -fsSL https://raw.githubusercontent.com/Themcyutiany/tpfile/main/install.sh | bash
```

没有 curl 的话用 wget 也可以：

```bash
wget -qO- https://raw.githubusercontent.com/Themcyutiany/tpfile/main/install.sh | bash
```

**如果直连 GitHub 很慢或失败**，先设置代理再运行上面的命令（脚本会自动检测并提示）：

```bash
export HTTPS_PROXY=http://你的代理地址:端口   # 例如：http://192.168.1.7:7897
wget -qO- https://raw.githubusercontent.com/Themcyutiany/tpfile/main/install.sh | bash
```


- 自动下载当前 CPU 架构（amd64 / arm64）的最新版本
- 普通用户装到 `~/.local/bin` 并自动加入 PATH（无需 sudo）；root 用户装到 `/usr/local/bin`
- 装完重开终端（或 `source ~/.bashrc`），任意目录输入 `tpfile` 即可

## 特性

- **多线程并行传输**：默认 4 条并行连接（`-t` 可调），大文件显著提速
- **双端进度条**：客户端显示发送进度，服务端实时显示接收进度（百分比 / 速度 / 剩余时间）
- **断线重试**：单个分块失败自动重试（`-r` 可调），传输结果以服务端落盘确认（ACK）为准
- **IPv6 原生支持**：服务端默认双栈监听（IPv4 + IPv6），客户端支持 `[::1]:1090`、`::1:1090` 等写法
- **SOCKS5 代理**：`--proxy 127.0.0.1:7897` 可走代理出站，直连不通时非常有用
- **目录传输**：`-f` 传目录会递归发送，并保留顶层文件夹名（如 `-f .minecraft`，接收端会生成 `.minecraft/...`）；以 `.` 开头的隐藏目录可用 `ls -a` 查看
- **目录日志合并**：服务端接收目录时，逐文件的"开始/完成"日志合并为一行的开始/完成汇总，不再刷屏
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
# 方式一：安装脚本（适用于已下载的 tar.gz 安装包）
bash install.sh ./tpfile-linux-amd64
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

**一键安装（推荐，自动下载最新版）：**

```powershell
irm https://raw.githubusercontent.com/Themcyutiany/tpfile/main/install.ps1 | iex
```

脚本会自动完成下面这些事：下载对应 CPU 架构的最新版本 → 安装到
`%LOCALAPPDATA%\tpfile\tpfile.exe` → 加入用户 PATH。完成后重新打开一个终端，
任意目录输入 `tpfile` 即可。

也可以下载安装包手动安装：

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
├── install.ps1           # Windows 一键安装脚本（irm ... | iex）
├── install.sh            # Linux 一键安装脚本（curl ... | bash）
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
b9e6d0e6737f3997026056bb0e33c957a5de72b6f5660a576d032418d45066da  tpfile-github.zip
1e38ce0665f7e5fafd546145d02e1669a9e6888a431a70f465fd54fc86cf9a42  tpfile-linux-amd64
f0f26933281b1e676e45c448bd8211a9b29018d73280a34f73f275e1828a32d0  tpfile-linux-amd64.tar.gz
5de7e6e7f134f563f2673ecf05950463ef3a39c9b045d2d0f76320b88af9730e  tpfile-linux-arm64
dc446d7a63a32840ba0044c9f0ca082c7b8cc2ad348daaaa841b6915a6faf0d5  tpfile-linux-arm64.tar.gz
e17f6e2288ff367c12a54e7595e77cdd9409d05b9060b0ff573b4d8b8150b037  tpfile-windows-amd64.exe
ebed9018059c9609d9dd7ebc33e04338e739d3e539f96d2c34f84314eab51e34  tpfile-windows-amd64.zip
8e76c692aa65dbf56f0de0169eaf06ebfb5218254aeb32c578477cba41bb4806  tpfile-windows-arm64.exe
e01e7cf447397d41459875586ca00e63d443b045e6e0fc3b4e2429f40efd5e9c  tpfile-windows-arm64.zip
```

## 版本

v1.1.0
