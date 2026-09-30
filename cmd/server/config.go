// config.go Загрузка JSON Конфигурация + Переопределение переменными окружения.
package main

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/prompt"
)

// Config конфигурация верхнего уровня.
type Config struct {
	Listen string `json:"listen"` // ":7863"
	APIKey string `json:"api_key"` // пустой = Без аутентификации
	AuthDir string `json:"auth_dir"` // ./auths
	StateFile string `json:"state_file"` // ./data/state.json

	Panel struct {
		// PackageDetailLimit На странице состава баллов на аккаунт по умолчанию показывается кол-во ближайших к истечению пакетов;<=0 откат 5。
		PackageDetailLimit int `json:"package_detail_limit"`
	} `json:"panel"`

	Logging struct {
		// RequestArchiveEnabled Метаданные запроса JSONL Переключатель архивации, по умолчанию true。
		RequestArchiveEnabled bool `json:"request_archive_enabled"`
		// RequestRetentionDays Дней хранения архива, по умолчанию 7；<=0 Фолбэк на дефолт.
		RequestRetentionDays int `json:"request_retention_days"`
		// RequestArchiveMaxMB общий лимит архивации (MiB），по умолчанию 100；<=0 Фолбэк на дефолт.
		RequestArchiveMaxMB int `json:"request_archive_max_mb"`
		// RequestClientInfo есть ли в логе запросов (архивное событие + stdout Строка лога + Работа панели
		// в логах) фиксировать источник вызова: клиент IP и User-Agent。по умолчанию true。
		//
		// почему сделано переключателем, а не всегда вкл: инфо об источнике для отладки"кто бьет в шлюз"первичная улика,
		// Но оно чем token чувствительно к подсчету (IP относится к PII), shared deployment/В мультитенантном сценарии может потребоваться
		// выключить. После выключения Event.ClientIP/UserAgent оставить пустым, в архиве поле не появляется.
		// горячее применение (через livecfg снимок), перезапуск не требуется.
		RequestClientInfo bool `json:"request_client_info"`
	} `json:"logging"`

	Cooldown struct {
		// hard_credit / err_threshold / err_cooldown Три исторических ключа выведены из употребления:
		// Жёсткий кулдаун фиксирован до след. дня 04:00（CooldownUntilTomorrow4AM），семантика последовательных ошибок включена в circuit breaker.
		// старый config эти ключи в JSON Неизвестные поля игнорируются без ошибки.
		SoftRate string `json:"soft_rate"` // »600s"，База кулдауна мягкого лимита
		// SoftRateMax Потолок экспоненциального бэкоффа мягкого кулдауна, по умолчанию "2h"。
		// null → фолбэк на дефолт, невалид → ошибка (стиль обработки как у soft_rate）。
		SoftRateMax string `json:"soft_rate_max"` // "2h"
	} `json:"cooldown"`

	Schedule struct {
		CheckinHours []int `json:"checkin_hours"` // [9,21]
		TravelHours []int `json:"travel_hours"` // [9,21]
		ActivityHours []int `json:"activity_hours"` // [10]
		KeepaliveHours []int `json:"keepalive_hours"` // [22]
		BlackcatHours []int `json:"blackcat_hours"` // [23] окно »совы" (23:00–08:00 счётчик)
		GrowthHours []int `json:"growth_hours"` // [1] Очередь задач роста (Sequential семейство разблокируется ежедневно в 00:00,01:00 автосканирование и выполнение)
		// CheckinEnabled/TravelEnabled/ActivityEnabled/KeepaliveEnabled/BlackcatEnabled явный выключатель отключения (по умолчанию true）。
		//
		// почему отдельный bool а не пустой массив/сентинел-значение"Отключено"：
		// - Пустой массив и null в старой семантике уже"Не настроено → откат к дефолту"занято, смена вердикта тихо инвертирует
		// все старые config поведение (пользователь хотел удалить одну строку, а отключил check-in);bool по умолчанию true
		// то на старую конфигурацию не влияет, полная обратная совместимость.
		// - Развязка переключателя и значения: при отключении сохраняется явно заданный пользователем час, повторное включение без доп. настройки.
		// - не нужно угадывать sentinel ([-1] и т.п.), невалидный час — ошибка с подсказкой использовать этот переключатель.
		// старый config этот ключ в нем из-за JSON Неизвестные поля игнорируются без ошибки.
		CheckinEnabled bool `json:"checkin_enabled"` // по умолчанию true；false = отключить регистрацию
		TravelEnabled bool `json:"travel_enabled"` // по умолчанию true；false = Полностью остановить Cat Travel
		ActivityEnabled bool `json:"activity_enabled"` // по умолчанию true；false = Остановить отчёт об активности
		KeepaliveEnabled bool `json:"keepalive_enabled"` // по умолчанию true；false = закрыть token Keepalive
		BlackcatEnabled bool `json:"blackcat_enabled"` // по умолчанию true；false = выкл. ночной режим
		GrowthEnabled bool `json:"growth_enabled"` // по умолчанию true；false = авто-планирование задач роста, связанных с

		// фоновое периодическое обновление баланса: между двумя точками check-in credits также остаётся свежим (панель/для наблюдения состояния).
		// Семантика разморозки как у check-in (баланс > 0 охлажденные аккаунты автоматически размораживаются), но без чекина не обновлять token。
		BalanceRefreshEnabled bool `json:"balance_refresh_enabled"` // по умолчанию true；false = Закрыть
		BalanceRefreshMinutes int `json:"balance_refresh_minutes"` // по умолчанию 5；<=0 откат 5
	} `json:"schedule"`

	Global struct {
		// Enabled global realm Переключатель маршрутизации. По умолчанию true：Realm() Обычно realm=global/
		// domain=workbuddy.ai аккаунт классифицируется как global и маршрутизировать global base/Путь.
		// Явно "enabled": false Отключено (аварийный выход, чисто CN блокировка: даже если auth записано realm=global
		// Также не маршрутизировать,auth.Realm() первый шлюз двойной страховки). Чисто CN Поведение деплоя без изменений:CN Аккаунт
		// постоянная проверка cn，global base Только в realm=global использовано на аккаунте.
		Enabled bool `json:"enabled"`
		// ChatBase / BillingBase Международный апстрим base Перекрытие; пусто = откат на встроенный дефолт
		// https://www.workbuddy.ai（internal/upstream.defaultGlobalBase）。
		ChatBase string `json:"chat_base"`
		BillingBase string `json:"billing_base"`
	} `json:"global"`

	Upstream struct {
		// TimeoutSeconds Короткий RPC（refresh/checkin/balance/FetchModels）Верхний лимит общего времени, по умолчанию 120。
		TimeoutSeconds int `json:"timeout_seconds"`
		// HeaderTimeoutSeconds Чат SSE Лимит до первого байта (заголовки ответа);<=0 откат TimeoutSeconds。
		HeaderTimeoutSeconds int `json:"header_timeout_seconds"`
		// IdleTimeoutSeconds Чат SSE лимит простоя потока (активная отдача данных продлевает жизнь, не рвать);<=0 откат к дефолту 300。
		IdleTimeoutSeconds int `json:"idle_timeout_seconds"`
		// UserAgent исходящий User-Agent Явное перекрытие (при непустом значении действует полный путь, приоритет над трёхсегментным дефолтом).
		// Применяется ко всем исходящим запросам:chat/refresh/checkin/balance/report/travel/FetchModels。
		// Значение по умолчанию выровнено с официальным WorkBuddy десктопный вид (трехсекционный), пользователь может переопределить полностью кастомным значением.
		UserAgent string `json:"user_agent"`
		// ClientVersion WorkBuddy Сегмент версии клиента (исходящий UA `WorkBuddy/<ver>` и заголовок принадлежности
		// X-IDE-Version）。пустой = Встроено по умолчанию (выравнивание с официальным 5.5.4 дистрибутив).
		ClientVersion string `json:"client_version"`
		// CliVersion исходящий UA Средний `CLI/<ver>` версия сегмента. Пусто = встроенный дефолт (официальный встроенный CLI 2.137.1）。
		CliVersion string `json:"cli_version"`
		// ClientName значение заголовка принадлежности расхода (X-Product / X-IDE-Name / X-IDE-Type / X-IDE-Version）。
		// пустой = Старое поведение X-Product="SaaS« Не задано X-IDE-*；Сопоставить "WorkBuddy" то следуют все четыре заголовка.
		ClientName string `json:"client_name"`
		// DeviceToken Риск-контроль устройства Token（X-Device-Token заголовок) глобальный fallback; пусто = Не инжектировать.
		// На каждый номер auth файла device_token ключ приоритетнее данного поля.
		DeviceToken string `json:"device_token"`
		// DeviceTokenFile device token Фолбэк пути к файлу (десктоп хоста с сохранением на диск token，5 минут читать кэш).
		DeviceTokenFile string `json:"device_token_file"`
		// PassthroughIP прозрачно прокидывать клиента IP в апстрим (по умолчанию false，граница безопасности обратного прокси).
		PassthroughIP bool `json:"passthrough_ip"`
	} `json:"upstream"`

	Features struct {
		// SanitizeBlacklistFingerprints Десенситизация отпечатка блеклиста тела исходящего запроса (по умолчанию true；false полное восстановление).
		SanitizeBlacklistFingerprints bool `json:"sanitize_blacklist_fingerprints"`
	} `json:"features"`

	Prompt struct {
		// Mode passthrough（по умолчанию)= прозрачная передача оригинала клиента system（фолбэк-ретрай всё равно переключится на Degraded）；
		// custom = Шлюз заменяет клиентский системный промпт своим system/developer；
		// append = Совместное использование: непрерывность в начале system/developer после блока вставить шлюз system，Существующие сообщения дословно без изменений (issue #129）。
		Mode string `json:"mode"` // "passthrough" / "custom" / "append"
		// File Путь к файлу промпта; пусто = встроенный по умолчанию defaultprompt.md；
		// Путь непустой, но нечитаем → Ошибка запуска (fail fast，избежать тихого отката к встроенному дефолту).
		File string `json:"file"`
	} `json:"prompt"`

	// PromptText Текст системного промпта после парсинга (custom/append использование режима).
	PromptText string `json:"-"`

	Upstash struct {
		URL string `json:"url"` // пустой = Чисто in-memory режим; поддержка полная rediss:// URL Или https://xxx.upstash.io host
		Token string `json:"token"` // url используется для сборки при неполной строке подключения rediss://default:<token>@<host>:6379
	} `json:"upstash"`

	Pool struct {
		MaxInFlight int `json:"max_in_flight"` // макс. кол-во запросов в полёте на аккаунт,0 = без ограничений
		MaxInFlightGlobal int `json:"max_in_flight_global"` // global Лимит in-flight на аккаунт в домене (WAF в строгой зоне риск-контроля concurrency снижается),0 = откат к дефолту 2
		BreakerThreshold int `json:"breaker_threshold"` // Превышение числа последовательных ошибок вызывает circuit breaker, по умолчанию 3
		BreakerCooldown string `json:"breaker_cooldown"` // Базовая длительность circuit breaker, по умолчанию »30m"
		BreakerCooldownMax string `json:"breaker_cooldown_max"` // Потолок экспоненциального бэкоффа, по умолчанию »6h"
		// понижение веса при серии поражений (issue #114）：ErrClient/Последовательный счётчик сбоев транспортного уровня типа "без штрафа аккаунту», при достижении порога
		// Временное исключение из пула. С охлаждением/circuit breaker сосуществуют, берётся больший без суммирования. По умолчанию 5 Раз / 10m。
		DegradeThreshold int `json:"degrade_threshold"` // число подряд идущих неудач триггерит понижение приоритета, по умолчанию 5
		DegradeCooldown string `json:"degrade_cooldown"` // Длительность понижения приоритета (фикс., без эксп. бэкоффа), по умолч. "10m"
		DegradeCooldownMax string `json:"degrade_cooldown_max"` // Ограничение сверху длительности понижения приоритета, по умолчанию "2h"（только если cooldown кламп только при превышении этого значения)
		IdleWeightPerHour float64 `json:"idle_weight_per_hour"` // компенсация простоя: неиспользовано за час +0.5 Вес
		IdleWeightMax float64 `json:"idle_weight_max"` // потолок компенсации простоя, по умолчанию 5.0
		// PreferExpiring Переключатель маршрутизации "сначала истекающие», по умолчанию true。Включено и expiring_soon Внутри окна
		// при наличии валидных батчей — сортировка по ближайшему сроку истечения; при выключении данные о сроке для выбора номера не используются.
		PreferExpiring bool `json:"prefer_expiring"`
		// ExpiringSoon Окно скоро истекающих баллов (напр., "168h"=7дн.): чекин/при обновлении баланса время истечения в
		// Баллы в этом окне попадают в приоритетный набор, далее сортировка по ближайшему сроку истечения. Пусто/0 = Отключить порог этого маршрута.
		ExpiringSoon string `json:"expiring_soon"`
		// CostExploreInterval costTier Окно исследования условий (issue #136 схема a′）：tier 0
		// Монопольный слой существует и tier 1 При наличии участников, время с последней разведки ≥ окно, то в этот раз pick Переключение слоя применения
		// tier 1-only（Исследование=Попутное перенаправление, ноль новых upstream-запросов; успех — выпуск, неудача — по существующему
		// стратегия ошибок). По умолчанию "30m«（≤48 Раз/день/модель);«0" Отключение (полный возврат к текущему поведению);
		// Пустое значение откатывается к дефолту.
		CostExploreInterval string `json:"cost_explore_interval"`
		// CreditFloor Гарантированный минимум баллов: если баланс аккаунта ниже этого значения, для моделей с фактической оплатой (tier 2）Больше не
		// Участвует в выборе номера — защита от пробития баланса платными запросами, чтобы не задеть даже бесплатные модели 402 Кулдаун до чекина следующего дня.
		// tier 0（бесплатно)/ tier 1（без наблюдений) не ограничивается; восстановление чекином превышает floor Автовосстановление.
		// По умолчанию 0 = Отключено; отрицательные значения клампятся 0。
		CreditFloor int64 `json:"credit_floor"`
	} `json:"pool"`

	SessionSticky struct {
		Enabled bool `json:"enabled"` // По умолчанию true
		TTL string `json:"ttl"` // привязка сессии TTL，По умолчанию »30m"
		GCInterval string `json:"gc_interval"` // сессия GC период, по умолчанию »5m"
	} `json:"session_sticky"`

	// После парсинга
	SoftRateDur time.Duration `json:"-"`
	SoftRateMaxDur time.Duration `json:"-"`
	BreakerCooldownDur time.Duration `json:"-"`
	BreakerCooldownMaxD time.Duration `json:"-"`
	DegradeCooldownDur time.Duration `json:"-"`
	DegradeCooldownMaxD time.Duration `json:"-"`
	SessionTTL time.Duration `json:"-"`
	SessionGCInterval time.Duration `json:"-"`
	BalanceRefreshInterval time.Duration `json:"-"` // 0 = Не запускать (enabled=false）
	ExpiringSoonDur time.Duration `json:"-"`
	// CostExploreIntervalDur После парсинга costTier Окно исследования (issue #136）；0 = Отключение.
	CostExploreIntervalDur time.Duration `json:"-"`
}

