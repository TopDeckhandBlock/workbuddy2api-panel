// global зондирование каталога моделей: чисто динамическая генерация имени модели и его окна / Метаданные возможностей (v3-config-merge）。
//
// Детекция в два параллельных потока:/v3/config（Основной путь,IDE UA полнофункциональная версия)+ семейство корпоративных эндпоинтов
// （/v2 → /console дополнение недостающего), объединение = v3 записи — основа, корпоративные эндпоинты — дополнение v3 отсутствующий id
// （Например gpt-5.3-codex Только в /v2 выдача). Поле множителя (credits）хотя поставляется вместе с каталогом, но
// только прокинуть на отображение, без инъекции costTier、не участвует в выборе номера.
//
// Чистая динамика: без отката к статическим спискам — если каталог не тянется, значит апстрим домена недоступен,
// фейковый список заставит клиента выбрать лишь 11102 модели (решение продукта: без фолбэка).
package upstream

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
)

// GlobalModelNames Международная версия (global realm）Исторический статический список (PLAN §7.2 Приложение 21 имя).
// после полной динамизации**больше не используется как база каталога моделей/Фолбэк**：/v1/models Пробрасывать только фактически выданные апстримом модели.
// сохранено только для исторической сверки (global e2e эталон по diff логов наблюдений).
var GlobalModelNames = []string{
	"default-model",
	"fast-model",
	"balanced-model",
	"primary-model",
	"hy4-preview",
	"gpt-5.6-sol",
	"gpt-5.6-terra",
	"deep-model",
	"deepseek-v4.1-flash",
	"gpt-6-astra",
	"hy4-preview-f",
	"hy3",
	"glm-5.2",
	"gpt-5.6-luna",
	"gpt-5.5",
	"gpt-5.4",
	"gpt-5.3-codex",
	"gemini-3.5-flash",
	"glm-5.3",
	"kimi-k3",
	"kimi-k2.6",
}

// fetchGlobalModelsCache Кэш результатов зондирования (семантическая ссылка CN Боковой handler.dynamicModelsCache：1h TTL +
// 5min отрицательный кэш при ошибке). По Client хранение в экземпляре (effortsMu тот же режим), тест — создать заново Client т.е. изоляция.
// Mutex Встроено, с modelList Нет гонки конкурентного чтения (единственная точка чтения/записи в этом файле).
type fetchGlobalModelsCache struct {
	sync.Mutex
	names []string // Кэш успеха: объединение имен моделей (дедуплицировано);nil = не прозондировано/ошибка
	infos []ModelInfo // кэш успеха: запись-объект со всеми полями (узкая таблица/Форма сбоя — nil）
	fetched time.Time
	lastFail time.Time
}

// globalModelsTTL / globalModelsFailCooldown длительность кэша пробы: успех 1h，ошибка 5min Негативный кэш.
const (
	globalModelsTTL = time.Hour
	globalModelsFailCooldown = 5 * time.Minute
)

// globalModelsProbePaths global Последовательность кандидатов эндпоинтов каталога enterprise-моделей (по realm Переключить base，путь"Семейство"）：
// /v2 Приоритет семейства (фактич. /v2/enterprises/personal/models 200 содержит полную таблицу моделей),
// /console Действие fallback（старый путь того же домена, или 500）。v3-config-merge после это семейство понижается до корпоративного резервного маршрута
// （/v3/config как основной маршрут, с параллельным пробингом семейства;gpt-5.3-codex и т.п. уникальные модели семейства попадают в объединение через это).
var globalModelsProbePaths = []string{
	"/v2/enterprises/personal/models",
	"/console/enterprises/personal/models",
}

// FetchGlobalModels детект global каталог имён моделей аккаунта и вернуть**список имен моделей**（без метаданных).
//
// Только динамика: при успехе вернуть объединение (дедупликация), кэш 1h；сбой (оба пути не 2xx / Ошибка парсинга /
// пустой список) запись 5min Негативный кэш, возврат nil（без статического фолбэка). Кэш/Попадание в негативный кэш: прямой возврат, ноль вызовов апстрима.
//
// Ответственность вызывающей стороны: только при наличии global вызывать для аккаунта (если нет — не детектить);GlobalEnabled При закрытии (аварийный выход)
// вызывать запрещено — данный метод вызывается globalOn(a) внутренний фолбэк, если аккаунт откатился из-за переключателя cn то вернуть nil。
func (c *Client) FetchGlobalModels(a *auth.Auth) []string {
	names, _ := c.fetchGlobalModelsOnce(a)
	return names
}

