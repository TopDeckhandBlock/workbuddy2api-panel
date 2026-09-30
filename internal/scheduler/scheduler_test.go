package scheduler

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/pool"
	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
)

func TestNextFire(t *testing.T) {
	loc := time.Local
	now := time.Date(2026, 7, 27, 10, 0, 0, 0, loc)
	next := nextFire(now, []int{9, 21})
	if next.Hour() != 21 || next.Day() != 27 {
		t.Errorf("next=%v want 21:00 same day", next)
	}
	now = time.Date(2026, 7, 27, 22, 0, 0, 0, loc)
	next = nextFire(now, []int{9, 21})
	if next.Hour() != 9 || next.Day() != 28 {
		t.Errorf("next=%v want 09:00 next day", next)
	}
	now = time.Date(2026, 7, 27, 9, 0, 0, 0, loc)
	next = nextFire(now, []int{9})
	if next.Day() != 28 {
		t.Errorf("exact match should roll to next day: %v", next)
	}
}

func TestNextFireMergesSchedules(t *testing.T) {
	now := time.Date(2026, 7, 27, 20, 0, 0, 0, time.Local)
	next := nextFire(now, []int{9, 21, 22})
	if next.Hour() != 21 {
		t.Errorf("next=%v want 21 (earliest of 21/22)", next)
	}
}

// TestNextWakeKeepaliveOnly если время check-in прошло — пробуждение по keepalive ровно в час.
func TestNextWakeKeepaliveOnly(t *testing.T) {
	s := New(Config{CheckinHours: []int{9}, KeepaliveHours: []int{22},
		TravelDisabled: true, ActivityDisabled: true})
	at, kinds := s.nextWake(time.Date(2026, 9, 11, 20, 0, 0, 0, time.Local))
	if want := time.Date(2026, 9, 11, 22, 0, 0, 0, time.Local); !at.Equal(want) {
		t.Errorf("next=%v want %v", at, want)
	}
	if len(kinds) != 1 || kinds[0] != taskKeepalive {
		t.Errorf("kinds=%v want [keepalive]", kinds)
	}
}

// TestNextWakeSameInstantFiresAll когда check-in и keep-alive назначены на один и тот же час, выполняются оба типа задач.
func TestNextWakeSameInstantFiresAll(t *testing.T) {
	s := New(Config{
		CheckinHours: []int{9, 22},
		TravelHours: []int{}, // Отключить влияние travel-моментов (тест только чекина+Keep-alive совпадает с ровным часом)
		ActivityHours: []int{}, // помеха от точки активности блокировки
		KeepaliveHours: []int{22},
		TravelDisabled: true,
		ActivityDisabled: true,
		BlackcatDisabled: true,
	})
	at, kinds := s.nextWake(time.Date(2026, 9, 11, 21, 30, 0, 0, time.Local))
	if want := time.Date(2026, 9, 11, 22, 0, 0, 0, time.Local); !at.Equal(want) {
		t.Errorf("next=%v want %v", at, want)
	}
	if !hasKind(kinds, taskCheckin) || !hasKind(kinds, taskKeepalive) {
		t.Errorf("kinds=%v want checkin+keepalive（две задачи одновременно)", kinds)
	}

	// 22 После наступления времени следующий раз — на следующий день 09:00，и содержит только check-in (travel/активный уже отключен).
	at, kinds = s.nextWake(time.Date(2026, 9, 11, 22, 30, 0, 0, time.Local))
	if want := time.Date(2026, 9, 12, 9, 0, 0, 0, time.Local); !at.Equal(want) {
		t.Errorf("next=%v want %v", at, want)
	}
	if len(kinds) != 1 || kinds[0] != taskCheckin {
		t.Errorf("kinds=%v want [checkin]", kinds)
	}
}

// TestNextWakeNothingScheduled При пустоте обоих типов задач вернуть нулевое значение,Run Ожидает только сигнал выхода.
func TestNextWakeNothingScheduled(t *testing.T) {
	s := &Scheduler{cfg: Config{}}
	at, kinds := s.nextWake(time.Now())
	if !at.IsZero() || len(kinds) != 0 {
		t.Errorf("at=%v kinds=%v want zero/nil", at, kinds)
	}
}

