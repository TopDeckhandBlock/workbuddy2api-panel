// config.go API страницы конфигурации панели: чтение текущей конфигурации, валидация и сохранение (горячее применение + отметка пункта перезапуска).
//
// распределение:cmd/server Удерживает Config Тип и логика валидации (Load/normalize），здесь только
// HTTP Оркестрация —GET Отображение,POST прокинуть в инжектированный SaveConfig Замыкание (от main Завершено
// "Валидация → сброс на диск → Горячее применение → Вернуть список полей, требующих перезапуска"）。
package panel

import (
	"io"
	"log"
	"net/http"
)

// getConfig возвращает содержимое и путь текущего конфиг-файла (фронтенд по schema отрисовка формы).
func (p *Panel) getConfig(w http.ResponseWriter, r *http.Request) {
	if p.cfg.LoadConfig == nil {
		writeErr(w, http.StatusNotImplemented, "config api not available")
		return
	}
	cfg, err := p.cfg.LoadConfig()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "load config: "+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok": true,
		"path": p.cfg.ConfigPath,
		"config": cfg,
	})
}

// saveConfig Сохранить конфигурацию:body является напрямую конфигурацией JSON（фронтенд по schema собрать полный объект).
// SaveConfig валидация внутри замыкания+сброс на диск+горячее применение; при ошибке валидации возврат 400 и без записи на диск.
func (p *Panel) saveConfig(w http.ResponseWriter, r *http.Request) {
	if p.cfg.SaveConfig == nil {
		writeErr(w, http.StatusNotImplemented, "config api not available")
		return
	}
	raw, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		writeErr(w, http.StatusBadRequest, "read body: "+err.Error())
		return
	}
	restartRequired, err := p.cfg.SaveConfig(raw)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if restartRequired == nil {
		restartRequired = []string{}
	}
	log.Printf("panel: конфигурация сохранена (горячее применение завершено; поля, требующие перезапуска %d шт.)", len(restartRequired))
	writeJSON(w, http.StatusOK, map[string]any{
		"ok": true,
		"restart_required": restartRequired,
	})
}
