// sse.go обработка апстрима SSE поток: агрегация в один OpenAI ответ, или проксировать клиенту.
package upstream

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"time"
)

// errEmptyStream Ответ апстрима 200 но нет действительного SSE фрейм данных (пустой поток/Только комментарий/[DONE]）。
// Заменить голый на sentinel-ошибку fmt.Errorf：StreamHint вызывающая сторона (handler стриминговый путь) требуется различать
// 「«Пустой поток апстрима» и "ошибка записи при разрыве клиента» — пустой поток — дефект апстрима, следует логировать 502 Наблюдение; ошибка записи —
// клиент уже ушел, метрика логов отличается.Aggregate и StreamHint Общий sentinel (errors.Is проверка).
var errEmptyStream = errors.New("upstream stream contained no valid data events")

// IsEmptyStreamError Сообщить, является ли ошибка "пустым потоком апстрима» (нет валидного SSE кадр) — для handler
// В стриминговом пути пустой поток считать неуспешным наблюдением (HTTP Заголовки уже отправлены, можно только 200，но лог/состояние должно сойтись к
// upstream_parse та же семантика), отличать от ошибок разрыва соединения с клиентом.
func IsEmptyStreamError(err error) bool { return errors.Is(err, errEmptyStream) }

// Aggregate Полное чтение SSE поток, агрегация delta.content для одиночного OpenAI chat.completion ответ.
// Шард/Полстроки от bufio.Reader.ReadString обработка; при встрече "data: [DONE]" Завершено.
// tool_calls в потоковом режиме delta Достижение (по index Объединение: первый фрагмент с id/type/name，далее передавать только arguments фрагмент).
func Aggregate(r io.Reader) (map[string]any, error) {
	br := bufio.NewReaderSize(r, 64*1024)
	var (
		id, model string
		created float64
		content strings.Builder
		reasoning strings.Builder
		role = "assistant"
		finishReason = "stop"
		usage map[string]any
		gotAnyContent bool
		validEvents int
		sawDone bool // Апстрим явно отправлял data: [DONE]（нормальное завершение)
		toolCalls = map[int]map[string]any{}
		toolOrder []int
		// toolSeq нехватка index tool_call источник порядковых номеров распределения: слот "последнее распределение» сохраняется между кадрами,
		// Инкремент в пределах одного кадра (см. mergeToolCallsChunk комментарий).
		toolSeq int
		// idIndex id → уже распределенный index：Сохраняется между фреймами (продолжение фрагмента id Часто встречается в последующих кадрах),
		// дефицит предложения index При ... по id возврат существующих вызовов (см. mergeToolCallsChunk комментарий).
		idIndex = map[string]int{}
	)
	// appendContent это "тело уже получено» (gotAnyContent latch）единственная точка записи:delta и
	// message Два пути content Всё должно вливаться через это, спецификация единственная (issue #142）——
	// S1 Пустая строка не считается "текст получен», не занимает latch Квота:OpenAI Стиль role-only первый кадр
	// （delta.content=""）И вся строка пуста message Кадр — штатный кадр, если установлен флаг пустой строки latch，
	// Последующий основной текст будет message ветки отката !gotAnyContent Гард тихо отклоняет;
	// S2 Пустая строка сама не содержит байт для добавления, пропустить WriteString согласуется с семантикой append.
	appendContent := func(txt string) {
		if txt == "" {
			return
		}
		content.WriteString(txt)
		gotAnyContent = true
	}
	// mergeToolCallsChunk фрагмент tool_calls Массив по index Объединить в сводную таблицу.
	// delta（потоковые фрагменты, по index накопление) и message（не delta целиком) использует общую логику мержа,
	// Гарантировать "идентичность от апстрима/имя функции не теряется,arguments семантика склейки консистентна».
	//
	// index Совместимость при отсутствии:OpenAI Требования стандарта delta кадра tool_call Лента index（метка принадлежности фрагмента),
	// Но часть апстримов его опускает. Ранее отсутствовал index Всё относить к 0——В сценариях с множественными вызовами различается call Объединено в один
	// index слот,arguments последовательно,name взаимное перекрытие (загрязнение данных). Фикс по "id Приоритет,lastIdx фолбэк»:
	// - Лента index → Нажать index Накопление (совместимый вид, без изменений);
	// - нехватка index Лента id И id уже встречалось → продолжить этот id Расположение index；
	// - нехватка index Лента id И id Является новым → Открыть новый sequence (множественные вызовы не объединяются);
	// - нехватка index отсутствует id → Добавить к последнему полученному фрагменту index（У одиночного вызова нет продолженных фрагментов id
	// — штатная форма), без истории — завести новый номер.
	// Лента index фрагменты как обычно по index накапливается, не затрагивается.
	// nextToolIndex Выделить следующий неконфликтующий слот index Порядковый номер: с toolSeq инкрементально пропускать существующие с
	// index（Комплаенс-потока index Да 0..N-1，нехватка index замещение не должно их перекрывать).
	nextToolIndex := func() int {
		for {
			idx := toolSeq
			toolSeq++
			if _, used := toolCalls[idx]; !used {
				return idx
			}
		}
	}
	mergeToolCallsChunk := func(tcs []any) {
		for _, tc := range tcs {
			call, ok := tc.(map[string]any)
			if !ok {
				continue
			}
			idx := -1
			if v, ok := call["index"].(float64); ok {
				idx = int(v)
			} else if cid, _ := call["id"].(string); cid != "" {
				if mid, seen := idIndex[cid]; seen {
					idx = mid // данный id Уже возвращено: продолжать существующий вызов (действует между кадрами)
				} else {
					idx = nextToolIndex()
				}
			} else if len(toolOrder) > 0 {
				idx = toolOrder[len(toolOrder)-1] // отсутствует id Фрагмент: продолжение последнего вызова
			} else {
				idx = nextToolIndex()
			}
			merged, seen := toolCalls[idx]
			if !seen {
				merged = map[string]any{"index": idx}
				toolCalls[idx] = merged
				toolOrder = append(toolOrder, idx)
			}
			if cid, _ := call["id"].(string); cid != "" {
				idIndex[cid] = idx
			}
			if cid, _ := merged["id"].(string); cid != "" {
				idIndex[cid] = idx
			}
			mergeToolCallDelta(merged, call)
		}
	}
	// mergeMessageFields Преобразовать не delta полный message контент объединяется в агрегацию (message выдается целиком,
	// непотоковая конкатенация,content брать только один раз).role/reasoning_content/tool_calls и delta Ветка
	// Изоморфный проброс;content аналогично установлено gotAnyContent，и delta пути latch семантическая консистентность
	// （Один фрейм целиком message после, далее delta кадр не добавляется повторно).
	mergeMessageFields := func(msg map[string]any) {
		if r2, ok := msg["role"].(string); ok && r2 != "" {
			role = r2
		}
		if txt, ok := msg["content"].(string); ok {
			appendContent(txt)
		}
		if rc, ok := msg["reasoning_content"].(string); ok {
			reasoning.WriteString(rc)
		}
		if tcs, ok := msg["tool_calls"].([]any); ok {
			mergeToolCallsChunk(tcs)
		}
	}
	for {
		line, err := br.ReadString('\n')
		if err != nil && err != io.EOF {
			return nil, err
		}
		line = strings.TrimRight(line, "\r\n")
		if strings.HasPrefix(line, "data: ") {
			payload := strings.TrimPrefix(line, "data: ")
			if payload == "[DONE]" {
				// явное завершение апстрима: остановить чтение,DONE Любые данные после — игнорируются.
				sawDone = true
				break
			} else {
				var chunk map[string]any
				if json.Unmarshal([]byte(payload), &chunk) == nil {
					// Счетчик валидных событий: только JSON Учитываются успешно распарсенные фреймы данных (при ошибке парсинга — тихо continue）。
					validEvents++
					if v, ok := chunk["id"].(string); ok && id == "" {
						id = v
					}
					if v, ok := chunk["model"].(string); ok && model == "" {
						model = v
					}
					if v, ok := chunk["created"].(float64); ok && created == 0 {
						created = v
					}
					if u, ok := chunk["usage"].(map[string]any); ok {
						usage = u
					}
					if ch, ok := chunk["choices"].([]any); ok {
						for _, ci := range ch {
							c, _ := ci.(map[string]any)
							if c == nil {
								continue
							}
							if fr, ok := c["finish_reason"].(string); ok && fr != "" {
								finishReason = fr
							}
							if delta, ok := c["delta"].(map[string]any); ok {
								if r2, ok := delta["role"].(string); ok && r2 != "" {
									role = r2
								}
								if txt, ok := delta["content"].(string); ok {
									appendContent(txt)
								}
								if rc, ok := delta["reasoning_content"].(string); ok {
									reasoning.WriteString(rc)
								}
								if tcs, ok := delta["tool_calls"].([]any); ok {
									mergeToolCallsChunk(tcs)
								}
							}
							// некоторые апстримы кладут полное сообщение в message в (не delta）：
							// Влить целиком (role/content/reasoning_content/tool_calls），
							// и delta Ветки изоморфны.delta Тело уже получено (gotAnyContent）то пропустить
							// （Избежать конфликта с delta Дублирование конкатенации пути —PR #134 latch семантика).
							if msg, ok := c["message"].(map[string]any); ok && !gotAnyContent {
								mergeMessageFields(msg)
							}
						}
					}
				}
			}
		}
		if err == io.EOF {
			break
		}
	}
	if validEvents == 0 {
		// Ответ апстрима 200 но без валидных событий данных (пустой поток/только [DONE]/только строки комментариев):
		// больше не синтезировать пустой content фиктивный success-ответ, сразу ошибка, обрабатывает handler маппится в 502 upstream_parse。
		return nil, errEmptyStream
	}
	if id == "" {
		id = fmt.Sprintf("chatcmpl-%d", time.Now().UnixNano())
	}
	if created == 0 {
		created = float64(time.Now().Unix())
	}
	message := map[string]any{
		"role": role,
		"content": content.String(),
	}
	if reasoning.Len() > 0 {
		message["reasoning_content"] = reasoning.String()
	}
	if len(toolOrder) > 0 {
		sort.Ints(toolOrder)
		calls := make([]map[string]any, 0, len(toolOrder))
		for _, idx := range toolOrder {
			calls = append(calls, toolCalls[idx])
		}
		// P1b：При обрыве потока tool_call arguments является неполным JSON（ошибка парсинга), грязные параметры не прокидывать
		// отдаётся клиенту — остаточные фрагменты будут распарсены клиентом как невалидные JSON зависшая сессия. Два источника усечения:
		// - finish_reason=="length"（модель из-за max_tokens досрочное прерывание);
		// - обрыв соединения с апстримом (EOF завершение, но не отправлено data: [DONE]，sawDone=false）。
		// Полные параметры сохраняются как есть (позитивный кейс — без изменений); пустые параметры (инструмент без параметров) — не усечение, также сохраняются.
		// Ранее распознавалось только finish_reason=="length"，EOF усеченный tool_calls Неполные параметры отправляются как есть.
		if finishReason == "length" || !sawDone {
			calls = dropTruncatedToolCalls(calls)
		}
		if len(calls) > 0 {
			message["tool_calls"] = calls
		}
	}
	resp := map[string]any{
		"id": id,
		"object": "chat.completion",
		"created": int64(created),
		"model": model,
		"choices": []any{
			map[string]any{
				"index": 0,
				"message": message,
				"finish_reason": finishReason,
			},
		},
	}
	if usage != nil {
		// OpenAI нестриминговый usage обязательно содержит total_tokens。Если апстрим шлет только prompt_tokens +
		// completion_tokens（У части upstream отсутствует последний фрейм total），шлюз синтезирует и дополняет — иначе строго по
		// schema Клиент с валидацией не получает total_tokens。уже есть total или если одного из двух нет — не дополнять
		// （Не выдумывать: при значении только с одной стороны нельзя синтезировать достоверный total）。
		resp["usage"] = normalizeUsageCacheAliases(ensureUsageTotal(usage))
	}
	return resp, nil
}

