// Pool Ядро пула аккаунтов: определение структуры, конструктор (New/Set* Инъекция), аренда в процессе (Acquire/Release）
// и добавление/удаление аккаунта (Add/SyncToDir/upsertLocked）。Выбор номера/Охлаждение/Статус/персистентность см. в других файлах пакета.
package pool

import (
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
)

type Pool struct {
	mu sync.RWMutex
	byUID map[string]*entry
	stateFp string
	dirty atomic.Bool // В памяти есть несохранённые изменения
	// store зеркало снимка состояния пула (redisstore.Store）；nil = Зеркало не требуется (не настроено Redis / Noop кроме того возможно nil）。
	// SaveState/LoadState Через его подключение, с локальным state.json и сохранить как бэкап для восстановления при запуске.
	store StoreSnapshotter
	// Настройка circuit breaker (SetBreaker инжект; значения по умолчанию см. defaultBreaker*）。
	breakerThreshold int
	breakerCooldown time.Duration
	breakerCooldownMax time.Duration
	// softRateMax Потолок экспоненциального бэкоффа мягкого кулдауна (SetSoftRateMax Инъекция; по умолчанию defaultSoftRateMax）。
	softRateMax time.Duration
	// costExploreInterval costTier Окно исследования условий (issue #136 схема a′，SetCostExploreInterval
	// Инъекция; по умолчанию defaultCostExploreInterval 30m）。tier 0 Монополия + tier 1 Существует и с момента последнего
	// Исследование ≥ окна, текущий pick Переключение слоя применения tier 1-only（Исследование=попутный reroute, без новых запросов к апстриму).
	// 0 = отключение (полный возврат к текущему поведению).
	costExploreInterval time.Duration
	// exploreLast каждый (realm, Модель) время последнего исследования, ключ = realm + "\x1f" + model。
	// состояние runtime (не персистится, вместе lastUsed/usedSeq метрика): сброс при перезапуске → каждый всё ещё замороженный
	// (Домен, Модель) до 1 раз немедленный повторный опрос; выпущенные аккаунты через ModelCosts Восстановление tier，Не платить дважды.
	// Запись только при событии exploration (tier 1 на период исчерпания — пауза, устаревание безвредно); симметричная очистка не делается.
	exploreLast map[string]time.Time
	// costExploreEvents накопл. кол-во событий исследования (/status проброс;pick Внутри write-lock ++，Не требуется atomic）。
	costExploreEvents int64
	// degradeThreshold / degradeCooldown / degradeCooldownMax параметры понижения приоритета при серии неудач
	// （SetDegrade инжект; значения по умолчанию см. defaultDegrade*，issue #114）。
	degradeThreshold int
	degradeCooldown time.Duration
	degradeCooldownMax time.Duration
	// creditFloor гарантированный минимум баллов (SetCreditFloor инъекция;0 = Отключено, по умолчанию — как есть).
	// Аккаунт credits < floor при**Платность подтверждена тестами**модель (tier 2，валидное наблюдение леджера) больше не участвует
	// Выбор номера — предотвращает пробитие баланса платными запросами, чтобы даже бесплатные модели 402 Кулдаун до чекина следующего дня.tier 0/1
	// не ограничено (гарантируется "остаток доступен», а не "ничего не вызывать»); восстановление за чекин
	// （SetCreditsDetailed）обход floor то автовосстановление. Весь пул исчерпан + весь tier 2 При выборе аккаунта
	// вернуть nil（строгая семантика: лучше 503 Не пробивать, пропустить=Вернуться к "прожиг до 0」текущее состояние).
	creditFloor int64
	// тюнинг компенсации простоя взвешенной маршрутизации (SetWeights инжект; значения по умолчанию см. defaultIdle*）。
	idleWeightPerHour float64
	idleWeightMax float64
	// preferExpiring Переключатель маршрутизации "сначала истекающие» (по умолчанию true）。включено и в окне скорого истечения есть валидный
	// при батче выбор номера внутри cost-слоя сначала по ближайшему истечению; после отключения — только обычный взвешенный роутинг.
	preferExpiring bool
	// maxInFlight макс. in-flight запросов на аккаунт;0 = без ограничений (аренда закрыта).
	maxInFlight int
	// maxInFlightGlobal global лимит in-flight на аккаунт в домене по уровням (WAF 403 исправление P1-1：global Домен
	// WAF риск-контроль строже, снижать concurrency);0 = не задано, fallback на maxInFlight（без градации, без регресса).
	maxInFlightGlobal int
	// randInt64N только для инжекта детерминированного RNG в тестах;nil использовать при math/rand/v2 глобальный источник.
	// Не задавать это поле в прод-коде.
	randInt64N func(n int64) int64
	// persistFails Локально state.json Счётчик последовательных ошибок записи на диск (только saveLocked Чтение/запись под блокировкой, без atomic）。
	// для троттлинга логов при ошибке записи на диск: первая неудача/Каждый N напоминаний/при восстановлении — по одной записи, чтобы избежать спама при полном диске.
	persistFails int
	// pickSeq Монотонно возрастающий порядковый номер выбора: каждый раз pick при выборе аккаунта инкремент с записью в entry.usedSeq，
	// для LRU Фолбэк/Защита от thundering herd обеспечивает и time.Now() Строгий полный порядок вне зависимости от точности (Windows ~0.5ms при точности
	// lastUsed wall-clock будут идентичны). Только pick чтение/запись по пути write-lock, без atomic。
	pickSeq uint64
	// stopCh Сигнал закрытия:Close Отключение делает startFlusher бэкенд goroutine Выход.
	// nil = не запущено flusher（stateFp когда пусто New не действует flusher）。
	stopCh chan struct{}
	// closeOnce Гарантировать Close Идемпотентность (повторные вызовы не дублируют close channel）。
	closeOnce sync.Once
}

