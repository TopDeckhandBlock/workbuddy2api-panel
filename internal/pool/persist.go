// Персистентность: локально state.json сброс на диск/загрузка,Redis Снапшот-образ (StoreSnapshotter）、
// бэкенд flusher、Выбрать новое для восстановления (RestoreFromSnapshot）。
package pool

import (
	"encoding/json"
	"log"
	"os"
	"path/filepath"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
)

var flushInterval = 5 * time.Second

// persistLogEvery при непрерывных ошибках записи на диск каждые N раз — одно напоминание (flusher 5s за один раз ≈ 1 раз в минуту),
// Избежать длительного переполнения диска/при потере прав — спам в логах.
const persistLogEvery = 12

// snapshot снимок состояния пула (Redis для зеркала). С локальным state.json Тот же источник (stateFile），
// Дополнительно с savedAt Метка времени для"Восстановление выбором нового"（Сравнение локального и Redis свежести снапшота).
type snapshot struct {
	stateFile
	SavedAt time.Time `json:"saved_at"`
}

// Pool Пул аккаунтов.
type StoreSnapshotter interface {
	SaveState(data []byte)
	LoadState() ([]byte, bool)
}

// RestoreFromSnapshot восстановление с выбором нового: сравнить локальное state.json и Redis снимок, берётся более новый.
//
// Локально**доступно**выбор по новизне (снапшот не старше локального → брать снапшот, иначе приоритет локального); локально**Недоступно**
// （state.json отсутствует или нечитаем, типично при первом запуске на новом томе/при запуске нового узла)**использовать снапшот**——в этот момент локально вообще
// нет доступных"Приоритет"состояния, снапшот — единственный источник рантайм-состояния в этом раунде, в этом суть снапшота как "бэкапа восстановления при запуске»
// сценарий. Ветвление: без снапшота / снимок без savedAt → Приоритет локального (нет критериев для сравнения).
//
// каждый случай логирует свой источник восстановления для сверки. Должно быть в SyncToDir Вызов до
// （SyncToDir только add/delete без записи значений: значения только локально load или данная функция использует снапшот).
func (p *Pool) RestoreFromSnapshot() {
	store := p.store
	if store == nil || p.stateFp == "" {
		return
	}
	localInfo, localErr := os.Stat(p.stateFp)
	raw, ok := store.LoadState()
	if !ok {
		if localErr == nil {
			log.Printf("pool: Источник восстановления=Локально state.json（отсутствует Redis снимок)")
		}
		return
	}
	var snap snapshot
	if json.Unmarshal(raw, &snap) != nil || snap.SavedAt.IsZero() {
		// снимок без savedAt：невозможно сравнить старое и новое, приоритет у локального.
		log.Printf("pool: Источник восстановления=Локально state.json（Redis снимок без saved_at）")
		return
	}
	if localErr != nil {
		// Локально недоступно → использовать снапшот (локально нет доступного"Приоритет"состояния).
		//
		// старая реализация объединяла этот случай с "локально новее» в один fall-through：не меняет память, не устанавливает dirty
		//（валидный снапшот тихо отброшен), и снова выведено"Локально state.json（Новее чем Redis Снапшот …）"——Один раз
		// Сравнение, которого никогда не было, уводило отладку к несуществующему локальному файлу; затем SyncToDir Только добавление/удаление, без записи значений,
		// Рабочее состояние всего пула (credits/Охлаждение/счётчик circuit-breaker/usedSeq/lastUsed）Обнулено.
		p.adoptSnapshot(snap)
		log.Printf("pool: Источник восстановления=Redis Снапшот (saved_at=%s)（Локально state.json Недоступно: %v）",
			snap.SavedAt.Format(time.RFC3339), localErr)
		return
	}
	if !localInfo.ModTime().After(snap.SavedAt) {
		// Снапшот не раньше локального → Используется снапшот.
		p.adoptSnapshot(snap)
		log.Printf("pool: Источник восстановления=Redis Снапшот (saved_at=%s)", snap.SavedAt.Format(time.RFC3339))
		return
	}
	// Достижение этой точки означает "локально существует и строго новее снапшота», вывод лога достоверен.
	log.Printf("pool: Источник восстановления=Локально state.json（Новее чем Redis Снапшот %s）", snap.SavedAt.Format(time.RFC3339))
}

