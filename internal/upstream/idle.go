// idle.go Чат SSE Мониторинг простоя в потоке: при активной отдаче данных продлевать без разрыва, рвать поток только при тишине сверх порога (освобождение лиза).
package upstream

import (
	"context"
	"io"
	"sync"
	"time"
)

// idleMonitoringBody в составе чата SSE body Внешний уровень:
// Каждый раз чтение до низкоуровневых данных (n>0）то обновить lastRead；бэкенд goroutine Периодическая проверка,
// тишина более idle — cancel Запрос context，прервать блокирующий Read。
type idleMonitoringBody struct {
	rc io.ReadCloser
	mu sync.Mutex
	lastRead time.Time
	stopOnce sync.Once
	stopCh chan struct{}
	cancel context.CancelFunc
}

func (b *idleMonitoringBody) Read(p []byte) (int, error) {
	n, err := b.rc.Read(p)
	if n > 0 {
		b.mu.Lock()
		b.lastRead = time.Now()
		b.mu.Unlock()
	}
	return n, err
}

// Close Остановить фон goroutine、отменить запрос context、закрыть underlying stream, гарантировать отсутствие утечек.
func (b *idleMonitoringBody) Close() error {
	b.stopOnce.Do(func() { close(b.stopCh) })
	b.cancel()
	return b.rc.Close()
}

func (b *idleMonitoringBody) idleFor() time.Duration {
	b.mu.Lock()
	defer b.mu.Unlock()
	return time.Since(b.lastRead)
}

// monitorBody Если idle<=0 прямой возврат исходного потока (мониторинг idle отключён);
// Иначе — мониторинг простоя апстрима. Отсчёт с возврата body начнётся после — этап первого байта обеспечивает
// Transport.ResponseHeaderTimeout управляет, здесь без опережения.
func monitorBody(rc io.ReadCloser, idle time.Duration, cancel context.CancelFunc) io.ReadCloser {
	if idle <= 0 {
		return rc
	}
	b := &idleMonitoringBody{
		rc: rc,
		lastRead: time.Now(),
		stopCh: make(chan struct{}),
		cancel: cancel,
	}
	go func() {
		t := time.NewTicker(idleTick(idle))
		defer t.Stop()
		for {
			select {
			case <-b.stopCh:
				return
			case <-t.C:
				if b.idleFor() > idle {
					cancel()
					return
				}
			}
		}
	}()
	return b
}

// idleTick Период мониторинга возврата:idle/4，Кламп на [10ms, 1s]。Малый idle также быстро обнаруживается, мин/макс значения предотвращают холостой ход.
func idleTick(idle time.Duration) time.Duration {
	d := idle / 4
	if d > time.Second {
		d = time.Second
	}
	if d < 10*time.Millisecond {
		d = 10 * time.Millisecond
	}
	return d
}
