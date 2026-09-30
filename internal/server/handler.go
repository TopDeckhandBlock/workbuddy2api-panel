// Package server экспонировать OpenAI совместимость HTTP интерфейс, внутренний драйвер pool выбор номера + upstream пересылка.
package server

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"strings"

	"sync"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/httpauth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/livecfg"
	"github.com/linguo2625469/workbuddy2api-panel/internal/logfmt"
	"github.com/linguo2625469/workbuddy2api-panel/internal/pool"
	"github.com/linguo2625469/workbuddy2api-panel/internal/prompt"
	"github.com/linguo2625469/workbuddy2api-panel/internal/reqlog"
	"github.com/linguo2625469/workbuddy2api-panel/internal/session"
	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
	"github.com/linguo2625469/workbuddy2api-panel/internal/usage"
)

// Config handler зависимость.
type Config struct {
	Pool *pool.Pool
	Upstream *upstream.Client
	APIKey string // пустой = без аутентификации (статическое значение; с Live При одновременном указании Live приоритет)
	MaxRotate int // Макс. число смен номера на запрос, по умолчанию 3
	// Session Маршрутизатор sticky-сессий (опционально;nil = Отключить sticky, чисто Pick ротация).
	Session *session.Router
	// StickyCount Вернуть текущее кол-во привязок sticky-сессий (для /status）；nil отчет по времени 0。
	StickyCount func() int
	// RedisMode Поле наблюдения ("upstash« / "noop"），Подача /status Проброс.
	RedisMode string
	SoftCooldown time.Duration // 429/База мягкого кулдауна для текста лимита, по умолчанию 600s（ПриНепрерывный срабатывании — экспоненциальный бэкофф, с потолком soft_rate_max）
	RefreshSkew time.Duration // token Досрочное обновление окна, по умолчанию 10m

	// Panel Панель управления handler（Опционально;nil = не монтировать). Смонтировано в /panel/ Под префиксом,
	// встроено в панель Bearer Аутентификация (тот же api_key）И встроенные статические ресурсы, главный роут только форвардит.
	Panel http.Handler

	// Live изменяемая в рантайме конфигурация (изменение онлайн через панель api_key / soft_rate / вступает в силу сразу при переключении десенситизации).
	// nil откат к статическим полям (сценарии теста и bare usage).
	Live *livecfg.Holder

	// PromptMode "custom«（Шлюз заменяет собственным промптом system）/ "passthrough"（сквозная передача).
	PromptMode string
	// PromptText custom Текст системного промпта, инжектируемый в режиме (из config.PromptText）。
	PromptText string

	// GlobalEnabled global realm Переключатель маршрутизации (config global.enabled，по умолчанию true）。
	// handler третий шлюз на стороне (с main инжект auth переключатель,upstream.GlobalEnabled соответствие):
	// false（явный escape-hatch) даже если auth realm=global также не предоставляет global: Имя модели
	// （modelList Не перечислять global список).
	GlobalEnabled bool

	// Usage Рекордер расхода на запрос (опционально;nil = не логировать).
	// В recordAttempt Вызов через эту единственную точку агрегации, поэтому потоковый/непотоковый, успешный/Любой сбой будет учтен,
	// и с pool накопители на аккаунт из одного источника, расхождения метрик нет.
	Usage *usage.Recorder

	// RequestLog метрики запросов и десенсибилизация JSONL архивация (опционально;nil = не логировать).
	RequestLog *reqlog.Recorder

	// RecordClientInfo Логировать ли источник вызова в логе запросов (клиент IP / User-Agent）。
	// из logging.request_client_info（по умолчанию true）；При закрытии reqlog поле источника события
	// остаётся пустым, в архиве и на панели источник не отображается.
	RecordClientInfo bool
}

// loadLive Вернуть снапшот текущего рантайма;Live для nil синтез статическими полями.
func (h *Handler) loadLive() livecfg.Snapshot {
	if h.cfg.Live != nil {
		return h.cfg.Live.Load()
	}
	return livecfg.Snapshot{
		APIKey: h.cfg.APIKey,
		SoftCooldown: h.cfg.SoftCooldown,
		RecordClientInfo: h.cfg.RecordClientInfo,
	}
}

// softCooldown Вернуть текущий базовый интервал мягкого кулдауна (приоритет hot-fix,<=0 Откат к дефолту).
func (h *Handler) softCooldown() time.Duration {
	if d := h.loadLive().SoftCooldown; d > 0 {
		return d
	}
	if h.cfg.SoftCooldown > 0 {
		return h.cfg.SoftCooldown
	}
	return 600 * time.Second
}

// notFoundCooldown апстрим 404 фиксированная короткая длительность кулдауна.
// и SoftCooldown Причина разделения трафика:404 это upstream**спорадически**Путь отсутствует, это не"данный аккаунт в rate-limit"，
// При совместном использовании soft_rate（600s Запуск + экспоненциальное повышение), единичный сбой 404 заштрафует хорошие аккаунты 10 минут с последовательным удвоением.
// поэтому фиксировано 60s достаточно защиты от лавины, не зависит от soft_rate конфиг, и не участвует в индексе мягкого бэкоффа.
const notFoundCooldown = 60 * time.Second

// ServiceName Идентификатор шлюза. Через /healthz тело ответа service Поле и X-Service Заголовок также пробрасывается:
// Хост (напр. workbuddy-switch управляемый шлюз-субпроцесс) пробует старый сервис на том же порту/При других сервисах даже если другая сторона
// вернуть 2xx также без этого маркера, хост определяет по этому"Ложный успех"。
const ServiceName = "workbuddy2api"

// Handler главный маршрут.
type Handler struct {
	cfg Config
	mux *http.ServeMux
	degrade degradeGate
	// wafIP WAF IP уровневый стейт-машина блокировки (fail-fast，wafip.go）：Короткое окно, много номеров WAF 403 →
	// ротация в период активации при WAF 403 Немедленное завершение (без увеличения объема запросов). Состояние внутри процесса, сбрасывается при перезапуске.
	wafIP wafIPGate
}

// NewHandler Сборка handler。
func NewHandler(cfg Config) *Handler {
	if cfg.MaxRotate <= 0 {
		cfg.MaxRotate = 3
	}
	if cfg.SoftCooldown <= 0 {
		cfg.SoftCooldown = 600 * time.Second // база мягкого лимита (при непрерывном срабатывании экспоненциальный бэкофф)
	}
	if cfg.RefreshSkew <= 0 {
		cfg.RefreshSkew = 10 * time.Minute
	}
	if cfg.PromptMode == "" {
		cfg.PromptMode = "custom" // по умолчанию custom：Собственный промпт шлюза
	}
	h := &Handler{cfg: cfg, mux: http.NewServeMux()}
	h.mux.HandleFunc("POST /v1/chat/completions", h.withAuth(h.chatCompletions))
	h.mux.HandleFunc("GET /v1/models", h.withAuth(h.models))
	h.mux.HandleFunc("GET /status", h.withAuth(h.status))
	h.mux.HandleFunc("GET /healthz", h.healthz)
	if cfg.Panel != nil {
		h.mux.Handle("/panel/", cfg.Panel) // /panel → /panel/ От ServeMux Авто-редирект
	}
	return h
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if h.cfg.RequestLog != nil && r.Method == http.MethodPost && r.URL.Path == "/v1/chat/completions" {
		trace := &requestTrace{id: reqlog.NewRequestID(), start: time.Now()}
		if h.loadLive().RecordClientInfo {
			trace.captureClientInfo(r)
		}
		r = r.WithContext(context.WithValue(r.Context(), requestTraceKey{}, trace))
		obs := &responseObserver{ResponseWriter: w}
		w.Header().Set("X-Request-Id", trace.id)
		h.cfg.RequestLog.Begin()
		defer func() {
			status := obs.status
			if status == 0 {
				status = http.StatusOK
			}
			h.cfg.RequestLog.Record(trace.event(status))
		}()
		h.mux.ServeHTTP(obs, r)
		return
	}
	h.mux.ServeHTTP(w, r)
}

func (h *Handler) withAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !httpauth.VerifyBearer(r, h.loadLive().APIKey) {
			writeOpenAIError(w, http.StatusUnauthorized, "invalid_api_key", "missing or invalid API key")
			return
		}
		next(w, r)
	}
}

func (h *Handler) healthz(w http.ResponseWriter, r *http.Request) {
	total, healthy, _, _, _ := h.cfg.Pool.CountsDetailed()
	// использовать ServableNow Критерий:healthy>0 но при полном заполнении in-flight chat Будет 503，Health-check должен быть по той же методике,
	// Иначе балансировщик продолжит слать трафик на неспособные обслуживать инстансы.
	status := http.StatusOK
	if !h.cfg.Pool.ServableNow() {
		status = http.StatusServiceUnavailable
	}
	// realm_servable Измерение обслуживаемости домена: не менять семантику health-check (проверка существования без изменений),
	// Только добавление CN/global Доступность каждого домена для наблюдения за двухдоменным развёртыванием (отдельный алерт при недоступности любого домена).
	realmServable := map[string]bool{
		"cn": h.cfg.Pool.ServableForRealm("cn"),
		"global": h.cfg.Pool.ServableForRealm("global"),
	}
	// Всегда без аутентификации (балансировщик/для health-check оркестрации достаточно 2xx/503 семантика), идентификация по service Поле + X-Service Двойная страховка заголовком.
	w.Header().Set("X-Service", ServiceName)
	writeJSON(w, status, map[string]any{
		"healthy": healthy,
		"total": total,
		"service": ServiceName,
		"realm_servable": realmServable,
	})
}

