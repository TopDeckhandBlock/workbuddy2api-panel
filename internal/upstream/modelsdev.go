// modelsdev.go models.dev фолбэк по требованию (context_length уровень в 4-уровневой цепочке поиска 4 уровень).
//
// локализация: только при "отсутствии динамического значения апстрима» + Отсутствует в статической таблице сидов + model.json некешированный» запрос модели
// models.dev，последняя страховка — короткий таймаут (по умолчанию 5s）、Тихий даунгрейд при ошибке (context_length Сброс
// DefaultContextWindow=1M），никогда не блокировать /v1/models Основной путь (поиск асинхронизирован, текущий запрос
// прямой возврат фолбэк-значения, запись после pull model.json для следующего попадания).
//
// источник данных (2026-09-16 Реверс, выводы см. .claude/reports/model-json-dynamic.md）：
// - официальная агрегация JSON Эндпоинт https://models.dev/api.json：~4.7MB Один документ,217 provider、
// Без аутентификации,Cloudflare Хостинг статического сайта;
// - Нет по модели/Нажать provider Под-эндпоинт (/z-ai.json и т.д. 302 возврат /），「реализация "по требованию» — это
// Однократная выгрузка полного набора документов + создать bare id Индекс (загрузка — один раз, далее переиспользование индекса внутри процесса);
// - schema：{ "<provider>": { "models": { "<id>": { "limit": {"context": N,
// "output": N} } } } }，Модель id есть голое имя (glm-5.2）и с пространством имен (openai/gpt-5.5）
// обе формы, индекс по хвостовому сегменту key；
// - много provider Значения с одинаковым именем могут расходиться (агрегирующий шлюз часто сообщает об изменениях):vendor официальный источник (zai/
// moonshotai/openai/google/deepseek/minimax）приоритет, остальное — по моде (консенсус).
package upstream

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"
)

// ModelsDevURL models.dev официальная агрегация JSON Эндпоинт (единственный эндпоинт, см. вывод реверса заголовка файла).
const ModelsDevURL = "https://models.dev/api.json"

// modelsDevTimeout таймаут одиночной выборки: крайний fallback, ждать не стоит (ТЗ §2：Например 5s）。
const modelsDevTimeout = 5 * time.Second

// modelsDevFetchCooldown Троттлинг pull (уровень процесса): документ — полный агрегат,5min внутри не запрашивать повторно
// （Та же модель 24h общее дросселирование вне негативного кэша, защита от частых повторов в коротком окне models.dev）。
const modelsDevFetchCooldown = 5 * time.Minute

// modelsDevNegativeTTL негативный кэш по модели: ненайденная модель 24h внутри без повторного запроса
// （ТЗ §2：Как модель 24h внутри не перепроверять, негативный кэш для ненайденных моделей предотвращает повторные запросы).
const modelsDevNegativeTTL = 24 * time.Hour

// modelsDevNegativesSoftCap Негативный кэш map мягкий лимит: при превышении размера — однократная очистка просроченных записей
// Сканирование. Пакетная амортизация чтобы избежать каждый раз lookup делать всё O(n) полный скан — № 4 Точка срабатывания уровня — в /v1/models внутри
// каждая модель вызывается по одному разу (handler перебор списка моделей поочередно V4），n при большом размере полный скан умножается на число запросов CPU
// Оверхед. Вытеснение только для TTL уже просроченных записей, поэтому ниже лимита без сканирования ни одна валидная запись не просрочится.
//
// Внимание: "сканировать только просроченные записи» недостаточно для гарантии ограниченности:TTL Из**Первый промах**отсчёт (см. lookup обратная запись
// проверка), если имя модели продолжает появляться в /v1/models в списке, один TTL Все записи в окне будут многократно
// Штамповать как fresh，Сканирование не удаляет ни одной записи, размер только растёт. Поэтому при превышении**Жесткий лимит**（двойной soft-лимит)
// дополнительно отбрасывать старейшую запись — негативный кэш чисто оптимизация производительности (miss дополнительно один запрос к внутрипроцессному индексу), потеря записи
// лишь заставит модель пройти ещё одно сканирование, внешняя семантика не меняется.
const modelsDevNegativesSoftCap = 1024

// modelsDevNegativesHardCap Негативный кэш map Жесткий лимит: при превышении отбрасывать старейшие записи по времени
// （См. modelsDevNegativesSoftCap комментарий). Взять двойной soft-лимит, дать "один раунд /v1/models добавлено»
// оставлен запас, при нормальном масштабе далеко не достигается.
const modelsDevNegativesHardCap = 2 * modelsDevNegativesSoftCap

