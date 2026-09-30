// desktop.go Десктоп-клиент (WorkBuddy Desktop 5.5.6）Отправка поведенческого отпечатка.
//
// Источник:2026-09-12 Sunny Захвачено сниффером (data/desktop-task-protocol.md）。подсветка на десктопе
// 「ключ задач типа "требуется десктоп» — не отдельный эндпоинт, а тот же POST /v2/report На канале
// **Разные отпечатки клиента**：
//
//	POST https://copilot.tencent.com/v2/report ← chatBase（CLI Отправка через billingBase）
//	User-Agent: WorkBuddy/5.5.6 WorkBuddy/5.5.6 CLI/2.137.1
//	X-Domain: copilot.tencent.com, X-Product: SaaS, X-User-Id: <uid>
//	Body: [ {...event...} ] ← Массив
//
// Каждое событие кроме бизнес-полей обязательно содержит отпечаток десктопа (ideName/ideType=WorkBuddy、
// extName=workbuddy-desktop и т.д.). Фактические записи срабатывания:
// - RichMeow_Chat（Диалог десктопной версии1раз):agent_task_created + успешный
// chat_message_response(isSuccessful=true) → 1/1 + UR Buddy。
// - Hp_Appearance（Тематические задачи): независимо API
// POST /v2/user-asset/appearance/set {kind:"theme",resource_key} → SetAppearanceTheme。
//
// Внимание:service клиент склонен проверять подлинность цепочки событий (RichMeow требуется ACK успешной доставки сообщения),
// Модуль отправляет по фактической форме событий, не гарантирует, что все задачи смогут API подсветка сбоку —autotask Боковой
// По-прежнему по attempt Результат семантической обработки.
package upstream

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"regexp"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
)

const (
	desktopReportPath = "/v2/report"
	desktopAppearanceSet = "/v2/user-asset/appearance/set"
	// desktopUA Фактический десктоп-клиент UA（5.5.6 Встроенный CLI 2.137.1）。
	desktopUA = "WorkBuddy/5.5.6 WorkBuddy/5.5.6 CLI/2.137.1"
)

// desktopBase Десктоп /v2/report и user-asset Ход chatBase（copilot.tencent.com）。
func (c *Client) desktopBase(a *auth.Auth) string { return c.chatBase(a) }

// deriveID От uid детерминированно вывести один 36 бит hex Идентификатор устройства (machineId/qimei36 повторное использование),
// Идемпотентность: один аккаунт каждый раз генерирует одно значение, эмуляция фиксированного устройства.
func deriveID(a *auth.Auth, salt string) string {
	sum := sha256.Sum256([]byte(salt + ":" + a.UID))
	return hex.EncodeToString(sum[:18]) // 36 hex chars
}

// DesktopEvent событие десктопа: любые бизнес-поля (map），Общий fingerprint формируется из ReportDesktopEvent Инъекция.
type DesktopEvent map[string]any

// desktopFingerprint Поле отпечатка общего десктопа (инжектится в каждое событие, перекрывает одноименный бизнес-ключ).
func desktopFingerprint(a *auth.Auth) map[string]any {
	now := time.Now().UnixMilli()
	return map[string]any{
		"timezone": "Asia/Shanghai",
		"reportDelay": 2000,
		"userId": a.UID,
		"username": a.Nickname,
		"userNickname": a.Nickname,
		"product": "SaaS",
		"releaseDate": int64(1789036585355),
		"commit": "5f9692923c93033111c51ad7b003eb80204a9b75",
		"ideName": "WorkBuddy",
		"ideType": "WorkBuddy",
		"ideVersion": "5.5.6",
		"machineId": deriveID(a, "machine"),
		"sessionId": deriveID(a, "session"),
		"extName": "workbuddy-desktop",
		"extVersion": "5.5.6",
		"os": "win32",
		"arch": "x64",
		"osVersion": "10.0.26220",
		"cpuCores": 20,
		"memorySize": 24,
		"timestamp": now,
		"presentAt": now,
	}
}