// Default конфигурация по умолчанию.
func Default() *Config {
	c := &Config{
		Listen: ":7863",
		APIKey: "",
		AuthDir: "./auths",
		StateFile: "./data/state.json",
	}
	c.Cooldown.SoftRate = "600s"
	c.Cooldown.SoftRateMax = "2h"
	c.Panel.PackageDetailLimit = 5
	c.Logging.RequestArchiveEnabled = true
	c.Logging.RequestRetentionDays = 7
	c.Logging.RequestArchiveMaxMB = 100
	// по умолчанию true реализуется явным присваиванием (тот же Schedule переключатель):JSON при отсутствии ключа в поле сохраняется это значение,
	// Только явно false только тогда закрывать запись источника.
	c.Logging.RequestClientInfo = true
	c.Schedule.CheckinHours = []int{9, 21}
	c.Schedule.TravelHours = []int{9, 21}
	c.Schedule.ActivityHours = []int{10}
	c.Schedule.KeepaliveHours = []int{22}
	c.Schedule.BlackcatHours = []int{23}
	c.Schedule.GrowthHours = []int{1}
	// Переключатель "по умолчанию true」Реализуется этими строками:Load Сначала взять Default() Повторно json.Unmarshal перекрытие,
	// Ключ отсутствует (или равен null）поле сохраняется как есть при true，Только явно false Только тогда закрывается.
	c.Schedule.CheckinEnabled = true
	c.Schedule.GrowthEnabled = true
	c.Schedule.TravelEnabled = true
	c.Schedule.ActivityEnabled = true
	c.Schedule.KeepaliveEnabled = true
	c.Schedule.BlackcatEnabled = true
	c.Schedule.BalanceRefreshEnabled = true
	c.Schedule.BalanceRefreshMinutes = 5
	c.Upstream.TimeoutSeconds = 120
	// HeaderTimeoutSeconds/IdleTimeoutSeconds По умолчанию 0（состояние "не задано»), fallback см. normalize()。
	c.Upstream.HeaderTimeoutSeconds = 0
	c.Upstream.IdleTimeoutSeconds = 0
	// Global.Enabled по умолчанию true（Чистый CN Поведение без изменений:CN аккаунт всегда считается cn，global base не используется);
	// ChatBase/BillingBase по умолчанию пусто (фолбэк на встроенное значение).
	c.Global.Enabled = true
	c.Features.SanitizeBlacklistFingerprints = true
	c.Prompt.Mode = "passthrough" // по умолчанию passthrough：прозрачная передача оригинала клиента system（Синхронизировать с апстримом;custom выбирается пользователем явно)
	c.Pool.MaxInFlight = 3
	// MaxInFlightGlobal по умолчанию 2：global Домен WAF более строгий риск-контроль, снижает конкурентность на один номер (WAF 403 исправление
	// P1-1）；0/Отрицательное число normalize откат к дефолту (с max_in_flight 0=семантика безлимита иная, ключ тарификации
	// 0 нет осмысленной семантики, откат к градации по умолчанию — самый стабильный).
	c.Pool.MaxInFlightGlobal = 2
	c.Pool.BreakerThreshold = 3
	c.Pool.BreakerCooldown = "30m"
	c.Pool.BreakerCooldownMax = "6h"
	c.Pool.DegradeThreshold = 5
	c.Pool.DegradeCooldown = "10m"
	c.Pool.DegradeCooldownMax = "2h"
	c.Pool.IdleWeightPerHour = 0.5
	c.Pool.IdleWeightMax = 5.0
	c.Pool.PreferExpiring = true
	c.Pool.ExpiringSoon = "168h" // Окно скорой экспирации по умолчанию 7 дн.: бонусные баллы официальных акций обычно истекают в течение двух недель
	// costTier Значение по умолчанию для explore 30m（issue #136：устранение монополии + попутное перенаправление без новых запросов);«0« Отключение.
	c.Pool.CostExploreInterval = "30m"
	c.SessionSticky.Enabled = true
	c.SessionSticky.TTL = "30m"
	c.SessionSticky.GCInterval = "5m"
	return c
}

