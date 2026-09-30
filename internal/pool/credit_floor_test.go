// гарантированный минимум баллов (credit floor）：Баланс аккаунта ниже floor при, для фактически тарифицируемой модели (tier 2）
// больше не участвует в выборе номера — чтобы платные запросы не пробили баланс и не затронули даже бесплатные модели 402 Кулдаун до чекина следующего дня.
// tier 0（бесплатно) и tier 1（без наблюдений) не затрагивается: гарантия — "остаток ещё доступен»,
// Не "ничего не трогать». Восстановление чекином (SetCreditsDetailed）После — автовосстановление.
package pool

import (
	"testing"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
)

// TestCreditFloorBlocksPaidBelowFloor Номер на дне отсекается в tier 2 кроме:
// ниже floor номер даже с максимальным весом баллов не может выписать билет для модели с фактической оплатой.
func TestCreditFloorBlocksPaidBelowFloor(t *testing.T) {
	withNoPickGap(t)
	p := New("")
	p.SetCostExploreInterval(0)
	p.SetCreditFloor(100)
	p.SetRandomSource(func(n int64) int64 { return 0 })
	p.Add(&auth.Auth{UID: "poor"})
	p.Add(&auth.Auth{UID: "rich"})

	// poor Достигнуто дно + фактическая оплата;rich баланс достаточен + Платно по факту замера.
	p.SetCredits("poor", 30, 0)
	p.SetCredits("rich", 1_000_000, 0)
	p.NoteModelCost("poor", "hy4-preview", 2.9, 1000)
	p.NoteModelCost("rich", "hy4-preview", 5.0, 1000)

	for i := 0; i < 20; i++ {
		a := p.PickExcludingForRealm(nil, "hy4-preview", "")
		if a == nil {
			t.Fatalf("№ %d возврат резервного номера nil，want rich（Номер на лимите затронут floor Перехват,rich должен обработать)", i)
		}
		if a.UID == "poor" {
			t.Fatalf("№ %d раз выбран poor（credits=30 < floor=100 и модель фактически платная),floor Следует перехватить", i)
		}
	}
}

// TestCreditFloorAllowsFreeBelowFloor Аккаунт на нуле для бесплатных моделей (tier 0）по-прежнему опционально:
// Цель страховки — "оставить баланс для бесплатных моделей», бесплатные запросы credit=0 Больше не списывать.
func TestCreditFloorAllowsFreeBelowFloor(t *testing.T) {
	withNoPickGap(t)
	p := New("")
	p.SetCostExploreInterval(0)
	p.SetCreditFloor(100)
	p.SetRandomSource(func(n int64) int64 { return 0 })
	p.Add(&auth.Auth{UID: "poor"})
	p.SetCredits("poor", 30, 0)
	p.NoteModelCost("poor", "hy4-preview", 0, 1000) // Фактически бесплатно

	a := p.PickExcludingForRealm(nil, "hy4-preview", "")
	if a == nil {
		t.Fatal("исчерпанные аккаунты для бесплатных моделей остаются выбираемыми,got nil")
	}
	if a.UID != "poor" {
		t.Fatalf("Выбрано %v，want poor（Бесплатные модели не подпадают под floor ограничение)", a.UID)
	}
}

// TestCreditFloorAllowsUnknownBelowFloor Аккаунт на дне для ненаблюдаемой модели (tier 1）по-прежнему опционально:
// Фактическая цена неизвестной модели не изучена, первый успех — зачисление и выпуск; если floor Подключение tier 1 блокировать всё,
// истечение леджера/После сброса при рестарте упёртые в дно аккаунты навсегда зависнут в дедлоке "невозможно обучиться заново».
func TestCreditFloorAllowsUnknownBelowFloor(t *testing.T) {
	withNoPickGap(t)
	p := New("")
	p.SetCostExploreInterval(0)
	p.SetCreditFloor(100)
	p.SetRandomSource(func(n int64) int64 { return 0 })
	p.Add(&auth.Auth{UID: "poor"})
	p.SetCredits("poor", 30, 0)
	// Безо всяких NoteModelCost：hy4-preview Для poor Да tier 1。

	a := p.PickExcludingForRealm(nil, "hy4-preview", "")
	if a == nil {
		t.Fatal("bottom-номер для моделей без наблюдений остаётся доступным,got nil")
	}
	if a.UID != "poor" {
		t.Fatalf("Выбрано %v，want poor（tier 1 Не зависит от floor ограничение)", a.UID)
	}
}

// TestCreditFloorAllBelowReturnsNil весь пул исчерпан + весь tier 2 при выборе номера вернуть nil：
// floor Это жёсткая семантика, лучше 503 И не пропускать платные запросы, пробивающие гарантию (пропуск=Вернуться к "прожиг до 0」текущее состояние).
func TestCreditFloorAllBelowReturnsNil(t *testing.T) {
	withNoPickGap(t)
	p := New("")
	p.SetCostExploreInterval(0)
	p.SetCreditFloor(100)
	p.Add(&auth.Auth{UID: "a"})
	p.Add(&auth.Auth{UID: "b"})
	for _, uid := range []string{"a", "b"} {
		p.SetCredits(uid, 10, 0)
		p.NoteModelCost(uid, "hy4-preview", 2.9, 1000)
	}

	if a := p.PickExcludingForRealm(nil, "hy4-preview", ""); a != nil {
		t.Fatalf("весь пул исчерпан и весь tier 2，want nil，got %v（floor Жесткая семантика: не пропускать)", a.UID)
	}
}

