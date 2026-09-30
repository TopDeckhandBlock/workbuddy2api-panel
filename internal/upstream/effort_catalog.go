// effort_catalog.go Тариф инференса (reasoning effort）Статическая fallback-таблица уровня продукта.
//
// Три уровня источников данных (включая референсный репозиторий reconcileWithFallback/buddy-adapter.ts:499-523 семантика):
// - Удаленный FetchModels / global зондирование уже распарсенного supportedEfforts/defaultEffort бакет (авторитетный, приоритетный);
// - Данный файл по realm Отдельная продуктовая статическая фолбэк-таблица (дополнение при отсутствии удалённых данных);
// - оба отсутствуют → не в /v1/models Вывод effort Поле (omitted，не пустой массив).
//
// значения профилей копируются из референс-репозитория dsh-codearts：CN/CodeBuddy взять из src/product.ts CODEBUDDY_FALLBACK_MODELS
// ∪ src/buddy-adapter.ts:126-137 REASONING_EFFORTS；global/WorkBuddy взять из src/product.ts
// WORKBUDDY_FALLBACK_MODELS。Два realm для одной модели разные уровни (напр. deepseek-v4.1-flash：
// CN Три уровня ['low','high','max']、global Только ['high']），Поэтому**Нажать realm Шардирование**，Никогда не смешивать.
//
// только список**перечисляемый**модель тарифа; есть только фиксированный дефолтный тариф (glm-5.1/kimi-* medium）Аналогично заносится в таблицу,
// Но тариф — "перечислимый одиночный уровень», а не "без селектора», скопировано из эталонного репозитория как есть.
package upstream

// effortCap возможности тарифа одной модели (выравнивание по записям fallback-таблицы продукта reasoningEfforts + defaultReasoningEffort）。
type effortCap struct {
	efforts []string
	defaultEffort string
}

// cnEffortFallback CN / CodeBuddy статическая fallback-таблица.
// три уровня моделей defaultEffort все — high（product.ts CODEBUDDY_FALLBACK_MODELS Построчно defaultReasoningEffort）。
var cnEffortFallback = map[string]effortCap{
	"deepseek-v4-flash": {efforts: []string{"low", "high", "max"}},
	"deepseek-v4.1-flash": {efforts: []string{"low", "high", "max"}, defaultEffort: "high"},
	"deepseek-v4-pro": {efforts: []string{"low", "high", "xhigh"}, defaultEffort: "high"},
	"hy4-preview": {efforts: []string{"high"}, defaultEffort: "high"},
	"hy4-preview-x": {efforts: []string{"high"}},
	"hy3": {efforts: []string{"low", "high"}, defaultEffort: "high"},
	"hy3-x": {efforts: []string{"low", "high"}, defaultEffort: "high"},
	"glm-5.3": {efforts: []string{"low", "high", "max"}, defaultEffort: "high"},
	"glm-5.3-flash": {efforts: []string{"low", "high", "max"}, defaultEffort: "high"},
	"glm-5.2": {efforts: []string{"high", "xhigh"}, defaultEffort: "high"},
	"glm-5.1": {efforts: []string{"medium"}},
	"glm-5v-turbo": {efforts: []string{"medium"}},
	"kimi-k3-1": {efforts: []string{"medium"}},
	"kimi-k2.7": {efforts: []string{"medium"}},
	"kimi-k2.6": {efforts: []string{"medium"}},
	"minimax-m3": {efforts: []string{"medium"}},
}

// globalEffortFallback global / WorkBuddy Статическая резервная таблица международной версии.
// Внимание deepseek-v4.1-flash В международной версии**только ['high']**（product.ts:190 На практике IDE кэш),
// и CN три уровня намеренно различаются — в сторону WorkBuddy апстрим отправляет low/max — недопустимый параметр 400。
var globalEffortFallback = map[string]effortCap{
	"fast-model": {efforts: []string{"medium"}},
	"balanced-model": {efforts: []string{"medium"}},
	"primary-model": {efforts: []string{"high"}},
	"hy4-preview-f": {efforts: []string{"high"}, defaultEffort: "high"},
	"hy3": {efforts: []string{"low", "high"}, defaultEffort: "high"},
	"deepseek-v4.1-flash": {efforts: []string{"high"}},
	"gpt-6-astra": {efforts: []string{"low", "medium", "high", "xhigh", "max"}, defaultEffort: "high"},
	"gpt-5.6-sol": {efforts: []string{"low", "medium", "high", "xhigh", "max"}, defaultEffort: "high"},
	"gpt-5.6-terra": {efforts: []string{"low", "medium", "high", "xhigh", "max"}, defaultEffort: "high"},
	"gpt-5.6-luna": {efforts: []string{"low", "medium", "high", "xhigh", "max"}, defaultEffort: "high"},
	"gpt-5.5": {efforts: []string{"low", "medium", "high", "xhigh"}, defaultEffort: "high"},
	"gpt-5.4": {efforts: []string{"low", "medium", "high", "xhigh"}, defaultEffort: "high"},
	"gpt-5.3-codex": {efforts: []string{"medium"}},
	"gemini-3.5-flash": {efforts: []string{"medium"}},
	"glm-5.3": {efforts: []string{"low", "high", "max"}, defaultEffort: "high"},
	"glm-5.2": {efforts: []string{"high", "xhigh"}, defaultEffort: "high"},
	"kimi-k3": {efforts: []string{"medium"}},
	"kimi-k2.6": {efforts: []string{"medium"}},
}

