package server

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/pool"
	"github.com/linguo2625469/workbuddy2api-panel/internal/prompt"
	"github.com/linguo2625469/workbuddy2api-panel/internal/redisstore"
	"github.com/linguo2625469/workbuddy2api-panel/internal/session"
	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
	"github.com/linguo2625469/workbuddy2api-panel/internal/usage"
)

// TestMain Лог таблицы чата по умолчанию выкл (chatLogEnabled=false），устранение go test за период stdout Шум.
// Тест проверки вывода строк таблицы (logging_test.go в ChatLogs/LogChatRow серия) использовать withChatLog временно включено.
func TestMain(m *testing.M) {
	chatLogEnabled = false
	os.Exit(m.Run())
}

const sseOK = "data: {\"id\":\"chatcmpl-1\",\"object\":\"chat.completion.chunk\",\"created\":1753600000,\"model\":\"glm-5.2\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"你好\"}}]}\n\n" +
	"data: {\"id\":\"chatcmpl-1\",\"object\":\"chat.completion.chunk\",\"created\":1753600000,\"model\":\"glm-5.2\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":1,\"completion_tokens\":1,\"total_tokens\":2}}\n\n" +
	"data: [DONE]\n\n"

// newFakeUpstream Вернуть один ChatStream Ход fake upstream.Client。
// fake На основании Authorization Заголовок определяет поведение.
func newFakeUpstream(t *testing.T, behavior func(auth string) (status int, body string, isStream bool)) *upstream.Client {
	t.Helper()
	return &upstream.Client{
		HTTP: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			authz := r.Header.Get("Authorization")
			status, body, isStream := behavior(authz)
			ct := "application/json"
			if isStream {
				ct = "text/event-stream"
			}
			return &http.Response{
				StatusCode: status,
				Header: http.Header{"Content-Type": []string{ct}},
				Body: io.NopCloser(strings.NewReader(body)),
			}, nil
		})},
		ChatBaseCN: "https://fake.example",
		BillingBaseCN: "https://fake.example",
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// bindStore Запись зеркального вызова sticky-привязки (без сети), для D4 Сквозная привязка ассертов сходится к итоговому успешному аккаунту.
type bindStore struct {
	redisstore.Noop
	mu sync.Mutex
	binds map[string]string
	delCnt int
}

func newBindStore() *bindStore { return &bindStore{binds: map[string]string{}} }

func (b *bindStore) SetBind(key, uid string, ttl time.Duration) {
	b.mu.Lock()
	b.binds[key] = uid
	b.mu.Unlock()
}
func (b *bindStore) DelBind(key string) {
	b.mu.Lock()
	b.delCnt++
	delete(b.binds, key)
	b.mu.Unlock()
}
func (b *bindStore) LoadBinds() map[string]string {
	b.mu.Lock()
	defer b.mu.Unlock()
	out := map[string]string{}
	for k, v := range b.binds {
		out[k] = v
	}
	return out
}
func (b *bindStore) lastUID(key string) (string, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	u, ok := b.binds[key]
	return u, ok
}

// testPoolWith Построить для всех аккаунтов credits=1000 пула и инжектировать детерминированный источник случайности:
// randInt64N всегда возвращает 0 → pickWeighted кандидат с макс. баллом из обязательного набора (первый).
// это делает зависимость"bad выбран первым"ротационный тест (напр. TestChatRotatesOnHardCredit）полностью детерминировано,
// больше не подвержено взвешенному рандому и flake。
func testPoolWith(auths ...*auth.Auth) *pool.Pool {
	p := pool.New("")
	p.SetRandomSource(func(n int64) int64 { return 0 })
	for _, a := range auths {
		p.Add(a)
		p.SetCredits(a.UID, 1000, 0)
	}
	return p
}

// TestChatLargeBodyNoGatewayLimit Тело запроса без лимита на стороне шлюза (max_body_mb удалено):
// Число MB валидный для body Полное чтение и штатный запрос к апстриму, шлюз больше не 413（Проблемы превышения лимита — на апстрим
// Естественный ответ, выравнивание с апстримом e34cfa4 соглашение).
func TestChatLargeBodyNoGatewayLimit(t *testing.T) {
	var calls int
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		calls++
		return 200, sseOK, true
	})
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	h := NewHandler(Config{Pool: p, Upstream: up})

	pad := strings.Repeat("a", 4<<20) // 4MB 合法 JSON 字符串值
	body := []byte(`{"model":"glm-5.2","messages":[],"pad":"` + pad + `"}`)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions", bytes.NewReader(body)))
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s (large valid body must proceed)", rec.Code, rec.Body)
	}
	if calls != 1 {
		t.Errorf("upstream calls=%d want 1", calls)
	}
}

// TestChatBadParamsRotatesWithoutPenalty апстрим 400 + Unmarshal chat params failed（11101）
// → данный класс относится к ErrBadParams：аккаунт не штрафуется (без кулдауна/Нет блокировок/без учёта circuit breaker/отсутствует errTotal），Но**все равно ротируется**
// （Ретрай со сменой аккаунта может попасть на аккаунт с другими правами). E2E-ассерты bad сбой,good успех, аккаунт цел.
func TestChatBadParamsRotatesWithoutPenalty(t *testing.T) {
	calls := map[string]int{}
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		calls[authz]++
		if authz == "Bearer at-bad" {
			return 400, `{"code":11101,"msg":"Unmarshal chat params failed with error: unexpected EOF"}`, false
		}
		return 200, sseOK, true
	})
	p := testPoolWith(
		&auth.Auth{UID: "bad", AccessToken: "at-bad", ExpiresAt: 9999999999},
		&auth.Auth{UID: "good", AccessToken: "at-good", ExpiresAt: 9999999999},
	)
	p.SetCredits("bad", 2000, 0) // 确定性源 r=0 → 先选 bad
	p.SetCredits("good", 1000, 0)
	h := NewHandler(Config{Pool: p, Upstream: up})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"glm-5.2","messages":[]}`)))
	if rec.Code != 200 {
		t.Fatalf("code=%d body=%s (want 200 after rotate to good)", rec.Code, rec.Body)
	}
	if calls["Bearer at-bad"] != 1 || calls["Bearer at-good"] != 1 {
		t.Errorf("calls=%v want bad/good 各 1 次", calls)
	}
	// аккаунт исправен: без cooldown, без бана, без счетчика circuit breaker, без errTotal。
	st, _ := p.Status("bad")
	if st.Cooling || st.Disabled || st.ErrTotal != 0 || st.BreakerFails != 0 {
		t.Errorf("ErrBadParams must not penalize account: %+v", st)
	}
}

// TestChatAllBadParams503CarriesUpstreamBody Все аккаунты 11101 Время 503 Текст должен содержать
// Исходный upstream 11101 информация (уже не пустая no_healthy_account）。
// текущее состояние — сквозная передача lastErr.Error()（включая апстрим body），данный тест фиксирует это как регрессию.
func TestChatAllBadParams503CarriesUpstreamBody(t *testing.T) {
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		return 400, `{"code":11101,"msg":"Unmarshal chat params failed with error: unexpected EOF"}`, false
	})
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	h := NewHandler(Config{Pool: p, Upstream: up})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"glm-5.2","messages":[]}`)))
	if rec.Code != 503 {
		t.Fatalf("code=%d body=%s (want 503)", rec.Code, rec.Body)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "11101") || !strings.Contains(body, "Unmarshal chat params failed") {
		t.Errorf("503 message should carry upstream 11101 info: %s", body)
	}
}

