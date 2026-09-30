package pool

import (
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
)

// ---------------------------------------------------------------------------
// 6004 Уровень модели limit Независимый кулдаун (issue：независимый таймер на модель)
// ---------------------------------------------------------------------------

// TestModelCooldownsIndependent Ядро: модель A Триггер 6004（Сброс 2h после), модель B Повторно триггерит
// 6004（Сброс 1h после)→
// 1. A кулдаун сохраняется независимо:1h После A всё ещё в лимите,B уже восстановлен;
// 2. until（уровня всех аккаунтов) не перекрывается ни одной моделью 6004 перезапись.
func TestModelCooldownsIndependent(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	base := time.Now()
	resetA := base.Add(2 * time.Hour)
	resetB := base.Add(1 * time.Hour)

	p.mu.Lock()
	e := p.byUID["u1"]
	e.modelCooldowns = map[string]modelCooldown{
		"glm-5.3": {Until: resetA, ResetAt: resetA},
		"hy3-x": {Until: resetB, ResetAt: resetB},
	}
	p.mu.Unlock()

	now := base.Add(90 * time.Minute)
	if e.healthyForModel(now, "glm-5.3") {
		t.Fatalf("90m После A(glm-5.3, reset 2h) все еще в лимите, но healthyForModel Пропущено")
	}
	if !e.healthyForModel(now, "hy3-x") {
		t.Fatalf("90m После B(hy3-x, reset 1h) должен был восстановиться, но healthyForModel Всё равно блокировать")
	}
	if !e.until.IsZero() {
		t.Errorf("until=%v Должно быть нулевым значением (6004 охлаждение на уровне модели не записывается until）", e.until)
	}
}

// TestModelCooldownsBDoesNotOverwriteA Модель B Триггер 6004 после,A дедлайн кулдауна не перезаписывается:
// Это issue ядро — старая реализация использует одиночный until поле,B перезапишет A。
func TestModelCooldownsBDoesNotOverwriteA(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	resetA := time.Now().Add(2 * time.Hour)
	// Симулировать реальный путь дважды 6004：A(2h) затем B(1h)。
	p.CooldownSoftForModel("u1", 600*time.Second, resetA, "glm-5.3", "6004 model rate limit")
	p.CooldownSoftForModel("u1", 600*time.Second, time.Now().Add(1*time.Hour), "hy3-x", "6004 model rate limit")

	p.mu.RLock()
	e := p.byUID["u1"]
	mcA, okA := e.modelCooldowns["glm-5.3"]
	until := e.until
	p.mu.RUnlock()
	if !okA {
		t.Fatalf("A(glm-5.3) запись кулдауна модели потеряна (была B перекрыть?)")
	}
	if d := mcA.Until.Sub(resetA); d < -time.Second || d > time.Second {
		t.Errorf("A until=%v want ~%v（B cooldown не должен перекрывать A дедлайн)", mcA.Until, resetA)
	}
	if !until.IsZero() {
		t.Errorf("until=%v Должно быть нулевым значением (6004 Никогда не писать на уровне аккаунта until）", until)
	}
}

// TestCooldownSoftForModelDoesNotClobberUntil с временем парсинга 6004 не записывать until
// （иначе глобальный кулдаун аккаунта загрязнится временем сброса модели), писать только modelCooldowns[model]。
func TestCooldownSoftForModelDoesNotClobberUntil(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	reset := time.Now().Add(30 * time.Minute)
	p.CooldownSoftForModel("u1", 600*time.Second, reset, "glm-5.3", "6004 model rate limit")
	p.mu.RLock()
	e := p.byUID["u1"]
	until := e.until
	mc, ok := e.modelCooldowns["glm-5.3"]
	p.mu.RUnlock()
	if !until.IsZero() {
		t.Errorf("until=%v должен быть ноль (6004 не записывать until）", until)
	}
	if !ok || mc.Until.IsZero() {
		t.Errorf("modelCooldowns[glm-5.3]=%+v ok=%v，должно уже быть записано охлаждение модели", mc, ok)
	}
}

