// logging.go табличный лог уровня запроса: каждый /v1/chat/completions После завершения запроса вывести строку в stdout。
package server

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strings"
	"sync/atomic"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/logfmt"
	"github.com/linguo2625469/workbuddy2api-panel/internal/pool"
	"github.com/linguo2625469/workbuddy2api-panel/internal/reqlog"
	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
)

// maxUserAgentLen архив и отображение на панели сохраняют UA Лимит в байтах.UA полностью контролируется клиентом
// Свободный текст (браузер часто 150+ символов, вредоносный клиент может вставить несколько KB），Перед сбросом на диск необходимо усечение,
// Иначе один запрос может раздуть архивную строку. Усечение влияет только на отображение, не на обработку запроса.
const maxUserAgentLen = 200

// chatSeq Порядковый номер запроса на уровне процесса.
var chatSeq atomic.Int64

// chatLogEnabled Главный переключатель логов таблицы чатов. На проде всегда true；
// Тестовый пакет через TestMain Установить false Закрыть stdout шум, нужен тест с ассертом вывода строк withChatLog временное включение (R5）。
var chatLogEnabled = true

// chatLogOut Цель вывода табличного лога чата. По умолчанию в проде os.Stdout；main При включении панели управления
// Через SetChatLogOutput инжект MultiWriter，Зеркалировать каждую строку в /panel/api/logs кольцевой буфер,
// stdout Поведение не меняется. Вызвать один раз до старта сервиса (без окна гонки).
var chatLogOut io.Writer = os.Stdout

// SetChatLogOutput Заменить цель вывода логов таблицы чата (только main вызывается один раз на старте).
func SetChatLogOutput(w io.Writer) { chatLogOut = w }

// chatStat одиночный chat Статистика логов запросов;handler Монтировать defer，после выхода запроса логировать строку.
type chatStat struct {
	start time.Time
	model string
	mode string // "stream" | "sync"
	uid string // Полный uid，При отображении брать только первые 8 бит
	nick string // ник аккаунта (синхронно с выбором), строка пайплайна проходит logfmt.Label Склеить в "никнейм(uid8)"
	ttfb time.Duration
	toks int // <0 обозначает usage отсутствует → Отображение "-"
	status int
	requestID string
	outcome string
	attempts int
	credit float64
	hasCredit bool
	promptTokens int64
	completionTokens int64
	totalTokens int64

	// источник вызова (клиент IP / User-Agent）。пустой = не собрано (logging.request_client_info
	// Закрыто, или не chat путь), на уровне отображения всегда как "-" Фолбэк.
	clientIP string
	userAgent string

	logged bool
}

// newChatStat Вход по запросу handler момент как начало для построения объекта статистики;toks По умолчанию -1（usage отсутствует).
func newChatStat(now time.Time, body []byte, stream bool) *chatStat {
	mode := "sync"
	if stream {
		mode = "stream"
	}
	return &chatStat{start: now, model: parseModelFromBody(body), mode: mode, toks: -1}
}

// done Идемпотентно пишет одну строку табличного лога.
func (s *chatStat) done() {
	if s.logged {
		return
	}
	s.logged = true
	logChatRowEx(s.ttfb, time.Since(s.start), s.model, s.mode, s.uid, s.nick, s.status, s.toks,
		s.requestID, s.outcome, s.attempts, s.credit, s.hasCredit, s.clientIP, s.userAgent)
}

// chatStatsReader Захват при стриминговой прокси-передаче SSE последнего кадра usage.completion_tokens точное значение,
// и записать первый data кадра TTFB；Исходные байты возвращаются downstream как есть (прозрачно).
// внимание: не делать rune оценка,token числа полностью берутся из апстрима usage。
type chatStatsReader struct {
	br *bufio.Reader
	start time.Time
	ttfb time.Duration
	seen bool // уже встречался первый data Кадр (TTFB учитывается только один раз)
	promptTokens int
	completionTokens int
	totalTokens int
	hasPromptTokens bool
	hasCompletionTokens bool
	hasTotalTokens bool
	// credit последний фрейм upstream usage.credit（фактически списанные баллы за этот раз), для учёта стоимости (NoteModelCost）。
	hasCredit bool
	credit float64
	errorFrame bool
	pend []byte // Кэш прочитанных, но не возвращенных строк
}

