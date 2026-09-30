package server

import (
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/reqlog"
)

// captureStdout Перенаправление os.Stdout（вместе с chatLogOut，См. SetChatLogOutput точка инъекции)
// и перехватить fn Весь вывод за период.
func captureStdout(t *testing.T, fn func()) string {
	t.Helper()
	old := os.Stdout
	oldOut := chatLogOut
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stdout = w
	chatLogOut = w
	fn()
	os.Stdout = old
	chatLogOut = oldOut
	_ = w.Close()
	raw, _ := io.ReadAll(r)
	return string(raw)
}

// withChatLog Временно включить лог таблицы чатов (TestMain по умолчанию выключено), восстановить после теста.
// Только для кейсов проверки вывода строк таблицы.
func withChatLog(t *testing.T) {
	t.Helper()
	old := chatLogEnabled
	chatLogEnabled = true
	t.Cleanup(func() { chatLogEnabled = old })
}

func TestChatStatsReaderTokensFromUsage(t *testing.T) {
	r := newChatStatsReaderSince(strings.NewReader(sseOK), time.Now())
	// Гарантировать TTFB через гранулярность часов:Windows чистого чтения из памяти сверху недостаточно 1ms，time.Since Возможно взять 0。
	time.Sleep(3 * time.Millisecond)
	if _, err := io.Copy(io.Discard, r); err != nil {
		t.Fatalf("copy: %v", err)
	}
	toks, ok := r.Tokens()
	if !ok || toks != 1 {
		t.Fatalf("tokens=%d ok=%v, want 1/true (from usage, not rune count)", toks, ok)
	}
	usage := r.Usage()
	if !usage.HasPromptTokens || usage.PromptTokens != 1 || !usage.HasCompletionTokens || usage.CompletionTokens != 1 || !usage.HasTotalTokens || usage.TotalTokens != 2 {
		t.Fatalf("usage=%+v, want prompt=1 completion=1 total=2", usage)
	}
	if r.TTFB() <= 0 {
		t.Errorf("ttfb=%v want >0", r.TTFB())
	}
}

func TestChatStatsReaderNoUsage(t *testing.T) {
	sse := "data: {\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\ndata: [DONE]\n\n"
	r := newChatStatsReaderSince(strings.NewReader(sse), time.Now())
	_, _ = io.Copy(io.Discard, r)
	if toks, ok := r.Tokens(); ok || toks != 0 {
		t.Errorf("tokens=%d ok=%v, want 0/false for missing usage", toks, ok)
	}
}

func TestChatStatsReaderLastFrameUsageWins(t *testing.T) {
	sse := "data: {\"usage\":{\"completion_tokens\":5}}\n\n" +
		"data: {\"usage\":{\"completion_tokens\":12}}\n\n" +
		"data: [DONE]\n\n"
	r := newChatStatsReaderSince(strings.NewReader(sse), time.Now())
	_, _ = io.Copy(io.Discard, r)
	toks, ok := r.Tokens()
	if !ok || toks != 12 {
		t.Fatalf("tokens=%d ok=%v, want 12 (last frame wins)", toks, ok)
	}
}

func TestChatStatsReaderTTFBOnlyOnDataFrame(t *testing.T) {
	start := time.Now().Add(-2 * time.Second)
	var s chatStatsReader
	s.start = start
	s.parseSSELine("event: ping")
	if s.TTFB() != 0 {
		t.Errorf("non-data line must not set TTFB: %v", s.TTFB())
	}
	s.parseSSELine("data: {\"choices\":[]}")
	first := s.TTFB()
	if first < time.Second {
		t.Errorf("first data frame TTFB=%v want >=2s", first)
	}
	s.parseSSELine("data: {\"choices\":[]}")
	if s.TTFB() != first {
		t.Errorf("second frame changed TTFB: %v -> %v", first, s.TTFB())
	}
}

