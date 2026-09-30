// Кулдаун и circuit breaker:Cooldown（фиксированный по длительности кулдаун на уровне аккаунта),CooldownSoftRate（Мягкий кулдаун на уровне аккаунта, выравнивание с
// время сброса upstream или ограниченный бэкофф),CooldownSoftForModel（Мягкий кулдаун на уровне модели, выравнивание по wall-clock сброса),
// BlockModelBackoff/Clear（11102 отрицательный кэш), лимит мягкого охлаждения, накопление срабатываний circuit breaker, разморозка чекина.
package pool

import (
	"strings"
	"time"
)

func (p *Pool) SetCredits(uid string, credits, total int64) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if e, ok := p.byUID[uid]; ok {
		e.credits = credits
		e.creditsTotal = total
		p.dirty.Store(true)
	}
}

// NoteCheckinDone Отметить аккаунт как отметившийся сегодня (успешная отметка и апстрим"Сегодня уже отмечено"идемпотентные отклонения тоже считаются).
// запись локальной даты, истекает естественно после полуночи; кулдаун не затрагивается/статус отключено (домены регистрации и охлаждения ортогональны).
func (p *Pool) NoteCheckinDone(uid string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if e, ok := p.byUID[uid]; ok {
		day := time.Now().Format("2006-01-02")
		if e.lastCheckinDay != day {
			e.lastCheckinDay = day
			p.dirty.Store(true)
		}
	}
}

// SetCreditsDetailed обновить баланс аккаунта/общая сумма, набор скоро истекающих в окне конфигурации и ближайшее будущее
// Партии с истёкшим сроком.earliestAt При нуле или не в будущем очистить самую раннюю партию;expiring/earliestRemaining
// все clamp к [0, credits]，Исключить загрязнение выбора номера грязными данными апстрима.
func (p *Pool) SetCreditsDetailed(uid string, credits, total, expiring int64, earliestAt time.Time, earliestRemaining int64) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if e, ok := p.byUID[uid]; ok {
		if credits < 0 {
			credits = 0
		}
		if expiring < 0 {
			expiring = 0
		}
		if expiring > credits {
			expiring = credits
		}
		now := time.Now()
		if earliestRemaining < 0 {
			earliestRemaining = 0
		}
		if earliestRemaining > credits {
			earliestRemaining = credits
		}
		if earliestAt.IsZero() || !earliestAt.After(now) || earliestRemaining == 0 {
			earliestAt = time.Time{}
			earliestRemaining = 0
		}
		e.credits = credits
		e.creditsTotal = total
		e.creditsExpiring = expiring
		e.creditsEarliestExpiry = earliestAt
		e.creditsEarliestRemaining = earliestRemaining
		p.dirty.Store(true)
	}
}

// ClearExpiringSnapshots Очистить скоро истекающие у всех аккаунтов/Кэш с ближайшим истечением. Вызывается при изменении окна конфигурации,
// Исключить использование маршрутных данных старого окна до записи нового снапшота; пересоздастся при следующем check-in или обновлении баланса.
func (p *Pool) ClearExpiringSnapshots() {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, e := range p.byUID {
		e.creditsExpiring = 0
		e.creditsEarliestExpiry = time.Time{}
		e.creditsEarliestRemaining = 0
	}
	p.dirty.Store(true)
}

