// Эволюция и опрос статуса аккаунта: отключено/12153 проверка непрерывного счетчика, учет успехов и ошибок, разморозка/реанимация,
// и запрос статуса (Status/AvailableUIDs/PickByUID/CountsDetailed/ServableNow/List）。
package pool

import (
	"log"
	"sort"
	"strings"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/logfmt"
)

func (p *Pool) Disable(uid, reason string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if e, ok := p.byUID[uid]; ok {
		p.disableLocked(e, reason)
	}
}

// NoteSessionDead записать один раз ErrSessionDead（12153）——**Не отключать сразу**。
// старое поведение — один раз 12153 т.е. Disable，Но 12153 Будет срабатывать временно (сетевой джиттер/Кратковременный сбой апстрима/refresh
// гонка), одна ошибка с перманентным баном убьет здоровые аккаунты (P0-1 Разведка:13 шт. disabled все № refresh
// успех — жертва исторической ложной классификации). Сменить на непрерывные sessionDeadThreshold раз до отключения:
// Счетчик +1，достигнут порог → Disable（reason=12153 session dead）и сбросить счетчик;
// refresh Успех / Любой успех / ручное восстановление → ClearSessionDead Сброс счетчика.
// вернуть true Означает достижение порога и выполнение отключения в этот раз.
// Даже если аккаунт уже disabled，Счётчик всё равно накапливается и возвращается false Перед N-1 раз — но keepalive будет пропущено
// disabled номер, фактически только "уже disabled "воскрес после и счетчик не сброшен» — только в таких сценариях сюда попадает.
func (p *Pool) NoteSessionDead(uid string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	e, ok := p.byUID[uid]
	if !ok {
		return false
	}
	e.sessionDeadFails++
	if e.sessionDeadFails < sessionDeadThreshold {
		return false
	}
	e.sessionDeadFails = 0
	p.disableLocked(e, sessionDeadReason)
	return true
}

// ClearSessionDead Сброс последовательных 12153 счётчик — вызывается в любой момент подтверждения живости аккаунта:
// refresh успех (RunKeepaliveNow）、chat успех (NoteSuccess）、ручное восстановление (ReviveDisabled）。
func (p *Pool) ClearSessionDead(uid string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if e, ok := p.byUID[uid]; ok {
		e.sessionDeadFails = 0
	}
}

// ReviveDisabled вручную/точка восстановления эндпоинта: очистка disabled + reason + непрерывно 12153 Подсчёт,
// Аккаунт возвращается в пул (если нет другого cooldown/при срабатывании circuit breaker сразу доступен для выбора, health-check подхватывает автоматически).
// **Не изменять** Disabled При выборе номера/Существующая семантика эндпоинта статуса:disabled номер по-прежнему не участвует в выборе,
// До воскрешения данным методом. Несуществующий uid No-op.
func (p *Pool) ReviveDisabled(uid string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if e, ok := p.byUID[uid]; ok && e.disabled {
		e.disabled = false
		e.reason = ""
		e.sessionDeadFails = 0
		p.dirty.Store(true)
	}
}

// Revive в эксплуатационной метрике"Безусловное восстановление"：Сброс блокировки, кулдауна (вкл. счетчик мягкого бэкоффа) и состояния circuit breaker.
// и ReviveDisabled（только сброс отключения) и ReenableIfCredits（только сброс охлаждения, без затрагивания circuit breaker) отличие:
// Метод сбрасывает все штрафные состояния, для панели управления"Разморозка"Использование кнопки — ручное восстановление доступности номера в один клик при решении.
// uid не существует — вернуть false（Для различения вызывающей стороной"Аккаунт не существует«и»уже восстановлен"）。
func (p *Pool) Revive(uid string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	e, ok := p.byUID[uid]
	if !ok {
		return false
	}
	e.disabled = false
	e.until = time.Time{}
	e.coolKind = 0
	e.reason = ""
	e.softStreak = 0
	e.modelCooldowns = nil // освобождение от rate limit уровня модели сбрасывается вместе с кулдауном (защита от утечки в последующий rate limit уровня аккаунта)
	e.sessionDeadFails = 0
	e.fails = 0
	e.retryCount = 0
	e.breakerUntil = time.Time{}
	p.dirty.Store(true)
	return true
}

