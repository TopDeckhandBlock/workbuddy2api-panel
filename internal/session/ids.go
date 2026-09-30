// ids.go семейство заголовков сессии ID парсинг и генерация (issue #35 агрегация в фоне).
//
// Официальный CodeBuddy CLI Семейство исходящих заголовков (X-Conversation-ID / X-Conversation-Request-ID /
// X-Request-ID / X-B3-*），Бэкенд по X-Conversation-Request-ID（диалоговых раундов) агрегировать запросы;
// данный файл предоставляет conversationId Извлечение, уровень сообщения 32 hex messageID、а также"Тот же ключ сессии
// стабильное переиспользование" conversationRequestID ленивый кэш, для handler Генерация вне цикла ротации, внутри цикла
// reuse (смена номера/повтор/деградация всех однотипных ID → бэкенд больше не фрагментирован).
package session

import (
	cryptorand "crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math/rand/v2"
	"strings"
)

// ResolveConversationID извлечение семейства заголовков сессии из тела запроса conversationId（snake/camel Две формы,
// Переиспользовать ExtractKey порядок распознавания:metadata Приоритет,snake имеет приоритет над camel）。
// и ExtractKey различия:**Принимается только conversationId，Никогда не откатываться user_id**——X-Conversation-ID
// Семантика:"Диалог ID"，user_id Фолбэк загрязнит критерий агрегации по диалогам на бэкенде.
// При отсутствии вернуть ""（Без подделки: приоритет — проброс исходного значения клиента, если клиент не передал — не отправлять).
func ResolveConversationID(body []byte) string {
	if len(body) == 0 {
		return ""
	}
	var obj map[string]any
	if err := json.Unmarshal(body, &obj); err != nil {
		return ""
	}
	if meta, ok := obj["metadata"].(map[string]any); ok {
		if v := strOrEmpty(meta["conversation_id"]); v != "" {
			return v
		}
		if v := strOrEmpty(meta["conversationId"]); v != "" {
			return v
		}
	}
	if v := strOrEmpty(obj["conversation_id"]); v != "" {
		return v
	}
	return strOrEmpty(obj["conversationId"])
}

// NewMessageID генерация на уровне сообщения ID：32 бит hex（UUID v4 форма длины без дефисов), выровнять с официальным
// X-Request-ID / X-Conversation-Message-ID。crypto/rand при сбое (теоретически невозможно) — откат
// math/rand/v2 Двойной uint64 Сборка 32 hex——Конст. 32 hex、всегда валиден, можно безопасно использовать как B3 TraceId。
func NewMessageID() string {
	b := make([]byte, 16)
	if _, err := cryptorand.Read(b); err == nil {
		return hex.EncodeToString(b)
	}
	// Крайний фолбэк при сбое источника энтропии: всё равно гарантируем 32 hex（fallbackID Не panic、непустая строка).
	return fmt.Sprintf("%016x%016x", uint64(rand.Uint64())|1, rand.Uint64())
}

// deriveSalt случайная соль при старте процесса: для всех стабильных агрегаций ID чистый дериват с единой солью, чтобы дериват нельзя
// По содержимому внешне управляемого ключа (ключ сессии/ключ уровня раунда) предвычислен; обновление при перезапуске (старый раунд диалога при перезапуске/Сессия уже
// завершение, разрыв не образуется). Сессионный (RequestIDForKey）и уровень раунда (TurnRequestID）Используют одну соль.
var deriveSalt = NewMessageID()

