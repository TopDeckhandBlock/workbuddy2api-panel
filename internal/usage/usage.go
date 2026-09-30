// Package usage Логировать и агрегировать по каждому запросу token Использование, для отображения в представлении "Использование» панели.
//
// и internal/pool TokenUsage различие:
// - pool TokenUsage Да**один накопительный счетчик на аккаунт**，сохранять только общее количество и "последний раз»,
// Без временной размерности, фильтрация по модели невозможна/детализация по времени;
// - Данный пакет по (Тайм-слайс, realm, uid, model, rate) Накопление по бакетам, поэтому можно вывести "сколько сегодня израсходовала каждая модель»
// 「Этот час prompt "насколько быстро растет» и т.п., и может храниться долго.
//
// Стратегия хранения (автопонижение гранулярности шардов, общий объём ограничен):
// - Близко hourlyKeep в пределах часа: часовой бакет (детально, для пиков)
// - Ранее: свернуть в суточный бакет,**сохранять бессрочно**（см. долгосрочный тренд)
//
// Сброс на диск:data/usage.json，Атомарная замена + debounce-обновление (по умолчанию 30s），Не теряется при перезапуске.
// Верхняя граница числа бакетов ≈ Количество аккаунтов × Количество моделей × (hourlyKeep + Прошедших дней)，Фактически один бакет около 90 байт.
package usage

