package upstream

import (
	"encoding/json"
	"strings"
	"testing"
)

// extractCacheKey Из переписанного body извлечь из prompt_cache_key значение поля.
func extractCacheKey(t *testing.T, body []byte) string {
	t.Helper()
	var obj map[string]any
	if err := json.Unmarshal(body, &obj); err != nil {
		t.Fatalf("unmarshal body: %v", err)
	}
	v, _ := obj["prompt_cache_key"].(string)
	return v
}

// TestInjectPromptCacheKey_PreservesExisting покрыть кейсы задач 1：
// inbound body Уже содержит prompt_cache_key → Исходное значение сохраняется, шлюзом не перезаписывается.
func TestInjectPromptCacheKey_PreservesExisting(t *testing.T) {
	// Arrange
	uid := "user-abc-123"
	conv := "conv-xyz"
	in := `{"model":"glm-5.2","messages":[],"prompt_cache_key":"client-set-key"}`

	// Act
	out := InjectPromptCacheKey([]byte(in), uid, conv)

	// Assert
	if got := extractCacheKey(t, out); got != "client-set-key" {
		t.Fatalf("existing prompt_cache_key overwritten: got=%q want client-set-key", got)
	}
}

// TestInjectPromptCacheKey_UsesConversationID покрыть кейсы задач 2：
// body отсутствует prompt_cache_key Но есть conversation_id → использовать его как источник хеша сессии.
// формат wb2a-<uid8>-<conversationHash>：conversation_id появляется не дословно, а управляет хеш-сегментом
// （Совм. id Совм. key、Разное id Разное key）。при этом всё ещё содержит uid8 префикс изоляции.
func TestInjectPromptCacheKey_UsesConversationID(t *testing.T) {
	uid := "user-abc-123"
	in := `{"model":"glm-5.2","messages":[],"conversation_id":"conv-42"}`

	out := InjectPromptCacheKey([]byte(in), uid, "")
	got := extractCacheKey(t, out)
	if !strings.HasPrefix(got, "wb2a-") {
		t.Fatalf("expected wb2a- prefix, got=%q", got)
	}
	if !strings.Contains(got, "user-abc") {
		t.Fatalf("expected uid8 isolation prefix, got=%q", got)
	}
	// Совм. conversation_id Два исходящих → Совм. key（стабильно).
	out2 := InjectPromptCacheKey([]byte(in), uid, "")
	if got2 := extractCacheKey(t, out2); got2 != got {
		t.Fatalf("non-deterministic key for same conversation_id: %q vs %q", got, got2)
	}
	// Разное conversation_id → Разное key（различение по хеш-сегменту).
	inB := `{"model":"glm-5.2","messages":[],"conversation_id":"conv-99"}`
	gotB := extractCacheKey(t, InjectPromptCacheKey([]byte(inB), uid, ""))
	if gotB == got {
		t.Fatalf("different conversation_id produced same key: %q", got)
	}
}

// TestInjectPromptCacheKey_GeneratesStableKey покрыть кейсы задач 3：
// body Оба отсутствуют → шлюз генерирует стабильный ключ, два одинаковых входа на одном аккаунте дают один и тот же key。
func TestInjectPromptCacheKey_GeneratesStableKey(t *testing.T) {
	uid := "user-abc-123"
	conv := "conv-stable"
	in := `{"model":"glm-5.2","messages":[]}`

	a := InjectPromptCacheKey([]byte(in), uid, conv)
	b := InjectPromptCacheKey([]byte(in), uid, conv)
	ka, kb := extractCacheKey(t, a), extractCacheKey(t, b)
	if ka != kb {
		t.Fatalf("non-deterministic key for same input+uid: a=%q b=%q", ka, kb)
	}
}

// TestInjectPromptCacheKey_AccountIsolation покрыть кейсы задач 4：
// Разные аккаунты → key различаются (изолированная проверка).
func TestInjectPromptCacheKey_AccountIsolation(t *testing.T) {
	conv := "conv-shared"
	in := `{"model":"glm-5.2","messages":[]}`

	ka := extractCacheKey(t, InjectPromptCacheKey([]byte(in), "user-aaa-111", conv))
	kb := extractCacheKey(t, InjectPromptCacheKey([]byte(in), "user-bbb-222", conv))
	if ka == kb {
		t.Fatalf("cross-account key collision: both=%q", ka)
	}
	if !strings.HasPrefix(ka, "wb2a-") || !strings.HasPrefix(kb, "wb2a-") {
		t.Fatalf("keys must carry wb2a- prefix: a=%q b=%q", ka, kb)
	}
}

// TestInjectPromptCacheKey_Format покрыть кейсы задач 5：
// cache key Валидация формата (вкл. uid8 префикс).
func TestInjectPromptCacheKey_Format(t *testing.T) {
	uid := "1234567890abcdef"
	in := `{"model":"glm-5.2","messages":[]}`
	out := InjectPromptCacheKey([]byte(in), uid, "conv-fmt")
	got := extractCacheKey(t, out)
	if !strings.HasPrefix(got, "wb2a-") {
		t.Fatalf("expected wb2a- prefix, got=%q", got)
	}
	if !strings.Contains(got, "12345678") {
		t.Fatalf("expected uid8 (12345678) in key, got=%q", got)
	}
}

// TestInjectPromptCacheKey_EmptyConversation покрыть кейсы задач 3 Граница:
// отсутствует conversation_id и без идентификатора сессии → Сгенерированный ключ содержит uid8 Но conversationHash Сегмент — константа (без переиспользования префикса).
// не должно быть ошибки, не должно быть пустой строки (key непусто).
func TestInjectPromptCacheKey_EmptyConversation(t *testing.T) {
	uid := "user-abc-123"
	in := `{"model":"glm-5.2","messages":[]}`
	out := InjectPromptCacheKey([]byte(in), uid, "")
	got := extractCacheKey(t, out)
	if got == "" {
		t.Fatalf("expected non-empty key when no conversation, got empty")
	}
	if !strings.HasPrefix(got, "wb2a-") {
		t.Fatalf("expected wb2a- prefix, got=%q", got)
	}
	if !strings.Contains(got, "user-abc") {
		t.Fatalf("expected uid8 in key, got=%q", got)
	}
}

// TestPrepareBodyOptNoCacheKeyInjection покрыть кейсы задач 6：
// PrepareBodyOpt（sanitize=false старого входа) поведение не меняется — без инъекции cache key。
// обратная совместимость: передавать только body Не выдавать cacheKey В контексте нельзя вводить новые поля.
func TestPrepareBodyOptNoCacheKeyInjection(t *testing.T) {
	in := `{"model":"glm-5.2","messages":[]}`
	out := PrepareBodyOpt([]byte(in), false)
	var obj map[string]any
	if err := json.Unmarshal(out, &obj); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if _, present := obj["prompt_cache_key"]; present {
		t.Fatalf("PrepareBodyOpt must NOT inject prompt_cache_key, but got: %v", obj["prompt_cache_key"])
	}
}
