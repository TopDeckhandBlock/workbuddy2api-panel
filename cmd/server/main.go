// main.go workbuddy2api Точка входа: загрузка конфигурации, сборка pool、запустить планировщик и HTTP сервис.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/livecfg"
	"github.com/linguo2625469/workbuddy2api-panel/internal/panel"
	"github.com/linguo2625469/workbuddy2api-panel/internal/pool"
	"github.com/linguo2625469/workbuddy2api-panel/internal/redisstore"
	"github.com/linguo2625469/workbuddy2api-panel/internal/reqlog"
	"github.com/linguo2625469/workbuddy2api-panel/internal/scheduler"
	"github.com/linguo2625469/workbuddy2api-panel/internal/server"
	"github.com/linguo2625469/workbuddy2api-panel/internal/session"
	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
	"github.com/linguo2625469/workbuddy2api-panel/internal/usage"
)

// appVersion Версия шлюза (fork версия: панель + система задач), прокидывается в /panel/api/overview。
const appVersion = "1.11.10-panel"

// usagePathFor От state Вывести путь файла использования из пути файла: тот же каталог, имя файла usage.json。
// Так config изменить в state_file данные использования следуют вместе, доп. настройки не нужны.
func usagePathFor(stateFile string) string { return stateSibling(stateFile, "usage.json") }

// stateSibling Возврат и state Путь к файлу с указанным именем в той же директории (для относительного пути — fallback в текущую директорию).
// usage.json（запись расхода) и output_probes.json（зондирование лимита модели) используют общее правило.
func stateSibling(stateFile, name string) string {
	dir := filepath.Dir(stateFile)
	if dir == "" || dir == "." {
		return name
	}
	return filepath.Join(dir, name)
}

