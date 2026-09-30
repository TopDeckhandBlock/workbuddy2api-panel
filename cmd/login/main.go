// login.go — WorkBuddy OAuth Вход (процесс авторизации устройства,CN realm；--realm=global для международной версии).
//
// Две подкоманды, от login.sh Детерминизм по порядку:
//
//	login [--realm=cn|global] url → POST /v2/plugin/auth/state?platform=CLI Взять state+authUrl，
//	 state Сброс /tmp/wb2api-login-state.json，stdout Печать авторизации URL
//	login [--realm=cn|global] poll → Чтение state，GET /v2/plugin/auth/token?state= Один раз,
//	 После успеха снова GET /v2/plugin/login/account?state= Взять uid/nickname，
//	 stdout вывести полностью token+account JSON（Содержит realm ключа)
//
// --realm По умолчанию cn。Нажать realm Переключение upstream-эндпоинта и Origin/Referer：
//
//	cn → https://copilot.tencent.com（Origin: https://www.codebuddy.cn）
//	global → https://www.workbuddy.ai（Origin: https://www.workbuddy.ai）
//
// state сохранение на диск с realm，poll обратное чтение с проверкой и командная строка --realm совпадает (защита от смешения доменов).
// отсутствует PKCE（workbuddy Device flow выпускается сервером state）。
package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"os"
	"path/filepath"
	"strings"
	"time"

	auth2 "github.com/linguo2625469/workbuddy2api-panel/internal/auth"
)

// константа upstream:CN → copilot.tencent.com（Origin для codebuddy.cn）；global → www.workbuddy.ai
// （base и Origin/Referer тот же домен). Эндпоинт URL От realmConfig Нажать realm Собирается динамически, больше не хардкод.
const (
	upstreamBaseCN = "https://copilot.tencent.com"
	upstreamBaseGlobal = "https://www.workbuddy.ai"
	clientUA = "CLI/2.63.2 CodeBuddy/2.63.2"
	originRefererCN = "https://www.codebuddy.cn"
	originRefererGlobal = "https://www.workbuddy.ai"
)

// логин state путь сохранения на диск (var для удобной подмены временного файла в тестах)
// Portable across OSes: the upstream hardcoded "/tmp/...", which on
// Windows resolves to <drive>:\tmp\... and aborts the OAuth flow with
// "The system cannot find the path specified". os.TempDir() is /tmp on Linux.
var stateFile = filepath.Join(os.TempDir(), "wb2api-login-state.json")

// exitFunc Для подмены в тестах (по умолчанию os.Exit；В тестах временно заменить на panic перехват внутри процесса fatal）。
var exitFunc = os.Exit

// realmConfig Нажать realm Вернуть апстрим base и Origin/Referer origin：global →
// (www.workbuddy.ai, www.workbuddy.ai)；cn/Недопустимый/по умолчанию → (copilot.tencent.com, codebuddy.cn)。
func realmConfig(realm string) (base, origin string) {
	if realm == realmGlobal {
		return upstreamBaseGlobal, originRefererGlobal
	}
	return upstreamBaseCN, originRefererCN
}

// commonHeaders Нажать origin установить общие заголовки запроса (Origin/Referer Случайный realm изменение).
// вернуть func(*http.Request)，Вызывающей стороной по realm Выбранный origin Создать один раз и переиспользовать.
func commonHeaders(origin string) func(*http.Request) {
	return func(req *http.Request) {
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "application/json, text/plain, */*")
		req.Header.Set("X-Requested-With", "XMLHttpRequest")
		req.Header.Set("Origin", origin)
		req.Header.Set("Referer", origin+"/")
		req.Header.Set("User-Agent", clientUA)
	}
}

// apiEnvelope и main.go:429-433 консистентно
type apiEnvelope struct {
	Code int `json:"code"`
	Msg string `json:"msg"`
	Data json.RawMessage `json:"data"`
}

