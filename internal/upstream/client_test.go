package upstream

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
)

func TestClassify(t *testing.T) {
	cases := []struct {
		status int
		body string
		want ErrKind
	}{
		{402, ``, ErrHardCredit},
		{400, `{"code":1,"msg":"Недостаточно средств"}`, ErrHardCredit},
		{403, `insufficient credits`, ErrHardCredit},
		{200, `{"code":10001,"msg":"недостаточно баллов, пополните баланс"}`, ErrHardCredit},
		{400, `{"code":1,"msg":"лимит исчерпан"}`, ErrHardCredit},
		{429, ``, ErrSoftRate},
		// Текст ограничения скорости (issue #28）：статус-код не 429 также должен распознаваться как soft-лимит,
		// Иначе аккаунт не уйдет в кулдаун и будет снова выбран в след. запросе.
		{200, `{"code":11140,"msg":"The model provider is rate-limiting requests. Please wait a moment and try again."}`, ErrSoftRate},
		{400, `rate limit`, ErrSoftRate},
		{403, `usage limit reached`, ErrSoftRate},
		// "model usage limit exceeded" Не семантика баланса (без credit/quota/Баллы/Квота и др. биллинговые термины),
		// троттлинг расхода на стороне модели → короткое охлаждение (ошибочная трактовка как жесткого охлаждения приостановит номера с остатком до следующего дня 04:00）。
		{200, `{"code":1,"msg":"model usage limit exceeded"}`, ErrSoftRate},
		{200, `{"code":1,"msg":"too many requests"}`, ErrSoftRate},
		{500, `rate-limited upstream`, ErrSoftRate}, // Текст лимита приоритетнее 5xx Классификация
		// Блокировка контент-политикой (HTTP 400 + текст модерации): ложноположительный сигнал, аккаунт не штрафовать, идти в retry с даунгрейдом.
		{400, `Illegal API invocation from an unapproved channel`, ErrContentBlocked},
		{400, `{"code":11128,"msg":"blocked by security policy"}`, ErrContentBlocked},
		{400, `unapproved channel`, ErrContentBlocked},
		// общий 4xx（не модерационный текст): всё равно считать ErrClient，только смена аккаунта без штрафа.
		{400, `bad request`, ErrClient},
		// ErrBadParams：Ошибка парсинга тела запроса (HTTP 400 + Unmarshal chat params failed / code 11101）。
		// это"отправляемое в upstream body есть проблема"（усечение шлюзом уже 413 устранено, остаток — malformed клиента JSON），
		// Смена аккаунта — то же самое 400，аккаунт не штрафуется. Конкретное слово приоритетнее общего 4xx。
		{400, `{"code":11101,"msg":"Unmarshal chat params failed with error: unexpected EOF"}`, ErrBadParams},
		{400, `Unmarshal chat params failed`, ErrBadParams},
		{400, `{"code":11101,"msg":"x"}`, ErrBadParams},
		// формат изображения/ошибка данных — детерминированная ошибка запроса, после классификации без ротации и без штрафа аккаунта.
		{400, `{"code":11101,"msg":"Parse message failed: invalid image_url content at index 2: json: cannot unmarshal string into Go value of type v2.ImageContent"}`, ErrImageInvalid},
		{400, `{"code":11135,"msg":"invalid_image_data"}`, ErrImageInvalid},
		{400, `invalid_image_data`, ErrImageInvalid},
		// 11135 Бизнес-код должен допускать JSON Пусто (5d5223d：Литерал marker покрывает только компактную форму).
		{400, `{"code": 11135, "msg":"image data invalid"}`, ErrImageInvalid},
		{400, `{"error":{"code": "11135", "message":"image invalid"}}`, ErrImageInvalid},
		// Защита от слишком широкого:11133（Модель не поддерживает изображения) не попадает в image_invalid。
		{400, `{"code": 11133, "msg":"model does not support image"}`, ErrClient},
		{200, `quota exceeded`, ErrHardCredit},
		// session Сообщение о смерти приоритетнее сообщения о лимите (401+12153 Требуется ручной релогин, короткий кулдаун бессмыслен).
		{401, `{"code":12153,"msg":"Offline user session not found, rate limit"}`, ErrSessionDead},
		{401, `Offline user session not found`, ErrSessionDead},
		{401, `{"code":12153,"msg":"Offline user session not found"}`, ErrSessionDead},
		{401, `{"code":9999,"msg":"bad token"}`, ErrClient},
		{500, `boom`, ErrServer},
		{503, `unavailable`, ErrServer},
		{200, ``, ErrNone},
		// 11102「«У этого бэкенда нет такой модели»: детерминированный ответ, относится к ErrModelBlocked（(Аккаунт,Модель) обход negative cache).
		{404, `{"code":11102,"msg":"model [deepseek-v3-2-volc] service info not found"}`, ErrModelBlocked},
		{400, `{"error":{"code":"11102","message":"model service info not found"}}`, ErrModelBlocked},
		{400, `{"msg":"service info not found"}`, ErrModelBlocked},
		// 11102 попадание в requestId не считается (нельзя ошибочно обходить доступную модель).
		{404, `{"requestId":"11102","msg":"ok"}`, ErrNotFound},
		// 429 + 11102 → семантика ограничения скорости (ErrSoftRate），это не отсутствие модели.
		{429, `{"code":11102,"msg":"service info not found"}`, ErrSoftRate},
		// 429 + Формулировка баланса → семантика ограничения скорости (fork-scan-absorb T-3，точка исправления в этот раз): ответ rate limit
		// body частая передача "quota exceeded«/«Недостаточно лимита" и т.п. кросс-биллинг/формулировка двух границ лимита,
		// hardRule В 429 ранее ложное срабатывание ErrHardCredit Жёсткое охлаждение до следующего дня 04:00，впустую потеряно аккаунтов около 12h。
		// код статуса авторитетнее ключевых слов: при реальном исчерпании баланса идти по 402，не 429 quota формулировка
		// всё равно относится к hardRule（Выше {200,"quota exceeded"} семантика неизменна).
		{429, `quota exceeded`, ErrSoftRate},
		{429, `{"code":1,"msg":"quota exceeded, please wait"}`, ErrSoftRate},
		{429, `insufficient credits`, ErrSoftRate},
		{429, `{"code":1,"msg":"Недостаточно лимита"}`, ErrSoftRate},
		{429, `недостаточно баллов, пополните баланс`, ErrSoftRate},
		// 429 + Защита от регрессии по коду ошибки уровня аккаунта (accountFault всё ещё перед 429 решение):429+14017 Если
		// Попадает в status==429 фолбэк ошибочно отнесет soft_rate，сбои уровня аккаунта и т.п. не самовосстанавливаются.
		{429, `{"code":14017,"msg":"trial not activated"}`, ErrAccountFault},
		{429, `{"error":{"data":{"code":11140,"msg":"request illegal"}}}`, ErrAccountFault},
		// Issue #175：14018 явно означает исчерпание баллов аккаунта, даже если HTTP Статус: 429 также должен
		// идёт жёсткий кулдаун по баллам; обычный с тем же текстом, но без этого бизнес-кода 429 мягкое ограничение сохраняется.
		{429, `{"code":14018,"msg":"Credits exhausted"}`, ErrHardCredit},
		{429, `{"error":{"data":{"code":"14018","msg":"Credits exhausted"}}}`, ErrHardCredit},
		{429, `{"requestId":"14018","msg":"Credits exhausted"}`, ErrSoftRate},
		{429, `{"code":1,"msg":"Credits exhausted"}`, ErrSoftRate},
		// WAF 403（P0-1）：403 + Без бизнес-конверта (без "code":/«msg": поле)→ ErrWafBlock。
		// Пустое тело / HTML Страница перехвата / plain text / не конверт JSON все совпадения.
		{403, ``, ErrWafBlock},
		{403, `<html><body>403 Forbidden</body></html>`, ErrWafBlock},
		{403, `Forbidden`, ErrWafBlock},
		{403, `{"message":"blocked by waf"}`, ErrWafBlock},
		{403, `<head><script>...</script></head><body>blocked</body>`, ErrWafBlock},
		// 403 С бизнес-конвертом — по существующей классификации (P0-1 Ограничение: не перехватывать бизнес- 403）。
		{403, `{"code":11128,"msg":"blocked by security policy"}`, ErrContentBlocked},
		{403, `{"code":60001,"msg":"quota exceeded"}`, ErrHardCredit},
		{403, `{"code":1,"msg":"unknown business error"}`, ErrClient},
		// не 403 тело ошибки без обертки не попадает в WAF Классификация (WAF привязка решения 403 форма).
		{400, `bad request`, ErrClient},
		{429, ``, ErrSoftRate},
	}
	for _, c := range cases {
		if got := Classify(c.status, c.body); got != c.want {
			t.Errorf("Classify(%d,%q)=%v want %v", c.status, c.body, got, c.want)
		}
	}
}