func main() {
	cfgPath := flag.String("config", "config.json", "Путь к файлу конфигурации (по умолчанию текущая директория config.json;при отсутствии автоматически генерировать рекомендуемую конфигурацию)")
	flag.Parse()

	cfg, err := Load(*cfgPath)
	if err != nil {
		// errors.Is только тогда можно выявить Load внутри fmt.Errorf("%w") обёртка;os.IsNotExist нельзя.
		if errors.Is(err, fs.ErrNotExist) {
			// Первый запуск: в директории нет конфигурации → автосоздание рекомендуемой конфигурации (вкл. рандом api_key）Перезагрузить.
			// двойной клик exe / Голый прогон docker включается сразу, без ручного копирования примера.
			if key, werr := WriteDefault(*cfgPath); werr == nil {
				log.Printf("config %s Не существует, сгенерирована рекомендуемая конфигурация (api_key=%s，записано в этом файле, можно править вручную)", *cfgPath, key)
				cfg, err = Load(*cfgPath)
			}
			if err != nil {
				// Ошибка генерации (каталог только для чтения и т.п.): откат к чистому дефолту + env（фолбэк на старое поведение), не блокирует запуск.
				log.Printf("config %s not found (auto-generate failed), using defaults+env: %v", *cfgPath, err)
				cfg, err = Load("")
			}
		}
		if err != nil {
			log.Fatalf("load config: %v", err)
		}
	}

	auths, err := auth.LoadDir(cfg.AuthDir)
	if err != nil {
		log.Fatalf("load auths: %v", err)
	}
	log.Printf("loaded %d account(s) from %s", len(auths), cfg.AuthDir)

	// redisstore：Не настроено/Ошибка подключения → Noop（чистый in-memory режим, весь функционал как обычно).
	store := redisstore.New(cfg.Upstash.URL, cfg.Upstash.Token)

	p := pool.New(cfg.StateFile)
	// Последовательность останова: сначала pool.Close()（последний раз Flush → SaveState Уже отправлено в store），
	// Повторно store.Close() Дренаж незавершённых асинхронных записей (последняя запись Redis зеркало должно завершить запись перед закрытием соединения).
	defer func() {
		p.Close()
		_ = store.Close()
	}()
	p.SetStore(store)
	p.RestoreFromSnapshot() // Выбор нового для восстановления:Redis Снимок применяется только если новее локального, иначе приоритет у локального
	p.SyncToDir(auths) // и auths синхронизация каталога: добавление нового аккаунта, исключение аккаунтов с удалёнными файлами (статус сохраняется)

	// Circuit Breaker + Лимит in-flight (вкл. global по уровням)+ Понижение приоритета при серии неудач + Тюнинг компенсации простоя (с config инъекция,
	// неположительное значение — откат к дефолту).
	p.SetBreaker(cfg.Pool.BreakerThreshold, cfg.BreakerCooldownDur, cfg.BreakerCooldownMaxD)
	p.SetMaxInFlight(cfg.Pool.MaxInFlight)
	p.SetMaxInFlightGlobal(cfg.Pool.MaxInFlightGlobal) // global Домен WAF градация риск-контроля (P1-1）
	p.SetDegrade(cfg.Pool.DegradeThreshold, cfg.DegradeCooldownDur, cfg.DegradeCooldownMaxD)
	p.SetSoftRateMax(cfg.SoftRateMaxDur) // Потолок экспоненциального бэкоффа мягкого кулдауна (soft_rate_max，По умолчанию 2h）
	p.SetCostExploreInterval(cfg.CostExploreIntervalDur) // costTier Окно исследования (issue #136，По умолчанию 30m；0 останов)
	p.SetCreditFloor(cfg.Pool.CreditFloor) // гарант баллов (по умолч. 0 = закрыто)
	p.SetWeights(cfg.Pool.IdleWeightPerHour, cfg.Pool.IdleWeightMax)
	p.SetPreferExpiring(cfg.Pool.PreferExpiring)

	// Липкая маршрутизация сессии (отключаемая).
	var sessRouter *session.Router
	redisMode := "noop"
	if _, ok := store.(redisstore.Noop); !ok {
		redisMode = "upstash"
	}
	if cfg.SessionSticky.Enabled {
		sessRouter = session.New(session.Config{
			TTL: cfg.SessionTTL,
			GCInterval: cfg.SessionGCInterval,
			Store: store,
			Available: p.AvailableUIDs,
			// realm Замыкание восприятия: имена моделей с префиксом по realm Фильтрация доступных аккаунтов (кросс- realm без утечки);
			// голое имя через cn（текущее состояние без регрессий). Внутри замыкания resolveModel снять префикс, затем по realm Фильтрация.
			AvailableForModel: realmAwareAvailableForModel(p),
		})
		sessRouter.LoadFromStore() // При запуске из Redis восстановление sticky (чтение только здесь)
		sessRouter.StartGC()
		defer sessRouter.StopGC()
	}
	sessCount := func() int {
		if sessRouter != nil {
			return sessRouter.Count()
		}
		return 0
	}

	up := upstream.New()
	// Короткий RPC Верхний лимит общей длительности (refresh/checkin/balance/FetchModels），Семантика не меняется.
	up.HTTP.Timeout = time.Duration(cfg.Upstream.TimeoutSeconds) * time.Second
	// Чат SSE Лимит до первого байта (заголовки ответа):cfg Уже normalize（дефолтный fallback timeout_seconds）。
	up.HeaderTimeout = time.Duration(cfg.Upstream.HeaderTimeoutSeconds) * time.Second
	if tr, ok := up.ChatHTTP.Transport.(*http.Transport); ok {
		tr.ResponseHeaderTimeout = up.HeaderTimeout
	}
	// Чат SSE Лимит простоя в потоке (S3 чтение мониторинга простоя).
	up.IdleTimeout = time.Duration(cfg.Upstream.IdleTimeoutSeconds) * time.Second
	up.SanitizeFingerprints.Store(cfg.Features.SanitizeBlacklistFingerprints)
	// исходящий UA и заголовок принадлежности (issue #42 + синхронизация апстрима):
	// UserAgent Если не пусто — полностью переопределяет;ClientVersion/CliVersion по умолчанию выравнивание под официальный формат;
	// ClientName Если не пусто chat Инъекция пути X-IDE-* Четыре заголовка (атрибуция расхода выровнена с официальным десктопом).
	up.UserAgent = cfg.Upstream.UserAgent
	up.ClientVersion = cfg.Upstream.ClientVersion
	up.CliVersion = cfg.Upstream.CliVersion
	up.ClientName = cfg.Upstream.ClientName
	up.DeviceToken = cfg.Upstream.DeviceToken
	up.DeviceTokenFile = cfg.Upstream.DeviceTokenFile
	up.PassthroughIP = cfg.Upstream.PassthroughIP
	// global realm Маршрутизация (config global секция): свитч на стороне upstream (первый шлюз)+ base Перезапись;
	// auth Переключатель на стороне (auth.SetGlobalEnabled）— второй шлюз, оба config global.enabled。
	up.GlobalEnabled = cfg.Global.Enabled
	up.ChatBaseGlobal = cfg.Global.ChatBase
	up.BillingBaseGlobal = cfg.Global.BillingBase
	auth.SetGlobalEnabled(cfg.Global.Enabled)
	// model.json подключение локального кэша (context_length/max_output_tokens Четырёхуровневая цепочка поиска № 3 уровень):
	// Каталог данных и state.json в том же стиле (Docker volume путь персистентности). При первом отсутствии/Авто-повреждение
	// фолбэк-репозиторий со встроенным сидом;models.dev После успешного pull по требованию — атомарная запись обратно.
	upstream.SetModelCatalogPath(stateSibling(cfg.StateFile, "model.json"))

	sch := scheduler.New(scheduler.Config{
		Pool: p,
		Upstream: up,
		CheckinHours: cfg.Schedule.CheckinHours,
		TravelHours: cfg.Schedule.TravelHours,
		ActivityHours: cfg.Schedule.ActivityHours,
		KeepaliveHours: cfg.Schedule.KeepaliveHours,
		BlackcatHours: cfg.Schedule.BlackcatHours,
		GrowthHours: cfg.Schedule.GrowthHours,
		// приоритет списания скоро истекающих баллов: чекин/Обновление баланса батчится по этому окну (issue:истечение баллов).
		ExpiringSoonWindow: cfg.ExpiringSoonDur,
		CheckinDisabled: !cfg.Schedule.CheckinEnabled,
		TravelDisabled: !cfg.Schedule.TravelEnabled,
		ActivityDisabled: !cfg.Schedule.ActivityEnabled,
		KeepaliveDisabled: !cfg.Schedule.KeepaliveEnabled,
		BlackcatDisabled: !cfg.Schedule.BlackcatEnabled,
		GrowthDisabled: !cfg.Schedule.GrowthEnabled,
	})
	switch {
	case !cfg.Schedule.CheckinEnabled:
		log.Printf("Чекин отключен (schedule.checkin_enabled=false）")
	default:
		log.Printf("Регистрация включена:%v точка (чекин + разморозка запроса баланса)", cfg.Schedule.CheckinHours)
	}
	switch {
	case !cfg.Schedule.TravelEnabled:
		log.Printf("Путешествие котиков отключено (schedule.travel_enabled=false）")
	default:
		log.Printf("Кошачье путешествие включено:%v точка (независимое расписание: адопция / Отправить / получение награды)", cfg.Schedule.TravelHours)
	}
	switch {
	case !cfg.Schedule.ActivityEnabled:
		log.Printf("Активный репорт отключен (schedule.activity_enabled=false）")
	default:
		log.Printf("Активная отчётность включена:%v точка (ежедневно 1 раз, подсветка серийного входа + Разблокировать first_buddy）", cfg.Schedule.ActivityHours)
	}
	if !cfg.Schedule.KeepaliveEnabled {
		log.Printf("token keep-alive отключен (schedule.keepalive_enabled=false）")
	} else {
		log.Printf("token Keep-alive включен:%v Точка", cfg.Schedule.KeepaliveHours)
	}
	switch {
	case !cfg.Schedule.BlackcatEnabled:
		log.Printf("ночной режим отключён (schedule.blackcat_enabled=false）")
	default:
		log.Printf("ночной режим включен:%v точка (23:00–08:00 Окно glm-5.2 дополнение диалога)", cfg.Schedule.BlackcatHours)
	}
	switch {
	case !cfg.Schedule.BalanceRefreshEnabled:
		log.Printf("Фоновое обновление баланса отключено (schedule.balance_refresh_enabled=false）")
	case cfg.BalanceRefreshInterval > 0:
		log.Printf("Фоновое обновление баланса: каждые %s（в момент чекина дополнительно обновить как обычно)", cfg.BalanceRefreshInterval)
	}

	// Зеркало логов админ-панели: стандартный log（stderr）и chat Лог таблицы (stdout）Двухканальное копирование в
	// кольцевой буфер панели, для /panel/api/logs чтение; поведение вывода в консоль полностью без изменений.
	// live Несет горячо обновляемые поля (api_key/soft_rate/переключатель десенситизации), онлайн-замена при сохранении конфига панели.
	live := livecfg.New(livecfg.Snapshot{
		APIKey: cfg.APIKey,
		SoftCooldown: cfg.SoftRateDur,
		SanitizeFingerprints: cfg.Features.SanitizeBlacklistFingerprints,
		RecordClientInfo: cfg.Logging.RequestClientInfo,
	})
	// Счётчик использования: с state Файл в том же каталоге, вместе с state_file Конфигурация переносится вместе.
	// datapath От state Выводится из пути файла, без добавления параметра конфигурации.
	usagePath := usagePathFor(cfg.StateFile)
	rec := usage.New(usagePath)
	rec.Start()
	defer rec.Stop()
	log.Printf("[usage] позапросный учет расхода включен: %s (%s)", usagePath, rec.Describe())

	// метрики запросов всегда включены;JSONL Архив пишет только десенсибилизированные метаданные, ошибка записи на диск не влияет на запрос чата.
	requestLog := reqlog.New(reqlog.Config{
		Dir: stateSibling(cfg.StateFile, "request-logs"),
		Enabled: cfg.Logging.RequestArchiveEnabled,
		RetentionDays: cfg.Logging.RequestRetentionDays,
		MaxBytes: int64(cfg.Logging.RequestArchiveMaxMB) << 20,
	})
	defer requestLog.Close()
	rs := requestLog.Snapshot().Archive
	if rs.Enabled {
		log.Printf("[reqlog] Метрики запросов включены;JSONL Архив %s（сохранить %d дн., лимит %d MiB）",
			rs.Dir, cfg.Logging.RequestRetentionDays, cfg.Logging.RequestArchiveMaxMB)
	} else {
		log.Printf("[reqlog] Метрики запросов включены;JSONL архив закрыт")
	}

	pn := panel.New(panel.Config{
		Pool: p,
		Usage: rec,
		RequestLog: requestLog,
		Upstream: up,
		Scheduler: sch,
		AuthDir: cfg.AuthDir,
		APIKey: cfg.APIKey,
		RedisMode: redisMode,
		StickyCount: sessCount,
		Version: appVersion,
		Live: live,
		// данные зондирования лимита модели (scripts/probe_max_tokens.py --panel-out запись):
		// и state Файл в той же директории, по умолчанию data/output_probes.json。
		ProbeFile: stateSibling(cfg.StateFile, "output_probes.json"),
		ConfigPath: *cfgPath,
		LoadConfig: func() (any, error) {
			return Load(*cfgPath)
		},
		SaveConfig: func(raw []byte) ([]string, error) {
			return saveConfig(raw, *cfgPath, live, p, up, sch)
		},
	})
	// Очередь задач роста — ежедневное автоисполнение (тот же пайплайн, что "выполнить все задачи»):Sequential После разблокировки семейства в полночь
	// Ручное сканирование не требуется;hook Возврат — сразу запуск (асинхронное выполнение), при уже выполняющемся — пропуск внутри.
	sch.SetGrowthHook(pn.RunGrowthQueueOnce)
	log.SetOutput(io.MultiWriter(os.Stderr, pn.Logs()))
	server.SetChatLogOutput(io.MultiWriter(os.Stdout, pn.Logs()))

	h := server.NewHandler(server.Config{
		Pool: p,
		Upstream: up,
		APIKey: cfg.APIKey,
		Session: sessRouter,
		StickyCount: sessCount,
		RedisMode: redisMode,
		SoftCooldown: cfg.SoftRateDur,
		Panel: pn,
		Live: live,
		Usage: rec,
		RequestLog: requestLog,
		PromptMode: cfg.Prompt.Mode,
		PromptText: cfg.PromptText,
		// переключатель логирования источника через livecfg применяется на горячую; здесь же заполняется статическое поле для Live для nil 
		// прямое использование/Тестовый путь получает то же значение по умолчанию.
		RecordClientInfo: cfg.Logging.RequestClientInfo,
		// handler третий шлюз на стороне (global realm）：false（при явном escape-выходе) не включать global: имя модели.
		GlobalEnabled: cfg.Global.Enabled,
	})

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go sch.Run(ctx)
	sch.StartBalanceRefresh(ctx, cfg.BalanceRefreshInterval)

	srv := &http.Server{
		Addr: cfg.Listen,
		Handler: h,
		ReadHeaderTimeout: 30 * time.Second,
		// ReadTimeout покрывает чтение всего запроса (вкл. body）：защита от медленных body Зависание соединения.
		// Тело запроса уже без лимита на стороне шлюза (max_body_mb удалить),60s в десятки раз от обычной полосы MB
		// значение остатка загрузки; сверхбольшой body при таймауте медленной загрузки повторяет клиент.
		ReadTimeout: 60 * time.Second,
		// IdleTimeout keep-alive Сбор простаивающих соединений: совместно с chat исходящий ctx Защита от накопления утечек соединений при распространении.
		// Внимание:SSE Во время стримингового ответа соединение не idle, не обрывается этим правилом; без глобального WriteTimeout
		// （Легальная длительность потоковой генерации может достигать нескольких минут, глобально WriteTimeout Приведет к ложному завершению транзитных SSE）。
		IdleTimeout: 120 * time.Second,
	}
	go func() {
		<-ctx.Done()
		p.Flush() // Триггер по сигналу: сначала сброс на диск, затем graceful shutdown
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdownCtx)
	}()

	log.Printf("workbuddy2api listening on %s (api_key=%v),Панель управления http://127.0.0.1%s/panel/", cfg.Listen, cfg.APIKey != "", panelListenPath(cfg.Listen))
	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Fatalf("http: %v", err)
	}
	log.Printf("bye")
}

