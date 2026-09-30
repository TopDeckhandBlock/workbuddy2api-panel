package scheduler

import (
	"context"
	"encoding/json"
	"fmt"
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

// travelStub Эмуляция growth все эндпоинты домена, логирование кол-ва вызовов и параметров запроса.
type travelStub struct {
	buddy string // /buddy/info data.buddy оригинал ("null" или объект)
	state string // /travel/status data Исходный текст
	firstStatus int // /buddy/first HTTP Код статуса (не 0 возвращать как бизнес-ошибку)

	infoCalls, statusCalls, departCalls atomic.Int32
	claimCalls, firstCalls, agreeCalls atomic.Int32
	location, record atomic.Int64
}

func (s *travelStub) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/activity/growth/buddy/info":
			s.infoCalls.Add(1)
			fmt.Fprintf(w, `{"code":0,"msg":"ok","data":{"buddy":%s}}`, s.buddy)
		case "/activity/growth/buddy/travel/status":
			s.statusCalls.Add(1)
			fmt.Fprintf(w, `{"code":0,"msg":"ok","data":%s}`, s.state)
		case "/activity/growth/buddy/travel/depart":
			s.departCalls.Add(1)
			var body struct {
				LocationID int `json:"location_id"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			s.location.Store(int64(body.LocationID))
			w.Write([]byte(`{"code":0,"msg":"ok","data":{}}`))
		case "/activity/growth/buddy/travel/claim":
			s.claimCalls.Add(1)
			var body struct {
				RecordID int64 `json:"record_id"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			s.record.Store(body.RecordID)
			w.Write([]byte(`{"code":0,"msg":"ok","data":{"reward_credit":9}}`))
		case "/activity/growth/buddy/first":
			s.firstCalls.Add(1)
			if s.firstStatus >= 400 {
				w.WriteHeader(s.firstStatus)
				w.Write([]byte(`{"code":400,"msg":"first_buddy task not completed yet"}`))
				return
			}
			w.Write([]byte(`{"code":0,"msg":"ok","data":{"buddy":{"id":1,"name":"Архив Мяу"}}}`))
		case "/activity/growth/buddy/agreement":
			s.agreeCalls.Add(1)
			w.Write([]byte(`{"code":0,"msg":"ok","data":{"agreed":true}}`))
		default:
			http.Error(w, "not found", 404)
		}
	})
}

func (s *travelStub) server() *httptest.Server {
	return httptest.NewServer(s.handler())
}

// billingAndGrowthServer Одновременно симулировать billing（check-in/Баланс/обновление) и growth（путешествие) эндпоинт,
// для кросс-доменных кейсов типа "завершить чекин и заодно запустить travel».
func billingAndGrowthServer(stub *travelStub) *httptest.Server {
	growth := stub.handler()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/daily-checkin"):
			w.Write([]byte(`{"code":0,"msg":"ok","data":{}}`))
		case strings.HasSuffix(r.URL.Path, "/get-user-resource"):
			w.Write([]byte(`{"code":0,"data":{"Response":{"Data":{"Accounts":[{"CycleCapacitySize":1000,"CycleCapacityRemain":500,"CycleCapacityUsed":0}]}}}}`))
		case strings.HasSuffix(r.URL.Path, "/token/refresh"):
			w.Write([]byte(`{"code":0,"data":{"accessToken":"new","expiresIn":3600}}`))
		default:
			growth.ServeHTTP(w, r)
		}
	}))
}

// fastTravel отключить лимит между аккаунтами, чтобы тесты не ждали впустую 800ms。
func fastTravel(t *testing.T) {
	t.Helper()
	old := travelAccountDelay
	travelAccountDelay = 0
	t.Cleanup(func() { travelAccountDelay = old })
}

