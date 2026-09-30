// Package upstream Инкапсуляция для CodeBuddy Апстрим (chat / billing / auth）Все HTTP вызов,
// И классификация ошибок (драйвер pool стейт-машина кулдауна).
package upstream

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/logfmt"
)

// ErrKind классификация ошибок,pool На основе этого определяется длительность кулдауна.
type ErrKind int

const (
	ErrNone ErrKind = iota // Успех
	ErrHardCredit // Недостаточно баланса (402 Или body ключевого слова)→ Длительный кулдаун
	ErrSoftRate // 429 мягкий rate limit → короткий кулдаун
	ErrSessionDead // 401 + 12153 offline session Истёк → Отключено
	ErrNotFound // 404 Спорадическая ошибка апстрима → короткое охлаждение, без накопления счётчика ошибок (защита от каскада)
	ErrServer // 5xx Сбой апстрима
	ErrContentBlocked // Блокировка контент-политикой (400 + текст на модерацию)→ Аккаунт не штрафуется, уход в ретрай с понижением
	ErrBadParams // Ошибка парсинга тела запроса (400 + Unmarshal chat params failed / 11101）→ Аккаунт не штрафуется, ротация продолжается
	ErrAccountFault // Авторизация на уровне аккаунта/Сбой квоты (11140 request illegal / 14017 trial not activated）→ ротация с cooldown, без бесконечных ретраев
	ErrModelBlocked // 11102「бэкенд не имеет этой модели»→ (Аккаунт,Модель) обход негативного кэша, переключение модели/Смена аккаунта
	ErrWafBlock // 403 + Небизнесовая оболочка (APISIX WAF Страница перехвата/пустое тело)→ Мягкий кулдаун аккаунта + Бэкофф с джиттером
	ErrPromptTooLong // 11115「prompt is too long」→ ошибка уровня запроса (превышение контекста — проблема запроса, а не аккаунта): без штрафа аккаунта, без ротации, на выходе проброс оригинала
	ErrImageInvalid // формат запроса изображения/Данные недействительны → Ошибка уровня запроса: без штрафа и ротации аккаунта, в конце проброс оригинала
	ErrClient // прочее 4xx / Бизнес-ошибка
)

func (k ErrKind) String() string {
	switch k {
	case ErrHardCredit:
		return "hard_credit"
	case ErrSoftRate:
		return "soft_rate"
	case ErrSessionDead:
		return "session_dead"
	case ErrNotFound:
		return "not_found"
	case ErrServer:
		return "server"
	case ErrContentBlocked:
		return "content_blocked"
	case ErrBadParams:
		return "bad_params"
	case ErrModelBlocked:
		return "model_blocked"
	case ErrWafBlock:
		return "waf_block"
	case ErrPromptTooLong:
		return "prompt_too_long"
	case ErrImageInvalid:
		return "image_invalid"
	case ErrAccountFault:
		return "account_fault"
	case ErrClient:
		return "client"
	default:
		return "none"
	}
}

// Error классифицированная ошибка апстрима.
type Error struct {
	Kind ErrKind
	Status int
	Msg string
	// RetryAfter явно указанное апстримом время ожидания (Retry-After с / retry-after-ms /
	// x-ratelimit-reset парсинг заголовка, см. ParseRetryAfter）。нулевое значение = Апстрим явно не указал,
	// Длительность кулдауна откатывается к значению, вычисленному вызывающей стороной. Точка монтирования в Error конверт:Kind решает "штрафовать или нет»,
	// RetryAfter определяет "на сколько штрафовать», равноправно с ответом upstream.
	RetryAfter time.Duration
}

func (e *Error) Error() string {
	return fmt.Sprintf("upstream %s (http %d): %s", e.Kind, e.Status, e.Msg)
}

// hardMarkers Ключевые слова недостаточного баланса (сравнение в нижнем регистре + сравнение оригинала на китайском, двухканальный).
var hardMarkers = []string{
	"insufficient credit", "no credit", "credit exhausted", "credits exhausted", "out of credit",
	"quota exceeded", "quota exhaust", "payment required", "credit not enough",
	"not enough credit",
	"Недостаточно баллов", "Недостаточно лимита", "Недостаточно средств", "баллы исчерпаны", "лимит исчерпан", "Нет баллов",
}

// softRateMarkers ограничение скорости/Ключевые слова троттлинга (сравнение в нижнем регистре + сравнение оригинала на китайском, двухканальный).
// апстрим при коде состояния не 429 также вернет семантику лимитирования (напр. 200 + code 11140
// "The model provider is rate-limiting requests."、400 + "rate limit"），
// Если такой ответ не распознан, аккаунт ни охлаждается, ни триггерит размыкание, при следующем запросе снова будет выбран (issue #28）。
//
// Словарь по совпадению подстрок, лучше меньше да лучше: включать только явно указывающее на "скорость запросов/формулировка "потребление модели дросселируется».
// Дефисная форма (rate-limiting / rate-limited）требуется отдельная колонка —Contains Не пересекает '-'。
// "too many« будет хит "too many tokens" Такая ошибка параметров клиента стоит мягкого охлаждения аккаунта
// Один SoftCooldown（По умолчанию 60s）самовосстановление после, значительно меньше цены пропуска лимита с повторным выбором того же номера.
var softRateMarkers = []string{
	"rate limit", // rate limit / rate limits / rate limiting
	"rate-limiting",
	"rate-limited",
	"too many requests",
	"too many",
	"usage limit", // usage limit reached / model usage limit exceeded（Троттлинг по использованию, не биллинговый баланс)
	"Слишком частые запросы", "ограничение скорости",
}

var sessionDeadMarkers = []string{"Offline user session not found", "12153"}

// accountFaultMarkers Авторизация на уровне аккаунта/Ключевые слова сбоя квоты (поиск подстроки без учета регистра).
//
// локализация: такого рода ошибка — это**Собственный статус аккаунта**детерминированный локальный сбой, не формат запроса, не временный rate-limit,
// и не ложное срабатывание контента — повторные ретраи лишь триггерят антифрод апстрима/Проверка квоты, необходимо ротировать кулдаун этого аккаунта.
// - "request illegal"（code 11140）→ апстрим auth/auth_forbidden，Риск-контроль авторизации на уровне аккаунта,
// требуется повторно OAuth восстановление только после логина, короткий кулдаун лишь не дает дальше слать впустую.
// - code 14017（"trial not activated" / "The trial version is not yet activated"）→
// апстрим quota/quota_not_activated，register незавершенный триал/неактивированный аккаунт, также на уровне аккаунта.
//
// Внимание 11140 **нельзя**Нажать code проверка: данный code Также несет текст лимита уровня модели ("The model provider
// is rate-limiting requests."），в таком сценарии необходимо сохранять ErrSoftRate（ниже softRateMarkers
// после решения). Поэтому здесь принимаем только msg ключевые слова "request illegal"（auth_forbidden фактический текст).
// 14017 Текст един (без неоднозначности soft-лимита), можно безопасно индексировать.
var accountFaultMarkers = []string{
	"request illegal",
	"trial not activated",
	"trial version is not yet activated",
}

// contentBlockedMarkers ключевые слова блокировки контент-политикой (матчинг подстроки без учёта регистра).
//
// позиционирование: апстрим проверяет по точному посимвольному отпечатку,system Шаблонная фраза источника (напр. Claude Code/Codex
// инжект-инструкция) триггер HTTP 400 + текст ниже. Это "ложное срабатывание» (валидный трафик ошибочно забракован модерацией),
// не проблема аккаунта — баланс аккаунта в норме, без лимита,session Не умер, поэтому ErrContentBlocked
// В applyErrorPolicy не штрафовать аккаунт (без кулдауна/Circuit Breaker/NoteError），переведено на деградирующий ретрай через шлюз.
var contentBlockedMarkers = []string{
	"blocked by security policy",
	"unapproved channel",
	"illegal api invocation",
}

// badParamsMarkers ключевые слова ошибки парсинга тела запроса (issue #41 связка):HTTP 400 + апстрим
// "Unmarshal chat params failed...«（code 11101）。это«отправляемое в upstream body есть проблема"，
// Не связано со здоровьем аккаунта — без штрафа, но с ротацией (commit B）。
var badParamsMarkerMsg = "Unmarshal chat params failed"

// invalidImageMarkers формат запроса изображения/данные недействительны (HTTP 400） **текст**форма. Ошибки такого типа вызваны
// Определяется содержимым запроса, а не проблемой аккаунта: смена аккаунта не изменит то же body результат парсинга. Типичные форматы upstream включают
// `Parse message failed: invalid image_url content`、invalid_image_data、
// `replace the image`。
//
// бизнес-код 11135 Не размещать здесь:code Решение должно толерировать JSON Пусто (`"code": 11135`），
// Литерал marker Покрывает только компактную форму, поэтому единый путь через codeMarker（См. Classify 400 ветка,
// и hint.go isInvalidImageData та же метрика; апстрим 5d5223d Copilot review исправление).
var invalidImageMarkers = []string{
	"invalid image_url content",
	"invalid_image_data",
	"replace the image",
}

// Диагностика: превышение контекста — это**проблема запроса — не проблема аккаунта**——тот же body Отправка с любого аккаунта всё равно
// превышение лимита, и WAF fail-fast Та же философия (ошибки, точно не связанные с аккаунтом, не штрафуют номер и не ротируют, зря тратя здоровые номера
// квота запросов).marker два канала:
// - `"code":11115`：бизнес-конверт code Поле (JSON допуск на пробелы;`«code":«11115"` Строка
// паттерн тоже считается совпадением);
// - "prompt is too long"：msg Текст (без учета регистра).
//
// Только в 400/404/413 определение по статус-коду на уровне запроса (429+11115 Вероятность крайне низкая, приоритет у семантики rate limit,
// 5xx считается ошибкой сервера в приоритете). Цена ложного срабатывания (хорошо body Отнесено к prompt_too_long）：аккаунт не штрафуется +
// без ротации + Проброс оригинала, клиент видит оригинал апстрима и может сам диагностировать, цена контролируема.
var promptTooLongMarkers = []string{
	`"code":11115`,
	`"code": 11115`,
	`"code":"11115"`,
	"prompt is too long",
}

// isPromptTooLongStatus 11115 только на уровне запроса 4xx проверка выше (см. promptTooLongMarkers комментарий).
func isPromptTooLongStatus(status int) bool {
	return status == http.StatusBadRequest || status == http.StatusNotFound ||
		status == http.StatusRequestEntityTooLarge
}

// alreadyCheckinMarkers "Сегодня уже отмечено"ключевое слово (upstream при повторном check-in возвращает code!=0，
// На практике code=10001/14001 "Сегодня уже отмечено«/«Сегодня уже отмечено"）。Только для *Error.Msg делать contains-совпадение,
// Сетевой уровень/Ошибки уровня парсинга здесь не распознаются (см. IsAlreadyCheckin）。
var alreadyCheckinMarkers = []string{"今日已签到", "Уже отмечено", "already"}
var badParamsMarkerCode = `"code":11101`

// softRateResetLoc апстрим 429 6004 Время сброса в тексте фиксированно по UTC+8 Пояснение (текст апстрима такой,
// не зависит от часового пояса контейнера).
var softRateResetLoc = time.FixedZone("UTC+8", 8*60*60)

// SoftRateResetLoc фиксированный часовой пояс времени сброса (для конструирования теста/assert единая метрика часового пояса).
func SoftRateResetLoc() *time.Location { return softRateResetLoc }

// modelRateLimitCode явно указывает на "уровень модели 429 бизнес-логика "rate limit» code。
// Апстрим использует это для выражения"Превышен лимит использования этой модели"（code 6004，msg содержит "будет через … сброс»),
// а не весь аккаунт в rate limit — аккаунт здоров, ограничена только эта модель (issue #31）。
const modelRateLimitCode = "6004"

// softRateResetPatternCN/EN Текст сброса при совпадении (CN「Будет … сброс»/ global Английское имя домена
// "reset at <время в фиксированном формате>"），захватить временную строку посередине.
const softRateResetPatternCN = `将在 (.+?) 重置`
const softRateResetPatternEN = `(?i)reset at (\d{4}-\d{2}-\d{2} \d{2}:\d{2}:\d{2})`

// Регулярка для rate limit прекомпилируется на уровне пакета var：IsModelRateLimit / ParseRateReset При каждой ошибке
// классификация, каждый лимит body вызов на, внутри тела функции MustCompile — чистые потери; шторм ошибок (429
// особенно при флуде). Шаблоны — только константы.regexp потокобезопасно (сопоставление только для чтения), доп. блокировка не требуется.
var (
	reModelRateLimit = regexp.MustCompile(`"code"\s*:\s*"?` + modelRateLimitCode + `"?`)
	reSoftRateResetCN = regexp.MustCompile(softRateResetPatternCN)
	reSoftRateResetEN = regexp.MustCompile(softRateResetPatternEN)
)

// softRateTimeLayout Формат времени сброса апстрима (без суффикса часового пояса; часовой пояс фиксирован UTC+8）。
const softRateTimeLayout = "2006-01-02 15:04:05"

// IsModelRateLimit отчет 429 body указывает ли явно на лимит уровня модели (бизнес code 6004）。
// Для различения"Soft-лимит уровня аккаунта«（охлаждение по аккаунту) и«лимит использования на уровне модели"（смена модели — сразу доступно).
func IsModelRateLimit(body string) bool {
	// `"code":6004` / `«code": 6004` / `«code":«6004"` оба могут сработать (JSON допуск по пробелам).
	return reModelRateLimit.MatchString(body)
}

// modelBlockCode бизнес-ошибка, явно указывающая "у данного бэкенда нет такой модели» code。
const modelBlockCode = "11102"

// modelBlockMsgMarker 11102 Детерминированный текст ответа (официальный error message фиксированная фраза).
// Принимать только эту узкую фразу, не принимать "model ... not found" Широкий regex — последний заденет
// not found Формулировка.
const modelBlockMsgMarker = "service info not found"

