// tasks.go Интерфейс панели "Задания за баллы»: запрос прогресса задач, принятие задачи, получение награды.
//
// Возможности апстрима (internal/upstream/tasks.go）трехслойная тонкая обертка; отображение таблицы на фронте
// current/target Прогресс и статус доступности, достаточно кнопки в админке, без внешнего Python Скрипт.
package panel

import (
	"encoding/json"
	"log"
	"net/http"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
)

// acceptBatchGap Межбатчевый троттлинг при массовом принятии (выравнивание со скриптом 1.05s метрика, во избежание антифрод-контроля апстрима).
var acceptBatchGap = 1050 * time.Millisecond

// accountByUID Получить учетные данные аккаунта; если отсутствуют — записать 404 и вернуть nil。
func (p *Panel) accountByUID(w http.ResponseWriter, uid string) *auth.Auth {
	a := p.cfg.Pool.AuthByUID(uid)
	if a == nil {
		writeErr(w, http.StatusNotFound, "account not found")
		return nil
	}
	return a
}

// accountTasks Запрос всех задач одного аккаунта (прогресс/Статус/доступно к получению).
func (p *Panel) accountTasks(w http.ResponseWriter, r *http.Request) {
	uid := r.PathValue("uid")
	a := p.accountByUID(w, uid)
	if a == nil {
		return
	}
	tasks, err := p.cfg.Upstream.ListTasks(a)
	if err != nil {
		writeErr(w, http.StatusBadGateway, "list tasks: "+err.Error())
		return
	}
	// Объединение задач в терминах мини-программы (school_season / Sequential_Tasks_1 и т.д. только при mp выдача списка заголовков).
	// mp Список — супермножество дефолтной выборки (фактически включает обычные задачи), по task_code Дедупликация; при ошибке тихо.
	if mpTasks, mpErr := p.cfg.Upstream.ListTasksMP(a); mpErr == nil {
		seen := map[string]bool{}
		for _, t := range tasks {
			seen[t.TaskCode] = true
		}
		for _, t := range mpTasks {
			if !seen[t.TaskCode] {
				tasks = append(tasks, t)
				seen[t.TaskCode] = true
			}
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "tasks": tasks})
}

// accountTaskAccept Принять задачу (регистрация; идемпотентно).
func (p *Panel) accountTaskAccept(w http.ResponseWriter, r *http.Request) {
	uid := r.PathValue("uid")
	a := p.accountByUID(w, uid)
	if a == nil {
		return
	}
	var body struct {
		TaskCodes []string `json:"task_codes"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || len(body.TaskCodes) == 0 {
		writeErr(w, http.StatusBadRequest, "task_codes required")
		return
	}
	if err := p.cfg.Upstream.AcceptTasks(a, body.TaskCodes); err != nil {
		writeErr(w, http.StatusBadGateway, "accept: "+err.Error())
		return
	}
	log.Printf("panel: Принять задачу uid=%s codes=%v", uid, body.TaskCodes)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// taskAcceptAll Принять все непринятые задачи этого аккаунта (пропустить уже accepted/claimed ).
//
// Почему это стоит делать:accept не дает прогресса (прогресс зажигается событиями действий), но нормализует автомат состояний
// （not_accepted → accepted → completed），Также удобно для последующей фильтрации"Задачи, на которые я записывался"。
// апстрим scripts комментарий также рекомендуется"Сначала accept"。
// Пакетная отправка шардируется (upstream для task_codes длина массива без публичного лимита, консервативно на партию 20 шт.).
func (p *Panel) taskAcceptAll(w http.ResponseWriter, r *http.Request) {
	uid := r.PathValue("uid")
	a := p.accountByUID(w, uid)
	if a == nil {
		return
	}
	tasks, err := p.cfg.Upstream.ListTasks(a)
	if err != nil {
		writeErr(w, http.StatusBadGateway, "list tasks: "+err.Error())
		return
	}
	var codes []string
	for _, t := range tasks {
		// пропустить завершенные/Уже получено/принятый;locked не трогать (апстрим не открыл).
		if t.Claimed || t.Locked || t.AcceptStatus == "accepted" || t.AcceptStatus == "completed" {
			continue
		}
		codes = append(codes, t.TaskCode)
	}
	if len(codes) == 0 {
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "accepted": 0, "message": "Все задачи приняты"})
		return
	}
	const batch = 20
	accepted := 0
	var failed []string
	for i := 0; i < len(codes); i += batch {
		end := i + batch
		if end > len(codes) {
			end = len(codes)
		}
		if err := p.cfg.Upstream.AcceptTasks(a, codes[i:end]); err != nil {
			log.Printf("panel: Пакетный приём — сбой uid=%s codes=%v err=%v", uid, codes[i:end], err)
			failed = append(failed, codes[i:end]...)
			continue
		}
		accepted += end - i
		time.Sleep(acceptBatchGap) // Троттлинг между батчами (выравнивание со скриптом 1.05s метрика)
	}
	// Задачи мини-программы принимать пакетно отдельно (список по умолчанию не содержит mp код,accept также требуется mp заголовок).
	if mpTasks, mpErr := p.cfg.Upstream.ListTasksMP(a); mpErr == nil {
		var mpCodes []string
		for _, t := range mpTasks {
			if t.Claimed || t.Locked || t.AcceptStatus == "accepted" || t.AcceptStatus == "completed" {
				continue
			}
			mpCodes = append(mpCodes, t.TaskCode)
		}
		if len(mpCodes) > 0 {
			if err := p.cfg.Upstream.AcceptTasksMP(a, mpCodes); err != nil {
				log.Printf("panel: mp Пакетный приём — сбой uid=%s err=%v", uid, err)
				failed = append(failed, mpCodes...)
			} else {
				accepted += len(mpCodes)
				time.Sleep(acceptBatchGap)
			}
		}
	}
	log.Printf("panel: Принять всё uid=%s Принять=%d ошибка=%d", uid, accepted, len(failed))
	resp := map[string]any{"ok": true, "accepted": accepted, "failed": failed}
	if len(failed) > 0 {
		resp["message"] = "Часть задач не принята (отклонено апстримом), можно ретрай"
	}
	writeJSON(w, http.StatusOK, resp)
}

// accountTaskClaim Получение награды за задание (при невыполнении условий апстрим вернёт бизнес-ошибку, пробрасывается на фронтенд как есть).
func (p *Panel) accountTaskClaim(w http.ResponseWriter, r *http.Request) {
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
	// задачи в контуре мини-программы идут chat Домен mp награда за header (без header фактически не выдается); остальное Web Интерфейс эндпоинта.
	var credit, energy int64
	var err error
	if isMPTaskCode(body.TaskCode) {
		credit, energy, err = p.cfg.Upstream.ClaimRewardMP(a, body.TaskCode)
	} else {
		credit, energy, err = p.cfg.Upstream.ClaimReward(a, body.TaskCode)
	}
	if err != nil {
		writeErr(w, http.StatusBadGateway, "claim: "+err.Error())
		return
	}
	if credit == 0 && energy == 0 {
		log.Printf("panel: получить награду за задание uid=%s code=%s（Уже получено, нового нет)", uid, body.TaskCode)
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "already_claimed": true, "message": "награда уже получена ранее"})
		return
	}
	log.Printf("panel: получить награду за задание uid=%s code=%s +%dРазделить +%dВозможность", uid, body.TaskCode, credit, energy)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "credit": credit, "energy": energy})
}
