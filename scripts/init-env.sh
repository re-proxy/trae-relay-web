#!/bin/sh
# Run from the project root. Never replace an existing encryption key.
set -eu
if [ -e .env ] || [ -L .env ]; then
    printf '%s\n' '.env 已存在，未覆盖。升级请保留原管理密码和加密主密钥。' >&2
    exit 1
fi
if ! command -v openssl >/dev/null 2>&1; then
    printf '%s\n' '未找到 openssl。请参考 README 的手动配置方式；脚本不会安装软件。' >&2
    exit 1
fi
admin_password=$(openssl rand -base64 32)
master_key=$(openssl rand -base64 32)
umask 077
set -C
printf 'RELAY_ADMIN_PASSWORD=%s\nRELAY_MASTER_KEY=%s\n' "$admin_password" "$master_key" > .env
printf '%s\n' '已生成 .env。请打开该文件查看管理密码（RELAY_ADMIN_PASSWORD），并妥善备份。' '下一步：docker compose up -d --build'
