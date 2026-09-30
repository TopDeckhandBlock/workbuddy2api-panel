'use strict';
/* ── Статус ─────────────────────────────────────────────────────────── */
const LS_KEY = 'wb2api.key', LS_THEME = 'wb2api.theme';
let theme = localStorage.getItem(LS_THEME) || 'auto'; // auto | light | dark
let view = 'accounts';
let overviewData = null, cfgLoaded = null;
let logPin = true, loginState = null, loginTimer = null;
let refTimer = null;
/* состояние фильтра уровня представления (объявление уровня модуля вверху файла, избегать верхнего уровня go() Срабатывает при выполнении раньше заявленного времени TDZ）。 */
let mdFilter = { q: '', realm: '', cap: '', effort: '', promo: '', sort: 'default' };
let mdAll = [], mdProbes = {}, mdProbeOf = () => undefined;
let reqFilter = { q: '', outcome: '' };
let reqEntries = [];
let usDim = 'account', usCreditDim = 'account', usSort = 'total';
let usageData = null;
let reqRangeState = null; // Временной диапазон логов запросов (на странице использования см. trangeState）

const $ = id => document.getElementById(id);

/* ── тема ─────────────────────────────────────────────────────────── */
/* Переключение двух состояний (легкое/тёмная), при первом визите — по системным настройкам; клик всегда переключает видимый внешний вид, интуитивно. */
function effTheme() {
 return theme === 'auto' ? (matchMedia('(prefers-color-scheme: light)').matches ? 'light' : 'dark') : theme;
}
function applyTheme() {
 const eff = effTheme();
 document.documentElement.dataset.theme = eff;
 $('icoTheme').innerHTML = eff === 'light'
 ? '<circle cx="8" cy="8" r="3"/><path d="M8 1v2M8 13v2M1 8h2M13 8h2M3.2 3.2l1.4 1.4M11.4 11.4l1.4 1.4M12.8 3.2l-1.4 1.4M4.6 11.4l-1.4 1.4"/>'
 : '<path d="M13.2 9.6A5.6 5.6 0 0 1 6.4 2.8a5.6 5.6 0 1 0 6.8 6.8z"/>';
 $('btnTheme').title = eff === 'light' ? 'Переключить на тёмную тему' : 'Переключить на светлую тему';
}
addEventListener('change', applyTheme);
$('btnTheme').onclick = () => {
 theme = effTheme() === 'light' ? 'dark' : 'light';
 localStorage.setItem(LS_THEME, theme);
 applyTheme();
};
applyTheme();

