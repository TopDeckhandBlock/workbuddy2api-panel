// Package headers Сконструировать три типа апстрим-заголовков (common / chat / billing / refresh）。
// правило из docs/api-reference.md §0/§4/§6。
package upstream

import (
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"strings"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/session"
)

const (
	// defaultClientVersion исходящий WorkBuddy сегмент версии клиента (UA `WorkBuddy/<ver>` и
	// группы заголовков белого списка X-IDE-Version）。выравнивание с официальным WorkBuddy Desktop Версия дистрибутива (5.5.4）。
	// config upstream.client_version Перезаписываемо (пусто = встроено по умолчанию).
	defaultClientVersion = "5.5.4"
	// defaultCliVersion исходящий UA Средний `CLI/<ver>` Версия сегмента. Выравнивание со встроенной официальной CLI（2.137.1）。
	// config upstream.cli_version Перезаписываемо (пусто = встроено по умолчанию).
	defaultCliVersion = "2.137.1"

	originRefererCN = "https://www.codebuddy.cn"
	originRefererGlobal = "https://www.workbuddy.ai"
)

// originRefererFor По аккаунту realm вернуть Origin/Referer Базовый домен:
// global → https://www.workbuddy.ai；cn（включая глобальный свитч выключен)→ https://www.codebuddy.cn。
func originRefererFor(a *auth.Auth) string {
	if a != nil && a.IsGlobal() {
		return originRefererGlobal
	}
	return originRefererCN
}

// clientVersion действующий WorkBuddy Версия клиента:Client.ClientVersion если не пусто — брать,
// иначе встроенный по умолчанию defaultClientVersion。
func (c *Client) clientVersion() string {
	if c != nil && c.ClientVersion != "" {
		return c.ClientVersion
	}
	return defaultClientVersion
}

// cliVersion действующий CLI Версия:Client.CliVersion если не пусто — брать его, иначе встроенный дефолт defaultCliVersion。
func (c *Client) cliVersion() string {
	if c != nil && c.CliVersion != "" {
		return c.CliVersion
	}
	return defaultCliVersion
}

// defaultWorkBuddyUAFor Сборка исходящего запроса клиента по умолчанию UA（Официальный десктоп-клиент RestOperations форма слоя):
// `WorkBuddy/<clientVersion> <platform>/<clientVersion> CLI/<cliVersion>`。
// Сегмент платформы (второй сегмент) бренд по realm Переключение —CN использовать applicationName то же значение `WorkBuddy`，
// global использовать официальную международную версию productName `WorkBuddy AI`（intl Доказательства реверс-инжиниринга проекта:
// `WorkBuddy/5.5.2 WorkBuddy AI/5.5.2 CLI/5.5.2`）。
// global Аккаунт отправлен не в тот сегмент платформы (`WorkBuddy` не `WorkBuddy AI`）Может триггерить апстрим 403 code 11140
// "request illegal« Риск-контроль. Официально никакого UA рандомизация, поэтому по умолчанию детерминированно.
func (c *Client) defaultWorkBuddyUAFor(a *auth.Auth) string {
	platform := "WorkBuddy"
	if a != nil && a.IsGlobal() {
		platform = "WorkBuddy AI"
	}
	return "WorkBuddy/" + c.clientVersion() + " " + platform + "/" + c.clientVersion() + " CLI/" + c.cliVersion()
}

// defaultWorkBuddyUA вернуть CN дефолт формы UA（Форма аккаунта по умолчанию — CN，Без регрессии, совместимо с существующими вызовами/тест).
func (c *Client) defaultWorkBuddyUA() string {
	return c.defaultWorkBuddyUAFor(nil)
}

// userAgent Вернуть текущий исходящий UA（исходящий путь клиента:chat/refresh/FetchModels）。
// Приоритет:Client.UserAgent（config user_agent）явное переопределение > По аккаунту realm по умолчанию для WorkBuddy Трехэтапная схема.
// Явное переопределение совместимо с текущей логикой: при наличии польз. значения — приоритет ему (кастомный бренд/версия),
// Если не настроено — дефолт официального десктопа (global замена `WorkBuddy AI` сегмент платформы).
func (c *Client) userAgent(a *auth.Auth) string {
	if c != nil && c.UserAgent != "" {
		return c.UserAgent
	}
	return c.defaultWorkBuddyUAFor(a)
}

