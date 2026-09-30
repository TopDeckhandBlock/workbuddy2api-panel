package upstream

import (
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
)

// uaCaptureTransport Логирование исходящего запроса User-Agent。
type uaCaptureTransport struct {
	ua *string
}

func (t uaCaptureTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	*t.ua = r.Header.Get("User-Agent")
	return jsonResp(200, `{"code":0}`), nil
}

// TestUserAgentDefaultEmptyKeepsClientUA по умолчанию (UserAgent/client_name пусто) поведение:
// chat/refresh путь UA=По умолчанию WorkBuddy Трехсегментный;billing путь (report/travel/balance）
// один сегмент WorkBuddy/<ver>（выравнивание с официальным banner группа заголовков белого списка, по умолчанию подделка отпечатка десктопа);
// Явно client_name="SaaS« только тогда восстановление«billing Не задано UA"Старое поведение.
func TestUserAgentDefaultEmptyKeepsClientUA(t *testing.T) {
	for _, tc := range []struct {
		name string
		call func(c *Client) error
		wantUA string
	}{
		{
			name: "chat",
			call: func(c *Client) error {
				rc, status, _, err := c.ChatStream(&auth.Auth{AccessToken: "at", UID: "u1"}, []byte(`{"model":"glm-5.2","messages":[]}`), "", ChatMeta{})
				if status != 200 {
					t.Fatalf("chat status=%d", status)
				}
				if rc != nil {
					rc.Close()
				}
				return err
			},
			wantUA: defaultUAString,
		},
		{
			name: "billing_report",
			call: func(c *Client) error {
				return c.ReportChatActivity(&auth.Auth{AccessToken: "at", UID: "u1"}, "cid", "")
			},
			wantUA: "WorkBuddy/5.5.4",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var ua string
			c := &Client{
				HTTP: &http.Client{Transport: uaCaptureTransport{ua: &ua}},
				ChatHTTP: &http.Client{Transport: uaCaptureTransport{ua: &ua}},
				ChatBaseCN: "https://chat.example",
				BillingBaseCN: "https://billing.example",
			}
			if err := tc.call(c); err != nil {
				t.Fatalf("call: %v", err)
			}
			if ua != tc.wantUA {
				t.Errorf("UA = %q want %q", ua, tc.wantUA)
			}
		})
	}
}

// TestUserAgentOverrideAllOutbound После явной установки chat/billing/refresh полное покрытие путей.
// использовать env Прямая проверка алиаса fields Передача в headers поведения.
func TestUserAgentOverrideAllOutbound(t *testing.T) {
	a := &auth.Auth{AccessToken: "at", UID: "u1", RefreshToken: "rt"}
	ua := "WorkBuddy/9.9.9"
	c := &Client{
		HTTP: &http.Client{Transport: rtFunc(func(r *http.Request) (*http.Response, error) {
			if got := r.Header.Get("User-Agent"); got != ua {
				t.Errorf("UA = %q want %q (path=%s)", got, ua, r.URL.Path)
			}
			return jsonResp(200, `{"code":0}`), nil
		})},
		ChatHTTP: &http.Client{Transport: rtFunc(func(r *http.Request) (*http.Response, error) {
			if got := r.Header.Get("User-Agent"); got != ua {
				t.Errorf("Chat UA = %q want %q", got, ua)
			}
			return jsonResp(200, `{"code":0}`), nil
		})},
		ChatBaseCN: "https://chat.example",
		BillingBaseCN: "https://billing.example",
		UserAgent: ua,
	}
	// chat
	if rc, status, _, err := c.ChatStream(a, []byte(`{"model":"deepseek-v4-flash","messages":[]}`), "", ChatMeta{}); status != 200 || err != nil {
		t.Errorf("chat: status=%d err=%v", status, err)
	} else if rc != nil {
		rc.Close()
	}
	// refresh（RefreshHeaders→CommonHeaders）
	c.HTTP.Transport = rtFunc(func(r *http.Request) (*http.Response, error) {
		if got := r.Header.Get("User-Agent"); got != ua {
			t.Errorf("Refresh UA = %q want %q", got, ua)
		}
		return jsonResp(200, `{"code":0,"data":{"accessToken":"nat","refreshToken":"nrt"}}`), nil
	})
	if err := c.RefreshToken(a); err != nil {
		t.Errorf("refresh: %v", err)
	}
}