// TestIsModelRateLimit Проверка 429 body Явно ли указывает на лимит уровня модели (code 6004）。
func TestIsModelRateLimit(t *testing.T) {
	cases := []struct {
		body string
		want bool
	}{
		// 6004：Rate-limit на уровне модели (issue #31 основной сценарий).
		{`{"code":6004,"msg":"将在 2026-09-11 18:33:27 UTC+8 重置"}`, true},
		{`{"code": 6004,"msg":"x"}`, true},
		// прочее code（не лимит на уровне модели)→ не считается.
		{`{"code":11140,"msg":"The model provider is rate-limiting requests."}`, false},
		{`{"code":1,"msg":"429 rate limit"}`, false},
	}
	for _, c := range cases {
		if got := IsModelRateLimit(c.body); got != c.want {
			t.Errorf("IsModelRateLimit(%q)=%v want %v", c.body, got, c.want)
		}
	}
}

// TestParseSoftRateReset Парсинг апстрима 429 6004 msg внутри "через … время "сброса» (## UTC+8）。
func TestParseSoftRateReset(t *testing.T) {
	future := time.Now().Add(35 * time.Minute)
	ts := future.In(softRateResetLoc).Format("2006-01-02 15:04:05")
	cases := []struct {
		name string
		body string
		ok bool
	}{
		{"6004 с меткой времени+UTC+8 суффикс", `{"code":6004,"msg":"将在 ` + ts + ` UTC+8 重置"}`, true},
		{"6004 С временем без суффикса", `{"code":6004,"msg":"将在 ` + ts + ` 重置"}`, true},
		{"6004 без временной метки", `{"code":6004,"msg":"model usage limit exceeded"}`, false},
		{"не 6004 но со временем (ParseRateReset единый парсинг; exemption на уровне модели — на стороне вызывающего по 6004 проверка)", `{"code":11140,"msg":"将在 ` + ts + ` UTC+8 重置"}`, true},
		{"Неверный формат времени", `{"code":6004,"msg":"将在 Завтра 重置"}`, false},
		{"пустой body", ``, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, ok := ParseRateReset(c.body)
			if ok != c.ok {
				t.Fatalf("ok=%v want %v (body=%s)", ok, c.ok, c.body)
			}
			if ok {
				// результат парсинга = ts В UTC+8 Wall-clock в данном смысле (усечение до минут), должен совпадать с future Разница ±2 мин.
				if d := got.Sub(future); d < -2*time.Minute || d > 2*time.Minute {
					t.Errorf("parsed=%v want ~%v (diff %v)", got, future, d)
				}
				if got.Location() != time.UTC {
					// разных указателей FixedZone равенство экземпляров по offset проверка, здесь только assert offset。
					if _, off := got.Zone(); off != 8*60*60 {
						t.Errorf("zone offset=%d want +08:00", off)
					}
				}
			}
		})
	}
}