import (
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// hourlyKeep Время хранения часового бакета; при превышении сворачивается в суточный.
const hourlyKeep = 90 * 24 * time.Hour

// flushInterval интервал дебаунса записи на диск.
const flushInterval = 30 * time.Second

// maxBuckets Жесткий лимит числа бакетов. При превышении немедленно запустить свертку, чтобы аномальный трафик не исчерпал память/файл переполнен.
const maxBuckets = 400_000

// hourLayout / dayLayout Формат времени ключа шардирования (локальный часовой пояс, соответствует интуиции пользователя).
const (
	hourLayout = "2006-01-02T15"
	dayLayout = "2006-01-02"
)

// fileVersion Да usage.json текущая версия формата. Версия 2 добавить поле наблюдения баллов, версия 3
// добавлено секционирование множителя применения модели; отсутствующие поля старых версий загружаются как ноль, старые данные не отбрасываются.
const fileVersion = 3

// bucket Один (Тайм-слайс, realm, uid, model, rate) накопленный объем.
// JSON имя поля намеренно короткое, т.к. число бакетов растёт со временем.
type bucket struct {
	Scope string `json:"s"` // »h:2006-01-02T15« Или »d:2006-01-02"
	Realm string `json:"r"` // cn / global
	UID string `json:"u"` // Аккаунт uid
	Model string `json:"m"` // Исходное имя модели апстрима
	Rate string `json:"x,omitempty"` // Действующий на момент запроса множитель баллов (нормализованное значение; старый бакет пуст)
	Req int64 `json:"q"` // число запросов (включая неудачные)
	Err int64 `json:"e"` // Количество неудач
	PT int64 `json:"p"` // prompt tokens
	CT int64 `json:"c"` // completion tokens
	TT int64 `json:"t"` // total tokens（сумма как отдал апстрим)
	LatMs int64 `json:"l"` // накопление задержки (ms）
	LatN int64 `json:"ln"` // количество выборок задержки
	TPS float64 `json:"v"` // Накопленная скорость вывода
	TPSN int64 `json:"vn"` // Кол-во выборок скорости
	CR float64 `json:"cr,omitempty"` // usage.credit Накопительно (только явно существующие наблюдения)
	CRN int64 `json:"cn,omitempty"` // usage.credit Число выборок (различать отсутствие поля и реальное 0）
	CRT int64 `json:"ct,omitempty"` // одновременно имеет credit и token Token Итого
}

// file Структура на диске.
type file struct {
	Version int `json:"version"`
	Saved string `json:"saved"`
	Buckets []bucket `json:"buckets"`
}

// Recorder потокобезопасный счётчик использования.
type Recorder struct {
	mu sync.Mutex
	path string
	buckets map[string]*bucket // key: scope|realm|uid|model|rate
	dirty bool
	started time.Time

	stopOnce sync.Once
	stop chan struct{}
	done chan struct{}
}

// New Создать логгер.path при пустом значении запись на диск отключена (только память, для тестов).
func New(path string) *Recorder {
	r := &Recorder{
		path: path,
		buckets: make(map[string]*bucket),
		started: time.Now(),
		stop: make(chan struct{}),
		done: make(chan struct{}),
	}
	if path != "" {
		if err := r.load(); err != nil {
			log.Printf("[usage] Чтение %s сбой (начиная с нуля): %v", path, err)
		}
	}
	return r
}

// Start Запустить фоновую запись с дебаунсом и свёртку.Stop до этого работал непрерывно.
func (r *Recorder) Start() {
	go func() {
		defer close(r.done)
		t := time.NewTicker(flushInterval)
		defer t.Stop()
		for {
			select {
			case <-r.stop:
				r.flush(true)
				return
			case <-t.C:
				r.mu.Lock()
				n := len(r.buckets)
				r.mu.Unlock()
				if n > maxBuckets {
					r.Rollup(time.Now())
				}
				r.flush(false)
			}
		}
	}()
}

// Stop остановить фоновый цикл и выполнить финальный flush на диск.
func (r *Recorder) Stop() {
	r.stopOnce.Do(func() { close(r.stop) })
	<-r.done
}

// Delta прирост расхода за одну попытку запроса (с pool.TokenUsageDelta той же формы, избегать межпакетных зависимостей).
type Delta struct {
	PromptTokens int64
	HasPromptTokens bool
	CompletionTokens int64
	HasCompletion bool
	TotalTokens int64
	HasTotal bool
	Credit float64
	HasCredit bool
	ModelRate string
	LatencyMs int64
	HasLatency bool
	TokensPerSecond float64
	HasTPS bool
}

// Add Записать одну попытку запроса.
//
// ok=false означает неудачу попытки (ошибка передачи / апстрим >=400 / Ошибка парсинга). Неудачные попытки обычно
// Отсутствует usage，Но**всё равно учитывается в счётчиках запросов и ошибок**——усиление ретраев видно только по этой колонке.
func (r *Recorder) Add(now time.Time, realm, uid, model string, d Delta, ok bool) {
	if r == nil {
		return
	}
	if realm == "" {
		realm = "cn"
	}
	if model == "" {
		model = "(unknown)"
	}
	scope := "h:" + now.Format(hourLayout)
	key := scope + "|" + realm + "|" + uid + "|" + model + "|" + d.ModelRate

	r.mu.Lock()
	defer r.mu.Unlock()

	b := r.buckets[key]
	if b == nil {
		b = &bucket{Scope: scope, Realm: realm, UID: uid, Model: model, Rate: d.ModelRate}
		r.buckets[key] = b
	}
	b.Req++
	if !ok {
		b.Err++
	}
	if d.HasPromptTokens {
		b.PT += d.PromptTokens
	}
	if d.HasCompletion {
		b.CT += d.CompletionTokens
	}
	if d.HasTotal {
		b.TT += d.TotalTokens
	} else if d.HasPromptTokens || d.HasCompletion {
		// Апстрим не передал total：использовать pt+ct fallback, гарантирующий непрерывность общей метрики.
		b.TT += d.PromptTokens + d.CompletionTokens
	}
	if d.HasCredit {
		b.CR += d.Credit
		b.CRN++
		// Пропорция использует только одновременно имеющиеся в одном запросе credit и token выборок, избегать
		// Только token старые записи или только credit наблюдения примешиваются в знаменатель.
		if d.HasTotal {
			b.CRT += d.TotalTokens
		} else if d.HasPromptTokens || d.HasCompletion {
			b.CRT += d.PromptTokens + d.CompletionTokens
		}
	}
	if d.HasLatency {
		b.LatMs += d.LatencyMs
		b.LatN++
	}
	if d.HasTPS {
		b.TPS += d.TokensPerSecond
		b.TPSN++
	}
	r.dirty = true
}

// Rollup вынести превышающее hourlyKeep часовые бакеты сворачиваются в суточные (по локальному календарному дню).
// Идемпотентность: повторное сворачивание в пределах одного часа не дублирует подсчет (сначала суммирование, затем удаление исходного бакета).
func (r *Recorder) Rollup(now time.Time) {
	if r == nil {
		return
	}
	cutoff := now.Add(-hourlyKeep)

	r.mu.Lock()
	defer r.mu.Unlock()

	type move struct{ from, to string }
	var moves []move
	for k, b := range r.buckets {
		if !strings.HasPrefix(b.Scope, "h:") {
			continue
		}
		ts, err := time.ParseInLocation(hourLayout, strings.TrimPrefix(b.Scope, "h:"), time.Local)
		if err != nil || !ts.Before(cutoff) {
			continue
		}
		day := "d:" + ts.Format(dayLayout)
		moves = append(moves, move{from: k, to: day + "|" + b.Realm + "|" + b.UID + "|" + b.Model + "|" + b.Rate})
	}
	for _, m := range moves {
		src := r.buckets[m.from]
		if src == nil {
			continue
		}
		dst := r.buckets[m.to]
		if dst == nil {
			cp := *src
			cp.Scope = strings.SplitN(m.to, "|", 2)[0]
			dst = &cp
			r.buckets[m.to] = dst
		} else {
			dst.Req += src.Req
			dst.Err += src.Err
			dst.PT += src.PT
			dst.CT += src.CT
			dst.TT += src.TT
			dst.LatMs += src.LatMs
			dst.LatN += src.LatN
			dst.TPS += src.TPS
			dst.TPSN += src.TPSN
			dst.CR += src.CR
			dst.CRN += src.CRN
			dst.CRT += src.CRT
		}
		delete(r.buckets, m.from)
	}
	if len(moves) > 0 {
		r.dirty = true
		log.Printf("[usage] Свернуть %d часовых бакетов как дневной бакет (сохранение %v гранулярность)", len(moves), hourlyKeep)
	}
}

// ---------------------------------------------------------------- Персистентность ----

func (r *Recorder) load() error {
	raw, err := os.ReadFile(r.path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	var f file
	if err := json.Unmarshal(raw, &f); err != nil {
		return err
	}
	for i := range f.Buckets {
		b := f.Buckets[i]
		r.buckets[b.Scope+"|"+b.Realm+"|"+b.UID+"|"+b.Model+"|"+b.Rate] = &b
	}
	log.Printf("[usage] Восстановлено %d бакетов использования (%s）", len(r.buckets), r.path)
	return nil
}

func (r *Recorder) flush(force bool) {
	if r == nil || r.path == "" {
		return
	}
	r.mu.Lock()
	if !r.dirty && !force {
		r.mu.Unlock()
		return
	}
	snap := file{Version: fileVersion, Saved: time.Now().Format(time.RFC3339), Buckets: make([]bucket, 0, len(r.buckets))}
	for _, b := range r.buckets {
		snap.Buckets = append(snap.Buckets, *b)
	}
	r.dirty = false
	r.mu.Unlock()

	raw, err := json.Marshal(snap)
	if err != nil {
		log.Printf("[usage] Ошибка сериализации: %v", err)
		return
	}
	if err := os.MkdirAll(filepath.Dir(r.path), 0o755); err != nil {
		log.Printf("[usage] ошибка создания каталога: %v", err)
		return
	}
	tmp := r.path + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		log.Printf("[usage] ошибка записи временного файла: %v", err)
		return
	}
	if err := os.Rename(tmp, r.path); err != nil {
		log.Printf("[usage] Сбой атомарной замены: %v", err)
	}
}

// Save Немедленная запись на диск (вызов при "Обновить» панели или перед закрытием).
func (r *Recorder) Save() { r.flush(true) }

// ---------------------------------------------------------------- Агрегация ----

// Agg группа накопительных показателей.
type Agg struct {
	Requests int64 `json:"requests"`
	Errors int64 `json:"errors"`
	PromptTokens int64 `json:"prompt_tokens"`
	CompletionTok int64 `json:"completion_tokens"`
	TotalTokens int64 `json:"total_tokens"`
	Credits float64 `json:"credits"`
	CreditSamples int64 `json:"credit_samples"`
	CreditTokens int64 `json:"credit_tokens"`
	CreditsPer1MTokens float64 `json:"credits_per_1m_tokens"`
	AvgLatencyMs float64 `json:"avg_latency_ms"`
	AvgTPS float64 `json:"avg_tokens_per_second"`
}

// aggAcc — аккумулятор в процессе агрегации:Agg Хранить только готовые результаты, для среднего нужно количество выборок
// Корректное взвешивание (нельзя усреднять средние по бакетам), поэтому количество сэмплов сохраняется здесь.
type aggAcc struct {
	Agg
	latSum int64
	latSamples int64
	tpsSum float64
	tpsSamples int64
}

func (g *aggAcc) add(b *bucket) {
	g.Requests += b.Req
	g.Errors += b.Err
	g.PromptTokens += b.PT
	g.CompletionTok += b.CT
	g.TotalTokens += b.TT
	g.Credits += b.CR
	g.CreditSamples += b.CRN
	g.CreditTokens += b.CRT
	g.latSum += b.LatMs
	g.latSamples += b.LatN
	g.tpsSum += b.TPS
	g.tpsSamples += b.TPSN
}

func (g *aggAcc) finish() Agg {
	a := g.Agg
	if g.latSamples > 0 {
		a.AvgLatencyMs = float64(g.latSum) / float64(g.latSamples)
	}
	if g.tpsSamples > 0 {
		a.AvgTPS = g.tpsSum / float64(g.tpsSamples)
	}
	if g.CreditTokens > 0 {
		a.CreditsPer1MTokens = g.Credits / float64(g.CreditTokens) * 1_000_000
	}
	return a
}

// KeyedAgg Строка, агрегированная по измерению.
type KeyedAgg struct {
	Key string `json:"key"`
	Realm string `json:"realm,omitempty"`
	Extra string `json:"extra,omitempty"` // в строке аккаунта — никнейм
	Agg
}

// Point Точка на временной шкале.
type Point struct {
	T string `json:"t"`
	Scope string `json:"scope"` // "hour" | "day"
	Agg
}

// CreditAgg строка статистики списания баллов.Key на уровне аккаунта это UID，В разрезе модели — голое имя модели;
// Rate Используется только в разрезе модели; знаменатель доли учитывает только credit Одновременно существующих Token выборка.
type CreditAgg struct {
	Key string `json:"key"`
	Realm string `json:"realm,omitempty"`
	Nickname string `json:"nickname,omitempty"`
	Rate string `json:"rate,omitempty"`
	Requests int64 `json:"requests"`
	Credits float64 `json:"credits"`
	CreditSamples int64 `json:"credit_samples"`
	CreditTokens int64 `json:"credit_tokens"`
	CreditsPer1MTokens float64 `json:"credits_per_1m_tokens"`
}

type creditAcc struct {
	CreditAgg
}

func (a *creditAcc) add(b *bucket) {
	a.Requests += b.Req
	a.Credits += b.CR
	a.CreditSamples += b.CRN
	a.CreditTokens += b.CRT
}

func (a *creditAcc) finish() CreditAgg {
	out := a.CreditAgg
	if a.CreditTokens > 0 {
		out.CreditsPer1MTokens = a.Credits / float64(a.CreditTokens) * 1_000_000
	}
	return out
}

// Snapshot все данные представления использования, полученные панелью за один фетч.
type Snapshot struct {
	Totals Agg `json:"totals"`
	ByRealm []KeyedAgg `json:"by_realm"`
	ByAccount []KeyedAgg `json:"by_account"`
	ByModel []KeyedAgg `json:"by_model"`
	Series []Point `json:"series"`
	CreditByAccount []CreditAgg `json:"credit_by_account"`
	CreditByModel []CreditAgg `json:"credit_by_model"`
	Buckets int `json:"buckets"`
	FileBytes int64 `json:"file_bytes"`
	Since string `json:"since,omitempty"`
	// WindowFrom/WindowTo Фактический статистический интервал этого раза (локальное время,RFC3339），для панели
	// Калибр отображения — в интервале "Кастом» пользователь должен видеть, по какому именно отрезку считает сервер.
	// Пустая строка = на этой стороне без ограничения (вся история / по сегодня).
	WindowFrom string `json:"window_from,omitempty"`
	WindowTo string `json:"window_to,omitempty"`
	Generated string `json:"generated"`
}

// Window окно статистики использования. Три калибра парсятся по приоритету (см. bounds）：
// - From/To Любое ненулевое → Явный интервал [From, To]（To нулевое значение = без верхнего лимита)
// - Иначе Hours>0 → Скользящее окно: назад от текущего часа Hours-1 ч
// - Иначе → Вся история
//
// Почему для явного интервала используется "начало бакета попадает в [From, To] внутри» а не пересечение: гранулярность часового бакета
// ровно один час, пользователь выбирает до 14:00 при этом 14:00 Включение бакета текущего часа интуитивно; при этом это также
// разрешить Hours Метрика побитово совпадает с историческим поведением (исходная реализация — ts.Before(from) то пропустить).
type Window struct {
	Hours int
	From time.Time
	To time.Time
}

// bounds распарсить фактически действующий [from, to]；Ноль означает отсутствие лимита на этой стороне.
func (w Window) bounds() (time.Time, time.Time) {
	if !w.From.IsZero() || !w.To.IsZero() {
		return w.From, w.To
	}
	if w.Hours <= 0 {
		return time.Time{}, time.Time{}
	}
	h := w.Hours
	if h > 24*60 {
		h = 24 * 60
	}
	return time.Now().Truncate(time.Hour).Add(-time.Duration(h-1) * time.Hour), time.Time{}
}

// bucketTime Переместить бакет scope Парсить как локальное время; грязные scope вернуть false（Не попадает ни в один срез).
func bucketTime(scope string) (time.Time, bool) {
	if strings.HasPrefix(scope, "h:") {
		ts, err := time.ParseInLocation(hourLayout, strings.TrimPrefix(scope, "h:"), time.Local)
		return ts, err == nil
	}
	ts, err := time.ParseInLocation(dayLayout, strings.TrimPrefix(scope, "d:"), time.Local)
	return ts, err == nil
}

// Snapshot Агрегация**в выбранном окне**бакет, формирует все данные представления использования за один pull панели.
//
// hours>0：Окно = [текущий час (ровно)-(hours-1)ч, now]，Сводка карточек/по домену/По аккаунту/по модели/
// Последовательность**все**Подсчёт по единому окну — при смене окна все числа меняются (долгое время было"Карточка —
// накоплено за всю историю,hours меняется только временной шард"в этой трактовке, в интерфейсе читается как«фильтрация не сработала"，уже deprecated).
// часовой бакет по ровному часу; суточный бакет (Rollup свернутые долгосрочные данные) входят в окно от начала суток, поэтому часовое окно
// естественно не содержит более ранних дневных бакетов.
// hours<=0：Вся история (включая свёрнутые дневные бакеты) для просмотра долгосрочного тренда в опции "Вся история».
//
// nicks Да uid→Маппинг никнеймов, только для отображения.
func (r *Recorder) Snapshot(hours int, nicks map[string]string) Snapshot {
	return r.SnapshotWithRates(hours, nicks, nil)
}

// SnapshotWithRates и Snapshot аналогично, но для старых бакетов без исторического множителя подставлять текущий
// обратная заливка коэффициента модели.currentRate При возврате пустой строки строка считается как“Неизвестный коэффициент”Агрегация, без подделки цены.
func (r *Recorder) SnapshotWithRates(hours int, nicks map[string]string, currentRate func(realm, model string) string) Snapshot {
	return r.SnapshotWindow(Window{Hours: hours}, nicks, currentRate)
}

// SnapshotWindow Агрегация**в выбранном окне**бакет, формирует все данные представления использования за один pull панели.
//
// семантика окна см. Window：скользящее окно (Hours）/ Явный интервал (From-To）/ Вся история. Сводка карточек,
// По домену, по аккаунту, по модели, по времени**все**Статистика по единому окну — при переключении окна все показатели меняются синхронно
// （долгое время был"Карточка — кумулятивно за всю историю,hours меняется только временной шард«в этой трактовке, в интерфейсе читается как"фильтр не
// Вступает в силу"，устарело). Часовой бакет входит в окно по ровному часу; суточный бакет (Rollup свернутые долгоживущие данные) попадают в окно по началу суток,
// поэтому часовое окно естественно не включает более ранние дневные бакеты.
func (r *Recorder) SnapshotWindow(w Window, nicks map[string]string, currentRate func(realm, model string) string) Snapshot {
	if r == nil {
		return Snapshot{Generated: time.Now().Format(time.RFC3339)}
	}
	from, to := w.bounds()
	explicit := !w.From.IsZero() || !w.To.IsZero()
	windowed := !from.IsZero() || !to.IsZero()

	r.mu.Lock()
	bs := make([]bucket, 0, len(r.buckets))
	for _, b := range r.buckets {
		bs = append(bs, *b)
	}
	r.mu.Unlock()

	var total aggAcc
	realmAgg := map[string]*aggAcc{}
	acctAgg := map[string]*aggAcc{}
	acctRealm := map[string]string{}
	modelAgg := map[string]*aggAcc{}
	hourSeries := map[string]*aggAcc{}
	daySeries := map[string]*aggAcc{}
	creditAcctAgg := map[string]*creditAcc{}
	creditModelAgg := map[string]*creditAcc{}
	rateCache := map[string]string{}

	// Начало данных (самый ранний шард всей БД): не зависит от окна, означает"Запись с какого момента началась"。scope Лексикографический порядок
	// т.е. хронологический порядок (сортировка одного формата внутри одного префикса;"d:« Всегда раньше "h:"——Дневной бакет только из 90 Свертка часов за N дней).
	since := ""
	matched := 0
	for i := range bs {
		b := &bs[i]
		if b.Scope < since || since == "" {
			since = b.Scope
		}
		if windowed {
			ts, ok := bucketTime(b.Scope)
			// Невалидные бакеты с ошибкой парсинга не попадают в оконную агрегацию (и не должны учитываться ни в одной метрике).
			if !ok {
				continue
			}
			if !from.IsZero() && ts.Before(from) {
				continue
			}
			if !to.IsZero() && ts.After(to) {
				continue
			}
		}
		matched++
		total.add(b)

		if realmAgg[b.Realm] == nil {
			realmAgg[b.Realm] = &aggAcc{}
		}
		realmAgg[b.Realm].add(b)

		if acctAgg[b.UID] == nil {
			acctAgg[b.UID] = &aggAcc{}
		}
		acctAgg[b.UID].add(b)
		// Один аккаунт принадлежит только одному realm，записать здесь для отображения колонки "домен» на фронте;
		// keyed() Realm Поле по умолчанию пустое (оно по key группировка, неизвестно realm）。
		if acctRealm[b.UID] == "" {
			acctRealm[b.UID] = b.Realm
		}

		if modelAgg[b.Model] == nil {
			modelAgg[b.Model] = &aggAcc{}
		}
		modelAgg[b.Model].add(b)

		if strings.HasPrefix(b.Scope, "h:") {
			scope := strings.TrimPrefix(b.Scope, "h:")
			if hourSeries[scope] == nil {
				hourSeries[scope] = &aggAcc{}
			}
			hourSeries[scope].add(b)
		} else {
			scope := strings.TrimPrefix(b.Scope, "d:")
			if daySeries[scope] == nil {
				daySeries[scope] = &aggAcc{}
			}
			daySeries[scope].add(b)
		}
		if b.CRN > 0 {
			ca := creditAcctAgg[b.UID]
			if ca == nil {
				ca = &creditAcc{CreditAgg: CreditAgg{
					Key: b.UID,
					Realm: b.Realm,
					Nickname: nicks[b.UID],
				}}
				creditAcctAgg[b.UID] = ca
			}
			ca.add(b)

			model := canonicalUsageModel(b.Model)
			rate := b.Rate
			if rate == "" && currentRate != nil {
				cacheKey := b.Realm + "\x00" + model
				if cached, ok := rateCache[cacheKey]; ok {
					rate = cached
				} else {
					rate = currentRate(b.Realm, model)
					rateCache[cacheKey] = rate
				}
			}
			modelKey := model + "\x00" + rate
			cm := creditModelAgg[modelKey]
			if cm == nil {
				cm = &creditAcc{CreditAgg: CreditAgg{Key: model, Rate: rate}}
				creditModelAgg[modelKey] = cm
			}
			cm.add(b)
		}
	}

	snap := Snapshot{
		Totals: total.finish(),
		ByRealm: keyed(realmAgg, func(k string) (string, string) {
			return k, ""
		}),
		ByAccount: keyed(acctAgg, func(k string) (string, string) {
			return k, nicks[k]
		}),
		ByModel: keyed(modelAgg, func(k string) (string, string) { return k, "" }),
		CreditByAccount: creditKeyed(creditAcctAgg),
		CreditByModel: creditKeyed(creditModelAgg),
		Buckets: matched,
		Generated: time.Now().Format(time.RFC3339),
	}
	for i := range snap.ByAccount {
		snap.ByAccount[i].Realm = acctRealm[snap.ByAccount[i].Key]
	}

	// точки дня (по возрастанию)+ часовые точки (по возрастанию) склеиваются в непрерывную временную последовательность.
	dayKeys := make([]string, 0, len(daySeries))
	for k := range daySeries {
		dayKeys = append(dayKeys, k)
	}
	sort.Strings(dayKeys)
	for _, k := range dayKeys {
		snap.Series = append(snap.Series, Point{T: k, Scope: "day", Agg: daySeries[k].finish()})
	}
	hourKeys := make([]string, 0, len(hourSeries))
	for k := range hourSeries {
		hourKeys = append(hourKeys, k)
	}
	sort.Strings(hourKeys)
	for _, k := range hourKeys {
		snap.Series = append(snap.Series, Point{T: k, Scope: "hour", Agg: hourSeries[k].finish()})
	}

	if r.path != "" {
		if fi, err := os.Stat(r.path); err == nil {
			snap.FileBytes = fi.Size()
		}
	}
	// since Удалить scope Префикс («h:2026-09-16T13« → "2026-09-16T13«）Для отображения на фронтенде;
	// При отсутствии бакетов остается пустым (без данных не подделывать начало).
	snap.Since = strings.TrimPrefix(strings.TrimPrefix(since, "h:"), "d:")
	// Отображение фактически действующего интервала:**только явный диапазон**。скользящее окно от hours выражение (фронтенд сам
	// известно, какой пресет выбран), вся история без интервалов — оба отображения станут шумом.
	if explicit {
		if !w.From.IsZero() {
			snap.WindowFrom = w.From.Format(time.RFC3339)
		}
		if !w.To.IsZero() {
			snap.WindowTo = w.To.Format(time.RFC3339)
		}
	}
	return snap
}

func canonicalUsageModel(model string) string {
	model = strings.TrimSpace(model)
	for _, prefix := range []string{"cn:", "global:"} {
		if strings.HasPrefix(model, prefix) {
			model = strings.TrimPrefix(model, prefix)
			break
		}
	}
	if model == "" {
		return "(unknown)"
	}
	return model
}

func creditKeyed(m map[string]*creditAcc) []CreditAgg {
	out := make([]CreditAgg, 0, len(m))
	for _, v := range m {
		out = append(out, v.finish())
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Credits != out[j].Credits {
			return out[i].Credits > out[j].Credits
		}
		if out[i].CreditTokens != out[j].CreditTokens {
			return out[i].CreditTokens > out[j].CreditTokens
		}
		if out[i].Key != out[j].Key {
			return out[i].Key < out[j].Key
		}
		return out[i].Rate < out[j].Rate
	})
	return out
}

