// autotask.go Реализация действия задачи "Выполнить в один клик» на панели.
//
// Основание проектирования: апстрим scripts/task_*.py Вывод тестов + 2026-09-12 реверс протокола десктоп-отпечатка
// （data/desktop-task-protocol.md）——
// - first_buddy（+300 мин):report（Предварительная разблокировка)→ agreement → buddy/first
// - chat_5（+100 мин): накопительно 5 запись chat_request_send Отчёт
// - Model_chat_GLM5.2（+100 мин):accept → использовать glm-5.2 один реальный диалог → отчет (выравнивание полей модели)
// - RichMeow_Chat：десктопный отпечаток (workbuddy-desktop）Полная цепочка событий диалога, чисто API Подсвечивается (проверено на трёх аккаунтах)
// - Buddy_App / Buddy_App_QQ：buddyapp событие пяти подряд, чисто API подсвечивается (проверено на двух аккаунтах)
// - automation_1：automated_task_create_suc событие, чисто API подсвечивается (проверено на двух аккаунтах)
// - Library_read：web Домен web_element_click(library_doc_intro_click)（факт-тест на трёх аккаунтах)
// - template_5 / playbook_prompt / create_canvas：asar событие-критерий, полученное реверсом
// （template_used / playbook_prompt_send / wbx_design_canvas_*），Чистый API Подсвечивается (проверено на трёх аккаунтах)
// - expert_5 / Expert_team_use_3：Реальный список экспертов + Цепочка вызовов + Реальный chat（Сервер requestId）
// - expert_actual_use（факт-тест на трёх аккаунтах)
// - Hp_Appearance：appearance/set + appearance_skin_apply событие (проверено на двух аккаунтах)
//
// Все еще не взломано:skill_1（предположительно требуется реальный Skill вызов инструмента).
// Не делать:Expert_lighthouse（требуется реальная авторизация коннектора),Expert_Philanthropy（реальное пожертвование).
//
// все действия идемпотентны: уже claimed/Достигшие цели задачи пропускаются напрямую, без повторного расхода квоты апстрима.
package panel

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"math/rand/v2"
	"net/http"
	"strings"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/logfmt"
	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
)

// autoAction Автоматизируемое действие задачи.
type autoAction struct {
	TaskCode string // Целевая задача code
	Desc string // Описание для отображения
	Attempt bool // true = пробный тип (апстрим не подтвердил возможность скриптования, запуск может не засчитаться)
	run func(p *Panel, a *auth.Auth) (string, error)
}

// autoActions Таблица реализованных действий задач (порядок = порядок выполнения: сначала разблокировать зависимости).
// first_buddy Разблокировка зависит от активного отчета, поэтому chat_5/first_buddy выполнение всегда с report шаги.
var autoActions = []autoAction{
	{
		TaskCode: "chat_5",
		Desc: "Отчёт 5 событий активности диалога (автодобавление недостающего)",
		run: runChat5,
	},
	{
		TaskCode: "first_buddy",
		Desc: "отчёт разблокирует → принять соглашение → Получить первого Buddy（+300 мин)",
		run: runFirstBuddy,
	},
	{
		TaskCode: "Model_chat_GLM5.2",
		Desc: "Принять задачу → glm-5.2 один реальный диалог → Выравнивание отчета по моделям",
		run: runModelChat,
	},
	{
		TaskCode: "RichMeow_Chat",
		Desc: "Отчет цепочки событий десктопного отпечатка (проверено: чисто API можно подсветить, проверено на трех аккаунтах)",
		run: runRichMeow,
	},
	{
		TaskCode: "Buddy_App",
		Desc: "отчет «вход в Buddy приложения» цепочка событий (проверено: чисто API Можно подсветить)",
		run: runBuddyApp,
	},
	{
		TaskCode: "Buddy_App_QQ",
		Desc: "Отправка цепочки событий «вход в помощник учителя Penguin» (проверено: чисто API Можно подсветить)",
		run: runBuddyApp,
	},
	{
		TaskCode: "automation_1",
		Desc: "Отправка события «создание задачи по расписанию» (проверено: чисто API Можно подсветить)",
		run: runAutomationCreate,
	},
	{
		TaskCode: "Library_read",
		Desc: "Отправка события «чтение описания базы знаний» (проверено: чисто API Можно подсветить)",
		run: runLibraryRead,
	},
	{
		TaskCode: "template_5",
		Desc: "отчет группы событий «создание задачи по шаблону» ×5(Проверено: три аккаунта подсвечены)",
		run: runTemplateUse,
	},
	{
		TaskCode: "playbook_prompt",
		Desc: "репорт «отправка такого же кейса-вдохновения» Prompt」Группа событий (проверено: три аккаунта активны)",
		run: runPlaybookPrompt,
	},
	{
		TaskCode: "create_canvas",
		Desc: "Отправка группы событий «Создание креативного холста» (проверено: три аккаунта активны,+300 мин)",
		run: runCreateCanvas,
	},
	{
		TaskCode: "expert_5",
		Desc: "Вызов реального эксперта+Цепочка использования ×5（список маркета экспертов+Реальный chat，Проверено: три аккаунта подсвечены)",
		run: runExpertUse,
	},
	{
		TaskCode: "Expert_team_use_3",
		Desc: "Вызов настоящей группы экспертов+Цепочка использования ×3（Проверено: три аккаунта подсвечены)",
		run: runExpertTeamUse,
	},
	{
		TaskCode: "Hp_Appearance",
		Desc: "настроить тему API + событие активации скина (проверено: два аккаунта активированы)",
		run: runAppearance,
	},
	{
		TaskCode: "skill_1",
		Desc: "реальный диалог + skill_info Событие загрузки навыка (проверено: Renjie2 подсветка)",
		run: runSkillFresh,
	},
	{
		TaskCode: "Expert_lighthouse",
		Desc: "Вызов реального легковесного cloud-эксперта+цепочка использования (chat цепочка has_expert，Проверено: подсвечены два аккаунта)",
		run: runExpertLighthouse,
	},
	{
		TaskCode: "black_cat",
		Desc: "Сова:23:00–08:00 Внутри окна glm-5.2 Дополнение диалога (вне окна — подсказка «попробуйте позже»)",
		Attempt: true,
		run: runBlackCat,
	},
	{
		TaskCode: "school_season",
		Desc: "Кампусный день (в терминах мини-приложения):accept → mini Диалог+activityId Отчёт → Получение награды (+100c+5e）",
		run: runSchoolSeason,
	},
	{
		TaskCode: "Sequential_Tasks_1",
		Desc: "Первый диалог мини-приложения (метрика мини-приложения):accept → mini репорт диалога → Получение награды (+100c+5e）",
		run: runSequentialChat,
	},
	{
		TaskCode: "Sequential_Tasks_2",
		Desc: "выбор диалога с экспертом в мини-программе (терминология мини-программы): эксперт по рынку id → accept → expert_actual_use Отчёт → Получение награды (+200c+5e）",
		run: runMiniExpert,
	},
	{
		TaskCode: "Sequential_Tasks_3",
		Desc: "Пять диалогов мини-приложения (по метрике мини-приложения):accept → mini репорт диалога ×5（Автодоплата разницы)→ Получение награды (+300c+5e）",
		run: runSequentialChat5,
	},
	{
		TaskCode: "Sequential_Tasks_4",
		Desc: "плановая задача мини-приложения (резерв, ежедневно в 00:00 разблокируется один этап):accept → Событие создания задачи по расписанию (PC same-origin)→ получение награды (критерий ожидает проверки разблокировки)",
		run: runSequentialAutomation,
	},
	{
		TaskCode: "Sequential_Tasks_5",
		Desc: "Мини-программа использует GLM5.2（резерв):accept → с полем модели mini репорт диалога → получение награды (критерий ожидает проверки разблокировки)",
		run: runSequentialModelChat,
	},
	{
		TaskCode: "Sequential_Tasks_6",
		Desc: "мини-программа: десять диалогов (резерв):accept → mini репорт диалога ×target（Автодоплата разницы)→ Получение награды",
		run: runSequentialChat10,
	},
	{
		TaskCode: "Sequential_Tasks_7",
		Desc: "Функция вдохновения (зарезервировано, возм. PC метрика +500c+5e）：accept → группа событий Inspiration (PC+mp две формы)→ получение награды (критерий ожидает проверки разблокировки)",
		run: runSequentialPlaybook,
	},
}

