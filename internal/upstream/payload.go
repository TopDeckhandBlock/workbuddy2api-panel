// payload.go Перезаписать отправляемое в апстрим chat Тело запроса:
// 1. Принудительно stream:true（апстрим отклоняет не-потоковый)
// 2. tool_choice нормализация (у апстрима это поле — string，Форма объекта будет 400 code=11101）
// 3. image_url нормализация (апстрим принимает только OpenAI объектная форма, строка будет 400 code=11101）
package upstream

import (
	"encoding/json"
	"log"
	"strings"
)

// PrepareBodyOpt Заказ pass Перезапись;sanitize=false поведение полностью восстанавливается (только принудительно stream + Нормализация tool_choice）。
func PrepareBodyOpt(src []byte, sanitize bool) []byte {
	return PrepareBodyOptWithEffortsAndDefault(src, sanitize, nil, nil)
}

// PrepareBodyOptWithEfforts В PrepareBodyOpt на базе по моделям supportedEfforts Деградация reasoning_effort：
// Только если запрос явно передал и модель не поддерживает этот уровень, заменить на ≤макс. поддерживаемый тир для запрошенного тира; если все поддерживаемые тиры выше запрошенного — берётся минимальный;
// Неизвестная модель/неизвестный уровень/Если поле отсутствует — всегда пробрасывать.efforts для nil Означает неизвестно (без даунгрейда).
//
// Обёртка обратной совместимости: не передавать defaultEfforts（без объявления модели — профиль по умолчанию),thinking.go Хардкод отката high。
func PrepareBodyOptWithEfforts(src []byte, sanitize bool, efforts map[string][]string) []byte {
	return PrepareBodyOptWithEffortsAndDefault(src, sanitize, efforts, nil)
}

// PrepareBodyOptWithEffortsAndDefault Полный пайплайн:efforts Деградация + thinking.go Нажать
// defaultEfforts（заполнение пофайлу по умолчанию, заявленному моделью).defaultEfforts для nil При этом совместимо со старым поведением
// （deepseek откат к хардкоду при отсутствии профиля high）。
func PrepareBodyOptWithEffortsAndDefault(src []byte, sanitize bool, efforts map[string][]string, defaultEfforts map[string]string) []byte {
	if len(src) == 0 {
		return src
	}
	var obj map[string]any
	if err := json.Unmarshal(src, &obj); err != nil {
		return src
	}
	obj["stream"] = true
	// max_completion_tokens → max_tokens Перевод (поглощение upstream PR #116，Closes #117）：
	// OpenAI в спецификации max_tokens Уже deprecated、max_completion_tokens — новое поле;
	// DeepSeek Harness и т.п. новые клиенты отправляют только алиас.WorkBuddy Апстрим (CN /v2 и global
	// /console тот же источник) распознает только max_tokens——Прозрачная передача алиаса будет проигнорирована апстримом с откатом к лимиту вывода по умолчанию
	// （На практике 32000），Длиннопоточная задача прервана.
	translateMaxCompletionTokens(obj)
	// stream_options только если body дополнить если явно не передано {include_usage: true}（D7）：
	// Официальный CLI в стриме поле обязательно, апстрим по нему возвращает в последнем фрейме usage расход; при явной передаче не перезаписывать.
	if _, has := obj["stream_options"]; !has {
		obj["stream_options"] = map[string]any{"include_usage": true}
	}
	normalizeToolChoice(obj)
	normalizeToolPatterns(obj)
	normalizeRoles(obj)
	normalizeImageURL(obj)
	// tool Сопряжение в два шага (см. tool_pairing.go）：Сначала пересортировка, затем очистка. Выполняется для всех моделей (независимо от
	// deepseek-only sanitize переключатель). Это страховочная сетка "пропустить запрос» — неполные пары
	// tool_calls/tool Результат заставит апстрим отвечать на каждое следующее сообщение 400，необходимо предварительно исключить;
	// Вставка "не» в середину результата tool Сообщение (Codex image_resize_notice）также считается разрывом пары,
	// Сначала repack сдвинуть позже, затем cleanup Удалять orphan'ы, единая логика с обеих сторон.
	if msgs, ok := obj["messages"].([]any); ok {
		msgs, _ = repackToolResultBlocks(msgs)
		msgs, _ = cleanupOrphanToolCalls(msgs)
		// Без изменений оба шага возвращают исходное slice，обратная запись здесь — no-op; любая перестановка шагов/Удалить
		// （Даже если последующие шаги без изменений) должен попасть в obj——Нельзя записывать обратно только на "последнем шаге изменения»,
		// Иначе repack Результат одиночного применения будет исходным slice потеря из-за перезаписи.
		obj["messages"] = msgs
	}
	// DeepSeek Переключатель цепочки рассуждений (см. thinking.go）：инжект thinking.type=enabled + при отсутствии профиля подставить дефолтный.
	// До normalizeReasoningEffort выполнение: добавленный дефолтный тариф тоже идет по существующему пайплайну деградации,
	// При неподдержке моделью дефолтного тарифа автопереход на ≤ максимальный поддерживаемый уровень дефолтного тарифа (без выхода на несоответствующий тариф).
	modelName, _ := obj["model"].(string)
	injectThinking(obj, lookupDefaultEffort(defaultEfforts, modelName))
	normalizeReasoningEffort(obj, efforts)
	// DeepSeek консистентность за несколько раундов:assistant Сообщение содержит reasoning заполнение следа при наличии reasoning_content
	// （requiresReasoningContentOnAssistantMessages，См. thinking.go）。
	backfillReasoningContent(obj)
	if sanitize {
		if msgs, ok := obj["messages"].([]any); ok {
			sanitizeMessages(msgs)
		}
	}
	out, err := json.Marshal(obj)
	if err != nil {
		return src
	}
	return out
}

