// Package pool Пул аккаунтов: единый автомат состояний (здоровый/Охлаждение/Circuit Breaker/понижение приоритета при серии неудач)+ аренда в процессе + приоритет ближайшего истечения/Взвешенный выбор + state.json Персистентность.
package pool

import (
	"sync/atomic"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
)

// degradeReason понижение веса при серии поражений (issue #114）Запись reason Фиксированный текст: домена охлаждения
// reason（"429 rate limit« / «waf 403 block« / »Недостаточно средств"）общее поле,
// эксплуатация в /status в одном месте видно "почему понижен вес/охлаждение», без добавления полей учёта.
const degradeReason = "consecutive failures"

type CoolKind int

const (
	CoolHard CoolKind = iota // Недостаточно средств → кулдаун до следующего дня 04:00（до восстановления чекина)
	CoolSoft // 429 → короткий кулдаун
)

func (k CoolKind) String() string {
	switch k {
	case CoolHard:
		return "hard_credit"
	case CoolSoft:
		return "soft_rate"
	}
	return "unknown"
}

// TokenUsage Накопленное число chat-запросов аккаунта token сводка использования (без исходных учетных данных).
type TokenUsage struct {
	RequestCount int64 `json:"request_count,omitempty"`
	UsageCount int64 `json:"usage_count,omitempty"`
	PromptTokens int64 `json:"prompt_tokens,omitempty"`
	CompletionTokens int64 `json:"completion_tokens,omitempty"`
	TotalTokens int64 `json:"total_tokens,omitempty"`
	LastLatencyMs int64 `json:"last_latency_ms,omitempty"`
	LastTokensPerSecond *float64 `json:"last_tokens_per_second,omitempty"`
	LastUsedAt time.Time `json:"last_used_at,omitempty"`
	LastModel string `json:"last_model,omitempty"`
}

// TokenUsageDelta — попытка чат-аккаунта usage Инкремент.
// каждый Has* Поле для различения отсутствия поля у upstream и того, что значение поля действительно 0。
type TokenUsageDelta struct {
	Model string
	HasPromptTokens bool
	PromptTokens int64
	HasCompletionTokens bool
	CompletionTokens int64
	HasTotalTokens bool
	TotalTokens int64
	HasLatencyMs bool
	LatencyMs int64
	HasTokensPerSecond bool
	TokensPerSecond float64
}

// Status Внешний статус отдельного аккаунта (десенсибилизирован).
type Status struct {
	UID string `json:"uid"`
	Nickname string `json:"nickname,omitempty"`
	Credits int64 `json:"credits"`
	CreditsTotal int64 `json:"credits_total,omitempty"` // Общий лимит баллов (агрегация тарифов);0 = неизвестно (стар. state/ошибка запроса)
	// CreditsExpiring / CreditsEarliestExpiry / CreditsEarliestRemaining описание текущих баллов в
	// давление истечения.CreditsExpiring — остаток баллов в окне конфигурации; последние два — из всех будущих партий с истечением
	// самая ранняя партия и её остаток, для WorkDaddy Той же категории“приоритет ближайшего истечения”Использование маршрутизации.
	CreditsExpiring int64 `json:"credits_expiring,omitempty"`
	CreditsEarliestExpiry time.Time `json:"credits_earliest_expiry,omitempty"`
	CreditsEarliestRemaining int64 `json:"credits_earliest_remaining,omitempty"`
	Cooling bool `json:"cooling"`
	CoolKind string `json:"cool_kind,omitempty"`
	CoolRemaining int64 `json:"cool_remaining_sec,omitempty"`
	Until time.Time `json:"until,omitempty"`
	Reason string `json:"reason,omitempty"`
	SoftStreak int `json:"soft_streak,omitempty"` // Количество последовательных мягких cooldown (показатель экспоненциального backoff, см. entry.softStreak）
	// RateLimitedModels список моделей, всё ещё в лимите (issue #36 реестр лимитов).
	// только "со временем парсинга 6004」запускаемый независимый кулдаун уровня модели (modelCooldowns неистёкшие записи) — не пусто,
	// Одна строка на модель; по этому эксплуатация видит"Аккаунт A модель X Все еще в лимите, ожидается Z Восстановление времени"。Истекает — исчезает (обнуление).
	RateLimitedModels []RateLimitedModel `json:"rate_limited_models,omitempty"`
	// Realm Домен аккаунтов (cn/global，auth.Realm() Вычисляемое значение; вкл. global.enabled шлюз переключателя).
	// для панели/Интерфейс статуса с группировкой по доменам.
	Realm string `json:"realm,omitempty"`
	Disabled bool `json:"disabled"`
	DisabledReason string `json:"disabled_reason,omitempty"` // Только disabled Аккаунт: причина блокировки (видна эксплуатации)
	SuccessCount int64 `json:"success_count,omitempty"`
	ErrTotal int64 `json:"err_total,omitempty"`
	LastSuccessTime time.Time `json:"last_success,omitempty"`
	LastErrTime time.Time `json:"last_err,omitempty"`
	// CheckinDone Локально сегодня уже отмечено (успешная отметка или апстрим"Сегодня уже отмечено"идемпотентные отклонения тоже считаются).
	// global у доменных аккаунтов нет системы чекинов, всегда false。Кнопка отметки на панели отображается на основе этого check-in/Подписано.
	CheckinDone bool `json:"checkin_done,omitempty"`
	TokenUsage TokenUsage `json:"token_usage,omitempty"`
	// ModelCosts фактический реестр стоимости по каждой модели (P1-anti-monopoly наблюдаемость): эксплуатация сверяется по этому
	//「почему всегда выбирается он» —tier 0（бесплатно) монополия / tier 2 Сортировка по цене за единицу наглядна.
	// Только modelCostTTL валидных наблюдений внутри, по строке на модель (cost_per_1k + last_seen +
	// samples）；нет наблюдений/всё истекло → nil（tier 1 неизвестный слой).
	ModelCosts []ModelCostStatus `json:"model_costs,omitempty"`
	// ConsecutiveFails счетчик последовательных неудач (для понижения веса при серии неудач, см. entry.consecutiveFails）。
	// нулевое значение тоже пробрасывается (опер. метрика: с err_total/session_dead_fails согласовано, отсутствие нулевого значения введет в заблуждение
	// ошибочно принять за"Нет записи"，фактически нулевое значение было omitempty опущено).
	ConsecutiveFails int `json:"consecutive_fails"`
	DegradeUntil time.Time `json:"degrade_until,omitempty"` // Дедлайн понижения веса за серию неудач (ненулевой и не истёк = в понижении приоритета)
	// Рантайм-состояние (без персистентности): кол-во запросов в полёте + Состояние circuit breaker.
	InFlight int `json:"in_flight"`
	BreakerFails int `json:"breaker_fails"`
	BreakerUntil time.Time `json:"breaker_until,omitempty"`
}

