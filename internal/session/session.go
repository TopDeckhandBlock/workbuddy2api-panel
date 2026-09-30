// Package session Sticky-маршрутизация сессии: одна и та же сессия (conversationId / metadata ключ) по возможности привязывать к одному аккаунту.
//
// Справочник по проектированию antigravityProxyGo internal/session（fast-path RLock / двухсегментное распределение / TTL / персистентность),
// но переведено в чистую память + redisstore Асинхронное зеркало:
// - при попадании идти по RLock быстрая проверка (большинство запросов уже привязано);
// - промах/При истечении — через write-lock re-check Отложенное распределение, избегать совпадений key Параллельное повторное распределение (TOCTOU защита);
// - Приоритет распределения"Свободный аккаунт"（хеш доступных номеров без привязки к сессии), затем хеш всего пула (двухсегментная стратегия);
// - LastActive Скользящее продление,TTL истечение — на бэкенде GC или ленивая очистка просрочки по fast path;
// - Каждое изменение привязки fire-and-forget зеркалировать в redisstore（защита от потери sticky при рестарте).
package session

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"log"
	"strings"
	"sync"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/redisstore"
)

// entry Привязка одного сеанса.
type entry struct {
	uid string
	lastActive time.Time
}

// Config зависимость маршрутизации;Available вернуть"Доступные аккаунты"（healthy и in-flight не заполнен) упорядоченный uid Список,
// От pool.AvailableUIDs предоставляет.Store может быть redisstore.Noop（только в памяти).
type Config struct {
	TTL time.Duration
	GCInterval time.Duration
	Store redisstore.Store
	Available func() []string
	// AvailableForModel Возвращать по модели запроса"доступно на этой модели"учётная запись (healthy и in-flight не заполнен,
	// и не попал под лимит модели/лимит).nil откат при Available（без измерения по модели, поведение как до внедрения).
	//
	// Зачем sticky нужен уровень модели: привязка помнит только uid，при этом в рамках одной сессии модель может смениться. Аккаунт 6004
	// после лимита уровня модели для**другие модели**Всё ещё доступно (issue #31 исключение), тогда если только по доступности на уровне аккаунта
	// валидация, сессия залипнет на этом номере с повторными фейлами — именно"После лимита смена номера невозможна"источник визуального восприятия.
	AvailableForModel func(model string) []string
}

// Router Роутер sticky-сессий.
type Router struct {
	mu sync.RWMutex
	entries map[string]entry
	cfg Config
	stop chan struct{}
}

// New Собрать роутер. Если cfg.Store для nil то использовать Noop（только в памяти);cfg.Available для nil Считать пустым пулом.
// TTL/GCInterval иначе дефолт (30m / 5m）——main Из config передается после парсинга, здесь фолбэк.
func New(cfg Config) *Router {
	if cfg.Store == nil {
		cfg.Store = redisstore.Noop{}
	}
	if cfg.TTL <= 0 {
		cfg.TTL = 30 * time.Minute
	}
	if cfg.GCInterval <= 0 {
		cfg.GCInterval = 5 * time.Minute
	}
	return &Router{entries: map[string]entry{}, cfg: cfg}
}

// StartGC Запустить фон GC goroutine（идемпотентно). При выходе из процесса вызвать StopGC。
//
// stop channel Должен быть при запуске goroutine захвачено до**локальная переменная**：goroutine В select внутри
// переоценка каждый раунд r.stop — чтение без блокировки, а StopGC удерживая write-lock выставить его в nil——является гонкой данных
// （-race воспроизводимо), и при чтении nil затем позволить этому case Постоянная блокировка (nil channel никогда не ready),
// в итоге отключение полностью не срабатывает:goroutine больше никогда не выйдет,ticker Бесконечное срабатывание gcOnce（goroutine
// Утечка + после отключения всё ещё продолжается GC）。После захвата локальных переменных,close(stop) и select наблюдается тот же
// channel，StopGC обязательно позволит goroutine Выход.
func (r *Router) StartGC() {
	r.mu.Lock()
	if r.stop != nil {
		r.mu.Unlock()
		return
	}
	stop := make(chan struct{})
	r.stop = stop
	r.mu.Unlock()

	go func() {
		t := time.NewTicker(r.cfg.GCInterval)
		defer t.Stop()
		for {
			select {
			case <-stop:
				return
			case <-t.C:
				r.gcOnce(time.Now())
			}
		}
	}()
}

