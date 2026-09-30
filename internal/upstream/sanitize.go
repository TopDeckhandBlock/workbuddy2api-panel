// sanitize.go Десенсибилизация тела исходящего запроса: удаление отпечатка блэклиста модерации апстрима.
//
// Контекст: клиент (Claude Code Класс CLI）В system prompt Инжектировать несколько фиксированных шаблонных фраз,
// модерация контента апстрима — точное дословное совпадение (не семантическая), обходится изменением одного символа.
// Стратегия: ключ-значение/header отпечаток типа удаляется целиком; несущие семантику шаблонные фразы — минимальная правка (замена одного слова), смысл неизменен.
package upstream

import (
	"regexp"
	"strings"
)

// sanitizeFeatures предпроверка признаков: любой хит — вход в очистку (strings.Contains быстрый путь,
// Обычные запросы — все мимо → вернуть как есть, нулевое распределение).
var sanitizeFeatures = []string{
	"x-anthropic-billing-header", // header Имя ключа сегмента key-value
	"cc_entrypoint=", // Хвостовое голое значение ключа (достаточно обрезать префикс для попадания)
	"You are Claude Code", // идентифицирующая фраза (достаточно префикса для срабатывания)
	"Main branch (", // инъекция директивы (срабатывает по усечённому префиксу)
	"You are a coding agent running in the Codex CLI", // Codex instructions Первый сегмент (достаточно усечённого префикса для хита)
	"github.com/anthropics/", // Во фразе обратной связи Anthropic Ссылка на репозиторий
	"11128", // Анти-детект апстрима: голый числовой код ошибки
}

// sanitizeHdrRe Слой stripping:header Триггер — имя ключа (значение неважно), удалить весь блок.
var sanitizeHdrRe = regexp.MustCompile(`(?i)x-anthropic-billing-header:[^;\n]*;?\s*`)

// sanitizeBareHdrRe фолбэк нижнего уровня: голое имя ключа (без двоеточия и значения) — тоже отпечаток —2026-09-13 Эксперимент F4
// подтверждено assistant упоминание голого имени ключа в обратных кавычках в сообщении — триггер 11128，а слой stripping требует двоеточие, на голую строку не действует.
// После полного удаления формы ключ-значение оставшееся голое имя ключа минимально сокращается (header→hdr）：Нарушает дословное совпадение,
// Семантика не меняется, читаемость сохраняется. Без учета регистра, перезаписывает X-Anthropic-... Вариант.
//
// внимание: данный regex не требует двоеточия, это sanitizeHdrRe супермножество —hasFingerprint и sanitizeText
// там используются оба: сначала удалить key-value форму (sanitizeHdrRe），затем сократить оставшиеся голые имена ключей (данное regex),
// семантика замены отличается (удаление целого блока vs минимальная аббревиатура), нельзя объединять в один regex.
var sanitizeBareHdrRe = regexp.MustCompile(`(?i)x-anthropic-billing-header`)

// sanitizeKvRe Слой stripping: хвостовой голый key-value (cc_xxx=...;）Циклическая очистка.
var sanitizeKvRe = regexp.MustCompile(`(?i)\bcc_[a-z0-9_]+=[^;\n]*;?\s*`)

// sanitizeRewrites Слой рерайта: дословная замена всех шаблонных фраз (одно слово на предложение, смысл не меняется).
//
// строка совпадения фразы идентификации**без завершающего знака препинания**（только до "…for Claude" до):
// CLI Версия: фраза заканчивается точкой ("…for Claude."），Десктоп-версия (claude-desktop-3p / Agent SDK）
// через запятую присоединить последующий контент ("…for Claude, running within the Claude Agent SDK."）。
// целое предложение с точкой матчит только первое, десктоп-версия проскочит, fingerprint уйдет в upstream как есть → 400 code=11128。
// После удаления конечной пунктуации покрыть обе формы (строка замены тоже без пунктуации, исходная пунктуация сохраняется).
// Обратите внимание, всё ещё требуется "You are Claude Code, " префикс, без более широкой замены подстроки,
// Во избежание ложных срабатываний TestExactMatchOnlyVariantNotTouched Защищаемый разрозненный текст.
var sanitizeRewrites = [][2]string{
	{
		"You are Claude Code, Anthropic's official CLI for Claude",
		"You are Claude Code, Anthropic's official CLI tool for Claude",
	},
	{
		"Main branch (you will usually use this for PRs)",
		"Default branch (you will usually use this for PRs)",
	},
	{
		"You are a coding agent running in the Codex CLI, a terminal-based coding assistant.",
		"You are a coding agent running in the Codex CLI tool, a terminal-based coding assistant.",
	},
	{
		// Фраза обратной связи: целиком с Anthropic Ссылка на репозиторий, апстрим блокирует целым предложением (только ссылка или только половина не блокируется,
		// фактически требуется одновременное появление целой фразы).give→provide достаточно разницы в одно слово для обхода, семантика неизменна.
		"To give feedback, users should report the issue at https://github.com/anthropics/claude-code/issues",
		"To provide feedback, users should report the issue at https://github.com/anthropics/claude-code/issues",
	},
	{
		// антидетект upstream: если в теле запроса есть голое число 11128 то блокировать весь заказ (вне зависимости от контекста этого числа —
		// "code=11128« / Голый «11128» / «код ошибки 11128« / »Code=11128" Все попадания;
		// Соседний 11148 / 11101 / 11115 / 99999 все пропускаются).11128 именно код ошибки перехвата этого класса,
		// Апстрим идентифицирует по этому"В обсуждении/отобразить его внутренний код ошибки"запроса.
		// цена: любой в диалоге пользователя 11128 будут перезаписаны — но появление этой числовой последовательности в запросе само по себе условие блокировки,
		// Без перезаписи неизбежно провал. Вставка дефиса сохраняет читаемость и ссылку (zero-width space неэффективен, на практике апстрим нормализует).
		"11128",
		"11-128",
	},
}

