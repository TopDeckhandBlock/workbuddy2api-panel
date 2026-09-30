package usage

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// Успех/Счётчик неудачных попыток,total pt+ct резервный критерий, по домену/Агрегация аккаунтов.
func TestAddAndTotals(t *testing.T) {
	r := New("")
	now := time.Now()
	r.Add(now, "cn", "uid1", "glm-5.2", Delta{PromptTokens: 100, HasPromptTokens: true, CompletionTokens: 50, HasCompletion: true, Credit: 1.5, HasCredit: true, ModelRate: "0.05", LatencyMs: 200, HasLatency: true}, true)
	// неудачных попыток: нет usage → Учитывать только кол-во запросов и ошибок,token Не добавлять.
	r.Add(now, "global", "uid1", "claude-4.6", Delta{}, false)
	// Апстрим не передал total использовать при pt+ct fallback, гарантирующий непрерывность общей метрики.
	r.Add(now, "cn", "uid1", "glm-5.2", Delta{PromptTokens: 10, HasPromptTokens: true, CompletionTokens: 5, HasCompletion: true}, true)

	s := r.Snapshot(24, nil)
	if s.Totals.Requests != 3 || s.Totals.Errors != 1 {
		t.Fatalf("requests/errors = %d/%d, want 3/1", s.Totals.Requests, s.Totals.Errors)
	}
	if s.Totals.PromptTokens != 110 || s.Totals.CompletionTok != 55 {
		t.Fatalf("pt/ct = %d/%d, want 110/55", s.Totals.PromptTokens, s.Totals.CompletionTok)
	}
	if s.Totals.TotalTokens != 165 {
		t.Fatalf("tt = %d, want 165（отсутствует total При ... по pt+ct фолбэк)", s.Totals.TotalTokens)
	}
	if s.Totals.Credits != 1.5 || s.Totals.CreditSamples != 1 || s.Totals.CreditTokens != 150 || s.Totals.CreditsPer1MTokens != 10000 {
		t.Fatalf("credit totals = %+v, want credits=1.5 samples=1 tokens=150 ratio=10000", s.Totals)
	}
	if s.Totals.AvgLatencyMs != 200 {
		t.Fatalf("avg latency = %v, want 200", s.Totals.AvgLatencyMs)
	}
	if len(s.ByRealm) != 2 {
		t.Fatalf("by_realm = %d Пункт, want 2", len(s.ByRealm))
	}
	if s.ByAccount[0].Realm == "" {
		t.Fatal("by_account отсутствует строка realm Метка")
	}
	if len(s.CreditByAccount) != 1 || s.CreditByAccount[0].Key != "uid1" ||
		s.CreditByAccount[0].CreditSamples != 1 || s.CreditByAccount[0].CreditsPer1MTokens != 10000 {
		t.Fatalf("credit_by_account = %+v, want one uid1 row", s.CreditByAccount)
	}
	if len(s.CreditByModel) != 1 || s.CreditByModel[0].Key != "glm-5.2" ||
		s.CreditByModel[0].Rate != "0.05" || s.CreditByModel[0].CreditsPer1MTokens != 10000 {
		t.Fatalf("credit_by_model = %+v, want one glm-5.2 rate=0.05 row", s.CreditByModel)
	}
}