// ModelCostStatus одиночный (Аккаунт, Модель) строка учета затрат (P1-anti-monopoly наблюдаемость).
// tier не выделять отдельное поле:cost_per_1k ≤ 0 т.е. tier 0（бесплатно),> 0 т.е. tier 2（платно),
// Нет наблюдений — tier 1——Стороной вызывающего/Панель пушится по значению, без дрейфа двойного представления.
type ModelCostStatus struct {
	Model string `json:"model"`
	// CostPer1k фактически на тысячу token цена за единицу (EMA сглаженное значение).≤0 = Фактически бесплатно (tier 0）。
	CostPer1k float64 `json:"cost_per_1k"`
	// LastSeen Момент последнего наблюдения (при истечении исчезает из реестра, как modelCostTTL Критерий).
	LastSeen time.Time `json:"last_seen"`
	// Samples накопленное число наблюдений (EMA см. степень сходимости).
	Samples int `json:"samples,omitempty"`
}

// RateLimitedModel Строка реестра для одной rate-limited модели (issue #36）。
type RateLimitedModel struct {
	Model string `json:"model"`
	// Kind различать лимитирование и недоступность модели:6004 Да rate_limit，11102 Да model_unavailable。
	Kind string `json:"kind"`
	// Until момент истечения охлаждения = индивидуальный дедлайн охлаждения данной модели (modelCooldowns[m].Until，после усечения),
	// при лимитировании нескольких моделей больше не равно Status.Until（уровень аккаунта).
	Until time.Time `json:"until,omitempty"`
	// ResetAt апстрим "через … сброса» исходное wall-clock время (без soft_rate_max усечение);
	// После усечения Until==ResetAt При ... опускается ResetAt пусть в реестре естественно станет на один столбец меньше.
	ResetAt time.Time `json:"reset_at,omitempty"`
	// Reason Причина срабатывания (текст для эксплуатации).
	Reason string `json:"reason,omitempty"`
}

