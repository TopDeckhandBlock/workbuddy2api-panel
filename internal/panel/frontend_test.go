package panel

import (
	"bytes"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// TestAppJSSyntax app.js должен проходить JS синтаксическая проверка парсера.
//
// зачем нужно:app.js Да go:embed Статические ресурсы в бинарнике,Go компилятор не проверяет его содержимое —
// ключ литерала объекта без кавычек (Model_chat_GLM5.2 Парсится как обращение к свойству + числовой литерал)
// вызовет белый экран всей панели, а все Go тесты всё ещё зелёные. Тест переносит синтаксическую валидацию раньше в CI。
// отсутствует node Пропуск в окружении (не блокирует отсутствие Node сборочной машины).
func TestAppJSSyntax(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not available; skipping JS syntax check")
	}
	path, err := filepath.Abs("app.js")
	if err != nil {
		t.Fatal(err)
	}
	out, err := exec.Command(node, "--check", path).CombinedOutput()
	if err != nil {
		t.Fatalf("app.js syntax error:\n%s", out)
	}
}

// TestIndexHTMLNoInlineScript index.html не должен содержать inline <script> блок:
// Строго CSP（script-src 'self'）будет блокировать инлайн-скрипты, страница станет полностью недоступна.
// форма внешней ссылки <script src="..."> Разрешено.
func TestIndexHTMLNoInlineScript(t *testing.T) {
	p := newTestPanel()
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, httptest.NewRequest("GET", "/panel/", nil))
	body := rec.Body.String()

	rest := body
	for {
		idx := strings.Index(rest, "<script")
		if idx < 0 {
			break
		}
		rest = rest[idx:]
		end := strings.Index(rest, ">")
		if end < 0 {
			break
		}
		tag := rest[:end+1]
		if !strings.Contains(tag, "src=") {
			t.Fatalf("index.html contains inline <script> (blocked by CSP): %s", tag)
		}
		rest = rest[end:]
	}
}

// TestAppJSTopLevelSmoke app.js дым-тест вычисления верхнего уровня (v1.11.3/1.11.4 рантайм-шлюз, добавленный после двух подряд сбоев):
// node + DOM выполнение заглушки app.js（включая по hash попадающее в представления go() вызов верхнего уровня), перехватить TDZ/
// ReferenceError ошибка рантайма класса —Go Боковой frontend_test Не выполнять JS，проверка на уровне синтаксиса это полностью игнорирует.
// отсутствует node пропуск окружения (CI/легковесный движок не затрагивается);harness и app.js считается одинаковым (app.js Верхний уровень
// start() setInterval Приведёт к node Цикл событий не завершается, поэтому успешный путь явно exit(0)）。
func TestAppJSTopLevelSmoke(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed; JS smoke skipped")
	}
	harness := `const fs = require('fs');
const vm = require('vm');
const src = fs.readFileSync(process.argv[2], 'utf8');
const inert = new Proxy(function () {}, {
 get(t, k) { if (k === Symbol.toPrimitive) return () => ''; return inert; },
 set() { return true; },
 apply() { return inert; },
 construct() { return inert; },
 has() { return true; },
});
const sandbox = new Proxy({
 location: { hash: process.env.SMOKE_HASH || '#taskscenter' },
 history: { replaceState() {} },
 localStorage: { getItem: () => null, setItem() {} },
 navigator: { clipboard: { writeText: () => Promise.resolve() } },
 document: { querySelectorAll: () => [], querySelector: () => inert, getElementById: () => inert, addEventListener() {}, documentElement: inert, head: inert, body: inert, createElement: () => inert, cookie: '' },
 fetch: () => new Promise(() => {}),
 addEventListener() {}, removeEventListener() {},
 matchMedia: () => ({ matches: false, addEventListener() {} }),
 setInterval, clearInterval, setTimeout, clearTimeout,
 console, JSON, Math, Date, Number, String, Boolean, Object, Array, Promise, Map, Set, RegExp, Error, TypeError, isNaN, parseInt, parseFloat, encodeURIComponent, decodeURIComponent, URL, Symbol, Proxy, Reflect,
}, { get(t, k) { return t[k]; }, has() { return true; } });
sandbox.window = sandbox; sandbox.globalThis = sandbox;
vm.createContext(sandbox);
try {
 vm.runInContext(src, sandbox, { filename: 'app.js' });
 console.log('SMOKE OK');
 process.exit(0);
} catch (e) {
 console.log('SMOKE FAIL:', (e && e.stack ? e.stack : e).toString().split('\n').slice(0, 5).join('\n'));
 process.exit(1);
}
`
	hf, err := os.CreateTemp(t.TempDir(), "smoke-*.cjs")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := hf.WriteString(harness); err != nil {
		t.Fatal(err)
	}
	hf.Close()
	for _, hash := range []string{"#taskscenter", "#accounts", "#usage", "#models", "#config", "#logs", "#packages"} {
		cmd := exec.Command(node, hf.Name(), "app.js")
		cmd.Dir = "." // Рабочий каталог тестов = internal/panel
		cmd.Env = append(os.Environ(), "SMOKE_HASH="+hash)
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("app.js Вычисление на верхнем уровне %s Крэш: %v\n%s", hash, err, out)
		}
		if !bytes.Contains(out, []byte("SMOKE OK")) {
			t.Fatalf("app.js smoke %s не пройдено:\n%s", hash, out)
		}
	}
}