func TestChatStatsReaderBytesPassthrough(t *testing.T) {
	sse := "data: {\"content\":\"你好\"}\n\ndata: [DONE]\n\n"
	r := newChatStatsReaderSince(strings.NewReader(sse), time.Now())
	out, _ := io.ReadAll(r)
	if string(out) != sse {
		t.Errorf("passthrough mismatch:\n got %q\nwant %q", out, sse)
	}
}

func TestRequestMetricsRecordsStream(t *testing.T) {
	up := newFakeUpstream(t, func(string) (int, string, bool) {
		return 200, sseOK, true
	})
	reqLog := reqlog.New(reqlog.Config{})
	h := NewHandler(Config{
		Pool: testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999}),
		Upstream: up,
		RequestLog: reqLog,
	})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"glm-5.2","stream":true,"messages":[]}`))
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
	if got := rec.Header().Get("X-Request-Id"); !strings.HasPrefix(got, "req-") {
		t.Fatalf("X-Request-Id=%q", got)
	}
	s := reqLog.Snapshot()
	if s.Completed != 1 || s.Succeeded != 1 || s.Failed != 0 || len(s.Recent) != 1 {
		t.Fatalf("metrics = %+v", s)
	}
	e := s.Recent[0]
	if e.Outcome != reqlog.OutcomeSuccess || e.Status != http.StatusOK || !e.OK || e.Model != "glm-5.2" || e.TotalTokens != 2 || e.Attempts != 1 {
		t.Fatalf("event = %+v", e)
	}
}

// источник вызова должен попасть в архивное событие:X-Forwarded-For Первый сегмент (реальный клиент за reverse proxy)+ После усечения UA。
func TestRequestMetricsCapturesClientInfo(t *testing.T) {
	up := newFakeUpstream(t, func(string) (int, string, bool) {
		return 200, sseOK, true
	})
	reqLog := reqlog.New(reqlog.Config{})
	h := NewHandler(Config{
		Pool: testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999}),
		Upstream: up,
		RequestLog: reqLog,
		RecordClientInfo: true,
	})
	req := httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"glm-5.2","stream":true,"messages":[]}`))
	req.Header.Set("X-Forwarded-For", "203.0.113.7, 10.0.0.1")
	req.Header.Set("User-Agent", "python-requests/2.31.0")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
	s := reqLog.Snapshot()
	if len(s.Recent) != 1 {
		t.Fatalf("recent = %+v", s.Recent)
	}
	if got := s.Recent[0].ClientIP; got != "203.0.113.7" {
		t.Errorf("client ip = %q want 203.0.113.7 (XFF first hop)", got)
	}
	if got := s.Recent[0].UserAgent; got != "python-requests/2.31.0" {
		t.Errorf("user agent = %q", got)
	}
}