func keyed(m map[string]*aggAcc, label func(string) (string, string)) []KeyedAgg {
	out := make([]KeyedAgg, 0, len(m))
	for k, v := range m {
		key, extra := label(k)
		out = append(out, KeyedAgg{Key: key, Extra: extra, Agg: v.finish()})
	}
	// Сортировка по общему объёму по убыванию; при равенстве по key По возрастанию, гарантирует стабильный вывод (фронтенд diff без джиттера).
	sort.Slice(out, func(i, j int) bool {
		if out[i].TotalTokens != out[j].TotalTokens {
			return out[i].TotalTokens > out[j].TotalTokens
		}
		if out[i].Requests != out[j].Requests {
			return out[i].Requests > out[j].Requests
		}
		return out[i].Key < out[j].Key
	})
	return out
}

// Describe Возвращает однострочную человекочитаемую сводку занятости (для лога запуска).
func (r *Recorder) Describe() string {
	if r == nil {
		return "disabled"
	}
	r.mu.Lock()
	n := len(r.buckets)
	r.mu.Unlock()
	var sz int64
	if r.path != "" {
		if fi, err := os.Stat(r.path); err == nil {
			sz = fi.Size()
		}
	}
	return fmt.Sprintf("%d buckets, file %d bytes", n, sz)
}