// billingUA класс белого списка (billing/checkin/banner）исходящий UA：один сегмент `WorkBuddy/<clientVersion>`
// （Официальный banner Форма явного переопределения, без CLI сегмент). Включено по умолчанию (подделка официального desktop-отпечатка);
// Явно client_name=«SaaS« Не задавать UA（Восстановить старое поведение,Go По умолчанию UA）。
func (c *Client) billingUA() string {
	if c == nil || c.attributionClientName() == "SaaS" {
		return ""
	}
	return "WorkBuddy/" + c.clientVersion()
}

// resolveDeviceToken Парсинг текущего запроса X-Device-Token получение значения.
// Приоритет:auth.Auth.DeviceToken（на аккаунт)> Client.DeviceToken（config глобально)> Фолбэк на файл.
// все три пусты/При ошибке чтения вернуть пустую строку (вызывающая сторона не инжектит заголовок, graceful degradation).
func (c *Client) resolveDeviceToken(a *auth.Auth) string {
	if a != nil && a.DeviceToken != "" {
		return a.DeviceToken
	}
	if c != nil && c.DeviceToken != "" {
		return c.DeviceToken
	}
	if c != nil && c.DeviceTokenFile != "" {
		return readDeviceTokenFile(c.DeviceTokenFile)
	}
	return ""
}

// injectDeviceToken В req инжект X-Device-Token Заголовок (только если получено непустое token）。
func (c *Client) injectDeviceToken(req *http.Request, a *auth.Auth) {
	if tok := c.resolveDeviceToken(a); tok != "" {
		req.Header.Set("X-Device-Token", tok)
	}
}

// deriveAccountStableID Нажать uid + Стабильная деривация соли по назначению 36 hex Устройство/Идентификатор сессии.
// стабильность между рестартами (фиксированная соль "wb2a:"，Не меняется со сменой процесса — это с session Принципиальное различие соли, производной от пакета:
// это случайная соль уровня процесса для измерения ключа сессии, обновляется при перезапуске; данная функция — измерение аккаунта, должна быть постоянна между перезапусками),
// Различается между аккаунтами (uid разное — значит разное), одинаковое uid Одно назначение — одно значение (идемпотентность). Использовать sha256 с проектом
// Существующее производное (session/ids.go、cache_key.go）Сохранять консистентность; обрезка 36 hex Даёт больше энтропии.
//
// Два назначения:
// - purpose="machine" → X-Machine-ID（на уровне устройства, стабилен между сессиями)
// - purpose="session" → X-Session-ID（фиксированная сессия аккаунта, стабильна между перезапусками)
//
// и injectDeviceToken X-Device-Token Сосуществуют без конфликта: выдано апстримом при логине
// Реальный токен устройства (если есть — отправлять, приоритетный); данный заголовок — стабильный отпечаток "одно виртуальное устройство на аккаунт»,
// Защита от блокировки множества аккаунтов апстримом из-за отсутствия фингерпринта устройства/дрейф связан с риск-контролем. Это разные семейства заголовков, официальный десктоп шлёт оба.
func deriveAccountStableID(uid, purpose string) string {
	sum := sha256.Sum256([]byte("wb2a:" + purpose + ":" + uid))
	return hex.EncodeToString(sum[:18]) // 36 hex chars
}

// injectAccountStableHeaders В req инжект X-Machine-ID / X-Session-ID：Нажать uid Стабильно
// Производный, фиксирован между рестартами, различается между аккаунтами.uid При пустом не инжектировать (анонимный запрос без device ID, upstream не требует).
func (c *Client) injectAccountStableHeaders(req *http.Request, a *auth.Auth) {
	if a == nil || a.UID == "" {
		return
	}
	req.Header.Set("X-Machine-ID", deriveAccountStableID(a.UID, "machine"))
	req.Header.Set("X-Session-ID", deriveAccountStableID(a.UID, "session"))
}