// Rollup вынести превышающее hourlyKeep часовые бакеты сворачиваются в суточные, идемпотентно: повторное сворачивание не дублирует подсчет.
// метрика окна:24h Окно не включает 100 дневной бакет N дней назад;hours=0（вся история) только тогда содержит дневные точки.
func TestRollupIdempotent(t *testing.T) {
	r := New("")
	old := time.Now().AddDate(0, 0, -100) // 100 дней назад, превышение 90 дни/часы сохранить
	r.Add(old, "cn", "u", "m", Delta{PromptTokens: 7, HasPromptTokens: true}, true)
	r.Add(old, "cn", "u", "m", Delta{PromptTokens: 7, HasPromptTokens: true}, true)
	r.Add(time.Now(), "cn", "u", "m", Delta{PromptTokens: 1, HasPromptTokens: true}, true)

	r.Rollup(time.Now())
	after := r.Snapshot(24, nil)
	if after.Totals.Requests != 1 || after.Totals.PromptTokens != 1 {
		t.Fatalf("24h Окно totals = %d/%d, want 1/1（дневной бакет вне окна не входит в агрегацию)", after.Totals.Requests, after.Totals.PromptTokens)
	}
	if len(after.Series) != 1 || after.Series[0].Scope != "hour" {
		t.Fatalf("series = %+v, want Только текущий час 1 точек", after.Series)
	}

	all := r.Snapshot(0, nil)
	if all.Totals.Requests != 3 || all.Totals.PromptTokens != 15 {
		t.Fatalf("Вся история totals = %d/%d, want 3/15", all.Totals.Requests, all.Totals.PromptTokens)
	}
	if len(all.Series) != 2 || all.Series[0].Scope != "day" || all.Series[1].Scope != "hour" {
		t.Fatalf("series = %+v, want дневная отметка впереди + часовая отметка позже", all.Series)
	}

	r.Rollup(time.Now())
	again := r.Snapshot(0, nil)
	if again.Totals.Requests != 3 || again.Totals.PromptTokens != 15 {
		t.Fatalf("После вторичного сворачивания totals = %d/%d, want 3/15（идемпотентность нарушена)", again.Totals.Requests, again.Totals.PromptTokens)
	}
}

// сброс на диск→Восстановление нового инстанса без потери данных; структура на диске версионирована.
func TestFlushLoadRoundtrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "usage.json")
	r1 := New(path)
	r1.Add(time.Now(), "cn", "u1", "glm-5.2", Delta{PromptTokens: 42, HasPromptTokens: true, TotalTokens: 42, HasTotal: true}, true)
	r1.Save()

	r2 := New(path)
	s := r2.Snapshot(24, nil)
	if s.Totals.Requests != 1 || s.Totals.TotalTokens != 42 {
		t.Fatalf("После восстановления totals = %d/%d, want 1/42", s.Totals.Requests, s.Totals.TotalTokens)
	}
	raw, _ := os.ReadFile(path)
	var f file
	if err := json.Unmarshal(raw, &f); err != nil || f.Version != fileVersion || len(f.Buckets) != 1 {
		t.Fatalf("Ошибка файла на диске: err=%v buckets=%d", err, len(f.Buckets))
	}
}

// Версия 1 В файле нет поля баллов: восстановить как ноль, старый Token Данные остаются видимыми без искажения пропорций.
func TestLoadLegacyWithoutCredit(t *testing.T) {
	path := filepath.Join(t.TempDir(), "usage.json")
	legacy := `{"version":1,"saved":"2026-09-28T00:00:00+08:00","buckets":[{"s":"h:2026-09-28T10","r":"cn","u":"u1","m":"glm-5.2","q":1,"p":42,"t":42}]}`
	if err := os.WriteFile(path, []byte(legacy), 0o600); err != nil {
		t.Fatal(err)
	}
	r := New(path)
	s := r.Snapshot(0, nil)
	if s.Totals.TotalTokens != 42 || s.Totals.CreditSamples != 0 || s.Totals.CreditsPer1MTokens != 0 {
		t.Fatalf("legacy totals = %+v, want token-only history", s.Totals)
	}
	if len(s.CreditByAccount) != 0 || len(s.CreditByModel) != 0 {
		t.Fatalf("legacy credit dimensions = %+v / %+v, want none", s.CreditByAccount, s.CreditByModel)
	}
}

