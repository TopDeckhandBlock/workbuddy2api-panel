// context_catalog.go context_length / max_output_tokens Статическая резервная таблица знаний на уровне полей.
//
// источник данных 4 уровня (model-json-dynamic ТЗ; вход в цепочку поиска в model_catalog.go 
// ContextWindowListingV4 / MaxOutputTokensListingV4，Данный файл — № 2 уровень):
// - динамическое значение upstream (ModelInfo.ContextWindow/MaxTokens，т.е. maxInputTokens/maxOutputTokens）Авторитетный, приоритет;
// - Статическая таблица знаний этого файла (дополняется при нуле на удаленной стороне;model.json отсутствует/фолбэк на этапе компиляции при повреждении);
// - model.json Локальный кэш (каталог данных, включая рантайм models.dev Дополнение значений по требованию, см. model_catalog.go）；
// - models.dev Загрузка по требованию (асинхронно без блокировки; всё ещё неизвестно → context_length 1M fallback — лучше переоценить
// Не занижать: цена завышения — клиент не усекает, ошибка апстрима ретраится; цена занижения — даунстрим-клиент
// （Codex/ZCode/Claude Code Нажать context_length преждевременная обрезка) — потеря контекста впустую;
// max_output_tokens неизвестно → Поле опущено (нет достоверных данных для оценки верхнего лимита вывода, не выдумывать)).
//
// Таблица знаний**Определение в одном месте,CN/global общий для двух доменов**：context_length — неотъемлемое свойство модели —fork
// 706412584 Фактический вывод: "две зоны — один и тот же набор API два деплоя», то же id Контекст согласован, без по realm
// Необходимость шардирования (с effort уровня realm шарды намеренно различаются).
//
// Два типа источников значений, помечать построчно комментарием:
// - fork Фактически:706412584 прямое подключение к апстриму /console/enterprises/personal/models 
// maxInputTokens（/tmp/fork_diffs/70_927f57f9.diff，2026-09-13 фактическое измерение;
// global сторона не поддаётся прямому измерению, по аналогии id экстраполяция);
// - models.dev：https://models.dev/ записываемое значение (2026-09-16 запрос, взять несколько provider Консенсусное значение;
// Официальный источник напр. moonshotai/zai приоритет). Отсутствующие в списке или сильно неоднозначные не выдумывать.
package upstream

// DefaultContextWindow модели, отсутствующей и в таблице знаний context_length Фолбэк:1M。
// фактический порядок большинства upstream-моделей с большим окном; завышение лучше занижения (см. шапку файла).
const DefaultContextWindow int64 = 1000000

// contextCap Контекстные возможности модели (резервные записи на уровне полей).
// context Должно быть >0 (иначе запись бессмысленна, сразу 1M фолбэк);
// maxOutput для 0 означает, что верхний лимит вывода неизвестен → max_output_tokens Поле опущено (не выдумывать).
type contextCap struct {
	context int64
	maxOutput int64
}

