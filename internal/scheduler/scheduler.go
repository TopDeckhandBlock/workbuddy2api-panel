// Package scheduler Задача по расписанию: check-in / активный отчет / Путешествие котиков / token keepalive Четыре независимых расписания.
// После успешной отметки повторно запросить баланс, баланс > 0 аккаунты на кулдауне автоматически размораживаются.
package scheduler

import (
	"context"
	"errors"
	"fmt"
	"log"
	"sync"
	"sync/atomic"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/logfmt"
	"github.com/linguo2625469/workbuddy2api-panel/internal/pool"
	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
)

// Config Зависимость планировщика.
//
// Переключатель задания назван "Отключить», а не "Включить»: нулевое значение Config т.е. все 4 типа задач включены (hours откат к дефолту),
// Поведение дословно как до введения флага (старые вызывающие/старые тесты без изменений).
type Config struct {
	Pool *pool.Pool
	Upstream *upstream.Client
	CheckinHours []int // По умолчанию [9, 21]
	TravelHours []int // По умолчанию [9,21]：Отправка за один проход + замкнутый цикл получения награды за один проход
	ActivityHours []int // По умолчанию [10]
	KeepaliveHours []int // По умолчанию [22]
	BlackcatHours []int // По умолчанию [23]：Ночная сова (23:00–08:00 окно подсчета)
	GrowthHours []int // По умолчанию [1]：Очередь задач роста (Sequential Семейство разблокирует одно звено ежедневно в 00:00,
	// 01:00 автосканирование+выполнение; избегать ровно 00:00 для предотвращения гонки разблокировки)

	// ExpiringSoonWindow Окно скоро истекающих баллов: чекин/при обновлении/проверке баланса проставить время истечения
	// <= now+window баланс пакета помечен как"Скоро истекает"（pool На этой основе — приоритет earliest-expire-first, см.
	// entry.creditsEarliestExpiry）。<=0 Без порога маршрутизации; отображение — по полным попакетным данным.
	ExpiringSoonWindow time.Duration

	// CheckinDisabled явное отключение расписания check-in (соответствует config schedule.checkin_enabled=false）。
	// После отключения точек чекина больше нет. Путешествие больше не идет прицепом к чекину (выделено в отдельный график).
	CheckinDisabled bool
	// TravelDisabled Явное отключение расписания путешествия котиков (schedule.travel_enabled=false）。
	TravelDisabled bool
	// ActivityDisabled явно отключить расписание активного репорта (schedule.activity_enabled=false）。
	ActivityDisabled bool
	// KeepaliveDisabled явно отключено token Расписание keep-alive (schedule.keepalive_enabled=false）。
	KeepaliveDisabled bool
	// BlackcatDisabled Явное отключение расписания "совы» (schedule.blackcat_enabled=false）。
	BlackcatDisabled bool
	// GrowthDisabled Явное отключение автопланирования задач роста (schedule.growth_enabled=false）。
	GrowthDisabled bool

	// GrowthHook колбэк выполнения очереди задач роста (panel.RunGrowthQueueOnce：Сканировать все аккаунты
	// бэклог и выполнить, тот же пайплайн, что и кнопка панели "Выполнить все задачи»). Планировщик отвечает только за момент, не за реализацию —
	// panel В scheduler сконструировать после, использовать SetGrowthHook Последующее монтирование;nil при наступлении времени пропустить.
	GrowthHook func()
}

// Scheduler Планировщик.
type Scheduler struct {
	cfg Config

	// mu/adoptTried Записи о неудачах в день принятия:uid → календарный день (CST）。Аккаунты, не достигшие порога, в этот день больше не ретраятся,
	// Избежать многократных ретраев к upstream в один день; сброс при рестарте процесса (без персистентности).
	mu sync.Mutex
	adoptTried map[string]string

	// schedMu защита параметров расписания (момент/переключатель);Reconfigure Можно менять на горячую в рантайме (вызывается при сохранении конфигурации в панели).
	// rearmSchedule/rearmBalance — уведомление "расписание изменено, немедленный пересчет»:Run и цикл обновления баланса потребляют раздельно,
	// использовать отдельные независимые channel（Совм. channel двумя select потребление потеряет сигнал).
	schedMu sync.Mutex
	rearmSchedule chan struct{}
	rearmBalance chan struct{}

	// balanceInterval Интервал обновления баланса (нс,0=пауза).atomic чтение/запись: цикл исполнения каждый раунд читает текущее значение,
	// SetBalanceInterval допускается горячее изменение в любой момент (сохранение конфига в панели).
	balanceInterval atomic.Int64
}

