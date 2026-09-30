// taskcenter.go панель "Центр задач»: сканирование задач по всем аккаунтам + очередь исполнения (настраиваемая конкурентность)+
// Очередь задач роста.
//
// Семантика:
// - Сканирование (scan_all）：параллельно тянуть список growth-задач на аккаунт (по умолчанию+в метрике мини-программы),
// Свести в"Не завершено и поддается автоматизации"список TODO (только чтение, не исполнять).
// - Очередь выполнения (run_queue + queue）：группировать задачи по аккаунтам и выполнять очередью — внутри аккаунта
// Последовательно (повторное использование per-account блокировка, и одиночная задача/взаимное исключение в один клик), конкурентность между аккаунтами
// （concurrency Ограничение семафора, по умолчанию 1）。Статус очереди доступен для опроса.
package panel

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"sort"
	"sync"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
)

// ---------------------------------------------------------------------------
// Сканирование (только чтение)
// ---------------------------------------------------------------------------

// scanAccountItem Результат сканирования одного аккаунта.
type scanAccountItem struct {
	UID string `json:"uid"`
	Nickname string `json:"nickname"`
	Growth []upstream.Task `json:"growth,omitempty"`
	GrowthErr string `json:"growth_error,omitempty"`
}

// growthPending Выполнена ли задача"Не завершено и поддается автоматизации"。
func growthPending(t upstream.Task) bool {
	if t.Claimed {
		return false
	}
	// заблокированные апстримом задачи не попадают в todo:Sequential Семейство: ежедневно в 00:00 открывается этап, сразу после выполнения предыдущего
	// следующее звено с выдачей но locked Форма появляется в списке — попадание в очередь только accept не проводить по учету
	// сбой (ежедневное окно блокировки), в 00:00 разблокируется и вернётся в todo. Остальное locked（апстрим не открыт)
	// Та же семантика: не должно пробоваться автоматически.
	if t.Locked {
		return false
	}
	if t.Target > 0 && t.Current >= t.Target {
		// достигнуто, но не получено: тоже в очередь (очередь автополучит при исполнении) — только для задач с авто-действием,
		// Иначе при исполнении очереди из-за autoActionFor для nil Сразу ошибка.
		return autoActionFor(t.TaskCode) != nil
	}
	return autoActionFor(t.TaskCode) != nil
}

// tasksScanAll Сканирование всех аккаунтов: задания роста (незавершённые+автоматизируемо, вкл. mp объединение метрик).
// Только чтение, параллельный фетч (аккаунтов — единицы).
func (p *Panel) tasksScanAll(w http.ResponseWriter, r *http.Request) {
	states := p.cfg.Pool.List()
	items := make([]scanAccountItem, len(states))
	var wg sync.WaitGroup
	for i, st := range states {
		if st.Disabled {
			continue
		}
		wg.Add(1)
		go func(i int, uid string) {
			defer wg.Done()
			a := p.cfg.Pool.AuthByUID(uid)
			if a == nil {
				return
			}
			it := &items[i]
			it.UID, it.Nickname = uid, a.Nickname
			// D4 Гейт:global учётная запись отсутствует CN система заданий роста, без вызовов апстрима.
			if a.IsGlobal() {
				return
			}
			if tasks, err := p.cfg.Upstream.ListTasks(a); err != nil {
				it.GrowthErr = err.Error()
			} else {
				for _, t := range tasks {
					if growthPending(t) {
						it.Growth = append(it.Growth, t)
					}
				}
			}
			// Задача в формате мини-программы (school_season День кампуса / Sequential_Tasks_1 первый диалог мини-приложения)
			// Только при mp рассылка списка заголовков, без пересечения с дефолтным набором — объединить в список ожидания;mp Ошибка списка
			// тихо (без mp Развертывание задачи/по окончании акции — нулевое влияние).
			if mpTasks, err := p.cfg.Upstream.ListTasksMP(a); err == nil {
				seen := map[string]bool{}
				for _, t := range it.Growth {
					seen[t.TaskCode] = true
				}
				for _, t := range mpTasks {
					if growthPending(t) && !seen[t.TaskCode] {
						it.Growth = append(it.Growth, t)
					}
				}
			}
		}(i, st.UID)
	}
	wg.Wait()
	pending := 0
	for _, it := range items {
		pending += len(it.Growth)
	}
	log.Printf("panel: сканирование очереди завершено: все аккаунты ожидают обработки %d Пункт", pending)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "accounts": items, "pending_count": pending})
}

