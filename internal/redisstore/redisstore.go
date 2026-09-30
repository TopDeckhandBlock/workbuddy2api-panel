// Package redisstore Инкапсуляция Upstash（Redis）Персистентность с фолбэком в память (Noop）。
//
// Ограничения проектирования:Upstash через публичную сеть TLS，Однократно RTT Возможно 50~300ms，поэтому все операции записи —
// fire-and-forget（бэкенд goroutine + при сбое только debug лог), операции чтения только при запуске
// （загрузить снапшот sticky-сессии, восстановить cooldown/снапшот circuit breaker). В основном память,Redis вспомогательно.
//
// Не настроено url / При ошибке соединения деградация до Noop：Весь функционал работает штатно (чистый in-memory режим),
// Верхний уровень пишет только одно стартовое warning-сообщение.
package redisstore

import (
	"context"
	"log"
	"strings"
	"sync"
	"time"

	"github.com/redis/go-redis/v9"
)

// keyTTL зеркало sticky-сессии + дефолт снапшота состояния TTL（redis страховка на стороне, защита от долгого зависания грязных данных).
const keyTTL = 7 * 24 * time.Hour

// writeConcurrencyLimit fire-and-forget Лимит незавершённых асинхронных записей (обнаружено 4：Запись goroutine
// Без ограничения семафором, при высокой скорости записи возможен мгновенный всплеск). Превышение ставится в очередь без отбрасывания — семантика записи не меняется (см. goWrite）。
const writeConcurrencyLimit = 8

// Store оставлены только методы текущего этапа. Контекст строится внутри реализации (чтение с коротким таймаутом, запись fire-and-forget）。
type Store interface {
	// SetBind Асинхронная зеркальная sticky-привязка сессии (key→uid），Лента TTL。
	SetBind(key, uid string, ttl time.Duration)
	// DelBind Асинхронное удаление sticky-привязки сессии.
	DelBind(key string)
	// LoadBinds полное чтение привязки sticky-сессии (key→uid，key префикс уже снят); вызывать только при старте (синхронно).
	// для восстановления sticky-маппинга при холодном старте (защита от потери стикки при рестарте).
	LoadBinds() map[string]string
	// SaveState Статус асинхронного пула записи JSON Снимок (с локальным state.json сосуществует, только как бэкап для восстановления).
	SaveState(data []byte)
	// LoadState Чтение снапшота состояния пула; вызывается только при запуске (синхронно).
	LoadState() ([]byte, bool)
	// Close Отключение Store：Upstash Дождаться выполнения всех отправленных асинхронных записей, затем закрыть нижележащее соединение
	// （Семантика останова: последняя транзакция Redis образ должен быть полностью записан), последующие новые записи отбрасываются; идемпотентно.
	// Noop — no-op. Перед выходом процесса в pool.Close() Последующий вызов.
	Close() error
}

const (
	bindPrefix = "wb2api:bind:"
	stateKey = "wb2api:state"
	readTimeout = 3 * time.Second
)

// New Согласно url+token Сборка Store。
// - url пусто → Noop（режим только в памяти)
// - url уже полный rediss:// URL то напрямую ParseURL；Иначе использовать token сборка rediss://default:token@host:6379
// - Ping ошибка → Noop + предупреждение при запуске (жёсткое требование даунгрейда: не из-за Redis недоступен и завершается ошибкой)
func New(url, token string) Store {
	if url == "" {
		log.Printf("[redisstore] upstash не настроено, переход в чисто in-memory режим (Noop деградация)")
		return Noop{}
	}

	full := normalizeURL(url, token)
	opt, err := redis.ParseURL(full)
	if err != nil {
		log.Printf("[redisstore] Предупреждение: redis Ошибка парсинга строки подключения (%v)，Деградация Noop", err)
		return Noop{}
	}
	opt.ReadTimeout = readTimeout
	opt.WriteTimeout = readTimeout
	client := redis.NewClient(opt)

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := client.Ping(ctx).Err(); err != nil {
		log.Printf("[redisstore] Предупреждение: upstash Ошибка подключения (%v)，Деградация Noop（режим только в памяти)", err)
		_ = client.Close()
		return Noop{}
	}
	log.Printf("[redisstore] upstash Подключено (addr=%s)", opt.Addr)
	return &Upstash{
		client: client,
		sem: make(chan struct{}, writeConcurrencyLimit),
		done: make(chan struct{}),
	}
}

