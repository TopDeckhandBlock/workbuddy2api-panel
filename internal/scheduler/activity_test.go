package scheduler

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/pool"
	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
)

// fastActivity отключить rate-limit между аккаунтами при активном репорте, чтобы тесты не ждали зря 800ms。
func fastActivity(t *testing.T) {
	t.Helper()
	old := activityAccountDelay
	activityAccountDelay = 0
	t.Cleanup(func() { activityAccountDelay = old })
}

// reportStub Запись /v2/report Количество вызовов и userId。
type reportStub struct {
	calls atomic.Int32
	uids atomic.Int32
	bodies atomic.Int32
}

func (s *reportStub) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v2/report" {
			s.calls.Add(1)
			uid := r.Header.Get("X-User-Id")
			if uid != "" {
				s.uids.Add(1)
			}
			s.bodies.Add(1) // Отметка о получении body（Assert массив содержит userId В upstream покрытие unit-тестами пакета)
			w.Write([]byte(`{"code":0,"msg":"OK"}`))
			return
		}
		http.Error(w, "not found", 404)
	})
}

// TestRunActivityNowReportsEachAccount перебор каждого доступного аккаунта в пуле с однократным отчетом.
func TestRunActivityNowReportsEachAccount(t *testing.T) {
	fastActivity(t)
	stub := &reportStub{}
	srv := httptest.NewServer(stub.handler())
	defer srv.Close()

	p := pool.New("")
	p.Add(&auth.Auth{UID: "u1", AccessToken: "at", RefreshToken: "rt", ExpiresAt: 9999999999})
	p.Add(&auth.Auth{UID: "u2", AccessToken: "at", RefreshToken: "rt", ExpiresAt: 9999999999})
	up := &upstream.Client{HTTP: srv.Client(), ChatBaseCN: srv.URL, BillingBaseCN: srv.URL}
	s := New(Config{Pool: p, Upstream: up})

	s.RunActivityNow()

	if n := stub.calls.Load(); n != 2 {
		t.Errorf("report calls=%d want 2（отчет по каждому номеру один раз)", n)
	}
	if n := stub.uids.Load(); n != 2 {
		t.Errorf("report with X-User-Id=%d want 2（каждый номер обязательно содержит userId）", n)
	}
}

// TestRunActivityNowSkipsDisabledAndNoToken заблокированный аккаунт и отсутствие token Пропуск аккаунта.
func TestRunActivityNowSkipsDisabledAndNoToken(t *testing.T) {
	fastActivity(t)
	stub := &reportStub{}
	srv := httptest.NewServer(stub.handler())
	defer srv.Close()

	p := pool.New("")
	p.Add(&auth.Auth{UID: "ok", AccessToken: "at", RefreshToken: "rt", ExpiresAt: 9999999999})
	p.Add(&auth.Auth{UID: "dis", AccessToken: "at", RefreshToken: "rt", ExpiresAt: 9999999999})
	p.Add(&auth.Auth{UID: "notoken", AccessToken: "", RefreshToken: "", ExpiresAt: 9999999999})
	p.Disable("dis", "test")
	up := &upstream.Client{HTTP: srv.Client(), ChatBaseCN: srv.URL, BillingBaseCN: srv.URL}
	s := New(Config{Pool: p, Upstream: up})

	s.RunActivityNow()

	if n := stub.calls.Load(); n != 1 {
		t.Errorf("report calls=%d want 1（Только ok аккаунт)", n)
	}
}

// TestRunActivityNowErrorDoesNotAbort Ошибка отчета по одному аккаунту не влияет на дальнейший обход.
func TestRunActivityNowErrorDoesNotAbort(t *testing.T) {
	fastActivity(t)
	var okCalls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v2/report" {
			// streak самопроверка и пр. не- report Запрос сразу возвращает 200（Без счётчика, только подсчёт /v2/report）。
			w.Write([]byte(`{"code":0,"data":{}}`))
			return
		}
		uid := r.Header.Get("X-User-Id")
		if uid == "fail" {
			w.WriteHeader(500)
			w.Write([]byte(`boom`))
			return
		}
		okCalls.Add(1)
		w.Write([]byte(`{"code":0}`))
	}))
	defer srv.Close()

	p := pool.New("")
	p.Add(&auth.Auth{UID: "fail", AccessToken: "at", RefreshToken: "rt", ExpiresAt: 9999999999})
	p.Add(&auth.Auth{UID: "ok", AccessToken: "at", RefreshToken: "rt", ExpiresAt: 9999999999})
	up := &upstream.Client{HTTP: srv.Client(), ChatBaseCN: srv.URL, BillingBaseCN: srv.URL}
	s := New(Config{Pool: p, Upstream: up})

	s.RunActivityNow() // Не должен panic

	if n := okCalls.Load(); n != 1 {
		t.Errorf("ok account report calls=%d want 1（сбойный аккаунт не влияет на дальнейший обход)", n)
	}
}