// translateMaxCompletionTokens взять OpenAI Алиас max_completion_tokens транслировать в апстрим
// распознанный max_tokens（Поглотить апстрим PR #116）。Правило: явно max_tokens приоритет (алиас — только удаление);
// Псевдоним — неположительное значение (0/null/отрицательное число) не переводить (0/null Семантика — "не задано», отрицательное — недопустимое значение,
// Перевод — перенос мусора в max_tokens）；Нечисловой алиас (строка и др. некорректные данные) не переводить (как есть
// Сквозная передача — репортит апстрим 11101 ошибка параметра). Единый критерий для обоих доменов:CN /v2 и global /console Это один и тот же набор
// API，перевод без разделения realm。
func translateMaxCompletionTokens(obj map[string]any) {
	alias, has := obj["max_completion_tokens"]
	delete(obj, "max_completion_tokens") // Независимо от перевода, алиасы всегда удалять (уменьшить body объём и шум при отладке)
	if !has {
		return
	}
	if _, explicit := obj["max_tokens"]; explicit {
		return // Явно max_tokens Приоритет: алиасы только удалять, не переводить
	}
	// json.Unmarshal Число → float64（округлить до целого и перезаписать, избегать 1.28e5 Экспоненциальная запись/дробное число
	// хвост → апстрим body）；Защитная совместимость с другими числовыми типами (int семейство — ручная сборка map вызывающая сторона).
	switch v := alias.(type) {
	case float64:
		if v > 0 && v == float64(int64(v)) {
			obj["max_tokens"] = int64(v)
		}
	case int64:
		if v > 0 {
			obj["max_tokens"] = v
		}
	case int:
		if v > 0 {
			obj["max_tokens"] = int64(v)
		}
	}
}

// effortRank уровни от низкого к высокому.
var effortRank = map[string]int{"off": 0, "minimal": 1, "low": 2, "medium": 3, "high": 4, "xhigh": 5, "max": 6}