func TestChatNonStreamAggregates(t *testing.T) {
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		if authz != "Bearer at1" {
			t.Errorf("auth=%q", authz)
		}
		return 200, sseOK, true
	})
	h := NewHandler(Config{
		Pool: testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999}),
		Upstream: up,
	})
	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"glm-5.2","messages":[{"role":"user","content":"hi"}]}`))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
	var resp map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("resp not json: %v body=%s", err, rec.Body)
	}
	if resp["object"] != "chat.completion" {
		t.Errorf("object=%v", resp["object"])
	}
	msg := resp["choices"].([]any)[0].(map[string]any)["message"].(map[string]any)
	if msg["content"] != "你好" {
		t.Errorf("content=%q", msg["content"])
	}
	st, ok := h.cfg.Pool.Status("u1")
	if !ok {
		t.Fatal("account status missing")
	}
	if st.TokenUsage.RequestCount != 1 || st.TokenUsage.UsageCount != 1 ||
		st.TokenUsage.PromptTokens != 1 || st.TokenUsage.CompletionTokens != 1 || st.TokenUsage.TotalTokens != 2 {
		t.Errorf("token usage=%+v", st.TokenUsage)
	}
	if st.TokenUsage.LastLatencyMs < 1 || st.TokenUsage.LastTokensPerSecond == nil || *st.TokenUsage.LastTokensPerSecond <= 0 {
		t.Errorf("latest performance=%+v", st.TokenUsage)
	}
}

func TestChatStreamPassthrough(t *testing.T) {
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		return 200, sseOK, true
	})
	h := NewHandler(Config{
		Pool: testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999}),
		Upstream: up,
	})
	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"glm-5.2","stream":true,"messages":[{"role":"user","content":"hi"}]}`))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("code=%d", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.Contains(ct, "text/event-stream") {
		t.Errorf("ct=%q", ct)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "你好") || !strings.Contains(body, "data: [DONE]") {
		t.Errorf("body=%q", body)
	}
	st, ok := h.cfg.Pool.Status("u1")
	if !ok {
		t.Fatal("account status missing")
	}
	if st.TokenUsage.RequestCount != 1 || st.TokenUsage.UsageCount != 1 ||
		st.TokenUsage.PromptTokens != 1 || st.TokenUsage.CompletionTokens != 1 || st.TokenUsage.TotalTokens != 2 {
		t.Errorf("token usage=%+v", st.TokenUsage)
	}
	if st.TokenUsage.LastLatencyMs < 1 || st.TokenUsage.LastTokensPerSecond == nil || *st.TokenUsage.LastTokensPerSecond <= 0 {
		t.Errorf("latest performance=%+v", st.TokenUsage)
	}
}

func TestChatRecordsCreditForStreamAndSync(t *testing.T) {
	const sseCredit = "data: {\"id\":\"chatcmpl-credit\",\"object\":\"chat.completion.chunk\",\"created\":1753600000,\"model\":\"glm-5.2\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"ok\"}}],\"usage\":{\"prompt_tokens\":4,\"completion_tokens\":6,\"total_tokens\":10,\"credit\":1.25}}\n\n" +
		"data: [DONE]\n\n"
	for _, stream := range []bool{false, true} {
		name := "sync"
		if stream {
			name = "stream"
		}
		t.Run(name, func(t *testing.T) {
			up := newFakeUpstream(t, func(string) (int, string, bool) {
				return 200, sseCredit, true
			})
			rec := usage.New("")
			h := NewHandler(Config{
				Pool: testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999}),
				Upstream: up,
				Usage: rec,
			})
			body := `{"model":"glm-5.2","messages":[]}`
			if stream {
				body = `{"model":"glm-5.2","stream":true,"messages":[]}`
			}
			recorder := httptest.NewRecorder()
			h.ServeHTTP(recorder, httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(body)))
			if recorder.Code != http.StatusOK {
				t.Fatalf("code=%d body=%s", recorder.Code, recorder.Body)
			}
			s := rec.Snapshot(24, nil)
			if s.Totals.Credits != 1.25 || s.Totals.CreditSamples != 1 || s.Totals.CreditTokens != 10 || s.Totals.CreditsPer1MTokens != 125000 {
				t.Fatalf("usage totals = %+v, want credit=1.25 tokens=10 ratio=125000", s.Totals)
			}
			if len(s.CreditByAccount) != 1 || s.CreditByAccount[0].Key != "u1" ||
				len(s.CreditByModel) != 1 || s.CreditByModel[0].Key != "glm-5.2" {
				t.Fatalf("credit dimensions = %+v / %+v", s.CreditByAccount, s.CreditByModel)
			}
		})
	}
}

