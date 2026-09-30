package server

import "strings"

// resolveModel Парсинг протокола имени модели (PLAN D6）：
//
//	Распределенный префикс: "[realm:]model"
//
// Взять первый ":«，предыдущий сегмент как раз «cn»/«global" только тогда отрезать; иначе считать голым именем,realm=cn、bare=исходная строка.
// Регистрозависимо (префикс должен быть точным enum в нижнем регистре).bare сразу egress/Выбор номера/Исходное имя модели, используемое в биллинге.
//
// экспортировать как ResolveModel（cmd/server/main.go требуется sticky-замыкание), внутри пакета — сокр. resolveModel。
func resolveModel(model string) (realm, bare string) {
	idx := strings.IndexByte(model, ':')
	if idx < 0 {
		return "cn", model
	}
	prefix := model[:idx]
	if prefix != "cn" && prefix != "global" {
		return "cn", model
	}
	return prefix, model[idx+1:]
}

// ResolveModel Да resolveModel экспортируемый интерфейс (межпакетный вызов).
func ResolveModel(model string) (realm, bare string) { return resolveModel(model) }
