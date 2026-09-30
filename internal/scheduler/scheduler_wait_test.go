// scheduler_wait_test.go Закрепить waitSlot три состояния и wall-clock метрика:timer ожидание на основе монотонных
// таймер, сон машины замораживает его — ожидание по сегментам + Переоценка по wall-clock каждый интервал ограничивает влияние заморозки одним интервалом
// （「При сне меньше всего периода ожидания fire исправление кейса "перенос, пропуск wall-clock момента без догона»).
package scheduler

import (
	"context"
	"testing"
	"time"
)

func newTestScheduler() *Scheduler {
	s := New(Config{})
	return s
}

// TestWaitSlotFiresAtWallclock возврат по расписанию slotFired：
// - next Уже прошло (wall-clock прошел момент,timer ещё не заданной формы)→ вернуть немедленно, не ждать следующий чанк;
// - next ещё не наступило (многоэтапное ожидание)→ Возврат только по наступлении времени, не ранее запланированного момента.
func TestWaitSlotFiresAtWallclock(t *testing.T) {
	s := newTestScheduler()

	// Состояние, уже пройденное по wall-clock: цель исправления — заморозка сна timer после пробуждения сразу обнаружено наступление времени.
	past := time.Now().Add(-time.Second)
	if got := s.waitSlot(context.Background(), past, time.Hour); got != slotFired {
		t.Fatalf("При наступлении момента выполнить немедленно slotFired，got %v", got)
	}

	// Будущий момент: многосегментное ожидание до наступления.
	next := time.Now().Add(45 * time.Millisecond)
	start := time.Now()
	if got := s.waitSlot(context.Background(), next, 10*time.Millisecond); got != slotFired {
		t.Fatalf("По достижении времени должен slotFired，got %v", got)
	}
	if time.Since(start) < 40*time.Millisecond {
		t.Errorf("Ранний возврат: ожидание %v，Запланированное время не наступило", time.Since(start))
	}
}

// TestWaitSlotRearm получено в ожидании rearmSchedule（онлайн-изменение конфига с перестановкой)→ Немедленно slotRearm，
// Не дожидаться полного оставшегося времени (длина отрезка не влияет rearm отзывчивость).
func TestWaitSlotRearm(t *testing.T) {
	s := newTestScheduler()
	next := time.Now().Add(2 * time.Second)

	go func() {
		time.Sleep(30 * time.Millisecond)
		s.rearmSchedule <- struct{}{}
	}()
	start := time.Now()
	if got := s.waitSlot(context.Background(), next, 10*time.Millisecond); got != slotRearm {
		t.Fatalf("rearm должен вернуть slotRearm，got %v", got)
	}
	if time.Since(start) > 500*time.Millisecond {
		t.Errorf("rearm Слишком медленный ответ:%v", time.Since(start))
	}
}

// TestWaitSlotCancel ctx отмена → slotCancel，graceful shutdown.
func TestWaitSlotCancel(t *testing.T) {
	s := newTestScheduler()
	ctx, cancel := context.WithCancel(context.Background())
	next := time.Now().Add(2 * time.Second)

	go func() {
		time.Sleep(30 * time.Millisecond)
		cancel()
	}()
	start := time.Now()
	if got := s.waitSlot(ctx, next, 10*time.Millisecond); got != slotCancel {
		t.Fatalf("При отмене должен вернуть slotCancel，got %v", got)
	}
	if time.Since(start) > 500*time.Millisecond {
		t.Errorf("Отмена: слишком медленный ответ:%v", time.Since(start))
	}
}
