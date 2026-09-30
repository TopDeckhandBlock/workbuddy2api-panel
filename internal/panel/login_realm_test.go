package panel

import (
	"testing"
)

// TestLoginEndpoints realm→маппинг эндпоинтов:global все три эндпоинта в workbuddy.ai домен,
// cn/пустой/Недопустимое значение → copilot.tencent.com（нулевой регресс как фолбэк).
func TestLoginEndpoints(t *testing.T) {
	cases := []struct {
		realm string
		wantState string
		wantOrigin string
	}{
		{"global", "https://www.workbuddy.ai/v2/plugin/auth/state?platform=CLI", "https://www.workbuddy.ai"},
		{"cn", "https://copilot.tencent.com/v2/plugin/auth/state?platform=CLI", "https://www.codebuddy.cn"},
		{"", "https://copilot.tencent.com/v2/plugin/auth/state?platform=CLI", "https://www.codebuddy.cn"},
		{"weird", "https://copilot.tencent.com/v2/plugin/auth/state?platform=CLI", "https://www.codebuddy.cn"},
	}
	for _, c := range cases {
		t.Run(c.realm, func(t *testing.T) {
			st, tok, acct, origin := loginEndpoints(c.realm)
			if st != c.wantState {
				t.Errorf("realm=%q state=%q want %q", c.realm, st, c.wantState)
			}
			if origin != c.wantOrigin {
				t.Errorf("realm=%q origin=%q want %q", c.realm, origin, c.wantOrigin)
			}
			// token/account Эндпоинт должен совпадать с state Совм. base。
			if c.realm == "global" {
				if tok[:len("https://www.workbuddy.ai")] != "https://www.workbuddy.ai" ||
					acct[:len("https://www.workbuddy.ai")] != "https://www.workbuddy.ai" {
					t.Errorf("global token/account endpoints should be workbuddy.ai: %q %q", tok, acct)
				}
			} else {
				if tok[:len("https://copilot.tencent.com")] != "https://copilot.tencent.com" ||
					acct[:len("https://copilot.tencent.com")] != "https://copilot.tencent.com" {
					t.Errorf("cn token/account endpoints should be copilot.tencent.com: %q %q", tok, acct)
				}
			}
		})
	}
}