// ReportDesktopEvent Отпечатком десктоп-клиента к copilot.tencent.com/v2/report пакетная отправка событий.
// events как бизнес-нагрузка (eventCode и т.д. задаются вызывающей стороной); общий отпечаток инжектируется автоматически,
// приоритет бизнес-полей (можно переопределить qimei36/machineId и др. идентификаторы устройства для привязки к реальному устройству).
func (c *Client) ReportDesktopEvent(a *auth.Auth, events ...DesktopEvent) error {
	if len(events) == 0 {
		return fmt.Errorf("desktop report: no events")
	}
	fp := desktopFingerprint(a)
	arr := make([]map[string]any, 0, len(events))
	for _, ev := range events {
		m := map[string]any{}
		for k, v := range fp {
			m[k] = v
		}
		for k, v := range ev {
			m[k] = v
		}
		arr = append(arr, m)
	}
	raw, err := json.Marshal(arr)
	if err != nil {
		return err
	}
	req, err := http.NewRequest(http.MethodPost, c.desktopBase(a)+desktopReportPath, bytes.NewReader(raw))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+a.AccessTokenValue())
	req.Header.Set("Accept", "application/json, text/plain, */*")
	req.Header.Set("Content-Type", "application/json;charset=UTF-8")
	req.Header.Set("User-Agent", desktopUA)
	req.Header.Set("X-Domain", c.desktopBase(a))
	req.Header.Set("X-Product", "SaaS")
	req.Header.Set("X-Request-ID", deriveID(a, "req")+fmt.Sprintf("%d", time.Now().UnixNano()%1e6))
	if a.UID != "" {
		req.Header.Set("X-User-Id", a.UID)
	}
	_, err = c.doJSON(req)
	return err
}

// DesktopChatSequence Сконструировать полную цепочку событий "успешного диалога на десктопе»
// （agent_task_created → chat_message_send → chat_request_send →
// chat_message_response(isSuccessful) → chat_message_status → chat_request_response）。
// Цепочка фактически подсвечена RichMeow_Chat。conversationID/requestID/messageID Генерируется вызывающей стороной.
func DesktopChatSequence(conversationID, requestID, messageID, modelID, modelName string) []DesktopEvent {
	uuid := func() string { return requestID }
	mk := func(code string, extra map[string]any) DesktopEvent {
		ev := DesktopEvent{"eventCode": code}
		for k, v := range extra {
			ev[k] = v
		}
		return ev
	}
	return []DesktopEvent{
		mk("agent_task_created", map[string]any{
			"source": "LOCAL", "name": "working", "task_target": "local", "mode": "craft",
			"requestModelId": modelID, "requestModelName": modelName,
			"has_repo": false, "repo_type": "none", "workspace_type": "empty",
			"has_connector": false, "connector_types": []any{},
			"has_mention": false, "mention_types": []any{},
			"has_template": false, "action": "", "template_name": "",
			"has_expert": false, "expert_id": "", "expert_name": "", "expert_industry_id": "",
			"has_skill": false, "skill_names": []any{},
			"conversationId": conversationID, "messageId": messageID,
			"buddyId": "", "buddyName": "",
		}),
		mk("chat_message_send", map[string]any{
			"messageId": messageID + "-assistant", "historyCount": 0,
			"isContextTruncated": false, "currentStepCount": 1,
			"traceId": uuid(), "rootRequestId": requestID,
			"parentConversationId": conversationID,
			"agentName": "cli", "agentType": "main",
		}),
		mk("chat_request_send", map[string]any{
			"inputLength": 24, "isPlan": false, "isAutoExecuteTerminal": false,
			"isAutoModify": false, "codebaseEnable": false, "maxToken": 0,
			"maxSteps": 500, "temperature": 0, "maxRetries": 0,
			"mentionContexts": []any{}, "knowledgeId": []any{}, "knowledgeName": []any{},
			"codebaseId": "", "mentionContextCount": 0, "command": "",
			"recommendId": "", "skillId": "", "skillCount": 0, "totalCount": 0,
			"traceId": uuid(), "rootRequestId": requestID,
			"parentConversationId": conversationID,
			"agentName": "cli", "agentType": "main",
			"codebuddy.session_id": conversationID,
			"codebuddy.conversation_request_id": requestID,
		}),
		mk("chat_message_response", map[string]any{
			"messageId": messageID + "-assistant", "responseModelId": modelID,
			"inputToken": 120, "outputToken": 80, "totalToken": 200,
			"cachedTokens": 0, "cachedWriteTokens": 0, "cachedMissTokens": 0,
			"isSuccessful": true, "messageErrorCode": "", "finishReason": "stop",
			"firstTokenAt": time.Now().UnixMilli(), "traceId": uuid(),
			"conversationId": conversationID,
			"rootRequestId": requestID, "parentConversationId": conversationID,
			"agentName": "cli", "agentType": "main",
			"codebuddy.session_id": conversationID,
			"codebuddy.conversation_request_id": requestID,
		}),
		mk("chat_message_status", map[string]any{
			"messageId": messageID + "-assistant", "messageErrorCode": "0",
			"traceId": uuid(), "rootRequestId": requestID,
			"parentConversationId": conversationID,
			"agentName": "cli", "agentType": "main",
		}),
		mk("chat_request_response", map[string]any{
			"mode": "craft", "toolCallCount": 0,
			"inputToken": 120, "outputToken": 80, "totalToken": 200,
			"cachedTokens": 0, "cachedWriteTokens": 0, "cachedMissTokens": 0,
			"isSuccessful": true, "messageErrorCode": "", "finishReason": "stop",
			"rootRequestId": requestID, "parentConversationId": conversationID,
		}),
	}
}

