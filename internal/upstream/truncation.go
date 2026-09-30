// truncation.go Детекция неполных параметров вызова инструмента (заимствовано из реф. репозитория sse.ts:158-167
// isTruncatedArguments семантика).
//
// контекст:SSE Поток прерван (обрыв соединения / finish_reason==length）при этом вызов инструмента arguments
// останется лишь половина JSON。в этот момент если шлюз передаст грязные параметры клиенту как есть, парсинг клиента выдаст ошибку невалидности JSON и зависание сессии.
// обработка в референс-репозитории — отбросить неполный вызов и запустить ретрай (репорт max-tokens），а не дополнять до {} Подделка легитимного вида.
//
// ключевое различие: только "непустое но непарсируемое" считается усечением. пустая строка — валидный безаргументный tool; парсится но неверный тип
// （скаляр / массив) — ошибка вывода модели, отдать клиенту schema Достаточно проверки ответа, здесь не оценивается.
package upstream

import (
	"encoding/json"
	"strings"
)

// isTruncatedArguments Определить, усечена ли строка параметров инструмента из-за потери фрагмента (отличать от "у инструмента изначально нет параметров»).
// - Пустая строка / Только пробелы → false（валидный безаргументный инструмент);
// - Непусто, но JSON Ошибка парсинга → true（усечение);
// - Поддаётся парсингу (вкл. null/скаляр/массив и любой валидный JSON）→ false。
func isTruncatedArguments(raw string) bool {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return false
	}
	var v any
	return json.Unmarshal([]byte(trimmed), &v) != nil
}

// dropTruncatedToolCalls отфильтровать arguments Полный tool_call（Вернуть новое slice）。
// только на основании isTruncatedArguments проверка, без изменения сохранённых вызовов (ноль изменений для позитивных кейсов).
func dropTruncatedToolCalls(calls []map[string]any) []map[string]any {
	kept := make([]map[string]any, 0, len(calls))
	for _, call := range calls {
		fn, _ := call["function"].(map[string]any)
		if fn == nil {
			kept = append(kept, call)
			continue
		}
		args, _ := fn["arguments"].(string)
		if isTruncatedArguments(args) {
			continue
		}
		kept = append(kept, call)
	}
	return kept
}
