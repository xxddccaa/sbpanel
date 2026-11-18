#!/usr/bin/env bash
# Print the release notes for a tag: its CHANGELOG.md section plus install instructions.
set -euo pipefail

tag="${1:?usage: release-notes.sh vX.Y.Z}"
notes="$(awk -v tag="$tag" '
	/^## / { if (found) exit; if (index($0, "[" tag "]")) { found = 1; next } }
	found { print }
' CHANGELOG.md)"

if [ -n "$notes" ]; then
	printf '%s\n\n' "$notes"
fi

cat <<'TEXT'
## 安装 / 更新

```bash
bash <(curl -fsSL https://raw.githubusercontent.com/xxddccaa/sbpanel/main/install.sh)
```

已经安装过的，也可以运行 `sbpanel update`。下载的文件都可以用 `sha256sums.txt` 校验。
TEXT