// Формат измерения списания баллов должен быть стабильным, и при отсутствии выборки/Нет совпадения Token нельзя подделать пропорцию.
func TestAppJSCreditDimensionFormatting(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed; credit formatting test skipped")
	}
	script := `const fs = require('fs');
const vm = require('vm');
const src = fs.readFileSync(process.argv[2], 'utf8');
	const start = src.indexOf('function trimFixed');
const end = src.indexOf('function usStat');
if (start < 0 || end < 0) throw new Error('credit helpers not found');
const ctx = { Number, String, RegExp };
vm.createContext(ctx);
vm.runInContext(
 src.slice(start, end) +
 '\nthis.fmtCredit=fmtCredit; this.fmtCreditRatio=fmtCreditRatio; this.fmtModelRate=fmtModelRate;',
 ctx
);
process.stdout.write(JSON.stringify({
 credit: ctx.fmtCredit(1.25),
 zero: ctx.fmtCredit(0),
 hundred: ctx.fmtCredit(100),
 ratio: ctx.fmtCreditRatio(12.5, 2, 400),
 noSamples: ctx.fmtCreditRatio(12.5, 0, 400),
 noTokens: ctx.fmtCreditRatio(12.5, 2, 0),
 rate: ctx.fmtModelRate('0.5'),
 noRate: ctx.fmtModelRate(''),
}));`
	f, err := os.CreateTemp(t.TempDir(), "credit-format-*.cjs")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(script); err != nil {
		t.Fatal(err)
	}
	f.Close()
	out, err := exec.Command(node, f.Name(), "app.js").CombinedOutput()
	if err != nil {
		t.Fatalf("credit formatting node test failed: %v\n%s", err, out)
	}
	const want = `{"credit":"1.25","zero":"0","hundred":"100","ratio":"12.5 / 1M","noSamples":"—","noTokens":"—","rate":"x0.5","noRate":"—"}`
	if strings.TrimSpace(string(out)) != want {
		t.Fatalf("credit formatting=%s want %s", out, want)
	}
}