// modelCooldown одиночный (Аккаунт, Модель) независимая запись кулдауна на уровне модели.
// Несет два смысла "модель недоступна на этом аккаунте»:
// - 6004 Лимит на уровне модели:Until Синхронизировать wall clock со сбросом апстрима;ResetAt Записать авторитетный момент восстановления.
// - 11102 У бэкенда нет этой модели:Until — экспоненциальный бэкофф TTL（6h от, потолок 24h）；Hits Запись
// Накопленное число попаданий управляет бэкоффом (6004 отсутствует hits концепт,Hits Конст. 0）。
type modelCooldown struct {
	// Until дедлайн охлаждения этой модели (6004：now+min(resetAt-now, soft_rate_max)；11102：now+Бэкофф TTL）。
	Until time.Time
	// ResetAt апстрим "через … сброса» исходное wall-clock время (без soft_rate_max усечение).
	// и Until различие аналогично:Until возможно усечение,ResetAt — авторитетный момент восстановления апстрима.
	// 11102 без текста сброса,ResetAt Постоянное нулевое значение.
	ResetAt time.Time
	// Reason Причина срабатывания (текст для эксплуатации, то же Status.Reason）。
	Reason string
	// Hits 11102 Накопленные хиты негативного кэша (драйвер экспоненциального бэкоффа).6004 Запись Hits Конст. 0。
	Hits int
	// AuditOnly для true только для отображения статуса (например, без времени сброса 6004），
	// healthyForModel и modelExempt Необходимо игнорировать, чтобы не менять логику выбора номеров.
	AuditOnly bool
}

// modelCostTTL Срок годности наблюдения стоимости. Брать 6 Часы: покрывает как"ночью бесплатно"такого рода повременной акции
// Одна сессия, при этом вчерашняя цена не определяет сегодняшний выбор — если просроченные бесплатные наблюдения вечны,
// Днем уже платные аккаунты продолжат считаться бесплатными.
const modelCostTTL = 6 * time.Hour

