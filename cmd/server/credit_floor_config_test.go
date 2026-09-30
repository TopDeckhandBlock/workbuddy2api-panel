// credit_floor_config_test.go pool.credit_floor Тест конфигурации:
// По умолчанию 0（закрыто, без регрессии)/ перезапись файла / кламп отрицательных значений 0 / большое значение допустимо.
package main

import (
	"os"
	"path/filepath"
	"testing"
)

// TestCreditFloorDefault Ключ отсутствует → По умолчанию 0（fallback отключен, поведение как до внедрения).
func TestCreditFloorDefault(t *testing.T) {
	c := Default()
	if err := c.normalize(); err != nil {
		t.Fatalf("normalize: %v", err)
	}
	if c.Pool.CreditFloor != 0 {
		t.Errorf("credit_floor=%d want 0 (default off)", c.Pool.CreditFloor)
	}
}

// TestCreditFloorParsedFromFile явная конфигурация переопределяет дефолт.
func TestCreditFloorParsedFromFile(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "c.json")
	os.WriteFile(fp, []byte(`{"pool":{"credit_floor":100}}`), 0o600)
	c, err := Load(fp)
	if err != nil {
		t.Fatal(err)
	}
	if c.Pool.CreditFloor != 100 {
		t.Errorf("credit_floor=%d want 100", c.Pool.CreditFloor)
	}
}

// TestCreditFloorNegativeClamped кламп отрицательных значений 0（невалидное — выкл., без ошибки: опечатка старой конфигурации не ломает запуск).
func TestCreditFloorNegativeClamped(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "c.json")
	os.WriteFile(fp, []byte(`{"pool":{"credit_floor":-5}}`), 0o600)
	c, err := Load(fp)
	if err != nil {
		t.Fatal(err)
	}
	if c.Pool.CreditFloor != 0 {
		t.Errorf("credit_floor=%d want 0 (negative clamped)", c.Pool.CreditFloor)
	}
}
