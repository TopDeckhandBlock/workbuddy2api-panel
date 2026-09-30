// global_register.go Международная версия (global realm）Регистрация и активация нового аккаунта и доработка региона.
//
// Фон (ANALYSIS-workbuddy-client-reverse.md）：новый global Аккаунту необходимо сначала завершить регион регистрации
// （/login/register/user/complete дополнить регион) повторить вызов register активация интерфейса Trial，chat только тогда не будет ошибки
// 14017 trial not activated。Трейс (обратно от web Страница дозаполнения регистрации RegisterRegion-*.js）：
//
//	POST /billing/area/get-country-code {filterForbidden:1} → список доступных стран
//	POST /billing/area/get-user-area-info {action:getUserAreaInfo} → Определить текущий регион
//	POST /console/login/account {attributes:{countryCode,countryFullName,countryName}} → отправка региона (идемпотентно)
//	GET /auth/realms/copilot/overseas/user/register?userId=<uid> → регистрация активации (code:200 Успех;code:500 "region required" требуется указать регион)
//	POST /billing/ide/trial → одноразовый пакет пополнения (идемпотентный ключ 14051，См. trial.go）
//
// Внимание к телу ответа:get-country-code / get-user-area-info data Да JSON Строка
// （двойной конверт), требуется повторный парсинг.
package upstream

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
)

// globalWebUA Международная версия web Конец UA（Страница дозаполнения регистрации идет через web Отпечаток, не десктоп CLI отпечаток).
const globalWebUA = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 " +
	"(KHTML, like Gecko) Chrome/120.0.0.0 Safari/537.36"

// GlobalCountry опциональный регион регистрации (соответствует get-country-code list элементов).
type GlobalCountry struct {
	EnName string `json:"EnName"` // Полное имя на английском (countryFullName）
	Name string `json:"Name"` // отображаемое имя
	IOS2 string `json:"IOS2"` // двухбуквенный код (countryName）
	IOS3 string `json:"IOS3"`
	Code string `json:"Code"` // Цифровой код (countryCode）
}

// globalRegisterBase регистрации эндпоинта активации base（Цепочка регистрации в www.workbuddy.ai，и globalBillingBase тот же домен).
// вынесено в отдельный метод для тестируемости/подмены.
func (c *Client) globalRegisterBase() string {
	if c.BillingBaseGlobal != "" {
		return c.BillingBaseGlobal
	}
	return defaultGlobalBase
}

// globalRegisterReq общая конструкция запроса цепочки регистрации:web Фингерпринт UA + Origin/Referer Тот же домен + Bearer。
func (c *Client) globalRegisterReq(method, url, token string, body any) (*http.Request, error) {
	var rdr io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		rdr = bytes.NewReader(raw)
	}
	req, err := http.NewRequest(method, url, rdr)
	if err != nil {
		return nil, err
	}
	base := c.globalRegisterBase()
	req.Header.Set("User-Agent", globalWebUA)
	req.Header.Set("Accept", "application/json, text/plain, */*")
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Origin", base)
	req.Header.Set("Referer", base+"/")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	return req, nil
}

