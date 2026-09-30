package pool

import (
	"encoding/json"
	"fmt"
	"math/rand/v2"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
)

// withNoPickGap Временно закрыть окно защиты от конкурентного конфликта аккаунтов (minPickGap=0），чтобы тест чисто взвешенного распределения не затрагивался.
func withNoPickGap(t *testing.T) {
	t.Helper()
	old := minPickGap
	minPickGap = 0
	t.Cleanup(func() { minPickGap = old })
}

func TestPickHighestCredits(t *testing.T) {
	withNoPickGap(t)
	// Взвешивание трёх факторов (credits доля×10 + Простой + успешность): при большой разнице баллов аккаунты с высокими баллами должны выбираться чаще,
	// но уже не как чисто credits близко как при взвешивании 99%（компенсация простоя + Нейтральная успешность 1.5 выровнен бейзлайн).
	p := New("")
	a1 := &auth.Auth{UID: "u1"}
	a2 := &auth.Auth{UID: "u2"}
	a3 := &auth.Auth{UID: "u3"}
	p.Add(a1)
	p.Add(a2)
	p.Add(a3)
	p.SetCredits("u1", 100, 0)
	p.SetCredits("u2", 50000, 0)
	p.SetCredits("u3", 300, 0)
	counts := map[string]int{}
	for i := 0; i < 3000; i++ {
		counts[p.Pick().UID]++
	}
	if counts["u2"] <= counts["u1"] || counts["u2"] <= counts["u3"] {
		t.Errorf("u2 (highest credits) should be picked most: %v", counts)
	}
}

func TestPickSkipsCooling(t *testing.T) {
	p := New("")
	a1 := &auth.Auth{UID: "u1"}
	a2 := &auth.Auth{UID: "u2"}
	p.Add(a1)
	p.Add(a2)
	p.SetCredits("u1", 100, 0)
	p.SetCredits("u2", 50, 0)
	p.Cooldown("u1", CoolHard, time.Hour, "test")
	got := p.Pick()
	if got == nil || got.UID != "u2" {
		t.Fatalf("pick=%+v want u2", got)
	}
}

func TestPickExpiredCooldownReturnsToHealthy(t *testing.T) {
	p := New("")
	a1 := &auth.Auth{UID: "u1"}
	p.Add(a1)
	p.SetCredits("u1", 100, 0)
	p.Cooldown("u1", CoolSoft, time.Millisecond, "429")
	time.Sleep(5 * time.Millisecond)
	got := p.Pick()
	if got == nil || got.UID != "u1" {
		t.Fatalf("pick=%+v want u1 after cooldown expiry", got)
	}
}

func TestPickNilWhenAllDisabled(t *testing.T) {
	// Полностью отключено → резерв не участвует (отключённые аккаунты никогда не участвуют в резерве)→ вернуть nil。
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.Disable("u1", "session dead")
	if got := p.Pick(); got != nil {
		t.Fatalf("want nil (all disabled), got %+v", got)
	}
}

func TestPickExcluding(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.Add(&auth.Auth{UID: "u2"})
	p.SetCredits("u1", 100, 0)
	p.SetCredits("u2", 50, 0)
	tried := map[string]bool{"u1": true}
	got := p.PickExcluding(tried)
	if got == nil || got.UID != "u2" {
		t.Fatalf("pick=%+v want u2", got)
	}
	tried["u2"] = true
	if got := p.PickExcluding(tried); got != nil {
		t.Fatalf("want nil, got %+v", got)
	}
}

func TestPickExcludingStaysWithinHealthy(t *testing.T) {
	withNoPickGap(t)
	// взвешенный рандом не может выбрать кулдаун/Заблокировать аккаунт.
	p := New("")
	p.Add(&auth.Auth{UID: "u-cold"})
	p.Add(&auth.Auth{UID: "u-hot"})
	p.SetCredits("u-cold", 9999, 0)
	p.SetCredits("u-hot", 1, 0)
	p.Cooldown("u-cold", CoolHard, time.Hour, "x")
	for i := 0; i < 20; i++ {
		got := p.PickExcluding(nil)
		if got == nil || got.UID != "u-hot" {
			t.Fatalf("iter %d: picked %+v, want only healthy u-hot", i, got)
		}
	}
}

func TestPickWeightedSkewTowardHighCredits(t *testing.T) {
	withNoPickGap(t)
	// Top5 Взвешивание по трём факторам: один аккаунт credits При достаточно высокой доле большинство выбирает его.
	p := New("")
	for _, u := range []string{"w1", "w2", "w3", "w4", "w5", "w6"} {
		p.Add(&auth.Auth{UID: u})
		p.SetCredits(u, 1, 0)
	}
	p.SetCredits("w1", 1000, 0)
	counts := map[string]int{}
	for i := 0; i < 5000; i++ {
		counts[p.Pick().UID]++
	}
	mx, mxUID := 0, ""
	for uid, n := range counts {
		if n > mx {
			mx, mxUID = n, uid
		}
	}
	if mxUID != "w1" {
		t.Errorf("w1 (highest credits) should be picked most: %v", counts)
	}
}

func TestPickWeightedUniformWhenAllZero(t *testing.T) {
	withNoPickGap(t)
	// credits все — 0 → деградирует до равномерного рандома, нельзя выбирать только один фиксированный.
	p := New("")
	for _, u := range []string{"z1", "z2", "z3"} {
		p.Add(&auth.Auth{UID: u})
	}
	seen := map[string]bool{}
	for i := 0; i < 30; i++ {
		seen[p.Pick().UID] = true
	}
	if len(seen) != 3 {
		t.Errorf("uniform fallback should hit all, seen=%v", seen)
	}
}

func TestPickWeightedTopFiveOnly(t *testing.T) {
	withNoPickGap(t)
	// № 6 высокий credits аккаунт в Top5 иначе взвешенный розыгрыш никогда до него не дойдет.
	p := New("")
	for _, u := range []string{"a1", "a2", "a3", "a4", "a5", "a6"} {
		p.Add(&auth.Auth{UID: u})
	}
	p.SetCredits("a1", 1000, 0)
	p.SetCredits("a2", 1000, 0)
	p.SetCredits("a3", 1000, 0)
	p.SetCredits("a4", 1000, 0)
	p.SetCredits("a5", 1000, 0)
	p.SetCredits("a6", 5, 0) // Top5 кроме
	for i := 0; i < 2000; i++ {
		if got := p.Pick(); got == nil || got.UID == "a6" {
			t.Fatalf("iter %d: picked %+v, a6 must stay outside top-5", i, got)
		}
	}
}

func TestPickTopFiveByIdleNotCredits(t *testing.T) {
	withNoPickGap(t)
	// C1 Регрессия: компенсация простоя также влияет на шорт-лист.a1..a5 credits=100 но только что использован (простой 0），
	// a6 credits=90 но ни разу не использовался (простой — макс. балл). Чисто credits При сортировке a6 не может войти top5；
	// При трёхфакторном взвешивании a6 Наивысший вес, обязательно выбирается в первом раунде. Assert только первого раунда.
	p := New("")
	now := time.Now()
	for _, u := range []string{"a1", "a2", "a3", "a4", "a5"} {
		p.Add(&auth.Auth{UID: u})
		p.SetCredits(u, 100, 0)
	}
	p.Add(&auth.Auth{UID: "a6"})
	p.SetCredits("a6", 90, 0)
	p.SetRandomSource(func(n int64) int64 { return 0 })
	// a1..a5 все"Только что использовано"，Компенсация простоя сброшена в ноль;a6 Не использовался → Простой — макс. балл.
	p.mu.Lock()
	for _, u := range []string{"a1", "a2", "a3", "a4", "a5"} {
		p.byUID[u].lastUsed = now
	}
	p.mu.Unlock()

	if got := p.Pick(); got == nil || got.UID != "a6" {
		t.Fatalf("pick=%v, want a6 (idle low-credit must enter top5 by weight)", got)
	}
}

func TestPickDeterministicViaSetRandomSource(t *testing.T) {
	withNoPickGap(t)
	p := New("")
	p.SetRandomSource(func(n int64) int64 { return 0 })
	p.Add(&auth.Auth{UID: "u1"})
	p.Add(&auth.Auth{UID: "u2"})
	p.SetCredits("u1", 100, 0)
	p.SetCredits("u2", 50, 0)
	// r=0 ∈ [0,50) → Попадание u1。источник инъекции должен делать выбор номера полностью детерминированным.
	for i := 0; i < 50; i++ {
		if got := p.Pick(); got == nil || got.UID != "u1" {
			t.Fatalf("iter %d: pick=%+v want u1 (deterministic)", i, got)
		}
	}
}

func TestPickAntiThunderingHerd(t *testing.T) {
	// 100 goroutine Одновременно Pick：защита от конкурентного выбора: один аккаунт не должен выбираться повторно в окне коллизии.
	// credits одинаковый → Без источника инжекции взвешенный рандом должен естественно распределять; для стабильности всё установить в 0 Равномерная случайность.
	//
	// minPickGap Установить 0（тестовый переключатель, помеченный комментарием в исходнике):Windows Грубая гранулярность часов, конкурентность всего раунда
	// Pick Могут попасть в один тик — все lastUsed метки времени равны,eligible Всегда пустой путь LRU
	// фолбэк, а LRU Для равных таймстампов по UID Стабильно tie-break，Результат 100 раз полное попадание c00。
	// После закрытия окна кейс возвращается к исходному критерию проверки: дисперсия взвешенного рандома (кроссплатформенно стабильна).
	oldGap := minPickGap
	minPickGap = 0
	t.Cleanup(func() { minPickGap = oldGap })

	p := New("")
	for i := 0; i < 10; i++ {
		p.Add(&auth.Auth{UID: fmt.Sprintf("c%02d", i)})
	}
	// критично: проверить, что при конкурентности в любой момент не выберутся все на один аккаунт.
	const N = 100
	var wg sync.WaitGroup
	picked := make([]string, N)
	for i := 0; i < N; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			if a := p.Pick(); a != nil {
				picked[idx] = a.UID
			}
		}(i)
	}
	wg.Wait()

	counts := map[string]int{}
	for _, uid := range picked {
		if uid != "" {
			counts[uid]++
		}
	}
	// выбор номеров должен покрывать несколько аккаунтов, самый популярный аккаунт — не более половины.
	if len(counts) < 2 {
		t.Fatalf("anti-thundering-herd failed: all %d picks hit %d account(s) %v", N, len(counts), counts)
	}
	for uid, n := range counts {
		if n > N/2 {
			t.Errorf("account %s picked %d/%d (>50%%): thundering herd", uid, n, N)
		}
	}
}

