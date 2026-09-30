// Package prompt предоставляет собственный системный промпт шлюза: встроенный по умолчанию + перезапись файла + нейтральный промпт даунгрейда.
//
// Контекст: клиент (Claude Code/Codex и т.д. CLI）В system prompt инжект фиксированной шаблонной фразы,
// Апстрим-модерация по точному дословному совпадению ложно режет легитимный трафик (issue #36/PR39 11128）。
// решение: шлюз перед отправкой заменяет клиентский system/developer Сообщение,
// Устранить в корне system Ложное срабатывание отпечатка источника (пользователь/assistant строка отпечатка в сообщении по-прежнему от
// internal/upstream/sanitize.go очистка, два слоя накладываются, не взаимозаменяемы).
package prompt

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"os"
)

//go:embed defaultprompt.md
var defaultPrompt string

// Degraded Фолбэк-промпт: для обработки ложных срабатываний, намеренно предельно краткий нейтральный.
//
// Сценарий срабатывания:passthrough В режиме запрос заблокирован контент-политикой апстрима (HTTP 400 + текст на модерацию),
// При признании ложным срабатыванием отпечатка — повтор с минимальным нейтральным промптом один раз. Не фреймворк обхода — только для обхода
// system ложное срабатывание источника, не меняет семантику валидности команды пользователя.
const Degraded = "You are a helpful assistant. Respond in the user's language, follow the user's instructions, and be direct and concise."

// Load Нажать mode и file Загрузить текст системного промпта.
// - file Непусто → Чтение файла (не существует/при ошибке чтения вернуть error，вызывающая сторона fail fast）；
// - file пустой → Вернуть встроенный defaultPrompt。
//
// mode здесь только сквозное логирование (фактически custom/passthrough роутинг решает вызывающая сторона),
// Load отвечает только за"получить текст промпта"，Семантика маршрутизации не важна.
func Load(mode, file string) (string, error) {
	if file == "" {
		return defaultPrompt, nil
	}
	raw, err := os.ReadFile(file)
	if err != nil {
		return "", fmt.Errorf("prompt file %s: %w", file, err)
	}
	return string(raw), nil
}

// Rewrite Парсинг OpenAI тело запроса и заменить системный промпт:
// - Удалить messages Все в role для system/developer сообщения;
// - В messages Вставка записи в начало {"role":"system",«content":systemPrompt}；
// - остальные поля и user/assistant/tool сообщение дословно без изменений.
//
// Ошибка парсинга → вернуть как есть (никогда не падает):Rewrite — критический путь перезаписи исходящих,
// Любая ошибка парсинга не должна блокировать проксирование запроса, пусть апстрим обрабатывает по исходной семантике.
func Rewrite(body []byte, systemPrompt string) []byte {
	if len(body) == 0 || systemPrompt == "" {
		return body
	}
	var obj map[string]any
	if err := json.Unmarshal(body, &obj); err != nil {
		return body
	}
	msgs, ok := obj["messages"].([]any)
	if !ok {
		// отсутствует messages несоответствие поля или типа → Вставка одной записи system после остальные поля сохранить как есть.
		obj["messages"] = []any{map[string]any{"role": "system", "content": systemPrompt}}
		if out, err := json.Marshal(obj); err == nil {
			return out
		}
		return body
	}
	// Отфильтровать все system/developer сообщение, сохранить user/assistant/tool и другие роли.
	kept := make([]any, 0, len(msgs)+1)
	for _, m := range msgs {
		mm, ok := m.(map[string]any)
		if !ok {
			kept = append(kept, m)
			continue
		}
		role, _ := mm["role"].(string)
		if role == "system" || role == "developer" {
			continue
		}
		kept = append(kept, m)
	}
	// вставка одной записи в начало system Сообщение (prepend избежать семантики глобальной пересортировки).
	rewritten := append(
		[]any{map[string]any{"role": "system", "content": systemPrompt}},
		kept...,
	)
	obj["messages"] = rewritten
	out, err := json.Marshal(obj)
	if err != nil {
		return body
	}
	return out
}

// Append Парсинг OpenAI тело запроса и в"подряд с начала system/developer Блок"вставить запись после
// Собственный шлюз system промпт (issue #129 append режим):
// - начальный непрерывный блок = Из messages[0] начиная с role для system/developer（Точная строка
// совпадение, с Rewrite сообщения (единый критерий удаления); при первом не system/developer
// сообщение (вкл. не- map сообщение, нет role сообщения) — стоп;
// - Точка вставки = после конца непрерывного блока (длина блока 0 тогда сразу messages в самом начале);
// - все существующие сообщения (вкл. начальный блок, промежуточные system、user/assistant/tool）Дословно без изменений
// ——спецификация проекта клиента/Используется совместно с соглашениями инструментов и промптом шлюза.
//
// Защита и Rewrite Построчно совпадает: пусто body / пустой systemPrompt / Поврежден JSON → Вернуть как есть
// （никогда не падает); без messages Поле → messages=[Шлюз system]，остальные поля без изменений.
//
// проверка границ требует явного одновременного совпадения system и developer：Нормализация (developer→system）В
// Downstream prepareBody normalizeRoles，Append в начальном блоке при выполнении developer
// или developer。Роль сообщения шлюза: system а не developer——апстрим role Белый список
// не содержит developer，Вставить developer равносильно созданию неизбежной нормализации и лишнего 11128 Окно риска.
func Append(body []byte, systemPrompt string) []byte {
	if len(body) == 0 || systemPrompt == "" {
		return body
	}
	var obj map[string]any
	if err := json.Unmarshal(body, &obj); err != nil {
		return body
	}
	msgs, ok := obj["messages"].([]any)
	if !ok {
		// отсутствует messages несоответствие поля или типа → Вставка одной записи system после остальные поля сохранить как есть.
		obj["messages"] = []any{map[string]any{"role": "system", "content": systemPrompt}}
		if out, err := json.Marshal(obj); err == nil {
			return out
		}
		return body
	}
	// Сканировать непрерывную последовательность в начале system/developer блок, при первом не- system/developer Немедленная остановка.
	insertAt := 0
	for _, m := range msgs {
		mm, ok := m.(map[string]any)
		if !ok {
			break
		}
		role, _ := mm["role"].(string)
		if role != "system" && role != "developer" {
			break
		}
		insertAt++
	}
	gw := map[string]any{"role": "system", "content": systemPrompt}
	// Существующие сообщения дословно без изменений: только конкатенация в точке вставки, без перестановки и перезаписи элементов.
	rewritten := make([]any, 0, len(msgs)+1)
	rewritten = append(rewritten, msgs[:insertAt]...)
	rewritten = append(rewritten, gw)
	rewritten = append(rewritten, msgs[insertAt:]...)
	obj["messages"] = rewritten
	out, err := json.Marshal(obj)
	if err != nil {
		return body
	}
	return out
}