func TestChatRotatesOnHardCredit(t *testing.T) {
	calls := map[string]int{}
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		calls[authz]++
		if authz == "Bearer at-bad" {
			return 402, `{"code":1,"msg":"余额不足"}`, false
		}
		return 200, sseOK, true
	})
	p := testPoolWith(
		&auth.Auth{UID: "bad", AccessToken: "at-bad", ExpiresAt: 9999999999},
		&auth.Auth{UID: "good", AccessToken: "at-good", ExpiresAt: 9999999999},
	)
	// разрешить bad Сначала выбирается с большим количеством баллов
	p.SetCredits("bad", 2000, 0)
	p.SetCredits("good", 1000, 0)
	h := NewHandler(Config{Pool: p, Upstream: up, SoftCooldown: time.Minute})
	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"glm-5.2","messages":[]}`))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
	if calls["Bearer at-bad"] != 1 || calls["Bearer at-good"] != 1 {
		t.Errorf("calls=%v", calls)
	}
	st, _ := p.Status("bad")
	if !st.Cooling || st.Reason == "" {
		t.Errorf("bad account should be cooling: %+v", st)
	}
	if st.TokenUsage.RequestCount != 1 || st.TokenUsage.UsageCount != 0 {
		t.Errorf("bad token usage=%+v", st.TokenUsage)
	}
	good, _ := p.Status("good")
	if good.TokenUsage.RequestCount != 1 || good.TokenUsage.TotalTokens != 2 {
		t.Errorf("good token usage=%+v", good.TokenUsage)
	}
}

// TestChatSoftCoolsOnRateLimitBody сквозная регрессия issue #28：Апстрим использует не 429 Код состояния
// （400 + текст об ограничении) означает лимит на стороне модели, аккаунт должен перейти в CoolSoft кулдаун, а не просто смена номера.
// До исправления Classify Возврат ErrClient → applyErrorPolicy Ход default ветка только меняет номер без штрафа,
// Аккаунт остаётся в пуле доступных, следующий запрос снова его выберет.
func TestChatSoftCoolsOnRateLimitBody(t *testing.T) {
	calls := map[string]int{}
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		calls[authz]++
		if authz == "Bearer at-bad" {
			return 400, `{"code":1,"msg":"The model provider is rate-limiting requests. Please wait a moment and try again."}`, false
		}
		return 200, sseOK, true
	})
	p := testPoolWith(
		&auth.Auth{UID: "bad", AccessToken: "at-bad", ExpiresAt: 9999999999},
		&auth.Auth{UID: "good", AccessToken: "at-good", ExpiresAt: 9999999999},
	)
	// разрешить bad сначала выбирается с большим количеством очков (с TestChatRotatesOnHardCredit тот же детерминированный приём).
	p.SetCredits("bad", 2000, 0)
	p.SetCredits("good", 1000, 0)
	const soft = 45 * time.Second
	h := NewHandler(Config{Pool: p, Upstream: up, SoftCooldown: soft})

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"glm-5.2","messages":[]}`)))
	if rec.Code != 200 {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
	if calls["Bearer at-bad"] != 1 || calls["Bearer at-good"] != 1 {
		t.Errorf("calls=%v want bad/good 各 1 次", calls)
	}
	st, _ := p.Status("bad")
	if !st.Cooling || st.CoolKind != "soft_rate" {
		t.Fatalf("bad 应进入 soft_rate 冷却: %+v", st)
	}
	// Длительность cooldown берется из инжектированного SoftCooldown，Без зависимости от реального ожидания.
	if max := int64(soft / time.Second); st.CoolRemaining <= 0 || st.CoolRemaining > max {
		t.Errorf("cool_remaining_sec=%d want in (0,%d]", st.CoolRemaining, max)
	}

	// охлаждение активно: один аккаунт нельзя выбрать повторно в период охлаждения.
	before := calls["Bearer at-bad"]
	rec2 := httptest.NewRecorder()
	h.ServeHTTP(rec2, httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"glm-5.2","messages":[]}`)))
	if rec2.Code != 200 {
		t.Fatalf("second code=%d body=%s", rec2.Code, rec2.Body)
	}
	if calls["Bearer at-bad"] != before {
		t.Errorf("冷却中 账号不应再次被选中: calls=%v", calls)
	}
}

func TestApplyErrorPolicySoftRateExponentialBackoff(t *testing.T) {
	// Поглощение семантики апстрима: с распарсенным временем сброса body Синхронизация с wall-clock; без временной метки — ограниченный бэкофф, и
	// **повторные срабатывания во время охлаждения не суммируются**（старый «удвоение каждый раз» как раз доводит весь пул до 2h виновник лимита).
	// Семантика накопления между кулдаунами определяется pool пакет CooldownSoftRate покрытие тестами; здесь якорь handler Боковой
	// форма вызова: подряд 3 ошибок мягкого rate-limit → Впервые 600s，далее не продлевается.
	p := pool.New("")
	p.Add(&auth.Auth{UID: "u1"})
	h := NewHandler(Config{Pool: p, SoftCooldown: 600 * time.Second})

	for i := 1; i <= 3; i++ {
		h.applyErrorPolicy("u1", upstream.ErrSoftRate, "", "", nil)
		st, _ := p.Status("u1")
		if !st.Cooling || st.CoolKind != "soft_rate" {
			t.Fatalf("call %d: 应为 soft_rate 冷却: %+v", i, st)
		}
		if st.SoftStreak != 1 {
			t.Errorf("call %d: 冷却中兜底探测不应推进 soft_streak, got %d", i, st.SoftStreak)
		}
		if st.CoolRemaining < 600-3 || st.CoolRemaining > 600 {
			t.Errorf("call %d: cool_remaining_sec=%d want ~600（不堆加）", i, st.CoolRemaining)
		}
	}
}

func TestApplyErrorPolicyNotFoundUsesFixedBase(t *testing.T) {
	// 404 Разделение: эпизодический апстрим 404 база кулдауна фиксирована 60s（notFoundCooldown），не брать soft_rate
	// 600s база, также не зависит от его настроенного значения.Cooldown после рефакторинга 404 Фиксированный кулдаун
	//（без накопления, без триггера circuit breaker), после успеха — автовосстановление по истечении.
	p := pool.New("")
	p.Add(&auth.Auth{UID: "u1"})
	h := NewHandler(Config{Pool: p, SoftCooldown: 20 * time.Minute}) // soft_rate 配得很大，验证 404 不受其影响

	notFoundSec := int64(notFoundCooldown / time.Second)
	for i := 1; i <= 3; i++ {
		h.applyErrorPolicy("u1", upstream.ErrNotFound, "", "", nil)
		st, _ := p.Status("u1")
		if !st.Cooling || st.CoolKind != "soft_rate" {
			t.Fatalf("call %d: 应为 soft 冷却: %+v", i, st)
		}
		if st.SoftStreak != 0 {
			t.Errorf("call %d: 404 固定冷却不应推进 soft_streak, got %d", i, st.SoftStreak)
		}
		// База берется из notFoundCooldown（60s）а не инжектированный soft_rate（20m），повторный триггер не продлевает.
		if st.CoolRemaining < notFoundSec-3 || st.CoolRemaining > notFoundSec {
			t.Errorf("call %d: 404 cool_remaining_sec=%d want ~%d（固定基数 %ds，非 soft_rate）",
				i, st.CoolRemaining, notFoundSec, notFoundSec)
		}
	}
}

// TestNewHandlerSoftCooldownDefault end-to-end: не инжектировано SoftCooldown база откатывается к 600s（Изначально 60s）。
func TestNewHandlerSoftCooldownDefault(t *testing.T) {
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		if authz == "Bearer at-bad" {
			return 429, `{"code":1,"msg":"rate limit"}`, false
		}
		return 200, sseOK, true
	})
	p := testPoolWith(
		&auth.Auth{UID: "bad", AccessToken: "at-bad", ExpiresAt: 9999999999},
		&auth.Auth{UID: "good", AccessToken: "at-good", ExpiresAt: 9999999999},
	)
	p.SetCredits("bad", 2000, 0)
	p.SetCredits("good", 1000, 0)
	h := NewHandler(Config{Pool: p, Upstream: up}) // 不注入 SoftCooldown

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"glm-5.2","messages":[]}`)))
	if rec.Code != 200 {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
	st, _ := p.Status("bad")
	if !st.Cooling || st.CoolKind != "soft_rate" {
		t.Fatalf("bad 应进入 soft_rate 冷却: %+v", st)
	}
	if st.CoolRemaining <= 599 || st.CoolRemaining > 600 {
		t.Errorf("default soft cooldown cool_remaining_sec=%d want 600", st.CoolRemaining)
	}
}

// TestChatStickyFollowsFinalSuccess E2E-верификация D4：После неудачи sticky-номера и успеха при смене номера привязка сессии сходится к успешному номеру.
func TestChatStickyFollowsFinalSuccess(t *testing.T) {
	st := newBindStore()
	sess := session.New(session.Config{
		TTL: time.Minute,
		Store: st,
		Available: func() []string { return []string{"bad", "good"} },
	})
	p := testPoolWith(
		&auth.Auth{UID: "bad", AccessToken: "at-bad", ExpiresAt: 9999999999},
		&auth.Auth{UID: "good", AccessToken: "at-good", ExpiresAt: 9999999999},
	)
	// Предварительно привязать сессию к bad（эмуляция исторической sticky-привязки),bad сбой,good Успех → привязку переключить на good。
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		if authz == "Bearer at-bad" {
			return 500, `{"code":500}`, false
		}
		return 200, sseOK, true
	})
	h := NewHandler(Config{
		Pool: p,
		Upstream: up,
		Session: sess,
		SoftCooldown: time.Minute,
	})
	// Предварительная привязка:sess.Bind("conv-1", "bad")，затем тело запроса с тем же conversation_id。
	sess.Bind("conv-1", "bad")
	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"glm-5.2","messages":[],"metadata":{"conversation_id":"conv-1"}}`))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
	// привязка должна сходиться к итоговому успешному good。
	if uid, ok := st.lastUID("conv-1"); !ok || uid != "good" {
		t.Fatalf("sticky binding should follow final success to good, got %s ok=%v (binds=%v)", uid, ok, st.binds)
	}
}

// TestChatStickySuccessKeepsBinding Липкий аккаунт — сразу успех → привязка неизменна (по-прежнему этот номер).
func TestChatStickySuccessKeepsBinding(t *testing.T) {
	st := newBindStore()
	sess := session.New(session.Config{
		TTL: time.Minute,
		Store: st,
		Available: func() []string { return []string{"good"} },
	})
	p := testPoolWith(&auth.Auth{UID: "good", AccessToken: "at-good", ExpiresAt: 9999999999})
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		return 200, sseOK, true
	})
	h := NewHandler(Config{Pool: p, Upstream: up, Session: sess, SoftCooldown: time.Minute})
	sess.Bind("conv-1", "good")
	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"glm-5.2","messages":[],"metadata":{"conversation_id":"conv-1"}}`))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
	if uid, ok := st.lastUID("conv-1"); !ok || uid != "good" {
		t.Fatalf("binding should stay good after success, got %s ok=%v", uid, ok)
	}
}