// Время rate-limit модели должно поддерживать и апстрим reset_at、Шлюз until и без времени сброса — три формы.
func TestAppJSRateLimitMeta(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed; rate limit formatting test skipped")
	}
	script := `const fs = require('fs');
const vm = require('vm');
const src = fs.readFileSync(process.argv[2], 'utf8');
const start = src.indexOf('function dur(');
const end = src.indexOf('function rateLimitRowsHtml');
if (start < 0 || end < 0) throw new Error('rate limit helpers not found');
const ctx = { Date, Number, String, Math };
vm.createContext(ctx);
vm.runInContext(src.slice(start, end) + '\nthis.rateLimitMeta=rateLimitMeta;', ctx);
const now = new Date(2026, 8, 28, 14, 0, 0).getTime();
const reset = new Date(2026, 8, 28, 16, 0, 0).getTime();
const until = new Date(2026, 8, 28, 15, 0, 0).getTime();
const rate = ctx.rateLimitMeta({ model: 'glm-5.3', kind: 'rate_limit', reset_at: new Date(reset).toISOString(), until: new Date(until).toISOString() }, now);
const unavailable = ctx.rateLimitMeta({ model: 'missing', kind: 'model_unavailable', until: new Date(until).toISOString() }, now);
const unknown = ctx.rateLimitMeta({ model: 'glm-5.3', kind: 'rate_limit' }, now);
process.stdout.write(JSON.stringify({
 rate: rate.detail,
 unavailable: unavailable.detail,
 unknown: unknown.detail,
}));`
	f, err := os.CreateTemp(t.TempDir(), "rate-limit-format-*.cjs")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(script); err != nil {
		t.Fatal(err)
	}
	f.Close()
	out, err := exec.Command(node, f.Name(), "app.js").CombinedOutput()
	if err != nil {
		t.Fatalf("rate-limit formatting node test failed: %v\n%s", err, out)
	}
	const want = `{"rate":"ожидается 2026-09-28 16:00 разблокировка（остаток 2 ч 00 мин） · шлюз самый быстрый 1 ч 00 мин повторить после","unavailable":"ожидается 1 ч 00 мин повторить после","unknown":"ожидаемое время разблокировки неизвестно"}`
	if strings.TrimSpace(string(out)) != want {
		t.Fatalf("rate-limit formatting=%s want %s", out, want)
	}
}

// Строка лога запроса должна быть компактной, читаемой и содержать источник вызова (IP / UA）；при отсутствии источника — как — Фолбэк.
func TestAppJSRequestLogFormatting(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed; request log formatting test skipped")
	}
	script := `const fs = require('fs');
const vm = require('vm');
const src = fs.readFileSync(process.argv[2], 'utf8');
const escStart = src.indexOf('function esc(');
const escEnd = src.indexOf('function ago(');
const fmtStart = src.indexOf('function fmtTok(');
const fmtEnd = src.indexOf('function usStat(');
const reqStart = src.indexOf('function requestLogText');
const reqEnd = src.indexOf('function fmtBytes');
if ([escStart, escEnd, fmtStart, fmtEnd, reqStart, reqEnd].some(v => v < 0)) throw new Error('request log helpers not found');
const ctx = { Date, Number, String, Math, RegExp, isNaN };
vm.createContext(ctx);
vm.runInContext(
 src.slice(escStart, escEnd) + src.slice(fmtStart, fmtEnd) + src.slice(reqStart, reqEnd) +
 '\nthis.requestLogText=requestLogText;',
 ctx
);
const time = new Date(2026, 8, 28, 14, 5, 6).toISOString();
const good = { time, status: 200, outcome: 'success', model: 'glm-5.3', account: 'Аккаунт(uid8)', duration_ms: 1250, total_tokens: 2300, credit_known: true, credit: 0.12, request_id: 'req-1', client_ip: '203.0.113.7', user_agent: 'python-requests/2.31.0' };
const noSource = { ...good, request_id: 'req-3', client_ip: '', user_agent: '' };
process.stdout.write(JSON.stringify({
 good: ctx.requestLogText(good),
 noSource: ctx.requestLogText(noSource),
}));`
	f, err := os.CreateTemp(t.TempDir(), "request-log-format-*.cjs")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(script); err != nil {
		t.Fatal(err)
	}
	f.Close()
	out, err := exec.Command(node, f.Name(), "app.js").CombinedOutput()
	if err != nil {
		t.Fatalf("request log formatting node test failed: %v\n%s", err, out)
	}
	text := "14:05:06 | 200 Успех | glm-5.3 | Аккаунт(uid8) | 203.0.113.7 | python-requests/2.31.0 | 1.25s | 2.3k tok | 0.12 credit | req-1"
	noSource := "14:05:06 | 200 Успех | glm-5.3 | Аккаунт(uid8) | — | — | 1.25s | 2.3k tok | 0.12 credit | req-3"
	want := `{"good":` + strconv.Quote(text) + `,"noSource":` + strconv.Quote(noSource) + `}`
	if strings.TrimSpace(string(out)) != want {
		t.Fatalf("request log formatting=%s want %s", out, want)
	}
}