// TestCooldownSoftForModelCapsUntilKeepsResetAt 6004 Запись modelCooldowns：
// until усечь до soft_rate_max，reset_at Сохранить исходное wall-clock время апстрима (issue #36 миграция семантики реестра).
func TestCooldownSoftForModelCapsUntilKeepsResetAt(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.SetSoftRateMax(10 * time.Minute)
	reset := time.Now().Add(2 * time.Hour) // Значительно превышает лимит → until усечь до 10m，reset_at сохранить 2h
	p.CooldownSoftForModel("u1", 600*time.Second, reset, "glm-5.3", "6004 model rate limit")
	p.mu.RLock()
	mc, ok := p.byUID["u1"].modelCooldowns["glm-5.3"]
	p.mu.RUnlock()
	if !ok {
		t.Fatal("modelCooldowns отсутствует glm-5.3")
	}
	if rem := mc.Until.Sub(time.Now()); rem <= 0 || rem > 10*time.Minute+time.Second {
		t.Errorf("Until Должен быть в (0,10m] интервал, фактический остаток %v", rem)
	}
	if d := mc.ResetAt.Sub(reset); d < -time.Second || d > time.Second {
		t.Errorf("ResetAt=%v want ~2h После=%v", mc.ResetAt, reset)
	}
}

// TestHealthyForModelAfterModelSpecific6004 6004 Блокировать только эту модель:
// Аккаунт недоступен для триггерной модели, доступен для других моделей (issue #31 сохранение исключения); уровень аккаунта healthy всё ещё true.
func TestHealthyForModelAfterModelSpecific6004(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	reset := time.Now().Add(30 * time.Minute)
	p.CooldownSoftForModel("u1", 600*time.Second, reset, "glm-5.3", "6004 model rate limit")
	p.mu.RLock()
	e := p.byUID["u1"]
	p.mu.RUnlock()

	now := time.Now()
	if e.healthyForModel(now, "glm-5.3") {
		t.Fatal("Триггер-модель glm-5.3 должен быть недоступен для выбора")
	}
	if !e.healthyForModel(now, "hy3-x") {
		t.Fatal("другие модели hy3-x должен быть опциональным (исключение для модели)")
	}
	if !e.healthy(now) {
		t.Fatal("Уровень аккаунта healthy должен всё ещё true（6004 блокируется только модель, аккаунт не блокируется)")
	}
}

// TestModelCooldownsTwoLimitsBothBlock Две модели одного аккаунта одновременно 6004：Обе модели недоступны для выбора
// （нет кулдауна на уровне аккаунта), другие модели по-прежнему доступны.
func TestModelCooldownsTwoLimitsBothBlock(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	resetA := time.Now().Add(2 * time.Hour)
	resetB := time.Now().Add(2 * time.Hour)
	p.mu.Lock()
	e := p.byUID["u1"]
	e.modelCooldowns = map[string]modelCooldown{
		"glm-5.3": {Until: resetA, ResetAt: resetA},
		"hy3-x": {Until: resetB, ResetAt: resetB},
	}
	p.mu.Unlock()

	now := time.Now()
	if e.healthyForModel(now, "glm-5.3") {
		t.Fatal("glm-5.3 Должен блокироваться собственным кулдауном")
	}
	if e.healthyForModel(now, "hy3-x") {
		t.Fatal("hy3-x Должен блокироваться собственным кулдауном")
	}
	if !e.healthyForModel(now, "other") {
		t.Fatal("other Модель должна оставаться доступной (лимит по нескольким моделям не должен делать аккаунт недоступным)")
	}
}

// TestModelCooldownsPreservedByNoteSuccess успех (NoteSuccess）нельзя очищать на уровне модели 6004 Кулдаун:
// Если очистить,B успех модели стирает A Независимый кулдаун модели — как раз этот issue Исправляемый ключевой дефект.
func TestModelCooldownsPreservedByNoteSuccess(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	resetA := time.Now().Add(2 * time.Hour)
	p.CooldownSoftForModel("u1", 600*time.Second, resetA, "glm-5.3", "6004 model rate limit")
	p.NoteSuccess("u1") // Другая модель успешна
	p.mu.RLock()
	mc, ok := p.byUID["u1"].modelCooldowns["glm-5.3"]
	p.mu.RUnlock()
	if !ok {
		t.Fatalf("NoteSuccess После A(glm-5.3) Независимый кулдаун сброшен — независимость моделей нарушена")
	}
	if d := mc.Until.Sub(resetA); d < -time.Second || d > time.Second {
		t.Errorf("A until=%v want ~%v（Не должен быть NoteSuccess помеха)", mc.Until, resetA)
	}
}