// newChatStatsReaderSince по since для TTFB Начало отсчёта (обычно вход запроса в handler момент).
func newChatStatsReaderSince(r io.Reader, since time.Time) *chatStatsReader {
	return &chatStatsReader{br: bufio.NewReaderSize(r, 64*1024), start: since}
}

// TTFB Вернуть первый data время доставки кадра; при отсутствии кадра — 0。
func (s *chatStatsReader) TTFB() time.Duration { return s.ttfb }

// Tokens вернуть последний кадр usage.completion_tokens и отсутствие; отсутствует usage Время ok=false。
func (s *chatStatsReader) Tokens() (int, bool) { return s.completionTokens, s.hasCompletionTokens }

// Credit вернуть последний кадр usage.credit（фактически списанные баллы за этот раз) и наличие пропуска.
func (s *chatStatsReader) Credit() (float64, bool) { return s.credit, s.hasCredit }

// TotalTokens вернуть последний кадр usage.total_tokens И наличие/отсутствие.
func (s *chatStatsReader) TotalTokens() (int, bool) { return s.totalTokens, s.hasTotalTokens }

// Usage вернуть уже полученное в стриминговом ответе token usage Поле.
func (s *chatStatsReader) Usage() pool.TokenUsageDelta {
	return pool.TokenUsageDelta{
		HasPromptTokens: s.hasPromptTokens,
		PromptTokens: int64(s.promptTokens),
		HasCompletionTokens: s.hasCompletionTokens,
		CompletionTokens: int64(s.completionTokens),
		HasTotalTokens: s.hasTotalTokens,
		TotalTokens: int64(s.totalTokens),
	}
}

// parseSSELine парсить строку "data: {...}"：Первый кадр — запись TTFB，Содержит usage тогда доверять точному completion_tokens。
func (s *chatStatsReader) parseSSELine(line string) {
	line = strings.TrimRight(line, "\r\n")
	if !strings.HasPrefix(line, "data: ") {
		return
	}
	payload := strings.TrimPrefix(line, "data: ")
	if payload == "[DONE]" {
		return
	}
	if !s.seen {
		s.seen = true
		s.ttfb = time.Since(s.start)
	}
	var chunk struct {
		Error json.RawMessage `json:"error"`
		Usage *struct {
			PromptTokens *int `json:"prompt_tokens"`
			CompletionTokens *int `json:"completion_tokens"`
			TotalTokens *int `json:"total_tokens"`
			Credit *float64 `json:"credit"`
		} `json:"usage"`
	}
	if json.Unmarshal([]byte(payload), &chunk) != nil || chunk.Usage == nil {
		if json.Unmarshal([]byte(payload), &chunk) == nil && len(chunk.Error) > 0 {
			s.errorFrame = true
		}
		return
	}
	if len(chunk.Error) > 0 {
		s.errorFrame = true
	}
	if chunk.Usage.PromptTokens != nil {
		s.hasPromptTokens = true
		s.promptTokens = *chunk.Usage.PromptTokens
	}
	if chunk.Usage.CompletionTokens != nil {
		s.hasCompletionTokens = true
		s.completionTokens = *chunk.Usage.CompletionTokens
	}
	if chunk.Usage.TotalTokens != nil {
		s.hasTotalTokens = true
		s.totalTokens = *chunk.Usage.TotalTokens
	}
	if chunk.Usage.Credit != nil {
		s.hasCredit = true
		s.credit = *chunk.Usage.Credit
	}
}

// SawErrorFrame было ли прокинуто в потоке отчётов SSE error Кадр.
func (s *chatStatsReader) SawErrorFrame() bool { return s.errorFrame }

// Read возврат исходных данных с одновременным разбором статистики TTFB/token。
func (s *chatStatsReader) Read(p []byte) (int, error) {
	if len(s.pend) > 0 {
		n := copy(p, s.pend)
		s.pend = s.pend[n:]
		return n, nil
	}
	line, err := s.br.ReadString('\n')
	if line != "" {
		s.parseSSELine(line)
		s.pend = []byte(line)
		n := copy(p, s.pend)
		s.pend = s.pend[n:]
		return n, nil
	}
	return 0, err
}

