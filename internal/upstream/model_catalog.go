// model_catalog.go context_length / max_output_tokens Четырёхуровневая цепочка поиска + model.json
// Локальный кеш (ТЗ model-json-dynamic）。
//
// Цепочка поиска (32a3c13 третий уровень → четыре уровня, динамическое значение апстрима всегда авторитетно):
// 1. динамическое значение upstream (ModelInfo.ContextWindow/MaxTokens）——авторитетно, всегда переопределяет model.json
// （даже если последнее обновится: апстрим — источник истины, ТЗ §очистка);
// 2. Статическая таблица сидов (context_catalog.go contextCapFallback，фолбэк на этапе компиляции);
// 3. model.json локальный кэш (каталог данных, включая загруженное во время выполнения из models.dev дополняющее значение + Сид репозитория
// embed начального значения); повреждение → деградировать seed и WARN без краша (отказоустойчивость ручного входа обслуживания);
// 4. Триггер models.dev Подгрузка по требованию (асинхронно, без блокировки текущего ответа)→ Запись значения model.json →
// В этот раз сначала записать 1M/пропуск, следующее попадание в кэш; негативный кэш 24h。
//
// model.json Потокобезопасность чтения/записи (один sync.Mutex Блокировка на всё время + Атомарная запись на диск tmp+rename，
// Несколько запросов одновременно miss одна модель пишется только один раз); файл поврежден/незаписываемость — тихий даунгрейд (таблица сидов→1M）。
//
// принадлежность состояния: уровень пакета catalogState синглтон (с modelsDev fetcher тот же режим, один на процесс).
// Для тестов resetModelCatalog / loadModelCatalogAt Изоляция.
package upstream