// TestChatStickyFullFallsBackToRotation E2E-верификация C3 Семантика: когда sticky-аккаунты переполнены и недоступны,
// Запрос в том же раунде отвязывается и откатывается к обычной ротации с выбором здорового аккаунта, привязка сходится к итоговому успешному номеру — без холостого раунда.
func TestChatStickyFullFallsBackToRotation(t *testing.T) {
	st := newBindStore()
	sess := session.New(session.Config{
		TTL: time.Minute,
		Store: st,
		Available: func() []string { return []string{"bad", "good"} },
	})
	p := testPoolWith(
		&auth.Auth{UID: "bad", AccessToken: "at-bad", ExpiresAt: 9999999999},
		&auth.Auth{UID: "good", AccessToken: "at-good", ExpiresAt: 9999999999},
	)
	// bad занимает единственный in-flight слот:PickByUID Вернет nil（healthy Но inFlight заполнено)→ Отвязать + откат с ротацией.
	p.SetMaxInFlight(1)
	p.Acquire("bad")

	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		return 200, sseOK, true
	})
	h := NewHandler(Config{
		Pool: p,
		Upstream: up,
		Session: sess,
		SoftCooldown: time.Minute,
	})
	sess.Bind("conv-1", "bad")
	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"glm-5.2","messages":[],"metadata":{"conversation_id":"conv-1"}}`))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
	// Перегруженный sticky-номер отвязан, привязка сходится к итоговому успешному номеру good。
	if uid, ok := st.lastUID("conv-1"); !ok || uid != "good" {
		t.Fatalf("sticky binding should fall back to good, got %s ok=%v (binds=%v)", uid, ok, st.binds)
	}
	p.Release("bad")
}

func TestChatHardCreditCooldownUntilNextDay4AM(t *testing.T) {
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		if authz == "Bearer at-bad" {
			return 402, `{"code":1,"msg":"余额不足"}`, false
		}
		return 200, sseOK, true
	})
	p := testPoolWith(
		&auth.Auth{UID: "bad", AccessToken: "at-bad", ExpiresAt: 9999999999},
		&auth.Auth{UID: "good", AccessToken: "at-good", ExpiresAt: 9999999999},
	)
	p.SetCredits("bad", 2000, 0) // bad 积分高，确定性源 → 先被选中
	p.SetCredits("good", 1000, 0)
	h := NewHandler(Config{Pool: p, Upstream: up})
	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"glm-5.2","messages":[]}`))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
	st, ok := p.Status("bad")
	if !ok || !st.Cooling {
		t.Fatalf("bad should be cooling: %+v ok=%v", st, ok)
	}
	if st.Reason != "Недостаточно средств" {
		t.Errorf("reason=%q", st.Reason)
	}
	// Жесткий кулдаун кредита только на следующий день 04:00，а не фиксированный 12h/Настроенная длительность.
	if st.Until.Hour() != 4 {
		t.Errorf("until hour=%d want 4 (next-day 04:00)", st.Until.Hour())
	}
	// До след. дня 04:00 Максимум 28h（раннее утро 00:00~04:00 межвыполнение now→на следующий день 04:00 Диапазон > 24h，считается нормой).
	if d := time.Until(st.Until); d <= 0 || d > 28*time.Hour {
		t.Errorf("until %v not within (0,28h]: %v", st.Until, d)
	}
	// Мгновенная смена аккаунта успешна:good выбрано.
	stGood, _ := p.Status("good")
	if stGood.Cooling || stGood.Disabled {
		t.Errorf("good should stay healthy: %+v", stGood)
	}
}

func TestChat429Code14018UsesHardCreditCooldown(t *testing.T) {
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		if authz == "Bearer at-bad" {
			return 429, `{"code":14018,"msg":"Credits exhausted"}`, false
		}
		return 200, sseOK, true
	})
	p := testPoolWith(
		&auth.Auth{UID: "bad", AccessToken: "at-bad", ExpiresAt: 9999999999},
		&auth.Auth{UID: "good", AccessToken: "at-good", ExpiresAt: 9999999999},
	)
	p.SetCredits("bad", 2000, 2000)
	p.SetCredits("good", 1000, 1000)
	h := NewHandler(Config{Pool: p, Upstream: up, SoftCooldown: time.Minute})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"glm-5.2","messages":[]}`)))
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body.String())
	}
	st, ok := p.Status("bad")
	if !ok || !st.Cooling {
		t.Fatalf("14018 account should be cooling: %+v ok=%v", st, ok)
	}
	if st.Reason != "Недостаточно средств" || st.Until.Hour() != 4 {
		t.Fatalf("14018 should use hard-credit cooldown, got reason=%q until=%v", st.Reason, st.Until)
	}
	if got := p.Pick(); got == nil || got.UID != "good" {
		t.Fatalf("hard-cooled 14018 account must not be fallback-picked, got %+v", got)
	}
}

// TestChat6004ModelResetCoolsToParsedTime сквозная регрессия issue #31：апстрим 429 + code 6004
// +「Будет … сброс»→ Охлаждение until Точно равно времени парсинга (а не 600s Фиксированная база/экспоненциальный бэкофф),
// и логирование модели-триггера → Запросы той же модели остаются в охлаждении, запросы со сменой модели — опционально по exemption.
func TestChat6004ModelResetCoolsToParsedTime(t *testing.T) {
	// Использовать future 5 минут времени сброса (wall-clock）Формирование ответа апстрима.
	reset := time.Now().Add(5 * time.Minute)
	ts := reset.In(upstream.SoftRateResetLoc()).Format("2006-01-02 15:04:05")
	var calls int
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		calls++
		if authz == "Bearer at-bad" {
			return 429, `{"code":6004,"msg":"将在 ` + ts + ` UTC+8 重置"}`, false
		}
		return 200, sseOK, true
	})
	p := testPoolWith(
		&auth.Auth{UID: "bad", AccessToken: "at-bad", ExpiresAt: 9999999999},
		&auth.Auth{UID: "good", AccessToken: "at-good", ExpiresAt: 9999999999},
	)
	p.SetCredits("bad", 2000, 0)
	p.SetCredits("good", 1000, 0)
	// Изоляция для breaker помехи: порог срабатывания по умолчанию 3，один фейл не триггерит.
	h := NewHandler(Config{Pool: p, Upstream: up})
	req := httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"glm-5.3","messages":[]}`))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("code=%d body=%s (want 200 after rotate to good)", rec.Code, rec.Body)
	}
	// bad уже вошел 6004 Независимый кулдаун на уровне модели: на уровне аккаунта не cooling，Строка реестра until ≈ reset。
	st, _ := p.Status("bad")
	if st.Cooling {
		t.Fatalf("6004-with-reset should NOT set account-level cooling: %+v", st)
	}
	if len(st.RateLimitedModels) != 1 || st.RateLimitedModels[0].Model != "glm-5.3" {
		t.Fatalf("want single model ledger row glm-5.3: %+v", st.RateLimitedModels)
	}
	if d := st.RateLimitedModels[0].Until.Sub(reset); d < -time.Second || d > time.Second {
		t.Errorf("model until=%v want ~reset=%v (diff %v)", st.RateLimitedModels[0].Until, reset, d)
	}
	// Модель-триггер записи (bad внутри пула private Поле должно пройти через Status невидимо, использовать поведенческий assert):
	// Та же модель glm-5.3 запросы не должны выбираться bad（всё ещё в кулдауне);
	// Разные модели hy3-x запросы должны исключаться из выбора охлаждения bad（максимальный балл).
	p.SetRandomSource(func(n int64) int64 { return 0 })
	same := p.PickExcludingForModel(nil, "glm-5.3")
	if same == nil || same.UID != "good" {
		t.Fatalf("same-model pick should skip bad (still cooling), got %+v", same)
	}
	diff := p.PickExcludingForModel(nil, "hy3-x")
	if diff == nil || diff.UID != "bad" {
		t.Fatalf("different-model pick should bypass bad soft cooling, got %+v", diff)
	}
}

// TestChat6004WithoutResetFallsBackToBackoff 6004 без временной метки → Откатить 600s Мягкий кулдаун по кардинальности
// （состояние без изменений).
func TestChat6004WithoutResetFallsBackToBackoff(t *testing.T) {
	var calls int
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		calls++
		if authz == "Bearer at-bad" {
			return 429, `{"code":6004,"msg":"model usage limit exceeded"}`, false
		}
		return 200, sseOK, true
	})
	p := testPoolWith(
		&auth.Auth{UID: "bad", AccessToken: "at-bad", ExpiresAt: 9999999999},
		&auth.Auth{UID: "good", AccessToken: "at-good", ExpiresAt: 9999999999},
	)
	p.SetCredits("bad", 2000, 0)
	p.SetCredits("good", 1000, 0)
	h := NewHandler(Config{Pool: p, Upstream: up, SoftCooldown: time.Minute})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"glm-5.3","messages":[]}`)))
	if rec.Code != 200 {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
	st, _ := p.Status("bad")
	if !st.Cooling || st.CoolKind != "soft_rate" {
		t.Fatalf("bad should be soft cooling: %+v", st)
	}
	// Длительность cooldown = инжект soft База(60s)，Вне времени парсинга (без текста сброса).
	if st.CoolRemaining <= 0 || st.CoolRemaining > 60 {
		t.Errorf("cool_remaining_sec=%d want ~60 (soft base, not parsed)", st.CoolRemaining)
	}
	if len(st.RateLimitedModels) != 1 || st.RateLimitedModels[0].Model != "glm-5.3" || st.RateLimitedModels[0].Kind != "rate_limit" {
		t.Fatalf("rate-limited models=%+v, want audit row for glm-5.3", st.RateLimitedModels)
	}
}

func TestChatAllUnavailableReturns503(t *testing.T) {
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		return 402, `{"code":1,"msg":"余额不足"}`, false
	})
	h := NewHandler(Config{
		Pool: testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999}),
		Upstream: up,
	})
	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"glm-5.2","messages":[]}`))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 503 {
		t.Errorf("code=%d body=%s", rec.Code, rec.Body)
	}
	var e map[string]any
	json.Unmarshal(rec.Body.Bytes(), &e)
	if e["error"] == nil {
		t.Errorf("want error envelope: %s", rec.Body)
	}
}

