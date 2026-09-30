// report.go growth домен интерфейса "отчёт об активности диалога»:POST {billingBase}/v2/report。
// Копировать как в клиенте chat_request_send Форма события (вкл. conversationId/mode/inputLength и все поля,
// Не использовать минимальный 3 поле, защита от будущего ужесточения апстрима). Событие должно содержать userId（=Аккаунт uid），при отсутствии — сервер
// 200 но тихо отбрасывается (на практике см. REPORT-active-map.md §2）。
//
// один репорт одновременно подсвечивает growth Последовательный вход + Разблокировать first_buddy Задача (пререквизит усыновления).
// Метрика риск-контроля: на аккаунт в сутки 1 раза достаточно (activity_hours одна точка времени), без высокочастотной отчётности по многим точкам.
package upstream

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
)

// reportPath канал отчёта об активности (проверено на практике).
const reportPath = "/v2/report"

// billingJSON Отправка billing Домен (billingBase，codebuddy.cn）Запросить и распаковать конверт;body для nil без тела запроса.
// и travel.go growthJSON Симметрично (growth Домен идет через chatBase + BillingHeaders；billing Домен идет через billingBase）。
// report/checkin и т.д. billing Эндпоинты общие: заголовки запроса унифицированы BillingHeaders，Конверт и семантика ошибок аналогичны doJSON。
func (c *Client) billingJSON(a *auth.Auth, method, path string, body any) (json.RawMessage, error) {
	var rdr io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		rdr = bytes.NewReader(raw)
	}
	req, err := http.NewRequest(method, c.billingBase(a)+path, rdr)
	if err != nil {
		return nil, err
	}
	c.BillingHeaders(req, a)
	return c.doJSON(req)
}

// billingMeterJSON только для /billing/meter Эндпоинт семейства (get-user-resource / daily-checkin）
// Нажать realm Двойной путь fallback：global сначала нет /v2 префикс,ErrNotFound при вторичной замене есть /v2 Префикс
// （расхождение старого/нового пути апстрима);cn Один путь (есть /v2）Статус без изменений. Только global realm только тогда есть несколько путей.
func (c *Client) billingMeterJSON(a *auth.Auth, paths []string, method string, body any) (json.RawMessage, error) {
	var lastErr error
	for i, path := range paths {
		data, err := c.billingJSON(a, method, path, body)
		if err == nil {
			return data, nil
		}
		lastErr = err
		// Только 404 Смена пути (имеет смысл только если путь не существует fallback）；Остальные ошибки возвращаются напрямую.
		var ue *Error
		if !errors.As(err, &ue) || ue.Kind != ErrNotFound || i == len(paths)-1 {
			return nil, err
		}
	}
	return nil, lastErr
}

// billingRetryDelay check-in/базовый интервал ретрая при транзиентной ошибке биллинговых вызовов обслуживания типа баланса.
// Отдельная переменная для сокращения в тестах (в проде фиксировано 2s：№ 1 повторные попытки и т.д. 2s、№ 2 вторичный и т.п. 4s）。
var billingRetryDelay = 2 * time.Second

// isTransientBillingErr отчет err Стоит ли делать ограниченный ретрай для биллинговых/сервисных вызовов:
// апстрим 5xx（ErrServer，На практике встречается эпизодически "code 10000 / API request failed with status
// code: 500"）или сетевая ошибка (не *Error сбой передачи). Бизнес-ошибка (code!=0 
// Уже отмечено/Ошибка параметра,4xx、ограничение) не ретраить — ретрай лишь повторно провалится так же.
func isTransientBillingErr(err error) bool {
	if err == nil {
		return false
	}
	var ue *Error
	if errors.As(err, &ue) {
		return ue.Kind == ErrServer
	}
	return true
}

// retryBillingTransient Для чекина/для низкочастотных служебных вызовов типа баланса — ограниченный ретрай при транзиентной ошибке:
// макс. допробить 2 раз (интервал 2s、4s），При первом успехе или нетранзиентной ошибке — немедленный возврат.chat Горячий путь
// не использовать эту стратегию — у неё своя семантика ротации аккаунтов, ретрай усилит запросы в полёте.
func (c *Client) retryBillingTransient(fn func() error) error {
	err := fn()
	if err == nil || !isTransientBillingErr(err) {
		return err
	}
	for i := 1; i <= 2; i++ {
		time.Sleep(time.Duration(i) * billingRetryDelay)
		if err = fn(); err == nil || !isTransientBillingErr(err) {
			return err
		}
	}
	return err
}

