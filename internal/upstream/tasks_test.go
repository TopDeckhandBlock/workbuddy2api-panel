package upstream

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
)

// TestClaimRewardWebEndpoint получение награды через Web Домен (workbuddy.cn）、код задачи в пути, без body。
// Это с CLI Домен (copilot.tencent.com/v2/.../reward/claim，task_code В body）Ключевое отличие —
// последний путь не существует, ранее приводило к длительному 400 "task not completed« ошибочно классифицировано как«апстрим не поддерживает получение"。
func TestClaimRewardWebEndpoint(t *testing.T) {
	var gotPath, gotMethod, gotBody string
	var gotPlatform, gotReferer string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotMethod = r.URL.Path, r.Method
		gotPlatform = r.Header.Get("x-client-platform")
		gotReferer = r.Header.Get("Referer")
		buf := make([]byte, 64)
		n, _ := r.Body.Read(buf)
		gotBody = string(buf[:n])
		w.Write([]byte(`{"code":0,"msg":"OK","data":{"already_claimed":false,"credit":100,"energy":5}}`))
	}))
	defer srv.Close()

	c := &Client{HTTP: srv.Client(), ChatBaseCN: srv.URL, BillingBaseCN: srv.URL, WebBaseCN: srv.URL}
	a := &auth.Auth{AccessToken: "at", UID: "u1"}

	credit, energy, err := c.ClaimReward(a, "Model_chat_GLM5.2")
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	if credit != 100 || energy != 5 {
		t.Errorf("credit/energy = %d/%d, want 100/5", credit, energy)
	}
	if gotMethod != http.MethodPost {
		t.Errorf("method=%s want POST", gotMethod)
	}
	if want := "/activity/growth/tasks/Model_chat_GLM5.2/claim"; gotPath != want {
		t.Errorf("path=%q want %q（код задачи обязателен в пути)", gotPath, want)
	}
	if gotBody != "" {
		t.Errorf("claim не должен содержать body，got %q", gotBody)
	}
	if gotPlatform != "web" {
		t.Errorf("x-client-platform=%q want web", gotPlatform)
	}
	if !strings.Contains(gotReferer, "workbuddy.cn") {
		t.Errorf("Referer=%q Должен указывать на workbuddy.cn", gotReferer)
	}
}

// TestClaimRewardAlreadyClaimed Повторное получение: апстрим возвращает already_claimed=true，
// локально считать как"Без доп. награды, но без ошибки"（семантика идемпотентности).
func TestClaimRewardAlreadyClaimed(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"code":0,"msg":"OK","data":{"already_claimed":true}}`))
	}))
	defer srv.Close()
	c := &Client{HTTP: srv.Client(), WebBaseCN: srv.URL}
	credit, energy, err := c.ClaimReward(&auth.Auth{AccessToken: "at", UID: "u1"}, "chat_5")
	if err != nil {
		t.Fatalf("already_claimed should not error: %v", err)
	}
	if credit != 0 || energy != 0 {
		t.Errorf("already claimed should yield 0/0, got %d/%d", credit, energy)
	}
}

// TestClaimRewardNotCompleted Не достигнуто: upstream 400 + task not completed должно пробрасываться как ошибка.
func TestClaimRewardNotCompleted(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(400)
		json.NewEncoder(w).Encode(map[string]any{"code": 400, "msg": "task not completed"})
	}))
	defer srv.Close()
	c := &Client{HTTP: srv.Client(), WebBaseCN: srv.URL}
	if _, _, err := c.ClaimReward(&auth.Auth{AccessToken: "at", UID: "u1"}, "chat_5"); err == nil {
		t.Fatal("want error for not-completed task")
	}
}
