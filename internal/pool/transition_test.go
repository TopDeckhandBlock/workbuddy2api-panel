package pool

import (
	"testing"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
)

// Тест ортогональности переходов стейт-машины: фокус transition.go Семантика сведённого "единого авторитетного автомата».
// entry выбираемость определяется четырьмя ортогональными измерениями (disabled / Cooldown-домен until+coolKind+softStreak+
// modelCooldowns / Circuit Breaker fails+retryCount+breakerUntil / sessionDeadFails），Данный файл
// Зафиксировать границы примитива миграции по этим четырем измерениям, особенно дефекты старой реализации:
// - Disable старая реализация только устанавливает disabled+reason，Не трогать until/modelCooldowns → 「disabled Но
// cooling」Гибридное состояние (сомнительно 4）。новая семантика:disableLocked Установить disabled и очистить домен охлаждения.
// - reviveCoolingLocked（Разморозка чекином) очищает только cooldown-домен, не трогает circuit breaker (C5 семантика).
//
// и pool_test.go / modelcooldown_test.go / sessiondead_test.go различия: те тесты
// Фиксирует поведение по измерениям, данный файл фиксирует границы перехода "меж-измерений» (охлаждение↔Отключено↔circuit breaker'ы не пересекаются).

// прямое чтение entry исходные поля домена кулдауна (Status не экспонировать coolKind Значение и modelCooldowns）。
func coolingDomain(t *testing.T, p *Pool, uid string) (until time.Time, coolKind CoolKind, reason string, softStreak int, modelCooldowns int) {
	t.Helper()
	p.mu.RLock()
	defer p.mu.RUnlock()
	e, ok := p.byUID[uid]
	if !ok {
		t.Fatalf("uid %s missing", uid)
	}
	return e.until, e.coolKind, e.reason, e.softStreak, len(e.modelCooldowns)
}

// TestTransitionDisableClearsCoolingDomain Подозрительные точки 4 Исправление: сначала охлаждение (мягкое охлаждение + 6004 Уровень модели
// охлаждение) затем Disable → полная очистка домена кулдауна (until/coolKind/reason/softStreak/modelCooldowns），
// оставить только disabled терминальное состояние. Старая реализация только выставляла disabled+reason，Появится "disabled=true Но cooling=true」
// и остаток modelCooldowns гибридное состояние.
func TestTransitionDisableClearsCoolingDomain(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.Cooldown("u1", CoolSoft, 600*time.Second, "429") // until + coolKind=soft + softStreak=1
	p.CooldownSoftForModel("u1", time.Minute, time.Now().Add(5*time.Minute), "glm-5.3", "6004") // modelCooldowns=1（softStreak Увеличить до 2）

	_, _, reason, _, mc := coolingDomain(t, p, "u1")
	if reason == "" || mc != 1 {
		t.Fatalf("precondition: должен сначала быть в soft-кулдауне+Кулдаун на уровне модели (reason=%q modelCooldowns=%d)", reason, mc)
	}

	p.Disable("u1", "account banned")

	st, _ := p.Status("u1")
	until, kind, reason, streak, mc := coolingDomain(t, p, "u1")
	if !st.Disabled {
		t.Fatal("Disable после следует disabled")
	}
	if !until.IsZero() {
		t.Errorf("Disable После until=%v должно быть ноль (сброс домена кулдауна)", until)
	}
	if kind != 0 {
		t.Errorf("Disable После coolKind=%v Должно быть нулевым значением", kind)
	}
	if reason != "account banned" {
		t.Errorf("Disable После reason=%q должна быть причина отключения", reason)
	}
	if streak != 0 {
		t.Errorf("Disable После softStreak=%d Должно быть 0（сброс домена охлаждения)", streak)
	}
	if mc != 0 {
		t.Errorf("Disable После modelCooldowns=%d Должно быть 0（Исключение модели сбрасывается вместе с доменом охлаждения)", mc)
	}
	if st.Cooling {
		t.Errorf("Disable после не должно отображаться cooling（Отключение — более сильное терминальное состояние, не смешивать): %+v", st)
	}
}

// TestTransitionDisablePreservesBreaker Подозрительные точки 4 вторая половина исправления:Disable очищать только домен охлаждения,
// Не трогать circuit breaker. Срабатывание — "подряд 5xx сбой» (с авторизацией/session ортогонально), при реактивации после отключения
// Наблюдение circuit breaker остаётся активным, не должно перекрываться отключением.
func TestTransitionDisablePreservesBreaker(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.SetBreaker(1, time.Hour, time.Hour)
	p.NoteError("u1") // Триггер circuit breaker (fails=0, retryCount=1, breakerUntil=1h）

	bt, ok := p.breakerUntil("u1")
	if !ok || bt.IsZero() {
		t.Fatal("precondition: breaker should be open")
	}

	p.Disable("u1", "session dead")

	if bt, ok := p.breakerUntil("u1"); !ok || bt.IsZero() {
		t.Fatal("Disable После breakerUntil следует сохранить (circuit breaker и блокировка ортогональны)")
	}
	if fails := p.breakerFails("u1"); fails != 0 {
		t.Errorf("Disable После fails=%d（Уже установлено 0，не должен быть Disable умышленное изменение)", fails)
	}
	// disabled Приоритет: даже если брейкер активен, аккаунт недоступен для выбора.
	if got := p.Pick(); got != nil {
		t.Fatalf("disabled аккаунт недоступен для выбора (включая период размыкания),got %+v", got)
	}
}

