// Выбор номера:Pick Кластер (healthy Стратификация стоимости + вес скоро истекающего виртуального инстанса + Фолбэк полного кулдауна + фильтрация по заполнению in-flight).
package pool

import (
	"log"
	"math/rand/v2"
	"sort"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/logfmt"
)

// Pick Единая точка выбора номера (без ротации на уровне запроса, без realm Фильтрация, по умолчанию уровень аккаунта с учетом модели).
// требуется ротация на уровне запроса (tried）или раздельный пул (realm）использовать при PickExcludingForRealm。
func (p *Pool) Pick() *auth.Auth {
	return p.pick(nil, "", "")
}

// PickExcluding То же, но пропустить tried в uid（ротация на уровне запроса).
// Стратегия выбора:healthy выбор топ-N аккаунтов по весу 5 имя, затем в Top5 внутри — взвешенная случайная выборка с равным весом,
// цель — размазать хотспоты, избежать постоянного попадания в один аккаунт.
func (p *Pool) PickExcluding(tried map[string]bool) *auth.Auth {
	return p.pick(tried, "", "")
}

// PickExcludingForModel выбор с учётом модели: эквивалентно PickExcluding，Но для "6004 в кулдауне уровня модели
// аккаунта» для исключения модели — запрашиваемая модель и её trigger При несовпадении моделей считается доступным (issue #31）。
// reqModel при пустом значении — обычный PickExcluding（не влияет на существующую семантику вызовов).
func (p *Pool) PickExcludingForModel(tried map[string]bool, reqModel string) *auth.Auth {
	return p.pick(tried, reqModel, "")
}

// PickExcludingForRealm Осведомленность о модели + выбор номера по пулам: кандидаты сначала по Realm()==realm Фильтрация
// （realm пустой = не фильтровать, деградирует в PickExcludingForModel），Далее судить по health-критерию модели.
// Подача handler В global/cn Разделение трафика в двух доменах (global Запросы модели маршрутизируются только global аккаунтом).
func (p *Pool) PickExcludingForRealm(tried map[string]bool, reqModel, realm string) *auth.Auth {
	return p.pick(tried, reqModel, realm)
}