// normalizeURL взять url+token Нормализовать в напрямую ParseURL полный rediss:// URL。
// Если url уже содержит scheme（rediss://、redis://、https://...upstash.io и т.д.):
// - rediss:// Или redis:// вернуть как есть (уже полная строка подключения)
// - Остальное (напр. https://xxx.upstash.io）Снять "://" Префикс берет только host，затем по
// "rediss://default:<token>@<host>:6379" сборка
func normalizeURL(url, token string) string {
	if len(url) >= 8 && (url[:8] == "rediss:/" || url[:7] == "redis:/") {
		return url
	}
	host := url
	if i := strings.Index(host, "://"); i >= 0 {
		host = host[i+3:]
	}
	return "rediss://default:" + token + "@" + host + ":6379"
}

// Upstash фактическая реализация:redis.Client Инкапсуляция.
//
// Лимит конкурентной записи (обнаружено 4）：три асинхронные записи совместно используют sem（cap=writeConcurrencyLimit семафор),
// При превышении лимита in-flight записей новые ставятся в очередь без отбрасывания — семантика остаётся fire-and-forget，просто"бесконечное накопление"
// Сходится к"очередь с ограничением"。Close ранее отправленные записи (включая очередь) гарантированно завершатся,Close Новая отправка после
// запись отбрасывается.
type Upstash struct {
	client *redis.Client
	// sem Семафор записи (ограниченные in-flight записи).cap=1 деградирует до последовательной записи для наблюдения семантики планирования в тестах.
	sem chan struct{}
	// done флаг отключения (Close закрытие).sem и done От New инициализация; в тестах можно конструировать напрямую
	//（client=nil，goWrite/Close без обращения к сети).
	done chan struct{}
	// closeOnce Гарантировать Close идемпотентность (многократный вызов закрывает только один раз done channel）。
	closeOnce sync.Once
	// submitMu Сузить goWrite коммит/Гонка при остановке:goWrite Сначала зарегистрировать wg повторно запросить done，
	// Close Сначала закрыть done подождать еще wg——после взаимного исключения сторон, "Close ранее отправленная запись обязательно выполнится»
	// Больше не зависит от goroutine Порядок диспетчеризации (83d18ae в оригинале есть окно: запись в очередь в Close
	// закрыть done позже достигнет чекпоинта — будет ошибочно отброшено,close_test.go:138 стабильно воспроизводится).
	submitMu sync.Mutex
	// wg отправленная незавершенная запись (Close для дренажа).
	wg sync.WaitGroup
}

// goWrite по fire-and-forget выполнить способом fn：Слот записи (sem）Ограниченный параллелизм,Close отправленная ранее запись
// Обязательно выполняется (целостность образа при остановке),Close Отправленная позже запись отбрасывается (процесс уже выходит).
func (u *Upstash) goWrite(fn func()) {
	u.closeOnceGuard()
	u.submitMu.Lock()
	u.wg.Add(1)
	select {
	case <-u.done:
		u.submitMu.Unlock()
		u.wg.Done() // запись после останова: регистрация сразу отменяется, напрямую отбрасывается
		return
	default:
	}
	u.submitMu.Unlock()
	go func() {
		defer u.wg.Done()
		// Блокирует только слот конкурентной записи, без проверки done：если здесьselect done，заблокированная в очереди запись выполнится в
		// close(done) При пробуждении всё уходит в ветку отбрасывания (selrand5 На практике 100%），「Close Перед
		// контракт "отправленная запись (включая очередь) гарантированно выполнится» нарушен внутренней проверкой —close_test.go:138
		// поэтому периодические сбои (около 20%：Зависит от write-1/write-2 кто первым захватит единственный слот). Отбросить
		// семантика уже определяется точкой коммита (submitMu в done проверка) единственный ответственный; коммит здесь заморожен на
		// wg в,Close wg.Wait Обязательно дождаться завершения.
		u.sem <- struct{}{}
		defer func() { <-u.sem }()
		fn()
	}()
}

