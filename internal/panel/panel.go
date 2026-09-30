// Package panel встроенный Web Админ-панель: обзор пула аккаунтов, обслуживание одного аккаунта (разморозка/Отключено/check-in/
// Обновить баланс/удаление), внутри браузера OAuth добавление аккаунта (горячая загрузка в пул без рестарта), ручная пакетная
// check-in/keepalive и кольцевой буфер логов выполнения (зеркало log Пакет и chat лог таблицы).
//
// Ограничения проектирования:
// - Фронтенд go:embed Один файл (index.html），Без внешних зависимостей сборки, поставляется вместе с бинарём;
// - шлюз с переиспользованием аутентификации api_key（Bearer），и /v1/* Единая метрика;api_key пусто = Без аутентификации
// （только локально/использование в приватной сети). Панель HTML Сам по себе без секретов, доступна анонимная загрузка, ключ выдается только /panel/api/*；
// - Не переопределять семантику существующего пула: все ops-операции применяются к pool уже есть вход (Revive/Disable/Remove...），
// Добавление аккаунта через auth.SaveAtomic + pool.Add，После рестарта с auths/ Каталоги естественно выровнены.
package panel

import (
	"encoding/json"
	"log"
	"net/http"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/httpauth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/livecfg"
	"github.com/linguo2625469/workbuddy2api-panel/internal/pool"
	"github.com/linguo2625469/workbuddy2api-panel/internal/reqlog"
	"github.com/linguo2625469/workbuddy2api-panel/internal/scheduler"
	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
	"github.com/linguo2625469/workbuddy2api-panel/internal/usage"
)

// Config Зависимость панели (main сборка/инъекция).
type Config struct {
	Pool *pool.Pool
	Upstream *upstream.Client
	Scheduler *scheduler.Scheduler // Ручной триггер чекина/keep-alive;nil при этом соответствующий интерфейс возвращает 501
	AuthDir string // OAuth Каталог сохранения учётных данных после входа
	APIKey string // пустой = Без аутентификации (семантика как у основного сервиса); с Live При одновременном указании Live Приоритет
	RedisMode string // "upstash« / »noop"，только наблюдаемаяПрозрачная передача
	Version string // Версия панели (для отображения)

	// Live изменяемая в рантайме конфигурация (изменение онлайн применяется немедленно).
	Live *livecfg.Holder

	// ConfigPath config.json Путь и загрузчик (для чтения/записи страницы конфигурации).
	// LoadConfig Возвращает распарсенный объект конфигурации (отображение на фронте/для проверки, конкретный тип определяется main определяется инжектированным замыканием);
	// nil страница конфигурации возвращает 501。
	ConfigPath string
	LoadConfig func() (any, error)
	// SaveConfig Валидировать и сохранить конфигурацию на диск, вернуть список полей, требующих перезапуска; затем main Инжектированный
	// ApplyConfig замыкание завершает горячее применение (параметры пула/Планирование/ключ/десенсибилизация).error конфиг не пишется на диск.
	SaveConfig func(raw []byte) (restartRequired []string, err error)

	// StickyCount Возвращает кол-во привязок sticky-сессий;nil отчет по времени 0。
	StickyCount func() int

	// Usage рекордер расхода по запросам (nil = Ответ API квоты/расхода 501）。
	Usage *usage.Recorder
	// RequestLog Метрики запросов и архивация (nil = Соответствующий интерфейс возвращает 501）。
	RequestLog *reqlog.Recorder

	// ProbeFile Файл результатов зондирования лимита вывода модели (scripts/probe_max_tokens.py --panel-out
	// запись; пусто или файл отсутствует = model_probes эндпоинт вернул пустое множество, на панели не отображаются метки реальных измерений).
	// Только для отображения: шлюз не парсит и не использует содержимое для маршрутизации/Решение об исходящем запросе.
	ProbeFile string
}

// Panel Панель управления handler。способ монтирования: внешний mux Handle("/panel/", panel)，
// текущий mux pattern все с /panel Префикс (внешний слой не снимает префикс).
type Panel struct {
	cfg Config
	mux *http.ServeMux
	started time.Time
	logs *Ring

	// logins в процессе OAuth Сессия авторизации устройства (state → информация о сессии).
	// poll Успех или таймаут (loginTTL）после удаление; процесс панели постоянно живет, емкость естественно ограничена.
	loginMu sync.Mutex
	logins map[string]loginSession

	// taskMu/taskLocks задач в один клик per-account Мьютекс: действия задач одного аккаунта
	// （одна задача / полный объем) одновременно только один запуск. Повторный клик сразу возвращает 409"всё ещё выполняется"，
	// а не гонять параллельно дважды, тратя запросы к апстриму (хотя действие идемпотентно,expert система каждый проход содержит 8 реальных диалогов).
	// Между разными аккаунтами не взаимоисключающе (параллель сохраняется).TryLock семантика, записи блокировок постоянны (кол-во аккаунтов ограничено).
	taskMu sync.Mutex
	taskLocks map[string]*sync.Mutex

	// Очередь исполнения центра задач (taskcenter.go）。
	queueOnce sync.Once
	q *queueState
}