// autoActionFor найти действие для задачи; если нет — вернуть nil（Автоматизация невозможна).
func autoActionFor(code string) *autoAction {
	for i := range autoActions {
		if autoActions[i].TaskCode == strings.TrimSpace(code) {
			return &autoActions[i]
		}
	}
	return nil
}

// autoActionIndex Задача в autoActions порядок в (выполнение очереди по порядку зависимостей; неизвестное возвращает большое значение).
func autoActionIndex(code string) int {
	for i := range autoActions {
		if autoActions[i].TaskCode == code {
			return i
		}
	}
	return 1 << 20
}

// mpTaskCodes Эксклюзивные задачи роста для мини-программы: по умолчанию (без mp заголовок) не появляется в списке,
// accept/claim Требуется для всех X-Client-Platform: miniprogram。при появлении новой задачи регистрировать здесь.
var mpTaskCodes = map[string]bool{
	"school_season": true, // кампус-день (mini chat + activityId）
	"Sequential_Tasks_1": true, // Первый диалог мини-приложения (mini chat，отсутствует activityId）
	"Sequential_Tasks_2": true, // выбор эксперта в мини-программе и завершение валидного диалога (mp Фингерпринт expert_actual_use）
	"Sequential_Tasks_3": true, // Мини-программа завершена 5 разговоров (и Tasks_1 той же формы,target=5 накопительно по записям)
	// Tasks_4..7 Наличие проверено тестом (accept вернуть prerequisite not met цепная зависимость;Tasks_8 not
	// found потолок). Цепочка разблокирует одно звено ежедневно в 00:00 (task locked until следующий день), критерий — issue #42
	// описание + mpsrc Предустановки формы событий, после разблокировки — поштучная калибровка.
	"Sequential_Tasks_4": true,
	"Sequential_Tasks_5": true,
	"Sequential_Tasks_6": true,
	"Sequential_Tasks_7": true,
}

// isMPTaskCode является ли задача отчёта эксклюзивной для мини-программы (определяет обратное чтение/Принять/получение награды через mp вариант).
func isMPTaskCode(code string) bool { return mpTaskCodes[code] }

// taskByCode загрузить список задач и найти одну задачу; если не найдена — вернуть nil（не считается ошибкой).
// Двойной калибр:mp Эксклюзивная задача не найдена в списке по умолчанию, авто-фолбэк mp Список (только для зарегистрированных mp код,
// неизвестный код — лишний запрос к апстриму не делать).
func (p *Panel) taskByCode(a *auth.Auth, code string) (*upstream.Task, error) {
	tasks, err := p.cfg.Upstream.ListTasks(a)
	if err != nil {
		return nil, err
	}
	for i := range tasks {
		if tasks[i].TaskCode == code {
			return &tasks[i], nil
		}
	}
	if isMPTaskCode(code) {
		return p.taskByCodeMP(a, code)
	}
	return nil, nil
}

// claimPollAttempts / claimPollGap Ограниченные параметры опроса для обратного чтения при достижении порога.
// контекст: подсчет очков апстрима — это**Асинхронно**— после отправки поведенческого события прогресс обновляется через несколько секунд (по факту Model_chat
// Сразу после завершения диалога при чтении всё ещё 0/1，Около 5-8 Изменится только через секунд 1/1）。Однократное обратное чтение даст ложное срабатывание"не достиг порога"，
// тем самым пропуск автополучения. Здесь максимум опросов N раз, интервал каждый раз gap，Общий бюджет ок. 12 с.
var (
	claimPollAttempts = 4
	claimPollGap = 3 * time.Second
)

// taskByCodeWaiting обратное чтение задачи, если не достигнуто — ожидание опросом в рамках лимита (асинхронный скоринг апстрима).
// уже достигнуто (claimable）немедленный возврат; при исчерпании бюджета вернуть последний результат (возможно, всё ещё не достиг цели).
func (p *Panel) taskByCodeWaiting(a *auth.Auth, code string) (*upstream.Task, error) {
	t, err := p.taskByCode(a, code)
	if err != nil || t == nil {
		return t, err
	}
	if t.Claimable || t.Claimed {
		return t, nil
	}
	for i := 1; i < claimPollAttempts; i++ {
		time.Sleep(claimPollGap)
		t2, err2 := p.taskByCode(a, code)
		if err2 != nil {
			return t, nil // Ошибка запроса во время опроса не перезаписывает уже полученный результат
		}
		if t2 != nil {
			t = t2
			if t.Claimable || t.Claimed {
				return t, nil
			}
		}
	}
	return t, nil
}

// taskByCodeMP получить список задач через API мини-программы и найти одну задачу; если не найдено — вернуть nil。
// задачи только для мини-программы (school_season / Sequential_Tasks_1）Не отображается в списке дефолтных метрик.
func (p *Panel) taskByCodeMP(a *auth.Auth, code string) (*upstream.Task, error) {
	tasks, err := p.cfg.Upstream.ListTasksMP(a)
	if err != nil {
		return nil, err
	}
	for i := range tasks {
		if tasks[i].TaskCode == code {
			return &tasks[i], nil
		}
	}
	return nil, nil
}

// acceptWithVerifyMP accept и обратное чтение для проверки регистрации: апстрим существует 200+OK Но accept не реально
// Форма регистрации (при этом все события-отчёты не зачисляются, задания никогда не подсвечиваются, upstream task_runner c793ae3
// факт. проверка) — решение по обратному чтению accept_status считать эталоном, не вступило в силу — повторить один раз.
func (p *Panel) acceptWithVerifyMP(a *auth.Auth, code string) bool {
	for attempt := 1; attempt <= 2; attempt++ {
		if err := p.cfg.Upstream.AcceptTasksMP(a, []string{code}); err != nil {
			log.Printf("autotask %s %s: accept Попытка%d: %v", logfmt.Label(a.UID, a.Nickname), code, attempt, err)
			continue
		}
		time.Sleep(mpActionGap)
		t, err := p.taskByCodeMP(a, code)
		if err == nil && t != nil && t.AcceptStatus != "not_accepted" && t.AcceptStatus != "" {
			return true
		}
		log.Printf("autotask %s %s: accept Попытка%d не зарегистрировано, вступает в силу (обратное чтение=%q）", logfmt.Label(a.UID, a.Nickname), code, attempt, acceptStatusOr(t))
	}
	return false
}

// acceptStatusOr Безопасное чтение задачи accept_status（nil Возврат задачи "?"）。
func acceptStatusOr(t *upstream.Task) string {
	if t == nil {
		return "?"
	}
	if t.AcceptStatus == "" {
		return "?"
	}
	return t.AcceptStatus
}

// mpActionGap mp Интервал записи задач (accept/Отчёт/между получением наград, защита от флуда).
var mpActionGap = 2 * time.Second

// mpChatEventGap mp событие диалога (chat_request_send）интервал в человеческом ритме. Апстрим к
// Sequential_Tasks_3「5 валидных диалогов» с античит-проверкой: события, отправленные с интервалом в секунды, сначала засчитываются
// Прогресс (обратное чтение 5/5、accept_status даже кратковременно переходит в completed），впоследствии признано недействительным — полный откат
// （Откат прогресса,claim вернуть 400 "task not completed"）——2026-09-26 На практике 2s Серийная отправка
// 4 записей полностью нет,45s с интервалом поштучно отчитываться, все живы и claim +300c+5e успешно. Перед отправкой каждой записи
// sleep gap + 0~10s джиттер; ждать и первую (повторная отправка сразу после отката остаточного прогресса предыдущего раунда тоже неэффективна).
var mpChatEventGap = 45 * time.Second