// ---------------------------------------------------------------------------
// P1：Обратное чтение после активного репорта streak Самопроверка
// ---------------------------------------------------------------------------

// activityStreakStub Эмуляция /v2/report（200 успех)+ /activity/growth/streak（days настраивается).
type activityStreakStub struct {
	reportCalls atomic.Int32
	days int // streak возвращаемое количество дней подряд входа
	streakErr bool // разрешить streak вернуть 500
	noUserId bool // К тесту: отчет без userId（Сервер 200 но тихо отбрасывается)
	streakHits atomic.Int32
}

func (s *activityStreakStub) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v2/report":
			s.reportCalls.Add(1)
			w.Write([]byte(`{"code":0,"msg":"OK"}`))
		case "/activity/growth/streak":
			s.streakHits.Add(1)
			if s.streakErr {
				w.WriteHeader(500)
				w.Write([]byte(`boom`))
				return
			}
			fmt.Fprintf(w, `{"code":0,"data":{"streak":{"days":%d}}}`, s.days)
		default:
			http.Error(w, "not found", 404)
		}
	})
}

// activityStreakScheduler Конструкция с streak Самопроверка stub планировщик.
func activityStreakScheduler(t *testing.T, srv *httptest.Server) (*Scheduler, *pool.Pool) {
	t.Helper()
	p := pool.New("")
	p.Add(&auth.Auth{UID: "u1", AccessToken: "at", RefreshToken: "rt", ExpiresAt: 9999999999})
	up := &upstream.Client{HTTP: srv.Client(), ChatBaseCN: srv.URL, BillingBaseCN: srv.URL}
	return New(Config{Pool: p, Upstream: up}), p
}

// TestRunActivityNowSelfCheckDaysNormal Обратное чтение после успешной отправки streak：days>=1 → без алерта.
func TestRunActivityNowSelfCheckDaysNormal(t *testing.T) {
	fastActivity(t)
	stub := &activityStreakStub{days: 3}
	srv := httptest.NewServer(stub.handler())
	defer srv.Close()

	p := pool.New("")
	p.Add(&auth.Auth{UID: "u1", AccessToken: "at", RefreshToken: "rt", ExpiresAt: 9999999999})
	up := &upstream.Client{HTTP: srv.Client(), ChatBaseCN: srv.URL, BillingBaseCN: srv.URL}
	s := New(Config{Pool: p, Upstream: up})

	s.RunActivityNow()
	if stub.reportCalls.Load() != 1 || stub.streakHits.Load() != 1 {
		t.Errorf("report_calls=%d streak_hits=%d want 1/1", stub.reportCalls.Load(), stub.streakHits.Load())
	}
	// days>=1：checkActivityStreak вернуть false（подозрений нет).
	if s.checkActivityStreak(p.AuthByUID("u1")) {
		t.Fatal("days>=1 Без алерта")
	}
}

// TestRunActivityNowSelfCheckSilentDrop Отчёт 200 Но streak.days=0 → алерт (silent drop?）。
func TestRunActivityNowSelfCheckSilentDrop(t *testing.T) {
	fastActivity(t)
	stub := &activityStreakStub{days: 0}
	srv := httptest.NewServer(stub.handler())
	defer srv.Close()

	p := pool.New("")
	p.Add(&auth.Auth{UID: "u1", AccessToken: "at", RefreshToken: "rt", ExpiresAt: 9999999999})
	up := &upstream.Client{HTTP: srv.Client(), ChatBaseCN: srv.URL, BillingBaseCN: srv.URL}
	s := New(Config{Pool: p, Upstream: up})

	if !s.checkActivityStreak(p.AuthByUID("u1")) {
		t.Fatal("days=0 требуется алерт (отчёт 200 Но silent drop?）")
	}
}

// TestRunActivityNowSelfCheckGETFailure обратное чтение GET ошибка → Предупреждение, но не влияет на основной процесс (отчет успешно отправлен).
func TestRunActivityNowSelfCheckGETFailure(t *testing.T) {
	fastActivity(t)
	stub := &activityStreakStub{days: 1, streakErr: true}
	srv := httptest.NewServer(stub.handler())
	defer srv.Close()

	p := pool.New("")
	p.Add(&auth.Auth{UID: "u1", AccessToken: "at", RefreshToken: "rt", ExpiresAt: 9999999999})
	up := &upstream.Client{HTTP: srv.Client(), ChatBaseCN: srv.URL, BillingBaseCN: srv.URL}
	s := New(Config{Pool: p, Upstream: up})

	// Прямой assert checkActivityStreak：GET ошибка → Алерт.
	if !s.checkActivityStreak(p.AuthByUID("u1")) {
		t.Fatal("streak GET при сбое должен быть алерт (reported but unverifiable）")
	}
	// Обратное чтение — только чтение oracle：GET Сбой не влияет на уже выполненную отправку, текущий раунд проходит (обход продолжается).
	if stub.streakHits.Load() != 1 {
		t.Errorf("streak_hits=%d want 1（GET Отмечено и при неудаче streak запрос)", stub.streakHits.Load())
	}
}