// tryLockAccount Попытка захвата блокировки задачи аккаунта; если уже выполняется — возврат false。
func (p *Panel) tryLockAccount(uid string) bool {
	p.taskMu.Lock()
	if p.taskLocks == nil {
		p.taskLocks = make(map[string]*sync.Mutex)
	}
	mu := p.taskLocks[uid]
	if mu == nil {
		mu = &sync.Mutex{}
		p.taskLocks[uid] = mu
	}
	p.taskMu.Unlock()
	return mu.TryLock()
}

// unlockAccount освободить лок задачи аккаунта (с tryLockAccount сопряжение).
func (p *Panel) unlockAccount(uid string) {
	p.taskMu.Lock()
	mu := p.taskLocks[uid]
	p.taskMu.Unlock()
	if mu != nil {
		mu.Unlock()
	}
}

// loginTTL Авторизация URL максимальный срок действия: просроченный state прямая утилизация,
// предотвратить"открыл окно добавления аккаунта и ушёл"сессия зависает навсегда.
const loginTTL = 15 * time.Minute

// loginSession в процессе OAuth Сессия: момент создания + realm（cn/global，для сохранения на диск и переключения эндпоинта).
type loginSession struct {
	created time.Time
	realm string // "cn« / »global"，по умолчанию cn
}

// New Панель сборки.
func New(cfg Config) *Panel {
	if cfg.RedisMode == "" {
		cfg.RedisMode = "noop"
	}
	p := &Panel{
		cfg: cfg,
		mux: http.NewServeMux(),
		started: time.Now(),
		logs: NewRing(500),
		logins: map[string]loginSession{},
	}
	p.routes()
	return p
}

// Logs Возвращает кольцевой буфер логов (main Через MultiWriter Образ log и chat входят табличные логи).
func (p *Panel) Logs() *Ring { return p.logs }

func (p *Panel) routes() {
	p.mux.HandleFunc("GET /panel/{$}", p.index)
	p.mux.HandleFunc("GET /panel/app.js", p.appScript)
	p.mux.HandleFunc("GET /panel/api/overview", p.withAuth(p.overview))
	p.mux.HandleFunc("GET /panel/api/logs", p.withAuth(p.logsHandler))
	p.mux.HandleFunc("GET /panel/api/request_metrics", p.withAuth(p.requestMetrics))
	p.mux.HandleFunc("GET /panel/api/request_logs", p.withAuth(p.requestLogs))
	p.mux.HandleFunc("GET /panel/api/models", p.withAuth(p.models))
	p.mux.HandleFunc("POST /panel/api/login/start", p.withAuth(p.loginStart))
	p.mux.HandleFunc("GET /panel/api/login/poll", p.withAuth(p.loginPoll))
	p.mux.HandleFunc("GET /panel/api/login/regions", p.withAuth(p.loginRegions))
	p.mux.HandleFunc("POST /panel/api/import/cockpit", p.withAuth(p.importCockpit))
	p.mux.HandleFunc("POST /panel/api/accounts/{uid}/revive", p.withAuth(p.accountRevive))
	p.mux.HandleFunc("POST /panel/api/accounts/{uid}/disable", p.withAuth(p.accountDisable))
	p.mux.HandleFunc("POST /panel/api/accounts/{uid}/checkin", p.withAuth(p.accountCheckin))
	p.mux.HandleFunc("POST /panel/api/accounts/{uid}/balance", p.withAuth(p.accountBalance))
	p.mux.HandleFunc("POST /panel/api/accounts/{uid}/remove", p.withAuth(p.accountRemove))
	p.mux.HandleFunc("GET /panel/api/accounts/{uid}/tasks", p.withAuth(p.accountTasks))
	p.mux.HandleFunc("POST /panel/api/accounts/{uid}/tasks/accept", p.withAuth(p.accountTaskAccept))
	p.mux.HandleFunc("POST /panel/api/accounts/{uid}/tasks/accept_all", p.withAuth(p.taskAcceptAll))
	p.mux.HandleFunc("POST /panel/api/accounts/{uid}/tasks/claim", p.withAuth(p.accountTaskClaim))
	p.mux.HandleFunc("POST /panel/api/accounts/{uid}/tasks/auto", p.withAuth(p.accountTaskAuto))
	p.mux.HandleFunc("POST /panel/api/accounts/{uid}/tasks/auto_all", p.withAuth(p.accountTaskAutoAll))
	p.mux.HandleFunc("POST /panel/api/tasks/scan_all", p.withAuth(p.tasksScanAll))
	p.mux.HandleFunc("POST /panel/api/tasks/run_queue", p.withAuth(p.tasksRunQueue))
	p.mux.HandleFunc("GET /panel/api/tasks/queue", p.withAuth(p.tasksQueueStatus))
	p.mux.HandleFunc("GET /panel/api/school/vouchers", p.withAuth(p.schoolVouchers))
	p.mux.HandleFunc("POST /panel/api/checkin_all", p.withAuth(p.checkinAll))
	p.mux.HandleFunc("POST /panel/api/travel_all", p.withAuth(p.travelAll))
	p.mux.HandleFunc("POST /panel/api/activity_all", p.withAuth(p.activityAll))
	p.mux.HandleFunc("POST /panel/api/keepalive_all", p.withAuth(p.keepaliveAll))
	p.mux.HandleFunc("POST /panel/api/balance_all", p.withAuth(p.balanceAll))
	p.mux.HandleFunc("GET /panel/api/packages", p.withAuth(p.packages))
	p.mux.HandleFunc("GET /panel/api/usage", p.withAuth(p.usage))
	p.mux.HandleFunc("POST /panel/api/usage/save", p.withAuth(p.usageSave))
	p.mux.HandleFunc("GET /panel/api/model_probes", p.withAuth(p.modelProbes))
	p.mux.HandleFunc("GET /panel/api/config", p.withAuth(p.getConfig))
	p.mux.HandleFunc("POST /panel/api/config", p.withAuth(p.saveConfig))
}