// TestCreditFloorRecoversAfterCheckin автовосстановление после чекина/восполнения:floor только чтение текущего credits，
// SetCreditsDetailed обновить авторитетный баланс свыше floor пропуск немедленно, без какого-либо сброса.
func TestCreditFloorRecoversAfterCheckin(t *testing.T) {
	withNoPickGap(t)
	p := New("")
	p.SetCostExploreInterval(0)
	p.SetCreditFloor(100)
	p.SetRandomSource(func(n int64) int64 { return 0 })
	p.Add(&auth.Auth{UID: "only"})
	p.SetCredits("only", 30, 0)
	p.NoteModelCost("only", "hy4-preview", 2.9, 1000)

	if a := p.PickExcludingForRealm(nil, "hy4-preview", ""); a != nil {
		t.Fatalf("В период достижения дна должен блокироваться,got %v", a.UID)
	}
	// Чекин-восстановление: авторитетный баланс обновлён до floor выше.
	p.SetCreditsDetailed("only", 500, 0, 0, time.Time{}, 0)
	a := p.PickExcludingForRealm(nil, "hy4-preview", "")
	if a == nil {
		t.Fatal("После восстановления (credits=500 ≥ floor=100）следует восстановить доступность,got nil")
	}
	if a.UID != "only" {
		t.Fatalf("Выбрано %v，want only", a.UID)
	}
}

// TestCreditFloorZeroDisables floor=0（по умолчанию) полностью отключено: поведение дословно как до внедрения.
func TestCreditFloorZeroDisables(t *testing.T) {
	withNoPickGap(t)
	p := New("")
	p.SetCostExploreInterval(0)
	// Не вызывать SetCreditFloor：0 = по умолчанию отключено.
	p.SetRandomSource(func(n int64) int64 { return 0 })
	p.Add(&auth.Auth{UID: "poor"})
	p.SetCredits("poor", 1, 0)
	p.NoteModelCost("poor", "hy4-preview", 2.9, 1000)

	a := p.PickExcludingForRealm(nil, "hy4-preview", "")
	if a == nil || a.UID != "poor" {
		t.Fatalf("floor=0 следует отключить гарантированный минимум (номер с оплатой по достижению дна остается опциональным),got %v", a)
	}
}

// TestCreditFloorBoundaryAtFloor credits строго равно floor тогда не блокировать:
// Семантика — "ниже floor только тогда блокировать» (credits < floor），равно floor всё ещё выше безопасного порога.
func TestCreditFloorBoundaryAtFloor(t *testing.T) {
	withNoPickGap(t)
	p := New("")
	p.SetCostExploreInterval(0)
	p.SetCreditFloor(100)
	p.SetRandomSource(func(n int64) int64 { return 0 })
	p.Add(&auth.Auth{UID: "edge"})
	p.NoteModelCost("edge", "hy4-preview", 2.9, 1000)
	// Внимание к порядку:NoteModelCost Будет реально списан баланс (локальная интерполяция), граничные значения ставить только после наблюдения.
	p.SetCredits("edge", 100, 0)

	a := p.PickExcludingForRealm(nil, "hy4-preview", "")
	if a == nil || a.UID != "edge" {
		t.Fatalf("credits=100 == floor=100 следует пропускать (блокировать только ниже),got %v", a)
	}
}

// TestCreditFloorStickyPathBlocked Липкий путь также floor Ограничение:
// аккаунт-дно даже при привязке сессией не должен продолжать выдавать билеты для платных моделей (PickByUIDForModel вернуть nil，
// handler отвязка и смена номера на стороне).
func TestCreditFloorStickyPathBlocked(t *testing.T) {
	p := New("")
	p.SetCreditFloor(100)
	p.Add(&auth.Auth{UID: "sticky"})
	p.SetCredits("sticky", 30, 0)
	p.NoteModelCost("sticky", "hy4-preview", 2.9, 1000)

	if a := p.PickByUIDForModel("sticky", "hy4-preview"); a != nil {
		t.Fatalf("sticky-номер достиг дна + Платная модель должна быть floor блокировать (want nil），got %v", a.UID)
	}
	// Бесплатные модели не затронуты: тот же sticky-ID штатно выдаёт тикеты.
	p.NoteModelCost("sticky", "free-model", 0, 1000)
	if a := p.PickByUIDForModel("sticky", "free-model"); a == nil {
		t.Fatal("sticky-номер достиг дна + бесплатная модель должна штатно выдавать тикет,got nil")
	}
}