// StopGC остановить фон GC（идемпотентность).
func (r *Router) StopGC() {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.stop != nil {
		close(r.stop)
		r.stop = nil
	}
}

// LoadFromStore При запуске из redisstore Восстановление привязки (память перекрывает локальное, чтение только здесь).
// существующая локальная привязка сохранена —Redis Только для восстановления из бэкапа, после создания локальная копия — авторитетна.
func (r *Router) LoadFromStore() {
	binds := r.cfg.Store.LoadBinds()
	if len(binds) == 0 {
		return
	}
	now := time.Now()
	r.mu.Lock()
	loaded := 0
	for key, uid := range binds {
		if _, exists := r.entries[key]; exists {
			continue
		}
		r.entries[key] = entry{uid: uid, lastActive: now}
		loaded++
	}
	r.mu.Unlock()
	if loaded > 0 {
		log.Printf("[session] Из Redis Восстановление %d Привязка sticky-сессии, шт.", loaded)
	}
}

// Resolve Вернуть сессию key Привязываемый аккаунт uid，ok=false означает отсутствие доступных аккаунтов на данный момент.
// без измерения модели (эквивалентно ResolveForModel(key, "")），Оставлено для вызывающей стороны без привязки к модели.
func (r *Router) Resolve(key string) (string, bool) {
	return r.ResolveForModel(key, "")
}

// ResolveForModel Вернуть сессию key аккаунт, привязываемый к этой модели uid。
// Попадание и аккаунт доступен для этой модели → Прокрутка lastActive и сразу вернуть; иначе (привязанный номер уже в кулдауне/заполнено/
// затротллинг этой моделью) — перераспределение.
//
// Почему обязательно указывать модель: привязка хранит только uid，Одна сессия может сменить модель; аккаунт 6004 после лимита уровня модели
// Для других моделей всё ещё доступно (см. pool.healthyForModel softRateModel исключение). Если только на уровне аккаунта
// проверка доступности, сессия будет закреплена на одном"Недоступно для текущей модели"повторные сбои на аккаунте.
func (r *Router) ResolveForModel(key, model string) (string, bool) {
	now := time.Now()
	available := r.availableSet(model)

	// ── Fast path: RLock Быстрый поиск ──────────────────────────────
	r.mu.RLock()
	e, found := r.entries[key]
	r.mu.RUnlock()
	if found && !expired(e, now, r.cfg.TTL) {
		if available[e.uid] {
			r.touch(key, e.uid, now)
			return e.uid, true
		}
		// Привязанный номер в кулдауне/заполнено → истёк, попадает в перераспределение по медленному пути.
	}

	// ── Slow path: Write-lock re-check Распределение после ────────────────────
	r.mu.Lock()
	defer r.mu.Unlock()

	// re-check：параллельный тот же key Возможно уже другим goroutine распределено.
	if e2, found2 := r.entries[key]; found2 && !expired(e2, now, r.cfg.TTL) {
		if available[e2.uid] {
			r.entries[key] = entry{uid: e2.uid, lastActive: now}
			return e2.uid, true
		}
		delete(r.entries, key) // Истекло: очистить и перераспределить
	}

	uids := r.availableSlice(model)
	if len(uids) == 0 {
		return "", false
	}

	// Двухэтапная стратегия: приоритет"Свободный аккаунт"（доступный номер, не привязанный ни к одной сессии), затем весь пул.
	bound := map[string]bool{}
	for _, v := range r.entries {
		bound[v.uid] = true
	}
	var idle []string
	for _, u := range uids {
		if !bound[u] {
			idle = append(idle, u)
		}
	}
	pool2 := idle
	if len(pool2) == 0 {
		pool2 = uids
	}
	uid := pool2[hashIndex(key, len(pool2))]

	prev, existed := r.entries[key]
	r.entries[key] = entry{uid: uid, lastActive: now}
	if existed && prev.uid != uid {
		r.cfg.Store.DelBind(key)
	}
	r.cfg.Store.SetBind(key, uid, r.cfg.TTL)
	return uid, true
}

// touch Прокрутка lastActive и асинхронное зеркалирование (запись последнего раза только при попадании в fast path).
func (r *Router) touch(key, uid string, now time.Time) {
	r.mu.Lock()
	r.entries[key] = entry{uid: uid, lastActive: now}
	r.mu.Unlock()
	r.cfg.Store.SetBind(key, uid, r.cfg.TTL)
}