// TestRunActivityNowSkipsSelfCheckOnReportFail ошибка отправки отчета → Без запуска самопроверки (SKIP，бессмысленное обратное чтение).
func TestRunActivityNowSkipsSelfCheckOnReportFail(t *testing.T) {
	fastActivity(t)
	var streakHits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v2/report":
			w.WriteHeader(500)
			w.Write([]byte(`boom`))
		case "/activity/growth/streak":
			streakHits.Add(1)
			w.Write([]byte(`{"code":0,"data":{"streak":{"days":1}}}`))
		}
	}))
	defer srv.Close()

	p := pool.New("")
	p.Add(&auth.Auth{UID: "u1", AccessToken: "at", RefreshToken: "rt", ExpiresAt: 9999999999})
	up := &upstream.Client{HTTP: srv.Client(), ChatBaseCN: srv.URL, BillingBaseCN: srv.URL}
	s := New(Config{Pool: p, Upstream: up})

	s.RunActivityNow() // не сообщать об успехе → Без самопроверки
	if streakHits.Load() != 0 {
		t.Errorf("streak hits=%d want 0（ошибка отчёта — без самопроверки)", streakHits.Load())
	}
}

// TestRunCheckinDoesNotTriggerTravel завершение чекина больше не запускает путешествие (путешествие вынесено в отдельный шедулер).
func TestRunCheckinDoesNotTriggerTravel(t *testing.T) {
	fastTravel(t)
	stub := &travelStub{buddy: "null"}
	srv := billingAndGrowthServer(stub)
	defer srv.Close()

	s, _ := newTravelScheduler(t, srv, "u1")
	s.RunCheckinNow()

	// чекин больше не запускает путешествие попутно:buddy/info Не должен вызываться.
	if n := stub.infoCalls.Load(); n != 0 {
		t.Errorf("buddy/info calls=%d want 0（путешествие отделено от чекина)", n)
	}
}

// TestNextWakeTravelIndependent У путешествия отдельные тайм-поинты, не влияет на чекин.
func TestNextWakeTravelIndependent(t *testing.T) {
	s := New(Config{
		CheckinHours: []int{21},
		TravelHours: []int{9},
		ActivityHours: []int{10},
		KeepaliveHours: []int{22},
	})
	at, kinds := s.nextWake(time.Date(2026, 9, 11, 8, 0, 0, 0, time.Local))
	if want := time.Date(2026, 9, 11, 9, 0, 0, 0, time.Local); !at.Equal(want) {
		t.Errorf("next=%v want %v（путешествие 09:00 независимый момент)", at, want)
	}
	if len(kinds) != 1 || kinds[0] != taskTravel {
		t.Errorf("kinds=%v want [travel]", kinds)
	}
}

// TestNextWakeActivityIndependent Отчет об активности имеет отдельную точку времени.
func TestNextWakeActivityIndependent(t *testing.T) {
	s := New(Config{
		CheckinHours: []int{21},
		TravelHours: []int{9},
		ActivityHours: []int{10},
		KeepaliveHours: []int{22},
	})
	at, kinds := s.nextWake(time.Date(2026, 9, 11, 9, 30, 0, 0, time.Local))
	if want := time.Date(2026, 9, 11, 10, 0, 0, 0, time.Local); !at.Equal(want) {
		t.Errorf("next=%v want %v（активен 10:00 независимый момент)", at, want)
	}
	if len(kinds) != 1 || kinds[0] != taskActivity {
		t.Errorf("kinds=%v want [activity]", kinds)
	}
}

// TestNextWakeTravelDisabled после отключения путешествий в расписании больше нет точек путешествий (чекин как обычно).
func TestNextWakeTravelDisabled(t *testing.T) {
	s := New(Config{
		CheckinHours: []int{9, 21},
		TravelHours: []int{9},
		TravelDisabled: true,
		KeepaliveHours: []int{22},
	})
	at, kinds := s.nextWake(time.Date(2026, 9, 11, 8, 0, 0, 0, time.Local))
	// Путешествия отключены → 09:00 момент путешествия не должен появляться, ближайший — 09:00 чекин (тот же час, но чекин не отключён).
	if want := time.Date(2026, 9, 11, 9, 0, 0, 0, time.Local); !at.Equal(want) {
		t.Errorf("next=%v want %v", at, want)
	}
	if !hasKind(kinds, taskCheckin) {
		t.Errorf("kinds=%v want Содержит checkin", kinds)
	}
	if hasKind(kinds, taskTravel) {
		t.Errorf("kinds=%v Не должен содержать travel（отключено)", kinds)
	}
}

