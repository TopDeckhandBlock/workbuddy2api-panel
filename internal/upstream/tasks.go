// tasks.go growth Интерфейс домена "Задачи»: запрос списка / Принять / получить награду.
//
// Источник: апстрим scripts/task_common.py Фактический калибр (list_tasks/accept_tasks/claim_reward），
// После интеграции в основную программу внешний скрипт больше не нужен.
//
// эндпоинт (chatBase，BillingHeaders）：
// - GET /v2/activity/growth/tasks полный список задач (вкл. progress/accept_status）
// - POST /v2/activity/growth/tasks/accept {"task_codes":[...]} not_accepted → accepted
// - POST /v2/activity/growth/tasks/reward/claim {"task_code":«..."} Получение награды в завершенном состоянии
//
// Ключевые моменты семантики:
// - accept Да"Регистрация"，Не дает прогресса; прогресс зажигается поведенческими событиями сервера (напр. chat_request_send отчёт,
// реальный диалог), поэтому accept после — идемпотентный повтор.
// - claim Только при progress Доступно к получению после выполнения условий; повторное получение возвращает бизнес-ошибку (безопасно, без предварительной проверки состояния).
package upstream

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/url"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
)

// growth путь задачи домена (с scripts/task_common.py Выравнивание).
const (
	tasksListPath = "/v2/activity/growth/tasks"
	tasksAcceptPath = "/v2/activity/growth/tasks/accept"
)

// mpPlatform значение заголовка мини-программы: задачи только для мини-программы (Sequential_Tasks_1 / school_season）
// выдача списка,accept、claim Требования сквозного тракта X-Client-Platform: miniprogram。
const mpPlatform = "miniprogram"

// Task внешнее представление отдельной задачи (имена полей как у апстрима JSON выравнивание, лишние поля не пробрасываются).
type Task struct {
	TaskCode string `json:"task_code"`
	Title string `json:"title,omitempty"`
	Description string `json:"description,omitempty"` // Инструкция по действиям (вкл. описание переходов)
	TaskDesc string `json:"task_desc,omitempty"` // краткое описание условий достижения
	Credit int64 `json:"credit,omitempty"` // Бонусные баллы (апстрим reward_credit）
	Energy int64 `json:"energy,omitempty"` // Бонусная энергия (апстрим reward_energy）
	HasReward bool `json:"has_reward,omitempty"` // наличие награды
	RewardBuddy bool `json:"reward_buddy,omitempty"`
	TaskType string `json:"task_type,omitempty"` // single（одноразово)/ Кумулятивный тип
	Tag string `json:"tag,omitempty"` // метка стороны (PC и т.д.)
	JumpURL string `json:"jump_url,omitempty"` // Протокол редиректа клиента (workbuddy://...）
	Locked bool `json:"locked,omitempty"` // Метка upstream не разблокирована
	Target int64 `json:"target"` // целевое количество (всегда выводится:0 является валидным значением прогресса)
	Current int64 `json:"current"` // Текущий прогресс (всегда выводится:0 является валидным значением прогресса)
	AcceptStatus string `json:"accept_status,omitempty"`
	Status string `json:"status,omitempty"` // статус задачи апстрима (complete и т.д.)
	Claimable bool `json:"claimable,omitempty"` // Прогресс достигнут, но не получено (локальная оценка)
	Claimed bool `json:"claimed,omitempty"` // Уже получено (accept_status == claimed）
}

// ListTasks Получение полного списка задач (дефолтная выборка, без заголовка-меткиКонец).
// Ответ в формате data.tasks[]，Поля элемента зависят от типа задачи (progress Возможно {current,target} или плоско),
// здесь мягкий парсинг: пробовать обе формы.
func (c *Client) ListTasks(a *auth.Auth) ([]Task, error) {
	data, err := c.growthJSON(a, http.MethodGet, tasksListPath, nil)
	if err != nil {
		return nil, err
	}
	return parseGrowthTasks(data)
}

// ListTasksMP Получить список задач в формате мини-программы (X-Client-Platform: miniprogram）。
// задачи только для мини-программы (Sequential_Tasks_1「первый диалог мини-приложения»/ school_season「День кампуса» и т.д.)
// Выдается только в этом режиме — в списке по умолчанию отсутствует,accept/claim также требует этот заголовок (отсутствие заголовка accept вернуть
// task not found，апстрим task_runner факт. замер).**На практике mp Список — супермножество дефолтной выборки**
// （Содержит RichMeow/Model_chat и прочие обычные задачи + wb_wechat_oa_subscribe_task и т.д. mp эксклюзивно),
// При мердже вызывающая сторона должна по task_code Дедупликация.
func (c *Client) ListTasksMP(a *auth.Auth) ([]Task, error) {
	data, err := c.growthJSONMP(a, http.MethodGet, tasksListPath, nil)
	if err != nil {
		return nil, err
	}
	return parseGrowthTasks(data)
}