// panelListenPath Из listen Извлечение адреса ":port" формат, для сборки панели логов запуска URL
// （":7863« Или «0.0.0.0:7863« → »:7863"；аномальный ввод возвращается как есть).
func panelListenPath(listen string) string {
	for i := len(listen) - 1; i >= 0; i-- {
		if listen[i] == ':' {
			return listen[i:]
		}
	}
	return listen
}

// saveConfig Сохранение конфигурации панели: валидация → сброс на диск → Горячее применение → Вернуть список полей, требующих перезапуска.
//
// Область горячего применения (компромисс дизайна):
// - api_key / cooldown.soft_rate / features.sanitize_blacklist_fingerprints → livecfg Снапшот
// - pool.* → pool.SetBreaker/SetMaxInFlight/SetSoftRateMax/SetWeights/SetCostExploreInterval/SetPreferExpiring/SetCreditFloor
// - schedule.* → scheduler.Reconfigure/SetBalanceInterval/SetExpiringSoonWindow
//
// Требует перезапуска (затрагивает адрес прослушивания,HTTP client таймаут,auth_dir и зависимости этапа сборки):
// - listen / auth_dir / state_file / upstream.* / upstash.* / session_sticky.*（TTL класс)
//
// для записи на диск"Сначала запись tmp Повторно rename"Атомарная замена с приоритетом исходного файла на диске JSON структура (меняется только
// ключи, покрываемые формой панели), избегать перезаписи пользовательских полей-комментариев/неизвестные ключи стираются — здесь сразу целиком
// Конфигурация после валидации сериализации, неизвестные ключи в json.Unmarshal уже утеряно, поэтому сначала объединить исходное map。
func saveConfig(raw []byte, path string, live *livecfg.Holder, p *pool.Pool, up *upstream.Client, sch *scheduler.Scheduler) ([]string, error) {
	// 1) парсинг исходного JSON для map（сохранить неизвестные ключи, введенные пользователем вручную), затем наложить ключи, отправленные с панели.
	oldRaw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read current config: %w", err)
	}
	var cur, incoming map[string]any
	if err := json.Unmarshal(oldRaw, &cur); err != nil {
		cur = map[string]any{}
	}
	if err := json.Unmarshal(raw, &incoming); err != nil {
		return nil, fmt.Errorf("parse submitted config: %w", err)
	}
	merged := mergeConfigMaps(cur, incoming)

	// 2) валидация (тот же набор, что при запуске Default+normalize），При неудаче — прямой возврат без сохранения на диск.
	newCfg, err := ParseConfig(mergedJSON(merged))
	if err != nil {
		return nil, err
	}

	// 3) сохранение на диск (атомарная замена).
	out, err := json.MarshalIndent(merged, "", " ")
	if err != nil {
		return nil, fmt.Errorf("marshal config: %w", err)
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, out, 0o600); err != nil {
		return nil, fmt.Errorf("write config: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		// A single-file Docker bind mount cannot be renamed over its mount
		// target (Linux returns EBUSY / "device or resource busy"). Keep the
		// atomic path for regular files, but update the mounted file in place
		// for this specific deployment shape.
		if !errors.Is(err, syscall.EBUSY) {
			return nil, fmt.Errorf("replace config: %w", err)
		}
		f, openErr := os.OpenFile(path, os.O_WRONLY|os.O_TRUNC, 0o600)
		if openErr != nil {
			_ = os.Remove(tmp)
			return nil, fmt.Errorf("replace config (bind mount fallback): %w", openErr)
		}
		_, writeErr := f.Write(out)
		if writeErr == nil {
			writeErr = f.Sync()
		}
		closeErr := f.Close()
		// При ошибке записи сохранять tmp（смонтированный файл уже O_TRUNC Повреждение,tmp Внутри — полностью новое содержимое,
		// допускается ручное восстановление); очистка только после успешной записи.
		if writeErr != nil {
			return nil, fmt.Errorf("replace config (bind mount fallback, Полное новое содержимое сохраняется в %s): %w", tmp, writeErr)
		}
		_ = os.Remove(tmp)
		if closeErr != nil {
			return nil, fmt.Errorf("replace config (bind mount fallback): %w", closeErr)
		}
	}

	// 4) Горячее применение: все поля с немедленным эффектом применяются сразу, с выводом списка полей, требующих перезапуска.
	live.Store(livecfg.Snapshot{
		APIKey: newCfg.APIKey,
		SoftCooldown: newCfg.SoftRateDur,
		SanitizeFingerprints: newCfg.Features.SanitizeBlacklistFingerprints,
		RecordClientInfo: newCfg.Logging.RequestClientInfo,
	})
	up.SanitizeFingerprints.Store(newCfg.Features.SanitizeBlacklistFingerprints)
	p.SetBreaker(newCfg.Pool.BreakerThreshold, newCfg.BreakerCooldownDur, newCfg.BreakerCooldownMaxD)
	p.SetMaxInFlight(newCfg.Pool.MaxInFlight)
	p.SetMaxInFlightGlobal(newCfg.Pool.MaxInFlightGlobal)
	p.SetDegrade(newCfg.Pool.DegradeThreshold, newCfg.DegradeCooldownDur, newCfg.DegradeCooldownMaxD)
	p.SetSoftRateMax(newCfg.SoftRateMaxDur)
	p.SetCostExploreInterval(newCfg.CostExploreIntervalDur) // costTier Окно exploration применяется горячо (0 останов)
	p.SetCreditFloor(newCfg.Pool.CreditFloor) // Гарант баллов вступает в силу на горячую (0 = закрыто)
	p.SetWeights(newCfg.Pool.IdleWeightPerHour, newCfg.Pool.IdleWeightMax)
	p.SetPreferExpiring(newCfg.Pool.PreferExpiring)
	sch.SetExpiringSoonWindow(newCfg.ExpiringSoonDur)
	sch.Reconfigure(
		newCfg.Schedule.CheckinHours, newCfg.Schedule.TravelHours,
		newCfg.Schedule.ActivityHours, newCfg.Schedule.KeepaliveHours, newCfg.Schedule.BlackcatHours,
		newCfg.Schedule.GrowthHours,
		!newCfg.Schedule.CheckinEnabled, !newCfg.Schedule.TravelEnabled,
		!newCfg.Schedule.ActivityEnabled, !newCfg.Schedule.KeepaliveEnabled, !newCfg.Schedule.BlackcatEnabled,
		!newCfg.Schedule.GrowthEnabled)
	sch.SetBalanceInterval(newCfg.BalanceRefreshInterval)

	return restartRequiredFields(newCfg), nil
}