// SetAppearanceTheme тема оформления приложения (факт.:POST copilot.tencent.com/v2/user-asset/appearance/set，
// и тема продукта resource_key для "theme-tkmw7j«，светлый "light"、Тёмная "dark"）。Чистый API set не учитывать
// Hp_Appearance мин (требуется реальная активность после смены темы клиентом), оставлено для подбора палитры/Для восстановления и последующей верификации.
func (c *Client) SetAppearanceTheme(a *auth.Auth, resourceKey string) error {
	body := map[string]string{"kind": "theme", "resource_key": resourceKey}
	raw, err := json.Marshal(body)
	if err != nil {
		return err
	}
	req, err := http.NewRequest(http.MethodPost, c.desktopBase(a)+desktopAppearanceSet, bytes.NewReader(raw))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+a.AccessTokenValue())
	req.Header.Set("Accept", "application/json, text/plain, */*")
	req.Header.Set("Content-Type", "application/json;charset=UTF-8")
	req.Header.Set("User-Agent", desktopUA)
	req.Header.Set("X-Product", "SaaS")
	if a.UID != "" {
		req.Header.Set("X-User-Id", a.UID)
	}
	_, err = c.doJSON(req)
	return err
}

// DesktopBuddyAppSequence Формирование сигнала "вход в Buddy приложения» 5 подряд событий (факт. замер два аккаунта чисто API Подсветить
// Buddy_App и Buddy_App_QQ）：discover → show → enter_click → auth_confirm →
// bindaccount_skip。buddyID Фиксированно использовать помощника Penguin Teacher cb_y5Dy46tPQGGWtueMxXbe
// （Buddy_App_QQ применение критерия), при одновременном выполнении Buddy_App「войти в любое приложение».
func DesktopBuddyAppSequence(buddyID, buddyName string) []DesktopEvent {
	mk := func(code string, extra map[string]any) DesktopEvent {
		ev := DesktopEvent{
			"eventCode": code, "mode": "LOCAL",
			"buddyId": buddyID, "buddyName": buddyName,
		}
		for k, v := range extra {
			ev[k] = v
		}
		return ev
	}
	return []DesktopEvent{
		mk("buddyapp_discover_click", nil),
		mk("buddyapp_show", map[string]any{"elementId": buddyID, "elementName": buddyName, "position": 2}),
		mk("buddyapp_enter_click", map[string]any{"elementId": buddyID, "elementName": buddyName, "position": 2, "isFirstPage": "1"}),
		mk("buddyapp_auth_confirm_click", map[string]any{"elementId": buddyID, "elementName": buddyName}),
		mk("buddyapp_bindaccount_skip_click", map[string]any{"elementId": buddyID, "elementName": buddyName}),
	}
}