// RequestIDForKey стабильный ключ сессии возврата conversationRequestID：sha256(Соль|Клавиша) Перед 16 байт
// hex，Чисто производный (без кэша, без TTL、Память не растет с числом ключей — ранее sync.Map Ленивый кэш вместе с
// количество ключей сессии растёт без ограничений, чисто производные естественно ограничены).
// - Совм. key：внутри процесса детерминированно одно значение (один раз user send/агрегация нескольких раундов одной сессии);
// - Аномальный key：Каждый независим, различны между собой;
// - пустой key：Генерация нового значения каждый раз (без сессии — нет"стабильно в рамках сессии"Семантика — вызывающая сторона должна на уровне запроса
// захват с переиспользованием,handler один забор вне цикла ротации — естественный шаринг).
//
// Возвращаемое значение всегда 32 hex（NewMessageID форма), можно использовать напрямую как B3 TraceId（16/32 hex валидно).
func RequestIDForKey(key string) string {
	if key == "" {
		return NewMessageID()
	}
	sum := sha256.Sum256([]byte(deriveSalt + "|" + key))
	return hex.EncodeToString(sum[:16])
}

// TurnKey Производный ключ агрегации уровня "раунд диалога»:body внутри**Последняя запись** role=="user" сообщения
// 「порядковый номер + текст».
//
// зачем нужно: клиент без ключа сессии (OpenAI Протокол совместимости —dsh / Codex / Cherry Studio
// В теле запроса и т.п. отсутствует как conversationId также отсутствует metadata ключ) приведёт к ExtractKey всегда возвращает пустую строку,
// агрегационный ключ семейства заголовков сессии может генерироваться только заново на каждый запрос,agent мульти-тур в детализации расхода апстрима всё равно один запрос
// одна запись. Эта функция дает таким клиентам**Не зависит от клиента**ключ уровня раунда: в пределах одной отправки пользователя
// Все вызовы апстрима (tool call Многократный / Повтор со сменой номера / повтор с даунгрейдом)body последняя запись в user Сообщение
// Константа → тот же ключ; пользователь отправляет следующее сообщение → Смена ключа.
//
// Почему не брать первую запись user Сообщение: первое неизменно в рамках сессии, все раунды сессии объединяются в
// один и тот же ключ агрегации (смешивание между раундами диалога). Только взятие последней записи соответствует официальному X-Conversation-Request-ID
// семантика "раунда диалога». Порядковый номер включён в ключ: одинаковые вопросы в разных раундах ("продолжить"）Не будет объединено в
// один раунд.
//
// отсутствует body / отсутствует messages / отсутствует user Сообщение / Сообщение без текста → ""（Фолбэк вызывающей стороны на рандом уровня запроса
// ID，не подделывать ключ агрегации).
func TurnKey(body []byte) string {
	if len(body) == 0 {
		return ""
	}
	var obj struct {
		Messages []struct {
			Role string `json:"role"`
			Content json.RawMessage `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(body, &obj); err != nil {
		return ""
	}
	for i := len(obj.Messages) - 1; i >= 0; i-- {
		if obj.Messages[i].Role != "user" {
			continue
		}
		sig := contentSignature(obj.Messages[i].Content)
		if sig == "" {
			// Последняя запись user Сообщение без подписываемого контента (пусто/null/пустой parts）→ В этом раунде ключ агрегации не создаётся.
			// не искать дальше назад: позиция этого сообщения фиксирована в пределах раунда, поиск назад заставит ключ следовать за step дрейф.
			return ""
		}
		return fmt.Sprintf("u%d:%s", i, sig)
	}
	return ""
}

// contentText получение сообщений content текст: строковый — вернуть как есть; массив (мультимодальный parts）
// склейка part text поле. Без текста (только изображение / null / неизвестная форма) вернуть ""。
func contentText(raw json.RawMessage) string {
	s := strings.TrimSpace(string(raw))
	if s == "" || s == "null" {
		return ""
	}
	switch s[0] {
	case '"':
		var str string
		if err := json.Unmarshal(raw, &str); err != nil {
			return ""
		}
		return str
	case '[':
		var parts []struct {
			Text string `json:"text"`
		}
		if err := json.Unmarshal(raw, &parts); err != nil {
			return ""
		}
		var b strings.Builder
		for _, p := range parts {
			b.WriteString(p.Text)
		}
		return b.String()
	}
	return ""
}

// contentSignature получение сообщений content детерминированная подпись (G1 Исправление — чисто имиджевая карусель больше не фрагментируется):
// - string Форма: возврат текста, с contentText Результат**Полностью совпадает**——ключ-значение пути в plain text
// неизменно, round-robin ключ существующих сессий/нулевой дрейф sticky-ключа (контракт обратной совместимости);
// - форма массива (мультимодальный parts）：текст part（type для "« Или »text"，и contentText
// единый формат конкатенации) бесшовная склейка как в оригинале; нетекстовый part Добавить "[type:сводка]"——саммари брать из
// part исходных байтов sha256 Перед 8 hex。data: base64 встроенное изображение может быть слишком длинным, оригинал в ключе приведёт к
// Усиливает нагрузку производного хеша (см. TurnKey примечание в заголовке), только краткая сводка;type + Резюме оригинала естественно различает
// Разное содержимое/нетекстовое количество part множество, доп. счётчик не нужен.
//
// Нет подписываемого контента (пусто / null / Пустой массив / plain text part все — пустые строки) вернуть ""（не подделывать —
// вызывающая сторона откатывается к прежней семантике пустого ключа).TurnKey и firstUserText（sticky-фолбэк) используют эту функцию,
// слепые зоны изображений обоих каналов исправлены вместе.
func contentSignature(raw json.RawMessage) string {
	s := strings.TrimSpace(string(raw))
	if s == "" || s == "null" {
		return ""
	}
	// Строковая форма: подпись пути plain-text == contentText Результат (нулевой дрейф ключ-значение).
	if s[0] == '"' {
		var str string
		if err := json.Unmarshal(raw, &str); err != nil {
			return ""
		}
		return str
	}
	if s[0] != '[' {
		return ""
	}
	var parts []json.RawMessage
	if err := json.Unmarshal(raw, &parts); err != nil {
		return ""
	}
	var b strings.Builder
	hasNonText := false
	for _, pr := range parts {
		var p struct {
			Type string `json:"type"`
			Text string `json:"text"`
		}
		if err := json.Unmarshal(pr, &p); err != nil {
			return ""
		}
		if p.Type == "" || p.Type == "text" {
			// текст part：Бесшовная склейка (с contentText полностью единая метрика — весь текст part массив
			// Подпись == contentText результат, нулевой дрейф существующих ключей).
			b.WriteString(p.Text)
			continue
		}
		// Нетекстовый part：type + Исходный текст sha256 Перед 8 hex（Сверхдлинный data: URL Записывается только краткая сводка).
		hasNonText = true
		sum := sha256.Sum256(pr)
		fmt.Fprintf(&b, "\n[%s:%s]\n", p.Type, hex.EncodeToString(sum[:4]))
	}
	out := b.String()
	if !hasNonText {
		return out
	}
	return strings.TrimSpace(out)
}

// TurnRequestID Возвращает агрегацию по ключу уровня раунда ID：sha256(Соль|Клавиша) Перед 16 байтовый hex（32 бит,
// и NewMessageID Та же форма, можно напрямую использовать как B3 TraceId）。
//
// Чисто производное, без кэша, без TTL、Память не растет с числом запросов в процессе —— Это отличается от уровня сессии
// RequestIDForKey Наоборот: количество ключей сессии ограничено (тот же источник, что и sticky-сессии) — может постоянно храниться в кэше, а ключи уровня раунда
// Каждый раунд диалога — новая запись, кэш должен быть ограничен, производный естественно ограничен.
// пустой ключ возвращает новое случайное значение (при отсутствии раунда для агрегации сохраняется прежнее поведение "каждый запрос независимо»).
func TurnRequestID(turnKey string) string {
	if turnKey == "" {
		return NewMessageID()
	}
	sum := sha256.Sum256([]byte(deriveSalt + "|" + turnKey))
	return hex.EncodeToString(sum[:16])
}