func (h *Handler) status(w http.ResponseWriter, r *http.Request) {
	total, healthy, cooling, disabled, inFlightFull := h.cfg.Pool.CountsDetailed()
	sticky := 0
	if h.cfg.StickyCount != nil {
		sticky = h.cfg.StickyCount()
	}
	redisMode := h.cfg.RedisMode
	if redisMode == "" {
		redisMode = "noop"
	}
	// cost_explore Журнал исследования (issue #136 §5 наблюдаемость): накопленное число событий исследования + каждый
	// (Домен, Модель) последнее время исследования (ключ "realm|model"）。и accounts[].model_costs
	// Сопоставление строк показывает "Исследование→выпуск» — сквозной тракт (единый источник истины, без двойного представления). Без регрессий, только добавление ключей.
	exploreEvents, exploreLast := h.cfg.Pool.CostExploreStatus()
	writeJSON(w, http.StatusOK, map[string]any{
		"accounts": h.cfg.Pool.List(),
		"total": total,
		"healthy": healthy,
		"cooling": cooling,
		"disabled": disabled,
		"in_flight_full": inFlightFull,
		// realm_totals Сводка счетчиков с группировкой по доменам (двойн. realm при сосуществовании эксплуатация сразу видит доступность каждого домена):
		// Только новые поля, существующие total/healthy/cooling/disabled/in_flight_full Ключ агрегации неизменен (нулевая регрессия).
		"realm_totals": map[string]map[string]int{
			"cn": countsMapFrom(h.cfg.Pool.CountsDetailedForRealm("cn")),
			"global": countsMapFrom(h.cfg.Pool.CountsDetailedForRealm("global")),
		},
		"sticky_sessions": sticky,
		"redis_mode": redisMode,
		// credit_floor Действующее минимальное гарантированное значение баллов (0 = закрыто). И accounts[].credits +
		// model_costs Сверкой можно определить "почему аккаунт не выдаёт билет на модель». Нулевые значения тоже пишутся явно
		// （С точки зрения эксплуатации: отсутствие создаёт впечатление отсутствия записи).
		"credit_floor": h.cfg.Pool.CreditFloor(),
		// cost_explore события и per-model таймстемп (значение времени из encoding/json Запись RFC3339）。
		"cost_explore": map[string]any{
			"events_total": exploreEvents,
			"per_model": exploreLast,
		},
	})
}

// countsMapFrom взять CountsDetailed упаковать 5-tuple в /status моделирование группировки по доменам.
func countsMapFrom(total, healthy, cooling, disabled, inFlightFull int) map[string]int {
	return map[string]int{
		"total": total,
		"healthy": healthy,
		"cooling": cooling,
		"disabled": disabled,
		"in_flight_full": inFlightFull,
	}
}

// dynamicModelsCache динамический кэш моделей.
var dynamicModelsCache struct {
	sync.RWMutex
	ids []upstream.ModelInfo
	fetched time.Time // время последнего успешного pull
	lastFail time.Time // Время последней неудачной выборки (негативный кэш)
}

const (
	// dynamicModelsTTL Время кэширования каталога моделей. Ранее было 1h；Сжать до 10min синхронизация "панель в реалтайме,
	// API кэша» дрейф (PR #38 отчет): при добавлении модели в каталог панель сразу видима, публично
	// /v1/models Отставание максимум на один TTL。короче уже нецелесообразно — каждый промах это 2 проверок апстрима.
	dynamicModelsTTL = 10 * time.Minute
	modelsFetchFailCooldown = 5 * time.Minute
)

// models Возврат списка моделей: чисто динамический (кэш 10min），ошибка/Без аккаунтов — пустой список (без статического фолбэка —
// не удалось вытянуть каталог — значит upstream недоступен, фейковый список заставит клиента выбрать 11102 модели).
func (h *Handler) models(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"object": "list",
		"data": h.modelList(),
	})
}

// fmtCreditsPrefix Из апстрима credits Извлечь множитель из оригинала и отформатировать как "[x0.05 credit]"。
// Форматы upstream не унифицированы:"x0.05 credits« / «x0.29« / »x0.00 credits" и т.д.,
// Единое извлечение xЧисло Часть, перейти "credits" суффикс.
func fmtCreditsPrefix(raw string) string {
	s := strings.TrimSpace(raw)
	s = strings.TrimSuffix(s, "credits")
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}
	return "[" + s + " credit]"
}

// applyModelInfoFields все поля объекта upstream-модели (ModelInfo）Записать правило по принципу "пропускать пустые значения»
// Слияние /v1/models Запись:name/description/credits/tags/vendor/флаг возможности/
// max_allowed_size/reasoning_effort/reasoning_summary。CN динамическая ветка и global
// Общая ветка попадания детекции (объекты моделей двух доменов изоморфны), гарантирует единый набор выходных полей.
// Без перекрытия id/object/created/owned_by и базовые поля, заранее заданные вызывающей стороной; не выданные апстримом
// Поле (нулевое значение) полностью опускается — не выдумывать.
func applyModelInfoFields(entry map[string]any, mi upstream.ModelInfo) map[string]any {
	if mi.Name != "" {
		entry["name"] = mi.Name
	}
	if mi.Description != "" {
		// префикс множителя очков: из "x0.05 credits« / "x0.29« и т.п. форматы — извлечь только цифры,
		// Унифицировано в "[x0.05 credit]« Префикс подставляется description，Для прямого отображения на downstream-панели.
		if mi.Credits != "" {
			entry["description"] = fmtCreditsPrefix(mi.Credits) + " " + mi.Description
		} else {
			entry["description"] = mi.Description // descriptionZh Описание на китайском
		}
	}
	if mi.Credits != "" {
		entry["credits"] = mi.Credits // исходный текст множителя баллов (напр. "x0.05"),Только отображение
	}
	if len(mi.Tags) > 0 {
		entry["tags"] = mi.Tags
	}
	if mi.Vendor != "" {
		entry["vendor"] = mi.Vendor
	}
	if mi.IsDefault {
		entry["is_default"] = true
	}
	if mi.SupportsImages {
		entry["supports_images"] = true // экспонирование мультимодальных возможностей
	}
	if mi.SupportsReasoning {
		entry["supports_reasoning"] = true
		if mi.CanDisableThinking {
			entry["can_disable_thinking"] = true
		}
	}
	if mi.SupportsToolCall {
		entry["supports_tool_call"] = true
	}
	if mi.OnlyReasoning {
		entry["only_reasoning"] = true
	}
	if mi.MaxAllowedSize > 0 {
		entry["max_allowed_size"] = mi.MaxAllowedSize
	}
	if mi.ReasoningEffort != "" {
		entry["reasoning_effort"] = mi.ReasoningEffort
	}
	if mi.ReasoningSummary != "" {
		entry["reasoning_summary"] = mi.ReasoningSummary
	}
	return entry
}

// modelList Список моделей:CN К выводу модели единообразно добавляется "cn:" Префикс (gateway Протокол маршрутизации, и resolveModel
// симметрично);global.enabled=true добавить при global: список intl-версии с префиксом.
// чисто динамический: сбой динамической загрузки/Без номера → Для этого домена пустой список, без статического фолбэка.
func (h *Handler) modelList() []map[string]any {
	out := make([]map[string]any, 0)
	for _, mi := range h.fetchDynamicModels() {
		entry := map[string]any{
			"id": "cn:" + mi.ID,
			"object": "model",
			"created": 1753600000,
			"owned_by": "workbuddy",
		}
		// context_length / max_output_tokens четырехуровневый поиск (upstream.model_catalog）：
		// динамическое значение upstream (maxInputTokens/maxOutputTokens）авторитетный → статическая таблица seed'ов →
		// model.json Локальный кэш → models.dev Подгрузка по требованию (асинхронно, не блокирует текущий ответ, после получения
		// Запись model.json для следующего попадания)→ 1M Фолбэк / max_output_tokens Пропущено.
		// Нулевое значение апстрима больше не прокидывается как фиктивное 131072（Вводит в заблуждение Codex/ZCode и т.д. по context_length Заранее
		// обрезка, напрасная потеря контекста).
		entry["context_length"] = upstream.ContextWindowListingV4(mi.ID, mi.ContextWindow, h.cfg.Upstream.HTTP)
		if mo, ok := upstream.MaxOutputTokensListingV4(mi.ID, mi.MaxTokens, h.cfg.Upstream.HTTP); ok {
			entry["max_output_tokens"] = mo
		}
		// Все поля объекта модели апстрима пробрасываются (name/описание/Тег/Множитель/флаги возможностей и т.п., пустые значения опускаются).
		entry = applyModelInfoFields(entry, mi)
		// effort экспонирование возможности — удаленно supportedEfforts авторитетен, при отсутствии — fallback на CN статическая fallback-таблица
		// （клиент может обнаружить тариф, без слепой передачи). Без тарифа → Пропустить поле.
		if efforts, def := upstream.EffortListing("cn", mi.ID, mi.Efforts, mi.DefaultEffort); efforts != nil {
			entry["reasoning_supported_efforts"] = efforts
			if def != "" {
				entry["reasoning_default_effort"] = def
			}
		}
		out = append(out, entry)
	}
	// global Список моделей: только GlobalEnabled=true вывести при (аварийный выход).
	// Список = результат чисто динамического зондирования (fetchGlobalModels，ошибка/Без номера → пусто).
	if h.cfg.GlobalEnabled {
		// global Домен effort трехуровневый поиск capability: проба бакета выдачи (авторитетный)→ статическая fallback-таблица → Пропущено.
		// Сначала fetchGlobalModels（Внутренняя проба и запись effort бакет), затем по id Сделать снапшот.
		globalIDs, globalAccount := h.fetchGlobalModels()
		// полнопольная запись формы объекта зондирования (с fetchGlobalModels совместное использование одного кэша зондирования):
		// Попадание id только тогда отдавать расширенные поля; узкая таблица/ошибка → nil，по raw ID Вывод записей (без выдуманных полей).
		// globalAccount для nil（отсутствует global №) возвращает nil，Пропустить маппинг расширенных полей.
		globalInfos := map[string]upstream.ModelInfo{}
		for _, mi := range h.cfg.Upstream.FetchGlobalModelInfos(globalAccount) {
			globalInfos[mi.ID] = mi
		}
		globalEfforts, globalDefaults := h.cfg.Upstream.GlobalEffortSnapshot()
		for _, id := range globalIDs {
			entry := map[string]any{
				"id": "global:" + id,
				"object": "model",
				"created": 1753600000,
				"owned_by": "workbuddy",
			}
			// context_length / max_output_tokens четырёхуровневый поиск (с CN динамическая ветка — тот же калибр).
			var remoteCtx, remoteOut int64
			if mi, ok := globalInfos[id]; ok {
				entry = applyModelInfoFields(entry, mi)
				remoteCtx, remoteOut = mi.ContextWindow, mi.MaxTokens
			}
			entry["context_length"] = upstream.ContextWindowListingV4(id, remoteCtx, h.cfg.Upstream.HTTP)
			if mo, ok := upstream.MaxOutputTokensListingV4(id, remoteOut, h.cfg.Upstream.HTTP); ok {
				entry["max_output_tokens"] = mo
			}
			if efforts, def := upstream.EffortListing("global", id, globalEfforts[id], globalDefaults[id]); efforts != nil {
				entry["reasoning_supported_efforts"] = efforts
				if def != "" {
					entry["reasoning_default_effort"] = def
				}
			}
			out = append(out, entry)
		}
	}
	return out
}