// Acquire занимает один in-flight слот аккаунта;false означает, что аккаунт достиг лимита (или не существует).
// Должно быть при успехе Pick последующий вызов; ответственность вызывающей стороны defer Release。
func (p *Pool) startFlusher() {
	stopCh := make(chan struct{})
	// при запуске goroutine синхронная запись перед (во избежание конфликта с тестом flushInterval гонка восстанавливающей записи);
	// stopCh Синхронная регистрация,Close только тогда можно надежно остановить (New и startFlusher между ними нет окна конкурентности).
	p.stopCh = stopCh
	interval := flushInterval
	go func() {
		t := time.NewTicker(interval)
		defer t.Stop()
		for {
			select {
			case <-t.C:
				p.mu.Lock()
				if p.dirty.Swap(false) {
					p.saveLocked()
				}
				p.mu.Unlock()
			case <-stopCh:
				return
			}
		}
	}()
}

// Flush синхронно сбросить состояние памяти на диск (идемпотентно: без изменений запись не выполняется). Вызывать перед выходом процесса.
func (p *Pool) Flush() {
	p.mu.Lock()
	if p.dirty.Swap(false) {
		p.saveLocked()
	}
	p.mu.Unlock()
}

// Add Добавить аккаунт; если уже существует — сохранить статус, обновить credentials (upsert один аккаунт, не влияет на другие аккаунты).
func (p *Pool) load() {
	raw, err := os.ReadFile(p.stateFp)
	if err != nil {
		return
	}
	var sf stateFile
	if json.Unmarshal(raw, &sf) != nil {
		return
	}
	p.applyAccountsLocked(sf.Accounts)
}

// applyAccountsLocked Перекрыть персистентным состоянием аккаунта/Вставка byUID（placeholder учетные данные,Add при замене всего).
// Локально load() и Redis общее восстановление из снапшота; вызывающая сторона уже должна иметь p.mu。
func (p *Pool) applyAccountsLocked(accounts map[string]stateAccount) {
	now := time.Now()
	for uid, s := range accounts {
		// err_total приоритет; у старого файла err_count（непрерывных ошибок) как одноразовый источник миграции (берется большее из двух,
		// Максимально сохранять исторические сигналы наблюдения — в старой семантике err_count также реально происходили ошибки, нельзя отбрасывать).
		errTotal := s.ErrTotal
		if int64(s.ErrCount) > errTotal {
			errTotal = int64(s.ErrCount)
		}
		e := &entry{
			a: &auth.Auth{UID: uid}, // placeholder，Add будет заменено на полные учетные данные
			credits: s.Credits,
			creditsTotal: s.CreditsTotal,
			creditsExpiring: s.CreditsExpiring,
			creditsEarliestExpiry: s.CreditsEarliestExpiry,
			creditsEarliestRemaining: s.CreditsEarliestRemaining,
			disabled: s.Disabled,
			reason: s.Reason,
			until: s.Until,
			coolKind: s.CoolKind,
			successCount: s.SuccessCount,
			errTotal: errTotal,
			lastErr: s.LastErr,
			lastSuccess: s.LastSuccess,
			lastCheckinDay: s.LastCheckinDay,
			tokenUsage: s.TokenUsage,
			softStreak: s.SoftStreak,
			sessionDeadFails: s.SessionDeadFails,
			consecutiveFails: s.ConsecutiveFails,
		}
		// ленивая очистка снапшота истечения на текущий момент: просроченные, с нулевым остатком или превышающие общий баланс грязные данные не восстанавливаются.
		if e.creditsExpiring < 0 {
			e.creditsExpiring = 0
		}
		if e.creditsExpiring > e.credits {
			e.creditsExpiring = e.credits
		}
		if e.creditsEarliestRemaining < 0 || e.creditsEarliestRemaining > e.credits {
			e.creditsEarliestRemaining = 0
		}
		if e.creditsEarliestRemaining == 0 || e.creditsEarliestExpiry.IsZero() || !now.Before(e.creditsEarliestExpiry) {
			e.creditsEarliestExpiry = time.Time{}
			e.creditsEarliestRemaining = 0
		}
		// восстановление персистентности circuit breaker:breakerUntil восстанавливать только неистёкшее (истёкшее не оживлять),retryCount Только при
		// сохранять пока circuit breaker активен (иначе сброс в ноль, не хранить бесполезный backoff).
		if s.BreakerUntil != nil && now.Before(*s.BreakerUntil) {
			e.breakerUntil = *s.BreakerUntil
			e.retryCount = s.RetryCount
		}
		// Понижение веса при серии неудач: восстановление только если не истекло (истекло/нулевое значение не пишется и не восстанавливается).
		if s.DegradeUntil != nil && now.Before(*s.DegradeUntil) {
			e.degradeUntil = *s.DegradeUntil
		}
		// Независимый кулдаун на уровне модели (6004 сброс wall-clock / 11102 негативный кэш): ленивая фильтрация просроченных записей.
		if len(s.ModelCooldowns) > 0 {
			for m, mc := range s.ModelCooldowns {
				if mc.Until.IsZero() || !now.Before(mc.Until) {
					continue
				}
				if e.modelCooldowns == nil {
					e.modelCooldowns = map[string]modelCooldown{}
				}
				e.modelCooldowns[m] = modelCooldown{Until: mc.Until, ResetAt: mc.ResetAt, Reason: mc.Reason, AuditOnly: mc.AuditOnly}
			}
		}
		// книга стоимости: ленивая фильтрация просроченного (modelCostTTL вне — не восстанавливается)+ отсеять записи с битой структурой
		// （Отрицательный per1k / Ноль LastSeen——аномалия апстрима или грязные данные от ручной правки старого файла).
		if len(s.ModelCosts) > 0 {
			for m, mc := range s.ModelCosts {
				if mc.LastSeen.IsZero() || now.Sub(mc.LastSeen) > modelCostTTL || mc.CostPer1k < 0 {
					continue
				}
				if e.modelCost == nil {
					e.modelCost = map[string]modelCostEntry{}
				}
				e.modelCost[m] = modelCostEntry{CostPer1k: mc.CostPer1k, LastSeen: mc.LastSeen, Samples: mc.Samples}
			}
		}
		p.byUID[uid] = e
	}
}