// growthJSONMP Отправка growth Запрос домена (по метрике мини-программы: наложение X-Client-Platform: miniprogram）
// и распаковать конверт. Семантика та же, growthJSON。
func (c *Client) growthJSONMP(a *auth.Auth, method, path string, body any) (json.RawMessage, error) {
	var rdr io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		rdr = bytes.NewReader(raw)
	}
	req, err := http.NewRequest(method, c.chatBase(a)+path, rdr)
	if err != nil {
		return nil, err
	}
	c.BillingHeaders(req, a)
	req.Header.Set("X-Client-Platform", mpPlatform)
	return c.doJSON(req)
}

// AcceptTasksMP Принимать ограниченные задачи мини-приложения (mp заголовок; при отсутствии заголовка фактически task not found）。
// Идемпотентная семантика как у AcceptTasks。
func (c *Client) AcceptTasksMP(a *auth.Auth, taskCodes []string) error {
	_, err := c.growthJSONMP(a, http.MethodPost, tasksAcceptPath, map[string]any{"task_codes": taskCodes})
	return err
}

// ClaimRewardMP Получение награды за лимитированное задание мини-программы:chat Домен /activity/growth/tasks/{code}/claim
// + mp заголовок (апстрим task_runner claim_one(mp=True) аналогично);chat Домен 400 деградация при Web Домен
// эндпоинт получения награды (ClaimReward，x-client-platform: web форма). Возврат (credit, energy, err)。
func (c *Client) ClaimRewardMP(a *auth.Auth, taskCode string) (credit, energy int64, err error) {
	req, err := http.NewRequest(http.MethodPost,
		c.chatBase(a)+"/activity/growth/tasks/"+url.PathEscape(taskCode)+"/claim", nil)
	if err != nil {
		return 0, 0, err
	}
	c.BillingHeaders(req, a)
	req.Header.Set("X-Client-Platform", mpPlatform)
	data, err := c.doJSON(req)
	if err != nil {
		// chat домен для этого пути 400（Часть задач/форма тенанта)→ Web Даунгрейд домена (проверено, доступно к получению).
		if ue, ok := err.(*Error); ok && ue.Status == http.StatusBadRequest {
			return c.ClaimReward(a, taskCode)
		}
		return 0, 0, err
	}
	return parseClaimReward(data)
}

// parseClaimReward Парсинг ответа получения награды data：{"already_claimed«:bool,«credit»:n,«energy":n}。
func parseClaimReward(data json.RawMessage) (credit, energy int64, err error) {
	var resp struct {
		AlreadyClaimed bool `json:"already_claimed"`
		Credit int64 `json:"credit"`
		Energy int64 `json:"energy"`
	}
	if err := json.Unmarshal(data, &resp); err != nil {
		return 0, 0, err
	}
	return resp.Credit, resp.Energy, nil
}

