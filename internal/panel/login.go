// login.go встроенный в панель WorkBuddy CN OAuth Процесс авторизации устройства (cmd/login внутрипроцессный порт).
//
//	POST /panel/api/login/start → Взять state+authUrl，state хранение внутри процесса (больше не попадает в /tmp，
//	 Исходный план в Windows недоступно на ), вернуть авторизацию URL；
//	GET /panel/api/login/poll → фронтенд панели каждые 3s опрос этого API; если не завершено — возврат done=false，
//	 получить после завершения uid/nickname、сохранение учётных данных на диск auths/workbuddy-<uid>.json、горячая загрузка в пул
//	 （pool.Add + Revive），И попутно чекин + обновление баланса —— Загрузка новых аккаунтов без перезапуска.
//
// отсутствует PKCE（workbuddy Device flow выпускается сервером state），заголовки запроса и апстрим-эндпоинт и cmd/login сохранять консистентность.
package panel

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
)

const (
	upstreamBaseCN = "https://copilot.tencent.com"
	upstreamBaseGlobal = "https://www.workbuddy.ai"
	clientUA = "CLI/2.63.2 CodeBuddy/2.63.2"
	originRefererCN = "https://www.codebuddy.cn"
	originRefererGlobal = "https://www.workbuddy.ai"
)

// loginEndpoints Нажать realm вернуть три эндпоинта авторизации устройства (auth/state、token、account）+ Origin。
// realm=="global" → Международная версия (workbuddy.ai тот же домен);cn/Недопустимый/по умолчанию → CN（нулевой регресс).
func loginEndpoints(realm string) (state, token, account, origin string) {
	if realm == "global" {
		base := upstreamBaseGlobal
		return base + "/v2/plugin/auth/state?platform=CLI",
			base + "/v2/plugin/auth/token?state=",
			base + "/v2/plugin/login/account?state=",
			originRefererGlobal
	}
	base := upstreamBaseCN
	return base + "/v2/plugin/auth/state?platform=CLI",
		base + "/v2/plugin/auth/token?state=",
		base + "/v2/plugin/login/account?state=",
		originRefererCN
}

// loginHTTP Только для авторизации устройства client：короткий таймаут, без cookie（Каждый запрос несет state，без состояния сессии).
var loginHTTP = &http.Client{Timeout: 30 * time.Second}

