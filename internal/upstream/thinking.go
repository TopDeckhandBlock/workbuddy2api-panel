// thinking.go DeepSeek цепочка рассуждений вкл.: инъекция в тело исходящего запроса thinking:{type:"enabled"} + Уровень по умолчанию.
//
// Первопричина (issue #43，Hermes Реверс официального клиента codebuddy.js подтверждено):
// Официальный клиент для deepseek Системная метка модели thinkingFormat:"deepseek" + requiresReasoningContentOnAssistantMessages，
// При запросе "вкл. мышление» передавать явно thinking:{type:"enabled"}，Иначе апстрим по умолчанию отвечает без reasoning
// （цепочка рассуждений не возвращается). Шлюз payload слой ранее полностью не знал об этом поле, в транзитных запросах этого флага нет
// → Апстрим не отдает цепочку рассуждений;glm/kimi идти по другому thinkingFormat（qwen Система enable_thinking или включено по умолчанию) поэтому нормально.
//
// возврат на доработку (Hermes #43 приёмочное тестирование):
//
//	thinking.type=enabled Одного поля недостаточно — реальный апстрим deepseek-v4-flash Для "без reasoning_effort」голый запрос
//	все равно отвечать без reasoning (reasoning_content Длина 0），Лента reasoning_effort:high только тогда есть цепочка рассуждений.
//	обратный codebuddy.js подтверждение:isThinkingEnabled = !!(reasoning_summary || reasoning_effort || reasoning?.effort)，
//	case "deepseek" enabled ветка сохраняется в фактическом исходящем трафике одновременно reasoning_effort，Официальный "вкл. рассуждение»= thinking.type:enabled
//	+ Тариф effort；дефолтный профиль из reasoning.defaultEffort ?? Фолбэк "high"（configure thinking При отсутствии источника warn fallback to 'high'）。
//
// поведение выровнено с официальным клиентом (комбинация двух каналов):
// - thinking.type уже явно enabled / disabled → явное управление клиентом, никогда не переопределять;disabled при этом копировать как есть case Поведение
// Удалить reasoning_effort（snake/camel два поля).enabled но отсутствует effort → дополнить дефолтным профилем (официальный configure поведение).
// - отсутствует thinking / thinking.type пустой / уже есть reasoning_effort → инжект {type:"enabled"} + дополнить дефолтным профилем.
// - Явно reasoning_effort Никакого перекрытия и даунгрейда (даунгрейд — на payload.go normalizeReasoningEffort）。
// - не deepseek модель (glm/kimi/qwen и т.д.)→ без изменений.
package upstream

import (
	"strings"
)

// defaultDeepSeekEffort фолбэк на дефолтный профиль официального клиента (configure thinking При отсутствии источника warn fallback to 'high'，
// REASONING_SUPPLEMENTS.defaultEffort Также является "high"）。После дополнения идти через normalizeReasoningEffort пайплайн деградации,
// Модель не поддерживается high при этом автоматически попадает в ≤high максимальный поддерживаемый уровень.
const defaultDeepSeekEffort = "high"

// lookupDefaultEffort Из FetchModels Кэшированный defaultEfforts Таблица ищет профиль по умолчанию по имени модели.
// nil map или модель не закеширована → пустая строка (thinking.go Хардкод отката high）。
// Ключ — модель ID как есть (с efforts Выравнивание кэша:normalizeReasoningEffort точное совпадение model）。
func lookupDefaultEffort(defaultEfforts map[string]string, model string) string {
	if len(defaultEfforts) == 0 || model == "" {
		return ""
	}
	return defaultEfforts[model]
}

// isDeepSeekModel имя модели начинается с deepseek в качестве префикса (без учёта регистра).
// перекрытие deepseek-v4.1-flash / deepseek-v4-pro / deepseek-r1 и др. варианты;
// Префиксное совпадение по официальному thinkingFormat:"deepseek" критерий определения, избегать пропуска инжекта.
func isDeepSeekModel(model string) bool {
	return strings.HasPrefix(strings.ToLower(strings.TrimSpace(model)), "deepseek")
}

