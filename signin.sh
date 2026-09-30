#!/bin/bash
# Скрипт массовой отметки: обход auths/ все под workbuddy-*.json Аккаунт
# Использование: ./signin.sh [auths_dir]
set -e
cd "$(dirname "$0")"

BIN=./signin_bin
if [ ! -x "$BIN" ]; then
 echo "build signin_bin ..."
 go build -o "$BIN" ./cmd/signin
fi

exec "$BIN" "${1:-auths}"