// FetchGlobalModelInfos детект global Каталог моделей аккаунта с возвратом всех полей ModelInfo Список.
// и FetchGlobalModels Совместное использование одного зондирования и кэша (names + infos единая запись в кэш):
// Форма объекта 200 → Запись со всеми полями; узкотабличная форма / ошибка пробы / Негативный кэш / не global Маршрутизирующий аккаунт
// → nil（Вызывающая сторона по ID Вывод списка — голые записи, без генерации полей).
// Аккаунт из-за GlobalEnabled откат свитча cn при этом не пробовать (globalOn фолбэк, ноль вызовов апстрима).
func (c *Client) FetchGlobalModelInfos(a *auth.Auth) []ModelInfo {
	_, infos := c.fetchGlobalModelsOnce(a)
	return infos
}

// fetchGlobalModelsOnce решение одной проверки (попадание в кэш/Негативный кэш/запуск пробы), возврат (names, infos)。
// чисто динамически: успех = Дедупликация объединения; полный провал = nil（без fallback на статику).
// infos Только при успешном детекте объектной формы не nil。
func (c *Client) fetchGlobalModelsOnce(a *auth.Auth) (names []string, infos []ModelInfo) {
	if !c.globalOn(a) {
		// Аварийный фолбэк: аккаунт не маршрутизируется global апстрим → Без зондирования (ноль вызовов апстрима).
		return nil, nil
	}

	c.globalModels.Lock()
	if len(c.globalModels.names) > 0 && time.Since(c.globalModels.fetched) < globalModelsTTL {
		names, infos := c.globalModels.names, c.globalModels.infos
		c.globalModels.Unlock()
		return names, infos
	}
	if !c.globalModels.lastFail.IsZero() && time.Since(c.globalModels.lastFail) < globalModelsFailCooldown {
		// В период охлаждения negative cache: не бить повторно в апстрим, сразу считать ошибкой (без статического fallback).
		c.globalModels.Unlock()
		return nil, nil
	}
	c.globalModels.Unlock()

	names, infos, efforts, defaults, err := c.probeGlobalModels(a)
	if err != nil || len(names) == 0 {
		// Ошибка пробы: негативный кэш + вернуть nil（effort Бакет без записи,prepareBody Ход globalEffortMap статический fallback).
		c.globalModels.Lock()
		c.globalModels.lastFail = time.Now()
		c.globalModels.names = nil
		c.globalModels.infos = nil
		c.globalModels.Unlock()
		return nil, nil
	}
	// global Домен effort возможность: зондируемый выданный supportedEfforts/defaultEffort Авторитетная запись global Бакет
	// （raw remote，не включать в статическую таблицу — статический фолбэк в prepareBody globalEffortMap и
	// /v1/models EffortListing по требованию внутри fallback）。пустая проба не пишется (защита от очистки существующих бакетов).
	if len(efforts) > 0 || len(defaults) > 0 {
		c.storeEfforts("global", efforts, defaults)
	}
	c.storeModelRates("global", infos)

	// успех: дедупликация результатов проб.names/infos все уходит в кэш; чувствительные к выбору поля вроде множителя — только для отображения,
	// Не инжектировать costTier。
	seen := make(map[string]bool, len(names))
	merged := make([]string, 0, len(names))
	for _, id := range names {
		if id == "" || seen[id] {
			continue
		}
		seen[id] = true
		merged = append(merged, id)
	}

	c.globalModels.Lock()
	c.globalModels.names = merged
	c.globalModels.infos = infos
	c.globalModels.fetched = time.Now()
	c.globalModels.lastFail = time.Time{}
	c.globalModels.Unlock()
	return merged, infos
}