// Фильтрация записей запросов:IP / UA / Модель / Аккаунт / Запрос ID совпадение по вхождению (токенизация по пробелам AND）+ Точное совпадение результата.
func TestAppJSRequestMatch(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed; request filter test skipped")
	}
	script := `const fs = require('fs');
const vm = require('vm');
const src = fs.readFileSync(process.argv[2], 'utf8');
const start = src.indexOf('function reqMatch');
const end = src.indexOf('function reqOutcomeTag');
if (start < 0 || end < 0) throw new Error('reqMatch not found');
const ctx = {};
vm.createContext(ctx);
vm.runInContext(src.slice(start, end) + '\nthis.reqMatch=reqMatch;', ctx);
const base = { outcome: 'success', client_ip: '203.0.113.7', user_agent: 'python-requests/2.31.0', model: 'cn:glm-5.3', account: 'Пример(uid8)', request_id: 'req-1' };
const other = { outcome: 'http_error', client_ip: '198.51.100.4', user_agent: 'Mozilla/5.0 Chrome/120', model: 'global:hy3', account: 'Сторона А(uid9)', request_id: 'req-2' };
const rows = [base, other];
const pick = f => rows.filter(e => ctx.reqMatch(e, f)).map(e => e.request_id);
process.stdout.write(JSON.stringify({
 all: pick({ q: '', outcome: '' }),
 byIP: pick({ q: '203.0.113', outcome: '' }),
 byUA: pick({ q: 'chrome/120', outcome: '' }),
 byModel: pick({ q: 'glm', outcome: '' }),
 multiKw: pick({ q: 'glm success', outcome: '' }),
 multiMiss: pick({ q: 'glm chrome', outcome: '' }),
 byOutcome: pick({ q: '', outcome: 'http_error' }),
 combined: pick({ q: '198.51', outcome: 'http_error' }),
}));`
	f, err := os.CreateTemp(t.TempDir(), "request-filter-*.cjs")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(script); err != nil {
		t.Fatal(err)
	}
	f.Close()
	out, err := exec.Command(node, f.Name(), "app.js").CombinedOutput()
	if err != nil {
		t.Fatalf("request filter node test failed: %v\n%s", err, out)
	}
	// q Для outcome не участвует в матчинге (outcome есть отдельный дропдаун),multiKw внутри success Не попадает ни в одно поле.
	const want = `{"all":["req-1","req-2"],"byIP":["req-1"],"byUA":["req-2"],"byModel":["req-1"],"multiKw":[],"multiMiss":[],"byOutcome":["req-2"],"combined":["req-2"]}`
	if strings.TrimSpace(string(out)) != want {
		t.Fatalf("request filter=%s want %s", out, want)
	}
}

