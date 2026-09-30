// trial.go global Эксклюзивный "одноразовый trial получение пакета "дозаправка»:POST {billingBase}/billing/ide/trial。
// Только global Применимо к аккаунту (CN нет такой конечной точки); идемпотентный код 14051 = Уже получено (считается нормой, не ошибкой).
// это global единственное естественное действие начисления баллов (без чекина/центр задач, см. PLAN D4）。
package upstream

import (
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
)

// trialPath global trial Эндпоинт пакета дозаправки (Maquer/workbuddy-checkin факт. замер).
const trialPath = "/billing/ide/trial"

// trialAlreadyMarkers Код идемпотентности 14051「два отпечатка "уже получено»:
// - "code=14051"：doJSON Для HTTP 200 + Бизнес code не 0 собранный в момент Msg Формат;
// - `"code":14051`：HTTP ≥400 Время doJSON исходный JSON body Прямо поместить в Msg。
var trialAlreadyMarkers = []string{"code=14051", `"code":14051`}

// ClaimTrial Получить одноразовый trial доп-пакет. Только global Настраивается на уровне аккаунта (CN нет такого эндпоинта):
// не global → сразу ошибка (слой tools дополнительно проверит, это защита на стороне клиента).
// вернуть claimed：true=успешно получено заново;false=Уже получено (идемпотентно, не считается ошибкой).
func (c *Client) ClaimTrial(a *auth.Auth) (claimed bool, err error) {
	if a == nil || a.Realm() != "global" {
		return false, fmt.Errorf("claim trial: only global accounts")
	}
	_, err = c.billingJSON(a, http.MethodPost, trialPath, nil)
	if err != nil {
		var ue *Error
		if errors.As(err, &ue) && trialAlreadyErr(ue.Msg) {
			return false, nil // Уже получено: идемпотентный успех, не ошибка
		}
		return false, err
	}
	return true, nil
}

// trialAlreadyErr Ошибка решения Msg Наличие кода идемпотентности 14051（уже получено).
func trialAlreadyErr(msg string) bool {
	for _, m := range trialAlreadyMarkers {
		if strings.Contains(msg, m) {
			return true
		}
	}
	return false
}