// CommonHeaders установить все API Общие заголовки запроса.
func (c *Client) CommonHeaders(req *http.Request, a *auth.Auth) {
	req.Header.Set("Content-Type", "application/json")
	// Accept по умолчанию не потоковый application/json（D6：Убрать нестрогие text/plain, */*）。
	// chat Потоковый путь в ChatHeaders Перекрыть как event-stream。
	req.Header.Set("Accept", "application/json")
	req.Header.Set("X-Requested-With", "XMLHttpRequest")
	origin := originRefererFor(a)
	req.Header.Set("Origin", origin)
	req.Header.Set("Referer", origin+"/")
	req.Header.Set("User-Agent", c.userAgent(a))
	// X-CodeBuddy-Request: 1（Заголовок шлюза риск-контроля официального клиента, все API Запрос обязательно содержит,D1）。
	req.Header.Set("X-CodeBuddy-Request", "1")
	// Accept-Language Нажать realm Переключение (D5）：CN zh-CN，global en-US。Официальный клиент по домену аккаунта
	// Отправлять соответствующий языковой идентификатор, чтобы апстрим не сработал по антифроду из-за отсутствия языка.
	req.Header.Set("Accept-Language", acceptLanguageFor(a))
	// X-Machine-ID / X-Session-ID：Нажать uid Стабильно деривированный device-заголовок уровня аккаунта (см.
	// injectAccountStableHeaders）。инъекция в CommonHeaders——chat Через ChatHeaders
	// Наложение CommonHeaders Наследуется по умолчанию;billing Домен инжектируется отдельно, покрытие всего исходящего трафика.
	c.injectAccountStableHeaders(req, a)
}

// acceptLanguageFor По аккаунту realm вернуть Accept-Language：global → en-US，cn → zh-CN。
func acceptLanguageFor(a *auth.Auth) string {
	if a != nil && a.IsGlobal() {
		return "en-US"
	}
	return "zh-CN"
}

// injectCodeBuddyRequest В req инжект X-CodeBuddy-Request: 1。
// billing Домен не прошёл CommonHeaders，Отдельная инъекция для полного покрытия исходящего трафика (D1）。
func (c *Client) injectCodeBuddyRequest(req *http.Request) {
	req.Header.Set("X-CodeBuddy-Request", "1")
}

// ChatMeta Один раз chat исходящие метаданные семейства заголовков сессии (issue #35：Бэкенд по X-Conversation-Request-ID
// Агрегированный запрос, официальный клиент за один раз user send все внутри tool call/повтор/Смена аккаунта с переиспользованием того же ID）。
// handler генерировать вне цикла ротации conversationID / conversationRequestID，Каждый исходящий вызов в цикле
// Переиспользовать то же значение;TraceID прокинуть входящее значение (пусто = откат conversationRequestID）。
// messageID（на уровне сообщений, каждое независимо) — ChatHeaders Генерируется внутри, внешний показ не требуется.
type ChatMeta struct {
	ConversationID string // X-Conversation-ID：body Извлечённое входящее значение, если пусто — не отправлять (приоритет сквозной передачи, без подделки)
	ConversationRequestID string // X-Conversation-Request-ID / X-Root-Request-ID：Ключ агрегации, обязательная отправка
	TraceID string // X-Trace-ID：Прозрачное значение входящего запроса, при пустом — fallback conversationRequestID
}