// normalizeReasoningEffort по модели supportedEfforts Деградация reasoning_effort（snake/camel Совместимость по двум полям).
// - Поддержка модели для запрошенного тира → прозрачная передача как есть
// - запрошенный профиль не поддерживается → Изменить на ≤Максимально поддерживаемый уровень запроса (даунгрейд)
// - все поддерживаемые уровни выше запрошенного → Брать минимальный поддерживаемый тариф (минимальное отклонение)
// - Неизвестная модель/неизвестный уровень/Поле не передано/Модель не кэширована → Всегда проксировать как есть
func normalizeReasoningEffort(obj map[string]any, efforts map[string][]string) {
	if len(efforts) == 0 {
		return
	}
	model, _ := obj["model"].(string)
	if model == "" {
		return
	}
	supported, ok := efforts[model]
	if !ok || len(supported) == 0 {
		return
	}
	key := ""
	if _, present := obj["reasoning_effort"]; present {
		key = "reasoning_effort"
	} else if _, present := obj["reasoningEffort"]; present {
		key = "reasoningEffort"
	} else {
		return
	}
	reqStr, ok := obj[key].(string)
	if !ok {
		return
	}
	reqStr = strings.TrimSpace(strings.ToLower(reqStr))
	reqIdx, known := effortRank[reqStr]
	if !known {
		return
	}
	// В ≤Выбрать максимальный поддерживаемый тир из тиров запроса; перезаписывать только при попадании и отличии от запроса.
	best, bestIdx := "", -1
	for _, s := range supported {
		idx, k := effortRank[strings.TrimSpace(strings.ToLower(s))]
		if k && idx <= reqIdx && idx > bestIdx {
			best, bestIdx = s, idx
		}
	}
	if best != "" {
		if !strings.EqualFold(best, reqStr) {
			obj[key] = best
			log.Printf("reasoning_effort downgraded model=%s %s -> %s", model, reqStr, best)
		}
		return
	}
	// Все поддерживаемые уровни выше запрошенного: брать минимальный поддерживаемый.
	lowest, lowestIdx := "", 1<<30
	for _, s := range supported {
		idx, k := effortRank[strings.TrimSpace(strings.ToLower(s))]
		if k && idx < lowestIdx {
			lowest, lowestIdx = s, idx
		}
	}
	if lowest != "" {
		obj[key] = lowest
		log.Printf("reasoning_effort floored model=%s %s -> %s", model, reqStr, lowest)
	}
}

// normalizeRoles взять messages внутри developer роли унифицированы в system。
//
// Контекст: апстрим к messages role Проверка поля по белому списку,developer Не в вайтлисте,
// При хите сразу HTTP 400 code=11128。developer Да OpenAI в новой спецификации system псевдоним для
// （Codex / Cursor и т.д. новые клиенты используют его для передачи system инструкция уровня), переписать как system без потери семантики.
//
// Данная нормализация — "совместимость протокола» (дополняет upstream role белый список), а не "десенсибилизация контента»,
// Поэтому намеренно с SanitizeFingerprints / sanitize Развязка параметров: даже если sanitize=false также нормально нормализовать.
//
// Принимается только developer это значение: остальные role（system/user/assistant/tool/любые неизвестные значения) сохранять как есть,
// Не мержить, не пересортировывать, не удалять сообщения (апстрим для мульти system поведение пока не замерено, слияние внесёт новые переменные).
func normalizeRoles(obj map[string]any) {
	msgs, ok := obj["messages"].([]any)
	if !ok {
		return
	}
	for i, m := range msgs {
		msg, ok := m.(map[string]any)
		if !ok {
			continue
		}
		role, ok := msg["role"].(string)
		if !ok {
			continue
		}
		if strings.EqualFold(strings.TrimSpace(role), "developer") {
			msg["role"] = "system"
			log.Printf("role normalized developer->system idx=%d", i)
		}
	}
}

