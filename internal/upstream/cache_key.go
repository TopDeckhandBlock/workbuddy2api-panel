// cache_key.go Инжект в upstream prompt_cache_key Поле (P0 оптимизация затрат).
//
// реверс-тест (buddy-adapter.ts:706-714）：Поддержка апстрим-сервера prompt_cache_key，
// тот же сегмент 8k token префикс:
// - без → prompt_cache_hit_tokens=0, credit≈0.34
// - Лента → prompt_cache_hit_tokens=7808, credit≈0.02（Снижение стоимости ~17×）
//
// Шлюз здесь для каждого исходящего chat В запрос инжектируется стабильный, изолированный по аккаунту cache key，
// Позволяет последовательным запросам одного клиента к одному аккаунту переиспользовать префиксный кэш апстрима.
package upstream

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strings"
)

// InjectPromptCacheKey в уже переписанном исходящем body инжект на prompt_cache_key Поле.
//
// Приоритет:
// 1. body Уже содержит prompt_cache_key → исходное значение сохраняется (клиент сам знает, какой ключ переиспользовать)
// 2. body Уже содержит conversation_id / conversationId → использовать как источник хеша сессии
// 3. ничего нет → использовать inbound conversationID параметр (из X-Conversation-ID парсинг заголовка)
//
// Ограничение безопасности — изоляция по аккаунту:
// - Формат генерации ключа `wb2a-<uid8>-<convHex>`
// - uid8 это аккаунт UID Перед 8 символов, между аккаунтами никогда не совпадает → ключи кэша между аккаунтами никогда не коллизируют
// - Кросс-аккаунтное переиспользование одного cache key приведёт к попаданию апстрима в префикс-кэш чужого аккаунта и утечке чужого диалога, поэтому uid является фактором жёсткой изоляции
//
// входной параметр uid для аккаунта UID（если пусто, использовать "-"，но ключ все равно будет инжектирован; вызывающая сторона должна передавать реальный UID）。
// входной параметр conversationID идентификатор сессии, распарсенный шлюзом (body внутри нет conversation_id использовать его как источник хеша).
// Когда оба источника пусты convHex — фиксированное значение (префикс не переиспользуется в каждой новой сессии, но сегмент изоляции аккаунта сохраняется).
// body При невозможности парсинга вернуть как есть (с prepareBody семантикасовпадение: плохой body без повторной классификации как ошибки).
func InjectPromptCacheKey(body []byte, uid, conversationID string) []byte {
	if len(body) == 0 {
		return body
	}
	var obj map[string]any
	if err := json.Unmarshal(body, &obj); err != nil {
		return body
	}
	// Приоритет 1：клиент уже явно передал key → Никогда не перезаписывать.
	if existing, ok := obj["prompt_cache_key"].(string); ok && existing != "" {
		return body
	}
	// Приоритет 2：body внутри conversation_id / conversationId использовать как источник хеша сессии.
	conv := conversationID
	if v := strField(obj, "conversation_id"); v != "" {
		conv = v
	} else if v := strField(obj, "conversationId"); v != "" {
		conv = v
	}
	key := buildCacheKey(uid, conv)
	obj["prompt_cache_key"] = key
	out, err := json.Marshal(obj)
	if err != nil {
		return body
	}
	return out
}

// buildCacheKey Генерация `wb2a-<uid8>-<convHex>` стабильность формата cache key。
//
// uid8 Предоставляет изолированный сегмент аккаунта;convHex = sha256(uid + conversationID)[:16] hex Предоставить сегмент сессии
// （один аккаунт — одна сессия стабильно, разные сессии — по-разному). При пустом источнике сессии convHex по-прежнему uid отдельное хеширование,
// гарантировать отсутствие коллизий между аккаунтами, но не переиспользовать одну пустую сессию (пустая сессия = семантика новой сессии).
func buildCacheKey(uid, conversation string) string {
	uid8 := uid
	if len(uid8) > 8 {
		uid8 = uid8[:8]
	}
	if uid8 == "" {
		uid8 = "-"
	}
	sum := sha256.Sum256([]byte(uid + "|" + conversation))
	convHex := hex.EncodeToString(sum[:16])
	return "wb2a-" + uid8 + "-" + convHex
}

// strField Из map получить string поле, не string или вернуть пустую строку ""。
func strField(obj map[string]any, key string) string {
	v, ok := obj[key].(string)
	if !ok {
		return ""
	}
	return strings.TrimSpace(v)
}