// TestModelCooldownsSurviveRevive check-in/Разморозка обновления баланса (reviveCoolingLocked）нельзя сбрасывать
// Уровень модели 6004 Кулдаун — доказательство снятия лимита — истечение wall-clock сброса upstream, а не восстановление баланса; обновление баланса
// Периодическая задача каждые 5 минут через ReenableIfCredits Достигнув сюда, если здесь очистить реестр, коллизия лимита будет ошибочно распознана
// здоров, перевыбрать и снова хит 429，Защита кулдауном всего пула фактически отсутствует (воспроизведено на пуле из двух номеров:expiring==0 
// номер каждый 5 реестр стирается раз в минут,expiring>0 номер идёт через SetCreditsDetailed избежал, поведение асимметрично).
func TestModelCooldownsSurviveRevive(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.CooldownSoftForModel("u1", 600*time.Second, time.Now().Add(time.Hour), "glm-5.3", "6004")
	p.ReenableIfCredits("u1", 500, 0)
	p.mu.RLock()
	mc, ok := p.byUID["u1"].modelCooldowns["glm-5.3"]
	p.mu.RUnlock()
	if !ok {
		t.Fatal("revive После modelCooldowns[glm-5.3] следует сохранить — восстановление баланса не является доказательством снятия лимита")
	}
	if rem := time.Until(mc.Until); rem < 55*time.Minute || rem > time.Hour+time.Minute {
		t.Errorf("6004 until Должен сохранять ~1h без изменений, got remaining=%v", rem)
	}
}

// TestModelCooldownsLazyCleanup Просроченный кулдаун модели в pick（пути write-lock) лениво очищается.
func TestModelCooldownsLazyCleanup(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.mu.Lock()
	e := p.byUID["u1"]
	e.modelCooldowns = map[string]modelCooldown{
		"old": {Until: time.Now().Add(-time.Minute), ResetAt: time.Now().Add(-time.Minute)},
	}
	p.mu.Unlock()
	got := p.PickExcludingForModel(nil, "fresh")
	if got == nil || got.UID != "u1" {
		t.Fatalf("cooldown просроченной модели не должен блокировать u1, got %+v", got)
	}
	p.mu.RLock()
	_, still := e.modelCooldowns["old"]
	p.mu.RUnlock()
	if still {
		t.Error("просроченные записи охлаждения модели должны pick очищается при")
	}
}

// TestModelCooldownsExpiredAllowsSameModel после истечения запросы той же модели тоже пропускаются (read путь выбираем даже без очистки).
func TestModelCooldownsExpiredAllowsSameModel(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.mu.Lock()
	e := p.byUID["u1"]
	e.modelCooldowns = map[string]modelCooldown{
		"glm-5.3": {Until: time.Now().Add(-time.Nanosecond), ResetAt: time.Now().Add(-time.Nanosecond)},
	}
	p.mu.Unlock()
	if !e.healthyForModel(time.Now(), "glm-5.3") {
		t.Fatal("после истечения запросы той же модели должны пропускаться")
	}
}

// TestRateLimitedModelsMultiModel 6004 Одновременный рейт-лимит нескольких моделей → /status Показать весь реестр.
func TestRateLimitedModelsMultiModel(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	resetA := time.Now().Add(2 * time.Hour)
	resetB := time.Now().Add(1 * time.Hour)
	p.mu.Lock()
	e := p.byUID["u1"]
	e.modelCooldowns = map[string]modelCooldown{
		"glm-5.3": {Until: resetA, ResetAt: resetA, Reason: "6004 model rate limit"},
		"hy3-x": {Until: resetB, ResetAt: resetB, Reason: "6004 model rate limit"},
	}
	p.mu.Unlock()

	st, ok := p.Status("u1")
	if !ok {
		t.Fatal("status missing")
	}
	if len(st.RateLimitedModels) != 2 {
		t.Fatalf("rate_limited_models=%+v want 2 Строка", st.RateLimitedModels)
	}
	wantModels := map[string]bool{"glm-5.3": true, "hy3-x": true}
	for _, row := range st.RateLimitedModels {
		if !wantModels[row.Model] {
			t.Errorf("unexpected row model=%q", row.Model)
		}
		if row.Kind != "rate_limit" {
			t.Errorf("%s kind=%q want rate_limit", row.Model, row.Kind)
		}
		delete(wantModels, row.Model)
	}
	if len(wantModels) != 0 {
		t.Errorf("Отсутствует строка: %v", wantModels)
	}
}

