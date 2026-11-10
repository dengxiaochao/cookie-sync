#!/usr/bin/env bash
set -euo pipefail

repo_dir="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
binary="$repo_dir/cookie-sync"

# 使用已编译的程序，避免重新构建导致监听 443 的文件能力丢失。
if [[ ! -x "$binary" ]]; then
    printf '未找到可执行程序，请先在 %s 运行 go build -o cookie-sync .\n' "$repo_dir" >&2
    exit 1
fi

# 默认使用项目内证书；追加参数可覆盖默认值，Ctrl+C 直接结束服务。
exec "$binary" \
    -tls-cert "$repo_dir/certs/dashboard.crt" \
    -tls-key "$repo_dir/certs/dashboard.key" \
    "$@"
