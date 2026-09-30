// Package httpauth Общий для шлюза и панели Bearer Примитив аутентификации.
//
// причина выделения в отдельный пакет:server（/v1/*、/status）и panel（/panel/api/*）Аутентификация в двух местах
// должны быть полностью в одной метрике — ранее каждая сторона копировала себе экземпляр"Прямое сравнение строк"реализации, легко дрейфует,
// и оба несут тайминг side-channel. После унификации здесь критерий один и сравнение естественно в константное время.
package httpauth

import (
	"crypto/sha256"
	"crypto/subtle"
	"net/http"
	"strings"
)

// bearerPrefix префикс схемы аутентификации (чувствителен к регистру, с HTTP соответствует спецификации и существующей реализации).
const bearerPrefix = "Bearer "

// VerifyBearer проверка наличия корректного в заголовке запроса Bearer ключ.
//
// key пусто означает"Аутентификация не включена"，всегда возвращает true（Вызывающая сторона на этом основании пропускает).
// используется для сравнения SHA-256 сводка + subtle.ConstantTimeCompare：
// - константное время, без утечки по длине префиксного совпадения;
// - Сначала хеш, затем сравнение, разница длин поглощается хешем (нет раннего возврата из-за разной длины);
// - сам дайджест необратим, даже при боковом канале исходный ключ не получить.
func VerifyBearer(r *http.Request, key string) bool {
	if key == "" {
		return true
	}
	authz := r.Header.Get("Authorization")
	if !strings.HasPrefix(authz, bearerPrefix) {
		// Отсутствует заголовок/Неверный подход: все равно выполнить сравнение дайджестов, сохраняя профиль задержки.
		subtle.ConstantTimeCompare(digest(""), digest(key))
		return false
	}
	tok := authz[len(bearerPrefix):]
	return subtle.ConstantTimeCompare(digest(tok), digest(key)) == 1
}

// digest вернуть s SHA-256（Фиксированная длина 32 байт, для сравнения за константное время).
func digest(s string) []byte {
	sum := sha256.Sum256([]byte(s))
	return sum[:]
}