type rtFunc func(*http.Request) (*http.Response, error)

func (f rtFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func jsonResp(status int, body string) *http.Response {
	return &http.Response{
		StatusCode: status,
		Header: http.Header{"Content-Type": []string{"application/json"}},
		Body: io.NopCloser(strings.NewReader(body)),
	}
}

func testClient(fn rtFunc) *Client {
	return &Client{
		HTTP: &http.Client{Transport: fn},
		ChatBaseCN: "https://chat.example",
		BillingBaseCN: "https://billing.example",
	}
}

func TestRefreshSuccess(t *testing.T) {
	c := testClient(func(r *http.Request) (*http.Response, error) {
		if !strings.HasSuffix(r.URL.Path, "/v2/plugin/auth/token/refresh") {
			return nil, errors.New("wrong path: " + r.URL.Path)
		}
		if r.Header.Get("X-Refresh-Token") != "oldrt" {
			return nil, errors.New("missing X-Refresh-Token")
		}
		return jsonResp(200, `{"code":0,"msg":"ok","data":{"accessToken":"newat","refreshToken":"newrt","expiresIn":3600}}`), nil
	})
	a := &auth.Auth{AccessToken: "at", RefreshToken: "oldrt", ExpiresAt: 1}
	if err := c.RefreshToken(a); err != nil {
		t.Fatalf("refresh: %v", err)
	}
	if a.AccessToken != "newat" || a.RefreshToken != "newrt" {
		t.Errorf("tokens not updated: %+v", a)
	}
	if a.ExpiresAt <= 1 {
		t.Errorf("expiresAt not advanced: %d", a.ExpiresAt)
	}
}

func TestRefreshPreservesExpiryWhenOmitted(t *testing.T) {
	c := testClient(func(r *http.Request) (*http.Response, error) {
		return jsonResp(200, `{"code":0,"data":{"accessToken":"newat"}}`), nil
	})
	a := &auth.Auth{AccessToken: "at", RefreshToken: "rt", ExpiresAt: 1753600000}
	if err := c.RefreshToken(a); err != nil {
		t.Fatalf("refresh: %v", err)
	}
	if a.ExpiresAt != 1753600000 {
		t.Errorf("expiresAt should be preserved, got %d", a.ExpiresAt)
	}
	if a.RefreshToken != "rt" {
		t.Errorf("refreshToken should be preserved, got %s", a.RefreshToken)
	}
}

func TestRefreshSessionDead(t *testing.T) {
	c := testClient(func(r *http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: 401,
			Header: http.Header{"Content-Type": []string{"application/json"}},
			Body: io.NopCloser(strings.NewReader(`{"code":12153,"msg":"Offline user session not found"}`)),
		}, nil
	})
	a := &auth.Auth{AccessToken: "at", RefreshToken: "rt", ExpiresAt: 1}
	err := c.RefreshToken(a)
	if err == nil {
		t.Fatal("want error")
	}
	var ue *Error
	if !errors.As(err, &ue) {
		t.Fatalf("want *Error, got %T %v", err, err)
	}
	if ue.Kind != ErrSessionDead {
		t.Errorf("kind=%v want ErrSessionDead", ue.Kind)
	}
}