// Load Читать из файла, затем использовать WB2A_* env перезапись.
func Load(path string) (*Config, error) {
	c := Default()
	if path != "" {
		// Проверка каталога:Docker bind mount при отсутствии файла на хосте тихо создаётся одноимённый каталог,
		// прямо ReadFile будет ошибка "Incorrect function« и подобные неясные ошибки — здесь actionable подсказки.
		if st, statErr := os.Stat(path); statErr == nil && st.IsDir() {
			return nil, fmt.Errorf("config %s — это директория, а не файл —"+
				"Docker если при деплое на хосте отсутствует config.json，bind mount Будет создан каталог с тем же именем."+
				"сначала `cp config.example.json config.json` или удалите каталог (программа автоматически сгенерирует конфигурацию)", path)
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("read config: %w", err)
		}
		if _, err := ParseConfigInto(raw, c); err != nil {
			return nil, err
		}
	}
	applyEnv(c)
	if err := c.normalize(); err != nil {
		return nil, err
	}
	return c, nil
}

// ParseConfigInto взять JSON Перекрывает до c на и normalize（Не делать env、файл не читается).
// Сохранение конфигурации панели идёт по этому пути: с Load полностью тот же парсинг/Логика валидации, избегать расхождения в двух местах.
func ParseConfigInto(raw []byte, c *Config) (*Config, error) {
	if err := json.Unmarshal(raw, c); err != nil {
		return nil, fmt.Errorf("parse config: %w", err)
	}
	if err := c.normalize(); err != nil {
		return nil, err
	}
	return c, nil
}