// runMPMiniChatTask growth универсальный цикл для ограниченных задач мини-программы домена:
// mp Запрос → accept（с проверкой обратным чтением после регистрации)→ mini chat Отправка событий (withActivityId Решение
// с признаком back-to-school activityId：school_season обязательно,Sequential_Tasks_1 без — сервер по
// source=mini_program Связь по отпечатку)→ обратное чтение → выполнил условие — получи награду.
func (p *Panel) runMPMiniChatTask(a *auth.Auth, code string, withActivityId bool) (string, error) {
	t, err := p.taskByCodeMP(a, code)
	if err != nil {
		return "", err
	}
	if t == nil {
		return "mp задача не выдана по критерию (активность, возможно, завершена)", nil
	}
	if t.Claimed {
		return "Уже получено", nil
	}
	if t.AcceptStatus == "not_accepted" || t.AcceptStatus == "" {
		if !p.acceptWithVerifyMP(a, code) {
			return "accept не зарегистрировано, вступает в силу (апстрим 200+OK но в незачисленном состоянии), до следующей попытки", nil
		}
		// accept Прогресс задачи до — null（target выдача 0），Фолбэк target=1 будет недоучет —
		// Tasks_6 первый прогон, факт:accept после фактического target=10，только дополнить 1 записей — ложно считать норму выполненной и идти получать награду
		// （claim 400 task not completed）。после принятия перечитать для получения реального target/current。
		if t2, err := p.taskByCodeMP(a, code); err == nil && t2 != nil {
			t = t2
		}
	}
	// уже достигнуто (вкл. completed не получено): забрать награду напрямую.
	target := t.Target
	if target <= 0 {
		target = 1
	}
	if t.Current >= target || t.AcceptStatus == "completed" {
		credit, energy, err := p.cfg.Upstream.ClaimRewardMP(a, code)
		if err != nil {
			return "", err
		}
		return fmt.Sprintf("полученная награда (+%dc +%de）", credit, energy), nil
	}
	// критерий отчёта:Доплата разницы mini chat событий. Перед каждой sleep mpChatEventGap+Джиттер —
	// Серия запросов будет признана upstream-антифродом недействительной (см. mpChatEventGap комментарий), лучше медленно, чем ложный отчёт.
	need := target - t.Current
	for i := int64(0); i < need; i++ {
		time.Sleep(mpChatEventGap + time.Duration(rand.Int64N(int64(10*time.Second))))
		conv := fmt.Sprintf("wb2api-mp-%d-%d", time.Now().UnixMilli(), i)
		var ev map[string]any
		if withActivityId {
			ev = upstream.SchoolSeasonChatEvent(conv)
		} else {
			ev = upstream.SchoolChatTimesEvents(conv)
		}
		if err := p.cfg.Upstream.ReportMPEvent(a, ev); err != nil {
			return fmt.Sprintf("Завершено %d/%d прерывание после N отчетов: %v", i, need, err), nil
		}
	}
	// Обратное чтение (асинхронный скоринг, переиспользование ограниченного опроса claimPoll Компактная версия бюджета: два раунда с интервалом 3s）。
	for i := 0; i < 2; i++ {
		time.Sleep(claimPollGap)
		t2, err2 := p.taskByCodeMP(a, code)
		if err2 != nil || t2 == nil {
			continue
		}
		t = t2
		if t.Claimable || t.Claimed || t.Current >= target {
			break
		}
	}
	if t.Claimed {
		return "в этом раунде уже зачислено (claimed）", nil
	}
	if t.Current < target {
		return fmt.Sprintf("уже зарепорчено %d раз, но прогресс не достигнут %d/%d（асинхронный скоринг не зачислен, ретрай в следующий раз)", need, t.Current, target), nil
	}
	credit, energy, err := p.cfg.Upstream.ClaimRewardMP(a, code)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("задача подсвечена и награда получена (+%dc +%de）", credit, energy), nil
}

// runSchoolSeason Завершено school_season「«День кампуса» (growth ограничено мини-приложением домена).
// Критерий = mini chat_request_send + activityId=school_open_day_2026（отсутствует activityId
// Не подсвечивать, с school Связь домена с акцией к началу учебного года; апстрим task_runner e2e На практике +100c+5e）。
func runSchoolSeason(p *Panel, a *auth.Auth) (string, error) {
	return p.runMPMiniChatTask(a, "school_season", true)
}

// runSequentialChat Завершено Sequential_Tasks_1「выполняется внутри мини-приложения 1 действительных диалогов».
// Критерий = mini chat_request_send（отсутствует activityId，Сервер по source=mini_program
// Связь по отпечатку; апстрим task_runner На практике +100c+5e）。
func runSequentialChat(p *Panel, a *auth.Auth) (string, error) {
	return p.runMPMiniChatTask(a, "Sequential_Tasks_1", false)
}

// runSequentialChat5 Завершено Sequential_Tasks_3「выполнить внутри мини-приложения 5 действительных диалогов».
// Критерий и Sequential_Tasks_1 та же форма (mini Фингерпринт chat_request_send，отсутствует activityId），
// Только target=5——Сервер накапливает прогресс по числу отчётов, но**Требуется человеческий ритм**：Сначала подсчет серийных событий
// после отката античитом (claim 400 "task not completed"），От mpChatEventGap гарантия интервала
// （2026-09-26 Фактически:45s Пополнение по интервалу 5/5 → claim +300c+5e успешно, после получения accept_status
// =claimed стабильно без отката).
func runSequentialChat5(p *Panel, a *auth.Auth) (string, error) {
	return p.runMPMiniChatTask(a, "Sequential_Tasks_3", false)
}

// runSequentialChat10 Завершено Sequential_Tasks_6「выполнить внутри мини-приложения 10 эффективных диалогов» (зарезервировано).
// Критерий предполагает и Tasks_1/3 та же форма (mini chat_request_send），target Предоставляется задачей (обратное чтение),
// runMPMiniChatTask Досдача по разнице —issue #42 Имя target=10，ориентироваться на фактически выданное после разблокировки.
// Интервал в человеческом ритме также применим (mpChatEventGap）：9 запись × ~50s ≈ 8 минут/Аккаунт, ночная очередь допустима.
func runSequentialChat10(p *Panel, a *auth.Auth) (string, error) {
	return p.runMPMiniChatTask(a, "Sequential_Tasks_6", false)
}

// runSequentialEventTask Sequential общий каркас зарезервированных задач цепочки:mp Запрос → accept（с валидацией)
// → Отправка события-критерия (primary；не подсвечен и fallback при непустом добавить раунд)→ обратное чтение → Получение награды при достижении цели.
// Ежедневно в 00:00 разблокируется одно кольцо:locked В течение периода accept без проводки, вернуть и ждать следующего шедулинга (без ручного вмешательства).
func (p *Panel) runSequentialEventTask(a *auth.Auth, code string, primary, fallback func() error) (string, error) {
	t, err := p.taskByCodeMP(a, code)
	if err != nil {
		return "", err
	}
	if t == nil {
		return "mp Задача не выдана по данному каналу (предзадача не выполнена или активность не началась)", nil
	}
	if t.Claimed {
		return "Уже получено", nil
	}
	target := t.Target
	if target <= 0 {
		target = 1
	}
	if t.Current >= target || t.AcceptStatus == "completed" {
		credit, energy, err := p.cfg.Upstream.ClaimRewardMP(a, code)
		if err != nil {
			return "", err
		}
		return fmt.Sprintf("полученная награда (+%dc +%de）", credit, energy), nil
	}
	if t.AcceptStatus == "not_accepted" || t.AcceptStatus == "" {
		if !p.acceptWithVerifyMP(a, code) {
			return "accept Не зарегистрировано вступление в силу (задача может быть в ежедневном окне блокировки, автоповтор после разблокировки)", nil
		}
	}
	if err := primary(); err != nil {
		return fmt.Sprintf("критерий — ошибка репорта: %v", err), nil
	}
	// обратное чтение (два раунда с интервалом 3s）；Не подсвечено и есть fallback дополнительно отправить один раунд и перечитать.
	for round := 0; round < 2; round++ {
		time.Sleep(claimPollGap)
		t2, err2 := p.taskByCodeMP(a, code)
		if err2 != nil || t2 == nil {
			continue
		}
		t = t2
		if t.Claimable || t.Claimed || t.Current >= target {
			break
		}
		if round == 0 && fallback != nil {
			if err := fallback(); err != nil {
				return fmt.Sprintf("сообщить о неуспехе по резервному критерию: %v", err), nil
			}
		}
	}
	if t.Claimed {
		return "в этом раунде уже зачислено (claimed）", nil
	}
	if t.Current < target {
		return "уже отправлено, но прогресс не засчитан (форма критерия будет скорректирована после разблокировки, повтор в след. раз)", nil
	}
	credit, energy, err := p.cfg.Upstream.ClaimRewardMP(a, code)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("задача подсвечена и награда получена (+%dc +%de）", credit, energy), nil
}