// TestNextWakeCheckinDisabled После явного отключения чекина в расписании больше нет точек чекина (keep-alive как обычно).
func TestNextWakeCheckinDisabled(t *testing.T) {
	s := New(Config{CheckinDisabled: true, CheckinHours: []int{9, 21}, KeepaliveHours: []int{22},
		TravelDisabled: true, ActivityDisabled: true})
	at, kinds := s.nextWake(time.Date(2026, 9, 11, 20, 0, 0, 0, time.Local))
	if want := time.Date(2026, 9, 11, 22, 0, 0, 0, time.Local); !at.Equal(want) {
		t.Errorf("next=%v want %v（больше не должно быть 21 точечный чекин)", at, want)
	}
	if len(kinds) != 1 || kinds[0] != taskKeepalive {
		t.Errorf("kinds=%v want [keepalive]", kinds)
	}
}

// TestNextWakeKeepaliveDisabled После явного отключения keep-alive в расписании больше нет точек keep-alive (отметка как обычно).
func TestNextWakeKeepaliveDisabled(t *testing.T) {
	s := New(Config{KeepaliveDisabled: true, CheckinHours: []int{9, 21}, KeepaliveHours: []int{22},
		TravelDisabled: true, ActivityDisabled: true})
	at, kinds := s.nextWake(time.Date(2026, 9, 11, 20, 0, 0, 0, time.Local))
	if want := time.Date(2026, 9, 11, 21, 0, 0, 0, time.Local); !at.Equal(want) {
		t.Errorf("next=%v want %v（больше не должно быть 22 точечный keepalive)", at, want)
	}
	if len(kinds) != 1 || kinds[0] != taskCheckin {
		t.Errorf("kinds=%v want [checkin]", kinds)
	}
}

// TestNextWakeBothDisabledNothingScheduled Все пять типов задач явно отключены → Нет точек пробуждения.
func TestNextWakeBothDisabledNothingScheduled(t *testing.T) {
	s := New(Config{
		CheckinDisabled: true,
		TravelDisabled: true,
		ActivityDisabled: true,
		KeepaliveDisabled: true,
		BlackcatDisabled: true,
		CheckinHours: []int{9, 21},
		KeepaliveHours: []int{22},
	})
	at, kinds := s.nextWake(time.Now())
	if !at.IsZero() || len(kinds) != 0 {
		t.Errorf("at=%v kinds=%v want zero/nil", at, kinds)
	}
}

// TestRunAllDisabledNoSpinNoCalls все четыре типа задач отключены:Run Без холостого хода (только ожидание сигнала выхода),
// И не должен вызывать никаких апстрим-запросов.
func TestRunAllDisabledNoSpinNoCalls(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		http.Error(w, "no upstream call expected", 404)
	}))
	defer srv.Close()

	p := pool.New("")
	p.Add(&auth.Auth{UID: "u1", AccessToken: "at", RefreshToken: "rt", ExpiresAt: 9999999999})
	up := &upstream.Client{
		HTTP: srv.Client(),
		ChatBaseCN: srv.URL,
		BillingBaseCN: srv.URL,
	}
	s := New(Config{
		Pool: p,
		Upstream: up,
		CheckinDisabled: true,
		TravelDisabled: true,
		ActivityDisabled: true,
		KeepaliveDisabled: true,
	})

	ctx, cancel := context.WithTimeout(context.Background(), 250*time.Millisecond)
	defer cancel()
	start := time.Now()
	s.Run(ctx) // блокировка до ctx до отмены (нет момента для ожидания, не создавать timer）
	elapsed := time.Since(start)

	if calls.Load() != 0 {
		t.Errorf("upstream calls=%d want 0（все четыре типа отключены)", calls.Load())
	}
	if elapsed < 200*time.Millisecond {
		t.Errorf("Run returned after %v, before ctx done（не должен возвращаться досрочно)", elapsed)
	}
	if elapsed > 2*time.Second {
		t.Errorf("Run took %v（Не должен простаивать/активное ожидание)", elapsed)
	}
}

func hasKind(kinds []taskKind, k taskKind) bool {
	for _, v := range kinds {
		if v == k {
			return true
		}
	}
	return false
}

// fakeUpstream Одновременно симулировать billing и refresh。
type fakeUpstream struct {
	checkinCalls atomic.Int32
	refreshCalls atomic.Int32
	resourceRemain int64
	resourceEnd string
}

