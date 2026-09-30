// device_token_test.go X-Device-Token инжект + Юнит-тест fallback-чтения файла.
package upstream

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
)

// TestDeviceTokenInjected_WhenSet auth.Auth.DeviceToken Если не пусто chat/billing Инъекция во все запросы.
func TestDeviceTokenInjected_WhenSet(t *testing.T) {
	a := &auth.Auth{AccessToken: "at", UID: "u1", DeviceToken: "tok-from-auth"}
	// использовать server валидация на стороне, а не RoundTripper Перехват: ближе к реальному пути инъекции.
	for _, tc := range []struct {
		name string
		apply func(c *Client, req *http.Request)
		wantPath string
	}{
		{"chat", func(c *Client, req *http.Request) { c.ChatHeaders(req, a, "", ChatMeta{}) }, "/v2/chat/completions"},
		{"billing", func(c *Client, req *http.Request) { c.BillingHeaders(req, a) }, "/v2/report"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var got string
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				got = r.Header.Get("X-Device-Token")
				w.WriteHeader(200)
				_, _ = w.Write([]byte(`{"code":0}`))
			}))
			defer srv.Close()
			c := &Client{
				HTTP: srv.Client(),
				ChatHTTP: srv.Client(),
				ChatBaseCN: srv.URL,
				BillingBaseCN: srv.URL,
			}
			req, _ := http.NewRequest(http.MethodPost, srv.URL+tc.wantPath, nil)
			tc.apply(c, req)
			resp, err := c.HTTP.Do(req)
			if err != nil {
				t.Fatalf("do: %v", err)
			}
			resp.Body.Close()
			if got != "tok-from-auth" {
				t.Errorf("X-Device-Token = %q want %q", got, "tok-from-auth")
			}
		})
	}
}

// TestDeviceTokenNotInjected_WhenEmpty при пустых всех источниках заголовок не инжектировать.
func TestDeviceTokenNotInjected_WhenEmpty(t *testing.T) {
	a := &auth.Auth{AccessToken: "at", UID: "u1"} // DeviceToken пустой
	var got string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Get("X-Device-Token")
		w.WriteHeader(200)
		_, _ = w.Write([]byte(`{"code":0}`))
	}))
	defer srv.Close()
	c := &Client{
		HTTP: srv.Client(),
		ChatHTTP: srv.Client(),
		ChatBaseCN: srv.URL,
		BillingBaseCN: srv.URL,
		// DeviceToken / DeviceTokenFile Всё пусто
	}
	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/v2/chat/completions", nil)
	c.ChatHeaders(req, a, "", ChatMeta{})
	resp, err := c.HTTP.Do(req)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	resp.Body.Close()
	if got != "" {
		t.Errorf("X-Device-Token = %q want empty (not injected)", got)
	}
}

// TestDeviceTokenFromConfigOrFile_Overrides Приоритет:auth > config > файл.
// auth при наличии значения перезаписать config；auth когда пусто config запасной вариант;auth и config Если всё пусто — читать файл.
func TestDeviceTokenFromConfigOrFile_Overrides(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "device_token")
	if err := os.WriteFile(fp, []byte("tok-from-file\n"), 0o600); err != nil {
		t.Fatalf("write file: %v", err)
	}

	cases := []struct {
		name string
		auth string
		cfg string
		want string
	}{
		{"auth_over_config", "tok-auth", "tok-config", "tok-auth"},
		{"config_when_auth_empty", "", "tok-config", "tok-config"},
		{"file_when_auth_and_config_empty", "", "", "tok-from-file"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// для каждого кейса — отдельный кэш:device token Файловый кэш 5 мин.,case будет интерференция.
			save := resetDeviceTokenFileCache(fp)
			defer save()
			a := &auth.Auth{AccessToken: "at", UID: "u1", DeviceToken: tc.auth}
			var got string
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				got = r.Header.Get("X-Device-Token")
				w.WriteHeader(200)
				_, _ = w.Write([]byte(`{"code":0}`))
			}))
			defer srv.Close()
			c := &Client{
				HTTP: srv.Client(),
				ChatHTTP: srv.Client(),
				ChatBaseCN: srv.URL,
				BillingBaseCN: srv.URL,
				DeviceToken: tc.cfg,
				DeviceTokenFile: fp,
			}
			req, _ := http.NewRequest(http.MethodPost, srv.URL+"/v2/chat/completions", nil)
			c.ChatHeaders(req, a, "", ChatMeta{})
			resp, err := c.HTTP.Do(req)
			if err != nil {
				t.Fatalf("do: %v", err)
			}
			resp.Body.Close()
			if got != tc.want {
				t.Errorf("X-Device-Token = %q want %q", got, tc.want)
			}
		})
	}
}

// TestDeviceTokenFileTooLarge файл превышает 1KB при — игнорировать, не инжектировать.
func TestDeviceTokenFileTooLarge(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "device_token")
	if err := os.WriteFile(fp, []byte(strings.Repeat("x", 2048)), 0o600); err != nil {
		t.Fatalf("write file: %v", err)
	}
	save := resetDeviceTokenFileCache(fp)
	defer save()
	a := &auth.Auth{AccessToken: "at", UID: "u1"}
	c := &Client{DeviceTokenFile: fp}
	if tok := c.resolveDeviceToken(a); tok != "" {
		t.Errorf("resolveDeviceToken() = %q want empty (file too large)", tok)
	}
}

// resetDeviceTokenFileCache заменить глобально device token Кэшировать в файл и вернуть функцию восстановления.
// Файловый кэш 5 минут TTL，между тестами требуется очистка во избежание перекрестных помех.
func resetDeviceTokenFileCache(path string) (restore func()) {
	dtFileCache.mu.Lock()
	origPath := dtFileCache.path
	origTok := dtFileCache.token
	origRead := dtFileCache.readAt
	origErr := dtFileCache.lastErr
	dtFileCache.path = path
	dtFileCache.token = ""
	dtFileCache.readAt = time.Time{}
	dtFileCache.lastErr = nil
	dtFileCache.mu.Unlock()
	var once sync.Once
	return func() {
		once.Do(func() {
			dtFileCache.mu.Lock()
			dtFileCache.path = origPath
			dtFileCache.token = origTok
			dtFileCache.readAt = origRead
			dtFileCache.lastErr = origErr
			dtFileCache.mu.Unlock()
		})
	}
}