// closeOnceGuard защита от нуля Upstash（без New конструкция) в goWrite/Close Вверх nil-map краш типа:
// sem/done для nil досоздать при (cap=1）。Этот путь достигается только в тестах.
func (u *Upstash) closeOnceGuard() {
	if u.sem == nil || u.done == nil {
		u.sem = make(chan struct{}, 1)
		u.done = make(chan struct{})
	}
}

// Close Дождаться выполнения всех отправленных асинхронных записей, затем закрыть нижний уровень redis соединение; идемпотентно.
// последующие новые записи на запись отбрасываются (goWrite done проверка). Путь останова в pool.Close() вызов после:
// pool последний раз для Flush→SaveState Уже отправлено, метод гарантирует возврат только после завершения записи.
func (u *Upstash) Close() error {
	u.closeOnceGuard()
	// Сначала закрыть done подождать еще wg：и goWrite submitMu После мьютекса множество "закоммиченных записей»
	// заморожено (новые сабмиты далее отбрасываются),wg.Wait дренаж перезатирает in-flight + Два уровня очереди —
	// Исходный sem Метод зондирования ошибочно считает очередь пустой, когда queued write ещё не дошёл до точки захвата слота (83d18ae гонка).
	u.submitMu.Lock()
	u.closeOnce.Do(func() { close(u.done) })
	u.submitMu.Unlock()
	done := make(chan struct{})
	go func() { u.wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		// Фолбэк-таймаут (лимит одиночной записи 5s，10s запас): зависшая запись не должна блокировать выход процесса.
		log.Printf("[redisstore] WARN: Close таймаут ожидания inflight-записи, отмена (зеркало может быть не записано)")
	}
	return u.closeClient()
}

// closeClient Отключить нижний уровень redis Соединение (client для nil——тестовая конструкция — при пропуске).
func (u *Upstash) closeClient() error {
	if u.client == nil {
		return nil
	}
	return u.client.Close()
}

func bindKey(key string) string { return bindPrefix + key }

// SetBind Привязка липкой сессии асинхронного зеркала.
func (u *Upstash) SetBind(key, uid string, ttl time.Duration) {
	if ttl <= 0 {
		ttl = keyTTL
	}
	u.goWrite(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := u.client.Set(ctx, bindKey(key), uid, ttl).Err(); err != nil {
			log.Printf("[redisstore] debug: SetBind %s: %v", key, err)
		}
	})
}

// DelBind Асинхронное удаление sticky-привязки сессии.
func (u *Upstash) DelBind(key string) {
	u.goWrite(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := u.client.Del(ctx, bindKey(key)).Err(); err != nil {
			log.Printf("[redisstore] debug: DelBind %s: %v", key, err)
		}
	})
}

// SaveState Статус асинхронного пула записи JSON снимок.
func (u *Upstash) SaveState(data []byte) {
	u.goWrite(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := u.client.Set(ctx, stateKey, data, keyTTL).Err(); err != nil {
			log.Printf("[redisstore] debug: SaveState: %v", err)
		}
	})
}

// LoadState Синхронный снапшот статуса пула чтения.
func (u *Upstash) LoadState() ([]byte, bool) {
	ctx, cancel := context.WithTimeout(context.Background(), readTimeout)
	defer cancel()
	v, err := u.client.Get(ctx, stateKey).Bytes()
	if err != nil {
		return nil, false
	}
	return v, true
}

// LoadBinds полное чтение привязки sticky-сессии (SCAN bind:* префикс).
func (u *Upstash) LoadBinds() map[string]string {
	out := map[string]string{}
	ctx, cancel := context.WithTimeout(context.Background(), readTimeout)
	defer cancel()
	iter := u.client.Scan(ctx, 0, bindPrefix+"*", 200).Iterator()
	for iter.Next(ctx) {
		key := iter.Val()
		v, err := u.client.Get(ctx, key).Result()
		if err != nil {
			continue
		}
		out[strings.TrimPrefix(key, bindPrefix)] = v
	}
	return out
}

// Noop Деградация в чистую память: все методы — no-op.
type Noop struct{}

func (Noop) SetBind(string, string, time.Duration) {}
func (Noop) DelBind(string) {}
func (Noop) LoadBinds() map[string]string { return nil }
func (Noop) SaveState([]byte) {}
func (Noop) LoadState() ([]byte, bool) { return nil, false }
func (Noop) Close() error { return nil }