func TestChatSessionDeadDisables(t *testing.T) {
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		return 401, `{"code":12153,"msg":"Offline user session not found"}`, false
	})
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	h := NewHandler(Config{Pool: p, Upstream: up})
	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"glm-5.2","messages":[]}`))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 503 {
		t.Errorf("code=%d", rec.Code)
	}
	st, _ := p.Status("u1")
	if !st.Disabled {
		t.Errorf("account should be disabled: %+v", st)
	}
}

func TestChatTransportErrorDoesNotPenalize(t *testing.T) {
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	up := &upstream.Client{
		HTTP: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			return nil, errors.New("connection refused")
		})},
		ChatBaseCN: "https://fake.example",
		BillingBaseCN: "https://fake.example",
	}
	// ошибки транспорта не инкрементят счётчик circuit breaker: один transport error не должно суммироваться errTotal также не должен триггерить circuit breaker.
	h := NewHandler(Config{Pool: p, Upstream: up})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"glm-5.2","messages":[]}`)))
	if rec.Code != 503 {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
	st, _ := p.Status("u1")
	if st.Cooling || st.ErrTotal != 0 {
		t.Fatalf("transport error should not penalize account: %+v", st)
	}
}

func TestChatHTTP5xxPenalizes(t *testing.T) {
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	// Порог circuit breaker 1：Один раз 5xx сразу триггерит熔断 (семантика последовательных ошибок объединена в熔断器).
	p.SetBreaker(1, time.Hour, time.Hour)
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		return 500, `{"code":500}`, false
	})
	h := NewHandler(Config{Pool: p, Upstream: up})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"glm-5.2","messages":[]}`)))
	if rec.Code != 503 {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
	st, _ := p.Status("u1")
	if !st.Cooling {
		t.Fatalf("http 5xx should trip breaker (cooling) with threshold=1: %+v", st)
	}
}

func TestChatHTTP4xxClientDoesNotPenalize(t *testing.T) {
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		return 400, `{"code":400,"msg":"bad request"}`, false
	})
	h := NewHandler(Config{Pool: p, Upstream: up})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"glm-5.2","messages":[]}`)))
	if rec.Code != 503 {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
	st, _ := p.Status("u1")
	if st.Cooling || st.ErrTotal != 0 {
		t.Fatalf("generic 4xx should not penalize account: %+v", st)
	}
}

func TestModelsEndpoint(t *testing.T) {
	h := NewHandler(Config{Pool: testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at", ExpiresAt: 9999999999}), Upstream: upstream.New()})
	req := httptest.NewRequest("GET", "/v1/models", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("code=%d", rec.Code)
	}
	var resp map[string]any
	json.Unmarshal(rec.Body.Bytes(), &resp)
	if resp["object"] != "list" {
		t.Errorf("object=%v", resp["object"])
	}
	data := resp["data"].([]any)
	if len(data) < 5 {
		t.Errorf("models count=%d", len(data))
	}
	found := false
	for _, m := range data {
		if m.(map[string]any)["id"] == "cn:glm-5.2" {
			found = true
		}
	}
	if !found {
		t.Error("cn:glm-5.2 missing")
	}
}

func TestModelsDynamic(t *testing.T) {
	// очистить кэш
	dynamicModelsCache.Lock()
	dynamicModelsCache.ids = nil
	dynamicModelsCache.fetched = time.Time{}
	dynamicModelsCache.lastFail = time.Time{}
	dynamicModelsCache.Unlock()

	// фейковый апстрим возвращает динамическую модель (вкл. agents + maxInputTokens/maxOutputTokens + reasoning уровень)
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		return 200, `{"code":0,"data":{"models":[{"id":"dyn-model-a","maxInputTokens":65536,"maxOutputTokens":8192,"reasoning":{"effort":"medium","supportedEfforts":["low","medium","high"]}},{"id":"dyn-model-b","maxInputTokens":131072,"maxOutputTokens":16384},{"id":"glm-9.9","maxInputTokens":262144,"maxOutputTokens":32768}],"agents":[{"name":"cli","models":["dyn-model-a","dyn-model-b","glm-9.9"]}]}}`, false
	})
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	h := NewHandler(Config{Pool: p, Upstream: up})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/v1/models", nil))
	if rec.Code != 200 {
		t.Fatalf("code=%d", rec.Code)
	}
	var resp map[string]any
	json.Unmarshal(rec.Body.Bytes(), &resp)
	data := resp["data"].([]any)
	if len(data) != 3 {
		t.Fatalf("want 3 dynamic models, got %d: %v", len(data), data)
	}
	ids := map[string]bool{}
	for _, m := range data {
		ids[m.(map[string]any)["id"].(string)] = true
	}
	if !ids["cn:dyn-model-a"] || !ids["cn:glm-9.9"] {
		t.Errorf("dynamic ids missing: %v", ids)
	}

	// Маппинг полей assert:maxInputTokens → context_length，maxOutputTokens → max_output_tokens
	for _, m := range data {
		mm := m.(map[string]any)
		switch mm["id"] {
		case "dyn-model-a":
			if mm["context_length"].(float64) != 65536 {
				t.Errorf("dyn-model-a context_length=%v want 65536", mm["context_length"])
			}
			if mm["max_output_tokens"].(float64) != 8192 {
				t.Errorf("dyn-model-a max_output_tokens=%v want 8192", mm["max_output_tokens"])
			}
			// reasoning Отображение тарифа:supported_efforts + default_effort
			efforts, _ := mm["supported_efforts"].([]any)
			if len(efforts) != 3 || efforts[0] != "low" {
				t.Errorf("dyn-model-a supported_efforts=%v", mm["supported_efforts"])
			}
			if mm["default_effort"] != "medium" {
				t.Errorf("dyn-model-a default_effort=%v want medium", mm["default_effort"])
			}
		case "dyn-model-b":
			// Апстрим не вернул ответ reasoning → Оба поля тарифа опущены (клиент использует значения по умолчанию)
			if _, has := mm["supported_efforts"]; has {
				t.Errorf("dyn-model-b supported_efforts should be omitted, got %v", mm["supported_efforts"])
			}
			if _, has := mm["default_effort"]; has {
				t.Errorf("dyn-model-b default_effort should be omitted")
			}
		case "glm-9.9":
			if mm["context_length"].(float64) != 262144 {
				t.Errorf("glm-9.9 context_length=%v want 262144", mm["context_length"])
			}
			if mm["max_output_tokens"].(float64) != 32768 {
				t.Errorf("glm-9.9 max_output_tokens=%v want 32768", mm["max_output_tokens"])
			}
		}
	}

	// Второй вызов — из кэша (успех даже при отключенном апстриме)
	dynamicModelsCache.RLock()
	cached := len(dynamicModelsCache.ids)
	dynamicModelsCache.RUnlock()
	if cached != 3 {
		t.Errorf("cache not populated: %d", cached)
	}
}

func TestModelsDynamicFallsBackToStatic(t *testing.T) {
	// Чисто динамический (решение продукта): сбой апстрима → Пустой список (200），Без отката к статической таблице —
	// не удалось вытянуть каталог — значит upstream недоступен, фейковый список заставит клиента выбрать 11102 модели.
	dynamicModelsCache.Lock()
	dynamicModelsCache.ids = nil
	dynamicModelsCache.fetched = time.Time{}
	dynamicModelsCache.lastFail = time.Time{}
	dynamicModelsCache.Unlock()

	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		return 500, `boom`, false
	})
	h := NewHandler(Config{Pool: p, Upstream: up})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/v1/models", nil))
	if rec.Code != 200 {
		t.Fatalf("code=%d", rec.Code)
	}
	var resp map[string]any
	json.Unmarshal(rec.Body.Bytes(), &resp)
	data := resp["data"].([]any)
	if len(data) != 0 {
		t.Errorf("pure-dynamic failure should yield empty list, got %d", len(data))
	}
}

