package panel

import (
	"testing"
)

// TestTaskAccountLockSameAccountExclusive мьютекс блокировки задач одного аккаунта: второй tryLock должен завершиться ошибкой,
// После разблокировки можно получить снова. Это ключевая гарантия "повторный клик — завершение в один клик без конкурентного перезапуска».
func TestTaskAccountLockSameAccountExclusive(t *testing.T) {
	p := &Panel{}
	uid := "u1"

	if !p.tryLockAccount(uid) {
		t.Fatal("первая блокировка должна успеть")
	}
	if p.tryLockAccount(uid) {
		t.Fatal("Вторая блокировка тем же аккаунтом должна завершиться неудачей (мьютекс)")
	}
	p.unlockAccount(uid)

	if !p.tryLockAccount(uid) {
		t.Fatal("После разблокировки должна быть возможна повторная блокировка")
	}
	p.unlockAccount(uid)
}

// TestTaskAccountLockDifferentAccountsIndependent Блокировки разных аккаунтов не влияют друг на друга (параллельность сохраняется).
func TestTaskAccountLockDifferentAccountsIndependent(t *testing.T) {
	p := &Panel{}
	if !p.tryLockAccount("u1") {
		t.Fatal("u1 Блокировка должна успешно установиться")
	}
	if !p.tryLockAccount("u2") {
		t.Fatal("u2 Блокировка должна успеть (разные аккаунты не взаимоисключаются)")
	}
	p.unlockAccount("u2")
	p.unlockAccount("u1")
}

// TestTaskAccountLockCrossEntryShared одна задача auto и полный объем auto_all используют одну блокировку аккаунта
// （В handler уровни идут через tryLockAccount，здесь проверка совпадения пространства имён блокировки).
func TestTaskAccountLockCrossEntryShared(t *testing.T) {
	p := &Panel{}
	if !p.tryLockAccount("u1") {
		t.Fatal("u1 Блокировка должна успешно установиться")
	}
	// Эмуляция полного входа для одного и того же uid Блокировка — должна быть отклонена (иначе два входа могут выполняться конкурентно).
	if p.tryLockAccount("u1") {
		t.Fatal("Совм. uid Блокировка через разные входы должна падать (общий лок)")
	}
	p.unlockAccount("u1")
}
