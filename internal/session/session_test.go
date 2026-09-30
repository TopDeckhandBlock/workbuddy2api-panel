package session

import (
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/redisstore"
)

// countingStore фейк подсчета вызовов зеркала Store（без сети).
type countingStore struct {
	redisstore.Noop
	mu sync.Mutex
	setBinds int
	delBinds int
	binds map[string]string
}

func newCountingStore() *countingStore {
	return &countingStore{binds: map[string]string{}}
}

func (c *countingStore) SetBind(key, uid string, ttl time.Duration) {
	c.mu.Lock()
	c.setBinds++
	c.binds[key] = uid
	c.mu.Unlock()
}
func (c *countingStore) DelBind(key string) {
	c.mu.Lock()
	c.delBinds++
	delete(c.binds, key)
	c.mu.Unlock()
}
func (c *countingStore) LoadBinds() map[string]string {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := map[string]string{}
	for k, v := range c.binds {
		out[k] = v
	}
	return out
}

func routerWith(store redisstore.Store, avail []string, ttl time.Duration) *Router {
	return New(Config{
		TTL: ttl,
		Store: store,
		Available: func() []string { return avail },
	})
}

func TestWeightedVirtualCandidatesBiasNewSessions(t *testing.T) {
	st := newCountingStore()
	// обычный аккаунт 1 виртуальных инстансов, один аккаунт с истекающим сроком 3 виртуальных инстансов.
	r := routerWith(st, []string{"regular", "expiring", "expiring", "expiring"}, time.Minute)

	// Сначала привязать каждый реальный аккаунт, чтобы последующие новые сессии вошли в фазу хеширования всего пула, а не
	// Получено“приоритет непривязанным аккаунтам”Правила заполняются по очереди. Так можно напрямую проверить вес виртуального инстанса.
	r.Bind("warm-regular", "regular")
	r.Bind("warm-expiring", "expiring")

	counts := map[string]int{}
	const n = 4000
	for i := 0; i < n; i++ {
		key := fmt.Sprintf("session-%d", i)
		uid, ok := r.Resolve(key)
		if !ok {
			t.Fatal("Resolve returned !ok")
		}
		counts[uid]++
	}

	share := float64(counts["expiring"]) / float64(n)
	if share < 0.70 || share > 0.80 {
		t.Fatalf("weighted expiring share=%.3f counts=%v, want 70%%-80%%", share, counts)
	}
}

func TestSameKeySameAccount(t *testing.T) {
	r := routerWith(newCountingStore(), []string{"a1", "a2"}, time.Minute)
	u1, ok1 := r.Resolve("c1")
	u2, ok2 := r.Resolve("c1")
	if !ok1 || !ok2 || u1 != u2 {
		t.Fatalf("same key should map to same account: %s vs %s", u1, u2)
	}
	if r.Count() != 1 {
		t.Errorf("count=%d want 1", r.Count())
	}
}

func TestTTLExpiryReassigns(t *testing.T) {
	st := newCountingStore()
	r := routerWith(st, []string{"a1", "a2"}, 10*time.Millisecond)
	u1, _ := r.Resolve("c1")
	time.Sleep(20 * time.Millisecond)
	u2, ok := r.Resolve("c1")
	if !ok {
		t.Fatal("resolve after expiry should still succeed")
	}
	// После истечения можно перераспределить (возможно совпадение номера, но вернется валидный аккаунт).
	_ = u1
	_ = u2
	if r.Count() != 1 {
		t.Errorf("count=%d want 1 (reassigned, not duplicated)", r.Count())
	}
}

func TestBoundAccountCooldownReassigns(t *testing.T) {
	r := routerWith(newCountingStore(), []string{"a1"}, time.Minute)
	u1, _ := r.Resolve("c1")
	if u1 != "a1" {
		t.Fatalf("initial bind=%s want a1", u1)
	}
	// a1 Охлаждение → В списке доступных осталось только a2 → перераспределение обязательно на a2。
	r.cfg.Available = func() []string { return []string{"a2"} }
	u2, ok := r.Resolve("c1")
	if !ok {
		t.Fatal("resolve should succeed with fallback account")
	}
	if u2 == u1 {
		t.Fatalf("bound account %s cooled but still assigned", u1)
	}
	if u2 != "a2" {
		t.Fatalf("reassigned to %s want a2", u2)
	}
}