func (f *fakeUpstream) server() *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/daily-checkin"):
			f.checkinCalls.Add(1)
			w.Write([]byte(`{"code":0,"msg":"ok","data":{}}`))
		case strings.HasSuffix(r.URL.Path, "/get-user-resource"):
			// remain Требуется <= size（Захват данных [0,size]：грязные данные remain>size Будет зажато до size
			// ——реальные данные апстрима всегда консистентны, фактически Cycle{17,482,500}）。
			end := ""
			if f.resourceEnd != "" {
				end = `,"CycleEndTime":` + jsonString(f.resourceEnd)
			}
			w.Write([]byte(`{"code":0,"data":{"Response":{"Data":{"Accounts":[{"CycleCapacitySize":1000,"CycleCapacityRemain":` +
				jsonI64(f.resourceRemain) + `,"CycleCapacityUsed":0` + end + `}]}}}}`))
		case strings.HasSuffix(r.URL.Path, "/token/refresh"):
			f.refreshCalls.Add(1)
			w.Write([]byte(`{"code":0,"data":{"accessToken":"new","expiresIn":3600}}`))
		default:
			http.Error(w, "not found", 404)
		}
	}))
}

func jsonI64(v int64) string {
	b, _ := json.Marshal(v)
	return string(b)
}

func jsonString(v string) string {
	b, _ := json.Marshal(v)
	return string(b)
}

func TestRunCheckinReenablesCoolingAccount(t *testing.T) {
	f := &fakeUpstream{resourceRemain: 500}
	srv := f.server()
	defer srv.Close()

	p := pool.New("")
	a := &auth.Auth{UID: "u1", AccessToken: "at", RefreshToken: "rt", ExpiresAt: 9999999999}
	p.Add(a)
	p.Cooldown("u1", pool.CoolHard, time.Hour, "Недостаточно средств")

	up := &upstream.Client{
		HTTP: srv.Client(),
		ChatBaseCN: srv.URL,
		BillingBaseCN: srv.URL,
	}
	s := New(Config{
		Pool: p,
		Upstream: up,
		CheckinHours: []int{9, 21},
		KeepaliveHours: []int{22},
	})
	s.RunCheckinNow()
	if f.checkinCalls.Load() != 1 {
		t.Errorf("checkin calls=%d", f.checkinCalls.Load())
	}
	st, _ := p.Status("u1")
	if st.Cooling {
		t.Errorf("account should be reenabled after checkin with credits: %+v", st)
	}
	if st.Credits != 500 {
		t.Errorf("credits=%d want 500", st.Credits)
	}
}

func TestRunKeepaliveRefreshesTokens(t *testing.T) {
	f := &fakeUpstream{}
	srv := f.server()
	defer srv.Close()

	p := pool.New("")
	a := &auth.Auth{UID: "u1", AccessToken: "old", RefreshToken: "rt", ExpiresAt: 1}
	p.Add(a)

	up := &upstream.Client{
		HTTP: srv.Client(),
		ChatBaseCN: srv.URL,
		BillingBaseCN: srv.URL,
	}
	s := New(Config{Pool: p, Upstream: up})
	s.RunKeepaliveNow()
	if f.refreshCalls.Load() != 1 {
		t.Errorf("refresh calls=%d", f.refreshCalls.Load())
	}
	if a.AccessToken != "new" {
		t.Errorf("token not updated: %s", a.AccessToken)
	}
}

func TestRunKeepaliveSessionDeadDisables(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(401)
		w.Write([]byte(`{"code":12153,"msg":"Offline user session not found"}`))
	}))
	defer srv.Close()

	p := pool.New("")
	a := &auth.Auth{UID: "u1", AccessToken: "old", RefreshToken: "rt", ExpiresAt: 1}
	p.Add(a)

	up := &upstream.Client{
		HTTP: srv.Client(),
		ChatBaseCN: srv.URL,
		BillingBaseCN: srv.URL,
	}
	s := New(Config{Pool: p, Upstream: up})
	// P0-1：12153 непрерывно N раз до отключения. До 2 раз неудач обновления не должны банить аккаунт (защита от ложных срабатываний).
	s.RunKeepaliveNow()
	if st, _ := p.Status("u1"); st.Disabled {
		t.Fatalf("№ 1 Раз 12153 Не следует отключать: %+v", st)
	}
	s.RunKeepaliveNow()
	if st, _ := p.Status("u1"); st.Disabled {
		t.Fatalf("№ 2 Раз 12153 Не следует отключать: %+v", st)
	}
	// № 3 раз подряд 12153 → Отключено.
	s.RunKeepaliveNow()
	st, _ := p.Status("u1")
	if !st.Disabled {
		t.Errorf("№ 3 раз подряд 12153 следует отключить: %+v", st)
	}
	if st.DisabledReason != "12153 session dead" {
		t.Errorf("disabled_reason=%q want 12153 session dead", st.DisabledReason)
	}
}