// ensureUsageTotal В usage нехватка total_tokens Но prompt_tokens/completion_tokens когда оба присутствуют
// Дополнить total = prompt + completion（через новый map Слияние без изменения исходного upstream map）。
// любое отсутствие или уже существует total возвращается как есть.
func ensureUsageTotal(u map[string]any) map[string]any {
	if _, ok := u["total_tokens"]; ok {
		return u
	}
	pt, pok := num64(u["prompt_tokens"])
	ct, cok := num64(u["completion_tokens"])
	if !pok || !cok {
		return u
	}
	out := make(map[string]any, len(u)+1)
	for k, v := range u {
		out[k] = v
	}
	out["total_tokens"] = pt + ct
	return out
}

// num64 взять JSON number（float64/int64 все варианты) нормализуются в float64；нечисловой возврат ok=false。
//
// Внимание: с payload.go типа switch Разная семантика: там — поле алиаса запроса перевода (нечисловое отклоняется
// весь запрос), здесь — суммирование агрегированного ответа (нечисла пропускаются при синтезе). Защита от слияния двух мест в будущем.
func num64(v any) (float64, bool) {
	switch n := v.(type) {
	case float64:
		return n, true
	case int64:
		return float64(n), true
	case int:
		return float64(n), true
	default:
		return 0, false
	}
}

