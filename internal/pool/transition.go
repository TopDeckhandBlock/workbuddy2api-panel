// Единственная авторитетная реализация переходов автомата состояний аккаунта.
//
// entry "опциональность» определяется четырьмя ортогональными измерениями: disabled(disabled)、Кулдаун на уровне аккаунта(until/coolKind)、
// Кулдаун на уровне модели(modelCooldowns)、Circuit Breaker(breakerUntil)。Между измерениями сводится "примитивом миграции»,
// Запрещено разбрасывать эти поля по другим файлам — все точки входа (applyErrorPolicy / refresh / keepalive /
// check-in / выбора номера) изменения состояния должны идти через примитивы этого файла или через Cooldown/NoteError/NoteSuccess и т.д.
// Инкапсуляция (они вызывают примитивы этого файла под блокировкой).
//
// Матрица миграции (событие → Действие → поле):
//
//	disabled ← disableLocked（Disable / NoteSessionDead достижение порога)
//	until/coolKind ← Cooldown(CoolSoft/Hard) / CooldownSoftForModel Ветка без парсинга
//	modelCooldowns ← CooldownSoftForModel Есть ветка парсинга; disableLocked/Cooldown/clearCoolingLocked Очистить
//	 （reviveCoolingLocked не сброшен — восстановление баланса не является доказательством снятия лимита)
//	breakerUntil ← recordBreakerFailureLocked（Cooldown/NoteError подача);NoteSuccess Очистить
//	softStreak ← Cooldown(CoolSoft)/CooldownSoftForModel；NoteSuccess Очистка (revive сохранение: не зависит от баланса)
//	sessionDeadFails ← NoteSessionDead；ClearSessionDead/NoteSuccess/ReviveDisabled Очистить
//
// Ключевая ортогональность (сомнение 4 исправление):
// - Домен охлаждения (until/coolKind/softStreak/modelCooldowns）и circuit breaker (fails/retryCount/
// breakerUntil）Ортогонально: кулдаун — "недавно под ограничением»/баланс исчерпан», предохранитель для "повторных 5xx сбой».
// disableLocked очищается только домен кулдауна, без сброса circuit breaker — блокировка это авторизация/session Терминальное состояние, не должно перекрывать наблюдения circuit breaker.
// - clearCoolingLocked Является единственным источником "сброса cooldown-домена», disableLocked общий
// （отключение — терминальное состояние, кулдаун при этом аннулируется).reviveCoolingLocked（check-in/разморозка при обновлении баланса)**Больше не**
// Полная очистка: восстановление баланса — только разморозкой CoolHard，мягкий лимит и учет на уровне модели имеют свое время восстановления
// （см. подробнее reviveCoolingLocked комментарий).
package pool

import "time"

// clearCoolingLocked очистка домена охлаждения:until/coolKind/softStreak/modelCooldowns полный сброс в ноль,
// reason Очистить вместе. Предохранитель (fails/retryCount/breakerUntil）не входит в домен cooldown — не трогать.
// вызывающая сторона должна уже удерживать p.mu。
func (e *entry) clearCoolingLocked() {
	e.until = time.Time{}
	e.coolKind = 0
	e.reason = ""
	e.softStreak = 0
	e.modelCooldowns = nil // при сбросе домена охлаждения также сбрасывается независимый кулдаун уровня модели (освобождение модели исчезает)
}

// disableLocked отключить миграцию: установить disabled и очистить домен кулдауна (блокировка — более сильный терминальный статус недоступности, чем кулдаун).
//
// старый Disable установить только disabled+reason，Не трогать until/modelCooldowns/softStreak，появится
// 「disabled=true Но cooling=true / Остаток modelCooldowns」проблема консистентности — один сначала
// Жесткое охлаждение (до следующего дня 04:00）Повторно заблокированный аккаунт покажет два статуса одновременно. После блокировки кулдаун не имеет смысла
// （аккаунт вышел из выбора номера, дедлайн охлаждения больше не читается), поэтому очищается вместе.
//
// Сохранение circuit breaker: срабатывание — "последовательное 5xx сбой» (с авторизацией/вне сессии), после отключения и последующего восстановления
// Наблюдение circuit breaker остаётся активным, не должно перекрываться отключением.
func (p *Pool) disableLocked(e *entry, reason string) {
	e.clearCoolingLocked()
	e.disabled = true
	e.reason = reason
	p.dirty.Store(true)
}

// reviveCoolingLocked Разморозка при восстановлении баланса: очищать только**Кулдаун при исчерпании баланса**（CoolHard 
// until/coolKind/reason）И обновить credits/creditsTotal。
//
// Не изменять CoolSoft Backoff мягкого rate-limit,softStreak и modelCooldowns（6004 реестр уровня модели):
// доказательством восстановления двух последних является истечение сброса wall-clock апстрима или успешный пробинг, а не "на балансе есть деньги». Обновление баланса
// периодическая задача (каждые 5 минут) через ReenableIfCredits Достигнута эта точка — если здесь очистить весь домен кулдауна,
// Фактическое время жизни любого охлаждения лимитера сжато в один цикл обновления:6004 Ошибочное срабатывание лимита после очистки реестра
// здоров, перевыбрать и снова хит 429，Защита кулдауном всего пула фиктивна (воспроизведено на пуле из двух аккаунтов).softStreak
// также сохраняется: счётчик бэкоффа не зависит от баланса, определяется NoteSuccess（успех — сильнейшее доказательство восстановления) или естественное истечение
// Сходимость. Жёсткий кулдаун (CoolHard）авторитетное подтверждение восстановления — восстановление баланса (remain>0），разморозка как прежде.
// Не трогать circuit breaker (fails/retryCount/breakerUntil）——Успешный чекин подтверждает лишь восстановление баланса и
// billing Канал здоров ≠ доказательство chat канал исправен. Вызывающая сторона должна уже иметь p.mu。
func (p *Pool) reviveCoolingLocked(e *entry, credits, total int64) {
	e.credits = credits
	e.creditsTotal = total
	if e.coolKind == CoolHard {
		e.until = time.Time{}
		e.coolKind = 0
		e.reason = ""
	}
}
