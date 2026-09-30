// Домен выбора номера из пула: по realm（cn/global）фильтрация выбора и доступного набора.realm=="" Деградация к текущему состоянию.
package pool

import (
	"sort"
	"time"
)

// expiringVirtualSlots вес виртуального инстанса скоро истекающего аккаунта в кандидат-множестве новой сессии.
// 3:1 Это мягкое предпочтение, а не фиксированная пропорция: изменение состава аккаунтов естественно меняет итоговую долю.
const expiringVirtualSlots = 3

// AvailableUIDsForRealm Совм. AvailableUIDs，но возвращает только Realm()==realm аккаунта.
// realm=="" деградирует в AvailableUIDs（текущая семантика).
func (p *Pool) AvailableUIDsForRealm(realm string) []string {
	p.mu.RLock()
	defer p.mu.RUnlock()
	now := time.Now()
	uids := make([]string, 0, len(p.byUID))
	for uid, e := range p.byUID {
		if realm != "" && e.a.Realm() != realm {
			continue
		}
		if !e.healthy(now) {
			continue
		}
		if p.inFlightFull(e) {
			continue
		}
		uids = append(uids, uid)
	}
	sort.Strings(uids)
	return uids
}

// WeightedAvailableUIDsForModelRealm Вернуть список доступных аккаунтов с весом виртуального инстанса.
//
// у обычного аккаунта появилось 1 раз; появление валидных скоро истекающих аккаунтов expiringVirtualSlots раз. Вызывающая сторона продолжает по
// хеш исходного упорядоченного списка, позволяет новым сессиям мягко предпочитать скоро истекающие аккаунты. Дубликаты по UID После сортировки
// Развернуть, гарантируя единый список для разных процессов в топологии одного аккаунта.
//
// Данный метод — существующий AvailableUIDsForModelRealm инкрементальная точка входа, не меняет семантику старого метода и не
// изменение конфигурации, статуса или Redis schema。prefer_expiring=false в этом случае деградация до одного раза на аккаунт.
func (p *Pool) WeightedAvailableUIDsForModelRealm(model, realm string) []string {
	p.mu.RLock()
	defer p.mu.RUnlock()
	now := time.Now()
	uids := make([]string, 0, len(p.byUID))
	for uid, e := range p.byUID {
		if realm != "" && e.a.Realm() != realm {
			continue
		}
		if !e.healthyForModel(now, model) {
			continue
		}
		if p.inFlightFull(e) {
			continue
		}
		uids = append(uids, uid)
	}
	sort.Strings(uids)

	out := make([]string, 0, len(uids))
	for _, uid := range uids {
		slots := 1
		if p.preferExpiring {
			if e := p.byUID[uid]; expiringNow(e, now) {
				slots = expiringVirtualSlots
			}
		}
		for i := 0; i < slots; i++ {
			out = append(out, uid)
		}
	}
	return out
}

// AvailableUIDsForModelRealm Совм. AvailableUIDsForModel，но возвращает только Realm()==realm аккаунта
// （6004 исключение для модели по-прежнему действует).realm==«« деградирует в AvailableUIDsForModel。
func (p *Pool) AvailableUIDsForModelRealm(model, realm string) []string {
	p.mu.RLock()
	defer p.mu.RUnlock()
	now := time.Now()
	uids := make([]string, 0, len(p.byUID))
	for uid, e := range p.byUID {
		if realm != "" && e.a.Realm() != realm {
			continue
		}
		if !e.healthyForModel(now, model) {
			continue
		}
		if p.inFlightFull(e) {
			continue
		}
		uids = append(uids, uid)
	}
	sort.Strings(uids)
	return uids
}