// fetchGlobalModels вернуть global список моделей (чисто динамический результат детекта) и проверяемый аккаунт.
// Кеш/Фолбэк при ошибке фиксируется в upstream.FetchGlobalModels（Внутренний 1h + 5min отрицательный кэш).
// Данный метод отвечает только за"Когда зондировать"：В пуле нет global Аккаунт → пустой список + nil аккаунт (ноль вызовов апстрима).
// Возвращённый acct Для выборки caller'ом rich-данных на одном аккаунте ModelInfo（FetchGlobalModelInfos и
// FetchGlobalModels общий кэш, не вызовет повторного зондирования апстрима).
// GlobalEnabled=false Время modelList уже не входит в эту ветку (аварийный выход у вызывающей стороны gate）。
func (h *Handler) fetchGlobalModels() ([]string, *auth.Auth) {
	acct := h.cfg.Pool.PickExcludingForRealm(nil, "", "global")
	if acct == nil {
		return nil, nil
	}
	return h.cfg.Upstream.FetchGlobalModels(acct), acct
}

// fetchDynamicModels с первого доступного CN аккаунт запрашивает список моделей (вкл. contextWindow/maxTokens），
// Кеш 10min。
// Выбор номера и /panel/api/models Полностью в том же срезе (AvailableUIDsForRealm("cn") первый + AuthByUID），
// а не Pool.Pick()：Pick отсутствует realm фильтрация, в смешанном пуле может быть выбран global номером бить CN Эндпоинт,
// Проявляется как спорадический сбой/Панель и /v1/models два набора каталогов (PR #38 отчёт и исправление выбора номера).
// Кеш + 5min негативный кэш по прежней семантике**сохранить**（#38 Отказ в полной очистке кэша по исходному кейсу): публичный эндпоинт — на каждый запрос
// Получение в реальном времени = Каждый раз 2 проб апстрима, клиент периодически обновляет список моделей с постоянными запросами к апстриму; при сбое апстрима
// Без окна охлаждения повтор клиента сразу усиливает нагрузку — negative cache как раз для этого (см. handler_test поглощение
// апстрим 9832283 комментарий); и cachedModelsSnapshot（gateway_hint решение) зависит от записи в кэш.
func (h *Handler) fetchDynamicModels() []upstream.ModelInfo {
	dynamicModelsCache.RLock()
	if len(dynamicModelsCache.ids) > 0 && time.Since(dynamicModelsCache.fetched) < dynamicModelsTTL {
		out := dynamicModelsCache.ids
		dynamicModelsCache.RUnlock()
		return out
	}
	// Негативный кэш ошибки: в период кулдауна не запрашивать апстрим.
	if !dynamicModelsCache.lastFail.IsZero() && time.Since(dynamicModelsCache.lastFail) < modelsFetchFailCooldown {
		dynamicModelsCache.RUnlock()
		return nil
	}
	dynamicModelsCache.RUnlock()

	uids := h.cfg.Pool.AvailableUIDsForRealm("cn")
	if len(uids) == 0 {
		return nil
	}
	acct := h.cfg.Pool.AuthByUID(uids[0])
	if acct == nil {
		return nil
	}
	infos, err := h.cfg.Upstream.FetchModels(acct)
	if err != nil || len(infos) == 0 {
		// Ошибка pull — только в негативный кэш (5min lastFail），Не NoteError：NoteError подаётся chat
		// Circuit breaker,models Эндпоинт — спорадически 5xx Кросс-штраф chat Аккаунты со здоровым каналом;
		// models Ошибка получения ≠ Аккаунт chat недоступно.
		dynamicModelsCache.Lock()
		dynamicModelsCache.lastFail = time.Now()
		dynamicModelsCache.Unlock()
		return nil
	}
	dynamicModelsCache.Lock()
	dynamicModelsCache.ids = infos
	dynamicModelsCache.fetched = time.Now()
	dynamicModelsCache.lastFail = time.Time{} // при успехе — очистка негативного кэша
	dynamicModelsCache.Unlock()
	return infos
}

// cachedModelsSnapshot кэш каталога моделей только для чтения (TTL снимок внутри); кэш холодный/пустой → nil。
// Без исходящих вызовов upstream (hint Для решения: +1 по ошибочному пути FetchModels сетевой вызов замедляет
// ошибочный ответ и загрязняет семантику вызова апстрима).
func cachedModelsSnapshot() []upstream.ModelInfo {
	dynamicModelsCache.RLock()
	defer dynamicModelsCache.RUnlock()
	if len(dynamicModelsCache.ids) == 0 || time.Since(dynamicModelsCache.fetched) >= dynamicModelsTTL {
		return nil
	}
	return dynamicModelsCache.ids
}