// Bind Явно перевести сессию key Привязать к uid（идемпотентно перезаписывает старое значение) и асинхронно зеркалируется в redisstore。
// Подача"Липкое следование за итоговым успешным номером"Назначение: до возврата успешного запроса перепривязать сессию к фактически успешному аккаунту для стабильности следующего хопа в многораундовом диалоге
// Сходится к"для номера с устойчивым успехом в этой сессии"（Выравнивание antigravity семантика). Пусто key Прямой возврат (без сессии привязка не создаётся).
func (r *Router) Bind(key, uid string) {
	if key == "" || uid == "" {
		return
	}
	now := time.Now()
	r.mu.Lock()
	r.entries[key] = entry{uid: uid, lastActive: now}
	r.mu.Unlock()
	r.cfg.Store.SetBind(key, uid, r.cfg.TTL)
}

// Unbind Снять привязку сессии (вызывается при ошибке запроса, чтобы сессия была перераспределена при следующем вызове). Возвращает наличие.
func (r *Router) Unbind(key string) bool {
	r.mu.Lock()
	_, found := r.entries[key]
	if found {
		delete(r.entries, key)
	}
	r.mu.Unlock()
	if found {
		r.cfg.Store.DelBind(key)
	}
	return found
}

// Count Возвращает текущее кол-во привязок (для /status наблюдением).
func (r *Router) Count() int {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return len(r.entries)
}

// gcOnce Очистка TTL просроченные привязки и зеркально удалить.
func (r *Router) gcOnce(now time.Time) int {
	r.mu.Lock()
	var expiredKeys []string
	for key, e := range r.entries {
		if now.Sub(e.lastActive) > r.cfg.TTL {
			expiredKeys = append(expiredKeys, key)
		}
	}
	for _, key := range expiredKeys {
		delete(r.entries, key)
	}
	r.mu.Unlock()
	for _, key := range expiredKeys {
		r.cfg.Store.DelBind(key)
	}
	return len(expiredKeys)
}

// availableSet взять AvailableForModel(model) упорядоченный список в множество (для проверки попадания по fast path).
func (r *Router) availableSet(model string) map[string]bool {
	uids := r.availableSlice(model)
	set := make(map[string]bool, len(uids))
	for _, u := range uids {
		set[u] = true
	}
	return set
}

// availableSlice Безопасный вызов AvailableForModel；фолбэк при отсутствии инжекта Available（nil считать пустым пулом).
func (r *Router) availableSlice(model string) []string {
	if r.cfg.AvailableForModel != nil {
		return r.cfg.AvailableForModel(model)
	}
	if r.cfg.Available == nil {
		return nil
	}
	return r.cfg.Available()
}

func expired(e entry, now time.Time, ttl time.Duration) bool {
	return now.Sub(e.lastActive) > ttl
}

// hashIndex FNV-1a хеш по модулю (antigravity стабильный хеш двухсегментного распределения).
func hashIndex(key string, n int) int {
	var h uint32 = 2166136261
	for i := 0; i < len(key); i++ {
		h ^= uint32(key[i])
		h *= 16777619
	}
	return int(h % uint32(n))
}