// probeGlobalModels инициировать один раз global Проба каталога моделей (v3-config-merge）：
// /v3/config（основной,IDE UA полная версия) и семейство enterprise-эндпоинтов (/v2 → /console фолбэк, восполнение пробелов)
// **Конкурентность**Объединение после детекта. Возвращает список имён моделей (объединено, без дедупликации — дедупликация в
// fetchGlobalModelsOnce）、все поля ModelInfo（форма объекта; узкая таблица — nil）И effort
// Бакет возможностей (supportedEfforts/defaultEffort，может быть пустым). Объединенная метрика:v3 Приоритет — записи
// （credits и др. поля как v3 за основу), enterprise-эндпоинт только дополняет v3 отсутствующая модель id；дедупликация key =
// Модель id，Порядок вывода стабилен. Ошибка только если оба пути отказали (эквивалент исходному "все эндпоинты семейства недоступны 2xx」
// семантика негативного кэша); при отказе одного канала — фолбэк на результат другого + warn логи, без взаимного влияния.
func (c *Client) probeGlobalModels(a *auth.Auth) (names []string, infos []ModelInfo, efforts map[string][]string, defaults map[string]string, err error) {
	type probeResult struct {
		names []string
		infos []ModelInfo
		err error
	}
	// probeV3 Однократно /v3/config Проба (UA параметризовано). Данный эндпоинт для разных UA выдача**Разные наборы моделей**：
	// IDE UA и CLI UA у каждого свои уникальные модели (см. codeBuddyCLIUA комментарий), поэтому два конкурентных пути — объединение.
	probeV3 := func(ua string) chan probeResult {
		ch := make(chan probeResult, 1)
		go func() {
			// chatBase Уже по realm Переключить global base。
			byID, perr := c.fetchV3ConfigModelMap(a, ua)
			if perr != nil {
				ch <- probeResult{err: perr}
				return
			}
			ids := make([]string, 0, len(byID))
			outInfos := make([]ModelInfo, 0, len(byID))
			for _, mi := range byID {
				if nonChatModel(mi.ID, mi.MaxTokens, mi.Tags) {
					continue
				}
				ids = append(ids, mi.ID)
				outInfos = append(outInfos, mi)
			}
			sort.Strings(ids) // map Порядок итерации случаен, сортировка обеспечивает стабильность вывода
			ch <- probeResult{names: ids, infos: outInfos}
		}()
		return ch
	}
	v3IDECh := probeV3(codeBuddyIDEUA)
	v3CLICh := probeV3(codeBuddyCLIUA)
	enterpriseCh := make(chan probeResult, 1)
	go func() {
		// Семейство корпоративных эндпоинтов:/v2 Приоритетный → /console Fallback (существующий порядок probe, без регрессии).
		var lastErr error
		for _, path := range globalModelsProbePaths {
			names, infos, perr := c.globalModelsOnce(a, path)
			if perr != nil {
				lastErr = perr
				continue
			}
			enterpriseCh <- probeResult{names: names, infos: infos}
			return
		}
		enterpriseCh <- probeResult{err: lastErr}
	}()
	v3IDE := <-v3IDECh
	v3CLI := <-v3CLICh
	enterprise := <-enterpriseCh
	// v3 Самослияние двух путей:IDE поле маршрута авторитетно (ответ больше, поля одиночной записи полнее),CLI маршрут дополняет только отсутствующие модели id。
	// Один канал успешен — используется он; только при отказе обоих — с err Переход к проверке деградации downstream.
	var v3 probeResult
	switch {
	case v3IDE.err != nil && v3CLI.err != nil:
		v3 = probeResult{err: v3IDE.err}
	case v3IDE.err != nil:
		log.Printf("WARN: [upstream] global models: v3/config IDE-UA probe failed (CLI-UA only): %v", v3IDE.err)
		v3 = v3CLI
	case v3CLI.err != nil:
		log.Printf("WARN: [upstream] global models: v3/config CLI-UA probe failed (IDE-UA only): %v", v3CLI.err)
		v3 = v3IDE
	default:
		vn, vi := mergeGlobalCatalog(v3IDE.names, v3IDE.infos, v3CLI.names, v3CLI.infos)
		v3 = probeResult{names: vn, infos: vi}
	}

	if v3.err != nil && enterprise.err != nil {
		// оба канала неуспешны → Семантика негативного кэша (эквивалент — все эндпоинты исходного семейства недоступны 2xx）。
		return nil, nil, nil, nil, v3.err
	}
	if v3.err != nil {
		// /v3 деградация при ошибке: не тянет за собой результат enterprise-эндпоинта (деградация только enterprise-эндпоинт + warn）。
		log.Printf("WARN: [upstream] global models: v3/config probe failed (degraded to enterprise endpoint): %v", v3.err)
		names, infos, efforts, defaults = extractEfforts(enterprise.infos)
		return names, infos, efforts, defaults, nil
	}
	if enterprise.err != nil {
		log.Printf("WARN: [upstream] global models: enterprise endpoint failed (v3/config only): %v", enterprise.err)
		names, infos, efforts, defaults = extractEfforts(v3.infos)
		return names, infos, efforts, defaults, nil
	}
	// Оба пути успешны:v3 основа, слияние с дополнением enterprise-эндпоинта (вкл. effort объединение бакетов,v3 авторитет).
	v3Names, v3Infos, v3Efforts, v3Defaults := extractEfforts(v3.infos)
	if len(v3Names) == 0 {
		v3Names = v3.names
	}
	entNames, entInfos, entEfforts, entDefaults := extractEfforts(enterprise.infos)
	if len(entNames) == 0 {
		entNames = enterprise.names
	}
	names, infos = mergeGlobalCatalog(v3Names, v3Infos, entNames, entInfos)
	efforts = mergeEffortBuckets(v3Efforts, entEfforts)
	defaults = mergeEffortDefaults(v3Defaults, entDefaults)
	return names, infos, efforts, defaults, nil
}