// без времени сброса 6004 только запись AuditOnly Журнал: видно на странице аккаунта, но не меняет маршрутизацию модели.
func TestModelRateLimitAuditDoesNotAffectRouting(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.CooldownSoftRate("u1", time.Minute, time.Time{}, "429 rate limit")
	p.RecordModelRateLimitAudit("u1", "glm-5.3", "6004 model rate limit (reset unknown)")

	p.mu.Lock()
	e := p.byUID["u1"]
	mc, ok := e.modelCooldowns["glm-5.3"]
	if ok && mc.AuditOnly {
		mc.Until = time.Now().Add(time.Hour)
		e.modelCooldowns["glm-5.3"] = mc
	}
	p.mu.Unlock()
	if !ok || !mc.AuditOnly {
		t.Fatalf("audit entry missing: %+v ok=%v", mc, ok)
	}
	if e.modelCooled(time.Now(), "glm-5.3") {
		t.Fatal("AuditOnly Учёт не участвует в modelCooled")
	}
	if e.modelExempt() {
		t.Fatal("Только AuditOnly Реестр не должен переводить аккаунт в состояние исключения по модели")
	}

	st, _ := p.Status("u1")
	if len(st.RateLimitedModels) != 1 || st.RateLimitedModels[0].Kind != "rate_limit" {
		t.Fatalf("audit status = %+v, want one rate_limit row", st.RateLimitedModels)
	}
}

// 11102 и 6004 Общий реестр, но вывод должен различаться kind。
func TestRateLimitedModelKindModelUnavailable(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.BlockModelBackoff("u1", "missing-model", "11102 model unavailable")
	st, _ := p.Status("u1")
	if len(st.RateLimitedModels) != 1 || st.RateLimitedModels[0].Kind != "model_unavailable" {
		t.Fatalf("rows=%+v want model_unavailable", st.RateLimitedModels)
	}
}

// AuditOnly метка должна сохраняться между рестартами, иначе у 6004 отображаемые элементы будут забыты и участвуют в маршрутизации.
func TestModelRateLimitAuditPersists(t *testing.T) {
	dir := t.TempDir()
	fp := dir + "/state.json"
	p := New(fp)
	p.Add(&auth.Auth{UID: "u1"})
	p.CooldownSoftRate("u1", time.Minute, time.Time{}, "429 rate limit")
	p.RecordModelRateLimitAudit("u1", "glm-5.3", "6004 model rate limit (reset unknown)")
	p.Flush()

	p2 := New(fp)
	p2.Add(&auth.Auth{UID: "u1"})
	p2.mu.RLock()
	mc, ok := p2.byUID["u1"].modelCooldowns["glm-5.3"]
	p2.mu.RUnlock()
	if !ok || !mc.AuditOnly {
		t.Fatalf("audit entry not restored: %+v ok=%v", mc, ok)
	}
	if p2.byUID["u1"].modelExempt() {
		t.Fatal("restored AuditOnly entry must not create model exemption")
	}
}

// TestRateLimitedModelsMultiModelStableOutput Строки реестра многомоделей сортируются по имени модели (стабильный вывод).
func TestRateLimitedModelsMultiModelStableOutput(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	resetA := time.Now().Add(2 * time.Hour)
	resetB := time.Now().Add(1 * time.Hour)
	p.mu.Lock()
	e := p.byUID["u1"]
	e.modelCooldowns = map[string]modelCooldown{
		"hy3-x": {Until: resetB, ResetAt: resetB, Reason: "r"},
		"glm-5.3": {Until: resetA, ResetAt: resetA, Reason: "r"},
	}
	p.mu.Unlock()
	st, _ := p.Status("u1")
	got := []string{st.RateLimitedModels[0].Model, st.RateLimitedModels[1].Model}
	want := []string{"glm-5.3", "hy3-x"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("rows=%v want sorted %v", got, want)
	}
}

