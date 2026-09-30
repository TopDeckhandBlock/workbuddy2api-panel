package server

import (
	"sync"
	"time"
)

// degradeGate Стейт-машина деградации:passthrough когда в режиме запрос блокируется контент-политикой апстрима,
// переключиться на Degraded Нейтральный промпт до следующего дня 00:00 CST Сброс.
//
// Локализация: обработка ложных срабатываний (с internal/upstream/sanitize.go та же философия) — блокировка контента чаще
// system Ложное срабатывание отпечатка источника, обход минимальным нейтральным промптом; не adversarial-фреймворк.
//
// Конечный автомат: память процесса, сброс при перезапуске (допустимо: перезапуск срабатывает крайне редко, и custom режим вообще
// не попадает в fallback-ветку). Запросы в период деградации идут напрямую в Degraded，Больше не проверять сначала 400。
type degradeGate struct {
	mu sync.Mutex
	until time.Time
}

// Active Находится ли сейчас в периоде деградации (now < until）。
func (g *degradeGate) Active() bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	return time.Now().Before(g.until)
}

// Trigger триггерит даунгрейд до следующего дня 00:00 CST（Asia/Shanghai）。
// Если уже в периоде деградации — не продлевать (сохраняя точку earliest срабатывания 00:00 семантика сброса).
func (g *degradeGate) Trigger() {
	g.mu.Lock()
	defer g.mu.Unlock()
	if !time.Now().Before(g.until) {
		// уже истек/не сработало → Установить новый until；Если не истекло — сохранить исходное until（без продления).
		g.until = nextMidnightCST(time.Now())
	}
}

// nextMidnightCST вернуть now ближайший после Asia/Shanghai 00:00 момент (чистая функция, удобно для юнит-теста).
//
// Семантика границ:
// - 23:59 → на следующий день 00:00（через несколько секунд)
// - 00:00 → на следующий день 00:00（только что прошло 00:00，следующая полночь — уже следующие сутки)
//
// Использовать фиксированный +08:00 расчёт смещения, без зависимости от системной TZ (контейнер/часовой пояс хоста не определён).
func nextMidnightCST(now time.Time) time.Time {
	cst := time.FixedZone("CST", 8*60*60)
	// взять now Перейти к CST В представлении — текущий день 00:00，Еще на день; если 00:00 <= now то добавить еще один день.
	y, m, d := now.In(cst).Date()
	midnight := time.Date(y, m, d, 0, 0, 0, 0, cst)
	for !midnight.After(now) {
		midnight = midnight.Add(24 * time.Hour)
	}
	return midnight
}