func TestModelsFetchFailurePenalizesAccount(t *testing.T) {
	// Поглотить апстрим 9832283：models ошибка получения — только в негативный кэш, не NoteError——
	// NoteError подаётся chat Circuit breaker,models Эндпоинт 5xx Кросс-штраф chat Здоровый номер канала.
	dynamicModelsCache.Lock()
	dynamicModelsCache.ids = nil
	dynamicModelsCache.fetched = time.Time{}
	dynamicModelsCache.lastFail = time.Time{}
	dynamicModelsCache.Unlock()

	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	p.SetBreaker(1, time.Hour, time.Hour) // 熔断阈值 1：若仍罚号，一次 fetch 失败即熔断
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		return 500, `boom`, false
	})
	h := NewHandler(Config{Pool: p, Upstream: up})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/v1/models", nil))
	if rec.Code != 200 {
		t.Fatalf("code=%d", rec.Code)
	}
	st, _ := p.Status("u1")
	if st.Cooling {
		t.Fatalf("models fetch failure must not trip chat breaker: %+v", st)
	}
}

func TestModelsNegativeCacheOnFetchFailure(t *testing.T) {
	dynamicModelsCache.Lock()
	dynamicModelsCache.ids = nil
	dynamicModelsCache.fetched = time.Time{}
	dynamicModelsCache.lastFail = time.Time{}
	dynamicModelsCache.Unlock()

	var calls atomic.Int32 // FetchModels 企业/v3 两路并发探测回调，计数须原子
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		calls.Add(1)
		return 500, `boom`, false
	})
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	h := NewHandler(Config{Pool: p, Upstream: up})

	// Непрерывный 3 запросов, апстрим продолжает 500 → Должен срабатывать только 1 Раз fetch（отрицательный кэш активен),
	// в чисто динамическом режиме пустой список всё равно возвращается 200。
	for i := 0; i < 3; i++ {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest("GET", "/v1/models", nil))
		if rec.Code != 200 {
			t.Fatalf("req %d: code=%d body=%s", i, rec.Code, rec.Body)
		}
	}
	// Один раунд зондирования = 2 вызовов upstream (enterprise-эндпоинт + /v3/config конкурентность, негативный кэш только при отказе обоих путей).
	if got := calls.Load(); got != 2 {
		t.Errorf("want 2 probes (console + v3), got %d", got)
	}

	// Окончание периода кулдауна (отмотать timestamp неудачи назад 10 минут назад)→ следует повторно fetch。
	dynamicModelsCache.Lock()
	dynamicModelsCache.lastFail = time.Now().Add(-10 * time.Minute)
	dynamicModelsCache.Unlock()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/v1/models", nil))
	if rec.Code != 200 {
		t.Fatalf("after cooldown: code=%d", rec.Code)
	}
	if got := calls.Load(); got != 4 {
		t.Errorf("want 4 probes after cooldown (2 rounds x 2), got %d", got)
	}
}

func TestAPIKeyAuth(t *testing.T) {
	h := NewHandler(Config{
		Pool: testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at", ExpiresAt: 9999999999}),
		Upstream: upstream.New(),
		APIKey: "secret",
	})
	// отсутствует key
	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{}`))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 401 {
		t.Errorf("no key: code=%d", rec.Code)
	}
	// Ошибка key
	req = httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{}`))
	req.Header.Set("Authorization", "Bearer wrong")
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 401 {
		t.Errorf("wrong key: code=%d", rec.Code)
	}
	// Для key（запрос всё равно уйдёт в апстрим, но здесь апстрим client завершится ошибкой —— если только не 401 достаточно)
	req = httptest.NewRequest("GET", "/v1/models", nil)
	req.Header.Set("Authorization", "Bearer secret")
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Errorf("right key: code=%d", rec.Code)
	}
}

func TestStatusEndpoint(t *testing.T) {
	p := testPoolWith(&auth.Auth{UID: "u1", Nickname: "nick", AccessToken: "at", ExpiresAt: 9999999999})
	p.SetCredits("u1", 42, 0)
	h := NewHandler(Config{Pool: p, Upstream: upstream.New()})
	req := httptest.NewRequest("GET", "/status", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("code=%d", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, `"uid":"u1"`) || !strings.Contains(body, `"credits":42`) {
		t.Errorf("body=%s", body)
	}
	if strings.Contains(body, "AccessToken") || strings.Contains(body, `"at"`) {
		t.Error("token leaked in status output")
	}
	// Phase 3 агрегирующее поле.
	var statusBody map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &statusBody); err != nil {
		t.Fatalf("status not json: %v", err)
	}
	if statusBody["total"] != float64(1) || statusBody["healthy"] != float64(1) ||
		statusBody["cooling"] != float64(0) || statusBody["disabled"] != float64(0) ||
		statusBody["in_flight_full"] != float64(0) {
		t.Errorf("summary=%v want total=1 healthy=1 cooling=0 disabled=0 in_flight_full=0", statusBody)
	}
	// Phase v3：Уровень пула sticky_sessions + redis_mode。
	if statusBody["sticky_sessions"] != float64(0) {
		t.Errorf("sticky_sessions=%v want 0", statusBody["sticky_sessions"])
	}
	if statusBody["redis_mode"] != "noop" {
		t.Errorf("redis_mode=%v want noop", statusBody["redis_mode"])
	}
}

// TestStatusInFlightFull /status выводить счетчик заполненности:healthy и число аккаунтов с незавершёнными запросами.
func TestStatusInFlightFull(t *testing.T) {
	p := testPoolWith(
		&auth.Auth{UID: "full", AccessToken: "at", ExpiresAt: 9999999999},
		&auth.Auth{UID: "free", AccessToken: "at", ExpiresAt: 9999999999},
	)
	p.SetMaxInFlight(1)
	p.Acquire("full")
	defer p.Release("full")

	h := NewHandler(Config{Pool: p, Upstream: upstream.New()})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/status", nil))
	if rec.Code != 200 {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
	var statusBody map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &statusBody); err != nil {
		t.Fatalf("status not json: %v", err)
	}
	if statusBody["in_flight_full"] != float64(1) {
		t.Errorf("in_flight_full=%v want 1", statusBody["in_flight_full"])
	}
	if statusBody["healthy"] != float64(2) {
		t.Errorf("healthy=%v want 2 (full is still healthy by state-machine semantics)", statusBody["healthy"])
	}
}