// rewriteModel взять outbound chat body model поле заменяется на bare（остальные поля сохранить как есть).
// только если bare != Исходный model в момент — от chatCompletions вызов;body при невозможности парсинга вернуть как есть (без повторной ошибкизации).
func rewriteModel(body []byte, bare string) []byte {
	if len(body) == 0 || bare == "" {
		return body
	}
	var obj map[string]any
	if err := json.Unmarshal(body, &obj); err != nil {
		return body
	}
	if cur, ok := obj["model"].(string); !ok || cur == bare {
		return body
	}
	obj["model"] = bare
	out, err := json.Marshal(obj)
	if err != nil {
		return body
	}
	return out
}

// parseModelFromBody Из запроса JSON получить model поле, по умолчанию помечается "-"。
func parseModelFromBody(body []byte) string {
	var obj struct {
		Model string `json:"model"`
	}
	if err := json.Unmarshal(body, &obj); err != nil || obj.Model == "" {
		return "-"
	}
	return obj.Model
}

// usageDeltaFromResponse Извлечь явно существующее из нестримингового агрегированного ответа token Поле.
func usageDeltaFromResponse(resp map[string]any) pool.TokenUsageDelta {
	delta := pool.TokenUsageDelta{}
	u, ok := resp["usage"].(map[string]any)
	if !ok {
		return delta
	}
	read := func(key string) (int64, bool) {
		v, ok := u[key]
		if !ok {
			return 0, false
		}
		switch n := v.(type) {
		case float64:
			return int64(n), true
		case float32:
			return int64(n), true
		case int:
			return int64(n), true
		case int64:
			return n, true
		case json.Number:
			i, err := n.Int64()
			return i, err == nil
		default:
			return 0, false
		}
	}
	if n, ok := read("prompt_tokens"); ok {
		delta.HasPromptTokens, delta.PromptTokens = true, n
	}
	if n, ok := read("completion_tokens"); ok {
		delta.HasCompletionTokens, delta.CompletionTokens = true, n
	}
	if n, ok := read("total_tokens"); ok {
		delta.HasTotalTokens, delta.TotalTokens = true, n
	}
	return delta
}

// completionTokens Из Aggregate извлечь из возвращенного ответа usage.completion_tokens；При отсутствии вернуть -1。
func completionTokens(resp map[string]any) int {
	u, ok := resp["usage"].(map[string]any)
	if !ok {
		return -1
	}
	v, ok := u["completion_tokens"].(float64)
	if !ok {
		return -1
	}
	return int(v)
}

// uidPrefix показывать только uid Перед 8 бит; пусто uid Отображение "-"。
//
// Делегирование реализации logfmt.UID8，Избежать "Перехват 8 бит" Правила в server и logfmt дублирование записи в двух местах искажает.
func uidPrefix(uid string) string {
	return logfmt.UID8(uid)
}

type requestTraceKey struct{}

// requestTrace за один chat Общий идентификатор внутри запроса и итоговая статистика,ServeHTTP единый учет на выходе.
type requestTrace struct {
	id string
	start time.Time
	stat *chatStat
	// источник вызова, вход в handler единоразовый сбор в момент (см. ServeHTTP / captureClientInfo）。
	clientIP string
	userAgent string
}

// captureClientInfo Сбор источника вызова (клиент IP + После усечения UA）。При выкл. переключателе сохранять пустую строку:
// Информация об источнике чем token Чувствительно к подсчёту, запись на диск определяется logging.request_client_info Решение.
func (t *requestTrace) captureClientInfo(r *http.Request) {
	if t == nil || r == nil {
		return
	}
	t.clientIP = clientIPForLog(r)
	t.userAgent = logfmt.Truncate(r.UserAgent(), maxUserAgentLen)
}