// applySnapshotLocked использовать Redis Снапшот перекрывает состояние в памяти (уже применено после решения о выборе нового). Вызывающая сторона должна уже удерживать p.mu。
// adoptSnapshot использовать Redis снапшот — текущее состояние пула, и установить dirty при следующей записи материализовать локально
// state.json（иначе снапшот действует только в памяти, после восстановления при сбое вернётся старый локальный файл).
func (p *Pool) adoptSnapshot(s snapshot) {
	p.mu.Lock()
	p.applySnapshotLocked(s)
	p.mu.Unlock()
	p.dirty.Store(true)
}
func (p *Pool) applySnapshotLocked(s snapshot) {
	p.byUID = map[string]*entry{}
	p.applyAccountsLocked(s.Accounts)
}
func (p *Pool) saveLocked() {
	if p.stateFp == "" {
		return
	}
	sf := p.stateOverviewLocked()
	raw, err := json.MarshalIndent(sf, "", " ")
	if err != nil {
		p.notePersistFail(err)
		return
	}
	if dir := filepath.Dir(p.stateFp); dir != "" {
		_ = os.MkdirAll(dir, 0o755)
	}
	tmp := p.stateFp + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		p.notePersistFail(err)
		return
	}
	if err := os.Rename(tmp, p.stateFp); err != nil {
		p.notePersistFail(err)
		return
	}
	if p.persistFails > 0 {
		// Восстановление после серии сбоев: писать лог восстановления, чтобы избежать"Ошибки исчерпаны, но о восстановлении никто не знает"。
		log.Printf("pool: state.json Восстановление с диска (ранее непрерывные сбои %d раз)", p.persistFails)
		p.persistFails = 0
	}
	// Синхронно зеркалировать снапшот в Redis（fire-and-forget），С локальным state.json и сохранить как бэкап для восстановления.
	if p.store != nil {
		snapRaw, err := json.Marshal(snapshot{stateFile: sf, SavedAt: time.Now()})
		if err == nil {
			p.store.SaveState(snapRaw)
		}
	}
}