// mergeToolCallDelta Стриминговый tool_call Слияние фрагментов в накопительный объект:
// id/type/function.name Прямое перекрытие (последующие шарды обычно по умолчанию),function.arguments конкатенация.
func mergeToolCallDelta(merged, delta map[string]any) {
	if v, ok := delta["id"].(string); ok && v != "" {
		merged["id"] = v
	}
	if v, ok := delta["type"].(string); ok && v != "" {
		merged["type"] = v
	}
	df, _ := delta["function"].(map[string]any)
	if df == nil {
		return
	}
	mf, _ := merged["function"].(map[string]any)
	if mf == nil {
		mf = map[string]any{}
		merged["function"] = mf
	}
	if v, ok := df["name"].(string); ok && v != "" {
		mf["name"] = v
	}
	if v, ok := df["arguments"].(string); ok && v != "" {
		if prev, _ := mf["arguments"].(string); prev != "" {
			mf["arguments"] = prev + v
		} else {
			mf["arguments"] = v
		}
	}
}

// stripToolCallNames сведение потока tool_calls name семантика — "каждый index появляется только один раз»:
// первый фрагмент сохраняется function.name，Тот же index В последующих фрагментах name ключи всегда удаляются (независимо от того, апстрим
// пустая строка или повторяющаяся непустая строка). Это OpenAI реальная форма официального потока — первый фрейм с name，Последующие фреймы несут только
// arguments фрагмент, больше не появляется name ключа — поэтому общее предковое поведение аддитивных и перезаписывающих клиентов.
//
// обе модели потребления корректны в этой форме:
// - Накопительный тип (официальный WorkBuddy/CodeBuddy `name += tc_function?.name || ""`）：
// Последующие шарды name Ключ отсутствует → Добавить пустую строку, накопительно name сохранять уникальность, больше не склеивать в Bash×Количество кадров (issue #82）。
// - Перезаписывающий тип (hawklithm#2 / Grok Build `name ?? state.name` Или `if (name) state.name = name`）：
// Последующие шарды name Ключ отсутствует → сохранить уже построенный первый кадр name，не сбрасывается случайно пустой строкой.
// отсутствие ключа безопаснее пустой строки:`??` и truthy гард при отсутствующем ключе сохраняет старое значение,
// а для пустой строки,`??` будет ошибочно принято за сброс и очистит имя инструмента.
//
// seen запись каждого index Отправлен ли уже первый фрагмент (с name независимо от непустоты); удаление идемпотентно.
// трогать только function.name ключ,id/type/arguments прозрачная передача как есть.
func stripToolCallNames(obj map[string]any, seen map[int]bool) {
	choices, _ := obj["choices"].([]any)
	for _, ci := range choices {
		c, _ := ci.(map[string]any)
		if c == nil {
			continue
		}
		delta, _ := c["delta"].(map[string]any)
		if delta == nil {
			continue
		}
		tcs, _ := delta["tool_calls"].([]any)
		for _, tci := range tcs {
			tc, _ := tci.(map[string]any)
			if tc == nil {
				continue
			}
			idx := 0
			if v, ok := tc["index"].(float64); ok {
				idx = int(v)
			}
			if seen[idx] {
				// Первый фрагмент уже отправлен: удалить у данного фрагмента name Ключ (удаляется если есть, идемпотентно).
				if fn, _ := tc["function"].(map[string]any); fn != nil {
					delete(fn, "name")
				}
				continue
			}
			// Первое появление: сохранить name ключ как есть (первый чанк апстрима обычно с непустым name；пустой name также отправляется,
			// и OpenAI Для "первый кадр без name」допуск согласован), затем единое удаление по шардам.
			seen[idx] = true
		}
	}
}