// clientIPForLog извлечь клиента для отображения в логах IP。
//
// и upstream.ExtractClientIP различие: последний распознает только прокси-заголовок (X-Forwarded-For первый сегмент →
// X-Real-IP），т.к. его назначение — прокинуть клиент IP **прозрачно прокинуть апстриму**，откат на собственный адрес шлюза приведёт к
// загрязняет критерии апстрим-риск-контроля; в сценарии логов наоборот — при прямом подключении (без обратного прокси) RemoteAddr — единственная зацепка,
// необходим откат, иначе все источники на панели показывают "-"。Заголовок прокси приоритетно гарантирует получение реального клиента после reverse proxy.
func clientIPForLog(r *http.Request) string {
	if r == nil {
		return ""
	}
	if ip := upstream.ExtractClientIP(r); ip != "" {
		return ip
	}
	host, _, err := net.SplitHostPort(strings.TrimSpace(r.RemoteAddr))
	if err != nil {
		return strings.TrimSpace(r.RemoteAddr)
	}
	return host
}

// dashIfEmpty Пустые строки отображаются единообразно "-«（если поле источника не собрано, пустой столбец не оставлять).
func dashIfEmpty(s string) string {
	if strings.TrimSpace(s) == "" {
		return "-"
	}
	return s
}

func requestTraceFrom(r *http.Request) *requestTrace {
	if r == nil {
		return nil
	}
	tr, _ := r.Context().Value(requestTraceKey{}).(*requestTrace)
	return tr
}

func (t *requestTrace) event(status int) reqlog.Event {
	e := reqlog.Event{
		Time: t.start,
		RequestID: t.id,
		Path: "/v1/chat/completions",
		Status: status,
	}
	duration := time.Since(t.start)
	e.DurationMs = duration.Milliseconds()
	if e.DurationMs < 1 {
		e.DurationMs = 1
	}
	if t.stat != nil {
		s := t.stat
		e.Account = logfmt.Label(s.uid, s.nick)
		e.Model = s.model
		e.Outcome = s.outcome
		e.TTFBMs = s.ttfb.Milliseconds()
		e.Attempts = s.attempts
		e.PromptTokens = s.promptTokens
		e.CompletionTokens = s.completionTokens
		e.TotalTokens = s.totalTokens
		e.Credit = s.credit
		e.HasCredit = s.hasCredit
	}
	e.ClientIP = t.clientIP
	e.UserAgent = t.userAgent
	if e.Outcome == "" {
		if status >= 200 && status < 300 {
			e.Outcome = reqlog.OutcomeSuccess
		} else {
			e.Outcome = reqlog.OutcomeHTTPError
		}
	}
	e.OK = status >= 200 && status < 300 && e.Outcome == reqlog.OutcomeSuccess
	return e
}

// responseObserver Захват handler фактически записанное HTTP Состояние, с сохранением Flusher/Unwrap，
// Во избежание нарушения SSE покадровое обновление.
type responseObserver struct {
	http.ResponseWriter
	status int
}

func (o *responseObserver) WriteHeader(code int) {
	if o.status == 0 {
		o.status = code
	}
	o.ResponseWriter.WriteHeader(code)
}

func (o *responseObserver) Write(p []byte) (int, error) {
	if o.status == 0 {
		o.status = http.StatusOK
	}
	return o.ResponseWriter.Write(p)
}