// ChatHeaders В common Добавить поверх chat Выделенный заголовок аккаунта.
// поле по умолчанию: X-No-* Соглашение (с CodeBuddy Официальный CLI совпадает).
// clientIP клиент для данного запроса IP（Передается параметром, общие поля не читаются — во избежание гонок);
// PassthroughIP=false Или clientIP При пустом значении не инжектировать IP Заголовок.
// meta Метаданные семейства заголовков сессии (только добавление, без изменения существующих), см. injectConversationHeaders。
func (c *Client) ChatHeaders(req *http.Request, a *auth.Auth, clientIP string, meta ChatMeta) {
	c.CommonHeaders(req, a)
	// chat Потоковый Accept перекрытие CommonHeaders непотоковый дефолт для (D6）。
	req.Header.Set("Accept", "application/json, text/event-stream")
	// AccessToken Снимок под блокировкой:keepalive периодическое обновление будет в a.mu перезапись внутри, чтение вне лока — гонка данных
	// （См. auth.AccessTokenValue комментарий).
	if at := a.AccessTokenValue(); at != "" {
		req.Header.Set("Authorization", "Bearer "+at)
	} else {
		req.Header.Set("X-No-Authorization", "1")
	}
	if a.UID != "" {
		req.Header.Set("X-User-Id", a.UID)
	} else {
		req.Header.Set("X-No-User-Id", "1")
	}
	// Красная линия безопасности: никогда не в chat В запросе передается X-Refresh-Token。
	// заголовки enterprise и домена по realm Диспетчеризация:CN идти по существующей ветке (EnterpriseID/Domain Проброс как есть, по умолчанию X-No-*）；
	// global Аккаунт от injectGlobalChatHeaders Привести к виду международного клиента
	// （X-No-Enterprise-Id=1 заявлено: без enterprise + X-Domain=www.workbuddy.ai объявлен домен международной версии),
	// и без отката X-Domain к исходному значению сессии логина — выравнивание intl исходящий заголовок проекта.
	if a != nil && !a.IsGlobal() {
		if a.EnterpriseID != "" {
			req.Header.Set("X-Enterprise-Id", a.EnterpriseID)
		} else {
			req.Header.Set("X-No-Enterprise-Id", "1")
		}
		if d := a.DomainValue(); d != "" {
			req.Header.Set("X-Domain", d)
		} else {
			req.Header.Set("X-No-Department-Info", "1")
		}
	} else {
		c.injectGlobalChatHeaders(req, a)
	}
	// заголовок принадлежности расхода: по умолчанию подделка WorkBuddy фингерпринт десктопа (client_name="SaaS" восстановить старое поведение).
	c.injectAttribution(req)
	// Клиент IP Прозрачная передача (только PassthroughIP=true и текущий запрос содержит IP）。
	c.injectClientIP(req, clientIP)
	// Заголовок антифрода устройства:auth На каждый номер > config глобально > файловый фолбэк; если пусто — не инжектить.
	c.injectDeviceToken(req, a)
	// Семейство заголовков сессии (диалог/Запрос/Сообщение/B3 канал), см. injectConversationHeaders。
	c.injectConversationHeaders(req, meta)
}

// injectGlobalChatHeaders global Аккаунт (без enterprise ID） chat Выделенный заголовок declaration, выравнивание intl Проект
// （ANALYSIS-global-chat-solutions.md）：
// - X-No-Enterprise-Id: 1 Личный аккаунт без корпоративного ID，явное объявление (чтобы апстрим не применил по умолчанию/подозрительное решение)
// - X-Domain: www.workbuddy.ai Явно объявить домен международной версии (с Origin/Referer тот же домен)
//
// Только global realm инъекция;CN Аккаунт идет по существующему X-No-Department-Info и др. ветки, ноль регрессий.
func (c *Client) injectGlobalChatHeaders(req *http.Request, a *auth.Auth) {
	if a == nil || !a.IsGlobal() {
		return
	}
	req.Header.Set("X-No-Enterprise-Id", "1")
	req.Header.Set("X-Domain", "www.workbuddy.ai")
}

