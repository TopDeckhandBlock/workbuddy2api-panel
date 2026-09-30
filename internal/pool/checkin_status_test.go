// checkin_status_test.go Фиксация наблюдения "Сегодня уже отмечено»:NoteCheckinDone пометить текущий день,
// statusOf Вывод CheckinDone（истекает естественно при переходе через полночь), при персистировании не теряется.
package pool

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
)

func TestNoteCheckinDoneMarksToday(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.Add(&auth.Auth{UID: "u2"})
	if st, _ := p.Status("u1"); st.CheckinDone {
		t.Fatal("нечекинутый аккаунт не должен отображаться как чекинутый")
	}
	p.NoteCheckinDone("u1")
	st, ok := p.Status("u1")
	if !ok || !st.CheckinDone {
		t.Fatalf("u1 после отметки должно отображаться «отмечено»: %+v ok=%v", st, ok)
	}
	if st, _ := p.Status("u2"); st.CheckinDone {
		t.Fatal("u2 не помечено, не должно отображаться как отмечено")
	}
	// неизвестно uid Не panic、Не влияет на другие аккаунты.
	p.NoteCheckinDone("no-such-uid")
}

func TestCheckinDonePersists(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "state.json")
	p := New(fp)
	p.Add(&auth.Auth{UID: "u1"})
	p.NoteCheckinDone("u1")
	p.Flush()
	raw, err := os.ReadFile(fp)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `"last_checkin_day"`) {
		t.Fatalf("state.json отсутствует last_checkin_day:\n%s", raw)
	}
	p2 := New(fp)
	p2.Add(&auth.Auth{UID: "u1"})
	if st, _ := p2.Status("u1"); !st.CheckinDone {
		t.Fatal("После перезапуска теряется статус подписания за текущий день")
	}
}

func TestCheckinDoneExpiredYesterday(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "state.json")
	p := New(fp)
	p.Add(&auth.Auth{UID: "u1"})
	p.NoteCheckinDone("u1")
	p.Flush()
	raw, err := os.ReadFile(fp)
	if err != nil {
		t.Fatal(err)
	}
	yesterday := time.Now().AddDate(0, 0, -1).Format("2006-01-02")
	raw = []byte(strings.Replace(string(raw), time.Now().Format("2006-01-02"), yesterday, 1))
	if err := os.WriteFile(fp, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	p2 := New(fp)
	p2.Add(&auth.Auth{UID: "u1"})
	if st, _ := p2.Status("u1"); st.CheckinDone {
		t.Fatal("Вчерашний чекин не должен отображаться как сегодняшний")
	}
}
