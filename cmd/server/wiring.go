package main

import (
	"github.com/linguo2625469/workbuddy2api-panel/internal/pool"
	"github.com/linguo2625469/workbuddy2api-panel/internal/server"
)

// realmAwareAvailableForModel Построение sticky-маршрутизации сессии по доступности модели realm Восприятие замыкания.
//
// имя модели при sticky-назначении может содержать realm Префикс ("global:gpt-5.4« / "cn:glm-5.2"）：Должен извлекаться по префиксу
// realm + bareModel，Затем передать на фильтрацию домена выбора пула — иначе голое имя берёт весь пул,global номер будет sticky-назначен на
// CN запрос с префиксом (кросс- realm утечка). Голое имя/Явно cn → cn Набор;global: → global Коллекция.
//
// realm когда пустая строка pool.WeightedAvailableUIDsForModelRealm Деградация к текущему состоянию
// （AvailableUIDsForModel），Старые вызовы (имя модели без префикса) без изменения семантики.
//
// Список в ответе может дублировать одну и ту же запись для скоро истекающих аккаунтов UID，как вес виртуального инстанса; хеш-распределение сессий не требует осведомленности
// детали весов: быстрый путь с уже привязанным аккаунтом по-прежнему возвращает исходный аккаунт без миграции.
func realmAwareAvailableForModel(p *pool.Pool) func(model string) []string {
	return func(model string) []string {
		realm, bare := server.ResolveModel(model)
		return p.WeightedAvailableUIDsForModelRealm(bare, realm)
	}
}