// modelCostEntry рантайм-реестр стоимости (персистентный образ stateModelCost один-в-один соответствует его полям).
type modelCostEntry struct {
	CostPer1k float64
	LastSeen time.Time
	Samples int
}
type entry struct {
	a *auth.Auth
	credits int64
	creditsTotal int64 // Общий лимит баллов (UserResource Агрегация;0 = неизвестно)
	// creditsExpiring Подмножество доступных поинтов в окне конфигурации, скоро истекающих, — это credits часть.
	// creditsEarliestExpiry / creditsEarliestRemaining самый ранний из всех будущих партий с истечением срока;
	// оба — через check-in/обновление баланса, для earliest-expiry используется для маршрутизации и наблюдения состояния.
	creditsExpiring int64
	creditsEarliestExpiry time.Time
	creditsEarliestRemaining int64
	successCount int64 // Накопленный успех
	errTotal int64 // Накопленные ошибки (для веса успешности successRate = successCount/(successCount+errTotal)，без сброса)
	lastErr time.Time // Время последней ошибки
	lastSuccess time.Time // Время последнего успеха
	tokenUsage TokenUsage // запрос чата token Сводка расхода (персистентно)
	// lastCheckinDay локальная дата последнего успешного check-in ("2006-01-02"）。Успешный чекин и апстрим
	// идемпотентный отказ ("Сегодня уже отмечено"）всё считается;statusOf Вывод на основе этого CheckinDone Для отображения кнопки панели
	// check-in/уже отмечено. Персистентность: состояние за день не теряется при перезапуске.
	lastCheckinDay string
	coolKind CoolKind
	until time.Time // Дедлайн охлаждения (мгновенное охлаждение:CoolSoft 429 / CoolHard баланс исчерпан)
	disabled bool
	reason string
	lastUsed time.Time // момент последнего выбора (защита от конкурентной коллизии)
	// usedSeq монотонно возрастающий порядковый номер выбора: при каждом pick При выборе брать p.pickSeq Автоинкремент.
	// Windows и др. платформы time.Now() ограниченная точность (~0.5ms），высокая конкурентность/несколько при быстром последовательном выборе номеров
	// Аккаунт lastUsed полностью равны, на основе wall-clock LRU/Защита от thundering herd не срабатывает.
	// usedSeq Обеспечивает строгий полный порядок, не зависит от точности времени. Runtime, не персистируется.
	usedSeq uint64
	// breakerUntil / fails / retryCount для рабочего состояния circuit breaker (без персистентности).
	// fails является единственным"Последовательные сбои"счетчик: любая ошибка засчитывается, при достижении breakerThreshold ТриггерОтключение (экспоненциальный backoff),
	// Накопление по всем входам, успешно/Circuit Breaker/при общем revival сброс в ноль (сохранить retryCount управляет показателем бэкоффа).
	breakerUntil time.Time // Дедлайн circuit breaker'а (экспоненциальный бэкофф)
	fails int // Счётчик последовательных неудач (для circuit breaker, единственный авторитетный)
	retryCount int // кол-во срабатываний circuit breaker (показатель экспоненциального бэкоффа)
	// softStreak число последовательных мягких охлаждений (CoolSoft），независимо от circuit breaker fails **Cooldown-домен**Счетчик:
	// fails будет сброшено по circuit breaker и hard Охлаждение и NoteError загрязнение, невозможно выразить"Непрерывный мягкий rate-limit"。
	// Точек сброса всего две (обе — моменты подтверждённого восстановления аккаунта):NoteSuccess、reviveCoolingLocked。
	// персистентность (stateAccount.SoftStreak）：После перезапуска мягкий лимит остаётся в бэкоффе, не сбрасывается к базе.
	softStreak int
	// modelCooldowns 6004 Уровень модели limit **независимый**Таблица охлаждения:model → дедлайн охлаждения данной модели/Сброс.
	// и until（уровня всех аккаунтов) ортогонально:6004 запись только в эту таблицу, без записи until，поэтому несколько моделей одновременно 6004 Время
	// Независимый отсчёт, без перекрытия (A После срабатывания B повторный триггер,A дедлайн охлаждения не B перекрытие — это
	// Заказ until поле не может). Только 6004 Логировать при триггере; пусто map = Без лимита на уровне модели (без исключений).
	// Семантика рантайма (без персистентности): сброс при рестарте, деградация до уровня аккаунта until Текущее состояние кулдауна.
	modelCooldowns map[string]modelCooldown
	// sessionDeadFails непрерывно 12153（ErrSessionDead）Подсчет.12153 В проде срабатывает эпизодически
	// （Сетевой джиттер/Кратковременный сбой апстрима/refresh состояние гонки), бан навсегда за одну ошибку слишком жестко — бан только после N подряд.
	// персистентность (stateAccount.SessionDeadFails）：апстрим продолжает session dead сброс при перезапуске приведет к
	// Переобучение (повторное поглощение 2 неудач до отключения, в период каждый раз холостой вызов апстрима); точка сброса (refresh/chat успех,
	// ручное восстановление) также сохраняется на диск, после перезапуска старый счётчик не остаётся.
	sessionDeadFails int
	// consecutiveFails счетчик последовательных ошибок (понижение веса при серии,issue #114）——「фолбэк "причина неизвестна»:
	// перекрытие ErrClient（неизвестно 4xx）И сбои транспортного уровня (нет соединения с апстримом) типа applyErrorPolicy
	// default ветка без штрафа аккаунта. С sessionDeadFails изоморфно, но счётчики независимы:12153 конечное состояние
	// Да Disable，Конечное состояние здесь — временный вывод из пула (degradeUntil）。Точка обнуления:NoteSuccess。
	// персистентность (stateAccount.ConsecutiveFails + DegradeUntil）：restart Сброс обнулит
	// 「Продолжительный сбой апстрима + комбинация "частые перезапуски» заново дообучается до заполнения 5 раз;degradeUntil персистентность продлевает период даунгрейда
	// Сохранение состояния после перезапуска (с breakerUntil в той же метрике).
	consecutiveFails int
	// degradeUntil Дедлайн даунгрейда по серии неудач: если не ноль и не истек — аккаунт не участвует normal Выбор аккаунта (временный
	// вывода из пула). И кулдаун/Circuit Breaker**берётся более длинный, без суммирования**（healthy Считается как параллельное ИЛИ, достаточно одного условия для отсечки
	// пока не истёк — недоступен, действует всегда самый дальний), по истечении автовозврат в пул без явного сброса.
	degradeUntil time.Time
	// modelCost фактический реестр списаний:model → наблюдение. При каждом успешном запросе usage.credit
	// пересчитано (у апстрима нет"По расходу модели"интерфейс, только натурные испытания). При выборе аккаунта по этому "на данной модели
	// Бесплатно/дешевые номера» впереди (pick costTier жесткое разделение).
	// персистентность (stateAccount.ModelCosts，P1-anti-monopoly）：Сохранение знаний о стоимости после перезапуска;
	// сброс на диск/Восстановление по modelCostTTL Ленивая фильтрация, устаревшие наблюдения не восстанавливаются.
	modelCost map[string]modelCostEntry
	// inFlight Количество запросов в полете на аккаунт (runtime, без персистентности). Использовать atomic Избежать Pick Горячий путь берёт write-lock.
	inFlight atomic.Int64
}

// modelCostOf Возвращает для данного аккаунта в указанном model валидное наблюдение стоимости на ; при отсутствии или просрочке наблюдения вернуть ok=false。
func (e *entry) modelCostOf(model string, now time.Time) (modelCostEntry, bool) {
	if model == "" || len(e.modelCost) == 0 {
		return modelCostEntry{}, false
	}
	mc, ok := e.modelCost[model]
	if !ok {
		return modelCostEntry{}, false
	}
	if mc.LastSeen.IsZero() || now.Sub(mc.LastSeen) > modelCostTTL {
		return modelCostEntry{}, false // Истечение: временная акция (ночная бесплатность) не действует вне своего интервала
	}
	return mc, true
}