// newTravelScheduler конструктор travel планировщик с полным набором зависимостей.
func newTravelScheduler(t *testing.T, srv *httptest.Server, uids ...string) (*Scheduler, *pool.Pool) {
	t.Helper()
	p := pool.New("")
	for _, uid := range uids {
		p.Add(&auth.Auth{UID: uid, AccessToken: "at", RefreshToken: "rt", ExpiresAt: 9999999999})
	}
	up := &upstream.Client{HTTP: srv.Client(), ChatBaseCN: srv.URL, BillingBaseCN: srv.URL}
	return New(Config{Pool: p, Upstream: up, CheckinHours: []int{9, 21}, KeepaliveHours: []int{22}}), p
}

// TestRunCheckinNowNoLongerTriggersTravel завершение чекина больше не запускает путешествие (путешествие вынесено в отдельный шедулер).
// путешествие по независимым моментам (travel_hours）Триггер, отвязан от check-in.
func TestRunCheckinNowNoLongerTriggersTravel(t *testing.T) {
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

// TestRunTravelNowAdoptsOnNoBuddy Путешествия — отдельный график: без кота → принять соглашение + принять.
func TestRunTravelNowAdoptsOnNoBuddy(t *testing.T) {
	fastTravel(t)
	stub := &travelStub{buddy: "null"}
	srv := stub.server()
	defer srv.Close()

	s, _ := newTravelScheduler(t, srv, "u1")
	s.RunTravelNow()

	if n := stub.infoCalls.Load(); n != 1 {
		t.Errorf("buddy/info calls=%d want 1", n)
	}
	if n := stub.firstCalls.Load(); n != 1 {
		t.Errorf("buddy/first calls=%d want 1（нет кота — попробуйте взять из приюта)", n)
	}
	if n := stub.agreeCalls.Load(); n != 1 {
		t.Errorf("buddy/agreement calls=%d want 1", n)
	}
}

// TestRunTravelCoversIdleAccount Независимое планирование путешествий покрывает idle-аккаунты и отправляет.
func TestRunTravelCoversIdleAccount(t *testing.T) {
	fastTravel(t)
	stub := &travelStub{buddy: `{"id":7,"name":"Архив Мяу"}`,
		state: `{"state":"idle","daily_limit_reached":false}`}
	srv := stub.server()
	defer srv.Close()

	s, _ := newTravelScheduler(t, srv, "u1")

	s.RunTravelNow()

	// есть кот + idle + Лимит не достигнут → Отправлено.
	if n := stub.departCalls.Load(); n != 1 {
		t.Errorf("depart calls=%d want 1", n)
	}
}

// TestRunKeepaliveDoesNotTriggerTravel 22 точечный keepalive не триггерит travel: travel по отдельному расписанию.
func TestRunKeepaliveDoesNotTriggerTravel(t *testing.T) {
	fastTravel(t)
	stub := &travelStub{buddy: "null"}
	srv := billingAndGrowthServer(stub)
	defer srv.Close()

	s, _ := newTravelScheduler(t, srv, "u1")
	s.RunKeepaliveNow()

	if n := stub.infoCalls.Load(); n != 0 {
		t.Errorf("buddy/info calls=%d want 0（keep-alive не триггерит travel)", n)
	}
	if n := stub.firstCalls.Load(); n != 0 {
		t.Errorf("buddy/first calls=%d want 0", n)
	}
}

// TestRunTravelStateMachine Табличный драйвер покрывает все ветки автомата: за один проход — одно действие.
func TestRunTravelStateMachine(t *testing.T) {
	const buddyPresent = `{"id":7,"name":"Архив Мяу R"}`

	cases := []struct {
		name string
		buddy string
		state string
		firstStatus int
		wantInfo int32
		wantStatus int32
		wantDepart int32
		wantClaim int32
		wantFirst int32
		wantAgree int32
		wantLocation int64
		wantRecord int64
	}{
		{
			name: "без кота-Успешно принято", buddy: "null",
			wantInfo: 1, wantFirst: 1, wantAgree: 1,
		},
		{
			name: "без кота-порог не достигнут-Тихо пропустить", buddy: "null", firstStatus: 400,
			wantInfo: 1, wantFirst: 1, wantAgree: 1,
		},
		{
			name: "есть кот-idle-Лимит не достигнут-Отправить", buddy: buddyPresent,
			state: `{"state":"idle","daily_limit_reached":false}`,
			wantInfo: 1, wantStatus: 1, wantDepart: 1, wantLocation: 4,
		},
		{
			name: "есть кот-idle-Лимит достигнут-пропустить", buddy: buddyPresent,
			state: `{"state":"idle","daily_limit_reached":true}`,
			wantInfo: 1, wantStatus: 1,
		},
		{
			name: "есть кот-traveling-пропустить", buddy: buddyPresent,
			state: `{"state":"traveling","record_id":42}`,
			wantInfo: 1, wantStatus: 1,
		},
		{
			name: "есть кот-arrived-Получение награды", buddy: buddyPresent,
			state: `{"state":"arrived","record_id":42,"reward_credit":9}`,
			wantInfo: 1, wantStatus: 1, wantClaim: 1, wantRecord: 42,
		},
		{
			name: "есть кот-arrived-нехваткаrecord_id-пропустить получение награды", buddy: buddyPresent,
			state: `{"state":"arrived","record_id":0}`,
			wantInfo: 1, wantStatus: 1,
		},
		{
			name: "есть кот-Неизвестный статус-пропустить", buddy: buddyPresent,
			state: `{"state":"teleporting"}`,
			wantInfo: 1, wantStatus: 1,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fastTravel(t)
			stub := &travelStub{buddy: tc.buddy, state: tc.state, firstStatus: tc.firstStatus}
			srv := stub.server()
			defer srv.Close()

			s, _ := newTravelScheduler(t, srv, "u1")
			s.RunTravelNow()

			got := map[string]int32{
				"info": stub.infoCalls.Load(), "status": stub.statusCalls.Load(),
				"depart": stub.departCalls.Load(), "claim": stub.claimCalls.Load(),
				"first": stub.firstCalls.Load(), "agreement": stub.agreeCalls.Load(),
			}
			want := map[string]int32{
				"info": tc.wantInfo, "status": tc.wantStatus,
				"depart": tc.wantDepart, "claim": tc.wantClaim,
				"first": tc.wantFirst, "agreement": tc.wantAgree,
			}
			for k, w := range want {
				if got[k] != w {
					t.Errorf("%s calls=%d want %d", k, got[k], w)
				}
			}
			if tc.wantDepart > 0 && stub.location.Load() != tc.wantLocation {
				t.Errorf("location_id=%d want %d", stub.location.Load(), tc.wantLocation)
			}
			if tc.wantClaim > 0 && stub.record.Load() != tc.wantRecord {
				t.Errorf("record_id=%d want %d", stub.record.Load(), tc.wantRecord)
			}
		})
	}
}