// pick В healthy в пуле кандидатов выбрать аккаунт взвешенным случайным выбором и записать lastUsed（защита от коллизии аккаунтов при конкуренции).
// reqModel при непустом значении сменить критерий health на healthyForModel（6004 исключение модели активно).
// realm При непустом — наложение фильтра кандидатов Realm()==realm Предикат (домен выбора номера с разделением пулов).
func (p *Pool) pick(tried map[string]bool, reqModel, realm string) *auth.Auth {
	p.mu.Lock()
	defer p.mu.Unlock()
	now := time.Now()
	realmOK := func(e *entry) bool { return realm == "" || e.a.Realm() == realm }
	healthyOf := func(e *entry) bool { return realmOK(e) && e.healthy(now) }
	if reqModel != "" {
		healthyOf = func(e *entry) bool { return realmOK(e) && e.healthyForModel(now, reqModel) }
	}
	// floorBlocked Проверка блокировки минимального порога баллов (реализовано в floorBlockedForModel，совместно со sticky-маршрутом):
	// Достигнуто дно + Фактически платно (tier 2 валидных наблюдений) — блокировать;tier 0/1 Без ограничений.
	floorBlocked := func(e *entry) bool { return p.floorBlockedForModel(e, reqModel, now) }
	var cands []*entry
	for uid, e := range p.byUID {
		if tried != nil && tried[uid] {
			continue
		}
		// ленивая очистка просроченного охлаждения уровня модели и реестра стоимости (у обоих map Не разрастается бесконечно;
		// status read-only обход пропускает просроченные записи, но in-memory записи должны быть здесь реально удалены).
		e.pruneExpiredModelCooldowns(now)
		e.pruneExpiredModelCosts(now)
		if !healthyOf(e) {
			continue
		}
		if floorBlocked(e) {
			continue // Гарантия баллов: аккаунт на дне не берёт платные тестовые модели (tier 0/1 не ограничено)
		}
		if p.inFlightFull(e) {
			continue // in-flight заполнен: пропуск (max=0 без ограничения по времени не срабатывает)
		}
		cands = append(cands, e)
	}
	if len(cands) == 0 {
		// Фолбэк полного охлаждения: нет healthy при выборе кандидата — выбирать из охлаждаемых аккаунтов until Самый ранний истекающий
		// （Circuit Breaker/общий кулдаун expiry метрика, берётся более ранний дедлайн). Отключённые аккаунты никогда не участвуют в fallback.
		return p.pickEarliestExpiryLocked(tried, now, realm)
	}
	// top5 Короткий список усекается по убыванию веса (а не credits просто по убыванию): иначе компенсация простоя вообще не попадёт в
	// решение по короткому списку, низкий credits но давно простаивающие аккаунты никогда не попадут в очередь top5。
	// maxCredits Использовать единообразно**Метрика полного множества**（tier все до фильтрации healthy кандидаты): усечение, сортировка и
	// Веса розыгрыша на единой базе, веса двух этапов сопоставимы.
	var maxCredits int64
	for _, e := range cands {
		if e.credits > maxCredits {
			maxCredits = e.credits
		}
	}
	// Стратификация стоимости (reqModel при непустом): стратифицировать кандидатов по фактическому списанию для модели, оставить только оптимальный слой.
	// 0 = Фактически бесплатно по тестам (период бесплатного доступа/аккаунты с ночным бесплатным доступом, максимальный приоритет)
	// 1 = Нет наблюдений (включая просроченные)
	// 2 = Платно, проверено тестом
	// Почему«нет наблюдений«Идёт после«Платно, проверено тестом«Ранее: статус лимита/бесплатности нового аккаунта определялся только тестом,
	// если известный платный аккаунт всегда давит неизвестный, бесплатный никогда не выберется и никогда не обучится.
	// Почему жесткая фильтрация, а не только сортировка:pickWeighted будет взвешенный рандом среди кандидатов, при только сортировке
	// Платный номер всё ещё может выпасть, не достигает«приоритет бесплатному«семантика.
	costTier := func(e *entry) (int, float64) {
		mc, ok := e.modelCostOf(reqModel, now)
		if !ok {
			return 1, 0
		}
		if mc.CostPer1k <= 0 {
			return 0, 0
		}
		return 2, mc.CostPer1k
	}
	bestTier := 2
	hasTier1 := false
	explored := false // Текущий pick Переключен ли слой исследования (лог события после фиксации выбранного номера)
	for _, e := range cands {
		if ti, _ := costTier(e); ti < bestTier {
			bestTier = ti
		}
	}
	// Исследование условий (issue #136 схема a′）：tier 0 слой монополии существует (bestTier==0 И reqModel
	// непусто) и кандидат содержит tier 1（заморозка существует) и с момента последней проверки ≥ Окно (нулевое значение timer=никогда не исследовалось
	// →при первом выполнении условия сразу проба), в этот раз pick Переключение слоя применения tier 1-only——Исследование=Попутное перенаправление, превращает один
	// перенаправление существующего реального запроса пользователя на неизвестный номер (ноль новых запросов к апстриму;IP нулевой инкремент по измерению,WAF дружелюбно).
	// Успех → NoteModelCost Первое наблюдение → Выпуск (tier 0/2，следующий раунд pick вступает в силу немедленно);
	// ошибка → Существующая стратегия ошибок без изменений, без шторма зондирования.
	// hasTier1 Переиспользовать выше в этом цикле costTier предвычисленная метрика (контракт: один раз на кандидата).
	// timer Запись под одним локом: конкурентно pick Последовательный вход в write-lock, только один вошедший проходит проверку окна
	//（Естественная защита от повторного обхода).key = realm + "\x1f" + reqModel：Одинаковое имя модели может быть кросс-доменным,
	// Ритм исследования по (Домен, Модель) независимо;realm==""（Pick старая семантика) отдельным ключом.
	if p.costExploreInterval > 0 && bestTier == 0 && reqModel != "" {
		for _, e := range cands {
			if ti, _ := costTier(e); ti == 1 {
				hasTier1 = true
				break
			}
		}
		key := realm + "\x1f" + reqModel
		if hasTier1 && now.Sub(p.exploreLast[key]) >= p.costExploreInterval {
			p.exploreLast[key] = now
			p.costExploreEvents++
			bestTier = 1
			explored = true
		}
	}
	// Вес считается один раз: топ 5 усечение требует сортировки, если в sort вычисление на лету в компараторе weightOf будет преобразовано в O(n log n) Раз
	// Избыточные вычисления с плавающей точкой (46 аккаунт около 500 раз). Сначала сделать O(n) предвычисление, затем по (Вес, uid) Сортировка.
	// costTier/modelCostOf Аналогично каждый кандидат учитывается один раз (сохранение в tier/cost1k），Компаратор читает только поле кэша.
	type weighted struct {
		e *entry
		w float64
		tier int
		cost1k float64
	}
	ws := make([]weighted, 0, len(cands))
	for _, e := range cands {
		ti, ci := costTier(e)
		if ti == bestTier {
			ws = append(ws, weighted{e: e, w: p.routingWeightOf(e, maxCredits, now), tier: ti, cost1k: ci})
		}
	}
	// Перемешивание при равных весах: только если есть равные веса и число кандидатов превышает top5 тогда только для ws выполнить Fisher-Yates
	// шафл (причём**не расходует p.randInt64N Источник инъекции**，избежать изменения pickWeighted детерминированная семантика,
	// См. TestPickDeterministicViaSetRandomSource）。при равных весах или наличии tie — по лексикографическому порядку
	// усечение приведет к uid Аккаунты в конце очереди никогда не попадут в top5（аккаунты с равным весом голодают из-за лексикографического порядка,LRU Фолбэк
	// И только в top5 Внутренняя переадресация — первопричина thundering herd на одном номере). Для шафла используется отдельный time-seeded Источник,
	// Только на границе усечения создает случайный порядок с равным весом, не влияет на детерминированность взвешенной выборки.
	if len(ws) > 5 {
		eq := false
		for i := 1; i < len(ws); i++ {
			if ws[i].w == ws[0].w {
				eq = true
				break
			}
		}
		if eq {
			shuf := rand.New(rand.NewPCG(uint64(now.UnixNano()), uint64(len(ws))))
			shuf.Shuffle(len(ws), func(i, j int) { ws[i], ws[j] = ws[j], ws[i] })
		}
	}
	sort.SliceStable(ws, func(i, j int) bool {
		// costTier После жёсткой фильтрации ws Все на одном уровне, но всё равно по cost1k Сортировка по возрастанию (tier 2 внутри слоя дешевле за единицу
		// впереди;tier 0/1 уровень cost1k Конст. 0，сравнение вырождается в сравнение весов) — читать поле из кэша, без пересчёта.
		if ws[i].cost1k != ws[j].cost1k {
			return ws[i].cost1k < ws[j].cost1k // Уровень тарификации: сначала с низкой ценой за единицу
		}
		if ws[i].w != ws[j].w {
			return ws[i].w > ws[j].w
		}
		return ws[i].e.a.UID < ws[j].e.a.UID // Стабильный фолбэк (после шафла почти не срабатывает)
	})
	cands = cands[:0]
	for _, c := range ws {
		cands = append(cands, c.e)
	}
	// candsAll Сохранить полный список кандидатов до усечения (по убыванию веса) для LRU Фолбэк: выбирать самый старый во всём объёме,
	// Избежать top5 Лексикографическое усечение вызывает голодание аккаунтов с равным весом в конце списка (одна из причин thundering herd).
	candsAll := cands
	if len(cands) > 5 {
		cands = cands[:5]
	}
	var e *entry
	// защита от коллизии аккаунтов при конкурентности: фильтрация под локом по "моменту последнего выбора», но один батч конкурентных goroutine будет входить последовательно
	// данная функция (write-lock), каждый входящий lastUsed установить в now —— Тогда N-й в тот же момент 2..N шт.
	// Входящий видит предыдущий аккаунт lastUsed==now（Давность 0 < minPickGap），естественно вытесняется на другие аккаунты.
	// Ключевое:lastUsed Присваивание под блокировкой, чтобы проверка временного окна была реентерабельна при конкурентности.
	eligible := make([]*entry, 0, len(cands))
	for _, c := range cands {
		if now.Sub(c.lastUsed) >= minPickGap {
			eligible = append(eligible, c)
		}
	}
	if len(eligible) == 0 {
		// top5 все только что использованы:LRU Фолбэк, в**все кандидаты candsAll**（не только top5）Выбрать самый старый из.
		// использовать usedSeq Монотонный sequence number, а не lastUsed Сравнение по wall-clock:Windows и др. платформы time.Now() Точность
		// ~0.5ms，все при быстром последовательном выборе номеров lastUsed Полное совпадение,Before весь false Будет всегда выбирать
		// candsAll[0] приводит к скученности.usedSeq Строгий полный порядок, не зависит от точности времени.
		e = candsAll[0]
		for _, c := range candsAll[1:] {
			if c.usedSeq < e.usedSeq {
				e = c
			}
		}
	} else {
		e = p.pickWeighted(eligible) // eligible Сохранение порядка = top5 убывающее подмножество
	}
	if explored {
		// Журнал событий исследования (наблюдаемость): выбранный номер определяется только сейчас, поэтому лог в точке выбора.
		// результат выпуска замыкается соседними существующими логами (у бесплатных — без логов, у платных — через NoteModelCost
		// штатный путь).
		log.Printf("[pool] cost explore model=%s realm=%q acct=%s window=%s",
			reqModel, realm, logfmt.Label(e.a.UID, e.a.Nickname), p.costExploreInterval)
	}
	e.lastUsed = now // Мгновенная пометка под блокировкой: следующий входящий pick goroutine Сразу видно, что номер уже использован
	p.pickSeq++
	e.usedSeq = p.pickSeq // Монотонный sequence: гарантирует usedSeq строгий тотальный порядок (защита от thundering herd/LRU авторитетное основание)
	return e.a
}

