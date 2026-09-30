// device_token.go X-Device-Token резервное чтение файла (общий файл состояния с десктопом).
//
// в контейнере нет десктоп-клиента Turing Shield SDK，Невозможно как Python fork（xiaofan6ya/converter.py）так
// Брать текущее token。Здесь предоставляется альтернативный путь: хост передаёт сгенерированное на десктопе device token Сброс /app/data/device_token
// （или любой mount-путь), шлюз периодически читает инжект. Частота чтения ограничена 5 Кэш раз в минуту,>1KB или игнорировать при ошибке чтения
// （грейсфул-деградация без инъекции, не влияет на основной поток).
package upstream

import (
	"errors"
	"os"
	"strings"
	"sync"
	"time"
)

var errDeviceTokenTooLarge = errors.New("device token file too large")

// deviceTokenFile Кэш чтения файлов TTL（сек). Десктоп SDK Есть собственный кэш, здесь еще один слой
// чтобы каждый исходящий запрос не stat+read файл.
const deviceTokenFileTTL = 5 * time.Minute

// deviceTokenFileMaxLen token Макс. размер файла в байтах.token Обычно несколько сотен байт; свыше 1KB
// считается исключением (не token Содержимое / файл использован ошибочно), игнорировать, не инжектировать.
const deviceTokenFileMaxLen = 1024

// deviceTokenFileCache Кеш device token результат чтения файла (path → token+момента чтения).
type deviceTokenFileCache struct {
	mu sync.Mutex
	path string
	token string
	readAt time.Time
	lastErr error
}

var dtFileCache = &deviceTokenFileCache{}

// readDeviceTokenFile Чтение и кэширование device token Файл;5 повторное использование предыдущего результата в течение N минут.
// Возврат пустой строки означает отсутствие доступных token（файл не настроен / Ошибка чтения / Содержимое слишком длинное / пусто).
func readDeviceTokenFile(path string) string {
	if path == "" {
		return ""
	}
	// Быстрый путь: попадание в кэш и не истёк — сразу вернуть значение из кэша. Внимание: удерживать блокировку всё время (defer Unlock）——
	// Содержит 5 перечитывание по истечении раз в N минут (чтение файла под блокировкой). Частота вызовов крайне низкая (каждые 5min Максимум один раз
	// Файл IO，Лимит файла 1KB），Внутри блокировки IO Приемлемо; если в будущем появится NFS Монтирование + высококонкурентный
	// форма развертывания, затем включить singleflight обернуть секцию повторного чтения (YAGNI，сейчас не делать).
	dtFileCache.mu.Lock()
	defer dtFileCache.mu.Unlock()
	if path == dtFileCache.path && time.Since(dtFileCache.readAt) < deviceTokenFileTTL {
		return dtFileCache.token
	}
	// Промах или истечение кэша: перечитать файл.
	dtFileCache.path = path
	tok, err := readTrimmedFile(path, deviceTokenFileMaxLen)
	if err != nil {
		// Ошибка чтения: очистить кэш token，избежать инъекции просроченного/неверного значения.
		dtFileCache.token = ""
		dtFileCache.lastErr = err
		dtFileCache.readAt = time.Now()
		return ""
	}
	dtFileCache.token = tok
	dtFileCache.lastErr = nil
	dtFileCache.readAt = time.Now()
	return tok
}

// readTrimmedFile чтение файла и trim пробелы в начале/конце, превышает maxLen Вернуть ошибку (отклонить слишком длинный контент).
func readTrimmedFile(path string, maxLen int) (string, error) {
	info, err := os.Stat(path)
	if err != nil {
		return "", err
	}
	if info.Size() > int64(maxLen) {
		return "", errDeviceTokenTooLarge
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(raw)), nil
}