// ParseConfig разбор секции конфига на основе значений по умолчанию JSON（Эквивалентно Load ветка файла, но без чтения переменных окружения).
func ParseConfig(raw []byte) (*Config, error) {
	return ParseConfigInto(raw, Default())
}

// WriteDefault В path Сгенерировать рекомендуемую конфигурацию (автосоздание при первом запуске, открывается двойным кликом без ручного копирования примера).
// значение берется из Default()（включая таймаут/Circuit Breaker/расписание чекинов и др. рекомендуемые значения),api_key использовать crypto/rand Случайная генерация:
// Безопасный дефолт приоритетнее примера-плейсхолдера (listen Привязка 0.0.0.0，пустой key приведёт к прямому экспонированию шлюза в LAN).
// Вернуть сгенерированный key для вывода в стартовый лог. При наличии через O_EXCL Атомарный отказ, конфигурация пользователя не перезаписывается.
func WriteDefault(path string) (string, error) {
	raw := make([]byte, 18)
	if _, err := rand.Read(raw); err != nil {
		return "", fmt.Errorf("gen api_key: %w", err)
	}
	key := "sk-" + base64.RawURLEncoding.EncodeToString(raw)
	c := Default()
	c.APIKey = key
	_ = c.normalize() // Default() полностью валидно,normalize Только дополнение header/idle Отображаемое значение тайм-аута
	out, err := json.MarshalIndent(c, "", " ")
	if err != nil {
		return "", fmt.Errorf("marshal config: %w", err)
	}
	if dir := filepath.Dir(path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return "", fmt.Errorf("mkdir config dir: %w", err)
		}
	}
	// O_EXCL атомарный запрет перезаписи: даже если вызывающая сторона пропустила проверку"не существует"，и никогда скрытно не перезаписывать существующую конфигурацию пользователя.
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return "", fmt.Errorf("write config: %w", err)
	}
	defer f.Close()
	if _, err := f.Write(out); err != nil {
		return "", fmt.Errorf("write config: %w", err)
	}
	return key, nil
}