// ---------------------------------------------------------------------------
// Очередь исполнения
// ---------------------------------------------------------------------------

// queueItem Единица выполнения очереди.
type queueItem struct {
	UID string `json:"uid"`
	Nickname string `json:"nickname"`
	Kind string `json:"kind"` // growth
	Code string `json:"code"`
	Status string `json:"status"` // pending | running | done | skipped | error
	Message string `json:"message,omitempty"`
}

// queueState состояние выполнения очереди.Seq При каждом запуске +1——фронтенд рендерит только"Раунд, запущенный самостоятельно"，
// остаток после завершения выполнения items не перезапишет последующее представление результатов сканирования.
type queueState struct {
	mu sync.Mutex
	running bool
	startedAt time.Time
	items []queueItem
	conc int
	seq int
}

// Panel Поле очереди в Panel На структуре (panel.go）От initQueue ленивая инициализация;
// здесь централизованные аксессоры, во избежание изменений New Цепочка конструирования.
func (p *Panel) queue() *queueState {
	p.queueOnce.Do(func() { p.q = &queueState{} })
	return p.q
}

// tasksRunQueue Запустить очередь выполнения:{concurrency:1-4, growth:bool, school:bool}。
// Сначала просканировать, поставить в очередь все задачи (growth Внутри аккаунта autoActions Последовательное выполнение,
// school прогон замкнутого цикла поаккаунтно), внутри аккаунта последовательно, между аккаунтами — ограничение семафором конкурентности.
func (p *Panel) tasksRunQueue(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Concurrency int `json:"concurrency"`
		Growth bool `json:"growth"`
	}
	_ = json.NewDecoder(r.Body).Decode(&body)
	if !body.Growth {
		body.Growth = true
	}
	if body.Concurrency < 1 {
		body.Concurrency = 1
	}
	if body.Concurrency > 4 {
		body.Concurrency = 4
	}
	started, total, seq, msg := p.startGrowthQueue(body.Concurrency, body.Growth)
	switch {
	case seq == -1:
		writeErr(w, http.StatusConflict, msg)
	case !started:
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "started": false, "message": msg})
	default:
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "started": true, "total": total, "seq": seq})
	}
}

