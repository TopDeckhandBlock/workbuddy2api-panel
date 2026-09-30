package panel

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

// model_probes Три состояния эндпоинта: файл существует → Прозрачная передача + exists:true + updated_at；
// Не настроено / Файл отсутствует → пустое множество (панель деградирует к состоянию без меток); файл повреждён → 502。
// поле контракта (claimed/measured/verdict…）потребляется фронтендом, шлюз только проксирует без парсинга.
func TestModelProbesEndpoint(t *testing.T) {
	dir := t.TempDir()
	f := filepath.Join(dir, "output_probes.json")
	body := `{"version":1,"probes":{"cn:glm-5.2":{"claimed":131072,"measured":48000,` +
		`"verdict":"clamped","note":"кламп","tested_at":"2026-09-15 18:30:00","source":"probe_max_tokens.py"}}}`
	if err := os.WriteFile(f, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}

	get := func(cfg Config) *httptest.ResponseRecorder {
		p := New(cfg)
		req := httptest.NewRequest("GET", "/panel/api/model_probes", nil)
		req.Header.Set("Authorization", "Bearer test-key")
		rec := httptest.NewRecorder()
		p.ServeHTTP(rec, req)
		return rec
	}

	// 1) файл существует: сквозная передача + exists + updated_at
	rec := get(Config{Version: "test", APIKey: "test-key", ProbeFile: f})
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body.String())
	}
	var got struct {
		Probes map[string]json.RawMessage `json:"probes"`
		Exists bool `json:"exists"`
		UpdatedAt string `json:"updated_at"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if !got.Exists || len(got.Probes) != 1 || got.UpdatedAt == "" {
		t.Fatalf("exists=%v probes=%d updated_at=%q", got.Exists, len(got.Probes), got.UpdatedAt)
	}
	var rec1 struct {
		Verdict string `json:"verdict"`
		Measured int64 `json:"measured"`
	}
	if err := json.Unmarshal(got.Probes["cn:glm-5.2"], &rec1); err != nil {
		t.Fatal(err)
	}
	if rec1.Verdict != "clamped" || rec1.Measured != 48000 {
		t.Fatalf("probe passthrough = %+v", rec1)
	}

	// 2) Файл отсутствует: пустое множество 200（Не ошибка — нет данных = Без меток).
	// обратите внимание, каждый case использовать новую структуру: к уже не nil map повторно Unmarshal Это слияние, а не замена.
	rec = get(Config{Version: "test", APIKey: "test-key", ProbeFile: filepath.Join(dir, "nope.json")})
	if rec.Code != http.StatusOK {
		t.Fatalf("missing file: code=%d want 200", rec.Code)
	}
	var gotEmpty struct {
		Probes map[string]json.RawMessage `json:"probes"`
		Exists bool `json:"exists"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &gotEmpty); err != nil {
		t.Fatal(err)
	}
	if gotEmpty.Exists || len(gotEmpty.Probes) != 0 {
		t.Fatalf("missing file: exists=%v probes=%d, want false/0", gotEmpty.Exists, len(gotEmpty.Probes))
	}

	// 3) не настроено: аналогично отсутствию файла
	rec = get(Config{Version: "test", APIKey: "test-key"})
	if rec.Code != http.StatusOK {
		t.Fatalf("unconfigured: code=%d want 200", rec.Code)
	}

	// 4) повреждение файла:502（показать на панели ошибку чтения вместо тихой пустоты)
	bad := filepath.Join(dir, "bad.json")
	if err := os.WriteFile(bad, []byte(`{not json`), 0o600); err != nil {
		t.Fatal(err)
	}
	rec = get(Config{Version: "test", APIKey: "test-key", ProbeFile: bad})
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("corrupt file: code=%d want 502", rec.Code)
	}
}
