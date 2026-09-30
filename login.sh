#!/usr/bin/env bash
# login.sh — WorkBuddy CN OAuth логин → сброс на диск auth Файл
#
# Использование:
# ./login.sh
#
# Процесс:
# 1. POST /v2/plugin/auth/state Получить авторизацию URL（отсутствует PKCE，state выдается сервером)
# 2. Вы открываете в браузере URL Вход выполнен
# 3. вернуться сюда по y → poll Взять token+uid+nickname → check-in → сброс на диск auths/workbuddy-<uid>.json
# 4. Перезапуск workbuddy2api Контейнер загружает новый аккаунт
set -euo pipefail

cd "$(dirname "$0")"
AUTH_DIR="./auths"
CONTAINER="workbuddy2api"

mkdir -p "$AUTH_DIR"

# login Инструмент: компилировать только если отсутствует (после изменения исходников вручную go build -o login ./cmd/login）
LOGIN_BIN="./login"
if [[ ! -x "$LOGIN_BIN" ]]; then
 go build -o "$LOGIN_BIN" ./cmd/login
fi

echo "============================================================"
echo " WorkBuddy OAuth логин"
echo "============================================================"
echo ""

AUTH_URL=$("$LOGIN_BIN" url)

echo "Откройте следующую ссылку в браузере для завершения входа:"
echo ""
echo " $AUTH_URL"
echo ""

if command -v xclip &>/dev/null; then
 echo -n "$AUTH_URL" | xclip -selection clipboard 2>/dev/null && echo "(Скопировано в буфер обмена)"
elif command -v xsel &>/dev/null; then
 echo -n "$AUTH_URL" | xsel --clipboard 2>/dev/null && echo "(Скопировано в буфер обмена)"
fi

echo ""
read -rp "После завершения входа по y продолжить: " ans
if [[ "$ans" != "y" && "$ans" != "Y" ]]; then
 echo "отменено"
 exit 1
fi

echo ""
echo "Получение... token..."

RESULT=$("$LOGIN_BIN" poll) || {
 echo ""
 echo "Получить token Ошибка. Возможные причины:"
 echo " - Нажато до завершения логина y（Перезапустить ./login.sh повторить)"
 echo " - ошибка страницы логина (пришлите скриншот ошибки для диагностики)"
 exit 1
}

TOKEN=$(echo "$RESULT" | python3 -c "import json,sys; print(json.load(sys.stdin)['access_token'])")
REFRESH=$(echo "$RESULT" | python3 -c "import json,sys; print(json.load(sys.stdin)['refresh_token'])")
EXPIRES_IN=$(echo "$RESULT" | python3 -c "import json,sys; print(json.load(sys.stdin)['expires_in'])")
DOMAIN=$(echo "$RESULT" | python3 -c "import json,sys; print(json.load(sys.stdin).get('domain',''))")
USER_ID=$(echo "$RESULT" | python3 -c "import json,sys; print(json.load(sys.stdin).get('uid',''))")
ENT_ID=$(echo "$RESULT" | python3 -c "import json,sys; print(json.load(sys.stdin).get('enterprise_id',''))")
NICKNAME=$(echo "$RESULT" | python3 -c "import json,sys; print(json.load(sys.stdin).get('nickname',''))")

if [[ -z "$USER_ID" ]]; then
 echo "Не удалось получить uid，Проверьте token действителен ли"
 exit 1
fi

EXPIRES_AT=$(( $(date +%s) + EXPIRES_IN ))

# ─── Чек-ин (CN：POST codebuddy.cn/v2/billing/meter/daily-checkin，идемпотентно, без блокировки)───
# OAuth Все возвращаемые поля передаются через переменные окружения Python，и в кавычках heredoc：
# никнейм/domain/token значение может содержать кавычки/переносы, при конкатенации в Python исходник вызовет инъекцию.
WB2A_LOGIN_TOKEN="$TOKEN" \
WB2A_LOGIN_USER_ID="$USER_ID" \
WB2A_LOGIN_ENT_ID="$ENT_ID" \
WB2A_LOGIN_DOMAIN="$DOMAIN" \
python3 - <<'PYEOF'
import json, os, urllib.request, urllib.error

token = os.environ["WB2A_LOGIN_TOKEN"]
user_id = os.environ["WB2A_LOGIN_USER_ID"]
ent_id = os.environ["WB2A_LOGIN_ENT_ID"]
domain = os.environ["WB2A_LOGIN_DOMAIN"]