func TestNoSessionKeyPassthrough(t *testing.T) {
	// ExtractKey ключи сессии не найдены → Пустая строка (вызывающая сторона по пустой строке идёт по обычному Pick；router не будет вызван).
	got := ExtractKey([]byte(`{"model":"x","messages":[]}`))
	if got != "" {
		t.Errorf("ExtractKey should return empty, got %q", got)
	}
}

func TestExtractKeyPriority(t *testing.T) {
	cases := []struct {
		body string
		want string
	}{
		{`{"metadata":{"conversation_id":"mc","user_id":"mu"},"conversation_id":"top"}`, "mc"}, // metadata.conversation_id Приоритет
		{`{"conversation_id":"top"}`, "top"}, // Верхний уровень conversation_id
		{`{"metadata":{"user_id":"mu"}}`, ""}, // user_id sticky-ключ уже исключен (слишком грубая гранулярность), откат к ротации
		{`{"metadata":{"conversation_id":123}}`, ""}, // Не строка → пустой
		{`not-json`, ""}, // Недопустимый JSON → пустой
		// issue #35：фактически отправлено клиентом camelCase conversationId，ExtractKey Обязательно распознать.
		{`{"conversationId":"abc"}`, "abc"}, // Верхний уровень camelCase
		{`{"metadata":{"conversationId":"abc"}}`, "abc"}, // metadata.camelCase
		{`{"metadata":{"conversation_id":"snake","conversationId":"camel"}}`, "snake"}, // snake имеет приоритет над camel
		{`{"conversation_id":"snake","conversationId":"camel"}`, "snake"}, // Верхний уровень snake имеет приоритет над camel
		{`{"conversationId":123}`, ""}, // Число conversationId → пустой
		{`{"metadata":{"conversationId":456}}`, ""}, // metadata Число conversationId → пустой
		{`{"metadata":{"conversationId":"abc","user_id":"mu"}}`, "abc"}, // camel conversationId имеет приоритет над user_id
	}
	for _, c := range cases {
		if got := ExtractKey([]byte(c.body)); got != c.want {
			t.Errorf("ExtractKey(%s)=%q want %q", c.body, got, c.want)
		}
	}
}

func TestConcurrentSameKeyAssignsOnce(t *testing.T) {
	avail := []string{"a1", "a2", "a3", "a4", "a5"}
	r := routerWith(newCountingStore(), avail, time.Minute)

	const N = 100
	uids := make([]string, N)
	var wg sync.WaitGroup
	for i := 0; i < N; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			u, ok := r.Resolve("same-key")
			if ok {
				uids[idx] = u
			}
		}(i)
	}
	wg.Wait()

	// Все goroutine требуется захватить тот же аккаунт (write-lock re-check защита от повторного распределения).
	first := ""
	for _, u := range uids {
		if u == "" {
			t.Fatal("some goroutine failed to resolve")
		}
		if first == "" {
			first = u
		}
		if u != first {
			t.Fatalf("concurrent resolve assigned different accounts: %s vs %s", first, u)
		}
	}
	if r.Count() != 1 {
		t.Errorf("count=%d want 1 (single binding)", r.Count())
	}
}

// boundUID Прямое чтение привязки uid（Не триггерить Resolve перераспределение), для Bind для ассертов серии тестов (приватный внутри пакета helper）。
func (r *Router) boundUID(key string) (string, bool) {
	r.mu.RLock()
	e, ok := r.entries[key]
	r.mu.RUnlock()
	return e.uid, ok
}

