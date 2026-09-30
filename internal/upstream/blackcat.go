// blackcat.go ночная задача (black_cat）+ Стартовый набор/компенсация API。
//
// критерий (WorkBuddy-Daily Фактическая метрика проекта + проверка данным шлюзом):black_cat требуется в
// **23:00–08:00（внутри окна (локальная таймзона)**Завершено 3 Раз glm-5.2 диалог и репорт chat Цепочка событий;
// Действия вне окна не учитываются. Реальный диалог идёт через существующий шлюз ChatStream（glm-5.2），цепочка событий использует
// ReportChatActivityModel（chat_5 та же форма отчета).
package upstream

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
)

// InNightWindow находится ли сейчас в окне подсчёта "совы» (23:00–08:00 локальный часовой пояс).
func InNightWindow(now time.Time) bool {
	h := now.Hour()
	return h >= 23 || h < 8
}

// BlackcatNeed проверить black_cat остаток задач (сколько диалогов осталось выполнить).
// Задача не существует, вернуть 0（нечего делать); при ошибке загрузки вернуть ошибку.
func (c *Client) BlackcatNeed(a *auth.Auth) (int64, error) {
	tasks, err := c.ListTasks(a)
	if err != nil {
		return 0, err
	}
	for _, t := range tasks {
		if t.TaskCode == "black_cat" {
			if t.Claimed || t.Current >= t.Target {
				return 0, nil
			}
			return t.Target - t.Current, nil
		}
	}
	return 0, nil
}

// RunNightChats Ночная сова: отправка need Раз glm-5.2 Реальный диалог (чтение сухого потока) и отправка цепочки событий.
// Вернуть число успехов. Содержимое диалога очень короткое (1+1），потребление пренебрежимо мало.
func (c *Client) RunNightChats(a *auth.Auth, need int) (int64, error) {
	var ok int64
	for i := 0; i < need; i++ {
		body, _ := json.Marshal(map[string]any{
			"model": "glm-5.2",
			"messages": []map[string]any{{"role": "user", "content": "1+1равно скольки? Ответь прямо."}},
			"stream": true,
		})
		rc, status, respBody, err := c.ChatStream(a, body, "", ChatMeta{})
		if err != nil || status >= 400 {
			if rc != nil {
				rc.Close()
			}
			return ok, fmt.Errorf("№ %d неудач диалога N раз: http=%d err=%v body=%.120s", i+1, status, err, respBody)
		}
		_, _ = io.Copy(io.Discard, io.LimitReader(rc, 1<<20))
		rc.Close()
		if err := c.ReportChatActivityModel(a, fmt.Sprintf("wb2api-night-%d-%d", time.Now().UnixMilli(), i), "", "glm-5.2", "GLM-5.2"); err != nil {
			return ok, fmt.Errorf("№ %d неудач отправки отчета: %w", i+1, err)
		}
		ok++
		time.Sleep(4 * time.Second)
	}
	return ok, nil
}

// ClaimGift Получение стартового набора (один раз на аккаунт, повтор — бизнес-ошибка).
func (c *Client) ClaimGift(a *auth.Auth) (int64, error) {
	data, err := c.billingJSON(a, http.MethodPost, "/billing/meter/claim-gift", map[string]any{})
	if err != nil {
		return 0, err
	}
	var resp struct {
		Credit int64 `json:"credit"`
	}
	_ = json.Unmarshal(data, &resp)
	return resp.Credit, nil
}

// ClaimCompensation Получить компенсацию активности (есть — забрать, нет — бизнес-ошибка).
func (c *Client) ClaimCompensation(a *auth.Auth) (int64, error) {
	data, err := c.billingJSON(a, http.MethodPost, "/billing/meter/claim-compensation", map[string]any{})
	if err != nil {
		return 0, err
	}
	var resp struct {
		Credit int64 `json:"credit"`
	}
	_ = json.Unmarshal(data, &resp)
	return resp.Credit, nil
}

// HeatmapYesterdayMissed Проверить пропуск чекина вчера (heatmap cell score==0）。
func (c *Client) HeatmapYesterdayMissed(a *auth.Auth) (bool, error) {
	yesterday := time.Now().AddDate(0, 0, -1).Format("2006-01-02")
	data, err := c.growthJSON(a, http.MethodGet, "/activity/growth/heatmap", nil)
	if err != nil {
		return false, err
	}
	var resp struct {
		Cells []struct {
			Date string `json:"date"`
			Score int `json:"score"`
		} `json:"cells"`
	}
	if err := json.Unmarshal(data, &resp); err != nil {
		return false, err
	}
	for _, cell := range resp.Cells {
		if len(cell.Date) >= 10 && cell.Date[:10] == yesterday {
			return cell.Score == 0, nil
		}
	}
	return false, nil
}

// UseMakeupCard использовать картуДоп. подпись на указанную дату (сохраняет streak непрерывных входов; без карты — бизнес-ошибка).
func (c *Client) UseMakeupCard(a *auth.Auth, date string) error {
	_, err := c.growthJSON(a, http.MethodPost, "/activity/growth/makeup-cards/use",
		map[string]any{"target_date": date})
	return err
}