// ModelBlockReason 11102 Запись negative cache в pool.modelCooldowns внутри reason Префикс.
// handler Запись BlockModelBackoff；pool.BlockModelClear Нажать "11102" Запись идентификации по префиксу
// （и 6004 элемента "6004 model rate limit" reason не мешают друг другу).
const ModelBlockReason = "11102 model not available"

// IsModelBlocked отчет body является ли "у данного бэкенда нет этой модели»(11102) детерминированный ответ.
//
// Сравнивать только code/msg как отдельные поля, никогда не делать подстроковый поиск по всему тексту: тело ошибки также содержит requestId и т.п. поля,
// Матчинг по всему тексту приведет к тому, что "11102" попадание в ID иначе ошибочно обойти доступную модель. Решение =
// code поле строго равно "11102«，Или msg/message Поле совпало с узкой фразой "service info not
// found"（истинно при срабатывании любого из двух). Смотреть только 400/404：429 Лента 11102 относится к семантике лимитирования.
// Обход полей покрывает верхний уровень и error Вложенный объект — два уровня.
func IsModelBlocked(status int, body string) bool {
	if (status != http.StatusBadRequest && status != http.StatusNotFound) || body == "" {
		return false
	}
	// Лёгкая предпроверка:body нет ни "11102« и нет marker при этом сразу закорачивается (большинство 4xx возврат без аллокаций).
	if !strings.Contains(body, modelBlockCode) && !strings.Contains(strings.ToLower(body), modelBlockMsgMarker) {
		return false
	}
	var root map[string]any
	if err := json.Unmarshal([]byte(body), &root); err != nil {
		return false
	}
	nodes := []map[string]any{root}
	if inner, ok := root["error"].(map[string]any); ok {
		nodes = append(nodes, inner)
	}
	code, msg := "", ""
	for _, node := range nodes {
		for _, key := range []string{"code", "errCode", "error_code"} {
			if v, ok := node[key]; ok && v != nil && code == "" {
				code = strings.TrimSpace(fmt.Sprint(v))
			}
		}
		for _, key := range []string{"msg", "message"} {
			if v, ok := node[key].(string); ok && v != "" && msg == "" {
				msg = strings.TrimSpace(v)
			}
		}
	}
	if code == modelBlockCode {
		return true
	}
	return strings.Contains(strings.ToLower(msg), modelBlockMsgMarker)
}

// hasBusinessCode reports whether a JSON error envelope contains an exact
// business code in a field named "code«. Upstream envelopes vary between
// top-level and nested error/data objects, so walk the decoded structure.
func hasBusinessCode(body, want string) bool {
	var root any
	if err := json.Unmarshal([]byte(body), &root); err != nil {
		return false
	}
	var walk func(any) bool
	walk = func(value any) bool {
		switch node := value.(type) {
		case map[string]any:
			if code, ok := node["code"]; ok && strings.TrimSpace(fmt.Sprint(code)) == want {
				return true
			}
			for _, child := range node {
				if walk(child) {
					return true
				}
			}
		case []any:
			for _, child := range node {
				if walk(child) {
					return true
				}
			}
		}
		return false
	}
	return walk(root)
}

// hasBusinessEnvelope Сообщить об ошибке body наличие формы бизнес-конверта апстрима (JSON И содержит
// `"code«:` Или `«msg»:` поле).WAF 403 оценка (IsWafBlocked）Использовать "пустой бизнес-конверт»
// Различение APISIX WAF Страница блокировки (HTML/Пустое тело/plain text) и бизнес-слоем апстрима 403（Лента code/msg
// конверт, идёт по существующей классификации).JSON парсинг не делается: для наличия конверта достаточно совпадения имени поля —
// битый JSON но содержит `"msg":` строка по-прежнему обрабатывается консервативно как бизнес-ответ (лучше пропустить WAF и без ложного штрафа
// Бизнес 403，последние имеют собственную авторитетную классификацию).
func hasBusinessEnvelope(body string) bool {
	return strings.Contains(body, `"code":`) || strings.Contains(body, `"msg":`)
}

// IsWafBlocked отчет 403 Является ли ответ WAF форма перехвата:HTTP 403 И body Без бизнес-обёртки
// （отсутствует `"code":`/`«msg":` JSON Поле —HTML Страница блокировки, пустое тело, plain text — все считается срабатыванием).
// с бизнес-конвертом 403（11140 request illegal / 11128 и т.д.) всё равно идёт по существующей цепочке классификации.
// 403 Содержит accountFault сохранение текущего текста (ErrAccountFault），От Classify гарантия порядка правил.
func IsWafBlocked(status int, body string) bool {
	return status == http.StatusForbidden && !hasBusinessEnvelope(body)
}

// retryAfterHeaderCandidates Приоритетная последовательность заголовков ответа для парсинга длительности кулдауна:
// retry-after（с,RFC 7231）/ retry-after-ms（мс)/ x-ratelimit-reset
// （epoch секунды или миллисекунды, брать now+ остаток). Без учёта регистра (http.Header.Get уже нормализовано).
var retryAfterHeaderCandidates = []string{"Retry-After", "Retry-After-Ms", "X-Ratelimit-Reset"}

// retryAfterSanity верхний предел результата парсинга (превышение считается аномалией апстрима и отбрасывается, откат к локальному расчёту),
// и pool softRateMax По умолчанию 2h Тот же порядок.
const retryAfterSanity = 2 * time.Hour

// ParseRetryAfter из лимитера/перехват заголовков ответа, парсинг явно указанного апстримом времени ожидания:
// последовательно пробовать Retry-After（целых секунд)→ retry-after-ms（целых мс)→
// x-ratelimit-reset（чисто числовой по epoch с/вывод в миллисекундах;HTTP-Date Форма не поддерживается —
// на практике апстрим шлет число). Отсутствие любого заголовка/Недопустимый/неположительный/При превышении лимита пробовать следующий хед;
// если всё недоступно — вернуть false（вызывающая сторона откатывается к имеющемуся расчётному значению, ни в коем случае не выдумывать время ожидания).
func ParseRetryAfter(h http.Header) (time.Duration, bool) {
	for _, name := range retryAfterHeaderCandidates {
		v := strings.TrimSpace(h.Get(name))
		if v == "" {
			continue
		}
		if !isAllDigits(v) {
			continue // Не только цифры (напр. HTTP-Date）не парсить, лучше пропустить
		}
		n, ok := parseRetryNumber(v, name)
		if !ok {
			continue
		}
		if n <= 0 || n > retryAfterSanity {
			continue // неположительный/Аномально большое: отбросить (фолбэк на локальный расчет)
		}
		return n, true
	}
	return 0, false
}