func (h *Handler) chatCompletions(w http.ResponseWriter, r *http.Request) {
	// Клиент IP извлечение (передается с запросом в ChatStream，При отсутствии проброса upstream игнорируется на стороне);
	// устранение кросс-загрязнения конкурентности старой схемы общих полей (issue：ClientIP гонка).
	clientIP := upstream.ExtractClientIP(r)
	// тело запроса без лимита размера (max_body_mb уже удалено, выровнено с апстримом): полное чтение, проблемы превышения лимита передаются
	// Апстрим сам вернет ошибку (его ответ пройдет по существующей цепочке классификации ошибок, информативнее).#41 усечение
	// Защитная семантика сохраняется в пути ошибки чтения — после снятия предперехвата обрыв возможен только из-за разрыва соединения самим клиентом,
	// Чтение body При ошибке — обработка на месте 400，Не брать половину JSON подача в апстрим unmarshal Отчет unexpected EOF Ложное наказание аккаунта.
	body, err := io.ReadAll(r.Body)
	if err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request", "read body: "+err.Error())
		return
	}
	var peek struct {
		Stream bool `json:"stream"`
		Model string `json:"model"`
	}
	_ = json.Unmarshal(body, &peek)

	// realm разбор префикса (D6）：model Имя может содержать "[realm:]" префикс. Извлечь realm + bareModel，
	// bareModel Для выбора номера/Липкость/исходящий body Перезапись (префикс — протокол маршрутизации на стороне шлюза, апстрим принимает только чистое имя).
	// Голое имя → ("cn", Исходная строка)，CN текущее состояние — без регресса.
	realm, bareModel := resolveModel(peek.Model)
	modelRate := ""
	if h.cfg.Upstream != nil {
		modelRate = h.cfg.Upstream.ModelRate(realm, bareModel)
	}

	// Статистика на уровне запроса: на выходе сразу строка табличного лога (проходит любой путь).
	st := newChatStat(time.Now(), body, peek.Stream)
	if tr := requestTraceFrom(r); tr != nil {
		tr.stat = st
		// Источник в ServeHTTP Сбор на входе (только здесь известны флаги и заголовки запроса), здесь передать объекту статистики,
		// разрешить stdout Строка потока и событие архива используют одно значение источника, расхождения исключены.
		st.clientIP, st.userAgent = tr.clientIP, tr.userAgent
	}
	defer st.done()

	tried := map[string]bool{}
	var lastErr error

	// Липкость сессии: извлечь ключ сессии из тела запроса и распарсить привязанный номер (не найдено/если недействительно, то stickyUID пусто — идти через обычную ротацию).
	// ExtractKey Отвязано от sticky-переключателя (issue #35 сторона): при отключении sticky агрегирующий PK семейства заголовков сессии всё равно по
	// Уровень сессии (RequestIDForKey(sessKey)），Не деградировать тихо до round-robin — само извлечение не связано с аффинностью.
	sessKey := session.ExtractKey(body)
	stickyUID := ""
	if h.cfg.Session != nil && sessKey != "" {
		// Парсинг по модели: привязанный номер в**Текущая модель**Получено 6004 При достижении лимита считать недоступным → перераспределить,
		// а не зависать на лимитном номере с повторными ошибками («После лимита смена номера невозможна«правильное решение).
		if uid, ok := h.cfg.Session.ResolveForModel(sessKey, peek.Model); ok {
			stickyUID = uid
		}
	}

	// Ключ агрегации уровня раунда: по body последняя запись в user деривация сообщения (все апстрим-вызовы в раунде с одним ключом,
	// замена user смена ключа сообщения).#170 клиенты с ключом сессии также идут через ротацию на уровне раунда (выравнивание с официальным десктопом CLI
	// X-Conversation-Request-ID семантика на уровне раунда —TraceStartHook Каждый раз USER_PROMPT_SUBMIT
	// очистка и перегенерация), поэтому больше не лимитируется sessKey==«« только тогда считать;sessKey ниже по коду производным составным ключом
	// Ключ ввода (защита от коллизии текста одного раунда в разных сессиях).
	// Должно быть ниже prompt.Rewrite брать до — перезапись затронет messages контент, последующее чтение сдвинет ключ.
	turnKey := session.TurnKey(body)

	// gateway_hint Форма запроса для проверки (image_url part）：брать до рерайта (с turnKey
	// аналогично).11133「предпосылка для указания "модель не поддерживает изображения».
	reqHasImage := hasImagePart(body)

	// аренда in-flight: успешный выбор сразу резервирует слот; выход из функции (включая успех return и panic）Единое освобождение.
	var heldUID string
	defer func() {
		if heldUID != "" {
			h.cfg.Pool.Release(heldUID)
		}
	}()
	releaseHeld := func() {
		if heldUID != "" {
			h.cfg.Pool.Release(heldUID)
			heldUID = ""
		}
	}
	// unbindSticky Отвязать sticky-аккаунт текущей сессии (stickyUID когда непусто). Для "sticky-номер недоступен/«перехвачено» и fail Общий.
	// Идемпотентность:stickyUID если уже пусто — no-op; не снимет привязку другого раунда. Только когда Session != nil Время stickyUID только тогда непусто.
	unbindSticky := func() {
		if stickyUID != "" {
			h.cfg.Session.Unbind(sessKey)
			stickyUID = ""
		}
	}
	// fail В ветке ошибки ротации едино: освободить лиз + если упавший аккаунт — sticky, отвязать (переназначить при следующем запросе).
	fail := func(uid string) {
		releaseHeld()
		if stickyUID != "" && uid == stickyUID {
			unbindSticky()
		}
	}
	recordAttempt := func(uid string, delta pool.TokenUsageDelta, credit float64, hasCredit bool, started time.Time) {
		st.attempts++
		if delta.HasPromptTokens {
			st.promptTokens = delta.PromptTokens
		}
		if delta.HasCompletionTokens {
			st.completionTokens = delta.CompletionTokens
		}
		if delta.HasTotalTokens {
			st.totalTokens = delta.TotalTokens
		} else if delta.HasPromptTokens || delta.HasCompletionTokens {
			st.totalTokens = st.promptTokens + st.completionTokens
		}
		delta.Model = bareModel
		if delta.Model == "" {
			delta.Model = peek.Model
		}
		latency := time.Since(started)
		latencyMs := latency.Milliseconds()
		if latencyMs < 1 {
			latencyMs = 1
		}
		delta.HasLatencyMs = true
		delta.LatencyMs = latencyMs
		if delta.HasCompletionTokens && delta.CompletionTokens >= 0 && latencyMs > 0 {
			delta.HasTokensPerSecond = true
			delta.TokensPerSecond = float64(delta.CompletionTokens) * 1000 / float64(latencyMs)
		}
		h.cfg.Pool.RecordTokenUsage(uid, delta)

		// Запись таймлайна использования.ok по признаку "выдал ли апстрим usage」Условие: пусто delta Означает, что эта попытка
		// Ничего не получено token статистика (ошибка передачи / >=400 / ошибка парсинга), засчитывается как неудачная попытка.
		// Неудачи тоже учитываются в числе запросов — иначе amplification ретраев не видна в представлении "Использование».
		if h.cfg.Usage != nil {
			realm := "cn"
			if a, ok := h.cfg.Pool.Status(uid); ok && a.Realm != "" {
				realm = a.Realm
			}
			h.cfg.Usage.Add(time.Now(), realm, uid, delta.Model, usage.Delta{
				PromptTokens: delta.PromptTokens,
				HasPromptTokens: delta.HasPromptTokens,
				CompletionTokens: delta.CompletionTokens,
				HasCompletion: delta.HasCompletionTokens,
				TotalTokens: delta.TotalTokens,
				HasTotal: delta.HasTotalTokens,
				Credit: credit,
				HasCredit: hasCredit,
				ModelRate: modelRate,
				LatencyMs: delta.LatencyMs,
				HasLatency: delta.HasLatencyMs,
				TokensPerSecond: delta.TokensPerSecond,
				HasTPS: delta.HasTokensPerSecond,
			}, delta.HasTotalTokens || delta.HasCompletionTokens || delta.HasPromptTokens)
		}
	}

	// переписывание системного промпта (перед отправкой, перед ротацией; один раз на запрос).
	// - custom：Заменить клиентский промпт своим system/developer（Устранить в корне system ложное срабатывание отпечатка).
	// - append：подряд с начала system/developer после блока вставить собственный промпт, существующие сообщения — дословно без изменений
	// （спецификация проекта клиента/Соглашение об инструментах совместно с промптом шлюза,issue #129）。
	// - passthrough + Период даунгрейда: смена Degraded нейтральный промпт напрямую, без предварительного коллизии 400。
	// - passthrough / append вне деградации: прозрачная передача оригинала клиента system（append то вставить еще один шлюз system）。
	// Решение о даунгрейде:append в период деградации деградирует до replace（Rewrite(Degraded)）——append Лента
	// Повтор с исходным fingerprint детерминированно снова упрется в стену,replace одноразовое минимальное восстановление (issue #129 Дизайн §4）。
	degradedApplied := false
	if h.cfg.PromptMode == "custom" && h.cfg.PromptText != "" {
		body = prompt.Rewrite(body, h.cfg.PromptText)
	} else if h.cfg.PromptMode == "append" && h.cfg.PromptText != "" && !h.degrade.Active() {
		body = prompt.Append(body, h.cfg.PromptText)
	} else if (h.cfg.PromptMode == "passthrough" || h.cfg.PromptMode == "append") && h.degrade.Active() {
		body = prompt.Rewrite(body, prompt.Degraded)
		degradedApplied = true
	}

	// outbound model имя переопределено в bareModel（D6）：realm Префикс — протокол маршрутизации на стороне шлюза,
	// Апстрим не распознаёт префикс (global аккаунт также запрашивает голое имя модели). При голом имени bareModel==peek.Model Тождественно.
	if bareModel != peek.Model {
		body = rewriteModel(body, bareModel)
	}

	// Семейство заголовков сессии (issue #35）：Бэкенд по X-Conversation-Request-ID（уровень раунда диалога) агрегированный запрос,
	// официальный клиент один раз user send все внутри tool call/повтор/Смена аккаунта с переиспользованием того же ID。здесь**ротация
	// Вне цикла**Сгенерировать один раз, в цикле при каждом исходящем вызове переиспользовать как есть → Смена номера/повтор/деградация всех однотипных ID，Бэкенд больше не
	// Фрагментация (ранее шлюз ничего не отправлял, апстрим по HTTP поштучный учет запросов, десятки-сотни на один диалог
	// RequestID）。
	// - conversationID：body Извлечь (прозрачно передать исходное значение клиента, по умолчанию пустая строка — без подделки);
	// - conversationRequestID：inbound X-Conversation-Request-ID приоритет сквозной передачи, иначе по
	// Липкость key стабильная генерация внутри процесса; sticky key если тоже пусто — идти в поуровневый fallback (TurnKey/TurnRequestID），
	// отсутствует user при сообщении деградация до рандома на уровне запроса — один перехват внутри ротации шарится;
	// - messageID В ChatHeaders Генерация внутри каждого сообщения (независимо на уровне сообщения, без внешней видимости).
	chatMeta := upstream.ChatMeta{ConversationID: session.ResolveConversationID(body)}
	if v := r.Header.Get("X-Conversation-Request-ID"); v != "" {
		chatMeta.ConversationRequestID = v
	} else if turnKey != "" && sessKey != "" {
		// составной ключ уровня раунда:sessKey ключ ввода для защиты от коллизии текста одного раунда между сессиями (#170 унифицированный уровень раунда).
		chatMeta.ConversationRequestID = session.TurnRequestID(sessKey + ":" + turnKey)
	} else if turnKey != "" {
		// Клиент без ключа сессии: чисто раундовый ключ (fallback-семантика без изменений, дрейф значений существующих ключей сессии нулевой).
		chatMeta.ConversationRequestID = session.TurnRequestID(turnKey)
	} else if sessKey != "" {
		// фолбэк на остаточное пустое состояние (нет user Сообщение/нет подписываемого контента): агрегация на уровне сессии, лучше рандома на уровне запроса.
		chatMeta.ConversationRequestID = session.RequestIDForKey(sessKey)
	} else {
		// Нет ключа сессии и ключа раунда: рандом на уровне запроса (захват один раз за ротацию и шарится).
		chatMeta.ConversationRequestID = session.TurnRequestID("")
	}
	chatMeta.TraceID = r.Header.Get("X-Trace-ID")

	for i := 0; i < h.cfg.MaxRotate; i++ {
		// Выбор номера: приоритет у sticky-номера (PickByUIDForModel Доступность этой модели уже проверена + в пути не заполнен), иначе обычная ротация.
		var acct *auth.Auth
		if stickyUID != "" {
			acct = h.cfg.Pool.PickByUIDForModel(stickyUID, bareModel)
			if acct == nil || (realm != "" && acct.Realm() != realm) {
				// Sticky-ID недоступен для текущей модели (кулдаун/заполнено/данная модель 6004 лимит) или realm Несоответствие → Отвязать,
				// В этот раз откат к обычной ротации.
				unbindSticky()
				acct = nil
			}
		}
		if acct == nil {
			// Осведомленность о модели + realm выбор с учетом восприятия: включается при непустой модели 6004 освобождение от кулдауна на уровне модели
			// （healthyForModel），realm предикат фильтрует кросс-доменные аккаунты.
			acct = h.cfg.Pool.PickExcludingForRealm(tried, bareModel, realm)
		}
		if acct == nil {
			st.status = http.StatusServiceUnavailable
			break
		}
		st.uid = acct.UID
		// Синхронизация никнейма: в строке лога запроса только запись uid8 невозможно визуально определить какой номер, никнейм подставляется в строку лога при текущем выборе.
		st.nick = acct.Nickname
		tried[acct.UID] = true

		// занятие квоты in-transit:Pick аккаунты с исчерпанным лимитом пропущены, здесь CAS Страховка от гонки при конкурентном захвате слотов.
		if !h.cfg.Pool.Acquire(acct.UID) {
			// если захвачен именно sticky-аккаунт, сразу отвязать и откатиться к обычной ротации, чтобы в след. раунде не попасть на тот же
			// не тратить лишний раз sticky-аккаунт при полной загрузке PickByUID туда-обратно (семантика и fail()/PickByUID-nil соответствует отвязке).
			if stickyUID != "" && acct.UID == stickyUID {
				unbindSticky()
			}
			if !rotateBackoff(i, r.Context()) {
				// клиент уже отключён: смена аккаунта и ретрай бессмысленны, прекратить ротацию и пробросить конечную ошибку.
				break
			}
			continue // Последний слот захвачен конкурентно → Смена номера
		}
		heldUID = acct.UID

		// token Скоро истекает → Сначала refresh（cooldown при ошибке со сменой номера)
		if acct.NeedsRefresh(h.cfg.RefreshSkew) {
			if err := h.cfg.Upstream.RefreshToken(acct); err != nil {
				lastErr = err
				var ue *upstream.Error
				if errors.As(err, &ue) && ue.Kind == upstream.ErrSessionDead {
					h.cfg.Pool.Disable(acct.UID, "refresh session dead")
				} else {
					h.cfg.Pool.NoteError(acct.UID)
				}
				fail(acct.UID)
				if !rotateBackoff(i, r.Context()) {
					break // ctx Отмена: прервать ротацию (refresh бэкофф со сменой аккаунта при ошибке)
				}
				continue
			}
			if err := acct.SaveAtomic(); err != nil {
				// обновление успешно, но запись на диск не удалась: при следующем запуске будет использован старый token，должен экспонироваться
				log.Printf("ERR: [server] chat refresh acct=%s: save auth failed: %v", logfmt.Label(acct.UID, acct.Nickname), err)
			}
		}

		// Клиент IP Передача по запросу (PassthroughIP инжект при включении; устранение гонки по общему полю).
		attemptStarted := time.Now()
		rc, status, respBody, terr := h.cfg.Upstream.ChatStreamContext(r.Context(), acct, body, clientIP, chatMeta)
		// Классифицирующий конверт формируется за один раз:upstream Уже возвращено по ветке ошибки *upstream.Error（Kind +
		// Retry-After разбор заголовка). Ошибка транспортного уровня (не *Error）ветка смены номера с джиттером; защитная ветка
		// （terr для nil Но status>=400，Например ErrNone запасной) откат на локальный Classify，Двойная страховка.
		var uerr *upstream.Error
		if errors.As(terr, &uerr) {
			status = uerr.Status
		}
		if uerr == nil && terr != nil {
			// Сетевой джиттер: только смена аккаунта, без инкремента счётчикаОтключение (ошибки транспортного уровня слишком строги для последовательногоОтключение).
			// Фолбэк при серии неудач (issue #114）：подача счётчика серийных неудач — недоступность апстрима это "неудача с неизвестной причиной»,
			// серия неудач N раз временно выведен из пула, однократно/Единичный сбой не штрафуется (NoteFailures действие только при достижении внутреннего порога).
			// апстрим client уже размечено transport error лог.
			recordAttempt(acct.UID, pool.TokenUsageDelta{}, 0, false, attemptStarted)
			st.status = http.StatusServiceUnavailable
			lastErr = terr
			h.cfg.Pool.NoteFailures(acct.UID)
			fail(acct.UID)
			if !rotateBackoff(i, r.Context()) {
				break // ctx Отмена: прервать ротацию (ошибка транспортного уровня — смена номера с бэкоффом)
			}
			continue
		}
		if status >= 400 {
			recordAttempt(acct.UID, pool.TokenUsageDelta{}, 0, false, attemptStarted)
			st.status = status
			var kind upstream.ErrKind
			if uerr != nil {
				kind = uerr.Kind
			} else {
				kind = upstream.Classify(status, string(respBody))
				uerr = &upstream.Error{Kind: kind, Status: status, Msg: string(respBody)}
			}
			// Ложное срабатывание блокировки контента (passthrough/append первая встреча режима): считается system ложное срабатывание отпечатка,
			// триггер даунгрейда на следующий день 00:00 CST，замена Degraded Нейтральный промпт, повтор внутри запроса (append
			// ретрай с даунгрейдом также деградирует в replace——при наличии оригинала детерминированно снова коллизия 400）。
			// вторая попытка снова заблокирована (контент пользователя сам триггерит модерацию)→ Возврат ошибки контент-фильтра (см. ветку ниже).
			// проблема контента, а не аккаунта:applyErrorPolicy аккаунт не штрафуется (см. ErrContentBlocked ветка).
			if kind == upstream.ErrContentBlocked && (h.cfg.PromptMode == "passthrough" || h.cfg.PromptMode == "append") && !degradedApplied {
				h.degrade.Trigger()
				body = prompt.Rewrite(body, prompt.Degraded)
				degradedApplied = true
				delete(tried, acct.UID) // Даже пул из одного аккаунта получает шанс ретрая (ретрай с даунгрейдом занимает один слот)
				releaseHeld()
				log.Printf("content-blocked (likely fingerprint false positive) -> degraded prompt retry")
				continue
			}
			if kind == upstream.ErrContentBlocked {
				// Контент сработал на контент-WAF шлюза: немедленно вернуть клиенту,**без ротации**——смена любого аккаунта приведет к тому же
				// модерация, ротация — пустая трата времени. Аккаунт не штрафуется (ErrContentBlocked Ветка без кулдауна/Circuit Breaker/NoteError）。
				// error-passthrough：message Установить апстрим body оригинал (code/msg/requestId как есть),
				// больше не переписывать на фиксированный текст шлюза — клиент должен видеть реальную ошибку для диагностики.
				h.applyErrorPolicy(acct.UID, kind, string(respBody), bareModel, uerr)
				fail(acct.UID)
				msg := string(respBody)
				if strings.TrimSpace(msg) == "" {
					// пустой body Фолбэк: нет исходного текста апстрима для транзита, сохранить читаемый текст категории (не выдумывать оригинал).
					msg = "content blocked by upstream content firewall"
				}
				writeOpenAIErrorHint(w, http.StatusBadRequest, "content_blocked", msg,
					h.hintOf(upstream.ErrContentBlocked, string(respBody), bareModel, reqHasImage, uerr))
				st.status = http.StatusBadRequest
				st.outcome = reqlog.OutcomeHTTPError
				return
			}
			// 11115「prompt is too long」：Немедленно прокинуть исходный ответ апстрима клиенту,**без штрафа аккаунта и без ротации**
			// ——Превышение контекста — ошибка запроса (тот же body смена любого аккаунта — всё равно превышение лимита, зря тратится квота здорового аккаунта;
			// и WAF IP fail-fast та же философия: ошибка, точно не связанная с аккаунтом, сразу прерывает ротацию).
			// applyErrorPolicy ErrPromptTooLong ветка без действия,fail Освобождает только лиз.
			// message Установить апстрим body Оригинал (включая реальный token число и лимит — оригинал апстрима наиболее ценен
			// сообщение об ошибке, клиент должен видеть, запрещена перезапись фиксированным текстом).
			if kind == upstream.ErrPromptTooLong {
				h.applyErrorPolicy(acct.UID, kind, string(respBody), bareModel, uerr)
				fail(acct.UID)
				writeOpenAIErrorHint(w, http.StatusBadRequest, "prompt_too_long", promptTooLongMessage(string(respBody)),
					h.hintOf(upstream.ErrPromptTooLong, string(respBody), bareModel, reqHasImage, uerr))
				st.status = http.StatusBadRequest
				st.outcome = reqlog.OutcomeHTTPError
				return
			}
			// формат изображения/Данные недействительны: немедленно прокинуть оригинал апстрима клиенту, без штрафа для номера и без ротации.
			// Тот же body Смена аккаунта даёт тот же результат парсинга, ротация лишь усилит невалидные запросы.
			if kind == upstream.ErrImageInvalid {
				h.applyErrorPolicy(acct.UID, kind, string(respBody), bareModel, uerr)
				fail(acct.UID)
				msg := string(respBody)
				if strings.TrimSpace(msg) == "" {
					msg = "image request was rejected by upstream"
				}
				writeOpenAIErrorHint(w, http.StatusBadRequest, "image_invalid", msg,
					h.hintOf(upstream.ErrImageInvalid, string(respBody), bareModel, reqHasImage, uerr))
				st.status = http.StatusBadRequest
				st.outcome = reqlog.OutcomeHTTPError
				return
			}
			// lastErr Нести полностью body（uerr.Msg В upstream Обрезка на стороне 200 символы, прозрачная передача семантики
			// требуется полный оригинал)+ Kind/RetryAfter（Маппинг endpoint и длительность cooldown — общие).
			lastErr = &upstream.Error{Kind: kind, Status: status, Msg: string(respBody), RetryAfter: uerr.RetryAfter}
			h.applyErrorPolicy(acct.UID, kind, string(respBody), bareModel, uerr)
			fail(acct.UID)
			// WAF IP Уровень fail-fast（имеет приоритет над rotateBackoff Бэкофф —IP бэкофф бессмыслен при перехвате уровня):
			// данный раз WAF 403 Подать на вход IP автомат уровня, при активации (попадания по нескольким номерам в коротком окне,IP заблокирован, а не аккаунт)
			// то немедленно прервать ротацию — дальнейшая смена номера только усилит запросы MaxRotate Повтор на тот же egress IP，
			// усиление риск-контроля. мягкий кулдаун на уровне аккаунта — выше applyErrorPolicy штатный учёт.
			if kind == upstream.ErrWafBlock && h.wafIP.noteWaf(acct.UID) {
				break
			}
			if !rotateBackoff(i, r.Context()) {
				break // ctx Отмена: прервать ротацию (откат со сменой номера при ошибке классификации)
			}
			continue
		}
		h.cfg.Pool.NoteSuccess(acct.UID)
		// 11102 Сброс негативного кэша: для этого аккаунта и модели успех подтвержден тестом, немедленно снять обход (не ждать TTL истекает).
		// BlockModelClear Нажать "11102" reason Распознавание по префиксу, очищать только 11102 записей, не трогать 6004 независимый cooldown.
		h.cfg.Pool.BlockModelClear(acct.UID, bareModel)
		// Липкая привязка к итоговому успешному номеру: аккаунт, успешный в этом раунде, становится липкой привязкой сессии (перекрывает старую привязку).
		// Если sticky ошибка аккаунта, ротация на другой успешна — сессия перепривязывается к новому аккаунту, следующий хоп многораундового диалога без случайного выбора.
		if sessKey != "" && h.cfg.Session != nil {
			h.cfg.Session.Bind(sessKey, acct.UID)
		}
		if peek.Stream {
			// Потоковый режим: сразу закрыть апстрим после сквозной передачи body，Избежать defer Накапливается в сценарии ротации fd。
			st.status = http.StatusOK
			stats := newChatStatsReaderSince(rc, st.start)
			// gateway_hint（SSE）：статус успеха 200 Поток уже открыт, в середине error Добавляется при сквозной передаче фрейма
			// hint Поле (hintFn ленивое вычисление — в штатном потоке нулевые накладные расходы, только при реальном коллизии error кадр только
			// Собрать контекст запроса для решения).
			sErr := upstream.StreamHint(w, stats, upstream.FrameHintFunc(func() upstream.HintContext {
				return h.hintContext(bareModel, reqHasImage)
			}))
			switch {
			case upstream.IsEmptyStreamError(sErr):
				// апстрим 200 Но пустой поток (0 валидный кадр):StreamHint Уже записано error Кадр + [DONE]
				// фолбэк (HTTP Заголовки уже отправлены, можно только 200），Но это дефект апстрима, а не успех — лог/
				// Состояние сходится к 502 наблюдение, с нестриминговым Aggregate Пустой поток→502 upstream_parse
				// Та же семантика (ранее `_ =` Подавление ошибки с пометкой неуспешного потока как 200，эксплуатация видит ложный успех).
				// Принимается только IsEmptyStreamError：ошибка записи при дисконнекте клиента не помечается ложно (клиент уже ушёл,
				// 502 наблюдение бессмысленно).
				st.status = http.StatusBadGateway
				st.outcome = reqlog.OutcomeStreamError
				log.Printf("WARN: [server] stream acct=%s model=%s: empty upstream stream (200+0 frames)", logfmt.Label(acct.UID, acct.Nickname), bareModel)
			case sErr != nil:
				st.outcome = reqlog.OutcomeInterrupted
			case stats.SawErrorFrame():
				st.outcome = reqlog.OutcomeStreamError
			default:
				st.outcome = reqlog.OutcomeSuccess
			}
			credit, hasCredit := stats.Credit()
			recordAttempt(acct.UID, stats.Usage(), credit, hasCredit, attemptStarted)
			st.ttfb = stats.TTFB()
			// usage При отсутствии сохранить chatStat.toks -1 Сентинел (отсутствие наблюдения → Отображение "-"），
			// ноль не писать — иначе "не наблюдалось usage」сфальсифицировано как "измерено 0 token」，
			// и нестриминговый путь completionTokens вернуть -1 несогласованность метрики.
			if toks, hasUsage := stats.Tokens(); hasUsage {
				st.toks = toks
			}
			// Учет стоимости: последний фрейм usage Лента credit и token при подсчёте общего кол-ва фиксировать факт. цену за ед.,
			// для следующего выбора номера бесплатно/Дешевые аккаунты в начале.
			if hasCredit {
				st.credit = credit
				st.hasCredit = true
				if total, tok := stats.TotalTokens(); tok && total > 0 {
					h.cfg.Pool.NoteModelCost(acct.UID, bareModel, credit, total)
				}
			}
			rc.Close()
			return
		}
		resp, err := upstream.Aggregate(rc)
		rc.Close()
		if err != nil {
			recordAttempt(acct.UID, pool.TokenUsageDelta{}, 0, false, attemptStarted)
			// Ошибка парсинга upstream-потока: клиент еще не видел вывода, вернуть 502 и сообщить причину.
			writeOpenAIError(w, http.StatusBadGateway, "upstream_parse", err.Error())
			st.status = http.StatusBadGateway
			st.outcome = reqlog.OutcomeHTTPError
			return
		}
		credit, total, hasCredit := usageCreditTotal(resp)
		recordAttempt(acct.UID, usageDeltaFromResponse(resp), credit, hasCredit, attemptStarted)
		writeJSON(w, http.StatusOK, resp)
		st.status = http.StatusOK
		st.outcome = reqlog.OutcomeSuccess
		st.toks = completionTokens(resp)
		// Учет затрат (не потоковый): из агрегированного ответа usage получить credit и token Общее количество.
		if hasCredit {
			st.credit = credit
			st.hasCredit = true
			h.cfg.Pool.NoteModelCost(acct.UID, bareModel, credit, total)
		}
		return
	}
	// Проброс ошибки с терминала (error-passthrough）：Ошибки апстрима пробрасываются как есть, без нормализации в фиксированный текст.
	// Upstream вернул (*upstream.Error）→ error.message Установка**апстрим body Исходный текст**（code/msg/
	// requestId сохранить как есть).HTTP Код статуса по OpenAI Совместимое отображение категорий:ErrSoftRate → 429
	// （семантика rate limit, клиент должен ждать ретрай), остальное без изменений 503。Ошибка локального планировщика (нет доступных аккаунтов/
	// Джиттер транспортного уровня/Возвращено не апстримом lastErr）→ сохранить собственный текст no_healthy_account（Локальная ошибка
	// нет оригинала апстрима для проброса, не выдумывать).
	status := http.StatusServiceUnavailable
	code := "no_healthy_account"
	msg := "all accounts are temporarily unavailable, please retry later"
	// gateway_hint（Сквозная передача на краю): ошибки апстрима как Kind + Исходный текст + Определение формы запроса; класс локальной диспетчеризации
	// ошибка (без оригинала апстрима) фиксированная no_healthy_account hint。
	hint := upstream.NoHealthyAccountHint()
	var ue *upstream.Error
	if errors.As(lastErr, &ue) {
		hint = h.hintOf(ue.Kind, ue.Msg, bareModel, reqHasImage, ue)
		switch ue.Kind {
		case upstream.ErrSoftRate:
			status = http.StatusTooManyRequests
			code = "rate_limit_exceeded"
			msg = "rate limited: all accounts are cooling down, please wait a moment and try again"
		case upstream.ErrWafBlock:
			if h.wafIP.active() {
				// IP формулировка блокировки уровня (fail-fast Путь завершения): выход шлюза IP Получено WAF перехват,
				// Ротация уже стоп-лосс, после окна снимется автоматически. Ранний ретрай клиента бессмыслен (смена аккаунта без смены IP）；
				// при наличии оригинала апстрима приоритет у оригинала (ниже унифицировано).
				code = "waf_ip_blocked"
				msg = "waf ip-level block: upstream firewall is blocking the gateway IP, rotation stopped; retry after the block window expires"
			}
		}
		if s := strings.TrimSpace(ue.Msg); s != "" {
			// Приоритет оригинала апстрима: сквозная передача code/msg/requestId，Не конкатенировать локальный префикс.
			msg = s
		}
	}
	writeOpenAIErrorHint(w, status, code, msg, hint)
	st.status = status
	st.outcome = reqlog.OutcomeHTTPError
}