// ExtractKey Извлечь ключ сессии из тела запроса; пробовать по порядку, при отсутствии — откат к
// стабильный ключ, производный от контента (см. deriveKey），Если всё ещё пусто — вернуть пустую строку (никогда не падает).
// 1. metadata.conversation_id
// 2. metadata.conversationId
// 3. conversation_id
// 4. conversationId
// 5. prompt_cache_key（OpenAI префиксный ключ кэша, см. ниже)
// 6. производный ключ:system Промпт + Хеш первого сообщения пользователя (клиент не отправляет сессию id откат при,
// Лента user_id запрос не порождается — см. описание контракта ниже)
//
// Перед 5 пункты все conversation Измерение (уровень диалога).metadata.user_id Больше не используется как sticky-ключ
// （P1-anti-monopoly исключить):user гранулярность измерения слишком крупная — один user все параллельные диалоги
// будет закреплен за одним аккаунтом (диапазон sticky намного больше апстрима prompt cache граница уровня диалога), и ранее занимал верхний уровень
// conversation_id приоритет. После исключения отправить user_id клиентский фолбэк взвешенной ротации (с без-идентификатором
// тот же путь клиента), старый user_id привязка через TTL Естественное истечение, ключ исчезает без грязной привязки.
//
// issue #35：фактически отправлено клиентом camelCase conversationId，ранее распознавалось только snake_case，
// приводит к промаху sticky-маршрутизации, ротации аккаунтов в рамках одного диалога, кэшу контекста апстрима miss。Сейчас распознаются оба варианта имени,
// snake_case Приоритет выше camelCase（при совпадении одного диалога по разным именам с одним значением возвращается одно значение, естественное разделение без смешивания).
//
// № 5 Пункт prompt_cache_key：pi-ai Клиент драйвера (dsh и т.д.) сессию ID поместить в этот OpenAI
// В поле префиксного кэша (а не conversation_id），Шлюз в upstream Сторона и так его распознает (см.
// InjectPromptCacheKey Приоритет 1：если клиент уже передал — сохраняется исходное значение). После включения распознавания такие клиенты
// Попадание в sticky без изменения конфигурации. После явного ключа сессии, перед производным ключом, не перехватывает conversation
// Приоритет измерения.
//
// производного ключа user_id подавление (P1-anti-monopoly контракт в fallback продолжение пути):body Перенос
// metadata.user_id или верхний уровень user_id Время**не порождать**。ExtractKey Намеренно исключено user_id использовать как sticky-ключ
// （user гранулярность измерения слишком крупная — один user все параллельные диалоги будут прибиты к одному аккаунту), если путь деривации не задан
// Этот шлюз, отправлять только user_id запрос через хеш контента восстановит привязку, чтобы контракт fallback Путь недействителен
// （апстрим #169 аналогичный regression-fix). Такой клиентский fallback с взвешенной ротацией.
func ExtractKey(body []byte) string {
	if len(body) == 0 {
		return ""
	}
	var obj map[string]any
	if err := json.Unmarshal(body, &obj); err != nil {
		return ""
	}
	if meta, ok := obj["metadata"].(map[string]any); ok {
		if v := strOrEmpty(meta["conversation_id"]); v != "" {
			return v
		}
		if v := strOrEmpty(meta["conversationId"]); v != "" {
			return v
		}
	}
	if v := strOrEmpty(obj["conversation_id"]); v != "" {
		return v
	}
	if v := strOrEmpty(obj["conversationId"]); v != "" {
		return v
	}
	// 5. prompt_cache_key：OpenAI ключ префиксного кэша уровня сессии системы, семантика —«одна сессия переиспользует один и тот же
	// Префикс«，тот же источник, что и sticky-требование. Ставить после явного ключа сессии, перед производным ключом, без перехвата приоритета.
	if v := strOrEmpty(obj["prompt_cache_key"]); v != "" {
		return v
	}
	// Производный шлюз отката: с user Запросы с идентификатором измерения не порождают производные (см. контракт выше).
	if hasUserIDKey(obj) {
		return ""
	}
	return deriveKey(obj)
}

// hasUserIDKey Отчет распарсен body наличие user Идентификатор измерения (metadata.user_id или верхний уровень
// user_id，считается только непустая строка). Только для шлюза отката производных (ExtractKey）；При ошибке парсинга — игнорировать.
func hasUserIDKey(obj map[string]any) bool {
	if meta, ok := obj["metadata"].(map[string]any); ok {
		if strOrEmpty(meta["user_id"]) != "" {
			return true
		}
	}
	return strOrEmpty(obj["user_id"]) != ""
}

// derivedKeyPrefix Префикс производного ключа, с явной сессией id изоляция пространства имён:
// Даже если клиент передал вида "d-<hex>" явный id и не спутать с производным ключом (явно id приоритетный возврат).
const derivedKeyPrefix = "d-"