// При выкл. переключателе источник не собирается (в архиве не появляется IP/UA），но метрики запросов пишутся как обычно.
func TestRequestMetricsClientInfoDisabled(t *testing.T) {
	up := newFakeUpstream(t, func(string) (int, string, bool) {
		return 200, sseOK, true
	})
	reqLog := reqlog.New(reqlog.Config{})
	h := NewHandler(Config{
		Pool: testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999}),
		Upstream: up,
		RequestLog: reqLog,
	})
	req := httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"glm-5.2","stream":true,"messages":[]}`))
	req.Header.Set("X-Forwarded-For", "203.0.113.7")
	req.Header.Set("User-Agent", "python-requests/2.31.0")
	h.ServeHTTP(httptest.NewRecorder(), req)
	s := reqLog.Snapshot()
	if len(s.Recent) != 1 {
		t.Fatalf("recent = %+v", s.Recent)
	}
	if s.Recent[0].ClientIP != "" || s.Recent[0].UserAgent != "" {
		t.Fatalf("client info recorded while disabled: %+v", s.Recent[0])
	}
	if s.Recent[0].Model != "glm-5.2" {
		t.Errorf("model = %q, metrics should be unaffected", s.Recent[0].Model)
	}
}

// при отсутствии proxy-заголовка fallback на TCP Адрес пира — при прямом деплое это единственный индикатор источника.
func TestClientIPForLogFallsBackToRemoteAddr(t *testing.T) {
	req := httptest.NewRequest("POST", "/v1/chat/completions", nil)
	req.RemoteAddr = "198.51.100.9:54321"
	if got := clientIPForLog(req); got != "198.51.100.9" {
		t.Errorf("got %q want 198.51.100.9", got)
	}
	// приоритет заголовка прокси (X-Real-IP Фолбэк XFF）。
	req.Header.Set("X-Real-IP", "192.0.2.5")
	if got := clientIPForLog(req); got != "192.0.2.5" {
		t.Errorf("X-Real-IP got %q want 192.0.2.5", got)
	}
	req.Header.Set("X-Forwarded-For", "192.0.2.9, 10.1.1.1")
	if got := clientIPForLog(req); got != "192.0.2.9" {
		t.Errorf("XFF got %q want 192.0.2.9", got)
	}
	if got := clientIPForLog(nil); got != "" {
		t.Errorf("nil request got %q want empty", got)
	}
}

// Сверхдлинный UA Обрезка перед записью на диск:UA — свободный текст под контролем клиента, без усечения раздует строку архива.
func TestCaptureClientInfoTruncatesUserAgent(t *testing.T) {
	tr := &requestTrace{}
	req := httptest.NewRequest("POST", "/v1/chat/completions", nil)
	req.RemoteAddr = "198.51.100.9:1234"
	req.Header.Set("User-Agent", strings.Repeat("A", 5000))
	tr.captureClientInfo(req)
	if len(tr.userAgent) != maxUserAgentLen {
		t.Fatalf("ua len = %d want %d", len(tr.userAgent), maxUserAgentLen)
	}
}

func TestRequestMetricsDetectsStreamErrorFrame(t *testing.T) {
	const sseErr = "data: {\"error\":{\"message\":\"upstream failed\"}}\n\ndata: [DONE]\n\n"
	up := newFakeUpstream(t, func(string) (int, string, bool) {
		return 200, sseErr, true
	})
	reqLog := reqlog.New(reqlog.Config{})
	h := NewHandler(Config{
		Pool: testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999}),
		Upstream: up,
		RequestLog: reqLog,
	})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"glm-5.2","stream":true,"messages":[]}`)))
	s := reqLog.Snapshot()
	if len(s.Recent) != 1 || s.Recent[0].Outcome != reqlog.OutcomeStreamError || s.Recent[0].OK {
		t.Fatalf("stream error metrics = %+v", s.Recent)
	}
}

func TestParseModelFromBody(t *testing.T) {
	if got := parseModelFromBody([]byte(`{"model":"deepseek-v4-flash","stream":true}`)); got != "deepseek-v4-flash" {
		t.Errorf("got %q", got)
	}
	if got := parseModelFromBody([]byte(`{}`)); got != "-" {
		t.Errorf("got %q want -", got)
	}
	if got := parseModelFromBody([]byte(`not json`)); got != "-" {
		t.Errorf("got %q want -", got)
	}
}

func TestCompletionTokensExtraction(t *testing.T) {
	got := completionTokens(map[string]any{
		"usage": map[string]any{"prompt_tokens": 10.0, "completion_tokens": 234.0, "total_tokens": 244.0},
	})
	if got != 234 {
		t.Errorf("got %d want 234", got)
	}
	if got := completionTokens(map[string]any{}); got != -1 {
		t.Errorf("missing usage: got %d want -1", got)
	}
	if got := completionTokens(map[string]any{"usage": map[string]any{}}); got != -1 {
		t.Errorf("missing completion_tokens: got %d want -1", got)
	}
}

