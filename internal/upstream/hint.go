// hint.go Доп. поле описания ошибки шлюза (error.gateway_hint）Единственный источник истины.
//
// дисциплина (техзадание gateway-hint）：
// - error.message всегда апстрим body Прозрачная передача оригинала (5755fe3 принцип сквозной передачи без изменений);
// gateway_hint Делать только с message **Параллельно**доп. пояснение с точки зрения шлюза, никогда не заменяет/Обёртка message。
// - Тексты сосредоточены в этом файле (один Kind таблица + 11133/11135 определение формы), по ErrKind + Контекст
// （запрос с изображением/сопоставление capabilities каталога моделей), без распыления handler if-else。
// - Для непокрытых форм возвращать пустую строку → ответ без этого поля (не выдумывать).
// - hint Формулировка на английском (ошибки ориентированы на клиентский toolchain, английский — общий стандарт).
package upstream

import (
	"encoding/json"
	"net/http"
	"strings"
)

// GatewayHint Возвращать доп. пояснение со стороны шлюза по типу ошибки (error.gateway_hint значение поля).
// msg это ошибка upstream body оригинал (или SSE error Кадр payload）；kind является авторитетной классификацией
// （Classify / *Error конверт). Вернуть пустую строку = непокрытая форма, вызывающая сторона не передаёт поле.
//
// ctx с передачей решения hint требуемый контекст на стороне запроса (нулевое значение валидно, при отсутствии данных связанная форма откатывается к нейтральной
// hint или отсутствует hint）：
// - HasImage：содержит ли тело запроса image_url part（11133 "модель не поддерживает изображения» указывает на предусловие);
// - Model / ModelInCatalog / ModelSupportsImages：Каталог моделей для этой модели
// supports_images заявление (каталог не содержит → не выносить решение "не поддерживается», чтобы отсутствие данных не считалось неподдержкой).
//
// Порядок проверки:11133/11135 бизнес-код апстрима**До** Kind таблица — фактически эти два семейства относятся к ErrClient/
// ErrBadParams всё возможно (Classify словарь не содержит 11133），hint Уровень со встроенной проверкой (hint является дополнением
// указывает на неавторитетную классификацию, цена ошибки — лишь одно доп. нейтральное пояснение); остальное по Kind Отображение один к одному.
func GatewayHint(kind ErrKind, msg string, ctx HintContext) string {
	// 11133 model_param_invalid семейство (регресс-тест изображений: передача изображения модели без поддержки изображений, или любая
	// параметр отклонен провайдером модели). Только если запрос действительно с изображением и каталог может выдать для этой модели "изображения не поддерживаются»
	// только при таком решении давать подсказку "смените модель», иначе — нейтральная форма параметра (возможно любая проблема с параметрами, без указания на изображение).
	if isModelParamInvalid(msg) {
		if ctx.HasImage && ctx.ModelInCatalog && !ctx.ModelSupportsImages {
			return "model " + ctx.Model + " does not support images; pick one with supports_images=true from /v1/models"
		}
		return "request parameters were rejected by the model provider; check message format and model capabilities"
	}
	// 11135 invalid_image_data Семейство (данные изображения невалидны,Discussion #77 фактическая форма).
	if isInvalidImageData(msg) {
		return "image data rejected by upstream; use a real/valid image, may need a new conversation"
	}
	switch kind {
	case ErrPromptTooLong:
		return "request context exceeds the model's limit; reduce history/message size"
	case ErrImageInvalid:
		return "image request was rejected by upstream; check image_url format and image data"
	case ErrWafBlock:
		// Уровень аккаунта WAF 403 и IP Уровень fail-fast Совм. hint：действия обоих для клиента идентичны
		// （ждать окна и повторить, сменить аккаунт/немедленный ретрай бессмыслен).
		return "upstream WAF blocked the gateway; retry after the block window"
	case ErrSoftRate:
		return "rate limited by upstream; retry after reset"
	case ErrAccountFault:
		return "account-level fault at upstream (auth/quota state); the gateway will rotate or disable this account"
	case ErrSessionDead:
		return "account session expired at upstream; the account is disabled until re-login"
	case ErrHardCredit:
		return "account credits exhausted at upstream; waiting for daily check-in to restore"
	case ErrModelBlocked:
		return "upstream has no such model on this backend; switch model or retry on another account"
	case ErrContentBlocked:
		// формулировка без "upstream"：content_blocked В ответе есть существующая метрика без упоминания upstream
		// （handler_test сторожа утечек),hint Соблюдается единый критерий.
		return "request content was rejected by content policy; adjust the prompt and retry"
	default:
		// ErrNone/ErrNotFound/ErrServer/ErrBadParams/ErrClient и др. непокрытые формы: отсутствует hint。
		return ""
	}
}

