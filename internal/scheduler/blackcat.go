// blackcat.go Исполнитель задач "Ночная сова»:23:00–08:00 Пополнение аккаунтов пула внутри окна glm-5.2 Диалог
// и отправить цепочку событий (black_cat критерий). Срабатывание вне окна просто пропускается (только наблюдение, без ошибки).
package scheduler

import (
	"log"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/logfmt"
	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
)

// RunBlackcatNow Для всех доступных аккаунтов выполнить добивку диалогов "ночной совы» (вне окна — пропуск).
// От blackcat_hours расписание (по умолчанию [23]）Триггер; повторная проверка перед выполнением InNightWindow。
func (s *Scheduler) RunBlackcatNow() {
	if !upstream.InNightWindow(time.Now()) {
		log.Printf("blackcat: Сейчас отсутствует 23:00–08:00 Окно подсчета, пропустить")
		return
	}
	for _, st := range s.cfg.Pool.List() {
		if st.Disabled {
			continue
		}
		a := s.cfg.Pool.AuthByUID(st.UID)
		if a == nil || a.AccessTokenValue() == "" {
			continue
		}
		if a.IsGlobal() {
			continue // D4 Гейт:global отсутствует CN Система задач, без вызовов апстрима
		}
		need, err := s.cfg.Upstream.BlackcatNeed(a)
		if err != nil {
			log.Printf("blackcat %s: %v", logfmt.Label(a.UID, a.Nickname), err)
			continue
		}
		if need <= 0 {
			continue
		}
		ok, err := s.cfg.Upstream.RunNightChats(a, int(need))
		if err != nil {
			log.Printf("blackcat %s: %d/%d завершено, прервано: %v", logfmt.Label(a.UID, a.Nickname), ok, need, err)
			continue
		}
		log.Printf("blackcat %s: Завершено %d ночных диалогов", logfmt.Label(a.UID, a.Nickname), ok)
		time.Sleep(activityAccountDelay)
	}
}