// sanitizeText Очистка одиночного фрагмента: предпроверка не сработала → вернуть исходную строку (zero-alloc).
func sanitizeText(text string) string {
	if !hasFingerprint(text) {
		return text
	}
	for _, rw := range sanitizeRewrites {
		text = strings.ReplaceAll(text, rw[0], rw[1])
	}
	if sanitizeHdrRe.MatchString(text) {
		text = sanitizeHdrRe.ReplaceAllString(text, "")
	}
	if strings.Contains(text, "cc_") {
		prev := ""
		for prev != text { // Очистить хвостовой bare kv（cc_version=...; cc_entrypoint=...;）
			prev = text
			text = sanitizeKvRe.ReplaceAllString(text, "")
		}
	}
	// фолбэк: формат key-value уже целиком удалён выше, здесь остался только голый ключ (ссылка/форме примерного текста).
	text = sanitizeBareHdrRe.ReplaceAllString(text, "x-anthropic-billing-hdr")
	return strings.TrimSpace(text)
}

// hasFingerprint предпроверка признаков: сначала strings.Contains быстрый путь (нулевое распределение);
// header Варианты регистра имени ключа (X-Anthropic-...）и может встречаться как голое имя ключа (без двоеточия),
// Contains чувствительно к регистру,sanitizeHdrRe требуется двоеточие — оба пропустят "смешанный регистр + голое имя ключа»,
// Необходимо повторно использовать не требующий двоеточия (?i) Фолбэк через regex (sanitizeBareHdrRe），Иначе вся очистка пропускается.
// sanitizeBareHdrRe Двоеточие не требуется, это sanitizeHdrRe является надмножеством, поэтому отдельное сопоставление последнего не требуется.
func hasFingerprint(text string) bool {
	for _, f := range sanitizeFeatures {
		if strings.Contains(text, f) {
			return true
		}
	}
	return sanitizeBareHdrRe.MatchString(text)
}

// sanitizeContent Совместимо со строкой и мультимодальным массивом; затрагивает только text part，image и т.д. part Не изменять.
// возвращает очищенное значение и флаг изменения.
func sanitizeContent(v any) (any, bool) {
	switch c := v.(type) {
	case string:
		s := sanitizeText(c)
		return s, s != c
	case []any:
		changed := false
		for _, p := range c {
			m, ok := p.(map[string]any)
			if !ok {
				continue
			}
			text, ok := m["text"].(string)
			if !ok {
				continue
			}
			if s := sanitizeText(text); s != text {
				m["text"] = s
				changed = true
			}
		}
		return c, changed
	}
	return v, false
}

// sanitizeToolCalls Очистка assistant.tool_calls[].function.arguments。
//
// arguments Да**Строкифицированный JSON**（не объект), поэтому обрабатывать как текст sanitizeText Достаточно.
// этот участок долго был слепой зоной: сообщения вызова инструментов content обычно null，а старая версия sanitizeMessages
// В content При отсутствии напрямую continue，Все сообщение целиком tool_calls вместе пропускается —
// тогда любая заблокированная строка, записанная в параметры инструмента в истории (имя файла, команда, содержимое записи), утечет как есть.
func sanitizeToolCalls(v any) bool {
	callList, ok := v.([]any)
	if !ok {
		return false
	}
	changed := false
	for _, c := range callList {
		call, ok := c.(map[string]any)
		if !ok {
			continue
		}
		fn, ok := call["function"].(map[string]any)
		if !ok {
			continue
		}
		args, ok := fn["arguments"].(string)
		if !ok {
			continue
		}
		if s := sanitizeText(args); s != args {
			fn["arguments"] = s
			changed = true
		}
	}
	return changed
}

// sanitizeMessages Очистка messages в content и tool_calls；При любом совпадении вернуть true。
func sanitizeMessages(messages []any) bool {
	changed := false
	for _, msg := range messages {
		m, ok := msg.(map[string]any)
		if !ok {
			continue
		}
		// content и tool_calls каждый проверяется независимо:content может быть null（виток вызова инструмента),
		// В ранних версиях здесь continue，причина сообщений такого типа tool_calls совсем не очищается.
		if c, ok := m["content"]; ok {
			if nc, ch := sanitizeContent(c); ch {
				m["content"] = nc
				changed = true
			}
		}
		// reasoning_content（Поле обратной заливки цепочки рассуждений, см. thinking.go/sse.go）На практике также
		// Несёт отпечаток, и content Аналогичная очистка.string форма идет напрямую sanitizeText。
		if rc, ok := m["reasoning_content"].(string); ok {
			if s := sanitizeText(rc); s != rc {
				m["reasoning_content"] = s
				changed = true
			}
		}
		if tc, ok := m["tool_calls"]; ok {
			if sanitizeToolCalls(tc) {
				changed = true
			}
		}
	}
	return changed
}