// modelsDevMaxBody Лимит тела ответа при получении (по докам фактич. ~4.7MB，оставить запас; защита от зависания на аномально большом ответе).
const modelsDevMaxBody = 32 << 20

// modelsDevValueMax Верхний лимит проверки значения:context/output превышает 1e9 отклонить как грязные данные
// （Проверка верхнего лимита объема, ТЗ §2 Валидация значения; нижняя граница для положительных чисел — catalog Фолбэк на стороне записи).
const modelsDevValueMax = int64(1e9)

// modelsDevVendorSources Официальный vendor provider список приоритетов:models.dev включение 217 шт.
// provider，агрегирующий шлюз (merge-gateway/nano-gpt и т.д.) заявленное самим limit Часто расходится с официальным источником,
// приоритет выборки значений = попадание в данный список > Консенсус по моде.
var modelsDevVendorSources = map[string]bool{
	"zai": true, // Z.AI（glm официальный семейства)
	"moonshotai": true, // Moonshot AI（kimi официальный Family, международная версия)
	"moonshotai-cn": true, // Moonshot AI Китайская версия
	"openai": true,
	"google": true,
	"deepseek": true,
	"minimax": true,
}

// modelsDevEntry models.dev Результат выборки одной модели (modelsdev json limit подмножество).
type modelsDevEntry struct {
	Context int64
	Output int64
}

// modelsDevFetcher models.dev Ленивый загрузчик: синглтон уровня процесса (переменная уровня пакета modelsDev），
// Троттлинг выборки + Голый id кэш индексов + Негативный кэш той же модели. Для тестов resetModelsDev / независимый base URL
// изоляция инъекции (newModelsDevForTest）。
type modelsDevFetcher struct {
	mu sync.Mutex

	// doc голый после парсинга документа id Индекс (множественный provider объединение одноименных со взятием значения:vendor Приоритет/мода).
	// nil = Не загружено; пусто map（не nil）= Запрошено, но индекс пуст (считается кулдауном неудачи).
	doc map[string]modelsDevEntry

	lastFetch time.Time // Последняя попытка pull (успех и неудача учитываются, троттлинг охлаждения)
	fetched bool // Уже ли вытянуто (doc поле различает успех/неудачу)

	negatives map[string]time.Time // Запрошенная модель не найдена → Время записи (24h негативный кэш)
}

// modelsDev экземпляр пакетного пуллера (синглтон: один индекс документов и состояние троттлинга на весь процесс).
var modelsDev = &modelsDevFetcher{}

// resetModelsDev изоляция тестов: сброс состояния синглтона (doc/fetched/lastFetch/negatives）。
func resetModelsDev() {
	modelsDev.mu.Lock()
	modelsDev.doc = nil
	modelsDev.fetched = false
	modelsDev.lastFetch = time.Time{}
	modelsDev.negatives = nil
	modelsDev.mu.Unlock()
}