// При свертке часовых бакетов в дневные сохранять баллы, кол-во выборок и совпадения Token，пропорция не должна из-за Rollup дрейф.
func TestCreditSurvivesRollup(t *testing.T) {
	r := New("")
	old := time.Now().AddDate(0, 0, -100)
	r.Add(old, "cn", "u", "m", Delta{PromptTokens: 100, HasPromptTokens: true, TotalTokens: 100, HasTotal: true, Credit: 1.25, HasCredit: true, ModelRate: "0.5"}, true)
	r.Add(old.Add(2*time.Hour), "cn", "u", "m", Delta{PromptTokens: 300, HasPromptTokens: true, TotalTokens: 300, HasTotal: true, Credit: 3.75, HasCredit: true, ModelRate: "0.5"}, true)
	r.Rollup(time.Now())
	s := r.Snapshot(0, nil)
	if s.Totals.Credits != 5 || s.Totals.CreditSamples != 2 || s.Totals.CreditTokens != 400 || s.Totals.CreditsPer1MTokens != 12500 {
		t.Fatalf("rolled credit totals = %+v, want credits=5 tokens=400 ratio=12500", s.Totals)
	}
	if len(s.CreditByModel) != 1 || s.CreditByModel[0].Rate != "0.5" || s.CreditByModel[0].CreditsPer1MTokens != 12500 {
		t.Fatalf("rolled credit_by_model = %+v, want one rate-preserving row", s.CreditByModel)
	}
}

// по измерению модели“чистое имя модели + действующий коэффициент”Объединение; одинаковый множитель кросс-аккаунтно/Время объединяется, разные коэффициенты — в разные строки.
func TestCreditDimensionsRateGrouping(t *testing.T) {
	r := New("")
	now := time.Now()
	r.Add(now, "cn", "u1", "cn:glm-5.2", Delta{TotalTokens: 100, HasTotal: true, Credit: 1, HasCredit: true, ModelRate: "0.5"}, true)
	r.Add(now.Add(time.Hour), "cn", "u2", "glm-5.2", Delta{TotalTokens: 200, HasTotal: true, Credit: 2, HasCredit: true, ModelRate: "0.5"}, true)
	r.Add(now.Add(2*time.Hour), "cn", "u2", "glm-5.2", Delta{TotalTokens: 300, HasTotal: true, Credit: 6, HasCredit: true, ModelRate: "0.8"}, true)

	s := r.Snapshot(24, nil)
	if len(s.CreditByAccount) != 2 || len(s.CreditByModel) != 2 {
		t.Fatalf("dimensions accounts=%+v models=%+v, want 2 accounts and 2 model-rate rows", s.CreditByAccount, s.CreditByModel)
	}
	if s.CreditByModel[0].Key != "glm-5.2" || s.CreditByModel[0].Rate != "0.8" ||
		s.CreditByModel[0].Credits != 6 || s.CreditByModel[0].CreditTokens != 300 {
		t.Fatalf("first model row = %+v, want rate=0.8 credits=6 tokens=300", s.CreditByModel[0])
	}
	if s.CreditByModel[1].Rate != "0.5" || s.CreditByModel[1].Credits != 3 || s.CreditByModel[1].CreditTokens != 300 {
		t.Fatalf("merged model row = %+v, want rate=0.5 credits=3 tokens=300", s.CreditByModel[1])
	}
}

// При отсутствии множителя у старого бакета заполнить из текущего каталога и объединить с записями нового бакета с тем же множителем; при отсутствии каталога сохранить строки unknown.
func TestCreditLegacyRateFallback(t *testing.T) {
	r := New("")
	now := time.Now()
	r.Add(now, "cn", "u1", "glm-5.2", Delta{TotalTokens: 100, HasTotal: true, Credit: 1, HasCredit: true}, true)
	r.Add(now.Add(time.Hour), "cn", "u1", "glm-5.2", Delta{TotalTokens: 200, HasTotal: true, Credit: 2, HasCredit: true, ModelRate: "0.79"}, true)

	s := r.SnapshotWithRates(24, nil, func(realm, model string) string {
		if realm == "cn" && model == "glm-5.2" {
			return "0.79"
		}
		return ""
	})
	if len(s.CreditByModel) != 1 || s.CreditByModel[0].Rate != "0.79" ||
		s.CreditByModel[0].Credits != 3 || s.CreditByModel[0].CreditTokens != 300 {
		t.Fatalf("fallback model rows = %+v, want legacy merged into rate=0.79", s.CreditByModel)
	}
}