// New Сборка.
func New(cfg Config) *Scheduler {
	if len(cfg.CheckinHours) == 0 {
		cfg.CheckinHours = []int{9, 21}
	}
	if len(cfg.TravelHours) == 0 {
		cfg.TravelHours = []int{9, 21}
	}
	if len(cfg.ActivityHours) == 0 {
		cfg.ActivityHours = []int{10}
	}
	if len(cfg.KeepaliveHours) == 0 {
		cfg.KeepaliveHours = []int{22}
	}
	if len(cfg.BlackcatHours) == 0 {
		cfg.BlackcatHours = []int{23}
	}
	return &Scheduler{
		cfg: cfg,
		adoptTried: make(map[string]string),
		rearmSchedule: make(chan struct{}, 1),
		rearmBalance: make(chan struct{}, 1),
	}
}

// ExpiringSoonWindow Вернуть текущее окно маршрутизации с истекающим сроком (при чтении запись вместе с hot-конфигом в schedMu синхронизация ниже).
func (s *Scheduler) ExpiringSoonWindow() time.Duration {
	s.schedMu.Lock()
	defer s.schedMu.Unlock()
	return s.cfg.ExpiringSoonWindow
}

// SetExpiringSoonWindow горячее обновление окна скоро истекающих маршрутов. При изменении окна очистить старые снапшоты в пуле, чтобы в следующем раунде
// До перезаписи обновления баланса продолжать выбор номера по порядку самого раннего истечения из старого окна.
func (s *Scheduler) SetExpiringSoonWindow(d time.Duration) {
	if d < 0 {
		d = 0
	}
	s.schedMu.Lock()
	changed := s.cfg.ExpiringSoonWindow != d
	s.cfg.ExpiringSoonWindow = d
	poolRef := s.cfg.Pool
	s.schedMu.Unlock()
	if changed && poolRef != nil {
		poolRef.ClearExpiringSnapshots()
	}
}

// Reconfigure Горячее обновление параметров расписания (вызов после сохранения конфигурации в панели): изменить момент/переключатель с уведомлением активного цикла о пересчете.
// пустой hours считается "не настроено», сохраняется исходное значение (с config.normalize семантика отката совпадает).
// SetGrowthHook Монтирование/Заменить на колбэк очереди задач роста (panel создано позже scheduler，последующее подключение).
func (s *Scheduler) SetGrowthHook(fn func()) {
	s.schedMu.Lock()
	s.cfg.GrowthHook = fn
	s.schedMu.Unlock()
}

func (s *Scheduler) Reconfigure(checkinHours, travelHours, activityHours, keepaliveHours, blackcatHours, growthHours []int,
	checkinDisabled, travelDisabled, activityDisabled, keepaliveDisabled, blackcatDisabled, growthDisabled bool) {
	s.schedMu.Lock()
	if len(checkinHours) > 0 {
		s.cfg.CheckinHours = checkinHours
	}
	if len(travelHours) > 0 {
		s.cfg.TravelHours = travelHours
	}
	if len(activityHours) > 0 {
		s.cfg.ActivityHours = activityHours
	}
	if len(keepaliveHours) > 0 {
		s.cfg.KeepaliveHours = keepaliveHours
	}
	if len(blackcatHours) > 0 {
		s.cfg.BlackcatHours = blackcatHours
	}
	if len(growthHours) > 0 {
		s.cfg.GrowthHours = growthHours
	}
	s.cfg.CheckinDisabled = checkinDisabled
	s.cfg.TravelDisabled = travelDisabled
	s.cfg.ActivityDisabled = activityDisabled
	s.cfg.KeepaliveDisabled = keepaliveDisabled
	s.cfg.BlackcatDisabled = blackcatDisabled
	s.cfg.GrowthDisabled = growthDisabled
	s.schedMu.Unlock()
	poke(s.rearmSchedule)
	poke(s.rearmBalance)
}