// reviveCoolingLocked Размораживать только cooldown по исчерпанию баланса (CoolHard until/coolKind/reason）И обновить
// credits，Не трогать circuit breaker (fails/retryCount/breakerUntil）、бэкофф мягкого лимита (CoolSoft/softStreak）
// и реестр на уровне модели (modelCooldowns）——доказательство восстановления после rate-limit — истечение wall-clock сброса, а не восстановление баланса.
// check-in/Разморозка обновления баланса здесь: восстановление баланса лишь доказывает billing Канал здоров ≠ доказательство chat Канал исправен,
// Circuit breaker (подряд 5xx сигнал) и кулдаун лимита не должны перекрываться обновлением баланса.
// вызывающая сторона должна уже удерживать p.mu。
func (p *Pool) ReenableIfCredits(uid string, remain, total int64) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if e, ok := p.byUID[uid]; ok {
		if remain > 0 && !e.disabled {
			p.reviveCoolingLocked(e, remain, total)
		} else {
			e.credits = remain
			e.creditsTotal = total
		}
		// ReenableIfCredits только контекст агрегированного баланса; детали истечения должны SetCreditsDetailed
		// Перезаписать, нельзя использовать старое окно/Кэш старого батча.
		e.creditsExpiring = 0
		e.creditsEarliestExpiry = time.Time{}
		e.creditsEarliestRemaining = 0
		p.dirty.Store(true)
	}
}

// NoteError записать ошибку: подать в единственный счетчик последовательных неудач fails + накопленные ошибки errTotal。
// достигнуто breakerThreshold Триггер circuit breaker (экспоненциальный бэкофф), семантика последовательных сбоев полностью перенесена в прерыватель (больше нет отдельного err кулдаун).
func (p *Pool) NoteError(uid string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if e, ok := p.byUID[uid]; ok {
		e.errTotal++
		e.lastErr = time.Now()
		p.recordBreakerFailureLocked(e)
		p.dirty.Store(true)
	}
}

// NoteSuccess Успешный запрос инкрементирует счетчик успехов, обновляет lastSuccess，и сбросить состояние последовательных ошибок и circuit breaker.
// бинарная модель: сброс fails + retryCount + breakerUntil；Не трогать until/coolKind（это мгновенный кулдаун, истекает отдельно).
// Дополнительно очистить softStreak：Успех — сильнейшее доказательство восстановления аккаунта, счетчик последовательных soft-лимитов сбрасывается в ноль, бэкофф возвращается к базовому.
// Аналогично очистить sessionDeadFails и счётчик понижения за серию неудач (consecutiveFails/degradeUntil，issue #114）：
// Успех подтверждает доступность аккаунта, счётчик серии неудач и дедлайн временного вывода из пула сбрасываются.
// **Не трогать modelCooldowns**：6004 Уровень модели limit Таймер на модель независим, успех другой модели не сбрасывает
// Конец охлаждения данной модели (как раз"Независимо для каждой модели"семантика). Охлаждение на уровне модели только по истечении/восстановление/Уровень аккаунта
// кулдаун (Cooldown/reviveCoolingLocked）очистить.11102 Успешная очистка записи выполняется handler В
// при успехе и попадании в модель явно вызывать BlockModelClear（6004 неясно, семантика отличается).
func (p *Pool) NoteSuccess(uid string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if e, ok := p.byUID[uid]; ok {
		e.successCount++
		e.lastSuccess = time.Now()
		e.fails = 0
		e.retryCount = 0
		e.breakerUntil = time.Time{}
		e.softStreak = 0
		e.sessionDeadFails = 0
		e.consecutiveFails = 0
		e.degradeUntil = time.Time{}
		p.dirty.Store(true)
	}
}