// promptTooLongMessage 11115 Прозрачная передача message：апстрим body Оригинал (включая реальный token Число/
// верхний предел/requestId，клиент разбирается сам); пусто body Fallback — читаемый короткий текст категории (без выдумывания оригинала).
func promptTooLongMessage(body string) string {
	if strings.TrimSpace(body) == "" {
		return "prompt is too long"
	}
	return body
}

// usageCreditTotal Брать из агрегированного ответа usage.credit и total_tokens（непотоковый вход реестра затрат).
// Отсутствует любое поле/Недопустимый → ok=false（не логировать).
func usageCreditTotal(resp map[string]any) (credit float64, total int, ok bool) {
	usage, _ := resp["usage"].(map[string]any)
	if usage == nil {
		return 0, 0, false
	}
	c, _ := usage["credit"].(float64)
	t, _ := usage["total_tokens"].(float64)
	if t <= 0 {
		return 0, 0, false
	}
	return c, int(t), true
}

// rotateBackoff Экспоненциальный бэкофф между ротациями + Джиттер (WAF 403 исправление P0-2）：№ i ошибка ротации
// （continue ожидание перед сменой номера) backoffAfter(i)（500ms·2^i Потолок 8s，±25% джиттер),
// ctx Отмена (обрыв соединения клиента/graceful shutdown) возврат false——Вызывающая сторона немедленно прерывает ротацию (клиент уже ушёл,
// повтор со сменой номера бессмыслен). Backoff — "пауза перед сменой номера» для проскальзывания окна rate-limit upstream; обычный запрос с одним номером
// （первый успех) не проходит через эту функцию, нулевая стоимость.
func rotateBackoff(i int, ctx context.Context) bool {
	d := backoffAfter(i)
	if d <= 0 {
		return ctx.Err() == nil
	}
	if !sleepCtx(ctx, d) {
		log.Printf("WARN: [server] rotate backoff aborted: ctx cancelled")
		return false
	}
	return true
}

