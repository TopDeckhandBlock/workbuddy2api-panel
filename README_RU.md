# workbuddy2api-panel

**Self-hosted OpenAI-совместимый шлюз для Tencent CodeBuddy** (`copilot.tencent.com`) с веб-панелью управления, пулом аккаунтов, авто-выполнением заданий «роста» (+1950 кредитов на аккаунт) и полным комплектом защиты от блокировок (cooldown / circuit breaker / sticky-сессии).

> 🇷🇺 Русская версия README. Оригинал (fork базы): [linguo2625469/workbuddy2api-panel](https://github.com/linguo2625469/workbuddy2api-panel) / апстрим [Sliverkiss/workbuddy2api](https://github.com/Sliverkiss/workbuddy2api) (удалён автором).

## Что это

CodeBuddy — IDE-ассистент от Tencent со щедрыми бесплатными лимитами. Официального OpenAI-подобного API у него нет. Этот проект оборачивает аккаунты CodeBuddy в **обычный `/v1/chat/completions`**:

- **OAuth device-flow** для входа (панель → «Добавить аккаунт», credentials в `auths/`)
- Мульти-аккаунтный пул: маршрутизация по «минимальному остатку жизни токена» + cost-tier + взвешенный рандом, топ-5 кандидатов, anti-stampede
- **Автоматические задания** — 17 из 18 «growth tasks» выполняются чистым API (+~1950 кредитов +78 энергии на свежий аккаунт), включая sign-in, streak-redeem, lottery-вытягивание, «путешествие котика» (buddy travel)
- **Sticky-сессии** (`conversation_id`), опционально зеркалируются в Upstash Redis (TTL 7 дней) — рестарты не теряют привязки
- **Защита от рейтов**: 429 → soft-cooldown 600s с экспонентой (cap 2h), 404 → 60s, 402 → до 04:00 следующего дня, circuit breaker при серии фейлов, lease-limiter на in-flight
- **Поток и нормализация SSE**: исходящий всегда `stream:true`; non-stream = локальная агрегация; DeepSeek CoT-инъекция (`thinking.type=enabled`), `reasoning_content` backfill, effort-даунгрейды
- **Фингерпринт-санитайзер**: очистка blacklisted полей исходящего JSON (отключаемо)
- **Встроенная веб-панель**: акки/модели/конфиг/логи/задания, темы dark/light, hot-reload конфига, CSP + security headers, constant-time auth
- **Пробы реальных max_tokens**: `scripts/probe_max_tokens.py` эмпирически замеряет фактический потолок вывода каждого модели (заявленные обычно завышены; один из CN-моделей "1M" фактически отдаёт 32K)

## Быстрый старт (Windows, без Docker)

```powershell
# 1) Скачать Release-бинарь или собрать:
go build -trimpath -ldflags="-s -w" -o wb2api.exe ./cmd/server

# 2) Запустить (при старте нет config.json — сгенерится с random api_key, ключ печатается в логе ОДИН раз):
.\wb2api.exe -config config.json

# 3) Панель:
http://127.0.0.1:7863/panel/
# Кнопка «Добавить аккаунт» → OAuth → готово
```

### Docker

```bash
mkdir -p auths data && cp config.example.json config.json
docker run -d --name workbuddy2api \
  -p 7863:7863 -e TZ=Asia/Shanghai \
  -v ./auths:/app/auths -v ./data:/app/data -v ./config.json:/app/config.json \
  ghcr.io/linguo2625469/workbuddy2api-panel:latest

# healthcheck (без аккаунтов → 503):
curl -s http://localhost:7863/healthz
```

или `docker compose up -d --build` из корня репы.

## Модели (примеры)

`/v1/models` возвращает динамический список (кэш 1h) — deepseek-v4 / glm-5.2 / hunyuan / и др. с полями `context_length`, `max_output_tokens`, `reasoning_supported_efforts`, реальными коэффициентами стоимости (credit multiplier).

## API

| Endpoint | Auth | Назначение |
|---|---|---|
| `POST /v1/chat/completions` | Bearer | OpenAI-совместимый; stream / non-stream; request cap 8 MiB |
| `GET /v1/models` | Bearer | модельный список (dynamic) |
| `GET /status` | Bearer | статус пула + деталь по каждому акку |
| `GET /healthz` | — | health-check (200/503, X-Service header) |

```bash
curl -sN http://localhost:7863/v1/chat/completions \
  -H "Authorization: Bearer $KEY" -H "Content-Type: application/json" \
  -d '{"model":"deepseek-v4-flash","messages":[{"role":"user","content":"hi"}],"stream":true}'
```

## Web-панель (`/panel/`)

- **Аккаунт-пул**: стат-полоса (total/available/cooling/disabled/credits/sticky), per-account ops (check-in / balance / tasks / unfreeze / disable / remove), batch-кнопки
- **Добавить аккаунт**: OAuth device-flow целиком в браузере, hot-reload в пул без рестарта
- **Задания**: список с прогрессом, «Принять все», «One-click» на 17 задач, авто-клейм наград
- **Модели & tiers**: кредит-множители, effort-уровни, контекст/вывод лимиты + экспериментальные probe-данные
- **Usage**: метрики (requests/tokens/prompt/completion/fails/latency), time-series, breakdowns по акку/модели/домену, история списания кредитов
- **Конфиг**: online-редактор config.json с hot-apply (api_key, cooldowns, pool, schedule)
- **Логи**: request-log (time/result/model/account/IP/UA/tokens/credits), server-лог 500 строк по каналам (tasks/chat/system)

## Конфиг

Полный образец — `config.example.json` (все поля с дефолтами, только placeholder-значения, без секретов). Ключевые:

- `listen` (default `:7863`), `api_key` (пусто = без auth; иначе Bearer)
- `auth_dir` (`./auths`) — plaintext accessToken/refreshToken per-акк, файлы `workbuddy-<uid>.json`, 0600
- `pool.*` — fusion-параметры пула
- `schedule.*` — тайминги заданий: sign-in 09/21, activity 10, token-keepalive 22, cat-travel 09/21; balance-refresh каждые 5 мин
- `upstream.user_agent` — переопределение UA (default `CLI/2.63.2 CodeBuddy/2.63.2`)
- `prompt.mode` — `custom` (gateway-промпт заменяет client system) / `passthrough` (auto-degrade при интерсепте)
- `features.sanitize_blacklist_fingerprints` — on/off фингерпринт-очистку
- `upstash.*` — опциональное Redis-зеркало sticky-сессий

## Безопасность

- **Плaintext-токены в `auths/`** — держать вне git (`.gitignore` покрывает), chmod 600
- Service без TLS — за reverse-proxy (Nginx/Caddy) при публичном доступе
- Constant-time Bearer-сравнение (SHA-256 digest + `subtle.ConstantTimeCompare`), CSP `default-src 'none'`, `X-Frame-Options: DENY`, uid-whitelist против path-traversal
- Log-файлы не пишутся — только stdout; JSONL request-archive содержит только метаданные (модель/uid/IP/UA/status), без промптов/токенов

## Автоматизация growth-заданий (что делает one-click)

| Категория | Примеры задач | Награда/акк |
|---|---|---|
| Login/streak | `first_buddy` (adopt cat), sign-in, streak-redeem 7/14/28, lottery-draw | ~600c + energy + draw-tickets |
| Chat-events | `chat_5`, `Model_chat_GLM5.2`, `RichMeow_Chat` | ~300c |
| Desktop-event chains | `create_canvas`, `Buddy_App`, `template_5`, `playbook_prompt` | ~400c |
| Expert chains | `expert_5`, `Expert_team_use_3`, `skill_1`, `Expert_lighthouse` (free lightweight server month!) | ~400c |
| прочее | `Hp_Appearance` (theme), `Library_read`, `automation_1` | ~250c |

Все события эмулируются с корректным client-фингерпринтом (CLI `www.codebuddy.cn` / desktop `copilot.tencent.com` + WorkBuddy UA / web `www.workbuddy.cn` + `x-client-platform: web`). Claim — только через web-домен с mp-заголовками; has registration re-verify with auto-retry.

Не автоматизируется (1 из 18): `Expert_Philanthropy` — требует реального доната (сервер проверяет receipt).

## Статус тестов

```bash
go test ./...
# ok internal/pool, internal/server, internal/upstream, internal/panel, internal/httpauth, internal/scheduler
```

## Авторство и лицензия

MIT. Базируется на [linguo2625469/workbuddy2api-panel](https://github.com/linguo2625469/workbuddy2api-panel) (функциональная надстройка) и [Sliverkiss/workbuddy2api](https://github.com/Sliverkiss/workbuddy2api) (апстрим, удалён). Оригинальный README с подробным reverse-engineering разбором API CodeBuddy — [README.md](README.md) (Chinese).

> Проект для управления **своими** аккаунтами CodeBuddy. Использование в нарушение ToS (батч-регистрация, resale, paywalled redistribution) — не поддерживается и может привести к бану аккаунтов. Подробнее — в оригинальном README (раздел «Использование»).