// poke Неблокирующе отправить один сигнал пробуждения (если сигнал уже в очереди — игнорировать, семантически эквивалентно).
func poke(ch chan struct{}) {
	select {
	case ch <- struct{}{}:
	default:
	}
}

// nextFire вернуть now ближайшее время срабатывания ровно по часу после;hours — локальный час (0-23）。
func nextFire(now time.Time, hours []int) time.Time {
	var earliest time.Time
	for _, h := range hours {
		t := time.Date(now.Year(), now.Month(), now.Day(), h, 0, 0, 0, now.Location())
		if !t.After(now) {
			t = t.Add(24 * time.Hour)
		}
		if earliest.IsZero() || t.Before(earliest) {
			earliest = t
		}
	}
	return earliest
}

// taskKind тип задачи планировщика.
type taskKind int

const (
	taskCheckin taskKind = iota
	taskTravel
	taskActivity
	taskKeepalive
	taskBlackcat
	taskGrowth
)

// nextWake вернуть now ближайший момент пробуждения после и все задачи, подлежащие выполнению в этот момент.
// если несколько типов задач назначены на один час (напр., чекин и путешествие содержат 9），В этот момент требуется выполнить несколько типов задач одновременно.
// явно отключенные задачи не попадают в кандидаты (nextFire для его нулевого значения вернуть нулевое время,nextWake снова пропустить нулевую точку).
// параметры расписания в schedMu снапшот ниже, и Reconfigure изоляция конкурентной записи.
func (s *Scheduler) nextWake(now time.Time) (time.Time, []taskKind) {
	s.schedMu.Lock()
	checkinHours, keepaliveHours, blackcatHours := s.cfg.CheckinHours, s.cfg.KeepaliveHours, s.cfg.BlackcatHours
	travelHours, activityHours := s.cfg.TravelHours, s.cfg.ActivityHours
	checkinOff, keepaliveOff, blackcatOff := s.cfg.CheckinDisabled, s.cfg.KeepaliveDisabled, s.cfg.BlackcatDisabled
	growthHours, growthOff := s.cfg.GrowthHours, s.cfg.GrowthDisabled
	travelOff, activityOff := s.cfg.TravelDisabled, s.cfg.ActivityDisabled
	s.schedMu.Unlock()

	type slot struct {
		at time.Time
		kind taskKind
	}
	var slots []slot
	if !checkinOff {
		slots = append(slots, slot{nextFire(now, checkinHours), taskCheckin})
	}
	if !travelOff {
		slots = append(slots, slot{nextFire(now, travelHours), taskTravel})
	}
	if !activityOff {
		slots = append(slots, slot{nextFire(now, activityHours), taskActivity})
	}
	if !keepaliveOff {
		slots = append(slots, slot{nextFire(now, keepaliveHours), taskKeepalive})
	}
	if !blackcatOff {
		slots = append(slots, slot{nextFire(now, blackcatHours), taskBlackcat})
	}
	if !growthOff {
		slots = append(slots, slot{nextFire(now, growthHours), taskGrowth})
	}
	var earliest time.Time
	for _, sl := range slots {
		if sl.at.IsZero() {
			continue
		}
		if earliest.IsZero() || sl.at.Before(earliest) {
			earliest = sl.at
		}
	}
	if earliest.IsZero() {
		return time.Time{}, nil
	}
	var kinds []taskKind
	for _, sl := range slots {
		if !sl.at.IsZero() && sl.at.Equal(earliest) {
			kinds = append(kinds, sl.kind)
		}
	}
	return earliest, kinds
}