// extractEfforts извлечь из списка записей effort Бакет возможностей (supportedEfforts Приоритет массива;
// Нет массива, но defaultEffort Одиночный непустой профиль тоже учитывается defaults бакет) и попутно вернуть отсортированный names。
func extractEfforts(infos []ModelInfo) (names []string, out []ModelInfo, efforts map[string][]string, defaults map[string]string) {
	names = make([]string, 0, len(infos))
	for _, mi := range infos {
		if mi.ID == "" {
			continue
		}
		names = append(names, mi.ID)
		out = append(out, mi)
		if len(mi.Efforts) > 0 {
			if efforts == nil {
				efforts = make(map[string][]string)
			}
			efforts[mi.ID] = mi.Efforts
		}
		if mi.DefaultEffort != "" {
			if defaults == nil {
				defaults = make(map[string]string)
			}
			defaults[mi.ID] = mi.DefaultEffort
		}
	}
	return names, out, efforts, defaults
}

// mergeGlobalCatalog слияние двух каналов (v3 Основной, корпоративное восполнение):names Нажать id Дедупликация (v3 Исходный порядок впереди,
// доп. enterprise-эндпоинты добавляются после исходного порядка — стабильный вывод);infos Синхронное слияние (v3 Поле записи — авторитетно,
// Записи enterprise-эндпоинта только в id при отсутствии — в объединение).
// узкотабличная форма (infos nil）при этом сохранять nil——Не выдумывать отсутствующие поля объекта.
func mergeGlobalCatalog(primaryNames []string, primaryInfos []ModelInfo, secondaryNames []string, secondaryInfos []ModelInfo) (names []string, infos []ModelInfo) {
	if len(secondaryNames) == 0 {
		return primaryNames, primaryInfos
	}
	seen := make(map[string]bool, len(primaryNames)+len(secondaryNames))
	out := make([]string, 0, len(primaryNames)+len(secondaryNames))
	for _, id := range primaryNames {
		if id == "" || seen[id] {
			continue
		}
		seen[id] = true
		out = append(out, id)
	}
	var outInfos []ModelInfo
	if primaryInfos != nil {
		outInfos = make([]ModelInfo, 0, len(primaryInfos)+len(secondaryInfos))
		outInfos = append(outInfos, primaryInfos...)
	}
	for _, id := range secondaryNames {
		if id == "" || seen[id] {
			continue
		}
		seen[id] = true
		out = append(out, id)
		// ответ enterprise в узкой таблице (secondaryInfos nil / при превышении числа записей) данный id нет поля объекта,
		// infos оставить как есть (вызывающая сторона по id Вывод списка — голые записи, без генерации полей).
		for _, mi := range secondaryInfos {
			if mi.ID == id {
				outInfos = append(outInfos, mi)
				break
			}
		}
	}
	return out, outInfos
}