func applyEnv(c *Config) {
	if v := os.Getenv("WB2A_LISTEN"); v != "" {
		c.Listen = v
	}
	if v := os.Getenv("WB2A_API_KEY"); v != "" {
		c.APIKey = v
	}
	if v := os.Getenv("WB2A_AUTH_DIR"); v != "" {
		c.AuthDir = v
	}
	if v := os.Getenv("WB2A_STATE_FILE"); v != "" {
		c.StateFile = v
	}
	if v := os.Getenv("WB2A_SOFT_RATE"); v != "" {
		c.Cooldown.SoftRate = v
	}
	if v := os.Getenv("WB2A_SOFT_RATE_MAX"); v != "" {
		c.Cooldown.SoftRateMax = v
	}
	if v := os.Getenv("WB2A_TIMEOUT_SECONDS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			c.Upstream.TimeoutSeconds = n
		}
	}
	if v := os.Getenv("WB2A_HEADER_TIMEOUT_SECONDS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			c.Upstream.HeaderTimeoutSeconds = n
		}
	}
	if v := os.Getenv("WB2A_IDLE_TIMEOUT_SECONDS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			c.Upstream.IdleTimeoutSeconds = n
		}
	}
	if v := os.Getenv("WB2A_USER_AGENT"); v != "" {
		c.Upstream.UserAgent = v
	}
	if v := os.Getenv("WB2A_CLIENT_VERSION"); v != "" {
		c.Upstream.ClientVersion = v
	}
	if v := os.Getenv("WB2A_CLI_VERSION"); v != "" {
		c.Upstream.CliVersion = v
	}
	if v := os.Getenv("WB2A_CLIENT_NAME"); v != "" {
		c.Upstream.ClientName = v
	}
	if v := os.Getenv("WB2A_DEVICE_TOKEN"); v != "" {
		c.Upstream.DeviceToken = v
	}
	if v := os.Getenv("WB2A_DEVICE_TOKEN_FILE"); v != "" {
		c.Upstream.DeviceTokenFile = v
	}
	if v := os.Getenv("WB2A_PASSTHROUGH_IP"); v != "" {
		if b, err := strconv.ParseBool(v); err == nil {
			c.Upstream.PassthroughIP = b
		}
	}
	if v := os.Getenv("WB2A_SANITIZE_FINGERPRINTS"); v != "" {
		if b, err := strconv.ParseBool(v); err == nil {
			c.Features.SanitizeBlacklistFingerprints = b
		}
	}
	if v := os.Getenv("WB2A_PROMPT_MODE"); v != "" {
		c.Prompt.Mode = v
	}
	if v := os.Getenv("WB2A_PROMPT_FILE"); v != "" {
		c.Prompt.File = v
	}
	if v := os.Getenv("WB2A_EXPIRING_SOON"); v != "" {
		c.Pool.ExpiringSoon = v
	}
	if v := os.Getenv("WB2A_PREFER_EXPIRING"); v != "" {
		if b, err := strconv.ParseBool(v); err == nil {
			c.Pool.PreferExpiring = b
		}
	}
}

