// credit.go — WorkBuddy Запрос баллов (все аккаунты + итого),JSON Вывести в stdout。
//
// Использование:
//
//	go run ./cmd/credit # или после сборки ./credit
//
// Структура вывода:
//
//	{"service":"workbuddy","ts":N,
//	 "total":{"remain":N,"used":N,"size":N,"accounts":N,"ok":N,"failed":N},
//	 "accounts":[{"uid","nickname","remain","used","size","packages","ok","error?"}]}
//
// Интерфейс и логика агрегации:POST codebuddy.cn/v2/billing/meter/get-user-resource，Агрегировать всё package 
// Cycle* поле,TotalDosage Действие size Нижний предел.
package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

const billingBaseCN = "https://www.codebuddy.cn"

// billingBaseGlobal Биллинговый домен международной версии.global пробивка аккаунтом CN Домен получит 401：
// www.codebuddy.cn не распознавать workbuddy.ai token（На практике 401，workbuddy.ai Совм. token для code=0）。
const billingBaseGlobal = "https://www.workbuddy.ai"

// billingBaseFor По аккаунту realm Выбор биллингового домена.
//
// критерий и internal/auth.Realm() согласованно: явно realm=global Или domain попадает в
// workbuddy.ai семейство — обрабатывать как в международной версии.cmd/credit Ранее жёстко закодировано для всех аккаунтов CN домен,
// приводит к global Запрос баланса аккаунта всегда возвращает 401（На панели — кэшированное значение из пула, не realtime).
func billingBaseFor(af *authFile) string {
	if strings.EqualFold(strings.TrimSpace(af.Auth.Realm), "global") {
		return billingBaseGlobal
	}
	d := strings.ToLower(strings.TrimSpace(af.Auth.Domain))
	if d == "workbuddy.ai" || strings.HasSuffix(d, ".workbuddy.ai") {
		return billingBaseGlobal
	}
	return billingBaseCN
}

type authFile struct {
	Auth struct {
		AccessToken string `json:"accessToken"`
		Domain string `json:"domain"`
		Realm string `json:"realm"`
	} `json:"auth"`
	Account struct {
		UID string `json:"uid"`
		EnterpriseID string `json:"enterpriseId"`
		Nickname string `json:"nickname"`
	} `json:"account"`
}

type accountResult struct {
	UID string `json:"uid"`
	Nickname string `json:"nickname"`
	Remain *int64 `json:"remain"`
	Used *int64 `json:"used"`
	Size *int64 `json:"size"`
	Packages int `json:"packages,omitempty"`
	OK bool `json:"ok"`
	Error string `json:"error,omitempty"`
}

type resourcePackage struct {
	CapacityRemain int64 `json:"CapacityRemain"`
	CapacityUsed int64 `json:"CapacityUsed"`
	CapacitySize int64 `json:"CapacitySize"`
	CycleCapacityRemain int64 `json:"CycleCapacityRemain"`
	CycleCapacityUsed int64 `json:"CycleCapacityUsed"`
	CycleCapacitySize int64 `json:"CycleCapacitySize"`
}

// packageRemainUsed и billing.go:203-258 консистентно
func packageRemainUsed(a resourcePackage) (remain, used, size int64) {
	if a.CycleCapacitySize > 0 {
		remain = a.CycleCapacityRemain
		size = a.CycleCapacitySize
		if remain < 0 {
			remain = 0
		}
		if remain > size {
			remain = size
		}
		used = size - remain
		if a.CycleCapacityUsed > used {
			used = a.CycleCapacityUsed
			if size >= used {
				remain = size - used
			}
		}
		return remain, used, size
	}
	remain = a.CapacityRemain
	used = a.CapacityUsed
	size = a.CapacitySize
	if used == 0 && size > remain {
		used = size - remain
	}
	return remain, used, size
}

