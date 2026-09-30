package upstream

import (
	"encoding/json"
	"testing"
)

// assistantRC извлечь вывод messages внутри каждый assistant сообщения reasoning_content（Возврат при отсутствии поля ""+false）。
// возвращаемый слайс и messages Средний assistant сообщения соответствуют один к одному (не assistant сообщение пропущено).
func assistantRC(t *testing.T, out []byte) []string {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(out, &m); err != nil {
		t.Fatalf("unmarshal: %v (out=%s)", err, out)
	}
	var got []string
	msgs, _ := m["messages"].([]any)
	for _, mm := range msgs {
		msg, ok := mm.(map[string]any)
		if !ok {
			continue
		}
		role, _ := msg["role"].(string)
		if role != "assistant" {
			continue
		}
		if v, ok := msg["reasoning_content"].(string); ok {
			got = append(got, v)
		} else {
			got = append(got, "<absent>")
		}
	}
	return got
}

// TestBackfillReasoningContentDeepSeek DeepSeek Консистентность между раундами: история assistant сообщение имеет
// reasoning При следах все assistant Сообщение должно содержать reasoning_content（string）。
// выравнивание с официальным requiresReasoningContentOnAssistantMessages поведение.
func TestBackfillReasoningContentDeepSeek(t *testing.T) {
	cases := []struct {
		name string
		body string
		// ассертить только "assistant число сообщений» и "есть ли в каждом reasoning_content поле»,
		// значение — string достаточно (копия или пустая строка, проверяется конкретным кейсом).
		wantCount int
		wantVals []string // и assistant сообщения один-к-одному; пустая строка означает любое string
	}{
		{"assistant Лента reasoning отсутствует reasoning_content → Копировать",
			`{"model":"deepseek-v4-flash","messages":[
				{"role":"user","content":"u"},
				{"role":"assistant","content":"a","reasoning":"thought text"}]}`,
			1, []string{"thought text"}},
		{"assistant Лента reasoning_content сохранить как есть",
			`{"model":"deepseek-v4-flash","messages":[
				{"role":"assistant","content":"a","reasoning_content":"already there"}]}`,
			1, []string{"already there"}},
		{"Смешанные сессии дополняются полностью: нет reasoning assistant дополнить пустой строкой",
			`{"model":"deepseek-v4-flash","messages":[
				{"role":"user","content":"u"},
				{"role":"assistant","content":"a1","reasoning":"t1"},
				{"role":"user","content":"u2"},
				{"role":"assistant","content":"a2"}]}`,
			2, []string{"t1", ""}},
		// по новому контракту (гейт thinkingEnabled||hasTrace，issue #165）：L1 инжект enabled
		// после — ноль следов assistant также дополнить пустой строкой — ожидается из <absent> Изменить на ""（существует string）。
		{"assistant reasoning пустая строка → считается нулевым следом, но enabled дополнить пустой строкой",
			`{"model":"deepseek-v4-flash","messages":[
				{"role":"assistant","content":"a","reasoning":""}]}`,
			1, []string{""}},
		{"много assistant все с reasoning Копировать всё",
			`{"model":"deepseek-v4-flash","messages":[
				{"role":"assistant","content":"a1","reasoning":"r1"},
				{"role":"assistant","content":"a2","reasoning":"r2"}]}`,
			2, []string{"r1", "r2"}},
		// Официальный "string«!=typeof семантика: не string reasoning неоткуда копировать, выполняетсязаполнение "" Ветка.
		{"assistant reasoning не string Значение (число)→ дополнить пустой строкой",
			`{"model":"deepseek-v4-flash","messages":[
				{"role":"assistant","content":"a","reasoning":123}]}`,
			1, []string{""}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			out := PrepareBodyOptWithEfforts([]byte(c.body), false, nil)
			got := assistantRC(t, out)
			if len(got) != c.wantCount {
				t.Fatalf("assistant кол-во сообщений = %d want %d (out=%s)", len(got), c.wantCount, out)
			}
			for i, want := range c.wantVals {
				if want == "" {
					continue // произвольный string（дополнить пустой строкой/копии принимаются, но должны существовать)
				}
				if got[i] != want {
					t.Errorf("assistant[%d].reasoning_content = %q want %q (out=%s)", i, got[i], want, out)
				}
			}
		})
	}
}