// runSequentialAutomation Завершено Sequential_Tasks_4「создать отложенную задачу» (зарезервировано).
// mp в исходниках отсутствует automation Точка генерации события → критерий предположительно PC Метрика: переиспользование automation_1 тот же источник
// Событие (DesktopAutomationCreateEvent，PC задача: три аккаунта фактически проверены и активны).
func runSequentialAutomation(p *Panel, a *auth.Auth) (string, error) {
	return p.runSequentialEventTask(a, "Sequential_Tasks_4",
		func() error {
			return p.cfg.Upstream.ReportDesktopEvent(a, upstream.DesktopAutomationCreateEvent("wb2api Автоматизация"))
		}, nil)
}

// runSequentialModelChat Завершено Sequential_Tasks_5「Использовать GLM5.2」（зарезервировано).
// primary：mp событие диалога несёт requestModelId/requestModelName=glm-5.2（mpsrc main 32904
// фактическая форма точки отправки);fallback：PC Отчёт об активности доменной модели (Model_chat_GLM5.2 тот же источник).
func runSequentialModelChat(p *Panel, a *auth.Auth) (string, error) {
	return p.runSequentialEventTask(a, "Sequential_Tasks_5",
		func() error {
			conv := fmt.Sprintf("wb2api-mp-glm-%d", time.Now().UnixMilli())
			return p.cfg.Upstream.ReportMPEvent(a, upstream.MiniChatModelEvent(conv, "glm-5.2", "GLM-5.2"))
		},
		func() error {
			return p.cfg.Upstream.ReportChatActivityModel(a, fmt.Sprintf("wb2api-mp-glm-%d", time.Now().UnixMilli()), "", "glm-5.2", "GLM-5.2")
		})
}

// runSequentialPlaybook Завершено Sequential_Tasks_7「функция "Вдохновение» (резерв, возможно PC Критерий).
// primary：PC группа событий Inspiration (DesktopPlaybookPromptSequence，playbook_prompt Три аккаунта
// подтверждено тестом);fallback：mp Группа событий fingerprint-inspiration (MiniPlaybookEvents，mpsrc форма).
func runSequentialPlaybook(p *Panel, a *auth.Auth) (string, error) {
	ms := time.Now().UnixMilli()
	return p.runSequentialEventTask(a, "Sequential_Tasks_7",
		func() error {
			conv := fmt.Sprintf("wb2api-pb-%d", ms)
			req := fmt.Sprintf("wb2api-pb-req-%d", ms)
			return p.cfg.Upstream.ReportDesktopEvent(a,
				upstream.DesktopPlaybookPromptSequence(conv, req, "pm-gtm-launch-plan", "Выпуск нового продукта GTM План релиза — одна страница")...)
		},
		func() error {
			return p.cfg.Upstream.ReportMPEvent(a, upstream.MiniPlaybookEvents("pm-gtm-launch-plan", "Выпуск нового продукта GTM План релиза — одна страница")...)
		})
}

// runMiniExpert Завершено Sequential_Tasks_2「в мини-программе выбрать эксперта и завершить валидный диалог».
// Критерий = mp Фингерпринт expert_actual_use（**без** activityId/conversationId、
// extVersion=2.2.8、type=send_message——Фактическая форма из исходников мини-программы, с school Домен expert
// Не смешивать две методики подсчёта событий; апстрим task_runner фактическая отправка отчета сразу completed，claim +200c+5e）。
// Эксперт id должно быть реальным рыночным ex_ id（пустой id на сервере не учитывается)→ **accept До**Сначала разобрать маркет
// Список: если не удалось вытянуть — весь блок задач бездействует, чтобы избежать полусостояния "зарегистрировано, но не отправлено» (апстрим 9a26ae7
// ids аналогично пре-чеку). Переиспользовать существующий MarketExpertList（expert_5 задачи из одного источника, проверено на практике).
func runMiniExpert(p *Panel, a *auth.Auth) (string, error) {
	const code = "Sequential_Tasks_2"
	t, err := p.taskByCodeMP(a, code)
	if err != nil {
		return "", err
	}
	if t == nil {
		return "mp задача не выдана по критерию (активность, возможно, завершена)", nil
	}
	if t.Claimed {
		return "Уже получено", nil
	}
	target := t.Target
	if target <= 0 {
		target = 1 // Не accept mp задача progress для null，target фолбэк (проверено на апстриме)
	}
	// уже достигнуто (вкл. completed не получено): забрать награду напрямую.
	if t.Current >= target || t.AcceptStatus == "completed" {
		credit, energy, err := p.cfg.Upstream.ClaimRewardMP(a, code)
		if err != nil {
			return "", err
		}
		return fmt.Sprintf("полученная награда (+%dc +%de）", credit, energy), nil
	}
	// Префикс носителя критерия (accept До этого): реальный эксперт рынка id。
	experts, merr := p.cfg.Upstream.MarketExpertList(a, "")
	if merr != nil || len(experts) == 0 {
		return fmt.Sprintf("Маркет экспертов недоступен (%v），Пропустить во избежание полусостояния", merr), nil
	}
	e := experts[0]
	name := e.DisplayNameZH
	if name == "" {
		name = e.ProfessionZH
	}
	if t.AcceptStatus == "not_accepted" || t.AcceptStatus == "" {
		if !p.acceptWithVerifyMP(a, code) {
			return "accept не зарегистрировано, вступает в силу (апстрим 200+OK но в незачисленном состоянии), до следующей попытки", nil
		}
	}
	ev := upstream.MiniExpertUseEvent(e.ExpertID, name, e.ExpertType)
	if err := p.cfg.Upstream.ReportMPEvent(a, ev); err != nil {
		return fmt.Sprintf("Отчёт expert_actual_use ошибка: %v", err), nil
	}
	// Обратное чтение (асинхронный скоринг, два раунда с интервалом 3s——и runMPMiniChatTask тот же бюджет).
	for i := 0; i < 2; i++ {
		time.Sleep(claimPollGap)
		t2, err2 := p.taskByCodeMP(a, code)
		if err2 != nil || t2 == nil {
			continue
		}
		t = t2
		if t.Claimable || t.Claimed || t.Current >= target {
			break
		}
	}
	if t.Claimed {
		return "в этом раунде уже зачислено (claimed）", nil
	}
	if t.Current < target {
		return "Отправлено, но прогресс не зачислен (асинхронный скоринг, повтор в следующий раз)", nil
	}
	credit, energy, err := p.cfg.Upstream.ClaimRewardMP(a, code)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("задача подсвечена и награда получена (+%dc +%de）", credit, energy), nil
}