// normalizeImageURL совместимость OpenAI chat два типа мультимодального контента image_url синтаксис.
//
// OpenAI Chat Completions штатно использовать объектную форму {"url«:»...«,«detail»:«..."}，
// Часть клиентов (а также Responses -> Chat конвертер) отправит в виде строки "data:..." Или
// "https://..."。WorkBuddy Апстрим принимает только объект, на строку вернёт 400 code=11101
// "cannot unmarshal string into ... ImageContent"。
//
// Только преобразование формы: строка -> {"url": исходное значение}；существующий объект и его url/detail/mime_type
// Оставить как есть; пустая строка, отсутствующее значение, невалидное внутри объекта url Не подставлять значения по умолчанию, пусть апстрим вернёт реальную ошибку.
func normalizeImageURL(obj map[string]any) {
	msgs, ok := obj["messages"].([]any)
	if !ok {
		return
	}
	for _, rawMsg := range msgs {
		msg, ok := rawMsg.(map[string]any)
		if !ok {
			continue
		}
		parts, ok := msg["content"].([]any)
		if !ok {
			continue
		}
		for _, rawPart := range parts {
			part, ok := rawPart.(map[string]any)
			if !ok || part["type"] != "image_url" {
				continue
			}
			imageURL, ok := part["image_url"].(string)
			if !ok || imageURL == "" {
				continue
			}
			part["image_url"] = map[string]any{"url": imageURL}
		}
	}
}

// ensureConsoleSystem global realm Фолбэк system Инъекция (поглощение PR #45，Защита console Апстрим домена code 11-128）：
// Первое сообщение не system в момент messages Добавить одну запись в начало fallback system（"You are a helpful assistant."）。
// только для global Вызов запроса (CN состояние не меняется; даже если первая запись — system повторная инъекция не выполняется).
// body При невозможности парсинга вернуть как есть (с prepareBody семантикасовпадение: плохой body здесь повторно не переводить в ошибку).
func ensureConsoleSystem(body []byte) []byte {
	if len(body) == 0 {
		return body
	}
	var obj map[string]any
	if err := json.Unmarshal(body, &obj); err != nil {
		return body
	}
	msgs, ok := obj["messages"].([]any)
	if !ok || len(msgs) == 0 {
		return body
	}
	first, ok := msgs[0].(map[string]any)
	if ok {
		if role, _ := first["role"].(string); strings.EqualFold(strings.TrimSpace(role), "system") {
			return body // первая запись уже system：Не инжектировать
		}
	}
	obj["messages"] = append([]any{map[string]any{"role": "system", "content": "You are a helpful assistant."}}, msgs...)
	out, err := json.Marshal(obj)
	if err != nil {
		return body
	}
	return out
}

// normalizeToolChoice По апстриму Go struct（string тип) переписать OpenAI tool_choice。
// - "none" → Удалить tool_choice + Удалить tools/functions
// - {"type":«none"} → То же
// - {"type":"auto"/"required"} → Строка "auto"/«required"
// - {"type":"function",«function":{"name":"x"}} → Строка "x"
// - Другие объекты/нескаляр → Удалить tool_choice
func normalizeToolChoice(obj map[string]any) {
	suppress := func() {
		delete(obj, "tools")
		delete(obj, "functions")
	}
	tc, present := obj["tool_choice"]
	if !present {
		return
	}
	switch v := tc.(type) {
	case string:
		if strings.EqualFold(strings.TrimSpace(v), "none") {
			delete(obj, "tool_choice")
			suppress()
		}
	case map[string]any:
		typ, _ := v["type"].(string)
		typ = strings.ToLower(strings.TrimSpace(typ))
		switch typ {
		case "none":
			delete(obj, "tool_choice")
			suppress()
		case "auto", "required":
			obj["tool_choice"] = typ
		case "function":
			name := ""
			if fn, ok := v["function"].(map[string]any); ok {
				name, _ = fn["name"].(string)
			}
			if name == "" {
				name, _ = v["name"].(string)
			}
			if name = strings.TrimSpace(name); name != "" {
				obj["tool_choice"] = name
			} else {
				obj["tool_choice"] = "auto"
			}
		default:
			delete(obj, "tool_choice")
		}
	default:
		delete(obj, "tool_choice")
	}
}