// ServeHTTP Единая точка входа: сначала записать безопасные заголовки ответа, затем диспетчеризация, гарантируя страницы, статические ресурсы,API
// и 401 Все ответы с ошибкой содержат (API может быть также открыто напрямую в браузере).
func (p *Panel) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	setSecurityHeaders(w)
	p.mux.ServeHTTP(w, r)
}

// withAuth и server в метрике того же пакета Bearer аутентификация (через httpauth сравнение за константное время);
// api_key При пустом — пропуск. Ключ через livecfg Чтение снапшота: в панели изменено api_key，следующий запрос
// Применять новое значение сразу (без перезапуска).
func (p *Panel) withAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !httpauth.VerifyBearer(r, p.apiKey()) {
			writeErr(w, http.StatusUnauthorized, "invalid_api_key")
			return
		}
		next(w, r)
	}
}

// apiKey Текущий действующий ключ (Live Приоритет, откат к статическому полю).
func (p *Panel) apiKey() string {
	if p.cfg.Live != nil {
		return p.cfg.Live.Load().APIKey
	}
	return p.cfg.APIKey
}

// expiringSoonWindow Возвращает текущее активное окно быстро истекающей маршрутизации планировщика; при отсутствии планировщика на тестовой панели возвращает 0。
func (p *Panel) expiringSoonWindow() time.Duration {
	if p.cfg.Scheduler == nil {
		return 0
	}
	return p.cfg.Scheduler.ExpiringSoonWindow()
}

// ---------------------------------------------------------------------------
// Интерфейс только для чтения
// ---------------------------------------------------------------------------

// overview Обзор: счётчики пула + Состояние каждого аккаунта + Метаинформация панели.
func (p *Panel) overview(w http.ResponseWriter, r *http.Request) {
	total, healthy, cooling, disabled, inFlightFull := p.cfg.Pool.CountsDetailed()
	sticky := 0
	if p.cfg.StickyCount != nil {
		sticky = p.cfg.StickyCount()
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"version": p.cfg.Version,
		"uptime_sec": int(time.Since(p.started).Seconds()),
		"auth_required": p.apiKey() != "",
		"redis_mode": p.cfg.RedisMode,
		"sticky_sessions": sticky,
		"total": total,
		"healthy": healthy,
		"cooling": cooling,
		"disabled": disabled,
		"in_flight_full": inFlightFull,
		"accounts": p.cfg.Pool.List(),
	})
}

// logsHandler возвращает снапшот кольцевого буфера логов (по возрастанию времени, с меткой канала chat/task/sys）。
func (p *Panel) logsHandler(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"entries": p.logs.Snapshot()})
}