// backfillReasoningContent DeepSeek консистентность между раундами: гарантировать, что каждая assistant Сообщение содержит
// reasoning_content поле и значение string——т.е. requiresReasoningContentOnAssistantMessages
// （Официальный клиент matches Правило,issue #165 выравнивание с официальным apply гейт).
//
// Гейт (в соответствии с официальным ReasoningContentBackfillRule：thinkingEnabled || hasTrace），
// thinkingEnabled Взято из тела запроса после инъекции thinking.type == "enabled"（injectThinking выполнить первым,
// payload.go порядок конвейера уже гарантирован; данный шлюз для deepseek Безусловная инъекция enabled，эквивалентно не disabled в любом случае дополнить):
// - не deepseek Модель → Без изменений (isDeepSeekModel шлюз не двигается).
// - deepseek + enabled（Содержит L1 после инъекции)→ Каждая assistant Гарантировать reasoning_content Да
// string：уже есть string сохранить как есть (не перезаписывать);reasoning непустое string И rc не string →
// Копировать reasoning значение; оба отсутствуют → дополнить пустой строкой ""。Потеря сторонним клиентом обратного вызова inference (zero-trace)
// ниже официально и так дополняет, шлюз ранее портировал только hasTrace Половина (issue #165 точка исправления).
// - deepseek + disabled + без следов → без изменений (официальный thinkingEnabled=false И ec=false → без дополнения).
// - deepseek + disabled + Есть след → докомпенсация (официальный hasTrace половина, обе стороны совпадают).
//
// нормализация по официальному "string"!=typeof Семантика:reasoning_content для null/цифры и прочие не string
// значение не считается "имеющимся», уходит вДополнить ""/Копировать ветку (если ключ старого кода есть — пропуск,null будет считаться "уже есть», пропуск пополнения).
// hasTrace = любое сообщение в сессии с непустым reasoning（string）или уже есть reasoning_content Поле
// （В отличие от официального только сканирования assistant ширина калибра, влияет только на disabled ветка, декоративное отличие).
func backfillReasoningContent(obj map[string]any) {
	model, _ := obj["model"].(string)
	if !isDeepSeekModel(model) {
		return
	}
	msgs, ok := obj["messages"].([]any)
	if !ok || len(msgs) == 0 {
		return
	}
	// thinkingEnabled половина: после инъекции чтения thinking.type（С официальным el.thinkingEnabled соответствие).
	thinkingEnabled := false
	if th, ok := obj["thinking"].(map[string]any); ok {
		if typ, _ := th["type"].(string); strings.EqualFold(strings.TrimSpace(typ), "enabled") {
			thinkingEnabled = true
		}
	}
	// hasTrace половина: проверка наличия любых reasoning След (непустой reasoning или уже есть reasoning_content）。
	hasTrace := false
	for _, mm := range msgs {
		msg, ok := mm.(map[string]any)
		if !ok {
			continue
		}
		if r, ok := msg["reasoning"].(string); ok && r != "" {
			hasTrace = true
			break
		}
		if _, ok := msg["reasoning_content"]; ok {
			hasTrace = true
			break
		}
	}
	if !thinkingEnabled && !hasTrace {
		return
	}
	// Второй проход: все assistant Дополнение сообщения/Копировать reasoning_content поле, и зеркально гарантирует
	// reasoning Поле существует и непусто (issue #165 Доп. отзыв — часть аккаунтов/Тенант для thinking Форма
	// Валидация len(reasoning)>0：отсутствует/null/Пустая строка 400，пустая строка 200；Официальный CLI изначально для
	// assistant повесить на один раунд reasoning Текст, см. itemsToMessages applyPendingReasoning）。
	// условие пропуска учитывает только string（Официальный "string"!=typeof только тогда действовать):null/Нормализация чисел.
	// - reasoning уже непусто string → не трогать;
	// - rc непустое string → Зеркальная запись rc значение (оба поля в итоге существуют и непустые);
	// - оба отсутствуют/Всё пусто → Добавить один пробел " "（апстрим len>0 Не trim：Пропуск пустой строки через шлюз, пустая строка
	// Однако — плейсхолдер пустой строки имеет официальный Moonshot Правило "-" аналогичный прецедент, причём для контекста модели
	// без влияния на семантику: поле — сквозной бит проверки, не потребляется контентом).
	for _, mm := range msgs {
		msg, ok := mm.(map[string]any)
		if !ok {
			continue
		}
		role, _ := msg["role"].(string)
		if role != "assistant" {
			continue
		}
		rc, hasRC := msg["reasoning_content"].(string)
		if hasRC {
			// rc уже есть string → не перекрывать (сохранить прежнюю семантику).
		} else if r, ok := msg["reasoning"].(string); ok {
			rc = r
			msg["reasoning_content"] = rc
		} else {
			rc = ""
			msg["reasoning_content"] = rc
		}
		// Зеркало:reasoning отсутствует/null/Пустая строка → Нормализация (непустой rc приоритет, если ничего нет — дополнить " "）。
		if r, ok := msg["reasoning"].(string); ok && r != "" {
			continue // уже непусто → Без перекрытия
		}
		if rc != "" {
			msg["reasoning"] = rc
		} else {
			msg["reasoning"] = " "
		}
	}
}