// isAllDigits отчет s Является ли чисто числовым (предв. быстрый скрининг, без strconv после — проверка семантики).
func isAllDigits(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

// parseRetryNumber Пересчитать чисто числовую строку в длительность по метрике первого места.x-ratelimit-reset Да
// epoch Момент времени, а не длительность: в секундах (10 бит) и миллисекундная размерность (13 бит) всё по "now+ в этот момент
// пересчет по "остатку», если в прошлом — недоступно. Недостаточно разрядов (8 бит и ниже) невозможно определить epoch
// Семантическое отбрасывание (лучше недобрать: короткие строки чаще ошибочные заголовки вроде порядковых номеров).
func parseRetryNumber(v, headerName string) (time.Duration, bool) {
	// Верхний лимит 16 битовая защита int64 Переполнение (превышение epoch реалистичный порядок миллисекунд заведомо невалиден).
	if len(v) > 16 {
		return 0, false
	}
	var n int64
	for _, r := range v {
		n = n*10 + int64(r-'0')
	}
	switch headerName {
	case "Retry-After":
		// Сначала проверка лимита, затем умножение time.Second：16 -значное число × 1e9 будет переполнение int64 заворачивается в
		// малое положительное число, далее через вызов retryAfterSanity валидация считается валидным временем ожидания.
		if n > int64(retryAfterSanity/time.Second) {
			return 0, false
		}
		return time.Duration(n) * time.Second, true
	case "Retry-After-Ms":
		if n > int64(retryAfterSanity/time.Millisecond) {
			return 0, false
		}
		return time.Duration(n) * time.Millisecond, true
	default: // X-Ratelimit-Reset：epoch → остаток
		sec := n
		if len(v) >= 12 { // В миллисекундах (13 бит);11 Граница по секундам (цена ложного срабатывания — лишний подсчёт 1000 раз)
			sec = n / 1000
		}
		remain := time.Until(time.Unix(sec, 0))
		return remain, true
	}
}

// ParseRateReset из любого ответа rate-limit body внутри единый парсинг "через … время "сброса» (апстрим UTC+8 текст).
// При успехе вернуть распарсенное**Время по wall-clock**（Нажать UTC+8 пояснение), при ошибке вернуть ноль + false。
//
// идти ли через exemption на уровне модели, выравнивание даты/времени на until или modelCooldowns，Стороной решения о кулдауне (pool）Нажать
// IsModelRateLimit решение: эта функция отвечает только за "извлечение явно указанного апстримом момента восстановления». Текста со временем нет
// лимит также возвращается вызывающему как ограниченный бэкофф (время не выдумывать).
func ParseRateReset(body string) (time.Time, bool) {
	// CN Приоритет текста;global Домен 429 body в английской форме ("will reset at YYYY-MM-DD HH:MM:SS
	// UTC+8"），ранее распознавался только китайский → global Не удалось распарсить момент восстановления при лимитировании, откат к ограниченному базовому бэкоффу с повтором
	// удвоение (испр. "global удвоение экспоненты кулдауна домена"). англ. regex якорит время фиксированного формата, естественный язык
	// （"reset at the end of the day"）Несовпадение.
	m := reSoftRateResetCN.FindStringSubmatch(body)
	if len(m) < 2 {
		m = reSoftRateResetEN.FindStringSubmatch(body)
	}
	if len(m) < 2 {
		return time.Time{}, false
	}
	ts := strings.TrimSpace(m[1])
	ts = strings.TrimSuffix(ts, " UTC+8") // убрать суффикс, фиксированно по softRateResetLoc Пояснение
	t, err := time.ParseInLocation(softRateTimeLayout, ts, softRateResetLoc)
	if err != nil {
		return time.Time{}, false
	}
	return t, true
}

// Classify Нажать HTTP Код состояния + body определить категорию ошибки.
//
// Порядок проверки от "строгого» к "широкому», порядок каждого уровня семантически обоснован:
// 0. 11102（IsModelBlocked）——「«У этого бэкенда нет такой модели» — детерминированный ответ, семантически наиболее конкретный, проверяется первым
// （Принимается только 400/404，429+11102 семантика лимитирования — по п. 4 уровень).
// 1. 402 —— Истинный код исчерпания биллингового баланса — самый строгий, несамовосстанавливающийся, проверяется первым.
// 2. sessionDeadMarkers —— терминальное состояние, требующее ручного перелогина. Если 401 body одновременно содержит "12153" и
// "rate limit"（например примесь страниц ошибок шлюза), отнести к session_dead：Короткий кулдаун не спасает от фейла session，
// Ложная классификация как лимит оставит мертвый аккаунт в пуле с повторным выбором; и этот слой marker — точный термин (12153 и т.д.),
// Более специфично, чем широкая подстрока уровня rate-limit, специфичное приоритетнее общего.
// 3. accountFaultMarkers —— Авторизация на уровне аккаунта/Сбой квоты (11140 request illegal auth риск-контроль,
// 14017 trial not activated register не завершено). Должен быть до status==429 Критерий:
// 14017 часто содержит 429 Код статуса, если попадает в status==429 будет ошибочно отнесено к soft_rate（"ограничение скорости"Несоответствие семантики:
// троттлинг самовосстанавливается экспоненциальным бэкоффом и т.п., сбои уровня аккаунта и т.д. не приходят).11140 model вариант лимитирования уровня
// （rate-limiting текст) из-за marker Без этого текста автоматически попадает в softRateMarkers слой,
// не затрагивается.
// 4. 429 + code 14018 —— явное исчерпание баллов аккаунта, относится к ErrHardCredit（issue #175）。
// учитывать только структурированный бизнес-код, без опоры на возможный кросс-биллинг/догадки по текстам на границах лимитов.
// 5. status==429 —— Фолбэк по статус-коду rate limit (перед hardMarkers）：429 body частая передача
// "quota exceeded«/«Недостаточно лимита" и т.п. кросс-биллинг/формулировка двух границ лимита,hardMarkers предварительная проверка превратит
// ошибочное отнесение к rate limit ErrHardCredit Жёсткое охлаждение до следующего дня 04:00，впустую потеряно аккаунтов около 12h。Статус-код приоритетнее ключевых слов
// Более авторитетный сигнал; реальное исчерпание баланса определяется 402（№ 1 уровень) или 14018（№ 4 слой) перехват,
// не 429 кода статуса quota формулировка — см. ниже hardMarkers（№ 6 уровень).
// 6. hardMarkers —— не 429 Ответ содержит ключевые слова биллинга (200 бизнес-конверт / 403 конверт и т.д.).
// 7. softRateMarkers —— не 429 статус-код несет текст лимитирования (issue #28 точка исправления).
// Находясь здесь, может переопределить 200/400/403/5xx все статус-коды.
// 8. 11115 —— 「prompt is too long」Семантика на уровне запроса: проверка в 404/5xx С общим 4xx Фолбэк
// до (404 пометка выше 11115 если попадет ErrNotFound приведёт к ошибочному охлаждению аккаунта — превышение контекста не связано с аккаунтом).
// 9. 404 / 5xx —— Обычная классификация, не связанная с ограничением частоты.
// 10. IsWafBlocked —— 403 и без бизнес-конверта (HTML Страница перехвата/Пустое тело/чистый текст):APISIX WAF
// форма перехвата. Проверка в общем 4xx Фолбэк**До**：Ранее эта форма попадала ErrClient → Только смена аккаунта без штрафа →
// Каскадный 403。с бизнес-конвертом 403 уже перехвачено верхними слоями, до этого слоя не доходит.
// 11. Стратегия контента/ошибка параметра/прочее 4xx —— Универсальный фолбэк.
func Classify(status int, body string) ErrKind {
	// 11102「"Модель отсутствует на данном бэкенде« проверять первым: это детерминированный ответ "модель не существует на бэкенде", семантика выше
	// Биллинг/Лимит более специфичен — если не проверить сначала,msg внутри "service info not found" Будет перекрыто более широким
	// 4xx Фолбэк отнести к ErrClient（только смена номера без уклонения), этот битый номер останется в пуле и будет выбираться повторно.
	// Принимается только 400/404（См. IsModelBlocked），429+11102 внизу status==429 Уровень использует семантику лимитирования.
	if IsModelBlocked(status, body) {
		return ErrModelBlocked
	}
	// 402：Истинный код исчерпания биллингового баланса — самый строгий, несамовосстанавливающийся, проверяется первым.
	if status == http.StatusPaymentRequired {
		return ErrHardCredit
	}
	lower := strings.ToLower(body)
	// sessionDead / accountFault До status==429：Терминальные состояния уровня аккаунта и т.п. не самовосстанавливаются, коды rate limit
	// нельзя их маскировать (429+14017 Обязательно accountFault，401+12153 микширование "rate limit" Обязательно
	// sessionDead——этот слой marker — точное слово, конкретнее широкой подстроки уровня rate-limit, конкретное приоритетнее общего).
	for _, m := range sessionDeadMarkers {
		if strings.Contains(body, m) {
			return ErrSessionDead
		}
	}
	for _, m := range accountFaultMarkers {
		if strings.Contains(lower, strings.ToLower(m)) || strings.Contains(body, m) {
			return ErrAccountFault
		}
	}
	// 14018 Это явный бизнес-код исчерпания баллов аккаунта. Должен проверяться до общего 429 фолбэк, иначе будет ошибочно принято за
	// Самовосстанавливающийся soft-лимит с fallback-выбором при охлаждении всего пула (issue #175）。только по структурированным code
	// Проверка; без этого code "credits exhausted" Текст остаётся обычным 429 семантика мягкого rate limit.
	if status == http.StatusTooManyRequests && hasBusinessCode(body, "14018") {
		return ErrHardCredit
	}
	// status==429 До hardMarkers：Ответ rate limit body частая передача "quota exceeded"/
	// "Недостаточно лимита" и т.п. кросс-биллинг/формулировка двух границ лимита,hardMarkers предварительная проверка ошибочно отнесет rate limit к
	// ErrHardCredit Жёсткое охлаждение до следующего дня 04:00，впустую потеряно аккаунтов около 12h。статус-код авторитетнее ключевых слов
	// Сигнал: раз апстрим отдал 429，обрабатывать по семантике rate limit (лучше короткий cooldown с самовосстановлением, чем долгий с отбраковкой аккаунта);
	// реальное исчерпание баланса от 402（верхний уровень) или 14018（верхний уровень) перехватывает, не 429 кода статуса quota
	// формулировка — см. ниже hardMarkers（историческая семантика неизменна).
	if status == http.StatusTooManyRequests {
		return ErrSoftRate
	}
	for _, m := range hardMarkers {
		if strings.Contains(lower, strings.ToLower(m)) || strings.Contains(body, m) {
			return ErrHardCredit
		}
	}
	for _, m := range softRateMarkers {
		if strings.Contains(lower, strings.ToLower(m)) || strings.Contains(body, m) {
			return ErrSoftRate
		}
	}
	// 11115「prompt is too long」：считать находящимся в 404/5xx/WAF/Стратегия контента/ошибка параметра/общий 4xx
	// ранее — семантика уровня запроса наиболее специфична (превышение контекста), должна предшествовать общему fallback по статус-коду (404 фолбэк будет
	// Ошибочное отнесение ErrNotFound только кулдаун без проксирования;ErrClient только смена аккаунта, трата квоты здоровых аккаунтов).
	if isPromptTooLongStatus(status) {
		for _, m := range promptTooLongMarkers {
			if strings.Contains(body, m) || (m != strings.ToLower(m) && strings.Contains(lower, strings.ToLower(m))) {
				return ErrPromptTooLong
			}
		}
	}
	if status == http.StatusNotFound {
		return ErrNotFound
	}
	if status >= 500 {
		return ErrServer
	}
	// WAF 403（форма блокировки без бизнес-конверта): относить к контент-политике/ошибка параметра/общий 4xx до —
	// эти слои принимают только с текстом body，WAF Пустое тело/HTML Никогда не попадут в них marker，
	// но попадает ErrClient цена фолбэка — "только смена аккаунта без штрафа» (серия 403 корневая причина), необходимо развести до fallback.
	// С конвертом 403 на вышестоящих уровнях уже есть авторитетная классификация, не затрагивается.
	if IsWafBlocked(status, body) {
		return ErrWafBlock
	}
	// формат изображения/Ошибка данных — детерминированная ошибка уровня запроса: тот же body Смена аккаунта не меняет результат, сразу
	// fail-fast，чтобы не перебирать все здоровые аккаунты и всё равно в итоге 503 Вернуть клиенту.
	// 11135 Бизнес-код идёт через codeMarker（JSON допуск по пробелам), текст через invalidImageMarkers。
	if status == http.StatusBadRequest && codeMarker(lower, "11135") {
		return ErrImageInvalid
	}
	if status == http.StatusBadRequest {
		for _, m := range invalidImageMarkers {
			if strings.Contains(lower, m) {
				return ErrImageInvalid
			}
		}
	}
	// Блокировка контент-политикой (HTTP 400 + текст на модерацию): считается в общем ErrClient ранее.
	// Это ложный сигнал, аккаунт не штрафуется, обрабатывается деградацией и ретраем шлюза (см. handler.applyErrorPolicy）。
	if status >= 400 {
		for _, m := range contentBlockedMarkers {
			if strings.Contains(lower, m) {
				return ErrContentBlocked
			}
		}
		// Ошибка парсинга тела запроса (HTTP 400 + Unmarshal chat params failed / code 11101）：
		// это"отправляемое в upstream body есть проблема"。Усечение на стороне шлюза уже 413 Устранить (issue #41 commit A），
		// оставшийся источник — клиент JSON Сам по себе некорректен — смена аккаунта не поможет 400，не следует штрафовать номер (зря охлаждать хороший номер).
		// Возврат ErrBadParams：Без охлаждения/БезОтключение/ошибка не засчитывается, но**все равно ротируется**（У разных аккаунтов могут быть разные
		// права модели, стоит повторить попытку).
		if strings.Contains(body, badParamsMarkerMsg) || strings.Contains(body, badParamsMarkerCode) {
			return ErrBadParams
		}
		return ErrClient
	}
	// HTTP 200 Но бизнес code не 0 случаи с ключевыми словами баланса уже обработаны выше hardMarkers перехват.
	return ErrNone
}

// apiEnvelope Унифицированный конверт upstream.
type apiEnvelope struct {
	Code int `json:"code"`
	Msg string `json:"msg"`
	Data json.RawMessage `json:"data"`
}

// Client апстрим HTTP Клиент.Base Поле переопределяемо для тестов.
type Client struct {
	HTTP *http.Client

	// ChatHTTP Чат SSE выделенный client：Без общего лимита времени (Timeout=0），первый байт от
	// Transport.ResponseHeaderTimeout ограничение, простой в стриме — IdleTimeout ограничение.
	// и HTTP совместное использование одного *http.Transport Экземпляр, пул соединений не дублируется.
	ChatHTTP *http.Client

	// HeaderTimeout Чат SSE Таймаут до первого байта (заголовки ответа);<=0 означает не задано (фолбэк HTTP.Timeout）。
	HeaderTimeout time.Duration
	// IdleTimeout Чат SSE тайм-аут простоя в потоке;<=0 Означает отключение мониторинга простоя.
	IdleTimeout time.Duration

	// effortsMu/efforts Кэшировать каждую модель supportedEfforts（FetchModels обновление), для тела запроса effort фолбэк.
	// Нажать realm Иерархические бакеты (cn/global）：кросс-доменное зондирование одного имени модели effort коллекции могут различаться,
	// Смешивание бакетов приводит к загрязнению (C-2）。
	effortsMu sync.RWMutex
	efforts map[string]map[string][]string
	// defaultEfforts Кэшировать каждую модель reasoning.defaultEffort（FetchModels обновления), для
	// thinking.go бэкфилл: отсутствует явный effort приоритет — дефолтный уровень из объявления модели, пустая строка — fallback на хардкод high。
	// и efforts Совм. realm Иерархические бакеты (одно C-2 принцип изоляции), совместное использование effortsMu。
	defaultEfforts map[string]map[string]string
	// modelRates кеш текущего действующего множителя баллов по моделям (нормализованное значение, напр. "0.5"）。
	// и efforts общий realm Слои и блокировки; при каждом успешном обновлении каталога моделей соответствующая область заменяется целиком.
	modelRates map[string]map[string]string

	// globalModels Кеш global Результат зондирования каталога имен моделей (успех ∩ Статический overlay；
	// 1h TTL + 5min негативный кэш), см. global_models.go。Хранится на инстанс, тест создает новый Client т.е. изоляция.
	globalModels fetchGlobalModelsCache

	// SanitizeFingerprints переключатель десенсибилизации отпечатка блэклиста тела исходящего запроса (по умолчанию true；false полное восстановление).
	// горячее изменение конфига сохранением в панели + chat конкурентное чтение/запись на горячем пути, использовать atomic.Bool Устранение гонки данных.
	SanitizeFingerprints atomic.Bool

	// UserAgent исходящий User-Agent Явное перекрытие (при непустом значении действует полный путь, приоритет над трёхсегментным дефолтом).
	// пустой = официальная форма по умолчанию:chat/refresh/FetchModels Ход
	// `WorkBuddy/<ver> WorkBuddy/<ver> CLI/<cliVer>`；billing Ход `WorkBuddy/<ver>`
	// （только если client_name непусто).
	UserAgent string

	// ClientVersion WorkBuddy Сегмент версии клиента (исходящий UA `WorkBuddy/<ver>` + X-IDE-Version）。
	// пустой = Встроено по умолчанию (выравнивание с официальным 5.5.4 дистрибутив).
	ClientVersion string

	// CliVersion исходящий UA Средний `CLI/<ver>` версия сегмента. Пусто = встроенный дефолт (официальный встроенный CLI 2.137.1）。
	CliVersion string

	// ClientName значение заголовка принадлежности расхода (X-Product / X-IDE-Name / X-IDE-Type / X-IDE-Version）。
	// пустой = Старое поведение:X-Product="SaaS"，Не задано X-IDE-*（обратная совместимость, без скачкообразного изменения атрибуции).
	ClientName string

	// PassthroughIP прозрачно прокидывать клиента IP апстриму (X-Forwarded-For/X-Real-IP первый сегмент).
	// по умолчанию false（граница безопасности reverse proxy);handler В chat Путь по запросу clientIP Входящий ChatStream。
	PassthroughIP bool

	// DeviceToken Риск-контроль устройства Token（X-Device-Token заголовок) глобальный fallback-источник:config upstream.device_token。
	// приоритет парсинга:auth.Auth.DeviceToken > DeviceToken（config）> DeviceTokenFile（файл).
	DeviceToken string

	// DeviceTokenFile Устройство token Фолбэк пути к файлу (десктоп хоста с сохранением на диск token，5 минут читать кэш).
	DeviceTokenFile string

	ChatBaseCN string
	BillingBaseCN string
	// WebBaseCN Официальный сайт (workbuddy.cn）Домен: часть интерфейсов типа "получение награды за задание» доступна только в этом домене
	// （Web Используется центром роста;CLI Домен copilot.tencent.com возврат по одноимённому пути 400）。
	WebBaseCN string

	// ChatBaseGlobal / BillingBaseGlobal Международная версия (global realm）апстрим base。
	// пустой = дефолт по умолчанию https://www.workbuddy.ai（D5）。
	ChatBaseGlobal string
	BillingBaseGlobal string

	// GlobalEnabled включено ли global realm Маршрутизация (config global.enabled，по умолчанию true）。
	// false даже если пользователь в это время auth записано realm=global также**Не**Маршрутизировать в global base——
	// chatBase/billingBase вернуть CN base，Путь также идет через CN（двойная страховка, с auth.Realm() коррелирует с переключателем-шлюзом).
	GlobalEnabled bool
}

// New Дефолт для продакшена.Transport От newTransport() Централизованное конструирование (усиление слоя соединения: реально запретить h2 /
// TLS Таймаут хендшейка / Короткий keepalive детект / при ошибке очистить пул, параметры см. transport.go——Поглотить апстрим
// kongjianguan 4 опыт факт-теста серийных попаданий).
func New() *Client {
	tr := newTransport()
	c := &Client{
		HTTP: &http.Client{Timeout: 120 * time.Second, Transport: tr},
		ChatHTTP: &http.Client{Timeout: 0, Transport: tr}, // Без общей длительности; первый байт от ResponseHeaderTimeout Управление
		ChatBaseCN: "https://copilot.tencent.com",
		BillingBaseCN: "https://www.codebuddy.cn",
		WebBaseCN: "https://www.workbuddy.cn",
		// GlobalEnabled по умолчанию true（и config global.enabled по умолчанию true совпадает; чисто CN Поведение деплоя без изменений:
		// CN аккаунт всегда считается cn，global base Только в realm=global использовано на аккаунте).
		GlobalEnabled: true,
	}
	c.SanitizeFingerprints.Store(true)
	return c
}

// chatHTTP Возвращать только для чата client；не задано (напр., в тесте инжектируется только HTTP）откат при HTTP。
func (c *Client) chatHTTP() *http.Client {
	if c.ChatHTTP != nil {
		return c.ChatHTTP
	}
	return c.HTTP
}

// defaultGlobalBase по умолчанию global base（D5：config по умолчанию если не покрыто workbuddy.ai）。
const defaultGlobalBase = "https://www.workbuddy.ai"

// globalChatBase действующий global chat base：Client.ChatBaseGlobal если не пусто — брать, иначе дефолт.
func (c *Client) globalChatBase() string {
	if c.ChatBaseGlobal != "" {
		return c.ChatBaseGlobal
	}
	return defaultGlobalBase
}

// globalBillingBase действующий global billing base：Client.BillingBaseGlobal если не пусто — брать, иначе дефолт.
func (c *Client) globalBillingBase() string {
	if c.BillingBaseGlobal != "" {
		return c.BillingBaseGlobal
	}
	return defaultGlobalBase
}

// globalOn Сообщить, маршрутизируется ли аккаунт на global Апстрим:GlobalEnabled Включено и аккаунт Realm()==global。
// Двойная страховка:config переключатель — первый шлюз (со стороны апстрима),auth.Realm() шлюз-переключатель — второй рубеж (сторона аккаунта).
func (c *Client) globalOn(a *auth.Auth) bool {
	return c.GlobalEnabled && a != nil && a.Realm() == "global"
}

// константы путей:CN и global общий chat Исходящий путь (/v2 один путь).
const chatCompletionsPath = "/v2/chat/completions"

// chatPaths Возврат по realm chat последовательность кандидатов пути:
// global → [/v2]（#119 Фиксированный одиночный путь:/console Размещено на Tencent Cloud WAF body правила содержимого, обратные кавычки
// printf/whoami детерминированность признаков выполнения команд типа 403；/v2 Совм. base правило не применяется, фактически эквивалентный эндпоинт.
// известный компромисс: если апстрим в будущем закроет /v2，global chat полностью недоступен — тогда следует повторно включить /console
// Путь, данный комментарий —"сломается — тогда чинить"якорь);cn → [/v2]（один элемент, текущее состояние).
func (c *Client) chatPaths(a *auth.Auth) []string {
	return []string{chatCompletionsPath}
}

// billing Путь к доменному эндпоинту (billingBase + path）。balance/checkin и report（report.go）Тот же домен,
// Единый маршрут через billingJSON отправляет запрос.
const (
	billingMeterPath = "/billing/meter/get-user-resource" // global Приоритетный (в международной версии отсутствует /v2 префикс)
	dailyCheckinPath = "/billing/meter/daily-checkin" // global Приоритетный
	billingMeterPathV2 = "/v2/billing/meter/get-user-resource" // CN Текущее состояние / global fallback
	dailyCheckinPathV2 = "/v2/billing/meter/daily-checkin"
)

// billingMeterPaths Нажать realm вернуть billing/meter последовательность кандидатов пути домена:
// global → [отсутствует /v2, Есть /v2]（404 Время fallback）；cn → [Есть /v2]（текущее состояние дословно, ноль регрессий).
// действует только на get-user-resource / daily-checkin（/billing/meter/* семейство);report /v2/report Не участвует,
// прочее billing эндпоинт (growth и т.д.) путь не содержит /billing/meter префикс, исходная константа не затрагивается.
func (c *Client) billingMeterPaths(a *auth.Auth) []string {
	if c.globalOn(a) {
		return []string{billingMeterPath, billingMeterPathV2}
	}
	return []string{billingMeterPathV2}
}

// checkinMeterPaths Аналогично, для daily-checkin。
func (c *Client) checkinMeterPaths(a *auth.Auth) []string {
	if c.globalOn(a) {
		return []string{dailyCheckinPath, dailyCheckinPathV2}
	}
	return []string{dailyCheckinPathV2}
}

func (c *Client) chatBase(a *auth.Auth) string {
	if c.globalOn(a) {
		return c.globalChatBase()
	}
	return c.ChatBaseCN
}

// prepareBody Сборка тела исходящего запроса (переключатель десенсибилизации от Client.SanitizeFingerprints управление).
// realm для аккаунта Realm()（cn/global），Подача efforts Бакетирование кэша (кросс-доменное effort коллекции не загрязняют друг друга).
func (c *Client) prepareBody(body []byte, realm, uid, conversationID string) []byte {
	efforts, defs := c.effortsSnapshot(realm), c.defaultEffortsSnapshot(realm)
	if realmKey(realm) == "global" {
		// global Источник даунгрейда домена = Удаленный бакет зондирования (авторитетный)∪ статическая fallback-таблица продукта (глобально 21 уровни в имени, напр.
		// deepseek-v4.1-flash ['high']）。при пустом probe-бакете деградация по статической таблице, без сквозного проксирования
		//（issue #84：К WorkBuddy апстрим отправляет low/max невалидно, требуется откат до high）。
		efforts, defs = globalEffortMap(efforts, defs)
	}
	body = PrepareBodyOptWithEffortsAndDefault(body, c.SanitizeFingerprints.Load(), efforts, defs)
	// prompt_cache_key Инъекция (P0 Оптимизация затрат, снижение затрат ~17×）：стабильный кеш-ключ с изоляцией по аккаунту,
	// чтобы последовательные запросы одного клиента к одному аккаунту попадали в префиксный кэш апстрима.
	body = InjectPromptCacheKey(body, uid, conversationID)
	return body
}

// effortsSnapshot вернуть effort копия кэша capabilities;nil означает неизвестно (прозрачная передача без даунгрейда).
func (c *Client) effortsSnapshot(realm string) map[string][]string {
	c.effortsMu.RLock()
	defer c.effortsMu.RUnlock()
	bucket, ok := c.efforts[realmKey(realm)]
	if !ok || len(bucket) == 0 {
		return nil
	}
	cp := make(map[string][]string, len(bucket))
	for k, v := range bucket {
		cp[k] = v
	}
	return cp
}

// defaultEffortsSnapshot Вернуть указанный realm модель defaultEffort копия кэша;
// Для домена нет проб или заявленного дефолтного профиля → nil（thinking.go Хардкод отката high）。
func (c *Client) defaultEffortsSnapshot(realm string) map[string]string {
	c.effortsMu.RLock()
	defer c.effortsMu.RUnlock()
	bucket, ok := c.defaultEfforts[realmKey(realm)]
	if !ok || len(bucket) == 0 {
		return nil
	}
	cp := make(map[string]string, len(bucket))
	for k, v := range bucket {
		cp[k] = v
	}
	return cp
}

// realmKey Нормализация efforts Ключ кэша:cn/global。пустой realm считать как cn（старый вызов/имя модели без префикса).
func realmKey(realm string) string {
	if realm == "" {
		return "cn"
	}
	return realm
}

func (c *Client) billingBase(a *auth.Auth) string {
	if c.globalOn(a) {
		return c.globalBillingBase()
	}
	return c.BillingBaseCN
}

// webBase Возвращает домен официального сайта (интерфейсы получения наград за задачи; при отсутствии инъекции — fallback по умолчанию).
// realm Восприятие:global переключение аккаунта на международный сайт workbuddy.ai，CN использовать workbuddy.cn。
func (c *Client) webBase(a *auth.Auth) string {
	if c.globalOn(a) {
		return defaultGlobalBase
	}
	if c.WebBaseCN != "" {
		return c.WebBaseCN
	}
	return "https://www.workbuddy.cn"
}

// doJSON отправить запрос и распаковать конверт;HTTP не 2xx или бизнес-логике code != 0 при возврате с body фрагмента *Error。
// body ошибка чтения (обрыв соединения/Троттлинг при простое/усечение) возвращает обычную ошибку (не *Error）——Обрывок body не входит
// Classify，не участвует в пенальти аккаунта (сбой транспортного уровня не должен триггерить circuit breaker).
func (c *Client) doJSON(req *http.Request) (json.RawMessage, error) {
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, fmt.Errorf("read body: %w", err)
	}
	if resp.StatusCode >= 400 {
		kind := Classify(resp.StatusCode, string(raw))
		return nil, &Error{Kind: kind, Status: resp.StatusCode, Msg: truncate(string(raw), 200)}
	}
	var env apiEnvelope
	if err := json.Unmarshal(raw, &env); err != nil {
		return nil, fmt.Errorf("parse failed: %w (body: %s)", err, truncate(string(raw), 120))
	}
	if env.Code != 0 {
		kind := Classify(resp.StatusCode, env.Msg)
		if kind == ErrNone {
			kind = ErrClient
		}
		return nil, &Error{Kind: kind, Status: resp.StatusCode, Msg: fmt.Sprintf("code=%d msg=%s", env.Code, truncate(env.Msg, 160))}
	}
	return env.Data, nil
}