// TestTransitionSessionDeadDisableClearsCooling session смерть идет через NoteSessionDead непрерывно
// счётчик, по достижении порога через disableLocked：домен охлаждения сбрасывается полностью, нельзя оставлять "disabled но всё ещё cooling」
// гибридное состояние (ранее handler Ход Disable、на пороговом пути остается кулдаун, один сигнал — разная обработка).
func TestTransitionSessionDeadDisableClearsCooling(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.Cooldown("u1", CoolSoft, time.Hour, "429 rate limit") // в мягком охлаждении
	until, _, _, _, _ := coolingDomain(t, p, "u1")
	if until.IsZero() {
		t.Fatal("precondition: должен быть в мягком кулдауне")
	}

	if p.NoteSessionDead("u1") || p.NoteSessionDead("u1") || !p.NoteSessionDead("u1") {
		t.Fatal("№ 3 Раз NoteSessionDead Отключение при достижении порога")
	}
	st, _ := p.Status("u1")
	if !st.Disabled {
		t.Fatal("должен disabled")
	}
	if st.Cooling || !st.Until.IsZero() || st.SoftStreak != 0 {
		t.Errorf("session dead При отключении очищать домен охлаждения (без гибридного состояния): Cooling=%v Until=%v SoftStreak=%d",
			st.Cooling, st.Until, st.SoftStreak)
	}
	if st.DisabledReason != sessionDeadReason {
		t.Errorf("disabled_reason=%q want %q", st.DisabledReason, sessionDeadReason)
	}
}

// TestTransitionReviveKeepsSoftCoolingKeepsBreaker reviveCoolingLocked（check-in/Баланс
// семантика refresh/unfreeze: размораживает только кулдаун из-за исчерпания баланса (CoolHard），мягкое ограничение, охлаждение и 6004 Учет на уровне модели
// **сохранить**（признак восстановления после rate-limit — истечение wall-clock сброса, а не восстановление баланса)+ обновление credits，Предохранитель не срабатывает.
// Существующие одномерные тесты уже зафиксированы reason/softStreak/modelCooldowns，Данный кейс проверяет всё одним ассертом
// набор полей, фиксация примитивов миграции для домена охлаждения/Обработка домена circuit breaker всегда единообразна.
func TestTransitionReviveKeepsSoftCoolingKeepsBreaker(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	// Домен охлаждения: мягкое охлаждение + 6004 Охлаждение на уровне модели (softStreak накопительно).
	p.Cooldown("u1", CoolSoft, 600*time.Second, "429")
	p.CooldownSoftForModel("u1", time.Minute, time.Now().Add(5*time.Minute), "glm-5.3", "6004")
	// Домен circuit breaker: независимый сигнал, чекин не размораживает.
	p.SetBreaker(1, time.Hour, time.Hour)
	p.NoteError("u1")

	p.ReenableIfCredits("u1", 700, 0)

	st, _ := p.Status("u1")
	if st.Credits != 700 {
		t.Errorf("revive После credits=%d want 700", st.Credits)
	}
	until, kind, reason, streak, mc := coolingDomain(t, p, "u1")
	if until.IsZero() || kind == 0 || reason == "" {
		t.Errorf("revive нельзя снимать кулдаун мягкого лимита:until=%v kind=%v reason=%q", until, kind, reason)
	}
	if mc == 0 {
		t.Error("revive нельзя сбрасывать 6004 учёт на уровне модели (восстановление баланса не является доказательством снятия лимита)")
	}
	_ = streak // данный сценарий streak Конст. 0（Cooldown Вход с фиксированной длительностью не накапливается, с resetAt запись уровня модели также не суммируется)
	if bt, ok := p.breakerUntil("u1"); !ok || bt.IsZero() {
		t.Fatal("revive Нельзя сбрасывать circuit breaker (chat здоровье канала не подтверждено)")
	}
	// домен circuit breaker сохраняется → аккаунт не попадает в normal кандидат (только при полном кулдауне фолбэк всё равно может выбрать, семантика как у фолбэка circuit breaker).
	if p.internalHealthy("u1") {
		t.Fatal("revive В пост-аварийный период не должен healthy（домен circuit breaker не покрыт sign-in)")
	}
}

// TestTransitionReviveUnfreezesHardCooling охлаждение при исчерпании баланса (CoolHard）только тогда
// ReenableIfCredits объект разморозки: восстановление баланса (remain>0）Именно его авторитетное доказательство восстановления.
func TestTransitionReviveUnfreezesHardCooling(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.CooldownUntilTomorrow4AM("u1", "Недостаточно средств")
	p.ReenableIfCredits("u1", 700, 0)
	until, kind, reason, _, _ := coolingDomain(t, p, "u1")
	if !until.IsZero() || kind != 0 || reason != "" {
		t.Errorf("при восстановлении баланса разморозить CoolHard：until=%v kind=%v reason=%q", until, kind, reason)
	}
}