// mergeEffortBuckets Объединение двух потоков effort бакет: основной путь (v3）Авторитетно, корпоративный эндпоинт дополняет только отсутствующие тарифы модели основного маршрута.
func mergeEffortBuckets(primary, secondary map[string][]string) map[string][]string {
	if len(secondary) == 0 {
		return primary
	}
	out := primary
	if out == nil {
		out = make(map[string][]string, len(secondary))
	}
	for id, v := range secondary {
		if _, ok := out[id]; !ok {
			out[id] = v
		}
	}
	return out
}

// mergeEffortDefaults Объединение двух потоков defaultEffort：основной путь (v3）авторитетен, enterprise-эндпоинт только дополняет недостающее.
func mergeEffortDefaults(primary, secondary map[string]string) map[string]string {
	if len(secondary) == 0 {
		return primary
	}
	out := primary
	if out == nil {
		out = make(map[string]string, len(secondary))
	}
	for id, v := range secondary {
		if _, ok := out[id]; !ok {
			out[id] = v
		}
	}
	return out
}

// globalModelsOnce Зондирование одной конечной точки.2xx + Распарсить непустой список → (names, infos, nil)；Иначе (nil, nil, err)。
func (c *Client) globalModelsOnce(a *auth.Auth, path string) ([]string, []ModelInfo, error) {
	url := c.chatBase(a) + path // Нажать realm Переключить base：global Аккаунт → global base
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return nil, nil, err
	}
	c.CommonHeaders(req, a) // Общие заголовки запроса (Origin/Referer/UA），и FetchModels Та же модель
	// AccessToken Снапшот под блокировкой (см. auth.AccessTokenValue：keepalive обновление в a.mu перезапись внутри).
	req.Header.Set("Authorization", "Bearer "+a.AccessTokenValue())
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, nil, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		// Ошибка чтения → Ошибка транспортного уровня: оборванный body Не входит в парсинг (зондирование через негативный кэш lastFail，без штрафа для аккаунта).
		return nil, nil, fmt.Errorf("read body: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, nil, fmt.Errorf("global models status %d: %s", resp.StatusCode, truncate(string(raw), 120))
	}
	names, infos, _, _, err := parseGlobalModelNames(raw)
	return names, infos, err
}