func TestChatStreamSendsHeadersAndStreamTrue(t *testing.T) {
	var gotAuth, gotUID, gotProduct string
	var gotBody []byte
	c := testClient(func(r *http.Request) (*http.Response, error) {
		gotAuth = r.Header.Get("Authorization")
		gotUID = r.Header.Get("X-User-Id")
		gotProduct = r.Header.Get("X-Product")
		gotBody, _ = io.ReadAll(r.Body)
		return &http.Response{
			StatusCode: 200,
			Header: http.Header{"Content-Type": []string{"text/event-stream"}},
			Body: io.NopCloser(strings.NewReader("data: [DONE]\n\n")),
		}, nil
	})
	a := &auth.Auth{AccessToken: "at", UID: "u1", EnterpriseID: "e1"}
	rc, status, respBody, err := c.ChatStream(a, []byte(`{"model":"glm-5.2","messages":[]}`), "", ChatMeta{})
	if err != nil || status != 200 {
		t.Fatalf("chat: status=%d err=%v", status, err)
	}
	if respBody != nil {
		t.Errorf("200 response should carry nil body, got %q", respBody)
	}
	rc.Close()
	if gotAuth != "Bearer at" || gotUID != "u1" || gotProduct != "WorkBuddy" {
		t.Errorf("headers: auth=%q uid=%q product=%q", gotAuth, gotUID, gotProduct)
	}
	if !bytes.Contains(gotBody, []byte(`"stream":true`)) {
		t.Errorf("stream not forced: %s", gotBody)
	}
}