func TestUIDPrefix(t *testing.T) {
	if got := uidPrefix("00e26541abcdef012345"); got != "00e26541" {
		t.Errorf("long uid -> %q", got)
	}
	if got := uidPrefix("abc"); got != "abc" {
		t.Errorf("short uid -> %q", got)
	}
	if got := uidPrefix(""); got != "-" {
		t.Errorf("empty uid -> %q", got)
	}
}

func TestLogChatRowFormat(t *testing.T) {
	withChatLog(t)
	out := captureStdout(t, func() {
		logChatRow(412*time.Millisecond, 27100*time.Millisecond, "deepseek-v4-flash", "stream", "00e26541abcdef", "示例号", http.StatusOK, 1234)
	})
	for _, want := range []string{
		"| #", "deepseek-v4", "| stream |", "| 200 |", "示例号(00e26541)", "TTFB=412ms", "tok=1234", "tok/s   |", "total=",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("row missing %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "00e26541abcdef") {
		t.Errorf("full uid leaked: %s", out)
	}
}

func TestLogChatRowNoUsageShowsDash(t *testing.T) {
	withChatLog(t)
	out := captureStdout(t, func() {
		logChatRow(0, time.Second, "glm-5.2", "sync", "s1", "", http.StatusServiceUnavailable, -1)
	})
	for _, want := range []string{"TTFB=-", "tok=-", "tok=-      |", "| 503 |"} {
		if !strings.Contains(out, want) {
			t.Errorf("row missing %q:\n%s", want, out)
		}
	}
}

func TestLogChatRowExtendedFields(t *testing.T) {
	withChatLog(t)
	out := captureStdout(t, func() {
		logChatRowEx(10*time.Millisecond, 2*time.Second, "glm-5.3", "stream", "u123456789", "示例号",
			http.StatusOK, 42, "req-abc123", reqlog.OutcomeSuccess, 2, 1.25, true, "", "")
	})
	for _, want := range []string{"rid=req-abc123", "out=success", "try=2", "credit=1.2500"} {
		if !strings.Contains(out, want) {
			t.Errorf("extended row missing %q:\n%s", want, out)
		}
	}
	// если источник не собран — не добавлять src сегмент (формат старых строк не меняется, для совместимости со скриптами парсинга логов).
	if strings.Contains(out, "src=") {
		t.Errorf("empty source must not add src field:\n%s", out)
	}
}

// Источник вызова должен попасть в строку лога:IP использовать голое значение,UA использовать ShortUA Сжатый клиентский тег.
func TestLogChatRowSourceFields(t *testing.T) {
	withChatLog(t)
	out := captureStdout(t, func() {
		logChatRowEx(10*time.Millisecond, 2*time.Second, "glm-5.3", "stream", "u123456789", "示例号",
			http.StatusOK, 42, "req-abc123", reqlog.OutcomeSuccess, 1, 0, false,
			"203.0.113.7", "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/120.0.0.0 Safari/537.36")
	})
	for _, want := range []string{`src=203.0.113.7`, `ua="Chrome/120.0.0.0"`} {
		if !strings.Contains(out, want) {
			t.Errorf("source row missing %q:\n%s", want, out)
		}
	}
	// Полный браузер UA Не выводить весь фрагмент в строку лога (оставлять только клиентский тег).
	if strings.Contains(out, "AppleWebKit") {
		t.Errorf("full UA leaked into row:\n%s", out)
	}
}

// только IP Отсутствует UA（небраузерный клиент не отправляет заголовок),UA Отображение столбца "-"，IP Отображается как обычно.
func TestLogChatRowSourcePartial(t *testing.T) {
	withChatLog(t)
	out := captureStdout(t, func() {
		logChatRow(0, time.Second, "m", "sync", "u", "", 200, 1)
	})
	if strings.Contains(out, "src=") {
		t.Errorf("logChatRow has no source, want no src field:\n%s", out)
	}
}

func TestLogChatRowSeqIncrements(t *testing.T) {
	withChatLog(t)
	out := captureStdout(t, func() {
		logChatRow(0, time.Second, "m", "sync", "u", "", 200, 1)
		logChatRow(0, time.Second, "m", "sync", "u", "", 200, 1)
	})
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if len(lines) != 2 {
		t.Fatalf("want 2 lines, got %d:\n%s", len(lines), out)
	}
	first := strings.Fields(lines[0])[1]
	second := strings.Fields(lines[1])[1]
	if !strings.HasPrefix(first, "#") || !strings.HasPrefix(second, "#") {
		t.Fatalf("seq columns missing: %q %q", first, second)
	}
	if first == second {
		t.Errorf("seq not incremented: %q == %q", first, second)
	}
}

func TestChatLogsStreamRow(t *testing.T) {
	withChatLog(t)
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		return 200, sseOK, true
	})
	h := NewHandler(Config{
		Pool: testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999}),
		Upstream: up,
	})
	out := captureStdout(t, func() {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"glm-5.2","stream":true,"messages":[]}`))
		h.ServeHTTP(rec, req)
		if rec.Code != 200 {
			t.Fatalf("code=%d", rec.Code)
		}
	})
	for _, want := range []string{"| stream |", "| 200 |", "| u1 ", "TTFB=", "tok=1"} {
		if !strings.Contains(out, want) {
			t.Errorf("stream row missing %q:\n%s", want, out)
		}
	}
	if !strings.Contains(out, "tok=1") {
		t.Errorf("tok: want precise usage completion_tokens: %s", out)
	}
}

func TestChatLogsSyncRowTTFBDash(t *testing.T) {
	withChatLog(t)
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		return 200, sseOK, true
	})
	h := NewHandler(Config{
		Pool: testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999}),
		Upstream: up,
	})
	out := captureStdout(t, func() {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"glm-5.2","messages":[]}`))
		h.ServeHTTP(rec, req)
		if rec.Code != 200 {
			t.Fatalf("code=%d", rec.Code)
		}
	})
	for _, want := range []string{"| sync |", "| 200 |", "TTFB=-", "tok=1"} {
		if !strings.Contains(out, want) {
			t.Errorf("sync row missing %q:\n%s", want, out)
		}
	}
}