// TestRunKeepaliveSessionDeadResetBySuccess два раза 12153 после — обновление успешно → сброс счетчика,
// Повторный 12153 с 1 раз — пересчёт заново (без преследования за исторические ошибки).
func TestRunKeepaliveSessionDeadResetBySuccess(t *testing.T) {
	var fails atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if fails.Add(1) == 3 { // № 3 раз (второй раунд текущего цикла диспетчеризации) обновление успешно
			w.Write([]byte(`{"code":0,"data":{"accessToken":"new","expiresIn":3600}}`))
			return
		}
		w.WriteHeader(401)
		w.Write([]byte(`{"code":12153,"msg":"Offline user session not found"}`))
	}))
	defer srv.Close()

	p := pool.New("")
	a := &auth.Auth{UID: "u1", AccessToken: "old", RefreshToken: "rt", ExpiresAt: 1}
	p.Add(a)

	up := &upstream.Client{
		HTTP: srv.Client(),
		ChatBaseCN: srv.URL,
		BillingBaseCN: srv.URL,
	}
	s := New(Config{Pool: p, Upstream: up})
	s.RunKeepaliveNow() // 12153 #1
	s.RunKeepaliveNow() // 12153 #2
	if st, _ := p.Status("u1"); st.Disabled {
		t.Fatalf("precondition: Перед 2 раз не следует отключать: %+v", st)
	}
	s.RunKeepaliveNow() // Обновление успешно → сброс счетчика
	// Далее подряд 2 Раз 12153：Пересчёт с нуля, всё равно не блокировать (история счётчиков очищена).
	s.RunKeepaliveNow() // 12153 #1（новый счётчик)
	s.RunKeepaliveNow() // 12153 #2（новый счётчик)
	if st, _ := p.Status("u1"); st.Disabled {
		t.Fatalf("после успешного сброса счётчика обновления подряд 2 Раз 12153 Не следует отключать: %+v", st)
	}
	s.RunKeepaliveNow() // 12153 #3（новый счётчик)→ Отключено
	if st, _ := p.Status("u1"); !st.Disabled {
		t.Fatalf("новый счетчик № 3 Раз 12153 следует отключить: %+v", st)
	}
}

func TestCheckinErrorDoesNotCrash(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(500)
		w.Write([]byte(`boom`))
	}))
	defer srv.Close()

	p := pool.New("")
	p.Add(&auth.Auth{UID: "u1", AccessToken: "at", RefreshToken: "rt", ExpiresAt: 9999999999})
	up := &upstream.Client{
		HTTP: srv.Client(),
		ChatBaseCN: srv.URL,
		BillingBaseCN: srv.URL,
	}
	s := New(Config{Pool: p, Upstream: up})
	// Не должен panic
	s.RunCheckinNow()
	s.RunKeepaliveNow()
	_ = errors.New("unused")
}

// TestRunBalanceRefreshNowUpdatesCreditsAndRevives только запрос баланса (без check-in) для обновления credits
// и разморозить аккаунты на кулдауне с восстановленным балансом — ручное обновление панели и фоновая периодическая задача используют одну семантику.
func TestRunBalanceRefreshNowUpdatesCreditsAndRevives(t *testing.T) {
	f := &fakeUpstream{resourceRemain: 777}
	srv := f.server()
	defer srv.Close()

	p := pool.New("")
	p.Add(&auth.Auth{UID: "u1", AccessToken: "at", RefreshToken: "rt", ExpiresAt: 9999999999})
	p.Add(&auth.Auth{UID: "u2", AccessToken: "at", RefreshToken: "rt", ExpiresAt: 9999999999})
	p.Cooldown("u1", pool.CoolHard, time.Hour, "Недостаточно средств")
	p.Disable("u2", "manual")

	up := &upstream.Client{HTTP: srv.Client(), ChatBaseCN: srv.URL, BillingBaseCN: srv.URL}
	s := New(Config{Pool: p, Upstream: up})
	s.RunBalanceRefreshNow()

	st, _ := p.Status("u1")
	if st.Cooling || st.Credits != 777 {
		t.Errorf("u1 want revived with credits=777: cooling=%v credits=%d", st.Cooling, st.Credits)
	}
	if f.checkinCalls.Load() != 0 {
		t.Errorf("balance refresh must not checkin, got %d calls", f.checkinCalls.Load())
	}
	// Отключённые аккаунты не участвуют: их credits Сохранить 0（Не был UserResource перезапись разморозки).
	if st2, _ := p.Status("u2"); !st2.Disabled {
		t.Errorf("u2 must stay disabled")
	}
}