// requestMetrics возвращает внутрипроцессные метрики запросов, последние 100 записей и статуса архивации.
func (p *Panel) requestMetrics(w http.ResponseWriter, r *http.Request) {
	if p.cfg.RequestLog == nil {
		writeErr(w, http.StatusNotImplemented, "request logger not available")
		return
	}
	writeJSON(w, http.StatusOK, p.cfg.RequestLog.Snapshot())
}

// requestLogs Из JSONL архив читает последние запросы;limit По умолчанию 200、Максимум 1000。
// Поддержка по outcome/account/model/client_ip/user_agent Фильтр (строковые поля — совпадение по вхождению)
// и from/to Временной интервал (закрытый,unix с или RFC3339）——Фильтр "Журнал выполнения» панели,
// запрос источника и "сегодня / пользовательский интервал» — всё идёт сюда.
func (p *Panel) requestLogs(w http.ResponseWriter, r *http.Request) {
	if p.cfg.RequestLog == nil {
		writeErr(w, http.StatusNotImplemented, "request logger not available")
		return
	}
	limit := 200
	if v := r.URL.Query().Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			limit = n
		}
	}
	if limit > 1000 {
		limit = 1000
	}
	q := r.URL.Query()
	rows, err := p.cfg.RequestLog.ReadArchive(limit, reqlog.Filter{
		Outcome: q.Get("outcome"),
		Account: q.Get("account"),
		Model: q.Get("model"),
		ClientIP: q.Get("client_ip"),
		UserAgent: q.Get("user_agent"),
		From: parseTimeParam(q.Get("from")),
		To: parseTimeParam(q.Get("to")),
	})
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	// Пустой результат возвр. []（а не JSON null）：фронтенд ... null и"Архивное закрытие"Смешивание уведет в неверную ветку,
	// Отображать как последний запрос, не прошедший фильтр.
	if rows == nil {
		rows = []reqlog.Event{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"entries": rows, "limit": limit})
}

// models запрос в реальном времени списка upstream-моделей и reasoning Фактический тариф (прямое обращение к апстриму, без чтения слоя маршрутизации 1h кэш):
// Ответ"Какие уровни reasoning поддерживает данная модель"。попутное обновление client effort кэш capability downgrade.
// и /v1/models Двухдоменный вывод в единой метрике:CN доменная модель + "cn:« Префикс,global домен плюс "global:" Префикс
// （gateway протокол маршрутизации, отображаемый на фронтенде id это полный [параметр] для заполнения при вызове model значения).
// независимый пробинг и отказоустойчивость по доменам: если в домене нет доступных аккаунтов — пропуск всего домена; ошибка только когда оба домена пусты
// （вернуть детали ошибки 502，Ни один аккаунт не ответил 503）。
func (p *Panel) models(w http.ResponseWriter, r *http.Request) {
	out := make([]map[string]any, 0)
	var fetchErrs []string

	// CN Домен: есть доступные CN проверять только аккаунт (ранее безусловно Pool.Pick()+FetchModels——Выбрано global
	// при аккаунте ставить CN Эндпоинт неизбежно падает, в смешанном пуле — спорадически 502，Чистый global пул гарантированно упадет).
	if uids := p.cfg.Pool.AvailableUIDsForRealm("cn"); len(uids) > 0 {
		if acct := p.cfg.Pool.AuthByUID(uids[0]); acct != nil {
			infos, err := p.cfg.Upstream.FetchModels(acct)
			if err != nil {
				fetchErrs = append(fetchErrs, "cn: "+err.Error())
			} else {
				for _, mi := range infos {
					out = append(out, panelModelEntry("cn", mi, mi.Efforts, mi.DefaultEffort, p.cfg.Upstream.HTTP))
				}
			}
		}
	}

	// global Домен: свитч маршрутизации вкл. и есть доступные global запрос по аккаунту только тогда (отдельный эндпоинт каталога,FetchGlobalModelInfos；
	// Upstream.GlobalEnabled это один шлюз на стороне детекции, с main Собранный config global.enabled совпадает).
	if p.cfg.Upstream.GlobalEnabled {
		if uids := p.cfg.Pool.AvailableUIDsForRealm("global"); len(uids) > 0 {
			if acct := p.cfg.Pool.AuthByUID(uids[0]); acct != nil {
				infos := p.cfg.Upstream.FetchGlobalModelInfos(acct)
				if len(infos) == 0 {
					fetchErrs = append(fetchErrs, "global: апстрим не вернул доступных моделей")
				} else {
					efforts, defaults := p.cfg.Upstream.GlobalEffortSnapshot()
					for _, mi := range infos {
						out = append(out, panelModelEntry("global", mi, efforts[mi.ID], defaults[mi.ID], p.cfg.Upstream.HTTP))
					}
				}
			}
		}
	}

	if len(out) == 0 {
		if len(fetchErrs) > 0 {
			writeErr(w, http.StatusBadGateway, "fetch models: "+strings.Join(fetchErrs, "; "))
			return
		}
		writeErr(w, http.StatusServiceUnavailable, "Нет доступных аккаунтов: сначала добавьте аккаунт в панели, затем запросите")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "models": out})
}