func TestBindOverridesAndMirrors(t *testing.T) {
	// Bind Идемпотентная перезапись старого значения и асинхронное зеркалирование SetBind。
	st := newCountingStore()
	r := routerWith(st, []string{"a1", "a2"}, time.Minute)
	r.Bind("c1", "a1")
	if u, ok := r.boundUID("c1"); !ok || u != "a1" {
		t.Fatalf("bind c1->a1 then bound=%s ok=%v", u, ok)
	}
	// Перекрывает до a2
	r.Bind("c1", "a2")
	if u, _ := r.boundUID("c1"); u != "a2" {
		t.Fatalf("bind override should map c1->a2, got %s", u)
	}
	if r.Count() != 1 {
		t.Errorf("bind override must not duplicate entries, count=%d", r.Count())
	}
	st.mu.Lock()
	n := st.setBinds
	binds := map[string]string{}
	for k, v := range st.binds {
		binds[k] = v
	}
	st.mu.Unlock()
	if n != 2 {
		t.Errorf("SetBind mirror count=%d want 2", n)
	}
	if binds["c1"] != "a2" {
		t.Errorf("mirrored bind should be a2, got %s", binds["c1"])
	}
}

func TestBindIgnoresEmptyKey(t *testing.T) {
	st := newCountingStore()
	r := routerWith(st, []string{"a1"}, time.Minute)
	r.Bind("", "a1")
	r.Bind("c1", "")
	if r.Count() != 0 {
		t.Errorf("Bind with empty key/uid must be no-op, count=%d", r.Count())
	}
	st.mu.Lock()
	n := st.setBinds
	st.mu.Unlock()
	if n != 0 {
		t.Errorf("empty-key Bind must not mirror, setBinds=%d", n)
	}
}

func TestBindThenUnbindLifecycle(t *testing.T) {
	st := newCountingStore()
	r := routerWith(st, []string{"a1"}, time.Minute)
	r.Bind("c1", "a1")
	if !r.Unbind("c1") {
		t.Fatal("Unbind should report found")
	}
	if r.Count() != 0 {
		t.Errorf("count after unbind=%d want 0", r.Count())
	}
	st.mu.Lock()
	del := st.delBinds
	st.mu.Unlock()
	if del != 1 {
		t.Errorf("DelBind mirror count=%d want 1", del)
	}
}

func TestRedisMirrorSetBindCount(t *testing.T) {
	st := newCountingStore()
	r := routerWith(st, []string{"a1", "a2"}, time.Minute)
	r.Resolve("c1")
	r.Resolve("c1") // Быстрый путь touch → еще раз зеркалировать
	if st.setBinds < 1 {
		t.Errorf("SetBind mirror count=%d want >=1", st.setBinds)
	}
	r.Unbind("c1")
	if st.delBinds != 1 {
		t.Errorf("DelBind mirror count=%d want 1", st.delBinds)
	}
}

func TestGCCleansExpired(t *testing.T) {
	st := newCountingStore()
	r := routerWith(st, []string{"a1"}, 10*time.Millisecond)
	r.Resolve("c1")
	r.Resolve("c2")
	time.Sleep(20 * time.Millisecond)
	removed := r.gcOnce(time.Now())
	if removed != 2 {
		t.Errorf("gc removed=%d want 2", removed)
	}
	if r.Count() != 0 {
		t.Errorf("count after gc=%d want 0", r.Count())
	}
}

func TestLoadFromStoreRestores(t *testing.T) {
	st := newCountingStore()
	st.binds["c1"] = "a1"
	st.binds["c2"] = "a2"
	r := routerWith(st, []string{"a1", "a2"}, time.Minute)
	r.LoadFromStore()
	if r.Count() != 2 {
		t.Fatalf("restored count=%d want 2", r.Count())
	}
	u, ok := r.Resolve("c1")
	if !ok || u != "a1" {
		t.Errorf("restored c1 -> %s want a1", u)
	}
}

