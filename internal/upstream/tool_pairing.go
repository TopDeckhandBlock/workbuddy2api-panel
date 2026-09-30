// tool_pairing.go осиротевшее тело исходящего запроса tool_call↔tool парная очистка + tool Перестановка блоков результата
// （Вобрать референсный репозиторий sse.ts:91-123 resolveToolPairing семантика, адаптация под шлюз OpenAI wire форма сообщения).
//
// контекст:OpenAI для совместимости протокол требует tool_calls assistant сообщение, каждое из которых tool_call id
// Должен быть соответствующий один role:tool сообщение результата; иначе role:tool Сообщение также требует соответствующий префикс
// tool_call。при отсутствии любой из сторон апстрим всё равно HTTP 400 отклонить весь запрос.
//
// При ошибке выполнения инструмента (невалидные параметры, таймаут, инструмент не найден...) клиент assistant tool_calls
// Сохраняется в историю сессии, но сообщение результата не записывается. Эта битая история затем воспроизводится каждым запросом как есть — апстрим для последующих
// На каждое сообщение пользователя возвращать 400，вся сессия бракуется. Шлюз — последний рубеж: перед отправкой отбросить несопоставимые
// Элемент позволяет сессии самовосстановиться, лучше потерять контекст инструментов одного раунда, чем убить всю сессию.
package upstream

// repackToolResultBlocks Вставить в assistant.tool_calls с ним tool не между результатами tool Сообщение
// Перенести после всей группы, гарантируя одну партию tool_call результат в wire непрерывно на.
//
// контекст:Codex image_resize_notice фича превратит <image_resize_notice> Как одна запись
// developer/system сообщение вставляется в tool после вывода. При параллельных вызовах вставляется между двумя tool В середине результата:
//
//	assistant tool_calls=[c00 c01]
//	tool c00
//	developer <image_resize_notice> <- вставить между
//	tool c01
//
// OpenAI Совместимость с требованиями протокола tool результат сразу после assistant，Любое сообщение в середине считается разрывом пары, решает апстрим
// 11148（tool_call_sequence_broken）и заблокировать всю сессию. Здесь только меняется порядок, содержимое без изменений:
//
//	assistant tool_calls=[c00 c01] | tool c00 | X | tool c01
//	→ assistant tool_calls=[c00 c01] | tool c00 | tool c01 | X
//
// Порядок результатов сохраняется (в одной партии tool_call исходный относительный порядок = порядок результатов), без новой чувствительности к порядку
// проблема. При отсутствии вставляемых сообщений — ноль изменений, ноль аллокаций (возврат исходного slice）。
func repackToolResultBlocks(messages []any) ([]any, bool) {
	if len(messages) < 3 {
		return messages, false
	}
	out := make([]any, 0, len(messages))
	changed := false
	i := 0
	for i < len(messages) {
		m, ok := messages[i].(map[string]any)
		if !ok || m["role"] != "assistant" {
			out = append(out, messages[i])
			i++
			continue
		}
		tcs, hasCalls := m["tool_calls"].([]any)
		if !hasCalls || len(tcs) == 0 {
			out = append(out, messages[i])
			i++
			continue
		}
		want := map[string]bool{}
		for _, tci := range tcs {
			if tc, ok := tci.(map[string]any); ok {
				if id, _ := tc["id"].(string); id != "" {
					want[id] = true
				}
			}
		}
		// Сбор идущей сразу следом (допускается прерывание другими сообщениями) партии tool Результат в исходном относительном порядке.
		out = append(out, messages[i])
		i++
		var results []any
		var between []any
		sawNonTool := false
		for i < len(messages) {
			mm, ok := messages[i].(map[string]any)
			if !ok {
				break
			}
			role, _ := mm["role"].(string)
			if role == "tool" {
				id, _ := mm["tool_call_id"].(string)
				if !want[id] {
					break
				}
				results = append(results, messages[i])
				if sawNonTool {
					changed = true
				}
				i++
				continue
			}
			if len(results) == 0 {
				break // assistant после нет результата: передать cleanupOrphanToolCalls Обработка
			}
			// Следующая группа assistant.tool_calls — новый заголовок группы, нельзя поглощать как вставку: как только принят
			// between，Он больше никогда не обрабатывается внешним циклом как голова группы, и его партия результатов никогда не будет
			// до пересортировки (реальная сессия msg[181] именно так и было пропущено). Необходимо break Вернуть внешнему циклу.
			if role == "assistant" {
				if next, _ := mm["tool_calls"].([]any); len(next) > 0 {
					break
				}
			}
			// Пока результаты батча не собраны полностью, промежуточные сообщения считаются вставками, временно сохраняются для последующего сдвига.
			between = append(between, messages[i])
			sawNonTool = true
			i++
		}
		out = append(out, results...)
		out = append(out, between...)
	}
	if !changed {
		return messages, false
	}
	return out, true
}