// parseGlobalModelNames Совместимый парсинг каталога моделей с несколькими конвертами (международная площадка ранее в нескольких envelope Переключение между,
// Распознавание только одной формы ошибочно примет успешно вошедший аккаунт за"Без модели"）：
// - конверт:code существует и не 0 только тогда отклонять (нет code поле тоже пропускается);payload = data（непустой не
// null иначе — целый пакет;
// - Позиция в массиве моделей:payload сам является массивом, или map вниз models/items/list/data/result
// Любой ключ (рекурсивно искать первый непустой массив вниз по уровням) — перекрытие data.models / data.items /
// data.list / Верхний уровень models и др. варианты;
// - основная форма массива: массив объектов по dynModelEntry Парсинг всех полей (с CN Каталоги изоморфны, поле нулевое
// дрейф):maxInputTokens/maxOutputTokens/maxAllowedSize/supportsReasoning/
// supportsImages/reasoning.*；id фолбэк при отсутствии значения name；disabled исключить;
// - Узкая таблица: массив строк → Только ID，Метаданные оставить пустыми (окно покрывается 4-уровневой цепочкой поиска вызывающей стороны);
// - Динамический fallback: если основной парсинг не дал ни одной доступной записи (апстрим сменил ключи), поштучный lenient-парсинг объектов —
// id Последовательный откат id/modelId/model/name，фолбэк ключа окна contextWindow/maxTokens，
// reasoning уровень (defaultEffort Приоритет нового ключа,effort fallback старого ключа) сохраняется.
//
// одновременно выдать effort Бакет возможностей (supportedEfforts/defaultEffort）。
// парсинг успешен, но список пуст → вернуть ошибку (эквивалент"эндпоинт вернул неполные данные"）。
func parseGlobalModelNames(raw []byte) (names []string, infos []ModelInfo, efforts map[string][]string, defaults map[string]string, err error) {
	fail := func(e error) ([]string, []ModelInfo, map[string][]string, map[string]string, error) {
		return nil, nil, nil, nil, e
	}
	// Уровень конверта:code Отклонить не- 0 бизнес-код (отсутствие поля = пропустить, совместимо с endpoint'ом каталога без конверта).
	var envelope map[string]json.RawMessage
	if err := json.Unmarshal(raw, &envelope); err != nil {
		return fail(fmt.Errorf("global models parse: %w", err))
	}
	if codeRaw, ok := envelope["code"]; ok {
		var code int
		if json.Unmarshal(codeRaw, &code) == nil && code != 0 {
			return fail(fmt.Errorf("global models code=%d", code))
		}
	}
	payload := json.RawMessage(raw)
	if data, ok := envelope["data"]; ok {
		if t := strings.TrimSpace(string(data)); t != "" && t != "null" {
			payload = data
		}
	}
	arr, ok := resolveGlobalModelsArray(payload)
	if !ok {
		return fail(fmt.Errorf("global models empty list"))
	}

	// основная форма: массив объектов → dynModelEntry Все поля (с CN FetchModels Общий парсинг, нулевое расхождение метрик).
	var entries []dynModelEntry
	if json.Unmarshal(arr, &entries) == nil {
		out := make([]string, 0, len(entries))
		objInfos := make([]ModelInfo, 0, len(entries))
		for _, m := range entries {
			// id Цепочка фолбэка id→modelId→model→name（Нестрогий ключ встречается только в global в вариантах каталога).
			id := m.ID
			if id == "" {
				id = m.ModelID
			}
			if id == "" {
				id = m.Model
			}
			if id == "" {
				id = m.Name
			}
			if id == "" || m.Disabled {
				continue
			}
			out = append(out, id)
			mi := m.modelInfo()
			mi.ID = id // name в fallback-форме id взято из name，Выравнивание names Вывод
			objInfos = append(objInfos, mi)
			if len(m.Reasoning.SupportedEfforts) > 0 {
				if efforts == nil {
					efforts = make(map[string][]string)
				}
				efforts[id] = m.Reasoning.SupportedEfforts
			}
			if d := m.Reasoning.DefaultEffort; d != "" {
				if defaults == nil {
					defaults = make(map[string]string)
				}
				defaults[id] = d
			}
		}
		if len(out) > 0 {
			return out, objInfos, efforts, defaults, nil
		}
		// Доступные записи: 0（ни одно имя ключа не совпало): срабатывает динамический fallback ниже, здесь пусто не возвращается.
		efforts, defaults = nil, nil
	}

	// Узкая таблица: массив строк → Только ID（отсутствует effort Метаданные, без объектных полей → infos nil）。
	var strs []string
	if json.Unmarshal(arr, &strs) == nil {
		out := make([]string, 0, len(strs))
		for _, id := range strs {
			if id = strings.TrimSpace(id); id != "" {
				out = append(out, id)
			}
		}
		if len(out) > 0 {
			return out, nil, nil, nil, nil
		}
		return fail(fmt.Errorf("global models empty list"))
	}

	// динамический фолбэк: мягкий парсинг по объектам (при смене имени ключа апстримом не приводит к пустому списку всего домена).
	var items []any
	if json.Unmarshal(arr, &items) != nil {
		return fail(fmt.Errorf("global models parse: unsupported payload shape"))
	}
	out := make([]string, 0, len(items))
	infos = make([]ModelInfo, 0, len(items))
	for _, item := range items {
		switch v := item.(type) {
		case string: // голые значения в смешанном массиве ID：обрабатывать как запись узкой таблицы
			if id := strings.TrimSpace(v); id != "" {
				out = append(out, id)
				infos = append(infos, ModelInfo{ID: id})
			}
		case map[string]any:
			mi, ok := parseGlobalModelLoose(v)
			if !ok {
				continue
			}
			out = append(out, mi.ID)
			infos = append(infos, mi)
			if len(mi.Efforts) > 0 {
				if efforts == nil {
					efforts = make(map[string][]string)
				}
				efforts[mi.ID] = mi.Efforts
			}
			if mi.DefaultEffort != "" {
				if defaults == nil {
					defaults = make(map[string]string)
				}
				defaults[mi.ID] = mi.DefaultEffort
			}
		}
	}
	if len(out) == 0 {
		return fail(fmt.Errorf("global models empty list"))
	}
	return out, infos, efforts, defaults, nil
}

