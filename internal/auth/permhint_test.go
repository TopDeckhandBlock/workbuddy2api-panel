package auth

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// TestSaveAtomicPermissionHint Сохранение в каталоге без прав на запись, ошибка должна содержать Docker chown Руководство.
func TestSaveAtomicPermissionHint(t *testing.T) {
	if runtime.GOOS == "windows" || os.Geteuid() == 0 {
		t.Skip("Windows отсутствует POSIX Семантика прав / root Без ограничения прав, пропустить")
	}
	dir := t.TempDir()
	ro := filepath.Join(dir, "ro")
	os.MkdirAll(ro, 0o555) // Каталог только для чтения
	defer os.Chmod(ro, 0o755)
	a := &Auth{AccessToken: "at", RefreshToken: "rt", ExpiresAt: 1,
		UID: "u1", FilePath: filepath.Join(ro, "workbuddy-u1.json")}
	err := a.SaveAtomic()
	if err == nil {
		t.Fatal("сохранение в каталог только для чтения должно завершиться ошибкой")
	}
	if !strings.Contains(err.Error(), "chown") || !strings.Contains(err.Error(), "10001") {
		t.Errorf("ошибка прав должна содержать Docker руководство, фактически: %v", err)
	}
}
