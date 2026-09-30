package upstream

import (
	"encoding/json"
	"strings"
	"testing"
)

// getThinkingType Из вывода body извлечение thinking.type（При отсутствии поля вернуть пустую строку + существует ли).
func getThinkingType(t *testing.T, out []byte) (typ string, present bool) {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(out, &m); err != nil {
		t.Fatalf("unmarshal: %v (out=%s)", err, out)
	}
	th, ok := m["thinking"].(map[string]any)
	if !ok {
		return "", false
	}
	s, ok := th["type"].(string)
	if !ok {
		return "", true
	}
	return s, true
}

// TestInjectThinkingDeepSeekEnabled Инжект вкл. reasoning / вкл. attention:deepseek тело запроса моделиСистема не содержит
// thinking в этот момент необходимо инжектировать {type:"enabled"}，Иначе апстрим по умолчанию отвечает без рассуждения (цепочка мыслей не отображается).
func TestInjectThinkingDeepSeekEnabled(t *testing.T) {
	cases := []struct {
		name string
		body string
		wantTyp string
	}{
		{"deepseek отсутствует thinking инжект enabled",
			`{"model":"deepseek-v4-flash","messages":[]}`, "enabled"},
		{"DeepSeek Без учета регистра",
			`{"model":"DeepSeek-v4.1-flash","messages":[]}`, "enabled"},
		{"DEEPSEEK регистронезависимо (upper-case)",
			`{"model":"DEEPSEEK-R1","messages":[]}`, "enabled"},
		{"deepseek Лента reasoning_effort отсутствует thinking инжект enabled",
			`{"model":"deepseek-v4-flash","reasoning_effort":"medium","messages":[]}`, "enabled"},
		{"deepseek thinking объект type пустой Дополнить enabled",
			`{"model":"deepseek-v4-flash","thinking":{},"messages":[]}`, "enabled"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			out := PrepareBodyOptWithEfforts([]byte(c.body), false, nil)
			typ, present := getThinkingType(t, out)
			if !present {
				t.Fatalf("thinking Отсутствует поле (out=%s)", out)
			}
			if typ != c.wantTyp {
				t.Errorf("thinking.type = %q want %q (out=%s)", typ, c.wantTyp, out)
			}
		})
	}
}

// TestInjectThinkingDefaultEffort Основное доказательство для возврата на доработку: отсутствует effort Голый запрос должен одновременно содержать
// thinking.type=enabled и дефолтный профиль reasoning_effort（иначе апстрим deepseek-v4-flash цепочка рассуждений не открывается).
// Дефолтный тариф = фолбэк официального клиента "high"，и передать supportedEfforts через пайплайн даунгрейда попадает в валидный тир.
func TestInjectThinkingDefaultEffort(t *testing.T) {
	// голый запрос без параметров reasoning → инжект enabled + reasoning_effort="high"。
	out := PrepareBodyOptWithEfforts(
		[]byte(`{"model":"deepseek-v4-flash","messages":[{"role":"user","content":"hi"}]}`),
		false, nil)
	typ, present := getThinkingType(t, out)
	if !present || typ != "enabled" {
		t.Fatalf("thinking.type=%q present=%v want enabled (out=%s)", typ, present, out)
	}
	eff, ok := objFieldString(t, out, "reasoning_effort")
	if !ok || eff != "high" {
		t.Errorf("reasoning_effort=%q ok=%v want high（профиль по умолчанию)(out=%s)", eff, ok, out)
	}

	// Модель поддерживает только low/high → По умолчанию high После fallback-конвейера всё ещё high（допустимый диапазон).
	out = PrepareBodyOptWithEfforts(
		[]byte(`{"model":"deepseek-v4-flash","messages":[]}`),
		false, map[string][]string{"deepseek-v4-flash": {"low", "high"}})
	eff, _ = objFieldString(t, out, "reasoning_effort")
	if eff != "high" {
		t.Errorf("supportedEfforts=[low high] дефолт-профиль в=%q want high (out=%s)", eff, out)
	}

	// Модель поддерживает только minimal/low → По умолчанию high Даунгрейд до low（≤high максимально поддерживаемого уровня).
	out = PrepareBodyOptWithEfforts(
		[]byte(`{"model":"deepseek-v4-flash","messages":[]}`),
		false, map[string][]string{"deepseek-v4-flash": {"minimal", "low"}})
	eff, _ = objFieldString(t, out, "reasoning_effort")
	if eff != "low" {
		t.Errorf("supportedEfforts=[minimal low] Даунгрейд дефолтного уровня ниже=%q want low (out=%s)", eff, out)
	}

	// Явно enabled + нехватка effort → также подставить дефолтный профиль (официальный configure thinking поведение).
	out = PrepareBodyOptWithEfforts(
		[]byte(`{"model":"deepseek-v4-flash","thinking":{"type":"enabled"},"messages":[]}`),
		false, nil)
	if eff, _ = objFieldString(t, out, "reasoning_effort"); eff != "high" {
		t.Errorf("Явно enabled нехватка effort следует дополнить профилем по умолчанию, got %q (out=%s)", eff, out)
	}
}