func fetchUserResource(af *authFile) (remain, used, size int64, packs int, err error) {
	now := time.Now()
	body, _ := json.Marshal(map[string]any{
		"PageNumber": 1,
		"PageSize": 100,
		"ProductCode": "p_tcaca",
		"Status": []int{0, 3},
		"PackageEndTimeRangeBegin": now.Format("2006-01-02 15:04:05"),
		"PackageEndTimeRangeEnd": now.Add(365 * 101 * 24 * time.Hour).Format("2006-01-02 15:04:05"),
	})
	base := billingBaseFor(af)
	// global в домене отсутствует /v2 Префикс (с internal/upstream billingMeterPaths в той же метрике).
	path := "/v2/billing/meter/get-user-resource"
	if base == billingBaseGlobal {
		path = "/billing/meter/get-user-resource"
	}
	req, err := http.NewRequest(http.MethodPost, base+path, bytes.NewReader(body))
	if err != nil {
		return 0, 0, 0, 0, err
	}
	req.Header.Set("Authorization", "Bearer "+af.Auth.AccessToken)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Content-Type", "application/json")
	if af.Account.UID != "" {
		req.Header.Set("X-User-Id", af.Account.UID)
	}
	if af.Account.EnterpriseID != "" {
		req.Header.Set("X-Enterprise-Id", af.Account.EnterpriseID)
		req.Header.Set("X-Tenant-Id", af.Account.EnterpriseID)
	}
	if af.Auth.Domain != "" {
		req.Header.Set("X-Domain", af.Auth.Domain)
	}
	client := &http.Client{Timeout: 20 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return 0, 0, 0, 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return 0, 0, 0, 0, fmt.Errorf("http %d", resp.StatusCode)
	}
	var env struct {
		Code int `json:"code"`
		Msg string `json:"msg"`
		Data struct {
			Response struct {
				Data struct {
					TotalDosage int64 `json:"TotalDosage"`
					Accounts []resourcePackage `json:"Accounts"`
				} `json:"Data"`
			} `json:"Response"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&env); err != nil {
		return 0, 0, 0, 0, err
	}
	if env.Code != 0 {
		return 0, 0, 0, 0, fmt.Errorf("code=%d %s", env.Code, env.Msg)
	}
	for _, a := range env.Data.Response.Data.Accounts {
		r, u, s := packageRemainUsed(a)
		remain += r
		used += u
		size += s
	}
	packs = len(env.Data.Response.Data.Accounts)
	if size > 0 {
		if derived := size - remain; derived > used {
			used = derived
		}
	}
	if dosage := env.Data.Response.Data.TotalDosage; dosage > size {
		size = dosage
		if derived := size - remain; derived > used {
			used = derived
		}
	}
	return remain, used, size, packs, nil
}

func main() {
	pretty := len(os.Args) > 1 && os.Args[1] == "-pretty"
	authDir := "./auths"
	if v := os.Getenv("WB2A_AUTH_DIR"); v != "" {
		authDir = v
	}
	files, _ := filepath.Glob(filepath.Join(authDir, "workbuddy-*.json"))
	sort.Strings(files)

	accounts := make([]accountResult, 0, len(files))
	for _, f := range files {
		var af authFile
		raw, err := os.ReadFile(f)
		if err != nil || json.Unmarshal(raw, &af) != nil {
			continue
		}
		res := accountResult{UID: af.Account.UID, Nickname: af.Account.Nickname}
		if af.Auth.AccessToken == "" {
			res.Error = "no accessToken"
			accounts = append(accounts, res)
			continue
		}
		remain, used, size, packs, err := fetchUserResource(&af)
		if err != nil {
			res.Error = err.Error()
		} else {
			res.Remain = &remain
			res.Used = &used
			res.Size = &size
			res.Packages = packs
			res.OK = true
		}
		accounts = append(accounts, res)
		time.Sleep(200 * time.Millisecond)
	}

	var totalRemain, totalUsed, totalSize int64
	okCount := 0
	for _, a := range accounts {
		if a.OK {
			okCount++
			if a.Remain != nil {
				totalRemain += *a.Remain
			}
			if a.Used != nil {
				totalUsed += *a.Used
			}
			if a.Size != nil {
				totalSize += *a.Size
			}
		}
	}
	out := map[string]any{
		"service": "workbuddy",
		"ts": time.Now().Unix(),
		"total": map[string]any{
			"remain": totalRemain,
			"used": totalUsed,
			"size": totalSize,
			"accounts": len(accounts),
			"ok": okCount,
			"failed": len(accounts) - okCount,
		},
		"accounts": accounts,
	}
	if pretty {
		printPretty(accounts, totalRemain, totalUsed, totalSize, okCount)
		return
	}
	raw, _ := json.Marshal(out)
	fmt.Println(string(raw))
}

// printPretty человекочитаемый daily-отчёт: сводка в 4 строки, без детализации по аккаунтам.
func printPretty(accounts []accountResult, totalRemain, totalUsed, totalSize int64, okCount int) {
	withBalance := 0
	var failed []string
	for _, a := range accounts {
		if a.OK && a.Remain != nil && *a.Remain > 0 {
			withBalance++
		}
		if !a.OK {
			name := a.Nickname
			if name == "" && len(a.UID) >= 8 {
				name = a.UID[:8]
			}
			failed = append(failed, name+" "+a.Error)
		}
	}
	pct := int64(0)
	if totalSize > 0 {
		pct = totalRemain * 100 / totalSize
	}
	fmt.Printf("📊 WorkBuddy ежедневный отчет по баллам\n")
	fmt.Printf("Аккаунт: %d/%d\n", withBalance, len(accounts))
	fmt.Printf("Итого: %d/%d\n", totalRemain, totalSize)
	fmt.Printf("остаток: %d%%\n", pct)
	for _, f := range failed {
		fmt.Printf("⚠️ %s\n", f)
	}
}