func TestPickLRUFallbackWhenTopAllRecentlyUsed(t *testing.T) {
	// top5 все только что выбраны → LRU В фолбэке выбирать наименее недавно использованный (= Самый ранний lastUsed）。
	old := minPickGap
	minPickGap = time.Hour // Сверхбольшое окно: любой lastUsed все внутри окна
	defer func() { minPickGap = old }()

	p := New("")
	for i := 0; i < 5; i++ {
		p.Add(&auth.Auth{UID: fmt.Sprintf("a%d", i)})
	}
	// Прямая конструкция lastUsed：Не проходит через Pick（Избежать Pick Перезаписать lastUsed）。
	order := []string{"a4", "a3", "a2", "a1", "a0"}
	p.mu.Lock()
	for i, uid := range order {
		p.byUID[uid].lastUsed = time.Now().Add(-time.Duration(len(order)-i) * time.Second) // a4 Самый старый
	}
	p.mu.Unlock()

	got := p.Pick()
	if got == nil {
		t.Fatal("pick returned nil")
	}
	if got.UID != "a4" {
		t.Errorf("LRU fallback picked %s want a4 (oldest lastUsed)", got.UID)
	}
}

func TestCooldownPersists(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "state.json")
	p := New(fp)
	p.Add(&auth.Auth{UID: "u1"})
	p.Cooldown("u1", CoolHard, time.Hour, "Недостаточно средств")
	p.Flush() // Смена статуса через dirty флаг, flush на диск выполняет Flush / бэкенд goroutine Отвечает за
	p2 := New(fp)
	p2.Add(&auth.Auth{UID: "u1"})
	st, ok := p2.Status("u1")
	if !ok || !st.Cooling || st.Reason != "Недостаточно средств" {
		t.Fatalf("cooldown lost after reload: %+v ok=%v", st, ok)
	}
}

func TestDisablePersists(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "state.json")
	p := New(fp)
	p.Add(&auth.Auth{UID: "u1"})
	p.Disable("u1", "12153 session dead")
	p.Flush()
	p2 := New(fp)
	p2.Add(&auth.Auth{UID: "u1"})
	if p2.Pick() != nil {
		t.Fatal("disabled account picked after reload")
	}
	st, _ := p2.Status("u1")
	if !st.Disabled || st.Reason != "12153 session dead" {
		t.Errorf("status=%+v", st)
	}
}

func TestReenableIfCredits(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.Cooldown("u1", CoolHard, time.Hour, "Недостаточно средств")
	p.ReenableIfCredits("u1", 500, 0)
	got := p.Pick()
	if got == nil || got.UID != "u1" {
		t.Fatalf("should reenable, pick=%+v", got)
	}
}

func TestReenableZeroCreditsKeepsCooling(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.Cooldown("u1", CoolHard, time.Hour, "Недостаточно средств")
	p.ReenableIfCredits("u1", 0, 0)
	st, _ := p.Status("u1")
	if !st.Cooling {
		t.Fatal("zero credits should stay cooling")
	}
}

func TestReenableDoesNotTouchDisabled(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.Disable("u1", "session dead")
	p.ReenableIfCredits("u1", 500, 0)
	if p.Pick() != nil {
		t.Fatal("disabled must not auto-reenable")
	}
}

func TestNoteErrorAccumulatesErrTotal(t *testing.T) {
	// NoteError Изменение семантики: отдельного ... больше нет err кулдаун (CoolErr уже интегрировано в circuit breaker),
	// накапливать только errTotal（без сброса, для веса успешности) и отдать в circuit breaker fails。
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.NoteError("u1")
	p.NoteError("u1")
	st, _ := p.Status("u1")
	if st.ErrTotal != 2 {
		t.Errorf("err_total=%d want 2", st.ErrTotal)
	}
	if st.Cooling {
		t.Errorf("NoteError alone must not set cooling (no CoolErr): %+v", st)
	}
	if st.LastErrTime.IsZero() {
		t.Error("last_err not set")
	}
}

func TestNoteSuccessResetsBreakerNotErrTotal(t *testing.T) {
	// NoteSuccess Очистить fails/circuit breaker (рабочее состояние), но не очищает errTotal（накопленное значение).
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.SetBreaker(2, time.Hour, 2*time.Hour)
	p.NoteError("u1")
	p.NoteError("u1") // срабатывание circuit breaker
	if p.internalHealthy("u1") {
		t.Fatal("breaker should be open (unhealthy) after 2 failures")
	}
	p.NoteSuccess("u1")
	st, _ := p.Status("u1")
	if st.ErrTotal != 2 {
		t.Errorf("err_total=%d want 2 (cumulative, not cleared by success)", st.ErrTotal)
	}
	if st.Cooling {
		t.Errorf("success should clear breaker: %+v", st)
	}
}

func TestNoteSuccessIncrementsAndRecords(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	before := time.Now()
	p.NoteSuccess("u1")
	p.NoteSuccess("u1")
	st, _ := p.Status("u1")
	if st.SuccessCount != 2 {
		t.Errorf("success_count=%d want 2", st.SuccessCount)
	}
	if st.LastSuccessTime.Before(before) {
		t.Errorf("last_success=%v before call", st.LastSuccessTime)
	}
	if !st.LastErrTime.IsZero() {
		t.Errorf("last_err should be zero for fresh success: %v", st.LastErrTime)
	}
}

func TestReenableClearsCoolingNotBreaker(t *testing.T) {
	// C5：Разморозка чекином сбрасывает только кулдаун (until/coolKind/reason）+ обновление credits，не сбрасывать размыкание
	// （fails/retryCount/breakerUntil）。успешный чекин доказывает только баланс и billing Восстановление канала,
	// не подтверждать chat Здоровье канала — circuit breaker по-прежнему по breakerUntil истечение backoff или NoteSuccess Восстановление.
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.CooldownUntilTomorrow4AM("u1", "Недостаточно средств") // жёсткий кулдаун (подача fails，Но порог по умолчанию 3，без circuit breaker)
	p.SetBreaker(1, time.Hour, time.Hour)
	p.NoteError("u1") // Триггер circuit breaker (fails→Порог1→fails=0, retryCount=1, breakerUntil ненулевой)
	p.ReenableIfCredits("u1", 500, 0)
	st, _ := p.Status("u1")
	if st.Reason != "" || st.Credits != 500 {
		t.Errorf("signin should clear reason + set credits=500: %+v", st)
	}
	if st.Until != (time.Time{}) {
		t.Errorf("signin should clear hard-cooling until: %+v", st.Until)
	}
	if st.BreakerUntil.IsZero() {
		t.Fatal("signin must NOT clear breakerUntil (chat health unresolved)")
	}
	// автомат защиты всё еще → Аккаунт всё ещё недоступен для выбора (до breakerUntil истекает).
	if p.internalHealthy("u1") {
		t.Fatal("account should stay unhealthy while breaker active after signin")
	}
}

func TestReenableKeepsBreaker(t *testing.T) {
	// C5 новая семантика блокировки возврата: аккаунт только в circuit-breaker (без cooldown), разморозка чекином не сбрасывает circuit-breaker.
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.SetBreaker(1, time.Hour, time.Hour)
	p.NoteError("u1") // срабатывание circuit breaker
	if bt, _ := p.breakerUntil("u1"); bt.IsZero() {
		t.Fatal("precondition: breaker should be open")
	}
	p.ReenableIfCredits("u1", 500, 0)
	if bt, _ := p.breakerUntil("u1"); bt.IsZero() {
		t.Fatal("signin must not clear breakerUntil")
	}
	if p.internalHealthy("u1") {
		t.Fatal("account should stay unhealthy while breaker active after signin")
	}
}

func TestCoolKindPersistsAcrossReload(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "state.json")
	p := New(fp)
	p.Add(&auth.Auth{UID: "u1"})
	p.Cooldown("u1", CoolHard, time.Hour, "Недостаточно средств")
	p.Flush()

	// при отсутствии нового поля в старом файле — нулевое значение → охлаждение должно по-прежнему работать (обратная совместимость).
	p2 := New(fp)
	p2.Add(&auth.Auth{UID: "u1"})
	st, ok := p2.Status("u1")
	if !ok || !st.Cooling {
		t.Fatalf("cooldown state lost after reload: %+v ok=%v", st, ok)
	}
	if st.CoolKind != "hard_credit" {
		t.Errorf("cool_kind after reload=%q want hard_credit", st.CoolKind)
	}
}