// panelModelEntry сборка одной записи модели (общая для двух доменов):id Лента realm Префикс (значение вызова = отображаемое значение),
// context_length / max_output_tokens Идти по 4-уровневой цепочке поиска,effort Тариф по realm Выборка по домену
// EffortListing（Удалённый авторитетный источник ∪ статическая fallback-таблица) — с /v1/models Единая метрика, расхождение сторон устранено.
func panelModelEntry(realm string, mi upstream.ModelInfo, remoteEfforts []string, remoteDefault string, httpc *http.Client) map[string]any {
	entry := map[string]any{
		"id": realm + ":" + mi.ID,
		"name": mi.Name,
		"default_effort": mi.DefaultEffort,
		"supported_efforts": mi.Efforts,
		"can_disable_thinking": mi.CanDisableThinking,
		"supports_reasoning": mi.SupportsReasoning,
		"supports_images": mi.SupportsImages,
		"credits": mi.Credits,
		"description": mi.Description,
		"tags": mi.Tags,
		"vendor": mi.Vendor,
		"is_default": mi.IsDefault,
		"supports_tool_call": mi.SupportsToolCall,
		"only_reasoning": mi.OnlyReasoning,
		"reasoning_effort": mi.ReasoningEffort,
		"reasoning_summary": mi.ReasoningSummary,
	}
	// Ограниченное по времени предложение (modelPromotions）：credits котировка,promo_* — текущая действующая скидка
	//（WorkBuddy клиент отображает именно эту действующую цену). Фронтенд на этом основании показывает "действующая цена+Тег+зачёркнутая цена».
	if mi.PromoFactor != nil {
		entry["promo_factor"] = *mi.PromoFactor
		entry["promo_credits"] = mi.PromoCredits
	}
	if mi.PromoLabel != "" {
		entry["promo_label"] = mi.PromoLabel
	}
	if mi.PromoNote != "" {
		entry["promo_note"] = mi.PromoNote
	}
	if mi.MaxAllowedSize > 0 {
		entry["max_allowed_size"] = mi.MaxAllowedSize
	}
	entry["context_length"] = upstream.ContextWindowListingV4(mi.ID, mi.ContextWindow, httpc)
	if mo, ok := upstream.MaxOutputTokensListingV4(mi.ID, mi.MaxTokens, httpc); ok {
		entry["max_output_tokens"] = mo
	}
	if efforts, def := upstream.EffortListing(realm, mi.ID, remoteEfforts, remoteDefault); efforts != nil {
		entry["supported_efforts"] = efforts
		if def != "" {
			entry["default_effort"] = def
		}
	}
	return entry
}

// modelProbes вернуть результат пробы лимита вывода модели (scripts/probe_max_tokens.py --panel-out
// записываемый файл контракта), для пометки рисков фронтендом в колонке фактических замеров "модель и тариф».
//
// Граница проектирования: чистый read-only прокси — файл отсутствует/Не настроено — возврат пустого множества (панель деградирует без меток, совместимо с историческим поведением
// совпадает), сам шлюз не парсит семантику полей и не принимает на её основе решений о маршрутизации или исходящих запросах; после изменения лимитов выше по цепочке
// повторный прогон инструмента / следующий запрос обновит, перезапуск шлюза не требуется.
func (p *Panel) modelProbes(w http.ResponseWriter, r *http.Request) {
	out := map[string]any{"probes": map[string]json.RawMessage{}, "exists": false}
	if p.cfg.ProbeFile == "" {
		writeJSON(w, http.StatusOK, out)
		return
	}
	raw, err := os.ReadFile(p.cfg.ProbeFile)
	if err != nil {
		if os.IsNotExist(err) {
			writeJSON(w, http.StatusOK, out)
			return
		}
		writeErr(w, http.StatusInternalServerError, "read probes: "+err.Error())
		return
	}
	var f struct {
		Version int `json:"version"`
		Probes map[string]json.RawMessage `json:"probes"`
	}
	if err := json.Unmarshal(raw, &f); err != nil {
		writeErr(w, http.StatusBadGateway, "parse probes: "+err.Error())
		return
	}
	if f.Probes == nil {
		f.Probes = map[string]json.RawMessage{}
	}
	out["probes"] = f.Probes
	out["exists"] = true
	if fi, err := os.Stat(p.cfg.ProbeFile); err == nil {
		out["updated_at"] = fi.ModTime().Format(time.RFC3339)
	}
	writeJSON(w, http.StatusOK, out)
}

