// transport.go исходящий Transport единый источник истины конструкции (усиление на уровне соединения, поглощение kongjianguan
// 4 топ-3 из опыта комбо-тестов, см. .claude/reports/fork-scan-absorb.md T-2）：
// Реально заблокировано h2 / TLS Таймаут хендшейка / Короткий keepalive Детект, все параметры централизованы, тестируемы и настраиваемы.
// четвёртый элемент DisableKeepAlives по отчёту trade-off не поглощать (на каждый запрос TLS Накладные расходы на handshake и соединение
// намерение переиспользования противоположно), анализ см. .claude/reports/transport-hardening.md。
package upstream

import (
	"crypto/tls"
	"net"
	"net/http"
	"time"
)

// Параметры уровня соединения определены централизованно (с server/backoff.go в том же стиле: определение в одном месте, тест может прочитать и проверить).
const (
	// dialTimeout TCP Лимит установки соединений. Первый барьер для полумёртвых соединений: не подключилось — быстрый фейл
	// ротация меняет аккаунт, без ожидания системы TCP Окно ретрансляции (kongjianguan фактически half-open TCP Однократно
	// TTFB Карта 936s——По умолчанию Dialer без верхнего лимита таймаута).
	dialTimeout = 10 * time.Second
	// dialKeepAlive TCP keepalive период зондирования. По умолчанию Dialer 2h только отправлен первый пробник —
	// NAT В черной дыре 2h Достаточно полумертвых соединений, переиспользуются.15s период позволяет мёртвым соединениям в 15~30s внутри был
	// ядро прибивает (RST/ETIMEDOUT），сторона переиспользования сразу обнаруживает, а не ждет окна ретрансляции.
	dialKeepAlive = 15 * time.Second
	// tlsHandshakeTimeout TLS лимит хендшейка. Ранее полностью отсутствовал — при зависании хендшейка нет никакого
	// уровень fallback'а (ResponseHeaderTimeout таймер только после завершения записи запроса), остается только ждать до
	// HTTP.Client.Timeout(120s)。
	tlsHandshakeTimeout = 10 * time.Second
	// idleConnTimeout время удержания idle-пула соединений. С 90s получено 30s：WAF Апстрим после шторма
	// NGI регулярное закрытие простаивающих соединений,90s Соединения в пуле по большей части мертвы (kongjianguan то же значение);
	// на стороне переиспользования всё ещё есть 15s keepalive резервное распознавание.
	idleConnTimeout = 30 * time.Second
	// responseHeaderTimeout Чат SSE жёсткий лимит до первого байта (заголовки ответа). С 120s получено
	// 60s：kongjianguan Заметка: зафиксирован успешный запрос TTFB ранее достигало 16s（холодный старт медленной модели),
	// 60s ≈ 3.75× Наблюдать худший healthy TTFB, оставить запас на медленный холодный старт; и сценарии полумёртвых соединений
	// зависание одиночного запроса с 2 минут сжать до 1 минут (MaxRotate По умолчанию 3 худшая ротация за раз с 6 минут
	// Сжать до 3 минут). Не брать kongjianguan 20s：его 20s Да DisableKeepAlives+
	// timedConn 20s значения комбинации таймаутов записи, сохраняем reuse соединения, нужен собственный медленный cold start
	// оставить запас для наблюдения. Семантическая сверка (дисциплина ТЗ):ResponseHeaderTimeout Учитывать только ответ
	// длительность до прибытия заголовка, после прибытия заголовка SSE Длинный поток не затронут (простой в потоке от IdleTimeout мониторинг,
	// См. idle.go），Не убивать длинные потоки —transport_test.go Есть явный возврат.
	//
	// внимание: данная константа — лишь newTransport конструктор по умолчанию,main.go Будет по config
	// upstream.header_timeout_seconds Безусловная перезапись. Поэтому продовое эффективное значение = config
	// Распарсенное значение (когда не настроено normalize откат timeout_seconds，По умолчанию 120），текущий 60s Только как
	// 「Config Не подключено/страховочная сетка при "голом» тестовом использовании — и config.example.json значение
	// выравнивание во избежание расхождения метрики в трех местах (transport 60 / config откат 120 / example 60）。
	responseHeaderTimeout = 60 * time.Second
)