// applyErrorPolicy применять кулдаун к аккаунту по классу ошибки/Отключено/Стратегия circuit breaker (финальный автомат состояний).
// kind единственная авторитетная классификация (из upstream.Classify / ChatStreamContext *Error конверт),
// Здесь больше не по исходному status Повторная проверка. Только при chatCompletions Вызов внутри цикла ротации: блокировка контента
// Немедленно 400 Возврат, остальные типы continue Смена аккаунта (continue ранее через rotateBackoff бэкофф).
//
// десять путей, каждый со своей функцией:
// - ErrHardCredit → CooldownUntilTomorrow4AM：немедленное жесткое охлаждение до следующего дня 04:00（ожидание восстановления чекина).
// - ErrSoftRate → Приоритет выравнивания wall-clock сброса апстрима (с "будет через … при "сбросе» 6004 идёт через освобождение на уровне модели,
// не 6004 идёт на уровне аккаунта, без экспоненциального накопления); только без времени сброса — ограниченный бэкофф. Длительность кулдауна приоритетно из
// Retry-After заголовок (uerr.RetryAfter，body источник формы заголовка помимо wall-clock текста).
// - ErrWafBlock → Мягкий кулдаун на уровне аккаунта:**Не Disable**——WAF 403 Да IP/Сигнал rate-limit по отпечатку,
// наказан — сразу уход, по истечении — самовосстановление. Приоритет длительности Retry-After заголовок; при отсутствии — по wafCooldownBase(60s)
// Запуск · softStreak экспонента, потолок soft_rate_max существующий CooldownSoftRate Ограниченный бэкофф.
// база после jitterDur Джиттер (защита от синхронного истечения кулдауна у множества аккаунтов и повторной кластеризации).
// - ErrNotFound → Cooldown(CoolSoft, notFoundCooldown Фиксированный 60s)：короткий кулдаун против лавины.
// - ErrSessionDead → Disable：session смерть, перманентная блокировка (требуется ручной перелогин).
// - ErrContentBlocked → Аккаунт не штрафуется;passthrough При первой встрече — ретрай с даунгрейдом, если всё равно блокируется — вернуть 400。
// - ErrBadParams → Аккаунт не штрафуется (аналог. ErrContentBlocked обращение), но ротация продолжается.
// - ErrPromptTooLong → 11115：проблема запроса, а не аккаунта. Нулевое действие (без кулдауна/БезОтключение/
// Не NoteError、не подавать серию поражений),chatCompletions уже напрямую прокинут оригинал без ротации.
// - ErrImageInvalid → формат изображения/Данные невалидны: проблема запроса, а не аккаунта (тот же body
// Смена любого номера даст ту же ошибку парсинга). Нулевое действие (без кулдауна/БезОтключение/Не NoteError、
// не подавать серию поражений),chatCompletions уже напрямую прокинут оригинал без ротации.
// - ErrModelBlocked → BlockModelBackoff：(Аккаунт, Модель) 11102 Обход отрицательного кэша.
// - ErrServer → NoteError：Подача в единый счётчик последовательных ошибок fails + накопленные ошибки errTotal，
// достигнуто breakerThreshold триггеритОтключение (экспоненциальный бэкофф).
// - прочее (default：ErrClient/ErrNone）→ Только смена аккаунта без штрафа (защита от лавины), без подачи на circuit breaker;ErrClient
// Дополнительно прокинуть счётчик поражений подряд (NoteFailures，issue #114）：неизвестно 4xx серия неудач N раз временного вывода из пула.
//
// body Только при ErrSoftRate/ErrAccountFault ветка для парсинга времени сброса/разделение;model для запроса
// передаваемое имя модели.uerr Да ChatStreamContext Возвращаемый классифицированный конверт (может содержать RetryAfter）；
// нулевое значение/в защитном пути — nil，длительность кулдауна откатывается к имеющемуся расчёту.
func (h *Handler) applyErrorPolicy(uid string, kind upstream.ErrKind, body, model string, uerr *upstream.Error) {
	switch kind {
	case upstream.ErrHardCredit:
		// 402 + ключевое слово баланса = исчерпание баллов: синхронный кулдаун до следующего дня 04:00（Задача чекина 09/21 точка восстановления),
		// Асинхронная проверка не требуется (избыточно). Немедленно сменить аккаунт.
		h.cfg.Pool.CooldownUntilTomorrow4AM(uid, "Недостаточно средств")
	case upstream.ErrSoftRate:
		// Единое выравнивание по времени сброса апстрима: если body содержит "будет через … сброс», независимо от бизнеса code Да
		// 6004 или 11140 rate-limiting и т.п. формы — точное охлаждение до указанного wall-clock, без экспоненциального накопления.
		// - уровень модели (6004）→ CooldownSoftForModel：Запись modelCooldowns[model]，освобождение при переключении модели.
		// - уровень аккаунта (не 6004）→ CooldownSoftRate：Запись уровня аккаунта until，Не даёт исключения для модели.
		modelRateLimited := upstream.IsModelRateLimit(body)
		if resetAt, ok := upstream.ParseRateReset(body); ok {
			if modelRateLimited {
				h.cfg.Pool.CooldownSoftForModel(uid, h.softCooldown(), resetAt, model, "6004 model rate limit")
				return
			}
			h.cfg.Pool.CooldownSoftRate(uid, h.softCooldown(), resetAt, "429 rate limit")
			return
		}
		// body без текста сброса, но с Retry-After Заголовок → охлаждение до этого момента (без экспоненциального накопления).
		// заголовок приоритетнее "ограниченного бэкоффа», но ниже body Сброс текста (текст апстрима — более авторитетная формулировка).
		if uerr != nil && uerr.RetryAfter > 0 {
			h.cfg.Pool.CooldownSoftRate(uid, h.softCooldown(), time.Now().Add(uerr.RetryAfter), "429 rate limit (retry-after)")
			if modelRateLimited {
				h.cfg.Pool.RecordModelRateLimitAudit(uid, model, "6004 model rate limit (reset unknown)")
			}
			return
		}
		// без времени сброса → bounded backoff на уровне аккаунта (soft_rate От базы,softStreak Удвоение, потолок
		// soft_rate_max；резервный пробинг уже на кулдауне не удваивается). База h.softCooldown()
		// （Приоритет hot-fix), изменение через админ-панель soft_rate вступает в силу немедленно.
		h.cfg.Pool.CooldownSoftRate(uid, h.softCooldown(), time.Time{}, "429 rate limit")
		if modelRateLimited {
			h.cfg.Pool.RecordModelRateLimitAudit(uid, model, "6004 model rate limit (reset unknown)")
		}
	case upstream.ErrWafBlock:
		// WAF 403（форма блокировки без бизнес-конверта). Повторное использование мягкого кулдауна CooldownSoftRate семейство: мощность
		// wafCooldownBase（60s，После джиттера оседает [45s,75s]）、softStreak Экспоненциальное повышение, с потолком
		// soft_rate_max、fallback-проба в кулдауне не удваивается — полностью наследует текущую семантику.
		// Retry-After приоритет заголовка (WAF Страница перехвата может содержать этот заголовок). Не Disable。
		if uerr != nil && uerr.RetryAfter > 0 {
			h.cfg.Pool.CooldownSoftRate(uid, jitterDur(wafCooldownBase), time.Now().Add(uerr.RetryAfter), "waf 403 block (retry-after)")
			return
		}
		h.cfg.Pool.CooldownSoftRate(uid, jitterDur(wafCooldownBase), time.Time{}, "waf 403 block")
	case upstream.ErrSessionDead:
		h.cfg.Pool.Disable(uid, "12153 session dead")
	case upstream.ErrNotFound:
		// 404 Короткий кулдаун (мягкий кулдаун), защита от лавины. Фиксирован notFoundCooldown，Не зависит от soft_rate Бэкофф:
		// спорадическое отсутствие пути — не сигнал rate limit, нельзя эскалировать как лимит.
		h.cfg.Pool.Cooldown(uid, pool.CoolSoft, notFoundCooldown, "upstream 404")
	case upstream.ErrAccountFault:
		// Авторизация на уровне аккаунта/Сбой квоты по msg разделение (метрики и Classify accountFaultMarkers совпадает):
		// - "request illegal"（code 11140）→ Уровень аккаунта**Блокировка авторизации**：Жёсткое отключение (Disable）。
		// - 14017（trial not activated）→ register Не завершено, дополнить register после возможно самовосстановление,
		// **сохранять мягкий кулдаун**（блокировка заставит пользователя дозаполнить register после всё равно недоступно).
		// без учета регистра (с Classify marker совпадение в той же размерности).
		if strings.Contains(strings.ToLower(body), "request illegal") {
			h.cfg.Pool.Disable(uid, "account banned by upstream (11140 request illegal), re-login required")
			return
		}
		h.cfg.Pool.Cooldown(uid, pool.CoolSoft, h.softCooldown(), "account fault (14017)")
	case upstream.ErrServer:
		// 5xx сбой апстрима:Classify Уже ≥500 Классифицируется как ErrServer，здесь инкрементировать счётчик circuit breaker (больше не вручную status>=500）。
		h.cfg.Pool.NoteError(uid)
	case upstream.ErrContentBlocked:
		// Блокировка контент-политикой (ложное срабатывание): проблема контента, не аккаунта, аккаунт не штрафуется (без кулдауна/Circuit Breaker/NoteError）。
		// passthrough режим определяется chatCompletions внутри — деградированный ретрай;custom В этом режиме сюда ветка не должна заходить.
	case upstream.ErrPromptTooLong:
		// 11115「prompt is too long」：Проблема запроса, а не аккаунта (тот же body Заменить любой
		// номера превысили лимит). Нулевое действие (без охлаждения/БезОтключение/Не NoteError，Совм. ErrContentBlocked условия),
		// chatCompletions Уже напрямую прокинут оригинал без ротации — ветка только для полноты документации.
	case upstream.ErrImageInvalid:
		// формат изображения/Данные невалидны: проблема запроса, а не аккаунта (тот же body смена любого аккаунта всё равно
		// получена та же ошибка парсинга). Никаких действий,chatCompletions Уже fail-fast сквозная передача.
	case upstream.ErrBadParams:
		// Ошибка парсинга тела запроса (400 + Unmarshal chat params failed / 11101）：отправляемое в upstream body
		// есть проблема (на стороне шлюза усечение больше не выполняется, всё — некорректные запросы клиента JSON）。смена аккаунта — всё равно 400，
		// аккаунт не штрафуется (без кулдауна/Circuit Breaker/NoteError，Совм. ErrContentBlocked обработка); но**все равно ротируется**
		// ——У разных аккаунтов могут быть разные права на модели, стоит сменить номер и попробовать снова.
	case upstream.ErrModelBlocked:
		// 11102「У данного бэкенда нет этой модели»:(Аккаунт, Модель) Обход негативного кэша. Повторное использование modelCooldowns механизм
		// （и 6004 тот же домен), сторона выбора номера healthyForModel для этого аккаунта автоматически обходить эту модель.
		// Немедленная смена аккаунта (в этом раунде continue），Кулдаун для этого аккаунта и модели, при следующем выборе — пропуск.
		h.cfg.Pool.BlockModelBackoff(uid, model, upstream.ModelBlockReason)
	default:
		// остальные (ErrClient/ErrNone）：только смена номера без штрафа (анти-лавина), без триггера circuit breaker.
		// ErrClient（неизвестно 4xx）подача счётчика серии поражений (issue #114）：непрерывно N раз сбой данной формы →
		// Временное выведение аккаунта из пула (NoteFailures при достижении порога — понижение веса), за раз/Единичный сбой не штрафуется (без ложных срабатываний).ErrNone
		// Попадание сюда — защитный путь (status>=400 но классификация успешна), при неясной семантике не подавать.
		if kind == upstream.ErrClient {
			h.cfg.Pool.NoteFailures(uid)
		}
	}
}

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