// запрос моделей по условию: домен / Возможность / Грейд / Цена / ключевые слова, а также множитель, контекст, сортировка вывода.
func TestAppJSModelFilter(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed; model filter test skipped")
	}
	script := `const fs = require('fs');
const vm = require('vm');
const src = fs.readFileSync(process.argv[2], 'utf8');
const start = src.indexOf('function mdRateValue');
const end = src.indexOf('function mdRowHtml');
if (start < 0 || end < 0) throw new Error('model filter helpers not found');
const ctx = { Number, String, Array, Object, isFinite, parseFloat };
vm.createContext(ctx);
vm.runInContext(src.slice(start, end) + '\nthis.mdMatch=mdMatch; this.mdSortList=mdSortList; this.mdRateValue=mdRateValue;', ctx);
const models = [
 { id: 'cn:glm-5.2', name: 'GLM-5.2', vendor: 'Zhipu', tags: ['Визуал'], supports_tool_call: true, supports_images: true, supports_reasoning: true, can_disable_thinking: true, supported_efforts: ['high', 'xhigh'], default_effort: 'high', is_default: false, credits: '0.79', promo_factor: 0.5, promo_credits: '0.40', promo_label: 'Ночная скидка', context_length: 1000000, max_output_tokens: 131000 },
 { id: 'cn:hy3', name: 'Hy3', supports_tool_call: true, supports_images: true, supports_reasoning: true, can_disable_thinking: false, supported_efforts: ['low', 'high'], default_effort: 'high', is_default: false, credits: '0', promo_factor: 0, promo_credits: '0', promo_label: 'Бесплатно ограниченное время', context_length: 192000, max_output_tokens: 64000 },
 { id: 'global:hy3', name: 'Hy3 Global', supports_tool_call: false, supports_images: false, supports_reasoning: false, supported_efforts: [], is_default: false, credits: '0.11', context_length: 1000000, max_output_tokens: 393000 },
 { id: 'cn:auto', name: 'Auto', supports_tool_call: true, supports_images: true, supports_reasoning: true, is_default: true, credits: null, context_length: 256000, max_output_tokens: 32000 },
];
const ids = list => list.map(m => m.id);
const filter = f => ids(ctx.mdSortList(models.filter(m => ctx.mdMatch(m, f)), f));
process.stdout.write(JSON.stringify({
 all: ids(models),
 realm: filter({ realm: 'cn' }),
 tool: filter({ cap: 'tool' }),
 vision: filter({ cap: 'vision' }),
 reasoning: filter({ cap: 'reasoning' }),
 isDefault: filter({ cap: 'default' }),
 effortOff: filter({ effort: 'off' }),
 effortLow: filter({ effort: 'low' }),
 free: filter({ promo: 'free' }),
 promo: filter({ promo: 'promo' }),
 discount: filter({ promo: 'discount' }),
 q: filter({ q: 'glm zhipu' }),
 qMiss: filter({ q: 'glm nosuch' }),
 sortRate: filter({ sort: 'rate' }),
 sortContext: filter({ sort: 'context' }),
 sortOutput: filter({ sort: 'output' }),
 sortName: filter({ sort: 'name' }),
 rateFree: ctx.mdRateValue(models[1]),
 rateMissing: ctx.mdRateValue(models[3]),
}));`
	f, err := os.CreateTemp(t.TempDir(), "model-filter-*.cjs")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(script); err != nil {
		t.Fatal(err)
	}
	f.Close()
	out, err := exec.Command(node, f.Name(), "app.js").CombinedOutput()
	if err != nil {
		t.Fatalf("model filter node test failed: %v\n%s", err, out)
	}
	const want = `{"all":["cn:glm-5.2","cn:hy3","global:hy3","cn:auto"],` +
		`"realm":["cn:glm-5.2","cn:hy3","cn:auto"],` +
		`"tool":["cn:glm-5.2","cn:hy3","cn:auto"],` +
		`"vision":["cn:glm-5.2","cn:hy3","cn:auto"],` +
		`"reasoning":["cn:glm-5.2","cn:hy3","cn:auto"],` +
		`"isDefault":["cn:auto"],` +
		`"effortOff":["cn:glm-5.2"],` +
		`"effortLow":["cn:hy3"],` +
		`"free":["cn:hy3"],` +
		`"promo":["cn:glm-5.2","cn:hy3"],` +
		`"discount":["cn:glm-5.2"],` +
		`"q":["cn:glm-5.2"],` +
		`"qMiss":[],` +
		`"sortRate":["cn:hy3","global:hy3","cn:glm-5.2","cn:auto"],` +
		`"sortContext":["cn:glm-5.2","global:hy3","cn:auto","cn:hy3"],` +
		`"sortOutput":["global:hy3","cn:glm-5.2","cn:hy3","cn:auto"],` +
		`"sortName":["cn:auto","cn:glm-5.2","cn:hy3","global:hy3"],` +
		`"rateFree":0,"rateMissing":null}`
	if strings.TrimSpace(string(out)) != want {
		t.Fatalf("model filter=%s\nwant %s", out, want)
	}
}

