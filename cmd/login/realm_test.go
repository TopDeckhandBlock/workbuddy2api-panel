package main

import (
	"bytes"
	"reflect"
	"strings"
	"testing"
)

// TestParseRealmArgs перекрытие --realm Парсинг параметров: по умолчанию cn、Знак равенства/раздельный стиль записи,
// Недопустимое значение/ошибка при отсутствии значения, нормализация регистра, trim flag оставшиеся параметры после сохраняют относительный порядок.
func TestParseRealmArgs(t *testing.T) {
	cases := []struct {
		name string
		args []string
		wantRealm string
		wantRest []string
		wantErr bool
	}{
		{name: "по умолчанию cn", args: []string{"url"}, wantRealm: "cn", wantRest: []string{"url"}},
		{name: "Знак равенства global перед", args: []string{"--realm=global", "url"}, wantRealm: "global", wantRest: []string{"url"}},
		{name: "Знак равенства cn после", args: []string{"poll", "--realm=cn"}, wantRealm: "cn", wantRest: []string{"poll"}},
		{name: "Раздельный global", args: []string{"--realm", "global", "url"}, wantRealm: "global", wantRest: []string{"url"}},
		{name: "Ошибка недопустимого значения", args: []string{"--realm=foo", "url"}, wantErr: true},
		{name: "раздельная ошибка отсутствия значения", args: []string{"--realm", "url"}, wantErr: true},
		{name: "нормализация регистра", args: []string{"--realm=GLOBAL", "url"}, wantRealm: "global", wantRest: []string{"url"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			realm, rest, err := parseRealmArgs(c.args)
			if c.wantErr {
				if err == nil {
					t.Fatalf("parseRealmArgs(%v) err=nil, want error", c.args)
				}
				return
			}
			if err != nil {
				t.Fatalf("parseRealmArgs(%v) err=%v", c.args, err)
			}
			if realm != c.wantRealm {
				t.Errorf("realm=%q want %q", realm, c.wantRealm)
			}
			if !reflect.DeepEqual(rest, c.wantRest) {
				t.Errorf("rest=%v want %v", rest, c.wantRest)
			}
		})
	}
}

// TestResolveRealmInput Перекрывает ввод интерактивного выбора домена→realm Маппинг (login.sh ключевое решение ветки взаимодействия):
// "1«/"cn"/««（Enter по умолчанию)→ cn；"2"/"global" → global；Регистронезависимо; невалидный → ("",false)。
func TestResolveRealmInput(t *testing.T) {
	cases := []struct {
		input string
		want string
	}{
		{input: "1", want: "cn"},
		{input: "cn", want: "cn"},
		{input: "CN", want: "cn"},
		{input: "", want: "cn"}, // Enter по умолчанию
		{input: "2", want: "global"},
		{input: "global", want: "global"},
		{input: "GLOBAL", want: "global"},
	}
	for _, c := range cases {
		got, ok := resolveRealmInput(c.input)
		if !ok {
			t.Errorf("resolveRealmInput(%q) ok=false want true", c.input)
			continue
		}
		if got != c.want {
			t.Errorf("resolveRealmInput(%q)=%q want %q", c.input, got, c.want)
		}
	}
	// Некорректный ввод → (false)。
	for _, bad := range []string{"3", "cnn", "globalx", "foo"} {
		if got, ok := resolveRealmInput(bad); ok {
			t.Errorf("resolveRealmInput(%q)=(%q,true) want (_,false)", bad, got)
		}
	}
}

// TestPromptRealm перекрытие promptRealm I/O Поведение (out будет подключено os.Stderr，stdout Только исходящие realm）：
//
//	1 / 2 / Enter по умолчанию → Выводить раздельно cn / global / cn；Печатать все"Выбор версии логина"Подсказка;
//	Некорректный ввод → Предупреждение и фолбэк cn；EOF（неинтерактивный прямой пайп)→ откат cn。
func TestPromptRealm(t *testing.T) {
	cases := []struct {
		name string
		input string
		want string
		wantHint bool // В выводе встречается"Выбор версии логина"Подсказка
	}{
		{name: "Выбрать cn", input: "1\n", want: "cn", wantHint: true},
		{name: "Выбрать global", input: "2\n", want: "global", wantHint: true},
		{name: "Enter по умолчанию cn", input: "\n", want: "cn", wantHint: true},
		{name: "Нелегальный fallback cn", input: "foo\n", want: "cn", wantHint: true},
		{name: "EOF откат cn", input: "", want: "cn", wantHint: true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var out bytes.Buffer
			got := promptRealm(strings.NewReader(c.input), &out)
			if got != c.want {
				t.Errorf("promptRealm(%q)=%q want %q", c.input, got, c.want)
			}
			if c.wantHint && !strings.Contains(out.String(), "Выбор версии логина") {
				t.Errorf("prompt should contain 'Выбор версии логина', got %q", out.String())
			}
		})
	}
}

// TestRealmSubcommandSelection Верификация через команду realm Подкоманда (login.sh вызов интерактивной ветки):
// Отделение подкоманды --realm，Оставшиеся параметры как первые "realm"。
func TestRealmSubcommandSelection(t *testing.T) {
	realm, rest, err := parseRealmArgs([]string{"realm"})
	if err != nil {
		t.Fatalf("parseRealmArgs(realm) err=%v", err)
	}
	if realm != "cn" {
		t.Errorf("realm default=%q want cn", realm)
	}
	if len(rest) != 1 || rest[0] != "realm" {
		t.Errorf("rest=%v want [realm]", rest)
	}
}

// TestBuildLoginOutputRealmAlwaysSet Артефакты логина (login.sh На основании этого сохранить на диск auth файл) всегда содержит realm Ключ:
// Явно --realm приоритет, по умолчанию по domain вывод (echo исходящий global Аккаунт даже без явного указания несет global）。
// Это "логин с сохранением всегда с realm носитель теста контракта "идентификатор».
func TestBuildLoginOutputRealmAlwaysSet(t *testing.T) {
	tok := struct {
		AccessToken string `json:"accessToken"`
		RefreshToken string `json:"refreshToken"`
		ExpiresIn int64 `json:"expiresIn"`
		Domain string `json:"domain"`
	}{AccessToken: "at", RefreshToken: "rt", ExpiresIn: 3600, Domain: "www.codebuddy.cn"}
	acct := struct {
		UID string `json:"uid"`
		EnterpriseID string `json:"enterpriseId"`
		Nickname string `json:"nickname"`
	}{UID: "u1", Nickname: "n1"}

	cases := []struct {
		name string
		realm string
		domain string
		wantRealm string
	}{
		{name: "Явно global Приоритет", realm: "global", domain: "www.codebuddy.cn", wantRealm: "global"},
		{name: "Явно cn Приоритет", realm: "cn", domain: "www.workbuddy.ai", wantRealm: "cn"},
		{name: "По умолчанию по domain инференс global", realm: "", domain: "www.workbuddy.ai", wantRealm: "global"},
		{name: "по умолчанию пусто domain откат cn", realm: "", domain: "", wantRealm: "cn"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			tok.Domain = c.domain
			out := buildLoginOutput(tok, c.realm, acct)
			m, ok := out["realm"].(string)
			if !ok {
				t.Fatalf("missing realm key in login output: %v", out)
			}
			if m != c.wantRealm {
				t.Errorf("realm=%q want %q", m, c.wantRealm)
			}
		})
	}
}