// HintContext gateway_hint контекст на стороне запроса для решения (handler Сборка на стороне, см.
// Handler.hintContext）。Нулевое значение допустимо.
type HintContext struct {
	Model string // Запрос: чистое имя модели (опционально)
	HasImage bool // содержит ли тело запроса image_url part
	ModelSupportsImages bool // Каталог моделей supports_images Объявление (только ModelInCatalog имеет смысл при )
	ModelInCatalog bool // Есть ли модель в каталоге моделей (условие для вердикта "не поддерживается")
}

// noHealthyHint Ошибка локального диспетчера (в пуле нет доступных healthy номеров/джиттер транспортного уровня, нет исходного текста апстрима для проброса)
// фиксированный для hint。не входит GatewayHint：у него нет ErrKind，Это факт диспетчеризации самого шлюза.
const noHealthyHint = "no healthy account available in pool; check /status or retry later"

// NoHealthyAccountHint Ошибка локального планирования gateway_hint（и no_healthy_account code в комплекте).
func NoHealthyAccountHint() string { return noHealthyHint }

// FrameHintFunc вернуть SSE error кадра gateway_hint Функция проверки (Stream опциональный параметр).
// ctxFn Ленивое вычисление: только при фактическом столкновении с error вызывается только на кадре (нулевые накладные расходы в норм. потоке, запрос каталога моделей
// не срабатывает на каждый успешный запрос).
func FrameHintFunc(ctxFn func() HintContext) func(string) string {
	return func(payload string) string {
		if payload == "" || payload == "[DONE]" {
			return ""
		}
		return GatewayHint(FrameKind(payload), payload, ctxFn())
	}
}

// FrameKind Из SSE error Кадр payload решение ErrKind：6004 потоковая форма лимитирования на уровне модели
// （IsModelRateLimit по кадрам JSON прямое попадание) приоритет; остальное — внутри кадра error.message Ход
// Classify（Уровень запроса 400 метрика). не распознаётся → ErrNone（отсутствует hint）。
func FrameKind(payload string) ErrKind {
	if IsModelRateLimit(payload) {
		return ErrSoftRate
	}
	var f struct {
		Error struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if json.Unmarshal([]byte(payload), &f) != nil || f.Error.Message == "" {
		return ErrNone
	}
	return Classify(http.StatusBadRequest, f.Error.Message)
}

// isModelParamInvalid апстрим 11133 body оценка (code 11133 / extError.code=
// model_param_invalid / msg Семейство текстов). Критерий по подстроке:hint это доп. пояснение, неавторитетная классификация,
// Лучше шире, чем пропустить.
func isModelParamInvalid(body string) bool {
	lower := strings.ToLower(body)
	return codeMarker(lower, "11133") ||
		strings.Contains(lower, "model_param_invalid") ||
		strings.Contains(lower, "invalid request parameters") ||
		strings.Contains(lower, "request parameters do not meet the current model requirements")
}

// isInvalidImageData апстрим 11135 body оценка (code 11135 / invalid_image_data /
// "replace the image" msg семейство).
func isInvalidImageData(body string) bool {
	lower := strings.ToLower(body)
	return codeMarker(lower, "11135") ||
		strings.Contains(lower, "invalid_image_data") ||
		strings.Contains(lower, "replace the image")
}

// codeMarker JSON code попадание по полю (`"code":N` / `«code": N` / `«code":«N"` форма,
// и IsModelBlocked code оценка по тому же допуску).lower должно быть в нижнем регистре body。
func codeMarker(lower, code string) bool {
	for _, v := range []string{`"code":` + code, `"code": ` + code, `"code":"` + code + `"`, `"code": "` + code + `"`, `"code":" ` + code + `"`, `"code": '` + code + `'`} {
		if strings.Contains(lower, v) {
			return true
		}
	}
	return false
}