// Имя класса столбцов графика использования по времени не должно называться bar：Полоска баллов пула аккаунтов — это .bar{height:3px}，И SVG2 внутри
// height Да rect CSS геометрия — одноименный класс сплющит каждый столбец в 3px высоко, график выглядит"нет данных"。
// Этот баг виден только в браузере, поэтому здесь фиксируем имя класса.
func TestAppJSUsageChartBarClass(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed; usage chart test skipped")
	}
	script := `const fs = require('fs');
const vm = require('vm');
const src = fs.readFileSync(process.argv[2], 'utf8');
const start = src.indexOf('function parsePointTime');
const end = src.indexOf('function fmtTokTip');
const escStart = src.indexOf('function esc(');
const escEnd = src.indexOf('function ago(');
const fmtStart = src.indexOf('function fmtTok(');
const fmtEnd = src.indexOf('function usStat(');
if ([start, end, escStart, escEnd, fmtStart, fmtEnd].some(v => v < 0)) throw new Error('usage chart helpers not found');
const host = { innerHTML: '', textContent: '' };
const ctx = {
 Date, Number, String, Math, RegExp, isNaN, Set, Array, Object, Infinity,
 document: { getElementById: () => host },
 $: () => host,
};
vm.createContext(ctx);
vm.runInContext(src.slice(escStart, escEnd) + src.slice(fmtStart, fmtEnd) + src.slice(start, end) +
 '\nthis.renderUsageChart=renderUsageChart;', ctx);
const series = [
 { t: '2026-09-30T09', scope: 'hour', prompt_tokens: 35, completion_tokens: 16, total_tokens: 51, requests: 1 },
 { t: '2026-09-30T11', scope: 'hour', prompt_tokens: 978324, completion_tokens: 20621, total_tokens: 998945, requests: 39 },
 { t: '2026-09-30T13', scope: 'hour', prompt_tokens: 27400952, completion_tokens: 104913, total_tokens: 27505865, requests: 200 },
];
ctx.renderUsageChart(series);
const svg = host.innerHTML;
process.stdout.write(JSON.stringify({
 hasUsbar: svg.includes('class="usbar"'),
 hasBareBar: /class="bar"/.test(svg),
 hasGradient: svg.includes('usGradP') && svg.includes('usGradC'),
 barCount: (svg.match(/class="usbar"/g) || []).length,
 hasPeak: svg.includes('Пик'),
 hasAvg: svg.includes('среднее'),
 emptyState: (function () { ctx.renderUsageChart([]); return host.innerHTML.includes('us-empty'); })(),
}));`
	f, err := os.CreateTemp(t.TempDir(), "usage-chart-*.cjs")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(script); err != nil {
		t.Fatal(err)
	}
	f.Close()
	out, err := exec.Command(node, f.Name(), "app.js").CombinedOutput()
	if err != nil {
		t.Fatalf("usage chart node test failed: %v\n%s", err, out)
	}
	const want = `{"hasUsbar":true,"hasBareBar":false,"hasGradient":true,"barCount":6,"hasPeak":true,"hasAvg":true,"emptyState":true}`
	if strings.TrimSpace(string(out)) != want {
		t.Fatalf("usage chart=%s\nwant %s", out, want)
	}
}

