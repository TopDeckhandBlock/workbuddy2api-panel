// trial Единоразовое массовое получение global trial пакет-дозаправка: перебор auths все аккаунты в ,
// только для global выполнение аккаунтом /billing/ide/trial（CN нет такого эндпоинта, явно сообщить о неприменимости).
//
// Использование:
//
//	# Локально: в корне проекта (требуется config.json + auths/ + data/）прямо run
//	go run ./cmd/trial
//
//	# Внутри контейнера: сначала cp войти затем exec
//	docker cp trial workbuddy2api:/tmp/trial
//	docker exec -w /app workbuddy2api /tmp/trial
//
// Результаты выводятся поаккаунтно в stdout：
//
//	uid | nick | status | detail
//	----+------+--------+-------
//	... | GLOBAL | OK | trial granted
//	... | GLOBAL | ALREADY | уже получено (идемпотентно, не считается ошибкой)
//	... | CN | N/A | not applicable
package main

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
)

// classifyTrial Нормализация ClaimTrial результат (чистая функция, для main цикл и тест напрямую assert):
// err → FAIL；claimed → OK；иначе (идемпотентный код уже получен)→ ALREADY。
func classifyTrial(claimed bool, err error) (trialStatus, string) {
	switch {
	case err != nil:
		return trialFailed, err.Error()
	case claimed:
		return trialOK, "trial granted"
	default:
		return trialAlready, "already claimed (idempotent)"
	}
}

// trialStatus Один аккаунт trial Статус результата получения.
type trialStatus string

const (
	trialOK trialStatus = "OK"
	trialAlready trialStatus = "ALREADY"
	trialNotApp trialStatus = "N/A" // CN аккаунт неприменим
	trialFailed trialStatus = "FAIL"
)

type trialRow struct {
	uid string
	nick string
	status trialStatus
	detail string
}

func main() {
	authDir := "auths"
	if len(os.Args) > 1 {
		authDir = os.Args[1]
	}
	files, err := filepath.Glob(filepath.Join(authDir, "workbuddy-*.json"))
	if err != nil || len(files) == 0 {
		fmt.Fprintf(os.Stderr, "no auth files in %s\n", authDir)
		os.Exit(1)
	}
	sort.Strings(files)

	up := upstream.New()
	// trial Да global Выделенный эндпоинт: должен быть включен global realm роутинг, иначе upstream.New() 
	// GlobalEnabled нулевое значение false будет роутить запрос на CN base（codebuddy.cn）и неизбежно завершится ошибкой.
	up.GlobalEnabled = true
	var rows []trialRow
	for _, f := range files {
		r := trialRow{uid: filepath.Base(f)}
		raw, err := os.ReadFile(f)
		if err != nil {
			r.status, r.detail = trialFailed, "load: "+err.Error()
			rows = append(rows, r)
			continue
		}
		a, err := auth.Parse(raw)
		if err != nil {
			r.status, r.detail = trialFailed, "parse: "+err.Error()
			rows = append(rows, r)
			continue
		}
		a.FilePath = f
		r.uid, r.nick = a.UID, a.Nickname

		// Только global Применимо к аккаунту:CN Явно сообщить о неприменимости, запросы не отправлять.
		if !a.IsGlobal() {
			r.status, r.detail = trialNotApp, "CN account not applicable"
			rows = append(rows, r)
			continue
		}

		r.status, r.detail = classifyTrial(up.ClaimTrial(a))
		rows = append(rows, r)
	}

	var okN, alreadyN, notAppN, failN int
	fmt.Printf("uid | nick | status | detail\n")
	fmt.Printf("-------------------------------------+-------------+---------+------------------------------\n")
	for _, r := range rows {
		fmt.Printf("%-36s | %-11s | %-7s | %s\n",
			trunc(r.uid, 36), trunc(r.nick, 11), r.status, r.detail)
		switch r.status {
		case trialOK:
			okN++
		case trialAlready:
			alreadyN++
		case trialNotApp:
			notAppN++
		default:
			failN++
		}
	}
	fmt.Printf("\ntotal=%d ok=%d already=%d na=%d fail=%d\n",
		len(rows), okN, alreadyN, notAppN, failN)
}

func trunc(s string, n int) string {
	if len(s) > n {
		return s[:n]
	}
	return s
}