// TestUserAgentOverrideBilling Баланс/тип check-in billing Запрос также перекрывает.
func TestUserAgentOverrideBilling(t *testing.T) {
	a := &auth.Auth{AccessToken: "at", UID: "u1"}
	c := &Client{
		HTTP: &http.Client{Transport: rtFunc(func(r *http.Request) (*http.Response, error) {
			if got := r.Header.Get("User-Agent"); got != "CustomAgent/1" {
				t.Errorf("Billing UA = %q want CustomAgent/1", got)
			}
			return jsonResp(200, `{"code":0,"data":{"response":{"data":{"accounts":[{"PackageName":"x","CycleCapacitySize":100,"CycleCapacityUsed":0}]}}}}`), nil
		})},
		ChatBaseCN: "https://chat.example",
		BillingBaseCN: "https://billing.example",
		UserAgent: "CustomAgent/1",
	}
	if _, _, err := c.UserResource(a); err != nil {
		t.Errorf("userResource: %v", err)
	}
}

// TestFetchModelsUsesConfiguredUA FetchModels Вручную Set UA Тоже идёт через перекрытие.
func TestFetchModelsUsesConfiguredUA(t *testing.T) {
	a := &auth.Auth{AccessToken: "at", UID: "u1"}
	c := &Client{
		HTTP: &http.Client{Transport: rtFunc(func(r *http.Request) (*http.Response, error) {
			switch {
			case strings.HasSuffix(r.URL.Path, "/console/enterprises/personal/models"):
				if got := r.Header.Get("User-Agent"); got != "FetchAgent/2" {
					t.Errorf("personal/models UA = %q want FetchAgent/2", got)
				}
			case strings.HasSuffix(r.URL.Path, "/v3/config"):
				if got := r.Header.Get("User-Agent"); got != codeBuddyIDEUA {
					t.Errorf("v3/config UA = %q want %s", got, codeBuddyIDEUA)
				}
				return jsonResp(200, `{"code":0,"data":{"models":[]}}`), nil
			default:
				t.Errorf("path=%s", r.URL.Path)
			}
			return jsonResp(200, `{"code":0,"data":{"models":[{"id":"glm-5.2","name":"GLM","maxInputTokens":131072,"maxOutputTokens":8192,"reasoning":{"effort":"high","supportedEfforts":[]},"disabled":false}],"agents":[{"name":"cli","models":["glm-5.2"]}]}}`), nil
		})},
		ChatBaseCN: "https://chat.example",
		BillingBaseCN: "https://billing.example",
		UserAgent: "FetchAgent/2",
	}
	if _, err := c.FetchModels(a); err != nil {
		t.Errorf("fetchModels: %v", err)
	}
}

// --- A Сегмент:UA выравнивание с официальным WorkBuddy трехсегментный ---

const (
	defaultUAString = "WorkBuddy/5.5.4 WorkBuddy/5.5.4 CLI/2.137.1"
	explicitString = "MyCustomAgent/3.1"
	clientVerUAString = "WorkBuddy/6.0.0 WorkBuddy/6.0.0 CLI/2.137.1"
	billingUAWorkBuddy = "WorkBuddy/5.5.4"
	billingUACustomVer = "WorkBuddy/6.0.0"
	billingUAAgentString = "BillingAgent/1"
)

// TestUserAgentDefaultWorkBuddyShape Чат по умолчанию (без конфигурации)/Обновление исходящего UA =
// Официальный WorkBuddy Трехсегментный, старое значение `CLI/2.63.2 CodeBuddy/2.63.2` Уже заменено выравниванием.
func TestUserAgentDefaultWorkBuddyShape(t *testing.T) {
	a := &auth.Auth{AccessToken: "at", UID: "u1"}
	c := &Client{
		HTTP: &http.Client{Transport: uaCaptureTransport{ua: new(string)}},
		ChatHTTP: &http.Client{Transport: uaCaptureTransport{ua: new(string)}},
		ChatBaseCN: "https://chat.example",
		BillingBaseCN: "https://billing.example",
	}
	rc, status, _, err := c.ChatStream(a, []byte(`{"model":"deepseek-v4-flash","messages":[]}`), "", ChatMeta{})
	if status != 200 || err != nil {
		t.Fatalf("chat: status=%d err=%v", status, err)
	}
	if ua := c.chatLastUA(); ua != defaultUAString {
		t.Errorf("chat UA = %q want %q", ua, defaultUAString)
	}
	if rc != nil {
		rc.Close()
	}
}