req = urllib.request.Request(
 "https://www.codebuddy.cn/v2/billing/meter/daily-checkin",
 method="POST", data=b"{}",
 headers={
 "Authorization": "Bearer " + token,
 "Accept": "application/json",
 "Content-Type": "application/json",
 "X-User-Id": user_id,
 **({"X-Enterprise-Id": ent_id, "X-Tenant-Id": ent_id} if ent_id else {}),
 **({"X-Domain": domain} if domain else {}),
 })
try:
 with urllib.request.urlopen(req, timeout=15) as r:
 body = json.loads(r.read().decode() or "{}")
 if body.get("code") == 0:
 data = body.get("data") or {}
 print(f"check-in: Успех {json.dumps(data, ensure_ascii=False)[:150]}")
 else:
 print(f"check-in: {body.get('msg', json.dumps(body)[:150])}")
except urllib.error.HTTPError as e:
 # Бизнес-ошибки вроде "уже отмечено" также идут через 4xx（На практике code=10001 "Сегодня уже отмечено"）
 try:
 body = json.loads(e.read().decode() or "{}")
 print(f"check-in: {body.get('msg', 'http %d' % e.code)}")
 except Exception:
 print(f"check-in: http {e.code}")
except Exception as e:
 print(f"check-in: {e}")
PYEOF

# ─── сброс на диск auth файл (с internal/auth формат чтения совпадает)─────────────────
AUTH_FILE="$AUTH_DIR/workbuddy-${USER_ID}.json"
if [[ -f "$AUTH_FILE" ]]; then
 echo "Аккаунт уже существует (uid=${USER_ID}），Перезапишет учетные данные"
 ACTION="перекрытие"
else
 echo "Новый аккаунт (uid=${USER_ID}），добавлено auth Файл"
 ACTION="добавлено"
fi
WB2A_LOGIN_TOKEN="$TOKEN" \
WB2A_LOGIN_REFRESH="$REFRESH" \
WB2A_LOGIN_EXPIRES_AT="$EXPIRES_AT" \
WB2A_LOGIN_DOMAIN="$DOMAIN" \
WB2A_LOGIN_USER_ID="$USER_ID" \
WB2A_LOGIN_ENT_ID="$ENT_ID" \
WB2A_LOGIN_NICKNAME="$NICKNAME" \
WB2A_LOGIN_AUTH_FILE="$AUTH_FILE" \
WB2A_LOGIN_ACTION="$ACTION" \
python3 - <<'PYEOF'
import json, os

auth = {
 "account": {
 "uid": os.environ["WB2A_LOGIN_USER_ID"],
 "enterpriseId": os.environ["WB2A_LOGIN_ENT_ID"],
 "nickname": os.environ["WB2A_LOGIN_NICKNAME"],
 },
 "auth": {
 "accessToken": os.environ["WB2A_LOGIN_TOKEN"],
 "refreshToken": os.environ["WB2A_LOGIN_REFRESH"],
 "expiresAt": int(os.environ["WB2A_LOGIN_EXPIRES_AT"]),
 "domain": os.environ["WB2A_LOGIN_DOMAIN"],
 },
}
with open(os.environ["WB2A_LOGIN_AUTH_FILE"], "w") as f:
 json.dump(auth, f, indent=1)
print(f"Сохранено ({os.environ['WB2A_LOGIN_ACTION']}）: {os.environ['WB2A_LOGIN_AUTH_FILE']}")
PYEOF

# ─── перезапустить сервис ────────────────────────────────────────────
echo ""
if docker ps --format '{{.Names}}' | grep -q "^${CONTAINER}$"; then
 echo "Перезапуск $CONTAINER Загрузка нового аккаунта..."
 docker restart "$CONTAINER" >/dev/null
 sleep 2
 # API_KEY Из config.json Чтение (переменная не определена в скрипте,fallback только заполнитель, аутентификацию не пройдет)
 API_KEY=$(python3 -c "import json; print(json.load(open('config.json')).get('api_key',''))" 2>/dev/null)
 COUNT=$(curl -s http://127.0.0.1:7863/status -H "Authorization: Bearer ${API_KEY:-test_key}" 2>/dev/null | python3 -c "import json,sys; print(len(json.load(sys.stdin).get('accounts',[])))" 2>/dev/null || echo "?")
 echo "Сервис перезапущен, текущее кол-во аккаунтов: $COUNT"
else
 echo "Контейнер $CONTAINER Не запущено,auth Файл сохранён, автозагрузка при следующем запуске"
fi

echo ""
echo "============================================================"
echo " Вход выполнен!"
echo " UID: $USER_ID"
echo " Nickname: ${NICKNAME:-（не получено)}"
echo " Token: ${TOKEN:0:30}..."
echo " срок действия: $(date -d "@$EXPIRES_AT" '+%Y-%m-%d %H:%M' 2>/dev/null || echo "$EXPIRES_AT")"
echo "============================================================"