// Cooldown Охладить аккаунт до now+d（мгновенное охлаждение:CoolHard Баланс исчерпан / CoolSoft Фиксированный короткий кулдаун).
//
// После рефакторинга эта точка входа — "кулдаун уровня аккаунта с фиксированной длительностью», больше не выполняет две старые задачи:
// - Больше не кормить счетчик ошибок брейкера: брейкер только на "повторные сбои» (NoteError，5xx）Бэкофф.
// мягкий rate limit/У каждого исчерпанного баланса есть авторитетный момент восстановления (сброс wall-clock / 04:00 чекин), затем смерджить"Последовательные сбои"
// приведет к накоплению очереди нормальных ретраев пользователя. Семантика circuit breaker определяется NoteError Единственный драйвер (с until сохраняется ортогональность).
// - Больше не выполнять softStreak Экспоненциальное накопление: фикс. d т.е. итоговая длительность.CoolSoft для точного выравнивания используйте
// CooldownSoftRate（ограничено, выровнено по времени сброса апстрима).
func (p *Pool) Cooldown(uid string, kind CoolKind, d time.Duration, reason string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if e, ok := p.byUID[uid]; ok {
		e.until = time.Now().Add(d)
		e.coolKind = kind
		e.reason = reason
		// вход не модельного охлаждения: очищает модельное охлаждение, участвующее в роутинге, чтобы избежать предыдущего
		// утечка исключения модели на кулдаун уровня аккаунта;AuditOnly Запись не влияет на роутинг, оставить отображение.
		clearRoutingModelCooldownsLocked(e)
		p.dirty.Store(true)
	}
}

// CooldownSoftForModel 429 **Уровень модели**вход мягкого кулдауна (issue #31）：сдвинуть дедлайн охлаждения этой модели
// точное выравнивание по wall-clock сброса апстрима (без экспоненциального накопления, без softStreak подсчёт).
//
// - resetAt Ненулевое (со временем парсинга)→ modelCooldowns[model].Until = min(resetAt,
// now+softRateMax)，ResetAt Логирование исходного wall-clock апстрима (реестр ResetAt）。не записывать until
// （глобальный кулдаун аккаунта не загрязняется лимитом модели), смена модели сразу доступна (исключение модели).
// - resetAt Нулевое значение (без текста времени)→ Ограниченный бэкофф:base начиная с — по softStreak Удвоение, потолок
// softRateMax，И**в мягком кулдауне**（until не истёк) не продвигать/без продления (fallback-проба больше не
// кулдаун накапливается). Модель не логируется (без исключения).
//
// Отличие от старой реализации: при наличии времени сброса апстрима экспоненциальное наращивание не применяется; без времени сброса — фолбэк "в охлаждении»
// зондировать повторно 429」Больше не softStreak++ удвоение — именно это "весь пул сдвинут в 2h Причина лимита.
func (p *Pool) CooldownSoftForModel(uid string, base time.Duration, resetAt time.Time, model, reason string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if e, ok := p.byUID[uid]; ok {
		now := time.Now()
		if !resetAt.IsZero() {
			// есть время сброса апстрима: конец кулдауна = min(resetAt, now+softRateMax)，без экспоненциального увеличения.
			if e.modelCooldowns == nil {
				e.modelCooldowns = map[string]modelCooldown{}
			}
			e.modelCooldowns[model] = modelCooldown{
				Until: p.cappedSoftUntilLocked(now, resetAt),
				ResetAt: resetAt,
				Reason: reason,
			}
		} else {
			// без времени парсинга (обычный мягкий кулдаун): ограниченный backoff (base начиная с — по softStreak Удвоение, потолок
			// softRateMax）。Внимание:**в мягком кулдауне**（until не истёк) не продвигать/Не продлевать.
			if e.coolKind != CoolSoft || !now.Before(e.until) {
				d := p.softDurationLocked(base, e.softStreak+1)
				e.softStreak++
				e.until = now.Add(d)
			}
			e.coolKind = CoolSoft
			e.reason = reason
			clearRoutingModelCooldownsLocked(e)
		}
		p.dirty.Store(true)
	}
}