// healthy Сообщает, доступен ли аккаунт сейчас (не отключен, не в каком-либо кулдауне/Circuit Breaker/период понижения веса при серии неудач).
// Понижение приоритета и кулдаун при серии неудач/circuit-breaker входит в это решение (берётся больший без суммирования: три дедлайна — параллельное ИЛИ,
// пока хотя бы один не истёк — выбор недоступен, естественно "при параллельном существовании берётся более дальний» — явное сравнение длительности не нужно).
func (e *entry) healthy(now time.Time) bool {
	if e.disabled {
		return false
	}
	if !e.until.IsZero() && now.Before(e.until) {
		return false
	}
	if !e.breakerUntil.IsZero() && now.Before(e.breakerUntil) {
		return false
	}
	if !e.degradeUntil.IsZero() && now.Before(e.degradeUntil) {
		return false
	}
	return true
}

// modelExempt сообщить, находится ли аккаунт в "6004 форма "мягкий кулдаун на уровне модели»: есть любой действительный 6004
// Охлаждение на уровне модели (modelCooldowns не пусто) и ещё не отключено, безОтключение.
// в этом режиме аккаунт недоступен только для лимитируемой модели, для остальных остается доступным (issue #31）。
// healthyForModel и ServableNow Использовать общий предикат, гарантируя chat Критерии выбора номера и проверки живости совпадают.
// Ответственность вызывающей стороны now и проверка действительности охлаждения (метод проверяет только форму, а не истечение охлаждения).
func (e *entry) modelExempt() bool {
	if e.disabled || !e.until.IsZero() || !e.degradeUntil.IsZero() || !e.breakerUntil.IsZero() {
		return false
	}
	for _, mc := range e.modelCooldowns {
		if !mc.AuditOnly && !mc.Until.IsZero() {
			return true
		}
	}
	return false
}

// modelCooled Отчет аккаунта по указанному model находится ли в 6004 Охлаждение на уровне модели (индивидуальное охлаждение модели ещё не истекло).
// пустой reqModel / Не записано → false（не ограничивать аккаунт по измерению уровня модели).
func (e *entry) modelCooled(now time.Time, reqModel string) bool {
	if reqModel == "" {
		return false
	}
	mc, ok := e.modelCooldowns[reqModel]
	if !ok || mc.AuditOnly {
		return false
	}
	return !mc.Until.IsZero() && now.Before(mc.Until)
}

// healthyForModel Отчет аккаунта по указанному model Опционально ли (вкл. 6004 независимое решение о cooldown на уровне модели):
// - disabled → никогда не выбирается (высший приоритет);
// - Данная модель находится в 6004 Независимый кулдаун (modelCooldowns[reqModel] не истёк)→ Недоступно для выбора
// （при лимитировании нескольких моделей — независимо, без взаимного влияния);
// - Иначе → Фолбэк на уровень аккаунта healthy（until/breakerUntil измерению).
//
// В сравнении со старой реализацией (softRateModel Исключение для одного поля«Блокировать только одну модель, остальные — исключение«），Новая семантика поддерживается нативно
// Троттлинг любого числа моделей одновременно: B аккаунт под rate limit для A Запрос всё ещё может быть выбран (A не в modelCooldowns Перехват
// и на уровне аккаунта healthy выполняется). Пусто reqModel / Незарегистрированная модель → Эквивалентно healthy。
func (e *entry) healthyForModel(now time.Time, reqModel string) bool {
	if e.disabled {
		return false
	}
	if e.modelCooled(now, reqModel) {
		// данная модель в 6004 в независимом кулдауне → недоступно для выбора.
		return false
	}
	// Кулдаун на уровне аккаунта/Сначала проверка circuit breaker; если не в cooldown — решает здоровье на уровне аккаунта.
	return e.healthy(now)
}

