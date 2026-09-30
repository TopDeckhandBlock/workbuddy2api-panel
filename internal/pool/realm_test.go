package pool

import (
	"reflect"
	"testing"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
)

// realmPool Создать содержащий cn/global пул аккаунтов и обеспечить globalEnabled переключатель включен (по умолчанию).
func realmPool(t *testing.T) *Pool {
	t.Helper()
	withNoPickGap(t)
	auth.SetGlobalEnabled(true)
	t.Cleanup(func() { auth.SetGlobalEnabled(true) })
	p := New("")
	// CN аккаунт (явно cn realm или пусто realm+cn domain оба допустимы)→ Realm()=="cn"
	p.Add(&auth.Auth{UID: "cn1", Domain: "www.codebuddy.cn"})
	p.Add(&auth.Auth{UID: "cn2", Domain: ""}) // пустой domain → cn
	// global Аккаунт → Realm()=="global"
	p.Add(&auth.Auth{UID: "g1", Domain: "www.workbuddy.ai"})
	p.Add(&auth.Auth{UID: "g2", Domain: "workbuddy.ai"})
	return p
}

// realmOf прямое чтение аккаунта realm（Эквивалентно e.a.Realm()）。
func realmOf(a *auth.Auth) string { return a.Realm() }

func TestAvailableUIDsForRealm(t *testing.T) {
	p := realmPool(t)

	// все healthy → cn коллекция содержит только cn1/cn2，global коллекция содержит только g1/g2。
	got := p.AvailableUIDsForRealm("cn")
	want := []string{"cn1", "cn2"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("AvailableUIDsForRealm(cn)=%v want %v", got, want)
	}
	got = p.AvailableUIDsForRealm("global")
	want = []string{"g1", "g2"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("AvailableUIDsForRealm(global)=%v want %v", got, want)
	}

	// realm=="" Деградация к текущему состоянию (эквивалент AvailableUIDs：все healthy）。
	got = p.AvailableUIDsForRealm("")
	want = []string{"cn1", "cn2", "g1", "g2"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("AvailableUIDsForRealm()=%v want %v", got, want)
	}
}

func TestAvailableUIDsForModelRealm(t *testing.T) {
	p := realmPool(t)
	// 6004 Кулдаун модели только на cn-1 Вверх → AvailableUIDsForModelRealm("glm-5.2«,«cn") исключить его,
	// Но cn коллекция как минимум гарантирует cn-2；global Набор не зависит от cn Влияние кулдауна.
	p.CooldownSoftForModel("cn1", 10*time.Minute, time.Now().Add(30*time.Minute), "glm-5.2", "model 6004")

	got := p.AvailableUIDsForModelRealm("glm-5.2", "cn")
	if len(got) != 1 || got[0] != "cn2" {
		t.Errorf("AvailableUIDsForModelRealm(glm-5.2,cn)=%v want [cn2]", got)
	}
	got = p.AvailableUIDsForModelRealm("glm-5.2", "global")
	want := []string{"g1", "g2"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("AvailableUIDsForModelRealm(glm-5.2,global)=%v want %v", got, want)
	}
	// realm=="" → Эквивалентно AvailableUIDsForModel（Исключения для моделей как обычно:cn1 всё равно исключается кулдауном этой модели).
	got = p.AvailableUIDsForModelRealm("glm-5.2", "")
	want = []string{"cn2", "g1", "g2"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("AvailableUIDsForModelRealm(glm-5.2)=%v want %v", got, want)
	}
}

func TestWeightedAvailableUIDsForModelRealm(t *testing.T) {
	p := realmPool(t)
	now := time.Now()
	p.SetCreditsDetailed("cn2", 100, 100, 100, now.Add(time.Hour), 100)

	got := p.WeightedAvailableUIDsForModelRealm("glm-5.2", "cn")
	want := []string{"cn1", "cn2", "cn2", "cn2"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("weighted cn candidates=%v want %v", got, want)
	}

	// Аккаунты с полной загрузкой in-flight не попадают в кандидаты, вес скоро-истекающих не должен превышать текущий лимит concurrency.
	p.SetMaxInFlight(1)
	if !p.Acquire("cn2") {
		t.Fatal("failed to acquire cn2")
	}
	got = p.WeightedAvailableUIDsForModelRealm("glm-5.2", "cn")
	want = []string{"cn1"}
	p.Release("cn2")
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("in-flight-full weighted candidates=%v want %v", got, want)
	}

	// После отключения главного переключателя появляется только один раз на аккаунт, старое поведение полностью восстановлено.
	p.SetPreferExpiring(false)
	got = p.WeightedAvailableUIDsForModelRealm("glm-5.2", "cn")
	want = []string{"cn1", "cn2"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("disabled weighted cn candidates=%v want %v", got, want)
	}
}

func TestPickExcludingForRealm(t *testing.T) {
	p := realmPool(t)
	// realm=cn → Только из cn выбор из множества.
	seen := map[string]bool{}
	for i := 0; i < 50; i++ {
		a := p.PickExcludingForRealm(nil, "", "cn")
		if a == nil {
			t.Fatal("PickExcludingForRealm(cn) returned nil")
		}
		seen[a.UID] = true
		if realmOf(a) != "cn" {
			t.Fatalf("PickExcludingForRealm(cn) returned global account %s", a.UID)
		}
	}
	// global также только из global Коллекция.
	for i := 0; i < 50; i++ {
		a := p.PickExcludingForRealm(nil, "", "global")
		if a == nil {
			t.Fatal("PickExcludingForRealm(global) returned nil")
		}
		if realmOf(a) != "global" {
			t.Fatalf("PickExcludingForRealm(global) returned cn account %s", a.UID)
		}
	}
	// realm=="" → Текущая деградация: все healthy доступны для выбора.
	for i := 0; i < 50; i++ {
		a := p.PickExcludingForRealm(nil, "", "")
		if a == nil {
			t.Fatal("PickExcludingForRealm() returned nil")
		}
	}
}

func TestPickExcludingForRealmTried(t *testing.T) {
	p := realmPool(t)
	// tried исключить из realm после фильтрации (сохранение семантики ротации на уровне запроса).
	a := p.PickExcludingForRealm(map[string]bool{"cn1": true}, "", "cn")
	if a == nil {
		t.Fatal("tried cn1 → nil")
	}
	if a.UID == "cn1" {
		t.Fatalf("tried cn1 still picked: %s", a.UID)
	}
	if realmOf(a) != "cn" {
		t.Fatalf("picked non-cn %s", a.UID)
	}
}