// normalizeFrame по OpenAI пересборка фрейма по вайтлисту потокового стандарта: оставлять только стандартные поля,
// Исключить шум апстрима (finish_reason:"" → null、пустой content/refusal、пустой tool_calls Список,
// пустой placeholder function_call、неизвестные поля верхнего уровня), пусто delta ключи всегда опускаются,
// usage отсутствует → null，Гарантирует парсинг любым стандартным клиентом по спецификации.
func normalizeFrame(obj map[string]any) map[string]any {
	out := map[string]any{}
	for _, k := range []string{"id", "object", "created", "model", "system_fingerprint", "service_tier"} {
		if v, ok := obj[k]; ok && v != nil {
			out[k] = v
		}
	}
	if _, ok := out["object"]; !ok {
		out["object"] = "chat.completion.chunk"
	}
	if _, ok := out["id"]; !ok {
		out["id"] = "chatcmpl-wb2api"
	}
	if chs, ok := obj["choices"].([]any); ok {
		nchs := make([]any, 0, len(chs))
		for _, ci := range chs {
			c, ok := ci.(map[string]any)
			if !ok {
				continue
			}
			nc := map[string]any{}
			if idx, ok := c["index"]; ok {
				nc["index"] = idx
			}
			delta := map[string]any{}
			if d, ok := c["delta"].(map[string]any); ok {
				if v, ok := d["role"].(string); ok && v != "" {
					delta["role"] = v
				}
				if v, ok := d["content"].(string); ok && v != "" {
					delta["content"] = v
				}
				if v, ok := d["reasoning_content"].(string); ok && v != "" {
					delta["reasoning_content"] = v
				}
				if v, ok := d["refusal"].(string); ok && v != "" {
					delta["refusal"] = v
				}
				if tcs, ok := d["tool_calls"].([]any); ok && len(tcs) > 0 {
					delta["tool_calls"] = tcs
				}
				if fc, ok := d["function_call"]; ok && fc != nil {
					// пустой placeholder function_call（name/arguments полностью пусто) считать шумом и отбросить
					keep := false
					if fcm, ok2 := fc.(map[string]any); ok2 {
						n, _ := fcm["name"].(string)
						a, _ := fcm["arguments"].(string)
						keep = n != "" || a != ""
					} else {
						keep = true
					}
					if keep {
						delta["function_call"] = fc
					}
				}
			}
			nc["delta"] = delta
			if fr, ok := c["finish_reason"].(string); ok && fr != "" {
				nc["finish_reason"] = fr
			} else {
				nc["finish_reason"] = nil
			}
			nchs = append(nchs, nc)
		}
		out["choices"] = nchs
	}
	if rawUsage, ok := obj["usage"]; ok {
		if u, ok := rawUsage.(map[string]any); ok {
			out["usage"] = normalizeUsageCacheAliases(u)
		} else {
			out["usage"] = rawUsage
		}
	} else {
		out["usage"] = nil
	}
	return out
}