// doJSON и oauth.go:33-66 совпадает:{code,msg,data} конверт,code!=0 → error
func doJSON(client *http.Client, method, fullURL string, headers func(*http.Request), body io.Reader) (json.RawMessage, int, error) {
	req, err := http.NewRequest(method, fullURL, body)
	if err != nil {
		return nil, 0, err
	}
	if headers != nil {
		headers(req)
	} else {
		// заголовок по умолчанию:CN origin（С исходным commonHeaders() Поведение идентично, ноль регрессий)
		commonHeaders(originRefererCN)(req)
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 400 {
		return nil, resp.StatusCode, fmt.Errorf("http_error: upstream %d", resp.StatusCode)
	}
	if resp.StatusCode >= 300 {
		return nil, resp.StatusCode, fmt.Errorf("http_error: upstream redirect %d", resp.StatusCode)
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

func fatal(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "login: "+format+"\n", args...)
	exitFunc(1)
}

type loginState struct {
	State string `json:"state"`
	Realm string `json:"realm,omitempty"` // url записанное при сбросе на диск realm，poll обратное чтение с проверкой от смешения доменов
}

// realm Перечисление значений (с internal/auth Realm() нормализованный вывод совпадает).
const (
	realmCN = "cn"
	realmGlobal = "global"
)

// parseRealmArgs парсинг начала --realm=cn|global（или раздельный --realm <v>）flag，по умолчанию cn。
// Нормализация без учета регистра; невалидное значение/Ошибка отсутствия значения. Порядок остальных параметров (подкоманды) не меняется.
func parseRealmArgs(args []string) (realm string, rest []string, err error) {
	realm = realmCN
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "--realm":
			if i+1 >= len(args) {
				return "", nil, fmt.Errorf("--realm requires a value")
			}
			v := strings.ToLower(strings.TrimSpace(args[i+1]))
			if v != realmCN && v != realmGlobal {
				return "", nil, fmt.Errorf("invalid --realm %q (want cn|global)", args[i+1])
			}
			realm = v
			i++
		case strings.HasPrefix(a, "--realm="):
			v := strings.ToLower(strings.TrimSpace(strings.TrimPrefix(a, "--realm=")))
			if v != realmCN && v != realmGlobal {
				return "", nil, fmt.Errorf("invalid --realm %q (want cn|global)", v)
			}
			realm = v
		default:
			rest = append(rest, a)
		}
	}
	return realm, rest, nil
}

// resolveRealmInput нормализовать однострочный ввод интерактивного выбора домена в realm（Чистая функция,login.sh Ветка взаимодействия
// ключевое решение, тестируемо). Правило:
//
//	"1«/«cn»（без учёта регистра)/«"（Enter по умолчанию)→ cn
//	"2"/"global" → global
//	прочее → ("", false)（Вызывающая сторона возвращает дефолт cn）
func resolveRealmInput(input string) (string, bool) {
	switch strings.ToLower(strings.TrimSpace(input)) {
	case "", "1", "cn":
		return realmCN, true
	case "2", "global":
		return realmGlobal, true
	}
	return "", false
}

// promptRealm интерактивный выбор домена: к out Подсказка параметров печати (out Подключение stderr，stdout оставить для realm сам),
// Из in чтение строки, возврат нормализованного realm。Откат после предупреждения о невалидном вводе cn；EOF（Неинтерактивный/конвейер) откат cn。
func promptRealm(in io.Reader, out io.Writer) string {
	fmt.Fprintln(out, "Выбор версии логина: 1) Версия для материкового Китая(cn) 2) Международная версия(global) [По умолчанию 1/cn]: ")
	line, err := bufio.NewReader(in).ReadString('\n')
	if err != nil && line == "" {
		// EOF/Неинтерактивный → откат к дефолту cn
		return realmCN
	}
	if realm, ok := resolveRealmInput(line); ok {
		return realm
	}
	fmt.Fprintln(out, "Неверный выбор, по умолчанию CN-версия cn")
	return realmCN
}

// validateRealmMatch Валидация state Файл realm и командная строка --realm совпадает (защита от смешения доменов):
// state отсутствует realm（старый файл) пропустить; непусто и не совпадает → error。
func validateRealmMatch(stateRealm, cliRealm string) error {
	if stateRealm != "" && stateRealm != cliRealm {
		return fmt.Errorf("realm mismatch: state file realm=%q, command --realm=%q（url и poll Требуется единый realm）", stateRealm, cliRealm)
	}
	return nil
}

// runURL выполнить url Подкоманда: к upstreamBase state Эндпоинт POST Получить авторизацию URL，
// state сброс на диск (с realm），stdout печать authURL。out Подключение stdout；stateFile — путь сохранения на диск
// （можно инжектировать временный файл для теста). пусто realm считается дефолтом (вызывающая сторона уже нормализовала).
func runURL(base, origin, realm, statePath string, client *http.Client, out io.Writer) {
	headers := commonHeaders(origin)
	data, _, err := doJSON(client, http.MethodPost, base+"/v2/plugin/auth/state?platform=CLI", headers, bytes.NewReader([]byte("{}")))
	if err != nil {
		fatal("auth state failed: %v", err)
	}
	var st struct {
		State string `json:"state"`
		AuthURL string `json:"authUrl"`
	}
	if err := json.Unmarshal(data, &st); err != nil || st.State == "" || st.AuthURL == "" {
		fatal("auth state: missing state or authUrl")
	}
	raw, _ := json.Marshal(loginState{State: st.State, Realm: realm})
	if err := os.WriteFile(statePath, raw, 0o600); err != nil {
		fatal("write state: %v", err)
	}
	fmt.Fprintln(out, st.AuthURL)
}

