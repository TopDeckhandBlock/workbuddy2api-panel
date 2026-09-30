// checkin_retry_test.go Закреплённая отметка/Ограниченный ретрай транзиентных ошибок биллинговых вызовов обслуживания баланса:
// апстрим 5xx（На практике встречается эпизодически code 10000 / http 500）и ретрай при сетевом джиттере, бизнес-ошибка (уже чекин/
// ошибка параметра) — без ретрая, ретрай лишь повторит ту же ошибку.
package upstream

import (
	"errors"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
)

// shortBillingRetry Интервал ретрая для тестов (прод 2s раздует юнит-тесты до секунд).
func shortBillingRetry(t *testing.T) {
	t.Helper()
	old := billingRetryDelay
	billingRetryDelay = time.Millisecond
	t.Cleanup(func() { billingRetryDelay = old })
}

func TestDailyCheckinRetriesTransient500(t *testing.T) {
	shortBillingRetry(t)
	var calls int32
	c := testClient(func(r *http.Request) (*http.Response, error) {
		if !strings.HasSuffix(r.URL.Path, "/v2/billing/meter/daily-checkin") {
			return nil, errors.New("wrong path")
		}
		if atomic.AddInt32(&calls, 1) == 1 {
			return jsonResp(500, `{"code":10000,"msg":"API request failed with status code: 500"}`), nil
		}
		return jsonResp(200, `{"code":0,"data":{}}`), nil
	})
	if err := c.DailyCheckin(&auth.Auth{AccessToken: "at"}); err != nil {
		t.Fatalf("Впервые 500 Должен успешно повториться,err=%v", err)
	}
	if n := atomic.LoadInt32(&calls); n != 2 {
		t.Fatalf("calls=%d want 2（1 неудач + 1 попыток повтора)", n)
	}
}

func TestDailyCheckinRetriesExhausted(t *testing.T) {
	shortBillingRetry(t)
	var calls int32
	c := testClient(func(r *http.Request) (*http.Response, error) {
		atomic.AddInt32(&calls, 1)
		return jsonResp(500, `{"code":10000,"msg":"API request failed with status code: 500"}`), nil
	})
	err := c.DailyCheckin(&auth.Auth{AccessToken: "at"})
	if err == nil {
		t.Fatal("продолжается 500 должен вернуть ошибку")
	}
	if n := atomic.LoadInt32(&calls); n != 3 {
		t.Fatalf("calls=%d want 3（1 Раз + 2 попыток ретрая — лимит)", n)
	}
}

func TestDailyCheckinNoRetryOnBusinessError(t *testing.T) {
	shortBillingRetry(t)
	var calls int32
	c := testClient(func(r *http.Request) (*http.Response, error) {
		atomic.AddInt32(&calls, 1)
		// 「«Сегодня уже отмечено» — бизнес-отказ идемпотентности (code!=0），Повтор бессмысленен.
		return jsonResp(200, `{"code":14001,"msg":"Сегодня уже отмечено"}`), nil
	})
	err := c.DailyCheckin(&auth.Auth{AccessToken: "at"})
	if err == nil || !IsAlreadyCheckin(err) {
		t.Fatalf("err=%v want already-checkin business error", err)
	}
	if n := atomic.LoadInt32(&calls); n != 1 {
		t.Fatalf("calls=%d want 1（Бизнес-ошибки не ретраятся)", n)
	}
}

func TestDailyCheckinNoRetryOn4xx(t *testing.T) {
	shortBillingRetry(t)
	var calls int32
	c := testClient(func(r *http.Request) (*http.Response, error) {
		atomic.AddInt32(&calls, 1)
		return jsonResp(403, `{"code":11140,"msg":"request illegal"}`), nil
	})
	if err := c.DailyCheckin(&auth.Auth{AccessToken: "at"}); err == nil {
		t.Fatal("403 должен вернуть ошибку")
	}
	if n := atomic.LoadInt32(&calls); n != 1 {
		t.Fatalf("calls=%d want 1（4xx не мгновенно, без ретрая)", n)
	}
}

func TestUserResourceDetailedRetriesTransient500(t *testing.T) {
	shortBillingRetry(t)
	var calls int32
	c := testClient(func(r *http.Request) (*http.Response, error) {
		if !strings.HasSuffix(r.URL.Path, "/v2/billing/meter/get-user-resource") {
			return nil, errors.New("wrong path")
		}
		if atomic.AddInt32(&calls, 1) == 1 {
			return jsonResp(500, `{"code":10000,"msg":"API request failed with status code: 500"}`), nil
		}
		return jsonResp(200, `{"code":0,"data":{"Response":{"Data":{"Accounts":[
			{"PackageName":"p","CapacitySize":100,"CapacityRemain":60}
		]}}}}`), nil
	})
	remain, _, _, _, _, err := c.UserResourceDetailedWithExpiry(&auth.Auth{AccessToken: "at"}, 0)
	if err != nil || remain != 60 {
		t.Fatalf("remain=%d err=%v, want 60 nil", remain, err)
	}
	if n := atomic.LoadInt32(&calls); n != 2 {
		t.Fatalf("calls=%d want 2（1 неудач + 1 попыток повтора)", n)
	}
}