// cleanupOrphanToolCalls отбросить несопоставимое tool_call и tool Результат (все модели, независимо от
// deepseek-only sanitize переключатель). Семантика выровнена, см. репозиторий resolveToolPairing：
//
// - сбор по всем линиям role:tool сообщения tool_call_id（результирующий набор) и assistant.tool_calls[].id（набор вызовов);
// - батч assistant.tool_calls Нажать keepCalls Симметричная обрезка: оставлять только вызовы с парным результатом (частичное сохранение
// не оставит безрезультатных tool_call），Удалять целиком только если после фильтрации пусто tool_calls Ключ;
// - role:tool только в соответствующем tool_call сохраняется только если сохранено, иначе удалить всё сообщение;
// - Нет трафика инструментов → Исходный slice вернуть как есть,changed=false（нулевое распределение — нулевые изменения).
//
// это safety-net "пропустить запрос»: пока есть валидная пара, весь сегмент этих полей сохраняется, корректная пара никогда не отбрасывается.
// Вернуть очищенный slice（Без изменений равно исходному slice，не полагаться на то, было ли новое назначение) и произошло ли удаление.
func cleanupOrphanToolCalls(messages []any) ([]any, bool) {
	if len(messages) == 0 {
		return messages, false
	}
	callIDs := map[string]bool{}
	resultIDs := map[string]bool{}
	hasTraffic := false
	for _, m := range messages {
		msg, ok := m.(map[string]any)
		if !ok {
			continue
		}
		switch msg["role"] {
		case "tool":
			if id, ok := msg["tool_call_id"].(string); ok && id != "" {
				resultIDs[id] = true
				hasTraffic = true
			}
		case "assistant":
			if tcs, ok := msg["tool_calls"].([]any); ok {
				for _, tci := range tcs {
					tc, ok := tci.(map[string]any)
					if !ok {
						continue
					}
					if id, ok := tc["id"].(string); ok && id != "" {
						callIDs[id] = true
						hasTraffic = true
					}
				}
			}
		}
	}
	if !hasTraffic {
		return messages, false
	}
	// keepCalls：Вызов id полнота с обеих сторон (вызов существует и результат существует). Повтор id и неупорядоченные обрабатываются как множество.
	keepCalls := map[string]bool{}
	for id := range callIDs {
		if resultIDs[id] {
			keepCalls[id] = true
		}
	}
	changed := false
	// 1) assistant.tool_calls：Нажать keepCalls Симметричная обрезка — оставлять только вызовы с результатом, если после фильтрации пусто — удалить ключ.
	//
	// Историческая реализация — "каждый в батче id сохранять батчем только если все в наличии, иначе удалить весь tool_calls ключ». Это оставит
	// Нет основного результата: батч [c1,c2] Вернул только c1 при этом на стороне вызывающего пакетно удаляется, а tool{c1} По-прежнему по id Попадание
	// keepCalls сохранено —— исходящая нагрузка тогда становится "нет tool_calls assistant + Осиротевший tool」，
	// Апстрим считает 11148（tool calls and tool results do not match）и полностью блокирует сессию.
	// сейчас обе стороны шабрят один и тот же keepCalls Нажать id Симметричная обрезка (с 2) сторона удаления в той же трактовке),
	// любой ввод больше не даст неполной пары.
	for _, m := range messages {
		msg, ok := m.(map[string]any)
		if !ok {
			continue
		}
		if role, _ := msg["role"].(string); role != "assistant" {
			continue
		}
		tcs, ok := msg["tool_calls"].([]any)
		if !ok || len(tcs) == 0 {
			continue
		}
		keptCalls := make([]any, 0, len(tcs))
		for _, tci := range tcs {
			tc, ok := tci.(map[string]any)
			if !ok {
				continue
			}
			if id, _ := tc["id"].(string); keepCalls[id] {
				keptCalls = append(keptCalls, tc)
			}
		}
		if len(keptCalls) == len(tcs) {
			continue // вся партия на месте: ноль изменений
		}
		changed = true
		if len(keptCalls) == 0 {
			delete(msg, "tool_calls")
			continue
		}
		msg["tool_calls"] = keptCalls
	}
	// 2) role:tool Результат: только соответствующий tool_call сохранять только если сохранён родитель; orphan-результаты удалять целиком.
	kept := make([]any, 0, len(messages))
	for _, m := range messages {
		msg, ok := m.(map[string]any)
		if !ok {
			kept = append(kept, m)
			continue
		}
		if role, _ := msg["role"].(string); role == "tool" {
			id, _ := msg["tool_call_id"].(string)
			if !keepCalls[id] {
				changed = true
				continue
			}
		}
		kept = append(kept, m)
	}
	if !changed {
		return messages, false
	}
	return kept, true
}