// resolveGlobalModelsArray Из payload локализовать массив моделей:payload сам массив — использовать напрямую;
// Да map то последовательно пробовать models/items/list/data/result ключ (рекурсивный спуск, берется первый успешно парсируемый
// ветка массива). Массив не найден → false。
func resolveGlobalModelsArray(payload json.RawMessage) (json.RawMessage, bool) {
	trimmed := strings.TrimSpace(string(payload))
	if strings.HasPrefix(trimmed, "[") {
		return payload, true
	}
	var obj map[string]json.RawMessage
	if json.Unmarshal(payload, &obj) != nil {
		return nil, false
	}
	for _, key := range []string{"models", "items", "list", "data", "result"} {
		if nested, ok := obj[key]; ok {
			if arr, ok2 := resolveGlobalModelsArray(nested); ok2 {
				return arr, true
			}
		}
	}
	return nil, false
}

// parseGlobalModelLoose нестрогий парсинг объекта одной модели (fallback-путь с несколькими envelope):id Последовательный откат
// id/modelId/model/name；Окно/Откат ключа лимита contextWindow/maxTokens；reasoning Грейд
// defaultEffort Приоритет нового ключа,effort Фолбэк на старый ключ.disabled Исключить.Credits Никогда не парсить
// （PLAN §3.D2：Множитель не учитывается global путь).
func parseGlobalModelLoose(obj map[string]any) (ModelInfo, bool) {
	str := func(key string) string {
		s, _ := obj[key].(string)
		return strings.TrimSpace(s)
	}
	id := str("id")
	for _, k := range []string{"modelId", "model", "name"} {
		if id != "" {
			break
		}
		id = str(k)
	}
	if id == "" {
		return ModelInfo{}, false
	}
	if disabled, ok := obj["disabled"].(bool); ok && disabled {
		return ModelInfo{}, false
	}
	num := func(keys ...string) int64 {
		for _, k := range keys {
			if n, ok := obj[k].(float64); ok && n > 0 {
				return int64(n)
			}
		}
		return 0
	}
	mi := ModelInfo{
		ID: id,
		Name: str("name"),
		ContextWindow: num("maxInputTokens", "contextWindow"),
		MaxTokens: num("maxOutputTokens", "maxTokens"),
		MaxAllowedSize: num("maxAllowedSize"),
	}
	mi.SupportsReasoning, _ = obj["supportsReasoning"].(bool)
	mi.SupportsImages, _ = obj["supportsImages"].(bool)
	if r, ok := obj["reasoning"].(map[string]any); ok {
		reasonStr := func(key string) string {
			s, _ := r[key].(string)
			return strings.TrimSpace(s)
		}
		mi.DefaultEffort = reasonStr("defaultEffort")
		if mi.DefaultEffort == "" {
			mi.DefaultEffort = reasonStr("effort") // фолбэк на ключ старой модели, и CN аналог на стороне
		}
		mi.CanDisableThinking, _ = r["canDisableThinking"].(bool)
		if arr, ok := r["supportedEfforts"].([]any); ok {
			for _, item := range arr {
				if s, ok := item.(string); ok {
					if s = strings.TrimSpace(s); s != "" {
						mi.Efforts = append(mi.Efforts, s)
					}
				}
			}
		}
	}
	return mi, true
}
