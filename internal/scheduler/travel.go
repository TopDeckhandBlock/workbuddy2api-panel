// travel.go Автомат состояний патруля путешествия котиков: по моментам путешествия (travel_hours，По умолчанию 09 ч.) один проход по каждому доступному аккаунту в пуле.
// без кота → принять соглашение + Усыновление; есть кот → Нажать travel/status диспетчеризация Отправить / Получение награды / Пропустить.
package scheduler

import (
	"fmt"
	"log"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/logfmt"
	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
)

const (
	// travelLocationID Место отправки фиксировано 4（гостиница в древнем городе):4 Доход по N точкам/Интервалы длительности полностью совпадают, оптимального решения нет.
	travelLocationID = 4

	// travelStateIdle Свободен — можно назначать;travelStateTraveling в пути;travelStateArrived По прибытии можно получить награду.
	travelStateIdle = "idle"
	travelStateTraveling = "traveling"
	travelStateArrived = "arrived"
)

// travelAccountDelay Лимит между аккаунтами: всего аккаунтов около 40s，во избежание риск-контроля апстрима. Для теста можно установить 0。
var travelAccountDelay = 800 * time.Millisecond

// activityAccountDelay межаккаунтный rate limit активного отчета: тот же критерий, что и для travel, во избежание риск-контроля апстрима. Для теста можно выставить 0。
var activityAccountDelay = 800 * time.Millisecond

// adoptReportGap ожидание после пред-отчёта адопции: дать время на обработку события апстримом перед отправкой buddy/first。
// Выравнивание scripts/task_first_buddy.py фактический 1.05s Интервал. В тестах можно установить 0。
var adoptReportGap = 1050 * time.Millisecond

// cstZone ежедневный сброс апстрима по календарным суткам 00:00 CST（Asia/Shanghai）。В Китае нет DST, время фиксированное +8 достаточно,
// не зависит от контейнера tzdata。
var cstZone = time.FixedZone("CST", 8*60*60)

// travelDay вернуть t относящийся к календарному дню апстрима (CST），формат 2006-01-02。
func travelDay(t time.Time) string {
	return t.In(cstZone).Format("2006-01-02")
}

// RunTravelNow Немедленно выполнить обходную проверку всех доступных аккаунтов в пуле.
// Пропуск отключенных аккаунтов;401/Ошибка запроса — пропуск только этого аккаунта в текущем раунде (без принудительного обновления token，Передача 22:00 keepalive）；
// rate-limit между аккаунтами travelAccountDelay。
func (s *Scheduler) RunTravelNow() {
	first := true
	for _, st := range s.cfg.Pool.List() {
		if st.Disabled {
			continue
		}
		a := s.cfg.Pool.AuthByUID(st.UID)
		if a == nil || a.RefreshTokenValue() == "" {
			continue
		}
		if a.IsGlobal() {
			continue // D4 Гейт:global отсутствует CN Система задач, без вызовов апстрима
		}
		if !first {
			time.Sleep(travelAccountDelay)
		}
		first = false
		s.travelOne(a)
	}
}

// travelOne Конечный автомат на аккаунт/поездку: проверка наличия кота + Проверить статус + максимум одно действие, без опроса и ожидания.
func (s *Scheduler) travelOne(a *auth.Auth) {
	buddy, err := s.cfg.Upstream.BuddyInfo(a)
	if err != nil {
		log.Printf("travel %s: buddy-info: %v", logfmt.Label(a.UID, a.Nickname), err)
		return
	}
	if buddy == nil {
		s.travelAdopt(a)
		return
	}
	ts, err := s.cfg.Upstream.TravelStatus(a)
	if err != nil {
		log.Printf("travel %s: status: %v", logfmt.Label(a.UID, a.Nickname), err)
		return
	}
	switch ts.State {
	case travelStateArrived:
		s.travelClaim(a, ts)
	case travelStateIdle:
		s.travelDepart(a, ts)
	case travelStateTraveling:
		log.Printf("travel %s: skip (traveling record=%d)", logfmt.Label(a.UID, a.Nickname), ts.RecordID)
	default:
		log.Printf("travel %s: skip (unknown state %q)", logfmt.Label(a.UID, a.Nickname), ts.State)
	}
}