// accountTaskAuto Завершение задачи в один клик: выполнить соответствующее действие → Чтение прогресса → Сообщить результат.
func (p *Panel) accountTaskAuto(w http.ResponseWriter, r *http.Request) {
	uid := r.PathValue("uid")
	a := p.accountByUID(w, uid)
	if a == nil {
		return
	}
	var body struct {
		TaskCode string `json:"task_code"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.TaskCode == "" {
		writeErr(w, http.StatusBadRequest, "task_code required")
		return
	}
	act := autoActionFor(body.TaskCode)
	if act == nil {
		writeErr(w, http.StatusNotImplemented,
			"Задача требует взаимодействия внутри клиента (нет соответствующего API), авто-выполнение невозможно; выполните по инструкции в официальном клиенте")
		return
	}
	// per-account Мьютекс: если действие задачи того же аккаунта уже выполняется (одиночная или массовая), то сразу 409，
	// Не запускать параллельно повторно (действие идемпотентно, но expert/skill содержит реальные диалоги, повторный прогон тратит квоту).
	if !p.tryLockAccount(uid) {
		writeErr(w, http.StatusConflict, "Для этого аккаунта уже выполняется действие задачи, дождитесь завершения текущего раунда")
		return
	}
	defer p.unlockAccount(uid)
	// Предварительное чтение: выполненные задания пропускаются (идемпотентно, без вызова upstream).
	// taskByCode Уже двойная метрика (mp Авто-фолбэк эксклюзивного кода mp список).
	before, err := p.taskByCode(a, act.TaskCode)
	if err != nil {
		writeErr(w, http.StatusBadGateway, "list tasks: "+err.Error())
		return
	}
	if before == nil {
		writeErr(w, http.StatusNotFound, "у этого аккаунта нет данной задачи")
		return
	}
	isMP := isMPTaskCode(act.TaskCode)
	if before.Claimed {
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "skipped": true, "message": "награда по этой задаче уже получена"})
		return
	}
	msg, err := act.run(p, a)
	if err != nil {
		writeErr(w, http.StatusBadGateway, "ошибка выполнения: "+err.Error())
		return
	}
	// верификация обратным чтением: отчет 200 ≠ Учёт баллов (апстрим может тихо отбрасывать + асинхронный скоринг),
	// Использовать ограниченный опрос и другие асинхронные таймеры для финализации, затем решить, забирать ли награду автоматически (mp задачи перечитываются в той же метрике).
	var after *upstream.Task
	var aerr error
	if isMP {
		after, aerr = p.taskByCodeMP(a, act.TaskCode)
	} else {
		after, aerr = p.taskByCodeWaiting(a, act.TaskCode)
	}
	progressBefore, progressAfter := taskProgressText(before), ""
	claimable := false
	if aerr == nil && after != nil {
		progressAfter = taskProgressText(after)
		claimable = after.Claimable
	}
	resp := map[string]any{
		"ok": true,
		"message": msg,
		"progress_before": progressBefore,
		"progress_after": progressAfter,
		"claimable": claimable,
		"attempt": act.Attempt,
		"verify_supported": true,
	}
	// При достижении цели — автополучение награды (Web Конец claim）：взять"Завершено→Получение награды"Сведено в один шаг, повторный клик не нужен.
	if claimable {
		var credit, energy int64
		var cerr error
		if isMP {
			credit, energy, cerr = p.cfg.Upstream.ClaimRewardMP(a, act.TaskCode)
		} else {
			credit, energy, cerr = p.cfg.Upstream.ClaimReward(a, act.TaskCode)
		}
		if cerr == nil {
			resp["claimed"] = true
			resp["credit"] = credit
			resp["energy"] = energy
			if credit > 0 || energy > 0 {
				resp["message"] = msg + fmt.Sprintf(";награда уже получена автоматически +%d Разделить +%d Возможность", credit, energy)
			} else {
				resp["message"] = msg + ";награда уже получена ранее"
			}
		} else {
			resp["claim_error"] = cerr.Error()
			resp["message"] = msg + ";Условие выполнено, но получение награды не удалось, можно повторить вручную кнопкой «Получить» в списке задач"
		}
	}
	log.Printf("panel: Действие задачи uid=%s code=%s progress %s -> %s claimable=%v claimed=%v",
		uid, act.TaskCode, progressBefore, progressAfter, claimable, resp["claimed"])
	writeJSON(w, http.StatusOK, resp)
}

// taskProgressText Читаемое представление прогресса задачи (для сверки при обратном чтении).
func taskProgressText(t *upstream.Task) string {
	if t == nil {
		return "?"
	}
	if t.Target > 0 {
		return fmt.Sprintf("%d/%d", t.Current, t.Target)
	}
	if t.Claimed {
		return "claimed"
	}
	return t.AcceptStatus
}

// truncateStr обрезать текст ошибки (не прокидывать длинный ответ апстрима на фронтенд как есть).
func truncateStr(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

// ---------------------------------------------------------------------------
// реализация действий каждой задачи
// ---------------------------------------------------------------------------

// reportGap интервал между последовательными отчётами (выровнен по замерам скрипта апстрима 1.05s метрика, во избежание риск-контроля).
var reportGap = 1050 * time.Millisecond

// runChat5 Дополнить chat_5 прогресс: отчёт по дельте chat_request_send。
func runChat5(p *Panel, a *auth.Auth) (string, error) {
	t, err := p.taskByCode(a, "chat_5")
	if err != nil {
		return "", err
	}
	if t == nil {
		return "", fmt.Errorf("Задача не существует")
	}
	target := t.Target
	if target <= 0 {
		target = 5
	}
	need := target - t.Current
	if need <= 0 {
		return "Прогресс уже достигнут, отчет не требуется", nil
	}
	for i := int64(0); i < need; i++ {
		cid := fmt.Sprintf("wb2api-chat5-%d-%d", time.Now().UnixMilli(), i)
		if err := p.cfg.Upstream.ReportChatActivity(a, cid, ""); err != nil {
			return fmt.Sprintf("сообщить № %d/%d неудач: %v", i+1, need, err), nil
		}
		if i < need-1 {
			time.Sleep(reportGap)
		}
	}
	return fmt.Sprintf("уже доотправлено %d событий диалога", need), nil
}

// runFirstBuddy Адопция:report（разблокировка — предусловие)→ agreement → first。
func runFirstBuddy(p *Panel, a *auth.Auth) (string, error) {
	if err := p.cfg.Upstream.ReportChatActivity(a, fmt.Sprintf("wb2api-adopt-%d", time.Now().UnixMilli()), ""); err != nil {
		return "", fmt.Errorf("Пре-репорт: %w", err)
	}
	time.Sleep(reportGap) // оставить время на обработку события апстримом (метрика по фактическому замеру скрипта)
	if err := p.cfg.Upstream.BuddyAgreement(a); err != nil {
		return "", fmt.Errorf("принять соглашение: %w", err)
	}
	if err := p.cfg.Upstream.BuddyFirst(a); err != nil {
		if upstream.IsBuddyTaskIncomplete(err) {
			return "pre-check уже отправлен, но порог adoption не пройден (апстрим требует активности сегодня), повторите позже", nil
		}
		return "", fmt.Errorf("Получить Buddy: %w", err)
	}
	return "Уже получено Buddy（+300 Разделить +8 энергия)", nil
}

// runModelChat Завершено Model_chat_GLM5.2：accept → реальный диалог → Выровнять отчёт по модели.
func runModelChat(p *Panel, a *auth.Auth) (string, error) {
	const code, modelID, modelName = "Model_chat_GLM5.2", "glm-5.2", "GLM-5.2"
	// 1. accept（регистрация; сбой не блокирует — критерий — поведенческое событие)
	if err := p.cfg.Upstream.AcceptTasks(a, []string{code}); err != nil {
		log.Printf("panel: accept %s: %v（продолжить по поведенческой цепочке)", code, err)
	}
	time.Sleep(reportGap)
	// 2. Один реальный диалог (прямое доказательство критерия)
	body, _ := json.Marshal(map[string]any{
		"model": modelID,
		"messages": []map[string]any{
			{"role": "user", "content": "hi,Ответьте одним предложением"},
		},
		"stream": true,
	})
	rc, status, respBody, err := p.cfg.Upstream.ChatStream(a, body, "", upstream.ChatMeta{})
	if err != nil {
		return "", fmt.Errorf("Запрос диалога: %w", err)
	}
	if status >= 400 {
		rc.Close()
		return "", fmt.Errorf("Ошибка диалога http=%d: %s", status, truncateStr(string(respBody), 160))
	}
	// вычитать досуха SSE（Шлюз принудительно к апстриму flow：недочитывание оставит соединение висеть)
	_, _ = io.Copy(io.Discard, io.LimitReader(rc, 1<<20))
	rc.Close()
	time.Sleep(reportGap)
	// 3. Отчет выравнивания модели (триггер прогресса)
	if err := p.cfg.Upstream.ReportChatActivityModel(a, fmt.Sprintf("wb2api-glm52-%d", time.Now().UnixMilli()), "", modelID, modelName); err != nil {
		return "Диалог завершён, но отчёт о прогрессе не удался:" + err.Error(), nil
	}
	return "завершено glm-5.2 диалог и репорт", nil
}

// runRichMeow Завершено RichMeow_Chat（Диалог десктопной версии1раз).
// 2026-09-12 Проверено на 3 аккаунтах: по отпечатку десктопа (extName=workbuddy-desktop）направление
// copilot.tencent.com/v2/report Отправка полной цепочки событий диалога (agent_task_created →
// chat_message_response isSuccessful=true и т.д. 6 событие), чисто API можно активировать и получить награду
// （Цзычуань/Жэньцзе2 при отсутствии логина десктоп-клиента у двух аккаунтов 3 в течение сек. 0/1 → 1/1）。
// Hp_Appearance чистый API set Без начисления баллов (требуется активность клиента в теме), дифференцировать.
func runRichMeow(p *Panel, a *auth.Auth) (string, error) {
	ms := time.Now().UnixMilli()
	conv := fmt.Sprintf("wb2api-rm-%d", ms)
	req := fmt.Sprintf("wb2api-rm-req-%d", ms)
	msg := fmt.Sprintf("req-%d-user", ms)
	events := upstream.DesktopChatSequence(conv, req, msg, "fast-model", "fast-model")
	if err := p.cfg.Upstream.ReportDesktopEvent(a, events...); err != nil {
		return "", err
	}
	return "Полная цепочка событий диалога уже отправлена с фингерпринтом десктопа (agent_task_created→chat_response）", nil
}

// runBuddyApp Завершено Buddy_App / Buddy_App_QQ（вход в Buddy применения).
// 2026-09-12 Факт-тест на двух аккаунтах:buddyapp событие пяти подряд (discover→show→enter→auth_confirm→
// bind_skip）по workbuddy-desktop отправка fingerprint сразу подсвечивает, сервер не валидирует реальную авторизацию.
// использовать Помощник учителя Penguin (Buddy_App_QQ применение критерия) как носитель, одна группа событий одновременно удовлетворяет
// Buddy_App「«Вход в любое приложение» — две записи используют этот run，идемпотентность: пропуск fallback по статусу задачи.
func runBuddyApp(p *Panel, a *auth.Auth) (string, error) {
	events := upstream.DesktopBuddyAppSequence("cb_y5Dy46tPQGGWtueMxXbe", "ассистент учителя-пингвина")
	if err := p.cfg.Upstream.ReportDesktopEvent(a, events...); err != nil {
		return "", err
	}
	return "уже зарепорчено buddyapp вход в серию из 5 событий (покрывает также Buddy_App и Buddy_App_QQ）", nil
}

// runAutomationCreate Завершено automation_1（настройка автоматической задачи).
// 2026-09-12 Факт-тест на двух аккаунтах:automated_task_create_suc Событие чисто API репорт сразу подсвечивает,
// Не требуется реально создавать cron-задачу.
func runAutomationCreate(p *Panel, a *auth.Auth) (string, error) {
	if err := p.cfg.Upstream.ReportDesktopEvent(a,
		upstream.DesktopAutomationCreateEvent("wb2api Автоматизация")); err != nil {
		return "", err
	}
	return "Событие создания cron-задачи уже отправлено", nil
}

// runLibraryRead Завершено Library_read（база материалов опыта).
// 2026-09-12 фактические замеры трёх аккаунтов:web Домен /v2/report Отчёт web_element_click
// (elementId=library_doc_intro_click) сразу подсвечивается (space open/WS/inlong не являются критерием).
func runLibraryRead(p *Panel, a *auth.Auth) (string, error) {
	const docURL = "https://www.workbuddy.cn/space/d/o0KWYeynteVv06UnAZqIFm"
	if err := p.cfg.Upstream.ReportWebEvent(a, "web_element_click", docURL,
		"library_doc_intro_click", "WorkBuddyОписание базы знаний"); err != nil {
		return "", err
	}
	return "событие чтения интро базы знаний уже отправлено", nil
}

// runBlackCat Завершено black_cat（Сова, ночью 23:00–08:00 подсчёт).
// Критерий = Внутри ночного окна glm-5.2 реальный диалог + chat Отправка событий (WorkBuddy-Daily фактический калибр).
// Вне окна не выполнять (подсказки и т.п. по расписанию); шлюз blackcat_hours（По умолчанию 23 ч.) расписание автодополняется.
func runBlackCat(p *Panel, a *auth.Auth) (string, error) {
	if !upstream.InNightWindow(time.Now()) {
		return "Сейчас отсутствует 23:00–08:00 окно подсчета, действия не скорингируются; шлюз ежедневно в 23 автодополнение в точке", nil
	}
	need, err := p.cfg.Upstream.BlackcatNeed(a)
	if err != nil {
		return "", err
	}
	if need <= 0 {
		return "Прогресс достигнут, пополнение не требуется", nil
	}
	ok, err := p.cfg.Upstream.RunNightChats(a, int(need))
	if err != nil {
		return fmt.Sprintf("Завершено %d/%d прерывание после N раз: %v", ok, need, err), nil
	}
	return fmt.Sprintf("завершено %d ночных диалогов с отчетом", ok), nil
}

// runSkillFresh Завершено skill_1（популярные скиллы раннего доступа).
// 2026-09-12 Критерий (Zichuan вручную выполнил захват трафика row 209）：`skill_info` Событие (отпечаток десктопа) —
// {id:<Имя навыка>, skillId, skillVersion, toolStatus:"success", fileCount,
// source:"workbuddy-desktop"} JOIN реальная сессия (conversationId/requestId=Сервер
// id）。Предыдущий skill_request_send/skill_installed/skill_action Все направления — ошибочные.
// Жэньцзе2 На практике 0/1 → 1/1 подсветить.
func runSkillFresh(p *Panel, a *auth.Auth) (string, error) {
	conv, req, err := p.cfg.Upstream.DesktopChatWithExpert(a, "")
	if err != nil {
		return "", fmt.Errorf("реальный диалог: %w", err)
	}
	msgID := "msg-" + req[len(req)-8:]
	events := upstream.DesktopChatSequence(conv, req, msgID, "fast-model", "fast-model")
	for _, ev := range events {
		if ev["eventCode"] == "chat_message_response" {
			ev["finishReason"] = "tool_calls" // Семантика вызова инструмента моделью (загрузка навыка)
		}
	}
	events = append(events, upstream.DesktopEvent{
		"eventCode": "skill_info",
		"id": "Жуньцзэ Сяогуань · написание ежедневного отчета",
		"skillId": "skill_2097350077599879168",
		"skillVersion": "1.0.0",
		"toolStatus": "success",
		"fileCount": 56,
		"source": "workbuddy-desktop",
		"conversationId": conv, "requestId": req, "messageId": msgID,
		"requestModelId": "fast-model", "requestModelName": "fast-model",
		"traceId": req,
	})
	if err := p.cfg.Upstream.ReportDesktopEvent(a, events...); err != nil {
		return "", fmt.Errorf("skill_info событие: %w", err)
	}
	return "реальный диалог уже отправлен + skill_info событие загрузки навыка", nil
}

// runExpertLighthouse Завершено Expert_lighthouse（опыт "эксперт Tencent Lighthouse»).
// 2026-09-12 Критерий (реальные сэмплы Sunny row 868）：и expert_5 Изоморфно, но два различия —
// chat цепочки agent_task_created требуется has_expert:true + expert_id（по умолчанию мы false），
// expert_actual_use mode для "LOCAL"（не craft）。id Фиксированно легковесный облачный эксперт
// ex_2cvvUZQhDyeJ；requestId Должен быть реальным chat серверной части id。Цзычуань/Жэньцзе2 подсвечено тестами.
func runExpertLighthouse(p *Panel, a *auth.Auth) (string, error) {
	const lhID = "ex_2cvvUZQhDyeJ"
	lh := upstream.MarketExpert{
		ExpertID: lhID, ExpertType: "agent",
		DisplayNameZH: "эксперт Tencent Lighthouse", ProfessionZH: "эксперт Tencent Lighthouse", Version: "1.0.2",
	}
	// Если список маркета попал в реальную запись — использовать её данные (version и т.д. по данным сервера).
	if experts, err := p.cfg.Upstream.MarketExpertList(a, "agent"); err == nil {
		for _, e := range experts {
			if e.ExpertID == lhID {
				lh = e
				break
			}
		}
	}
	if err := p.cfg.Upstream.ReportDesktopEvent(a, upstream.DesktopExpertSummonSequence(lh)...); err != nil {
		return "", fmt.Errorf("Цепочка вызовов: %w", err)
	}
	conv, req, err := p.cfg.Upstream.DesktopChatWithExpert(a, lhID)
	if err != nil {
		return "", fmt.Errorf("реальный диалог: %w", err)
	}
	events := upstream.DesktopChatSequence(conv, req, "msg-"+req[len(req)-8:], "fast-model", "fast-model")
	for _, ev := range events {
		if ev["eventCode"] == "agent_task_created" {
			ev["has_expert"] = true
			ev["expert_id"] = lh.ExpertID
			ev["expert_name"] = lh.DisplayNameZH
			ev["expert_industry_id"] = ""
		}
	}
	events = append(events, upstream.DesktopExpertActualUseLocal(lh, conv, req))
	// Выверка деталей реальных сэмплов: легковесный cloud-эксперт actual_use type пусто,cost=0。
	events[len(events)-1]["type"] = ""
	events[len(events)-1]["cost"] = 0
	if err := p.cfg.Upstream.ReportDesktopEvent(a, events...); err != nil {
		return "", fmt.Errorf("Использовать событие: %w", err)
	}
	return "Отправлен легковесный облачный вызов эксперта+цепочка использования (реальный диалог requestId）", nil
}

// runAppearance Завершено Hp_Appearance（смена темы).
// 2026-09-12 Цзычуань/Жэньцзе2 фактически (desktopverify12）：Критерий — appearance_skin_apply
// событие {action:"apply«,source:«settings_close",id:<resourceKey>,...}（клиент в
// Отправка при выходе со страницы настроек при активной теме) — ранняя"Чистый API set Не засчитывать"вывод неточен,
// на деле тогда вызывался только appearance/set событие не отправлено. Комбинация:set API Оставить след + отправка события.
func runAppearance(p *Panel, a *auth.Auth) (string, error) {
	const themeKey = "theme-tkmw7j" // Peace Elite: яростная схватка золотой осени (Hp_Appearance тема-критерий)
	if err := p.cfg.Upstream.SetAppearanceTheme(a, themeKey); err != nil {
		return "", fmt.Errorf("настроить тему: %w", err)
	}
	time.Sleep(2 * time.Second)
	if err := p.cfg.Upstream.ReportDesktopEvent(a, upstream.DesktopEvent{
		"eventCode": "appearance_skin_apply", "action": "apply", "source": "settings_close",
		"id": themeKey, "vipLevel": 0, "series": "", "type": "unknown",
	}); err != nil {
		return "", err
	}
	return "Тема установлена, отправлено событие применения скина", nil
}

// runTemplateUse Завершено template_5（Использовать 5 шаблонов создают задачу).
// 2026-09-12 фактические замеры трёх аккаунтов:agent_task_created_with_template + template_used Группа событий
// （JOIN chat цепочка) один репорт 5 группа — сразу 5/5 подсветить.template_id сервер не проверяет подлинность.
func runTemplateUse(p *Panel, a *auth.Auth) (string, error) {
	templates := [][2]string{{"1", "глубокое исследование"}, {"2", "Генерация еженедельного отчёта"}, {"3", "Анализ конкурентов"}, {"4", "планирование активностей"}, {"5", "код-ревью"}}
	for i, tp := range templates {
		ms := time.Now().UnixMilli()
		conv := fmt.Sprintf("wb2api-tpl-%d-%d", ms, i)
		req := fmt.Sprintf("wb2api-tpl-req-%d-%d", ms, i)
		events := upstream.DesktopTemplateUseSequence(conv, req, tp[0], tp[1])
		if err := p.cfg.Upstream.ReportDesktopEvent(a, events...); err != nil {
			return fmt.Sprintf("№ %d Ошибка отправки события шаблона группы: %v", i+1, err), nil
		}
		time.Sleep(300 * time.Millisecond)
	}
	return "уже зарепорчено template_used ×5", nil
}

// runPlaybookPrompt Завершено playbook_prompt（кейс-вдохновение Dialog отправка в Prompt）。
// Критерий — playbook_prompt_send（Dialog отправка) а не показ карточки/Клик —asar Обратное подтверждение.
func runPlaybookPrompt(p *Panel, a *auth.Auth) (string, error) {
	ms := time.Now().UnixMilli()
	conv := fmt.Sprintf("wb2api-pb-%d", ms)
	req := fmt.Sprintf("wb2api-pb-req-%d", ms)
	events := upstream.DesktopPlaybookPromptSequence(conv, req, "pm-gtm-launch-plan", "Выпуск нового продукта GTM План релиза — одна страница")
	if err := p.cfg.Upstream.ReportDesktopEvent(a, events...); err != nil {
		return "", err
	}
	return "уже зарепорчено playbook_cta_click + playbook_prompt_send", nil
}

// runCreateCanvas Завершено create_canvas（Создание холста в режиме дизайн-креатива,+300 мин).
// Критерий — wbx_design_canvas_task_create/open（Ardot create_design инструмент завершает телеметрию).
func runCreateCanvas(p *Panel, a *auth.Auth) (string, error) {
	ms := time.Now().UnixMilli()
	conv := fmt.Sprintf("wb2api-canvas-%d", ms)
	req := fmt.Sprintf("wb2api-canvas-req-%d", ms)
	events := upstream.DesktopDesignCanvasSequence(conv, req)
	if err := p.cfg.Upstream.ReportDesktopEvent(a, events...); err != nil {
		return "", err
	}
	return "уже зарепорчено wbx_design_canvas_task_create/open", nil
}

// expertSummonGap интервал цепочки вызова экспертов (реальный ритм использования,v11 На практике 8s Успешность 100%）。
const expertSummonGap = 6 * time.Second

// runExpertUse Завершено expert_5（Использовать 5 экспертов платформы).
// 2026-09-12 Формула по замерам 3 аккаунтов: реальный список экспертов (id должен реально существовать)→ Цепочка вызовов
// （summon_click/summoned）→ Реальный chat Взять с сервера requestId → expert_actual_use。
// Самодельный эксперт id или самогенерация requestId Не учитываются.
func runExpertUse(p *Panel, a *auth.Auth) (string, error) {
	return runExpertBatch(p, a, "agent", 5)
}

// runExpertTeamUse Завершено Expert_team_use_3（Использовать 3 экспертных групп,expertType=team）。
func runExpertTeamUse(p *Panel, a *auth.Auth) (string, error) {
	return runExpertBatch(p, a, "team", 3)
}

// runExpertBatch Вызов эксперта+используется общая реализация. Ошибки по каждому пропускаются, возвращается сводка.
func runExpertBatch(p *Panel, a *auth.Auth, expertType string, count int) (string, error) {
	experts, err := p.cfg.Upstream.MarketExpertList(a, expertType)
	if err != nil {
		return "", fmt.Errorf("загрузить список экспертов: %w", err)
	}
	if len(experts) == 0 {
		return "", fmt.Errorf("список маркета экспертов пуст")
	}
	ok, fail := 0, 0
	for i, e := range experts {
		if ok >= count {
			break
		}
		// Цепочка вызова (web_element_click + summon_click + summoned）。
		summonEvents := upstream.DesktopExpertSummonSequence(e)
		if err := p.cfg.Upstream.ReportDesktopEvent(a, summonEvents...); err != nil {
			fail++
			continue
		}
		// Реальный chat（Лента X-Expert-Id）→ Сервер requestId。
		conv, req, cerr := p.cfg.Upstream.DesktopChatWithExpert(a, e.ExpertID)
		if cerr != nil {
			fail++
			continue
		}
		// событие использования (JOIN Сервер requestId）+ chat Цепочка.
		events := append(upstream.DesktopChatSequence(conv, req, "msg-"+req[len(req)-8:], "fast-model", "fast-model"),
			upstream.DesktopExpertActualUseEvent(e, conv, req))
		if err := p.cfg.Upstream.ReportDesktopEvent(a, events...); err != nil {
			fail++
			continue
		}
		ok++
		if i < len(experts)-1 {
			time.Sleep(expertSummonGap)
		}
	}
	_ = fail
	return fmt.Sprintf("Уже для %d реальных экспертов завершили вызов+цепочка использования (тип %s）", ok, expertType), nil
}

// ---------------------------------------------------------------------------
// Полностью автозавершено
// ---------------------------------------------------------------------------

// runAutoAll Последовательно выполнить все автоматизируемые задачи для одного аккаунта, вернуть результаты по каждой.
// Для "выполнить все автозадачи одним кликом»; сбой одного элемента не влияет на последующие.
//
// Процесс: сначала пакетно все непринятые задачи accept（Нормализованный автомат состояний; скрипту апстрима рекомендуется"Сначала accept"），
// Затем поэтапно выполнить цепочку действий.accept не является обязательным условием генерации прогресса, но делает последующие переходы состояний корректными.
func (p *Panel) runAutoAll(a *auth.Auth) []map[string]any {
	var out []map[string]any

	// этап 0：Массовое принятие непринятых задач (ошибка не блокирует — единственный критерий прогресса — поведенческие события).
	if tasks, err := p.cfg.Upstream.ListTasks(a); err == nil {
		var codes []string
		for _, t := range tasks {
			if !t.Claimed && !t.Locked && t.AcceptStatus != "accepted" && t.AcceptStatus != "completed" {
				codes = append(codes, t.TaskCode)
			}
		}
		if len(codes) > 0 {
			if err := p.cfg.Upstream.AcceptTasks(a, codes); err != nil {
				out = append(out, map[string]any{
					"task_code": "(Массовое принятие)", "status": "error",
					"message": "ошибка принятия задачи (не блокирует последующие): " + err.Error(),
				})
			} else {
				out = append(out, map[string]any{
					"task_code": "(Массовое принятие)", "status": "done",
					"message": fmt.Sprintf("Принято %d задач", len(codes)),
				})
				time.Sleep(reportGap)
			}
		}
	}

	// этап 0b：задачи по метрике мини-программы принимаются отдельно (по умолчанию список не содержит mp код; ошибка не блокирует).
	if mpTasks, err := p.cfg.Upstream.ListTasksMP(a); err == nil {
		var mpCodes []string
		for _, t := range mpTasks {
			if !t.Claimed && !t.Locked && t.AcceptStatus != "accepted" && t.AcceptStatus != "completed" {
				mpCodes = append(mpCodes, t.TaskCode)
			}
		}
		if len(mpCodes) > 0 {
			if err := p.cfg.Upstream.AcceptTasksMP(a, mpCodes); err != nil {
				out = append(out, map[string]any{
					"task_code": "(Массовое принятие-mp)", "status": "error",
					"message": "Принять неуспех задачи мини-программы (не блокирует последующее): " + err.Error(),
				})
			} else {
				out = append(out, map[string]any{
					"task_code": "(Массовое принятие-mp)", "status": "done",
					"message": fmt.Sprintf("Принято %d задач мини-приложения", len(mpCodes)),
				})
				time.Sleep(reportGap)
			}
		}
	}

	for _, act := range autoActions {
		item := map[string]any{"task_code": act.TaskCode, "desc": act.Desc}
		before, err := p.taskByCode(a, act.TaskCode)
		if err != nil {
			item["status"] = "error"
			item["message"] = "Ошибка запроса: " + err.Error()
			out = append(out, item)
			continue
		}
		if before == nil {
			item["status"] = "skipped"
			item["message"] = "у этого аккаунта нет такой задачи"
			out = append(out, item)
			continue
		}
		if before.Claimed || before.Current >= before.Target && before.Target > 0 {
			item["status"] = "skipped"
			item["message"] = "завершено (" + taskProgressText(before) + ")"
			out = append(out, item)
			continue
		}
		msg, err := act.run(p, a)
		if err != nil {
			item["status"] = "error"
			item["message"] = err.Error()
			out = append(out, item)
			continue
		}
		var after *upstream.Task
		if isMPTaskCode(act.TaskCode) {
			after, _ = p.taskByCodeMP(a, act.TaskCode)
		} else {
			after, _ = p.taskByCodeWaiting(a, act.TaskCode)
		}
		item["status"] = "done"
		item["message"] = msg
		item["progress_after"] = taskProgressText(after)
		// При достижении прогресса — автонаграда (mp Задача идёт через chat Домен mp метрика, остальные Web Интерфейс стороны).
		// ошибка получения награды не скрывает результат основного процесса:status по-прежнему done，Дополнительно claim_error для подсказки на фронтенде.
		if after != nil && after.Claimable {
			item["claimable"] = true
			var credit, energy int64
			var cerr error
			if isMPTaskCode(act.TaskCode) {
				credit, energy, cerr = p.cfg.Upstream.ClaimRewardMP(a, act.TaskCode)
			} else {
				credit, energy, cerr = p.cfg.Upstream.ClaimReward(a, act.TaskCode)
			}
			if cerr == nil {
				item["claimed"] = true
				item["credit"] = credit
				item["energy"] = energy
				if credit > 0 || energy > 0 {
					item["message"] = msg + fmt.Sprintf(";награда уже получена автоматически +%d Разделить +%d Возможность", credit, energy)
				} else {
					item["message"] = msg + ";награда уже получена ранее"
				}
			} else {
				item["claim_error"] = cerr.Error()
				item["message"] = msg + ";Условие выполнено, но получение награды не удалось (можно ретрай вручную в списке)"
			}
		}
		out = append(out, item)
		time.Sleep(reportGap) // Троттлинг между элементами
	}
	return out
}

// accountTaskAutoAll выполнить все автозадачи аккаунта в один клик.
func (p *Panel) accountTaskAutoAll(w http.ResponseWriter, r *http.Request) {
	uid := r.PathValue("uid")
	a := p.accountByUID(w, uid)
	if a == nil {
		return
	}
	// per-account Мьютекс (общий лок с действием одиночной задачи): повторный клик 409。
	if !p.tryLockAccount(uid) {
		writeErr(w, http.StatusConflict, "Для этого аккаунта уже выполняется действие задачи, дождитесь завершения текущего раунда")
		return
	}
	// использовать context Fallback-таймаут (цепочка задач + каждый элемент содержит реальный диалог, может занять длительное время).
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Minute)
	defer cancel()
	done := make(chan []map[string]any, 1)
	go func() {
		defer p.unlockAccount(uid) // Фактическое завершение пайплайна (а не HTTP возврат по таймауту) только тогда снимать блокировку
		done <- p.runAutoAll(a)
	}()
	select {
	case results := <-done:
		log.Printf("panel: Авто-задачи в один клик uid=%s Всего %d Пункт", uid, len(results))
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "results": results})
	case <-ctx.Done():
		// HTTP таймаут на стороне с возвратом, но фоновый пайплайн продолжает работу — лок в пайплайне goroutine освободить внутри,
		// Повторные клики в этот период будут 409 блокируется, два параллельных раунда невозможны.
		writeErr(w, http.StatusGatewayTimeout, "Таймаут выполнения (задача продолжает выполняться в фоне)")
	}
}
