# tpfile — 局域网交互式文件传输工具

`tpfile` 是一个轻量级、跨平台（Windows / Linux）的**交互式**文件传输工具：
服务端启动后等待客户端连接，每个连接就是一个「用户」；连接建立后，客户端和服务端
都可以在 `>` 提示符下输入指令互相传文件、查看目录、测延迟、踢人。纯 Go 标准库实现，
零第三方依赖，单个可执行文件即可运行。

## ⚡ 一键安装

**Windows（PowerShell 里粘贴这一行，回车）：**

```powershell
irm https://raw.githubusercontent.com/Themcyutiany/tpfile/main/install.ps1 | iex
```

**Linux（终端里粘贴这一行，回车，无需 sudo）：**

```bash
curl -fsSL https://raw.githubusercontent.com/Themcyutiany/tpfile/main/install.sh | bash
```

> 装完**重新打开终端**，任意目录输入 `tpfile` 即可使用。直连 GitHub 慢时先设置代理：
> `export HTTPS_PROXY=http://你的代理地址:端口`（Windows 则在 PowerShell 先执行 `$env:HTTPS_PROXY='http://你的代理地址:端口'`）。
> 也可以直接从仓库 `dist/` 目录或 [GitHub Releases](https://github.com/Themcyutiany/tpfile/releases) 下载编译好的二进制。

## ✨ 特性

- **交互式会话**：客户端连上服务端后不退出，像聊天一样持续传文件
- **双向传输**：客户端可上传，也可下载服务端文件；服务端同样可以推/拉用户文件
- **用户管理**：服务端可查看在线用户、查看用户目录、踢出用户
- **多线程并行**：每个文件 `-t` 个并行连接，多个文件 `-j` 个同时传输
- **单行进度**：进度条/速度/剩余时间/当前文件合并成一行实时刷新，传输中照样可以输入指令
- **目录传输**：发送文件夹会保留顶层文件夹名（如 `.minecraft`），隐藏目录用 `ls -a` 查看
- **目录日志合并**：接收文件夹时，逐文件日志合并为一行的开始/完成汇总，不再刷屏
- **IPv6 原生支持**：服务端默认双栈监听（IPv4 + IPv6）
- **SOCKS5 代理**：客户端可走 `--proxy` 代理出站
- **安全细节**：文件名清洗防目录穿越；同名文件自动追加 `(1)`、`(2)` 后缀

## ⚡ 快速开始

```bash
# 服务端（接收）：监听 1090 端口，文件保存到当前目录
tpfile -s

# 服务端：指定端口与保存目录
tpfile -s -p 9000 -d /data/incoming

# 客户端：连接服务端，进入交互会话
tpfile -c 192.168.1.5:1090
```

连接成功后两端都会出现 `>` 提示符，直接输入指令即可。

## ⌨️ 交互指令

### 客户端指令

| 指令 | 说明 |
| --- | --- |
| `tp 文件或文件夹` | 发送本地文件/目录到服务端（保留顶层文件夹名） |
| `tp -me 服务端文件` | 从服务端下载文件到本地 |
| `ls` | 查看服务端当前目录 |
| `ping` | 测试与服务端的延迟 |
| `stop` / `Ctrl+C` | 断开连接 |

### 服务端指令

| 指令 | 说明 |
| --- | --- |
| `list` | 列出已连接的用户（用户 id） |
| `ls 用户id` | 查看该用户客户端当前目录 |
| `ping 用户id` | 测试与该用户的延迟 |
| `kick 用户id` | 踢出该用户（对方会收到提示并退出） |
| `tp 文件 用户id` | 发送服务端文件到该用户 |
| `tp -me 用户id 文件` | 从该用户客户端拉取文件到服务端 |
| `stop` / `Ctrl+C` | 停止服务 |

> 服务端指令里的相对路径基于服务端保存目录（`-d`）；`tp -me` 里的路径是对方
> 客户端当前目录里的文件。传输进行中两端都可以继续输入其它指令。

## 🌰 使用示例

```bash
# 服务端
tpfile -s -d ~/incoming
# 用户 1 连接后：
list                    # 查看在线用户
ls 1                    # 查看用户 1 客户端的目录
tp server-file.txt 1    # 把服务端的 server-file.txt 发给用户 1
tp -me 1 client-file.txt  # 把用户 1 客户端的 client-file.txt 拉到服务端
ping 1                  # 测延迟
kick 1                  # 踢出用户 1

# 客户端
tpfile -c 192.168.1.5:1090
tp ./photos             # 上传整个目录（保留 photos/ 顶层文件夹名）
tp -me server-file.txt  # 下载服务端文件
ls                      # 看服务端目录
ping                    # 测延迟
stop                    # 断开
```

## 🛠️ 参数

| 参数 | 说明 |
| --- | --- |
| `-s, --serve` | 服务端模式 |
| `-c, --connect 地址` | 连接模式，如 `192.168.1.5:1090`、`[::1]:1090`；不带端口时默认 1090 |
| `-p, --port 端口` | 端口，默认 `1090` |
| `-d, --dir 目录` | 服务端保存目录，默认当前目录 |
| `-t, --threads N` | 每个文件的并行连接数，默认 `4` |
| `-j, --jobs N` | 同时并行传输的文件数，默认 `4` |
| `-r, --retries N` | 分块失败重试次数，默认 `3` |
| `--proxy 地址` | 客户端出站走 SOCKS5 代理，如 `127.0.0.1:7897` |
| `-v, --verbose` | 显示详细日志 |
| `--version` | 显示版本 |
| `-h, --help` | 显示帮助 |

## 🔧 工作原理

1. 客户端连接服务端后发送 `hello` 建立会话（服务端分配用户 id），并开放一个入站
   端口用于接收服务端的文件推送。
2. 传文件时，发送方把文件按 `-t` 切成若干分块，通过多条并行 TCP 连接发送；首部
   携带会话令牌，接收方校验令牌后按偏移落盘（`WriteAt`），分块可乱序到达。
3. 每个分块落盘后接收方回复 `ok` 确认，重试完全幂等；`-j` 让多个文件同时传输。
4. 控制连接上跑 `ls / ping / kick / send` 等指令，与文件传输互不阻塞，因此
   进度条显示时依然可以输入指令。

## 🧪 从源码构建与测试

需要 Go 1.26+（纯标准库，无第三方依赖）：

```bash
go build .                  # 构建当前平台
go test ./... -count=1      # 运行全部测试
go vet ./...                # 静态检查

# 或使用 Makefile
make build
make test
make release                # 交叉编译 Linux + Windows 共 4 个二进制到 dist/
```

## 📂 文件结构

```
tpfile/
├── main.go               # 命令行入口与参数解析
├── client.go             # 客户端：交互会话、入站接收、指令处理
├── server.go             # 服务端：用户管理、指令处理、推送/拉取
├── transfer.go           # 传输引擎：分块发送/接收、进度、重试（两端共用）
├── proto.go              # 协议：控制消息 + 分块首部
├── ui.go                 # 终端 UI：提示符、单行进度、日志协作
├── progress.go           # 发送端整体进度条
├── util.go               # 路径清洗、切块计划、地址解析等
├── proxy.go              # SOCKS5 代理客户端
├── *_test.go             # 单元测试与端到端集成测试
├── Makefile
├── install.ps1           # Windows 一键安装脚本
├── install.sh            # Linux 一键安装脚本
├── scripts/              # 安装脚本（打包进安装包）
└── dist/                 # 编译好的各平台二进制与安装包
```

## 🔐 校验文件哈希值

仓库 `dist/` 目录也直接存放了编译好的各平台二进制与安装包（与 Release 内容一致），可直接下载。

以下是所有发布文件的 SHA-256 校验值（与 `sha256sums.txt` 内容一致，可用于校验下载完整性）：

```
148f514fd8d63ea5773b81c74df07e0df21e7378a9986f8cf1bfe7d71500e25e  install.bat
fbc83c8077287f35fb186a3a1b5a06812b001e59a798696520598c1a46c8f779  install.ps1
1a80fe4f8a9520747867038a41d27dcd5f4ae30348bdb3c5a060913f5b56d001  install.sh
4d59d6ea8259df14a7f7bdfc0a242cba22f98f959b550fcba7a9321a1df78311  tpfile-github.zip
226f54d6f041de1a8fcec5651813aa694c5b13683cd06e3b97118a2fa6417cbd  tpfile-linux-amd64
a6cd766bab344f0ed20315403a450fe8517c3c96cf62519b77284785922118c1  tpfile-linux-amd64.tar.gz
a5326aa7ffc407524aee64ea5de481beba0df85c2dcf69337f4b2f6ce269ad8e  tpfile-linux-arm64
66e5ed5b2e6269a83c37bd71572a5e7ac615dae69b7cbe23166237cf404bab28  tpfile-linux-arm64.tar.gz
0933818dcae7fcd6526aa38ce6ff2b87a4610e683e3d60ad7a0836d707def640  tpfile-windows-amd64.exe
3e57af1c04fe2c5bd70b42448ed2984386038f74c6c8633e59caae66b2a25156  tpfile-windows-amd64.zip
2a9dc0e87942db6d1c3ffa7b98e3b72a9541914fbec6f0ea25cd4aa79c91a4cf  tpfile-windows-arm64.exe
75529ba7dd858fe3e1188783d9c6cb8f255cbba24077895d269c9c6b639e4891  tpfile-windows-arm64.zip
```

## 版本

v1.3.1