// floorBlockedForModel Проверка блокировки гарантированного минимума баллов (pick Обычная ротация и PickByUIDForModel Липкость
// единственный источник истины для пути):floor>0 и аккаунт исчерпан (credits < floor）и данная модель на этом аккаунте
// **Платность подтверждена тестами**（tier 2 при наличии валидного наблюдения) — true.
//
// - tier 0（бесплатно) не блокировать: резерв как раз "оставить баланс для бесплатных моделей», бесплатный запрос
// credit=0 больше не списывать баланс (NoteModelCost）。
// - tier 1（нет наблюдений/истечение наблюдения) не блокировать: первый успех — зачет и выпуск; если заблокировано — журнал просрочен
// （modelCostTTL 6h）или после сброса при рестарте исчерпанные аккаунты навсегда зависнут в дедлоке "невозможно переучиться».
// - model пусто (без контекста модели) — не блокировать: нет измерения стоимости,floor Невозможно определить тарификацию.
//
// Баланс по локальной интерполяции (авторитетное значение чекина - Каждая транзакция usage.credit Фактическое списание, см. NoteModelCost）：
// Только занижение, не завышение (задержка сверки у официалов безопасна), именно безопасное направление для гарантии минимума.
// вызывающая сторона должна уже удерживать p.mu（Чтение e.credits / e.modelCost）。
func (p *Pool) floorBlockedForModel(e *entry, model string, now time.Time) bool {
	if p.creditFloor <= 0 || model == "" || e.credits >= p.creditFloor {
		return false
	}
	mc, ok := e.modelCostOf(model, now)
	return ok && mc.CostPer1k > 0
}