// wakeupGraceDelay Грейс-период сети перед диспетчеризацией для догоняющего пробуждения:Windows Modern Standby exit После
// сетевой стек/DNS 1-2s восстанавливается только тогда (issue #152 На практике dial tcp lookup no such host и
// Kernel-Power 507 standby exit ≤1s совпадение), грейс-период 5s перекрытие 90%+ сценарий пробуждения.
// действует только для запоздалого догона (триггер вовремя — нулевая задержка), без конфигурации. Для теста можно сократить (с
// travelAccountDelay「в тесте можно установить 0」в той же метрике).
var wakeupGraceDelay = 5 * time.Second

// wakeupLateThreshold порог определения опоздания:now Опоздание от планового времени слота более чем на 1s только тогда считается наверстыванием опоздания.
// джиттер в миллисекундах (timer смещение уровня нормального срабатывания) не считается, чтобы избежать ложного продления при срабатывании точно по времени.
const wakeupLateThreshold = 1 * time.Second

// awaitWakeupGrace Grace-период сети перед догоняющей диспетчеризацией при позднем пробуждении: момент слота просрочен сверх порога
// （машина только вышла из сна) сначала дождаться заполнения wakeupGraceDelay Дать сетевому стеку/DNS Диспетчеризация после готовности.
// Точно по времени/Джиттер в пределах порога — пропуск без задержки.ctx отмена — немедленный возврат false（Graceful shutdown без ожидания grace-периода
// сон истёк, текущая партия отбрасывается, следующий раунд nextWake как и прежде из"сейчас"отсчёта). Вернуть, продолжать ли раздачу.
func awaitWakeupGrace(ctx context.Context, planned time.Time) bool {
	if late := time.Since(planned); late <= wakeupLateThreshold {
		return ctx.Err() == nil // триггер ровно по времени: пропуск без задержки
	}
	log.Printf("wakeup grace %s: late catch-up for slot %s", wakeupGraceDelay, planned.Format("15:04"))
	return sleepCtx(ctx, wakeupGraceDelay)
}

// wallclockCheckStep Проверка длительности по wall-clock: однократно при ожидании слота timer максимальная длительность, каждый интервал пробуждения использует
// Повторная проверка по wall-clock, наступило ли время. Значение — компромисс между "точностью момента» и "частотой пробуждения в простое» —60s Момент внутри интервала
// верхний предел отклонения 60s，Для чекина/задач keep-alive достаточно.
const wallclockCheckStep = time.Minute

// slotWake waitSlot трехстатусный результат.
type slotWake int

const (
	slotFired slotWake = iota // Wall-clock достиг планового времени: догнать текущую партию
	slotRearm // расписание изменено (Reconfigure）：верхний уровень пересчитывает следующее пробуждение
	slotCancel // ctx Отмена: graceful shutdown верхнего уровня
)

// waitSlot Поэтапное ожидание до next **Wall-clock**момент (next От nextFire использовать time.Date Конструирование,
// Не несет монотонных показаний,time.Until для него это чистая разница wall-clock).
//
// почему не за один раз time.NewTimer(time.Until(next)) Спать до конца:timer ожидание на монотонных часах,
// macOS / Windows Modern Standby Сон заморозит его — если длительность сна меньше всего ожидания,fire
// откладывается "длительность сна» (время по часам уже прошло,timer еще нужно ждать), момент упущен и не будет немедленного догона;
// Превышение сна над всем ожиданием безвредно (момент пробуждения timer истекает,awaitWakeupGrace догоняющий прогон).
// сегментированный сон, после каждого сегмента перепроверка по wall-clock, влияние фриза ограничено одним сегментом: первый сегмент после сна
// в конце обязательно обнаружится "системное время уже прошло момент» и сразу выполнится догон, верхний предел отклонения = step + Запас интервала сна.
//
// ctx отмена / rearmSchedule（онлайн-изменение конфигурации и пересортировка) в каждом сегменте select внутри возврат в любой момент, длина сегмента
// Не влияет на отзывчивость обоих. Возврат трех состояний см. slotWake。
func (s *Scheduler) waitSlot(ctx context.Context, next time.Time, step time.Duration) slotWake {
	for {
		wallRemain := time.Until(next)
		if wallRemain <= 0 {
			return slotFired
		}
		d := wallRemain
		if d > step {
			d = step
		}
		timer := time.NewTimer(d)
		select {
		case <-ctx.Done():
			timer.Stop()
			return slotCancel
		case <-s.rearmSchedule:
			timer.Stop()
			return slotRearm
		case <-timer.C:
			// в конце сегмента возврат к началу цикла с перепроверкой по wall-clock: при норм. продвижении срабатывание через N сегментов; монотонные часы
			// При заморозке сна системные часы сильно уходят вперёд, догоняющий запуск не позже чем через один интервал.
		}
	}
}