// TestRunTravelAdoptThresholdTriedOncePerDay порог не достигнут (400）одна попытка в сутки, последующие проверки тихо пропускаются.
func TestRunTravelAdoptThresholdTriedOncePerDay(t *testing.T) {
	fastTravel(t)
	stub := &travelStub{buddy: "null", firstStatus: 400}
	srv := stub.server()
	defer srv.Close()

	s, _ := newTravelScheduler(t, srv, "u1")
	s.RunTravelNow()
	s.RunTravelNow()
	s.RunTravelNow()

	if n := stub.firstCalls.Load(); n != 1 {
		t.Errorf("first calls=%d want 1（в этот день пробовать только один раз)", n)
	}
	if n := stub.agreeCalls.Load(); n != 1 {
		t.Errorf("agreement calls=%d want 1", n)
	}
	if n := stub.infoCalls.Load(); n != 3 {
		t.Errorf("info calls=%d want 3（все равно каждый прогон проверять наличие кота)", n)
	}
}

// TestRunTravelAdoptTriedExpiresNextDay после смены календарных суток разрешена повторная попытка принятия.
func TestRunTravelAdoptTriedExpiresNextDay(t *testing.T) {
	fastTravel(t)
	stub := &travelStub{buddy: "null", firstStatus: 400}
	srv := stub.server()
	defer srv.Close()

	s, _ := newTravelScheduler(t, srv, "u1")
	s.markAdoptTried("u1") // Сегодня уже пробовали
	s.RunTravelNow()
	if n := stub.firstCalls.Load(); n != 0 {
		t.Fatalf("first calls=%d want 0（уже пробовали сегодня)", n)
	}

	s.adoptTried["u1"] = "2000-01-01" // Симулировать вчерашнюю запись
	s.RunTravelNow()
	if n := stub.firstCalls.Load(); n != 1 {
		t.Errorf("first calls=%d want 1（При переходе через сутки повторить)", n)
	}
}