// pruneExpiredModelCosts Удалить modelCost просроченные записи в (ленивая очистка, с
// pruneExpiredModelCooldowns та же форма, та же точка вызова).
//
// Зачем это обязательно:modelCost ранее только в**Чтение**（modelCostOf）、**сброс на диск**（persist）、
// **Восстановление**（persist）、**status**（state.go）Делать в четырех местах«Фильтрация просроченных«，Сама запись в памяти
// никогда не освобождается — т.е. "map только увеличивается, не уменьшается». А entry.modelCost комментарий явно утверждает, что
// 「сброс на диск/Восстановление по modelCostTTL ленивая фильтрация, устаревшие наблюдения не воскрешаются (тот же modelCooldowns Метрика)»,
// Таблица охлаждения на уровне модели именно за счёт pruneExpiredModelCooldowns В pick путь с блокировкой записи выполняет фактическое удаление
// （См. pick.go「map не разрастается бесконечно»). Метрики расходятся: как только наблюдение модели истекает, она
// навсегда занимает слот памяти (очистка только при рестарте процесса) и в каждом следующем раунде pick обход, каждый раз status В обходе
// Многократно признаётся просроченным (просто никто не удаляет).
//
// наблюдение пишется только на пути успешного запроса (NoteModelCost），И model Без сверки с каталогом, поэтому рост ограничен
// "Имена ранее обслуженных моделей«Ограничение — не безграничная утечка, но также«Только увеличение, без уменьшения«таблица без GC.
// вызывающая сторона должна уже удерживать p.mu Блокировка записи.
func (e *entry) pruneExpiredModelCosts(now time.Time) {
	if len(e.modelCost) == 0 {
		return
	}
	for m, mc := range e.modelCost {
		if mc.LastSeen.IsZero() || now.Sub(mc.LastSeen) > modelCostTTL {
			delete(e.modelCost, m)
		}
	}
}

// pruneExpiredModelCooldowns Удалить modelCooldowns просроченные записи в (ленивая очистка).
// pick Путь write-блокировки и revive вызов, предотвращает map бесконечное разрастание;status Read-only обход естественно пропускает просроченные элементы,
// очистка не нужна. Вызывающая сторона должна уже владеть p.mu Блокировка записи.
func (e *entry) pruneExpiredModelCooldowns(now time.Time) {
	if len(e.modelCooldowns) == 0 {
		return
	}
	for m, mc := range e.modelCooldowns {
		if mc.Until.IsZero() || !now.Before(mc.Until) {
			delete(e.modelCooldowns, m)
		}
	}
}

// expiry Вернуть последнее действующее охлаждение аккаунта/Circuit Breaker/время окончания понижения приоритета (из трёх дедлайнов берётся earliest); вне кулдауна — ноль.
// Резервный выбор при полном кулдауне«Самый ранний срок истечения«для аккаунта. Понижение за серию неудач учитывается в fallback: пониженные номера участвуют в fallback (их неудача
// состояние — "причина неизвестна», полуоткрытая проба по истечении как раз fallback-семантика —CoolHard только тогда исключается).
func (e *entry) expiry(now time.Time) time.Time {
	var t time.Time
	if !e.until.IsZero() && now.Before(e.until) {
		t = e.until
	}
	if !e.breakerUntil.IsZero() && now.Before(e.breakerUntil) {
		if t.IsZero() || e.breakerUntil.Before(t) {
			t = e.breakerUntil
		}
	}
	if !e.degradeUntil.IsZero() && now.Before(e.degradeUntil) {
		if t.IsZero() || e.degradeUntil.Before(t) {
			t = e.degradeUntil
		}
	}
	return t
}

// fallbackKind Сообщить, к какому типу кулдауна относится fallback-аккаунт (soft：Мгновенный мягкий кулдаун/понижение веса при серии поражений;breaker：период размыкания).
// Вызывать только для аккаунтов, участвующих в фолбэке (CoolHard Уже pickEarliestExpiryLocked исключить). Критерий проверки:
// Если дедлайн circuit breaker — ближайший действующий (вкл.«только circuit breaker без мягкого кулдауна«），Обозначить как breaker；иначе пометить как soft
// （понижение веса при серии неудач и мягкий кулдаун объединены soft：Все пропускаются по своему дедлайну, обработка фолбэка без различий).
func (e *entry) fallbackKind(now time.Time) string {
	if !e.breakerUntil.IsZero() && now.Before(e.breakerUntil) {
		if e.until.IsZero() || !now.Before(e.until) || e.breakerUntil.Before(e.until) {
			return "breaker"
		}
	}
	return "soft"
}

