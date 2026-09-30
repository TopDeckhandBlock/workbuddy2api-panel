#!/usr/bin/env bash
# credit.sh — WorkBuddy ежедневный отчёт по баллам (по умолчанию — форматированный вывод)
#
# Использование:
# ./credit.sh # человекочитаемый daily-отчёт
# ./credit.sh -json # Исходный JSON
#
# Бинарное обновление: go build -o credit ./cmd/credit
set -euo pipefail
cd "$(dirname "$0")"
if [[ "${1:-}" == "-json" ]]; then
 exec ./credit
fi
exec ./credit -pretty