// TestRunTravelSkipsDisabledAndFailedAccounts отключенные аккаунты не проверять; ошибка одного аккаунта не влияет на остальные.
func TestRunTravelSkipsDisabledAndFailedAccounts(t *testing.T) {
	fastTravel(t)
	var deadCalls, okDepart, disabledCalls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Header.Get("X-User-Id") == "disabled":
			disabledCalls.Add(1)
			w.WriteHeader(401)
			w.Write([]byte(`{"code":12153,"msg":"Offline user session not found"}`))
		case r.Header.Get("X-User-Id") == "dead":
			deadCalls.Add(1)
			w.WriteHeader(401)
			w.Write([]byte(`{"code":12153,"msg":"Offline user session not found"}`))
		case r.URL.Path == "/activity/growth/buddy/info":
			w.Write([]byte(`{"code":0,"data":{"buddy":{"id":1}}}`))
		case r.URL.Path == "/activity/growth/buddy/travel/status":
			w.Write([]byte(`{"code":0,"data":{"state":"idle","daily_limit_reached":false}}`))
		case r.URL.Path == "/activity/growth/buddy/travel/depart":
			okDepart.Add(1)
			w.Write([]byte(`{"code":0,"data":{}}`))
		default:
			http.Error(w, "not found", 404)
		}
	}))
	defer srv.Close()

	s, p := newTravelScheduler(t, srv, "disabled", "dead", "ok")
	p.Disable("disabled", "test")
	s.RunTravelNow()

	if n := deadCalls.Load(); n != 1 {
		t.Errorf("dead account calls=%d want 1（401 Пропустить этот раунд, без принудительного обновления token）", n)
	}
	if n := disabledCalls.Load(); n != 0 {
		t.Errorf("disabled account calls=%d want 0", n)
	}
	// Сбой аккаунтов в начале списка не прерывает обход: последний ok Аккаунт штатно завершает диспетчеризацию.
	if n := okDepart.Load(); n != 1 {
		t.Errorf("ok account depart calls=%d want 1（сбойный аккаунт не влияет на дальнейший обход)", n)
	}
	st, ok := p.Status("ok")
	if !ok {
		t.Fatal("ok Аккаунт должен быть в пуле")
	}
	if st.Disabled {
		t.Errorf("ok аккаунт не должен затрагиваться: %+v", st)
	}
}

// TestRunTravelDisabledAccountSkipsAllCalls заблокированный аккаунт не отправляет ни одного запроса.
func TestRunTravelDisabledAccountSkipsAllCalls(t *testing.T) {
	fastTravel(t)
	stub := &travelStub{buddy: "null"}
	srv := stub.server()
	defer srv.Close()

	s, p := newTravelScheduler(t, srv, "u1")
	p.Disable("u1", "test")
	s.RunTravelNow()

	if n := stub.infoCalls.Load(); n != 0 {
		t.Errorf("info calls=%d want 0（отключённые аккаунты пропускаются)", n)
	}
}