// TestRateLimitedModelsEachModelHasOwnUntil Строка реестра Until = индивидуальный кулдаун этой модели до,
// Status.Until（уровня аккаунта) не затрагивается 6004 влияние (ноль при отсутствии кулдауна на уровне аккаунта).
func TestRateLimitedModelsEachModelHasOwnUntil(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	resetA := time.Now().Add(2 * time.Hour)
	resetB := time.Now().Add(1 * time.Hour)
	p.mu.Lock()
	e := p.byUID["u1"]
	e.modelCooldowns = map[string]modelCooldown{
		"glm-5.3": {Until: resetA, ResetAt: resetA, Reason: "r"},
		"hy3-x": {Until: resetB, ResetAt: resetB, Reason: "r"},
	}
	p.mu.Unlock()
	st, ok := p.Status("u1")
	if !ok {
		t.Fatal("status missing")
	}
	if !st.Until.IsZero() {
		t.Fatalf("Status.Until Должно быть нулевое значение (без охлаждения на уровне аккаунта),got %v", st.Until)
	}
	for _, row := range st.RateLimitedModels {
		want := resetA
		if row.Model == "hy3-x" {
			want = resetB
		}
		if d := row.Until.Sub(want); d < -time.Second || d > time.Second {
			t.Errorf("%s row.Until=%v want ~%v", row.Model, row.Until, want)
		}
	}
}

// TestModelCooldownsPickSkipsLimitedModel моделью X 6004 аккаунта, запрос X выбирается другой аккаунт,
// при запросе других моделей можно выбрать этот номер (модель входит по exemption normal выбор номера).
func TestModelCooldownsPickSkipsLimitedModel(t *testing.T) {
	withNoPickGap(t)
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.Add(&auth.Auth{UID: "u2"})
	p.SetCredits("u1", 1000, 0)
	p.SetCredits("u2", 1, 0)
	p.SetRandomSource(func(n int64) int64 { return 0 })
	p.CooldownSoftForModel("u1", time.Minute, time.Now().Add(5*time.Minute), "glm-5.3", "6004")
	if got := p.PickExcludingForModel(nil, "glm-5.3"); got == nil || got.UID != "u2" {
		t.Fatalf("glm-5.3 запрос должен пропускаться u1, got %+v", got)
	}
	if got := p.PickExcludingForModel(nil, "hy3-x"); got == nil || got.UID != "u1" {
		t.Fatalf("hy3-x Запрос должен быть исключен u1, got %+v", got)
	}
}

// TestServableNowModelCooldownStillServable Одна модель 6004 троттлинг не ломает health-check (пул всё ещё обслуживает),
// Глобальный кулдаун аккаунта (until）то обслуживание невозможно.
func TestServableNowModelCooldownStillServable(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.CooldownSoftForModel("u1", time.Minute, time.Now().Add(5*time.Minute), "glm-5.3", "6004")
	if !p.ServableNow() {
		t.Fatal("6004 Кулдаун модели не блокирует аккаунт,ServableNow должен true")
	}
}

// ---------------------------------------------------------------------------
// healthyForModel Приоритет (сначала уровень всего аккаунта, уровень модели 6004 после проверки)
// ---------------------------------------------------------------------------

// TestHealthyForModelAccountCooledBeatsModelNotCooled Глобальный кулдаун аккаунта (until）Приоритет над моделью
// Независимый кулдаун: уровень аккаунта until неистёкший аккаунт, даже если у этой модели нет 6004 Отдельный кулдаун также недоступен для выбора
// （старая реализация сначала проверяет modelCooled повторно запросить healthy，Логически эквивалентно, точка short-circuit другая; зафиксировать новую семантику:
// приоритет глобального кулдауна аккаунта).
func TestHealthyForModelAccountCooledBeatsModelNotCooled(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.Cooldown("u1", CoolSoft, time.Hour, "429 rate limit") // Уровень всех аккаунтов until кулдаун, без modelCooldowns
	p.mu.RLock()
	e := p.byUID["u1"]
	p.mu.RUnlock()

	if e.healthyForModel(time.Now(), "glm-5.3") {
		t.Fatal("Все аккаунты until В кулдауне и без отдельного кулдауна модели: модель также недоступна (приоритет общего кулдауна аккаунта)")
	}
	if e.healthyForModel(time.Now(), "") {
		t.Fatal("пустое имя модели также нельзя выбрать (эквивалентно healthy закоротить на кулдаун всех аккаунтов)")
	}
}