// deriveKey Генерация стабильного ключа сессии из содержимого сообщения:SHA-256(system текст + первая запись user текст) Перед 16 байт.
//
// Почему используется "system + первая запись user」а не все сообщения:
// - В многораундовом диалоге история добавляется каждый раунд, полный хеш меняется каждый раунд → Липкость полностью теряется;
// - system С первой записью user в рамках одного диалога константно, достаточно для различения диалогов;
// - Многократные запросы в одной сессии → Тот же ключ → Стабильно привязан к одному аккаунту (апстрим prompt попадание в кэш).
//
// При отсутствии текста пользователя (только картинка и т.п.) вернуть пустую строку: без стики, возврат к обычной ротации (безопасный даунгрейд).
func deriveKey(obj map[string]any) string {
	msgs, ok := obj["messages"].([]any)
	if !ok || len(msgs) == 0 {
		return ""
	}
	systemText, firstUserText := "", ""
	for _, m := range msgs {
		msg, ok := m.(map[string]any)
		if !ok {
			continue
		}
		text := messageText(msg["content"])
		switch strOrEmpty(msg["role"]) {
		case "system", "developer":
			if systemText == "" {
				systemText = text
			}
		case "user":
			if firstUserText == "" {
				// первая запись user По подписи контента (ids.go contentSignature）а не извлечение чистого текста:
				// Чистый цикл изображений (без text part）В messageText ниже всегда пустая строка → производный ключ недействителен
				//（слепая зона sticky-сессии первого изображения); изображение в режиме подписи part по [type:сводка] Входной ключ.
				firstUserText = userContentSignature(msg["content"])
			}
		}
		if firstUserText != "" && systemText != "" {
			break // все получено: прекратить обход длинной истории
		}
	}
	if firstUserText == "" {
		return "" // Нет сообщения пользователя: не к чему привязать сессию
	}
	sum := sha256.Sum256([]byte(systemText + "\x00" + firstUserText))
	return derivedKeyPrefix + hex.EncodeToString(sum[:16])
}

// userContentSignature Уже распарсенный content（any）Вернуть обратно RawMessage Последующая выборка**стабильность на уровне сессии**
// подпись, для deriveKey（липкий ключ) спец. С TurnKey contentSignature（от полного текста) отличается:
// текст part Склеивать как обычно; нетекстовое part Только вход "[type]" плейсхолдер**Не включать в дайджест контента**——та же логика
// Изображения URL Меняется каждый раунд (подпись URL/перекодирование base64）— реальный сценарий, включение саммари контента в ключ приведет к
// Производный ключ дрейфует по раундам, стики фактически нет (fork Существующий контракт:image url changes must not
// break derived key stability）。плейсхолдер всё ещё различим"Наличие изображения/Количество изображений"，первая запись — только изображение
// сообщение (без text part）отсюда может быть выведен непустой ключ (фикс слепой зоны sticky-сессии первого изображения, выравнивание с апстримом G1
// целевая семантика сохраняется fork метрика стабильности).marshal При ошибке обрабатывать как отсутствие контента (без подделки).
func userContentSignature(content any) string {
	raw, err := json.Marshal(content)
	if err != nil {
		return ""
	}
	st := strings.TrimSpace(string(raw))
	if st == "" || st == "null" {
		return ""
	}
	if st[0] == '"' {
		var str string
		if json.Unmarshal(raw, &str) != nil {
			return ""
		}
		return str
	}
	if st[0] != '[' {
		return ""
	}
	var parts []json.RawMessage
	if json.Unmarshal(raw, &parts) != nil {
		return ""
	}
	var b strings.Builder
	hasNonText := false
	for _, pr := range parts {
		var p struct {
			Type string `json:"type"`
			Text string `json:"text"`
		}
		if json.Unmarshal(pr, &p) != nil {
			return ""
		}
		if p.Type == "" || p.Type == "text" {
			b.WriteString(p.Text)
			continue
		}
		hasNonText = true
		b.WriteString("\n[" + p.Type + "]\n")
	}
	out := b.String()
	if !hasNonText {
		return out
	}
	return strings.TrimSpace(out)
}

// messageText Извлечь сообщение content текстовое представление.
// Совместимость: строка / [{type:"text«,text:«..."}] Массив (OpenAI мультимодальность); для остальных типов — пусто.
func messageText(content any) string {
	switch v := content.(type) {
	case string:
		return v
	case []any:
		var sb strings.Builder
		for _, part := range v {
			if p, ok := part.(map[string]any); ok {
				sb.WriteString(strOrEmpty(p["text"]))
			}
		}
		return sb.String()
	}
	return ""
}

// strOrEmpty взять JSON безопасное преобразование строкового поля string（нестроковый тип возвращает пусто).
func strOrEmpty(v any) string {
	s, _ := v.(string)
	return s
}
