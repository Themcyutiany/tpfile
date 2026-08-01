#!/usr/bin/env bash
# tpfile 安装脚本（支持 Linux / macOS）
#
# 用法（任选其一）：
#   bash scripts/install.sh                        # 自动选择：root 装系统目录，普通用户装用户目录
#   bash scripts/install.sh ./tpfile-linux-amd64   # 指定要安装的二进制文件
#   bash scripts/install.sh --user                 # 强制用户级安装（装到 ~/.local/bin，无需 sudo）
#   bash scripts/install.sh --system               # 强制系统级安装（装到 /usr/local/bin，需要 root）
#
# 说明：
#   - 用户级安装全程不需要 sudo，装到 ~/.local/bin，并自动把该目录加入 PATH
#   - tpfile 运行本身不需要 sudo（监听端口、收发文件都不需要管理员权限）
set -euo pipefail

MODE="auto"
SRC=""
POSITIONAL=()

for arg in "$@"; do
  case "$arg" in
    --user)   MODE="user" ;;
    --system) MODE="system" ;;
    -h|--help)
      cat <<'EOF'
用法：
  bash scripts/install.sh                        # 自动选择安装方式
  bash scripts/install.sh ./tpfile-linux-amd64   # 指定二进制文件
  bash scripts/install.sh --user                 # 用户级安装（无需 sudo）
  bash scripts/install.sh --system               # 系统级安装（需要 root）
EOF
      exit 0
      ;;
    *) POSITIONAL+=("$arg") ;;
  esac
done

# 定位要安装的二进制文件
if [ -n "${POSITIONAL[0]:-}" ]; then
  SRC="${POSITIONAL[0]}"
else
  for cand in ./tpfile-linux-amd64 ./tpfile-linux-arm64 ./dist/tpfile-linux-amd64 ./dist/tpfile-linux-arm64; do
    if [ -f "$cand" ]; then
      SRC="$cand"
      break
    fi
  done
fi

if [ -z "$SRC" ] || [ ! -f "$SRC" ]; then
  echo "错误：找不到 tpfile 二进制文件。" >&2
  echo "请先构建或下载，再重新运行，例如：" >&2
  echo "  bash scripts/install.sh ./tpfile-linux-amd64" >&2
  exit 1
fi

# 决定安装目录
case "$MODE" in
  user)   DEST_DIR="${XDG_BIN_HOME:-$HOME/.local/bin}" ;;
  system) DEST_DIR="/usr/local/bin" ;;
  auto)
    if [ "$(id -u)" -eq 0 ]; then
      DEST_DIR="/usr/local/bin"
    else
      DEST_DIR="${XDG_BIN_HOME:-$HOME/.local/bin}"
    fi
    ;;
esac

mkdir -p "$DEST_DIR"
DEST="$DEST_DIR/tpfile"
install -m 0755 "$SRC" "$DEST"
echo "✔ 安装完成：$DEST"

if [ "$DEST_DIR" = "/usr/local/bin" ]; then
  echo "✔ 所有用户现在都可以直接使用 tpfile 命令。"
else
  # 用户级安装：把 ~/.local/bin 加入 PATH（只加一次）
  SHELLRC=""
  case "${SHELL:-}" in
    *zsh)  SHELLRC="$HOME/.zshrc" ;;
    *fish) SHELLRC="$HOME/.config/fish/config.fish" ;;
    *bash) SHELLRC="$HOME/.bashrc" ;;
    *)
      if [ -f "$HOME/.bashrc" ]; then SHELLRC="$HOME/.bashrc"; else SHELLRC="$HOME/.profile"; fi
      ;;
  esac

  if [ "$(basename "$SHELLRC")" = "config.fish" ]; then
    LINE="fish_add_path $HOME/.local/bin"
  else
    LINE='export PATH="$HOME/.local/bin:$PATH"'
  fi

  if grep -qsF "$LINE" "$SHELLRC"; then
    echo "✔ PATH 已包含 ~/.local/bin，无需重复添加。"
  else
    printf '\n%s\n' "$LINE" >> "$SHELLRC"
    echo "✔ 已把 ~/.local/bin 加入 PATH（写入 $SHELLRC）"
  fi
  echo "请执行下面命令让 PATH 立即生效（或者重新打开终端）："
  echo "  source $SHELLRC"
fi

"$DEST" --version
echo "✔ 完成！现在可以在任意目录直接运行 tpfile，不需要 sudo。"
