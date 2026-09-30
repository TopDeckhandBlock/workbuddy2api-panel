// backoff.go Единственный источник истины для ротационного бэкоффа и джиттера (WAF 403 исправление P0-2/P0-1 общий):
// основание экспоненты/Потолок/коэффициент jitter определяется в одном месте,chatCompletions ротационный бэкофф и WAF База мягкого кулдауна
// （handler.wafCooldownBase）Используют общий jitterDur，не в handler и upstream записать каждому по копии.
package server

import (
	"context"
	"math/rand/v2"
	"time"
)

// rotateBackoffBase базис ротационного бэкоффа (выравнивание с официальным intl CLI computeRequestRetryDelayMs
// 500ms форма,WAF 403 Отчёт о конкурентном исследовании §2.1/§6 P0-2）。в тесте можно установить 0 Пропустить ожидание
// （TestMain Уже установлено 0 ускорение теста ротации; тест assert'а границы бэкоффа временно восстановлен).
var rotateBackoffBase = 500 * time.Millisecond

const (
	// rotateBackoffCap Потолок ротационного бэкоффа (отчёт P0-2 Рекомендация 8s：Лимит ротации по умолчанию 3 раз,
	// фактическая очередь ожидания 500ms/1s，лимит ограничивает только при экстремальной конфигурации MaxRotate）。
	rotateBackoffCap = 8 * time.Second
	// jitterFraction Коэффициент джиттера (±25%，Выравнивание intl CLI delay×(1±0.25) форма).
	jitterFraction = 0.25
)

// jitterDur Применить ограничение к длительности ±jitterFraction равномерный джиттер, возвращает [d·(1-f), d·(1+f)] значение интервала.
// d<=0 Возврат как есть (нулевое ожидание без джиттера). Джиттер нужен для разнесения синфазных ретраев множества запросов (WAF Rate limit по
// штраф за плотность, синхронный бэкофф снова скучкуется с фиксированным периодом).
func jitterDur(d time.Duration) time.Duration {
	if d <= 0 {
		return d
	}
	f := 1 + (rand.Float64()*2-1)*jitterFraction
	out := time.Duration(float64(d) * f)
	if out < 0 {
		return 0
	}
	return out
}

// backoffAfter Вернуть № n ротация по раундам (0 База: до смены номера при первом сбое n=0）Длительность backoff перед ожиданием:
// base·2^n Потолок rotateBackoffCap，повторно наложить ±25% джиттер.base Установить 0（при тесте) всегда 0。
// Использовать последовательное удвоение вместо сдвига:base После корректировки не нужно синхронно поддерживать лимит сдвига, переполнение покрывается сравнением с потолком.
func backoffAfter(n int) time.Duration {
	d := rotateBackoffBase
	if d <= 0 {
		return 0
	}
	for k := 0; k < n && d < rotateBackoffCap; k++ {
		d *= 2
		if d <= 0 { // Переполнение при удвоении в неположительное: считать как достижение лимита
			return jitterDur(rotateBackoffCap)
		}
	}
	if d > rotateBackoffCap {
		d = rotateBackoffCap
	}
	return jitterDur(d)
}

// sleepCtx отменяемое ожидание:ctx отмена — немедленный возврат false（Отключение клиента/Graceful shutdown не ждёт бэкоффа
// проснулся), ждать заполнения и вернуть true。d<=0 немедленный пропуск. И scheduler.sleepCtx Тот же режим (функция не
// Экспорт и scheduler Не должен быть server Обратная зависимость, реализуется на стороне потребления по метрике "эквивалент» из ТЗ).
func sleepCtx(ctx context.Context, d time.Duration) bool {
	if d <= 0 {
		return ctx.Err() == nil
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}