// TestBackfillReasoningContentNoTrace сессия без reasoning след + thinking disabled
// → Без изменений: не выдавать зря assistant сообщение плюс reasoning_content Поле.
// Официальный ReasoningContentBackfillRule гейт = thinkingEnabled || hasTrace；disabled
// и при отсутствии следов обе половины не подсвечиваются → Не дополнять (issue #165 до этого кейс не различается disabled независимо — не компенсировать,
// Теперь ужесточено по новому контракту до disabled Форма — чисто text + enabled Дополнение пустой строки —
// TestBackfillZeroTraceThinkingEnabled перекрытие).
func TestBackfillReasoningContentNoTrace(t *testing.T) {
	cases := []struct {
		name string
		body string
	}{
		{"disabled + Чистый text assistant Не изменять",
			`{"model":"deepseek-v4-flash","thinking":{"type":"disabled"},"messages":[
				{"role":"user","content":"u"},
				{"role":"assistant","content":"plain answer"}]}`},
		{"отсутствует assistant сообщение без изменений",
			`{"model":"deepseek-v4-flash","messages":[
				{"role":"user","content":"u"}]}`},
		{"messages при отсутствии — без действия",
			`{"model":"deepseek-v4-flash"}`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			out := PrepareBodyOptWithEfforts([]byte(c.body), false, nil)
			for _, rc := range assistantRC(t, out) {
				if rc != "<absent>" {
					t.Errorf("отсутствует reasoning След же добавлен reasoning_content=%q (out=%s)", rc, out)
				}
			}
		})
	}
}

// TestBackfillReasoningContentNonDeepSeek не deepseek Без изменений модели:
// reasoning поля остаются без изменений, без добавления reasoning_content。
func TestBackfillReasoningContentNonDeepSeek(t *testing.T) {
	body := `{"model":"glm-5.2","messages":[
		{"role":"assistant","content":"a","reasoning":"thought"}]}`
	out := PrepareBodyOptWithEfforts([]byte(body), false, nil)
	for _, rc := range assistantRC(t, out) {
		if rc != "<absent>" {
			t.Errorf("не deepseek Не должен backfill, got reasoning_content=%q (out=%s)", rc, out)
		}
	}
}

// TestBackfillReasoningContentBothFields одновременно с reasoning и reasoning_content：
// по reasoning_content считается эталоном (без перезаписи),reasoning Поле сохранено (совместимость) — выравнивание с клиентом matches Правило.
func TestBackfillReasoningContentBothFields(t *testing.T) {
	body := `{"model":"deepseek-v4-flash","messages":[
		{"role":"assistant","content":"a","reasoning":"t","reasoning_content":"existing"}]}`
	out := PrepareBodyOptWithEfforts([]byte(body), false, nil)
	got := assistantRC(t, out)
	if len(got) != 1 || got[0] != "existing" {
		t.Errorf("reasoning_content Следует использовать уже имеющееся значение: got %v (out=%s)", got, out)
	}
	// одновременно подтвердить reasoning Поле сохраняется как есть.
	var m map[string]any
	if err := json.Unmarshal(out, &m); err != nil {
		t.Fatal(err)
	}
	msgs, _ := m["messages"].([]any)
	first, _ := msgs[0].(map[string]any)
	if r, ok := first["reasoning"].(string); !ok || r != "t" {
		t.Errorf("reasoning поле изменено: %v (out=%s)", first, out)
	}
}