// runPoll выполнить poll подкоманда: чтение state Файл (realm валидация), в upstreamBase token Эндпоинт
// GET один раз, при успехе снова GET login/account（Лента Bearer），stdout вывести полностью token+account JSON。
// statePath Можно инжектировать временный файл для тестов.
func runPoll(base, origin, realm, statePath string, client *http.Client, out io.Writer) {
	raw, err := os.ReadFile(statePath)
	if err != nil {
		fatal("read state: %v (Сначала запустить login url)", err)
	}
	var ls loginState
	if err := json.Unmarshal(raw, &ls); err != nil {
		fatal("parse state: %v", err)
	}
	// защита от смешения доменов:state сброс на диск realm и командная строка --realm При несоответствии отклонить (url и poll должен быть в том же домене)
	if err := validateRealmMatch(ls.Realm, realm); err != nil {
		fatal("%v", err)
	}
	headers := commonHeaders(origin)
	// handlePollLogin (oauth.go:108-162)：auth/token — авторитетный эндпоинт статуса логина,
	// pending бизнес при code не 0（"login ing"），При завершении code=0 + token bundle
	tokRaw, status, errTok := doJSON(client, http.MethodGet, base+"/v2/plugin/auth/token?state="+ls.State, headers, nil)
	if errTok != nil {
		if status == 0 || status >= 500 {
			fatal("token endpoint error: %v", errTok)
		}
		fatal("логин не завершён (waiting for login）。Убедитесь, что вход в браузере выполнен, затем нажмите y")
	}
	var tok struct {
		AccessToken string `json:"accessToken"`
		RefreshToken string `json:"refreshToken"`
		ExpiresIn int64 `json:"expiresIn"`
		Domain string `json:"domain"`
	}
	if err := json.Unmarshal(tokRaw, &tok); err != nil || tok.AccessToken == "" {
		fatal("логин не завершён (waiting for login）。Убедитесь, что вход в браузере выполнен, затем нажмите y")
	}
	// login/account Взять uid/nickname（Лента Bearer）
	var acct struct {
		UID string `json:"uid"`
		EnterpriseID string `json:"enterpriseId"`
		Nickname string `json:"nickname"`
	}
	acctHeaders := func(r *http.Request) {
		headers(r)
		r.Header.Set("Authorization", "Bearer "+tok.AccessToken)
	}
	if acctRaw, _, errAcct := doJSON(client, http.MethodGet, base+"/v2/plugin/login/account?state="+ls.State, acctHeaders, nil); errAcct == nil {
		_ = json.Unmarshal(acctRaw, &acct)
	}
	oraw, _ := json.Marshal(buildLoginOutput(tok, realm, acct))
	fmt.Fprintln(out, string(oraw))
	os.Remove(statePath)
}

// buildLoginOutput сборка poll Полный вывод JSON（login.sh На основании этого сохранить на диск auth файл).
// realm никогда не пусто: явно --realm Приоритет (ResolveRealm обработка), иначе по ответу апстрима domain Вывод —
// гарантировать сохранение логина на диск auth файл всегда содержит realm ключ.
func buildLoginOutput(tok struct {
	AccessToken string `json:"accessToken"`
	RefreshToken string `json:"refreshToken"`
	ExpiresIn int64 `json:"expiresIn"`
	Domain string `json:"domain"`
}, realm string, acct struct {
	UID string `json:"uid"`
	EnterpriseID string `json:"enterpriseId"`
	Nickname string `json:"nickname"`
}) map[string]any {
	return map[string]any{
		"access_token": tok.AccessToken,
		"refresh_token": tok.RefreshToken,
		"expires_in": tok.ExpiresIn,
		"domain": tok.Domain,
		"realm": auth2.ResolveRealm(realm, tok.Domain),
		"uid": acct.UID,
		"enterprise_id": acct.EnterpriseID,
		"nickname": acct.Nickname,
	}
}

func main() {
	realm, rest, err := parseRealmArgs(os.Args[1:])
	if err != nil {
		fatal("%v (usage: login [--realm=cn|global] <url|poll|realm>)", err)
	}
	if len(rest) < 1 {
		fatal("usage: login [--realm=cn|global] <url|poll>")
	}
	// каждый процесс независим cookie jar（oauth.go:22-29：многопользовательский вход без смешивания сессий)
	jar, _ := cookiejar.New(nil)
	client := &http.Client{Timeout: 30 * time.Second, Jar: jar}

	base, origin := realmConfig(realm)

	switch rest[0] {
	case "url":
		runURL(base, origin, realm, stateFile, client, os.Stdout)

	case "poll":
		runPoll(base, origin, realm, stateFile, client, os.Stdout)

	case "realm":
		// Интерактивный выбор домена (login.sh отсутствует --realm Передача параметра и stdin для tty вызов при).
		// подсказка выводится в stderr，stdout выводить только нормализованное realm，Подача $( ) перехват.
		realm := promptRealm(os.Stdin, os.Stderr)
		fmt.Println(realm)

	default:
		fatal("unknown subcommand %q (want url|poll|realm)", rest[0])
	}
}