// ---------------------------------------------------------------------------
// эксплуатация аккаунта
// ---------------------------------------------------------------------------

// accountRevive Ручное восстановление: снять блокировку + Охлаждение + аварийный размыкатель (с точки зрения эксплуатации — безусловное восстановление).
func (p *Panel) accountRevive(w http.ResponseWriter, r *http.Request) {
	uid := r.PathValue("uid")
	if _, ok := p.cfg.Pool.Status(uid); !ok {
		writeErr(w, http.StatusNotFound, "account not found")
		return
	}
	p.cfg.Pool.Revive(uid)
	log.Printf("panel: revive uid=%s（ручной сброс блокировки/Охлаждение/аварийный размыкатель)", uid)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// accountDisable ручное отключение (исключён из выбора, требуется панель revive или восстановление после повторного входа).
func (p *Panel) accountDisable(w http.ResponseWriter, r *http.Request) {
	uid := r.PathValue("uid")
	if _, ok := p.cfg.Pool.Status(uid); !ok {
		writeErr(w, http.StatusNotFound, "account not found")
		return
	}
	p.cfg.Pool.Disable(uid, "manual disable (panel)")
	log.Printf("panel: disable uid=%s（ручное отключение)", uid)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// accountCheckin чекин по одному номеру:DailyCheckin + Разморозка запроса баланса (бизнес-ошибки вроде уже отмечено не блокируют обновление баланса),
// и scheduler.RunCheckinNow семантика одиночного номера консистентна.
func (p *Panel) accountCheckin(w http.ResponseWriter, r *http.Request) {
	uid := r.PathValue("uid")
	a := p.cfg.Pool.AuthByUID(uid)
	if a == nil {
		writeErr(w, http.StatusNotFound, "account not found")
		return
	}
	checkinMsg := ""
	checkinDone := false
	if err := p.cfg.Upstream.DailyCheckin(a); err != nil {
		checkinMsg = err.Error() // "Сегодня уже отмечено"прочие бизнес-ошибки — проверка баланса штатно
		// Идемпотентный отказ — тоже "сегодня уже отмечено», после флага кнопка показывает "Отмечено».
		if upstream.IsAlreadyCheckin(err) {
			p.cfg.Pool.NoteCheckinDone(uid)
			checkinDone = true
		}
	} else {
		p.cfg.Pool.NoteCheckinDone(uid)
		checkinDone = true
	}
	resp := map[string]any{"ok": true, "checkin_done": checkinDone}
	if checkinMsg != "" {
		resp["checkin_message"] = checkinMsg
	}
	remain, total, expiring, earliestAt, earliestRemaining, err := p.cfg.Upstream.UserResourceDetailedWithExpiry(a, p.expiringSoonWindow())
	if err != nil {
		resp["balance_error"] = err.Error()
		writeJSON(w, http.StatusOK, resp)
		return
	}
	p.cfg.Pool.ReenableIfCredits(uid, remain, total)
	p.cfg.Pool.SetCreditsDetailed(uid, remain, total, expiring, earliestAt, earliestRemaining)
	resp["credits"] = remain
	resp["credits_total"] = total
	log.Printf("panel: checkin uid=%s msg=%q credits=%d/%d", uid, checkinMsg, remain, total)
	writeJSON(w, http.StatusOK, resp)
}

// accountBalance обновление баланса одного номера: обновить снапшот баланса и срока, не затрагивая состояние кулдауна.
func (p *Panel) accountBalance(w http.ResponseWriter, r *http.Request) {
	uid := r.PathValue("uid")
	a := p.cfg.Pool.AuthByUID(uid)
	if a == nil {
		writeErr(w, http.StatusNotFound, "account not found")
		return
	}
	remain, total, expiring, earliestAt, earliestRemaining, err := p.cfg.Upstream.UserResourceDetailedWithExpiry(a, p.expiringSoonWindow())
	if err != nil {
		writeErr(w, http.StatusBadGateway, "user resource: "+err.Error())
		return
	}
	p.cfg.Pool.SetCreditsDetailed(uid, remain, total, expiring, earliestAt, earliestRemaining)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "credits": remain, "credits_total": total})
}