func TestStatusPortraitFields(t *testing.T) {
	// Phase 3：/status Для одиночного аккаунта нужно вернуть поля health-профиля.
	p := testPoolWith(&auth.Auth{UID: "u1", Nickname: "nick", AccessToken: "at", ExpiresAt: 9999999999})
	p.NoteSuccess("u1")
	p.NoteSuccess("u1")
	p.NoteError("u1") // 记录 last_err + err_total（累计，不冷却）
	p.Cooldown("u1", pool.CoolSoft, time.Hour, "429 rate limit")
	h := NewHandler(Config{Pool: p, Upstream: upstream.New()})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/status", nil))
	if rec.Code != 200 {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
	var body struct {
		Accounts []pool.Status `json:"accounts"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("status not json: %v", err)
	}
	if len(body.Accounts) != 1 {
		t.Fatalf("accounts=%d", len(body.Accounts))
	}
	st := body.Accounts[0]
	if !st.Cooling || st.CoolKind != "soft_rate" || st.CoolRemaining <= 0 {
		t.Errorf("cooling portrait=%+v", st)
	}
	if st.SuccessCount != 2 {
		t.Errorf("success_count=%d want 2", st.SuccessCount)
	}
	if st.ErrTotal != 1 {
		t.Errorf("err_total=%d want 1", st.ErrTotal)
	}
	if st.LastSuccessTime.IsZero() {
		t.Error("last_success should be set")
	}
	if st.LastErrTime.IsZero() {
		t.Error("last_err should be set")
	}
}

// TestHealthzEmptyPool Пустой пул (healthy=0）→ 503，означает временно недоступен.
func TestHealthzEmptyPool(t *testing.T) {
	h := NewHandler(Config{Pool: pool.New(""), Upstream: upstream.New()})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/healthz", nil))
	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("code=%d want 503 (healthy=0)", rec.Code)
	}
	var resp map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("healthz not json: %v body=%s", err, rec.Body)
	}
	if resp["healthy"] != float64(0) || resp["total"] != float64(0) {
		t.Errorf("healthz json=%v", resp)
	}
}

// TestHealthz503WhenNoHealthy Все аккаунты отключены/Охлаждение → 503。
func TestHealthz503WhenNoHealthy(t *testing.T) {
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at", ExpiresAt: 9999999999})
	p.Disable("u1", "session dead")
	h := NewHandler(Config{Pool: p, Upstream: upstream.New()})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/healthz", nil))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("code=%d want 503", rec.Code)
	}
	ct := rec.Header().Get("Content-Type")
	if !strings.Contains(ct, "application/json") {
		t.Errorf("ct=%q want json", ct)
	}
	var resp map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("healthz not json: %v body=%s", err, rec.Body)
	}
	if resp["healthy"] != float64(0) || resp["total"] != float64(1) {
		t.Errorf("healthz json=%v want healthy=0 total=1", resp)
	}
}

// TestHealthz503WhenAllInFlightFull Все аккаунты healthy но все занимают in-flight → 503（и chat в той же метрике),
// И healthy Семантика не изменилась (по-прежнему 1）：Полная загрузка — не изменение health-измерения стейт-машины, а отдельное наложение метрики liveness.
func TestHealthz503WhenAllInFlightFull(t *testing.T) {
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at", ExpiresAt: 9999999999})
	p.SetMaxInFlight(1)
	p.Acquire("u1") // 占满唯一在途名额
	defer p.Release("u1")

	if p.ServableNow() {
		t.Fatal("servable should be false when the only healthy account is in-flight full")
	}
	h := NewHandler(Config{Pool: p, Upstream: upstream.New()})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/healthz", nil))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("code=%d want 503 (healthy but all in-flight full)", rec.Code)
	}
	var resp map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("healthz not json: %v body=%s", err, rec.Body)
	}
	// healthy семантика не изменилась: аккаунт всё ещё healthy（занимает только in-flight, не кулдаун/отключено).
	if resp["healthy"] != float64(1) || resp["total"] != float64(1) {
		t.Errorf("healthz json=%v want healthy=1 total=1 (healthy semantics unchanged)", resp)
	}
}

// TestHealthz200WhenHealthy Есть рабочие аккаунты → 200。
func TestHealthz200WithHealthy(t *testing.T) {
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at", ExpiresAt: 9999999999})
	h := NewHandler(Config{Pool: p, Upstream: upstream.New()})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/healthz", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d want 200", rec.Code)
	}
	var resp map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("healthz not json: %v body=%s", err, rec.Body)
	}
	if resp["healthy"] != float64(1) || resp["total"] != float64(1) {
		t.Errorf("healthz json=%v want healthy=1 total=1", resp)
	}
}

// TestHealthzServiceIdentity /healthz независимо от 200 или 503 все должны содержать идентификатор шлюза
// （тело ответа service Поле + X-Service заголовок): проба хоста попадает на старый сервис того же порта/при других сервисах,
// Даже если другая сторона вернёт 2xx также не несет этот флаг, хост по нему определяет"Ложный успех"。
func TestHealthzServiceIdentity(t *testing.T) {
	cases := []struct {
		name string
		setup func(*pool.Pool)
		wantCode int
	}{
		{"healthy", func(*pool.Pool) {}, http.StatusOK},
		{"unhealthy", func(p *pool.Pool) { p.Disable("u1", "session dead") }, http.StatusServiceUnavailable},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at", ExpiresAt: 9999999999})
			tc.setup(p)
			h := NewHandler(Config{Pool: p, Upstream: upstream.New()})
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, httptest.NewRequest("GET", "/healthz", nil))
			if rec.Code != tc.wantCode {
				t.Fatalf("code=%d want %d", rec.Code, tc.wantCode)
			}
			if got := rec.Header().Get("X-Service"); got != ServiceName {
				t.Errorf("X-Service=%q want %q", got, ServiceName)
			}
			var resp map[string]any
			if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
				t.Fatalf("healthz not json: %v body=%s", err, rec.Body)
			}
			if resp["service"] != ServiceName {
				t.Errorf("service=%v want %q", resp["service"], ServiceName)
			}
		})
	}
}

// TestHealthzServiceIdentityWithoutAuth /healthz оставить без аутентификации (friendly к балансировщику):
// настроено api_key также не требуется Bearer，Поля идентификации возвращаются как обычно.
func TestHealthzServiceIdentityWithoutAuth(t *testing.T) {
	h := NewHandler(Config{
		Pool: testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at", ExpiresAt: 9999999999}),
		Upstream: upstream.New(),
		APIKey: "secret",
	})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/healthz", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("healthz must stay unauthenticated: code=%d", rec.Code)
	}
	if got := rec.Header().Get("X-Service"); got != ServiceName {
		t.Errorf("X-Service=%q want %q", got, ServiceName)
	}
}

func TestStatusRequiresAuth(t *testing.T) {
	p := testPoolWith(&auth.Auth{UID: "u1", Nickname: "nick", AccessToken: "at", ExpiresAt: 9999999999})
	h := NewHandler(Config{Pool: p, Upstream: upstream.New(), APIKey: "secret"})

	// отсутствует token → 401
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/status", nil))
	if rec.Code != 401 {
		t.Errorf("no token: code=%d", rec.Code)
	}

	// Лента token → 200
	rec = httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/status", nil)
	req.Header.Set("Authorization", "Bearer secret")
	h.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Errorf("with token: code=%d", rec.Code)
	}

	// /healthz без аутентификации всё равно 200
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/healthz", nil))
	if rec.Code != 200 {
		t.Errorf("healthz: code=%d", rec.Code)
	}
}

// TestContentBlockedTriggersDegradedRetry passthrough Первый запрос в режиме 400（11128 текст)
// → повтор с даунгрейдом (Degraded）→ 200，Незаметно для клиента. Проверка второго исходящего body для Degraded。
func TestContentBlockedTriggersDegradedRetry(t *testing.T) {
	// Логировать тело каждого исходящего запроса, assert что второй — Degraded текст.
	var bodies [][]byte
	up := &upstream.Client{
		HTTP: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			raw, _ := io.ReadAll(r.Body)
			bodies = append(bodies, raw)
			// впервые (body содержит исходный system）вернуть 400 Блокировка контента; последующий возврат 200。
			if len(bodies) == 1 {
				return &http.Response{
					StatusCode: 400,
					Header: http.Header{"Content-Type": []string{"application/json"}},
					Body: io.NopCloser(strings.NewReader(`{"code":11128,"msg":"blocked by security policy"}`)),
				}, nil
			}
			return &http.Response{
				StatusCode: 200,
				Header: http.Header{"Content-Type": []string{"text/event-stream"}},
				Body: io.NopCloser(strings.NewReader(sseOK)),
			}, nil
		})},
		ChatBaseCN: "https://fake.example",
	}
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	h := NewHandler(Config{Pool: p, Upstream: up, PromptMode: "passthrough"})

	req := httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"glm-5.2","stream":true,"messages":[{"role":"system","content":"原始指纹"},{"role":"user","content":"hi"}]}`))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("code=%d body=%s (want 200 after degraded retry)", rec.Code, rec.Body)
	}
	if len(bodies) != 2 {
		t.Fatalf("want 2 upstream calls (first 400 + retry), got %d", len(bodies))
	}
	// второй выход body messages заголовок system Содержимое должно быть Degraded текст.
	if !strings.Contains(string(bodies[1]), prompt.Degraded) {
		t.Errorf("second body should contain Degraded prompt: %s", bodies[1])
	}
	if strings.Contains(string(bodies[1]), "原始指纹") {
		t.Errorf("second body should not contain original system: %s", bodies[1])
	}
}

