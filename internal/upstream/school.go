// school.go Акция к началу учебного года (school-season，Период активности 2026-09-13 ~ 09-24）Чистый API автоматизация.
//
// критерий (2026-09-13 Мини-приложение MCP обратный + тест на трех аккаунтах,protocol.md §7.11）：
// - share_invite（Ежедневно +100c +1розыгрыш):POST /tasks/share-complete {channel:"wechat"}
// Сразу подсвечивается — отчёт только с фронтенда, сервер не проверяет квитанцию шаринга. Основная цель модуля.
// - chat_3_times / expert_use：Критерий привязан к нативной сессии песочницы мини-приложения (e2b runtime），
// webchat Обычные сессии не учитываются, только API Не делать (требуется ручной диалог в мини-программе).
// - Розыгрыш:POST /wheel/draw {draw_uuid}（Генерация на фронтенде uuid，Потребление 1 chance）。
//
// базовый URL эндпоинта www.codebuddy.cn（billing тот же домен); конверт {code,msg,data}，code=0 успешно.
package upstream

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
)

const schoolBase = "/portal/activity/school"

// schoolJSON Активности академии API запрос (снять обёртку, бизнес code≠0 возвращает с msg error）。
func (c *Client) schoolJSON(a *auth.Auth, method, path string, body map[string]any, out any) error {
	var raw []byte
	if body != nil {
		raw, _ = json.Marshal(body)
	}
	req, err := http.NewRequest(method, c.billingBase(a)+schoolBase+path, bytes.NewReader(raw))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+a.AccessTokenValue())
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	if a.UID != "" {
		req.Header.Set("X-User-Id", a.UID)
	}
	data, err := c.doJSON(req)
	if err != nil {
		return err
	}
	if out != nil {
		return json.Unmarshal(data, out)
	}
	return nil
}

// - chat_3_times：3 запись chat_request_send т.е. 3/3（conversationId Произвольный, десктоп/mp
// все семейства заголовков учитываются, реальная сессия песочницы не требуется).
// - expert_use：mp Цепочка событий отпечатка expert_summon_click + expert_summoned +
// expert_actual_use + chat_request_send（эксперт классификации "Back to School») — сразу подсвечивается.

const mpReportPath = "/v2/report"

// schoolOpenDayActivityID Сезон начала учёбы/Активность Campus Day id（событие activityId значение поля, общее для двух доменов).
const schoolOpenDayActivityID = "school_open_day_2026"

// mpEventBase Общий фингерпринт трекинга мини-программы (appservice wQ()+Ao() Выравнивание).
func mpEventBase(a *auth.Auth) map[string]any {
	return map[string]any{
		"timestamp": time.Now().UnixMilli(),
		"ideType": "WorkBuddy_MP",
		"ideVersion": "2.4.0",
		"extName": "workbuddy-mp",
		"extVersion": "2.4.0",
		"product": "SaaS",
		"ideName": "wx_app_cloud",
		"platform": "mini_program",
		"os": "windows",
		"osVersion": "11",
		"arch": "x64",
		"machineId": "0655736a-607f-4d9d-b430-58176ee9a090",
		"timezone": "Asia/Shanghai",
		"userId": a.UID,
		"userNickname": a.Nickname,
	}
}