// startGrowthQueue Сканировать бэклог всех аккаунтов и запустить очередь (HTTP「«Выполнить все задачи» и планировщик
// growth момент времени — общее ядро). Возврат (started, total, seq, msg)：seq==-1 Очередь отображения
// уже выполняется (конфликт);started=false Время msg Описание отсутствия исполнимых задач. Конкурентный захват
// [1,4]；growth переключатель аналогично HTTP Семантика входных параметров.
func (p *Panel) startGrowthQueue(concurrency int, growth bool) (started bool, total int, seq int, msg string) {
	if concurrency < 1 {
		concurrency = 1
	}
	if concurrency > 4 {
		concurrency = 4
	}
	q := p.queue()
	q.mu.Lock()
	if q.running {
		q.mu.Unlock()
		return false, 0, -1, "Очередь выполняется (прогресс в центре задач)"
	}
	// Сначала резервирование: если во время сканирования (сеть — секунды) параллельно триггернётся снова, сразу попадёт в верхний running
	// отклонить, избежать двух goroutine Одновременный запуск с взаимной перезаписью q.items/q.seq。Откат при отсутствии pending задач.
	q.running = true
	q.startedAt = time.Now()
	q.mu.Unlock()

	// Сканирование очереди задач (переиспользуется часть выборки логики сканирования).
	states := p.cfg.Pool.List()

	var accts []queueAccount
	var wg sync.WaitGroup
	var mu sync.Mutex
	for _, st := range states {
		if st.Disabled {
			continue
		}
		a := p.cfg.Pool.AuthByUID(st.UID)
		if a == nil {
			continue
		}
		wg.Add(1)
		go func(a *auth.Auth) {
			defer wg.Done()
			one := queueAccount{a: a}
			// D4 Гейт:global учётная запись отсутствует CN система заданий роста, без вызовов апстрима.
			if a.IsGlobal() {
				return
			}
			if growth {
				if tasks, err := p.cfg.Upstream.ListTasks(a); err == nil {
					for _, t := range tasks {
						if growthPending(t) {
							one.grow = append(one.grow, t)
						}
					}
					// Объединить TODO по метрике мини-приложения (с tasksScanAll Та же метрика:mp список — метрика по умолчанию
					// Супермножество, по code Дедупликация; при ошибке тихо). Ранее здесь пропущено слияние — скан показывает
					// mp в ожидании, а очередь сообщает"Нет исполняемых задач"。
					if mpTasks, mpErr := p.cfg.Upstream.ListTasksMP(a); mpErr == nil {
						seen := map[string]bool{}
						for _, t := range one.grow {
							seen[t.TaskCode] = true
						}
						for _, t := range mpTasks {
							if growthPending(t) && !seen[t.TaskCode] {
								one.grow = append(one.grow, t)
							}
						}
					}
					sort.Slice(one.grow, func(i, j int) bool { // Нажать autoActions Порядок (зависит от предыдущего)
						return autoActionIndex(one.grow[i].TaskCode) < autoActionIndex(one.grow[j].TaskCode)
					})
				}
			}
			if len(one.grow) > 0 {
				mu.Lock()
				accts = append(accts, one)
				mu.Unlock()
			}
		}(a)
	}
	wg.Wait()

	// сборка очереди (группировка по аккаунтам, сохранение порядка).
	var items []queueItem
	for _, one := range accts {
		for _, t := range one.grow {
			items = append(items, queueItem{UID: one.a.UID, Nickname: one.a.Nickname, Kind: "growth", Code: t.TaskCode, Status: "pending"})
		}
	}
	if len(items) == 0 {
		log.Printf("panel: Запуск очереди: нет исполнимых задач (все задачи аккаунтов выполнены)")
		q.mu.Lock()
		q.running = false
		q.startedAt = time.Time{}
		q.mu.Unlock()
		return false, 0, 0, "У всех аккаунтов нет ожидающих задач"
	}

	q.mu.Lock()
	q.items = items
	q.conc = concurrency
	q.seq++
	seq = q.seq
	q.mu.Unlock()

	go p.runQueueItems(accts, items, concurrency)
	log.Printf("panel: Запуск очереди:%d элементов (параллельность %d，рост %v）", len(items), concurrency, growth)
	return true, len(items), seq, ""
}

// RunGrowthQueueOnce Планировщик growth колбэк по времени (sch.SetGrowthHook монтирование): и
// 「кнопка "Выполнить все задачи» полностью через тот же пайплайн (рост, послед./паралл. 1）。Sequential Семейство
// Ежедневно в 00:00 разблокируется один этап, ранее продвижение только ручным сканом; этот колбэк автоматически продвигает цепочку на один этап в день.
// Асинхронное выполнение (startGrowthQueue Запуск goroutine мгновенный возврат), уже выполняется/Нет задач — безопасный пропуск.
func (p *Panel) RunGrowthQueueOnce() {
	started, total, _, _ := p.startGrowthQueue(1, true)
	if started {
		log.Printf("panel: Очередь периодических задач роста запущена (%d элементов)", total)
	}
}