// TestHealthyForModelAccountCooledWithModelCooldownStillBlocked кулдаун всех аккаунтов + данная модель
// также есть 6004 независимый кулдаун → невыбираемый (любой перехват одинаков, short-circuit по приоритету не пропустит ошибочно).
func TestHealthyForModelAccountCooledWithModelCooldownStillBlocked(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.Cooldown("u1", CoolSoft, time.Hour, "429 rate limit")
	p.CooldownSoftForModel("u1", time.Minute, time.Now().Add(5*time.Minute), "glm-5.3", "6004")
	p.mu.RLock()
	e := p.byUID["u1"]
	p.mu.RUnlock()
	if e.healthyForModel(time.Now(), "glm-5.3") {
		t.Fatal("кулдаун всех аккаунтов + Независимый кулдаун модели, двойная блокировка — всё равно недоступна для выбора")
	}
}

// TestHealthyForModelDisabledBeatsModelNotCooled disabled самый сильный cooldown на уровне всего аккаунта:
// Даже без индивидуального cooldown модель никогда не выбирается (disabled > уровень модели).
func TestHealthyForModelDisabledBeatsModelNotCooled(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.Disable("u1", "session dead")
	p.mu.RLock()
	e := p.byUID["u1"]
	p.mu.RUnlock()
	if e.healthyForModel(time.Now(), "glm-5.3") {
		t.Fatal("disabled Аккаунт недоступен для выбора даже без отдельного кулдауна модели")
	}
}

// TestHealthyForModelBreakerBeatsModelNotCooled Circuit breaker (breakerUntil）это на уровне всего аккаунта
// Кулдаун: в период circuit-break модель недоступна для выбора даже без индивид. кулдауна.
func TestHealthyForModelBreakerBeatsModelNotCooled(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.SetBreaker(1, time.Hour, time.Hour)
	p.NoteError("u1") // срабатывание circuit breaker
	p.mu.RLock()
	e := p.byUID["u1"]
	bt := e.breakerUntil
	p.mu.RUnlock()
	if bt.IsZero() {
		t.Fatal("precondition: breaker should be open")
	}
	if e.healthyForModel(time.Now(), "glm-5.3") {
		t.Fatal("В периодОтключение модель недоступна для выбора даже без индивидуального кулдауна")
	}
}

// TestHealthyForModelHealthyAccountNoModelCooldown Здоровье всех аккаунтов + Нет индивидуального кулдауна модели →
// Все модели доступны для выбора (базовая линия).
func TestHealthyForModelHealthyAccountNoModelCooldown(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.mu.RLock()
	e := p.byUID["u1"]
	p.mu.RUnlock()
	for _, m := range []string{"glm-5.3", "hy3-x", ""} {
		if !e.healthyForModel(time.Now(), m) {
			t.Errorf("здоровый аккаунт для модели %q Должен быть опциональным", m)
		}
	}
}

// TestHealthyForModelAccountCooledAllowsNothingAfterUntil другой конец дедлока приоритетов:
// Все аккаунты until после истечения охлаждения запросы без индивидуального охлаждения модели восстанавливаются (перехват решения на уровне модели).
func TestHealthyForModelAccountCooledAllowsNothingAfterUntil(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.Cooldown("u1", CoolSoft, time.Millisecond, "429")
	p.mu.RLock()
	e := p.byUID["u1"]
	p.mu.RUnlock()
	// until ещё не истёк: недоступен для выбора (приоритет: сначала проверка на уровне всего аккаунта).
	if e.healthyForModel(time.Now(), "glm-5.3") {
		t.Fatal("until Недоступно в период охлаждения")
	}
	// и т.д. until Истечение: у модели нет отдельного кулдауна → восстановление опционально.
	time.Sleep(10 * time.Millisecond)
	if !e.healthyForModel(time.Now(), "glm-5.3") {
		t.Fatal("until После истечения должен стать снова доступным")
	}
}

