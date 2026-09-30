package auth

import (
	"os"
	"path/filepath"
	"testing"
)

// withGlobalEnabled Временно открыть global realm переключатель (в проде по умолчанию вкл., этот хелпер лишь явно гарантирует),
// по завершении теста сброс во включенное состояние (по умолчанию).
func withGlobalEnabled(t *testing.T) {
	t.Helper()
	globalEnabled.Store(true)
	t.Cleanup(func() { globalEnabled.Store(true) })
}

// withGlobalDisabled Временно отключено global realm Переключатель (аварийный выход), по окончании теста сбрасывается во включённое состояние (по умолчанию).
func withGlobalDisabled(t *testing.T) {
	t.Helper()
	globalEnabled.Store(false)
	t.Cleanup(func() { globalEnabled.Store(true) })
}

func TestRealmExplicitGlobal(t *testing.T) {
	withGlobalEnabled(t)
	a := &Auth{realm: "global"}
	if got := a.Realm(); got != "global" {
		t.Errorf("Realm()=%q want global", got)
	}
	if !a.IsGlobal() {
		t.Error("IsGlobal()=false want true")
	}
}

func TestRealmExplicitCN(t *testing.T) {
	a := &Auth{realm: "cn"}
	if got := a.Realm(); got != "cn" {
		t.Errorf("Realm()=%q want cn", got)
	}
	if a.IsGlobal() {
		t.Error("IsGlobal()=true want false")
	}
}

func TestRealmDomainFallback(t *testing.T) {
	withGlobalEnabled(t)
	cases := []struct{ domain, want string }{
		{"www.workbuddy.ai", "global"},
		{"workbuddy.ai", "global"},
		{"sub.workbuddy.ai", "global"},
		{"www.codebuddy.cn", "cn"},
		{"", "cn"},
	}
	for _, c := range cases {
		a := &Auth{Domain: c.domain}
		if got := a.Realm(); got != c.want {
			t.Errorf("Domain=%q Realm()=%q want %q", c.domain, got, c.want)
		}
	}
}

func TestRealmEmptyFallsBackToCN(t *testing.T) {
	// переключатель по умолчанию включён (условие нулевой регрессии): пусто realm + пустой domain → cn（Старый CN ядро учетных данных).
	a := &Auth{}
	if got := a.Realm(); got != "cn" {
		t.Errorf("Realm()=%q want cn", got)
	}
	// Явно global → global（по умолчанию включено,Realm() больше не из-за"Не настроено"а константа cn）。
	ag := &Auth{realm: "global"}
	if got := ag.Realm(); got != "global" {
		t.Errorf("Realm()=%q want global", got)
	}
	// domain Фолбэк штатно (по умолчанию распознавание включено workbuddy.ai）。
	ad := &Auth{Domain: "www.workbuddy.ai"}
	if got := ad.Realm(); got != "global" {
		t.Errorf("Realm()=%q want global", got)
	}
}

// TestRealmDefaultOnForCNZeroRegression При переключателе по умолчанию вкл. старый CN учётные данные (без realm、отсутствует domain）
// Realm() Всегда равно cn——「включено по умолчанию» не влияет на чистый CN Поведение деплоя.
func TestRealmDefaultOnForCNZeroRegression(t *testing.T) {
	withGlobalEnabled(t)
	cases := []*Auth{
		{},
		{Domain: "www.codebuddy.cn"},
		{Domain: "codebuddy.cn"},
		{realm: "cn"},
		{realm: "cn", Domain: "www.codebuddy.cn"},
	}
	for _, a := range cases {
		if got := a.Realm(); got != "cn" {
			t.Errorf("%+v Realm()=%q want cn", a, got)
		}
		if a.IsGlobal() {
			t.Errorf("%+v IsGlobal()=true want false", a)
		}
	}
}

// TestRealmExplicitOffEscapeHatch Аварийный выход:SetGlobalEnabled(false) после постоянно cn，
// даже если realm=global / domain=workbuddy.ai（Чистый CN заблокировано, эквивалентно старому поведению по умолчанию).
func TestRealmExplicitOffEscapeHatch(t *testing.T) {
	withGlobalDisabled(t)
	cases := []struct {
		auth *Auth
	}{
		{&Auth{realm: "global"}},
		{&Auth{realm: "global", Domain: "www.workbuddy.ai"}},
		{&Auth{Domain: "www.workbuddy.ai"}},
	}
	for _, tc := range cases {
		if got := tc.auth.Realm(); got != "cn" {
			t.Errorf("%+v Realm()=%q want cn (switch off)", tc.auth, got)
		}
		if tc.auth.IsGlobal() {
			t.Errorf("%+v IsGlobal()=true want false (switch off)", tc.auth)
		}
	}
}

func TestParseNestedRealm(t *testing.T) {
	raw := []byte(`{"auth":{"accessToken":"at","refreshToken":"rt","expiresAt":1,"domain":"www.workbuddy.ai","realm":"global"},"account":{"uid":"u1"}}`)
	sa, err := Parse(raw)
	if err != nil {
		t.Fatalf("nested parse err: %v", err)
	}
	if sa.realm != "global" {
		t.Errorf("nested realm=%q want global", sa.realm)
	}
	// Явная вложенная форма realm пустой → не читается, через domain Fallback.
	raw2 := []byte(`{"auth":{"accessToken":"at","refreshToken":"rt","expiresAt":1},"account":{"uid":"u1"}}`)
	sa2, err := Parse(raw2)
	if err != nil {
		t.Fatalf("nested no-realm parse err: %v", err)
	}
	if sa2.realm != "" {
		t.Errorf("nested missing realm key should be zero, got %q", sa2.realm)
	}
}

