#!/usr/bin/env bash
# sbpanel 一键安装 / 更新脚本
#
#   bash <(curl -fsSL https://raw.githubusercontent.com/xxddccaa/sbpanel/main/install.sh)
#
# 可选环境变量：
#   SBPANEL_VERSION=v0.1.0          安装指定版本（默认最新版）
#   GH_PROXY=https://ghfast.top/    访问不了 GitHub 时使用的加速前缀
#   HTTPS_PROXY=http://...          也可以直接走代理下载
#
# 其余参数会原样传给 `sbpanel install`，例如：
#   bash <(curl -fsSL .../install.sh) --port 8092
#   bash <(curl -fsSL .../install.sh) --listen 127.0.0.1:8091
set -euo pipefail

REPO="xxddccaa/sbpanel"
VERSION="${SBPANEL_VERSION:-latest}"

die() {
	printf '\033[31m错误: %s\033[0m\n' "$*" >&2
	exit 1
}

[ "$(uname -s)" = "Linux" ] || die "只支持 Linux"
[ "$(id -u)" -eq 0 ] || die "需要 root 权限。请切换到 root，或者运行：sudo bash -c \"\$(curl -fsSL https://raw.githubusercontent.com/${REPO}/main/install.sh)\""
command -v systemctl >/dev/null 2>&1 || die "需要 systemd"
command -v tar >/dev/null 2>&1 || die "需要 tar"
command -v sha256sum >/dev/null 2>&1 || die "需要 sha256sum（coreutils）"

case "$(uname -m)" in
x86_64 | amd64) ARCH=amd64 ;;
aarch64 | arm64) ARCH=arm64 ;;
armv7* | armv8l) ARCH=armv7 ;;
i386 | i686) ARCH=386 ;;
*) die "暂不支持这个 CPU 架构: $(uname -m)" ;;
esac

if [ "$VERSION" = "latest" ]; then
	BASE="https://github.com/${REPO}/releases/latest/download"
else
	BASE="https://github.com/${REPO}/releases/download/${VERSION}"
fi
ASSET="sbpanel-linux-${ARCH}.tar.gz"

# download URL OUTPUT: tries $GH_PROXY first (when set), then GitHub directly.
download() {
	local url="$1" output="$2" candidate
	local candidates=()
	if [ -n "${GH_PROXY:-}" ]; then
		candidates+=("${GH_PROXY%/}/${url}")
	fi
	candidates+=("$url")
	for candidate in "${candidates[@]}"; do
		if command -v curl >/dev/null 2>&1; then
			curl -fsSL --retry 2 --connect-timeout 15 -o "$output" "$candidate" && return 0
		elif command -v wget >/dev/null 2>&1; then
			wget -q -T 15 -O "$output" "$candidate" && return 0
		else
			die "需要 curl 或 wget"
		fi
	done
	return 1
}

TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT

echo "→ 下载 sbpanel ${VERSION}（linux/${ARCH}）…"
download "${BASE}/${ASSET}" "${TMP}/${ASSET}" ||
	die "下载失败。访问不了 GitHub 的话，先设置 GH_PROXY（例如 GH_PROXY=https://ghfast.top/）或 HTTPS_PROXY 再运行"
download "${BASE}/sha256sums.txt" "${TMP}/sha256sums.txt" || die "下载校验文件失败"

echo "→ 校验 SHA-256…"
(cd "$TMP" && grep " ${ASSET}\$" sha256sums.txt | sha256sum -c --status) ||
	die "SHA-256 校验失败，文件可能不完整或被篡改"

tar -xzf "${TMP}/${ASSET}" -C "$TMP" sbpanel
chmod 755 "${TMP}/sbpanel"
"${TMP}/sbpanel" install "$@"