// injectThinking Нажать DeepSeek Правило переключателя chain-of-thought переписывает тело запроса. Не deepseek без изменений.
//
// Основная логика (выравнивание с официальным клиентом):
// - 「«Вкл. мышление» обязательно thinking.type=enabled + Есть effort уровень (Hermes #43 доказательство отклонения).
// - Явно thinking.type Непусто → Явное управление клиентом:enabled нехватка effort тогда подставить дефолтный профиль;
// disabled уважить и удалить reasoning_effort（snake/camel два поля).
// - отсутствует thinking / type пустой / уже есть effort → инжект enabled и дополнить профилем по умолчанию (уже есть effort без перезаписи).
//
// defaultEffort Профиль по умолчанию, объявленный для этой модели (из FetchModels Кеш reasoning.defaultEffort）；
// при пустой строке — fallback на хардкод defaultDeepSeekEffort（обратная совместимость).
func injectThinking(obj map[string]any, defaultEffort string) {
	model, _ := obj["model"].(string)
	if !isDeepSeekModel(model) {
		return
	}
	th, ok := obj["thinking"].(map[string]any)
	typ := ""
	if ok {
		typ, _ = th["type"].(string)
		typ = strings.TrimSpace(typ)
	}
	// Явное управление ветвлением:type Непустой (enabled/disabled все — явное намерение)→ Не изменять type。
	if typ != "" {
		if strings.EqualFold(typ, "disabled") {
			delete(obj, "reasoning_effort")
			delete(obj, "reasoningEffort")
			return // disabled：Откл. reasoning и без каких-либо effort（Копировать как в клиенте case поведение)
		}
		ensureDeepSeekEffort(obj, defaultEffort) // Явно enabled нехватка effort → Дополнить дефолтным тарифом
		return
	}
	// отсутствует thinking（Или thinking некорректное значение-необъект) или thinking объект type отсутствует/Пусто:
	// инжект enabled（Клиент case "deepseek" поведение). Есть reasoning_effort также идёт по этой ветке
	// （effort оставить для существующей логики деградации, переключатель остается включенным).
	if !ok {
		obj["thinking"] = map[string]any{"type": "enabled"}
	} else {
		th["type"] = "enabled"
	}
	ensureDeepSeekEffort(obj, defaultEffort)
}

// ensureDeepSeekEffort нехватка effort при отсутствии тарифа подставить дефолтный тариф (snake приоритет,camel фолбэк).
// Уже есть любой effort → Не перезаписывать (явный тир не трогать, даунгрейд делегировать normalizeReasoningEffort）。
// defaultEffort Пустая строка → Откат defaultDeepSeekEffort（Хардкод "high"）。
func ensureDeepSeekEffort(obj map[string]any, defaultEffort string) {
	_, hasSnake := obj["reasoning_effort"]
	if hasSnake {
		return
	}
	_, hasCamel := obj["reasoningEffort"]
	if hasCamel {
		return
	}
	if defaultEffort == "" {
		defaultEffort = defaultDeepSeekEffort
	}
	obj["reasoning_effort"] = defaultEffort
}