// TestExtractKeyDerivedFromContent клиент не отправляет сессию id то откат к ключу, производному от контента:
// Несколько раундов одного диалога (добавление истории)→ Ключ стабилен и неизменен; разные диалоги → Ключи различаются.
func TestExtractKeyDerivedFromContent(t *testing.T) {
	// Первый раунд
	turn1 := `{"model":"glm-5.3","messages":[{"role":"system","content":"Ты — ассистент"},{"role":"user","content":"Напиши алгоритм сортировки"}]}`
	// Раунд 2: история дополнена assistant и новый user（system С первой записью user без изменений)
	turn2 := `{"model":"glm-5.3","messages":[{"role":"system","content":"Ты — ассистент"},{"role":"user","content":"Напиши алгоритм сортировки"},{"role":"assistant","content":"ОК"},{"role":"user","content":"Заменить на быструю сортировку"}]}`
	k1, k2 := ExtractKey([]byte(turn1)), ExtractKey([]byte(turn2))
	if k1 == "" {
		t.Fatal("derived key should not be empty when messages present")
	}
	if k1 != k2 {
		t.Errorf("derived key must be stable across turns: turn1=%q turn2=%q", k1, k2)
	}
	if !strings.HasPrefix(k1, "d-") {
		t.Errorf("derived key should carry prefix d-: %q", k1)
	}
	// Разные диалоги (первое сообщение user различается)→ Разные ключи
	other := `{"model":"glm-5.3","messages":[{"role":"system","content":"Ты — ассистент"},{"role":"user","content":"перевести этот текст"}]}`
	if ExtractKey([]byte(other)) == k1 {
		t.Error("different first user message must yield a different derived key")
	}
	// Явно id приоритет над производным ключом
	withID := `{"conversation_id":"my-session","messages":[{"role":"user","content":"Напиши алгоритм сортировки"}]}`
	if got := ExtractKey([]byte(withID)); got != "my-session" {
		t.Errorf("explicit id must win over derived key, got %q", got)
	}
	// отсутствует messages / Чисто нетекстовый контент → пусто (откат к обычной ротации, без залипания)
	if got := ExtractKey([]byte(`{"model":"x"}`)); got != "" {
		t.Errorf("no messages should yield empty key, got %q", got)
	}
	if got := ExtractKey([]byte(`{"messages":[{"role":"user","content":[]}]}`)); got != "" {
		t.Errorf("text-less content should yield empty key, got %q", got)
	}
}

// TestExtractKeyMultimodalContent Мультимодальность content Производится из текстовой части массива.
func TestExtractKeyMultimodalContent(t *testing.T) {
	body := `{"messages":[{"role":"user","content":[{"type":"text","text":"Описание по изображению"},{"type":"image_url","image_url":{"url":"http://x/y.png"}}]}]}`
	k := ExtractKey([]byte(body))
	if k == "" || !strings.HasPrefix(k, "d-") {
		t.Fatalf("multimodal text should derive a key, got %q", k)
	}
	// Тот же текст (разные изображения)→ тот же ключ (изображения не участвуют в деривации, чтобы избежать подписи URL изменение нарушит стикинесс)
	body2 := `{"messages":[{"role":"user","content":[{"type":"text","text":"Описание по изображению"},{"type":"image_url","image_url":{"url":"http://x/z.png"}}]}]}`
	if ExtractKey([]byte(body2)) != k {
		t.Error("image url changes must not break derived key stability")
	}
	// первая запись — только изображение user（отсутствует text part）также должен порождать непустой ключ (фикс слепой зоны липкости сессии первого изображения;
	// изображение только как type-плейсхолдер, повтор того же изображения/замена URL все с одним ключом).
	imgOnly := `{"messages":[{"role":"user","content":[{"type":"image_url","image_url":{"url":"http://x/y.png"}}]}]}`
	k2 := ExtractKey([]byte(imgOnly))
	if k2 == "" || !strings.HasPrefix(k2, "d-") {
		t.Fatalf("pure-image first user should derive a key, got %q", k2)
	}
	imgOnly2 := `{"messages":[{"role":"user","content":[{"type":"image_url","image_url":{"url":"http://x/z.png"}}]}]}`
	if ExtractKey([]byte(imgOnly2)) != k2 {
		t.Error("pure-image url changes must keep same derived key")
	}
}