// restartRequiredFields Возвращает имена полей из текущего изменения, не применяемых на горячую и требующих перезапуска процесса.
// Всегда возвращает из полного списка"Связано с зависимостями сборки текущего процесса"элементов — панель уведомляет пользователя на их основе.
func restartRequiredFields(c *Config) []string {
	var out []string
	// эти поля внутри процесса слушаются адресом/HTTP client/Захват объектов этапа сборки, напр. хендлов каталогов.
	if c.Listen != "" {
		out = append(out, "listen")
	}
	if c.AuthDir != "" {
		out = append(out, "auth_dir")
	}
	if c.StateFile != "" {
		out = append(out, "state_file")
	}
	out = append(out, "upstream.timeout_seconds", "upstream.header_timeout_seconds", "upstream.idle_timeout_seconds")
	if c.Upstash.URL != "" || c.Upstash.Token != "" {
		out = append(out, "upstash")
	}
	out = append(out, "session_sticky.ttl", "session_sticky.gc_interval")
	out = append(out, "logging.request_archive_enabled", "logging.request_retention_days", "logging.request_archive_max_mb")
	return out
}

// mergeConfigMaps взять incoming глубоко мёрджится в cur（на месте), возврат cur。
// Покрытие вложенного объекта по ключам, а не замена целиком: форма панели отправляет только управляемые ею ключи,
// Неотправленные соседние ключи (включая неизвестные, введенные пользователем) остаются без изменений.
func mergeConfigMaps(cur, incoming map[string]any) map[string]any {
	for k, v := range incoming {
		if inMap, ok := v.(map[string]any); ok {
			if curMap, ok := cur[k].(map[string]any); ok {
				cur[k] = mergeConfigMaps(curMap, inMap)
				continue
			}
		}
		cur[k] = v
	}
	return cur
}

// mergedJSON объединённый map Сериализовать обратно JSON（Подача ParseConfig валидация).
func mergedJSON(m map[string]any) []byte {
	b, err := json.Marshal(m)
	if err != nil {
		return []byte("{}")
	}
	return b
}