// defaultBreaker* параметры по умолчанию для circuit breaker (FreeBuff2API эталонная метрика).
func New(stateFp string) *Pool {
	p := &Pool{
		byUID: map[string]*entry{},
		stateFp: stateFp,
		breakerThreshold: defaultBreakerThreshold,
		breakerCooldown: defaultBreakerCooldown,
		breakerCooldownMax: defaultBreakerCooldownMax,
		idleWeightPerHour: defaultIdleWeightPerHour,
		idleWeightMax: defaultIdleWeightMax,
		preferExpiring: true,
		degradeThreshold: defaultDegradeThreshold,
		degradeCooldown: defaultDegradeCooldown,
		degradeCooldownMax: defaultDegradeCooldownMax,
		// Дефолт исследования 30m：tier 0 При монополии tier 1 Окно исследования (issue #136）。Пользователь через
		// config Явно "0" Отключение (SetCostExploreInterval(0)）。
		costExploreInterval: defaultCostExploreInterval,
		exploreLast: map[string]time.Time{},
	}
	if stateFp != "" {
		p.load()
		p.startFlusher()
	}
	return p
}

// Close Остановить фоновую запись на диск goroutine и выполнить финальный flush на диск (идемпотентно).
// вызвать перед выходом процесса, устранить startFlusher goroutine утечка; отсутствие вызова не влияет на корректность
// （освобождается при выходе процесса), только гигиена жизненного цикла.
func (p *Pool) Close() {
	if p.stopCh == nil {
		return
	}
	p.closeOnce.Do(func() {
		close(p.stopCh)
	})
	p.Flush()
}

// SetBreaker Инжект параметров circuit breaker (main Из config вызов после парсинга). Неположительные значения сохраняют исходное (по умолчанию).
func (p *Pool) SetBreaker(threshold int, cooldown, cooldownMax time.Duration) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if threshold > 0 {
		p.breakerThreshold = threshold
	}
	if cooldown > 0 {
		p.breakerCooldown = cooldown
	}
	if cooldownMax > 0 {
		p.breakerCooldownMax = cooldownMax
	}
}

// SetSoftRateMax Внедрение лимита длительности мягкого cooldown с экспоненциальным backoff (main Из config вызов после парсинга).
// Неположительные значения сохраняют исходное (использовать по умолчанию 2h），Стиль аналогичен SetBreaker。
func (p *Pool) SetSoftRateMax(d time.Duration) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if d > 0 {
		p.softRateMax = d
	}
}

// SetCostExploreInterval инжект costTier Окно исследования условий (main Из config Вызов после парсинга,
// issue #136）。0 = Отключение (полный возврат к текущему поведению); положительное значение переопределяет дефолт 30m。
// Внимание: с SetSoftRateMax「отличается от "неположительное значение сохраняет дефолт»,0 Здесь это**Допустимые значения**（выключатель останова,
// и config "0« выравнивание семантики отключения) — не задавать 0 семантика не позволяет отключить исследование.
func (p *Pool) SetCostExploreInterval(d time.Duration) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if d < 0 {
		return // отрицательное значение невалидно, сохраняется текущее
	}
	p.costExploreInterval = d
}