// Snapshot Фильтрация по полному окну: данные вне окна не попадают**любой**Агрегация (карточка/Таблица/тайминг),
// при переключении окна цифры меняются;hours=0 вся история.Buckets — количество бакетов, попавших в окно.
func TestSnapshotWindowFilter(t *testing.T) {
	r := New("")
	now := time.Now()
	r.Add(now.Add(-48*time.Hour), "cn", "u", "m", Delta{PromptTokens: 5, HasPromptTokens: true}, true) // Окно(24h)Внешний
	r.Add(now, "cn", "u", "m", Delta{PromptTokens: 3, HasPromptTokens: true}, true) // Внутри окна
	s := r.Snapshot(24, nil)
	if s.Totals.Requests != 1 || s.Totals.PromptTokens != 3 {
		t.Fatalf("24h Окно totals = %d/%d, want 1/3（48h Данные до должны быть отфильтрованы)", s.Totals.Requests, s.Totals.PromptTokens)
	}
	if len(s.Series) != 1 || s.Series[0].Scope != "hour" || s.Series[0].PromptTokens != 3 {
		t.Fatalf("series = %+v, want Только внутри окна 1 часовых точек", s.Series)
	}
	if s.Buckets != 1 {
		t.Fatalf("buckets = %d, want 1（кол-во попаданий в бакеты внутри окна)", s.Buckets)
	}

	all := r.Snapshot(0, nil)
	if all.Totals.Requests != 2 || all.Totals.PromptTokens != 8 {
		t.Fatalf("Вся история totals = %d/%d, want 2/8", all.Totals.Requests, all.Totals.PromptTokens)
	}
	// since Начало данных всей БД, не зависит от окна.
	if all.Since == "" || s.Since != all.Since {
		t.Fatalf("since должно быть началом всей БД и не меняться с окном: all=%q windowed=%q", all.Since, s.Since)
	}
}

// Явный интервал ("сегодня»/「кастом») и скользящее окно используют единый полноохватный фильтр; интервал — закрытый
// （Начало бакета попадает на [From, To] внутри — сразу хит), и From/To Будет отражено на панели для сверки метрики.
func TestSnapshotExplicitWindow(t *testing.T) {
	r := New("")
	base := time.Now().Truncate(time.Hour).Add(-5 * time.Hour)
	for i := 0; i < 6; i++ {
		r.Add(base.Add(time.Duration(i)*time.Hour), "cn", "u", "m",
			Delta{PromptTokens: 10, HasPromptTokens: true}, true)
	}
	// брать только средние два часа (base+2h、base+3h）。
	s := r.SnapshotWindow(Window{
		From: base.Add(2 * time.Hour),
		To: base.Add(3 * time.Hour),
	}, nil, nil)
	if s.Totals.Requests != 2 || s.Totals.PromptTokens != 20 {
		t.Fatalf("Явный интервал totals = %d/%d, want 2/20", s.Totals.Requests, s.Totals.PromptTokens)
	}
	if len(s.Series) != 2 || s.Buckets != 2 {
		t.Fatalf("Явный интервал series/buckets = %d/%d, want 2/2", len(s.Series), s.Buckets)
	}
	if s.WindowFrom == "" || s.WindowTo == "" {
		t.Fatalf("явный интервал должен отображаться обратно window_from/window_to: %+v", s)
	}
	if _, err := time.Parse(time.RFC3339, s.WindowFrom); err != nil {
		t.Fatalf("window_from не RFC3339: %q", s.WindowFrom)
	}

	// только From（「форма "сегодня»): от этой точки до актуального — полное совпадение.
	only := r.SnapshotWindow(Window{From: base.Add(4 * time.Hour)}, nil, nil)
	if only.Totals.Requests != 2 {
		t.Fatalf("Только From totals = %d, want 2", only.Totals.Requests)
	}
	if only.WindowFrom == "" || only.WindowTo != "" {
		t.Fatalf("Только From Время window_to Должно быть пусто: %+v", only)
	}

	// пустое окно (From/To Все нули и Hours<=0）= вся история, и Snapshot(0) эквивалентно.
	all := r.SnapshotWindow(Window{}, nil, nil)
	if all.Totals.Requests != 6 {
		t.Fatalf("окно полностью нулевое totals = %d, want 6（вся история)", all.Totals.Requests)
	}
	if all.WindowFrom != "" || all.WindowTo != "" {
		t.Fatalf("Вся история не должна отображать интервал: %+v", all)
	}
}