// maxIdleConns / maxIdleConnsPerHost Емкость пула соединений (существующее значение, определяется централизованно).
const (
	maxIdleConns = 100
	maxIdleConnsPerHost = 20
)

// newDialer создать исходящий dialer (DialContext Timeout/KeepAlive Параметры сосредоточены здесь,
// для assert-проверки при обратном чтении в тестах).
func newDialer() *net.Dialer {
	return &net.Dialer{
		Timeout: dialTimeout,
		KeepAlive: dialKeepAlive,
	}
}

// newTransport Сконструировать общий исходящий Transport（HTTP и ChatHTTP один экземпляр, пул соединений не дублируется).
// Двухуровневая защита от half-dead соединений:
// - TLS уровень: пусто TLSNextProto Реально заблокировано h2（kongjianguan эмпирика вторичной коррекции:ForceAttemptHTTP2=false
// Только для кастомных Dial вступает в силу, по умолчанию TLS Через ALPN всё равно согласуется h2，полумертвый h2 повторное использование потока проявляется как
// "http2: timeout awaiting response headers"——Единственный корректный способ — очистить маппинг, чтобы ALPN
// После завершения нет h2 протокол доступен, соединение возвращается HTTP/1.1）。
// - TCP Уровень:DialContext 10s лимит соединений + 15s keepalive проба, half-open соединение на этапе установления
// и период реюза могут быть быстро распознаны (см. dialTimeout/dialKeepAlive комментарий).
func newTransport() *http.Transport {
	dialer := newDialer()
	return &http.Transport{
		DialContext: dialer.DialContext,
		// пустой TLSNextProto（не nil）Реально заблокировано h2：См. комментарий к функции. Обязательно make а не nil——
		// nil означает "разрешить стандартной библиотеке инжектить дефолт h2 маппинг»kongjianguan факт.: уст.
		// ForceAttemptHTTP2=false после лог всё равно сообщает h2 timeout，именно эта ловушка).
		TLSNextProto: make(map[string]func(authority string, c *tls.Conn) http.RoundTripper),
		TLSHandshakeTimeout: tlsHandshakeTimeout,
		MaxIdleConns: maxIdleConns,
		MaxIdleConnsPerHost: maxIdleConnsPerHost,
		IdleConnTimeout: idleConnTimeout,
		ResponseHeaderTimeout: responseHeaderTimeout,
	}
}

// closeIdler Реализующие данный интерфейс RoundTripper Поддержка очистки пула idle-соединений (*http.Transport、
// http2.Transport все выполнены; кастомная тестовая инъекция RoundTripper Может быть реализовано опционально).
type closeIdler interface {
	CloseIdleConnections()
}

// roundTripCloseIdle Очистить после сбоя запроса на транспортном уровне rt Принадлежность Transport пул idle-соединений
// （kongjianguan № 4 : неуспешное соединение может остаться в idle-пуле, ждать IdleConnTimeout только
// истек, следующий запрос снова его подхватит).
//
// Точка монтирования (вывод ТЗ "Точка оценки: обработка классификации ошибок»): классификация ошибок (Classify）
// Виден только бизнес-конверт — при сбое транспортного уровня вообще нет body Классифицируемо (см. doJSON/ChatStreamContext
// Для read body Обработка ошибки: не попадает в Classify、аккаунт не штрафуется). Корректная обработка такого сбоя — это как раз
// очистка пула уровня соединения, поэтому висит на Do параллельные egress'ы транспортного уровня (ChatStreamContext Do Ошибка
// ветка), а не applyErrorPolicy。
//
// Отключение — это best-effort：rt для nil или не реализовано closeIdler（как инжектированное в тесте rtFunc）Время
// Тихо пропустить.CloseIdleConnections Закрывать только idle-соединения, не затрагивая in-flight запросы; мгновенная цена —
// Следующий запрос — на один раз больше TCP+TLS Хендшейк, полумертвое соединение зависает при переиспользовании 60s риск полностью несоразмерен.
func roundTripCloseIdle(rt http.RoundTripper) {
	if rt == nil {
		return
	}
	if ci, ok := rt.(closeIdler); ok {
		ci.CloseIdleConnections()
	}
}
