// Package auth Парсинг WorkBuddy auth файл (вложенная форма/плоская двойная форма),
// Предоставляет refresh атомарная обратная запись после.
package auth

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/logfmt"
)

// Auth Это нормализованные учетные данные аккаунта (источник может быть плагином OAuth вложенная или рукописная плоская форма).
type Auth struct {
	// mu Сериализация RefreshToken Запись и SaveAtomic чтение, предотвращает запись полуобновления при конкуренции token。
	mu sync.Mutex

	AccessToken string
	RefreshToken string
	ExpiresAt int64 // Unix с
	Domain string
	// realm Домен аккаунтов ("cn« / "global"），Сохраняется в auth.realm（вложенная форма) или верхний уровень realm（плоская форма).
	// пустой = По умолчанию:Realm() Нажать domain Фолбэк по суффиксу, в итоге всегда непусто.
	//
	// Примечание к именованию:Go Поля и методы не могут одноимённо совпадать, персистентные поля — неэкспортируемые realm，для вычислительного аксессора
	// экспортированный Realm()（межпакетные вызовы только через методы).Parse/SaveAtomic/login Чтение/запись полей внутри пакета.
	realm string
	UID string
	EnterpriseID string
	Nickname string
	FilePath string // исходный файл;refresh после атомарно записать обратно сюда

	// DeviceToken Риск-контроль устройства Token（X-Device-Token заголовок), источник auth файла device_token ключ.
	// по умолчанию пусто = заголовок не инжектируется (в контейнере нет десктопа Turing SDK типичный деплой).
	// рукописная плоская форма auth Файл доступен для прямой записи "device_token«: »..."；Плагин OAuth Вложенная форма
	// Верхний уровень device_token также будет распарсено (деплой с общим файлом состояния для десктопа).
	DeviceToken string
}

// Lock Для других пакетов в том же процессе (upstream.RefreshToken）При перезаписи Auth Блокировка на время поля.
func (a *Auth) Lock() { a.mu.Lock() }

// Unlock Освободить a.Lock полученная блокировка.
func (a *Auth) Unlock() { a.mu.Unlock() }