// Верхний предел скользящего окна всё ещё 60 дней, и не конфликтует с явным интервалом (From/To приоритет).
func TestWindowBounds(t *testing.T) {
	// From/To имеет приоритет над Hours。
	from := time.Now().Add(-2 * time.Hour)
	gotFrom, gotTo := Window{Hours: 720, From: from}.bounds()
	if !gotFrom.Equal(from) || !gotTo.IsZero() {
		t.Fatalf("From должен иметь приоритет над Hours: from=%v to=%v", gotFrom, gotTo)
	}
	// только Hours：начальная точка = От текущего целого часа назад Hours-1 час.
	f, to := Window{Hours: 24}.bounds()
	want := time.Now().Truncate(time.Hour).Add(-23 * time.Hour)
	if !f.Equal(want) || !to.IsZero() {
		t.Fatalf("24h bounds = %v/%v, want %v/нулевое значение", f, to, want)
	}
	// Hours<=0 и без From/To = вся история.
	if f, to := (Window{}).bounds(); !f.IsZero() || !to.IsZero() {
		t.Fatalf("Пустое окно bounds = %v/%v, want нулевое значение/нулевое значение", f, to)
	}
	// Верхний лимит 60 дн.
	f60, _ := Window{Hours: 100000}.bounds()
	want60 := time.Now().Truncate(time.Hour).Add(-(24*60 - 1) * time.Hour)
	if !f60.Equal(want60) {
		t.Fatalf("Превышение лимита Hours не зажато 60 день: %v want %v", f60, want60)
	}
}

// Грязный scope（ошибка парсинга) ни в одну метрику не попадает и не валит весь снапшот.
func TestBucketTimeRejectsGarbage(t *testing.T) {
	if _, ok := bucketTime("h:not-a-time"); ok {
		t.Fatal("Грязный час scope следует считать сбоем")
	}
	if _, ok := bucketTime("d:2026-13-45"); ok {
		t.Fatal("Грязный день scope следует считать сбоем")
	}
	if ts, ok := bucketTime("h:2026-09-30T13"); !ok || ts.Hour() != 13 {
		t.Fatalf("Валидные часы scope Ошибка парсинга: %v %v", ts, ok)
	}
	if ts, ok := bucketTime("d:2026-09-30"); !ok || ts.Day() != 30 {
		t.Fatalf("Валидный день scope Ошибка парсинга: %v %v", ts, ok)
	}
}

// Stop триггер финального сброса на диск (Start после, даже если интервал дебаунса не истек, тоже фиксировать).
func TestLifecycleFlush(t *testing.T) {
	path := filepath.Join(t.TempDir(), "usage.json")
	r := New(path)
	r.Start()
	r.Add(time.Now(), "cn", "u", "m", Delta{PromptTokens: 9, HasPromptTokens: true}, true)
	r.Stop()
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("Stop после должен быть файл на диске: %v", err)
	}
}