// TestInjectThinkingEffortNotOverridden Уже явно задано reasoning_effort（snake/camel）
// Не перезаписывать, не понижать, не удалять; даунгрейд выполняет normalizeReasoningEffort отвечает отдельно.
func TestInjectThinkingEffortNotOverridden(t *testing.T) {
	cases := []struct {
		name string
		body string
	}{
		{"snake effort сохранить как есть",
			`{"model":"deepseek-v4-flash","thinking":{"type":"enabled"},"reasoning_effort":"medium","messages":[]}`},
		{"camel effort сохранить как есть",
			`{"model":"deepseek-v4-flash","reasoningEffort":"low","messages":[]}`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			out := PrepareBodyOptWithEfforts([]byte(c.body), false, nil)
			typ, _ := getThinkingType(t, out)
			if typ != "enabled" {
				t.Fatalf("thinking.type=%q want enabled (out=%s)", typ, out)
			}
			// camel Ветка: на входе только camel，injectThinking Добавлять иное запрещено snake Профиль по умолчанию.
			if strings.Contains(c.body, "reasoningEffort") {
				if _, ok := objFieldString(t, out, "reasoning_effort"); ok {
					t.Errorf("уже есть reasoningEffort но добавлено reasoning_effort Дефолтный тариф (out=%s)", out)
				}
			}
		})
	}
	// Явно effort + отсутствует thinking → инжект enabled Но effort не перезаписывать.
	out := PrepareBodyOptWithEfforts(
		[]byte(`{"model":"deepseek-v4-flash","reasoning_effort":"medium","messages":[]}`),
		false, nil)
	if eff, _ := objFieldString(t, out, "reasoning_effort"); eff != "medium" {
		t.Errorf("Явно effort был перезаписан %q (out=%s)", eff, out)
	}
}

// TestInjectThinkingDisabledNoDefaultEffort disabled сохранить существующую семантику:
// thinking.type=disabled Уважать намерение закрытия;reasoning_effort Удалить; нельзя снова дополнять дефолтный уровень.
func TestInjectThinkingDisabledNoDefaultEffort(t *testing.T) {
	out := PrepareBodyOptWithEfforts(
		[]byte(`{"model":"deepseek-v4-flash","thinking":{"type":"disabled"},"reasoning_effort":"high","messages":[]}`),
		false, nil)
	typ, present := getThinkingType(t, out)
	if !present || typ != "disabled" {
		t.Fatalf("thinking.type=%q present=%v want disabled (out=%s)", typ, present, out)
	}
	for _, k := range []string{"reasoning_effort", "reasoningEffort"} {
		if _, ok := objFieldString(t, out, k); ok {
			t.Errorf("%s должен быть удалён без подстановки дефолтного файла (disabled при)(out=%s)", k, out)
		}
	}
}

// objFieldString Извлечь поля верхнего уровня string Значение.
func objFieldString(t *testing.T, out []byte, key string) (string, bool) {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(out, &m); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	s, ok := m[key].(string)
	return s, ok
}

// TestInjectThinkingDeepSeekExplicitControl При явном управлении клиентом обязательно соблюдать:
// thinking.type Непустой (enabled/disabled）не должны перезаписываться.
func TestInjectThinkingDeepSeekExplicitControl(t *testing.T) {
	cases := []struct {
		name string
		body string
		wantTyp string
		wantEff string // Ожидается reasoning_effort значение;"" И wantEffAbsent=true означает удалить
	}{
		{"уже есть enabled Не изменять",
			`{"model":"deepseek-v4-flash","thinking":{"type":"enabled"},"messages":[]}`,
			"enabled", ""},
		{"уже есть enabled и с reasoning_effort Сохранять (не удалять effort）",
			`{"model":"deepseek-v4-flash","thinking":{"type":"enabled"},"reasoning_effort":"high","messages":[]}`,
			"enabled", "high"},
		{"уже есть disabled сохранить (уважение намерения закрытия)",
			`{"model":"deepseek-v4-flash","thinking":{"type":"disabled"},"messages":[]}`,
			"disabled", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			out := PrepareBodyOptWithEfforts([]byte(c.body), false, nil)
			typ, present := getThinkingType(t, out)
			if !present {
				t.Fatalf("thinking Отсутствует поле (out=%s)", out)
			}
			if typ != c.wantTyp {
				t.Errorf("thinking.type = %q want %q (out=%s)", typ, c.wantTyp, out)
			}
			if c.wantEff != "" {
				if eff, _ := objFieldString(t, out, "reasoning_effort"); eff != c.wantEff {
					t.Errorf("reasoning_effort = %q want %q (out=%s)", eff, c.wantEff, out)
				}
			}
		})
	}
}

