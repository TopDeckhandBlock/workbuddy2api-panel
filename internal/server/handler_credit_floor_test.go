// handler_credit_floor_test.go Наблюдаемость гарантированного минимума баллов и сквозная блокировка:
// /status прокинуть наружу credit_floor；при обращении исчерпанного аккаунта к платной модели у шлюза нет доступных номеров (503）。
package server

import (
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
)

// TestStatusCreditFloor /status прокинуть наружу pool уровня credit_floor действующее значение:
// при аудите видны и минимальные гарантии (совместно с accounts[].credits и model_costs можно считать установленным
// почему аккаунт не выдает тикет для модели). Выключено (0）также явно отображается — отсутствие заставит думать, что записи нет.
func TestStatusCreditFloor(t *testing.T) {
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at", ExpiresAt: 9999999999})
	p.SetCreditFloor(100)
	h := NewHandler(Config{Pool: p, Upstream: upstream.New()})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/status", nil))
	if rec.Code != 200 {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("status not json: %v", err)
	}
	if body["credit_floor"] != float64(100) {
		t.Errorf("credit_floor=%v want 100", body["credit_floor"])
	}
}

// TestStatusCreditFloorZeroOff Не настроено (по умолчанию 0）Также пробрасывается 0（записывать явно, ноль не опускать).
func TestStatusCreditFloorZeroOff(t *testing.T) {
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at", ExpiresAt: 9999999999})
	h := NewHandler(Config{Pool: p, Upstream: upstream.New()})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/status", nil))
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("status not json: %v", err)
	}
	if body["credit_floor"] != float64(0) {
		t.Errorf("credit_floor=%v want 0 (off)", body["credit_floor"])
	}
}

// TestCreditFloorBlocksPaidRequest End-to-end: достигнуто дно + Фактическая тарифицируемая модель → нет кандидатов для выбора номера,
// Ответ шлюза 503（строгая семантика: лучше 503 не пробивать; бесплатные модели доступны как обычно).
//
// Ограничение честности: в данном кейсе upstream фиктивный недоступный апстрим,floor вкл. и выкл. в итоге оба возвращаются в 503——
// если assert только 503，невозможно различить "floor заблокировано» и "апстрим недоступен», ассерт будет ложно выполнен по неверной причине.
// Реальное доказательство того, что перехват на уровне выбора номера — это**Сверочный assert**：контрольная группа (без включения floor）должен быть выбираем poor，
// экспериментальная группа (вкл floor）При тех же условиях номер не должен выбираться.
func TestCreditFloorBlocksPaidRequest(t *testing.T) {
	const body = `{"model":"paid-model","messages":[{"role":"user","content":"hi"}]}`
	// Контрольная группа: не задавать floor，остальное дословно совпадает (тот же нижний номер, тот же tier 2 наблюдением).
	ctlPool := testPoolWith(&auth.Auth{UID: "poor", AccessToken: "at", ExpiresAt: 9999999999})
	ctlPool.SetCredits("poor", 30, 0)
	ctlPool.NoteModelCost("poor", "paid-model", 2.9, 1000)
	if a := ctlPool.PickExcludingForRealm(nil, "paid-model", ""); a == nil {
		t.Fatal("Отказ префильтра контрольной группы: не задано floor должен быть выбран poor（иначе данный кейс не считается контрольным)")
	}

	// Экспериментальная группа: вкл floor 100。
	p := testPoolWith(&auth.Auth{UID: "poor", AccessToken: "at", ExpiresAt: 9999999999})
	p.SetCreditFloor(100)
	p.SetCredits("poor", 30, 0)
	p.NoteModelCost("poor", "paid-model", 2.9, 1000)
	if a := p.PickExcludingForRealm(nil, "paid-model", ""); a != nil {
		t.Fatalf("floor Должен быть в pool слой блокирует исчерпанные аккаунты,got %v（блокировка должна быть на уровне выбора номера, а не за счёт ошибки апстрима)", a.UID)
	}

	h := NewHandler(Config{Pool: p, Upstream: upstream.New()})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(body)))
	if rec.Code != 503 {
		t.Fatalf("code=%d want 503（весь пул исчерпан + Модель тарификации → Не пропускать), body=%s", rec.Code, rec.Body)
	}
	if !strings.Contains(rec.Body.String(), "no_healthy_account") {
		t.Errorf("body=%s want no_healthy_account", rec.Body)
	}
}

// TestCreditFloorAllowsFreeRequest End-to-end: тот же исчерпанный аккаунт для бесплатных моделей пропускается как обычно (не 503）。
// Это основа гарантии — после срабатывания система должна оставаться работоспособной.
func TestCreditFloorAllowsFreeRequest(t *testing.T) {
	p := testPoolWith(&auth.Auth{UID: "poor", AccessToken: "at", ExpiresAt: 9999999999})
	p.SetCreditFloor(100)
	p.SetCredits("poor", 30, 0)
	p.NoteModelCost("poor", "free-model", 0, 1000)

	// На уровне выбора номера билет обязателен (успех форварда апстрима вне скоупа кейса, см. TestCreditFloor* юнит-тест).
	if a := p.PickExcludingForRealm(nil, "free-model", ""); a == nil || a.UID != "poor" {
		t.Fatalf("упёртый в лимит номер при вызове бесплатной модели должен выбираться,got %v", a)
	}
	// pool слой и handler единая метрика уровня (во избежание handler Обход pool floor проверка).
	if p.PickByUIDForModel("poor", "free-model") == nil {
		t.Error("sticky-маршрут для бесплатных моделей не должен floor Блокировка")
	}
}

// TestCreditFloorPersistsAcquireRelease гарантированный перехват не влияет на семантику активных лизов:
// Достигнуто дно + платный → Acquire всё ещё занимает слот (floor Блокировка только на уровне выбора номера), освобождение идемпотентно.
func TestCreditFloorPersistsAcquireRelease(t *testing.T) {
	p := testPoolWith(&auth.Auth{UID: "poor", AccessToken: "at", ExpiresAt: 9999999999})
	p.SetCreditFloor(100)
	p.SetCredits("poor", 30, 0)
	p.NoteModelCost("poor", "paid-model", 2.9, 1000)
	p.SetMaxInFlight(2)

	if !p.Acquire("poor") {
		t.Fatal("Acquire должен завершиться успешно (floor не вмешивается в семантику аренды)")
	}
	p.Release("poor")
	p.Release("poor") // идемпотентный релиз не уходит в минус
}