// TestNextWakeActivityDisabled После отключения отчёта об активности в расписании больше нет активных точек.
func TestNextWakeActivityDisabled(t *testing.T) {
	s := New(Config{
		CheckinHours: []int{9, 21},
		ActivityHours: []int{10},
		ActivityDisabled: true,
		KeepaliveHours: []int{22},
	})
	at, kinds := s.nextWake(time.Date(2026, 9, 11, 9, 30, 0, 0, time.Local))
	if want := time.Date(2026, 9, 11, 21, 0, 0, 0, time.Local); !at.Equal(want) {
		t.Errorf("next=%v want %v（Активно отключён → пропустить 10:00）", at, want)
	}
	if hasKind(kinds, taskActivity) {
		t.Errorf("kinds=%v Не должен содержать activity（отключено)", kinds)
	}
}

// TestCheckinDisabledTravelStillRuns При отключённом check-in travel/Активные продолжают работу (критерий приемки 2）。
func TestCheckinDisabledTravelStillRuns(t *testing.T) {
	s := New(Config{
		CheckinHours: []int{9, 21},
		CheckinDisabled: true,
		TravelHours: []int{9},
		ActivityHours: []int{10},
		KeepaliveHours: []int{22},
	})
	at, kinds := s.nextWake(time.Date(2026, 9, 11, 8, 0, 0, 0, time.Local))
	if want := time.Date(2026, 9, 11, 9, 0, 0, 0, time.Local); !at.Equal(want) {
		t.Errorf("next=%v want %v（чекин отключён, путешествие 09:00 выполняется как обычно)", at, want)
	}
	if hasKind(kinds, taskCheckin) {
		t.Errorf("kinds=%v Не должен содержать checkin（отключено)", kinds)
	}
	if !hasKind(kinds, taskTravel) {
		t.Errorf("kinds=%v должен содержать travel（Регистрация отключена, но путешествие независимо)", kinds)
	}
}

// TestAllFourDisabledNoSpin все четыре типа задач отключены:Run Без холостого хода.
func TestAllFourDisabledNoSpin(t *testing.T) {
	s := New(Config{
		CheckinDisabled: true,
		TravelDisabled: true,
		ActivityDisabled: true,
		KeepaliveDisabled: true,
		BlackcatDisabled: true,
		CheckinHours: []int{9, 21},
		TravelHours: []int{9},
		ActivityHours: []int{10},
		KeepaliveHours: []int{22},
	})
	at, kinds := s.nextWake(time.Now())
	if !at.IsZero() || len(kinds) != 0 {
		t.Errorf("at=%v kinds=%v want zero/nil（все пять категорий отключены)", at, kinds)
	}
}

// TestNextWakeSameHourTravelAndCheckin При попадании путешествия и чекина на один час выполняются оба типа задач.
func TestNextWakeSameHourTravelAndCheckin(t *testing.T) {
	s := New(Config{
		CheckinHours: []int{9, 21},
		TravelHours: []int{9},
		ActivityHours: []int{10},
		KeepaliveHours: []int{22},
	})
	at, kinds := s.nextWake(time.Date(2026, 9, 11, 8, 0, 0, 0, time.Local))
	if want := time.Date(2026, 9, 11, 9, 0, 0, 0, time.Local); !at.Equal(want) {
		t.Errorf("next=%v want %v", at, want)
	}
	if !hasKind(kinds, taskCheckin) || !hasKind(kinds, taskTravel) {
		t.Errorf("kinds=%v want Содержит checkin+travel（Совм. 09:00 две задачи)", kinds)
	}
}

// TestRunDispatchesActivityAndTravel Run Раздача по расписанию activity и travel（без реального запроса к апстриму, использовать пустой пул).
func TestRunDispatchesActivityAndTravel(t *testing.T) {
	fastActivity(t)
	fastTravel(t)
	// пустой pool → RunActivityNow/RunTravelNow Обход 0 аккаунт — сразу возврат, без блокировки.
	p := pool.New("")
	up := &upstream.Client{}
	s := New(Config{
		Pool: p,
		Upstream: up,
		CheckinHours: []int{},
		TravelHours: []int{},
		ActivityHours: []int{},
		KeepaliveHours: []int{},
	})
	// Все четыре категории пусты hours → nextWake откат к дефолту → будет конструировать timer，ctx При отмене — сразу возврат.
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { s.Run(ctx); close(done) }()
	time.Sleep(20 * time.Millisecond)
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Run Не в ctx Возврат после отмены")
	}
}