// AccessTokenValue Чтение с блокировкой AccessToken（исходящие заголовки брать только отсюда, не читать поля напрямую).
//
// Почему нужна блокировка:RefreshToken В a.mu перезапись внутри AccessToken/RefreshToken/Domain/ExpiresAt
// （client.go「№ 2 участок (внутри блокировки): запись обратно после проверки консистентности снапшота»), а формирование всех исходящих заголовков
// （ChatHeaders / BillingHeaders / fetchEnterpriseModels / fetchV3Models /
// global_models）с планировщиком token Проверки читают эти поля вне блокировки. На проде обе стороны реально конкурентны:
// Scheduler.RunKeepaliveNow По расписанию для**Каждый**Обновление незаблокированных аккаунтов (независимо от наличия активных запросов),
// И handler основано на**тот же** *auth.Auth указатель формирует заголовок запроса (Pool.AuthByUID/List Возвращённый
// — это тот же объект в пуле). Несинхронизированное чтение — гонка данных (go test -race подтверждено).
func (a *Auth) AccessTokenValue() string {
	if a == nil {
		return ""
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.AccessToken
}

// DomainValue Чтение с блокировкой Domain（Совм. AccessTokenValue：RefreshToken перезаписать его внутри блокировки).
func (a *Auth) DomainValue() string {
	if a == nil {
		return ""
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.Domain
}

// RefreshTokenValue Чтение с блокировкой RefreshToken（Совм. AccessTokenValue：RefreshToken внутри блокировки
// перезаписывает его). Прегард планировщика "наличие учётных данных» (checkin/keepalive/travel 
// `a.RefreshToken == ""`）Получать значение только через это, не читать поле напрямую.
func (a *Auth) RefreshTokenValue() string {
	if a == nil {
		return ""
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.RefreshToken
}

// globalEnabled Глобальный переключатель:global realm Роутить ли (D5 двойная защита).
// Включено по умолчанию (с config global.enabled по умолчанию true совпадает):Realm() В норме по явному realm/
// domain решение global/cn。Явно SetGlobalEnabled(false)（config "enabled": false）Закрыть
// → Аварийный выход: только CN деплой, даже если auth файл записан realm=global Или domain для .workbuddy.ai
// также всегда считается cn——「единственный шлюз "блокировка только после закрытия» сводится к Realm()/IsGlobal() в.
var globalEnabled atomic.Bool

func init() { globalEnabled.Store(true) }

// SetGlobalEnabled инжект global realm Переключатель маршрутизации (false = Залочить чисто CN，аварийный выход).
func SetGlobalEnabled(enabled bool) { globalEnabled.Store(enabled) }

// GlobalEnabled отчет global realm Текущее состояние переключателя маршрутизации (тест/наблюдаемость эксплуатации).
func GlobalEnabled() bool { return globalEnabled.Load() }

// Realm Возвращает нормализованный домен аккаунта: явно Realm=="global« Или domain суффикс .workbuddy.ai → "global"，
// Иначе "cn"。Явно global имеет приоритет над domain Откат (D1）。
// глобальный свитч SetGlobalEnabled(false) при всегда "cn"（Аварийный выход: только CN заблокировано, не влияет на поведение по умолчанию).
// пустой realm + пустой domain → "cn"（Старый CN Сброс учетных данных к нулю).
func (a *Auth) Realm() string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.realmLocked()
}

// realmLocked Realm внутренняя реализация без блокировок: только**уже удерживается a.mu** используется вызывающей стороной (sync.Mutex Нереентерабельно,
// повторный вызов под блокировкой Realm() будет самоблокировка).realm От BackfillRealm Перезапись,Domain От RefreshToken внутри блокировки
// Перезапись, поэтому чтение должно быть под той же блокировкой, что и запись (см. AccessTokenValue комментарий).
func (a *Auth) realmLocked() string {
	if !globalEnabled.Load() {
		return "cn"
	}
	if strings.TrimSpace(a.realm) == "global" || isGlobalDomain(a.Domain) {
		return "global"
	}
	return "cn"
}

// ResolveRealm Нормализация realm（cn/global）：приоритет явно непустому, иначе по исходному domain инференс
// （isGlobalDomain）。Не зависит от escape hatch (escape hatch — блокировка маршрута, не должен влиять на определение идентификатора);
// domain также пусто → "cn"（Старый CN Сброс учетных данных к нулю).
func ResolveRealm(explicit, domain string) string {
	if r := strings.TrimSpace(explicit); r != "" {
		return r
	}
	if isGlobalDomain(domain) {
		return "global"
	}
	return "cn"
}

// BackfillRealm По умолчанию realm персистентно проставить метку для помеченного аккаунта:a.realm При пустоте — по "исходному domain вывод»
// запись обратно (cn/global），вернуть (Есть ли изменения, После нормализации realm)。Существующая метка не меняется (идемпотентно).
//
// осторожно используйте isGlobalDomain(a.Domain) Прямой вывод, а не Realm()——Realm() в аварийном выходе
// （SetGlobalEnabled(false)）постоянная деградация при cn，взять global Аккаунт захардкожен как cn навсегда загрязнит учетные данные
// （Аварийный выход — чисто CN временная блокировка деплоя, не должна перезаписывать данные на диске).domain также пусто — писать "cn"（Старый CN Учётные данные).
func (a *Auth) BackfillRealm() (bool, string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if strings.TrimSpace(a.realm) != "" {
		return false, a.realm
	}
	r := ResolveRealm("", a.Domain)
	a.realm = r
	return true, r
}

// RealmStored прямое чтение персистентного realm Идентификатор (может быть пустым = Не backfill старый файл,Realm() Будет fallback）。
func (a *Auth) RealmStored() string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.realm
}

// BackfillRealmFor Явная запись realm идентификатор (путь логина вне пакета:panel login Известно, что пользователь выбрал
// global，Прямая запись на диск realm=global，не зависит от domain вывод по суффиксу).realm должен быть cn/global，
// ошибка невалидного значения (защита от грязной записи). Вернуть, было ли изменение.
func BackfillRealmFor(a *Auth, realm string) (bool, error) {
	if a == nil {
		return false, fmt.Errorf("nil auth")
	}
	switch strings.TrimSpace(realm) {
	case "cn", "global":
	default:
		return false, fmt.Errorf("realm must be cn/global, got %q", realm)
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.realm == realm {
		return false, nil
	}
	a.realm = realm
	return true, nil
}

// IsGlobal Сообщить, принадлежит ли аккаунт к global realm（= Realm() == "global"）。
func (a *Auth) IsGlobal() bool { return a.Realm() == "global" }

// isGlobalDomain решение domain указывает ли на www.workbuddy.ai Семейство.
// одновременно принимает голый домен workbuddy.ai и любой поддомен (HasSuffix("www.workbuddy.ai") или сам bare-домен).
func isGlobalDomain(d string) bool {
	d = strings.ToLower(strings.TrimSpace(d))
	return d == "workbuddy.ai" || strings.HasSuffix(d, ".workbuddy.ai")
}

// NeedsRefresh отчет token Будет ли в within истекает внутри (или уже истек/отсутствует expiry）。
func (a *Auth) NeedsRefresh(within time.Duration) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.ExpiresAt <= 0 {
		return true
	}
	return time.Now().Add(within).Unix() >= a.ExpiresAt
}

// Parse Совместимость с двумя типами дисков:
//
//	Вложенная форма {"auth":{...},«account":{...}} （Плагин OAuth вывод)
//	Плоская форма {"accessToken":...,«uid":...} （Ручной ввод/старая версия)
func Parse(raw []byte) (*Auth, error) {
	if len(raw) == 0 {
		return nil, fmt.Errorf("empty auth storage")
	}
	var probe map[string]json.RawMessage
	if err := json.Unmarshal(raw, &probe); err != nil {
		return nil, fmt.Errorf("storage_parse_error: %w", err)
	}
	var a Auth
	if _, nested := probe["auth"]; nested {
		var n struct {
			Auth struct {
				AccessToken string `json:"accessToken"`
				RefreshToken string `json:"refreshToken"`
				ExpiresAt int64 `json:"expiresAt"`
				Domain string `json:"domain"`
				Realm string `json:"realm"`
			} `json:"auth"`
			Account struct {
				UID string `json:"uid"`
				EnterpriseID string `json:"enterpriseId"`
				Nickname string `json:"nickname"`
			} `json:"account"`
			// DeviceToken Верхний уровень device_token（Совместимость вложенной и плоской форм; при ручной записи вложенность не требуется auth объект).
			DeviceToken string `json:"device_token"`
		}
		if err := json.Unmarshal(raw, &n); err != nil {
			return nil, fmt.Errorf("storage_parse_error: %w", err)
		}
		a = Auth{
			AccessToken: n.Auth.AccessToken,
			RefreshToken: n.Auth.RefreshToken,
			ExpiresAt: n.Auth.ExpiresAt,
			Domain: n.Auth.Domain,
			realm: n.Auth.Realm,
			UID: n.Account.UID,
			EnterpriseID: n.Account.EnterpriseID,
			Nickname: n.Account.Nickname,
			DeviceToken: n.DeviceToken,
		}
	} else {
		var f struct {
			AccessToken string `json:"accessToken"`
			RefreshToken string `json:"refreshToken"`
			ExpiresAt int64 `json:"expiresAt"`
			Domain string `json:"domain"`
			Realm string `json:"realm"`
			UID string `json:"uid"`
			EnterpriseID string `json:"enterpriseId"`
			Nickname string `json:"nickname"`
			DeviceToken string `json:"device_token"`
		}
		if err := json.Unmarshal(raw, &f); err != nil {
			return nil, fmt.Errorf("storage_parse_error: %w", err)
		}
		a = Auth{
			AccessToken: f.AccessToken,
			RefreshToken: f.RefreshToken,
			ExpiresAt: f.ExpiresAt,
			Domain: f.Domain,
			realm: f.Realm,
			UID: f.UID,
			EnterpriseID: f.EnterpriseID,
			Nickname: f.Nickname,
			DeviceToken: f.DeviceToken,
		}
	}
	if strings.TrimSpace(a.AccessToken) == "" {
		return nil, fmt.Errorf("parse_error: missing accessToken")
	}
	return &a, nil
}

// SaveAtomic Атомарная запись в виде вложенной структуры FilePath（tmp + rename），Сохранять вложенную (читаемую плагином) форму.
// удерживать на всём протяжении a.mu：Предотвратить конфликт с RefreshToken изменить token Конкурентность по полям, исключить частичную запись.
// Защита:accessToken При пустом значении запись отклоняется, чтобы пустые credentials не перезаписали валидный файл.
func (a *Auth) SaveAtomic() error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if strings.TrimSpace(a.AccessToken) == "" {
		return fmt.Errorf("save refused: empty accessToken (uid=%s)", a.UID)
	}
	if a.FilePath == "" {
		return fmt.Errorf("no FilePath set")
	}
	doc := map[string]any{
		"auth": map[string]any{
			"accessToken": a.AccessToken,
			"refreshToken": a.RefreshToken,
			"expiresAt": a.ExpiresAt,
			"domain": a.Domain,
			"realm": a.realm,
		},
		"account": map[string]any{
			"uid": a.UID,
			"enterpriseId": a.EnterpriseID,
			"nickname": a.Nickname,
		},
	}
	// DeviceToken Запись на верхний уровень только если не пусто device_token：Избежать появления пустого ключа в старых файлах без этого поля
	// （сохранение совместимости с плагином OAuth форма вывода совпадает, плагин игнорирует неизвестные ключи).
	if a.DeviceToken != "" {
		doc["device_token"] = a.DeviceToken
	}
	raw, err := json.MarshalIndent(doc, "", " ")
	if err != nil {
		return err
	}
	tmp := a.FilePath + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		// Docker bind-mount типичный кейс проблем с правами: внутри контейнера app Пользователь (uid 10001）
		// Нет прав записи в смонтированный каталог хоста. Выдать actionable инструкцию, а не голую syscall ошибка.
		msg := fmt.Sprintf("запись %s ошибка: %v", tmp, err)
		if errors.Is(err, fs.ErrPermission) {
			msg += "\n（Docker Деплой: у пользователя в контейнере нет прав записи в смонтированную директорию хоста. Варианты решения:" +
				"1) На локальной машине uid запустить контейнер:PUID=$(id -u) PGID=$(id -g) docker compose up -d；" +
				"2) sudo chown -R 10001:10001 ./auths ./data ./config.json；" +
				"3) compose Настройка user: «0:0» по root выполнение)"
		}
		return errors.New(msg)
	}
	return os.Rename(tmp, a.FilePath)
}