// contextCapFallback context_length / max_output_tokens Таблица знаний (CN/global совместное использование).
// Каждый комментарий с пометкой источника: факт = fork 706412584 Прямое подключение CN /console На практике maxInputTokens
// （global сторона — та же id экстраполяция);models.dev = 2026-09-16 включить консенсусное значение; оценка = экстраполяция внутри семейства.
var contextCapFallback = map[string]contextCap{
	// ---- GLM Семейство (z-ai）----
	"glm-5.2": {context: 1000000, maxOutput: 131072}, // фактически (CN 1M；models.dev Консенсус 1M/131072）
	"glm-5.1": {context: 200000, maxOutput: 131072}, // фактически (CN 200K；models.dev Консенсус 200K/131072）
	"glm-5.3": {context: 1000000, maxOutput: 131072}, // Экстраполяция по замерам + models.dev Консенсус 1M/131072
	"glm-5.3-flash": {context: 1000000, maxOutput: 131072}, // models.dev Консенсус 1M/131072
	"glm-5v-turbo": {context: 200000, maxOutput: 131072}, // фактически (CN 200K；models.dev Консенсус 200K/131072）

	// ---- Kimi Семейство (moonshot）----
	"kimi-k2.7": {context: 256000, maxOutput: 65536}, // фактически (CN 256K）；Вывод 65536 для models.dev kimi-k2.7-code Оценка внутри семейства
	"kimi-k2.6": {context: 256000, maxOutput: 262144}, // фактически (CN 256K）；Вывод models.dev Официальный 262144
	"kimi-k2.5": {context: 164000, maxOutput: 262144}, // фактически (global на стороне так же id экстраполировать 164K）；Вывод models.dev Консенсус 262144
	"kimi-k3": {context: 1048576, maxOutput: 131072}, // models.dev официальный (moonshotai 1M/128K）
	"kimi-k2.8-preview": {context: 1048576, maxOutput: 0}, // models.dev（Kimi K2.8 Preview 1M；лимит вывода не включен, опущено)

	// ---- MiniMax / Hunyuan (tencent）----
	"minimax-m3": {context: 512000, maxOutput: 512000}, // фактически (CN 512K）；Вывод models.dev Консенсус 512000
	"hy3": {context: 192000, maxOutput: 64000}, // фактически (CN 192K/64K，repo hy3 выборка захвата — то же значение)
	"hy3-preview": {context: 262144, maxOutput: 64000}, // models.dev（Консенсус 262144/64000）
	"hy4-preview": {context: 1000000, maxOutput: 64000}, // Экстраполяция по замерам + models.dev（~1M/64000）
	"hy4-preview-x": {context: 1000000, maxOutput: 64000}, // экстраполяция по замерам (1M）；Вывод того же семейства hy4-preview оценка

	// ---- DeepSeek Семейство ----
	"deepseek-v4-pro": {context: 1000000, maxOutput: 384000}, // фактически (CN 1M；models.dev Консенсус 1M/384000）
	"deepseek-v4-flash": {context: 1000000, maxOutput: 384000}, // фактически (CN 1M；models.dev Консенсус 1M/384000）
	"deepseek-v4.1-flash": {context: 1000000, maxOutput: 384000}, // Экстраполяция по замерам + models.dev Консенсус 1M/384000

	// ---- OpenAI / Google（global семейство доменов)----
	"gpt-6-astra": {context: 1050000, maxOutput: 128000}, // models.dev（весь provider консистентно 1050000/128000）
	"gpt-5.6-sol": {context: 1050000, maxOutput: 128000}, // models.dev Консенсус
	"gpt-5.6-terra": {context: 1050000, maxOutput: 128000}, // models.dev Консенсус
	"gpt-5.6-luna": {context: 1050000, maxOutput: 128000}, // models.dev Консенсус
	"gpt-5.5": {context: 1050000, maxOutput: 128000}, // models.dev Консенсус
	"gpt-5.4": {context: 1050000, maxOutput: 128000}, // models.dev Консенсус
	"gpt-5.3-codex": {context: 400000, maxOutput: 128000}, // models.dev（весь provider консистентно 400000/128000）
	"gemini-3.5-flash": {context: 1048576, maxOutput: 65536}, // models.dev Консенсус

	// ---- global алиас доменной маршрутизации/Модель-псевдоним ----
	"auto": {context: 168000, maxOutput: 0}, // экстраполяция по замерам (fork global Статическая таблица 168K）；верхний лимит вывода неизвестен, опущено
}

// ContextWindowListing модель в /v1/models context_length（трёхуровневый поиск):
// remote（апстрим maxInputTokens）>0 тогда авторитетен; иначе lookup в таблице знаний; если всё ещё не найдено → DefaultContextWindow
// （1M，лучше переоценить, чем недооценить). Больше не выдавать ложные 131072。
func ContextWindowListing(model string, remote int64) int64 {
	if remote > 0 {
		return remote
	}
	if cap, ok := contextCapFallback[model]; ok && cap.context > 0 {
		return cap.context
	}
	return DefaultContextWindow
}

// MaxOutputTokensListing модель в /v1/models max_output_tokens（трёхуровневый поиск):
// remote（апстрим maxOutputTokens）>0 тогда авторитетен; иначе lookup в таблице знаний; если всё ещё не найдено → ok=false
// （вызывающая сторона опустила поле, не выдумывать лимит вывода). И ContextWindowListing 1M Фолбэк намеренно отличается:
// для верхнего лимита вывода нет безопасной стороны "лучше завысить», неизвестное — опускается.
func MaxOutputTokensListing(model string, remote int64) (int64, bool) {
	if remote > 0 {
		return remote, true
	}
	if cap, ok := contextCapFallback[model]; ok && cap.maxOutput > 0 {
		return cap.maxOutput, true
	}
	return 0, false
}