// parseGrowthTasks Парсинг growth Список задач data.tasks[]（По умолчанию с mp общая метрика).
func parseGrowthTasks(data json.RawMessage) ([]Task, error) {
	var resp struct {
		Tasks []struct {
			TaskCode string `json:"task_code"`
			Title string `json:"title"`
			Description string `json:"description"`
			TaskDesc string `json:"task_desc"`
			RewardCredit int64 `json:"reward_credit"` // Фактическое имя поля upstream (reward_ префикс)
			RewardEnergy int64 `json:"reward_energy"`
			HasReward bool `json:"has_reward"`
			RewardBuddy bool `json:"reward_buddy"`
			TaskType string `json:"task_type"`
			Tag string `json:"tag"`
			JumpURL string `json:"jump_url"`
			Locked bool `json:"locked"`
			AcceptStatus string `json:"accept_status"`
			Status string `json:"status"`
			Target int64 `json:"target"`
			Current int64 `json:"current"`
			Progress json.RawMessage `json:"progress"`
		} `json:"tasks"`
	}
	if err := json.Unmarshal(data, &resp); err != nil {
		return nil, err
	}
	out := make([]Task, 0, len(resp.Tasks))
	for _, t := range resp.Tasks {
		cur, tgt := t.Current, t.Target
		// progress Возможно {current,target} объект (фактическая метрика), перекрывает плоские поля.
		if len(t.Progress) > 0 && string(t.Progress) != "null" {
			var pr struct {
				Current int64 `json:"current"`
				Target int64 `json:"target"`
			}
			if json.Unmarshal(t.Progress, &pr) == nil && (pr.Target > 0 || pr.Current > 0) {
				cur, tgt = pr.Current, pr.Target
			}
		}
		claimed := t.AcceptStatus == "claimed"
		out = append(out, Task{
			TaskCode: t.TaskCode,
			Title: t.Title,
			Description: t.Description,
			TaskDesc: t.TaskDesc,
			Credit: t.RewardCredit,
			Energy: t.RewardEnergy,
			HasReward: t.HasReward,
			RewardBuddy: t.RewardBuddy,
			TaskType: t.TaskType,
			Tag: t.Tag,
			JumpURL: t.JumpURL,
			Locked: t.Locked,
			Target: tgt,
			Current: cur,
			AcceptStatus: t.AcceptStatus,
			Status: t.Status,
			Claimable: !claimed && tgt > 0 && cur >= tgt,
			Claimed: claimed,
		})
	}
	return out, nil
}

// AcceptTasks принять задачу (идемпотентно: уже accepted когда апстрим возвращает успех или бизнес-подсказку, не считается фатальной ошибкой).
func (c *Client) AcceptTasks(a *auth.Auth, taskCodes []string) error {
	_, err := c.growthJSON(a, http.MethodPost, tasksAcceptPath, map[string]any{"task_codes": taskCodes})
	return err
}

// ClaimReward Получить награду за отдельное задание.
//
// Источник эндпоинта (факт.):Web Запрос на получение награды центра роста ——
//
//	POST https://www.workbuddy.cn/activity/growth/tasks/<task_code>/claim
//	（код задачи в**путь**внутри, нет body；Лента x-client-platform: web Заголовок,Bearer аутентификацию)
//
// Ключевое отличие: ранее ошибочно использовался CLI Домен copilot.tencent.com 
// /v2/activity/growth/tasks/reward/claim（task_code поместить body），Этот путь**не существует**，
// всегда возвращает 400 "task not completed"，является реальной причиной предыдущей неудачи получения награды.
// данная реализация возвращает (credit, energy, err)：credit/energy награда за текущее зачисление (если уже получено — 0）。
func (c *Client) ClaimReward(a *auth.Auth, taskCode string) (credit, energy int64, err error) {
	req, err := http.NewRequest(http.MethodPost,
		c.webBase(a)+"/activity/growth/tasks/"+url.PathEscape(taskCode)+"/claim", nil)
	if err != nil {
		return 0, 0, err
	}
	// Web Форма заголовков запроса клиента (сверка с реальным запросом браузера):Origin/Referer указывает на workbuddy.cn Центр роста,
	// Лента x-client-platform: web Пометить источник.
	req.Header.Set("Authorization", "Bearer "+a.AccessTokenValue())
	req.Header.Set("Accept", "application/json, text/plain, */*")
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Origin", "https://www.workbuddy.cn")
	req.Header.Set("Referer", "https://www.workbuddy.cn/profile/growth-center")
	req.Header.Set("x-client-platform", "web")
	if ua := c.userAgent(a); ua != "" {
		req.Header.Set("User-Agent", ua)
	}
	if a.UID != "" {
		req.Header.Set("X-User-Id", a.UID)
	}
	if a.EnterpriseID != "" {
		req.Header.Set("X-Enterprise-Id", a.EnterpriseID)
		req.Header.Set("X-Tenant-Id", a.EnterpriseID)
	}
	if d := a.DomainValue(); d != "" {
		req.Header.Set("X-Domain", d)
	}

	data, err := c.doJSON(req)
	if err != nil {
		return 0, 0, err
	}
	// Ответ data：{"already_claimed«:bool,«credit»:100,«energy":5,...}
	var resp struct {
		AlreadyClaimed bool `json:"already_claimed"`
		Credit int64 `json:"credit"`
		Energy int64 `json:"energy"`
	}
	if err := json.Unmarshal(data, &resp); err != nil {
		return 0, 0, err
	}
	if resp.AlreadyClaimed {
		return 0, 0, nil // Идемпотентность: повторное получение — не ошибка, но без новой награды
	}
	return resp.Credit, resp.Energy, nil
}