// chatLastUA Захват из последнего запроса чата UA（текущий тест Client ChatHTTP transport запись).
func (c *Client) chatLastUA() string {
	if t, ok := c.ChatHTTP.Transport.(uaCaptureTransport); ok && t.ua != nil {
		return *t.ua
	}
	return ""
}

// TestUserAgentExplicitOverride config user_agent При непустом значении приоритет у явно заданного пользователем (совм. со старой логикой перезаписи).
func TestUserAgentExplicitOverride(t *testing.T) {
	a := &auth.Auth{AccessToken: "at", UID: "u1", RefreshToken: "rt"}
	c := &Client{
		HTTP: &http.Client{Transport: rtFunc(func(r *http.Request) (*http.Response, error) {
			if got := r.Header.Get("User-Agent"); got != explicitString {
				t.Errorf("UA = %q want %q (path=%s)", got, explicitString, r.URL.Path)
			}
			return jsonResp(200, `{"code":0,"data":{"accessToken":"nat","refreshToken":"nrt"}}`), nil
		})},
		ChatHTTP: &http.Client{Transport: uaCaptureTransport{ua: new(string)}},
		ChatBaseCN: "https://chat.example",
		BillingBaseCN: "https://billing.example",
		UserAgent: explicitString,
	}
	if rc, status, _, err := c.ChatStream(a, []byte(`{"model":"deepseek-v4-flash","messages":[]}`), "", ChatMeta{}); status != 200 || err != nil {
		t.Errorf("chat: status=%d err=%v", status, err)
	} else if rc != nil {
		rc.Close()
	}
	if got := c.chatLastUA(); got != explicitString {
		t.Errorf("chat UA = %q want %q", got, explicitString)
	}
	if err := c.RefreshToken(a); err != nil {
		t.Errorf("refresh: %v", err)
	}
}

// TestUserAgentClientVersionOverride config client_version вступает в силу:UA WorkBuddy Сегмент следует
// и попарно совпадают (platform Сегмент = applicationName сегмент),CLI сегмент остаётся по умолчанию.
func TestUserAgentClientVersionOverride(t *testing.T) {
	a := &auth.Auth{AccessToken: "at", UID: "u1"}
	c := &Client{
		HTTP: &http.Client{Transport: uaCaptureTransport{ua: new(string)}},
		ChatHTTP: &http.Client{Transport: uaCaptureTransport{ua: new(string)}},
		ChatBaseCN: "https://chat.example",
		BillingBaseCN: "https://billing.example",
		ClientVersion: "6.0.0",
	}
	rc, status, _, err := c.ChatStream(a, []byte(`{"model":"deepseek-v4-flash","messages":[]}`), "", ChatMeta{})
	if status != 200 || err != nil {
		t.Fatalf("chat: status=%d err=%v", status, err)
	}
	if got := c.chatLastUA(); got != clientVerUAString {
		t.Errorf("UA = %q want %q", got, clientVerUAString)
	}
	if rc != nil {
		rc.Close()
	}
}

