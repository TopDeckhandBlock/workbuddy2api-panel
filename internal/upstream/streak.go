// streak.go Центр роста — обмен за непрерывный вход + Розыгрыш API（2026-09-12 Центр роста bundle обратный + фактически проверено на странице).
//
// Механизм: уровни за последовательный вход (7d/14d/28d）Нажать**Дней непрерывного входа**Разблокировка; обмен (POST /activity/growth/redeem）
// Отправка credit/energy/Карта доп. чекина/**Количество розыгрышей**；Розыгрыш (POST /activity/growth/lottery/draw）расход за раз
// 1 Раз chances。Возврат погашения без разблокировки HTTP 403「Недостаточно дней подряд входа».
// client_token Идемпотентный токен, сгенерированный фронтендом (randomUUID）。
package upstream

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"

	"fmt"
	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"net/http"
)

// streakRedeemPath / lottery путь (growth домен,growthJSON Ход www.workbuddy.cn）。
const (
	streakRedeemPath = "/activity/growth/redeem"
	lotterySummaryPath = "/activity/growth/lottery/summary"
	lotteryDrawPath = "/activity/growth/lottery/draw"
)

// clientToken идемпотентный токен (фронтенд randomUUID та же семантика).
func clientToken() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return fmt.Sprintf("%s-%s-%s-%s-%s",
		hex.EncodeToString(b[0:4]), hex.EncodeToString(b[4:6]), hex.EncodeToString(b[6:8]),
		hex.EncodeToString(b[8:10]), hex.EncodeToString(b[10:16]))
}

// StreakFull Полный статус последовательного логина (GET /activity/growth/streak）。
type StreakFull struct {
	Streak struct {
		Days int `json:"days"`
		MonthTotalDays int `json:"month_total_days"`
		NextTier string `json:"next_tier"`
		NextTierRemaining int `json:"next_tier_remaining"`
	} `json:"streak"`
	MakeupCards struct {
		Balance int `json:"balance"`
		Max int `json:"max"`
	} `json:"makeup_cards"`
	RedemptionStatus struct {
		Tier7dStatus string `json:"tier_7d_status"`
		Tier14dStatus string `json:"tier_14d_status"`
		Tier28dStatus string `json:"tier_28d_status"`
		RemainingDays int `json:"remaining_days"`
		Tiers []struct {
			Tier string `json:"tier"`
			Days int `json:"days"`
			Credit int `json:"credit"`
			Energy int `json:"energy"`
			Cards int `json:"cards"`
			Chances int `json:"chances"`
		} `json:"tiers"`
	} `json:"redemption_status"`
}

// GrowthStreakFull вытянуть полный статус последовательного входа.
func (c *Client) GrowthStreakFull(a *auth.Auth) (*StreakFull, error) {
	data, err := c.growthJSON(a, http.MethodGet, streakPath, nil)
	if err != nil {
		return nil, err
	}
	out := &StreakFull{}
	if err := json.Unmarshal(data, out); err != nil {
		return nil, err
	}
	return out, nil
}

// GrowthRedeemTier уровень обмена за серию входов (tier: "7d«|«14d»|«28d"）。
// возврат без разблокировки *Error（HTTP 403「недостаточно дней подряд»), вызывающая сторона по locked достаточно пропустить статус.
func (c *Client) GrowthRedeemTier(a *auth.Auth, tier string) error {
	_, err := c.growthJSON(a, http.MethodPost, streakRedeemPath,
		map[string]any{"tier": tier, "client_token": clientToken()})
	return err
}

// LotteryChances Текущее число розыгрышей (GET /activity/growth/lottery/summary）。
func (c *Client) LotteryChances(a *auth.Auth) (int, error) {
	data, err := c.growthJSON(a, http.MethodGet, lotterySummaryPath, nil)
	if err != nil {
		return 0, err
	}
	var resp struct {
		Chances int `json:"chances"`
		Module struct {
			Enabled bool `json:"enabled"`
		} `json:"module"`
	}
	if err := json.Unmarshal(data, &resp); err != nil {
		return 0, err
	}
	return resp.Chances, nil
}

// LotteryDraw одна лотерея, возврат исходной нагрузки приза (prize форма полей определяется периодом активности, проксируется вызывающей стороне).
func (c *Client) LotteryDraw(a *auth.Auth) (json.RawMessage, error) {
	return c.growthJSON(a, http.MethodPost, lotteryDrawPath,
		map[string]any{"client_token": clientToken()})
}