// pickEarliestExpiryLocked фолбэк полного охлаждения: при неотключенном мягком охлаждении/из аккаунтов вОтключение выбрать с самым ранним дедлайном.
// уровни:disabled никогда не участвует;CoolHard（аккаунты с исчерпанным балансом, ожидающие чекина) также исключаются — при вызове обязательно 402，трата ротации и шумовые логи;
// CoolSoft и номера в circuit-breaker допускаются к участию (возможно уже восстановились, цена ошибки — одна ротация).
// Получено tried Исключение, занятые in-flight аккаунты также пропускаются (сохранение ротации на уровне запроса + семантика лиза). Нет доступных — вернуть nil。
func (p *Pool) pickEarliestExpiryLocked(tried map[string]bool, now time.Time, realm string) *auth.Auth {
	var best *entry
	for uid, e := range p.byUID {
		if tried != nil && tried[uid] {
			continue
		}
		if realm != "" && e.a.Realm() != realm {
			continue // фильтр по домену: кросс- в пуле realm Аккаунты в cooldown не участвуют в данном realm Фолбэк
		}
		if e.disabled {
			continue // отключенные аккаунты никогда не участвуют в фолбэке
		}
		if e.coolKind == CoolHard && !e.until.IsZero() && now.Before(e.until) {
			continue // Номер с исчерпанным балансом (в состоянии активен hard период кулдауна) не участвует в фолбэке: ждать восстановления чекина, вызов обязательно 402
		}
		if p.inFlightFull(e) {
			continue
		}
		exp := e.expiry(now)
		if exp.IsZero() {
			continue
		}
		if best == nil || exp.Before(best.expiry(now)) {
			best = e
		}
	}
	if best == nil {
		return nil
	}
	log.Printf("WARN: [pool] fallback_earliest_expiry acct=%s until=%s kind=%s", logfmt.Label(best.a.UID, best.a.Nickname), best.expiry(now).Format(time.RFC3339), best.fallbackKind(now))
	best.lastUsed = time.Now()
	// фолбэк тоже "выбран», должен совпадать с pick() обычный путь, путь sticky-попадания (PickByUIDForModel）
	// продвигать аналогично usedSeq/pickSeq：иначе аккаунт, многократно выбираемый fallback-ом usedSeq Всегда равно 0，В pick 
	// LRU Фолбэк (по usedSeq взять самый старый) в глазах всегда "самый старый», только что использованный сразу выбирается снова —
	// защита от концентрации/Защита от thundering herd не сработает (entry.usedSeq Контракт: при каждом выборе брать p.pickSeq автоинкремент).
	p.pickSeq++
	best.usedSeq = p.pickSeq
	return best.a
}