// Контрол диапазона времени: пресеты → Маппинг параметров запроса. Ключевое:
// - 「«сегодня» должен отправляться в часовом поясе браузера 00:00（часовой пояс сервера может не совпадать), и без to；
// - Пресет прокрутки rolling=true Отправка hours（выравнивание по ровному часу на сервере),rolling=false Пересчитать в from；
// - 「«Вся история» — оба не отправляются; "Кастом» — отправляется выбранное пользователем from/to。
func TestAppJSTimeRangeQuery(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed; time range test skipped")
	}
	script := `const fs = require('fs');
const vm = require('vm');
const src = fs.readFileSync(process.argv[2], 'utf8');
const start = src.indexOf('const TRANGE_PRESETS');
const end = src.indexOf('function rateLimitMeta');
if (start < 0 || end < 0 || end < start) throw new Error('trange helpers not found');
const host = { innerHTML: '' };
const ctx = {
 Date, Number, String, Math, Map, Array, Object, isNaN, URLSearchParams,
 document: { getElementById: () => host },
 $: () => host,
 esc: s => String(s == null ? '' : s),
};
vm.createContext(ctx);
vm.runInContext(src.slice(start, end) +
 '\nthis.trangeState=trangeState; this.trangeQuery=trangeQuery; this.trangeLabel=trangeLabel; this.trangeMidnight=trangeMidnight;', ctx);
const q = (preset, rolling) => {
 ctx.trangeState('t').preset = preset;
 return ctx.trangeQuery('t', rolling).toString();
};
const secOf = d => String(Math.floor(d.getTime() / 1000));
const approx = (qs, wantSec) => {
 const m = /(?:^|&)from=(\d+)/.exec(qs);
 return m && Math.abs(Number(m[1]) - wantSec) < 120;
};
const now = Date.now();
const todayQ = q('today', true);
process.stdout.write(JSON.stringify({
 todayIsMidnight: todayQ === 'from=' + secOf(ctx.trangeMidnight()),
 todayNoTo: !/to=/.test(todayQ),
 rolling24: q('24', true),
 rolling72: q('72', true),
 rolling0: q('0', true),
 log24From: approx(q('24', false), Math.floor((now - 24 * 3600e3) / 1000)),
 log24HasHours: /hours=/.test(q('24', false)),
 log7dFrom: approx(q('168', false), Math.floor((now - 168 * 3600e3) / 1000)),
 custom: (function () {
 const st = ctx.trangeState('t');
 st.preset = 'custom';
 st.from = new Date(2026, 8, 30, 9, 0, 0);
 st.to = new Date(2026, 8, 30, 18, 30, 0);
 return ctx.trangeQuery('t', true).toString();
 })(),
 labelCustom: ctx.trangeLabel('t'),
 labelToday: (function () { ctx.trangeState('t').preset = 'today'; return ctx.trangeLabel('t'); })(),
}));`
	f, err := os.CreateTemp(t.TempDir(), "trange-*.cjs")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(script); err != nil {
		t.Fatal(err)
	}
	f.Close()
	out, err := exec.Command(node, f.Name(), "app.js").CombinedOutput()
	if err != nil {
		t.Fatalf("time range node test failed: %v\n%s", err, out)
	}
	local := func(h, m int) string {
		return strconv.FormatInt(time.Date(2026, 9, 30, h, m, 0, 0, time.Local).Unix(), 10)
	}
	want := `{"todayIsMidnight":true,"todayNoTo":true,` +
		`"rolling24":"hours=24","rolling72":"hours=72","rolling0":"",` +
		`"log24From":true,"log24HasHours":false,"log7dFrom":true,` +
		`"custom":"from=` + local(9, 0) + `&to=` + local(18, 30) + `",` +
		`"labelCustom":"9-30 09:00 → 9-30 18:30","labelToday":"Сегодня"}`
	if strings.TrimSpace(string(out)) != want {
		t.Fatalf("time range=%s\nwant %s", out, want)
	}
}