// RecordModelRateLimitAudit логировать неучаствующие в роутинге моделей 6004 Элемент отображения.
// Типичный сценарий — 6004 Нет парсируемого времени сброса: аккаунт остаётся на прежнем ограниченном бэкофф-кулдауне,
// Данный метод только привязывает имя модели к e.until Отображение на странице аккаунтов апстрима, не влияет на healthyForModel。
func (p *Pool) RecordModelRateLimitAudit(uid, model, reason string) {
	if uid == "" || model == "" {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	e, ok := p.byUID[uid]
	if !ok {
		return
	}
	now := time.Now()
	if old, exists := e.modelCooldowns[model]; exists && !old.AuditOnly && old.Until.After(now) {
		return // уже есть реальный кулдаун модели, аудит-запись не должна перезаписывать deadline маршрутизации
	}
	until := e.until
	if until.IsZero() || !until.After(now) {
		until = now.Add(p.softRateMaxOr())
	}
	if e.modelCooldowns == nil {
		e.modelCooldowns = make(map[string]modelCooldown)
	}
	e.modelCooldowns[model] = modelCooldown{
		Until: until,
		Reason: reason,
		AuditOnly: true,
	}
	p.dirty.Store(true)
}

// clearRoutingModelCooldownsLocked Удалить кулдаун модели, участвующий в исключении выбора, сохранить AuditOnly Журнал учета.
// вызывающая сторона должна уже удерживать p.mu Блокировка записи.
func clearRoutingModelCooldownsLocked(e *entry) {
	for model, mc := range e.modelCooldowns {
		if !mc.AuditOnly {
			delete(e.modelCooldowns, model)
		}
	}
	if len(e.modelCooldowns) == 0 {
		e.modelCooldowns = nil
	}
}

// modelBlock TTL Константа (11102 бэкофф негативного кэша):
// первое попадание 6h；после истечения half-open разрешить пробный пропуск, при повторном попадании TTL = base × 2^min(hits-1, shift)；
// Потолок 24h（максимум одна повторная попытка в день). При успешном запросе этой модели BlockModelClear очистить.
const (
	modelBlockBaseTTL = 6 * time.Hour
	modelBlockShift = 4
	modelBlockMaxTTL = 24 * time.Hour
)

// BlockModelBackoff 11102「бэкенд не имеет этой модели» (Аккаунт, Модель) Вход отрицательного кэша
// （handler.applyErrorPolicy вызова). Повторное использование modelCooldowns механизм (без создания параллельного состояния):
// Запись modelCooldowns[model]，Until — экспоненциальный бэкофф TTL，сторона выбора аккаунта healthyForModel автоматически для этого
// Аккаунт обходит эту модель.
//
// Семантика и 6004 ортогонально:6004 это "модель под rate-limit, выравнивание сброса по wall-clock», данный вход — "официально подтверждено, что бэкенд
// модели нет, ретрай бессмысленен, только смена модели/сменить аккаунт».resetAt передавать не нужно (11102 без текста сброса),
// ResetAt сохранять нулевое значение, с 6004 общий реестр Until решение —11102 Запись будет как 11102 reason появляется в
// /status Журнал учета, виден для эксплуатации.
func (p *Pool) BlockModelBackoff(uid, model, reason string) {
	if uid == "" || model == "" {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	e, ok := p.byUID[uid]
	if !ok {
		return
	}
	now := time.Now()
	hits := 0
	if e.modelCooldowns != nil {
		hits = e.modelCooldowns[model].Hits
	}
	hits++
	ttl := modelBlockBaseTTL
	if d := ttl * (1 << uint(min(hits-1, modelBlockShift))); d < modelBlockMaxTTL {
		ttl = d
	} else {
		ttl = modelBlockMaxTTL
	}
	if e.modelCooldowns == nil {
		e.modelCooldowns = map[string]modelCooldown{}
	}
	e.modelCooldowns[model] = modelCooldown{
		Until: now.Add(ttl),
		Reason: reason,
		Hits: hits,
	}
	p.dirty.Store(true)
}

// BlockModelClear Очистить (Аккаунт, Модель) 11102 Запись негативного кэша (модель снова фактически доступна). Полуоткрытое зондирование
// или вызов после успешного штатного запроса к этой модели (handler успешный путь). Очищать только 11102 записей, не трогать 6004 независимый
// Таблица кулдауна —6004 имеет собственную wall-clock семантику сброса апстрима, при успехе не затирать.reason Различение по префиксу:
// 11102 элемента reason Всегда как "11102« Начало (см. upstream.BlockModelReason）。
func (p *Pool) BlockModelClear(uid, model string) {
	if uid == "" || model == "" {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	e, ok := p.byUID[uid]
	if !ok || len(e.modelCooldowns) == 0 {
		return
	}
	mc, exists := e.modelCooldowns[model]
	if !exists || !strings.HasPrefix(mc.Reason, "11102") {
		return
	}
	delete(e.modelCooldowns, model)
	if len(e.modelCooldowns) == 0 {
		e.modelCooldowns = nil
	}
	p.dirty.Store(true)
}

// CooldownSoftRate 429/текста rate-limit'а**Уровень аккаунта**вход мягкого кулдауна (handler.applyErrorPolicy вызов).
//
// Семантика:
// - resetAt Ненулевое (апстрим несёт авторитетное время сброса, независимо 6004 или 11140 rate-limiting）→
// На уровне аккаунта до этих wall-clock (усечено до softRateMax，никогда не накапливается экспоненциально);**Не**В
// modelCooldowns учет модели (семантика уровня аккаунта, без исключения при переключении модели — обычный лимит уровня аккаунта не должен
// из-за обхода при переключении модели).
// - resetAt нулевое значение и**Не в кулдауне**（Впервые/новый лимит после восстановления)→ Ограниченный backoff: по softStreak
// экспоненциальный бэкофф с потолком softRateMax。softStreak Считается только при реальном входе в новый период охлаждения,
// NoteSuccess/reviveCoolingLocked Сброс в ноль (с семантикой восстановления).
// - resetAt нулевое значение и**Уже в мягком кулдауне**（резервный проб снова попал 429）→ не продвигать streak、не продлевается
// until：повтор пользователя/Параллельные фолбэк-пробы не должны наращивать кулдаун — именно в старой реализации "чем больше ретраев, тем холоднее,
// Весь пул сдвинут в 2h виновник "потолка» (каждый пробный запрос softStreak++ экспоненциальное удвоение).
func (p *Pool) CooldownSoftRate(uid string, base time.Duration, resetAt time.Time, reason string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if e, ok := p.byUID[uid]; ok {
		now := time.Now()
		if !resetAt.IsZero() {
			e.until = p.cappedSoftUntilLocked(now, resetAt)
		} else if e.coolKind != CoolSoft || !now.Before(e.until) {
			// Новый лимит (вне эффективного soft-кулдауна): продвигать ограниченный бэкофф; фолбэк-проба (всё ещё в soft-кулдауне) без удвоения.
			d := p.softDurationLocked(base, e.softStreak+1)
			e.softStreak++
			e.until = now.Add(d)
		}
		e.coolKind = CoolSoft
		e.reason = reason
		clearRoutingModelCooldownsLocked(e) // Мягкий кулдаун уровня аккаунта: очистка маршрута — исключение, аудит-лог сохраняется
		p.dirty.Store(true)
	}
}

// cappedSoftUntilLocked Обрезать wall-clock сброса апстрима до softRateMax（now+softRateMax и resetAt
// Берется более ранний).resetAt Уже истек (смещение часов/текст просрочен) длительность зажимается к полуночи, немедленное восстановление.
// вызывающая сторона должна уже удерживать p.mu。
func (p *Pool) cappedSoftUntilLocked(now, resetAt time.Time) time.Time {
	cap := now.Add(p.softRateMaxOr())
	if resetAt.After(cap) {
		return cap
	}
	if resetAt.After(now) {
		return resetAt
	}
	return now.Add(time.Millisecond)
}

// softRateMaxOr Возвращает действующий softRateMax（При отсутствии инъекции — по умолчанию 2h），для расчета лимита.
// вызывающая сторона должна уже удерживать p.mu。
func (p *Pool) softRateMaxOr() time.Duration {
	if p.softRateMax > 0 {
		return p.softRateMax
	}
	return defaultSoftRateMax
}

// softDurationLocked по числу последовательных мягких кулдаунов масштабировать базу d Экспоненциальное усиление:d << (streak-1)，Потолок softRateMax。
// softRateMax Не инжектирован (<=0）При ... по defaultSoftRateMax считать.streak<=1 вернуть как есть при d。
// кол-во бит сдвига влево ограничено softStreakShiftMax Ограничение, избегать streak При очень большом значении — переполнение сдвига.
// вызывающая сторона должна уже удерживать p.mu。
func (p *Pool) softDurationLocked(d time.Duration, streak int) time.Duration {
	if streak <= 1 {
		return d
	}
	shift := streak - 1
	if shift > softStreakShiftMax {
		shift = softStreakShiftMax
	}
	d <<= shift
	max := p.softRateMax
	if max <= 0 {
		max = defaultSoftRateMax
	}
	if d > max || d <= 0 { // d<=0：Переполнение при сдвиге влево даёт отрицательное/ноль, также fallback по верхнему лимиту
		d = max
	}
	return d
}

// recordBreakerFailureLocked засчитать одну неудачу трипа; при достижении порога — трип с экспоненциальным backoff.
// Circuit breaker и кулдаун (until）Развязка: cooldown — фикс. длительность по типу ошибки, circuit breaker — для"Повторные сбои"Последовательное продление бана.
// вызывающая сторона должна уже удерживать p.mu。
func (p *Pool) recordBreakerFailureLocked(e *entry) {
	e.fails++
	if e.fails < p.breakerThreshold {
		return
	}
	d := p.breakerCooldown
	for i := 0; i < e.retryCount; i++ {
		d *= 2
		if d >= p.breakerCooldownMax {
			d = p.breakerCooldownMax
			break
		}
	}
	// Срабатывание circuit breaker: сбросить счетчик ошибок для накопления в следующем раунде;retryCount Инкрементально увеличивать экспоненту бэкоффа.
	e.fails = 0
	e.retryCount++
	e.breakerUntil = time.Now().Add(d)
}

// CooldownUntilTomorrow4AM Кулдаун до следующего 04:00（локальный часовой пояс).
// Используется для ErrHardCredit Сценарий: задачи чекина для аккаунтов с исчерпанными баллами (09:00/21:00）Восстановление.
func (p *Pool) CooldownUntilTomorrow4AM(uid string, reason string) {
	now := time.Now()
	p.Cooldown(uid, CoolHard, nextDay4AM(now).Sub(now), reason)
}

// nextDay4AM вернуть now ближайший после 04:00（и now Один часовой пояс).
// now в тот же день 04:00 до (раннее утро 00:00~04:00）возвращать текущий день при 04:00——На этот момент чекин еще не выполнен,
// Жесткий кулдаун, сработавший в этом окне, восстанавливается чекином в тот же день; возврат на следующий день даст холостой кулдаун около суток.
// 04:00 целое и после возвращает следующий день 04:00。
// time.Date переполнение по дням с автопереносом (конец месяца→Следующий месяц 1 число, конец года→Следующий год 1 номер), естественно покрывает переход через сутки/Кросс-месяц/Сквозь год.
func nextDay4AM(now time.Time) time.Time {
	if now.Hour() < 4 {
		return time.Date(now.Year(), now.Month(), now.Day(), 4, 0, 0, 0, now.Location())
	}
	return time.Date(now.Year(), now.Month(), now.Day()+1, 4, 0, 0, 0, now.Location())
}

// ReenableIfCredits check-in/Разморозка после обновления баланса: только когда remain > 0 и если аккаунт не отключен, разморозить
// **Кулдаун при исчерпании баланса**（CoolHard）。Мягкий лимит (CoolSoft）и реестр на уровне модели (modelCooldowns）
// Здесь не сбрасывается — признаком восстановления является истечение wall-clock сброса апстрима, а не восстановление баланса (период обновления баланса
// Задача каждые 5 мин доходит сюда, полная очистка сожмёт фактическое время жизни кулдауна лимита до одного цикла обновления).
// Внимание: не трогать circuit breaker — истечение блокировки (breakerUntil истечения) или в следующий раз chat успех (NoteSuccess）только тогда восстанавливается.
// reviveCoolingLocked Уже перенесено в transition.go（Единственная авторитетная реализация перехода конечного автомата).
