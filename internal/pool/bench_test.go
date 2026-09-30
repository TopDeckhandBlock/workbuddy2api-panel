package pool

import (
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
)

// Данный файл — P количественный базис для группового perf-ревью (go test -bench воспроизводимо), вывод см. REVIEW-conflicts-perf.md。

// benchPool Сборка 46 пул аккаунтов (выровнен с продом), все healthy И credits Все различны.
func benchPool(b *testing.B) *Pool {
	p := New("")
	for i := 0; i < 46; i++ {
		p.Add(&auth.Auth{UID: fmt.Sprintf("u%02d", i)})
		p.SetCredits(fmt.Sprintf("u%02d", i), int64(1000-i*13%900), 0)
	}
	return p
}

// BenchmarkPick46Accounts P1：46 Полное сканирование аккаунтов + Полная сортировка + время каждой жеребьёвки по трёхфакторным весам.
func BenchmarkPick46Accounts(b *testing.B) {
	p := benchPool(b)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		p.Pick()
	}
}

// BenchmarkStateSerialize46Accounts P2：46 Аккаунт state.json Сериализация (stateOverviewLocked + MarshalIndent）。
func BenchmarkStateSerialize46Accounts(b *testing.B) {
	p := benchPool(b)
	p.mu.Lock()
	sf := p.stateOverviewLocked()
	p.mu.Unlock()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := json.MarshalIndent(sf, "", " "); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkStateSerializeCompact46 P2 сравнение:json.Marshal（без отступа) затраты времени, количественная нижняя граница write amplification.
func BenchmarkStateSerializeCompact46(b *testing.B) {
	p := benchPool(b)
	p.mu.Lock()
	sf := p.stateOverviewLocked()
	p.mu.Unlock()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := json.Marshal(sf); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkSnapshotMarshal46 P2/P4 Связанное:saveLocked При сохранении на диск дополнительно зеркалировать один раз snapshot（Содержит savedAt）стоимость сериализации.
func BenchmarkSnapshotMarshal46(b *testing.B) {
	p := benchPool(b)
	p.mu.Lock()
	sf := p.stateOverviewLocked()
	p.mu.Unlock()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		now := time.Now()
		if _, err := json.Marshal(snapshot{stateFile: sf, SavedAt: now}); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkGoroutineSpawn P4 Квантование: за один раз fire-and-forget goroutine пейсинговый CPU Нижняя граница стоимости
// （Соответствует Session.SetBind / pool SaveState Каждое порождение образа goroutine накладные расходы, без учета сети,
//
//	Сеть через 5s Тайм-аут fire-and-forget）。
func BenchmarkGoroutineSpawn(b *testing.B) {
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		go func() { _ = 1 }()
	}
}