// travelDepart Отправлять когда idle и не достиг дневного лимита (ежедневно 1 раз, календарный день 00:00 CST сброс).
func (s *Scheduler) travelDepart(a *auth.Auth, ts *upstream.TravelState) {
	if ts.DailyLimitReached {
		log.Printf("travel %s: skip (daily limit reached)", logfmt.Label(a.UID, a.Nickname))
		return
	}
	if err := s.cfg.Upstream.TravelDepart(a, travelLocationID); err != nil {
		log.Printf("travel %s: depart: %v", logfmt.Label(a.UID, a.Nickname), err)
		return
	}
	log.Printf("travel %s: depart ok location=%d", logfmt.Label(a.UID, a.Nickname), travelLocationID)
}

// travelClaim Получение приза на станции (необходимо иметь record_id）。
func (s *Scheduler) travelClaim(a *auth.Auth, ts *upstream.TravelState) {
	if ts.RecordID == 0 {
		log.Printf("travel %s: claim skipped (arrived but no record_id)", logfmt.Label(a.UID, a.Nickname))
		return
	}
	reward, err := s.cfg.Upstream.TravelClaim(a, ts.RecordID)
	if err != nil {
		log.Printf("travel %s: claim record=%d: %v", logfmt.Label(a.UID, a.Nickname), ts.RecordID, err)
		return
	}
	log.Printf("travel %s: claim ok record=%d reward=%d", logfmt.Label(a.UID, a.Nickname), ts.RecordID, reward)
}

// travelAdopt усыновление при отсутствии кота, цепочка:report → agreement → buddy/first。
//
// report Должен сначала выполниться (scripts/task_first_buddy.py фактически): одна chat_request_send Отчёт
// Подсветить growth последовательный логин и**Разблокировать first_buddy задача**；когда не отправлено buddy/first вернёт
// 400 "first_buddy task not completed yet«——реальный источник этого порога —«За сегодня нет отчета об активности"，
// Не проблема аккаунта (report.go В комментарии также явно "разблокировать first_buddy задача (пререквизит адопшена)»).
// conversation Недостижение порога — ожидаемое поведение, засчитать одну попытку за день, далее тихо пропускать без повтора.
func (s *Scheduler) travelAdopt(a *auth.Auth) {
	if s.adoptTriedToday(a.UID) {
		return
	}
	// Предусловие: разблокировка first_buddy Задача (идемпотентна; сбой не блокирует, пусть buddy/first отдаётся по существующему пути ошибок).
	if err := s.cfg.Upstream.ReportChatActivity(a, fmt.Sprintf("wb2api-adopt-%d", time.Now().UnixMilli()), ""); err != nil {
		log.Printf("travel %s: adopt preflight report: %v", logfmt.Label(a.UID, a.Nickname), err)
	} else {
		time.Sleep(adoptReportGap) // Дать время на обработку событий апстрима (выровнено по замерам скрипта 1.05s метрика интервала)
	}
	if err := s.cfg.Upstream.BuddyAgreement(a); err != nil {
		log.Printf("travel %s: agreement: %v", logfmt.Label(a.UID, a.Nickname), err)
		return
	}
	err := s.cfg.Upstream.BuddyFirst(a)
	switch {
	case err == nil:
		log.Printf("travel %s: adopt ok (+300 credits)", logfmt.Label(a.UID, a.Nickname))
	case upstream.IsBuddyTaskIncomplete(err):
		s.markAdoptTried(a.UID)
		log.Printf("travel %s: adopt skipped (conversation threshold not reached, retry tomorrow)", logfmt.Label(a.UID, a.Nickname))
	default:
		log.Printf("travel %s: adopt: %v", logfmt.Label(a.UID, a.Nickname), err)
	}
}

// adoptTriedToday Достиг ли этот аккаунт сегодня порога усыновления.
func (s *Scheduler) adoptTriedToday(uid string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.adoptTried[uid] == travelDay(time.Now())
}

// markAdoptTried Зафиксировать, что аккаунт сегодня пытался усыновить/получить и не прошел порог.
func (s *Scheduler) markAdoptTried(uid string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.adoptTried[uid] = travelDay(time.Now())
}