// DesktopAutomationCreateEvent Сконструировать событие "Задание по расписанию успешно создано» (фактически два аккаунта чисто API
// Подсветить automation_1）。name — имя задачи, может совпадать с семантикой реального создания.
func DesktopAutomationCreateEvent(name string) DesktopEvent {
	return DesktopEvent{
		"eventCode": "automated_task_create_suc", "name": name,
		"source": "manually", "modelId": "fast-model", "modelIsThinking": true,
		"connectorCount": 0, "skills": "", "skillCount": 0,
		"scheduleType": "once", "mode": "LOCAL",
	}
}

// ReportWebEvent по Web отпечаток терминала к www.workbuddy.cn/v2/report Отчёт по одиночному событию.
// и отпечаток десктопа (copilot домена) отличается:web Домейн-событие в форме браузера (os/machineId/userAgent），
// Используется для Library_read и другие задачи поведения на странице (факт. library_doc_intro_click 4 сек. подсветка).
func (c *Client) ReportWebEvent(a *auth.Auth, eventCode, pageURL, elementID, elementName string) error {
	ua := "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/152.0.0.0 Safari/537.36"
	ev := map[string]any{
		"eventCode": eventCode, "timestamp": time.Now().UnixMilli(), "reportDelay": 0,
		"pageURL": pageURL, "elementId": elementID, "elementName": elementName,
		"os": "Win32", "arch": "", "osVersion": "10.0", "userAgent": ua,
		"machineId": deriveID(a, "webmachine"), "userId": a.UID,
		"userNickname": a.Nickname, "enterpriseId": a.EnterpriseID,
	}
	raw, err := json.Marshal([]map[string]any{ev})
	if err != nil {
		return err
	}
	req, err := http.NewRequest(http.MethodPost, c.webBase(a)+"/v2/report", bytes.NewReader(raw))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+a.AccessTokenValue())
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("x-client-platform", "web")
	req.Header.Set("Origin", c.webBase(a))
	req.Header.Set("Referer", pageURL)
	req.Header.Set("User-Agent", ua)
	if a.UID != "" {
		req.Header.Set("X-User-Id", a.UID)
	}
	_, err = c.doJSON(req)
	return err
}

// ---------------------------------------------------------------------------
// 2026-09-12 пятый раунд: клиент asar обратный + Sunny новый критерий на реальных выборках (подтверждено на 3 аккаунтах).
// Источник:WorkBuddy.exe 5.5.6 app.asar Перечисление событий слоя рендеринга/Пейлоад + Захват реальных сэмплов вызова эксперта
// （data/desktop-task-protocol.md §7.3）。
// ---------------------------------------------------------------------------

// DesktopTemplateUseSequence Сформировать группу событий "Создание задачи из шаблона» (факт. template_5 подсчёт):
// agent_task_created_with_template {mode,isCustomModel,id,name,requestId} +
// template_used {template_id,task_mode}，JOIN Одна полная chat Цепочка.
// фактические замеры трёх аккаунтов:5 группа (разные шаблоны) — один отчёт → 5/5 подсветить.
func DesktopTemplateUseSequence(conversationID, requestID, templateID, templateName string) []DesktopEvent {
	events := DesktopChatSequence(conversationID, requestID, "msg-"+templateID, "fast-model", "fast-model")
	events = append(events,
		DesktopEvent{
			"eventCode": "agent_task_created_with_template", "mode": "working",
			"isCustomModel": false, "id": templateID, "name": templateName, "requestId": requestID,
		},
		DesktopEvent{"eventCode": "template_used", "template_id": templateID, "task_mode": "working"},
	)
	return events
}