// RefreshToken Обновление access token；обновление при успехе a поле (значение по умолчанию сохраняет старое),
// Ответственность вызывающей стороны SaveAtomic。удерживать на всём протяжении a блокировка, защита от конкурентности SaveAtomic Чтение с половинным обновлением token。
// refreshIOTimeout обновить сеть эндпоинтов I/O лимит (выполнение вне двухфазной блокировки, защита от апстрима hang долгое удержание блокировки).
const refreshIOTimeout = 30 * time.Second

// refreshTokenExpiresInMax refresh Ответ expiresIn верхний предел порядка (10 год, чисто защитное значение:
// На практике R-D ответ всегда 5184000=60d）。превышение считать грязными данными апстрима, не писать ExpiresAt（сохранить старое значение),
// предотвратить NeedsRefresh вечно false приводит к token никогда не обновляется, наоборот истекает и становится недействительным.
const refreshTokenExpiresInMax = 10 * 365 * 24 * time.Hour

// RefreshToken Обновление access token；обновление при успехе a поле (значение по умолчанию сохраняет старое),
// Ответственность вызывающей стороны SaveAtomic。
//
// Модель потокобезопасности (двухфазная, сужение окна удержания блокировки):
// - Внутри блокировки только "чтение refreshToken снимок» и "запись нового после проверки неизменности token」Две короткие операции в памяти;
// - сеть I/O（doJSON）В**Вне блокировки**Выполнение с 30s ctx таймаут — избегать апстрима hang длительное время
// эксклюзивный a.mu，блокирует тот же аккаунт SaveAtomic / прочие обновления (issue:удержание блокировки 120s I/O）。
// - Перед записью повторно проверить консистентность снапшота: если вне блокировки другой goroutine Обновление завершено (refreshToken
// уже изменилось), результат этого раза применяется напрямую (новый token уже вступило в силу), повторная запись не нужна.
func (c *Client) RefreshToken(a *auth.Auth) error {
	// № 1 Секция (под блокировкой): чтение снапшота.
	a.Lock()
	rtSnapshot := a.RefreshToken
	atBefore := a.AccessToken
	a.Unlock()
	if strings.TrimSpace(rtSnapshot) == "" {
		return fmt.Errorf("no refreshToken")
	}

	endpoint := c.chatBase(a) + "/v2/plugin/auth/token/refresh"
	ctx, cancel := context.WithTimeout(context.Background(), refreshIOTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, nil)
	if err != nil {
		return err
	}
	// RefreshHeaders Чтение a поле (domain/uid и т.д.) инжектятся в заголовки запроса — нужно брать снапшот под блокировкой,
	// Использовать временный объект с явным покопийным копированием полей auth формирование заголовка (без копирования sync.Mutex，Избежать vet copies-lock）。
	a.Lock()
	hdrSnapshot := auth.Auth{
		AccessToken: a.AccessToken,
		RefreshToken: rtSnapshot,
		ExpiresAt: a.ExpiresAt,
		Domain: a.Domain,
		UID: a.UID,
		EnterpriseID: a.EnterpriseID,
		Nickname: a.Nickname,
		DeviceToken: a.DeviceToken,
	}
	a.Unlock()
	c.RefreshHeaders(req, &hdrSnapshot)

	// сеть I/O（вне блокировки,30s лимит).
	data, err := c.doJSON(req)
	if err != nil {
		return err
	}
	var tok struct {
		AccessToken string `json:"accessToken"`
		RefreshToken string `json:"refreshToken"`
		ExpiresIn int64 `json:"expiresIn"`
		Domain string `json:"domain"`
	}
	if err := json.Unmarshal(data, &tok); err != nil || tok.AccessToken == "" {
		return fmt.Errorf("refresh_failed: no accessToken in response — re-login required")
	}

	// № 2 секция (под блокировкой): после проверки консистентности снапшота записать обратно.
	a.Lock()
	defer a.Unlock()
	// гард обратной записи — AND семантика: за время вне блокировки другое обновление уже завершено → Два token должны меняться одновременно (фактически R-D：
	// refresh Ответ accessToken/refreshToken всегда вместе rotate，обратная запись пишет сразу в оба),AND
	// т.е. критерий "конкурентное обновление завершено»;AND и OR эквивалентно в реальной форме. Только OR будет дополнительно отброшено
	// 「Только одиночный token изменение» (напр. ручное изменение только auth Поле файла) не является условием отказа — в данном случае
	// результат перезаписывает ручное редактирование.
	if a.AccessToken != atBefore && a.RefreshToken != rtSnapshot {
		// Вне блокировки другой goroutine обновление завершено: новый token Уже применено, результат текущего вызова можно не записывать
		// （На практике R-E：на сервере отсутствует rotation Откат, два новых токена из конкурентного двойного обновления token все действительны,
		// Перезапись последним эквивалентна; ранний return избегает бессмысленной перезаписи и ExpiresAt джиттер).
		return nil
	}
	a.AccessToken = tok.AccessToken
	if tok.RefreshToken != "" {
		a.RefreshToken = tok.RefreshToken
	}
	if tok.Domain != "" {
		a.Domain = tok.Domain
	}
	// preserveExpiry：Ответ отсутствует expiresIn сохранять старое время истечения, избегать шторма обновлений.
	// На практике R-D Ответ всегда содержит expiresIn=5184000（60d）——Ветка по умолчанию — только защита, сохраняет старое значение
	// избежать дрейфа проверки истечения. Аналогично, свыше 10 года expiresIn Обработать как dirty, сохранить старое значение:
	// На практике JWT exp-iat и expiresIn Строгая самосогласованность (R-F），Сверхбольшие значения — только грязные данные апстрима,
	// Прямая запись затрет ExpiresAt сдвинет в абсурдное будущее → NeedsRefresh Всегда false → token никогда не обновлять
	// наоборот реально истекает и становится недействительным.
	if tok.ExpiresIn > 0 && time.Duration(tok.ExpiresIn)*time.Second < refreshTokenExpiresInMax {
		a.ExpiresAt = time.Now().Add(time.Duration(tok.ExpiresIn) * time.Second).Unix()
	}
	return nil
}