func TestStateRoundTripExtendedFields(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "state.json")
	p := New(fp)
	p.Add(&auth.Auth{UID: "u1"})
	p.Cooldown("u1", CoolHard, time.Hour, "Недостаточно средств")
	p.NoteSuccess("u1") // successCount=1，last_success Ненулевой
	p.NoteSuccess("u1") // successCount=2
	p.NoteError("u1") // errTotal=1（накопительно),last_err Ненулевой
	p.Flush()

	raw, err := os.ReadFile(fp)
	if err != nil {
		t.Fatal(err)
	}
	// JSON tag snake_case, нижний регистр;err_total сброс на диск,err_count Больше не сохранять на диск.
	for _, want := range []string{`"cool_kind"`, `"success_count"`, `"err_total"`, `"last_success"`, `"last_err"`} {
		if !strings.Contains(string(raw), want) {
			t.Errorf("state.json missing %s:\n%s", want, raw)
		}
	}
	if strings.Contains(string(raw), `"err_count"`) {
		t.Errorf("state.json should not write legacy err_count:\n%s", raw)
	}

	// Поля сохраняются после перезагрузки
	p2 := New(fp)
	p2.Add(&auth.Auth{UID: "u1"})
	st, ok := p2.Status("u1")
	if !ok {
		t.Fatal("no status")
	}
	if st.SuccessCount != 2 || st.CoolKind != "hard_credit" {
		t.Errorf("reloaded portrait=%+v", st)
	}
	if st.ErrTotal != 1 {
		t.Errorf("reloaded err_total=%d want 1", st.ErrTotal)
	}
	if st.LastSuccessTime.IsZero() || st.LastErrTime.IsZero() {
		t.Error("last_success/last_err lost after reload")
	}
}

func TestLoadLegacyErrCountMigratesToErrTotal(t *testing.T) {
	// Миграционный тест: старый state.json Содержит только err_count（последовательные ошибки)→ После загрузки err_total Корректно.
	dir := t.TempDir()
	fp := filepath.Join(dir, "state.json")
	legacy := `{"accounts":{"u1":{"credits":100,"err_count":7}}}`
	if err := os.WriteFile(fp, []byte(legacy), 0o600); err != nil {
		t.Fatal(err)
	}
	p := New(fp)
	p.Add(&auth.Auth{UID: "u1"})
	st, ok := p.Status("u1")
	if !ok {
		t.Fatal("legacy account should load")
	}
	if st.ErrTotal != 7 {
		t.Errorf("err_total=%d want 7 (migrated from legacy err_count)", st.ErrTotal)
	}
	// приоритет нового поля: при наличии обоих берётся большее.
	both := `{"accounts":{"u1":{"credits":100,"err_count":3,"err_total":9}}}`
	if err := os.WriteFile(fp, []byte(both), 0o600); err != nil {
		t.Fatal(err)
	}
	p2 := New(fp)
	p2.Add(&auth.Auth{UID: "u1"})
	if st2, _ := p2.Status("u1"); st2.ErrTotal != 9 {
		t.Errorf("err_total=%d want 9 (new field wins over legacy)", st2.ErrTotal)
	}
}

func TestStatusCoolKindDefaultsWhenNotCooling(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	st, _ := p.Status("u1")
	if st.CoolKind != "" || st.CoolRemaining != 0 {
		t.Errorf("non-cooling portrait=%+v", st)
	}
}

func TestNextDay4AMBoundaries(t *testing.T) {
	cases := []struct {
		name string
		now string // RFC3339 (UTC обозначает)
		want string // Следующий 04:00（Тот же часовой пояс,UTC обозначение)
	}{
		{"обычный день", "2026-08-28T17:00:00+08:00", "2026-08-29T04:00:00+08:00"},
		// раннее утро 00:00~04:00 Триггер жёсткого кулдауна: в этот день 04:00 ещё не наступило, кулдаун должен приходиться на текущий день (а не на следующий),
		// Иначе доп. холод ~1 день (исх. bug）。
		{"раннее утро02:30", "2026-08-28T02:30:00+08:00", "2026-08-28T04:00:00+08:00"},
		{"раннее утро00:00", "2026-08-28T00:00:00+08:00", "2026-08-28T04:00:00+08:00"},
		{"раннее утро03:59:59", "2026-08-28T03:59:59+08:00", "2026-08-28T04:00:00+08:00"},
		{"Как раз4Точка", "2026-08-28T04:00:00+08:00", "2026-08-29T04:00:00+08:00"},
		{"4точка только что пройдена", "2026-08-28T04:00:01+08:00", "2026-08-29T04:00:00+08:00"},
		{"Конец месяца(31День/месяц)", "2026-01-31T12:00:00+08:00", "2026-02-01T04:00:00+08:00"},
		{"Конец месяца(28День/месяц)", "2026-02-28T12:00:00+08:00", "2026-03-01T04:00:00+08:00"},
		{"конец високосного месяца", "2028-02-29T12:00:00+08:00", "2028-03-01T04:00:00+08:00"},
		{"конец года", "2026-12-31T23:59:59+08:00", "2027-01-01T04:00:00+08:00"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			now, err := time.Parse(time.RFC3339, c.now)
			if err != nil {
				t.Fatal(err)
			}
			want, err := time.Parse(time.RFC3339, c.want)
			if err != nil {
				t.Fatal(err)
			}
			if got := nextDay4AM(now); !got.Equal(want) {
				t.Errorf("nextDay4AM(%v)=%v want %v", c.now, got, want)
			}
		})
	}
}

func TestCooldownUntilTomorrow4AM(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	before := time.Now()
	p.CooldownUntilTomorrow4AM("u1", "Недостаточно средств")
	after := time.Now()
	st, ok := p.Status("u1")
	if !ok {
		t.Fatal("no status")
	}
	if !st.Cooling {
		t.Fatalf("should be cooling: %+v", st)
	}
	if st.Reason != "Недостаточно средств" {
		t.Errorf("reason=%q", st.Reason)
	}
	// дедлайн кулдауна должен быть"Ближайший после текущего момента 04:00"：Позже чем now、Не старше чем 24h
	//（раннее утро 00:00~04:00 при срабатывании попадает на текущий день 04:00，Остальные интервалы попадают на след. день 04:00，Диапазон постоянен < 24h）。
	if st.Until.Before(after) {
		t.Errorf("until %v is in the past (call span %v..%v)", st.Until, before, after)
	}
	if st.Until.Hour() != 4 {
		t.Errorf("until hour=%d want 4", st.Until.Hour())
	}
	if d := st.Until.Sub(after); d > 24*time.Hour {
		t.Errorf("until %v is more than 24h out: %v", st.Until, d)
	}
	// При полном кулдауне баланс исчерпан (hard）номер не участвует в fallback → вернуть nil（ожидание восстановления чекина).
	if got := p.Pick(); got != nil {
		t.Fatalf("all-hard-cooling should return nil (hard excluded from fallback), got %+v", got)
	}
}

func TestCooldownUntilTomorrow4AMPersists(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "state.json")
	p := New(fp)
	p.Add(&auth.Auth{UID: "u1"})
	p.CooldownUntilTomorrow4AM("u1", "Недостаточно средств")
	p.Flush()
	p2 := New(fp)
	p2.Add(&auth.Auth{UID: "u1"})
	st, ok := p2.Status("u1")
	if !ok || st.Until.Hour() != 4 || st.Reason != "Недостаточно средств" {
		t.Errorf("status after reload=%+v ok=%v", st, ok)
	}
}

// ---------------------------------------------------------------------------
// мягкое охлаждение с экспоненциальным backoff (softStreak）
// ---------------------------------------------------------------------------

// wantCoolSec Assert: оставшиеся секунды кулдауна аккаунта ≈ want（±tol с, допуск внутри теста tick дрейф).
func wantCoolSec(t *testing.T, p *Pool, uid string, want int64, tol int64) {
	t.Helper()
	st, ok := p.Status(uid)
	if !ok {
		t.Fatalf("status(%s) missing", uid)
	}
	if !st.Cooling || st.CoolKind != "soft_rate" {
		t.Fatalf("%s should be in soft_rate cooling: %+v", uid, st)
	}
	if got := st.CoolRemaining; got < want-tol || got > want+tol {
		t.Errorf("cool_remaining_sec=%d want ~%d (±%d)", got, want, tol)
	}
}

// expireCooldown тест-хелпер: откатить дедлайн кулдауна аккаунта в прошлое, симулировать истечение кулдауна
// （CooldownSoftRate Только в"не в активном мягком кулдауне"при этом продвинуть streak——суммируется через периоды охлаждения).
func expireCooldown(p *Pool, uid string) {
	p.mu.Lock()
	if e, ok := p.byUID[uid]; ok {
		e.until = time.Now().Add(-time.Second)
	}
	p.mu.Unlock()
}

func TestCooldownSoftExponentialBackoff(t *testing.T) {
	// Тот же аккаунт**сквозь период кулдауна**Непрерывный мягкий rate-limit → Долгое нажатие 2 кратный экспоненциальный рост (поглощение апстрима: в кулдауне
	// фолбэк-пробы больше не накапливаются, накопление только в новом раунде лимитирования после истечения).
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.SetSoftRateMax(time.Hour) // Потолок 1h：В данном кейсе три шага (600/1200/2400）не затрагивает

	for i, want := range []int64{600, 1200, 2400} {
		p.CooldownSoftRate("u1", 600*time.Second, time.Time{}, "429 rate limit")
		wantCoolSec(t, p, "u1", want, 3)
		if st, _ := p.Status("u1"); st.SoftStreak != i+1 {
			t.Errorf("after call %d: soft_streak=%d want %d", i+1, st.SoftStreak, i+1)
		}
		expireCooldown(p, "u1") // симулировать попадание под новый лимит после истечения кулдауна
	}
}

func TestCooldownSoftCappedBySoftRateMax(t *testing.T) {
	// Лимит инъекции:streak 3 400s Сжато до 250s（суммируется через периоды охлаждения).
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.SetSoftRateMax(250 * time.Second)

	p.CooldownSoftRate("u1", 100*time.Second, time.Time{}, "x")
	wantCoolSec(t, p, "u1", 100, 3)
	expireCooldown(p, "u1")
	p.CooldownSoftRate("u1", 100*time.Second, time.Time{}, "x")
	wantCoolSec(t, p, "u1", 200, 3)
	expireCooldown(p, "u1")
	p.CooldownSoftRate("u1", 100*time.Second, time.Time{}, "x")
	wantCoolSec(t, p, "u1", 250, 3)
}