// NoteModelCost записать фактическое наблюдение списания, обновить (Аккаунт, Модель) Книга учета затрат, и попутно
// Списание баланса аккаунта (credits/creditsExpiring/самая ранняя истекающая партия).credit для апстрима usage.credit（фактический в этот раз
// Списание=объем потребления),tokens Для текущего запроса token Всего (prompt+completion，Для пересчета единиц
// стоимость).tokens<=0 Не записывать: невозможно пересчитать цену за единицу, запись загрязнит ledger.
//
// использовать EMA Сглаживание (alpha=0.3，Около 5 сходимость за N наблюдений): единичный выброс не определяет выбор номера.
// Журнал сохраняется в state.json（stateAccount.ModelCosts）：после рестарта знания о стоимости сохраняются,
// лимит free/Ночной бесплатный кросс-рестарт без повторного платного пробинга; запись на диск/Восстановление всё по modelCostTTL
// Ленивая фильтрация — устаревшие цены (временные скидки) не переносятся через TTL восстановление.
// Событие окончания лимитированного бесплатного периода:tier 0 наблюдение (per1k≤0）Получено credit>0 При перекрытии наблюдения писать явный лог
// （Эксплуатация по этому понимает"Бесплатный обед окончен"），Проверка на входе записи, смотреть только значение до перезаписи.
//
// credits обратная запись вне чекина:credit это для текущего запроса**Расход**，это не остаточный баланс. Попутно списать
// credits и снапшот истечения, фактор баланса выбора номера сходится в реальном времени по мере расхода — старый подход только при чекине
// （Ежедневно 09:00/21:00 дважды) обновление, между двумя check-in (макс. 12h）Высокозатратный аккаунт сохраняет высокий вес до
// Попадание в пустоту 402；global Аккаунт не чекинится,credits Ранее был перманентный бан. Check-in всё равно периодически перезаписывает
// （ReenableIfCredits/SetCreditsDetailed по authoritative сброс баланса), списание — лишь
// Интерполированная оценка между двумя чекинами;credit=0（бесплатный запрос) баланс не списывать.
func (p *Pool) NoteModelCost(uid, model string, credit float64, tokens int) {
	if uid == "" || model == "" || tokens <= 0 {
		return
	}
	// Цена за тысячу token нормализация, устранение разницы длины запроса.
	per1k := credit / float64(tokens) * 1000
	if per1k < 0 {
		per1k = 0
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	e, ok := p.byUID[uid]
	if !ok {
		return
	}
	if credit > 0 {
		d := int64(credit + 0.5) // Округление
		if d > e.credits {
			d = e.credits // Зажим 0：Списание насквозь (задержка сверки/Потребление раньше учёта) не создаёт отрицательный баланс
		}
		e.credits -= d
		if e.creditsExpiring > 0 {
			if d > e.creditsExpiring {
				e.creditsExpiring = 0
			} else {
				e.creditsExpiring -= d
			}
		}
		if e.creditsEarliestRemaining > 0 {
			if d >= e.creditsEarliestRemaining {
				e.creditsEarliestRemaining = 0
				e.creditsEarliestExpiry = time.Time{}
			} else {
				e.creditsEarliestRemaining -= d
			}
		}
	}
	if e.modelCost == nil {
		e.modelCost = make(map[string]modelCostEntry)
	}
	const alpha = 0.3
	prev, seen := e.modelCost[model]
	if !seen {
		e.modelCost[model] = modelCostEntry{CostPer1k: per1k, LastSeen: time.Now(), Samples: 1}
	} else {
		// Событие окончания лимитированного бесплатного периода (проверка на входе записи, учитывается только значение до перезаписи): до этого tier 0（фактически бесплатно,
		// per1k≤0）и данный замер платный (per1k>0）——бесплатное окно аккаунта для этой модели завершено.
		if prev.CostPer1k <= 0 && per1k > 0 {
			log.Printf("[pool] model %s on uid %s: free tier ended, now %.3f credits/1k", model, logfmt.UID8(uid), per1k)
		}
		e.modelCost[model] = modelCostEntry{
			CostPer1k: prev.CostPer1k*(1-alpha) + per1k*alpha,
			LastSeen: time.Now(),
			Samples: prev.Samples + 1,
		}
	}
	p.dirty.Store(true) // Леджер персистирован: все точки записи помечают dirty
}

// RecordTokenUsage Логировать одну фактическую попытку чат-аккаунта и ответ апстрима usage Инкремент.
// usage при отсутствии поля счетчик запросов всё равно инкрементируется, но учитываются только явно существующие token Поле.
func (p *Pool) RecordTokenUsage(uid string, delta TokenUsageDelta) {
	p.mu.Lock()
	defer p.mu.Unlock()
	e, ok := p.byUID[uid]
	if !ok {
		return
	}
	usage := &e.tokenUsage
	usage.RequestCount++
	usage.LastUsedAt = time.Now()
	if delta.Model != "" {
		usage.LastModel = delta.Model
	}
	known := false
	if delta.HasPromptTokens && delta.PromptTokens >= 0 {
		usage.PromptTokens += delta.PromptTokens
		known = true
	}
	if delta.HasCompletionTokens && delta.CompletionTokens >= 0 {
		usage.CompletionTokens += delta.CompletionTokens
		known = true
	}
	if delta.HasTotalTokens && delta.TotalTokens >= 0 {
		usage.TotalTokens += delta.TotalTokens
		known = true
	}
	if known {
		usage.UsageCount++
	}
	if delta.HasLatencyMs && delta.LatencyMs >= 0 {
		usage.LastLatencyMs = delta.LatencyMs
	}
	if delta.HasTokensPerSecond && delta.TokensPerSecond >= 0 {
		speed := delta.TokensPerSecond
		usage.LastTokensPerSecond = &speed
	} else {
		// сбой или отсутствие completion_tokens не показывать старую пропускную способность предыдущего запроса.
		usage.LastTokensPerSecond = nil
	}
	p.dirty.Store(true)
}

// Status запрос статуса одного аккаунта.
func (p *Pool) Status(uid string) (Status, bool) {
	p.mu.RLock()
	defer p.mu.RUnlock()
	e, ok := p.byUID[uid]
	if !ok {
		return Status{}, false
	}
	return p.statusOf(uid, e), true
}

// AuthByUID вернуть полные учетные данные аккаунта (для планировщика/для ops-интерфейса).
func (p *Pool) AuthByUID(uid string) *auth.Auth {
	p.mu.RLock()
	defer p.mu.RUnlock()
	if e, ok := p.byUID[uid]; ok {
		return e.a
	}
	return nil
}

// AvailableUIDs Вернуть текущий healthy аккаунты, не исчерпавшие лимит in-flight UID список (по UID сортировка, стабильный вывод).
// Для sticky-маршрутизации сессии (internal/session）Выполнить быструю проверку попадания + двухсегментное распределение; если нет доступных — возвращается пустой слайс.
func (p *Pool) AvailableUIDs() []string {
	p.mu.RLock()
	defer p.mu.RUnlock()
	now := time.Now()
	uids := make([]string, 0, len(p.byUID))
	for uid, e := range p.byUID {
		if !e.healthy(now) {
			continue
		}
		if p.inFlightFull(e) {
			continue
		}
		uids = append(uids, uid)
	}
	sort.Strings(uids)
	return uids
}

// AvailableUIDsForModel Совм. AvailableUIDs，но заменить критерий health на healthyForModel：
// на этой модели 6004 Аккаунты с rate limit не включаются, а в**другие модели**аккаунты под rate limit включаются как обычно (исключение по модели).
// Для sticky-сессии: распределение по модели и проверка попадания;model при пустом эквивалентно AvailableUIDs。
func (p *Pool) AvailableUIDsForModel(model string) []string {
	p.mu.RLock()
	defer p.mu.RUnlock()
	now := time.Now()
	uids := make([]string, 0, len(p.byUID))
	for uid, e := range p.byUID {
		if !e.healthyForModel(now, model) {
			continue
		}
		if p.inFlightFull(e) {
			continue
		}
		uids = append(uids, uid)
	}
	sort.Strings(uids)
	return uids
}

// PickByUIDForModel Совм. PickByUID，но используется healthyForModel Проверка: привязанный номер в текущей модели
// 6004 при троттлинге возвращает nil，Позволяет вызывающей стороне (handler）Отвязать и откатиться к обычной ротации.
// Это sticky-производительность«Ротация возможна«ключевое: привязка хранит только uid，Если только на уровне аккаунта healthy Валидация,
// Аккаунт, попавший под лимит на уровне модели (аккаунт в целом здоров), будет выбираться до исчерпания лимита ротаций.
func (p *Pool) PickByUIDForModel(uid, model string) *auth.Auth {
	p.mu.Lock()
	defer p.mu.Unlock()
	e, ok := p.byUID[uid]
	if !ok {
		return nil
	}
	now := time.Now()
	if !e.healthyForModel(now, model) {
		return nil
	}
	// Гарантия баллов (липкий путь): с pick floorBlocked Тот же критерий — достижение дна + фактически платно — сразу блокировать.
	// вернуть nil После handler Отвязка sticky на стороне (unbindSticky）Обычная ротация со сменой номера, sticky-номер восстанавливается
	// после — перепривязка в следующей сессии.
	// частота логов: естественно не более одного на запрос — первый возврат nil сразу отвязывается, при дальнейшей ротации в этот путь больше не попадает
	// （доп. троттлинг не требуется); при sticky-продлении — по одному на каждый новый запрос, как раз непрерывное напоминание "баланс всё ещё оффлайн».
	if p.floorBlockedForModel(e, model, now) {
		log.Printf("WARN: [pool] credit floor: sticky acct=%s model=%s credits=%d < floor=%d, unbind (paid model held out)",
			logfmt.Label(e.a.UID, e.a.Nickname), model, e.credits, p.creditFloor)
		return nil
	}
	if p.inFlightFull(e) {
		return nil
	}
	e.lastUsed = now
	p.pickSeq++
	e.usedSeq = p.pickSeq
	return e.a
}

// PickByUID Если uid Текущий healthy и лимит in-flight не исчерпан, вернуть его credential (запись lastUsed антиколлизионный номер);
// Иначе вернуть nil。Для проверки попадания sticky-маршрутизации сессии и прямого доступа.
func (p *Pool) PickByUID(uid string) *auth.Auth {
	p.mu.Lock()
	defer p.mu.Unlock()
	e, ok := p.byUID[uid]
	if !ok {
		return nil
	}
	now := time.Now()
	if !e.healthy(now) {
		return nil
	}
	if p.inFlightFull(e) {
		return nil
	}
	e.lastUsed = now
	p.pickSeq++
	e.usedSeq = p.pickSeq
	return e.a
}

// CountsDetailed вернуть total/healthy/cooling/disabled/inFlightFull Счетчик пяти категорий.
// cooling Включая обычный кулдаун (until）и период circuit-breaker (breakerUntil）。
// Внимание:healthy Метрика без учета inFlight измерение (авторитетное решение стейт-машины, смотреть только disabled/until/breakerUntil）；
// inFlightFull Да healthy подмножество —healthy кол-во аккаунтов, достигших лимита in-flight, для /status отображать степень заполненности.
// и ServableNow разницу см. в комментарии к функции.
func (p *Pool) CountsDetailed() (total, healthy, cooling, disabled, inFlightFull int) {
	return p.countsDetailedForRealm("")
}

// CountsDetailedForRealm Совм. CountsDetailed，Но учитывается только Realm()==realm аккаунта;
// realm=="" без предиката (= CountsDetailed）。Подача /status Вывод с группировкой по доменам.
func (p *Pool) CountsDetailedForRealm(realm string) (total, healthy, cooling, disabled, inFlightFull int) {
	return p.countsDetailedForRealm(realm)
}

// countsDetailedForRealm общая реализация обхода для двух функций;realm=="" Без добавления предиката.
func (p *Pool) countsDetailedForRealm(realm string) (total, healthy, cooling, disabled, inFlightFull int) {
	p.mu.RLock()
	defer p.mu.RUnlock()
	now := time.Now()
	for _, e := range p.byUID {
		if realm != "" && e.a.Realm() != realm {
			continue
		}
		total++
		switch {
		case e.disabled:
			disabled++
		case !e.healthy(now):
			cooling++
		default:
			healthy++
			if p.inFlightFull(e) {
				inFlightFull++
			}
		}
	}
	return total, healthy, cooling, disabled, inFlightFull
}

// ServableNow Сообщить, обслуживает ли пул сейчас: есть хотя бы один healthy и аккаунты с незанятыми in-flight слотами.
// и CountsDetailed healthy Разная методика:healthy Только disabled/until/breakerUntil（авторитетное решение автомата),
// не смотреть inFlight；ServableNow Дополнительно накладывается in-flight измерение, с chat реальная достижимость (Pick будет пропущено inFlightFull аккаунт) выровнять.
// только для /healthz использовать, чтобы избежать«Все аккаунты healthy но все заняты«ложное срабатывание health-check при 200 И chat вернуть 503 Расхождение в подсчёте метрики.
func (p *Pool) ServableNow() bool {
	return p.ServableForRealm("")
}

// ServableForRealm Сообщить о realm Доступность: существует хотя бы один такой realm healthy и аккаунты с незанятыми in-flight слотами.
// и ServableNow В той же метрике (healthy или исключение/освобождение модели inFlightFull），Только суммирование Realm()==realm предикат.
// realm=="" деградирует в ServableNow（текущая семантика). Для /healthz Нажать realm экспонировать CN/global доступность каждого.
func (p *Pool) ServableForRealm(realm string) bool {
	p.mu.RLock()
	defer p.mu.RUnlock()
	now := time.Now()
	for _, e := range p.byUID {
		if realm != "" && e.a.Realm() != realm {
			continue
		}
		if p.inFlightFull(e) {
			continue
		}
		// семантика существования: на уровне аккаунта healthy，или в состоянии исключения на уровне модели (6004 Мягкое охлаждение одной модели —
		// для триггерной модели недоступно, для остальных моделей доступно). Health-check без контекста модели запроса, брать«есть обслуживаемый
		// Модель«и chat фактическая эквивалентность достижимости (issue #31 дополняется стороной health-check).
		if e.healthy(now) || e.modelExempt() {
			return true
		}
	}
	return false
}

// List Вернуть статусы всех аккаунтов (по UID сортировка, стабильный вывод).
func (p *Pool) List() []Status {
	p.mu.RLock()
	defer p.mu.RUnlock()
	uids := make([]string, 0, len(p.byUID))
	for uid := range p.byUID {
		uids = append(uids, uid)
	}
	sort.Strings(uids)
	out := make([]Status, 0, len(uids))
	for _, uid := range uids {
		out = append(out, p.statusOf(uid, p.byUID[uid]))
	}
	return out
}
func (p *Pool) statusOf(uid string, e *entry) Status {
	now := time.Now()
	st := Status{
		UID: uid,
		// Реестр лимитов (issue #36）：только "со временем парсинга 6004 мягкий кулдаун на уровне модели» всё ещё активен — не пусто,
		// одна строка на модель (modelCooldowns неистёкшие записи внутри), при одновременном лимитировании нескольких моделей показать всё.
		// Критерий истечения срока = Отдельный кулдаун для этой модели until не истёк; выводится только при выполнении условия, исчезает автоматически по истечении,
		// Обычный мягкий кулдаун (без таблицы на уровне модели)/жесткий cooldown без ledger (нулевой возврат).
		RateLimitedModels: p.rateLimitedModelsLocked(e, now),
		Realm: e.a.Realm(),
		Nickname: e.a.Nickname,
		Credits: e.credits,
		CreditsTotal: e.creditsTotal,
		CreditsExpiring: e.creditsExpiring,
		CreditsEarliestExpiry: e.creditsEarliestExpiry,
		CreditsEarliestRemaining: e.creditsEarliestRemaining,
		Cooling: now.Before(e.until) || now.Before(e.breakerUntil),
		Reason: e.reason,
		Disabled: e.disabled,
		SuccessCount: e.successCount,
		ErrTotal: e.errTotal,
		CheckinDone: e.lastCheckinDay == now.Format("2006-01-02"),
		TokenUsage: e.tokenUsage,
		LastSuccessTime: e.lastSuccess,
		LastErrTime: e.lastErr,
		Until: e.until,
		SoftStreak: e.softStreak,
		ModelCosts: p.modelCostsStatusLocked(e, now),
		ConsecutiveFails: e.consecutiveFails,
		DegradeUntil: e.degradeUntil,
		InFlight: int(e.inFlight.Load()),
		BreakerFails: e.fails,
		BreakerUntil: e.breakerUntil,
	}
	if st.Disabled {
		// заблокированный аккаунт отдает причину блокировки (эксплуатация не видит, почему умер).
		st.DisabledReason = e.reason
	}
	if st.Cooling {
		// оставшихся секунд кулдауна (округление вверх, чтобы избежать 0 отображается как истёкший).
		// Обычный кулдаун (until）и период circuit-breaker (breakerUntil）Возможно, активен только один из них,
		// Брать тот, что еще в будущем и с более поздним дедлайном, чтобы избежать ложного срабатывания только поОтключение 0 / unknown。
		remaining := int64(0)
		if now.Before(e.until) {
			if r := int64(time.Until(e.until).Seconds() + 0.999); r > remaining {
				remaining = r
			}
		}
		if now.Before(e.breakerUntil) {
			if r := int64(time.Until(e.breakerUntil).Seconds() + 0.999); r > remaining {
				remaining = r
				st.CoolKind = "breaker"
			}
		}
		st.CoolRemaining = remaining
		if st.CoolKind == "" {
			st.CoolKind = e.coolKind.String()
		}
	}
	return st
}

// modelCostsStatusLocked Собрать валидные строки реестра затрат аккаунта (P1-anti-monopoly наблюдаемость).
// Только modelCostTTL внутренние наблюдения в реестр (истёк/Нулевые значения пропускаются, едино со стороной чтения выбора номера);
// стабильная сортировка имен моделей. Вызывающая сторона должна уже удерживать блокировку.
func (p *Pool) modelCostsStatusLocked(e *entry, now time.Time) []ModelCostStatus {
	if len(e.modelCost) == 0 {
		return nil
	}
	models := make([]string, 0, len(e.modelCost))
	for m, mc := range e.modelCost {
		if mc.LastSeen.IsZero() || now.Sub(mc.LastSeen) > modelCostTTL {
			continue // Истёк/нулевое значение: не попадает в реестр (единый подход со стороной чтения выбора номера)
		}
		models = append(models, m)
	}
	if len(models) == 0 {
		return nil
	}
	sort.Strings(models)
	rows := make([]ModelCostStatus, 0, len(models))
	for _, m := range models {
		mc := e.modelCost[m]
		rows = append(rows, ModelCostStatus{
			Model: m,
			CostPer1k: mc.CostPer1k,
			LastSeen: mc.LastSeen,
			Samples: mc.Samples,
		})
	}
	return rows
}

// ---------------------------------------------------------------------------
// Персистентность
// ---------------------------------------------------------------------------

// rateLimitedModelsLocked собрать строки реестра моделей аккаунта, все еще в лимите (issue #36）。
// modelCooldowns Неистёкшие записи выводятся в стабильной сортировке по имени модели; все истекли/возврат пустой таблицы nil。
// Вызывающая сторона должна уже удерживать блокировку (statusOf Путь только для чтения удерживает RLock，данная функция только читает, не пишет).
func (p *Pool) rateLimitedModelsLocked(e *entry, now time.Time) []RateLimitedModel {
	if len(e.modelCooldowns) == 0 {
		return nil
	}
	// Сначала отсортировать имена моделей для стабильного вывода (map обход неупорядочен).
	models := make([]string, 0, len(e.modelCooldowns))
	for m := range e.modelCooldowns {
		models = append(models, m)
	}
	sort.Strings(models)
	rows := make([]RateLimitedModel, 0, len(models))
	for _, m := range models {
		mc := e.modelCooldowns[m]
		if !mc.Until.IsZero() && now.Before(mc.Until) {
			kind := "rate_limit"
			if strings.HasPrefix(mc.Reason, "11102") {
				kind = "model_unavailable"
			}
			row := RateLimitedModel{
				Model: m,
				Kind: kind,
				Until: mc.Until,
				Reason: mc.Reason,
			}
			// Исходные часы сброса апстрима: после обрыва until==resetAt при этом опускается (omitempty），в журнале только фактическое время восстановления.
			if !mc.ResetAt.IsZero() && !mc.ResetAt.Equal(mc.Until) {
				row.ResetAt = mc.ResetAt
			}
			rows = append(rows, row)
		}
	}
	if len(rows) == 0 {
		return nil
	}
	return rows
}