// injectConversationHeaders Инжектировать семейство заголовков сессии официального клиента (issue #35 агрегация в фоне).
// семейство заголовков — 4 уровня, каждый со своей функцией:
// - X-Conversation-ID：Уровень сессии, стабильно на несколько раундов (body conversationId）。если пусто — не отправлять —
// Приоритет прозрачной передаче исходного значения клиента, если клиент не передал — не подделывать, чтобы не ввести бэкенд в заблуждение и не создать неверную сессию.
// - X-Conversation-Request-ID：**Первичный ключ агрегации уровня раунда диалога**，Обязательная отправка. Один раз user send внутри
// Все tool call/повтор/Смена номера/При даунгрейде переиспользуется тот же → бэкенд агрегирует по нему в одну запись (без фрагментации).
// - X-Conversation-Message-ID = X-Request-ID：уровень сообщения, каждое независимо (32 бит hex）。
// - X-Root-Request-ID：= conversationRequestID（трассировка корневого запроса).
// - X-Trace-ID：Входящий passthrough или = conversationRequestID。
// - X-B3-TraceId / X-B3-SpanId / X-B3-Sampled：Семейство каналов.B3 спецификация признает только 16/32 hex
// TraceId и 16 hex SpanId；inbound conversationRequestID при невалидности TraceId откат
// messageID（Конст. 32 hex），SpanId получить messageID[:16]（обновляется на каждое сообщение).
func (c *Client) injectConversationHeaders(req *http.Request, meta ChatMeta) {
	convReqID := meta.ConversationRequestID
	if convReqID == "" {
		// нулевое значение meta（Прямой вызов ChatHeaders вызывающей стороны/тест) также гарантировать обязательную отправку ключа агрегации:
		// На этом уровне добавить один 32 hex，вызывающая сторона (handler）Уже стабильно сгенерировано, сюда не заходит.
		convReqID = session.NewMessageID()
	}
	messageID := session.NewMessageID()
	if meta.ConversationID != "" {
		req.Header.Set("X-Conversation-ID", meta.ConversationID)
	}
	req.Header.Set("X-Conversation-Request-ID", convReqID)
	req.Header.Set("X-Conversation-Message-ID", messageID)
	req.Header.Set("X-Request-ID", messageID)
	req.Header.Set("X-Root-Request-ID", convReqID)
	traceID := meta.TraceID
	if traceID == "" {
		traceID = convReqID
	}
	req.Header.Set("X-Trace-ID", traceID)
	b3Trace := convReqID
	if !validTraceID(b3Trace) {
		b3Trace = messageID // Недопустимый B3 TraceId → Откат константа 32 hex уровня сообщений ID
	}
	req.Header.Set("X-B3-TraceId", b3Trace)
	req.Header.Set("X-B3-SpanId", messageID[:16])
	req.Header.Set("X-B3-Sampled", "1")
}

// validTraceID Проверка B3 TraceId валидно ли:16 Или 32 бит hex（регистронезависимо).
// Сгенерировано официальным клиентом conversationRequestId Да 32 бит hex（UUID без дефисов), сквозное значение на входе
// может быть произвольной формы (включая дефис/Сверхдлинный/не hex），Прямо поместить в B3 Заголовок нарушит привязку цепочки (issue #35）。
func validTraceID(s string) bool {
	if len(s) != 16 && len(s) != 32 {
		return false
	}
	for i := 0; i < len(s); i++ {
		ch := s[i]
		if !((ch >= '0' && ch <= '9') || (ch >= 'a' && ch <= 'f') || (ch >= 'A' && ch <= 'F')) {
			return false
		}
	}
	return true
}

// attributionClientName активное имя владельца квоты:ClientName если не пусто — брать;
// пусто по умолчанию "WorkBuddy«（Подделка отпечатка официального десктоп-клиента; явно настроить "SaaS" можно восстановить старое поведение).
func (c *Client) attributionClientName() string {
	if c != nil && c.ClientName != "" {
		return c.ClientName
	}
	return "WorkBuddy"
}

