// travel.go growth домен интерфейса "Кошачье путешествие»: опрос статуса / Отправить / Получение награды / адопция / Протокол.
// Всё идёт через chatBase（copilot.tencent.com，без /v2 префикс)+ BillingHeaders，конверт аналогично doJSON。
package upstream

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
)

// growth путь домена (факт. замер).
const (
	travelStatusPath = "/activity/growth/buddy/travel/status"
	travelDepartPath = "/activity/growth/buddy/travel/depart"
	travelClaimPath = "/activity/growth/buddy/travel/claim"
	buddyInfoPath = "/activity/growth/buddy/info"
	buddyFirstPath = "/activity/growth/buddy/first"
	buddyAgreementPath = "/activity/growth/buddy/agreement"
	streakPath = "/activity/growth/streak"
)

// buddyTaskIncompleteMarker Ключевые слова бизнес-ошибки "не достигнут порог принятия" (HTTP 400 появляется).
const buddyTaskIncompleteMarker = "first_buddy task not completed yet"

// Buddy текущий профиль кота аккаунта;nil（data.buddy для null）означает отсутствие кота.
type Buddy struct {
	ID int64 `json:"id"`
	Name string `json:"name"`
}

// TravelState Статус путешествия котика.
type TravelState struct {
	State string `json:"state"` // idle / traveling / arrived
	DailyLimitReached bool `json:"daily_limit_reached"` // сегодня уже выдавался (календарные сутки 00:00 CST сброс)
	RecordID int64 `json:"record_id"` // в пути/запись о прибытии id，claim обязательное
	RewardCredit int64 `json:"reward_credit"` // по прибытии можно получить бонусные баллы
}

// growthJSON Отправка growth доменный запрос и распаковка конверта;body для nil без тела запроса.
// семантика ошибки и doJSON совпадает:HTTP не 2xx / Бизнес code != 0 → *Error。
func (c *Client) growthJSON(a *auth.Auth, method, path string, body any) (json.RawMessage, error) {
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
	return c.doJSON(req)
}

// TravelStatus Запрос статуса путешествия котика.
func (c *Client) TravelStatus(a *auth.Auth) (*TravelState, error) {
	data, err := c.growthJSON(a, http.MethodGet, travelStatusPath, nil)
	if err != nil {
		return nil, err
	}
	var st TravelState
	if err := json.Unmarshal(data, &st); err != nil {
		return nil, err
	}
	return &st, nil
}

// TravelDepart Отправить кота в путешествие;locationID На практике 1~4（Выгода/интервал длительности тот же).
func (c *Client) TravelDepart(a *auth.Auth, locationID int) error {
	_, err := c.growthJSON(a, http.MethodPost, travelDepartPath, map[string]any{"location_id": locationID})
	return err
}

// TravelClaim получить награду за прибытие, вернуть reward_credit。
func (c *Client) TravelClaim(a *auth.Auth, recordID int64) (int64, error) {
	data, err := c.growthJSON(a, http.MethodPost, travelClaimPath, map[string]any{"record_id": recordID})
	if err != nil {
		return 0, err
	}
	var resp struct {
		RewardCredit int64 `json:"reward_credit"`
	}
	if len(data) > 0 {
		// Отсутствие поля награды не считается ошибкой: вызывающая сторона по 0 достаточно залогировать.
		_ = json.Unmarshal(data, &resp)
	}
	return resp.RewardCredit, nil
}

// BuddyInfo запрос текущего профиля кота; возврат (nil, nil) означает отсутствие кота (data.buddy для null）。
func (c *Client) BuddyInfo(a *auth.Auth) (*Buddy, error) {
	data, err := c.growthJSON(a, http.MethodGet, buddyInfoPath, nil)
	if err != nil {
		return nil, err
	}
	var resp struct {
		Buddy json.RawMessage `json:"buddy"`
	}
	if err := json.Unmarshal(data, &resp); err != nil {
		return nil, err
	}
	// null / Отсутствует поле / пустые объекты обрабатывать как отсутствие cat.
	trimmed := strings.TrimSpace(string(resp.Buddy))
	if trimmed == "" || trimmed == "null" {
		return nil, nil
	}
	var b Buddy
	if err := json.Unmarshal(resp.Buddy, &b); err != nil {
		return nil, err
	}
	return &b, nil
}

// BuddyFirst Приютить первого кота. Нет кота и уже прошло conversation При достижении порога отправить 300 баллов.
// При недостижении порога вернуть HTTP 400（См. IsBuddyTaskIncomplete），Ожидаемое поведение, вызывающая сторона тихо пропускает.
func (c *Client) BuddyFirst(a *auth.Auth) error {
	_, err := c.growthJSON(a, http.MethodPost, buddyFirstPath, map[string]any{})
	return err
}

// BuddyAgreement Принятие соглашения (идемпотентно, повторный вызов без побочных эффектов).
func (c *Client) BuddyAgreement(a *auth.Auth) error {
	_, err := c.growthJSON(a, http.MethodPost, buddyAgreementPath, map[string]any{"agree": true})
	return err
}

// GrowthStreak Запрос дней непрерывного входа (только чтение oracle）。Ответ `data.streak.days`（probe_active.py
// фактическая методика измерения:`(s.get("data«, {}).get("streak", {}) or {}).get("days")`）。
// GET сбой (HTTP не 2xx / Бизнес code != 0）вернуть *Error；нехватка streak/days поле возвращает 0
// （days==0 т.е. активная самопроверка "отчет 200 но сигнал тревоги "тихо отбрасывается»).
func (c *Client) GrowthStreak(a *auth.Auth) (int, error) {
	data, err := c.growthJSON(a, http.MethodGet, streakPath, nil)
	if err != nil {
		return 0, err
	}
	var resp struct {
		Streak struct {
			Days int `json:"days"`
		} `json:"streak"`
	}
	if err := json.Unmarshal(data, &resp); err != nil {
		return 0, err
	}
	return resp.Streak.Days, nil
}

// IsBuddyTaskIncomplete Проверка "порог принятия не достигнут":HTTP 400 + first_buddy ключевые слова.
// Эту ошибку не ретраить в течение дня (избежать шторма ретраев на апстрим).
func IsBuddyTaskIncomplete(err error) bool {
	if err == nil {
		return false
	}
	var ue *Error
	if !errors.As(err, &ue) || ue.Status != http.StatusBadRequest {
		return false
	}
	return strings.Contains(strings.ToLower(ue.Msg), buddyTaskIncompleteMarker)
}