// TestHealthyForModelPriorityViaPick End-to-end: аккаунт в глобальном кулдауне даже без индивидуального кулдауна по модели
// также не выбирается; health + Только эта модель 6004 аккаунты идут по исключению модели (один аккаунт для разных моделей учитывается раздельно).
func TestHealthyForModelPriorityViaPick(t *testing.T) {
	withNoPickGap(t)
	p := New("")
	p.Add(&auth.Auth{UID: "cooled"})
	p.Add(&auth.Auth{UID: "exempt"})
	p.SetCredits("cooled", 100, 0)
	p.SetCredits("exempt", 50, 0)
	p.SetRandomSource(func(n int64) int64 { return 0 }) // r=0 → Максимальный балл cooled
	p.Cooldown("cooled", CoolSoft, time.Hour, "429") // Cooldown на уровне всего аккаунта, без записи на уровне модели
	p.CooldownSoftForModel("exempt", time.Minute, time.Now().Add(5*time.Minute), "glm-5.3", "6004")

	// Запрос other：cooled Перехват глобальным кулдауном аккаунтов (даже без независимого кулдауна модели),exempt исключение модели
	// （6004 Лочить только glm-5.3）→ Единственный кандидат exempt。
	if got := p.PickExcludingForModel(nil, "other"); got == nil || got.UID != "exempt" {
		t.Fatalf("other Запрос модели должен быть исключен exempt（глобального кулдауна всех аккаунтов cooled все равно блокируется),got %+v", got)
	}
	// Запрос glm-5.3：cooled блокировка кулдауна на весь аккаунт;exempt собственный 6004 Перехват → Нет здоровых кандидатов →
	// Глобальный фолбэк кулдауна учитывает только кулдаун уровня аккаунта (exempt отсутствует until/breakerUntil,expiry нулевые значения исключены),
	// Выбрать cooled（мягкий кулдаун участвует в fallback).
	if got := p.PickExcludingForModel(nil, "glm-5.3"); got == nil || got.UID != "cooled" {
		t.Fatalf("glm-5.3 Запрос:exempt самим собой 6004 перехват, фолбэк — охлаждение всех аккаунтов cooled，got %+v", got)
	}
}

// TestModelCooldownsNotPersisted modelCooldowns Состояние выполнения, без персистентности (сброс при перезапуске).
func TestModelCooldownsPersist(t *testing.T) {
	// 6004 сброс wall-clock может длиться часы, переживает рестарт — норма:model_cooldowns Персистентность,
	// После восстановления healthyForModel без потери памяти (поглощение апстрима 2f4c77b）。
	dir := t.TempDir()
	fp := dir + "/state.json"
	p := New(fp)
	p.Add(&auth.Auth{UID: "u1"})
	p.CooldownSoftForModel("u1", time.Minute, time.Now().Add(5*time.Minute), "glm-5.3", "6004")
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
	p2.mu.RLock()
	n := len(p2.byUID["u1"].modelCooldowns)
	p2.mu.RUnlock()
	if n != 1 {
		t.Errorf("после перезагрузки modelCooldowns=%d want 1（восстановление из персистентности)", n)
	}
}

// TestModelCooldownsPersistCompatOldState старый state.json отсутствует modelCooldowns Поля загружены корректно
// （отсутствие поля = 0, кулдаун модели сбрасывается до уровня аккаунта, обратная совместимость).
func TestModelCooldownsPersistCompatOldState(t *testing.T) {
	dir := t.TempDir()
	fp := dir + "/state.json"
	old := `{"accounts":{"u1":{"credits":100,"until":"2099-01-01T00:00:00Z","cool_kind":1}}}`
	if err := os.WriteFile(fp, []byte(old), 0o600); err != nil {
		t.Fatal(err)
	}
	p := New(fp)
	p.Add(&auth.Auth{UID: "u1"})
	p.mu.RLock()
	n := len(p.byUID["u1"].modelCooldowns)
	until := p.byUID["u1"].until
	p.mu.RUnlock()
	if n != 0 {
		t.Errorf("После загрузки старого файла modelCooldowns=%d want 0", n)
	}
	if until.IsZero() {
		t.Error("Старый файл until должен загружаться как обычно (совместимость с кулдауном на уровне аккаунта)")
	}
}