import (
	"embed"
	"encoding/json"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// modelSeedFS Сид-версия репозитория model.json（context_catalog Статическая таблица 27 миграция значения,source=seed）。
// Только model.json Источник начального значения при отсутствии; рабочего каталога model.json Если существует — приоритет за ним
// （Точка ручного обслуживания пользователем — прямое редактирование файла, толерантность формата обеспечивает loadModelCatalog страхующая валидация).
//
//go:embed model.json
var modelSeedFS embed.FS

// ModelCapEntry model.json одиночная запись (с той же схемой полей, что и статическая таблица + источник и время сбора).
// Source：seed（миграция сидов репозитория)/ modelsdev（загрузка по требованию в рантайме)/ manual（Ручной ввод пользователя
// ——Невозможно отличить ручной ввод от seed，Ручные записи сохраняют исходный source строка, семантически эквивалентно "не pull»).
// Context Обязательно >0（Ноль/отклонено проверкой отрицательных записей);MaxOutput 0 = Верхний лимит вывода неизвестен → Пропустить поле.
type ModelCapEntry struct {
	ContextLength int64 `json:"context_length"`
	MaxOutputTokens int64 `json:"max_output_tokens,omitempty"`
	FetchedAt string `json:"fetched_at,omitempty"` // RFC3339；seed Запись пуста
	Source string `json:"source"`
}

// modelCatalog model.json кэш-автомат (уровень пакета catalogState носитель полей синглтона).
type modelCatalog struct {
	mu sync.Mutex

	path string // Путь сохранения на диск (пусто = персистентность отключена: только в памяти + сид)
	entries map[string]ModelCapEntry
	loaded bool // entries уже инициализировано (включая деградированный режим при повреждении)
}

// catalogState синглтон уровня пакета: один на весь процесс model.json статус.
var catalogState = &modelCatalog{}

// initModelCatalogLocked Обеспечить entries Уже загружено (вызов с удержанием блокировки):
// - Рабочая директория model.json существует и валиден → Полная загрузка (невалидные записи отбрасываются и WARN）；
// - Файл не существует / повреждение целиком (невалидный JSON）→ сид embed Начальное значение (без сохранения на диск: сохранять "пользователь
// состояние "кэша пока нет», запись на диск только после первого успешного получения);
// - path пусто (не подключено, напр. тест/процесс-инструмент)→ Начальное значение сида.
func (c *modelCatalog) initLocked() {
	if c.loaded {
		return
	}
	c.entries = map[string]ModelCapEntry{}
	c.loaded = true
	if c.path == "" {
		c.loadSeedLocked()
		return
	}
	raw, err := os.ReadFile(c.path)
	if err != nil {
		// не существует / нечитаемо → Сид (форма при первом запуске).
		c.loadSeedLocked()
		return
	}
	var file map[string]ModelCapEntry
	if err := json.Unmarshal(raw, &file); err != nil {
		// полное повреждение: fallback-сид + WARN без краша (ТЗ §отказоустойчивость точки входа на ручном обслуживании).
		log.Printf("WARN: [upstream] model.json повреждён (фолбэк на встроенную таблицу сидов): path=%s err=%v", c.path, err)
		c.loadSeedLocked()
		return
	}
	for id, e := range file {
		if !validCapEntry(e) {
			log.Printf("WARN: [upstream] model.json отсев невалидных записей: model=%s entry=%+v", id, e)
			continue
		}
		c.entries[id] = e
	}
}

// loadSeedLocked Сид репозитория model.json впрыск entries（Вызов с удержанием блокировки).
func (c *modelCatalog) loadSeedLocked() {
	raw, err := modelSeedFS.ReadFile("model.json")
	if err != nil {
		// embed Гарантируется на этапе компиляции, теоретически недостижимо; защитный фолбэк через статическую таблицу (initLocked вызывающая сторона
		// поиск по цепочке № 2 уровень и так фолбэчит, здесь достаточно сохранить entries пусто).
		return
	}
	var seed map[string]ModelCapEntry
	if err := json.Unmarshal(raw, &seed); err != nil {
		return
	}
	for id, e := range seed {
		if validCapEntry(e) {
			c.entries[id] = e
		}
	}
}

// validCapEntry Проверка на уровне записи:context Положительное число + вывод неотрицательный (ТЗ §сторона записи с валидацией значения).
func validCapEntry(e ModelCapEntry) bool {
	return e.ContextLength > 0 && e.MaxOutputTokens >= 0
}

// get проверить model.json Кэш (№ 3 уровень). Вернуть записи и факт попадания.
// Только чтение памяти, без сети; загрузка и сетевые действия — ensure/отвечает сторона-триггер.
func (c *modelCatalog) get(model string) (ModelCapEntry, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.initLocked()
	e, ok := c.entries[model]
	if !ok || !validCapEntry(e) {
		return ModelCapEntry{}, false
	}
	return e, true
}

// put запись одного кэша (№ 4 уровня вызывается после успешной загрузки) и сохраняется на диск. Та же модель уже существует (обслуживалась вручную)
// всё равно перезаписывает — в рантайме приоритет у значения из pull models.dev Приоритет официального источника, достовернее ручного ввода;
// если не нужно перезаписывать, удалите model.json достаточно соответствующей записи в (значение, введенное вручную после загрузки, действует только в текущем процессе).
func (c *modelCatalog) put(model string, e ModelCapEntry) {
	if model == "" || !validCapEntry(e) {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.initLocked()
	c.entries[model] = e
	c.saveLocked()
}

// saveLocked Атомарная запись на диск (tmp + rename，pool state.json тот же режим). вызов под блокировкой.
// path пусто / каталог недоступен для записи / Ошибка сериализации → тихо (кэш в памяти остаётся активным, при следующем процессе — повторная загрузка).
func (c *modelCatalog) saveLocked() {
	if c.path == "" {
		return
	}
	raw, err := json.MarshalIndent(c.entries, "", " ")
	if err != nil {
		return
	}
	if dir := filepath.Dir(c.path); dir != "" {
		_ = os.MkdirAll(dir, 0o755)
	}
	tmp := c.path + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		log.Printf("WARN: [upstream] model.json Ошибка записи на диск (кэш в памяти всё ещё активен): path=%s err=%v", c.path, err)
		return
	}
	if err := os.Rename(tmp, c.path); err != nil {
		log.Printf("WARN: [upstream] model.json ошибка переименования при сохранении на диск: path=%s err=%v", c.path, err)
	}
}

// ---- на уровне пакета API（цепочка поиска 3/4 Уровень + подключение)----

// SetModelCatalogPath подключение model.json путь сохранения на диск (cmd/server вызов при старте,
// Каталог данных и state.json в том же стиле). Первый вызов применяется; последующие после загрузки обновляют только путь
// （без перезагрузки — внутри процесса в памяти entries считать эталоном).
func SetModelCatalogPath(path string) {
	c := catalogState
	c.mu.Lock()
	defer c.mu.Unlock()
	c.path = path
}

// modelCatalogGet № 3 Уровень:model.json попадание в кэш (содержит начальное значение seed и деградацию при повреждении).
func modelCatalogGet(model string) (ModelCapEntry, bool) {
	return catalogState.get(model)
}

// modelCatalogPut № 4 запись уровня:models.dev полученное значение попадает в кэш (с записью на диск).
func modelCatalogPut(model string, context, maxOutput int64) {
	modelCatalogPutSourced(model, context, maxOutput, "modelsdev")
}

// modelCatalogPutSourced Запись элемента указанного источника (тест может инжектировать fetched_at проверить формат записи на диск).
func modelCatalogPutSourced(model string, context, maxOutput int64, source string) {
	catalogState.put(model, ModelCapEntry{
		ContextLength: context,
		MaxOutputTokens: maxOutput,
		FetchedAt: time.Now().UTC().Format(time.RFC3339),
		Source: source,
	})
}

// resetModelCatalog изоляция тестов: сброс состояния синглтона (entries/path/loaded）。
func resetModelCatalog() {
	c := catalogState
	c.mu.Lock()
	defer c.mu.Unlock()
	c.entries = nil
	c.path = ""
	c.loaded = false
}

// loadModelCatalogAt Тестовая обвязка: после указания пути сразу триггерит загрузку (синхронно, можно ассертить файл
// поведение парсинга). В проде используется SetModelCatalogPath（ленивая инициализация при первом обращении get триггер загрузки).
func loadModelCatalogAt(path string) {
	SetModelCatalogPath(path)
	catalogState.mu.Lock()
	defer catalogState.mu.Unlock()
	catalogState.initLocked()
}

// ResetLookupChainForTest Межпакетный тестовый хук: очистка model.json Кэш и models.dev fetcher
// все singleton-состояния уровня пакета (вкл. cooldown выборки в процессе — защита server Асинхронность в конце теста пакета goroutine ввод
// реальная сеть, защита от кросс-тестового загрязнения кэша). Только тестовая ссылка (upstream внутри пакета resetModelsDev /
// resetModelCatalog эквивалент inline).
func ResetLookupChainForTest() {
	resetModelCatalog()
	resetModelsDev()
}

// ---- Четырёхуровневая цепочка поиска (для handler открытый вход, подпись и 32a3c13 совместимость с трёх-уровневой версией)----

// ContextWindowListingV4 четырёхуровневой цепочки поиска context_length Решение:
// 1. remote>0 Приоритет источника (динамическое значение upstream всегда переопределяет model.json，ТЗ §очистка);
// 2. Статическая таблица сидов (contextCapFallback）；
// 3. model.json кэш (содержит начальное значение seed / models.dev значение, дополненное в рантайме);
// 4. Вся цепочка miss и неотрицательный кэш → Асинхронный триггер models.dev Выгрузка (в этот раз возвращено DefaultContextWindow
// 1M，Без блокировки; запись после получения model.json для следующего попадания).
func ContextWindowListingV4(model string, remote int64, client *http.Client) int64 {
	if remote > 0 {
		return remote
	}
	if model == "" {
		return DefaultContextWindow
	}
	if cap, ok := contextCapFallback[model]; ok && cap.context > 0 {
		return cap.context // № 2 Уровень: статическая seed-таблица (фолбэк на этапе компиляции, всегда доступна)
	}
	if e, ok := modelCatalogGet(model); ok {
		return e.ContextLength // № 3 Уровень:model.json
	}
	if !modelsDev.negativeFresh(model) {
		// № 4 триггер уровня: сначала поиск в in-process индексе документов (после одной загрузки — резидент), при хите сразу в кэш
		// возврат (без ожидания асинхронной выборки); промах → Записать negative cache + Асинхронная загрузка (в этот раз сначала возврат 1M）。
		if e, ok := modelsDev.lookup(model); ok {
			modelCatalogPut(model, e.Context, e.Output)
			return e.Context
		}
		modelsDev.ensureDocAsync(client, "")
	}
	return DefaultContextWindow // № 4 Фолбэк уровня:1M（в этот раз вернуть сразу, после загрузки — попадание в следующий раз)
}

// MaxOutputTokensListingV4 четырёхуровневой цепочки поиска max_output_tokens решение (и context метрика
// Намеренно отличается: неизвестно → опущено, без запаса "лучше завысить»).
func MaxOutputTokensListingV4(model string, remote int64, client *http.Client) (int64, bool) {
	if remote > 0 {
		return remote, true
	}
	if model == "" {
		return 0, false
	}
	if cap, ok := contextCapFallback[model]; ok && cap.maxOutput > 0 {
		return cap.maxOutput, true // № 2 Уровень
	}
	if e, ok := modelCatalogGet(model); ok && e.MaxOutputTokens > 0 {
		return e.MaxOutputTokens, true // № 3 Уровень
	}
	if !modelsDev.negativeFresh(model) {
		// и ContextWindowListingV4 тот же триггер:lookup при хите сначала в кэш, затем возврат
		// （две цепочки поиска параллельно miss При одной модели второй вызывающий сразу получает только что записанное значение).
		if e, ok := modelsDev.lookup(model); ok {
			modelCatalogPut(model, e.Context, e.Output)
			return e.Output, e.Output > 0
		}
		modelsDev.ensureDocAsync(client, "")
	}
	return 0, false // № 4 фолбэк уровня: пропуск (лимит вывода не выдумывать)
}

// ---- № 4 обратный поток значения pull уровня (fetchDoc вызывать после успеха, записать model.json）----

// noteModelsDevMiss Промах запроса models.dev индекс → негативный кэш уже lookup запись.
// данная функция — lookup + Клеевой слой кэша записи:fetchDoc После выгрузки в документ для "ранее miss модель»
// повторно запросить и записать model.json（Следующий /v1/models Прямое попадание в № 3 уровень).
func (f *modelsDevFetcher) backfillMisses() {
	f.mu.Lock()
	doc := f.doc
	missed := make([]string, 0, len(f.negatives))
	for m := range f.negatives {
		missed = append(missed, m)
	}
	f.mu.Unlock()
	if doc == nil {
		return
	}
	for _, m := range missed {
		e, ok := doc[m]
		if !ok {
			continue // все еще не найдено: негативный кеш 24h вступает в силу, не записывать
		}
		modelCatalogPut(m, e.Context, e.Output)
		// Успешный рефлоу: удалить запись негативного кэша (модель уже имеет значение, далее по 3 кэш уровня,
		// больше не попадает в этот список).
		f.mu.Lock()
		delete(f.negatives, m)
		f.mu.Unlock()
	}
}