// При одинаковом сроке — по номиналу по убыванию; остальные неизрасходованные пакеты и ноль/Пакеты с отрицательным балансом агрегируются отдельно.
func TestAppJSDetailGroups(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed; detail groups test skipped")
	}
	script := `const fs = require('fs');
const vm = require('vm');
const src = fs.readFileSync(process.argv[2], 'utf8');
const start = src.indexOf('const PK_DEFAULT_DETAIL_LIMIT');
const end = src.indexOf('function renderPackages');
if (start < 0 || end < 0) throw new Error('detail group functions not found');
const ctx = { Date, Math, Number, String, Map, Array, Object, isFinite };
vm.createContext(ctx);
vm.runInContext(src.slice(start, end) + '\nthis.pkDetailGroups = pkDetailGroups; this.pkDetailLimit = pkDetailLimit;', ctx);
const input = [
 { id: 'small-late', size: 100, remain: 1, expires_at: 400 },
 { id: 'zero-early-b', size: 200, remain: 0, expires_at: 200 },
 { id: 'small-early', size: 100, remain: 2, expires_at: 200 },
 { id: 'large-unknown', size: 300, remain: 3, end_time: '' },
 { id: 'zero-early-a', size: 200, remain: -1, expires_at: 200 },
 { id: 'small-unknown', size: 100, remain: 1, end_time: '' },
 { id: 'large-early', size: 300, remain: 4, expires_at: 200 },
 { id: 'zero-late', size: 300, remain: 0, expires_at: 300 },
];
const before = input.map(p => p.id).join(',');
const out = ctx.pkDetailGroups(input, 2);
process.stdout.write(JSON.stringify({
 visible: out.visible.map(p => p.id),
 rest: out.rest.map(p => p.id),
 used: out.used.map(p => p.id),
 restSize: out.restSize,
 restRemain: out.restRemain,
 usedSize: out.usedSize,
 defaultLimit: ctx.pkDetailLimit({}),
 configuredLimit: ctx.pkDetailLimit({ panel: { package_detail_limit: 7 } }),
 unchanged: input.map(p => p.id).join(',') === before,
}));`
	f, err := os.CreateTemp(t.TempDir(), "detail-groups-*.cjs")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(script); err != nil {
		t.Fatal(err)
	}
	f.Close()
	out, err := exec.Command(node, f.Name(), "app.js").CombinedOutput()
	if err != nil {
		t.Fatalf("detail groups node test failed: %v\n%s", err, out)
	}
	const want = `{"visible":["large-early","small-early"],"rest":["small-late","large-unknown","small-unknown"],"used":["zero-early-b","zero-early-a","zero-late"],"restSize":500,"restRemain":5,"usedSize":700,"defaultLimit":5,"configuredLimit":7,"unchanged":true}`
	if strings.TrimSpace(string(out)) != want {
		t.Fatalf("detail groups=%s want %s", out, want)
	}
}

// точная агрегация оставшихся дней, clamp по общему балансу внутри аккаунта, партии без срока не попадают в график.
func TestAppJSExpirySummary(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed; expiry summary test skipped")
	}
	script := `const fs = require('fs');
const vm = require('vm');
const src = fs.readFileSync(process.argv[2], 'utf8');
const start = src.indexOf('const PK_ACCOUNT_COLORS');
const end = src.indexOf('function renderExpiryDistribution');
if (start < 0 || end < 0) throw new Error('expiry summary functions not found');
const ctx = { Date, Math, Number, String, Map, Array, Object, isFinite };
vm.createContext(ctx);
vm.runInContext(src.slice(start, end) + '\nthis.summarizeCreditDays = summarizeCreditDays; this.pkAccountColorMap = pkAccountColorMap;', ctx);
const day = 86400000, now = 100000;
const out = ctx.summarizeCreditDays([
 { uid: 'a', remain: 100, packages: [
 { name: 'soon-a', remain: 30, expires_at: now + day },
 { name: 'later', remain: 70, expires_at: now + 7 * day },
 ] },
 { uid: 'b', remain: 55, packages: [
 { name: 'soon-b', remain: 20, expires_at: now + day },
 { name: 'unknown', remain: 5, end_time: '' },
 ] },
 { uid: 'err', error: 'offline' },
], now);
process.stdout.write(JSON.stringify({
 rows: out.rows.map(row => ({ days: row.days, credits: row.credits })),
 accountCount: out.accountCount,
 unavailable: out.unavailable,
 colorA: ctx.pkAccountColorMap([{ uid: 'b' }, { uid: 'a' }]).get('a'),
 colorB: ctx.pkAccountColorMap([{ uid: 'a' }, { uid: 'b' }]).get('b'),
}));`
	f, err := os.CreateTemp(t.TempDir(), "expiry-*.cjs")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(script); err != nil {
		t.Fatal(err)
	}
	f.Close()
	out, err := exec.Command(node, f.Name(), "app.js").CombinedOutput()
	if err != nil {
		t.Fatalf("expiry summary node test failed: %v\n%s", err, out)
	}
	const want = `{"rows":[{"days":1,"credits":50},{"days":7,"credits":70}],"accountCount":3,"unavailable":1,"colorA":"#4f8cff","colorB":"#25b08b"}`
	if strings.TrimSpace(string(out)) != want {
		t.Fatalf("expiry summary=%s want %s", out, want)
	}
}