func writeJSON(w http.ResponseWriter, status int, v any) {
	raw, _ := json.Marshal(v)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(raw)
}

func writeOpenAIError(w http.ResponseWriter, status int, code, msg string) {
	writeJSON(w, status, map[string]any{
		"error": map[string]any{
			"message": msg,
			"type": "api_error",
			"code": code,
		},
	})
}

// wafCooldownBase WAF 403 база мягкого кулдауна (рекомендуется 60s от; джиттер ±25% после отката [45s,75s]，
// фактический вход в CooldownSoftRate затем по softStreak экспонента, потолок soft_rate_max）。
// и SoftCooldown Причина разделения трафика:WAF 403 Да IP/частотный контроль по отпечатку, соотношение сигналов 429「лимит на уровне аккаунта» — мягкий
// （сам аккаунт здоров), но чем 404 повтор (с прилипанием вызовет каскад);60s быстрого бэкоффа уровня достаточно, чтобы окно rate-limit
// пропуск. джиттер переиспользуется backoff.go jitterDur（единственный источник).
const wafCooldownBase = 60 * time.Second

// writeOpenAIErrorHint Совм. writeOpenAIError，дополнительно в error Привязка к объекту
// error.gateway_hint（hint при пустой строке поле не передавать — непокрытые кейсы не выдумывать).
// message по-прежнемуПрозрачная передача оригинала апстрима (hint только параллельное дополнение, без замены/Обёртка message）。
func writeOpenAIErrorHint(w http.ResponseWriter, status int, code, msg, hint string) {
	if hint == "" {
		writeOpenAIError(w, status, code, msg)
		return
	}
	writeJSON(w, status, map[string]any{
		"error": map[string]any{
			"message": msg,
			"type": "api_error",
			"code": code,
			"gateway_hint": hint,
		},
	})
}