/* ── Запрос ─────────────────────────────────────────────────────────── */
async function api(path, opts = {}) {
 const h = Object.assign({}, opts.headers || {});
 const k = localStorage.getItem(LS_KEY);
 if (k) h['Authorization'] = 'Bearer ' + k;
 if (opts.body) h['Content-Type'] = 'application/json';
 const r = await fetch('/panel/api/' + path, Object.assign({}, opts, { headers: h }));
 if (r.status === 401) { openKey(); throw new Error('ключ недействителен или не заполнен'); }
 const d = await r.json().catch(() => ({}));
 if (!r.ok) throw new Error(d.error || ('HTTP ' + r.status));
 return d;
}
function toast(msg, cls) {
 const el = document.createElement('div');
 el.className = 'tst ' + (cls || '');
 el.textContent = msg;
 $('toasts').appendChild(el);
 setTimeout(() => el.remove(), 3600);
}
// esc текст/Двойное безопасное экранирование атрибутов. Нельзя только div.innerHTML（Оно экранирует <>& но без экранирования кавычек),
// Иначе конкатенация строки в HTML Атрибут (напр. title="uid: ..."）кавычка может закрыть атрибут и инжектировать обработчик события.
// Явная замена 5 символов:& < > " '（& должен идти первым, чтобы избежать двойного экранирования).
function esc(s) {
 return String(s == null ? '' : s)
 .replace(/&/g, '&amp;')
 .replace(/</g, '&lt;')
 .replace(/>/g, '&gt;')
 .replace(/"/g, '&quot;')
 .replace(/'/g, '&#39;');
}
function ago(iso) {
 if (!iso || iso.startsWith('0001-')) return '—';
 const s = (Date.now() - new Date(iso)) / 1000;
 if (s < 0) return 'только что';
 if (s < 60) return Math.floor(s) + ' сек. назад';
 if (s < 3600) return Math.floor(s / 60) + ' минут назад';
 if (s < 86400) return Math.floor(s / 3600) + ' часов назад';
 return Math.floor(s / 86400) + ' дней назад';
}
function dur(sec) {
 sec = Math.max(0, Math.round(sec));
 const h = Math.floor(sec / 3600), m = Math.floor(sec % 3600 / 60), s = sec % 60;
 return h ? h + ' ч ' + String(m).padStart(2, '0') + ' мин' : m ? m + ' мин ' + String(s).padStart(2, '0') + ' с' : s + ' с';
}
function parseAPITime(value) {
 const text = String(value || '');
 if (!text || text.startsWith('0001-')) return 0;
 const ms = Date.parse(text);
 return Number.isFinite(ms) ? ms : 0;
}
function fmtLocalDateTime(ms) {
 const d = new Date(ms);
 const p = n => String(n).padStart(2, '0');
 return d.getFullYear() + '-' + p(d.getMonth() + 1) + '-' + p(d.getDate()) + ' ' +
 p(d.getHours()) + ':' + p(d.getMinutes());
}

/* ── Элемент выбора диапазона времени (расход / совместное использование журнала запросов)────────────────────────────────
 пресет: сегодня / Близко 24 ч / Близко 3 день / Близко 7 день / Близко 30 день / Вся история / Кастом.

 Почему интервалы всегда считает фронтенд:
 - 「«сегодня» должен быть**Локальный часовой пояс браузера** 00:00 начиная с. Часовой пояс сервера может не совпадать с браузером
 （Контейнер часто зависает TZ=Asia/Shanghai，а браузер может быть в любом часовом поясе), считает сервер"Сегодня"
 при кросс-таймзоне срежет не тот день.
 - 「«кастом» — это конкретный момент, выбранный пользователем, без вывода на сервере.

 Скользящая предустановка (последн. N ч/дней) — сохранить hours параметры: скользящее окно сервера, выровненное по часу, и старое
 поведение побитово идентично, фронтенд сам вычитает N за час будет переучтено/Не учтен один граничный бакет. */
const TRANGE_PRESETS = [
 ['today', 'Сегодня'],
 ['24', 'Близко 24 ч'],
 ['72', 'Близко 3 день'],
 ['168', 'Близко 7 день'],
 ['720', 'Близко 30 день'],
 ['0', 'Вся история'],
 ['custom', 'Пользовательские…'],
];
const TRANGE_DEFAULT = '72';
const trangeStates = new Map(); // hostId → { preset, from: Date|null, to: Date|null }

// dtLocalValue / dtLocalParse и <input type=datetime-local> Конвертация форматов значений
// （YYYY-MM-DDTHH:mm，Локальный часовой пояс;ES внутри"Строка даты со временем"по локальному парсингу, то что нужно).
function dtLocalValue(d) {
 const p = n => String(n).padStart(2, '0');
 return d.getFullYear() + '-' + p(d.getMonth() + 1) + '-' + p(d.getDate()) + 'T' +
 p(d.getHours()) + ':' + p(d.getMinutes());
}
function dtLocalParse(s) {
 if (!s) return null;
 const d = new Date(s);
 return isNaN(d.getTime()) ? null : d;
}

// trangeMidnight Сегодня 00:00（локальный часовой пояс).
function trangeMidnight() {
 const d = new Date();
 d.setHours(0, 0, 0, 0);
 return d;
}

function trangeState(id) {
 if (!trangeStates.has(id)) {
 // 「кастом» дать осмысленное значение по умолчанию: сегодня 00:00 → Сейчас.
 trangeStates.set(id, { preset: TRANGE_DEFAULT, from: trangeMidnight(), to: new Date() });
 }
 return trangeStates.get(id);
}

// trangeRender Отрисовать каркас контрола (идемпотентно: повторный вызов сохраняет текущее состояние).
function trangeRender(id) {
 const host = $(id);
 if (!host) return;
 const st = trangeState(id);
 const custom = st.preset === 'custom';
 host.innerHTML =
 '<select class="tr-preset" aria-label="Временной диапазон">' +
 TRANGE_PRESETS.map(([v, label]) =>
 '<option value="' + v + '"' + (v === st.preset ? ' selected' : '') + '>' + esc(label) + '</option>').join('') +
 '</select>' +
 '<span class="tr-custom"' + (custom ? '' : ' hidden') + '>' +
 '<input type="datetime-local" class="tr-from" value="' + esc(st.from ? dtLocalValue(st.from) : '') + '" aria-label="Время начала">' +
 '<span class="tr-sep">→</span>' +
 '<input type="datetime-local" class="tr-to" value="' + esc(st.to ? dtLocalValue(st.to) : '') + '" aria-label="время окончания">' +
 '</span>';
 const preset = host.querySelector('.tr-preset');
 if (preset) preset.onchange = () => {
 st.preset = preset.value;
 // при переключении с другого пресета на кастом сбросить интервал на"Сегодня 00:00 → сейчас"，
 // чтобы полугодовой интервал, оставленный пользователем в прошлый раз, не применялся молча.
 if (st.preset === 'custom' && (!st.from || !st.to)) { st.from = trangeMidnight(); st.to = new Date(); }
 trangeRender(id);
 trangeEmit(id);
 };
 const fromEl = host.querySelector('.tr-from');
 const toEl = host.querySelector('.tr-to');
 const readCustom = () => {
 st.from = dtLocalParse(fromEl.value);
 st.to = dtLocalParse(toEl.value);
 // инверсия периода — подсветить красным на месте (без тихой автокоррекции: пользователь может еще вводить).
 const bad = st.from && st.to && st.from > st.to;
 fromEl.classList.toggle('tr-bad', !!bad);
 toEl.classList.toggle('tr-bad', !!bad);
 if (bad) return;
 trangeEmit(id);
 };
 if (fromEl) fromEl.onchange = readCustom;
 if (toEl) toEl.onchange = readCustom;
}

const trangeHandlers = new Map();
// trangeBind отрисовать контрол и зарегистрировать колбэк изменения.**Не**Триггерить колбэк при биндинге: первая загрузка каждого view
// От go() Единый драйвер, повторный триггер здесь приведет к двойному вызову API при открытии страницы.
function trangeBind(id, onChange, preset) {
 trangeHandlers.set(id, onChange);
 if (preset) trangeState(id).preset = preset;
 trangeRender(id);
}
function trangeEmit(id) {
 const fn = trangeHandlers.get(id);
 if (fn) fn();
}

// trangeQuery Преобразовать текущий выбор в query-параметры.
// rolling=true → прокрутка пресетов отправки hours（выравнивание по целому часу на сервере), сегодня/Кастомная отправка from/to
// rolling=false → отправлять всегда from/to（архив — линейный лог, фронтенду проще считать интервал)
// 「«вся история» — оба не отправляются.
function trangeQuery(id, rolling) {
 const st = trangeState(id);
 const q = new URLSearchParams();
 const sec = d => Math.floor(d.getTime() / 1000);
 if (st.preset === 'custom') {
 if (st.from) q.set('from', sec(st.from));
 if (st.to) q.set('to', sec(st.to));
 return q;
 }
 if (st.preset === 'today') {
 q.set('from', sec(trangeMidnight()));
 return q;
 }
 if (st.preset === '0') return q;
 if (rolling) { q.set('hours', st.preset); return q; }
 q.set('from', sec(new Date(Date.now() - Number(st.preset) * 3600 * 1000)));
 return q;
}

// trangeLabel Человекочитаемый формат, для отображения интервала в местах вроде правого верхнего угла «Обзора использования».
function trangeLabel(id) {
 const st = trangeState(id);
 const found = TRANGE_PRESETS.find(p => p[0] === st.preset);
 if (st.preset !== 'custom') return found ? found[1] : '';
 if (!st.from && !st.to) return 'Пользовательский';
 const f = d => d ? (d.getMonth() + 1) + '-' + String(d.getDate()).padStart(2, '0') + ' ' +
 String(d.getHours()).padStart(2, '0') + ':' + String(d.getMinutes()).padStart(2, '0') : '…';
 return f(st.from) + ' → ' + f(st.to);
}
function rateLimitMeta(row, now) {
 const model = String(row && row.model || 'Неизвестная модель');
 const kind = String(row && row.kind || 'rate_limit');
 const resetAt = parseAPITime(row && row.reset_at);
 const until = parseAPITime(row && row.until);
 const deadline = resetAt || until;
 const remaining = deadline > now ? Math.round((deadline - now) / 1000) : 0;
 if (kind === 'model_unavailable') {
 return {
 model,
 kind,
 detail: remaining ? 'ожидается ' + dur(remaining) + ' повторить после' : 'ожидания повторного пробинга',
 title: model + '\nМодель сейчас недоступна' + (deadline ? '\nсамая ранняя повторная попытка:' + fmtLocalDateTime(deadline) : ''),
 };
 }
 let detail = resetAt
 ? 'ожидается ' + fmtLocalDateTime(resetAt) + ' разблокировка' + (remaining ? '（остаток ' + dur(remaining) + '）' : '')
 : (until ? 'ожидается ' + fmtLocalDateTime(until) + ' Восстановление (остаток ' + dur(remaining) + '）' : 'ожидаемое время разблокировки неизвестно');
 const title = [model, resetAt ? 'сброс апстрима:' + fmtLocalDateTime(resetAt) : 'Сброс апстрима: время неизвестно'];
 if (until && resetAt && until < resetAt) {
 detail += ' · шлюз самый быстрый ' + dur(Math.max(0, Math.round((until - now) / 1000))) + ' повторить после';
 title.push('самая ранняя попытка повтора шлюза:' + fmtLocalDateTime(until));
 }
 return { model, kind, detail, title: title.join('\n') };
}
function rateLimitRowsHtml(rows, now) {
 const list = Array.isArray(rows) ? rows.filter(row => row && row.model) : [];
 if (!list.length) return '';
 return '<div class="rate-limits">' + list.map(row => {
 const m = rateLimitMeta(row, now);
 return '<div class="rate-limit ' + (m.kind === 'model_unavailable' ? 'model-unavailable' : '') +
 '" title="' + esc(m.title) + '"><b>' + esc(m.model) + '</b><span>' + esc(m.detail) + '</span></div>';
 }).join('') + '</div>';
}

function formatTokenCount(tokens) {
 if (tokens == null || tokens === '') return '—';
 const n = Number(tokens);
 if (!Number.isFinite(n) || n < 0) return '—';
 if (n < 1000) return String(Math.round(n));
 const units = [['k', 1e3], ['m', 1e6], ['b', 1e9]];
 let unit = units[0];
 for (const candidate of units) {
 if (n >= candidate[1]) unit = candidate;
 }
 let value = n / unit[1];
 let rounded = Number(value.toFixed(1));
 // 999999 → 1m，а не 1000k；после округления — автоповышение единицы.
 const next = units[units.indexOf(unit) + 1];
 if (next && rounded >= 1000) {
 unit = next;
 value = n / unit[1];
 rounded = Number(value.toFixed(1));
 }
 return rounded + unit[0];
}

function formatLatency(ms) {
 if (ms == null || ms === '') return '—';
 const n = Number(ms);
 if (!Number.isFinite(n) || n <= 0) return '—';
 return n < 1000 ? Math.round(n) + 'ms' : (n / 1000).toFixed(1).replace(/\.0$/, '') + 's';
}
function formatRate(rate) {
 if (rate == null || rate === '') return '—';
 const n = Number(rate);
 if (!Number.isFinite(n) || n < 0) return '—';
 return n.toFixed(1) + 'tok/s';
}

/* ── Шлюз ключей ───────────────────────────────────────────────────────── */
function openKey() { $('keyVeil').classList.add('on'); setTimeout(() => $('keyInput').focus(), 60); }
$('btnKey').onclick = async () => {
 const v = $('keyInput').value.trim();
 if (!v) return;
 localStorage.setItem(LS_KEY, v);
 try {
 await api('overview');
 $('keyErr').hidden = true;
 $('keyVeil').classList.remove('on');
 start();
 } catch (e) { $('keyErr').hidden = false; }
};
$('keyInput').addEventListener('keydown', e => { if (e.key === 'Enter') $('btnKey').click(); });

/* ── маршрутизация ─────────────────────────────────────────────────────────── */
const TITLES = { accounts: 'Пул аккаунтов', usage: 'Расход', packages: 'состав баллов', taskscenter: 'Центр задач', models: 'модель и градация', config: 'Конфигурация', logs: 'Журнал выполнения' };
function go(v) {
 view = v;
 document.querySelectorAll('.view').forEach(s => s.hidden = s.id !== 'view-' + v);
 document.querySelectorAll('.nav a').forEach(a => a.classList.toggle('on', a.dataset.view === v));
 $('ttl').textContent = TITLES[v];
 if (v === 'models' && !$('mdBody').children.length) loadModels();
 if (v === 'config') loadConfig();
 if (v === 'logs') loadLogs();
 if (v === 'usage') loadUsage();
 if (v === 'packages') loadPackages();
 if (v === 'taskscenter') reattachQueueView();
}
document.querySelectorAll('.nav a').forEach(a => a.onclick = e => { e.preventDefault(); go(a.dataset.view); history.replaceState(null, '', '#' + a.dataset.view); });
/* При первом входе отложить до вычисления скрипта текущего раунда go()。
 Причина:go() синхронно триггерит загрузку данных представления (loadUsage/loadLogs/loadPackages…），А эти
 модульный уровень, читаемый функцией let/const（usageRateWarmAt、PK_* и т.д.) инициализируются только во второй половине файла —
 Прямая глубокая связь #usage / #packages При открытии страницы сработает TDZ（"Cannot access 'x' before
 initialization"），Проявляется как вечное отображение этой страницы"ошибка чтения"，при этом переход по навигации работает нормально.
 Задержка 0ms дать всему скрипту сначала полностью вычислиться — самый простой и надёжный способ исправить этот класс проблем. */
setTimeout(() => {
 const hash = (location.hash || '#accounts').slice(1);
 go(hash in TITLES ? hash : 'accounts');
}, 0);

/* ── Пул аккаунтов ───────────────────────────────────────────────────────── */
function renderAccounts(list) {
 const tb = $('accBody');
 if (!list.length) {
 tb.innerHTML = '<tr><td colspan="9"><div class="empty"><div class="big">Пул аккаунтов пуст</div>Нажмите в правом верхнем углу «Добавить аккаунт», войдите через браузер WorkBuddy Аккаунт</div></td></tr>';
 return;
 }
 // Есть общий лимит (credits_total）→ прогресс-бар по собственному остаток/Общая сумма проценты; в старых данных нет итога → Возврат в пул с наивысшим=100%
 const maxCred = Math.max(1, ...list.map(s => s.credits || 0));
 tb.innerHTML = list.map(s => {
 const bl = (new Date(s.breaker_until || 0) - Date.now()) / 1000;
 const dg = (new Date(s.degrade_until || 0) - Date.now()) / 1000;
 const cool = Math.max(s.cool_remaining_sec || 0, bl > 0 ? bl : 0, dg > 0 ? dg : 0);
 let cls = '', tag;
 if (s.disabled) { cls = 'off'; tag = '<span class="tag bad">Отключено</span>'; }
 else if (cool > 0) {
 cls = 'cool';
 const kind = bl > Math.max(s.cool_remaining_sec || 0, dg > 0 ? dg : 0) ? 'Circuit Breaker'
 : (dg > (s.cool_remaining_sec || 0) ? 'Понижение приоритета при серии неудач' : (s.cool_kind === 'hard_credit' ? 'кулдаун баллов' : 'кулдаун rate-limit'));
 tag = '<span class="tag warn">' + kind + ' · ' + dur(cool) + '</span>';
 } else tag = '<span class="tag ok">доступно</span>' + (s.in_flight ? '' : '');
 const note = s.reason ? '<div class="hint" style="font-size:11.5px;color:var(--ink-3);margin-top:3px">' + esc(s.reason) + '</div>' : '';
 const rateLimits = rateLimitRowsHtml(s.rate_limited_models, Date.now());
 const short = s.uid.length > 16 ? s.uid.slice(0, 16) + '…' : s.uid;
 const cred = s.credits == null ? '—' : (s.credits_total > 0 ? s.credits + '<span class="of">/' + s.credits_total + '</span>' : String(s.credits));
 const pct = s.credits_total > 0
 ? Math.min(100, Math.round((s.credits || 0) / s.credits_total * 100))
 : Math.round((s.credits || 0) / maxCred * 100);
 // Учёт затрат tooltip（model_costs）：фактическая цена на модель (≤0 = фактически бесплатно), эксплуатация на этом основании
 // см. "почему всегда выбирается он" — монополия бесплатных аккаунтов / Сортировка по цене за единицу наглядна.
 let credTip = s.credits_total > 0 ? 'остаток ' + s.credits + ' / Общая сумма ' + s.credits_total + '（' + pct + '%）' : 'баллы (относительно максимума в пуле)';
 const costs = (s.model_costs || []).filter(c => c.model);
 if (costs.length) {
 credTip += '\nФактическая цена за единицу (credits/1K）：\n' + costs.map(c =>
 ' ' + c.model + '：' + (c.cost_per_1k <= 0 ? 'Бесплатно' : c.cost_per_1k)).join('\n');
 }
 const frozen = s.disabled || cool > 0;
 const tu = s.token_usage || {};
 const req = tu.request_count || 0;
 const totalTok = formatTokenCount(tu.total_tokens);
 const totalTokUnit = totalTok === '—' ? '' : '<em>tok</em>';
 const latency = formatLatency(tu.last_latency_ms);
 const rate = formatRate(tu.last_tokens_per_second);
 const usageTitle = 'Последний раз:' + req + ' Раз / ' + totalTok + ' / Задержка ' + latency + ' / ' + rate;
 return '<tr class="' + cls + '" title="uid: ' + esc(s.uid) + '">' +
 '<td class="mark" aria-hidden="true"><i></i></td>' +
 '<td class="who"><div class="nm">' + (s.nickname ? esc(s.nickname) : '<span style="color:var(--ink-3)">Без названия</span>') + (s.realm === 'global' ? ' <span class="realm-tag">Международная версия</span>' : '') + '</div><div class="id">' + esc(short) + '</div></td>' +
 '<td>' + tag + note + rateLimits + '</td>' +
 '<td class="cred" title="' + esc(credTip) + '"><div class="n">' + cred + '</div><div class="bar"><i style="width:' + pct + '%"></i></div></td>' +
 '<td class="num">' + (s.success_count || 0) + ' <span style="color:var(--ink-3)">/</span> <span style="color:var(--bad)">' + (s.err_total || 0) + '</span></td>' +
 '<td class="num">' + (s.in_flight || 0) + '</td>' +
 '<td class="num usage-cell" title="' + esc(usageTitle) + '"><span class="usage-line" aria-label="' + esc(usageTitle) + '">' +
 '<span class="usage-item usage-count"><b>' + req + '</b><em>Раз</em></span>' +
 '<span class="usage-item usage-total"><b>' + totalTok + '</b>' + totalTokUnit + '</span>' +
 '<span class="usage-item usage-latency"><b>' + latency + '</b></span>' +
 '<span class="usage-item usage-rate"><b>' + rate + '</b></span>' +
 '</span></td>' +
 '<td class="num" style="color:var(--ink-3)">' + ago(s.last_success) + '</td>' +
 '<td class="acts">' +
 '<button class="xs ghost" data-a="checkin" data-u="' + esc(s.uid) + '"' + (s.checkin_done ? ' title="сегодня уже отмечено; нажмите для повторной отметки и обновления баланса"' : '') + '>' + (s.checkin_done ? 'уже отмечено' : 'check-in') + '</button>' +
 '<button class="xs ghost" data-a="balance" data-u="' + esc(s.uid) + '">Баланс</button>' +
 '<button class="xs ghost" data-a="tasks" data-u="' + esc(s.uid) + '">задача</button>' +
 (frozen ? '<button class="xs primary" data-a="revive" data-u="' + esc(s.uid) + '">Разморозка</button>'
 : '<button class="xs ghost" data-a="disable" data-u="' + esc(s.uid) + '">Отключено</button>') +
 '<button class="xs ghost danger" data-a="remove" data-u="' + esc(s.uid) + '">Удалить</button>' +
 '</td></tr>';
 }).join('');
}

async function loadOverview(quiet) {
 try {
 const d = await api('overview');
 overviewData = d;
 $('sTotal').textContent = d.total;
 $('sHealthy').textContent = d.healthy;
 $('sCooling').textContent = d.cooling;
 $('sDisabled').textContent = d.disabled;
 const remSum = (d.accounts || []).reduce((a, s) => a + (s.credits || 0), 0);
 const totSum = (d.accounts || []).reduce((a, s) => a + (s.credits_total || 0), 0);
 $('sCredits').textContent = totSum > 0 ? remSum + ' / ' + totSum : remSum;
 $('sSticky').textContent = d.sticky_sessions;
 $('navSub').textContent = 'v' + d.version;
 $('navVer').textContent = 'v' + d.version;
 $('navRedis').textContent = d.redis_mode === 'upstash' ? 'Redis Образ' : 'Локальная память';
 $('navState').textContent = d.healthy > 0 ? 'Сервис в норме' : (d.total ? 'нет доступных аккаунтов' : 'Аккаунт к добавлению');
 const p = $('navPulse');
 p.className = 'pulse' + (d.healthy > 0 ? '' : (d.total ? ' warn' : ' bad'));
 $('accNote').textContent = d.in_flight_full ? d.in_flight_full + ' аккаунтов в полете занято' : '';
 const up = Math.floor(d.uptime_sec);
 $('subMeta').textContent = 'Выполнение ' + (up >= 86400 ? Math.floor(up / 86400) + ' день ' : '') + Math.floor(up % 86400 / 3600) + ' Время ' + Math.floor(up % 3600 / 60) + ' Разделить';
 renderAccounts(d.accounts || []);
 } catch (e) { if (!quiet) toast(e.message, 'err'); }
}

$('accBody').addEventListener('click', async ev => {
 const b = ev.target.closest('button[data-a]');
 if (!b) return;
 const u = b.dataset.u, a = b.dataset.a;
 if (a === 'remove' && !confirm('Удаление аккаунта удалит состояние пула и auths/ Файл учетных данных в , безвозвратно. Подтвердить удаление?')) return;
 if (a === 'disable' && !confirm('После отключения аккаунт не участвует в выборе номера, требуется ручная разморозка для восстановления. Подтвердить отключение?')) return;
 b.disabled = true;
 try {
 if (a === 'checkin') {
 const r = await api('accounts/' + encodeURIComponent(u) + '/checkin', { method: 'POST' });
 toast('Чекин завершён' + (r.credits != null ? '，Баллы ' + r.credits + (r.credits_total > 0 ? '/' + r.credits_total : '') : '') + (r.checkin_message ? '（' + r.checkin_message + '）' : ''), 'ok');
 } else if (a === 'balance') {
 const r = await api('accounts/' + encodeURIComponent(u) + '/balance', { method: 'POST' });
 toast('баланс обновлен:' + r.credits + (r.credits_total > 0 ? ' / ' + r.credits_total : ''), 'ok');
 } else if (a === 'revive') {
 await api('accounts/' + encodeURIComponent(u) + '/revive', { method: 'POST' });
 toast('разморожен', 'ok');
 } else if (a === 'disable') {
 await api('accounts/' + encodeURIComponent(u) + '/disable', { method: 'POST' });
 toast('Отключено', 'ok');
 } else if (a === 'tasks') {
 openTasks(u);
 } else if (a === 'remove') {
 const r = await api('accounts/' + encodeURIComponent(u) + '/remove', { method: 'POST' });
 toast(r.file_error ? 'удалено (ошибка удаления файла учетных данных:' + r.file_error + '）' : 'Удалено', 'ok');
 }
 } catch (e) { toast(e.message, 'err'); }
 finally { b.disabled = false; loadOverview(true); }
});

$('btnCheckinAll').onclick = async () => {
 try { await api('checkin_all', { method: 'POST' }); toast('пакетный check-in запущен, результат см. в логе', 'ok'); }
 catch (e) { toast(e.message, 'err'); }
};
$('btnKeepaliveAll').onclick = async () => {
 try { await api('keepalive_all', { method: 'POST' }); toast('Keep-alive для всех запущен, результат см. в логе', 'ok'); }
 catch (e) { toast(e.message, 'err'); }
};
$('btnTravelAll').onclick = async () => {
 try { await api('travel_all', { method: 'POST' }); toast('Патрулирование travel уже запущено (вкл. цепочку adoption), см. лог', 'ok'); }
 catch (e) { toast(e.message, 'err'); }
};
$('btnActivityAll').onclick = async () => {
 try { await api('activity_all', { method: 'POST' }); toast('Отправка активности запущена, результат см. в логе', 'ok'); }
 catch (e) { toast(e.message, 'err'); }
};

/* ── Модель ─────────────────────────────────────────────────────────── */
/* Отметка фактического лимита:scripts/probe_max_tokens.py --panel-out Запись результата пробы,
 /panel/api/model_probes Только чтение, сквозная передача. Ключ-пробник с префиксом домена (cn:glm-5.2），таблица моделей
 Отображать чистое имя, по «точному совпадению или :связь по суффиксу». При отсутствии данных колонка откатывается к заявленному апстримом значению. */
function fmtK(n) { n = Number(n || 0); return n >= 1000 ? Math.round(n / 1000) + 'K' : String(n); }
function probeDays(ts) {
 if (!ts) return null;
 const t = new Date(String(ts).replace(' ', 'T'));
 const d = (Date.now() - t.getTime()) / 86400000;
 return isNaN(d) ? null : Math.floor(d);
}
function outCell(m, pr) {
 if (!pr) return '<td class="num">' + (m.max_output_tokens ? fmtK(m.max_output_tokens) : '—') + '</td>';
 const tip = 'заявляет ' + (pr.claimed ? fmtK(pr.claimed) : '?') + ' · На практике ' + (pr.measured ? fmtK(pr.measured) : '?') +
 (pr.note ? ' · ' + pr.note : '') + (pr.tested_at ? ' · Обнаружено в ' + pr.tested_at : '');
 const days = probeDays(pr.tested_at);
 const stale = days !== null && days > 30 ? ' · ' + days + ' дней назад' : '';
 if (pr.verdict === 'clamped' && pr.measured) {
 if (pr.claimed && pr.measured < pr.claimed) {
 const x = pr.claimed / pr.measured;
 const xs = (x >= 10 ? Math.round(x) : Math.round(x * 10) / 10) + '×';
 return '<td class="num" title="' + esc(tip) + '"><span style="color:var(--warn);font-weight:600">' +
 fmtK(pr.measured) + ' ⚠</span><div class="note">кламп ' + xs + stale + '</div></td>';
 }
 return '<td class="num" title="' + esc(tip) + '"><span style="color:var(--ok)">' + fmtK(pr.measured) +
 (pr.claimed && pr.measured > pr.claimed ? ' ↑' : ' ✓') + '</span></td>';
 }
 if (pr.verdict === 'at_least' && pr.measured)
 return '<td class="num" title="' + esc(tip) + '"><span style="color:var(--ink-3)">≥' + fmtK(pr.measured) + '</span></td>';
 return '<td class="num" title="' + esc(tip) + '"><span style="color:var(--ink-3)">?</span><div class="note">Не выявлено' + stale + '</div></td>';
}

/* rateCell столбец множителя: курс vs Действующая цена. Апстрим credits — это прайс (базовый коэффициент после штатной эксплуатации),
 modelPromotions для текущей действующей скидки (временно бесплатно factor=0 / Ночная скидка 50% 0.5 и т.д.) —
 WorkBuddy Клиент показывает именно действующую цену. При скидке: действующая цена крупным шрифтом + Тег + зачеркнутая цена,
 ховер с пояснением периода; нет factor Только тег (тип внепиковый): прайс + тег. */
function rateCell(m) {
 const tip = m.promo_note ? ' title="' + esc(m.promo_note) + '"' : '';
 if (m.promo_factor != null && m.promo_credits) {
 const base = m.credits ? ' <s style="color:var(--ink-3);font-size:11.5px">' + esc(m.credits) + '</s>' : '';
 const label = m.promo_label ? ' <span class="tag ok">' + esc(m.promo_label) + '</span>' : '';
 return '<span' + tip + ' style="cursor:help"><b>' + esc(m.promo_credits) + '</b>' + label + base + '</span>';
 }
 if (m.promo_label) {
 return '<span' + tip + ' style="cursor:help">' + (m.credits ? esc(m.credits) : '—') +
 ' <span class="tag warn">' + esc(m.promo_label) + '</span></span>';
 }
 return m.credits ? esc(m.credits) : '—';
}

async function loadModels() {
 const tb = $('mdBody');
 tb.innerHTML = '<tr><td colspan="7"><div class="empty">Запрос к апстриму…</div></td></tr>';
 try {
 // Данные зондирования — опциональное усиление: ошибка загрузки не влияет на сам список моделей
 const [d, pr] = await Promise.all([api('models'), api('model_probes').catch(() => ({}))]);
 mdAll = d.models || [];
 mdProbes = pr.probes || {};
 if (!mdAll.length) {
 tb.innerHTML = '<tr><td colspan="7"><div class="empty">Апстрим не вернул модель</div></td></tr>';
 $('mdCount').textContent = '';
 $('mdNote').textContent = 'Апстрим не вернул модель';
 return;
 }
 // Ключ пробы с префиксом домена (cn:glm-5.2），таблица моделей показывает чистое имя, по правилу «точное совпадение или :связь по суффиксу».
 const probeKeys = Object.keys(mdProbes);
 mdProbeOf = id => mdProbes[id] || mdProbes[probeKeys.find(k => k.endsWith(':' + id))];
 const hit = mdAll.filter(m => mdProbeOf(m.id)).length;
 $('mdNote').textContent = mdAll.length + ' моделей · кэш деградации обновлён' + (hit ? ' · ' + hit + ' имеет измеренный верхний предел' : '');
 renderModels();
 } catch (e) {
 mdAll = [];
 tb.innerHTML = '<tr><td colspan="7"><div class="empty">' + esc(e.message) + '</div></td></tr>';
 $('mdCount').textContent = '';
 }
}

/* ── Фильтрация моделей (запрос по условию)───────────────────────────────────────────
 каталог моделей грузится целиком за раз (десятки записей), фильтрация и сортировка полностью на фронтенде: смена условий без задержки и не
 Т.к. каждый вызов фильтра бьёт в апстрим —/panel/api/models это прямой realtime-запрос к апстриму, очень дорого.
 Между условиями AND；Пустое условие не участвует в проверке. */
// mdRateValue Текущее действующее значение множителя баллов: приоритет у промо-цены (временно бесплатно = 0），без множителя учитывать как
// Infinity в конец очереди (при сортировке"Нет цены"не должен выдавать себя за самый дешёвый).
function mdRateValue(m) {
 const raw = (m.promo_credits != null && m.promo_credits !== '') ? m.promo_credits : m.credits;
 const n = parseFloat(String(raw == null ? '' : raw).replace(/[^\d.]/g, ''));
 return Number.isFinite(n) ? n : Infinity;
}

// mdSearchText Поля, участвующие в поиске по ключевым словам (ID / Отображаемое имя / Вендор / описание / тег).
function mdSearchText(m) {
 return [m.id, m.name, m.vendor, m.description, (m.tags || []).join(' ')]
 .filter(Boolean).join(' ').toLowerCase();
}

// mdMatch Удовлетворяет ли отдельная модель всем условиям фильтрации.
function mdMatch(m, f) {
 f = f || mdFilter;
 if (f.q) {
 const text = mdSearchText(m);
 // Разбиение по пробелам с поштучным матчингом: несколько ключевых слов — это AND，Для удобства"cn Визуал"такие комбинированные запросы.
 for (const kw of f.q.toLowerCase().split(/\s+/).filter(Boolean)) {
 if (!text.includes(kw)) return false;
 }
 }
 if (f.realm && !String(m.id || '').startsWith(f.realm + ':')) return false;
 if (f.cap === 'tool' && !m.supports_tool_call) return false;
 if (f.cap === 'vision' && !m.supports_images) return false;
 if (f.cap === 'reasoning' && !m.supports_reasoning) return false;
 if (f.cap === 'default' && !m.is_default) return false;
 if (f.effort === 'off') {
 if (!m.can_disable_thinking) return false;
 } else if (f.effort && !(m.supported_efforts || []).includes(f.effort)) {
 return false;
 }
 const factor = m.promo_factor == null ? null : Number(m.promo_factor);
 if (f.promo === 'promo' && factor == null && !m.promo_label) return false;
 if (f.promo === 'free' && !(factor === 0)) return false;
 if (f.promo === 'discount' && !(factor != null && factor > 0)) return false;
 return true;
}

// mdSortList вернуть новый массив по текущему условию сортировки (не изменяет входные параметры, сохраняет исходный порядок апстрима для трассировки).
function mdSortList(list, f) {
 f = f || mdFilter;
 const out = list.slice();
 const num = v => { const n = Number(v || 0); return Number.isFinite(n) ? n : 0; };
 if (f.sort === 'rate') out.sort((a, b) => mdRateValue(a) - mdRateValue(b));
 else if (f.sort === 'context') out.sort((a, b) => num(b.context_length) - num(a.context_length));
 else if (f.sort === 'output') out.sort((a, b) => num(b.max_output_tokens) - num(a.max_output_tokens));
 else if (f.sort === 'name') out.sort((a, b) => String(a.id || '').localeCompare(String(b.id || '')));
 return out;
}

// mdRowHtml Строка одной модели (чистый рендер, для изолированного теста).
function mdRowHtml(m, pr) {
 const eff = (m.supported_efforts || []).slice();
 if (m.can_disable_thinking && eff.length && !eff.includes('off')) eff.push('off（можно отключить)');
 const effs = eff.length ? eff.map(e => '<span class="tag warn">' + esc(e) + '</span>').join(' ')
 : '<span style="color:var(--ink-3);font-size:12.5px">' + (m.supports_reasoning ? 'Фиксированный тариф · По умолчанию ' + esc(m.default_effort || '?') : 'мышление не поддерживается') + '</span>';
 // Бейдж возможности: модель по умолчанию / Вызов инструмента / Визуал / Чистый reasoning (все поля каталога апстрима прозрачно, отсутствующие не отображаются)
 const caps = [];
 if (m.is_default) caps.push('<span class="tag ok">По умолчанию</span>');
 if (m.supports_tool_call) caps.push('<span class="tag warn">Инструмент</span>');
 if (m.supports_images) caps.push('<span class="tag warn">Визуал</span>');
 if (m.supports_reasoning && !m.can_disable_thinking) caps.push('<span class="tag warn">Thinking всегда включен</span>');
 const capHtml = caps.length ? '<div class="id" style="margin-top:2px">' + caps.join(' ') + '</div>' : '';
 const tip = m.description ? ' title="' + esc(m.description) + '"' : '';
 return '<tr><td class="mark" aria-hidden="true"><i></i></td><td class="who"' + tip + '><div class="nm">' + esc(m.id) + '</div><div class="id">' + esc(m.name || '') + '</div>' + capHtml + '</td>' +
 '<td class="num">' + rateCell(m) + '</td>' +
 '<td>' + (m.default_effort ? '<span class="tag ok">' + esc(m.default_effort) + '</span>' : '<span style="color:var(--ink-3)">—</span>') + '</td>' +
 '<td class="efs" style="white-space:normal">' + effs + '</td>' +
 '<td class="num">' + (m.context_length ? Math.round(m.context_length / 1000) + 'K' : '—') + '</td>' +
 outCell(m, pr) + '</tr>';
}

function renderModels() {
 const tb = $('mdBody');
 const list = mdSortList(mdAll.filter(m => mdMatch(m)));
 if (!list.length) {
 tb.innerHTML = '<tr><td colspan="7"><div class="empty">Нет моделей, соответствующих текущим условиям фильтрации</div></td></tr>';
 } else {
 tb.innerHTML = list.map(m => mdRowHtml(m, mdProbeOf(m.id))).join('');
 }
 const filtered = list.length !== mdAll.length;
 $('mdCount').textContent = !mdAll.length ? ''
 : filtered ? 'Попадание ' + list.length + ' / ' + mdAll.length + ' моделей'
 : mdAll.length + ' моделей';
 $('mdCount').className = filtered ? 'note src-off' : 'note';
}

function resetModelFilter() {
 mdFilter = { q: '', realm: '', cap: '', effort: '', promo: '', sort: 'default' };
 $('mdQ').value = ''; $('mdRealm').value = ''; $('mdCap').value = '';
 $('mdEffort').value = ''; $('mdPromo').value = ''; $('mdSort').value = 'default';
 renderModels();
}

// Элемент фильтра: debounce поля ввода 120ms（длинный список не пересортировывать на каждое нажатие), выпадающий список — мгновенно.
let mdQTimer = null;
$('mdQ').oninput = () => {
 clearTimeout(mdQTimer);
 mdQTimer = setTimeout(() => { mdFilter.q = $('mdQ').value.trim(); renderModels(); }, 120);
};
for (const [id, key] of [['mdRealm', 'realm'], ['mdCap', 'cap'], ['mdEffort', 'effort'], ['mdPromo', 'promo'], ['mdSort', 'sort']]) {
 const el = $(id);
 if (!el) continue;
 el.onchange = () => { mdFilter[key] = el.value; renderModels(); };
}
$('mdReset').onclick = resetModelFilter;
$('btnModels').onclick = loadModels;

/* ── лог (канал: все/задача/Диалог/система) ─────────────────────────────── */
let logCh = 'all';
$('logChips').addEventListener('click', ev => {
 const b = ev.target.closest('button[data-ch]');
 if (!b) return;
 logCh = b.dataset.ch;
 document.querySelectorAll('#logChips .chip').forEach(c => c.classList.toggle('on', c === b));
 loadLogs();
});
async function loadLogs() {
 const box = $('logBox');
 const atEnd = box.scrollTop + box.clientHeight >= box.scrollHeight - 24;
 const limit = ($('reqLimit') && $('reqLimit').value) || 100;
 // Диапазон времени фильтруется на стороне архива (не фронтендом по уже загруженным записям): когда интервал попадает в более ранний период,
 // 「Недавнее N в «записях» этих записей не будет, необходимо запрашивать у сервера по времени.
 const rq = trangeQuery('reqRange', false);
 rq.set('limit', limit);
 try {
 const [d, metrics, requestRows] = await Promise.all([
 api('logs'),
 api('request_metrics').catch(() => ({})),
 api('request_logs?' + rq.toString()).catch(() => ({ entries: [] })),
 ]);
 // при включённом архиве приоритет у архива — «отсутствие записей в интервале» — валидный результат, откат к памяти недопустим
 // последний из 100 записей (иначе отобразит запросы вне условий фильтра и вне временного диапазона).
 // Откат к метрикам в памяти только при выключенном архиве, чтобы деплои без архива по-прежнему видели последние запросы.
 const archiveOn = !!(metrics && metrics.archive && metrics.archive.enabled);
 const recent = archiveOn ? (requestRows.entries || []) : (metrics.recent || []);
 renderRequestMetrics(metrics, recent);
 const entries = (d.entries || []).filter(e => logCh === 'all' || e.ch === logCh);
 box.innerHTML = entries.length
 ? entries.map(e => {
 const lvl = /error|ошибка|Ошибка/.test(e.text) ? ' e' : /warn|Охлаждение|Circuit Breaker/.test(e.text) ? ' w' : '';
 const t = e.ts ? new Date(e.ts).toLocaleTimeString('zh-CN', { hour12: false }) : '';
 const ch = logCh === 'all' ? '<i class="lch c-' + esc(e.ch) + '">' + ({ task: 'задача', chat: 'Диалог', sys: 'Система' }[e.ch] || e.ch) + '</i>' : '';
 return '<span class="ln' + lvl + '">' + ch + esc(t + ' ' + e.text) + '</span>';
 }).join('')
 : '<span style="color:var(--ink-3)">Логов пока нет</span>';
 if (logPin && atEnd) box.scrollTop = box.scrollHeight;
 const counts = {};
 for (const e of (d.entries || [])) counts[e.ch] = (counts[e.ch] || 0) + 1;
 $('logNote').textContent = logCh === 'all'
 ? 'задача ' + (counts.task || 0) + ' · Диалог ' + (counts.chat || 0) + ' · Система ' + (counts.sys || 0)
 : (logCh === 'task' ? 'задача' : logCh === 'chat' ? 'Диалог' : 'Система') + ' ' + entries.length + ' Строка';
 } catch (e) { /* В обзоре уже подсказано */ }
}

function renderRequestMetrics(m, entries) {
 m = m || {};
 const a = m.archive || {};
 $('reqSummary').textContent =
 'завершено ' + fmtTok(m.completed) +
 ' · Успех ' + (m.success_rate == null ? '—' : Number(m.success_rate).toFixed(1) + '%') +
 ' · HTTP ' + (m.http_success_rate == null ? '—' : Number(m.http_success_rate).toFixed(1) + '%') +
 ' · Среднее ' + fmtMs(m.avg_duration_ms) +
 ' · Выполняется ' + String(m.in_flight || 0);
 $('reqNote').textContent = a.enabled
 ? 'JSONL Архив ' + fmtBytes(a.bytes) + (a.dropped_writes ? ' · Отбросить ' + a.dropped_writes + ' запись' : '') +
 (a.last_error ? ' · Ошибка:' + a.last_error : '')
 : 'Только in-memory метрики,JSONL архив закрыт';

 reqEntries = entries || [];
 renderRequestTable();
}

/* reqMatch Фильтрация записей запросов:q Для IP/UA/Модель/Аккаунт/Запрос ID для токенизации по пробелам AND совпадение по вхождению,
 outcome точное совпадение. Оба выполняются на уже полученных записях (макс. 1000 записей), новые запросы не отправлять. */
function reqMatch(e, f) {
 f = f || reqFilter;
 if (f.outcome && String(e && e.outcome || '') !== f.outcome) return false;
 if (f.q) {
 const text = [e && e.client_ip, e && e.user_agent, e && e.model, e && e.account, e && e.request_id]
 .filter(Boolean).join(' ').toLowerCase();
 for (const kw of f.q.toLowerCase().split(/\s+/).filter(Boolean)) {
 if (!text.includes(kw)) return false;
 }
 }
 return true;
}

function reqOutcomeTag(e) {
 const outcome = String(e && e.outcome || '');
 const label = { success: 'Успех', http_error: 'HTTP Ошибка', stream_error: 'ошибка стрима', interrupted: 'прерывание' }[outcome] || outcome || '—';
 const cls = outcome === 'success' ? 'ok'
 : outcome === 'interrupted' ? 'warn'
 : outcome ? 'bad' : 'mute';
 return '<span class="tag ' + cls + '">' + esc(String(e && e.status || '—') + ' ' + label) + '</span>';
}

function reqTokenCell(e) {
 const total = Number(e && e.total_tokens || 0) ||
 (Number(e && e.prompt_tokens || 0) + Number(e && e.completion_tokens || 0));
 return total ? fmtTok(total) : '—';
}

function reqCreditCell(e) {
 if (!e || !e.credit_known) return '<span class="muted">—</span>';
 const v = Number(e.credit);
 return Number.isFinite(v) ? trimFixed(v.toFixed(2)) : '<span class="muted">—</span>';
}

/* renderRequestTable Отрисовка таблицы логов запросов. Колонка источника — фокус этой версии:IP Моноширинный шрифт для удобного сканирования,
 UA Однострочное усечение (полное значение в title внутри, сама строка использует requestLogText Действие tooltip）。 */
function renderRequestTable() {
 const list = reqEntries.filter(e => reqMatch(e));
 const tb = $('reqBody');
 if (!tb) return;
 tb.innerHTML = list.map(e => {
 const when = e && e.time ? new Date(e.time).toLocaleTimeString('zh-CN', { hour12: false }) : '—';
 const ip = e && e.client_ip ? e.client_ip : '';
 const ua = e && e.user_agent ? e.user_agent : '';
 const rid = e && e.request_id ? e.request_id : '';
 return '<tr title="' + esc(requestLogText(e)) + '">' +
 '<td class="num">' + esc(when) + '</td>' +
 '<td>' + reqOutcomeTag(e) + '</td>' +
 '<td>' + esc(e && e.model || '—') + '</td>' +
 '<td>' + esc(e && e.account || '—') + '</td>' +
 '<td>' + (ip ? '<span class="clip ip" title="' + esc(ip) + '">' + esc(ip) + '</span>' : '<span class="muted">—</span>') + '</td>' +
 '<td>' + (ua ? '<span class="clip" title="' + esc(ua) + '">' + esc(ua) + '</span>' : '<span class="muted">—</span>') + '</td>' +
 '<td class="num">' + fmtMs(e && e.duration_ms) + '</td>' +
 '<td class="num">' + reqTokenCell(e) + '</td>' +
 '<td class="num">' + reqCreditCell(e) + '</td>' +
 '<td>' + (rid ? '<span class="clip rid" title="' + esc(rid) + '">' + esc(rid) + '</span>' : '<span class="muted">—</span>') + '</td>' +
 '</tr>';
 }).join('') || '<tr><td colspan="10" class="empty">' +
 (reqEntries.length ? 'Нет записей запросов по текущим условиям фильтра' : 'Пока нет записей запросов') + '</td></tr>';

 const filtered = list.length !== reqEntries.length;
 // Старые записи в архиве без поля источника (записаны до релиза функции): в этом случае подсказка переключателя/По историческим причинам,
 // а не создавать впечатление сломанной фильтрации.
 const hasSource = reqEntries.some(e => e && (e.client_ip || e.user_agent));
 $('reqCount').textContent = !reqEntries.length ? ''
 : (filtered ? 'Попадание ' + list.length + ' / ' + reqEntries.length + ' запись' : reqEntries.length + ' запись') +
 (hasSource ? '' : ' · источник не залогирован');
 $('reqCount').className = (filtered || !hasSource) ? 'note src-off' : 'note';
}

/* Контрол фильтра записей запросов. Дебаунс поля поиска 150ms：Максимум 1000 Перерендер строками, не на каждый ключ.
 данная привязка верхнего уровня размещена в requestLogText до, чтобы"Срез чистой функции"фронтенд-тест в стиле
 （slice requestLogText → fmtBytes）получает только чистую функцию форматирования без побочных эффектов. */
let reqQTimer = null;
if ($('reqQ')) $('reqQ').oninput = () => {
 clearTimeout(reqQTimer);
 reqQTimer = setTimeout(() => { reqFilter.q = $('reqQ').value.trim(); renderRequestTable(); }, 150);
};
if ($('reqOutcome')) $('reqOutcome').onchange = () => {
 reqFilter.outcome = $('reqOutcome').value;
 renderRequestTable();
};
if ($('reqLimit')) $('reqLimit').onchange = loadLogs;
if ($('btnReqReload')) $('btnReqReload').onclick = loadLogs;
// Диапазон времени: по умолчанию «Вся история» — историческое поведение на странице записей запросов — это"Взять последний N запись"，
// Добавление сужающего интервала по умолчанию уменьшит количество записей при открытии страницы.
if ($('reqRange')) trangeBind('reqRange', loadLogs, '0');

function requestLogText(e) {
 const when = e && e.time ? new Date(e.time).toLocaleTimeString('zh-CN', { hour12: false }) : '—';
 const outcomeLabel = { success: 'Успех', http_error: 'HTTP Ошибка', stream_error: 'ошибка стрима', interrupted: 'прерывание' };
 const token = Number(e && e.total_tokens || 0) ||
 (Number(e && e.prompt_tokens || 0) + Number(e && e.completion_tokens || 0));
 let credit = 'credit —';
 if (e && e.credit_known) {
 const value = Number(e.credit);
 if (Number.isFinite(value)) credit = String(Number(value.toFixed(2))) + ' credit';
 }
 return [
 when,
 String(e && e.status || '—') + ' ' + (outcomeLabel[e && e.outcome] || (e && e.outcome) || '—'),
 e && e.model || '—',
 e && e.account || '—',
 e && e.client_ip || '—',
 e && e.user_agent || '—',
 fmtMs(e && e.duration_ms),
 fmtTok(token) + ' tok',
 credit,
 e && e.request_id || '—',
 ].join(' | ');
}

function fmtBytes(bytes) {
 const n = Number(bytes || 0);
 if (n < 1024) return n + ' B';
 if (n < 1024 * 1024) return (n / 1024).toFixed(1) + ' KB';
 return (n / 1024 / 1024).toFixed(1) + ' MB';
}
$('btnLogPin').onclick = () => {
 logPin = !logPin;
 $('btnLogPin').textContent = 'Автопрокрутка:' + (logPin ? 'Открыть' : 'закрыть');
};

/* ── Конфигурация ─────────────────────────────────────────────────────────── */
const CFG_MAP = {
 listen: ['listen'], api_key: ['api_key'],
 package_detail_limit: ['panel', 'package_detail_limit'],
 checkin_hours: ['schedule', 'checkin_hours'], checkin_enabled: ['schedule', 'checkin_enabled'], growth_hours: ['schedule', 'growth_hours'], growth_enabled: ['schedule', 'growth_enabled'],
 travel_hours: ['schedule', 'travel_hours'], travel_enabled: ['schedule', 'travel_enabled'],
 activity_hours: ['schedule', 'activity_hours'], activity_enabled: ['schedule', 'activity_enabled'],
 keepalive_hours: ['schedule', 'keepalive_hours'], keepalive_enabled: ['schedule', 'keepalive_enabled'],
 balance_refresh_enabled: ['schedule', 'balance_refresh_enabled'], balance_refresh_minutes: ['schedule', 'balance_refresh_minutes'],
 max_in_flight: ['pool', 'max_in_flight'], max_in_flight_global: ['pool', 'max_in_flight_global'],
 breaker_threshold: ['pool', 'breaker_threshold'],
 degrade_threshold: ['pool', 'degrade_threshold'], degrade_cooldown: ['pool', 'degrade_cooldown'],
 degrade_cooldown_max: ['pool', 'degrade_cooldown_max'],
 cost_explore_interval: ['pool', 'cost_explore_interval'],
 credit_floor: ['pool', 'credit_floor'],
 prefer_expiring: ['pool', 'prefer_expiring'], expiring_soon: ['pool', 'expiring_soon'],
 soft_rate: ['cooldown', 'soft_rate'], soft_rate_max: ['cooldown', 'soft_rate_max'],
 breaker_cooldown: ['pool', 'breaker_cooldown'], breaker_cooldown_max: ['pool', 'breaker_cooldown_max'],
 idle_weight_per_hour: ['pool', 'idle_weight_per_hour'], idle_weight_max: ['pool', 'idle_weight_max'],
 ttl: ['session_sticky', 'ttl'],
 timeout_seconds: ['upstream', 'timeout_seconds'], header_timeout_seconds: ['upstream', 'header_timeout_seconds'],
 idle_timeout_seconds: ['upstream', 'idle_timeout_seconds'], user_agent: ['upstream', 'user_agent'],
 prompt_mode: ['prompt', 'mode'], prompt_file: ['prompt', 'file'],
 sanitize_blacklist_fingerprints: ['features', 'sanitize_blacklist_fingerprints'],
 session_sticky_enabled: ['session_sticky', 'enabled'],
 request_client_info: ['logging', 'request_client_info'],
};
function dig(obj, path) { return path.reduce((o, k) => (o == null ? undefined : o[k]), obj); }
function put(obj, path, val) {
 let o = obj;
 for (let i = 0; i < path.length - 1; i++) { if (typeof o[path[i]] !== 'object' || o[path[i]] === null) o[path[i]] = {}; o = o[path[i]]; }
 o[path[path.length - 1]] = val;
}

async function loadConfig() {
 try {
 const d = await api('config');
 cfgLoaded = d.config;
 $('cfgPath').textContent = d.path || '';
 const f = $('cfgForm');
 for (const [name, path] of Object.entries(CFG_MAP)) {
 const el = f.elements[name];
 if (!el) continue;
 const v = dig(cfgLoaded, path);
 if (el.type === 'checkbox') el.checked = !!v;
 else if (Array.isArray(v)) el.value = v.join(', ');
 else el.value = v == null ? '' : v;
 }
 markDurationFields(); // После бэкфила сбросить состояние валидации (убрать оставшуюся красную рамку; текущее значение с бэкенда заведомо валидно)
 $('cfgNote').textContent = '';
 } catch (e) { toast('Ошибка чтения конфига:' + e.message, 'err'); }
}
function collectConfig() {
 const f = $('cfgForm'), out = {};
 for (const [name, path] of Object.entries(CFG_MAP)) {
 const el = f.elements[name];
 if (!el) continue;
 let v;
 if (el.type === 'checkbox') v = el.checked;
 else if (el.type === 'number') { v = el.value.trim() === '' ? undefined : Number(el.value); }
 else {
 const raw = el.value.trim();
 if (raw === '') v = undefined;
 else if (name.endsWith('_hours')) v = raw.split(/[,，\s]+/).filter(Boolean).map(Number);
 else v = raw;
 }
 if (v !== undefined) put(out, path, v);
 }
 return out;
}
/* Go мгновенная валидация поля длительности: пусто = используется текущее значение (collectConfig пропуск отправки); непустое должно быть
 ParseDuration Синтаксис (30m / 2h / 600s / 1h30m，комбинируемо, допускает дробные значения). С бэкендом
 config.go normalize() time.ParseDuration Единая метрика, грязные значения подсвечиваются красным на фронте,
 не ждать отказа при сохранении. */
const DURATION_RE = /^(\d+(\.\d+)?(ns|us|µs|ms|s|m|h))+$/;
const DURATION_FIELDS = ['soft_rate', 'soft_rate_max', 'breaker_cooldown', 'breaker_cooldown_max',
 'degrade_cooldown', 'degrade_cooldown_max', 'cost_explore_interval', 'expiring_soon', 'ttl'];
const DURATION_TIP = 'Формат должен быть Go длительность:30m / 2h / 600s / 1h30m';
function durationBad(name) {
 const el = $('cfgForm').elements[name];
 if (!el) return false;
 const v = el.value.trim();
 return v !== '' && !DURATION_RE.test(v);
}
function markDurationFields() {
 for (const name of DURATION_FIELDS) {
 const el = $('cfgForm').elements[name];
 if (!el) continue;
 const bad = durationBad(name);
 el.classList.toggle('invalid', bad);
 el.title = bad ? DURATION_TIP : '';
 }
}
$('cfgForm').addEventListener('input', ev => {
 if (DURATION_FIELDS.includes(ev.target.name)) markDurationFields();
});
$('btnEye').onclick = () => {
 const el = $('cfgKey');
 const show = el.type === 'password';
 el.type = show ? 'text' : 'password';
 $('btnEye').textContent = show ? 'Скрыть' : 'Отображение';
};
$('btnCfgReload').onclick = loadConfig;
$('cfgForm').onsubmit = async ev => {
 ev.preventDefault();
 // Перехват грязных значений поля длительности: подсветка красным + toast Указание имени, не отправлять запрос на сохранение (бэкенд также отклонит, здесь предпроверка).
 markDurationFields();
 const firstBad = DURATION_FIELDS.find(durationBad);
 if (firstBad) {
 const el = $('cfgForm').elements[firstBad];
 el.focus();
 toast('「' + (el.closest('.fld')?.querySelector('.lb')?.textContent || firstBad) + '」' + DURATION_TIP, 'err');
 return;
 }
 const btn = $('btnCfgSave');
 btn.disabled = true; btn.textContent = 'Сохранение…';
 try {
 const r = await api('config', { method: 'POST', body: JSON.stringify(collectConfig()) });
 const n = (r.restart_required || []).length;
 toast(n ? 'конфиг сохранён, в т.ч. ' + n + ' параметр требует перезапуска процесса' : 'конфигурация сохранена и сразу применена', 'ok');
 // ключ мог измениться: в текущей сессии использовать новое значение, чтобы следующий опрос не был 401。
 const k = $('cfgKey').value.trim();
 if (k) localStorage.setItem(LS_KEY, k);
 loadConfig();
 loadOverview(true);
 } catch (e) { toast('Ошибка сохранения:' + e.message, 'err'); }
 finally { btn.disabled = false; btn.textContent = 'Сохранить конфигурацию'; }
};

/* ── Добавить аккаунт ─────────────────────────────────────────────────────── */
function openAdd() {
 $('addVeil').classList.add('on');
 // сброс на вкладку логина
 switchAddTab('login');
 $('addPick').hidden = false;
 $('addLoad').hidden = true; $('addReady').hidden = true;
 $('addDone').hidden = true; $('addErr').hidden = true;
 $('importDone').hidden = true; $('importErr').hidden = true;
 $('btnCopyUrl').hidden = true; $('btnOpenUrl').hidden = true;
 $('btnStartLogin').hidden = false; $('btnStartLogin').disabled = false;
 stopPoll();
}
function switchAddTab(tab) {
 document.querySelectorAll('#addTabs .tab').forEach(b => b.classList.toggle('on', b.dataset.tab === tab));
 $('addTabLogin').hidden = tab !== 'login';
 $('addTabImport').hidden = tab !== 'import';
}
document.querySelectorAll('#addTabs .tab').forEach(b => {
 b.onclick = () => switchAddTab(b.dataset.tab);
});
function startAddLogin() {
 const realm = (document.querySelector('input[name="addRealm"]:checked') || {}).value || 'cn';
 $('btnStartLogin').disabled = true;
 $('addLoad').hidden = false; $('addErr').hidden = true;
 api('login/start', { method: 'POST', body: JSON.stringify({ realm }) }).then(r => {
 loginState = r.state;
 $('addUrl').textContent = r.url;
 $('addPick').hidden = true; // фиксация выбранного домена (сессия уже инициирована в этом домене)
 $('addLoad').hidden = true; $('addReady').hidden = false;
 $('btnStartLogin').hidden = true;
 $('btnCopyUrl').hidden = false; $('btnOpenUrl').hidden = false;
 loginTimer = setInterval(pollLogin, 3000);
 }).catch(e => {
 $('addLoad').hidden = true;
 $('btnStartLogin').disabled = false;
 $('addErr').hidden = false;
 $('addErr').textContent = e.message;
 });
}
function stopPoll() { if (loginTimer) { clearInterval(loginTimer); loginTimer = null; } }
async function pollLogin() {
 if (!loginState) return;
 try {
 const r = await api('login/poll?state=' + encodeURIComponent(loginState));
 if (r.done) {
 stopPoll();
 $('addReady').hidden = true;
 $('addDone').hidden = false;
 $('addDone').textContent = 'Добавлено ' + (r.nickname || r.uid) + (r.realm === 'global' ? '（международная версия)' : '') + (r.credits >= 0 ? ' · Баллы ' + r.credits + (r.credits_total > 0 ? '/' + r.credits_total : '') : '') + '，аккаунт уже загружен в пул';
 setTimeout(() => { closeAdd(); loadOverview(true); }, 1600);
 }
 } catch (e) {
 stopPoll();
 $('addReady').hidden = true;
 $('addErr').hidden = false;
 $('addErr').textContent = e.message + '（после закрытия добавить заново)';
 }
}
function closeAdd() { stopPoll(); loginState = null; $('addVeil').classList.remove('on'); }
$('btnCloseAdd').onclick = closeAdd;
$('btnStartLogin').onclick = startAddLogin;
$('btnOpenUrl').onclick = () => open($('addUrl').textContent, '_blank');
$('btnCopyUrl').onclick = () => navigator.clipboard.writeText($('addUrl').textContent)
 .then(() => toast('Ссылка скопирована', 'ok'), () => toast('копирование не удалось, выделите вручную для копирования', 'err'));
$('importFile').onchange = async () => {
 const file = $('importFile').files[0];
 if (!file) return;
 $('importDone').hidden = true; $('importErr').hidden = true;
 const fd = new FormData();
 fd.append('file', file);
 const h = {};
 const k = localStorage.getItem(LS_KEY);
 if (k) h['Authorization'] = 'Bearer ' + k;
 try {
 const r = await fetch('/panel/api/import/cockpit', { method: 'POST', body: fd, headers: h });
 const d = await r.json();
 if (!r.ok) throw new Error(d.error || ('HTTP ' + r.status));
 $('importDone').hidden = false;
 $('importDone').textContent = 'импорт завершён: успешно ' + d.imported + ' шт.' + (d.skipped ? '，пропустить ' + d.skipped + ' шт.' : '');
 if (d.errors && d.errors.length) {
 console.warn('import errors:', d.errors);
 }
 loadOverview(true);
 } catch (e) {
 $('importErr').hidden = false;
 $('importErr').textContent = 'ошибка импорта:' + e.message;
 }
 $('importFile').value = '';
};

/* ── верхнее действие ─────────────────────────────────────────────────────── */
$('btnAdd').onclick = openAdd;
$('btnRefresh').onclick = async () => {
 const b = $('btnRefresh');
 b.disabled = true; b.textContent = 'Обновление…';
 try {
 await api('balance_all', { method: 'POST' });
 await loadOverview(true);
 toast('Баланс обновлен из апстрима', 'ok');
 } catch (e) { toast('Ошибка обновления:' + e.message, 'err'); await loadOverview(true); }
 finally { b.disabled = false; b.textContent = 'Обновление'; }
 if (view === 'logs') loadLogs();
};

/* ── Опрос ─────────────────────────────────────────────────────────── */
function refreshVisible() {
 if (view === 'accounts') loadOverview(true);
 else if (view === 'logs') loadLogs();
 else if (view === 'taskscenter') reattachQueueView();
}
function start() {
 loadOverview(true);
 if (refTimer) clearInterval(refTimer);
 refTimer = setInterval(refreshVisible, 5000);
 checkAuthGate();
}
async function checkAuthGate() {
 try { await api('overview'); }
 catch (e) { if (String(e.message).includes('ключ') || String(e.message).includes('api_key')) return; }
}
start();

/* ── Задача баллов ─────────────────────────────────────────────────────── */
let taskUID = null;

// автозавершаемые задачи (с бэкендом autoActions согласованность таблиц): критерий — поведенческое событие, воспроизводимо через шлюз.
// Остальные задачи требуют взаимодействия в официальном клиенте, панель только показывает инструкцию (строка title подсказка).
// Внимание: ключ содержит точку (Model_chat_GLM5.2）Обязательно в кавычках, иначе будет распарсено как обращение к свойству + Числовой литерал.
const AUTO_TASKS = {
 'chat_5': 'Отчёт 5 событий активности диалога (автодобавление недостающего)',
 'first_buddy': 'отчёт разблокирует → принять соглашение → Получить первого Buddy',
 'Model_chat_GLM5.2': 'Принять задачу → glm-5.2 один реальный диалог → Выравнивание отчета по моделям',
 'RichMeow_Chat': 'Отправка цепочки событий fingerprint десктопа (проверено, подсвечивается)',
 'Buddy_App': 'отчет «вход в Buddy применения» цепочка событий (проверено, подсвечивается)',
 'Buddy_App_QQ': 'Отправка цепочки событий «Вход в Помощник учителя Penguin» (подтверждено, подсвечивается)',
 'automation_1': 'Отправка события «Создание задачи по расписанию» (проверено, подсвечивается)',
 'Library_read': 'отправить событие «прочитано введение в базу знаний» (проверено, подсвечивается)',
 'template_5': 'отчет группы событий «создание задачи по шаблону» ×5（проверено на трёх аккаунтах — подсвечено)',
 'playbook_prompt': 'репорт «отправка такого же кейса-вдохновения» Prompt」Группа событий (проверено на трех аккаунтах — подсвечено)',
 'create_canvas': 'репорт группы событий «создание холста дизайн-креатива» (на трех аккаунтах фактически зажигается,+300 мин)',
 'expert_5': 'Вызов реального эксперта+Цепочка использования ×5（Маркет экспертов+Реальный chat，проверено на трёх аккаунтах — подсвечено)',
 'Expert_team_use_3': 'Вызов настоящей группы экспертов+Цепочка использования ×3（проверено на трёх аккаунтах — подсвечено)',
 'Hp_Appearance': 'настроить тему API + Событие применения скина (подсветка проверена на двух аккаунтах)',
 'black_cat': 'Сова:23:00–08:00 Внутри окна glm-5.2 Дополнение диалога (подсказки вне окна и т.п. 23 точечное расписание)',
 'Expert_lighthouse': 'Вызов реального легковесного cloud-эксперта+цепочка использования (реальный диалог requestId，замер на двух аккаунтах — подтверждено)',
 'skill_1': 'реальный диалог + skill_info Событие загрузки навыка (фактически подсвечено)',
 'school_season': 'Кампусный день (в терминах мини-приложения):accept → mini Диалог+activityId Отчёт → Получение награды (+100c+5e）',
 'Sequential_Tasks_1': 'Первый диалог мини-приложения (метрика мини-приложения):accept → mini репорт диалога → Получение награды (+100c+5e）',
 'Sequential_Tasks_2': 'выбор диалога с экспертом в мини-программе (терминология мини-программы): эксперт по рынку id → accept → expert_actual_use Отчёт → Получение награды (+200c+5e）',
 'Sequential_Tasks_3': 'Пять диалогов мини-приложения (по метрике мини-приложения):accept → mini репорт диалога ×5（Автодоплата разницы)→ Получение награды (+300c+5e）',
 'Sequential_Tasks_4': 'плановая задача мини-приложения (резерв, ежедневно в 00:00 разблокируется один этап):accept → Событие создания задачи по расписанию → получение награды (критерий ожидает проверки разблокировки)',
 'Sequential_Tasks_5': 'Мини-программа использует GLM5.2（резерв):accept → с полем модели mini репорт диалога → получение награды (критерий ожидает проверки разблокировки)',
 'Sequential_Tasks_6': 'мини-программа: десять диалогов (резерв):accept → mini репорт диалога ×target（Автодоплата разницы)→ Получение награды',
 'Sequential_Tasks_7': 'Функция вдохновения (зарезервировано, возм. PC калибр):accept → группа событий Inspiration (PC+mp две формы)→ получение награды (критерий ожидает проверки разблокировки)'
};

function openTasks(uid) {
 taskUID = uid;
 $('taskWho').textContent = uid.slice(0, 16);
 $('taskVeil').classList.add('on');
 $('btnTaskReload').hidden = false;
 loadTasks();
}
function closeTasks() { $('taskVeil').classList.remove('on'); taskUID = null; }
$('btnCloseTask').onclick = closeTasks;
$('btnTaskReload').onclick = loadTasks;

// Принять всё: разом принять все непринятые задачи аккаунта (идемпотентно, пропуск уже принятых/уже получено).
$('btnTaskAcceptAll').onclick = async () => {
 if (!taskUID) return;
 const btn = $('btnTaskAcceptAll');
 btn.disabled = true; btn.textContent = 'Принимается…';
 try {
 const r = await api('accounts/' + encodeURIComponent(taskUID) + '/tasks/accept_all', { method: 'POST' });
 const n = r.accepted || 0;
 if (r.failed && r.failed.length) {
 toast(`Принято ${n} шт.,${r.failed.length} отклонено апстримом (повтор возможен)`, 'err');
 } else {
 toast(n ? `Принято ${n} задач` : (r.message || 'Все задачи приняты'), 'ok');
 }
 } catch (e) { toast(e.message, 'err'); }
 finally { btn.disabled = false; btn.textContent = 'Принять всё'; loadTasks(); }
};

// Одним кликом выполнить все автозадачи (долго: включает реальные диалоги, поштучная верификация чтением).
$('btnTaskAutoAll').onclick = async () => {
 if (!taskUID) return;
 const btn = $('btnTaskAutoAll');
 if (!confirm('Будет выполнено последовательно: досылка события диалога, получение Buddy、glm-5.2 диалог, попытка репорта.\nПроцесс около 1-2 мин (вкл. реальные диалоги), продолжить?')) return;
 btn.disabled = true; btn.textContent = 'Выполняется…';
 try {
 const r = await api('accounts/' + encodeURIComponent(taskUID) + '/tasks/auto_all', { method: 'POST' });
 const okN = (r.results || []).filter(x => x.status === 'done').length;
 const skipN = (r.results || []).filter(x => x.status === 'skipped').length;
 const errN = (r.results || []).filter(x => x.status === 'error').length;
 toast(`Выполнение завершено: успешно ${okN} элементов, пропустить ${skipN} Пункт${errN ? '，ошибка ' + errN + ' Пункт' : ''}`, errN ? 'err' : 'ok');
 console.log('auto_all results:', r.results);
 } catch (e) { toast(e.message, 'err'); }
 finally { btn.disabled = false; btn.textContent = 'Авто-задачи в один клик'; loadTasks(); }
};

async function loadTasks() {
 if (!taskUID) return;
 const st = $('taskState'), tb = $('taskTable');
 st.hidden = false;
 st.className = 'state';
 st.innerHTML = '<span class="dots">Идет запрос</span>';
 tb.hidden = true;
 try {
 const d = await api('accounts/' + encodeURIComponent(taskUID) + '/tasks');
 const list = d.tasks || [];
 if (!list.length) {
 st.className = 'state';
 st.textContent = 'У этого аккаунта пока нет задач';
 return;
 }
 // С прогрессом или доступные к получению — вверху, полученные — внизу — сразу видно"Что делать сейчас"。
 list.sort((a, b) => (a.claimed - b.claimed) || (b.claimable - a.claimable) || String(a.task_code).localeCompare(String(b.task_code)));
 $('taskBody').innerHTML = list.map(t => {
 // Прогресс:current возможно отсутствует (0 или опущено апстримом) — использовать ?? Фолбэк, чтобы не отрендерилось как "undefined / N"
 const cur = t.current ?? 0, tgt = t.target ?? 0;
 const prog = tgt ? cur + ' / ' + tgt : (tgt === 0 && cur > 0 ? String(cur) : '—');
 const parts = [];
 if (t.credit) parts.push('+' + t.credit + ' Разделить');
 if (t.energy) parts.push('+' + t.energy + ' Возможность');
 if (t.reward_buddy) parts.push('Buddy');
 const reward = parts.length ? parts.join(' ') : '—';
 const badge = t.claimed ? '<span class="tag ok">Уже получено</span>'
 : t.claimable ? '<span class="tag warn">Доступно к получению</span>'
 : t.locked ? '<span class="tag mute">Не разблокировано</span>'
 : t.accept_status === 'accepted' ? '<span class="tag mute">Выполняется</span>'
 : '<span class="tag mute">Не принято</span>';
 const acted = t.claimed || t.locked ? ''
 : t.claimable ? '<button class="xs primary" data-t="claim" data-c="' + esc(t.task_code) + '">Получить</button>'
 : AUTO_TASKS[t.task_code] ? '<button class="xs primary" data-t="auto" data-c="' + esc(t.task_code) + '" title="' + esc(AUTO_TASKS[t.task_code]) + '">Выполнить в один клик</button>'
 : t.accept_status === 'accepted' ? ''
 : '<button class="xs" data-t="accept" data-c="' + esc(t.task_code) + '">Принять</button>';
 // Инструкция по действиям (description/task_desc）Монтировать title Подсказка: как выполнить — показать пользователю
 const tip = [t.title, t.task_desc || t.description, t.jump_url ? 'Переход:' + t.jump_url : ''].filter(Boolean).join('\n');
 return '<tr title="' + esc(tip) + '"><td class="mark" aria-hidden="true"><i></i></td>' +
 '<td class="who"><div class="nm">' + esc(t.title || t.task_code) + '</div><div class="id">' + esc(t.task_code) + (t.tag ? ' · ' + esc(t.tag) : '') + '</div></td>' +
 '<td class="num">' + esc(prog) + '</td>' +
 '<td class="num">' + esc(reward) + '</td>' +
 '<td>' + badge + '</td>' +
 '<td class="acts">' + acted + '</td></tr>';
 }).join('');
 st.hidden = true;
 tb.hidden = false;
 } catch (e) {
 st.className = 'state err';
 st.textContent = e.message;
 }
}

$('taskBody').addEventListener('click', async ev => {
 const b = ev.target.closest('button[data-t]');
 if (!b || !taskUID) return;
 const kind = b.dataset.t, code = b.dataset.c;
 b.disabled = true;
 try {
 if (kind === 'auto') {
 // завершение в один клик: действие выполняет бэкенд → Чтение прогресса → отчет (до минут, включает реальные диалоги)
 b.textContent = 'Выполняется…';
 const r = await api('accounts/' + encodeURIComponent(taskUID) + '/tasks/auto', {
 method: 'POST', body: JSON.stringify({ task_code: code })
 });
 if (r.skipped) {
 toast(r.message || 'Пропущено', 'ok');
 } else {
 const advanced = r.progress_before !== r.progress_after;
 let msg = r.message || 'уже выполнено';
 if (r.progress_after) msg += `（Прогресс ${r.progress_before} → ${r.progress_after}）`;
 if (r.claimed) msg += '，Награда уже автоматически зачислена';
 else if (r.claimable) msg += r.claim_error ? '，можно нажать «Получить» для повтора' : '';
 else if (r.attempt && !advanced) msg += '；Прогресс не движется, задаче возможно требуется официальный клиент';
 toast(msg, (r.claimed || advanced) ? 'ok' : 'err');
 }
 loadOverview(true);
 } else {
 const path = 'accounts/' + encodeURIComponent(taskUID) + '/tasks/' + (kind === 'claim' ? 'claim' : 'accept');
 const body = kind === 'claim' ? { task_code: code } : { task_codes: [code] };
 await api(path, { method: 'POST', body: JSON.stringify(body) });
 toast(kind === 'claim' ? 'Награда уже получена' : 'Задача принята', 'ok');
 if (kind === 'claim') loadOverview(true);
 }
 } catch (e) { toast(e.message, 'err'); }
 finally { loadTasks(); }
});

/* ── Центр заданий: Снова в школу + Сканирование всех аккаунтов/очередь ──────────────────────────── */
// юнит заданий к началу учебного года:✓ Получено (зелёный) |◐ x/y В процессе (янтарный) |○ не выполнено (серый)
function staskHTML(t) {
 if (!t) return '<span class="stask todo"><span class="mark">·</span>—</span>';
 if (t.status === 'claimed') return '<span class="stask ok"><span class="mark">✓</span>уже получено</span>';
 if (t.status === 'completed') return '<span class="stask warn"><span class="mark">◆</span>доступно к получению</span>';
 if (t.status === 'in_progress') {
 const fr = t.target_count ? '<span class="fr">' + t.progress + '/' + t.target_count + '</span>' : '';
 return '<span class="stask warn"><span class="mark">◐</span>' + fr + '</span>';
 }
 return '<span class="stask todo"><span class="mark">○</span>Не реализовано</span>';
}
const LUCK_SVG = '<svg viewBox="0 0 16 16" fill="none" stroke="currentColor" stroke-width="1.4"><path d="M3.2 5.2 5 1.8l3 2.4 3-2.4 1.8 3.4-1.4 2.6 1.4 2.6-3.4 2.2H6l-3.4-2.2 1.4-2.6z" opacity=".9"/><circle cx="8" cy="9" r="1.1" fill="currentColor" stroke="none"/></svg>';
const SCHOOL_TITLES = {
 share_invite: 'Акция "Поделиться" +100c', desktop_chat_1_time: 'Опыт десктопа +100c（однократно)',
 chat_3_times: 'и AI Диалог 3 Раз +50c', expert_use: 'Вызов эксперта сезона «Снова в школу» +50c',
 task_student_verify: 'Студенческая верификация +100c（требуется реальная верификация, не делать)',
};

/* ── Упрощенный QR Энкодер (для QR кодов купонов)────────────────────────────────────
 подмножество спецификаций:byte режим,ECC L、Версия 1-5（все блоки с одиночной коррекцией, без межблочного перемежения), фиксированная маска 0。
 Целостность: спецификация допускает любую маску (декодер снимает маску по битам информации о формате), фиксированная маска не влияет
 сканируемость; уже использовано python qrcode Библиотека выполняет попиксельную кросс-валидацию для многовходовых многоверсионных данных (принудительно byte
 Режим + mask 0，5/5 все diff=0）。Панель CSP Разрешено только self，Внешняя ссылка QR Сервис недоступен. */
// qr_gen.js —— Упрощенный QR кодер (для браузера + node можно запустить кросс-валидацию)
// подмножество спецификаций:byte режим,ECC L、Версия 1-5（все блоки с одиночной коррекцией, без межблочного перемежения), фиксированная маска 0。
// Примечание о целостности: спецификация разрешает кодеру выбирать любую маску (декодер снимает маску сам по битам информации о формате),
// Фиксированная маска не влияет на сканируемость; код купона — короткий текст,v1-5（26 байт) более чем достаточно.

// GF(256) логарифм/Таблица экспонент (примитивный полином 0x11d）
const QR_EXP = new Array(512), QR_LOG = new Array(256);
(() => {
 let x = 1;
 for (let i = 0; i < 255; i++) { QR_EXP[i] = x; QR_LOG[x] = i; x <<= 1; if (x & 0x100) x ^= 0x11d; }
 for (let i = 255; i < 512; i++) QR_EXP[i] = QR_EXP[i - 255];
})();
const gmul = (a, b) => (a && b) ? QR_EXP[QR_LOG[a] + QR_LOG[b]] : 0;

// Параметры версий (индекс = Версия-1）：[кол-во токенов данных, число символов кода коррекции]，ECC L Один блок
const QR_V = [[19, 7], [34, 10], [55, 15], [80, 20], [108, 26]];
// Выровнять координаты центра паттерна (v2+；позиции, перекрывающиеся с поисковым паттерном, при размещении пропускаются)
const QR_ALIGN = [[], [6, 18], [6, 22], [6, 26], [6, 30]];
const QR_MASK = (r, c) => (r + c) % 2 === 0; // Режим маски 0

// сгенерировать полином (коэффициент старшей степени первым,g[0] Всегда равно 1）
function qrGenPoly(deg) {
 let g = [1];
 for (let i = 0; i < deg; i++) {
 const a = QR_EXP[i], ng = new Array(g.length + 1).fill(0);
 ng[0] = g[0];
 for (let j = 1; j < g.length; j++) ng[j] = g[j] ^ gmul(a, g[j - 1]);
 ng[g.length] = gmul(a, g[g.length - 1]);
 g = ng;
 }
 return g;
}

// Reed-Solomon Вычисление остатка (синтетическое деление), возвращает deg кодовых слов коррекции ошибок
function rsRem(data, deg) {
 const g = qrGenPoly(deg);
 const res = data.concat(new Array(deg).fill(0));
 for (let i = 0; i < data.length; i++) {
 const f = res[i];
 if (f) for (let j = 0; j < g.length; j++) res[i + j] ^= gmul(g[j], f);
 }
 return res.slice(data.length);
}

// текст → Поток токенов (byte режим:0100 + 8 битовый счётчик + данные + терминатор + 0xEC/0x11 заполнение)
function qrDataCodewords(text, dataCap) {
 const bytes = Array.from(new TextEncoder().encode(text));
 const bits = [];
 const push = (val, n) => { for (let i = n - 1; i >= 0; i--) bits.push((val >> i) & 1); };
 push(4, 4); // byte Режим
 push(bytes.length, 8); // v1-9 Счетчик 8 бит
 for (const b of bytes) push(b, 8);
 const cap = dataCap * 8;
 push(0, Math.min(4, cap - bits.length)); // терминатор
 while (bits.length % 8) bits.push(0);
 const out = [];
 for (let i = 0; i < bits.length; i += 8) {
 let v = 0; for (const b of bits.slice(i, i + 8)) v = (v << 1) | b;
 out.push(v);
 }
 for (let p = 0; out.length < dataCap; p ^= 1) out.push(p ? 0x11 : 0xEC);
 return out;
}

// главная точка входа:text → булева матрица (true=тёмный модуль)
function qrMatrix(text) {
 const bytes = Array.from(new TextEncoder().encode(text));
 // Выбор версии: требуется ≈ 2 префикс кода + длина текста, брать первую помещающуюся версию
 let ver = 0;
 for (let v = 0; v < QR_V.length; v++) { if (bytes.length + 2 <= QR_V[v][0]) { ver = v + 1; break; } }
 if (!ver) throw new Error('QR: text too long (>' + QR_V[4][0] + ' bytes)');
 const [dataCap, ecCap] = QR_V[ver - 1];
 const n = 17 + 4 * ver;

 const M = Array.from({ length: n }, () => new Array(n).fill(false));
 const F = Array.from({ length: n }, () => new Array(n).fill(false)); // заглушка функционального модуля

 const setF = (r, c, v) => { M[r][c] = v; F[r][c] = true; };
 // маркер позиционирования + разделительная полоса
 const finder = (r0, c0) => {
 for (let r = -1; r <= 7; r++) for (let c = -1; c <= 7; c++) {
 const rr = r0 + r, cc = c0 + c;
 if (rr < 0 || cc < 0 || rr >= n || cc >= n) continue;
 const dark = r >= 0 && r <= 6 && c >= 0 && c <= 6 && (r === 0 || r === 6 || c === 0 || c === 6 || (r >= 2 && r <= 4 && c >= 2 && c <= 4));
 setF(rr, cc, dark);
 }
 };
 finder(0, 0); finder(0, n - 7); finder(n - 7, 0);
 // калибровочная фигура (только между двумя позиционными метками:8..n-9，Не должно перекрывать сам позиционный паттерн)
 for (let r = 8; r <= n - 9; r++) setF(r, 6, r % 2 === 0);
 for (let c = 8; c <= n - 9; c++) setF(6, c, c % 2 === 0);
 // паттерн выравнивания (v2+，пропуск и перекрытие позиционирования)
 const align = QR_ALIGN[ver - 1] || [];
 for (const ar of align) for (const ac of align) {
 if (F[ar][ac]) continue;
 for (let r = -2; r <= 2; r++) for (let c = -2; c <= 2; c++)
 setF(ar + r, ac + c, Math.max(Math.abs(r), Math.abs(c)) !== 1);
 }
 // скрытый модуль + Информация о формате (ECC L=01，маска 0）——BCH(15,5) + 0x5412 XOR.
 // Порядок бит соответствует спецификации (с python qrcode побитная сверка):bit i Из LSB количество,
 // Копия 1 — в левый верхний угол L форма, копия 2 — вправо-вниз L форма.
 let fmt = (1 << 3) | 0; // L<<3 | mask
 let rem = fmt << 10;
 for (let i = 14; i >= 10; i--) if ((rem >> i) & 1) rem ^= 0x537 << (i - 10);
 fmt = ((fmt << 10) | rem) ^ 0x5412; // 15 бит
 const fb = i => (fmt >> i) & 1;
 // копия 1 (слева вверху): бит 0..5 → (i,8)；6 → (7,8)；7 → (8,8)
 for (let i = 0; i <= 5; i++) setF(i, 8, !!fb(i));
 setF(7, 8, !!fb(6)); setF(8, 8, !!fb(7));
 // копия при продлении + Копия 2 (правый низ): поз. 8..14 → (n-15+i, 8)；бит 0..7 → (8, n-1-i)；8 → (8,7)；9..14 → (8,14-i)
 for (let i = 8; i <= 14; i++) setF(n - 15 + i, 8, !!fb(i));
 for (let i = 0; i <= 7; i++) setF(8, n - 1 - i, !!fb(i));
 setF(8, 7, !!fb(8));
 for (let i = 9; i <= 14; i++) setF(8, 14 - i, !!fb(i));
 // темный модуль (всегда темный, в конце вертикального сегмента копии 1)
 setF(n - 8, 8, true);


 // Кодовые слова данных + Кодовые слова коррекции ошибок → битовый поток
 const dcw = qrDataCodewords(text, dataCap);
 const cw = dcw.concat(rsRem(dcw, ecCap));
 const bits = [];
 for (const b of cw) for (let i = 7; i >= 0; i--) bits.push((b >> i) & 1);

 // змеевидное размещение (парные столбцы, справа налево, пропустить 6 столбец), при записи данных — прямое XOR-маскирование
 let bi = 0, up = true;
 for (let x = n - 1; x > 0; x -= 2) {
 if (x === 6) x--;
 for (let i = 0; i < n; i++) {
 const r = up ? n - 1 - i : i;
 for (const c of [x, x - 1]) {
 if (F[r][c]) continue;
 const bit = bi < bits.length ? bits[bi++] : 0;
 M[r][c] = bit ? !QR_MASK(r, c) : QR_MASK(r, c);
 }
 }
 up = !up;
 }
 return M;
}

// Матрица → SVG（quiet zone 4 модуль)
function qrSVG(M, px) {
 const n = M.length, q = 4, total = n + q * 2;
 let s = '<svg viewBox="0 0 ' + total + ' ' + total + '" width="' + px + '" height="' + px + '" shape-rendering="crispEdges" role="img" style="background:#fff">';
 for (let r = 0; r < n; r++) for (let c = 0; c < n; c++)
 if (M[r][c]) s += '<rect x="' + (c + q) + '" y="' + (r + q) + '" width="1" height="1"/>';
 return s + '</svg>';
}

/* ── Запрос кодов купонов к началу учебного года (попап, имитация промо-страницы #/prizes?tab=vouchers）──────────── */
/* copyText：clipboard API Только в secure context（https/localhost）доступно,
 Удалённый http панель не получит navigator.clipboard → Деградация execCommand。 */
function copyText(text) {
 if (navigator.clipboard && window.isSecureContext) return navigator.clipboard.writeText(text);
 return new Promise((resolve, reject) => {
 const ta = document.createElement('textarea');
 ta.value = text;
 ta.style.cssText = 'position:fixed;opacity:0';
 document.body.appendChild(ta);
 ta.select();
 try { document.execCommand('copy') ? resolve() : reject(new Error('copy failed')); }
 catch (e) { reject(e); }
 finally { ta.remove(); }
 });
}

function vcCard(v) {
 const expired = v.valid_to && new Date(v.valid_to) < new Date();
 return '<div class="vc' + (expired ? ' expired' : '') + '">' +
 '<div class="hd"><span class="nm">' + esc(v.prize_name || v.sku_code || 'Купон') + '</span>' +
 (expired ? '<span class="tag bad">уже истек</span>' : '<span class="tag ok">Можно использовать</span>') + '</div>' +
 '<div class="meta">' +
 (v.valid_to ? 'Срок действия до ' + esc(v.valid_to) : 'Долгосрочно валидно') +
 (v.granted_at ? ' · ' + esc(v.granted_at.slice(0, 10)) + ' Выпал' : '') +
 '</div>' +
 '<div class="sep"></div>' +
 '<div class="ft"><span class="lab">Код купона</span><code>' + esc(v.code || '-') + '</code>' +
 '<span class="acts">' +
 (v.code ? '<button class="xs ghost" data-qr="' + esc(v.code) + '">QR-код</button>' : '') +
 '<button class="xs ghost" data-copy="' + esc(v.code || '') + '">Копировать</button>' +
 '</span></div>' +
 '</div>';
}

async function loadSchoolVouchers() {
 const body = $('vcBody');
 $('vcVeil').classList.add('on');
 body.innerHTML = '<div class="state"><span class="dots">Идет запрос</span></div>';
 $('vcNote').textContent = '';
 try {
 const d = await api('school/vouchers');
 const arr = d.accounts || [];
 const ok = arr.filter(a => !a.error);
 const total = ok.reduce((n, a) => n + (a.vouchers || []).length, 0);
 body.innerHTML = ok.filter(a => (a.vouchers || []).length).map(a =>
 '<div class="vc-acct"><span class="nm">' + esc(a.nickname || a.uid) + '</span>' +
 '<span>' + a.vouchers.length + ' шт.</span></div>' +
 a.vouchers.map(vcCard).join('')
 ).join('') || '<div class="empty"><div class="big">🎟️</div>купон еще не вытянут</div>';
 $('vcNote').textContent = total ? total + ' шт. купонов · ' + ok.filter(a => !(a.vouchers || []).length).length + ' аккаунтов не выбрано' : '';
 const errs = arr.filter(a => a.error);
 if (errs.length) {
 body.insertAdjacentHTML('beforeend', '<div class="note" style="color:var(--warn);margin-top:8px">Ошибка запроса:' +
 errs.map(a => esc(a.nickname || a.uid.slice(0, 8)) + '（' + esc(a.error) + '）').join('、') + '</div>');
 }
 body.querySelectorAll('button[data-copy]').forEach(b => b.onclick = async () => {
 try { await copyText(b.dataset.copy); toast('Код купона скопирован', 'ok'); }
 catch (e) { toast('копирование не удалось, выберите код купона вручную', 'err'); }
 });
 // QR-код: тело кода купона кодируется как QR（Предъявить в магазине для сканирования), переключение отображения по клику/Скрыть
 body.querySelectorAll('button[data-qr]').forEach(b => b.onclick = () => {
 const card = b.closest('.vc');
 const old = card.querySelector('.vc-qr');
 if (old) { old.remove(); return; }
 const box = document.createElement('div');
 box.className = 'vc-qr';
 try { box.innerHTML = qrSVG(qrMatrix(b.dataset.qr), 148); }
 catch (e) { box.innerHTML = '<span class="note">Ошибка генерации QR-кода:' + esc(e.message) + '</span>'; }
 card.appendChild(box);
 });
 } catch (e) {
 body.innerHTML = '<div class="state err">' + esc(e.message) + '</div>';
 }
}
$('btnSchoolVouchers').onclick = loadSchoolVouchers;
$('btnVcClose').onclick = () => $('vcVeil').classList.remove('on');
$('btnVcRefresh').onclick = loadSchoolVouchers;

/* Очередь задач роста.lastQueueSeq Запись поколений очередей, запущенных на этой странице: остаток после выполнения items
 （running=false Но seq остановка на старом значении) без обратной записи в представление — иначе результаты сканирования 3 секунд спустя предыдущим раундом
 перекрытие состояния очереди. */
let queueTimer = null, lastQueueSeq = 0;
const GROWTH_TITLES = {}; // code → отображаемое имя (подтягивается из списка задач при сканировании)
$('btnScanAll').onclick = async () => {
 const b = $('btnScanAll');
 // отключить опрос очереди: явное сканирование = Переключиться на представлениеЗадачи. Иначе следующий в очереди in-transit tick просканирует
 // результат сбрасывает и перерисовывает представление очереди (выполнение на сервере не затрагивается, просто прекращается realtime-запись в это представление).
 if (queueTimer) { clearInterval(queueTimer); queueTimer = null; }
 b.disabled = true; b.textContent = 'Сканирование…';
 try {
 const d = await api('tasks/scan_all', { method: 'POST' });
 renderQueue(groupItems(d), null, 'нет pending задач 🎉', 'Задания роста и акция «Снова в школу» для всех аккаунтов выполнены, приходите завтра.');
 } catch (e) { toast(e.message, 'err'); }
 finally { b.disabled = false; b.textContent = 'Сканирование очереди задач'; }
};
$('btnRunQueue').onclick = async () => {
 const conc = Number($('qcConc').value) || 1;
 if (!confirm('Сканировать все pending-задачи аккаунтов и ставить в очередь (конкурентность по аккаунтам ' + conc + '，внутри аккаунта последовательно).\nЗадачи с реальными диалогами выполняются дольше, продолжить?')) return;
 const b = $('btnRunQueue');
 b.disabled = true; b.textContent = 'Запуск…';
 try {
 const r = await api('tasks/run_queue', { method: 'POST', body: JSON.stringify({ concurrency: conc }) });
 if (!r.started) { toast(r.message || 'нет pending задач', 'ok'); return; }
 lastQueueSeq = r.seq || 0;
 toast('очередь запущена:' + r.total + ' элементов (параллельность ' + conc + '）', 'ok');
 startQueuePolling();
 } catch (e) { toast(e.message, 'err'); }
 finally { b.disabled = false; b.textContent = 'Выполнить все задачи'; }
};
// результат сканирования → Элементы группы (без статуса выполнения)
function groupItems(d) {
 const groups = [];
 for (const a of (d.accounts || [])) {
 const rows = [];
 for (const t of (a.growth || [])) {
 GROWTH_TITLES[t.task_code] = t.title || t.task_code;
 rows.push({ kind: 'growth', code: t.task_code, prog: t.target ? t.current + '/' + t.target : '—', status: 'scan' });
 }
 if (rows.length) groups.push({ uid: a.uid, nick: a.nickname, rows });
 }
 return groups;
}
const ST_WORDS = { done: 'Завершено', running: 'Выполняется', error: 'ошибка', skipped: 'пропустить', pending: 'Очередь', scan: 'Ожидает выполнения' };
function qrowHTML(it) {
 const isSchool = it.kind === 'school';
 const title = isSchool ? 'закрытие цикла к началу учебного года' : (GROWTH_TITLES[it.code] || it.code);
 const dotCls = it.status === 'scan' ? 'wait' : it.status === 'running' ? 'run' : it.status === 'error' ? 'err' : it.status === 'skipped' ? 'skip' : it.status === 'done' ? 'done' : 'wait';
 const stWord = it.status === 'scan' ? 'Ожидает выполнения' : (ST_WORDS[it.status] || it.status);
 return '<div class="qrow" title="' + esc(it.message || '') + '">' +
 '<span class="code">' + esc(it.code) + '</span>' +
 '<span class="name"><span class="t">' + esc(title) + '</span>' + (isSchool ? '<span class="tag mute">Сезон начала учёбы</span>' : '') + '</span>' +
 '<span class="prog">' + esc(it.prog || '') + '</span>' +
 '<span class="st"><span class="qdot ' + dotCls + '"></span>' + stWord + '</span>' +
 '<span class="msg">' + esc(it.message || '') + '</span>' +
 '</div>';
}
function renderQueue(groups, progress, emptyTitle, emptyDesc) {
 const empty = $('tcEmpty'), list = $('qcList');
 if (!groups.length) {
 empty.style.display = '';
 if (emptyTitle) empty.querySelector('.t').textContent = emptyTitle;
 if (emptyDesc) empty.querySelector('.d').textContent = emptyDesc;
 list.innerHTML = '';
 $('qProg').hidden = true; $('qcSummary').textContent = '';
 return;
 }
 empty.style.display = 'none';
 empty.style.display = 'none';
 let total = 0;
 list.innerHTML = groups.map(g => {
 total += g.rows.length;
 return '<div class="qgroup"><header><span class="nm">' + esc(g.nick || g.uid.slice(0, 12)) + '</span><span class="cnt">' + g.rows.length + ' задач в ожидании</span></header>' +
 g.rows.map(qrowHTML).join('') + '</div>';
 }).join('');
 $('qcSummary').textContent = total + ' Пункт';
 updateProgress(progress);
}
function updateProgress(q) {
 if (!q || !q.items) { $('qProg').hidden = true; return; }
 const total = q.items.length;
 const done = q.items.filter(it => it.status === 'done' || it.status === 'error' || it.status === 'skipped').length;
 $('qProg').hidden = false;
 $('qBarFill').style.width = (total ? Math.round(done / total * 100) : 0) + '%';
 $('qProgText').textContent = (q.running ? 'Выполняется ' : 'Уже завершено ') + done + ' / ' + total;
}
// Состояние очереди → Группировка (round-robin при исполнении)
function groupsFromQueue(items) {
 const by = new Map();
 for (const it of items) {
 if (!by.has(it.uid)) by.set(it.uid, { uid: it.uid, nick: it.nickname, rows: [] });
 by.get(it.uid).rows.push({
 kind: it.kind, code: it.code,
 prog: it.kind === 'school' ? '—' : '',
 status: it.status, message: it.message,
 });
 }
 return Array.from(by.values());
}
function startQueuePolling() {
 if (queueTimer) clearInterval(queueTimer);
 queueTimer = setInterval(async () => {
 let q;
 try { q = await api('tasks/queue'); } catch (e) { return; }
 if (!q.started) return;
 // Рендерит только очередь, запущенную на этой странице (после обновления страницы старые очереди не подхватываются).
 if (lastQueueSeq && q.seq !== lastQueueSeq) return;
 if (q.running) {
 renderQueue(groupsFromQueue(q.items || []), q);
 return;
 }
 // завершение: финальное состояние рендерится только один раз, затем таймер останавливается. Оставшиеся после этого items（running=false）Больше не
 // обратная запись представления — ранее результат «сканирования задач», только что открытый пользователем, в следующем tick Перезатереть.
 renderQueue(groupsFromQueue(q.items || []), q);
 clearInterval(queueTimer); queueTimer = null;
 toast('Выполнение очереди задач завершено', 'ok');
 }, 3000);
}
// reattachQueueView При возврате к представлению центра задач восстановить прогресс очереди: только если очередь, запущенная на этой странице, всё ещё
// выполнение только тогда перезапускает опрос (остаточное состояние/очередь другой страницы не перехватывает — представление не затирается старыми результатами).
function reattachQueueView() {
 // Полностью асинхронно:go() на верхнем уровне (app.js ~143 строка) при вызове, ниже в этом файле let/const
 //（queueTimer/lastQueueSeq и т.д.) еще не инициализировано — синхронное чтение сразу TDZ ReferenceError
 // прерывает весь скрипт.await только после трогать их (старый pollQueueOnce Именно за счет префикса await
 // повезло и пронесло).queueTimer "уже выполняется"проверка также перенесена в await после семантика не меняется.
 (async () => {
 try {
 const q = await api('tasks/queue');
 if (queueTimer) return; // опрос уже запущен (межпредставленческий, не прерывается)
 if (q.started && q.running && (!lastQueueSeq || q.seq === lastQueueSeq)) startQueuePolling();
 } catch (e) { /* тишина */ }
 })();
}

/* ── Расход ─────────────────────────────────────────────────────────── */
/* Графики — нативные SVG От руки: панель — это go:embed Один файл, без сборки, подключить библиотеку графиков
 Придётся тянуть сборщик — нецелесообразно. Здесь нужна только столбчатая диаграмма с накоплением, достаточно 20 строк. */

function fmtTok(n) {
 n = Number(n || 0);
 if (n >= 1e9) return (n / 1e9).toFixed(2) + 'B';
 if (n >= 1e6) return (n / 1e6).toFixed(2) + 'M';
 if (n >= 1e3) return (n / 1e3).toFixed(1) + 'k';
 return String(n);
}
function fmtMs(ms) {
 ms = Number(ms || 0);
 if (!ms) return '—';
 if (ms >= 1000) return (ms / 1000).toFixed(2) + 's';
 return Math.round(ms) + 'ms';
}
function fmtRate(r) { return r ? Number(r).toFixed(1) + ' tok/s' : '—'; }
function trimFixed(s) {
 if (!String(s).includes('.')) return String(s);
 return String(s).replace(/0+$/, '').replace(/\.$/, '');
}
function fmtCredit(n) {
 const v = Number(n || 0);
 if (!Number.isFinite(v)) return '—';
 return trimFixed(v.toFixed(2));
}
function fmtCreditRatio(v, samples, tokens) {
 if (!samples || !tokens) return '—';
 const n = Number(v || 0);
 if (!Number.isFinite(n)) return '—';
 return trimFixed(n.toFixed(4)) + ' / 1M';
}
function fmtModelRate(rate) {
 const s = String(rate || '').trim();
 return s ? 'x' + s : '—';
}

function usStat(v, k, cls) {
 return '<div class="stat ' + (cls || '') + '"><div class="v">' + esc(v) +
 '</div><div class="k">' + esc(k) + '</div></div>';
}

/* usKpi Карточки метрик страницы расхода. По сравнению с пулом аккаунтов .stat Еще два: семантическая палитра (cls）и подзаголовок (sub，
 поместить"Доля / Средняя скорость"такие поясняющие числа);bar является элементом внутри карточки HTML，передавать только при необходимости. */
function usKpi(v, k, cls, sub, bar) {
 return '<div class="kpi ' + (cls || '') + '">' +
 '<div class="k">' + esc(k) + '</div>' +
 '<div class="v">' + esc(v) + '</div>' +
 (bar || '') +
 (sub ? '<div class="s">' + esc(sub) + '</div>' : '') +
 '</div>';
}

/* usMixBar prompt/completion полоса доли. Ширина в процентах, а не в фиксированных пикселях: ширина колонок таблицы меняется с окном,
 Ширина в пикселях на узком экране переполняется, на широком выглядит пусто. */
function usMixBar(prompt, completion, total) {
 const t = Number(total || 0);
 if (!t) return '';
 const pp = Math.max(0, Math.min(100, Number(prompt || 0) / t * 100));
 const pc = Math.max(0, Math.min(100, Number(completion || 0) / t * 100));
 return '<span class="us-mix" title="prompt ' + pp.toFixed(1) + '% · completion ' + pc.toFixed(1) + '%">' +
 '<i class="p" style="width:' + pp.toFixed(2) + '%"></i>' +
 '<i class="c" style="width:' + pc.toFixed(2) + '%"></i>' +
 '</span>';
}

/* usPct Текст доли (0 значение не отображается "0.0%"，прямо —，избежать строки полностью из нулей). */
function usPct(part, total) {
 const t = Number(total || 0);
 if (!t) return '—';
 return (Number(part || 0) / t * 100).toFixed(1) + '%';
}

/* usRow Сгенерировать строку.mid — доп. ячейка после «Имя» и перед кол-вом запросов (напр. столбец «Домен»).
 withPerf Управление задержкой/два столбца скорости; переключатели столбцов передаются явно, чтобы избежать рассинхрона заголовка при изменении вызывающей стороной. */
function usRow(name, sub, a, mid, withPerf) {
 return '<tr>' +
 '<td class="mark" aria-hidden="true"></td>' +
 '<td>' + esc(name) + (sub ? '<div class="note">' + esc(sub) + '</div>' : '') + '</td>' +
 (mid || '') +
 '<td class="num">' + fmtTok(a.requests) + '</td>' +
 '<td class="num">' + (a.errors ? '<span style="color:var(--warn)">' + fmtTok(a.errors) + '</span>' : '—') + '</td>' +
 '<td class="num">' + fmtTok(a.prompt_tokens) + '</td>' +
 '<td class="num">' + fmtTok(a.completion_tokens) + '</td>' +
 '<td class="num">' + fmtTok(a.total_tokens) +
 usMixBar(a.prompt_tokens, a.completion_tokens, a.total_tokens) + '</td>' +
 (withPerf
 ? '<td class="num">' + fmtMs(a.avg_latency_ms) + '</td>' +
 '<td class="num">' + fmtRate(a.avg_tokens_per_second) + '</td>'
 : '') +
 '</tr>';
}

/* ── Детализация расхода: три измерения в одной таблице + переключение внутри страницы ──────────────────────────
 Аккаунт / Модель / три таблицы домена ранее занимали по одной box，Страница сильно вытянута по вертикали и структура заголовков почти одинакова.
 сейчас синтезировать один box：Заголовок таблицы генерируется из определения измерений, рендер строк переиспользуется usRow，переключение — ноль запросов. */
const US_DIMS = {
 account: { key: 'by_account', title: 'Аккаунт', withRealm: true, withPerf: true, span: 10 },
 model: { key: 'by_model', title: 'Модель', withRealm: false, withPerf: false, span: 7 },
 realm: { key: 'by_realm', title: 'realm', withRealm: false, withPerf: false, span: 7 },
};

// usSortRows сортировка по текущему полю по убыванию (по умолчанию сумма Token，наиболее релевантен максимальный).
function usSortRows(rows, sort) {
 const out = rows.slice();
 const num = v => { const n = Number(v || 0); return Number.isFinite(n) ? n : 0; };
 const val = a => sort === 'requests' ? num(a.requests)
 : sort === 'errors' ? num(a.errors)
 : sort === 'latency' ? num(a.avg_latency_ms)
 : num(a.total_tokens);
 out.sort((a, b) => val(b) - val(a));
 return out;
}

function usDimHead(dim) {
 const m = US_DIMS[dim] || US_DIMS.account;
 return '<tr><th class="mark" aria-hidden="true"></th><th>' + esc(m.title) + '</th>' +
 (m.withRealm ? '<th>Домен</th>' : '') +
 '<th class="num">Запрос</th><th class="num">ошибка</th>' +
 '<th class="num">Prompt</th><th class="num">Completion</th><th class="num">Итого</th>' +
 (m.withPerf ? '<th class="num">все с задержкой</th><th class="num">Средняя скорость</th>' : '') +
 '</tr>';
}

/* usTabsHtml Кнопка переключения измерений (с бейджем количества). Весь фрагмент innerHTML перезаписать, а не править поштучно class：
 Контейнера click слушатель делегированный, замена дочерних узлов не теряет события, код короче. */
function usTabsHtml(dims, active, counts) {
 return dims.map(([k, label]) => {
 const n = counts ? counts[k] : null;
 return '<button data-dim="' + k + '"' + (k === active ? ' class="on"' : '') + '>' +
 esc(label) + (n == null ? '' : '<span class="cnt">' + n + '</span>') + '</button>';
 }).join('');
}

const US_DIM_TABS = [['account', 'По аккаунту'], ['model', 'по модели'], ['realm', 'по домену']];

function renderUsageDim() {
 const d = usageData || {};
 const m = US_DIMS[usDim] || US_DIMS.account;
 const rows = usSortRows(d[m.key] || [], usSort);
 $('usDimTabs').innerHTML = usTabsHtml(US_DIM_TABS, usDim, {
 account: (d.by_account || []).length,
 model: (d.by_model || []).length,
 realm: (d.by_realm || []).length,
 });
 $('usDimHead').innerHTML = usDimHead(usDim);
 $('usDimBody').innerHTML = rows.map(x =>
 usRow(usDim === 'account' ? String(x.key || '').slice(0, 8) : x.key,
 usDim === 'account' ? (x.extra || '') : '',
 x,
 usDim === 'account' ? '<td class="num">' + esc(x.realm || '') + '</td>' : '',
 m.withPerf)
 ).join('') || '<tr><td colspan="' + m.span + '" class="empty">данных пока нет</td></tr>';
 $('usDimNote').textContent = rows.length + ' Строка · Количество запросов включает неудачные попытки';
}

/* списание баллов: по аккаунту / одна таблица на две размерности по модели (как выше, две box объединить в одно). */
function usCreditHead(dim) {
 return dim === 'model'
 ? '<tr><th class="mark" aria-hidden="true"></th><th>Модель</th><th>Множитель очков</th>' +
 '<th class="num">Запрос</th><th class="num">Списание баллов</th>' +
 '<th class="num">Валидная выборка Token</th><th class="num">Баллы / 1M Token</th></tr>'
 : '<tr><th class="mark" aria-hidden="true"></th><th>Аккаунт</th>' +
 '<th class="num">Запрос</th><th class="num">Списание баллов</th>' +
 '<th class="num">Валидная выборка Token</th><th class="num">Баллы / 1M Token</th></tr>';
}

function renderCreditDim() {
 const d = usageData || {};
 $('usCreditHead').innerHTML = usCreditHead(usCreditDim);
 const empty = 'пока нет записей списания баллов; до апгрейда содержит только Token история не подделывает баллы.';
 if (usCreditDim === 'model') {
 const models = d.credit_by_model || [];
 $('usCreditBody').innerHTML = models.map(row =>
 '<tr>' +
 '<td class="mark" aria-hidden="true"></td>' +
 '<td>' + esc(row.key || '—') + '</td>' +
 '<td>' + esc(fmtModelRate(row.rate)) + '</td>' +
 '<td class="num">' + fmtTok(row.requests) + '</td>' +
 '<td class="num">' + fmtCredit(row.credits) + '</td>' +
 '<td class="num">' + fmtTok(row.credit_tokens) + '</td>' +
 '<td class="num">' + fmtCreditRatio(row.credits_per_1m_tokens, row.credit_samples, row.credit_tokens) + '</td>' +
 '</tr>'
 ).join('') || '<tr><td colspan="7" class="empty">' + empty + '</td></tr>';
 } else {
 const accounts = d.credit_by_account || [];
 $('usCreditBody').innerHTML = accounts.map(row => {
 const uid = String(row.key || '');
 const account = row.nickname || uid.slice(0, 8) || '—';
 return '<tr>' +
 '<td class="mark" aria-hidden="true"></td>' +
 '<td>' + esc(account) + '<div class="note">' + esc(row.realm || '') + ' · ' + esc(uid.slice(0, 8)) + '</div></td>' +
 '<td class="num">' + fmtTok(row.requests) + '</td>' +
 '<td class="num">' + fmtCredit(row.credits) + '</td>' +
 '<td class="num">' + fmtTok(row.credit_tokens) + '</td>' +
 '<td class="num">' + fmtCreditRatio(row.credits_per_1m_tokens, row.credit_samples, row.credit_tokens) + '</td>' +
 '</tr>';
 }).join('') || '<tr><td colspan="6" class="empty">' + empty + '</td></tr>';
 }
 $('usCreditTabs').innerHTML = usTabsHtml([['account', 'По аккаунту'], ['model', 'по модели']], usCreditDim, {
 account: (d.credit_by_account || []).length,
 model: (d.credit_by_model || []).length,
 });
}

function renderUsage(d) {
 usageData = d || {};
 const t = usageData.totals || {};
 const total = Number(t.total_tokens || 0);
 const pt = Number(t.prompt_tokens || 0);
 const ct = Number(t.completion_tokens || 0);
 const reqs = Number(t.requests || 0);
 const errs = Number(t.errors || 0);
 const okRate = reqs ? (reqs - errs) / reqs * 100 : null;
 // для формирования записи требуется валидный CSS ширина,usPct Возвращать при отсутствии выборок "—"，Нельзя напрямую конкатенировать в style。
 const pctW = (part) => total ? Math.max(0, Math.min(100, Number(part || 0) / total * 100)).toFixed(2) + '%' : '0%';
 // Шесть карточек: основная метрика акцентным цветом,completion цветом успеха (в тон зеленым столбцам на графике),
 // ошибка/семантическая окраска задержки только при наличии значения — дашборд, где всё зелёное или всё жёлтое, не имеет акцентов.
 $('usStats').innerHTML =
 usKpi(fmtTok(reqs), 'Количество запросов', 'c-accent',
 errs ? 'из них неуспешно ' + errs + ' Раз' : 'все успешно') +
 usKpi(fmtTok(total), 'общий token', 'c-accent',
 'prompt ' + usPct(pt, total) + ' · completion ' + usPct(ct, total),
 '<div class="kbar"><i style="width:' + pctW(pt) + ';background:var(--accent)"></i>' +
 '<i style="width:' + pctW(ct) + ';background:var(--ok)"></i></div>') +
 usKpi(fmtTok(pt), 'prompt', 'c-soft', 'Доля ' + usPct(pt, total)) +
 usKpi(fmtTok(ct), 'completion', 'c-ok', 'Доля ' + usPct(ct, total)) +
 usKpi(String(errs), 'неудачная попытка', errs ? 'c-warn' : 'c-mute',
 okRate == null ? '—' : (errs ? 'Успешность ' + okRate.toFixed(1) + '%' : 'Успешность 100%')) +
 usKpi(fmtMs(t.avg_latency_ms), 'Средняя задержка', 'c-soft',
 t.avg_tokens_per_second ? 'Произношение ' + fmtRate(t.avg_tokens_per_second) : 'нет выборок rate');

 // карточки, детализация и таймлайн считаются по выбранному окну (при переключении окна цифры меняются);
 // 「вся история» вкл. 90 дневных бакетов, свёрнутых N дней назад. Здесь указана текущая метрика и точка отсчёта данных.
 const winLabel = trangeLabel('usRange');
 // Приоритет у фактического интервала из ответа сервера (при кастомном интервале это авторитетный критерий); у скользящего окна ответа нет,
 // Использовать собственный тег элемента управления.
 const rangeEcho = usageData.window_from
 ? String(usageData.window_from).replace('T', ' ').slice(0, 16) +
 (usageData.window_to ? ' → ' + String(usageData.window_to).replace('T', ' ').slice(0, 16) : ' → сейчас')
 : '';
 const note = (rangeEcho || winLabel ? (rangeEcho || winLabel) + ' · ' : '') +
 (usageData.buckets || 0) + ' бакетов' +
 (usageData.since ? ' · данные из ' + usageData.since.replace('T', ' ') : '') +
 (usageData.file_bytes ? ' · Файл ' + (usageData.file_bytes / 1024).toFixed(1) + ' KB' : '');
 $('usNote').textContent = note;
 $('usNote').title = note; // При усечении в одну строку на узком экране смотреть полностью по ховеру

 // Четыре карточки списания баллов и пояснения.
 $('usCreditStats').innerHTML =
 usKpi(fmtCredit(t.credits), 'Списание баллов', 'c-accent', 'По апстриму usage.credit Накопительно') +
 usKpi(fmtTok(t.credit_tokens), 'совпадение Token', 'c-mute', 'наблюдаемое одновременно с баллами Token') +
 usKpi(fmtCreditRatio(t.credits_per_1m_tokens, t.credit_samples, t.credit_tokens),
 'средний балл / 1M Token', 'c-ok', 'Чем ниже, тем выгоднее') +
 usKpi(String(t.credit_samples || 0), 'Валидные сэмплы баллов', 'c-mute', 'История без поля не участвует в пересчёте');
 $('usCreditNote').textContent =
 (usageData.credit_by_account || []).length + ' аккаунтов · ' +
 (usageData.credit_by_model || []).length + ' групп коэффициентов моделей · Учитываются только наблюдаемые одновременно с баллами Token';

 renderUsageDim();
 renderCreditDim();
 renderUsageChart(usageData.series || []);
}

// Переключение измерений / Элемент сортировки.
if ($('usDimTabs')) $('usDimTabs').addEventListener('click', ev => {
 const b = ev.target.closest('button[data-dim]');
 if (!b) return;
 usDim = b.dataset.dim;
 renderUsageDim();
});
if ($('usCreditTabs')) $('usCreditTabs').addEventListener('click', ev => {
 const b = ev.target.closest('button[data-dim]');
 if (!b) return;
 usCreditDim = b.dataset.dim;
 renderCreditDim();
});
if ($('usSort')) $('usSort').onchange = () => {
 usSort = $('usSort').value;
 renderUsageDim();
};

/* renderUsageChart Отрисовать стековую гистограмму.
 *
 * x Ось —**Реальная временная шкала**，Не равноудалено по порядковому номеру. Это важно: в данных есть 1 часовой
 * интервал, также существует 6~8 часовой разрыв (в периоды без запросов бакеты не создаются), равномерное распределение 8 ч
 * нарисовано как 1 часов одинаковой ширины, из-за чего "когда использовано" полностью искажается.
 *
 * более не используется preserveAspectRatio="none"：это viewBox Растянуть по ширине контейнера,
 * Столбцы и текст деформируются. Изменить на фиксированное соотношение, высота адаптируется по ширине контейнера.
 *
 * viewBox получить 1200×200（Исходный 760×180）：SVG по width:100% рендеринг, соотношение сторон определяет фактический
 * Высота — старая пропорция в 1500px в широкой основной области растянется до ~355px，При одном-двух столбцах весь блок почти
 * Пусто. Ширина viewBox сжать высоту при той же ширине до ~250px，Визуальный вес сопоставим с таблицей ниже.
 *
 * таймлайн парсить в локальном времени (бэкенд уже возвращает локальную TZ),day нажатие в день 00:00 участвует в позиционировании,
 * и hour Точки на одной непрерывной оси — дневной бакет и есть агрегация всех часов этого дня.
 */

/* parsePointTime Бэкенд- t Парсить в timestamp в миллисекундах. */
function parsePointTime(p) {
 // hour: "2026-09-16T13" day: "2026-09-16"
 const s = p.t.length === 13 ? p.t + ':00:00' : p.t + 'T00:00:00';
 const d = new Date(s);
 return isNaN(d.getTime()) ? null : d.getTime();
}

/* fmtTokTimeLabel короткая метка временного бакета, и x единая шкала оси (дневной бакет MM-DD，часовой бакет HH:00）。 */
function fmtTokTimeLabel(p) {
 const d = new Date(p.t);
 return p.scope === 'day'
 ? (d.getMonth() + 1) + '-' + String(d.getDate()).padStart(2, '0')
 : String(d.getHours()).padStart(2, '0') + ':00';
}

function renderUsageChart(series) {
 const host = $('usChart');

 // отбрасывать точки с непарсящимся временем, а не NaN Заражает всё изображение.
 const pts = [];
 for (const p of series) {
 const t = parsePointTime(p);
 if (t === null) continue;
 const pt = Number(p.prompt_tokens || 0);
 const ct = Number(p.completion_tokens || 0);
 pts.push({ t, scope: p.scope, raw: p.t, pt, ct, tt: Number(p.total_tokens || 0) || (pt + ct),
 req: p.requests || 0 });
 }
 if (!pts.length) {
 host.innerHTML = '<div class="us-empty">Нет данных об использовании. Начните диалог и обновите.</div>';
 $('usChartNote').textContent = '—';
 return;
 }

 const W = 1200, H = 200, PL = 58, PR = 14, PT = 18, PB = 30;
 const iw = W - PL - PR, ih = H - PT - PB;

 const t0 = pts[0].t;
 const t1 = pts[pts.length - 1].t;
 const span = Math.max(1, t1 - t0);

 const max = Math.max(1, ...pts.map(p => p.tt));
 const peak = pts.reduce((a, b) => (b.tt > a.tt ? b : a), pts[0]);
 const avg = pts.reduce((s, p) => s + p.tt, 0) / pts.length;
 $('usChartNote').textContent =
 pts.length + ' точек · Пик ' + fmtTok(peak.tt) + ' @ ' + fmtTokTimeLabel(peak) +
 ' · среднее ' + fmtTok(avg);

 // Ширина столбца — «минимальный реальный интервал» 70%，и зажато в допустимом диапазоне — окно растянуто до 30 При днях столбец будет
 // Становится тоньше, но не до невидимости.
 let minGap = Infinity;
 for (let i = 1; i < pts.length; i++) minGap = Math.min(minGap, pts[i].t - pts[i - 1].t);
 if (!isFinite(minGap) || minGap <= 0) minGap = span;
 const slot = iw * (minGap / span);
 const bw = Math.max(2, Math.min(30, slot * 0.7));

 // с начала и конца отступ в полширины столбца: иначе у первой и последней точек половина столбца выйдет за область отрисовки
 // （последний столбец обрезан у правого края карточки), шкала та же xOf，Метки и столбцы всегда выровнены.
 const xOf = t => PL + bw / 2 + (t - t0) / span * Math.max(1, iw - bw);
 const yOf = v => PT + ih - ih * (v / max);

 let out = '<svg viewBox="0 0 ' + W + ' ' + H + '" role="img" ' +
 'preserveAspectRatio="xMidYMid meet">';

 // Градиент столбца: верх непрозрачный, низ полупрозрачный, при наложении два сегмента различимы с первого взгляда (сплошные цвета рядом сливаются).
 // Внимание stop-color Обязательно через style а не presentation свойство —Blink/WebKit не парсить
 // внутри атрибута var()，Записать как stop-color="var(--accent)" Весь градиент станет невалидным (столбцы полностью прозрачны).
 out += '<defs>' +
 '<linearGradient id="usGradP" x1="0" y1="0" x2="0" y2="1">' +
 '<stop offset="0" style="stop-color:var(--accent);stop-opacity:1"/>' +
 '<stop offset="1" style="stop-color:var(--accent);stop-opacity:.6"/></linearGradient>' +
 '<linearGradient id="usGradC" x1="0" y1="0" x2="0" y2="1">' +
 '<stop offset="0" style="stop-color:var(--ok);stop-opacity:1"/>' +
 '<stop offset="1" style="stop-color:var(--ok);stop-opacity:.6"/></linearGradient>' +
 '</defs>';

 // y сетка осей + деление шкалы
 for (let i = 0; i <= 4; i++) {
 const y = PT + ih - (ih * i / 4);
 out += '<line class="gl" x1="' + PL + '" y1="' + y.toFixed(1) + '" x2="' + (W - PR) +
 '" y2="' + y.toFixed(1) + '"/>';
 out += '<text class="tk" x="' + (PL - 6) + '" y="' + (y + 3.5).toFixed(1) +
 '" text-anchor="end">' + fmtTok(max * i / 4) + '</text>';
 }

 // опорная линия среднего: сразу видно"аномально ли высок этот бар"，Удобнее, чем передавать только шкалу.
 // Метка слева: правая сторона часто занята пиковым столбцом (пиковый столбец обычно последний), слева не будет перекрыта.
 if (avg > 0 && avg < max) {
 const y = yOf(avg);
 out += '<line class="avg" x1="' + PL + '" y1="' + y.toFixed(1) + '" x2="' + (W - PR) +
 '" y2="' + y.toFixed(1) + '"/>';
 out += '<text class="tk-avg" x="' + (PL + 5) + '" y="' + (y - 4).toFixed(1) +
 '" text-anchor="start">среднее ' + fmtTok(avg) + '</text>';
 }

 // столбец
 const yBase = PT + ih;
 for (const p of pts) {
 const x = xOf(p.t) - bw / 2;
 const hTot = ih * (p.tt / max);
 const hP = p.tt ? hTot * (p.pt / p.tt) : 0;
 const hC = Math.max(p.tt && p.ct ? 1 : 0, hTot - hP);
 // Скругление только у верха стека (низ у оси — прямой угол, столбец выглядит как"Установить"на бейзлайне).
 // имя класса использует usbar а не bar：Полоска баллов пула аккаунтов — это .bar{height:3px}，И SVG2 внутри
 // height Да rect CSS геометрические свойства, одноименный класс сплющит каждый столбец в 3px выше (уже пройдено).
 if (hP > 0) out += '<rect class="usbar" x="' + x.toFixed(2) + '" y="' + (yBase - hP).toFixed(2) +
 '" width="' + bw.toFixed(2) + '" height="' + hP.toFixed(2) +
 '" fill="url(#usGradP)"' + (hC > 0 ? '' : ' rx="1.5"') + '/>';
 if (hC > 0) out += '<rect class="usbar" x="' + x.toFixed(2) + '" y="' + (yBase - hP - hC).toFixed(2) +
 '" width="' + bw.toFixed(2) + '" height="' + hC.toFixed(2) +
 '" fill="url(#usGradC)" rx="1.5"/>';
 out += '<title>' + esc(p.raw) + ' ' + fmtTok(p.pt) + ' prompt / ' +
 fmtTok(p.ct) + ' completion / ' + p.req + ' Раз</title>';
 }

 // Маркировка пиков: при узком столбце текст над вершиной, при широком — справа, чтобы не перекрывать столбец.
 {
 const px = xOf(peak.t);
 const py = yOf(peak.tt);
 const anchor = px > W - PR - 90 ? 'end' : 'middle';
 out += '<text class="tk-peak" x="' + Math.max(PL, Math.min(W - PR, px)).toFixed(1) +
 '" y="' + Math.max(10, py - 5).toFixed(1) + '" text-anchor="' + anchor + '">' +
 'Пик ' + fmtTok(peak.tt) + '</text>';
 }

 // x Базовая линия оси рисуется за столбцами, чтобы не перекрывать основание
 out += '<line class="ax" x1="' + PL + '" y1="' + yBase + '" x2="' + (W - PR) +
 '" y2="' + yBase + '"/>';

 // x Шкала оси: равномерно по реальному времени 6 позиций, взять эту позицию**Ближайший фактический столбец**Пометить тегом,
 // поэтому метка всегда попадает на точку с данными, не указывает на пустой интервал.
 const TICKS = Math.min(6, pts.length);
 const usedLabel = new Set();
 for (let k = 0; k < TICKS; k++) {
 const target = t0 + span * (TICKS === 1 ? 0.5 : k / (TICKS - 1));
 let bi = 0, best = Infinity;
 for (let i = 0; i < pts.length; i++) {
 const d = Math.abs(pts[i].t - target);
 if (d < best) { best = d; bi = i; }
 }
 if (usedLabel.has(bi)) continue;
 usedLabel.add(bi);
 const p = pts[bi];
 // крайние теги выровнять по краям, чтобы не обрезались
 const cx = xOf(p.t);
 const anchor = cx < PL + 14 ? 'start' : (cx > W - PR - 14 ? 'end' : 'middle');
 out += '<text class="tk" x="' + Math.max(PL, Math.min(W - PR, cx)).toFixed(1) +
 '" y="' + (PT + ih + 15) + '" text-anchor="' + anchor + '">' + esc(fmtTokTimeLabel(p)) + '</text>';
 }

 // при переходе через сутки добавить разделитель даты, чтобы «граница дня» была видна в длинном окне
 let prevDay = null;
 for (const p of pts) {
 const d = new Date(p.t).getDate();
 if (prevDay !== null && d !== prevDay) {
 const x = xOf(p.t).toFixed(1);
 out += '<line class="gl" x1="' + x + '" y1="' + PT + '" x2="' + x + '" y2="' +
 (PT + ih) + '" style="opacity:.45"/>';
 }
 prevDay = d;
 }

 out += '</svg>';
 host.innerHTML = out;
}

function fmtTokTip(v) { return fmtTok(v); }

let usageRateWarmAt = 0;
async function warmUsageModelRates() {
 if (Date.now() - usageRateWarmAt < 10 * 60 * 1000) return;
 try {
 await api('models');
 } catch (e) {
 // Обратная заливка множителя — опциональное усиление; сбой не блокирует статистику использования,10 повторите через минут.
 }
 usageRateWarmAt = Date.now();
}

async function loadUsage() {
 const q = trangeQuery('usRange', true);
 try {
 await warmUsageModelRates();
 const d = await api('usage?' + q.toString());
 renderUsage(d);
 } catch (e) {
 // при сбое очистить все три блока: если менять только график, останутся цифры предыдущего окна, выглядит как"Обновление успешно"。
 usageData = null;
 $('usChart').innerHTML = '<div class="us-empty">Ошибка чтения расхода:' + esc(e.message) + '</div>';
 $('usChartNote').textContent = '—';
 $('usStats').innerHTML = '';
 $('usCreditStats').innerHTML = '';
 $('usNote').textContent = '—';
 $('usCreditNote').textContent = '—';
 $('usDimNote').textContent = '—';
 $('usDimHead').innerHTML = '';
 $('usCreditHead').innerHTML = '';
 $('usDimTabs').innerHTML = usTabsHtml(US_DIM_TABS, usDim, null);
 $('usCreditTabs').innerHTML = usTabsHtml([['account', 'По аккаунту'], ['model', 'по модели']], usCreditDim, null);
 $('usDimBody').innerHTML = '<tr><td colspan="10" class="empty">Ошибка чтения расхода</td></tr>';
 $('usCreditBody').innerHTML = '<tr><td colspan="7" class="empty">Ошибка чтения расхода</td></tr>';
 }
}

if ($('btnUsage')) $('btnUsage').onclick = loadUsage;
// привязка контрола диапазона времени: любое изменение (переключение пресета / пользовательский период) — заново запросить расход.
if ($('usRange')) trangeBind('usRange', loadUsage);

/* ── состав баллов ─────────────────────────────────────────────────────── */
/* Баланс аккаунта — сумма нескольких пакетов баллов. Пакеты именуются по источнику («Пакет вирального роста внутреннейЭксплуатация», «Пакет привлечения новых пользователей»
 「персональная пробная версия»…), номинал от 6 До 1500 не равно, и**Выдача поштучно**。Поэтому степень выполнения двух задач
 у полностью идентичных аккаунтов баланс может отличаться на тысячи — разница только в пакетах. Здесь раскрываем детализацию по пакетам и для каждого
 Фиксированный цвет на имя пакета, одинаковый цвет при кросс-аккаунтном сравнении = один тип. */

const PK_COLORS = ['#4f8cff', '#25b08b', '#e8a33d', '#c96bd6', '#e2607a',
 '#5aa9e6', '#8fbf3f', '#b58b5a', '#7d8fa8', '#d4785c'];
const PK_ACCOUNT_COLORS = ['#4f8cff', '#25b08b', '#e8a33d', '#c96bd6',
 '#e2607a', '#20a4a4', '#8fbf3f', '#d4785c',
 '#7c83db', '#c48a2f', '#b45f8c', '#5aa9e6'];

function pkColor(i) { return PK_COLORS[i % PK_COLORS.length]; }

// pkAccountColorMap Нажать UID Стабильное назначение цветов: распределение после сортировки, обновление аккаунта/пересортировка не меняет цвет.
function pkAccountColorMap(list) {
 const uids = (list || [])
 .filter(a => a && !a.error && a.uid)
 .map(a => String(a.uid))
 .sort();
 const colors = new Map();
 uids.forEach((uid, i) => colors.set(uid, PK_ACCOUNT_COLORS[i % PK_ACCOUNT_COLORS.length]));
 return colors;
}

/* pkBySource Сгруппировать пакеты по имени, получить «источник → номинал/Баланс/количество». Это ключевое представление для сравнения:
 Разница двух аккаунтов обязательно проявится в номиналах нескольких источников. */
function pkBySource(packs) {
 const m = new Map();
 for (const p of packs) {
 // ключ группировки: code + name，а не только name：Апстрим использовал для «подарка за первый вход» и обычных акционных пакетов
 // **тот же PackageName и тот же PackageCode**，Только по name Объединит два типа в один,
 // Именно тогда «почему два аккаунта различаются 1500」причина незаметности. Здесь как минимум code занести в ключ,
 // И показывать на карточке самое раннее время выдачи.
 const k = (p.package_code || '') + '|' + (p.name || '(Без названия)');
 const e = m.get(k) || {
 key: k, name: p.name || '(Без названия)', code: p.package_code || '',
 n: 0, remain: 0, size: 0, used: 0, minEnd: '', minCreated: '',
 };
 e.n += 1;
 e.remain += Number(p.remain || 0);
 e.size += Number(p.size || 0);
 e.used += Number(p.used || 0);
 const t = (p.end_time || '').slice(0, 10);
 if (t && (!e.minEnd || t < e.minEnd)) e.minEnd = t;
 const c = (p.created_at || '').slice(0, 10);
 if (c && (!e.minCreated || c < e.minCreated)) e.minCreated = c;
 m.set(k, e);
 }
 return [...m.values()].sort((a, b) => b.size - a.size);
}

const PK_DEFAULT_DETAIL_LIMIT = 5;

function pkDetailLimitValue(raw) {
 const n = Number(raw);
 return Number.isFinite(n) && n > 0 ? Math.floor(n) : PK_DEFAULT_DETAIL_LIMIT;
}

function pkDetailLimit(cfg) {
 return pkDetailLimitValue(cfg && cfg.panel && cfg.panel.package_detail_limit);
}

const PK_DAY_MS = 24 * 3600 * 1000;

function pkExpiryMs(p) {
 const raw = Number(p && p.expires_at);
 if (Number.isFinite(raw) && raw > 0) return raw;
 const text = String((p && p.end_time) || '').trim();
 if (!text) return null;
 let iso = text.includes('T') ? text : text.replace(' ', 'T');
 if (!/(?:Z|[+-]\d\d:\d\d)$/.test(iso)) iso += '+08:00';
 const parsed = Date.parse(iso);
 return Number.isFinite(parsed) ? parsed : null;
}

// pkDetailGroups обслуживает только детализацию по пакетам одного аккаунта: пакеты с положительным балансом сначала выбирают элемент отображения по умолчанию по времени истечения,
// Остальные пакеты с положительным балансом и израсходованные сворачиваются отдельно; при одном сроке — сортировка по номиналу по убыванию.
function pkDetailCompare(a, b) {
 const sizeOf = p => {
 const n = Number(p && p.size);
 return Number.isFinite(n) ? n : 0;
 };
 const ea = pkExpiryMs(a), eb = pkExpiryMs(b);
 if (ea == null && eb != null) return 1;
 if (ea != null && eb == null) return -1;
 if (ea != null && eb != null && ea !== eb) return ea - eb;
 return sizeOf(b) - sizeOf(a);
}

function pkDetailGroups(packs, limit) {
 const active = [], used = [];
 let usedSize = 0, restSize = 0, restRemain = 0;
 for (const p of packs || []) {
 const remain = Number(p && p.remain);
 if (remain > 0) {
 active.push(p);
 continue;
 }
 used.push(p);
 const size = Number(p && p.size);
 if (Number.isFinite(size)) usedSize += size;
 }
 active.sort(pkDetailCompare);
 used.sort(pkDetailCompare);
 const visible = active.slice(0, pkDetailLimitValue(limit));
 const rest = active.slice(visible.length);
 for (const p of rest) {
 const size = Number(p && p.size);
 if (Number.isFinite(size)) restSize += size;
 const remain = Number(p && p.remain);
 if (Number.isFinite(remain)) restRemain += remain;
 }
 return { visible, rest, used, restSize, restRemain, usedSize };
}

function pkCreditOpacity(days) {
 if (days == null || !Number.isFinite(Number(days))) return 1;
 return 0.25 + 0.75 * Math.max(0, Math.min(29, Number(days) - 1)) / 29;
}

function pkExpiryText(expiresAt) {
 if (!expiresAt) return 'Без срока действия';
 const diff = expiresAt - Date.now();
 if (diff <= 0) return 'истек срок';
 const minutes = Math.max(1, Math.ceil(diff / 60000));
 if (minutes < 60) return 'остаток ' + minutes + ' минут';
 const hours = Math.ceil(diff / 3600000);
 if (hours < 24) return 'остаток ' + hours + ' ч';
 return 'остаток ' + Math.ceil(diff / PK_DAY_MS) + ' день';
}

function pkExpiryDateTime(expiresAt) {
 if (!expiresAt) return '—';
 return new Date(expiresAt).toLocaleString('zh-CN', {
 timeZone: 'Asia/Shanghai', hour12: false,
 year: 'numeric', month: '2-digit', day: '2-digit',
 hour: '2-digit', minute: '2-digit', second: '2-digit',
 });
}

function pkAccountSegments(a, now) {
 let balance = Math.max(0, Number(a.remain || 0));
 const out = [];
 for (const p of a.packages || []) {
 const remain = Number(p.remain || 0);
 if (!Number.isFinite(remain) || remain <= 0 || balance <= 0) continue;
 const amount = Math.min(balance, remain);
 const expiresAt = pkExpiryMs(p);
 out.push({
 amount,
 expiresAt,
 days: expiresAt == null ? null : Math.max(0, Math.ceil((expiresAt - now) / PK_DAY_MS)),
 source: p.name || 'Баллы',
 uid: String(a.uid || ''),
 accountName: a.nickname || String(a.uid || '').slice(0, 8) || 'Безымянный аккаунт',
 });
 balance -= amount;
 }
 return out.sort((x, y) => {
 if (x.expiresAt == null && y.expiresAt != null) return 1;
 if (x.expiresAt != null && y.expiresAt == null) return -1;
 return (x.expiresAt || 0) - (y.expiresAt || 0);
 });
}

// summarizeCreditDays Выравнивание WorkDaddy：Агрегация построчно по точному остатку дней, баланс без срока действия
// Не попадает в график и не угадывать дату истечения. Внутри аккаунта сначала ограничить суммы пакетов по общему балансу, чтобы избежать раздувания дублей вышестоящих записей.
function summarizeCreditDays(list, now) {
 const buckets = new Map();
 let unavailable = 0;
 for (const a of list || []) {
 if (a.error || !Number.isFinite(Number(a.remain))) {
 unavailable++;
 continue;
 }
 for (const segment of pkAccountSegments(a, now)) {
 if (segment.days == null) continue;
 let row = buckets.get(segment.days);
 if (!row) {
 row = { days: segment.days, credits: 0, segments: [] };
 buckets.set(segment.days, row);
 }
 row.credits += segment.amount;
 row.segments.push(segment);
 }
 }
 const rows = [...buckets.values()].sort((a, b) => a.days - b.days);
 for (const row of rows) {
 row.segments.sort((a, b) =>
 (a.expiresAt || Infinity) - (b.expiresAt || Infinity) ||
 a.accountName.localeCompare(b.accountName) ||
 a.source.localeCompare(b.source));
 }
 return { rows, accountCount: (list || []).length, unavailable };
}

function renderExpiryDistribution(list, now) {
 const summary = summarizeCreditDays(list, now);
 const colors = pkAccountColorMap(list);
 const rows = summary.rows.map(row => {
 const total = row.credits || 1;
 const nodes = row.segments.map(segment => {
 const color = colors.get(segment.uid) || 'var(--accent)';
 const title = segment.source + '\n' + fmtTok(segment.amount) + ' Баллы\nВремя истечения ' +
 pkExpiryDateTime(segment.expiresAt) + '（' + pkExpiryText(segment.expiresAt) + '）\n' +
 segment.accountName;
 return '<span class="pk-expiry-seg" style="--seg-color:' + color +
 ';opacity:' + pkCreditOpacity(segment.days).toFixed(5) +
 ';flex:' + Math.max(0.008, segment.amount / total).toFixed(4) +
 ' 1 0" title="' + esc(title) + '" aria-label="' + esc(title) + '"></span>';
 }).join('');
 return '<div class="pk-expiry-row"><span>' + esc(row.days === 0 ? 'истек срок' : row.days + ' день') +
 '</span><div class="pk-expiry-track">' + nodes + '</div><b>' + esc(fmtTok(row.credits)) +
 '</b></div>';
 }).join('');
 const foot = summary.accountCount + ' аккаунтов' +
 (summary.unavailable ? ' · ' + summary.unavailable + ' шт. баланс не получен' : '');
 const legend = (list || []).filter(a =>
 a && !a.error && a.uid && pkAccountSegments(a, now).some(s => s.days != null)
 ).map(a => '<span><i style="background:' + (colors.get(String(a.uid)) || 'var(--accent)') +
 '"></i>' + esc(a.nickname || String(a.uid).slice(0, 8)) + '</span>').join('');
 const hdr = '<div class="pk-expiry-hdr"><span>оставшиеся дни</span><span style="text-align:center">Остаток партии по каждому аккаунту</span><b>остаток баллов</b></div>';
 $('pkExpiry').innerHTML = (rows
 ? hdr + '<div class="pk-expiry-chart">' + rows + '</div>'
 : '<div class="pk-expiry-empty">Пока нет баллов для агрегации</div>') +
 (legend ? '<div class="pk-expiry-legend">' + legend + '</div>' : '') +
 '<div class="pk-expiry-foot">' + esc(foot) + '</div>';
}

function renderPackages(d, detailLimit) {
 const list = (d.accounts || []);
 const now = Date.now();
 const expiryColors = pkAccountColorMap(list);
 renderExpiryDistribution(list, now);
 if (!list.length) {
 $('pkSummary').innerHTML = '<div class="empty">Нет аккаунта</div>';
 return;
 }

 // Имя пакета → Стабильный цветовой код (единый между аккаунтами, удобно для визуального сопоставления)
 const names = [];
 for (const a of list) for (const s of pkBySource(a.packages || [])) {
 if (!names.includes(s.key)) names.push(s.key);
 }
 names.sort((x, y) => {
 const sz = n => Math.max(...list.map(a => {
 const f = pkBySource(a.packages || []).find(s => s.key === n);
 return f ? f.size : 0;
 }));
 return sz(y) - sz(x);
 });
 const colorOf = n => pkColor(names.indexOf(n));
 // Клавиша → отображаемое имя, общее для карточки и таблицы детализации (один источник — один цвет и имя).
 const labelOf = {};
 for (const a of list) for (const s of pkBySource(a.packages || [])) labelOf[s.key] = s;

 const maxRemain = Math.max(1, ...list.map(a => Number(a.remain || 0)));

 $('pkSummary').innerHTML = list.map(a => {
 if (a.error) {
 return '<div class="pk-card"><div class="who"><span class="nm">' +
 esc((a.nickname || a.uid.slice(0, 8))) + '</span>' +
 '<span class="realm">' + esc(a.realm || '') + '</span></div>' +
 '<div class="err">Ошибка запроса:' + esc(a.error) + '</div></div>';
 }
 const srcs = pkBySource(a.packages || []);
 const total = Math.max(1, Number(a.size || 0));
 const bar = srcs.map(s =>
 '<i style="width:' + (s.size / total * 100).toFixed(2) + '%;background:' +
 colorOf(s.key) + '" title="' + esc(s.name) + ' ' + fmtTok(s.size) + '"></i>'
 ).join('');
 const legend = srcs.map(s =>
 '<span><i style="background:' + colorOf(s.key) + '"></i>' +
 esc(s.name.replace(/^CodeBuddy/, '')) + ' x' + s.n + ' · ' + fmtTok(s.size) +
 (s.minCreated ? ' · Первый выпуск ' + esc(s.minCreated.slice(5)) : '') + '</span>'
 ).join('');
 const expiry = pkAccountSegments(a, now);
 const expiryTotal = Math.max(1, expiry.reduce((sum, s) => sum + s.amount, 0));
 const expiryColor = expiryColors.get(String(a.uid)) || 'var(--accent)';
 const expiryBar = expiry.length ? '<div class="expirybar" role="img" aria-label="распределение истечения баллов">' +
 expiry.map(s => {
 const title = s.source + '\n' + fmtTok(s.amount) + ' Баллы\nВремя истечения ' +
 pkExpiryDateTime(s.expiresAt) + '（' + pkExpiryText(s.expiresAt) + '）';
 return '<i style="background:' + expiryColor +
 ';opacity:' + pkCreditOpacity(s.days).toFixed(5) +
 ';flex:' + Math.max(0.008, s.amount / expiryTotal).toFixed(4) +
 ' 1 0" title="' + esc(title) + '"></i>';
 }).join('') + '</div>' : '';
 return '<div class="pk-card">' +
 '<div class="who"><span class="nm">' + esc(a.nickname || a.uid.slice(0, 8)) + '</span>' +
 '<span class="realm">' + esc(a.realm || '') + '</span></div>' +
 '<div class="big">' + fmtTok(a.remain) + '</div>' +
 '<div class="sub">Всего ' + fmtTok(a.size) + ' · ' + (a.packages || []).length +
 ' пакетов · Занимает высший ' + (Number(a.remain || 0) / maxRemain * 100).toFixed(0) + '%</div>' +
 '<div class="mixbar">' + bar + '</div>' +
 expiryBar +
 '<div class="pk-legend">' + legend + '</div>' +
 '</div>';
 }).join('');

 $('pkNote').textContent = list.length + ' аккаунтов · Запрос к апстриму в реальном времени';

 // детализация по пакетам: одна таблица на аккаунт, пакет**номинал**столбец — ключевой
 $('pkDetail').innerHTML = list.map(a => {
 if (a.error) return '';
 const groups = pkDetailGroups(a.packages || [], detailLimit);
 const rowOf = (p, rowGroup) => {
 const k = (p.package_code || '') + '|' + (p.name || '(Без названия)');
 const sub = (p.sub_product_code || '').replace(/^sp_tcaca_codebuddyide_?/, '') ||
 (p.package_code || '').replace(/^TCACA_/, '');
 return '<tr' + (rowGroup ? ' class="pk-hidden-row pk-' + rowGroup +
 '-row" data-pk-row="' + rowGroup + '" hidden' : '') +
 '><td class="mark" aria-hidden="true"><i style="background:' +
 colorOf(k) + '"></i></td>' +
 '<td>' + esc(p.name || '(Без названия)') +
 (sub ? '<div class="note">' + esc(sub) + '</div>' : '') + '</td>' +
 '<td class="num">' + fmtTok(p.size) + '</td>' +
 '<td class="num">' + fmtTok(p.remain) + '</td>' +
 '<td class="num">' + fmtTok(p.used) + '</td>' +
 '<td class="num">' + esc((p.created_at || '').slice(0, 16).replace('T', ' ') || '—') + '</td>' +
 '<td class="num">' + esc((p.end_time || '').slice(0, 10) || '—') + '</td>' +
 '</tr>';
 };
 const groupSummary = (group, label, count, size, remain) =>
 '<tr class="pk-group-summary"><td colspan="7"><button type="button" class="pk-group-toggle"' +
 ' data-pk-group="' + group + '" data-count="' + count + '" data-size="' + size +
 '" data-remain="' + remain + '" aria-expanded="false">' + label + '，Развернуть</button></td></tr>';
 const rows = groups.visible.map(p => rowOf(p, '')).join('');
 const restSummary = groups.rest.length
 ? groupSummary('rest', 'остаток не израсходован ' + groups.rest.length + ' пакетов (суммарный номинал ' +
 fmtTok(groups.restSize) + ' · остаток ' + fmtTok(groups.restRemain) + '）',
 groups.rest.length, groups.restSize, groups.restRemain) +
 groups.rest.map(p => rowOf(p, 'rest')).join('')
 : '';
 const usedSummary = groups.used.length
 ? groupSummary('used', 'Исчерпано ' + groups.used.length + ' пакетов (суммарный номинал ' +
 fmtTok(groups.usedSize) + '）', groups.used.length, groups.usedSize, 0) +
 groups.used.map(p => rowOf(p, 'used')).join('')
 : '';
 return '<div class="box"><header><h3>' +
 esc(a.nickname || a.uid.slice(0, 8)) + ' · ' + esc(a.realm || '') +
 '</h3><span class="grow"></span><span class="note">Баланс ' + fmtTok(a.remain) +
 ' / Общая сумма ' + fmtTok(a.size) + ' · доступно ' + (groups.visible.length + groups.rest.length) + ' пакетов' +
 (groups.used.length ? ' / Исчерпано ' + groups.used.length + ' шт.' : '') +
 ' · по умолчанию показывать ближайший срок истечения ' + pkDetailLimitValue(detailLimit) + ' запись</span>' +
 '</header><div class="tbl-wrap"><table class="acc"><thead><tr>' +
 '<th class="mark" aria-hidden="true"></th><th>Имя пакета / Источник</th>' +
 '<th class="num">номинал</th><th class="num">остаток</th><th class="num">Использовано</th>' +
 '<th class="num">Выдача</th><th class="num">истечение</th>' +
 '</tr></thead><tbody>' + rows + restSummary + usedSummary + '</tbody></table></div></div>';
 }).join('');
}

if ($('pkDetail')) $('pkDetail').addEventListener('click', ev => {
 const btn = ev.target.closest('button[data-pk-group]');
 if (!btn) return;
 const body = btn.closest('tbody');
 if (!body) return;
 const group = btn.dataset.pkGroup;
 const expanded = btn.getAttribute('aria-expanded') === 'true';
 body.querySelectorAll('tr[data-pk-row="' + group + '"]').forEach(row => { row.hidden = expanded; });
 const count = btn.dataset.count || '0';
 const size = btn.dataset.size || '0';
 const remain = btn.dataset.remain || '0';
 btn.setAttribute('aria-expanded', String(!expanded));
 if (group === 'rest') {
 btn.textContent = expanded
 ? 'остаток не израсходован ' + count + ' пакетов (суммарный номинал ' + fmtTok(size) + ' · остаток ' +
 fmtTok(remain) + '），Развернуть'
 : 'Свернуть остальные неиспользованные ' + count + ' пакетов';
 } else {
 btn.textContent = expanded
 ? 'Исчерпано ' + count + ' пакетов (суммарный номинал ' + fmtTok(size) + '），Развернуть'
 : 'Свернуть использованные ' + count + ' пакетов';
 }
});

async function loadPackages() {
 $('pkSummary').innerHTML = '<div class="empty">Запрос... (опрос апстрима в реальном времени по каждому аккаунту)</div>';
 $('pkDetail').innerHTML = '';
 $('pkExpiry').innerHTML = '<div class="pk-expiry-empty">Запрос…</div>';
 try {
 const [d, c] = await Promise.all([
 api('packages'),
 api('config').catch(() => null),
 ]);
 renderPackages(d, pkDetailLimit(c && c.config));
 } catch (e) {
 $('pkSummary').innerHTML = '<div class="empty">Ошибка чтения:' + esc(e.message) + '</div>';
 $('pkExpiry').innerHTML = '<div class="pk-expiry-empty">Ошибка чтения:' + esc(e.message) + '</div>';
 }
}

if ($('btnPk')) $('btnPk').onclick = loadPackages;