// Run главный цикл, блокировка до ctx Отмена.
// Reconfigure Триггер rearmSchedule при этом раннее пробуждение и пересчёт (новая точка времени/переключатель применяется немедленно).
func (s *Scheduler) Run(ctx context.Context) {
	for {
		next, kinds := s.nextWake(time.Now())
		if next.IsZero() {
			// Все 4 типа задач отключены: без холостого хода, ждать уведомления о перепланировании (включение онлайн-конфигом) или сигнала выхода.
			select {
			case <-ctx.Done():
				return
			case <-s.rearmSchedule:
				continue
			}
		}
		switch s.waitSlot(ctx, next, wallclockCheckStep) {
		case slotCancel:
			return
		case slotRearm:
			continue // Расписание изменено: пересчитать следующее пробуждение
		case slotFired:
			// Задача по расписанию фиксируется при планировании (не зависит от часа пробуждения), даже при позднем пробуждении не будет пропущена.
			// Позднее пробуждение (сон пересек момент слота,timer истекает только в момент пробуждения) сначала ждать сетевой grace-период:
			// Момент пробуждения DNS не готово, выдача с нулевым грейсом тратит единственный шанс догнать на заведомо неуспешный
			// В окне (issue #152）；триггер ровно по времени с нулевой задержкой не затрагивается.
			if !awaitWakeupGrace(ctx, next) {
				return // ctx Отмена: отбросить текущую партию, корректный выход
			}
			// При пробуждении — параллельная рассылка всего: по одному на тип goroutine，Семейство медленных задач (напр. отчёт об активности
			// мульти-аккаунт × Интервал ≈ сон на несколько минут) больше не блокирует другие семейства задач в том же слоте; перед возвратом дождаться всех
			// Завершение задачи (следующий раунд nextWake как и прежде из"сейчас"отсчёт, риск перекрытия нескольких раундов и
			// аналогично для серийной версии —nextWake выбирать только моменты после текущего).
			s.runBatch(ctx, kinds)
		}
	}
}

// runBatch Параллельная отправка пакета задач (несколько типов задач в один момент пробуждения), возврат после завершения всех.
// ctx При отмене — быстрое завершение через отменяемое ожидание внутри каждой задачи.
func (s *Scheduler) runBatch(ctx context.Context, kinds []taskKind) {
	var wg sync.WaitGroup
	for _, k := range kinds {
		wg.Add(1)
		go func(k taskKind) {
			defer wg.Done()
			switch k {
			case taskCheckin:
				s.RunCheckinNow()
			case taskTravel:
				s.RunTravelNow()
			case taskActivity:
				s.runActivity(ctx)
			case taskKeepalive:
				s.RunKeepaliveNow()
			case taskBlackcat:
				s.RunBlackcatNow()
			case taskGrowth:
				// Очередь задач роста: колбэк в panel асинхронный запуск на стороне (возврат не ждёт завершения),nil Если не смонтировано — пропустить.
				s.schedMu.Lock()
				hook := s.cfg.GrowthHook
				s.schedMu.Unlock()
				if hook != nil {
					hook()
				}
			}
		}(k)
	}
	wg.Wait()
}

