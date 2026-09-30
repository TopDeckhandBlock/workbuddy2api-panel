// ring.go Кольцевой буфер структурированных логов фиксированной емкости (потокобезопасный, реализация io.Writer）。
// main взять log Вывод пакета и chat Табличный лог через MultiWriter образ загружен, панель
// /panel/api/logs Чтение снапшота; старые строки сверх ёмкости по FIFO Отбраковано.
//
// при попадании строки в кольцо классификация канала по префиксу (chat=Строка таблицы запросов диалога / task=Действие задачи /
// sys=система и прочие), представление логов панели фильтруется по каналам — при большом трафике диалогов результаты задач не теряются.
package panel

import (
	"regexp"
	"strings"
	"sync"
	"time"
)

// Канал логов.
const (
	ChChat = "chat"
	ChTask = "task"
	ChSys = "sys"
)

// LogEntry Одна запись лога (timestamp — момент записи;log дата/время в начале строки пакета уже stripped).
type LogEntry struct {
	TS time.Time `json:"ts"`
	Ch string `json:"ch"`
	Text string `json:"text"`
}

// taskPrefixes префикс строки лога действий задачи (scheduler и panel Существующий критерий).
var taskPrefixes = []string{
	"school ", "streak-bonus ", "travel ", "blackcat ", "lottery ",
	"checkin ", "activity ", "keepalive ", "balance ", "user-resource ",
	"panel: задача", "panel: В один клик", "panel: checkin", "panel: Вручную",
	"panel: очередь", "panel: Код купона", "panel: Запуск очереди", "panel: Действие задачи",
	"panel: сканирование", "panel: Очередь", "panel: Выполнение",
}

// tsPrefixRe log Пакет по умолчанию flags（Дата времени) метка времени в начале строки.
var tsPrefixRe = regexp.MustCompile(`^\d{4}/\d{2}/\d{2} \d{2}:\d{2}:\d{2} `)

// classifyLine Классификация каналов по признакам начала строки.
func classifyLine(line string) string {
	if strings.HasPrefix(line, "| #") { // chat Лог таблицы (server/logging.go logChatRow）
		return ChChat
	}
	for _, p := range taskPrefixes {
		if strings.HasPrefix(line, p) {
			return ChTask
		}
	}
	return ChSys
}

// Ring кольцевой буфер логов.
type Ring struct {
	mu sync.Mutex
	entries []LogEntry
	cap int
}

// NewRing Создать емкостью capacity кольцевой лог (откат при неположительном значении 500）。
func NewRing(capacity int) *Ring {
	if capacity <= 0 {
		capacity = 500
	}
	return &Ring{cap: capacity}
}

// Write Нажать \n Нарезка в кольцо (реализация io.Writer）。Пустые строки отбрасываются; при превышении емкости вытесняется самая старая строка.
func (r *Ring) Write(p []byte) (int, error) {
	now := time.Now()
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, line := range strings.Split(strings.TrimRight(string(p), "\r\n"), "\n") {
		if line == "" {
			continue
		}
		text := tsPrefixRe.ReplaceAllString(line, "")
		r.entries = append(r.entries, LogEntry{TS: now, Ch: classifyLine(text), Text: text})
		if overflow := len(r.entries) - r.cap; overflow > 0 {
			r.entries = r.entries[overflow:]
		}
	}
	return len(p), nil
}

// Snapshot Возвращает все записи буфера в порядке записи (копия, вызывающая сторона может безопасно хранить).
func (r *Ring) Snapshot() []LogEntry {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]LogEntry, len(r.entries))
	copy(out, r.entries)
	return out
}