// TestRunTravelActionErrorsDoNotAbort ошибка апстрима на любом этапе влияет только на текущий раунд этого аккаунта: не panic、не прерывать обход.
func TestRunTravelActionErrorsDoNotAbort(t *testing.T) {
	fastTravel(t)
	var okDepart atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		uid := r.Header.Get("X-User-Id")
		fail := func() {
			w.WriteHeader(500)
			w.Write([]byte(`{"code":500,"msg":"boom"}`))
		}
		switch {
		case uid == "bdinfo": // Ошибка проверки наличия cat
			fail()
		case uid == "status" && r.URL.Path == "/activity/growth/buddy/travel/status": // Ошибка проверки статуса
			fail()
		case uid == "depart" && r.URL.Path == "/activity/growth/buddy/travel/depart": // Ошибка отправки
			fail()
		case uid == "claim" && r.URL.Path == "/activity/growth/buddy/travel/claim": // Не удалось получить награду
			fail()
		case uid == "agree" && r.URL.Path == "/activity/growth/buddy/agreement": // Ошибка принятия соглашения
			fail()
		case uid == "first" && r.URL.Path == "/activity/growth/buddy/first": // Неудача получения не-порогового типа
			fail()
		case r.URL.Path == "/activity/growth/buddy/info":
			if uid == "claim" {
				w.Write([]byte(`{"code":0,"data":{"buddy":{"id":1}}}`))
				return
			}
			if uid == "depart" || uid == "status" || uid == "ok" {
				w.Write([]byte(`{"code":0,"data":{"buddy":{"id":1}}}`))
				return
			}
			w.Write([]byte(`{"code":0,"data":{"buddy":null}}`))
		case r.URL.Path == "/activity/growth/buddy/travel/status":
			if uid == "claim" {
				w.Write([]byte(`{"code":0,"data":{"state":"arrived","record_id":42}}`))
				return
			}
			w.Write([]byte(`{"code":0,"data":{"state":"idle","daily_limit_reached":false}}`))
		case r.URL.Path == "/activity/growth/buddy/travel/depart":
			okDepart.Add(1)
			w.Write([]byte(`{"code":0,"data":{}}`))
		case r.URL.Path == "/activity/growth/buddy/agreement":
			w.Write([]byte(`{"code":0,"data":{"agreed":true}}`))
		default:
			http.Error(w, "not found", 404)
		}
	}))
	defer srv.Close()

	s, _ := newTravelScheduler(t, srv, "agree", "bdinfo", "claim", "depart", "first", "ok", "status")
	s.RunTravelNow() // Не должен panic

	// Последний аккаунт (uid После сортировки ok Идёт после status но всё после неудачных аккаунтов) отправка выполняется как обычно.
	if n := okDepart.Load(); n == 0 {
		t.Errorf("depart calls=%d want >0（ошибка отдельного аккаунта не должна прерывать обход)", n)
	}
}

// TestRunTravelLoopCancelsWhenDisabled при отсутствии задач на ровный час Run Без холостого хода,ctx При отмене — сразу возврат.
func TestRunTravelLoopCancelsWhenDisabled(t *testing.T) {
	s := &Scheduler{cfg: Config{}, adoptTried: map[string]string{}}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { s.Run(ctx); close(done) }()
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Run Не в ctx Возврат после отмены")
	}
}

// TestRunTravelLoopStopsOnCancel когда есть расписание, ожидающее таймер ctx Отмена должна немедленно выходить.
func TestRunTravelLoopStopsOnCancel(t *testing.T) {
	s := New(Config{CheckinHours: []int{9}, KeepaliveHours: []int{22}})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { s.Run(ctx); close(done) }()
	time.Sleep(20 * time.Millisecond) // разрешить Run вход в select Ожидание
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Run Не в ctx Возврат после отмены")
	}
}

// TestTravelDayAlignsCST Ежедневный сброс по CST определение по календарным суткам (UTC 17:00 Уже следующие сутки CST）。
func TestTravelDayAlignsCST(t *testing.T) {
	cases := []struct {
		name string
		in time.Time
		want string
	}{
		{"UTC раннее утро = CST за текущий день", time.Date(2026, 9, 11, 2, 0, 0, 0, time.UTC), "2026-09-11"},
		{"UTC 16:00 = CST на следующий день 00:00", time.Date(2026, 9, 11, 16, 0, 0, 0, time.UTC), "2026-09-12"},
		{"UTC 15:59 Всё ещё CST за текущий день", time.Date(2026, 9, 11, 15, 59, 0, 0, time.UTC), "2026-09-11"},
	}
	for _, c := range cases {
		if got := travelDay(c.in); got != c.want {
			t.Errorf("%s: travelDay=%s want %s", c.name, got, c.want)
		}
	}
}