// sleepCtx отменяемое ожидание:ctx отмена — немедленный возврат false（graceful shutdown не ждет пробуждения rate limiter),
// Ждать заполнения и вернуть true。d<=0 Немедленно пропустить.
func sleepCtx(ctx context.Context, d time.Duration) bool {
	if d <= 0 {
		return ctx.Err() == nil
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

// RunCheckinNow немедленно выполнить check-in для всех аккаунтов + обновление баланса + Разморозка.
// аккаунты на кулдауне тоже участвуют (чекин как раз для их разморозки); отключенные — пропуск.
// путешествие отделено от чекина в отдельный график (travel_hours），больше не совмещать с чекином.
// В конце добавлен менеджер серийного входа (streak.go）：автообмен доступных номиналов + Попытки розыгрыша автоматически расходуются —
// Обмен за серию входов разблокируется по дням, привязан к ежедневному чекину — "в день достижения автоматически завершает обмен»→замкнутый цикл розыгрыша».
func (s *Scheduler) RunCheckinNow() {
	expiringSoon := s.ExpiringSoonWindow()
	for _, st := range s.cfg.Pool.List() {
		if st.Disabled {
			continue
		}
		a := s.cfg.Pool.AuthByUID(st.UID)
		if a == nil || a.RefreshTokenValue() == "" {
			continue
		}
		// D4 Гейт:realm=global У аккаунта нет системы чекинов, пропуск (без вызовов апстрима во избежание риск-контроля).
		// Через auth.Realm() Единое решение: аварийный выход (global.enabled=false）вниз global аккаунт понижен до cn、
		// Нажать CN обработка — намеренная семантика аварийного выхода (чисто CN деплой жестко блокирует всё global），Совпадает с местом использования.
		if a.IsGlobal() {
			continue
		}
		if err := s.cfg.Upstream.DailyCheckin(a); err != nil {
			// "Сегодня уже отмечено«это идемпотентный успех (апстрим на повторную регистрацию возвращает code!=0），Больше не считать сбоем error строк.
			if upstream.IsAlreadyCheckin(err) {
				s.cfg.Pool.NoteCheckinDone(st.UID)
				log.Printf("checkin %s: Сегодня уже отмечено (идемпотентно)", logfmt.Label(st.UID, st.Nickname))
			} else {
				log.Printf("checkin %s: %v", logfmt.Label(st.UID, st.Nickname), err)
			}
			// Остальные бизнес-ошибки также переходят к запросу баланса
		} else {
			// до первого успешного чекина — тишина — при диагностике "сработал ли чекин» следов нет (идемпотентная строка только в
			// появляется при повторном срабатывании), успех тоже пишет строку.
			s.cfg.Pool.NoteCheckinDone(st.UID)
			log.Printf("checkin %s: успешная регистрация", logfmt.Label(st.UID, st.Nickname))
		}
		// Проверка баланса по бакетам: баллы внутри окна конфигурации помечаются отдельно, одновременно фиксируется ближайшая будущая партия к истечению.
		remain, total, expiring, earliestAt, earliestRemaining, err := s.cfg.Upstream.UserResourceDetailedWithExpiry(a, expiringSoon)
		if err != nil {
			log.Printf("user-resource %s: %v", logfmt.Label(st.UID, st.Nickname), err)
			continue
		}
		s.cfg.Pool.ReenableIfCredits(st.UID, remain, total)
		s.cfg.Pool.SetCreditsDetailed(st.UID, remain, total, expiring, earliestAt, earliestRemaining)
	}
	s.RunStreakBonusNow()
}

// RunActivityNow Немедленно выполнить отчет об активности диалога для всех доступных аккаунтов в пуле.
// отключённые аккаунты пропускаются; нет AccessToken пропуск; лимит между аккаунтами activityAccountDelay。
// один репорт одновременно подсвечивает growth Последовательный вход + Разблокировать first_buddy Задача.
// После успешной отправки — догоняющий прогон streak Самопроверка (checkActivityStreak）：Обратное чтение дней подряд, обнаружено
// 「Отчёт 200 Но streak тихое отбрасывание "не выросло» (только чтение oracle，Без ретрая).
// RunActivityNow отсутствует ctx Внешняя точка входа (панель/Тест — одноразовый триггер); основной цикл планировщика —
// runActivity（ctx при отмене сразу бросить оставшиеся аккаунты, не дожидаясь сна лимита).
func (s *Scheduler) RunActivityNow() {
	s.runActivity(context.Background())
}

// runActivity Обход активного репорта, вместе с ctx Отмена — немедленный выход.
func (s *Scheduler) runActivity(ctx context.Context) {
	first := true
	for _, st := range s.cfg.Pool.List() {
		if st.Disabled {
			continue
		}
		a := s.cfg.Pool.AuthByUID(st.UID)
		if a == nil || a.AccessTokenValue() == "" {
			continue
		}
		if a.IsGlobal() {
			continue // D4 Гейт:global Нет центра задач/Активная система, без upstream-вызовов
		}
		if !first {
			if !sleepCtx(ctx, activityAccountDelay) {
				return // graceful shutdown: не ждать окончания sleep лимита, оставшиеся аккаунты — в следующем раунде
			}
		}
		first = false
		cid := fmt.Sprintf("wb2api-%d", time.Now().UnixMilli())
		if err := s.cfg.Upstream.ReportChatActivity(a, cid, ""); err != nil {
			log.Printf("activity %s: %v", logfmt.Label(a.UID, a.Nickname), err)
			continue
		}
		s.checkActivityStreak(a) // Отчёт успешно отправлен → обратное чтение streak Самопроверка
	}
}

// checkActivityStreak После успешной отправки перечитать дни непрерывного входа (только чтение oracle，обнаружен тихий сбой).
// контекст:REPORT-active-map.md §2 Фактически "отчет 200 но тихо отбрасывать» (отсутствует userId Время progress без изменений),
// Отчёт 200 ≠ streak Подсчет очков — требуется замкнутый цикл проверки обратным чтением.
// Критерий детекции аномалий:days==0 → warn（report OK but streak.days=0 (silent drop?)）；
// GET ошибка → warn но не влияет на основной поток (репорт уже успешен, идемпотентность по суткам, без ретрая).
// Лог — одна строка на аккаунт, наглядно grep：`activity %s: streak days=%d`（логируется и при успехе, для сверки).
// вернуть true Означает "отчет OK Но streak подозрительный» (days==0 или ошибка обратного чтения), для assert'ов теста.
func (s *Scheduler) checkActivityStreak(a *auth.Auth) bool {
	days, err := s.cfg.Upstream.GrowthStreak(a)
	if err != nil {
		log.Printf("activity %s: streak check failed (report OK): %v", logfmt.Label(a.UID, a.Nickname), err)
		return true
	}
	if days == 0 {
		log.Printf("activity %s: report OK but streak.days=0 (silent drop?)", logfmt.Label(a.UID, a.Nickname))
		return true
	}
	log.Printf("activity %s: streak days=%d", logfmt.Label(a.UID, a.Nickname), days)
	return false
}

// RunKeepaliveNow Немедленно обновить все аккаунты token；session автоотключение нерабочего.
// 12153 Отключение через Pool.NoteSessionDead **Непрерывный счётчик**Семантика: однократный сбой обновления больше не банит аккаунт сразу,
// непрерывно sessionDeadThreshold раз (3 раз) только тогда отключать (P0-1：13 шт. disabled — всё исторические ложноположительные).
// Обновление успешно → ClearSessionDead сброс счётчика (у ошибочно помеченных аккаунтов есть путь восстановления).
func (s *Scheduler) RunKeepaliveNow() {
	for _, st := range s.cfg.Pool.List() {
		if st.Disabled {
			continue
		}
		a := s.cfg.Pool.AuthByUID(st.UID)
		if a == nil || a.RefreshTokenValue() == "" {
			continue
		}
		if err := s.cfg.Upstream.RefreshToken(a); err != nil {
			log.Printf("keepalive %s: %v", logfmt.Label(st.UID, st.Nickname), err)
			var ue *upstream.Error
			if errors.As(err, &ue) && ue.Kind == upstream.ErrSessionDead {
				if s.cfg.Pool.NoteSessionDead(st.UID) {
					log.Printf("keepalive %s: непрерывно %d Раз 12153 session dead — Отключено", logfmt.Label(st.UID, st.Nickname), pool.SessionDeadThreshold())
				}
			}
			continue
		}
		s.cfg.Pool.ClearSessionDead(st.UID) // При успешном обновлении сбросить счетчик ложных срабатываний, при ошибке не накапливать
		if err := a.SaveAtomic(); err != nil {
			log.Printf("keepalive %s save: %v", logfmt.Label(st.UID, st.Nickname), err)
		}
	}
}

// RunBalanceRefreshNow Параллельно опросить баланс всех не отключенных аккаунтов и обновить в пуле credits。
// Семантика разморозки как у чекина (ReenableIfCredits：Баланс > 0 охлаждаемые аккаунты автоматически размораживаются),
// но без чекина и без обновления token——Разрешать только"Баллы"эта метрика поддерживается актуальной.
// для повторного использования двумя типами входов: фоновая периодическая задача (StartBalanceRefresh）и ручное полное обновление панели.
func (s *Scheduler) RunBalanceRefreshNow() {
	var wg sync.WaitGroup
	expiringSoon := s.ExpiringSoonWindow()
	for _, st := range s.cfg.Pool.List() {
		if st.Disabled {
			continue
		}
		a := s.cfg.Pool.AuthByUID(st.UID)
		if a == nil {
			continue
		}
		wg.Add(1)
		go func(a *auth.Auth, uid string) {
			defer wg.Done()
			remain, total, expiring, earliestAt, earliestRemaining, err := s.cfg.Upstream.UserResourceDetailedWithExpiry(a, expiringSoon)
			if err != nil {
				log.Printf("balance %s: %v", logfmt.Label(uid, a.Nickname), err)
				return
			}
			s.cfg.Pool.ReenableIfCredits(uid, remain, total)
			s.cfg.Pool.SetCreditsDetailed(uid, remain, total, expiring, earliestAt, earliestRemaining)
		}(a, st.UID)
	}
	wg.Wait()
}

// StartBalanceRefresh Фоновое периодическое обновление баланса (независимо ticker goroutine，ctx отмена — немедленный стоп).
// interval<=0 Не запускать (schedule.balance_refresh_enabled=false Время main достаточно не вызывать).
// независимо от Run почасовое расписание: баланс — минутная метрика, расширять ради него нецелесообразно nextFire гранулярность.
// Доступно во время выполнения SetBalanceInterval Горячее изменение интервала (вступит в силу в следующем раунде).
func (s *Scheduler) StartBalanceRefresh(ctx context.Context, interval time.Duration) {
	if interval <= 0 {
		return
	}
	s.balanceInterval.Store(int64(interval))
	go func() {
		var logged time.Duration
		for {
			cur := time.Duration(s.balanceInterval.Load())
			if cur != logged {
				log.Printf("scheduler: Фоновое обновление баланса каждые %s（отображение на паузе 0s）", cur)
				logged = cur
			}
			if cur <= 0 {
				// Приостановлено hot-изменением: ждать уведомления о перестановке (пробуждение при повторном включении) или выйти.
				select {
				case <-ctx.Done():
					return
				case <-s.rearmBalance:
					continue
				}
			}
			timer := time.NewTimer(cur)
			select {
			case <-ctx.Done():
				timer.Stop()
				return
			case <-s.rearmBalance:
				timer.Stop() // Интервал изменён: немедленный пересчёт по новому значению
			case <-timer.C:
				s.RunBalanceRefreshNow()
			}
		}
	}()
}

// SetBalanceInterval Горячее изменение интервала обновления баланса;<=0 означает паузу цикла (когда переключатель выключен на панели).
func (s *Scheduler) SetBalanceInterval(d time.Duration) {
	if d < 0 {
		d = 0
	}
	s.balanceInterval.Store(int64(d))
	poke(s.rearmBalance)
}
