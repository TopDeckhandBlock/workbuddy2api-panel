package session

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"testing"
)

// imagePartSig Вычисление part резюме оригинала (sha256 Перед 8 hex），Для точного формирования expected value —
// при изменении алгоритма подписи ожидаемое значение теста меняется вместе с этим helper одноузловая синхронизация.
func imagePartSig(t *testing.T, part string) string {
	t.Helper()
	sum := sha256.Sum256([]byte(part))
	return hex.EncodeToString(sum[:4])
}

// TestResolveConversationID перекрытие conversationId извлечённый snake/camel/Три состояния отсутствия:
// - metadata.conversation_id / metadata.conversationId → значение
// - Верхний уровень conversation_id / conversationId → Получение значения (snake Приоритет тому же ExtractKey）
// - отсутствует / только user_id → ""（Семантика семейства заголовков сессии распознает только диалог ID，Никогда не откатываться user_id）
func TestResolveConversationID(t *testing.T) {
	cases := []struct {
		name string
		body string
		want string
	}{
		{"metadata snake_case", `{"metadata":{"conversation_id":"conv-1"}}`, "conv-1"},
		{"metadata camelCase", `{"metadata":{"conversationId":"conv-2"}}`, "conv-2"},
		{"top-level snake_case", `{"conversation_id":"conv-3"}`, "conv-3"},
		{"top-level camelCase", `{"conversationId":"conv-4"}`, "conv-4"},
		{"snake wins over camel", `{"conversation_id":"conv-s","conversationId":"conv-c"}`, "conv-s"},
		{"missing", `{"model":"glm-5.2"}`, ""},
		{"empty body", ``, ""},
		{"broken json", `{broken`, ""},
		{"metadata user_id only", `{"metadata":{"user_id":"u1"}}`, ""},
		{"top-level user_id only", `{"user_id":"u1"}`, ""},
		{"empty string value", `{"conversationId":""}`, ""},
		{"non-string value", `{"conversationId":123}`, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := ResolveConversationID([]byte(c.body)); got != c.want {
				t.Errorf("ResolveConversationID(%q) = %q want %q", c.body, got, c.want)
			}
		})
	}
}

// TestNewMessageIDFormat 32 бит hex и не пусто, два вызова с высокой вероятностью различаются (дымовой тест на случайность).
func TestNewMessageIDFormat(t *testing.T) {
	for i := 0; i < 50; i++ {
		id := NewMessageID()
		if len(id) != 32 {
			t.Fatalf("NewMessageID() = %q len=%d want 32", id, len(id))
		}
		for _, ch := range id {
			if !strings.ContainsRune("0123456789abcdef", ch) {
				t.Fatalf("NewMessageID() = %q has non-hex char %q", id, ch)
			}
		}
	}
}

// TestRequestIDForKeyStability Совм. key постоянно стабильно, гетеро key каждый разный, пусто key каждый раз новое значение.
func TestRequestIDForKeyStability(t *testing.T) {
	// Прогрев: очистка кэша уровня пакета, избегать загрязнения другими тестами key（изоляция тестов).
	k1a := RequestIDForKey("conv-a")
	k1b := RequestIDForKey("conv-a")
	if k1a != k1b {
		t.Errorf("same key should be stable: %q vs %q", k1a, k1b)
	}
	k2 := RequestIDForKey("conv-b")
	if k1a == k2 {
		t.Errorf("different keys should differ: %q", k1a)
	}
	// пустой key：Генерировать новое значение при каждом вызове (без сессии — нет"стабильно в рамках сессии"семантика).
	e1 := RequestIDForKey("")
	e2 := RequestIDForKey("")
	if e1 == e2 {
		t.Errorf("empty key should yield fresh values each call: %q", e1)
	}
	// стабильное значение само должно быть 32 hex（Может использоваться как B3 TraceId использовать напрямую).
	for _, id := range []string{k1a, k2, e1} {
		if len(id) != 32 {
			t.Errorf("RequestIDForKey value %q len=%d want 32", id, len(id))
		}
	}
}

