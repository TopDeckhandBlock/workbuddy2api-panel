package upstream

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
)

// TestGrowthStreakParsesDays Парсинг data.streak.days（probe_active.py в той же метрике).
func TestGrowthStreakParsesDays(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/activity/growth/streak" {
			t.Errorf("path=%s want /activity/growth/streak", r.URL.Path)
		}
		if r.Method != http.MethodGet {
			t.Errorf("method=%s want GET", r.Method)
		}
		w.Write([]byte(`{"code":0,"msg":"ok","data":{"streak":{"days":3,"total_rewards":1}}}`))
	}))
	defer srv.Close()

	c := &Client{HTTP: srv.Client(), ChatBaseCN: srv.URL, BillingBaseCN: srv.URL}
	days, err := c.GrowthStreak(&auth.Auth{AccessToken: "at", UID: "u1"})
	if err != nil {
		t.Fatalf("GrowthStreak: %v", err)
	}
	if days != 3 {
		t.Errorf("days=%d want 3", days)
	}
}

// TestGrowthStreakDefaultZero нехватка streak/days Поле → 0（days==0 т.е. сигнал самопроверки/тревоги).
func TestGrowthStreakDefaultZero(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"code":0,"data":{}}`))
	}))
	defer srv.Close()

	c := &Client{HTTP: srv.Client(), ChatBaseCN: srv.URL, BillingBaseCN: srv.URL}
	days, err := c.GrowthStreak(&auth.Auth{AccessToken: "at", UID: "u1"})
	if err != nil {
		t.Fatalf("GrowthStreak: %v", err)
	}
	if days != 0 {
		t.Errorf("days=%d want 0（отсутствующее поле — нулевое значение)", days)
	}
}

// TestGrowthStreakServerError HTTP не 2xx → *Error。
func TestGrowthStreakServerError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(500)
		w.Write([]byte(`boom`))
	}))
	defer srv.Close()

	c := &Client{HTTP: srv.Client(), ChatBaseCN: srv.URL, BillingBaseCN: srv.URL}
	if _, err := c.GrowthStreak(&auth.Auth{AccessToken: "at", UID: "u1"}); err == nil {
		t.Fatal("want error on 500")
	}
}

// TestGrowthStreakBusinessCode Бизнес code не 0 → ошибка (не 2xx тихие сбои вне этого тоже должны наблюдаться).
func TestGrowthStreakBusinessCode(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
		w.Write([]byte(`{"code":12000,"msg":"internal"}`))
	}))
	defer srv.Close()

	c := &Client{HTTP: srv.Client(), ChatBaseCN: srv.URL, BillingBaseCN: srv.URL}
	if _, err := c.GrowthStreak(&auth.Auth{AccessToken: "at", UID: "u1"}); err == nil {
		t.Fatal("want error on non-zero business code")
	}
}