// CostExploreStatus Вывод журнала исследования (/status исп.): накопленное число событий исследования + каждый (Домен, Модель)
// Последний момент исследования (внутри ключа \x1f Разделитель выводится как "|«，и model_costs читается построчным сопоставлением
// 「Исследование→выпуска» — полный цикл).RLock Только обход для чтения;map размер зависит от "обслуженных (Домен, Модель)」Коллекция
// Ограничение (с modelCost та же граница, естественно ограничено).
func (p *Pool) CostExploreStatus() (events int64, last map[string]time.Time) {
	p.mu.RLock()
	defer p.mu.RUnlock()
	last = make(map[string]time.Time, len(p.exploreLast))
	for k, ts := range p.exploreLast {
		// Клавиша realm+«\x1f"+model → Вывод "|«（JSON Безопасно читаемо;\x1f непечатаемый).
		last[strings.ReplaceAll(k, "\x1f", "|")] = ts
	}
	return p.costExploreEvents, last
}

// SetWeights Параметр компенсации простоя для взвешенной маршрутизации. Неположительные значения сохраняют исходное (по умолчанию).
func (p *Pool) SetWeights(idlePerHour, idleMax float64) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if idlePerHour > 0 {
		p.idleWeightPerHour = idlePerHour
	}
	if idleMax > 0 {
		p.idleWeightMax = idleMax
	}
}

// SetPreferExpiring переключатель маршрутизации "ближайший срок истечения — первым» (main Из config вызов после парсинга).
func (p *Pool) SetPreferExpiring(enabled bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.preferExpiring = enabled
}

// SetDegrade Инжект параметра понижения веса при серии неудач (main Из config Вызов после парсинга,issue #114）。
// неположительное значение сохраняет исходное (дефолт, см. defaultDegrade*），Стиль аналогичен SetBreaker/SetSoftRateMax。
func (p *Pool) SetDegrade(threshold int, cooldown, cooldownMax time.Duration) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if threshold > 0 {
		p.degradeThreshold = threshold
	}
	if cooldown > 0 {
		p.degradeCooldown = cooldown
	}
	if cooldownMax > 0 {
		p.degradeCooldownMax = cooldownMax
	}
}

// CreditFloor Пробрасывать действующее минимальное значение баллов (/status исп.).0 = Закрыто.
func (p *Pool) CreditFloor() int64 {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.creditFloor
}

// SetCreditFloor Инжект нижнего порога баллов (main Из config вызов после парсинга).
// 0 = Отключено (по умолчанию как есть, ноль — возврат); отрицательное невалидно, сохраняется исходное (0）。
// См. семантику Pool.creditFloor Комментарий к полю.
func (p *Pool) SetCreditFloor(n int64) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if n >= 0 {
		p.creditFloor = n
	}
}

// SetMaxInFlight Инжект макс. кол-ва in-flight запросов на аккаунт;0 = без лимита. Отрицательное сохраняет исходное.
func (p *Pool) SetMaxInFlight(n int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if n >= 0 {
		p.maxInFlight = n
	}
}

// SetMaxInFlightGlobal инжект global Лимит in-flight на аккаунт в домене (WAF 403 исправление P1-1 по уровням);
// 0 = Не задано,global Откат аккаунта maxInFlight（Без градации). Отрицательные значения сохраняются как есть.
func (p *Pool) SetMaxInFlightGlobal(n int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if n >= 0 {
		p.maxInFlightGlobal = n
	}
}

// inFlightLimit Сообщает действующий in-flight лимит аккаунта (global приоритет по уровням, fallback maxInFlight）；
// 0 = не ограничено. Вызывающая сторона должна уже иметь p.mu（или снапшот старше limit，См. Acquire）。
func (p *Pool) inFlightLimit(e *entry) int {
	if p.maxInFlightGlobal > 0 && e.a.Realm() == "global" {
		return p.maxInFlightGlobal
	}
	return p.maxInFlight
}

// SetStore инжект зеркала снимка состояния пула (redisstore.Store）。nil означает без зеркалирования (чисто локальное восстановление).
// Должно быть в SyncToDir предыдущий вызов, чтобы«Восстановление выбором нового«Происходит до выравнивания аккаунтов.
func (p *Pool) SetStore(s StoreSnapshotter) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.store = s
}

