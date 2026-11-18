#!/usr/bin/env bash
# sbpanel 一键安装 / 更新脚本
#
#   bash <(curl -fsSL https://raw.githubusercontent.com/xxddccaa/sbpanel/main/install.sh)
#
# 自动选择安装方式：
#   root + systemd       装成 systemd 服务，开机自启
#   root，没有 systemd   （容器、Kubernetes 等）装到系统目录，以后台进程运行
#   普通用户             装到 ~/.local，以后台进程运行
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
command -v tar >/dev/null 2>&1 || die "需要 tar"
if [ "$(id -u)" -ne 0 ]; then
	echo "→ 当前不是 root，将安装到 ~/.local（只对当前用户有效，没有开机自启）"
fi

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

# exec_tmpdir prints a new temporary directory that allows running programs. /tmp is
# often mounted noexec (hardened servers, Docker --tmpfs), so try a few places.
exec_tmpdir() {
	local base dir
	for base in "${TMPDIR:-/tmp}" /var/tmp "${HOME:-/root}/.cache" /dev/shm "$PWD"; do
		mkdir -p "$base" 2>/dev/null || continue
		dir="$(mktemp -d "${base}/sbpanel-install.XXXXXX" 2>/dev/null)" || continue
		printf '#!/bin/sh\nexit 0\n' >"${dir}/probe" 2>/dev/null && chmod 700 "${dir}/probe" && "${dir}/probe" 2>/dev/null &&
			{ rm -f "${dir}/probe"; echo "$dir"; return 0; }
		rm -rf "$dir"
	done
	return 1
}

TMP="$(exec_tmpdir)" || die "找不到可以运行程序的临时目录（/tmp 等都禁止执行）。可以设置 TMPDIR 指向一个允许执行的目录再运行"
trap 'rm -rf "$TMP"' EXIT

echo "→ 下载 sbpanel ${VERSION}（linux/${ARCH}）…"
download "${BASE}/${ASSET}" "${TMP}/${ASSET}" ||
	die "下载失败。访问不了 GitHub 的话，先设置 GH_PROXY（例如 GH_PROXY=https://ghfast.top/）或 HTTPS_PROXY 再运行"
download "${BASE}/sha256sums.txt" "${TMP}/sha256sums.txt" || die "下载校验文件失败"

echo "→ 校验 SHA-256…"
expected="$(awk -v name="$ASSET" '$2 == name || $2 == "*" name { print $1 }' "${TMP}/sha256sums.txt")"
[ -n "$expected" ] || die "校验文件里没有 ${ASSET}"
if command -v sha256sum >/dev/null 2>&1; then
	actual="$(sha256sum "${TMP}/${ASSET}" | awk '{ print $1 }')"
elif command -v shasum >/dev/null 2>&1; then
	actual="$(shasum -a 256 "${TMP}/${ASSET}" | awk '{ print $1 }')"
elif command -v openssl >/dev/null 2>&1; then
	actual="$(openssl dgst -sha256 "${TMP}/${ASSET}" | awk '{ print $NF }')"
else
	die "需要 sha256sum、shasum 或 openssl 之一来校验下载的文件"
fi
[ "$actual" = "$expected" ] || die "SHA-256 校验失败，文件可能不完整或被篡改"

tar -xzf "${TMP}/${ASSET}" -C "$TMP" sbpanel
chmod 755 "${TMP}/sbpanel"
"${TMP}/sbpanel" install "$@"