func TestParseFlatRealm(t *testing.T) {
	withGlobalEnabled(t)
	raw := []byte(`{"accessToken":"at","refreshToken":"rt","expiresAt":1,"uid":"u2","realm":"global"}`)
	fa, err := Parse(raw)
	if err != nil {
		t.Fatalf("flat parse err: %v", err)
	}
	if fa.realm != "global" {
		t.Errorf("flat realm=%q want global", fa.realm)
	}
	if !fa.IsGlobal() {
		t.Error("flat global account IsGlobal()=false want true")
	}
	// Плоская форма отсутствует realm Клавиша → нулевое значение → CN。
	flatCN := []byte(`{"accessToken":"at","refreshToken":"rt","expiresAt":1,"uid":"u3"}`)
	fc, err := Parse(flatCN)
	if err != nil {
		t.Fatalf("flat cn parse err: %v", err)
	}
	if fc.realm != "" {
		t.Errorf("flat missing realm should be zero, got %q", fc.realm)
	}
}

func TestSaveAtomicWritesRealm(t *testing.T) {
	withGlobalEnabled(t)
	dir := t.TempDir()
	fp := filepath.Join(dir, "workbuddy-global.json")
	a := &Auth{AccessToken: "at", RefreshToken: "rt", ExpiresAt: 1,
		UID: "g1", realm: "global", FilePath: fp}
	if err := a.SaveAtomic(); err != nil {
		t.Fatalf("save: %v", err)
	}
	raw, err := os.ReadFile(fp)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	b, err := Parse(raw)
	if err != nil {
		t.Fatalf("reparse: %v", err)
	}
	if b.realm != "global" {
		t.Errorf("roundtrip realm=%q want global", b.realm)
	}
	if !b.IsGlobal() {
		t.Error("roundtrip global account IsGlobal()=false")
	}
}

// TestBackfillRealmDomain CN/global по исходному domain вывод (backfill как пустой экспорт realm сервис полей).
func TestBackfillRealmDomain(t *testing.T) {
	t.Parallel() // Не трогать глобальный переключатель
	cases := []struct {
		domain string
		want string
	}{
		{"www.workbuddy.ai", "global"},
		{"workbuddy.ai", "global"},
		{"sub.workbuddy.ai", "global"},
		{"www.codebuddy.cn", "cn"},
		{"codebuddy.cn", "cn"},
		{"", "cn"}, // пустой domain + пустой realm → cn（Старый CN ядро учётных данных)
	}
	for _, c := range cases {
		a := &Auth{Domain: c.domain}
		changed, got := a.BackfillRealm()
		if !changed {
			t.Errorf("Domain=%q backfill changed=false want true", c.domain)
		}
		assertRealmStored(a, c.want, t)
		if got != c.want {
			t.Errorf("Domain=%q backfill got realm=%q want %q", c.domain, got, c.want)
		}
	}
}

// TestBackfillRealmEscapeHatchFree при закрытой аварийной лазейке backfill всё равно по исходному domain Вывод (не зависит от Realm() влияние даунгрейда).
func TestBackfillRealmEscapeHatchFree(t *testing.T) {
	withGlobalDisabled(t) // Аварийный выход закрыт:Realm() Конст. cn，Но backfill не должно быть загрязнено
	a := &Auth{Domain: "www.workbuddy.ai"}
	if g := a.Realm(); g != "cn" {
		t.Fatalf("precondition Realm()=%q want cn (escape hatch on)", g)
	}
	changed, got := a.BackfillRealm()
	if !changed {
		t.Fatal("backfill changed=false want true (escape hatch free)")
	}
	assertRealmStored(a, "global", t)
	if got != "global" {
		t.Errorf("backfill got=%q want global (escape hatch must not corrupt backfill)", got)
	}
}

// TestBackfillRealmIdempotent уже есть realm файлы с меткой не модифицируются (идемпотентно).
func TestBackfillRealmIdempotent(t *testing.T) {
	t.Parallel()
	a := &Auth{Domain: "www.workbuddy.ai", realm: "cn"} // уже есть cn，domain будет выведено global——никогда не перезаписывать
	changed, got := a.BackfillRealm()
	if changed {
		t.Errorf("backfill changed=true want false (existing realm must win)")
	}
	if got != "cn" {
		t.Errorf("backfill got=%q want cn (existing realm preserved)", got)
	}
	assertRealmStored(a, "cn", t)
}

func assertRealmStored(a *Auth, want string, t *testing.T) {
	t.Helper()
	if a.realm != want {
		t.Errorf("stored realm field=%q want %q", a.realm, want)
	}
}

// TestResolveRealm чистая функция, нормализация явная realm，При отсутствии по domain вывод (с BackfillRealm
// Общий источник; не зависит от аварийного выхода). Явное значение приоритетнее domain Инференс.
func TestResolveRealm(t *testing.T) {
	t.Parallel() // Чистая функция: не трогает глобальные переключатели
	cases := []struct {
		explicit, domain, want string
	}{
		{"global", "www.codebuddy.cn", "global"}, // Явный приоритет:cn domain также записать global
		{"cn", "www.workbuddy.ai", "cn"}, // Явный приоритет:global domain также записать cn
		{"", "www.workbuddy.ai", "global"}, // По умолчанию по domain инференс
		{"", "workbuddy.ai", "global"},
		{"", "codebuddy.cn", "cn"},
		{"", "", "cn"}, // пустой domain → cn（Старый CN возврат учётных данных к нулю)
	}
	for _, c := range cases {
		if got := ResolveRealm(c.explicit, c.domain); got != c.want {
			t.Errorf("ResolveRealm(%q,%q)=%q want %q", c.explicit, c.domain, got, c.want)
		}
	}
}