// TestBillingUA_WhenClientNameSet billing/checkin Путь:client_name если непусто — использовать один сегмент
// `WorkBuddy/<clientVersion>`（без CLI сегмент, выравнивание с официальным banner/check-in явная группа заголовков).
func TestBillingUA_WhenClientNameSet(t *testing.T) {
	a := &auth.Auth{AccessToken: "at", UID: "u1"}
	// По умолчанию client_version → WorkBuddy/5.5.4
	c := &Client{
		HTTP: &http.Client{Transport: rtFunc(func(r *http.Request) (*http.Response, error) {
			if got := r.Header.Get("User-Agent"); got != billingUAWorkBuddy {
				t.Errorf("billing UA = %q want %q (path=%s)", got, billingUAWorkBuddy, r.URL.Path)
			}
			return jsonResp(200, `{"code":0,"data":{"response":{"data":{"accounts":[{"PackageName":"x","CycleCapacitySize":100,"CycleCapacityUsed":0}]}}}}`), nil
		})},
		ChatBaseCN: "https://chat.example",
		BillingBaseCN: "https://billing.example",
		ClientName: "WorkBuddy",
	}
	if _, _, err := c.UserResource(a); err != nil {
		t.Errorf("userResource: %v", err)
	}
	// Пользовательский client_version → WorkBuddy/6.0.0
	c2 := &Client{
		HTTP: &http.Client{Transport: rtFunc(func(r *http.Request) (*http.Response, error) {
			if got := r.Header.Get("User-Agent"); got != billingUACustomVer {
				t.Errorf("billing UA = %q want %q", got, billingUACustomVer)
			}
			return jsonResp(200, `{"code":0,"data":{"response":{"data":{"accounts":[{"PackageName":"x","CycleCapacitySize":100,"CycleCapacityUsed":0}]}}}}`), nil
		})},
		ChatBaseCN: "https://chat.example",
		BillingBaseCN: "https://billing.example",
		ClientName: "WorkBuddy",
		ClientVersion: "6.0.0",
	}
	if _, _, err := c2.UserResource(a); err != nil {
		t.Errorf("userResource v2: %v", err)
	}
	// Явно user_agent Всё ещё приоритетнее billingUA
	c3 := &Client{
		HTTP: &http.Client{Transport: rtFunc(func(r *http.Request) (*http.Response, error) {
			if got := r.Header.Get("User-Agent"); got != billingUAAgentString {
				t.Errorf("billing UA = %q want %q", got, billingUAAgentString)
			}
			return jsonResp(200, `{"code":0,"data":{"response":{"data":{"accounts":[{"PackageName":"x","CycleCapacitySize":100,"CycleCapacityUsed":0}]}}}}`), nil
		})},
		ChatBaseCN: "https://chat.example",
		BillingBaseCN: "https://billing.example",
		ClientName: "WorkBuddy",
		UserAgent: billingUAAgentString,
	}
	if _, _, err := c3.UserResource(a); err != nil {
		t.Errorf("userResource v3: %v", err)
	}
}

// TestBillingUA_WhenClientNameEmpty client_name пустой = По умолчанию как в официальном десктоп-клиенте:
// billing UA один сегмент WorkBuddy/<clientVersion>；Явно client_name="SaaS" Не задавать UA。
func TestBillingUA_WhenClientNameEmpty(t *testing.T) {
	a := &auth.Auth{AccessToken: "at", UID: "u1"}
	var ua string
	const fullResp = `{"code":0,"data":{"response":{"data":{"accounts":[{"PackageName":"x","CycleCapacitySize":100,"CycleCapacityUsed":0}]}}}}`
	c := &Client{
		HTTP: &http.Client{Transport: rtFunc(func(r *http.Request) (*http.Response, error) {
			ua = r.Header.Get("User-Agent")
			return jsonResp(200, fullResp), nil
		})},
		ChatBaseCN: "https://chat.example",
		BillingBaseCN: "https://billing.example",
		ClientVersion: "6.0.0",
	}
	if _, _, err := c.UserResource(a); err != nil {
		t.Errorf("userResource: %v", err)
	}
	if ua != "WorkBuddy/6.0.0" {
		t.Errorf("billing UA = %q want WorkBuddy/6.0.0 (default desktop fingerprint)", ua)
	}
	if got := c.billingUA(); got != "WorkBuddy/6.0.0" {
		t.Errorf("billingUA() = %q want WorkBuddy/6.0.0", got)
	}
	// Явно SaaS Восстановить старое поведение (без установки UA）。
	c.ClientName = "SaaS"
	if got := c.billingUA(); got != "" {
		t.Errorf("billingUA() SaaS = %q want empty", got)
	}
}

var _ = io.Discard