func TestChatLogsErrorRow(t *testing.T) {
	withChatLog(t)
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		return 402, `{"code":1,"msg":"余额不足"}`, false
	})
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	h := NewHandler(Config{Pool: p, Upstream: up})
	out := captureStdout(t, func() {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"glm-5.2","messages":[]}`))
		h.ServeHTTP(rec, req)
		if rec.Code != 503 {
			t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
		}
	})
	for _, want := range []string{"| u1 ", "| 503 |", "tok=-"} {
		if !strings.Contains(out, want) {
			t.Errorf("error row missing %q:\n%s", want, out)
		}
	}
}

func TestHealthzDoesNotLogTableRow(t *testing.T) {
	withChatLog(t) // 日志开启也应无表格行：非 chat 路由根本不走 logChatRow
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	h := NewHandler(Config{Pool: p, Upstream: newFakeUpstream(t, func(string) (int, string, bool) {
		return 200, sseOK, true
	})})
	out := captureStdout(t, func() {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest("GET", "/healthz", nil))
		if rec.Code != 200 {
			t.Fatalf("code=%d", rec.Code)
		}
		rec2 := httptest.NewRecorder()
		h.ServeHTTP(rec2, httptest.NewRequest("GET", "/v1/models", nil))
		rec3 := httptest.NewRecorder()
		h.ServeHTTP(rec3, httptest.NewRequest("GET", "/status", nil))
	})
	if strings.Contains(out, "| #") {
		t.Errorf("healthz/models/status must not emit table rows:\n%s", out)
	}
}
