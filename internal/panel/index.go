// index.go статические ресурсы панели и security-заголовки.
//
// Ресурс через go:embed Вшивается в бинарь (деплоится вместе с сервисом, без внешних шагов сборки):
// - index.html Каркас страницы
// - app.js вся логика фронтенда (отдельный файл, не inline, чтобы включить без unsafe-inline строгий CSP）
//
// пара security-заголовков"Страница панели и все /panel/api/* Ответ"единое применение:CSP скрипты разрешены только с этого сервиса,
// Запрещено iframe вложенность (защита от clickjacking), запрет MIME сниффинг, с декларацией отсутствия утечки Referer исходящий.
package panel

import (
	_ "embed"
	"net/http"
)

//go:embed index.html
var indexHTML []byte

//go:embed app.js
var appJS []byte

// csp политика безопасности контента (строгая версия, без unsafe-inline）：
// - default-src 'none' по умолчанию всё запрещено, открытие поштучно
// - script-src 'self' запускать только same-origin скрипты (app.js）；на странице нет inline-обработчиков событий/Инлайн-скрипт
// - style-src 'self' 'unsafe-inline'
// style инлайн — проектный компромисс: на странице немного style="..." свойства (ширина прогресс-бара, ширина столбцов таблицы),
// разрешение inline-стилей не ведёт к выполнению скриптов; внешние домены стилей всё равно запрещены и @import Внешняя ссылка.
// - connect-src 'self' Фронтенд fetch Только для этого сервиса
// - img-src 'self' data: Иконка/Инлайн-изображение
// - form-action 'none' у страницы нет цели отправки формы (страница конфигурации — JS коммит)
// - frame-ancestors 'none' Запрещено любым сайтом iframe Вложенность (кликджекинг)
// - base-uri 'none' инжект запрещен <base> Перезапись относительного пути
const csp = "default-src 'none'; script-src 'self'; style-src 'self' 'unsafe-inline'; " +
	"connect-src 'self'; img-src 'self' data:; form-action 'none'; " +
	"frame-ancestors 'none'; base-uri 'none'"

// setSecurityHeaders записать единые security-заголовки панели (страница и API нужны оба,API также содержит JSON данные).
func setSecurityHeaders(w http.ResponseWriter) {
	w.Header().Set("Content-Security-Policy", csp)
	w.Header().Set("X-Content-Type-Options", "nosniff") // Запрет MIME Сниффинг
	w.Header().Set("X-Frame-Options", "DENY") // Фолбэк для старых браузеров (CSP frame-ancestors эквивалент)
	w.Header().Set("Referrer-Policy", "no-referrer") // Не раскрывать адрес панели внешним сайтам
	w.Header().Set("Cross-Origin-Opener-Policy", "same-origin")
	w.Header().Set("Cross-Origin-Resource-Policy", "same-origin")
}

// index Страница панели вывода (статика без секретов; интерфейс данных /panel/api/* только тогда — аутентификация).
func (p *Panel) index(w http.ResponseWriter, r *http.Request) {
	setSecurityHeaders(w)
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(indexHTML)
}

// appScript Логика вывода фронтенда (same-origin скрипт, для CSP script-src 'self' загрузка).
func (p *Panel) appScript(w http.ResponseWriter, r *http.Request) {
	setSecurityHeaders(w)
	w.Header().Set("Content-Type", "text/javascript; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(appJS)
}