func (c *Config) normalize() error {
	var err error
	if c.Panel.PackageDetailLimit <= 0 {
		c.Panel.PackageDetailLimit = 5
	}
	if c.Logging.RequestRetentionDays <= 0 {
		c.Logging.RequestRetentionDays = 7
	}
	if c.Logging.RequestArchiveMaxMB <= 0 {
		c.Logging.RequestArchiveMaxMB = 100
	}
	if c.SoftRateDur, err = time.ParseDuration(c.Cooldown.SoftRate); err != nil {
		return fmt.Errorf("cooldown.soft_rate: %w", err)
	}
	// При пустом значении — fallback по умолчанию 2h（Default() значение уже установлено; этот fallback перекрывает явный "" и Default() сценарий обхода).
	if c.Cooldown.SoftRateMax == "" {
		c.Cooldown.SoftRateMax = "2h"
	}
	if c.SoftRateMaxDur, err = time.ParseDuration(c.Cooldown.SoftRateMax); err != nil {
		return fmt.Errorf("cooldown.soft_rate_max: %w", err)
	}
	if c.BreakerCooldownDur, err = time.ParseDuration(c.Pool.BreakerCooldown); err != nil {
		return fmt.Errorf("pool.breaker_cooldown: %w", err)
	}
	if c.BreakerCooldownMaxD, err = time.ParseDuration(c.Pool.BreakerCooldownMax); err != nil {
		return fmt.Errorf("pool.breaker_cooldown_max: %w", err)
	}
	if c.DegradeCooldownDur, err = time.ParseDuration(c.Pool.DegradeCooldown); err != nil {
		return fmt.Errorf("pool.degrade_cooldown: %w", err)
	}
	if c.DegradeCooldownMaxD, err = time.ParseDuration(c.Pool.DegradeCooldownMax); err != nil {
		return fmt.Errorf("pool.degrade_cooldown_max: %w", err)
	}
	if c.SessionTTL, err = time.ParseDuration(c.SessionSticky.TTL); err != nil {
		return fmt.Errorf("session_sticky.ttl: %w", err)
	}
	if c.SessionGCInterval, err = time.ParseDuration(c.SessionSticky.GCInterval); err != nil {
		return fmt.Errorf("session_sticky.gc_interval: %w", err)
	}
	// окно скорого истечения: пусто = отключено (ExpiringSoonDur 0）；Непустое должно быть парсируемым (опечатка fail fast）。
	if c.Pool.ExpiringSoon != "" {
		if c.ExpiringSoonDur, err = time.ParseDuration(c.Pool.ExpiringSoon); err != nil {
			return fmt.Errorf("pool.expiring_soon: %w", err)
		}
	}
	if c.ExpiringSoonDur < 0 {
		c.ExpiringSoonDur = 0
		c.Pool.ExpiringSoon = "0"
	}
	// costTier Окно исследования (issue #136）：При пустом значении — fallback по умолчанию 30m（Default уже установлено; этот fallback покрывает
	// Явно ""）；«0" — валидное значение (отключение, полный возврат к текущему поведению), без отката; отрицательные clamp 0 аналогично отключению
	//（"−5m" без осмысленной семантики).
	if c.Pool.CostExploreInterval == "" {
		c.Pool.CostExploreInterval = "30m"
	}
	if c.CostExploreIntervalDur, err = time.ParseDuration(c.Pool.CostExploreInterval); err != nil {
		return fmt.Errorf("pool.cost_explore_interval: %w", err)
	}
	if c.CostExploreIntervalDur < 0 {
		c.CostExploreIntervalDur = 0
	}
	// Гарантия баллов: отрицательные clamp 0（= закрытие).0 — валидный дефолт (выкл.), фолбэк на null не требуется.
	if c.Pool.CreditFloor < 0 {
		c.Pool.CreditFloor = 0
	}
	if c.Pool.BreakerThreshold <= 0 {
		c.Pool.BreakerThreshold = 3
	}
	// параметры понижения веса при серии поражений по умолчанию нормализуются к единице (невалидный/не задано — откат к значению по умолчанию, и breaker_threshold В том же стиле).
	if c.Pool.DegradeThreshold <= 0 {
		c.Pool.DegradeThreshold = 5
	}
	if c.Pool.DegradeCooldown == "" {
		c.Pool.DegradeCooldown = "10m"
	}
	if c.Pool.DegradeCooldownMax == "" {
		c.Pool.DegradeCooldownMax = "2h"
	}
	// global in-flight по уровням:0/Отрицательное считается как не задано, фолбэк к дефолту 2（WAF 403 исправление P1-1）。
	if c.Pool.MaxInFlightGlobal <= 0 {
		c.Pool.MaxInFlightGlobal = 2
	}
	if c.Pool.IdleWeightPerHour <= 0 {
		c.Pool.IdleWeightPerHour = 0.5
	}
	if c.Pool.IdleWeightMax <= 0 {
		c.Pool.IdleWeightMax = 5.0
	}
	if c.Upstream.TimeoutSeconds <= 0 {
		c.Upstream.TimeoutSeconds = 120
	}
	// header дефолтный fallback timeout（сохранить"смена номера до первого байта"сохр. прежнюю семантику);idle По умолчанию большое встроенное значение.
	// ТЗ предусматривает:0 во всех случаях считается как"не задано«идёт по умолчанию, реальный«Отключено"Отложить на потом (во избежание неоднозначности).
	if c.Upstream.HeaderTimeoutSeconds <= 0 {
		c.Upstream.HeaderTimeoutSeconds = c.Upstream.TimeoutSeconds
	}
	if c.Upstream.IdleTimeoutSeconds <= 0 {
		c.Upstream.IdleTimeoutSeconds = 300
	}
	if !strings.HasPrefix(c.Listen, ":") && !strings.Contains(c.Listen, ":") {
		c.Listen = ":" + c.Listen
	}
	// Пустой массив и null перекрыть после десериализации Default() значение расписания (сохраняется только при отсутствии ключа), здесь дополнить.
	// пустой = Не настроено → Откат к дефолту; "Отключено» всегда через *_enabled=false，не путать между собой.
	if len(c.Schedule.CheckinHours) == 0 {
		c.Schedule.CheckinHours = []int{9, 21}
	}
	if len(c.Schedule.TravelHours) == 0 {
		c.Schedule.TravelHours = []int{9, 21}
	}
	if len(c.Schedule.ActivityHours) == 0 {
		c.Schedule.ActivityHours = []int{10}
	}
	if len(c.Schedule.KeepaliveHours) == 0 {
		c.Schedule.KeepaliveHours = []int{22}
	}
	if len(c.Schedule.BlackcatHours) == 0 {
		c.Schedule.BlackcatHours = []int{23}
	}
	if len(c.Schedule.GrowthHours) == 0 {
		c.Schedule.GrowthHours = []int{1}
	}
	// Фоновое обновление баланса: при включении minutes<=0 откат к дефолту 5；При закрытии interval Сохранить 0（не запускать).
	if c.Schedule.BalanceRefreshEnabled {
		if c.Schedule.BalanceRefreshMinutes <= 0 {
			c.Schedule.BalanceRefreshMinutes = 5
		}
		c.BalanceRefreshInterval = time.Duration(c.Schedule.BalanceRefreshMinutes) * time.Minute
	}
	if err := c.validateScheduleHours(); err != nil {
		return err
	}
	return c.normalizePrompt()
}