// lookup запрос одной модели (context, output, found)：
// - Индекс совпал и значение валидно → found=true；
// - Промах индекса (включая неготовность индекса)→ Записать в negatives（Является и 24h негативный кэш, также
// fetchDoc после успеха backfillMisses список возврата — "ранее miss модель»),found=false。
//
// Только чтение индекса в памяти, без сетевых запросов; сетевые действия выполняет ensureDocAsync（goroutine внутри) отвечает.
// Внимание:doc до готовности miss Также записывать negatives——backfillMisses При рефлоу при обнаружении сразу писать
// model.json и удалить записи негативного кэша (freshLookup），не найденные оставить 24h Негативный кэш.
func (f *modelsDevFetcher) lookup(model string) (modelsDevEntry, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.doc != nil {
		if e, ok := f.doc[model]; ok {
			return e, true
		}
	}
	// Промах (индекс есть, но модели нет, или индекс не готов): логировать miss。
	if f.negatives == nil {
		f.negatives = make(map[string]time.Time)
	}
	now := time.Now()
	// При промахе "повторный запрос» время записи не обновляется:TTL Только из**Первый промах**отсчет с.
	// Иначе перезапись = продление записи при каждом запросе — постоянно присутствующий в /v1/models из списка
	// Неизвестная модель (каждая listing Через ContextWindowListingV4 + MaxOutputTokensListingV4
	// Каждый опрашивается один раз, т.е. на запрос 2 запись обратно) его записи всегда fresh，нижний lazy-eviction ни одной записи не вычистит;
	// т.к. вытеснение сканируется только при превышении мягкого лимита, после превышения размер только растёт (неограниченный рост в пределах жизненного цикла процесса).
	// Продление не влияет на поведение:negativeFresh проверяется только наличие + within TTL，Не истек TTL Записи
	// Независимо от продления одинаково закорачивает 4 триггер уровня.
	if _, seen := f.negatives[model]; !seen {
		f.negatives[model] = now
	}
	// Ленивое удаление просроченных записей негативного кэша:TTL После истечения negativeFresh Изначально считается false（эквивалентно отсутствию),
	// хранение записи далее — лишь утечка памяти —models.dev Никогда не индексируемые имена моделей (model задаётся клиентом произвольно)
	// Проверка один раз — постоянное хранение, единственная точка удаления на всю БД backfillMisses Удалять только модели, найденные в документации,
	// эти записи никогда не будутУтилизация,map Неограниченный рост в течение жизненного цикла процесса.
	// сканировать только при превышении мягкого лимита размера (амортиз. O(1)，см. обоснование в modelsDevNegativesSoftCap комментарий).
	if len(f.negatives) > modelsDevNegativesSoftCap {
		for m, t := range f.negatives {
			if now.Sub(t) >= modelsDevNegativeTTL {
				delete(f.negatives, m)
			}
		}
	}
	// жёсткий лимит как fallback:TTL Неистёкшие записи и так не подлежат вытеснению (предыдущее сканирование уже удалило истёкшие), если размер
	// Если всё ещё выше hard-лимита — значит на входе слишком много имён моделей, отбросить самые старые по времени, урезать до soft-лимита
	// （чистая оптимизация производительности: негативный кэш miss лишь лишний поиск по внутрипроцессному индексу, потеря записи не влияет на семантику).
	if len(f.negatives) > modelsDevNegativesHardCap {
		cut := len(f.negatives) - modelsDevNegativesSoftCap
		oldest := make([]string, 0, len(f.negatives))
		for m := range f.negatives {
			oldest = append(oldest, m)
		}
		sort.Slice(oldest, func(i, j int) bool { return f.negatives[oldest[i]].Before(f.negatives[oldest[j]]) })
		for _, m := range oldest[:cut] {
			delete(f.negatives, m)
		}
	}
	return modelsDevEntry{}, false
}

// negativeFresh находится ли модель в периоде действия негативного кэша (для короткого замыкания цепочки поиска № 4 уровень срабатывания).
func (f *modelsDevFetcher) negativeFresh(model string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	t, ok := f.negatives[model]
	return ok && time.Since(t) < modelsDevNegativeTTL
}

// ensureDocAsync Обеспечить models.dev Индекс документов доступен (асинхронно, без блокировки вызывающей стороны):
// Индекс уже существует / В период cooldown выборки / Уже кто-то тянет (in-flight дедупликация)→ прямой возврат.
// Иначе с goroutine Вытянуть и распарсить, после завершения записать f.doc（тихий фейл: только обновить lastFetch Кулдаун,
// Следующий поиск всё равно идёт через 1M фолбэк, без шторма ретраев).
func (f *modelsDevFetcher) ensureDocAsync(client *http.Client, baseOverride string) {
	f.mu.Lock()
	if f.doc != nil {
		f.mu.Unlock()
		return
	}
	if f.fetched && time.Since(f.lastFetch) < modelsDevFetchCooldown {
		// уже запрашивалось (успешно или с ошибкой) и в период охлаждения: больше не вызывать models.dev。
		f.mu.Unlock()
		return
	}
	// in-flight дедупликация: fetched/lastFetch Сначала выставить в "в процессе на этот раз»,
	// последующие конкурентные вызовы в окне кулдауна возвращаются сразу, без повторного pull.
	f.fetched = true
	f.lastFetch = time.Now()
	f.mu.Unlock()

	go f.fetchDoc(client, baseOverride)
}

// fetchDoc Получить и распарсить models.dev Документация, создать bare id Индекс (goroutine выполняется внутри, никогда не panic
// Проброс вверх: любой сбой — только тихий кулдаун).
// отклонить nil client（Без отката http.DefaultClient）：Продакшен-вызывающая сторона всегда передаёт не- nil，nil означает лишь
// Упущение в тестах —DefaultClient Без таймаута (риск зависания) и бьёт в реальную сеть (загрязнение тестов + неопределённая задержка),
// тишина WARN + Возврат (с fetch Ошибка — та же семантика, даунгрейд 1M fallback) делает пропуски явными.
func (f *modelsDevFetcher) fetchDoc(client *http.Client, baseOverride string) {
	if client == nil {
		log.Printf("WARN: [upstream] models.dev fetch: nil client rejected (no DefaultClient fallback, silent fallback to 1M)")
		return
	}
	url := ModelsDevURL
	if baseOverride != "" {
		url = baseOverride
	}
	ctx, cancel := context.WithTimeout(context.Background(), modelsDevTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		log.Printf("WARN: [upstream] models.dev fetch: build request: %v", err)
		return
	}
	resp, err := client.Do(req)
	if err != nil {
		log.Printf("WARN: [upstream] models.dev fetch failed (silent fallback to 1M): %v", err)
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		log.Printf("WARN: [upstream] models.dev fetch status %d (silent fallback to 1M)", resp.StatusCode)
		return
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, modelsDevMaxBody))
	if err != nil {
		log.Printf("WARN: [upstream] models.dev fetch read: %v", err)
		return
	}
	doc, err := parseModelsDevDoc(raw)
	if err != nil {
		log.Printf("WARN: [upstream] models.dev parse failed (silent fallback to 1M): %v", err)
		return
	}
	f.mu.Lock()
	f.doc = doc
	f.mu.Unlock()
	// После готовности документа "ранее miss возврат модели "...» model.json（№ 4 Уровень → № 3 уровень,
	// Следующий /v1/models прямое попадание в кэш). При отсутствии — хранить негативный кэш.
	f.backfillMisses()
}

