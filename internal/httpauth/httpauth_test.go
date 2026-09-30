package httpauth

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func req(authz string) *http.Request {
	r := httptest.NewRequest("GET", "/", nil)
	if authz != "" {
		r.Header.Set("Authorization", authz)
	}
	return r
}

func TestVerifyBearer(t *testing.T) {
	cases := []struct {
		name string
		key string
		authz string
		want bool
	}{
		{"пустой key пропуск (аутентификация не включена)", "", "", true},
		{"пустой key также пропускает любой заголовок", "", "Bearer whatever", true},
		{"Корректно key", "sk-abc123", "Bearer sk-abc123", true},
		{"Ошибка key", "sk-abc123", "Bearer sk-wrong", false},
		{"нехватка Authorization Заголовок", "sk-abc123", "", false},
		{"нехватка Bearer Префикс", "sk-abc123", "sk-abc123", false},
		{"Несовпадение регистра префикса (спецификация требует точного соответствия)", "sk-abc123", "bearer sk-abc123", false},
		{"Лишние пробелы", "sk-abc123", "Bearer  sk-abc123", false},
		{"префикс тот же, но контент короче", "sk-abc123", "Bearer sk-abc12", false},
		{"Префикс одинаковый, но контент длиннее", "sk-abc123", "Bearer sk-abc1234", false},
		{"key Является префиксом", "sk-abc", "Bearer sk-abcdef", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := VerifyBearer(req(c.authz), c.key); got != c.want {
				t.Errorf("VerifyBearer(key=%q, authz=%q) = %v, want %v", c.key, c.authz, got, c.want)
			}
		})
	}
}

// TestVerifyBearerWithoutHeaderStillCompares путь без заголовка не должен из-за"Ранний возврат"а выявляется разница форм:
// Здесь проверяется только факт возврата false и не panic（свойство константного времени нельзя проверить юнит-тестом, гарантируется реализацией).
func TestVerifyBearerWithoutHeaderStillCompares(t *testing.T) {
	if VerifyBearer(req(""), "any-key") {
		t.Error("missing header must not pass")
	}
}

func TestDigestIsFixedLength(t *testing.T) {
	// Хеши входов разной длины должны быть равной длины (предусловие сравнения за константное время)
	if len(digest("")) != len(digest("a-much-longer-secret-value")) {
		t.Error("digest length must not depend on input length")
	}
	if len(digest("x")) != 32 {
		t.Errorf("sha256 digest length = %d, want 32", len(digest("x")))
	}
}