func (o *responseObserver) Flush() {
	if f, ok := o.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func (o *responseObserver) Unwrap() http.ResponseWriter { return o.ResponseWriter }

// Фиксированная ширина столбца строки лога запросов (ширина отображения, не байты). Брать фиксированную ширину, а не растягивать по естественной длине контента,
// Чтобы stdout внутри сотни-тысячи строк можно сканировать вертикально — иначе разная длина имён моделей, китайские никнеймы дополняются пробелами по байтам
// смещение, невозможно визуально выровнять по столбцам (именно это в прошлой версии 11 проблема, решаемая жесткой обрезкой по байтам).
const (
	// chatModelWidth перекрытие realm Префикс + Самое длинное имя модели:«global:« (7) + "deepseek-v4.1-flash« (19) = 26。
	// Старый 11 усечение по байтам разрежет "cn:deepseek-v4-flash« Разбить на "cn:deepseek«，Можно принять за другую модель.
	chatModelWidth = 26
	// chatAcctWidth Вмещать "никнейм(uid8)«：Китайские никнеймы по 2 Список/считать по символам,5 символов на китайском + "(xxxxxxxx)" = 20 Столбец.
	chatAcctWidth = 22
	chatTTFBWidth = 8
	chatTokWidth = 6
	chatRateWidth = 11 // вида "183.6tok/s"
)

// logChatRow Печать одной строки табличного лога на запрос (вывод chatLogOut，отсутствует log префикс метки времени).
//
// Параметры:
// - model：имя модели (вкл. realm префикс), превышение chatModelWidth усечение (имя модели — ASCII，усечение по байтам = ширина колонки);
// - uid/nick：Полный uid и ник аккаунта, через logfmt.Label Склеить в "никнейм(uid8)" отображение — только
// uid8 визуально невозможно определить аккаунт, для идентификации нужен доп. запрос auths/，диагностика + один хоп;
// - toks<0 обозначает usage Отсутствует, показать "-"。
func logChatRow(ttfb, total time.Duration, model, mode, uid, nick string, status int, toks int) {
	logChatRowEx(ttfb, total, model, mode, uid, nick, status, toks, "", "", 0, 0, false, "", "")
}

// logChatRowEx это запрос с ID、Расширенная строка лога с полями результата, повтора, баллов и источника вызова. Старые вызовы сохраняются
// исходный формат;requestID расширенные поля добавляются только если не пусто; если оба параметра источника пусты — сегмент источника не добавляется.
func logChatRowEx(ttfb, total time.Duration, model, mode, uid, nick string, status int, toks int,
	requestID, outcome string, attempts int, credit float64, hasCredit bool,
	clientIP, userAgent string) {
	if !chatLogEnabled {
		return
	}
	seq := chatSeq.Add(1)
	model = logfmt.Pad(logfmt.Truncate(model, chatModelWidth), chatModelWidth)
	// тег аккаунта только дополняется, не усекается: при переполнении лучше расширить строку, чем потерять ник (ник — главный ключ для диагностики).
	acct := logfmt.Pad(logfmt.Label(uid, nick), chatAcctWidth)
	tokField := "-"
	tokpsField := "-"
	if toks >= 0 {
		tokField = fmt.Sprintf("%d", toks)
		if total > 0 {
			tokpsField = fmt.Sprintf("%.1ftok/s", float64(toks)/total.Seconds())
		} else {
			tokpsField = "0.0tok/s"
		}
	}
	ttfbMS := "-"
	if ttfb > 0 {
		ttfbMS = fmt.Sprintf("%dms", ttfb.Milliseconds())
	}
	extra := ""
	if requestID != "" {
		if outcome == "" {
			outcome = reqlog.OutcomeHTTPError
			if status >= 200 && status < 300 {
				outcome = reqlog.OutcomeSuccess
			}
		}
		creditField := "-"
		if hasCredit {
			creditField = fmt.Sprintf("%.4f", credit)
		}
		extra = fmt.Sprintf(" rid=%s | out=%s | try=%d | credit=%s |", requestID, outcome, attempts, creditField)
	}
	// Источник вызова:IP Использовать парсимое raw-значение (для удобства grep），UA использовать ShortUA Сжатый тег клиента
	// и заключить в кавычки (внутри тега возможны пробелы, напр. `OpenAI/Python 1.30.0` будет получено только OpenAI/Python）。
	// Если оба не собраны — не добавлять, формат старой строки без изменений.
	src := ""
	if clientIP != "" || userAgent != "" {
		ua := "-"
		if s := logfmt.ShortUA(userAgent); s != "" {
			ua = `"` + s + `"`
		}
		src = fmt.Sprintf(" src=%s ua=%s |", dashIfEmpty(clientIP), ua)
	}
	fmt.Fprintf(chatLogOut, "| #%03d | %s | %s | %s | %d | %s | TTFB=%s | tok=%s | %s | total=%.1fs |%s%s\n",
		seq,
		time.Now().Format("15:04:05"),
		model,
		mode,
		status,
		acct,
		logfmt.Pad(ttfbMS, chatTTFBWidth),
		logfmt.Pad(tokField, chatTokWidth),
		logfmt.Pad(tokpsField, chatRateWidth),
		total.Seconds(),
		extra,
		src,
	)
}