// TestContentBlockedStickyDegraded после даунгрейда новый запрос идёт напрямую в Degraded（Больше не проверять сначала 400）。
func TestContentBlockedStickyDegraded(t *testing.T) {
	var firstCall bool
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		if !firstCall {
			firstCall = true
			return 400, `{"code":11128,"msg":"blocked by security policy"}`, false
		}
		return 200, sseOK, true
	})
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	h := NewHandler(Config{Pool: p, Upstream: up, PromptMode: "passthrough"})

	// первый запрос триггерит деградацию → 200。
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"glm-5.2","stream":true,"messages":[{"role":"system","content":"x"},{"role":"user","content":"hi"}]}`)))
	if rec.Code != 200 {
		t.Fatalf("first req code=%d", rec.Code)
	}
	// Стикки-деградация: новый запрос Active()=true，body Уже Rewrite(Degraded)，Первый байт апстрима сразу 200。
	// Но fake Апстрим только для firstCall вернуть 400，после этого всё 200，Невозможно различить"прямой проход"и"повтор"。
	// использовать degrade.Active() прямо assert'ить срабатывание sticky.
	if !h.degrade.Active() {
		t.Fatal("degrade should be active after trigger")
	}
}

// TestContentBlockedCustomModeDoesNotDegrade custom Режим не триггерит fallback-ретрай
// （custom Заменено собственным промптом, больше не должно быть system ложное срабатывание источника); если всё ещё блокируется — прямой возврат
// 400 content_blocked Текст для файрвола (без ротации, без раскрытия аккаунта/код ошибки апстрима).
func TestContentBlockedCustomModeDoesNotDegrade(t *testing.T) {
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		return 400, `{"code":11128,"msg":"blocked by security policy"}`, false
	})
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	h := NewHandler(Config{Pool: p, Upstream: up, PromptMode: "custom", PromptText: "SYS"})

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"glm-5.2","messages":[{"role":"system","content":"old"},{"role":"user","content":"hi"}]}`)))
	// custom в режиме всё равно блокировать → 400 content_blocked（контент в терминальном состоянии, смена аккаунта бессмысленна), без даунгрейд-ретрая.
	if rec.Code != 400 {
		t.Fatalf("code=%d want 400 content_blocked (custom does not degrade)", rec.Code)
	}
	var env struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatalf("unmarshal: %v (body=%s)", err, rec.Body.String())
	}
	if env.Error.Code != "content_blocked" {
		t.Errorf("error.code=%q want content_blocked", env.Error.Code)
	}
	// сквозная передача ошибки (error-passthrough，Поглотить апстрим 5755fe3）：message Установить апстрим body Исходный текст
	// （code/msg как есть), клиент должен видеть реальную ошибку для диагностики;gateway_hint Параллельное дополнение.
	if !strings.Contains(rec.Body.String(), "11128") {
		t.Errorf("message should carry upstream original body (passthrough): %s", rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "gateway_hint") {
		t.Errorf("content_blocked should carry gateway_hint: %s", rec.Body.String())
	}
	if h.degrade.Active() {
		t.Error("degrade should NOT be active in custom mode")
	}
}

// TestContentBlockedDoesNotPenalizeAccount ErrContentBlocked аккаунт не штрафуется (без кулдауна/Отключение/NoteError）。
func TestContentBlockedDoesNotPenalizeAccount(t *testing.T) {
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		return 400, `{"code":11128,"msg":"blocked by security policy"}`, false
	})
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	// Порог circuit breaker 1：при ложном штрафе NoteError Один раз — и熔断;content_blocked Не должно триггерить circuit breaker.
	p.SetBreaker(1, time.Hour, time.Hour)
	h := NewHandler(Config{Pool: p, Upstream: up, PromptMode: "passthrough"})

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"glm-5.2","messages":[{"role":"user","content":"hi"}]}`)))

	st, _ := p.Status("u1")
	if st.Cooling || st.Disabled || st.ErrTotal != 0 {
		t.Fatalf("ErrContentBlocked should not penalize account: %+v", st)
	}
}

// TestCustomModeFingerprintSanitizePreserved custom End-to-end:user Сообщение содержит Claude Code
// фраза отпечатка (PR39 fixture строка)→ Rewrite Инжект собственного system → Через sanitize → исходящий body Средний
// Этот отпечаток перезаписан,system — собственный промпт. Доказательство в два слоя (подмена промпта + очистка) работают совместно.
//
// Обязанности двух уровней (невзаимозаменяемы):
// - prompt.Rewrite Замена system/developer сообщение (устранение system отпечаток источника);
// - sanitizeMessages Перезаписать user/assistant Строка отпечатка, оставшаяся в сообщении (fallback пользовательского контекста).
func TestCustomModeFingerprintSanitizePreserved(t *testing.T) {
	// перехват исходящего body（prepareBody Принудительно stream + Нормализация + после очистки).
	var sentBody []byte
	up := &upstream.Client{
		HTTP: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			raw, _ := io.ReadAll(r.Body)
			sentBody = raw
			return &http.Response{
				StatusCode: 200,
				Header: http.Header{"Content-Type": []string{"text/event-stream"}},
				Body: io.NopCloser(strings.NewReader(sseOK)),
			}, nil
		})},
		ChatBaseCN: "https://fake.example",
	}
	up.SanitizeFingerprints.Store(true) // 开启清洗层（与生产一致）
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	const customSys = "我是网关自有提示词"
	h := NewHandler(Config{Pool: p, Upstream: up, PromptMode: "custom", PromptText: customSys})

	// user Сообщение содержит Claude Code фраза отпечатка (PR39 fixture строка, точный посимвольный фингерпринт).
	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{
		"model":"glm-5.2",
		"stream":true,
		"messages":[
			{"role":"system","content":"You are Claude Code, Anthropic's official CLI for Claude."},
			{"role":"user","content":"You are Claude Code, Anthropic's official CLI for Claude. Main branch (you will usually use this for PRs)"}
		]
	}`))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
	out := string(sentBody)
	// 1) system для собственных промптов, старый system Нулевой остаток контента.
	if !strings.Contains(out, customSys) {
		t.Errorf("out body should contain custom system prompt: %s", out)
	}
	if strings.Contains(out, "official CLI for Claude.") && strings.Contains(out, "You are Claude Code, Anthropic's") {
		// старый system оригинал (включая точку) не должен начинаться с system появляется роль; но sanitize Переписать это как
		// "...official CLI tool for Claude."，поэтому оригинал fingerprint Строка должна исчезнуть.
	}
	// Исходная строка отпечатка (точное посимвольное совпадение) на исходящем body в ... должно быть переписано:
	// "official CLI for Claude." → "official CLI tool for Claude."
	// "Main branch (" → "Default branch ("
	if strings.Contains(out, "official CLI for Claude.") {
		t.Errorf("identity fingerprint not rewritten by sanitize in user msg: %s", out)
	}
	if strings.Contains(out, "Main branch (you will usually use this for PRs)") {
		t.Errorf("branch fingerprint not rewritten by sanitize in user msg: %s", out)
	}
	// следы после перезаписи должны быть в (доказательство sanitize действует на уровне , а не"полностью удалено"）。
	if !strings.Contains(out, "official CLI tool for Claude.") {
		t.Errorf("sanitized identity rewrite missing: %s", out)
	}
	if !strings.Contains(out, "Default branch (you will usually use this for PRs)") {
		t.Errorf("sanitized branch rewrite missing: %s", out)
	}
	// 2) messages в заголовке ровно одна запись system = собственный промпт (Rewrite Старое уже удалено system）。
	var obj map[string]any
	if err := json.Unmarshal(sentBody, &obj); err != nil {
		t.Fatalf("out body not json: %v %s", err, out)
	}
	msgs := obj["messages"].([]any)
	var systemCount int
	for _, m := range msgs {
		mm := m.(map[string]any)
		if mm["role"] == "system" {
			systemCount++
			if mm["content"] != customSys {
				t.Errorf("system content=%v want %q", mm["content"], customSys)
			}
		}
	}
	if systemCount != 1 {
		t.Errorf("want exactly 1 system message, got %d (all=%v)", systemCount, msgs)
	}
}