func TestFetchModelsEffortsDriveBodyDowngrade(t *testing.T) {
	var outbound []byte
	c := testClient(func(r *http.Request) (*http.Response, error) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/console/enterprises/personal/models"):
			return jsonResp(200, `{"code":0,"data":{"models":[
				{"id":"glm-5.2","name":"GLM-5.2","maxInputTokens":131072,"maxOutputTokens":8192,"reasoning":{"effort":"high","supportedEfforts":["low","high"]}}
			],"agents":[{"name":"cli","models":["glm-5.2"]}]}}`), nil
		case strings.HasSuffix(r.URL.Path, "/v3/config"):
			return jsonResp(200, `{"code":0,"data":{"models":[]}}`), nil
		default:
			outbound, _ = io.ReadAll(r.Body)
			return &http.Response{
				StatusCode: 200,
				Header: http.Header{"Content-Type": []string{"text/event-stream"}},
				Body: io.NopCloser(strings.NewReader("data: [DONE]\n\n")),
			}, nil
		}
	})
	a := &auth.Auth{AccessToken: "at", UID: "u1"}
	infos, err := c.FetchModels(a)
	if err != nil {
		t.Fatalf("fetch models: %v", err)
	}
	if len(infos) != 1 {
		t.Fatalf("infos=%+v", infos)
	}
	// ModelInfo.Efforts должен содержать supportedEfforts，DefaultEffort должен содержать reasoning.effort
	if len(infos[0].Efforts) != 2 || infos[0].Efforts[0] != "low" {
		t.Errorf("infos[0].Efforts=%v", infos[0].Efforts)
	}
	if infos[0].DefaultEffort != "high" {
		t.Errorf("infos[0].DefaultEffort=%q want high", infos[0].DefaultEffort)
	}
	// glm-5.2 Поддерживается только low/high，Запрос max → Понизить до high
	rc, status, _, err := c.ChatStream(a, []byte(`{"model":"glm-5.2","reasoning_effort":"max","messages":[]}`), "", ChatMeta{})
	if err != nil || status != 200 {
		t.Fatalf("chat: status=%d err=%v", status, err)
	}
	rc.Close()
	var m map[string]any
	if err := json.Unmarshal(outbound, &m); err != nil {
		t.Fatalf("outbound unmarshal: %v (%s)", err, outbound)
	}
	if got, _ := m["reasoning_effort"].(string); got != "high" {
		t.Errorf("reasoning_effort=%v want high (outbound=%s)", m["reasoning_effort"], outbound)
	}
}

func TestChatStreamHardCreditError(t *testing.T) {
	c := testClient(func(r *http.Request) (*http.Response, error) {
		return jsonResp(402, `{"code":1,"msg":"Недостаточно средств"}`), nil
	})
	a := &auth.Auth{AccessToken: "at", UID: "u1"}
	_, status, respBody, err := c.ChatStream(a, []byte(`{}`), "", ChatMeta{})
	if status != 402 {
		t.Errorf("status=%d", status)
	}
	// Конверт ошибки формируется за один раз:≥400 возврат классифицированных *Error（Kind + body полный объем всё равно через respBody пробрасывается)
	var ue *Error
	if !errors.As(err, &ue) || ue.Kind != ErrHardCredit {
		t.Fatalf("hard credit should return classified *Error envelope, got %v", err)
	}
	if len(respBody) == 0 {
		t.Errorf("body should still be returned for passthrough")
	}
}

// TestChatStreamReadsMultipleChunksOverRealTransport идти по реальному net/http Транспортный уровень,
// Регресс defer cancel() приводит к тому, что со второго блока body Read вернуть context canceled обрыв потока bug。
func TestChatStreamReadsMultipleChunksOverRealTransport(t *testing.T) {
	const frames = 6
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		flusher, ok := w.(http.Flusher)
		if !ok {
			t.Error("http.ResponseWriter does not implement http.Flusher")
			return
		}
		for i := 1; i <= frames; i++ {
			if _, err := fmt.Fprintf(w, "data: chunk-%d\n\n", i); err != nil {
				return
			}
			flusher.Flush()
			time.Sleep(20 * time.Millisecond)
		}
	}))
	defer srv.Close()

	c := New()
	c.ChatBaseCN = srv.URL
	c.IdleTimeout = 5 * time.Second

	a := &auth.Auth{AccessToken: "at", UID: "u1"}
	rc, status, _, err := c.ChatStream(a, []byte(`{"model":"glm-5.2","messages":[]}`), "", ChatMeta{})
	if err != nil || status != 200 {
		t.Fatalf("chat: status=%d err=%v", status, err)
	}
	defer rc.Close()

	buf := make([]byte, 1)
	var got string
	for i := 0; i < frames; i++ {
		if _, err := io.ReadFull(rc, buf); err != nil {
			t.Fatalf("read %d: %v (real transport body must not be cut)", i, err)
		}
		got += string(buf)
	}
	if strings.Contains(got, "context canceled") {
		t.Fatalf("body read hit context canceled, got %q", got)
	}
}