// inFlightFull Сообщить, исчерпал ли аккаунт лимит in-flight слотов (лимит по realm уровни, см. inFlightLimit；
// limit=0 без ограничений → Конст. false）。вызывающая сторона должна уже удерживать p.mu（подойдет read-lock или write-lock, метод только читает лимит).
func (p *Pool) inFlightFull(e *entry) bool {
	limit := p.inFlightLimit(e)
	if limit <= 0 {
		return false
	}
	return e.inFlight.Load() >= int64(limit)
}

// minPickGap окно защиты от конкурентного коллизии номеров: один аккаунт не выбирается повторно в этом окне (кроме top5 все только что использованы).
// Продовый дефолт 100ms；для теста чисто взвешенного распределения можно временно установить 0 закрыть антиколлизионный номер.
var minPickGap = 100 * time.Millisecond

// pickWeighted Взвешенный рандом (claude-api selectWeightedRandom эталонный критерий):
//
//		weight = credits доля × 10 + idleWeight
//
//	 - credits доля = Данный аккаунт credits / Максимум внутри кандидат-сета credits（во избежание взрыва размерности)
//	 - idleWeight = min(Расстояние lastUsed количество часов × idleWeightPerHour, idleWeightMax)；Неиспользованным — максимальный балл
//
// credits весь 0 при этом всё равно по idleWeight Взвешенно (без вырождения в равномерное распределение).
// Вес — float, использовать int64 фиксированная точка (×1e6）жеребьёвка с сохранением инъекции детерминированного источника случайности (randInt64N семантика неизменна).
// Источник энтропии — приоритетное использование p.randInt64N（только для детерминизма тестовой инъекции),nil откат при math/rand/v2 глобальный источник.
func (p *Pool) pickWeighted(cands []*entry) *entry {
	now := time.Now()
	var maxCredits int64
	for _, e := range cands {
		if e.credits > maxCredits {
			maxCredits = e.credits
		}
	}
	const scale = 1_000_000 // точечное увеличение:int64 взвешенная лотерея с накоплением веса (целые числа)
	weights := make([]int64, len(cands))
	var total int64
	for i, e := range cands {
		w := p.routingWeightOf(e, maxCredits, now)
		weights[i] = int64(w * scale)
		total += weights[i]
	}
	rnd := rand.Int64N
	if p.randInt64N != nil {
		rnd = p.randInt64N
	}
	if total <= 0 {
		return cands[int(rnd(int64(len(cands))))]
	}
	r := rnd(total)
	var acc int64
	for i, e := range cands {
		acc += weights[i]
		if r < acc {
			return e
		}
	}
	return cands[len(cands)-1]
}