// parseModelsDevDoc Парсинг models.dev api.json：{provider:{models:{id:{limit:{context,
// output}}}}} → Голый id Индекс. Одноимённых несколько provider Приоритет источника значения, 4 уровня: официальный vendor Источник
// （modelsDevVendorSources）> Мода голосов > provider Лексикографический порядок > появляется первым.
// № 3 Уровень provider лексикографический порядок детерминирован tie-break：При агрегации поддерживать минимальный из кандидатов provider Имя
// （minProvider，Совм. doc стабильный identity селектора), устранить map вызванный рандомизацией порядка итераций
// 「дрожание значения "один тикет — кто первый, того и» (тот же binary Два fetch одного документа могут записать разные значения в model.json，
// /v1/models context_length невоспроизводимо). Не вводить "лексикографический порядок значений» — это
// 「эвристика "кого выбрать» → "какое значение выбрать», семантика хуже чем provider имя чистое.
func parseModelsDevDoc(raw []byte) (map[string]modelsDevEntry, error) {
	var doc map[string]struct {
		Models map[string]struct {
			Limit *struct {
				Context int64 `json:"context"`
				Output int64 `json:"output"`
			} `json:"limit"`
		} `json:"models"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		return nil, fmt.Errorf("models.dev doc: %w", err)
	}
	// Одноименный id Сбор кандидатов значений:vendorOfficial пометить официальный источник,votes Подсчет моды,
	// minProvider Поддерживать минимум, наблюдавшийся у кандидата provider имя (tie-break исп.).
	type candidate struct {
		entry modelsDevEntry
		vendor bool
		votes int
		aggKey string // Дедупликация и агрегация key（Много одинаковых значений provider только подсчет голосов без дублирующего сохранения)
		minProvider string
	}
	byModel := map[string][]candidate{}
	for provider, pv := range doc {
		for fullID, mv := range pv.Models {
			if mv.Limit == nil {
				continue
			}
			id := fullID
			if i := strings.LastIndex(fullID, "/"); i >= 0 {
				id = fullID[i+1:]
			}
			if id == "" {
				continue
			}
			// Валидация значения (ТЗ §2）：Положительное число + верхний предел масштаба, грязные значения не попадают в индекс.
			ctx, out := mv.Limit.Context, mv.Limit.Output
			if ctx <= 0 || ctx > modelsDevValueMax {
				continue
			}
			if out < 0 || out > modelsDevValueMax {
				continue
			}
			key := fmt.Sprintf("%d/%d", ctx, out)
			cs := byModel[id]
			dup := false
			for i := range cs {
				if cs[i].aggKey == key {
					cs[i].votes++
					if modelsDevVendorSources[provider] {
						cs[i].vendor = true
					}
					if provider < cs[i].minProvider {
						cs[i].minProvider = provider
					}
					dup = true
					break
				}
			}
			if !dup {
				byModel[id] = append(cs, candidate{
					entry: modelsDevEntry{Context: ctx, Output: out},
					vendor: modelsDevVendorSources[provider],
					votes: 1,
					aggKey: key,
					minProvider: provider,
				})
			}
		}
	}
	out := make(map[string]modelsDevEntry, len(byModel))
	for id, cs := range byModel {
		best := 0
		for i, c := range cs {
			// Приоритет: официальный vendor Источник > Мода голосов > provider лексикографический порядок (tie-break детерминированность).
			cur := cs[best]
			better := false
			if c.vendor && !cur.vendor {
				better = true
			} else if c.vendor == cur.vendor && c.votes > cur.votes {
				better = true
			} else if c.vendor == cur.vendor && c.votes == cur.votes && c.minProvider < cur.minProvider {
				better = true
			}
			if better {
				best = i
			}
		}
		out[id] = cs[best].entry
	}
	return out, nil
}