func TestUserResourceAggregation(t *testing.T) {
	c := testClient(func(r *http.Request) (*http.Response, error) {
		if !strings.HasSuffix(r.URL.Path, "/v2/billing/meter/get-user-resource") {
			return nil, errors.New("wrong path: " + r.URL.Path)
		}
		if r.Method != http.MethodPost {
			return nil, errors.New("want POST")
		}
		body, _ := io.ReadAll(r.Body)
		if !bytes.Contains(body, []byte(`"ProductCode":"p_tcaca"`)) {
			return nil, errors.New("missing ProductCode: " + string(body))
		}
		return jsonResp(200, `{"code":0,"data":{"Response":{"Data":{"TotalCount":2,"TotalDosage":3000,"Accounts":[
			{"PackageName":"пакет отметки","CapacitySize":2000,"CapacityRemain":1200,"CapacityUsed":800,"CycleCapacitySize":2000,"CycleCapacityRemain":1200,"CycleCapacityUsed":800},
			{"PackageName":"пробный пакет","CapacitySize":1000,"CapacityRemain":300,"CapacityUsed":700,"CycleCapacitySize":1000,"CycleCapacityRemain":300,"CycleCapacityUsed":700}
		]}}}}`), nil
	})
	a := &auth.Auth{AccessToken: "at", UID: "u1"}
	remain, total, err := c.UserResource(a)
	if err != nil {
		t.Fatalf("resource: %v", err)
	}
	if remain != 1500 {
		t.Errorf("remain=%d want 1500", remain)
	}
	if total != 3000 {
		t.Errorf("total=%d want 3000", total)
	}
}

func TestUserResourceNegativeClamped(t *testing.T) {
	c := testClient(func(r *http.Request) (*http.Response, error) {
		return jsonResp(200, `{"code":0,"data":{"Response":{"Data":{"Accounts":[
			{"PackageName":"p","CycleCapacitySize":100,"CycleCapacityRemain":-50,"CycleCapacityUsed":150}
		]}}}}`), nil
	})
	remain, total, err := c.UserResource(&auth.Auth{AccessToken: "at"})
	if err != nil || remain != 0 {
		t.Errorf("remain=%d err=%v, want 0 (clamped)", remain, err)
	}
	if total != 100 {
		t.Errorf("total=%d want 100", total)
	}
}

func TestDailyCheckinAlready(t *testing.T) {
	c := testClient(func(r *http.Request) (*http.Response, error) {
		if !strings.HasSuffix(r.URL.Path, "/v2/billing/meter/daily-checkin") {
			return nil, errors.New("wrong path")
		}
		return jsonResp(200, `{"code":14001,"msg":"今日已签到"}`), nil
	})
	err := c.DailyCheckin(&auth.Auth{AccessToken: "at"})
	if err == nil || !strings.Contains(err.Error(), "今日已签到") {
		t.Errorf("err=%v", err)
	}
}

func TestBasesAlwaysCN(t *testing.T) {
	c := testClient(nil)
	cn := &auth.Auth{Domain: ""}
	other := &auth.Auth{Domain: "example.com"}
	if c.chatBase(cn) != "https://chat.example" || c.billingBase(cn) != "https://billing.example" {
		t.Error("cn bases wrong")
	}
	// Конст. CN：domain различие не меняет upstream host。
	if c.chatBase(other) != c.chatBase(cn) || c.billingBase(other) != c.billingBase(cn) {
		t.Error("bases must be CN regardless of domain")
	}
}

func TestNewChatClientNoTotalTimeoutAndSharedTransport(t *testing.T) {
	c := New()
	if c.ChatHTTP == nil {
		t.Fatal("ChatHTTP should be initialized")
	}
	if c.ChatHTTP.Timeout != 0 {
		t.Errorf("ChatHTTP.Timeout=%v want 0 (no total cap)", c.ChatHTTP.Timeout)
	}
	// совместное использование одного Transport Экземпляр, пул соединений не дублируется.
	if c.ChatHTTP.Transport != c.HTTP.Transport {
		t.Errorf("ChatHTTP and HTTP must share the same *http.Transport")
	}
	htr, ok := c.ChatHTTP.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("Transport type=%T", c.ChatHTTP.Transport)
	}
	if htr.ResponseHeaderTimeout != 60*time.Second { // усиление транспортного уровня: лимит заголовков ответа с 120s получено 60s（Медленный холодный старт оставляет 3.75× остаток)
		t.Errorf("ResponseHeaderTimeout=%v want 60s", htr.ResponseHeaderTimeout)
	}
}