// hasImagePart сообщить, содержит ли тело chat-запроса мультимодальность image_url part（OpenAI Совместимая форма
// messages[].content[] {type:"image_url"}）。битый/Все остальные формы false（hint Боковой
// Лучше меньше, да лучше: если не удалось определить наличие изображения, не выдавать указание "модель не поддерживает изображения»).
func hasImagePart(body []byte) bool {
	var peek struct {
		Messages []struct {
			Content []struct {
				Type string `json:"type"`
			} `json:"content"`
		} `json:"messages"`
	}
	if json.Unmarshal(body, &peek) != nil {
		return false
	}
	for _, m := range peek.Messages {
		for _, p := range m.Content {
			if p.Type == "image_url" {
				return true
			}
		}
	}
	return false
}

// hintContext сборка chatCompletions gateway_hint Контекст решения: запрос голого имени модели +
// Наличие изображения + Каталог моделей supports_images заявление (каталог не содержит → ModelInCatalog=false，
// БезПроверка "не поддерживается» для защиты от ложного срабатывания при отсутствии данных). Вызывается только на ветке ошибки (успешные запросы — ноль оверхеда).
//
// Запрос каталога читает только существующий снапшот кэша (cachedModelsSnapshot），**Не инициировать подтяжку апстрима**：Путь ошибки
// Добавить один раз FetchModels Сетевой вызов и замедляет ответ с ошибкой, и загрязняет семантику вызова апстрима (при шторме ошибок усиливает
// объем запросов — и WAF IP fail-fast противоречит философии "не увеличивать объем запросов»). Холодный кэш (последний 10min Не
// подтянуто)→ ModelInCatalog=false，11133 откат к нейтральному hint（лучше меньше, да лучше, не выдумывать факты о возможностях).
func (h *Handler) hintContext(bareModel string, hasImage bool) upstream.HintContext {
	ctx := upstream.HintContext{Model: bareModel, HasImage: hasImage}
	if bareModel == "" {
		return ctx
	}
	for _, mi := range cachedModelsSnapshot() {
		if mi.ID == bareModel {
			ctx.ModelInCatalog = true
			ctx.ModelSupportsImages = mi.SupportsImages
			return ctx
		}
	}
	return ctx
}

// hintOf унифицированная сквозная передача конечной ошибки hint Вход:kind + Исходный текст upstream + контекст запроса →
// gateway_hint Текст (upstream.GatewayHint единый источник истины).uerr для nil откат при
// body Решение по исходному тексту (защитный путь).transport ошибка уровня (lastErr не *upstream.Error И
// Апстрим не ответил body）→ отсутствует hint（не выдумывать).
func (h *Handler) hintOf(kind upstream.ErrKind, body, bareModel string, hasImage bool, uerr *upstream.Error) string {
	msg := body
	if uerr != nil && uerr.Msg != "" {
		msg = uerr.Msg
	}
	return upstream.GatewayHint(kind, msg, h.hintContext(bareModel, hasImage))
}