// globalRegisterJSON отправить запрос цепочки регистрации и распаковать внешний конверт (code/msg）。
func (c *Client) globalRegisterJSON(req *http.Request) (code int, msg string, raw json.RawMessage, err error) {
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return 0, "", nil, err
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	var env struct {
		Code int `json:"code"`
		Msg string `json:"msg"`
		Data json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(data, &env); err != nil {
		return 0, "", nil, fmt.Errorf("global register parse: %w", err)
	}
	return env.Code, env.Msg, env.Data, nil
}

// GlobalFetchCountries получить список доступных регионов регистрации (global вызов после логина аккаунта).
// intlOnly=true тогда по международной версии web фильтрация по белому списку (HK/MO/SG/TH/PH/MY/ID，Выравнивание web набор отображения).
func (c *Client) GlobalFetchCountries(a *auth.Auth, intlOnly bool) ([]GlobalCountry, error) {
	if a == nil || a.Realm() != "global" {
		return nil, fmt.Errorf("fetch countries: only global accounts")
	}
	req, err := c.globalRegisterReq(http.MethodPost, c.globalRegisterBase()+"/billing/area/get-country-code", a.AccessTokenValue(), map[string]any{"filterForbidden": 1})
	if err != nil {
		return nil, err
	}
	code, msg, raw, err := c.globalRegisterJSON(req)
	if err != nil {
		return nil, err
	}
	if code != 0 {
		return nil, fmt.Errorf("get-country-code: %s (code=%d)", msg, code)
	}
	// data Да JSON Строка (двойной конверт) или объект, требуется вторичный парсинг.
	var inner struct {
		Data struct {
			List []GlobalCountry `json:"list"`
		} `json:"data"`
	}
	if s := strings.TrimSpace(string(raw)); strings.HasPrefix(s, "\"") {
		var s2 string
		if err := json.Unmarshal(raw, &s2); err != nil {
			return nil, fmt.Errorf("country list unwrap: %w", err)
		}
		raw = json.RawMessage(s2)
	}
	if err := json.Unmarshal(raw, &inner); err != nil {
		return nil, fmt.Errorf("country list parse: %w", err)
	}
	list := inner.Data.List
	if !intlOnly {
		return list, nil
	}
	// Международная версия web фильтрация по белому списку (HK, MO, SG, TH, PH, MY, ID，выравнивание порядка web отображение).
	whitelist := []string{"HK", "MO", "SG", "TH", "PH", "MY", "ID"}
	byCode := make(map[string]GlobalCountry, len(list))
	for _, ctry := range list {
		byCode[ctry.IOS2] = ctry
	}
	out := make([]GlobalCountry, 0, len(whitelist))
	for _, code := range whitelist {
		if ctry, ok := byCode[code]; ok {
			out = append(out, ctry)
		}
	}
	return out, nil
}

// GlobalRegisterStatus отчет global статус регистрации/активации аккаунта: требуется ли дополнить регион, активирован ли.
func (c *Client) GlobalRegisterStatus(a *auth.Auth) (activated bool, needsRegion bool, msg string, err error) {
	if a == nil || a.Realm() != "global" {
		return false, false, "", fmt.Errorf("register status: only global accounts")
	}
	req, err := c.globalRegisterReq(http.MethodGet,
		c.globalRegisterBase()+"/auth/realms/copilot/overseas/user/register?userId="+a.UID,
		a.AccessTokenValue(), nil)
	if err != nil {
		return false, false, "", err
	}
	req.Header.Set("X-User-Id", a.UID)
	code, m, _, err := c.globalRegisterJSON(req)
	if err != nil {
		return false, false, "", err
	}
	switch {
	case code == 200:
		return true, false, "register success", nil
	case code == 500 || strings.Contains(strings.ToLower(m), "region required"):
		return false, true, m, nil
	default:
		return false, false, m, nil
	}
}

// GlobalSubmitRegion Отправка региона регистрации (идемпотентно).country из GlobalFetchCountries。
func (c *Client) GlobalSubmitRegion(a *auth.Auth, country GlobalCountry) error {
	if a == nil || a.Realm() != "global" {
		return fmt.Errorf("submit region: only global accounts")
	}
	attrs := map[string]any{
		"countryCode": []string{country.Code},
		"countryFullName": []string{country.EnName},
		"countryName": []string{country.IOS2},
	}
	req, err := c.globalRegisterReq(http.MethodPost, c.globalRegisterBase()+"/console/login/account",
		a.AccessTokenValue(), map[string]any{"attributes": attrs})
	if err != nil {
		return err
	}
	code, msg, _, err := c.globalRegisterJSON(req)
	if err != nil {
		return err
	}
	if code != 0 {
		return fmt.Errorf("submit region: %s (code=%d)", msg, code)
	}
	return nil
}

// GlobalCompleteRegistration Активация регистрацией в один клик: сначала проверить статус, при необходимости дополнить регион по региону по умолчанию (первый из вайтлиста,
// Обычно HK）Повторная активация после коммита.activated Означает, что аккаунт активен и доступен после вызова. Идемпотентно (если уже активен — прямой возврат).
// Ошибка не блокирует вызывающую сторону (panel login уже сохранено на диск), возвращается только ошибка для лога.
func (c *Client) GlobalCompleteRegistration(a *auth.Auth) (activated bool, err error) {
	activated, needsRegion, msg, err := c.GlobalRegisterStatus(a)
	if err != nil {
		return false, err
	}
	if activated {
		return true, nil
	}
	if !needsRegion {
		return false, fmt.Errorf("register not activated: %s", msg)
	}
	// требуется указать регион: загрузить белый список, взять первый (HK）Отправить.
	countries, err := c.GlobalFetchCountries(a, true)
	if err != nil {
		return false, fmt.Errorf("fetch countries: %w", err)
	}
	if len(countries) == 0 {
		return false, fmt.Errorf("no countries available")
	}
	if err := c.GlobalSubmitRegion(a, countries[0]); err != nil {
		return false, fmt.Errorf("submit region: %w", err)
	}
	// Повторная проверка активации.
	activated, needsRegion, msg, err = c.GlobalRegisterStatus(a)
	if err != nil {
		return false, err
	}
	if !activated {
		return false, fmt.Errorf("register still not activated after region submit: %s", msg)
	}
	return true, nil
}