// accountRemove Удаление аккаунта: сначала исключить из пула (немедленная запись на диск state），затем удалить auth файл.
func (p *Panel) accountRemove(w http.ResponseWriter, r *http.Request) {
	uid := r.PathValue("uid")
	a := p.cfg.Pool.Remove(uid)
	if a == nil {
		writeErr(w, http.StatusNotFound, "account not found")
		return
	}
	fileMsg := ""
	if a.FilePath != "" {
		if err := os.Remove(a.FilePath); err != nil && !os.IsNotExist(err) {
			fileMsg = err.Error()
		}
	}
	if fileMsg != "" {
		log.Printf("panel: remove uid=%s（auth Ошибка удаления файла: %s）", uid, fileMsg)
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "file_error": fileMsg})
		return
	}
	log.Printf("panel: remove uid=%s（выведен из пула и файл учетных данных удален)", uid)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// ---------------------------------------------------------------------------
// Пакетные задачи
// ---------------------------------------------------------------------------

// checkinAll Ручной запуск полной регистрации (асинхронно, прогресс в зоне логов/изменение статуса аккаунта).
func (p *Panel) checkinAll(w http.ResponseWriter, r *http.Request) {
	if p.cfg.Scheduler == nil {
		writeErr(w, http.StatusNotImplemented, "scheduler not available")
		return
	}
	go p.cfg.Scheduler.RunCheckinNow()
	log.Printf("panel: ручной полный check-in уже запущен (вкл. кошачье путешествие)")
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "started": true})
}

// travelAll ручной запуск полной проверки путешествий котиков (асинхронно).
func (p *Panel) travelAll(w http.ResponseWriter, r *http.Request) {
	if p.cfg.Scheduler == nil {
		writeErr(w, http.StatusNotImplemented, "scheduler not available")
		return
	}
	go p.cfg.Scheduler.RunTravelNow()
	log.Printf("panel: ручной полный обход-проверка уже запущен")
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "started": true})
}

// activityAll Ручной триггер полного отчёта активности (асинхронно; подсветить серию входов + предусловие разблокировки и принятия).
func (p *Panel) activityAll(w http.ResponseWriter, r *http.Request) {
	if p.cfg.Scheduler == nil {
		writeErr(w, http.StatusNotImplemented, "scheduler not available")
		return
	}
	go p.cfg.Scheduler.RunActivityNow()
	log.Printf("panel: Ручной полный отчёт об активности запущен")
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "started": true})
}

// keepaliveAll ручной триггер полного объема token keepalive (асинхронное выполнение).
func (p *Panel) keepaliveAll(w http.ResponseWriter, r *http.Request) {
	if p.cfg.Scheduler == nil {
		writeErr(w, http.StatusNotImplemented, "scheduler not available")
		return
	}
	go p.cfg.Scheduler.RunKeepaliveNow()
	log.Printf("panel: Ручной полный keepalive уже запущен")
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "started": true})
}

// balanceAll ручное полное обновление баланса: параллельный опрос апстрима, запись обратно в пул credits（включая семантику разморозки),
// Возврат после завершения — панель сразу подтягивает overview — это актуальное значение. Аккаунтов мало (единицы),
// Синхронное ожидание (лимит ограничен коротким RPC ограничение таймаута) чем"слепое обновление после триггера"Более предсказуемый опыт.
func (p *Panel) balanceAll(w http.ResponseWriter, r *http.Request) {
	if p.cfg.Scheduler == nil {
		writeErr(w, http.StatusNotImplemented, "scheduler not available")
		return
	}
	p.cfg.Scheduler.RunBalanceRefreshNow()
	log.Printf("panel: Ручное полное обновление баланса завершено")
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "accounts": p.cfg.Pool.List()})
}

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