// RestoreFromSnapshot восстановление с выбором нового: сравнить локальное state.json и Redis снимок, берётся более новый.
// нет снапшота, снапшот без savedAt、или отсутствует локально/При невозможности чтения считается как«Приоритет локального/Пропустить снапшот«，
// Одновременно логировать источник восстановления. Должно быть в SyncToDir вызов до (SyncToDir только добавление/удаление без записи значений).
// Acquire для uid занимает один in-flight слот (вызов при попадании в sticky-сессию); в пределах лимита пула — возврат true。
// Квота израсходована entry.inFlight атомарный инкремент, при заполнении вернуть false。лимит на аккаунт realm разбивка по тарифам
// （global файл maxInFlightGlobal，P1-1；фолбэк не задан maxInFlight）。
func (p *Pool) Acquire(uid string) bool {
	p.mu.RLock()
	e, ok := p.byUID[uid]
	if !ok {
		p.mu.RUnlock()
		return false
	}
	limit := p.inFlightLimit(e)
	p.mu.RUnlock()
	if limit <= 0 {
		// без лимита: счётчик всё равно инкрементируется (для мониторинга), но отказ никогда не возвращается.
		e.inFlight.Add(1)
		return true
	}
	for {
		cur := e.inFlight.Load()
		if cur >= int64(limit) {
			return false
		}
		if e.inFlight.CompareAndSwap(cur, cur+1) {
			return true
		}
	}
}

// Release Освободить один in-flight слот. Идемпотентно уменьшить до 0 до ... (защита от двойного освобождения в минус).
func (p *Pool) Release(uid string) {
	p.mu.RLock()
	e, ok := p.byUID[uid]
	p.mu.RUnlock()
	if !ok {
		return
	}
	for {
		cur := e.inFlight.Load()
		if cur <= 0 {
			return
		}
		if e.inFlight.CompareAndSwap(cur, cur-1) {
			return
		}
	}
}

// SetRandomSource Только для тестов: инжект детерминированного источника случайности; в прод-коде не вызывать.
// Источник инъекции берется из n∈[0,n) после,pickWeighted результат розыгрыша полностью предсказуем.
func (p *Pool) SetRandomSource(fn func(n int64) int64) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.randInt64N = fn
}

// Add Добавить аккаунт; если уже существует — сохранить статус, обновить credentials (upsert один аккаунт, не влияет на другие аккаунты).
func (p *Pool) Add(a *auth.Auth) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.upsertLocked(a)
}

// SyncToDir Выровнять пул по последнему сканированию: новые аккаунты добавить, исчезнувшие удалить (состояние сохраняется).
// сохранение результата исключения обратно state.json，Чтобы удалённый аккаунт при следующем запуске не был load() восстановление.
func (p *Pool) SyncToDir(auths []*auth.Auth) {
	p.mu.Lock()
	defer p.mu.Unlock()
	seen := make(map[string]bool, len(auths))
	for _, a := range auths {
		seen[a.UID] = true
		p.upsertLocked(a)
	}
	changed := false
	for uid := range p.byUID {
		if !seen[uid] {
			delete(p.byUID, uid)
			changed = true
		}
	}
	if changed {
		p.saveLocked()
	}
}

// Remove Удалить аккаунт из пула с немедленной записью на диск (для админ-панели). Возвращает учетные данные удаленного аккаунта
// （Содержит FilePath，для удаления вызывающей стороной auth файл);uid не существует — вернуть nil。
// находящихся в полёте запросов Release Для удалённых записей — no-op，Ожидание не требуется.
func (p *Pool) Remove(uid string) *auth.Auth {
	p.mu.Lock()
	defer p.mu.Unlock()
	e, ok := p.byUID[uid]
	if !ok {
		return nil
	}
	delete(p.byUID, uid)
	p.dirty.Store(true)
	p.saveLocked()
	return e.a
}

// upsertLocked обновить или вставить один аккаунт; если существует — заменить только учетные данные, сохранить credits/cooling статус.
// вызывающая сторона должна уже удерживать p.mu；Add и SyncToDir Совместно использует это upsert Логика.
func (p *Pool) upsertLocked(a *auth.Auth) {
	if e, ok := p.byUID[a.UID]; ok {
		e.a = a // сохранить credits/cooling Статус
		return
	}
	p.byUID[a.UID] = &entry{a: a}
}

// Pick вернуть healthy аккаунт с макс. баллами; нет доступных — возврат nil。