// normalizeToolPatterns Нормализация tools в поддереве pattern нестандартное экранирование `\_`（→ `_`）。
//
// апстрим для tools[].function.parameters Выполнять строго JSON Schema/Проверка регулярной грамматикой,pattern Содержит
// `\_`（экранированное литеральное подчёркивание) будет отклонено целиком:400 code=11129 invalid_function_call_
// parameters（displayMsg「определение инструмента невалидно»).`\_` не является валидным экранированием ни в одном диалекте regex,
// но все основные движки (RE2/PCRE/JS Annex B）все толерантно считать как `_` сам — валидатор апстрима выше их
// Все строже (в сравнении с V8 Строгая грамматика u флаг, единственная реализация также отклоняет). Кейс:ZCode exa Плагин
// agent_run Инструмент runId/previousRunId Лента `^agent\_run\_`，deepseek детерминизм всего семейства 400
// → на стороне шлюза отнести к ErrClient только смена номера без штрафа, но инкремент счетчика поражений подряд → ротация исчерпана 5 Серия поражений триггерит понижение за серию поражений,
// Клиент 503（2026-09-29/30 два реальных кейса).schema отклонение уровня — смена аккаунта бесполезна, исправлять только до отправки.
//
// нормализация без потерь:`\_` и `_` Семантика матчинга одинакова во всех движках (проверено на каждом движке + контрольный пробник апстрима: после нормализации
// 200），Функциональность инструмента без изменений. Меняется только tools поддерево (pattern значение + patternProperties ключ);
// в теле сообщения `\_`（Например Windows путь C:\_x）не трогать. Прочие нестандартные экранирования (`\:` и т.д.) не подтверждено
// триггер, без расширения охвата — при реальном кейсе обсудим отдельно. Независимо от sanitize Переключатель: это "пропустить запрос», а не десенсибилизация.
func normalizeToolPatterns(obj map[string]any) {
	rawTools, ok := obj["tools"].([]any)
	if !ok {
		return
	}
	for _, raw := range rawTools {
		tool, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		// OpenAI Форма tools[].function.parameters；Голый tools[].parameters совместимо.
		if fn, ok := tool["function"].(map[string]any); ok {
			unescapePatternLiteralEscapes(fn["parameters"])
		}
		unescapePatternLiteralEscapes(tool["parameters"])
	}
}

// unescapePatternLiteralEscapes Рекурсивная перезапись schema В дереве pattern значение и patternProperties
// в ключе `\_` → `_`（patternProperties ключ тоже regex;map Ключ нельзя менять на месте, при попадании пересоздать
// этот слой).
func unescapePatternLiteralEscapes(node any) {
	switch n := node.(type) {
	case map[string]any:
		if p, ok := n["pattern"].(string); ok && strings.Contains(p, `\_`) {
			n["pattern"] = strings.ReplaceAll(p, `\_`, `_`)
		}
		if props, ok := n["patternProperties"].(map[string]any); ok {
			rebuilt := false
			fixed := make(map[string]any, len(props))
			for k, v := range props {
				if strings.Contains(k, `\_`) {
					k = strings.ReplaceAll(k, `\_`, `_`)
					rebuilt = true
				}
				fixed[k] = v
			}
			if rebuilt {
				n["patternProperties"] = fixed
			}
		}
		for _, v := range n {
			unescapePatternLiteralEscapes(v)
		}
	case []any:
		for _, v := range n {
			unescapePatternLiteralEscapes(v)
		}
	}
}
