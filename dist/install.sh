#!/usr/bin/env bash
# tpfile 一键安装脚本（Linux）
# 用法：
#   curl -fsSL https://raw.githubusercontent.com/Themcyutiany/tpfile/main/install.sh | bash
#   wget -qO-  https://raw.githubusercontent.com/Themcyutiany/tpfile/main/install.sh | bash
# 说明：无需 sudo（普通用户装到 ~/.local/bin 并自动加入 PATH；root 装到 /usr/local/bin）。
set -euo pipefail

REPO="Themcyutiany/tpfile"

# macOS 暂未提供官方安装包
if [ "$(uname -s)" = "Darwin" ]; then
  echo "tpfile 目前只提供 Windows / Linux 安装包，macOS 请从源码构建。" >&2
  exit 1
fi

# 1. 获取最新版本号（优先 GitHub API，失败时回退到固定版本）
TAG=""
if command -v curl >/dev/null 2>&1; then
  TAG="$(curl -fsSL --connect-timeout 10 "https://api.github.com/repos/$REPO/releases/latest" 2>/dev/null | sed -n 's/.*"tag_name": *"\([^"]*\)".*/\1/p' | head -n1 || true)"
fi
if [ -z "$TAG" ] && command -v wget >/dev/null 2>&1; then
  TAG="$(wget -qO- --timeout=10 "https://api.github.com/repos/$REPO/releases/latest" 2>/dev/null | sed -n 's/.*"tag_name": *"\([^"]*\)".*/\1/p' | head -n1 || true)"
fi
if [ -z "$TAG" ]; then TAG="1.4.1"; fi

# 2. 根据 CPU 架构选择安装包
case "$(uname -m)" in
  x86_64|amd64) SUFFIX="amd64" ;;
  aarch64|arm64) SUFFIX="arm64" ;;
  *)
    echo "错误：不支持该 CPU 架构：$(uname -m)" >&2
    exit 1
    ;;
esac
ASSET="tpfile-linux-$SUFFIX.tar.gz"
URL="https://github.com/$REPO/releases/download/$TAG/$ASSET"

# 3. 下载到临时目录并解压
TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT
echo "正在下载 $ASSET（版本 $TAG）..."
# 若已设置代理环境变量，提示用户正在走代理
if [ -n "${HTTPS_PROXY:-}${https_proxy:-}${ALL_PROXY:-}${all_proxy:-}" ]; then
  echo "使用代理：${HTTPS_PROXY:-${https_proxy:-${ALL_PROXY:-${all_proxy:-}}}}"
fi
# 优先 curl（带进度条 + 自动重试），失败时回退到 wget（同样带进度条）
if command -v curl >/dev/null 2>&1; then
  if ! curl -fL --retry 3 --progress-bar --connect-timeout 15 -o "$TMP/$ASSET" "$URL"; then
    wget --show-progress --timeout=30 -O "$TMP/$ASSET" "$URL"
  fi
else
  wget --show-progress --timeout=30 -O "$TMP/$ASSET" "$URL"
fi
echo ""
# 3.1 校验下载完整性（对照发布页的 sha256sums.txt）
if command -v sha256sum >/dev/null 2>&1; then
  SUMFILE="$TMP/sha256sums.txt"
  if { command -v curl >/dev/null 2>&1 && curl -fsSL --connect-timeout 10 "https://github.com/$REPO/releases/download/$TAG/sha256sums.txt" -o "$SUMFILE" 2>/dev/null; } || { command -v wget >/dev/null 2>&1 && wget -qO- --timeout=10 "https://github.com/$REPO/releases/download/$TAG/sha256sums.txt" 2>/dev/null > "$SUMFILE"; }; then
    WANT="$(awk -v a="$ASSET" '$2 == a {print $1; exit}' "$SUMFILE")"
    if [ -n "$WANT" ]; then
      GOT="$(sha256sum "$TMP/$ASSET" | awk '{print $1}')"
      if [ "$GOT" != "$WANT" ]; then
        echo "错误：$ASSET 的 SHA-256 与发布页不一致，请重新运行安装。" >&2
        exit 1
      fi
      echo "校验通过：$ASSET 与发布页 SHA-256 一致"
    else
      echo "警告：发布页未列出 $ASSET 的校验值，跳过校验。"
    fi
  else
    echo "警告：无法获取校验文件，跳过校验。"
  fi
fi

tar -xzf "$TMP/$ASSET" -C "$TMP"

# 4. 决定安装目录并安装
if [ "$(id -u)" -eq 0 ]; then
  DEST_DIR="/usr/local/bin"
else
  DEST_DIR="${XDG_BIN_HOME:-$HOME/.local/bin}"
fi
mkdir -p "$DEST_DIR"
DEST="$DEST_DIR/tpfile"
install -m 0755 "$TMP/tpfile-linux-$SUFFIX" "$DEST"
echo "已安装到：$DEST"

# 5. 用户级安装时把目录加入 PATH（只加一次）
if [ "$DEST_DIR" != "/usr/local/bin" ]; then
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
    echo "PATH 已包含 ~/.local/bin，无需重复添加。"
  else
    printf '\n%s\n' "$LINE" >> "$SHELLRC"
    echo "已把 ~/.local/bin 加入 PATH（写入 $SHELLRC）"
  fi
  echo "请执行下面命令让 PATH 立即生效（或者重新打开终端）："
  echo "  source $SHELLRC"
fi

# 6. 验证
"$DEST" --version
echo "安装完成！现在可以在任意目录直接运行 tpfile。"