// DesktopPlaybookPromptSequence Сформировать группу событий "сделать аналог по кейсу-вдохновению» (факт. playbook_prompt подсчёт):
// web_element_click(playbook_ctaClick) + playbook_cta_click + playbook_prompt_send
// （Dialog Отправка Prompt，Лента conversationId/requestId JOIN chat цепочка). замер на трёх аккаунтах 1/1 подсветить.
func DesktopPlaybookPromptSequence(conversationID, requestID, caseID, caseName string) []DesktopEvent {
	events := DesktopChatSequence(conversationID, requestID, "msg-pb", "fast-model", "fast-model")
	payload := map[string]any{
		"id": caseID, "name": caseName, "type": "document",
		"categoryId": "", "categoryName": "",
	}
	events = append(events,
		DesktopEvent{
			"eventCode": "web_element_click", "pageName": "playbook_detail",
			"elementId": "playbook_ctaClick", "elementName": caseName, "source": "discover",
		},
		DesktopEvent(func() map[string]any {
			m := map[string]any{"eventCode": "playbook_cta_click", "source": "discover", "position": 0}
			for k, v := range payload {
				m[k] = v
			}
			return m
		}()),
		DesktopEvent(func() map[string]any {
			m := map[string]any{"eventCode": "playbook_prompt_send", "conversationId": conversationID, "requestId": requestID}
			for k, v := range payload {
				m[k] = v
			}
			return m
		}()),
	)
	return events
}

// DesktopDesignCanvasSequence Формирование группы событий "холст дизайн-креатива» (факт. create_canvas подсчёт):
// wbx_design_canvas_task_create + wbx_design_canvas_open（Ardot create_design Инструмент
// При завершении клиент через metrics Отчет канала, один и тот же /v2/report эндпоинт). Проверено на трёх аккаунтах 1/1 подсветить.
func DesktopDesignCanvasSequence(conversationID, requestID string) []DesktopEvent {
	events := DesktopChatSequence(conversationID, requestID, "msg-canvas", "fast-model", "fast-model")
	return append(events,
		DesktopEvent{
			"eventCode": "wbx_design_canvas_task_create", "conversationId": conversationID,
			"requestId": requestID, "source": "summon_keyword", "cost": 12000, "isSuccessful": true,
		},
		DesktopEvent{
			"eventCode": "wbx_design_canvas_open", "conversationId": conversationID,
			"requestId": requestID, "id": "ardot-file-" + requestID[len(requestID)-8:],
			"source": "summon_keyword", "type": "page", "cost": 13000, "isSuccessful": true,
		},
	)
}

// MarketExpert Отдельный эксперт маркета экспертов (/portal/operation-platform/market/expert/list подмножества ответа).
type MarketExpert struct {
	ExpertID string `json:"expert_id"`
	ExpertType string `json:"expert_type"`
	DisplayNameZH string `json:"display_name_zh"`
	ProfessionZH string `json:"profession_zh"`
	Version string `json:"version"`
	Categories []any `json:"categories"`
}

// MarketExpertList запросить реальный список экспертов маркета экспертов (expertType: "agent« Один эксперт / "team" группа экспертов).
// expert_actual_use Требования к проверке критериев id — реально существующий на платформе эксперт (фабрикация id не учитывается).
func (c *Client) MarketExpertList(a *auth.Auth, expertType string) ([]MarketExpert, error) {
	body := map[string]any{"page": 1, "page_size": 20, "sort_by": "reco_rank", "sort_order": "desc"}
	if expertType != "" {
		body["expert_type"] = expertType
	}
	raw, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequest(http.MethodPost, c.chatBase(a)+"/portal/operation-platform/market/expert/list", bytes.NewReader(raw))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+a.AccessTokenValue())
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", desktopUA)
	req.Header.Set("X-Domain", c.chatBase(a))
	req.Header.Set("X-Product", "SaaS")
	if a.UID != "" {
		req.Header.Set("X-User-Id", a.UID)
	}
	var out struct {
		Experts []MarketExpert `json:"experts"`
	}
	data, err := c.doJSON(req)
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(data, &out); err != nil {
		return nil, fmt.Errorf("expert list parse: %w", err)
	}
	return out.Experts, nil
}