// staticEffortCap Нажать realm Взять статическую fallback-запись; при промахе вернуть zero effortCap（efforts=nil）。
// realm Через realmKey Нормализация (пусто → "cn"），и efforts кэш-бакет того же калибра.
func staticEffortCap(realm, model string) effortCap {
	table := cnEffortFallback
	if realmKey(realm) == "global" {
		table = globalEffortFallback
	}
	return table[model]
}

// EffortListing Вычисление модели в /v1/models Подлежащий раскрытию effort возможность (трёхуровневый поиск + защита профилем по умолчанию).
//
// remoteEfforts/remoteDefault является удаленным (FetchModels / global зондирование) распарсенное значение;
// remoteEfforts если не пусто — считать авторитетным (без отката к статической таблице), иначе — откат к продуктовой статической fallback-таблице;
// оба отсутствуют → efforts вернуть nil（вызывающая сторона опустила поле, пустой массив не выводить).
//
// defaultEffort Только в "efforts непусто и default Попадание efforts」возвращается только когда
// （Синхронизировать с референс-репозиторием resolveModel `defaultEffort ∈ efforts` Защита: не заявлять неподдерживаемый дефолт).
// remoteDefault Пустая строка не фолбэчится на статический дефолт — дефолт из той же таблицы тарифов:remote Если есть тариф — использовать remote Профиль по умолчанию,
// для статического fallback-тарифа использовать статический дефолт, чтобы избежать кросс-источниковой склейки "тариф статический, а дефолт — remote」противоречивая комбинация.
func EffortListing(realm, model string, remoteEfforts []string, remoteDefault string) (efforts []string, defaultEffort string) {
	var src effortCap
	switch {
	case len(remoteEfforts) > 0:
		src = effortCap{efforts: remoteEfforts, defaultEffort: remoteDefault}
	default:
		src = staticEffortCap(realm, model)
	}
	if len(src.efforts) == 0 {
		return nil, ""
	}
	efforts = append([]string(nil), src.efforts...)
	if src.defaultEffort != "" && containsEffort(efforts, src.defaultEffort) {
		defaultEffort = src.defaultEffort
	}
	return efforts, defaultEffort
}

// containsEffort Определение принадлежности к тарифу (точное совпадение, сверка с эталонным репозиторием `efforts.includes(defaultEffort)`）。
func containsEffort(efforts []string, want string) bool {
	for _, e := range efforts {
		if e == want {
			return true
		}
	}
	return false
}

// globalEffortMap global Для деградации домена effort таблица capabilities: статическая fallback-таблица как база, удалённый bucket перекрывает (приоритет авторитета).
//
// prepareBody Для global Запрос вызывает эту функцию (а не напрямую удаленный бакет), потому что global Апстрим может не выдавать
// supportedEfforts——в этот момент также обязательно даунгрейд по статической таблице продукта (issue #84：deepseek-v4.1-flash Международная версия
// Принимается только high，передано клиентом low/max Обязательно деградирует до high，иначе апстрим 400 уничтожит запрос).
// Семантическое выравнивание — см. репозиторий effortsFor（remoteMeta → productFallback → статическая таблица), берутся только первые два уровня:
// удалённый бакет (проба распарсена)→ Статическая таблица данного продукта (этот файл), при отсутствии тарифа — отсутствует (без fallback к общей статической таблице).
func globalEffortMap(remoteEfforts map[string][]string, remoteDefaults map[string]string) (map[string][]string, map[string]string) {
	efforts := make(map[string][]string, len(globalEffortFallback)+len(remoteEfforts))
	defs := make(map[string]string, len(globalEffortFallback)+len(remoteDefaults))
	// Статический fallback как база.
	for id, cap := range globalEffortFallback {
		efforts[id] = append([]string(nil), cap.efforts...)
		if cap.defaultEffort != "" {
			defs[id] = cap.defaultEffort
		}
	}
	// Авторитетное переопределение с удалённой стороны (только если удалённая сторона действительно выдала тариф модели).
	for id, v := range remoteEfforts {
		if len(v) > 0 {
			efforts[id] = v
		}
	}
	for id, v := range remoteDefaults {
		if v != "" {
			defs[id] = v
		}
	}
	return efforts, defs
}