// injectAttribution Инжект заголовка принадлежности расхода (X-Agent-Purpose / X-IDE-* / X-Product）。
// Только при chat/completions Путь активен (ChatHeaders вызов).
//
// по умолчанию (ClientName пусто) — выравнивание с официалом WorkBuddy Отпечаток десктопа:X-Agent-Purpose="conversation"
// + X-IDE-Name/Type/Product="WorkBuddy" + X-IDE-Version=client_version，атрибуция расхода апстрима
// Больше не появляется client/agentPurpose пустой "признак шлюза». Явно ClientName="SaaS" Восстановить старое поведение
// （Только X-Product="SaaS"，Не задано X-IDE-*）；При другом значении все четыре следуют ему.
func (c *Client) injectAttribution(req *http.Request) {
	name := c.attributionClientName()
	if name == "SaaS" {
		req.Header.Set("X-Product", "SaaS")
		return
	}
	req.Header.Set("X-Agent-Purpose", "conversation")
	req.Header.Set("X-IDE-Name", name)
	req.Header.Set("X-IDE-Type", name)
	req.Header.Set("X-IDE-Version", c.clientVersion())
	req.Header.Set("X-Product", name)
}

// injectClientIP В PassthroughIP При включении clientIP Параметры проксируются в upstream (три эквивалентных заголовка).
func (c *Client) injectClientIP(req *http.Request, clientIP string) {
	if c == nil || !c.PassthroughIP || clientIP == "" {
		return
	}
	req.Header.Set("X-Forwarded-For", clientIP)
	req.Header.Set("X-Real-IP", clientIP)
	req.Header.Set("X-Client-IP", clientIP)
}

// ExtractClientIP извлечь клиента из входящего запроса IP первый сегмент (X-Forwarded-For Первый сегмент, фолбэк X-Real-IP）。
func ExtractClientIP(r *http.Request) string {
	if r == nil {
		return ""
	}
	if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
		for i := 0; i < len(xff); i++ {
			if xff[i] == ',' {
				return strings.TrimSpace(xff[:i])
			}
		}
		return strings.TrimSpace(xff)
	}
	if real := strings.TrimSpace(r.Header.Get("X-Real-IP")); real != "" {
		return real
	}
	return ""
}

// BillingHeaders billing Заголовок запроса интерфейса.
// UA Семантика: по умолчанию**Не задавать**（Оставить как есть,Go Дефолт на стороне клиента UA）；только при явной конфигурации
// c.UserAgent перезапись только если не пусто — чтобы не затереть путь по умолчанию billing ввести новый UA отпечаток.
func (c *Client) BillingHeaders(req *http.Request, a *auth.Auth) {
	// AccessToken снапшот под локом (тот же ChatHeaders：keepalive Можно в a.mu перезапись внутри).
	req.Header.Set("Authorization", "Bearer "+a.AccessTokenValue())
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Content-Type", "application/json")
	c.injectCodeBuddyRequest(req)
	// Accept-Language Нажать realm Переключение (D5，billing Домен не прошёл CommonHeaders，отдельная инъекция).
	req.Header.Set("Accept-Language", acceptLanguageFor(a))
	if c != nil && c.UserAgent != "" {
		req.Header.Set("User-Agent", c.UserAgent)
	} else if ua := c.billingUA(); ua != "" {
		req.Header.Set("User-Agent", ua)
	}
	if a.UID != "" {
		req.Header.Set("X-User-Id", a.UID)
	}
	if a.EnterpriseID != "" {
		req.Header.Set("X-Enterprise-Id", a.EnterpriseID)
		req.Header.Set("X-Tenant-Id", a.EnterpriseID)
	}
	if d := a.DomainValue(); d != "" {
		req.Header.Set("X-Domain", d)
	}
	// Заголовок антифрода устройства:billing Домен (report/travel/balance/checkin）Аналогично инжектируется.
	c.injectDeviceToken(req, a)
}

// RefreshHeaders refresh специфичный для эндпоинта заголовок (X-Refresh-Token Допускается только здесь).
func (c *Client) RefreshHeaders(req *http.Request, a *auth.Auth) {
	c.CommonHeaders(req, a)
	req.Header.Set("X-Refresh-Token", a.RefreshToken)
	if a.EnterpriseID != "" {
		req.Header.Set("X-Enterprise-Id", a.EnterpriseID)
	}
	// X-Auth-Refresh-Source Выравнивание с официальным клиентом refresh идентификатор канала "plugin"（D3）。
	req.Header.Set("X-Auth-Refresh-Source", "plugin")
}