// LoadDir сканировать и парсить dir вниз workbuddy*.json；Файлы с ошибкой парсинга тихо пропускаются (лог запуска ведет вызывающая сторона).
// Попутно выполнить realm маркер миграции legacy-данных: для пустого realm auth Авто backfill（Исходный domain вывод) и SaveAtomic
// Сброс на диск, одноразово дополнить старый файл realm ключ. Ошибка записи одного файла не блокирует запуск (log WARN Продолжить),
// Избежать истории auth если отдельные файлы в каталоге недоступны для записи, весь сервис не запустится.
func LoadDir(dir string) ([]*Auth, error) {
	files, err := filepath.Glob(filepath.Join(dir, "workbuddy*.json"))
	if err != nil {
		return nil, err
	}
	// seenUID повтор UID Проверка: то же UID При появлении в нескольких файлах (двойной realm Одноименный UID вероятность близка к нулю)
	// ввод WARN Алерт содержит два пути к файлам, по правилу "побеждает загруженный последним» сохраняется текущее поведение (результат загрузки не меняется).
	seenUID := make(map[string]string, len(files))
	var out []*Auth
	for _, f := range files {
		raw, err := os.ReadFile(f)
		if err != nil {
			continue
		}
		a, err := Parse(raw)
		if err != nil {
			continue
		}
		a.FilePath = f
		if prev, ok := seenUID[a.UID]; ok {
			log.Printf("WARN: uid %s duplicated across %s and %s — Последнее перекрывает (разные realm Одноименный UID？）",
				logfmt.Label(a.UID, a.Nickname), prev, f)
		}
		seenUID[a.UID] = f
		if a.RealmStored() == "" {
			if changed, r := a.BackfillRealm(); changed {
				if err := a.SaveAtomic(); err != nil {
					log.Printf("WARN: auth %s realm backfill save: %v", logfmt.Label(a.UID, a.Nickname), err)
				} else if r == "global" {
					log.Printf("auth %s миграция существующих данных: Дополнить realm=global（domain=%s）", logfmt.Label(a.UID, a.Nickname), a.Domain)
				}
			}
		}
		out = append(out, a)
	}
	return out, nil
}