// ReportMPEvent по отпечатку мини-программы к www.codebuddy.cn/v2/report пакетная отправка событий.
func (c *Client) ReportMPEvent(a *auth.Auth, events ...map[string]any) error {
	if len(events) == 0 {
		return fmt.Errorf("mp report: no events")
	}
	base := mpEventBase(a)
	arr := make([]map[string]any, 0, len(events))
	for _, ev := range events {
		m := map[string]any{}
		for k, v := range base {
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
	req, err := http.NewRequest(http.MethodPost, c.BillingBaseCN+mpReportPath, bytes.NewReader(raw))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+a.AccessTokenValue())
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	if a.UID != "" {
		req.Header.Set("X-User-Id", a.UID)
	}
	req.Header.Set("X-Client-Product", "workbuddy-mp")
	req.Header.Set("X-Client-Version", "2.4.0")
	req.Header.Set("X-Client-Platform", "mp-weixin")
	req.Header.Set("X-Platform", "wechatmp")
	_, err = c.doJSON(req)
	return err
}

// SchoolChatTimesEvents создать запись chat_request_send Событие (chat_3_times подсчёт).
func SchoolChatTimesEvents(conversationID string) map[string]any {
	rid := "wb2api-" + clientToken()
	return map[string]any{
		"eventCode": "chat_request_send",
		"inputLength": 14, "isPlan": false, "isAutoExecuteTerminal": false,
		"isAutoModify": false, "codebaseEnable": false, "maxToken": 0,
		"maxSteps": 500, "temperature": 0, "maxRetries": 0,
		"mentionContexts": []any{}, "knowledgeId": []any{}, "knowledgeName": []any{},
		"codebaseId": "", "mentionContextCount": 0, "command": "",
		"recommendId": "", "skillId": "", "skillCount": 0, "totalCount": 0,
		"traceId": rid, "rootRequestId": rid,
		"parentConversationId": conversationID, "conversationId": conversationID,
		"messageId": "msg-" + rid[len(rid)-8:],
		"agentName": "mp", "agentType": "main",
		"codebuddy.session_id": conversationID,
		"codebuddy.conversation_request_id": rid,
	}
}

// SchoolSeasonChatEvent конструктор growth Домен "День кампуса» (school_season）Событие-критерий:
// mini Фингерпринт chat_request_send + activityId=school_open_day_2026（и school Домен
// Аналогично началу учебного сезона activityId связь; фактически нет activityId событие не подсвечивается). Форма события и
// SchoolChatTimesEvents Изоморфный (school Домен chat_3_times та же модель), только добавление activityId。
func SchoolSeasonChatEvent(conversationID string) map[string]any {
	ev := SchoolChatTimesEvents(conversationID)
	ev["activityId"] = schoolOpenDayActivityID
	return ev
}

// MiniExpertUseEvent конструктор growth Домен Sequential_Tasks_2「Выбрать эксперта в мини-программе и завершить
// событие-критерий "валидного диалога»:mp Фингерпринт expert_actual_use。Структура выровнена с исходником мини-программы
// app-service.js реальная точка отправки (апстрим task_runner На практике 2026-09-23：при отправке сразу completed，
// claim +200c+5e）。и school домена SchoolExpertUseEvents Да**Два подхода к подсчёту**，Не копировать дословно:
// - без conversationId/activityId——Реальное событие — обе поля отсутствуют;
// - extVersion использовать версию самого мини-приложения 2.2.8（перекрытие mpEventBase 2.4.0）；
// - source=mini_program + type Фиксированный "send_message"（мини-программа всегда отправляет это значение).
//
// expertID Должен быть реальным из маркета экспертов ex_ id（ListMarketExperts），пустой id На стороне сервера не начисляется/не списывается.
func MiniExpertUseEvent(expertID, expertName, expertType string) map[string]any {
	if expertType == "" {
		expertType = "agent"
	}
	if expertName == "" {
		expertName = expertID
	}
	return map[string]any{
		"eventCode": "expert_actual_use", "reportDelay": 0,
		"extVersion": "2.2.8", "source": "mini_program",
		"id": expertID, "name": expertID,
		"expertTitle": expertName, "type": "send_message",
		"characterCount": 12, "expertType": expertType,
	}
}

// MiniChatModelEvent mp Событие диалога + поле модели (Sequential_Tasks_5「Использовать GLM5.2」Критерий
// Носитель): мини-приложение chat_request_send Реальная точка отправки (mpsrc main 32904 модуль) с
// requestModelId / requestModelName——Tasks_1/3 голое событие диалога без модели, задача модели
// Требуется данная форма (критерий требует проверки разблокировкой на практике).
func MiniChatModelEvent(conversationID, modelID, modelName string) map[string]any {
	ev := SchoolChatTimesEvents(conversationID)
	ev["requestModelId"] = modelID
	ev["requestModelName"] = modelName
	return ev
}

// MiniPlaybookEvents mp Группа событий fingerprint-inspiration (Sequential_Tasks_7「критерий "функции Inspiration»
// носитель, выравнивание структуры mpsrc main 73640/73665 точка отправки:playbook_cta_click →
// playbook_prompt_send）。issue #42 назвать задачу PC Метрика (+500c+5e）——PC Последовательность
// （DesktopPlaybookPromptSequence）Проверено: подсвечено тестом playbook_prompt，данная группа как mp Форма
// Дополнение (задача в mp в цепочке, критерий по какой стороне — требует проверки на практике).
func MiniPlaybookEvents(caseID, caseName string) []map[string]any {
	base := map[string]any{
		"id": caseID, "name": caseName, "type": "document",
		"categoryId": "", "categoryName": "",
		"skills": "", "skillNames": "",
	}
	cta := map[string]any{
		"eventCode": "playbook_cta_click", "source": "discover", "position": 1,
		"extVersion": "2.2.8",
	}
	for k, v := range base {
		cta[k] = v
	}
	send := map[string]any{
		"eventCode": "playbook_prompt_send", "source": "discover",
		"promptLength": 96, "isOfficial": 1,
		"conversationId": "wb2api-mp-pb-" + clientToken(),
		"extVersion": "2.2.8",
	}
	for k, v := range base {
		send[k] = v
	}
	return []map[string]any{cta, send}
}

// ---- Мой код купона (#/prizes?tab=vouchers，2026-09-16 подключение)----

// SchoolVoucher Сторонний купон, выигранный в лотерее к началу учебного года (KFC/Luckin/Kugou и др.).
// структура полей по реальному образцу ответа:GET /vouchers Однократная полная выборка (без пагинации),data.items[]。
type SchoolVoucher struct {
	GrantID int64 `json:"grant_id"`
	DrawUUID string `json:"draw_uuid,omitempty"`
	SKUCode string `json:"sku_code,omitempty"` // kfc_ice_cream / voucher_luckin / voucher_kugou …
	PrizeName string `json:"prize_name,omitempty"` // Мороженое KFC
	Code string `json:"code"` // Тело кода купона (копировать для погашения кассиром)
	ValidFrom string `json:"valid_from,omitempty"` // Апстрим часто пуст
	ValidTo string `json:"valid_to,omitempty"` // "2026-10-24"
	GrantedAt string `json:"granted_at,omitempty"` // RFC3339
}

// SchoolVouchers запрос списка кодов купонов Back-to-School аккаунта (только чтение).
// записи с выпадением баллов не на этом эндпоинте (это /rewards type=credit запись).
func (c *Client) SchoolVouchers(a *auth.Auth) ([]SchoolVoucher, error) {
	var out struct {
		Items []SchoolVoucher `json:"items"`
	}
	if err := c.schoolJSON(a, http.MethodGet, "/vouchers", nil, &out); err != nil {
		return nil, err
	}
	return out.Items, nil
}
