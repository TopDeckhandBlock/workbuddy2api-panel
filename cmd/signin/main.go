// signin одноразовый инструмент пакетного check-in: обход ./auths/workbuddy-*.json Все аккаунты,
// Авто RefreshToken（при истечении), вызывать по одному daily-checkin，попутно проверить баланс.
package main

import (
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
)

type row struct {
	file string
	uid string
	nick string
	status string // OK | ALREADY | FAIL | AUTH_INVALID | LOAD_ERR
	detail string
	remain int64
	hasQuota bool
}

func main() {
	dir := "auths"
	if len(os.Args) > 1 {
		dir = os.Args[1]
	}
	files, err := filepath.Glob(filepath.Join(dir, "workbuddy-*.json"))
	if err != nil || len(files) == 0 {
		fmt.Fprintf(os.Stderr, "no auth files in %s\n", dir)
		os.Exit(1)
	}
	sort.Strings(files)
	up := upstream.New()

	var rows []row
	okN, alreadyN, failN := 0, 0, 0
	for _, f := range files {
		r := row{file: filepath.Base(f)}
		raw, err := os.ReadFile(f)
		if err != nil {
			r.status, r.detail = "LOAD_ERR", err.Error()
			rows = append(rows, r)
			failN++
			continue
		}
		a, err := auth.Parse(raw)
		if err != nil {
			r.status, r.detail = "LOAD_ERR", err.Error()
			rows = append(rows, r)
			failN++
			continue
		}
		a.FilePath = f
		r.uid, r.nick = a.UID, a.Nickname

		// refresh Истёк token
		if a.NeedsRefresh(2 * 3600) {
			if err := up.RefreshToken(a); err != nil {
				if ue, ok := err.(*upstream.Error); ok && ue.Kind == upstream.ErrSessionDead {
					r.status = "AUTH_INVALID"
				} else {
					r.status = "FAIL"
				}
				r.detail = "refresh: " + short(err.Error())
				rows = append(rows, r)
				failN++
				continue
			}
			// refresh затем запись обратно в файл (проблема прав исправлена); сбой сохранения должен быть явным, иначе после рестарта — откат к старому token
			if err := a.SaveAtomic(); err != nil {
				log.Printf("signin %s save: %v", a.UID, err)
			}
		}

		err = up.DailyCheckin(a)
		switch {
		case err == nil:
			r.status = "OK"
			okN++
		default:
			// DailyCheckin Возврат при уже выполненном чекине code!=0 Ошибка
			if isAlready(err.Error()) {
				r.status = "ALREADY"
				r.detail = short(err.Error())
				alreadyN++
			} else {
				r.status = "FAIL"
				r.detail = short(err.Error())
				failN++
			}
		}
		// Попутно проверить баланс
		if remain, _, qerr := up.UserResource(a); qerr == nil {
			r.remain, r.hasQuota = remain, true
		}
		rows = append(rows, r)
	}

	// отчет
	fmt.Printf("uid | nick | status | remain | detail\n")
	fmt.Printf("-------------------------------------+-------------+--------------+--------+------------------------------\n")
	for _, r := range rows {
		remain := "-"
		if r.hasQuota {
			remain = fmt.Sprintf("%d", r.remain)
		}
		fmt.Printf("%-36s | %-11s | %-12s | %-6s | %s\n",
			trunc(r.uid, 36), trunc(r.nick, 11), r.status, remain, r.detail)
	}
	fmt.Printf("\ntotal=%d ok=%d already=%d fail=%d\n", len(rows), okN, alreadyN, failN)
}

// решение о наличии подписи:code не 0 И содержит "Уже отмечено«/«already»/«checkin" и т.п. текст
func isAlready(msg string) bool {
	s := strings.ToLower(msg)
	return strings.Contains(s, "Уже отмечено") ||
		strings.Contains(s, "already") ||
		strings.Contains(s, "checkin") ||
		strings.Contains(s, "code=400")
}

func trunc(s string, n int) string {
	if len(s) > n {
		return s[:n]
	}
	return s
}

func short(s string) string {
	s = strings.ReplaceAll(s, "\n", " ")
	if len(s) > 60 {
		return s[:60]
	}
	return s
}