// usage Возвращает агрегацию расхода по запросам. Окно статистики — 3 варианта:
// - from/to（unix с): явный интервал для "сегодня» и "пользовательский» — интервал формируется браузером по
// Рассчитать в локальном часовом поясе перед отправкой, иначе "сегодня» будет рассчитано неверно при расхождении часовых поясов сервера и браузера;
// - hours：Скользящее окно (по умолчанию 72，Верхний лимит 1440=60 дн.), сводка по карточкам/по домену/По аккаунту/по модели/
// Последовательность**все**статистика по данному окну; явно hours=0 означает всю историю (включая 90 дневной бакет, свёрнутый N дней назад);
// - ничего не выдавать: эквивалентно hours=72（сохранение поведения старого вызывающего).
func (p *Panel) usage(w http.ResponseWriter, r *http.Request) {
	if p.cfg.Usage == nil {
		writeErr(w, http.StatusNotImplemented, "usage recorder not available")
		return
	}
	q := r.URL.Query()
	win := usage.Window{}
	win.From = parseTimeParam(q.Get("from"))
	win.To = parseTimeParam(q.Get("to"))
	if win.From.IsZero() && win.To.IsZero() {
		win.Hours = 72
		if v := q.Get("hours"); v != "" {
			if n, err := strconv.Atoi(v); err == nil && n >= 0 {
				win.Hours = n
			}
		}
		if win.Hours > 1440 {
			win.Hours = 1440
		}
	}
	// Никнейм только для отображения, берётся из снапшота пула (без учётных данных).
	nicks := map[string]string{}
	for _, s := range p.cfg.Pool.List() {
		if s.Nickname != "" {
			nicks[s.UID] = s.Nickname
		}
	}
	var currentRate func(realm, model string) string
	if p.cfg.Upstream != nil {
		currentRate = p.cfg.Upstream.ModelRate
	}
	writeJSON(w, http.StatusOK, p.cfg.Usage.SnapshotWindow(win, nicks, currentRate))
}

// parseTimeParam разбор временных query-параметров:unix сек (по умолчанию фронтенда) или RFC3339（Для ручной отладки
// Интерфейс/скрипта). Пустая строка и невалидные значения возвращают ноль = На этой стороне без ограничений, без ошибки — параметр интервала опционален
// усиление, одна опечатка в from Не должно блокировать открытие всей страницы использования.
func parseTimeParam(v string) time.Time {
	v = strings.TrimSpace(v)
	if v == "" {
		return time.Time{}
	}
	if n, err := strconv.ParseInt(v, 10, 64); err == nil {
		if n <= 0 {
			return time.Time{}
		}
		// Совместимость секунд и миллисекунд (фронтенд может напрямую Date.now() передано выше).
		if n > 1e12 {
			return time.UnixMilli(n)
		}
		return time.Unix(n, 0)
	}
	if t, err := time.Parse(time.RFC3339, v); err == nil {
		return t
	}
	if t, err := time.ParseInLocation("2006-01-02T15:04", v, time.Local); err == nil {
		return t
	}
	return time.Time{}
}

// usageSave Немедленно сбросить бакет использования из памяти на диск (в норме фоном 30s отвечает debounce-обновление).
func (p *Panel) usageSave(w http.ResponseWriter, r *http.Request) {
	if p.cfg.Usage == nil {
		writeErr(w, http.StatusNotImplemented, "usage recorder not available")
		return
	}
	p.cfg.Usage.Save()
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// packages Возвращает состав пакетов баллов всех аккаунтов для сравнения в представлении "Состав баллов».
//
// Опрос апстрима по каждому аккаунту (лимит параллелизма, чтобы не упереться в лимит апстрима), ошибка только на соответствующем аккаунте
// Метка error，Не влияет на другие аккаунты — один аккаунт token Истечение не должно оставлять всю страницу пустой.
func (p *Panel) packages(w http.ResponseWriter, r *http.Request) {
	accts := p.cfg.Pool.List()
	type row struct {
		UID string `json:"uid"`
		Nickname string `json:"nickname"`
		Realm string `json:"realm"`
		Remain int64 `json:"remain"`
		Size int64 `json:"size"`
		Packages []upstream.CreditPackage `json:"packages"`
		Error string `json:"error,omitempty"`
	}
	out := make([]row, len(accts))

	sem := make(chan struct{}, 3)
	var wg sync.WaitGroup
	for i, s := range accts {
		wg.Add(1)
		go func(i int, s pool.Status) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()

			it := row{UID: s.UID, Nickname: s.Nickname, Realm: s.Realm}
			a := p.cfg.Pool.AuthByUID(s.UID)
			if a == nil {
				it.Error = "account not loaded"
				out[i] = it
				return
			}
			packs, remain, size, err := p.cfg.Upstream.CreditPackages(a)
			if err != nil {
				it.Error = err.Error()
				out[i] = it
				return
			}
			it.Packages = packs
			it.Remain = remain
			it.Size = size
			out[i] = it
		}(i, s)
	}
	wg.Wait()

	// Баланс по убыванию: большие впереди, удобно сравнивать с малыми.
	sort.SliceStable(out, func(i, j int) bool { return out[i].Remain > out[j].Remain })
	writeJSON(w, http.StatusOK, map[string]any{"accounts": out})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	raw, _ := json.Marshal(v)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(raw)
}

func writeErr(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]any{"ok": false, "error": msg})
}