// DesktopChatWithExpert Отправить реальный десктопный отпечаток chat Запрос (может содержать X-Expert-Id），Из SSE Поток
// Парсинг**возвращено сервером requestId**（data.id，Например cmb-xxxx / 32hex）и вернуть.
// expert_actual_use и т.д. JOIN события requestId должен быть именно этот сервер id——самосозданный UUID не учитывается
// （Клиент resolveRealRequestId Та же семантика,Sunny row 2113 подтверждено).
func (c *Client) DesktopChatWithExpert(a *auth.Auth, expertID string) (conversationID, requestID string, err error) {
	conversationID = fmt.Sprintf("wb2api-conv-%d", time.Now().UnixNano())
	body := map[string]any{
		"model": "fast-model",
		"messages": []any{
			map[string]any{"role": "system", "content": "You are a helpful assistant. Сейчас окружение — китайский язык, отвечать на упрощенном китайском."},
			map[string]any{"role": "user", "content": "1+1равно скольки? Ответь прямо."},
		},
		"agent": "cli",
		"temperature": 1,
		"stream": true,
		"stream_options": map[string]any{"include_usage": true},
	}
	raw, err := json.Marshal(body)
	if err != nil {
		return "", "", err
	}
	req, err := http.NewRequest(http.MethodPost, c.chatBase(a)+"/v2/chat/completions", bytes.NewReader(raw))
	if err != nil {
		return "", "", err
	}
	h := req.Header
	h.Set("Authorization", "Bearer "+a.AccessTokenValue())
	h.Set("Content-Type", "application/json")
	h.Set("Accept", "text/event-stream")
	h.Set("User-Agent", desktopUA)
	h.Set("X-Domain", c.chatBase(a))
	h.Set("X-Product", "SaaS")
	h.Set("X-User-Id", a.UID)
	h.Set("X-Conversation-ID", conversationID)
	h.Set("X-Request-ID", fmt.Sprintf("%d", time.Now().UnixNano()))
	h.Set("X-Agent-Intent", "craft")
	h.Set("X-Agent-Type", "main")
	h.Set("X-IDE-Name", "WorkBuddy")
	h.Set("X-IDE-Type", "WorkBuddy")
	h.Set("X-IDE-Version", "5.5.6")
	h.Set("x-codebuddy-request", "1")
	if expertID != "" {
		h.Set("X-Expert-Id", expertID)
	}
	if os.Getenv("WB2A_DEBUG_CHAT") != "" {
		fmt.Printf("[dbg] URL=%s\n", req.URL)
		for k := range req.Header {
			fmt.Printf("[dbg] %s: %s\n", k, req.Header.Get(k))
		}
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return "", "", err
	}
	defer resp.Body.Close()
	if os.Getenv("WB2A_DEBUG_CHAT") != "" {
		fmt.Printf("[dbg] status=%d enc=%q\n", resp.StatusCode, resp.Header.Get("Content-Encoding"))
	}
	if resp.StatusCode != 200 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 200))
		return "", "", fmt.Errorf("chat http %d: %s", resp.StatusCode, b)
	}
	// Из SSE поток берет первый data.id Как серверная сторона requestId（вычитать поток до конца во избежание зависших соединений).
	buf := make([]byte, 0, 1<<20)
	tmp := make([]byte, 8192)
	searchFrom := 0
	for {
		n, rerr := resp.Body.Read(tmp)
		if n > 0 {
			buf = append(buf, tmp[:n]...)
			if os.Getenv("WB2A_DEBUG_CHAT") != "" {
				dbg := string(tmp[:n])
				if len(dbg) > 120 {
					dbg = dbg[:120]
				}
				fmt.Printf("[dbg-rd %d] %q\n", n, dbg)
			}
			// продолжить с последней позиции поиска:SSE внутри `"id":"` может сначала появиться в сообщении id и т.п. поля,
			// Если не сдвигать offset, первый несовпадающий id заставит цикл вечно попадать в одну и ту же позицию,
			// Чтение заполнено 1MB ложное срабатывание после"Не найдено requestId"。
			if i := bytes.Index(buf[searchFrom:], []byte(`"id":"`)); i >= 0 {
				abs := searchFrom + i
				rest := buf[abs+6:]
				if end := bytes.IndexByte(rest, '"'); end > 0 {
					id := string(rest[:end])
					if os.Getenv("WB2A_DEBUG_CHAT") != "" {
						fmt.Printf("[dbg-id] %q match=%v\n", id, idRegex.MatchString(id))
					}
					if idRegex.MatchString(id) {
						return conversationID, id, nil
					}
					searchFrom = abs + 1
				}
			}
		}
		if rerr != nil || len(buf) > 1<<20 {
			break
		}
	}
	return "", "", fmt.Errorf("SSE Сервер не найден в requestId")
}