// ChatStream Отправка chat Запросить и вернуть оригинал SSE body Поток (ответственность вызывающей стороны Close）。
// Эквивалентно ChatStreamContext(context.Background(), ...)：Без семантики отмены вызывающей стороны.
// Вызывающей стороне, которой нужно разорвать связку с клиентом, использовать ChatStreamContext входящий запрос ctx。
//
// global realm：Сначала пробить /console/chat/completions，404/405 При этом тот же base вторичная замена /v2/chat/completions
// （развилка старого/нового пути апстрима,PLAN R9 fallback порядок).cn：/v2/chat/completions Состояние без изменений.
func (c *Client) ChatStream(a *auth.Auth, body []byte, clientIP string, meta ChatMeta) (rc io.ReadCloser, status int, respBody []byte, err error) {
	return c.ChatStreamContext(context.Background(), a, body, clientIP, meta)
}

// ChatStreamContext Совм. ChatStream，Но с ctx Производный запрос context：вызывающая сторона (handler）Входящий
// r.Context() после, дисконнект клиента/Отмена запроса немедленно прерывает активные upstream-вызовы, освобождает соединения и квоту аккаунта,
// Больше не холостой ход до IdleTimeout。ctx для nil откат при Background。успешного потока cancel по-прежнему
// monitorBody Close Перехват (reqCtx Отмена и явный Close срабатывание любого условия — разрыв).
//
// Путь ошибки (≥400 и не fallback код статуса) кроме (status, respBody) кроме того возвращает**уже классифицированные**
// *Error（Kind Конверт + Retry-After разбор заголовков): классификация клиентских ошибок выполняется здесь за один раз,handler
// больше не для body повторный Classify（устранение дрейфа двух путей "классификация апстримом + повторная классификация шлюзом»),
// Retry-After Также передается вместе с конвертом.respBody всё равно возвращается как есть (семантика сквозной передачи ошибки:message Проброс на апстрим
// оригинал). Считается ErrNone ответ (теоретически не существует, защита)err для nil，handler Нажать
// respBody Собственный фолбэк.
//
// global chat авто #119 После замера идти фиксированно через /v2（chat на уровне отсутствует fallback цепочка;billing уровня 404
// fallback существует независимо, семантика не затронута).ensureConsoleSystem В prepareBody после применяется единообразно
// глобальный скрипт: первое сообщение не system предварительный фолбэк по времени system（Защита console Апстрим домена code 11-128；
// #119 После global исходящий фиксированный /v2，Этот фолбэк сохранён — апстрим для /v2 требуется ли system Без фактических измерений
// доказательство от противного, после удаления нет пути отката).
func (c *Client) ChatStreamContext(ctx context.Context, a *auth.Auth, body []byte, clientIP string, meta ChatMeta) (rc io.ReadCloser, status int, respBody []byte, err error) {
	if ctx == nil {
		ctx = context.Background()
	}
	prepared := c.prepareBody(body, a.Realm(), a.UID, meta.ConversationID)
	if c.globalOn(a) {
		prepared = ensureConsoleSystem(prepared)
	}
	// reqCtx cancel Явный вызов на каждом выходе (Do ошибка / ≥400 / передача успешной ветки monitorBody），
	// Все ветки цикла обязательно return——Без хвостового fallback-кода цикла (ранее внешний var cancel Никогда не присваивалось +
	// Хвост недостижим cancel() является скрытым nil-panic，Удалено;chatPaths всегда непусто, гарантируется конструктором).
	for _, path := range c.chatPaths(a) {
		endpoint := c.chatBase(a) + path
		req, err := http.NewRequest(http.MethodPost, endpoint, bytes.NewReader(prepared))
		if err != nil {
			return nil, 0, nil, err
		}
		c.ChatHeaders(req, a, clientIP, meta)
		// От вызывающей стороны ctx деривация: сохранять распространение отмены (родитель ctx отмена → текущий ctx отмена),
		// Одновременно monitorBody.Close Всё ещё независимо cancel данная ветка (обрыв потока при простое).
		reqCtx, cancel := context.WithCancel(ctx)
		req = req.WithContext(reqCtx)
		resp, err := c.chatHTTP().Do(req)
		if err != nil {
			cancel()
			log.Printf("ERR: [upstream] chat_stream acct=%s: transport error: %v", logfmt.Label(a.UID, a.Nickname), err)
			// Сбой транспортного уровня → Очистка idle-соединений общего пула (усиление на уровне соединений): сбойное соединение может всё ещё
			// остается в пуле idle, следующий запрос снова его подхватит — только за счет IdleConnTimeout и т.п. истекает
			// недостаточно, только активная очистка пула устраняет причину.
			roundTripCloseIdle(c.chatHTTP().Transport)
			return nil, 0, nil, err
		}
		if resp.StatusCode >= 400 {
			raw, rerr := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
			resp.Body.Close()
			cancel()
			// body Ошибка чтения (дросселирование/обрезка)→ Ошибка транспортного уровня: оборванный raw не возвращать вызывающей стороне в Classify，
			// Иначе handler Боковой applyErrorPolicy Будет штрафовать номер по категории ложного срабатывания.
			if rerr != nil {
				log.Printf("ERR: [upstream] chat_stream acct=%s: read body: %v", logfmt.Label(a.UID, a.Nickname), rerr)
				return nil, 0, nil, fmt.Errorf("read body: %w", rerr)
			}
			kind := Classify(resp.StatusCode, string(raw))
			log.Printf("WARN: [upstream] chat_stream acct=%s: upstream %d %s body=%s",
				logfmt.Label(a.UID, a.Nickname), resp.StatusCode, kind, truncate(string(raw), 200))
			// ≥400 Прямой возврат (#119 После global Один путь /v2，chat на уровне отсутствует fallback цепочка).
			// классификация один раз, далее Kind Возврат в конверте (вкл. Retry-After разбор заголовка):
			// ErrNone это защитная ветка (≥400 Не должно порождать None），Вернуть оригинал, чтобы handler Фолбэк.
			if kind == ErrNone {
				return nil, resp.StatusCode, raw, nil
			}
			ue := &Error{Kind: kind, Status: resp.StatusCode, Msg: truncate(string(raw), 200)}
			if d, ok := ParseRetryAfter(resp.Header); ok {
				ue.RetryAfter = d
			}
			return nil, resp.StatusCode, raw, ue
		}
		// Ветка успеха:cancel Передать владение monitorBody（его Close Будет cancel）；
		// IdleTimeout<=0 Время monitorBody Возвращать нижестоящий поток как есть, без регулирования cancel——допустимо:
		// распространение отмены через http.Transport В body Close / родительский ctx Обработка при отмене, соединение корректно очищается.
		return monitorBody(resp.Body, c.IdleTimeout, cancel), resp.StatusCode, nil, nil
	}
	panic("unreachable: chatPaths is never empty") // for range При пустом множестве компилятор всё равно требует fallback return；chatPaths Всегда непусто (гарантия конструктора), недостижимо
}

// ModelInfo динамическая информация о модели (вкл. maxInputTokens/maxOutputTokens + все поля объекта модели апстрима).
// CN /console и global /v2 Объект модели изоморфен, поэтому структура общая; опущенные апстримом поля остаются нулевыми,
// /v1/models на стороне отдавать по правилу "null опускается» (не выдумывать).
type ModelInfo struct {
	ID string
	Name string
	ContextWindow int64 // = maxInputTokens
	MaxTokens int64 // = maxOutputTokens（reasoning и финальный ответ делят этот бюджет, у апстрима нет отдельного лимита на reasoning)
	Efforts []string // reasoning.supportedEfforts（пустой=неизвестно/фиксированный тариф)
	DefaultEffort string // reasoning.defaultEffort（новый ключ модели) или reasoning.effort（ключ старой модели); пусто=Не возвращено

	// все поля каталога моделей (models-full-fields）：
	Description string // descriptionZh Описание на китайском
	Credits string // credits исходный текст множителя баллов (напр. "x0.05"），Только отображение, без участия в выборе номера
	Tags []string // tags Тег модели (вкл. badge:Бесплатно ограниченное время и т.д.)
	Vendor string // vendor идентификатор вендора
	IsDefault bool // isDefault модель по умолчанию или нет
	SupportsReasoning bool // supportsReasoning Поддерживается ли reasoning
	SupportsToolCall bool // supportsToolCall поддерживает ли вызов инструментов
	OnlyReasoning bool // onlyReasoning Чистая reasoning-модель или нет
	SupportsImages bool // Верхний уровень supportsImages（Мультимодальные возможности, прокинуть в /v1/models）
	MaxAllowedSize int64 // maxAllowedSize максимально допустимый контекст (с maxInputTokens метрики параллельны, апстрим выдает каждую отдельно)
	CanDisableThinking bool // reasoning.canDisableThinking：мышление можно отключить (off тариф доступен)
	ReasoningEffort string // reasoning.effort Режим рассуждения (с supportedEfforts массивы из разных источников)
	ReasoningSummary string // reasoning.summary Режим сводки рассуждений (напр. "auto"）

	// Скидка (modelPromotions，/v3/config data.modelPromotions）：Credits Да**Курс**
	//（базовый коэффициент после регуляризации),Promo* это действующая лимитированная акция — панель показывает по ней "действующую цену +
	// Тег + курс».PromoFactor для nil Означает отсутствие machine-readable Скидка (напр. "внепиковая»
	// "Использование» только текст периода без factor），Только навесить тег/подсказка.
	PromoFactor *float64 // Коэффициент скидки (0=Ограниченное время бесплатно,0.5=скидка 50%);nil=отсутствует
	PromoCredits string // Исходный текст коэффициента после скидки (напр. "0x« / »0.50x"），Только отображение
	PromoLabel string // Текст бейджа (лимитированно бесплатно / Ночная скидка / использование со сдвигом пиков)
	PromoNote string // hover Пояснение к оригиналу (включая период/описание даты)
}

// dynModelEntry Каталог моделей апстрима (CN /console и global /v2 изоморфная) форма парсинга одиночной модели,
// FetchModels и global_models.go совместное использование проб.iconUrl/descriptionEn/параметры генерации и т.д.
// Не парсить по принципу "не раскрывать».modelInfo() Да dynEntry→ModelInfo Единственный источник истины для маппинга,
// Исключить дрейф маппинга между двумя доменами.
type dynModelEntry struct {
	ID string `json:"id"`
	Name string `json:"name"`
	// ModelID / Model id мягкий fallback-ключ (только global Резервное использование мульти-конверта каталога,CN каталог
	// эти два ключа не выдаются; поле добавлено здесь только чтобы typed парсинг может"Видно"них).
	ModelID string `json:"modelId"`
	Model string `json:"model"`
	Description string `json:"descriptionZh"`
	Credits string `json:"credits"`
	Tags []string `json:"tags"`
	Vendor string `json:"vendor"`
	IsDefault bool `json:"isDefault"`
	MaxInputTokens int64 `json:"maxInputTokens"`
	MaxOutputTokens int64 `json:"maxOutputTokens"`
	MaxAllowedSize int64 `json:"maxAllowedSize"`
	Disabled bool `json:"disabled"`
	SupportsImages bool `json:"supportsImages"`
	SupportsReason bool `json:"supportsReasoning"`
	SupportsTool bool `json:"supportsToolCall"`
	OnlyReasoning bool `json:"onlyReasoning"`
	Reasoning struct {
		Effort string `json:"effort"`
		Summary string `json:"summary"`
		DefaultEffort string `json:"defaultEffort"`
		CanDisableThinking bool `json:"canDisableThinking"`
		SupportedEfforts []string `json:"supportedEfforts"`
	} `json:"reasoning"`
}

// modelInfo построить по распарсенным записям ModelInfo（dynEntry→ModelInfo единственный источник истины для маппинга).
// defaultEffort Совместимость старого и нового ключей:defaultEffort Приоритет, fallback по умолчанию effort。
func (m dynModelEntry) modelInfo() ModelInfo {
	def := m.Reasoning.DefaultEffort
	if def == "" {
		def = m.Reasoning.Effort
	}
	return ModelInfo{
		ID: m.ID,
		Name: m.Name,
		ContextWindow: m.MaxInputTokens,
		MaxTokens: m.MaxOutputTokens,
		Efforts: m.Reasoning.SupportedEfforts,
		DefaultEffort: def,
		SupportsImages: m.SupportsImages,
		Description: m.Description,
		Credits: m.Credits,
		Tags: m.Tags,
		Vendor: m.Vendor,
		IsDefault: m.IsDefault,
		SupportsReasoning: m.SupportsReason,
		SupportsToolCall: m.SupportsTool,
		OnlyReasoning: m.OnlyReasoning,
		MaxAllowedSize: m.MaxAllowedSize,
		CanDisableThinking: m.Reasoning.CanDisableThinking,
		ReasoningEffort: m.Reasoning.Effort,
		ReasoningSummary: m.Reasoning.Summary,
	}
}