func TestChatStreamRoutesToChatHTTP(t *testing.T) {
	// Явная инъекция ChatHTTP（идентифицируемая метка), проверка ChatStream Использовать его, а не HTTP。
	chatHit, httpHit := false, false
	c := testClient(func(*http.Request) (*http.Response, error) {
		httpHit = true
		return jsonResp(200, `{}`), nil
	})
	c.ChatHTTP = &http.Client{Transport: rtFunc(func(*http.Request) (*http.Response, error) {
		chatHit = true
		return &http.Response{
			StatusCode: 200,
			Header: http.Header{"Content-Type": []string{"text/event-stream"}},
			Body: io.NopCloser(strings.NewReader("data: [DONE]\n\n")),
		}, nil
	})}
	a := &auth.Auth{AccessToken: "at", UID: "u1"}
	rc, status, _, err := c.ChatStream(a, []byte(`{}`), "", ChatMeta{})
	if err != nil || status != 200 {
		t.Fatalf("chat: status=%d err=%v", status, err)
	}
	rc.Close()
	if !chatHit {
		t.Error("ChatStream should use ChatHTTP")
	}
	if httpHit {
		t.Error("ChatStream must not use HTTP")
	}
}

func TestChatHTTPNilFallsBackToHTTP(t *testing.T) {
	c := testClient(func(*http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: 200,
			Header: http.Header{"Content-Type": []string{"text/event-stream"}},
			Body: io.NopCloser(strings.NewReader("data: [DONE]\n\n")),
		}, nil
	})
	if c.chatHTTP() != c.HTTP {
		t.Error("chatHTTP() should fall back to HTTP when ChatHTTP is nil")
	}
}

func TestFetchModelsDefaultEffortDualKeyAndSizes(t *testing.T) {
	// двойной ключ апстрима: старая модель reasoning.effort（auto），Новая модель (glm-5.3 системы) только reasoning.defaultEffort；
	// credits/maxAllowedSize/canDisableThinking/supportsReasoning Поля размера и capabilities также должны пробрасываться.
	c := testClient(func(r *http.Request) (*http.Response, error) {
		return jsonResp(200, `{"code":0,"data":{"models":[
			{"id":"glm-5.3","maxInputTokens":1000000,"maxOutputTokens":48000,"maxAllowedSize":1000000,"credits":"x0.79","supportsReasoning":true,"reasoning":{"defaultEffort":"high","canDisableThinking":true,"supportedEfforts":["low","high","max"]}},
			{"id":"auto","maxInputTokens":168000,"maxOutputTokens":32000,"supportsReasoning":true,"reasoning":{"effort":"high"}}
		],"agents":[{"name":"cli","models":["glm-5.3","auto"]}]}}`), nil
	})
	a := &auth.Auth{AccessToken: "at", UID: "u1"}
	infos, err := c.FetchModels(a)
	if err != nil {
		t.Fatalf("fetch models: %v", err)
	}
	byID := map[string]ModelInfo{}
	for _, mi := range infos {
		byID[mi.ID] = mi
	}
	g := byID["glm-5.3"]
	if g.DefaultEffort != "high" {
		t.Errorf("glm-5.3 DefaultEffort=%q want high (from defaultEffort key)", g.DefaultEffort)
	}
	if !g.CanDisableThinking || !g.SupportsReasoning {
		t.Errorf("glm-5.3 capability flags: canDisable=%v supportsReasoning=%v want true/true", g.CanDisableThinking, g.SupportsReasoning)
	}
	if g.MaxAllowedSize != 1000000 || g.MaxTokens != 48000 || g.Credits != "x0.79" {
		t.Errorf("glm-5.3 sizes: maxAllowed=%d maxOut=%d credits=%q", g.MaxAllowedSize, g.MaxTokens, g.Credits)
	}
	if got := c.ModelRate("cn", "glm-5.3"); got != "0.79" {
		t.Errorf("glm-5.3 ModelRate=%q want 0.79", got)
	}
	if au := byID["auto"]; au.DefaultEffort != "high" {
		t.Errorf("auto DefaultEffort=%q want high (from legacy effort key)", au.DefaultEffort)
	}
}