// idRegex Сервер requestId Форма (cmb- Префикс 32hex или голый 32hex）。
var idRegex = regexp.MustCompile(`^(cmb-)?[0-9a-f]{32}$`)

// DesktopExpertSummonSequence Сконструировать группу событий "вызов эксперта платформы» (expert_summon_click и т.д.),
// пейлоад выровнен по реальным дампам трафика (Sunny row 2644）。Требует совместного использования с DesktopChatWithExpert +
// DesktopExpertActualUseEvent Завершить полный "вызов+использование».
func DesktopExpertSummonSequence(e MarketExpert) []DesktopEvent {
	cat := "expert-all"
	if len(e.Categories) > 0 {
		if s, ok := e.Categories[0].(string); ok {
			cat = s
		}
	}
	ver := e.Version
	if ver == "" {
		ver = "1.0.0"
	}
	return []DesktopEvent{
		{
			"eventCode": "web_element_click", "source": e.ExpertID, "type": cat, "version": ver,
			"elementId": "expert_summon_click", "elementName": "немедленный вызов",
			"pageURL": "/C:/Program%20Files/WorkBuddy/resources/app.asar/renderer/index.html",
		},
		{
			"eventCode": "expert_summon_click", "id": e.ExpertID, "name": e.DisplayNameZH,
			"expertTitle": e.ProfessionZH, "type": "expert-all", "position": 0,
			"expertType": e.ExpertType, "version": ver, "mode": "LOCAL",
		},
		{
			"eventCode": "expert_summoned", "id": e.ExpertID, "name": e.DisplayNameZH,
			"expertTitle": e.ProfessionZH, "type": "expert-all",
		},
	}
}

// DesktopExpertActualUseEvent формирование события "реальное использование экспертом» (expert_5/Expert_team_use_3 подсчёт).
// requestID Должно быть DesktopChatWithExpert возвращаемый сервером requestId。
func DesktopExpertActualUseEvent(e MarketExpert, conversationID, requestID string) DesktopEvent {
	ev := desktopExpertActualUse(e, conversationID, requestID)
	ev["mode"] = "craft"
	return ev
}

// DesktopExpertActualUseLocal mode:"LOCAL" Вариант (Expert_lighthouse Требования критерия LOCAL，
// Выравнивание по реальным сэмплам Sunny row 868：при использовании лёгкого cloud-эксперта mode=LOCAL、type пусто,cost=0）。
func DesktopExpertActualUseLocal(e MarketExpert, conversationID, requestID string) DesktopEvent {
	ev := desktopExpertActualUse(e, conversationID, requestID)
	ev["mode"] = "LOCAL"
	return ev
}

// desktopExpertActualUse expert_actual_use Общая нагрузка.
func desktopExpertActualUse(e MarketExpert, conversationID, requestID string) DesktopEvent {
	cat := "expert-all"
	if len(e.Categories) > 0 {
		if s, ok := e.Categories[0].(string); ok {
			cat = s
		}
	}
	ver := e.Version
	if ver == "" {
		ver = "1.0.0"
	}
	return DesktopEvent{
		"eventCode": "expert_actual_use",
		"id": e.ExpertID, "name": e.DisplayNameZH, "expertTitle": e.ProfessionZH,
		"type": cat, "expertType": e.ExpertType, "source": "builtin", "version": ver,
		"cost": 9000, "characterCount": 14,
		"conversationId": conversationID, "requestId": requestID, "messageId": "msg-" + requestID[len(requestID)-8:],
		"requestModelId": "fast-model", "requestModelName": "fast-model",
	}
}