// chatRequestEvent Клиент chat_request_send полная форма события (с probe_active.py chat_event Выравнивание).
// userId — обязательное поле (= a.UID）；conversationId Генерируется вызывающей стороной, реальная сессия не требуется.
type chatRequestEvent struct {
	EventCode string `json:"eventCode"`
	Timestamp int64 `json:"timestamp"`
	ReportDelay int `json:"reportDelay"`
	Mode string `json:"mode"`
	ConversationID string `json:"conversationId"`
	RequestID string `json:"requestId"`
	InputLength int `json:"inputLength"`
	RequestModelID string `json:"requestModelId"`
	RequestModelName string `json:"requestModelName"`
	IsPlan bool `json:"isPlan"`
	IsAutoExecuteTerminal bool `json:"isAutoExecuteTerminal"`
	IsAutoModify bool `json:"isAutoModify"`
	CodebaseEnable bool `json:"codebaseEnable"`
	MaxToken int `json:"maxToken"`
	MaxSteps int `json:"maxSteps"`
	Temperature int `json:"temperature"`
	MaxRetries int `json:"maxRetries"`
	MentionContexts []any `json:"mentionContexts"`
	KnowledgeID []any `json:"knowledgeId"`
	KnowledgeName []any `json:"knowledgeName"`
	CodebaseID string `json:"codebaseId"`
	MentionContextCount int `json:"mentionContextCount"`
	Command string `json:"command"`
	ExpertID string `json:"expertId"`
	RecommendID string `json:"recommendId"`
	SkillID string `json:"skillId"`
	SkillCount int `json:"skillCount"`
	TotalCount int `json:"totalCount"`
	FileURI string `json:"fileUri"`
	PresentAt int64 `json:"presentAt"`
	TraceID string `json:"traceId"`
	RootRequestID string `json:"rootRequestId"`
	ParentConversationID string `json:"parentConversationId"`
	AgentName string `json:"agentName"`
	AgentType string `json:"agentType"`
	UserID string `json:"userId"`
}

// ReportChatActivity Отправить апстриму отчет об активности диалога (chat_request_send）。
// conversationID Генерируется вызывающей стороной (напр. wb2api-<ms>），Реальная сессия не требуется — сервер не проверяет консистентность.
// requestID Уникальный идентификатор текущего запроса (при многораундовой отправке в одной сессии — разный для каждой записи); при пустом — fallback conversationID。
// семантика ошибки и doJSON совпадает:HTTP не 2xx / Бизнес code != 0 → *Error。
func (c *Client) ReportChatActivity(a *auth.Auth, conversationID, requestID string) error {
	return c.ReportChatActivityModel(a, conversationID, requestID, "deepseek-v4-flash", "DeepSeek V4 Flash")
}

// ReportChatActivityModel То же, но можно указать модель в отчёте: для задач типа "попробовать модель»
// выравнивание на фактическую модель (напр. Model_chat_GLM5.2 Требуется requestModelId=glm-5.2 и независимо requestID）。
func (c *Client) ReportChatActivityModel(a *auth.Auth, conversationID, requestID, modelID, modelName string) error {
	if requestID == "" {
		requestID = conversationID
	}
	if modelID == "" {
		modelID = "deepseek-v4-flash"
	}
	if modelName == "" {
		modelName = modelID
	}
	now := time.Now().UnixMilli()
	ev := chatRequestEvent{
		EventCode: "chat_request_send",
		Timestamp: now,
		ReportDelay: 0,
		Mode: "craft",
		ConversationID: conversationID,
		RequestID: requestID,
		InputLength: 12,
		RequestModelID: modelID,
		RequestModelName: modelName,
		IsPlan: false,
		IsAutoExecuteTerminal: false,
		IsAutoModify: false,
		CodebaseEnable: false,
		MaxToken: 0,
		MaxSteps: 0,
		Temperature: 0,
		MaxRetries: 0,
		MentionContexts: []any{},
		KnowledgeID: []any{},
		KnowledgeName: []any{},
		CodebaseID: "",
		MentionContextCount: 0,
		Command: "",
		ExpertID: "",
		RecommendID: "",
		SkillID: "",
		SkillCount: 0,
		TotalCount: 0,
		FileURI: "",
		PresentAt: now,
		TraceID: "",
		RootRequestID: requestID,
		ParentConversationID: conversationID,
		AgentName: "default",
		AgentType: "conversation",
		UserID: a.UID,
	}
	raw, err := json.Marshal([]chatRequestEvent{ev})
	if err != nil {
		return err
	}
	_, err = c.billingJSON(a, http.MethodPost, reportPath, json.RawMessage(raw))
	return err
}