func TestModelRateCacheEffectiveAndNormalized(t *testing.T) {
	c := New()
	factor := 0.5
	c.storeModelRates("cn", []ModelInfo{
		{ID: "base", Credits: "x0.50 credits"},
		{ID: "promo", Credits: "x0.80", PromoFactor: &factor, PromoCredits: "0.50x"},
	})
	if got := c.ModelRate("cn", "base"); got != "0.5" {
		t.Fatalf("base rate=%q want 0.5", got)
	}
	if got := c.ModelRate("cn", "promo"); got != "0.5" {
		t.Fatalf("promo rate=%q want 0.5", got)
	}
	if got := normalizeModelRate("x0.05 credits"); got != "0.05" {
		t.Fatalf("normalizeModelRate=%q want 0.05", got)
	}
	c.storeModelRates("cn", []ModelInfo{{ID: "base", Credits: "x0.79"}})
	if got := c.ModelRate("cn", "base"); got != "0.79" {
		t.Fatalf("refreshed base rate=%q want 0.79", got)
	}
	if got := c.ModelRate("cn", "promo"); got != "" {
		t.Fatalf("stale promo rate=%q want empty after full refresh", got)
	}
}

func TestFetchModelsOverlaysV3ConfigCapabilities(t *testing.T) {
	// CLI Каталог для flash сокращённые поля (128K / Фиксированный high）；IDE /v3/config Предоставить полные возможности.
	var sawIDE bool
	c := testClient(func(r *http.Request) (*http.Response, error) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/console/enterprises/personal/models"):
			return jsonResp(200, `{"code":0,"data":{"models":[
				{"id":"deepseek-v4.1-flash","name":"Deepseek-V4.1-Flash","maxInputTokens":1000000,"maxOutputTokens":128000,"credits":"x0.03 credits","supportsReasoning":true,"onlyReasoning":true,"reasoning":{"effort":"high","summary":"auto"}}
			],"agents":[{"name":"cli","models":["deepseek-v4.1-flash"]}]}}`), nil
		case strings.HasSuffix(r.URL.Path, "/v3/config"):
			sawIDE = true
			if r.Header.Get("User-Agent") != codeBuddyIDEUA {
				t.Errorf("v3/config UA=%q want %s", r.Header.Get("User-Agent"), codeBuddyIDEUA)
			}
			if r.Header.Get("X-Product") != "SaaS" {
				t.Errorf("X-Product=%q want SaaS", r.Header.Get("X-Product"))
			}
			if r.Header.Get("X-User-Id") != "u1" {
				t.Errorf("X-User-Id=%q want u1", r.Header.Get("X-User-Id"))
			}
			return jsonResp(200, `{"code":0,"data":{"models":[
				{"id":"deepseek-v4.1-flash","name":"Deepseek-V4.1-Flash","maxInputTokens":1000000,"maxOutputTokens":393216,"credits":"x0.03","supportsReasoning":true,"onlyReasoning":true,"reasoning":{"canDisableThinking":true,"defaultEffort":"high","summary":"auto","supportedEfforts":["low","high","max"]}}
			]}}`), nil
		default:
			t.Errorf("unexpected path %s", r.URL.Path)
			return jsonResp(404, `{}`), nil
		}
	})
	a := &auth.Auth{AccessToken: "at", UID: "u1", Domain: "copilot.tencent.com"}
	infos, err := c.FetchModels(a)
	if err != nil {
		t.Fatalf("fetch models: %v", err)
	}
	if !sawIDE {
		t.Fatal("expected /v3/config request")
	}
	if len(infos) != 1 {
		t.Fatalf("infos=%+v", infos)
	}
	mi := infos[0]
	if mi.MaxTokens != 393216 {
		t.Errorf("MaxTokens=%d want 393216", mi.MaxTokens)
	}
	if mi.ContextWindow != 1000000 {
		t.Errorf("ContextWindow=%d want 1000000", mi.ContextWindow)
	}
	if !mi.CanDisableThinking || !mi.SupportsReasoning {
		t.Errorf("flags canDisable=%v supportsReasoning=%v", mi.CanDisableThinking, mi.SupportsReasoning)
	}
	if mi.DefaultEffort != "high" {
		t.Errorf("DefaultEffort=%q want high", mi.DefaultEffort)
	}
	if got := strings.Join(mi.Efforts, ","); got != "low,high,max" {
		t.Errorf("Efforts=%v want low,high,max", mi.Efforts)
	}
}