// stateAccount Персистентное состояние одного аккаунта (JSON tag snake_case в нижнем регистре, обратная совместимость: отсутствующее поле — нулевое значение).
type stateAccount struct {
	Credits int64 `json:"credits"`
	CreditsTotal int64 `json:"credits_total,omitempty"`
	Disabled bool `json:"disabled"`
	Reason string `json:"reason,omitempty"`
	Until time.Time `json:"until,omitempty"`
	CoolKind CoolKind `json:"cool_kind"`
	SuccessCount int64 `json:"success_count,omitempty"`
	// err_total накопительный счётчик ошибок. Старая версия err_count（последовательные ошибки) всё ещё читаемо: при загрузке маппится на err_total，
	// только одноразовая миграция, без обратной записи err_count。
	ErrTotal int64 `json:"err_total,omitempty"`
	ErrCount int `json:"err_count,omitempty"` // источник миграции для совместимости со старыми файлами, только чтение
	LastSuccess time.Time `json:"last_success,omitempty"`
	LastErr time.Time `json:"last_err,omitempty"`
	// LastCheckinDay локальная дата последнего успешного check-in (entry.lastCheckinDay тот же источник).
	// Персистентность для сохранения статуса "сегодня отмечено»: после отметки перезапуск не откатывает кнопку панели в "Отметиться».
	LastCheckinDay string `json:"last_checkin_day,omitempty"`
	TokenUsage TokenUsage `json:"token_usage,omitempty"`
	// Счетчик runtime-состояния (soft_streak/session_dead_fails/credits_expiring）Не использовать omitempty：
	// отсутствие нуля вводит в заблуждение, будто"Нет записи"，фактически нулевое значение опущено.
	SoftStreak int `json:"soft_streak"`
	// SessionDeadFails непрерывно 12153 Счётчик (проверка session прогресс умершего). Персистентность для сохранения
	// 「после рестарта последовательный счётчик продолжает накапливаться».
	SessionDeadFails int `json:"session_dead_fails"`
	// ConsecutiveFails Счетчик последовательных неудач (прогресс понижения при серии неудач, см. entry.consecutiveFails）。
	ConsecutiveFails int `json:"consecutive_fails"`
	// DegradeUntil понижение веса при серии неудач до (issue #114）。персистится только неистёкшее (запись на диск/восстановление — ленивое
	// фильтрация), избежать потери состояния при рестарте в период понижения приоритета; истечение/Нулевые значения не писать. Семантика указателя та же BreakerUntil。
	DegradeUntil *time.Time `json:"degrade_until,omitempty"`
	// BreakerUntil Дедлайн circuit breaker (экспоненциальный бэкофф). Персистится только неистёкшее (запись на диск/восстановление — всё с ленивой фильтрацией),
	// Во избежание потери состояния при перезапуске в период размыкания:breakerUntil Блокировка выбора номера сохраняется после перезапуска в будущем. Истечение/Нулевые значения не писать.
	// использовать *time.Time（а не time.Time）：Go omitempty Для не-указателя time.Time Нулевое значение
	// Не применяется (сериализуется в 0001-01-01T00:00:00Z）；указатель nil только тогда может быть действительно omitempty опустить,
	// и сохранение на диск"Просроченное не писать"калибр совпадает.
	BreakerUntil *time.Time `json:"breaker_until,omitempty"`
	// RetryCount Счётчик срабатываний circuit breaker (экспонента для backoff). Персистится для сохранения"Чем дольше вОтключение, тем дольше"накопление бэкоффа —
	// сброс при рестарте заставит повторныйОтключение стартовать только с минимального бэкоффа. При восстановлении если BreakerUntil если истекло — сбросить в ноль.
	RetryCount int `json:"retry_count,omitempty"`
	// CreditsExpiring Подмножество скоро истекающих баллов (credits подмножество). персистентность для сохранения маршрута с истечением
	// （предпочтение маршрута с наиболее ранним истечением) — между рестартом и следующим check-in не должно теряться.
	CreditsExpiring int64 `json:"credits_expiring"`
	// CreditsEarliestExpiry / CreditsEarliestRemaining Самая ранняя будущая партия к истечению и остаток.
	// и CreditsExpiring сохраняется персистентно, после рестарта первый запрос может использовать последний снапшот баланса.
	CreditsEarliestExpiry time.Time `json:"credits_earliest_expiry,omitempty"`
	CreditsEarliestRemaining int64 `json:"credits_earliest_remaining,omitempty"`
	// ModelCooldowns Независимая таблица кулдауна на уровне модели (model → запись кулдауна:6004 сброс wall-clock / 11102
	// бэкофф негативного кэша). Персистентность:6004 синхронизация со сбросом wall-clock апстрима, охлаждение одной модели может длиться несколько часов,
	// Перезапуски — норма; без персистентности приведет к healthyForModel После перезапуска забывается, снова попадает на мины.
	// При восстановлении ленивая фильтрация просроченных записей.
	ModelCooldowns map[string]stateModelCooldown `json:"model_cooldowns,omitempty"`
	// ModelCosts Фактический ledger списаний (model → наблюдение цены за единицу, см. entry.modelCost）。Персистентность
	// （P1-anti-monopoly）：Знания о стоимости сохраняются после рестарта, лимит/Ночное бесплатное окно между перезапусками больше не
	// Повторное платное зондирование. Сохранить на диск/Восстановление всё по modelCostTTL ленивая фильтрация (6h вне — не писать и не восстанавливать —
	// Устаревшая цена не восстанавливается); на стороне восстановления отбрасывать невалидные значения (отриц. per1k/Ноль LastSeen записи с поврежденной структурой).
	ModelCosts map[string]stateModelCost `json:"model_costs,omitempty"`
}