// TestInjectThinkingDisabledDeletesEffort Явно disabled Время reasoning_effort Удалить вместе
// （копировать официальный клиент case "deepseek" поведение: ветка по умолчанию delete reasoning_effort）。
// snow/camel удалить оба поля.
func TestInjectThinkingDisabledDeletesEffort(t *testing.T) {
	cases := []struct {
		name string
		body string
	}{
		{"disabled + snake effort Удалить",
			`{"model":"deepseek-v4-flash","thinking":{"type":"disabled"},"reasoning_effort":"high","messages":[]}`},
		{"disabled + camel effort Удалить",
			`{"model":"deepseek-v4-flash","thinking":{"type":"disabled"},"reasoningEffort":"high","messages":[]}`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			out := PrepareBodyOptWithEfforts([]byte(c.body), false, nil)
			typ, present := getThinkingType(t, out)
			if !present || typ != "disabled" {
				t.Fatalf("thinking.type = %q present=%v want disabled (out=%s)", typ, present, out)
			}
			for _, k := range []string{"reasoning_effort", "reasoningEffort"} {
				if _, ok := objFieldString(t, out, k); ok {
					t.Errorf("%s должно быть удалено (явно disabled при)(out=%s)", k, out)
				}
			}
		})
	}
}

// TestInjectThinkingSkipNonDeepSeek не deepseek нулевые изменения модели: нет thinking Нельзя добавлять из ничего,
// уже есть thinking Сохранить как есть.
func TestInjectThinkingSkipNonDeepSeek(t *testing.T) {
	cases := []struct {
		name string
		body string
	}{
		{"glm отсутствует thinking Не инжектировать", `{"model":"glm-5.2","messages":[]}`},
		{"glm уже есть thinking сохранить", `{"model":"glm-5.2","thinking":{"type":"enabled"},"messages":[]}`},
		{"kimi отсутствует thinking Не инжектировать", `{"model":"kimi-k2.5","messages":[]}`},
		{"qwen система не инжектирует", `{"model":"qwen2.5-coder-32b","messages":[]}`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			// записать, содержит ли вход уже thinking и значения, вывод должен быть побитово семантически идентичен.
			var in map[string]any
			if err := json.Unmarshal([]byte(c.body), &in); err != nil {
				t.Fatalf("unmarshal input: %v", err)
			}
			inTh, inHadThink := in["thinking"]
			out := PrepareBodyOptWithEfforts([]byte(c.body), false, nil)
			var got map[string]any
			if err := json.Unmarshal(out, &got); err != nil {
				t.Fatalf("unmarshal out: %v (out=%s)", err, out)
			}
			outTh, outHadThink := got["thinking"]
			if inHadThink != outHadThink {
				t.Errorf("thinking изменение характера появления: in=%v(had=%v) out=%v(had=%v)",
					inTh, inHadThink, outTh, outHadThink)
			}
			if inHadThink {
				// сохранённый thinking исходные значения должны совпадать (JSON уровень семантики).
				inJSON, _ := json.Marshal(inTh)
				outJSON, _ := json.Marshal(outTh)
				if string(inJSON) != string(outJSON) {
					t.Errorf("thinking было изменено: in=%s out=%s", inJSON, outJSON)
				}
			}
			// не deepseek Добавление запрещено thinking связанные поля; разрешённые новые поля:
			// stream（принудительный стриминг)+ stream_options（D7 include_usage，CLI потоковая обязательная отправка).
			allowedNew := map[string]bool{"stream": true, "stream_options": true}
			if len(got) > len(in)+len(allowedNew) {
				for k := range got {
					if _, had := in[k]; !had && !allowedNew[k] {
						t.Errorf("не deepseek Новое поле %q (out=%s)", k, out)
					}
				}
			}
		})
	}
}

// TestInjectThinkingStringPreserved инъекция не должна ломать model/messages и др. существующие поля.
func TestInjectThinkingStringPreserved(t *testing.T) {
	out := PrepareBodyOptWithEfforts(
		[]byte(`{"model":"deepseek-v4-flash","messages":[{"role":"user","content":"hi"}],"temperature":0.7}`),
		false, nil)
	var m map[string]any
	if err := json.Unmarshal(out, &m); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if m["model"] != "deepseek-v4-flash" || m["temperature"] != 0.7 {
		t.Errorf("существующие поля изменены: %v", m)
	}
	if _, ok := m["messages"].([]any); !ok {
		t.Errorf("messages нарушение структуры: %v", m)
	}
	if !strings.Contains(string(out), `"stream":true`) {
		t.Errorf("stream Не принудительно: %s", out)
	}
}