// nonChatModel Определить, является ли модель недиалоговой (должна фильтроваться из списка моделей).
// Источник:harness buddy.ts:547-555。Три типа правил:
// - id Префикс nes-/completion-/codewise-：Встроить/Автодополнение/Спец-модель для кода, при выборе репортит code=11102。
// - maxOutputTokens ≤ 256：tiny Вывод — не диалоговая модель.
// - tags Содержит text-to-image：Модель генерации изображений, не для данного шлюза.
func nonChatModel(id string, maxOutputTokens int64, tags []string) bool {
	id = strings.ToLower(strings.TrimSpace(id))
	for _, p := range [...]string{"nes-", "completion-", "codewise-"} {
		if strings.HasPrefix(id, p) {
			return true
		}
	}
	if maxOutputTokens > 0 && maxOutputTokens <= 256 {
		return true
	}
	for _, t := range tags {
		if t == "text-to-image" {
			return true
		}
	}
	return false
}

// codeBuddyIDEUA /v3/config Требуется возможность распарсить CodeBuddy версии UA。
// CLI трехсегментный WorkBuddy UA будет получен урезанный каталог (flash Вывод 128K、отсутствует supportedEfforts）；
// Официальный IDE Заголовок `CodeBuddyIDE/4.12.0 CodeBuddy/4.12.0` только тогда вернуть полный capabilities
// （flash：393216 + low/high/max）。
// версия должна следовать за апстримом IDE Сопровождение релиза:UAn При устаревшей версии эндпоинт может также вернуть урезанный каталог.
const codeBuddyIDEUA = "CodeBuddyIDE/4.12.0 CodeBuddy/4.12.0"

// codeBuddyCLIUA CLI трехсегментный UA。**фактически (2026-09-22）Данная конечная точка для разных UA выданные наборы моделей различаются**：
// - IDE UA → 14 записей (10 шт. chat：Содержит o4-mini / enhance-1.0 / auto-chat，**отсутствует deepseek Серия**）
// - CLI UA → 22 записей (22 шт. chat：**Содержит deepseek-v4.1-flash / deepseek-v4.1-flash-sg /
// gpt-6-astra / kimi-k2.8-preview**，но без o4-mini / enhance-1.0 / auto-chat）
//
// Внимание: два момента противоположны старому комментарию, не делать выводы по старому комментарию:
// 1. Старый комментарий гласит "CLI UA Получить урезанный каталог,IDE UA только тогда возвращается полный набор возможностей» — фактически количество моделей наоборот,
// Но **IDE размер ответа больше**（26003B vs 21111B），Поэтому "полные возможности» следует понимать как**Поля одиночной записи полнее**，
// а не больше моделей. У каждого из двух каналов есть уникальные модели, оба необходимы.
// 2. Данная константа только для global второй канал детекта на стороне;CN сторона всё ещё идёт codeBuddyIDEUA один канал.
const codeBuddyCLIUA = "CLI/2.63.2 CodeBuddy/2.63.2"

// FetchModels Вызов upstream API динамических моделей (CN сторона;global см. аккаунт global_models.go семейство).
//
// v3-config-merge：Динамический каталог = /v3/config（основной,IDE UA полнофункциональная версия)+ enterprise-эндпоинт
// （/console，cli фильтрация по поверхности, дополнение) — объединение, два пути**Конкурентность**Детект. Объединение с дедупликацией key = Модель id，
// v3 Приоритет записей (credits и др. поля как v3 за основу), enterprise-эндпоинт только дополняет v3 отсутствующая модель.
// /v3 сбой (400/Ошибка сети/ошибка парсинга) не тянет результат enterprise-эндпоинта — деградация до только enterprise-эндпоинта,warn лог;
// И наоборот (два независимых отказоустойчивых тракта).
func (c *Client) FetchModels(a *auth.Auth) ([]ModelInfo, error) {
	type probeResult struct {
		infos []ModelInfo
		err error
	}
	enterpriseCh := make(chan probeResult, 1)
	v3Ch := make(chan probeResult, 1)
	go func() {
		infos, err := c.fetchEnterpriseModels(a)
		enterpriseCh <- probeResult{infos, err}
	}()
	go func() {
		infos, err := c.fetchV3Models(a)
		v3Ch <- probeResult{infos, err}
	}()
	enterprise := <-enterpriseCh
	v3 := <-v3Ch
	if enterprise.err != nil && v3.err != nil {
		return nil, enterprise.err // Отказ обоих путей: вернуть ошибку корпоративного эндпоинта (без дрейфа семантики вызывающей стороны)
	}
	if v3.err != nil {
		// /v3 деградация при ошибке: не тянет за собой результат enterprise-эндпоинта (деградация только enterprise-эндпоинт + warn）。
		log.Printf("WARN: [upstream] fetch models: v3/config probe failed (degraded to enterprise endpoint): %v", v3.err)
	}
	if enterprise.err != nil {
		log.Printf("WARN: [upstream] fetch models: enterprise endpoint failed (v3/config only): %v", enterprise.err)
	}
	out := mergeModelInfos(v3.infos, enterprise.infos)
	if len(out) == 0 {
		return nil, fmt.Errorf("models api returned empty list")
	}
	c.storeModelRates(a.Realm(), out)
	// Обновление effort Кэш capabilities (для деградации тела запроса; без supportedEfforts модель не попадает в бакет).
	// При пустом бакете пропуск записи: чтобы "отсутствие данных тарифа в пробе» не очистило существующий бакет.
	cache := make(map[string][]string, len(out))
	defCache := make(map[string]string, len(out))
	for _, mi := range out {
		if len(mi.Efforts) > 0 {
			cache[mi.ID] = mi.Efforts
		}
		if mi.DefaultEffort != "" {
			defCache[mi.ID] = mi.DefaultEffort
		}
	}
	if len(cache) == 0 && len(defCache) == 0 {
		return out, nil
	}
	// По аккаунту зондирования realm запись в соответствующий бакет:CN проба только на вход cn бакет,global То же имя модели не загрязняется (C-2）。
	c.storeEfforts(a.Realm(), cache, defCache)
	return out, nil
}

// mergeModelInfos объединение двух каталогов моделей:primary преимущественно (аналог. id по primary эталон — записи —
// credits и т.п. поля — авторитетен главный эндпоинт),secondary только дополнить primary отсутствующий id。
// дедупликация key = Модель id；порядок вывода = primary Исходный порядок впереди,secondary Дополнительные поля (secondary исходный порядок)
// после — стабильный вывод, не зависит от map Порядок итерации.
func mergeModelInfos(primary, secondary []ModelInfo) []ModelInfo {
	if len(secondary) == 0 {
		return primary
	}
	seen := make(map[string]bool, len(primary)+len(secondary))
	out := make([]ModelInfo, 0, len(primary)+len(secondary))
	for _, mi := range primary {
		if mi.ID == "" || seen[mi.ID] {
			continue
		}
		seen[mi.ID] = true
		out = append(out, mi)
	}
	for _, mi := range secondary {
		if mi.ID == "" || seen[mi.ID] {
			continue
		}
		seen[mi.ID] = true
		out = append(out, mi)
	}
	return out
}

// fetchEnterpriseModels одноканальный проб enterprise-эндпоинта модели (/console/enterprises/personal/models）。
// Правила парсинга:agents[cli].models Фильтрация + nonChatModel исключить + disabled Исключить.
func (c *Client) fetchEnterpriseModels(a *auth.Auth) ([]ModelInfo, error) {
	// избегать имени локальной переменной url（Данный пакет уже import net/url，одноимённость вызовет путаницу при чтении).
	endpoint := c.chatBase(a) + "/console/enterprises/personal/models"
	req, err := http.NewRequest(http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, err
	}
	c.CommonHeaders(req, a) // Переиспользование общих заголовков запроса (Origin/Referer/UA/Accept/Content-Type）
	// AccessToken Снапшот под блокировкой (см. auth.AccessTokenValue：keepalive обновление в a.mu перезапись внутри).
	req.Header.Set("Authorization", "Bearer "+a.AccessTokenValue())
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		// Ошибка чтения → ошибка транспортного уровня (handler на стороне этот путь не NoteError）。
		return nil, fmt.Errorf("read body: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("models api status %d: %s", resp.StatusCode, truncate(string(raw), 120))
	}
	var env struct {
		Code int `json:"code"`
		Data struct {
			Models []dynModelEntry `json:"models"`
			Agents []struct {
				Name string `json:"name"`
				Models []string `json:"models"`
			} `json:"agents"`
		} `json:"data"`
	}
	if err := json.Unmarshal(raw, &env); err != nil {
		return nil, fmt.Errorf("models parse: %w", err)
	}
	if env.Code != 0 {
		return nil, fmt.Errorf("models api code=%d", env.Code)
	}
	var cliIDs []string
	for _, ag := range env.Data.Agents {
		if ag.Name == "cli" {
			cliIDs = ag.Models
			break
		}
	}
	if len(cliIDs) == 0 {
		return nil, fmt.Errorf("no cli agent models found")
	}
	// dynMap сбор полей модели;nonChatModel фильтрация при записи dynMap выполнить до,
	// убедиться, что недиалоговые записи (nes-/completion-/codewise- Префикс,maxOutputTokens≤256、
	// tags Содержит text-to-image）Вообще не попадает в список возврата (источник:harness buddy.ts:547-555）。
	dynMap := make(map[string]dynModelEntry, len(env.Data.Models))
	for _, m := range env.Data.Models {
		if nonChatModel(m.ID, m.MaxOutputTokens, m.Tags) {
			continue
		}
		dynMap[m.ID] = m
	}
	out := make([]ModelInfo, 0, len(cliIDs))
	for _, id := range cliIDs {
		if m, ok := dynMap[id]; ok && !m.Disabled {
			out = append(out, m.modelInfo())
		}
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("models api returned empty list")
	}
	return out, nil
}

// fetchV3Models Одноканальный пробник /v3/config（IDE UA полная версия с возможностями, см. codeBuddyIDEUA）。
// v3 брать полный объём на стороне models（Не по agents[cli] Фильтрация, и global единый критерий пробы), по тому же
// nonChatModel правило отсеивает недиалоговые записи (selected выберет модель — отчет code=11102）。
// При ошибке вернуть ошибку (вызывающая сторона деградирует до только enterprise-эндпоинта).
func (c *Client) fetchV3Models(a *auth.Auth) ([]ModelInfo, error) {
	byID, err := c.fetchV3ConfigModelMap(a, codeBuddyIDEUA)
	if err != nil {
		return nil, err
	}
	out := make([]ModelInfo, 0, len(byID))
	for _, mi := range byID {
		if nonChatModel(mi.ID, mi.MaxTokens, mi.Tags) {
			continue
		}
		out = append(out, mi)
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("v3/config returned empty models")
	}
	return out, nil
}

// v3ModelPromotion /v3/config data.modelPromotions Определение одиночной скидки (2026-09-23 На практике
// 7 записей:deepseek система скидки 50% вне пика,glm-5.2 ночная скидка 50%,hy3 и hy4-preview-f Ограниченное время бесплатно).
// discount присутствует только в части записей: есть factor считается действующей ценой; тип "использование вне пика» — только слот
// Текст (factor Скрыто в hover в тексте, без машиночитаемого значения), отдаются только метка и описание.
type v3ModelPromotion struct {
	Enabled bool `json:"enabled"`
	Priority int `json:"priority"`
	ModelIDs []string `json:"modelIds"`
	Badge *struct {
		Label string `json:"label"`
	} `json:"badge"`
	Discount *struct {
		DiscountedCredits string `json:"discountedCredits"`
		Factor float64 `json:"factor"`
	} `json:"discount"`
	Hover *struct {
		TextZh string `json:"textZh"`
	} `json:"hover"`
	Schedule *struct {
		Daily []struct {
			Start string `json:"start"` // "23:00"
			End string `json:"end"` // »7:50"（может пересекать полночь)
		} `json:"daily"`
		Timezone string `json:"timezone"` // На практике всегда Asia/Shanghai
		ValidFrom string `json:"validFrom"` // RFC3339，Может опускаться
		ValidUntil string `json:"validUntil"`
	} `json:"schedule"`
}

// promoZone льготная таймзона: upstream всегда Asia/Shanghai（UTC+8 без летнего времени), использовать FixedZone Без зависимостей
// Система tzdata（Windows отсутствует IANA при библиотеке LoadLocation будет сбой).
var promoZone = time.FixedZone("CST", 8*3600)

// promoClock Парсинг "HH:MM" — минуты текущих суток; при невалидном значении вернуть (-1, false)。
func promoClock(hhmm string) (int, bool) {
	parts := strings.Split(hhmm, ":")
	if len(parts) != 2 {
		return -1, false
	}
	h, err1 := strconv.Atoi(strings.TrimSpace(parts[0]))
	m, err2 := strconv.Atoi(strings.TrimSpace(parts[1]))
	if err1 != nil || err2 != nil || h < 0 || h > 24 || m < 0 || m > 59 {
		return -1, false
	}
	return h*60 + m, true
}

// promoActive оценка скидки в now вступило ли в силу:enabled + validFrom/validUntil Внутри + попадает в
// Любой daily Окно (с поддержкой через полночь, напр. 23:00→7:50）。schedule для nil считается действующим весь день.
func promoActive(p *v3ModelPromotion, now time.Time) bool {
	if !p.Enabled {
		return false
	}
	if sc := p.Schedule; sc != nil {
		if sc.ValidFrom != "" {
			from, err := time.Parse(time.RFC3339, sc.ValidFrom)
			if err == nil && now.Before(from) {
				return false
			}
		}
		if sc.ValidUntil != "" {
			until, err := time.Parse(time.RFC3339, sc.ValidUntil)
			if err == nil && !now.Before(until) {
				return false
			}
		}
		if len(sc.Daily) > 0 {
			cur := now.Hour()*60 + now.Minute()
			inWindow := false
			for _, w := range sc.Daily {
				st, ok1 := promoClock(w.Start)
				ed, ok2 := promoClock(w.End)
				if !ok1 || !ok2 {
					continue
				}
				if st <= ed {
					if cur >= st && cur < ed {
						inWindow = true
						break
					}
				} else if cur >= st || cur < ed { // Через полночь (23:00→7:50）
					inWindow = true
					break
				}
			}
			if !inWindow {
				return false
			}
		}
	}
	return true
}

