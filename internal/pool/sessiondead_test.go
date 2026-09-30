package pool

import (
	"path/filepath"
	"testing"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
)

// TestNoteSessionDeadThresholdNotReached Перед 2 раз подряд 12153 Не Disable（защита от ложного срабатывания).
func TestNoteSessionDeadThresholdNotReached(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	if p.NoteSessionDead("u1") {
		t.Fatal("№ 1 Раз 12153 Не следует отключать")
	}
	if p.NoteSessionDead("u1") {
		t.Fatal("№ 2 Раз 12153 Не следует отключать")
	}
	st, ok := p.Status("u1")
	if !ok {
		t.Fatal("no status")
	}
	if st.Disabled {
		t.Fatalf("непрерывно 2 Раз 12153 Не следует отключать: %+v", st)
	}
	// Пока порог не достигнут, аккаунт всё ещё выбираем (keepalive сбой не загрязняет выбор номера).
	if got := p.Pick(); got == nil || got.UID != "u1" {
		t.Fatalf("аккаунт должен оставаться опциональным, got %+v", got)
	}
}

// TestNoteSessionDeadDisablesAtThird Подряд № 3 Раз 12153 → Отключить и сбросить счетчик.
func TestNoteSessionDeadDisablesAtThird(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.NoteSessionDead("u1")
	p.NoteSessionDead("u1")
	if !p.NoteSessionDead("u1") {
		t.Fatal("№ 3 Раз 12153 следует отключить")
	}
	st, _ := p.Status("u1")
	if !st.Disabled {
		t.Fatalf("№ 3 после N раз должен disabled: %+v", st)
	}
	if st.Reason != "12153 session dead" {
		t.Errorf("reason=%q want 12153 session dead", st.Reason)
	}
	// После отключения больше не доступно для выбора.
	if p.Pick() != nil {
		t.Fatal("отключенные аккаунты нельзя выбрать")
	}
}

// TestClearSessionDeadResetsCount Промежуточный успех (refresh успех) сбросить счётчик, далее из 1 Пересчитать заново.
func TestClearSessionDeadResetsCount(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.NoteSessionDead("u1")
	p.NoteSessionDead("u1")
	p.ClearSessionDead("u1") // Эмуляция refresh Успех: сброс счётчика
	if p.NoteSessionDead("u1") {
		t.Fatal("после сброса счётчика № 1 раз не следует отключать")
	}
	if p.NoteSessionDead("u1") {
		t.Fatal("после сброса счётчика № 2 раз не следует отключать")
	}
	if !p.NoteSessionDead("u1") {
		t.Fatal("после сброса счётчика № 3 раз следует отключить (с 1 Пересчитать заново 3 раз)")
	}
}

// TestNoteSuccessClearsSessionDeadCount Любой успех (chat успех) также session сильное доказательство что жив,
// также сбрасывает счётчик — как и refresh Критерий успеха единый.
func TestNoteSuccessClearsSessionDeadCount(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.NoteSessionDead("u1")
	p.NoteSessionDead("u1")
	p.NoteSuccess("u1")
	if p.NoteSessionDead("u1") {
		t.Fatal("после успешного сброса счётчика на 1 раз не следует отключать")
	}
}

// TestReviveDisabled Точка восстановления: очистить disabled + reason + счетчик ложных срабатываний, аккаунт возвращается в пул.
func TestReviveDisabled(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.NoteSessionDead("u1")
	p.NoteSessionDead("u1")
	p.NoteSessionDead("u1") // триггерит блокировку
	if st, _ := p.Status("u1"); !st.Disabled {
		t.Fatal("precondition: должен быть уже отключён")
	}
	p.ReviveDisabled("u1")
	st, ok := p.Status("u1")
	if !ok {
		t.Fatal("no status")
	}
	if st.Disabled {
		t.Fatalf("revive Следует очистить disabled: %+v", st)
	}
	if st.Reason != "" {
		t.Errorf("reason=%q want Пусто (revive Очистить reason）", st.Reason)
	}
	if got := p.Pick(); got == nil || got.UID != "u1" {
		t.Fatalf("После восстановления аккаунт должен вернуться в пул, got %+v", got)
	}
	// счётчик ложных срабатываний также сбрасывается: после восстановления отсчёт заново до максимума 3 раз до отключения.
	p.NoteSessionDead("u1")
	if p.NoteSessionDead("u1") {
		t.Fatal("После восстановления на 2 раз не блокировать (сбросить счетчик)")
	}
	if !p.NoteSessionDead("u1") {
		t.Fatal("После восстановления на 3 раз следует отключить (счетчик с нуля достаточно 3 раз)")
	}
}

// TestReviveDisabledPersists при восстановлении очистить disabled + reason сохранение на диск (после рестарта без отката).
func TestReviveDisabledPersists(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "state.json")
	p := New(fp)
	p.Add(&auth.Auth{UID: "u1"})
	p.Disable("u1", "12153 session dead")
	p.ReviveDisabled("u1")
	p.Flush()

	p2 := New(fp)
	p2.Add(&auth.Auth{UID: "u1"})
	st, ok := p2.Status("u1")
	if !ok || st.Disabled || st.Reason != "" {
		t.Fatalf("revive Должен персиститься (disabled=%v reason=%q）ok=%v", st.Disabled, st.Reason, ok)
	}
}

// TestStatusDisabledReasonDisabled when disabled, Status прокинуть наружу disabled_reason。
func TestStatusDisabledReasonDisabled(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.Disable("u1", "12153 session dead")
	st, _ := p.Status("u1")
	if !st.Disabled || st.DisabledReason != "12153 session dead" {
		t.Errorf("disabled_reason=%q want 12153 session dead (disabled=%v)", st.DisabledReason, st.Disabled)
	}
}

// TestStatusDisabledReasonClearedByRevive После восстановления disabled_reason Сброс в пусто.
func TestStatusDisabledReasonClearedByRevive(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.Disable("u1", "12153 session dead")
	p.ReviveDisabled("u1")
	st, _ := p.Status("u1")
	if st.DisabledReason != "" {
		t.Errorf("revive После disabled_reason=%q want пустой", st.DisabledReason)
	}
	if st.Reason != "" {
		t.Errorf("revive После reason=%q want пустой", st.Reason)
	}
}