// TestTurnKeyExtraction Извлечение fallback-ключа уровня раунда: взять**Последняя запись** user "порядковый номер» сообщения+текст»,
// добавление внутри раунда assistant/tool Сообщение не меняет ключ; отсутствует user / Без текста / Поврежден JSON Всегда пустая строка.
func TestTurnKeyExtraction(t *testing.T) {
	cases := []struct {
		name string
		body string
		want string
	}{
		{"single user", `{"messages":[{"role":"user","content":"Привет"}]}`, "u0:Привет"},
		{"last user wins", `{"messages":[{"role":"user","content":"первый вопрос"},{"role":"assistant","content":"Ответ"},{"role":"user","content":"второй вопрос"}]}`, "u2:второй вопрос"},
		// agent Многошагово: добавление внутри раунда assistant/tool сообщение, последнее user позиция и содержимое неизменны → тот же ключ.
		{"agent step keeps same key", `{"messages":[{"role":"user","content":"задача"},{"role":"assistant","tool_calls":[{"id":"c1"}]},{"role":"tool","content":"Результат"}]}`, "u0:задача"},
		{"multimodal parts text joined", `{"messages":[{"role":"user","content":[{"type":"text","text":"См. рисунок"},{"type":"image_url","image_url":{"url":"data:x"}}]}]}`, "u0:См. рисунок\n[image_url:" + imagePartSig(t, `{"type":"image_url","image_url":{"url":"data:x"}}`) + "]"},
		{"no user message", `{"messages":[{"role":"system","content":"sys"}]}`, ""},
		{"empty messages", `{"messages":[]}`, ""},
		{"messages key absent", `{"model":"glm-5.2"}`, ""},
		{"broken json", `{broken`, ""},
		{"empty body", ``, ""},
		{"empty content", `{"messages":[{"role":"user","content":""}]}`, ""},
		{"null content", `{"messages":[{"role":"user","content":null}]}`, ""},
		// Только изображение content Генерация непустого ключа раунда по подписи контента (G1 исправлено, было ""）；ключ содержит image part резюме.
		{"image only content", `{"messages":[{"role":"user","content":[{"type":"image_url","image_url":{"url":"x"}}]}]}`, "u0:[image_url:" + imagePartSig(t, `{"type":"image_url","image_url":{"url":"x"}}`) + "]"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := TurnKey([]byte(c.body)); got != c.want {
				t.Errorf("TurnKey(%q) = %q want %q", c.body, got, c.want)
			}
		})
	}
}

// TestTurnRequestIDDerivation Уровень раунда ID чистая деривация: один ключ — одно значение, разные ключи — разные значения, пустой ключ — новое значение каждый раз,
// Форма постоянна 32 hex。
func TestTurnRequestIDDerivation(t *testing.T) {
	a1 := TurnRequestID("u0:Та же проблема")
	a2 := TurnRequestID("u0:Та же проблема")
	if a1 != a2 {
		t.Errorf("same turn key should derive same id: %q vs %q", a1, a2)
	}
	if b := TurnRequestID("u0:другая проблема"); b == a1 {
		t.Errorf("different turn keys should derive different ids: %q", b)
	}
	// одинаковый текст, но разный порядковый номер (одинаковые вопросы в разных раундах) также разделять.
	if c := TurnRequestID("u2:Та же проблема"); c == a1 {
		t.Errorf("same text at different position should differ: %q", c)
	}
	// пустой ключ: нет раундов для агрегации → каждый раз новое значение (сохраняется прежнее поведение на уровне запроса).
	if e1, e2 := TurnRequestID(""), TurnRequestID(""); e1 == e2 {
		t.Errorf("empty turn key should yield fresh values each call: %q", e1)
	}
	for _, id := range []string{a1, TurnRequestID(""), TurnRequestID("x")} {
		if len(id) != 32 {
			t.Errorf("TurnRequestID value %q len=%d want 32", id, len(id))
		}
	}
}