// TestNextWakeGrowthSlot growth планирование в кандидаты + Отключить выход (ежедневное авто-выполнение очереди заданий роста).
func TestNextWakeGrowthSlot(t *testing.T) {
	s := New(Config{GrowthHours: []int{1}})
	at, kinds := s.nextWake(time.Date(2026, 9, 27, 0, 10, 0, 0, time.Local))
	hasGrowth := false
	for _, k := range kinds {
		if k == taskGrowth {
			hasGrowth = true
		}
	}
	if !hasGrowth || at.Hour() != 1 || at.Day() != 27 {
		t.Fatalf("growth Слот: at=%v kinds=%v（Ожидается 09-27 01:00 Содержит taskGrowth）", at, kinds)
	}
	// После отключения не попадает в кандидаты (остальные kind пусто → nextWake возврат нулевого значения)
	s2 := New(Config{GrowthHours: []int{1}, GrowthDisabled: true})
	_, kinds2 := s2.nextWake(time.Date(2026, 9, 27, 0, 10, 0, 0, time.Local))
	for _, k := range kinds2 {
		if k == taskGrowth {
			t.Fatal("После отключения growth Все еще в кандидатах")
		}
	}
}

func TestRunBalanceRefreshReplacesExpirySnapshot(t *testing.T) {
	end := time.Now().In(time.FixedZone("CST", 8*3600)).Add(24 * time.Hour).Format("2006-01-02 15:04:05")
	f := &fakeUpstream{resourceRemain: 100, resourceEnd: end}
	srv := f.server()
	defer srv.Close()

	p := pool.New("")
	p.Add(&auth.Auth{UID: "u1", AccessToken: "at", RefreshToken: "rt", ExpiresAt: 9999999999})
	p.SetCreditsDetailed("u1", 999, 999, 999, time.Now().Add(time.Hour), 999)

	up := &upstream.Client{HTTP: srv.Client(), ChatBaseCN: srv.URL, BillingBaseCN: srv.URL}
	s := New(Config{Pool: p, Upstream: up, ExpiringSoonWindow: 7 * 24 * time.Hour})
	s.RunBalanceRefreshNow()

	st, _ := p.Status("u1")
	if st.Credits != 100 || st.CreditsExpiring != 100 || st.CreditsEarliestRemaining != 100 || st.CreditsEarliestExpiry.IsZero() {
		t.Fatalf("snapshot=%+v", st)
	}

	f.resourceEnd = ""
	s.RunBalanceRefreshNow()
	st, _ = p.Status("u1")
	if st.CreditsExpiring != 0 || st.CreditsEarliestRemaining != 0 || !st.CreditsEarliestExpiry.IsZero() {
		t.Fatalf("zero expiry refresh did not clear snapshot=%+v", st)
	}
}

func TestSetExpiringSoonWindowClearsSnapshot(t *testing.T) {
	p := pool.New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.SetCreditsDetailed("u1", 100, 100, 50, time.Now().Add(time.Hour), 50)
	s := New(Config{Pool: p, ExpiringSoonWindow: 7 * 24 * time.Hour})

	s.SetExpiringSoonWindow(24 * time.Hour)
	if got := s.ExpiringSoonWindow(); got != 24*time.Hour {
		t.Fatalf("window=%v want 24h", got)
	}
	st, _ := p.Status("u1")
	if st.CreditsExpiring != 0 || st.CreditsEarliestRemaining != 0 {
		t.Fatalf("window change did not clear snapshot=%+v", st)
	}
}