// Stream Проброс на апстрим SSE До w（После покадровой нормализации flush），Гарантировать запись хотя бы одного [DONE]。
// Вызывающая сторона должна предварительно установить status 200；собственные настройки данной функции SSE headers。
// Стратегия стриминга: покадровая сквозная передача (нормализация уже очищена content шум), восстановление плавного стриминга как у апстрима.
//
// StreamHint Вариант (gateway_hint задача): апстрим error Добавляется при сквозной передаче фрейма
// error.gateway_hint Поле —message Оригинал без изменений,hint параллельное дополнение;hintFn Возвращает пустую строку
// Или nil при и Stream поведение побайтово идентично.
func Stream(w http.ResponseWriter, r io.Reader) error {
	return StreamHint(w, r, nil)
}

// StreamHint Совм. Stream，Но апстрим error перед пробросом фрейма hintFn(payload) записать возвращаемое значение в
// error.gateway_hint。hintFn для nil или вернуть пустую строку → Прозрачная передача как есть (без перезаписи).
// фолбэк пустого потока error Кадр ("empty upstream stream"）без hint（Локальная форма сбоя шлюза
// не покрыто, не выдумывать).
func StreamHint(w http.ResponseWriter, r io.Reader, hintFn func(string) string) error {
	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache")
	h.Set("Connection", "keep-alive")
	h.Set("X-Accel-Buffering", "no")
	fl, _ := w.(http.Flusher)

	// toolCallSeen межкадровая запись delta.tool_calls в котором уже отправлен первый чанк index，
	// для поштучной chunk схождение при сквозной передаче name для "каждый index раз» (выравнивание OpenAI официальный поток).
	toolCallSeen := map[int]bool{}

	// firstID уровень сообщений сквозного потока id База: кэшировать первый непустой upstream id，Потеря последующих кадров/при пустой строке переиспользовать
	// （issue #35：тот же SSE Все фреймы сообщения используют один реальный id，Бэкенд по id Объединение; ранее промежуточный фрейм
	// Дополнять всегда chatcmpl-wb2api сентинел, вызывает коллизию потока id расщепление). Во всём потоке нет реального id → Только тогда появляется sentinel.
	firstID := ""

	// writeRaw Записать кадр как есть (в обход normalizeFrame）И flush。апстрим error Кадр (error-passthrough）
	// и пустые фреймы ошибок потока необходимо сохранить error поле, не должно срезаться белым списком, поэтому выводится здесь.
	// gateway_hint：апстрим error перед выводом кадра по hintFn Дополнительно error.gateway_hint Поле
	// （error Добавить ключ к объекту,message/code/requestId и т.д. оригинал без изменений;hintFn для
	// nil / Пустая строка / не JSON Кадр → выводится как есть, без переписывания).
	writeRaw := func(payload string) error {
		if hint := frameGatewayHint(hintFn, payload); hint != "" {
			payload = attachHintToErrorFrame(payload, hint)
		}
		if _, werr := io.WriteString(w, "data: "+payload+"\n\n"); werr != nil {
			return werr
		}
		if fl != nil {
			fl.Flush()
		}
		return nil
	}

	// writeFrame взять payload После пересоздания по белому списку согласно спецификации data: Запись фрейма и flush。
	// Только JSON При успешном парсинге засчитывается как одна успешная пересылка (JSON При ошибке парсинга — фолбэк с записью как есть, без подсчёта).
	writeFrame := func(payload string) (int, error) {
		var obj map[string]any
		valid := 0
		if json.Unmarshal([]byte(payload), &obj) == nil {
			// прозрачная передача фрейма ошибки апстрима (error-passthrough）：Лента error Кадр ключа**Записать как есть**，Не идет через
			// normalizeFrame Белый список — белый список снимет error поле, клиент не видит upstream
			// code/msg/requestId。error.message т.е. оригинал апстрима (напр. 6004 ограничение частоты, блокировка модерацией),
			// учитывать в валидных фреймах (во избежание ошибочной дозаписи пустого потока "empty upstream stream"）。
			if _, hasErr := obj["error"]; hasErr {
				if werr := writeRaw(payload); werr != nil {
					return 0, werr
				}
				return 1, nil
			}
			// Сначала по index сходимость tool_calls name（Каждый index Сохраняется только первый фрагмент, последующие фрагменты удалить name ключ), затем нормализованно прокинуть.
			stripToolCallNames(obj, toolCallSeen)
			// id Докачка: первый фрейм непустой реальный id кэш; последующие кадры отсутствуют id / пустой id всегда использовать значение из кэша,
			// Имеет собственный id фреймы без изменений (фреймы разных веток потока могут каждый id）。
			if firstID == "" {
				if v, ok := obj["id"].(string); ok && v != "" {
					firstID = v
				}
			} else {
				if v, ok := obj["id"].(string); !ok || v == "" {
					obj["id"] = firstID
				}
			}
			if raw, err := json.Marshal(normalizeFrame(obj)); err == nil {
				payload = string(raw)
			}
			valid = 1
		}
		if _, werr := io.WriteString(w, "data: "+payload+"\n\n"); werr != nil {
			return 0, werr
		}
		if fl != nil {
			fl.Flush()
		}
		return valid, nil
	}

	br := bufio.NewReaderSize(r, 64*1024)
	validFrames := 0
readLoop:
	for {
		line, err := br.ReadString('\n')
		trimmed := strings.TrimRight(line, "\r\n")
		switch {
		case strings.HasPrefix(trimmed, "data: [DONE]"):
			// явное завершение апстрима: остановить чтение,DONE Любые последующие данные (включая мусорные фреймы) больше не проксируются.
			// [DONE] Единая запись после цикла, гарантирует ровно одну.
			break readLoop
		case strings.HasPrefix(trimmed, "data: "):
			n, werr := writeFrame(strings.TrimPrefix(trimmed, "data: "))
			validFrames += n
			if werr != nil {
				return werr
			}
		case trimmed != "":
			// Комментарий/Остальные строки:Прозрачная передача как есть
			if _, werr := io.WriteString(w, line); werr != nil {
				return werr
			}
			if fl != nil {
				fl.Flush()
			}
		}
		// Пустые строки (разделитель фреймов) поглощаются: функция генерирует сама "\n\n"
		if err != nil {
			if err == io.EOF {
				break
			}
			return err
		}
	}
	// Пустой поток (0 валидный фрейм): сначала записать один фрейм error（Обход normalizeFrame сохранить как есть error поле),
	// докомпенсация [DONE] Гарантировать корректное завершение клиента и вернуть не- nil error для логирования вызывающей стороной.
	// локальный fallback-кадр пустого потока шлюза идет через hintFn=nil путь прямой записи: данная форма не покрыта (не выдумывать hint），
	// И writeRaw hintFn Замыкание в ветке пустого потока может захватить контекст предыдущего кадра и вызвать ложное сопоставление.
	if validFrames == 0 {
		_ = writeRaw(`{"error":{"message":"empty upstream stream","type":"upstream_error","code":"upstream_parse"}}`)
	}
	// гарантировать запись ровно одного [DONE]（при пропуске апстрима — страхующая догрузка).
	if _, err := io.WriteString(w, "data: [DONE]\n\n"); err != nil {
		return err
	}
	if fl != nil {
		fl.Flush()
	}
	if validFrames == 0 {
		return errEmptyStream
	}
	return nil
}

// frameGatewayHint получить error кадра gateway_hint（hintFn отсутствует/При ошибке возвращается пустая строка → не прикладывать).
func frameGatewayHint(hintFn func(string) string, payload string) string {
	if hintFn == nil {
		return ""
	}
	// panic Изоляция:hint считается доп. функцией, любой дефект реализации не должен пробивать основной сквозной путь.
	defer func() { _ = recover() }()
	return strings.TrimSpace(hintFn(payload))
}

// attachHintToErrorFrame В error кадра error Привязка к объекту gateway_hint Поле.
// message/code/requestId и другие существующие ключи сохраняются как есть (только добавление, без изменения); не JSON / отсутствует error объект →
// payload Возврат как есть (лучше не добавлять hint и без нарушения сквозной передачи оригинала).
func attachHintToErrorFrame(payload, hint string) string {
	var obj map[string]any
	if json.Unmarshal([]byte(payload), &obj) != nil {
		return payload
	}
	e, ok := obj["error"].(map[string]any)
	if !ok {
		return payload
	}
	e["gateway_hint"] = hint
	out, err := json.Marshal(obj)
	if err != nil {
		return payload
	}
	return string(out)
}