// notePersistFail записать один раз локально state.json ошибка сохранения на диск, логирование по правилам троттлинга:
// Первый сбой (статус успех→сбой) логировать полную ошибку, каждый persistLogEvery последовательных сбоев — одно уведомление,
// Остальные последовательные сбои тихо (flusher 5s один раз, при длительном переполнении диска не спамить).
// Лог успешного восстановления от saveLocked единая метка на успешном пути. С redisstore Три асинхронные записи
// "при ошибке только лог, без проброса наверх"Выравнивание парадигмы, но сбой записи на диск — слепая зона для эксплуатации, поэтому доп. троттлинг (notification）。
func (p *Pool) notePersistFail(err error) {
	if p.persistFails == 0 {
		log.Printf("pool: state.json ошибка сохранения на диск: %v", err)
	} else if p.persistFails%persistLogEvery == 0 {
		log.Printf("pool: state.json Последовательные ошибки записи на диск %d Раз: %v", p.persistFails, err)
	}
	p.persistFails++
}

// stateOverviewLocked Собрать текущее состояние памяти как stateFile（Для сохранения на диск + переиспользование снапшот-зеркала). Вызывающая сторона должна уже иметь p.mu。
func (p *Pool) stateOverviewLocked() stateFile {
	now := time.Now()
	sf := stateFile{Accounts: map[string]stateAccount{}}
	for uid, e := range p.byUID {
		s := stateAccount{
			Credits: e.credits,
			CreditsTotal: e.creditsTotal,
			Disabled: e.disabled,
			Reason: e.reason,
			Until: e.until,
			CoolKind: e.coolKind,
			SuccessCount: e.successCount,
			ErrTotal: e.errTotal,
			LastSuccess: e.lastSuccess,
			LastErr: e.lastErr,
			LastCheckinDay: e.lastCheckinDay,
			TokenUsage: e.tokenUsage,
			SoftStreak: e.softStreak,
			SessionDeadFails: e.sessionDeadFails,
			ConsecutiveFails: e.consecutiveFails,
			CreditsExpiring: e.creditsExpiring,
			CreditsEarliestExpiry: e.creditsEarliestExpiry,
			CreditsEarliestRemaining: e.creditsEarliestRemaining,
		}
		// дедлайн circuit breaker: запись на диск только если не истёк (указатель nil только тогда может быть omitempty фактически опущено).
		if !e.breakerUntil.IsZero() && now.Before(e.breakerUntil) {
			u := e.breakerUntil
			s.BreakerUntil = &u
			s.RetryCount = e.retryCount
		}
		// Дедлайн понижения за серию неудач: сохранять только если не истёк.
		if !e.degradeUntil.IsZero() && now.Before(e.degradeUntil) {
			u := e.degradeUntil
			s.DegradeUntil = &u
		}
		// Независимый кулдаун на уровне модели: ленивая фильтрация просроченных записей (Hits Без записи на диск, после перезапуска 11102 бэкофф переобучается с базы).
		if len(e.modelCooldowns) > 0 {
			for m, mc := range e.modelCooldowns {
				if mc.Until.IsZero() || !now.Before(mc.Until) {
					continue
				}
				if s.ModelCooldowns == nil {
					s.ModelCooldowns = map[string]stateModelCooldown{}
				}
				s.ModelCooldowns[m] = stateModelCooldown{Until: mc.Until, ResetAt: mc.ResetAt, Reason: mc.Reason, AuditOnly: mc.AuditOnly}
			}
		}
		// Ledger стоимости: ленивая фильтрация просроченных наблюдений (modelCostTTL вне не писать — устаревшая цена не восстанавливается).
		if len(e.modelCost) > 0 {
			for m, mc := range e.modelCost {
				if mc.LastSeen.IsZero() || now.Sub(mc.LastSeen) > modelCostTTL {
					continue
				}
				if s.ModelCosts == nil {
					s.ModelCosts = map[string]stateModelCost{}
				}
				s.ModelCosts[m] = stateModelCost{CostPer1k: mc.CostPer1k, LastSeen: mc.LastSeen, Samples: mc.Samples}
			}
		}
		sf.Accounts[uid] = s
	}
	return sf
}
