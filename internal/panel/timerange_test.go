package panel

import (
	"encoding/json"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/pool"
	"github.com/linguo2625469/workbuddy2api-panel/internal/reqlog"
	"github.com/linguo2625469/workbuddy2api-panel/internal/usage"
)

// parseTimeParam это "сегодня / Единственная точка входа для интервала "кастом»: фронтенд по умолчанию отправляет unix с, вручную
// При вызове API можно использовать RFC3339 / datetime-local；невалидные значения и пустые строки — фолбэк"Без лимита"，
// Нельзя, чтобы опечатка в параметре обнуляла статистику всей страницы 500。
func TestParseTimeParam(t *testing.T) {
	want := time.Date(2026, 9, 30, 14, 5, 0, 0, time.Local)
	cases := []struct {
		name string
		in string
		want time.Time
	}{
		{"Пустая строка", "", time.Time{}},
		{"Пусто", " ", time.Time{}},
		{"unix с", "1786000000", time.Unix(1786000000, 0)},
		{"unix мс", "1786000000000", time.UnixMilli(1786000000000)},
		{"Нулевое значение как отсутствие лимита", "0", time.Time{}},
		{"отрицательные — как без лимита", "-5", time.Time{}},
		{"RFC3339", want.Format(time.RFC3339), want},
		{"Локально datetime-local", "2026-09-30T14:05", want},
		{"Недопустимый", "not-a-time", time.Time{}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := parseTimeParam(c.in)
			if !got.Equal(c.want) {
				t.Fatalf("parseTimeParam(%q) = %v, want %v", c.in, got, c.want)
			}
		})
	}
}

// usage Параметр интервала интерфейса должен реально применяться к агрегации:from/to при вступлении в силу hours игнорируется,
// И эхо в ответе window_from/window_to для сверки метрики на панели.
func TestUsageHandlerAcceptsExplicitRange(t *testing.T) {
	rec := usage.New("")
	base := time.Now().Truncate(time.Hour).Add(-5 * time.Hour)
	for i := 0; i < 6; i++ {
		rec.Add(base.Add(time.Duration(i)*time.Hour), "cn", "u1", "glm-5.2",
			usage.Delta{PromptTokens: 10, HasPromptTokens: true}, true)
	}
	p := New(Config{Version: "test", APIKey: "k", Usage: rec, Pool: pool.New("")})

	get := func(query string) map[string]any {
		t.Helper()
		req := httptest.NewRequest("GET", "/panel/api/usage"+query, nil)
		req.Header.Set("Authorization", "Bearer k")
		rr := httptest.NewRecorder()
		p.ServeHTTP(rr, req)
		if rr.Code != 200 {
			t.Fatalf("usage%s -> %d %s", query, rr.Code, rr.Body)
		}
		var out map[string]any
		if err := json.Unmarshal(rr.Body.Bytes(), &out); err != nil {
			t.Fatal(err)
		}
		return out
	}

	from := base.Add(2 * time.Hour).Unix()
	to := base.Add(3 * time.Hour).Unix()
	// одновременно передать hours=720：Параметры интервала должны иметь приоритет, иначе будет посчитано как 30 дневной полный объём.
	got := get("?hours=720&from=" + strconv.FormatInt(from, 10) + "&to=" + strconv.FormatInt(to, 10))
	totals := got["totals"].(map[string]any)
	if reqs := totals["requests"].(float64); reqs != 2 {
		t.Fatalf("Число запросов за интервал = %v, want 2（from/to должен иметь приоритет над hours）", reqs)
	}
	if got["window_from"] == nil || got["window_to"] == nil {
		t.Fatalf("ответ должен эхо-возвращать window_from/window_to: %+v", got)
	}

	// при отсутствии параметра интервала — fallback на дефолт 72h，и без отображения интервала.
	def := get("")
	if def["window_from"] != nil || def["window_to"] != nil {
		t.Fatalf("Окно по умолчанию не должно отображать интервал: %+v", def)
	}
	if r := def["totals"].(map[string]any)["requests"].(float64); r != 6 {
		t.Fatalf("лимит запросов окна по умолчанию = %v, want 6", r)
	}

	// hours=0（вся история) также без отображения интервала.
	all := get("?hours=0")
	if r := all["totals"].(map[string]any)["requests"].(float64); r != 6 {
		t.Fatalf("общее число исторических запросов = %v, want 6", r)
	}
}

// При пустом архиве интерфейс должен вернуть []，а не JSON null：фронтенд ... null и"Архивное закрытие"Смешаны вместе
// будет выбрана неверная ветка — покажет последний запрос вне временного интервала.
func TestRequestLogsEmptyRangeReturnsArray(t *testing.T) {
	rec := reqlog.New(reqlog.Config{Enabled: true, Dir: t.TempDir(), MaxBytes: 1 << 20, RetentionDays: 7})
	rec.Record(reqlog.Event{Time: time.Now().Add(-48 * time.Hour), RequestID: "old",
		Status: 200, OK: true, Outcome: reqlog.OutcomeSuccess})
	rec.Close()

	p := New(Config{Version: "test", APIKey: "k", Pool: pool.New(""), RequestLog: rec})
	future := strconv.FormatInt(time.Now().Add(time.Hour).Unix(), 10)
	req := httptest.NewRequest("GET", "/panel/api/request_logs?from="+future, nil)
	req.Header.Set("Authorization", "Bearer k")
	rr := httptest.NewRecorder()
	p.ServeHTTP(rr, req)
	if rr.Code != 200 {
		t.Fatalf("code=%d body=%s", rr.Code, rr.Body)
	}
	var out struct {
		Entries []reqlog.Event `json:"entries"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if out.Entries == nil {
		t.Fatalf("пустой интервал должен вернуть []，Получить null: %s", rr.Body)
	}
	if len(out.Entries) != 0 {
		t.Fatalf("Будущий интервал не должен попадать ни на одну запись: %+v", out.Entries)
	}
}