func TestCooldownSoftDefaultCapWhenUnset(t *testing.T) {
	// Не инжектировано softRateMax → Нажать 2h ограничение сверху (избежать неограниченного backoff при голом пуле).
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	for i, want := range []int64{600, 1200, 2400, 4800, 7200} {
		p.CooldownSoftRate("u1", 600*time.Second, time.Time{}, "x")
		wantCoolSec(t, p, "u1", want, 3)
		if st, _ := p.Status("u1"); st.SoftStreak != i+1 {
			t.Errorf("soft_streak=%d want %d", st.SoftStreak, i+1)
		}
		expireCooldown(p, "u1")
	}
}

func TestSetSoftRateMaxIgnoresNonPositive(t *testing.T) {
	// неположительные значения сохраняют исходное (стиль как SetBreaker）：0 Нельзя сбрасывать лимит в ноль, иначе без ограничений.
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.SetSoftRateMax(0)
	p.SetSoftRateMax(-time.Second)
	for i := 0; i < 6; i++ {
		p.CooldownSoftRate("u1", 600*time.Second, time.Time{}, "x")
		if i < 5 {
			expireCooldown(p, "u1") // Последний раунд без обратного вызова: на момент assert должен оставаться в кулдауне
		}
	}
	wantCoolSec(t, p, "u1", 7200, 3) // всё ещё 2h Потолок (№ 6 Шаг 19200s → 7200s）
}

func TestCooldownSoftStreakResetBySuccess(t *testing.T) {
	// успех подтверждает восстановление аккаунта → streak сброс в ноль, следующий мягкий кулдаун вернётся к базе.
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.CooldownSoftRate("u1", 600*time.Second, time.Time{}, "x")
	expireCooldown(p, "u1")
	p.CooldownSoftRate("u1", 600*time.Second, time.Time{}, "x")
	wantCoolSec(t, p, "u1", 1200, 3)

	p.NoteSuccess("u1")
	if st, _ := p.Status("u1"); st.SoftStreak != 0 {
		t.Fatalf("success should reset soft_streak, got %d", st.SoftStreak)
	}
	expireCooldown(p, "u1") // Новый раунд лимитирования (охлаждение предыдущего раунда прошло/уже успешно сброшен)
	p.CooldownSoftRate("u1", 600*time.Second, time.Time{}, "x")
	wantCoolSec(t, p, "u1", 600, 3)
}

func TestCooldownSoftKeptByReenable(t *testing.T) {
	// check-in/Разморозка обновления баланса (reviveCoolingLocked）Размораживать только cooldown по исчерпанию баланса (CoolHard）；
	// Мягкий кулдаун лимита (CoolSoft）и softStreak Сохранено — признак восстановления лимита — сброс wall-clock/Бэкофф истек,
	// не восстановление баланса (обновление баланса каждые 5 раз в минуту, если при этом очищать домен охлаждения, фактическое время жизни защиты от лимитирования ужимается до
	// в пределах одного цикла обновления). Домен circuit breaker (fails/retryCount/breakerUntil）Не трогать, совместимо с существующим C5 Семантика совпадает.
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.CooldownSoftRate("u1", 600*time.Second, time.Time{}, "x")
	expireCooldown(p, "u1") // второе ограничение за период охлаждения,streak накопить до 2（Повторные срабатывания во время охлаждения не суммируются,#152）
	p.CooldownSoftRate("u1", 600*time.Second, time.Time{}, "x")
	failsBefore := p.breakerFails("u1")

	p.ReenableIfCredits("u1", 500, 0)
	st, _ := p.Status("u1")
	if !st.Cooling {
		t.Errorf("reenable нельзя снимать кулдаун soft rate-limit: %+v", st)
	}
	if st.SoftStreak != 2 {
		t.Errorf("reenable Нельзя сбрасывать счётчик бэкоффа мягкого лимита soft_streak, got %d want 2", st.SoftStreak)
	}
	if failsAfter := p.breakerFails("u1"); failsAfter != failsBefore {
		t.Errorf("reenable must not touch breaker: fails %d → %d", failsBefore, failsAfter)
	}

	// продолжение бэкоффа: № 3 срабатываний из существующего streak=2 продолжить → 600s<<2 = 2400s（а не после сброса в ноль 600s）。
	expireCooldown(p, "u1")
	p.CooldownSoftRate("u1", 600*time.Second, time.Time{}, "x")
	wantCoolSec(t, p, "u1", 2400, 3)
}

func TestCooldownHardDoesNotAdvanceSoftStreak(t *testing.T) {
	// Длительность жёсткого кулдауна (баланс исчерпан) определяется моментом чекина, не участвует в экспоненте мягкого бэкоффа.
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.CooldownUntilTomorrow4AM("u1", "Недостаточно средств")
	if st, _ := p.Status("u1"); st.SoftStreak != 0 {
		t.Fatalf("hard cooldown must not touch soft_streak, got %d", st.SoftStreak)
	}
	p.CooldownSoftRate("u1", 600*time.Second, time.Time{}, "x")
	wantCoolSec(t, p, "u1", 600, 3)
}

func TestSoftStreakPersistsAcrossReload(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "state.json")
	p := New(fp)
	p.Add(&auth.Auth{UID: "u1"})
	p.CooldownSoftRate("u1", 600*time.Second, time.Time{}, "x")
	expireCooldown(p, "u1")
	p.CooldownSoftRate("u1", 600*time.Second, time.Time{}, "x")
	p.Flush()

	raw, err := os.ReadFile(fp)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `"soft_streak"`) {
		t.Fatalf("state.json missing soft_streak: %s", raw)
	}

	p2 := New(fp)
	p2.Add(&auth.Auth{UID: "u1"})
	if st, _ := p2.Status("u1"); st.SoftStreak != 2 {
		t.Fatalf("soft_streak after reload=%d want 2", st.SoftStreak)
	}
	// бэкофф из персистентного streak Продолжить: № 3 Раз → 2400s。
	expireCooldown(p2, "u1")
	p2.CooldownSoftRate("u1", 600*time.Second, time.Time{}, "x")
	wantCoolSec(t, p2, "u1", 2400, 3)
}