// applyModelPromotions Привязать текущую активную скидку к элементу каталога: при множественном попадании одной модели брать priority
// Максимум (фактически glm-5.2 дневное время badge-only(50) и ночная скидка 50%(100) привязка priority+daily Двухтрековый
// переключение). Отсутствует discount Записи объекта также помечаются тегом/Описание (тип со сдвигом нагрузки),PromoFactor Сохранить nil。
func applyModelPromotions(out map[string]ModelInfo, promos []v3ModelPromotion) {
	if len(promos) == 0 || len(out) == 0 {
		return
	}
	now := time.Now().In(promoZone)
	type cand struct {
		prio int
		p *v3ModelPromotion
	}
	best := map[string]cand{}
	for i := range promos {
		p := &promos[i]
		if !promoActive(p, now) {
			continue
		}
		for _, id := range p.ModelIDs {
			if _, ok := out[id]; !ok {
				continue // модель вне каталога (напр. одноименная global вариант) не монтировать
			}
			if b, seen := best[id]; !seen || p.Priority > b.prio {
				best[id] = cand{prio: p.Priority, p: p}
			}
		}
	}
	for id, c := range best {
		mi := out[id]
		if c.p.Badge != nil {
			mi.PromoLabel = c.p.Badge.Label
		}
		if c.p.Hover != nil {
			mi.PromoNote = c.p.Hover.TextZh
		}
		if c.p.Discount != nil {
			f := c.p.Discount.Factor
			mi.PromoFactor = &f
			mi.PromoCredits = c.p.Discount.DiscountedCredits
		}
		out[id] = mi
	}
}

// storeEfforts Нажать realm запись effort бакет кэша capabilities (efforts + defaultEfforts），Потокобезопасность.
// Подача CN FetchModels и global Общий проб: после попадания полученного тира модели в бакет тело исходящего запроса
// normalizeReasoningEffort только тогда даунгрейд по домену.efforts и defs при пустоте всего удалить данный realm Бакет
// （эквивалентно "для этого домена нет уровня для даунгрейда»). Вызывающая сторона пропускает запись при "нет новых данных».
func (c *Client) storeEfforts(realm string, efforts map[string][]string, defs map[string]string) {
	c.effortsMu.Lock()
	defer c.effortsMu.Unlock()
	if c.efforts == nil {
		c.efforts = make(map[string]map[string][]string)
	}
	if c.defaultEfforts == nil {
		c.defaultEfforts = make(map[string]map[string]string)
	}
	k := realmKey(realm)
	if len(efforts) == 0 && len(defs) == 0 {
		delete(c.efforts, k)
		delete(c.defaultEfforts, k)
		return
	}
	c.efforts[k] = efforts
	c.defaultEfforts[k] = defs
}

// normalizeModelRate нормализовать исходный текст множителя апстрима в сравнимый числовой ключ.
// совместимость "x0.05« / "x0.05 credits« / "0.50x« и т.п.; при невозможности числовой конвертации оставить после удаления
// credits Оригинал после суффикса и пробела, не выдумывать коэффициент.
func normalizeModelRate(raw string) string {
	s := strings.TrimSpace(raw)
	if s == "" {
		return ""
	}
	if strings.HasSuffix(strings.ToLower(s), "credits") {
		s = strings.TrimSpace(s[:len(s)-len("credits")])
	}
	if strings.HasPrefix(strings.ToLower(s), "x") {
		s = strings.TrimSpace(s[1:])
	} else if strings.HasSuffix(strings.ToLower(s), "x") {
		s = strings.TrimSpace(s[:len(s)-1])
	}
	if s == "" {
		return ""
	}
	v, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return strings.TrimSpace(raw)
	}
	return strconv.FormatFloat(v, 'f', -1, 64)
}

// effectiveModelRate возвращает действующий множитель модели: при наличии машиночитаемой скидки берётся цена со скидкой,
// иначе брать прайс; если оба отсутствуют — пусто.
func effectiveModelRate(mi ModelInfo) string {
	if mi.PromoFactor != nil && strings.TrimSpace(mi.PromoCredits) != "" {
		return normalizeModelRate(mi.PromoCredits)
	}
	return normalizeModelRate(mi.Credits)
}

// storeModelRates Нажать realm полная замена снапшота коэффициентов модели. Каталог успешно обновлён, но нет парсируемых
// при множителе записать пустой бакет, чтобы старый множитель не выдавал себя за текущую цену.
func (c *Client) storeModelRates(realm string, infos []ModelInfo) {
	rates := make(map[string]string, len(infos))
	for _, mi := range infos {
		if mi.ID == "" {
			continue
		}
		if rate := effectiveModelRate(mi); rate != "" {
			rates[mi.ID] = rate
		}
	}
	c.effortsMu.Lock()
	defer c.effortsMu.Unlock()
	if c.modelRates == nil {
		c.modelRates = make(map[string]map[string]string)
	}
	c.modelRates[realmKey(realm)] = rates
}

// ModelRate возвращает актуальный коэффициент указанной доменной модели из последнего успешного обновления; если неизвестно — пустая строка.
func (c *Client) ModelRate(realm, model string) string {
	if c == nil || model == "" {
		return ""
	}
	c.effortsMu.RLock()
	defer c.effortsMu.RUnlock()
	return c.modelRates[realmKey(realm)][model]
}

// GlobalEffortSnapshot Экспорт global Домен effort Кэш capabilities (зондирование/рассылка ∪ бакет после статического фолбэк-мерджа),
// Подача /v1/models Вывод reasoning_supported_efforts / reasoning_default_effort。
// Возвращает копию; бакет не заполнен (нет global аккаунт или никогда не зондировался)→ nil（откат вызывающей стороны к статической резервной таблице).
func (c *Client) GlobalEffortSnapshot() (efforts map[string][]string, defaults map[string]string) {
	return c.effortsSnapshot("global"), c.defaultEffortsSnapshot("global")
}

// v3ConfigDomain /v3/config X-Domain：Приоритетная запись аккаунта на диск domain，Иначе chatBase host。
func v3ConfigDomain(a *auth.Auth, chatBase string) string {
	if a != nil {
		// Domain Снапшот под блокировкой (см. auth.DomainValue：keepalive обновление в a.mu перезапись внутри).
		if d := strings.TrimSpace(a.DomainValue()); d != "" {
			d = strings.TrimPrefix(d, "https://")
			d = strings.TrimPrefix(d, "http://")
			return strings.TrimSuffix(d, "/")
		}
	}
	if u, err := url.Parse(chatBase); err == nil && u.Host != "" {
		return u.Host
	}
	return "copilot.tencent.com"
}