// weightOf вычислить обычный взвешенный скор одного аккаунта.
func (p *Pool) weightOf(e *entry, maxCredits int64, now time.Time) float64 {
	w := 1.0
	// 1. credits доля ×10（будет учтено в mid-credit якорь, избежать затрагивания всех 0 Время credits элемент — 0）。
	if maxCredits > 0 {
		w += float64(e.credits) / float64(maxCredits) * 10
	}
	// 2. Компенсация простоя.
	if e.lastUsed.IsZero() {
		w += p.idleWeightMax // Не использовался → максимум баллов
	} else {
		hours := now.Sub(e.lastUsed).Hours()
		idleW := hours * p.idleWeightPerHour
		if idleW > p.idleWeightMax {
			idleW = p.idleWeightMax
		}
		if idleW < 0 {
			idleW = 0 // lastUsed При времени в будущем (откат часов) — clamp 0
		}
		w += idleW
	}
	// 3.（исходный "процент успеха ×3」фактор удалён, выровнено с апстримом success-ema-review：errTotal Пожизненный
	// накопительно, только инкремент, успешность = successCount/(successCount+errTotal) заставит ранее сбойный
	// Аккаунт перманентно ограничен без восстановления; мгновенный health-сигнал покрыт кулдауном/Circuit Breaker/приём понижения веса при серии неудач.)
	return w
}

// expiringNow Сообщить, есть ли у аккаунта действующие скоро истекающие партии баллов.
func expiringNow(e *entry, now time.Time) bool {
	return e.creditsExpiring > 0 &&
		e.creditsEarliestRemaining > 0 &&
		!e.creditsEarliestExpiry.IsZero() &&
		e.creditsEarliestExpiry.After(now)
}

// routingWeightOf Поверх веса обычного аккаунта наложить количество быстро истекающих виртуальных инстансов.prefer_expiring=false
// или при отсутствии у аккаунта валидных скоро-истекающих партий число инстансов всегда = 1，Результат и старый weightOf полностью совпадает.
func (p *Pool) routingWeightOf(e *entry, maxCredits int64, now time.Time) float64 {
	w := p.weightOf(e, maxCredits, now)
	if p.preferExpiring && expiringNow(e, now) {
		return w * expiringVirtualSlots
	}
	return w
}

// SetCredits Обновить баланс аккаунта.