// stateModelCooldown одиночный (Аккаунт, Модель) персистентная запись независимого cooldown на уровне модели, с runtime-состоянием
// modelCooldown Изоморфный (Until/ResetAt/Reason имя поля выровнено с семантикой), запись на диск/Восстановление round-trip без потерь.
// Hits без сохранения на диск (после перезапуска 11102 бэкофф с 6h переобучение базы, то же modelCost Критерий).
type stateModelCooldown struct {
	Until time.Time `json:"until"`
	ResetAt time.Time `json:"reset_at,omitempty"`
	Reason string `json:"reason,omitempty"`
	AuditOnly bool `json:"audit_only,omitempty"`
}

// stateModelCost одиночный (Аккаунт, Модель) персистентная запись наблюдения за стоимостью, и рабочее состояние modelCostEntry
// изоморфно (единое представление, память и диск без двух схем).
type stateModelCost struct {
	CostPer1k float64 `json:"cost_per_1k"`
	LastSeen time.Time `json:"last_seen"`
	Samples int `json:"samples,omitempty"`
}

// stateFile Формат персистентности.
type stateFile struct {
	Accounts map[string]stateAccount `json:"accounts"`
}

// flushInterval Период фоновой записи на диск.
const (
	defaultBreakerThreshold = 3
	defaultBreakerCooldown = 30 * time.Minute
	defaultBreakerCooldownMax = 6 * time.Hour
)

// defaultSoftRateMax Лимит по умолчанию для экспоненциального бэкоффа мягкого кулдауна:softRateMax Не инжектирован (<=0）тогда считать по этому значению,
// Исключить тесты/При прямом использовании пула бэкофф без ограничения.
const defaultSoftRateMax = 2 * time.Hour

// defaultDegrade* Параметры по умолчанию для понижения при серии неудач (issue #114）：серия неудач 5 временных выходов из пула 10 мин.
const (
	defaultDegradeThreshold = 5
	defaultDegradeCooldown = 10 * time.Minute
	defaultDegradeCooldownMax = 2 * time.Hour
)

// defaultCostExploreInterval costTier Окно по умолчанию для исследования условий (issue #136 схема a′）。
// получить 30m：≤ 48 Раз/день/Модель арифметика верхнего лимита исследования (24h/30m=48），и размером пула и QPS не связано.
// Исследование=попутный reroute (перенаправить существующий реал. запрос пользователя на tier 1 аккаунта), ноль новых апстрим-запросов;
// Инкрементальная стоимость — лишь ожидаемая разница биллинга "запрос мог идти на бесплатный аккаунт, фактически пошел на потенциально платный»,
// И tier 1 После исчерпания (все номера активной модели изучены) налоговая база сходится к 0。config Явно "0" Отключение.
const defaultCostExploreInterval = 30 * time.Minute

// sessionDeadThreshold непрерывно ErrSessionDead（12153）При достижении этого числа — перманентное отключение.
// 12153 Будет срабатывать временно (сетевой джиттер/Кратковременный сбой апстрима/refresh гонка), старое поведение — отключение после одной неудачи
// убьёт здоровые аккаунты ложно (P0-1：13 шт. disabled номера — всё ложные срабатывания).3 подряд для признания dead: допуск случайных флуктуаций,
// и не приведёт к реальной смерти session Остаётся в пуле и многократно выбирается.
const sessionDeadThreshold = 3

// sessionDeadReason 12153 Считается как session Персистентность при завершении reason。
const sessionDeadReason = "12153 session dead"

// SessionDeadThreshold раскрыть последовательные 12153 порог блокировки (для scheduler лог/ссылка на эксплуатационную документацию).
func SessionDeadThreshold() int { return sessionDeadThreshold }

// softStreakShiftMax макс. сдвиг влево для бэкоффа мягкого охлаждения (защита от 1<<streak переполнение в отрицательное число/ноль).
// независимо от streak Сколько бы ни накопилось, логика лимита сработает первой, это значение — лишь защита от переполнения.
const softStreakShiftMax = 16

// StoreSnapshotter минимальный интерфейс снапшота состояния пула (redisstore.Store выполнено;Noop пустая реализация безопасна).
// С локальным state.json Сосуществуют как бэкап для восстановления при запуске: снапшот применяется только если новее локального, иначе приоритет у локального.
const (
	defaultIdleWeightPerHour = 0.5
	defaultIdleWeightMax = 5.0
)

// New построение пула;stateFp если не пусто, попытаться загрузить старое состояние и запустить фоновую периодическую запись на диск goroutine。