// fetchV3ConfigModelMap Запросить с апстрима IDE Каталог конфигурации, по модели id Построение таблицы возможностей.
// Этот эндпоинт для UA чувствительный: обязательно передавать CodeBuddy/CodeBuddyIDE версия, иначе 400 code=12403。
// ua для данного запроса User-Agent；пустая строка эквивалентна codeBuddyIDEUA。Этот эндпоинт для UA Чувствительный и**Разное UA выдача
// Разные наборы моделей**（См. codeBuddyCLIUA комментарий),global Зондирование на основе этого параллельно берет объединение двух путей.
func (c *Client) fetchV3ConfigModelMap(a *auth.Auth, ua string) (map[string]ModelInfo, error) {
	req, err := http.NewRequest(http.MethodGet, c.chatBase(a)+"/v3/config", nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json, text/plain, */*")
	req.Header.Set("X-Requested-With", "XMLHttpRequest")
	// AccessToken снапшот под локом (тот же fetchEnterpriseModels）。
	req.Header.Set("Authorization", "Bearer "+a.AccessTokenValue())
	if a != nil && a.UID != "" {
		req.Header.Set("X-User-Id", a.UID)
	}
	req.Header.Set("X-Domain", v3ConfigDomain(a, c.chatBase(a)))
	req.Header.Set("X-Product", "SaaS")
	if ua == "" {
		ua = codeBuddyIDEUA
	}
	req.Header.Set("User-Agent", ua)
	c.injectCodeBuddyRequest(req)
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 2<<20))
	if err != nil {
		// Ошибка чтения → Ошибка транспортного уровня: оборванный body Не входит в парсинг (без штрафа для номера).
		return nil, fmt.Errorf("read body: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("v3/config status %d: %s", resp.StatusCode, truncate(string(raw), 120))
	}
	var env struct {
		Code int `json:"code"`
		Data struct {
			Models []dynModelEntry `json:"models"`
			// Баннер пробной модели: апстрим "N модели "дней бесплатного пробного периода» размещаются здесь,**не в data.models внутри**。
			// На практике global Боковой hy4-preview-f Встречается только здесь (modelId=hy4-preview-f、
			// targetModelId=hy4-preview、trialDays=14），Чистый data.models парсер его пропустит.
			ProductFeaturesConfig struct {
				ModelTrialBanner struct {
					Banners []struct {
						ModelID string `json:"modelId"`
						TargetModelID string `json:"targetModelId"`
					} `json:"banners"`
				} `json:"ModelTrialBanner"`
			} `json:"productFeaturesConfig"`
			ModelPromotions []v3ModelPromotion `json:"modelPromotions"`
		} `json:"data"`
	}
	if err := json.Unmarshal(raw, &env); err != nil {
		return nil, fmt.Errorf("v3/config parse: %w", err)
	}
	if env.Code != 0 {
		return nil, fmt.Errorf("v3/config code=%d", env.Code)
	}
	out := make(map[string]ModelInfo, len(env.Data.Models))
	for _, m := range env.Data.Models {
		if strings.TrimSpace(m.ID) == "" {
			continue
		}
		out[m.ID] = m.modelInfo()
	}
	// добавить trial-баннер модели (ModelTrialBanner）：Апстрим превращает "N модели "N-дневного бесплатного триала» только здесь,
	// data.models внутри нет, поэтому чисто каталоговый парсинг пропустит (фактически global Боковой hy4-preview-f Если так,
	// Но данная модель**Фактически вызываемый**）。
	//
	// Метрика метаданных: поле capabilities (context/maxTokens/efforts/reasoning и т.д.) из targetModelId
	// наследование существующих записей — триал и его целевой релиз — модели одного семейства, возможности должны совпадать;
	// Но **Credits и Tags Явная очистка**——Они описывают«После подтверждения«платежная и маркетинговая информация
	// （Например hy4-preview x0.29 и badge），Использование на бесплатном триале введёт в заблуждение отображение ниже по потоку.
	//
	// firstUseTimeKey / trialDays Атрибут**Уровень аккаунта**Пробный статус, не прокидывается downstream.
	for _, b := range env.Data.ProductFeaturesConfig.ModelTrialBanner.Banners {
		id := strings.TrimSpace(b.ModelID)
		if id == "" {
			continue
		}
		if _, exists := out[id]; exists {
			continue
		}
		mi := ModelInfo{ID: id}
		if tgt := strings.TrimSpace(b.TargetModelID); tgt != "" {
			if base, ok := out[tgt]; ok {
				mi = base
				mi.ID = id
			}
		}
		mi.Credits = ""
		mi.Tags = nil
		out[id] = mi
	}
	// подвешена текущая действующая лимитированная акция (modelPromotions）：Credits поле —**Курс**（Базис после штатного ввода
	// коэффициент, напр. hy4-preview-f x0.29），И WorkBuddy Клиент отображает действующую цену (пробная/
	// В окне скидки factor Скидка) — панель отображает на основе этого "действующую цену + Тег + курс».
	applyModelPromotions(out, env.Data.ModelPromotions)

	if len(out) == 0 {
		return nil, fmt.Errorf("v3/config returned empty models")
	}
	return out, nil
}

// UserResource Запрос баланса баллов и общего лимита аккаунта (агрегация всех тарифов).remain кламп отрицательных значений 0；
// total взять и remain Поле лимита из того же источника (CycleCapacitySize приоритет, без возврата квоты периода
// CapacitySize），Отсутствует апстрим size тариф по remain фолбэк, гарантия непревышения процента 100%。
// CreditPackage Детализация состава одного пакета баллов (для панели "Состав баллов»).
//
// Даже при одинаковом прогрессе заданий баланс двух аккаунтов может отличаться на тысячи — разница кроется в пакетах**номинал и
// Источник**в ("Пакет вирального роста для внутреннего рынка» "Пакет привлечения новых» выдаются поштучно, номинал 6~1500 не равно).
// По агрегированному значению это не видно, поэтому выводим детализацию по пакетам.
type CreditPackage struct {
	Name string `json:"name"`
	Remain int64 `json:"remain"`
	Used int64 `json:"used"`
	Size int64 `json:"size"`
	// EndTime Время окончания периода пакета (апстрим ExpiredTime / PackageEndTime / CycleEndTime
	// взять первое непустое поле по приоритету).
	EndTime string `json:"end_time,omitempty"`
	// ExpiresAt и EndTime Одноисточниковый Unix Метка времени в мс для агрегации на панели по точным оставшимся дням.
	ExpiresAt int64 `json:"expires_at,omitempty"`
	// CreatedAt Момент выдачи,RFC3339。**Единственный критерий различия "подарка за первый вход» и "награды за активность»**：
	// двух типов пакетов PackageName и PackageCode полностью идентичны (например, оба "пакет вирального роста для внутреннего рынка»+
	// TCACA_code_007_*），по имени не различить, только время показывает, было ли отправлено в момент первой авторизации аккаунта.
	CreatedAt string `json:"created_at,omitempty"`
	// PackageCode / SubProductCode Идентификатор типа пакета апстрима. То же Name Разное Code пакет может
	// — разные источники; одинаковый Code разные номиналы — поэтапная выдача из одного источника (первый вход 1500 И активность 300 именно так).
	PackageCode string `json:"package_code,omitempty"`
	SubProductCode string `json:"sub_product_code,omitempty"`
	SubProductName string `json:"sub_product_name,omitempty"`
	// Cycle для true Пакет периодической выдачи (чтение Cycle* поле), иначе читать Capacity*。
	Cycle bool `json:"cycle,omitempty"`
}

// CreditPackages Возвращает текущий попакетный состав аккаунта.remain/size суммирование по пакетам.
//
// выбор полей и UserResourceDetailed Единый критерий агрегации:CycleCapacitySize > 0 При ... по
// расчет по полю периода, иначе по Capacity подсчёт по полю — два пути нельзя смешивать, иначе один пакет будет учтён дважды.
func (c *Client) CreditPackages(a *auth.Auth) ([]CreditPackage, int64, int64, error) {
	now := time.Now()
	body := map[string]any{
		"PageNumber": 1,
		"PageSize": 100,
		"ProductCode": "p_tcaca",
		"Status": []int{0, 3},
		"PackageEndTimeRangeBegin": now.Format(packageEndLayout),
		"PackageEndTimeRangeEnd": now.Add(365 * 101 * 24 * time.Hour).Format(packageEndLayout),
	}
	data, err := c.billingMeterJSON(a, c.billingMeterPaths(a), http.MethodPost, body)
	if err != nil {
		return nil, 0, 0, err
	}
	// внимание на уровень:doJSON уже расшифровано apiEnvelope и вернуть env.Data，поэтому здесь с
	// Response Начать разбор —**нельзя**обернуть ещё одним слоем Code/Data，Иначе Accounts Всегда пусто,
	// проявляется как "каждый номер» 0 пакетов» (на практике наступали).
	var resp struct {
		Response struct {
			Data struct {
				Accounts []struct {
					PackageName string `json:"PackageName"`
					CapacityRemain int64 `json:"CapacityRemain"`
					CapacityUsed int64 `json:"CapacityUsed"`
					CapacitySize int64 `json:"CapacitySize"`
					CycleCapacityRemain int64 `json:"CycleCapacityRemain"`
					CycleCapacityUsed int64 `json:"CycleCapacityUsed"`
					CycleCapacitySize int64 `json:"CycleCapacitySize"`
					// Имя поля срока действия в апстриме имеет три варианта:ExpiredTime / PackageEndTime
					// В CN/global В полном наборе фактически измеренных полей всегда константа miss（См. UserResourceDetailed
					// комментарий в месте), фактически выдаётся CycleEndTime——читаются все три, используется тот, где есть значение.
					ExpiredTime string `json:"ExpiredTime"`
					PackageEndTime string `json:"PackageEndTime"`
					CycleEndTime string `json:"CycleEndTime"`
					// момент выдачи (epoch мс).
					CreateTime int64 `json:"CreateTime"`
					PackageCode string `json:"PackageCode"`
					SubProductCode string `json:"SubProductCode"`
					SubProductName string `json:"SubProductName"`
				} `json:"Accounts"`
			} `json:"Data"`
		} `json:"Response"`
	}
	if err := json.Unmarshal(data, &resp); err != nil {
		return nil, 0, 0, fmt.Errorf("packages parse: %w", err)
	}
	packs := resp.Response.Data.Accounts
	out := make([]CreditPackage, 0, len(packs))
	var sumRemain, sumSize int64
	for _, p := range packs {
		cp := CreditPackage{
			Name: p.PackageName,
			PackageCode: p.PackageCode,
			SubProductCode: p.SubProductCode,
			SubProductName: p.SubProductName,
		}
		switch {
		case p.ExpiredTime != "":
			cp.EndTime = p.ExpiredTime
		case p.PackageEndTime != "":
			cp.EndTime = p.PackageEndTime
		default:
			cp.EndTime = p.CycleEndTime
		}
		if cp.EndTime != "" {
			if end, perr := time.ParseInLocation(packageEndLayout, cp.EndTime, softRateResetLoc); perr == nil {
				cp.ExpiresAt = end.UnixMilli()
			}
		}
		// CreateTime Да epoch мс;0 означает, что апстрим не передал, оставить пустым, а не подделывать 1970。
		if p.CreateTime > 0 {
			cp.CreatedAt = time.UnixMilli(p.CreateTime).Format(time.RFC3339)
		}
		if p.CycleCapacitySize > 0 {
			cp.Cycle = true
			cp.Remain, cp.Size = p.CycleCapacityRemain, p.CycleCapacitySize
			cp.Used = cp.Size - cp.Remain
			if p.CycleCapacityUsed > cp.Used {
				cp.Used = p.CycleCapacityUsed
				cp.Remain = cp.Size - cp.Used
			}
			if cp.Remain < 0 {
				cp.Remain = 0
			}
		} else {
			cp.Remain, cp.Used, cp.Size = p.CapacityRemain, p.CapacityUsed, p.CapacitySize
			if cp.Used == 0 && cp.Size > cp.Remain {
				cp.Used = cp.Size - cp.Remain
			}
		}
		sumRemain += cp.Remain
		sumSize += cp.Size
		out = append(out, cp)
	}
	// сортировка по номиналу по убыванию: крупные пакеты видны сразу — именно там вероятнее расхождения.
	sort.SliceStable(out, func(i, j int) bool { return out[i].Size > out[j].Size })
	return out, sumRemain, sumSize, nil
}

func (c *Client) UserResource(a *auth.Auth) (remain, total int64, err error) {
	remain, total, _, err = c.UserResourceDetailed(a, 0)
	return remain, total, err
}

// packageEndLayout Формат wall-clock истечения тарифа апстрима (UTC+8，и softRateResetLoc в той же метрике).
const packageEndLayout = "2006-01-02 15:04:05"

// parsePackageEndTime Единый парсинг времени истечения тарифа апстрима. Пустое значение, ошибка формата — возврат false，
// Вызывающая сторона консервативно не включает этот пакет в маршрут с самым ранним истечением.
func parsePackageEndTime(raw string) (time.Time, bool) {
	if raw == "" {
		return time.Time{}, false
	}
	t, err := time.ParseInLocation(packageEndLayout, raw, softRateResetLoc)
	if err != nil {
		return time.Time{}, false
	}
	return t, true
}

// UserResourceDetailed В UserResource дополнительно вернуть подмножество баллов "скоро истекает»:
// soon > 0 и тариф CycleEndTime Парсинг успешен и момент истечения ≤ now+soon баланс зачисляется в expiring
// （pool приоритетное списание по этому признаку, чтобы бонусные баллы за официальные акции не сгорели);soon ≤ 0 Время expiring
// Конст. 0（бакетинг отключен, поведение как до внедрения).expiring Да remain часть.
//
// критерий времени истечения — CycleEndTime（Факт апстрима:CN/global Во всех полях обоих доменов отсутствует PackageEndTime，
// Старый критерий всегда miss Вызвать expiring Конст. 0；CycleEndTime — реальное время истечения, выданное апстримом —
// global Bonus Pack 14 срок истечения подарочных баллов за дни — это поле). Ошибка парсинга/отсутствующий тариф — консервативно
// Не учитывается expiring（чтобы ошибочно не пометить как скоро истекающее и не поднять в очереди).
// получение данных одного пакета — единый вызов packageRemainUsed（и CreditPackages единый источник истины, включая remain
// Зажим [0,size] и used Исправление; устранение дрейфа дублирующей логики — старый промежуточный switch Клампить только отрицательные, грязные данные upstream
// CycleRemain>Size будет завышено).
func (c *Client) UserResourceDetailed(a *auth.Auth, soon time.Duration) (remain, total, expiring int64, err error) {
	remain, total, expiring, _, _, err = c.UserResourceDetailedWithExpiry(a, soon)
	return remain, total, expiring, err
}

// UserResourceDetailedWithExpiry В UserResourceDetailed на этой основе вернуть ближайшую будущую партию с истечением:
// earliestAt это самый ранний доступный момент истечения,earliestRemaining Сумма остатков всех пакетов с положительным балансом на один момент времени.
// уже истёк, остаток — 0、Пакеты без срока годности или с непарсируемым сроком не считаются самой ранней партией; при отсутствии валидных партий возвращается ноль.
func (c *Client) UserResourceDetailedWithExpiry(a *auth.Auth, soon time.Duration) (remain, total, expiring int64, earliestAt time.Time, earliestRemaining int64, err error) {
	now := time.Now()
	body := map[string]any{
		"PageNumber": 1,
		"PageSize": 100,
		"ProductCode": "p_tcaca",
		"Status": []int{0, 3},
		"PackageEndTimeRangeBegin": now.Format(packageEndLayout),
		"PackageEndTimeRangeEnd": now.Add(365 * 101 * 24 * time.Hour).Format(packageEndLayout),
	}
	// Запрос баланса также с ограниченным ретраем транзиентных ошибок (сразу после чекина user-resource спорадически 500 Приведёт к
	// Данный аккаунт пропустил текущую разморозку/обновление снапшота с истёкшим сроком, только ждать следующего цикла обновления).
	var data json.RawMessage
	err = c.retryBillingTransient(func() error {
		var e error
		data, e = c.billingMeterJSON(a, c.billingMeterPaths(a), http.MethodPost, body)
		return e
	})
	if err != nil {
		return 0, 0, 0, time.Time{}, 0, err
	}
	var resp struct {
		Response struct {
			Data struct {
				Accounts []struct {
					PackageName string `json:"PackageName"`
					CycleEndTime string `json:"CycleEndTime"` // »2006-01-02 15:04:05"，по умолчанию/пустой = Без срока действия
					CapacitySize int64 `json:"CapacitySize"`
					CapacityRemain int64 `json:"CapacityRemain"`
					CapacityUsed int64 `json:"CapacityUsed"`
					CycleCapacitySize int64 `json:"CycleCapacitySize"`
					CycleCapacityRemain int64 `json:"CycleCapacityRemain"`
					CycleCapacityUsed int64 `json:"CycleCapacityUsed"`
				} `json:"Accounts"`
			} `json:"Data"`
		} `json:"Response"`
	}
	if err := json.Unmarshal(data, &resp); err != nil {
		return 0, 0, 0, time.Time{}, 0, fmt.Errorf("resource parse: %w", err)
	}
	for _, acct := range resp.Response.Data.Accounts {
		r, _, size := packageRemainUsed(respAccount{
			CapacityRemain: acct.CapacityRemain,
			CapacityUsed: acct.CapacityUsed,
			CapacitySize: acct.CapacitySize,
			CycleCapacityRemain: acct.CycleCapacityRemain,
			CycleCapacityUsed: acct.CycleCapacityUsed,
			CycleCapacitySize: acct.CycleCapacitySize,
		})
		if r < 0 {
			r = 0
		}
		if size < r {
			size = r
		}
		remain += r
		total += size
		if r <= 0 {
			continue
		}
		end, ok := parsePackageEndTime(acct.CycleEndTime)
		if !ok || !end.After(now) {
			continue
		}
		if earliestAt.IsZero() || end.Before(earliestAt) {
			earliestAt = end
			earliestRemaining = r
		} else if end.Equal(earliestAt) {
			earliestRemaining += r
		}
		// Бакетизация: только soon>0 и действительно внутри окна → expiring。
		if soon > 0 && !end.After(now.Add(soon)) {
			expiring += r
		}
	}
	return remain, total, expiring, earliestAt, earliestRemaining, nil
}

// respAccount Подача packageRemainUsed Парсимое поле тарифа (CreditPackages попакетная структура изоморфна).
type respAccount struct {
	CapacityRemain int64
	CapacityUsed int64
	CapacitySize int64
	CycleCapacityRemain int64
	CycleCapacityUsed int64
	CycleCapacitySize int64
}

// packageRemainUsed Агрегированного одиночного тарифа remain/used/size（и CreditPackages/cmd/credit 
// Для консистентности исторического учета сведено к единому источнику истины).Cycle приоритет у срочного пакета: использовать CycleCapacity
// три поля,used получить CycleUsed и size-remain большее из них; иначе откат Capacity три поля.
func packageRemainUsed(a respAccount) (remain, used, size int64) {
	if a.CycleCapacitySize > 0 {
		remain = a.CycleCapacityRemain
		size = a.CycleCapacitySize
		if remain < 0 {
			remain = 0
		}
		if remain > size {
			remain = size
		}
		used = size - remain
		if a.CycleCapacityUsed > used {
			used = a.CycleCapacityUsed
			if size >= used {
				remain = size - used
			}
		}
		return remain, used, size
	}
	remain = a.CapacityRemain
	used = a.CapacityUsed
	size = a.CapacitySize
	if used == 0 && size > remain {
		used = size - remain
	}
	return remain, used, size
}

// DailyCheckin Выполнить ежедневный check-in. Уже отмечено (бизнес code не 0）также возвращает ошибку, вызывающая сторона по msg Различение.
// спорадический апстрим 5xx（code 10000）Делать ограниченный ретрай (см. retryBillingTransient）——однократный джиттер больше не
// иначе аккаунт пропустит отметку на весь день; бизнес-ошибки типа "уже отмечено» не ретраить.
func (c *Client) DailyCheckin(a *auth.Auth) error {
	return c.retryBillingTransient(func() error {
		_, err := c.billingMeterJSON(a, c.checkinMeterPaths(a), http.MethodPost, map[string]any{})
		return err
	})
}

// IsAlreadyCheckin отчет err означает ли"Сегодня уже отмечено"（upstream идемпотентно отклоняет повторный check-in).
// только с категорией *Error（Бизнес code Или HTTP ошибка): сетевой уровень/Ошибка уровня парсинга не считается идемпотентным успехом,
// иначе догнавшая отметка после простоя при джиттере будет ошибочно учтена как already，аккаунт фактически не отметился за день, но признан нормальным.
func IsAlreadyCheckin(err error) bool {
	var ue *Error
	if !errors.As(err, &ue) {
		return false
	}
	for _, m := range alreadyCheckinMarkers {
		if strings.Contains(ue.Msg, m) || strings.Contains(strings.ToLower(ue.Msg), strings.ToLower(m)) {
			return true
		}
	}
	return false
}

func truncate(s string, n int) string {
	return logfmt.Truncate(s, n)
}