func commonHeaders(req *http.Request, origin string) {
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/plain, */*")
	req.Header.Set("X-Requested-With", "XMLHttpRequest")
	req.Header.Set("Origin", origin)
	req.Header.Set("Referer", origin+"/")
	req.Header.Set("User-Agent", clientUA)
}

// validUID валидация возвращенного апстримом uid Можно ли безопасно использовать для сборки имени файла.
// Разрешать только буквы, цифры, подчеркивание, дефис (сторона Tencent uid Фактически измерено как UUID форма),
// Лимит длины 64 Фолбэк: аномально длинная строка; отклонить . / \ и символы пути с пустой строкой.
func validUID(uid string) bool {
	if uid == "" || len(uid) > 64 {
		return false
	}
	for _, c := range uid {
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9', c == '-', c == '_':
		default:
			return false
		}
	}
	return true
}

// apiEnvelope и upstream Та же форма:{code,msg,data}，code!=0 считать бизнес-ошибкой.
type apiEnvelope struct {
	Code int `json:"code"`
	Msg string `json:"msg"`
	Data json.RawMessage `json:"data"`
}

// doJSON отправить один раз JSON Запросить и распаковать конверт.origin для Origin/Referer Базовый домен (с realm переключение).
func doJSON(method, fullURL, bearer string, body io.Reader, origin string) (json.RawMessage, int, error) {
	req, err := http.NewRequest(method, fullURL, body)
	if err != nil {
		return nil, 0, err
	}
	commonHeaders(req, origin)
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	resp, err := loginHTTP.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode >= 300 {
		return nil, resp.StatusCode, fmt.Errorf("http_error: upstream %d", resp.StatusCode)
	}
	var env apiEnvelope
	if err := json.Unmarshal(raw, &env); err != nil {
		return nil, resp.StatusCode, fmt.Errorf("parse failed: %w", err)
	}
	if env.Code != 0 {
		return nil, resp.StatusCode, fmt.Errorf("code=%d msg=%s", env.Code, env.Msg)
	}
	return env.Data, resp.StatusCode, nil
}

// loginStart Инициировать авторизацию устройства:POST auth/state Получить авторизацию URL。
// body может содержать {"realm":«global"}（по умолчанию cn）；state запись сессии realm，poll Совм. realm Сброс на диск.
func (p *Panel) loginStart(w http.ResponseWriter, r *http.Request) {
	realm := "cn"
	if r.Body != nil {
		var reqBody struct {
			Realm string `json:"realm"`
		}
		if err := json.NewDecoder(io.LimitReader(r.Body, 1<<12)).Decode(&reqBody); err == nil {
			if reqBody.Realm == "global" {
				realm = "global"
			}
		}
	}
	epState, _, _, origin := loginEndpoints(realm)
	data, status, err := doJSON(http.MethodPost, epState, "", bytes.NewReader([]byte("{}")), origin)
	if err != nil {
		writeErr(w, http.StatusBadGateway, fmt.Sprintf("auth state (upstream %d): %v", status, err))
		return
	}
	var st struct {
		State string `json:"state"`
		AuthURL string `json:"authUrl"`
	}
	if err := json.Unmarshal(data, &st); err != nil || st.State == "" || st.AuthURL == "" {
		writeErr(w, http.StatusBadGateway, "auth state: missing state or authUrl")
		return
	}
	p.loginMu.Lock()
	// попутно чистить просроченные сессии, защита от"Открыть попап" state Задержка.
	for s, sess := range p.logins {
		if time.Since(sess.created) > loginTTL {
			delete(p.logins, s)
		}
	}
	p.logins[st.State] = loginSession{created: time.Now(), realm: realm}
	p.loginMu.Unlock()
	log.Printf("panel: Инициировать OAuth Добавить аккаунт realm=%s（state=%s...）", realm, st.State[:min(8, len(st.State))])
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "url": st.AuthURL, "state": st.State, "realm": realm})
}

// loginPoll опрос состояния логина. Не завершено → {done:false}；Завершено → Создание учётных данных, запись на диск, hot-reload, check-in.
func (p *Panel) loginPoll(w http.ResponseWriter, r *http.Request) {
	state := r.URL.Query().Get("state")
	if state == "" {
		writeErr(w, http.StatusBadRequest, "missing state")
		return
	}
	p.loginMu.Lock()
	sess, known := p.logins[state]
	p.loginMu.Unlock()
	if !known {
		writeErr(w, http.StatusNotFound, "unknown or expired state（пожалуйста, повторно инициируйте добавление аккаунта)")
		return
	}
	_, epToken, epAcct, origin := loginEndpoints(sess.realm)

	// auth/token — авторитетный эндпоинт статуса логина:pending бизнес при code не 0（"login ing"）。
	tokRaw, _, err := doJSON(http.MethodGet, epToken+state, "", nil, origin)
	if err != nil {
		// pending / Не завершено: фронтенд панели продолжает polling.
		writeJSON(w, http.StatusOK, map[string]any{"done": false, "message": err.Error()})
		return
	}
	var tok struct {
		AccessToken string `json:"accessToken"`
		RefreshToken string `json:"refreshToken"`
		ExpiresIn int64 `json:"expiresIn"`
		Domain string `json:"domain"`
	}
	if err := json.Unmarshal(tokRaw, &tok); err != nil || tok.AccessToken == "" {
		writeJSON(w, http.StatusOK, map[string]any{"done": false, "message": "waiting for login"})
		return
	}

	// выполнено: взять uid/nickname（сбой не блокирует, отсутствует только отображаемое имя).
	var acct struct {
		UID string `json:"uid"`
		EnterpriseID string `json:"enterpriseId"`
		Nickname string `json:"nickname"`
	}
	if acctRaw, _, err := doJSON(http.MethodGet, epAcct+state, tok.AccessToken, nil, origin); err == nil {
		_ = json.Unmarshal(acctRaw, &acct)
	}
	if acct.UID == "" {
		writeErr(w, http.StatusBadGateway, "login done but no uid（token отправлено, но не удалось получить данные аккаунта, повторите)")
		return
	}
	// UID Из ответа апстрима, без валидации используется для сборки имени файла — уязвимость Path Traversal
	// （filepath.Join("./auths", "workbuddy-../../evil.json") → auths/evil.json）。
	// UID — идентификатор аккаунта на стороне Tencent, фактически UUID（hex и дефис), поэтому пропускать только [A-Za-z0-9_-]。
	if !validUID(acct.UID) {
		writeErr(w, http.StatusBadGateway, "Возвращённое апстримом uid Содержит недопустимые символы, запись отклонена (защита от path traversal)")
		return
	}

	// сохранение кредов на диск (вложенная форма, и auths/ Соответствует существующему формату каталога)→ Горячая загрузка в пул.
	if err := os.MkdirAll(p.cfg.AuthDir, 0o755); err != nil {
		writeErr(w, http.StatusInternalServerError, "mkdir auth dir: "+err.Error())
		return
	}
	a := &auth.Auth{
		AccessToken: tok.AccessToken,
		RefreshToken: tok.RefreshToken,
		ExpiresAt: time.Now().Add(time.Duration(tok.ExpiresIn) * time.Second).Unix(),
		Domain: tok.Domain,
		UID: acct.UID,
		EnterpriseID: acct.EnterpriseID,
		Nickname: acct.Nickname,
		FilePath: filepath.Join(p.cfg.AuthDir, fmt.Sprintf("workbuddy-%s.json", acct.UID)),
	}
	// global Логин: сохранение на диск auth.realm=global（Realm() по этому определяется домен; если не указано — зависит от domain откат по суффиксу).
	if sess.realm == "global" {
		if _, err := auth.BackfillRealmFor(a, "global"); err != nil {
			writeErr(w, http.StatusInternalServerError, "set realm: "+err.Error())
			return
		}
	} else {
		// CN также явно дополнить realm Ключ (идемпотентность), чтобы auth Унифицированная форма файлов (с LoadDir выравнивание миграции существующих данных).
		_, _ = a.BackfillRealm()
	}
	if err := a.SaveAtomic(); err != nil {
		writeErr(w, http.StatusInternalServerError, "save auth: "+err.Error())
		return
	}
	p.cfg.Pool.Add(a)
	p.cfg.Pool.Revive(acct.UID) // Новый вход = Ручное восстановление: очистить унаследованные блокировки старого номера/Охлаждение/Circuit Breaker

	// Попутно отметить посещение + Обновление баланса (идемпотентно; сбой не влияет на результат логина, отражается только в поле ответа).
	// realm Ветка:CN Ход DailyCheckin；global отсутствует CN Система чекинов, заменена на активацию при регистрации + trial Получить
	// （D4 Гейт аналогично scheduler：CN Эндпоинт задач для global никаких вызовов не инициировать).
	checkinMsg := ""
	remain := int64(-1)
	total := int64(0)
	if sess.realm == "global" {
		// Регистрация/активация (идемпотентно):region required автоматически подставить регион (первый из whitelist,HK）после — повторная активация.
		// Ошибка не блокирует результат логина (auth уже сохранено на диск), отражается только в поле ответа.
		if activated, err := p.cfg.Upstream.GlobalCompleteRegistration(a); err != nil {
			checkinMsg = "Ошибка активации регистрации: " + err.Error()
			log.Printf("panel: global регистрация активации uid=%s: %v", acct.UID, err)
		} else if activated {
			log.Printf("panel: global регистрация активации uid=%s Завершено", acct.UID)
		}
		// trial Пакет-дозаправка (идемпотентный 14051 = Уже получено, не ошибка).
		if claimed, err := p.cfg.Upstream.ClaimTrial(a); err != nil {
			checkinMsg = joinMsg(checkinMsg, "trial Ошибка получения: "+err.Error())
			log.Printf("panel: global trial uid=%s: %v", acct.UID, err)
		} else if claimed {
			log.Printf("panel: global trial uid=%s уже получено", acct.UID)
		}
	} else {
		if err := p.cfg.Upstream.DailyCheckin(a); err != nil {
			checkinMsg = err.Error()
		}
	}
	if rm, tt, err := p.cfg.Upstream.UserResource(a); err == nil {
		remain, total = rm, tt
		p.cfg.Pool.ReenableIfCredits(acct.UID, rm, tt)
	}

	p.loginMu.Lock()
	delete(p.logins, state)
	p.loginMu.Unlock()
	log.Printf("panel: Новый аккаунт горячо загружен uid=%s nickname=%q realm=%s（Вступает в силу без перезапуска)", acct.UID, acct.Nickname, sess.realm)
	writeJSON(w, http.StatusOK, map[string]any{
		"done": true,
		"uid": acct.UID,
		"nickname": acct.Nickname,
		"realm": sess.realm,
		"credits": remain,
		"credits_total": total,
		"checkin_message": checkinMsg,
	})
}

// joinMsg конкатенация login Сообщение после завершения (несколько частей через ";», пустые пропускаются).
func joinMsg(parts ...string) string {
	out := ""
	for _, s := range parts {
		if s == "" {
			continue
		}
		if out != "" {
			out += "；"
		}
		out += s
	}
	return out
}

// loginRegions вернуть global Регистрация, опциональный регион (panel Используется для попапа выбора региона на фронтенде;CN не вызывать).
// При отсутствии аккаунта — статический fallback из белого списка (только отображение на фронте, без зависимости от upstream).
func (p *Panel) loginRegions(w http.ResponseWriter, r *http.Request) {
	// Статический вайтлист (выровнен с intl-версией web Набор отображения): фронтенд панели только для чтения, состояние аккаунта не требуется.
	writeJSON(w, http.StatusOK, map[string]any{
		"ok": true,
		"regions": []map[string]string{
			{"code": "HK", "name": "Hong Kong"},
			{"code": "MO", "name": "Macao"},
			{"code": "SG", "name": "Singapore"},
			{"code": "TH", "name": "Thailand"},
			{"code": "PH", "name": "Philippines"},
			{"code": "MY", "name": "Malaysia"},
			{"code": "ID", "name": "Indonesia"},
		},
	})
}
