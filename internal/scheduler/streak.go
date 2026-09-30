// streak.go Менеджер серийного входа: после расписания чекинов автопроверка уровней обмена серии → если доступно к обмену — обменять → Розыгрыш по числу попыток.
//
// Фон (2026-09-12）：тариф ежедневного входа центра роста (7d/14d/28d）разблокировка по дням непрерывного входа, выдача при обмене
// credit/energy/Карта доп. чекина/попытки розыгрыша; попытки только через обмен. Кнопка обмена в UI всегда кликабельно,
// но пока не разблокировано — сервер 403「недостаточно дней входа подряд» — поэтому прогонять после ежедневного чекина (идемпотентно),
// В день достижения срока автоматически выполнить "обмен» → лотерея» — замкнутый цикл, без ручного контроля.
package scheduler

import (
	"encoding/json"
	"log"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/logfmt"
)

// RunStreakBonusNow Выполнить последовательный обмен для всех доступных аккаунтов + Розыгрыш (идемпотентно:locked/без лимита — автопропуск).
// По расписанию check-in (RunCheckinNow）вызов в конце; также ручной запуск с панели.
func (s *Scheduler) RunStreakBonusNow() {
	for _, st := range s.cfg.Pool.List() {
		if st.Disabled {
			continue
		}
		a := s.cfg.Pool.AuthByUID(st.UID)
		if a == nil || a.AccessTokenValue() == "" {
			continue
		}
		if a.IsGlobal() {
			continue // D4 Гейт:global отсутствует CN Система задач, без вызовов апстрима
		}
		s.streakBonusAccount(a)
	}
}

// streakBonusAccount Один аккаунт: доотметка для сохранения серии входов → Подарочный набор/компенсация → Обменять все разблокированные уровни → Выбраны все chances。
func (s *Scheduler) streakBonusAccount(a *auth.Auth) {
	// 0. доподписание для сохранения серии: если вчера пропуск и есть карта доподписания — восполнить (при разрыве серии отсчет начинается заново 7 дн.).
	s.makeupYesterday(a)
	// 0.5 Подарочный набор/компенсация (один раз на номер, иначе тихое пропускание бизнес-ошибки).
	if credit, err := s.cfg.Upstream.ClaimGift(a); err == nil {
		log.Printf("streak-bonus %s: 🎊 Стартовый набор +%dc", logfmt.Label(a.UID, a.Nickname), credit)
	}
	if credit, err := s.cfg.Upstream.ClaimCompensation(a); err == nil {
		log.Printf("streak-bonus %s: 🎊 Компенсационное получение +%dc", logfmt.Label(a.UID, a.Nickname), credit)
	}

	full, err := s.cfg.Upstream.GrowthStreakFull(a)
	if err != nil {
		log.Printf("streak-bonus %s: %v", logfmt.Label(a.UID, a.Nickname), err)
		return
	}
	statuses := map[string]string{
		"7d": full.RedemptionStatus.Tier7dStatus,
		"14d": full.RedemptionStatus.Tier14dStatus,
		"28d": full.RedemptionStatus.Tier28dStatus,
	}
	for _, tier := range full.RedemptionStatus.Tiers {
		status := statuses[tier.Tier]
		if status == "locked" || status == "claimed" {
			continue
		}
		if err := s.cfg.Upstream.GrowthRedeemTier(a, tier.Tier); err != nil {
			// Не разблокировано (403）ожидаемо — игнорировать; остальное логировать.
			log.Printf("streak-bonus %s: redeem %s: %v", logfmt.Label(a.UID, a.Nickname), tier.Tier, err)
			continue
		}
		log.Printf("streak-bonus %s: ★ обмен %s уровень (+%dc +%de Карта×%d Розыгрыш×%d）",
			a.UID, tier.Tier, tier.Credit, tier.Energy, tier.Cards, tier.Chances)
	}
	// розыгрыш: по текущему chances Полностью выбрано (выданные обменом попытки уже начислены на сервере).
	chances, err := s.cfg.Upstream.LotteryChances(a)
	if err != nil {
		log.Printf("streak-bonus %s: lottery summary: %v", logfmt.Label(a.UID, a.Nickname), err)
		return
	}
	for i := 0; i < chances; i++ {
		raw, err := s.cfg.Upstream.LotteryDraw(a)
		if err != nil {
			log.Printf("streak-bonus %s: draw: %v", logfmt.Label(a.UID, a.Nickname), err)
			return
		}
		log.Printf("streak-bonus %s: 🎲 №%dИзвлечь %s", logfmt.Label(a.UID, a.Nickname), i+1, compactJSON(raw))
	}
	if chances > 0 {
		log.Printf("streak-bonus %s: розыгрыш завершён %d Раз", logfmt.Label(a.UID, a.Nickname), chances)
	}
}

// compactJSON усечение payload призов (читаемость лога в одну строку).
func compactJSON(raw json.RawMessage) string {
	s := string(raw)
	if len(s) > 220 {
		return s[:220] + "…"
	}
	return s
}

// makeupYesterday при пропуске вчера и наличии картыДоп. подпись — автодокомпенсация (сохранение streak непрерывных дней).
// Без карты / без пропусков / ошибки запроса — silent (не влияют на основной поток).
func (s *Scheduler) makeupYesterday(a *auth.Auth) {
	missed, err := s.cfg.Upstream.HeatmapYesterdayMissed(a)
	if err != nil || !missed {
		return
	}
	full, err := s.cfg.Upstream.GrowthStreakFull(a)
	if err != nil || full.MakeupCards.Balance <= 0 {
		return
	}
	yesterday := time.Now().AddDate(0, 0, -1).Format("2006-01-02")
	if err := s.cfg.Upstream.UseMakeupCard(a, yesterday); err != nil {
		log.Printf("streak-bonus %s: Доп. чекин %s ошибка: %v", logfmt.Label(a.UID, a.Nickname), yesterday, err)
		return
	}
	log.Printf("streak-bonus %s: ★ чекин восполнен картойдоп. подпись %s（Бао Ляньдэн)", logfmt.Label(a.UID, a.Nickname), yesterday)
}