// runQueueItems исполнитель очереди: группировка по аккаунтам, внутри аккаунта последовательно (per-account блокировка),
// конкурентность между аккаунтами (семафор). Результат каждого элемента пишется обратно в статус очереди.
func (p *Panel) runQueueItems(accts []queueAccount, items []queueItem, concurrency int) {
	q := p.queue()
	defer func() {
		q.mu.Lock()
		q.running = false
		q.mu.Unlock()
		log.Printf("panel: Выполнение очереди завершено (всего %d элементов)", len(items))
	}()

	sem := make(chan struct{}, concurrency)
	var wg sync.WaitGroup
	for _, one := range accts {
		wg.Add(1)
		go func(one queueAccount) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			// per-account взаимоисключение: с одиночной задачей/завершение в один клик использует общий лок.
			if !p.tryLockAccount(one.a.UID) {
				p.queueSet(q, one.a.UID, func(it *queueItem) {
					it.Status, it.Message = "skipped", "у аккаунта уже выполняется другое действие задачи, пропуск"
				})
				return
			}
			defer p.unlockAccount(one.a.UID)
			// префикс: массово принять непринятые задачи. Апстрим к not_accepted задачи не учитываются —
			// В панели "Завершить в один клик» этот шаг всегда был, в пути очереди ранее пропущен (проявляется как отчет 200 Но прогресс
			// всегда not_accepted、невозможно получить награду). Ошибка не блокирует (критерий прогресса — поведенческое событие).
			if accepted := p.acceptPendingTasks(one.a); accepted > 0 {
				time.Sleep(reportGap) // Оставить время на переход состояния апстрима
			}
			for i := range q.items {
				uid, kind, code := q.snapshotAt(i)
				if uid != one.a.UID {
					continue
				}
				p.queueMarkAt(i, "running", "")
				var msg string
				var err error
				switch kind {
				case "growth":
					msg, err = p.runGrowthQueued(one.a, code)
				}
				if err != nil {
					p.queueMarkAt(i, "error", err.Error())
				} else {
					p.queueMarkAt(i, "done", msg)
				}
				time.Sleep(reportGap) // Троттлинг между элементами
			}
		}(one)
	}
	wg.Wait()
}

// queueAccount Юнит аккаунта исполнения очереди (runQueueItems параметр).
type queueAccount struct {
	a *auth.Auth
	grow []upstream.Task
}

// snapshotAt Чтение триплета записи под блокировкой (избежать удержания указателя вне блокировки).
func (q *queueState) snapshotAt(i int) (uid, kind, code string) {
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.items[i].UID, q.items[i].Kind, q.items[i].Code
}

// queueMarkAt обновление статуса элемента очереди по индексу (массив элементов фиксирован, без добавления/удаления).
func (p *Panel) queueMarkAt(i int, status, msg string) {
	q := p.queue()
	q.mu.Lock()
	q.items[i].Status, q.items[i].Message = status, msg
	q.mu.Unlock()
}

// queueSet Нажать uid Массовое изменение статуса.
func (p *Panel) queueSet(q *queueState, uid string, fn func(*queueItem)) {
	q.mu.Lock()
	defer q.mu.Unlock()
	for i := range q.items {
		if q.items[i].UID == uid {
			fn(&q.items[i])
		}
	}
}

// acceptPendingTasks пакетно принять непринятые задачи аккаунта, вернуть кол-во принятых (при ошибке вернуть 0 не блокирует).
func (p *Panel) acceptPendingTasks(a *auth.Auth) int {
	tasks, err := p.cfg.Upstream.ListTasks(a)
	if err != nil {
		return 0
	}
	var codes []string
	for _, t := range tasks {
		if !t.Claimed && !t.Locked && t.AcceptStatus != "accepted" && t.AcceptStatus != "completed" {
			codes = append(codes, t.TaskCode)
		}
	}
	if len(codes) == 0 {
		return 0
	}
	if err := p.cfg.Upstream.AcceptTasks(a, codes); err != nil {
		log.Printf("panel: очередь accept uid=%s: %v（не блокирует)", a.UID, err)
		return 0
	}
	log.Printf("panel: очередь accept uid=%s: Принято %d задач", a.UID, len(codes))
	return len(codes)
}