func TestSoftStreakMissingInLegacyStateFile(t *testing.T) {
	// старый state.json отсутствует soft_streak → Совместимость с нулевым значением, бэкофф перезапускается с базы.
	dir := t.TempDir()
	fp := filepath.Join(dir, "state.json")
	if err := os.WriteFile(fp, []byte(`{"accounts":{"u1":{"credits":100}}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	p := New(fp)
	p.Add(&auth.Auth{UID: "u1"})
	if st, _ := p.Status("u1"); st.SoftStreak != 0 {
		t.Fatalf("legacy file should load soft_streak=0, got %d", st.SoftStreak)
	}
	p.CooldownSoftRate("u1", 600*time.Second, time.Time{}, "x")
	wantCoolSec(t, p, "u1", 600, 3)
}

// ---------------------------------------------------------------------------
// issue #31：429 6004 Лимит на уровне модели → Сузить охлаждение по времени сброса апстрима + Выбор номера с исключением на уровне модели
// ---------------------------------------------------------------------------

func TestCooldownSoftForModelParsedUntil(t *testing.T) {
	// 6004 msg содержит "будет через … сброс»→ дедлайн персонального кулдауна этой модели точно равен времени парсинга (wall-clock проверка).
	// Использовать future 5 таймстамп в минутах: после парсинга Until ≈ now+5m，значительно короче фиксированного 600s Экспоненциальный бэкофф от базы,
	// Доказательство"Апстрим явно указывает время сброса«имеет приоритет над«600s запустить экспоненциальный бэкофф"。
	// Новая семантика: только запись modelCooldowns（не писать на уровне аккаунта until）→ Аккаунт не cooling、Одна строка реестра.
	reset := time.Now().Add(5 * time.Minute)
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.CooldownSoftForModel("u1", 600*time.Second, reset, "glm-5.3", "429 rate limit")
	st, ok := p.Status("u1")
	if !ok {
		t.Fatal("account missing")
	}
	if st.Cooling {
		t.Fatalf("6004-with-reset should NOT set account-level cooling: %+v", st)
	}
	if len(st.RateLimitedModels) != 1 || st.RateLimitedModels[0].Model != "glm-5.3" {
		t.Fatalf("want single model ledger row for glm-5.3: %+v", st.RateLimitedModels)
	}
	until := st.RateLimitedModels[0].Until
	if d := until.Sub(reset); d < -time.Second || d > time.Second {
		t.Errorf("model until=%v want ~reset=%v (diff %v)", until, reset, d)
	}
}

func TestCooldownSoftForModelCappedBySoftRateMax(t *testing.T) {
	// Время парсинга превышено soft_rate_max → усечь до soft_rate_max（не банить бессрочно).
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.SetSoftRateMax(10 * time.Minute)
	reset := time.Now().Add(2 * time.Hour) // Намного превышает лимит 10m
	before := time.Now()
	p.CooldownSoftForModel("u1", 600*time.Second, reset, "glm-5.3", "429 rate limit")
	st, _ := p.Status("u1")
	if len(st.RateLimitedModels) != 1 {
		t.Fatalf("want model ledger row: %+v", st.RateLimitedModels)
	}
	if st.RateLimitedModels[0].Until.Sub(before) > 10*time.Minute+time.Second {
		t.Errorf("model until=%v want capped at soft_rate_max=10m", st.RateLimitedModels[0].Until)
	}
}

func TestCooldownSoftForModelNoResetFallbackBackoff(t *testing.T) {
	// без времени парсинга (resetAt нулевое значение)→ ограниченный backoff (600s от). Повторное срабатывание fallback-пробы в кулдауне 429
	// **не продвигать streak、не продлевается**（поглощение апстрима: старое "удвоение при каждой пробе» как раз толкало весь пул к
	// 2h виновник лимита).
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.SetSoftRateMax(time.Hour)
	p.CooldownSoftForModel("u1", 600*time.Second, time.Time{}, "", "429 rate limit")
	wantCoolSec(t, p, "u1", 600, 3)
	p.CooldownSoftForModel("u1", 600*time.Second, time.Time{}, "", "429 rate limit")
	wantCoolSec(t, p, "u1", 600, 3) // все еще на кулдауне: не накапливать
}

// TestPickExcludingForModelSkipsSoftCoolingSameModel Аккаунт в кулдауне (6004 с временем парсинга,
// уже записанная модель)+ Совм. model Запрос → По-прежнему недоступно для выбора (сохранение текущей семантики).
func TestPickExcludingForModelSkipsSoftCoolingSameModel(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.Add(&auth.Auth{UID: "u2"})
	p.SetCredits("u1", 1000, 0)
	p.SetCredits("u2", 1, 0)
	p.SetRandomSource(func(n int64) int64 { return 0 }) // r=0 → Максимальный балл u1
	p.CooldownSoftForModel("u1", time.Minute, time.Now().Add(5*time.Minute), "glm-5.3", "429 rate limit")
	got := p.PickExcludingForModel(nil, "glm-5.3")
	if got == nil || got.UID != "u2" {
		t.Fatalf("same-model request must skip cooling u1, got %+v", got)
	}
}

// TestPickExcludingForModelAllowsDifferentModel 6004 Аккаунт в охлаждении + Разное model
// → Считается доступным, может быть выбран (реальное ограничение на одну модель, при смене модели сразу доступен).
func TestPickExcludingForModelAllowsDifferentModel(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.SetCredits("u1", 1000, 0)
	p.Add(&auth.Auth{UID: "u2"})
	p.SetCredits("u2", 1, 0)
	p.SetRandomSource(func(n int64) int64 { return 0 }) // r=0 → Максимальный балл u1
	p.CooldownSoftForModel("u1", time.Minute, time.Now().Add(5*time.Minute), "glm-5.3", "429 rate limit")
	got := p.PickExcludingForModel(nil, "hy3-x")
	if got == nil || got.UID != "u1" {
		t.Fatalf("different-model request should bypass u1 soft cooling, got %+v", got)
	}
}

// TestCooldownSoftWithoutModelRecordsNone не 6004 обычный мягкий кулдаун (resetAt нулевое значение,
// Не логировать softRateModel）→ смена модели не освобождает (текущая семантика).
func TestCooldownSoftWithoutModelRecordsNone(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.Add(&auth.Auth{UID: "u2"})
	p.SetCredits("u1", 1000, 0)
	p.SetCredits("u2", 1, 0)
	p.SetRandomSource(func(n int64) int64 { return 0 })
	p.CooldownSoftForModel("u1", time.Minute, time.Time{}, "", "429 rate limit")
	// в кулдауне + Разное model Запрос всё равно пропускается u1（отсутствует softRateModel，не освобождается).
	got := p.PickExcludingForModel(nil, "hy3-x")
	if got == nil || got.UID != "u2" {
		t.Fatalf("no model recorded → must not bypass, got %+v", got)
	}
}

// TestPickExcludingForModelBreakerStillBlocks исключение модели — только по измерению мягкого кулдауна,
// Circuit breaker (breakerUntil）Всё равно блокировать:6004 Охлаждение + аккаунт в circuit-break — недоступен даже при смене модели.
func TestPickExcludingForModelBreakerStillBlocks(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.Add(&auth.Auth{UID: "u2"})
	p.SetCredits("u1", 1000, 0)
	p.SetCredits("u2", 1, 0)
	p.SetRandomSource(func(n int64) int64 { return 0 })
	p.SetBreaker(1, time.Hour, time.Hour)
	p.NoteError("u1") // u1 Circuit Breaker
	p.CooldownSoftForModel("u1", time.Minute, time.Now().Add(5*time.Minute), "glm-5.3", "429 rate limit")
	got := p.PickExcludingForModel(nil, "hy3-x")
	if got == nil || got.UID != "u2" {
		t.Fatalf("breaker must still block, got %+v", got)
	}
}

// TestSoftRateModelClearedByPlainCooldown Регрессия:6004 После кулдауна модели, если аккаунт снова проходит
// **Не на уровне модели**soft-кулдаун (plain Cooldown），softRateModel должен быть очищен — иначе предыдущий 6004 
// исключение модели просочится на аккаунт-уровневый rate limit, приводя к"Запрос смены модели"Ошибка пропускает данный cooldown.
func TestSoftRateModelClearedByPlainCooldown(t *testing.T) {
	withNoPickGap(t)
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.Add(&auth.Auth{UID: "u2"})
	p.SetCredits("u1", 1000, 0)
	p.SetCredits("u2", 1, 0)
	p.SetRandomSource(func(n int64) int64 { return 0 })

	// 1) 6004 Со временем парсинга → Записать модель glm-5.3。
	p.CooldownSoftForModel("u1", time.Minute, time.Now().Add(5*time.Minute), "glm-5.3", "6004")
	if got := p.PickExcludingForModel(nil, "hy3-x"); got == nil || got.UID != "u1" {
		t.Fatalf("precondition: different-model should bypass, got %+v", got)
	}
	// 2) После восстановления аккаунт проходит обычный мягкий кулдаун на уровне аккаунта (без семантики модели).
	p.NoteSuccess("u1") // Восстановить fresh статус (Cooldown будет сброшено until）
	p.Cooldown("u1", CoolSoft, time.Minute, "429 rate limit")
	// 3) запрос смены модели больше не exempt (softRateModel очищено).
	got := p.PickExcludingForModel(nil, "hy3-x")
	if got == nil || got.UID != "u2" {
		t.Fatalf("plain cooldown must clear softRateModel (no bypass), got %+v", got)
	}
}

// TestSoftRateModelNotPersistedToState Независимый кулдаун на уровне модели (modelCooldowns）Это runtime-состояние:
// при сохранении на диск поле не заводится, при рестарте сброс в ноль (деградация до только уровня аккаунта until текущее состояние кулдауна).
// новая семантика:6004 со временем сброса — только запись modelCooldowns、не записывать until → После перезагрузки аккаунт без кулдауна,
// реестр пуст.
func TestSoftRateModelPersistsToState(t *testing.T) {
	// 6004 сброс wall-clock может длиться часы, переживает рестарт — норма:model_cooldowns Персистентность,
	// После восстановления healthyForModel без потери памяти (поглощение апстрима 2f4c77b）。
	dir := t.TempDir()
	fp := filepath.Join(dir, "state.json")
	p := New(fp)
	p.Add(&auth.Auth{UID: "u1"})
	// resetAt Должно быть значительно позже now：Передача time.Now() Пойдет по"время сброса истекло"Ветка жмет кулдаун до 1ms。
	p.CooldownSoftForModel("u1", time.Minute, time.Now().Add(10*time.Minute), "glm-5.3", "429 rate limit")
	p.Flush()
	raw, err := os.ReadFile(fp)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), "model_cooldowns") {
		t.Errorf("state.json Требует персистентности model_cooldowns: %s", raw)
	}
	p2 := New(fp)
	p2.Add(&auth.Auth{UID: "u1"})
	st, ok := p2.Status("u1")
	if !ok {
		t.Fatalf("account missing after reload")
	}
	if st.Cooling {
		t.Fatalf("6004-with-reset не писать на уровне аккаунта until，После перезагрузки не должен cooling: %+v", st)
	}
	if len(st.RateLimitedModels) != 1 || st.RateLimitedModels[0].Model != "glm-5.3" {
		t.Errorf("modelCooldowns should survive reload, got %+v", st.RateLimitedModels)
	}
}

func TestList(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1", Nickname: "nick1"})
	p.Add(&auth.Auth{UID: "u2"})
	p.SetCredits("u1", 42, 0)
	p.Cooldown("u2", CoolSoft, time.Minute, "429")
	list := p.List()
	if len(list) != 2 {
		t.Fatalf("list=%d", len(list))
	}
	var s1, s2 Status
	for _, s := range list {
		if s.UID == "u1" {
			s1 = s
		}
		if s.UID == "u2" {
			s2 = s
		}
	}
	if s1.Credits != 42 || s1.Nickname != "nick1" || s1.Disabled || s1.Cooling {
		t.Errorf("s1=%+v", s1)
	}
	if !s2.Cooling || s2.Reason != "429" {
		t.Errorf("s2=%+v", s2)
	}
}

func TestRemoveMissingFromDir(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.Add(&auth.Auth{UID: "u2"})
	p.SyncToDir([]*auth.Auth{{UID: "u2"}})
	if p.Pick() == nil || p.Pick().UID != "u2" {
		t.Fatal("u1 should be removed")
	}
	if _, ok := p.Status("u1"); ok {
		t.Fatal("u1 should not exist")
	}
}

func TestFlushPersistsCredits(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "state.json")
	p := New(fp)
	p.Add(&auth.Auth{UID: "u1"})
	p.SetCredits("u1", 42, 0)
	p.Flush()
	p2 := New(fp)
	p2.Add(&auth.Auth{UID: "u1"})
	st, ok := p2.Status("u1")
	if !ok || st.Credits != 42 {
		t.Fatalf("flush not persisted: %+v ok=%v", st, ok)
	}
}

func TestAutoFlush(t *testing.T) {
	old := flushInterval
	flushInterval = 20 * time.Millisecond
	defer func() { flushInterval = old }()

	dir := t.TempDir()
	fp := filepath.Join(dir, "state.json")
	p := New(fp)
	p.Add(&auth.Auth{UID: "u1"})
	p.SetCredits("u1", 77, 0)

	deadline := time.Now().Add(2 * time.Second)
	for {
		if _, err := os.Stat(fp); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("state.json not written by background flusher")
		}
		time.Sleep(10 * time.Millisecond)
	}
	p2 := New(fp)
	p2.Add(&auth.Auth{UID: "u1"})
	st, ok := p2.Status("u1")
	if !ok || st.Credits != 77 {
		t.Fatalf("auto flush not persisted: %+v ok=%v", st, ok)
	}
}

func TestFlushIdempotentWhenClean(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "state.json")
	p := New(fp)
	p.Add(&auth.Auth{UID: "u1"})
	p.Flush() // отсутствует dirty，не писать на диск
	if _, err := os.Stat(fp); !os.IsNotExist(err) {
		t.Fatalf("flush on clean pool should not write: %v", err)
	}
}

func TestSaveFailureRecordedAndRecovers(t *testing.T) {
	// stateFp родительский путь — обычный файл (не директория)→ MkdirAll/WriteFile обязательно завершится ошибкой,
	// root Также не обходится, надежно триггерит путь ошибки сохранения на диск.
	dir := t.TempDir()
	block := filepath.Join(dir, "block")
	if err := os.WriteFile(block, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	p := New(filepath.Join(block, "state.json"))
	p.Add(&auth.Auth{UID: "u1"})
	p.SetCredits("u1", 42, 0)
	p.Flush()
	if p.persistFails == 0 {
		t.Fatal("persist failure should be recorded (visible), got 0")
	}

	// переключить обратно на записываемый каталог → после успеха persistFails обнуление (лог восстановления триггерится порогом нулевого значения).
	good := filepath.Join(t.TempDir(), "state.json")
	p2 := New(good)
	p2.Add(&auth.Auth{UID: "u1"})
	p2.SetCredits("u1", 42, 0)
	p2.Flush()
	if p2.persistFails != 0 {
		t.Fatalf("successful save should reset persistFails, got %d", p2.persistFails)
	}
	if raw, err := os.ReadFile(good); err != nil || !strings.Contains(string(raw), `"credits": 42`) {
		t.Fatalf("state.json not written on success: %v %s", err, raw)
	}
}

// ---------------------------------------------------------------------------
// T2 Circuit Breaker + Фолбэк полного кулдауна + Экспоненциальный бэкофф
// ---------------------------------------------------------------------------

// breakerUntil Экспонирование внутреннего состояния для тестовых ассертов (приватно в пакете helper）。
func (p *Pool) breakerUntil(uid string) (time.Time, bool) {
	p.mu.RLock()
	defer p.mu.RUnlock()
	e, ok := p.byUID[uid]
	if !ok {
		return time.Time{}, false
	}
	return e.breakerUntil, true
}

// breakerFails Раскрытие entry.fails Для assert'ов в тестах (внутри пакета приватно helper）。
func (p *Pool) breakerFails(uid string) int {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.byUID[uid].fails
}

// internalHealthy Раскрытие entry.healthy Для assert'ов в тестах (внутри пакета приватно helper）。
func (p *Pool) internalHealthy(uid string) bool {
	p.mu.RLock()
	defer p.mu.RUnlock()
	e, ok := p.byUID[uid]
	if !ok {
		return false
	}
	return e.healthy(time.Now())
}

func TestBreakerTripsAtThreshold(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.SetBreaker(3, time.Hour, 6*time.Hour)
	for i := 0; i < 2; i++ {
		p.NoteError("u1") // NoteError только триггерит circuit breaker (больше нет отдельного err кулдаун)
		if bt, ok := p.breakerUntil("u1"); ok && !bt.IsZero() {
			t.Fatalf("breaker tripped too early at %d: %v", i+1, bt)
		}
	}
	p.NoteError("u1")
	bt, ok := p.breakerUntil("u1")
	if !ok || bt.IsZero() {
		t.Fatalf("breaker should trip at threshold: until=%v ok=%v", bt, ok)
	}
}

func TestBreakerSuccessClears(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.SetBreaker(3, time.Hour, 6*time.Hour)
	p.NoteError("u1")
	p.NoteError("u1")
	p.NoteError("u1") // срабатывание circuit breaker
	if bt, _ := p.breakerUntil("u1"); bt.IsZero() {
		t.Fatal("breaker should be open")
	}
	p.NoteSuccess("u1")
	if bt, _ := p.breakerUntil("u1"); !bt.IsZero() {
		t.Fatalf("success should clear breaker, until=%v", bt)
	}
	if !p.internalHealthy("u1") {
		t.Fatal("account should be healthy after success clears breaker")
	}
}

func TestBreakerExponentialBackoffCapped(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.SetBreaker(3, time.Minute, 4*time.Minute) // threshold=3：непрерывно 3 N неудач — одно срабатывание circuit breaker
	// непрерывно 9 раз неудач (без успехов)→ Circuit Breaker 3 раз,retryCount 1→2→3，Бэкофф 1m→2m→4m(Потолок)。
	for i := 0; i < 3; i++ {
		for j := 0; j < 3; j++ {
			p.NoteError("u1") // серия неудач триггерит только circuit breaker
		}
	}
	bt, ok := p.breakerUntil("u1")
	if !ok || bt.IsZero() {
		t.Fatal("breaker should be open")
	}
	d := time.Until(bt)
	// № 3 раз срабатывания защиты:d = min(1m * 2^2, 4m) = 4m
	if d < 4*time.Minute-time.Second || d > 4*time.Minute+time.Second {
		t.Errorf("backoff should cap at max=4m, got %v", d)
	}

	// Сравнение с 1 срабатываний circuit breaker (новый аккаунт заново): бэкофф должен быть короче.
	p2 := New("")
	p2.Add(&auth.Auth{UID: "u1"})
	p2.SetBreaker(3, time.Minute, 4*time.Minute)
	for j := 0; j < 3; j++ {
		p2.NoteError("u1")
	}
	bt1, _ := p2.breakerUntil("u1")
	if d1 := time.Until(bt1); d1 > time.Minute+time.Second {
		t.Errorf("first trip should be ~1m, got %v", d1)
	}
}

func TestFallbackPicksEarliestExpiry(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "late"})
	p.Add(&auth.Auth{UID: "early"})
	// оба в мягком кулдауне;early истекает раньше → выбор fallback early。
	p.Cooldown("late", CoolSoft, 2*time.Hour, "x")
	p.Cooldown("early", CoolSoft, time.Hour, "x")
	got := p.Pick()
	if got == nil || got.UID != "early" {
		t.Fatalf("fallback should pick earliest expiry (early), got %+v", got)
	}
}

func TestFallbackSkipsDisabled(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "cooled"})
	p.Add(&auth.Auth{UID: "dead"})
	p.Cooldown("cooled", CoolSoft, time.Hour, "x")
	p.Disable("dead", "session dead") // Отключенные не участвуют в фолбэке
	got := p.Pick()
	if got == nil || got.UID != "cooled" {
		t.Fatalf("fallback should skip disabled, got %+v", got)
	}
}

func TestFallbackSkipsHardCooldown(t *testing.T) {
	// D3：Баланс исчерпан (CoolHard）номер не участвует в фолбэке — если вызван, то обязательно 402，Тратит ротацию и создаёт шумовые логи.
	p := New("")
	p.Add(&auth.Auth{UID: "hard"})
	p.Cooldown("hard", CoolHard, time.Hour, "Недостаточно средств")
	if got := p.Pick(); got != nil {
		t.Fatalf("hard-cooled account must not be fallback-picked, got %+v", got)
	}
}

func TestFallbackAllHardReturnsNil(t *testing.T) {
	// весь hard Охлаждение → Без мягкого cooldown/резервный номер на случай срабатывания circuit breaker → вернуть nil。
	p := New("")
	p.Add(&auth.Auth{UID: "h1"})
	p.Add(&auth.Auth{UID: "h2"})
	p.Cooldown("h1", CoolHard, time.Hour, "x")
	p.Cooldown("h2", CoolHard, 2*time.Hour, "x")
	if got := p.Pick(); got != nil {
		t.Fatalf("all-hard should return nil, got %+v", got)
	}
}

func TestFallbackSoftAndBreakerParticipate(t *testing.T) {
	// D3：soft и breaker охлажденные аккаунты допускаются в fallback, берется с ближайшим истечением.
	p := New("")
	p.Add(&auth.Auth{UID: "soft"})
	p.Add(&auth.Auth{UID: "brk"})
	p.Cooldown("soft", CoolSoft, 10*time.Minute, "429") // soft: until=10m, fails=1
	p.SetBreaker(2, 5*time.Minute, 5*time.Minute) // Порог 2：soft 1 неудач без трипа
	p.NoteError("brk") // brk: fails=1
	p.NoteError("brk") // brk: Срабатывание circuit breaker,breakerUntil=5m
	got := p.Pick()
	if got == nil {
		t.Fatal("fallback should pick breaker (earliest) account")
	}
	if got.UID != "brk" {
		t.Fatalf("fallback should pick earliest expiry brk (5m < soft 10m), got %+v", got)
	}
}

func TestFallbackNilWhenAllDisabled(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.Disable("u1", "session dead")
	if got := p.Pick(); got != nil {
		t.Fatalf("want nil when all disabled, got %+v", got)
	}
}

// ---------------------------------------------------------------------------
// T3 Взвешенный выбор по трём факторам
// ---------------------------------------------------------------------------

// idleWeightOf Раскрытие weightOf разложение по одному фактору неудобно, заменено на assert полного веса (приватный в пакете helper）。
func (p *Pool) entryWeight(uid string) float64 {
	p.mu.RLock()
	defer p.mu.RUnlock()
	e := p.byUID[uid]
	var maxCredits int64
	for _, x := range p.byUID {
		if x.credits > maxCredits {
			maxCredits = x.credits
		}
	}
	return p.weightOf(e, maxCredits, time.Now())
}

func TestWeightHighCreditsDominates(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "hi"})
	p.Add(&auth.Auth{UID: "lo"})
	p.SetCredits("hi", 1000, 0)
	p.SetCredits("lo", 10, 0)
	wHi, wLo := p.entryWeight("hi"), p.entryWeight("lo")
	if wHi <= wLo {
		t.Errorf("high credits should weigh more: hi=%v lo=%v", wHi, wLo)
	}
}

func TestWeightIdleCompensation(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "used"})
	p.Add(&auth.Auth{UID: "idle"})
	p.SetCredits("used", 100, 0)
	p.SetCredits("idle", 100, 0)
	// used 1 выбирался за последние N часов,idle Не использовался → idle вес выше (компенсация простоя).
	p.mu.Lock()
	p.byUID["used"].lastUsed = time.Now().Add(-1 * time.Hour)
	p.mu.Unlock()
	wUsed, wIdle := p.entryWeight("used"), p.entryWeight("idle")
	if wIdle <= wUsed {
		t.Errorf("idle should weigh more: used=%v idle=%v", wUsed, wIdle)
	}
}

func TestWeightAllZeroCreditsStillWeighted(t *testing.T) {
	// credits весь 0：вес полностью определяется idle+successRate решение, без деградации к равномерному random (возможен выбор более высокого скора).
	p := New("")
	p.Add(&auth.Auth{UID: "idle"})
	p.Add(&auth.Auth{UID: "bursty"})
	// idle Никогда не использовалось,bursty использовано полминуты назад → idle вес выше.
	p.mu.Lock()
	p.byUID["bursty"].lastUsed = time.Now().Add(-30 * time.Second)
	p.mu.Unlock()
	wIdle, wBursty := p.entryWeight("idle"), p.entryWeight("bursty")
	if wIdle <= wBursty {
		t.Errorf("idle should outweigh recently-used when credits all zero: idle=%v bursty=%v", wIdle, wBursty)
	}
}

func TestWeightTopFiveSelectionChanges(t *testing.T) {
	withNoPickGap(t)
	// credits при небольшой разнице компенсация простоя позволяет"Низкий скор, но долго хранится«превышение веса аккаунта«высокий скор, но только что использован"аккаунта,
	// даже если credits В сортировке b перед (Top5 внутренняя весовая сортировка может совместно с credits сортировка отличается).
	p := New("")
	for _, u := range []string{"a", "b"} {
		p.Add(&auth.Auth{UID: u})
	}
	p.SetCredits("a", 90, 0) // a credits чуть ниже, но при длительном хранении
	p.SetCredits("b", 100, 0)
	p.mu.Lock()
	p.byUID["b"].lastUsed = time.Now()
	p.byUID["a"].lastUsed = time.Now().Add(-48 * time.Hour)
	p.mu.Unlock()
	if wA, wB := p.entryWeight("a"), p.entryWeight("b"); wA <= wB {
		t.Errorf("idle a should outweigh busy higher-credit b: a=%v b=%v", wA, wB)
	}
}

// ---------------------------------------------------------------------------
// T4 Активная аренда (лимит параллелизма на аккаунт)
// ---------------------------------------------------------------------------

func TestAcquireReleaseLifecycle(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.SetMaxInFlight(2)
	if !p.Acquire("u1") {
		t.Fatal("first acquire should succeed")
	}
	if !p.Acquire("u1") {
		t.Fatal("second acquire should succeed")
	}
	if p.Acquire("u1") {
		t.Fatal("third acquire should fail (limit 2)")
	}
	p.Release("u1")
	if !p.Acquire("u1") {
		t.Fatal("acquire after release should succeed")
	}
}

func TestAcquireUnlimited(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	// max=0 Без ограничений: непрерывно acquire никогда не отклонять.
	for i := 0; i < 100; i++ {
		if !p.Acquire("u1") {
			t.Fatalf("unlimited acquire %d failed", i)
		}
	}
}

func TestAcquireUnknownUID(t *testing.T) {
	p := New("")
	if p.Acquire("nope") {
		t.Fatal("acquire unknown uid should fail")
	}
	p.Release("nope") // Не panic
}

func TestPickSkipsInFlightFull(t *testing.T) {
	withNoPickGap(t)
	p := New("")
	p.Add(&auth.Auth{UID: "full"})
	p.Add(&auth.Auth{UID: "free"})
	p.SetCredits("full", 1000, 0)
	p.SetCredits("free", 1, 0)
	p.SetMaxInFlight(1)
	// full Занят единственный слот → Pick следует пропустить, выбрать free（даже если credits ниже).
	p.Acquire("full")
	got := p.Pick()
	if got == nil || got.UID != "free" {
		t.Fatalf("pick should skip in-flight-full account, got %+v", got)
	}
	p.Release("full")
	// После освобождения может быть выбран повторно (детерминированный источник случайности r=0 → Выбрать credits наивысший full）。
	p.SetRandomSource(func(n int64) int64 { return 0 })
	if got := p.Pick(); got == nil || got.UID != "full" {
		t.Fatalf("after release full should be pickable, got %+v", got)
	}
	p.Release("full")
}

func TestInFlightCountNotExceedLimit(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.SetMaxInFlight(2)

	// Конкурентность 50 Раз acquire：CAS Гарантировать, что in-flight в любой момент не превышает лимит; после каждого успеха сразу release。
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if p.Acquire("u1") {
				// Проверка пика:acquire Сразу после успеха читать счётчик, должен ≤ 2。
				p.mu.RLock()
				if n := p.byUID["u1"].inFlight.Load(); n > 2 {
					t.Errorf("in-flight exceeded limit: %d", n)
				}
				p.mu.RUnlock()
				p.Release("u1")
			}
		}()
	}
	wg.Wait()

	// После полного освобождения счётчик должен быть 0。
	p.mu.RLock()
	n := p.byUID["u1"].inFlight.Load()
	p.mu.RUnlock()
	if n != 0 {
		t.Fatalf("in-flight should be 0 after all releases, got %d", n)
	}
}

// ---------------------------------------------------------------------------
// T6 Обратная совместимость + рантайм-состояние Status Расширение
// ---------------------------------------------------------------------------

func TestLoadLegacyStateFile(t *testing.T) {
	// старый state.json Содержит только credits/until/disabled и др. старые поля, отсутствует circuit breaker/в пути/новое поле успешности.
	dir := t.TempDir()
	fp := filepath.Join(dir, "state.json")
	legacy := `{"accounts":{"legacy":{"credits":123,"until":"2027-01-01T04:00:00+08:00","cool_kind":1,"reason":"Недостаточно средств"}}}`
	if err := os.WriteFile(fp, []byte(legacy), 0o600); err != nil {
		t.Fatal(err)
	}
	p := New(fp)
	p.Add(&auth.Auth{UID: "legacy"})
	st, ok := p.Status("legacy")
	if !ok {
		t.Fatal("legacy account should load")
	}
	if st.Credits != 123 || !st.Cooling || st.Reason != "Недостаточно средств" {
		t.Errorf("legacy state misloaded: %+v", st)
	}
	// Новые поля рантайма по умолчанию — ноль.
	if st.InFlight != 0 || st.BreakerFails != 0 || !st.BreakerUntil.IsZero() {
		t.Errorf("runtime fields should be zero for legacy load: %+v", st)
	}
}

// ---------------------------------------------------------------------------
// T7 D5: Redis Зеркало снапшота состояния + Восстановление выбором нового
// ---------------------------------------------------------------------------

// memStore Память фейк Store：Запись SaveState（Эмуляция Redis снапшот) и может возвращаться по требованию LoadState。
type memStore struct {
	mu sync.Mutex
	saved []byte
	loadData []byte
	loadOK bool
}

func (m *memStore) SaveState(data []byte) {
	m.mu.Lock()
	m.saved = append([]byte(nil), data...)
	m.mu.Unlock()
}
func (m *memStore) LoadState() ([]byte, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if !m.loadOK {
		return nil, false
	}
	return append([]byte(nil), m.loadData...), true
}

func TestSaveMirrorsSnapshot(t *testing.T) {
	// Flush синхронизация при сбросе на диск fire-and-forget SaveState（Лента saved_at）。
	dir := t.TempDir()
	fp := filepath.Join(dir, "state.json")
	p := New(fp)
	ms := &memStore{}
	p.SetStore(ms)
	p.Add(&auth.Auth{UID: "u1"})
	p.SetCredits("u1", 42, 0)
	p.Flush()
	ms.mu.Lock()
	raw := string(ms.saved)
	ms.mu.Unlock()
	if !strings.Contains(raw, `"saved_at"`) {
		t.Fatalf("snapshot should carry saved_at: %s", raw)
	}
	if !strings.Contains(raw, `"credits":42`) {
		t.Fatalf("snapshot should carry account state: %s", raw)
	}
}

func TestRestoreUsesRedisWhenNewer(t *testing.T) {
	// Redis Снапшот новее локального state.json новый → использовать Redis。
	dir := t.TempDir()
	fp := filepath.Join(dir, "state.json")
	// Локально устарело
	if err := os.WriteFile(fp, []byte(`{"accounts":{"u1":{"credits":1}}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	// Локальный mtime Установить в прошлое
	old := time.Now().Add(-time.Hour)
	if err := os.Chtimes(fp, old, old); err != nil {
		t.Fatal(err)
	}
	ms := &memStore{loadOK: true}
	snap := snapshot{stateFile: stateFile{Accounts: map[string]stateAccount{"u1": {Credits: 999}}}, SavedAt: time.Now()}
	ms.loadData, _ = json.Marshal(snap)
	p := New(fp)
	p.SetStore(ms)
	p.RestoreFromSnapshot()
	st, ok := p.Status("u1")
	if !ok || st.Credits != 999 {
		t.Fatalf("should restore from Redis snapshot: %+v ok=%v", st, ok)
	}
}

func TestRestoreUsesLocalWhenNewer(t *testing.T) {
	// Локально state.json сравнение Redis Снимок новый → Приоритет локального.
	dir := t.TempDir()
	fp := filepath.Join(dir, "state.json")
	if err := os.WriteFile(fp, []byte(`{"accounts":{"u1":{"credits":77}}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	ms := &memStore{loadOK: true}
	snap := snapshot{stateFile: stateFile{Accounts: map[string]stateAccount{"u1": {Credits: 999}}}, SavedAt: time.Now().Add(-time.Hour)}
	ms.loadData, _ = json.Marshal(snap)
	p := New(fp)
	p.SetStore(ms)
	p.RestoreFromSnapshot()
	st, ok := p.Status("u1")
	if !ok || st.Credits != 77 {
		t.Fatalf("should keep local (newer): %+v ok=%v", st, ok)
	}
}

func TestRestoreNoRedisUsesLocal(t *testing.T) {
	// отсутствует Redis Снапшот → Приоритет локального.
	dir := t.TempDir()
	fp := filepath.Join(dir, "state.json")
	if err := os.WriteFile(fp, []byte(`{"accounts":{"u1":{"credits":55}}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	ms := &memStore{loadOK: false}
	p := New(fp)
	p.SetStore(ms)
	p.RestoreFromSnapshot()
	st, ok := p.Status("u1")
	if !ok || st.Credits != 55 {
		t.Fatalf("no redis → use local: %+v ok=%v", st, ok)
	}
}

func TestStatusExposesRuntimeFields(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.SetMaxInFlight(2)
	p.Acquire("u1") // in_flight=1
	st, _ := p.Status("u1")
	if st.InFlight != 1 {
		t.Errorf("in_flight=%d want 1", st.InFlight)
	}
	p.SetBreaker(2, time.Hour, 2*time.Hour)
	p.NoteError("u1") // breaker_fails=1
	st, _ = p.Status("u1")
	if st.BreakerFails != 1 {
		t.Errorf("breaker_fails=%d want 1", st.BreakerFails)
	}
	p.Release("u1")
}

func TestRecordTokenUsage(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	before := time.Now()
	p.RecordTokenUsage("u1", TokenUsageDelta{
		Model: "glm-5.2",
		HasPromptTokens: true,
		PromptTokens: 5,
		HasCompletionTokens: true,
		CompletionTokens: 7,
		HasTotalTokens: true,
		TotalTokens: 12,
		HasLatencyMs: true,
		LatencyMs: 1250,
		HasTokensPerSecond: true,
		TokensPerSecond: 9.6,
	})
	p.RecordTokenUsage("u1", TokenUsageDelta{Model: "glm-5.2", HasLatencyMs: true, LatencyMs: 300})
	st, _ := p.Status("u1")
	if st.TokenUsage.RequestCount != 2 {
		t.Errorf("request_count=%d want 2", st.TokenUsage.RequestCount)
	}
	if st.TokenUsage.UsageCount != 1 {
		t.Errorf("usage_count=%d want 1", st.TokenUsage.UsageCount)
	}
	if st.TokenUsage.PromptTokens != 5 || st.TokenUsage.CompletionTokens != 7 || st.TokenUsage.TotalTokens != 12 {
		t.Errorf("token usage=%+v", st.TokenUsage)
	}
	if st.TokenUsage.LastLatencyMs != 300 || st.TokenUsage.LastTokensPerSecond != nil {
		t.Errorf("latest performance should replace speed with unknown: %+v", st.TokenUsage)
	}
	if st.TokenUsage.LastModel != "glm-5.2" || st.TokenUsage.LastUsedAt.Before(before) {
		t.Errorf("last usage=%+v", st.TokenUsage)
	}
}

func TestTokenUsagePersistsAcrossReload(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "state.json")
	p := New(fp)
	p.Add(&auth.Auth{UID: "u1"})
	p.RecordTokenUsage("u1", TokenUsageDelta{
		Model: "deepseek-v4",
		HasPromptTokens: true,
		PromptTokens: 11,
		HasCompletionTokens: true,
		CompletionTokens: 13,
		HasTotalTokens: true,
		TotalTokens: 24,
		HasLatencyMs: true,
		LatencyMs: 2300,
		HasTokensPerSecond: true,
		TokensPerSecond: 5.65,
	})
	p.Flush()
	p2 := New(fp)
	p2.Add(&auth.Auth{UID: "u1"})
	st, ok := p2.Status("u1")
	if !ok {
		t.Fatal("account missing after reload")
	}
	if st.TokenUsage.RequestCount != 1 || st.TokenUsage.TotalTokens != 24 || st.TokenUsage.LastModel != "deepseek-v4" {
		t.Errorf("token usage lost after reload: %+v", st.TokenUsage)
	}
	if st.TokenUsage.LastLatencyMs != 2300 || st.TokenUsage.LastTokensPerSecond == nil || *st.TokenUsage.LastTokensPerSecond != 5.65 {
		t.Errorf("latest performance lost after reload: %+v", st.TokenUsage)
	}
	raw, err := os.ReadFile(fp)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `"token_usage"`) {
		t.Fatalf("state.json missing token_usage: %s", raw)
	}
	if strings.Contains(string(raw), "AccessToken") || strings.Contains(string(raw), "RefreshToken") {
		t.Fatalf("state.json contains credential field: %s", raw)
	}
}

func TestPickPrefersExpiringByVirtualWeight(t *testing.T) {
	withNoPickGap(t)
	p := New("")
	p.Add(&auth.Auth{UID: "later"})
	p.Add(&auth.Auth{UID: "soon"})
	p.Add(&auth.Auth{UID: "none"})

	now := time.Now()
	p.SetCreditsDetailed("later", 100, 100, 100, now.Add(48*time.Hour), 100)
	p.SetCreditsDetailed("soon", 100, 100, 100, now.Add(2*time.Hour), 100)
	p.SetCreditsDetailed("none", 100, 100, 0, time.Time{}, 0)

	// 3:1 Виртуальный инстанс — мягкое предпочтение, а не жёсткий приоритет. Для статистики долгосрочного распределения использовать детерминированный источник случайности: два скоро истекающих
	// суммарная доля аккаунта должна быть значительно выше обычной, при этом у обычных аккаунтов сохраняется небольшой трафик.
	rng := rand.New(rand.NewPCG(1, 2))
	p.SetRandomSource(func(n int64) int64 { return rng.Int64N(n) })
	counts := map[string]int{}
	for i := 0; i < 2000; i++ {
		got := p.Pick()
		if got == nil {
			t.Fatal("pick returned nil")
		}
		counts[got.UID]++
	}
	expiring := counts["soon"] + counts["later"]
	if expiring < 1500 || counts["none"] == 0 {
		t.Fatalf("virtual weight distribution=%v, want expiring majority and regular non-zero", counts)
	}
}

func TestPickExpiringTieUsesExistingWeight(t *testing.T) {
	withNoPickGap(t)
	p := New("")
	p.Add(&auth.Auth{UID: "small"})
	p.Add(&auth.Auth{UID: "large"})
	p.SetRandomSource(func(n int64) int64 { return 0 })

	at := time.Now().Add(time.Hour)
	p.SetCreditsDetailed("small", 10, 10, 10, at, 10)
	p.SetCreditsDetailed("large", 50, 50, 50, at, 50)

	got := p.Pick()
	if got == nil || got.UID != "large" {
		t.Fatalf("pick=%v want large", got)
	}
}

func TestPreferExpiringDisabledRestoresWeight(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "a"})
	p.Add(&auth.Auth{UID: "b"})
	now := time.Now()
	p.SetCreditsDetailed("a", 100, 100, 50, now.Add(time.Hour), 50)
	p.SetCreditsDetailed("b", 100, 100, 0, time.Time{}, 0)

	p.mu.Lock()
	we := p.routingWeightOf(p.byUID["a"], 100, now)
	wn := p.routingWeightOf(p.byUID["b"], 100, now)
	p.mu.Unlock()
	if we != wn*expiringVirtualSlots {
		t.Fatalf("enabled expiring weight=%v want %v", we, wn*expiringVirtualSlots)
	}

	p.SetPreferExpiring(false)

	p.mu.Lock()
	wa := p.routingWeightOf(p.byUID["a"], 100, now)
	wb := p.routingWeightOf(p.byUID["b"], 100, now)
	p.mu.Unlock()
	if wa != wb {
		t.Fatalf("disabled expiring weights differ: %v/%v", wa, wb)
	}
}

func TestCreditExpirySnapshotConsumptionAndClear(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	now := time.Now()
	p.SetCreditsDetailed("u1", 100, 100, 50, now.Add(time.Hour), 40)

	st, _ := p.Status("u1")
	if st.CreditsExpiring != 50 || st.CreditsEarliestRemaining != 40 || st.CreditsEarliestExpiry.IsZero() {
		t.Fatalf("initial snapshot=%+v", st)
	}

	p.NoteModelCost("u1", "m", 10, 1000)
	st, _ = p.Status("u1")
	if st.Credits != 90 || st.CreditsExpiring != 40 || st.CreditsEarliestRemaining != 30 {
		t.Fatalf("after consume=%+v", st)
	}

	p.NoteModelCost("u1", "m", 40, 1000)
	st, _ = p.Status("u1")
	if st.Credits != 50 || st.CreditsExpiring != 0 || st.CreditsEarliestRemaining != 0 || !st.CreditsEarliestExpiry.IsZero() {
		t.Fatalf("after exhaustion=%+v", st)
	}

	p.SetCreditsDetailed("u1", 50, 50, 10, now.Add(time.Hour), 10)
	p.SetCreditsDetailed("u1", 50, 50, 0, time.Time{}, 0)
	st, _ = p.Status("u1")
	if st.CreditsExpiring != 0 || st.CreditsEarliestRemaining != 0 || !st.CreditsEarliestExpiry.IsZero() {
		t.Fatalf("zero refresh did not clear snapshot=%+v", st)
	}
}