// normalizePrompt Валидация prompt.mode и по file Загрузка текста промпта (custom/append режим).
//
// mode Невалидный (не passthrough/custom/append）Ошибка запуска, избегать тихого отката к ветке;
// custom/append В режиме file непусто, но нечитаемо → ошибка (fail fast），file пустой → использовать встроенный дефолт
// （Два режима используют один путь загрузки,PromptText оба непустые).
// passthrough Режим не загружает текст (прозрачная передача оригинала клиента system，Текст для деградации prompt.Degraded）。
func (c *Config) normalizePrompt() error {
	switch m := strings.ToLower(strings.TrimSpace(c.Prompt.Mode)); m {
	case "", "passthrough":
		c.Prompt.Mode = "passthrough"
	case "custom":
		c.Prompt.Mode = "custom"
	case "append":
		c.Prompt.Mode = "append"
	default:
		return fmt.Errorf("prompt.mode: %q не является допустимым значением (passthrough / custom / append）", c.Prompt.Mode)
	}
	if c.Prompt.Mode == "custom" || c.Prompt.Mode == "append" {
		text, err := prompt.Load(c.Prompt.Mode, c.Prompt.File)
		if err != nil {
			return err
		}
		c.PromptText = text
	}
	return nil
}

// validateScheduleHours проверить, что час по расписанию попадает в 0-23。
//
// почему не использовать `[-1]` смысл sentinel-значений вроде«Отключено«：невалидный час тихо проглатывается, пользователь думает, что отключил чекин,
// фактически может быть воспринято как штатное выполнение в другой ровный час; здесь сразу fast-fail с указанием правильного переключателя в сообщении об ошибке
// （checkin_enabled / keepalive_enabled），исключить подбор пользователем значения сентинела наугад.
func (c *Config) validateScheduleHours() error {
	if err := checkHourRange("schedule.checkin_hours", "checkin_enabled", c.Schedule.CheckinHours); err != nil {
		return err
	}
	if err := checkHourRange("schedule.travel_hours", "travel_enabled", c.Schedule.TravelHours); err != nil {
		return err
	}
	if err := checkHourRange("schedule.activity_hours", "activity_enabled", c.Schedule.ActivityHours); err != nil {
		return err
	}
	if err := checkHourRange("schedule.keepalive_hours", "keepalive_enabled", c.Schedule.KeepaliveHours); err != nil {
		return err
	}
	if err := checkHourRange("schedule.blackcat_hours", "blackcat_enabled", c.Schedule.BlackcatHours); err != nil {
		return err
	}
	return checkHourRange("schedule.growth_hours", "growth_enabled", c.Schedule.GrowthHours)
}

func checkHourRange(field, switchKey string, hours []int) error {
	for _, h := range hours {
		if h < 0 || h > 23 {
			return fmt.Errorf("%s: %d не валидный час (0-23）；Чтобы отключить задачу, установите schedule.%s=false", field, h, switchKey)
		}
	}
	return nil
}