// runGrowthQueued выполнение одиночной задачи роста (действие + обратное чтение + Автополучение награды; и
// accountTaskAuto та же семантика, результат возвращается текстом).
func (p *Panel) runGrowthQueued(a *auth.Auth, code string) (string, error) {
	act := autoActionFor(code)
	if act == nil {
		return "", fmt.Errorf("задача %s Нет автоматических действий", code)
	}
	// taskByCode Уже двойная метрика (mp Авто-фолбэк эксклюзивного кода mp список).
	before, err := p.taskByCode(a, code)
	if err != nil {
		return "", err
	}
	if before == nil {
		return "у этого аккаунта нет такой задачи", nil
	}
	isMP := isMPTaskCode(code)
	if before.Claimed {
		return "Завершено (получено)", nil
	}
	msg, err := act.run(p, a)
	if err != nil {
		return "", err
	}
	var after *upstream.Task
	if isMP {
		after, _ = p.taskByCodeMP(a, code)
	} else {
		after, _ = p.taskByCodeWaiting(a, code)
	}
	if after != nil && after.Claimable {
		var credit, energy int64
		var cerr error
		if isMP {
			credit, energy, cerr = p.cfg.Upstream.ClaimRewardMP(a, code)
		} else {
			credit, energy, cerr = p.cfg.Upstream.ClaimReward(a, code)
		}
		if cerr == nil && (credit > 0 || energy > 0) {
			msg += fmt.Sprintf("；Автополучение награды +%d Разделить +%d Возможность", credit, energy)
		}
	}
	if after != nil {
		msg += "(Прогресс " + taskProgressText(after) + ")"
	}
	log.Printf("panel: очередь growth uid=%s code=%s: %s", a.UID, code, msg)
	return msg, nil
}

// tasksQueueStatus состояние очереди (для polling).
func (p *Panel) tasksQueueStatus(w http.ResponseWriter, r *http.Request) {
	q := p.queue()
	q.mu.Lock()
	defer q.mu.Unlock()
	items := make([]queueItem, len(q.items))
	copy(items, q.items)
	writeJSON(w, http.StatusOK, map[string]any{
		"running": q.running,
		"total": len(items),
		"conc": q.conc,
		"started": !q.startedAt.IsZero(),
		"started_at": q.startedAt,
		"seq": q.seq,
		"items": items,
	})
}

// schoolVouchers Мои коды купонов: поштучно CN Проверка аккаунтом сезона "Снова в школу» /vouchers（3 Конкурентность, и packages
// аналогичный лимит), сбой помечается только на соответствующем аккаунте error。global у аккаунта нет сезона Back-to-School, вызов апстрима не отправлять.
func (p *Panel) schoolVouchers(w http.ResponseWriter, r *http.Request) {
	accts := p.cfg.Pool.List()
	type row struct {
		UID string `json:"uid"`
		Nickname string `json:"nickname"`
		Vouchers []upstream.SchoolVoucher `json:"vouchers"`
		Err string `json:"error,omitempty"`
	}
	out := make([]row, len(accts))
	sem := make(chan struct{}, 3)
	var wg sync.WaitGroup
	for i, st := range accts {
		if st.Disabled {
			continue // не занято, в конце строки убрать
		}
		a := p.cfg.Pool.AuthByUID(st.UID)
		if a == nil {
			continue
		}
		wg.Add(1)
		go func(i int, a *auth.Auth) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			it := row{UID: a.UID, Nickname: a.Nickname}
			switch {
			case a.IsGlobal():
				it.Err = "global realm（нет акции к началу учебного года)"
			default:
				vs, err := p.cfg.Upstream.SchoolVouchers(a)
				if err != nil {
					it.Err = err.Error()
				} else {
					it.Vouchers = vs
				}
			}
			out[i] = it
		}(i, a)
	}
	wg.Wait()
	res := make([]row, 0, len(out))
	for _, it := range out {
		if it.UID != "" {
			res = append(res, it)
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"accounts": res})
}