// TestBackfillComposesWithInjectThinking backfill и injectThinking Комбинированная семантика:
// Явно disabled Время reasoning_effort Удален, но backfill hasTrace Вторая половина работает как обычно
// （Многократная консистентность не теряется при отключении цепочки рассуждений);disabled + Без следа — без изменений (thinkingEnabled половина не светится).
func TestBackfillComposesWithInjectThinking(t *testing.T) {
	// R6：disabled + Есть след → Компенсировать копированием ( reasoning）。
	body := `{"model":"DEEPSEEK-v4-flash","thinking":{"type":"disabled"},"reasoning_effort":"high","messages":[
		{"role":"user","content":"u"},
		{"role":"assistant","content":"a","reasoning":"thought"}]}`
	out := PrepareBodyOptWithEfforts([]byte(body), false, nil)
	got := assistantRC(t, out)
	if len(got) != 1 || got[0] != "thought" {
		t.Errorf("disabled Время backfill все равно должен действовать: got %v (out=%s)", got, out)
	}
	if typ, present := getThinkingType(t, out); !present || typ != "disabled" {
		t.Errorf("thinking.type следует сохранить disabled, got %q present=%v", typ, present)
	}
	for _, k := range []string{"reasoning_effort", "reasoningEffort"} {
		if _, ok := objFieldString(t, out, k); ok {
			t.Errorf("%s должен быть удален (disabled при)", k)
		}
	}

	// R2：disabled + Нулевой след → без изменений (одна из четырех веток нового контракта).
	body = `{"model":"DEEPSEEK-v4-flash","thinking":{"type":"disabled"},"messages":[
		{"role":"user","content":"u"},
		{"role":"assistant","content":"plain"}]}`
	out = PrepareBodyOptWithEfforts([]byte(body), false, nil)
	for _, rc := range assistantRC(t, out) {
		if rc != "<absent>" {
			t.Errorf("disabled+Нулевой след Не должен backfill, got reasoning_content=%q (out=%s)", rc, out)
		}
	}
}

// TestBackfillZeroTraceThinkingEnabled issue #165 Якорь воспроизведения (R1）：многокруговой без следов deepseek，
// Через L1 injectThinking инжект enabled после,thinkingEnabled Половина подсвечена → Каждая assistant
// Гарантировать reasoning_content Да string（здесь нет reasoning копируемо, всё дополнить пустыми строками).
// Официальный ReasoningContentBackfillRule гейт — thinkingEnabled || hasTrace，
// при потере сторонним клиентом обратного вызова инференса (zero-trace) официал всё равно восполняет, шлюз ранее портировал только hasTrace половина.
func TestBackfillZeroTraceThinkingEnabled(t *testing.T) {
	body := `{"model":"deepseek-v4-flash","messages":[
		{"role":"user","content":"u1"},
		{"role":"assistant","content":"a1"},
		{"role":"user","content":"u2"},
		{"role":"assistant","content":"a2"},
		{"role":"user","content":"u3"}]}`
	out := PrepareBodyOptWithEfforts([]byte(body), false, nil)
	// прекондиция: исходящий действительно после инъекции enabled форма (thinkingEnabled определение тела запроса после read-инъекции).
	if typ, present := getThinkingType(t, out); !present || typ != "enabled" {
		t.Fatalf("thinking.type=%q present=%v want enabled (out=%s)", typ, present, out)
	}
	got := assistantRC(t, out)
	if len(got) != 2 {
		t.Fatalf("assistant кол-во сообщений = %d want 2 (out=%s)", len(got), out)
	}
	for i, rc := range got {
		if rc != "" { // Существующий string（включая пустую строку) считается выполненным;<absent>（ключ отсутствует) — условие не выполнено
			t.Errorf("Нулевой след enabled В режиме assistant[%d].reasoning_content = %q want существует и равен \"\" (out=%s)", i, rc, out)
		}
	}
}

// TestBackfillNullAndNonStringNormalized issue #165 null/не string Нормализация (R3）：
// Официальное условие пропуска — "string"!=typeof reasoning_content только затем действует —null/число будет старым кодом
// if _, ok（ключ существует — пропуск) как "уже есть» пропуск, новый контракт нормализуется в ""。
// Внимание hasTrace Половина:reasoning_content само наличие ключа — уже след, гейт обязательно сработает.
func TestBackfillNullAndNonStringNormalized(t *testing.T) {
	cases := []struct {
		name string
		body string
	}{
		{"reasoning_content:null нормализовать в пустую строку",
			`{"model":"deepseek-v4-flash","messages":[
				{"role":"assistant","content":"a","reasoning_content":null}]}`},
		{"reasoning_content:Число нормализовать в пустую строку",
			`{"model":"deepseek-v4-flash","messages":[
				{"role":"assistant","content":"a","reasoning_content":123}]}`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			out := PrepareBodyOptWithEfforts([]byte(c.body), false, nil)
			got := assistantRC(t, out)
			if len(got) != 1 {
				t.Fatalf("assistant кол-во сообщений = %d want 1 (out=%s)", len(got), out)
			}
			if got[0] != "" || got[0] == "<absent>" {
				t.Errorf("reasoning_content должно быть нормализовано к \"\", got %q (out=%s)", got[0], out)
			}
		})
	}
}
