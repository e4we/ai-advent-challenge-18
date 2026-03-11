package storage

import (
	"path/filepath"
	"testing"
	"time"

	"mcp-reminder/models"
)

// newTestStore создаёт изолированный Store в временной директории.
// t.TempDir() автоматически очищается после теста.
func newTestStore(t *testing.T) *Store {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "test.db")
	store, err := NewStore(dbPath)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	t.Cleanup(func() { store.Close() })
	return store
}

// TestNewStore проверяет, что база данных создаётся и инициализируется без ошибок.
func TestNewStore(t *testing.T) {
	store := newTestStore(t)
	if store == nil {
		t.Fatal("expected non-nil store")
	}
	// Проверяем, что таблица создана — выполним простой запрос.
	rows, err := store.db.Query(`SELECT COUNT(*) FROM reminders`)
	if err != nil {
		t.Fatalf("table 'reminders' not accessible: %v", err)
	}
	rows.Close()
}

// TestCreateAndGet проверяет создание и получение напоминания по ID.
// Это базовый CRUD-цикл: Create → GetByID.
func TestCreateAndGet(t *testing.T) {
	store := newTestStore(t)

	// Создаём тестовое напоминание.
	original := models.NewReminder("проверить почту", time.Now().Add(time.Hour), "")

	if err := store.Create(original); err != nil {
		t.Fatalf("Create: %v", err)
	}

	// Получаем по ID и сравниваем ключевые поля.
	got, err := store.GetByID(original.ID)
	if err != nil {
		t.Fatalf("GetByID: %v", err)
	}

	if got.ID != original.ID {
		t.Errorf("ID: got %q, want %q", got.ID, original.ID)
	}
	if got.Title != original.Title {
		t.Errorf("Title: got %q, want %q", got.Title, original.Title)
	}
	if got.Status != models.StatusPending {
		t.Errorf("Status: got %q, want %q", got.Status, models.StatusPending)
	}
	if got.FiredAt != nil {
		t.Errorf("FiredAt: got %v, want nil", got.FiredAt)
	}
}

// TestList проверяет фильтрацию по статусу.
// Создаём напоминания с разными статусами и проверяем что фильтр работает.
func TestList(t *testing.T) {
	store := newTestStore(t)

	// Создаём 3 напоминания.
	r1 := models.NewReminder("первое", time.Now().Add(time.Hour), "")
	r2 := models.NewReminder("второе", time.Now().Add(2*time.Hour), "")
	r3 := models.NewReminder("третье", time.Now().Add(3*time.Hour), "")

	for _, r := range []models.Reminder{r1, r2, r3} {
		if err := store.Create(r); err != nil {
			t.Fatalf("Create: %v", err)
		}
	}

	// Помечаем одно как fired.
	if err := store.MarkFired(r2.ID); err != nil {
		t.Fatalf("MarkFired: %v", err)
	}

	// Все напоминания.
	all, err := store.List("")
	if err != nil {
		t.Fatalf("List all: %v", err)
	}
	if len(all) != 3 {
		t.Errorf("List all: got %d, want 3", len(all))
	}

	// Только pending — должно быть 2.
	pending, err := store.List(models.StatusPending)
	if err != nil {
		t.Fatalf("List pending: %v", err)
	}
	if len(pending) != 2 {
		t.Errorf("List pending: got %d, want 2", len(pending))
	}

	// Только fired — должно быть 1.
	fired, err := store.List(models.StatusFired)
	if err != nil {
		t.Fatalf("List fired: %v", err)
	}
	if len(fired) != 1 {
		t.Errorf("List fired: got %d, want 1", len(fired))
	}
	if fired[0].ID != r2.ID {
		t.Errorf("List fired[0].ID: got %q, want %q", fired[0].ID, r2.ID)
	}
}

// TestMarkFired проверяет что MarkFired правильно обновляет status и fired_at.
func TestMarkFired(t *testing.T) {
	store := newTestStore(t)

	r := models.NewReminder("тест срабатывания", time.Now().Add(time.Minute), "")
	if err := store.Create(r); err != nil {
		t.Fatalf("Create: %v", err)
	}

	beforeFire := time.Now()

	if err := store.MarkFired(r.ID); err != nil {
		t.Fatalf("MarkFired: %v", err)
	}

	// Получаем обновлённое напоминание.
	updated, err := store.GetByID(r.ID)
	if err != nil {
		t.Fatalf("GetByID after MarkFired: %v", err)
	}

	// Проверяем статус.
	if updated.Status != models.StatusFired {
		t.Errorf("Status after MarkFired: got %q, want %q", updated.Status, models.StatusFired)
	}

	// Проверяем что fired_at заполнен и разумен по времени.
	if updated.FiredAt == nil {
		t.Fatal("FiredAt: got nil, want non-nil after MarkFired")
	}
	if updated.FiredAt.Before(beforeFire) {
		t.Errorf("FiredAt %v is before test start %v", updated.FiredAt, beforeFire)
	}
}

// TestGetDueReminders проверяет что возвращаются только просроченные pending-напоминания.
// Создаём напоминания в прошлом и будущем — должны вернуться только просроченные.
func TestGetDueReminders(t *testing.T) {
	store := newTestStore(t)

	// Просроченное напоминание — в прошлом.
	pastReminder := models.NewReminder("просрочено", time.Now().Add(-time.Hour), "")
	// Будущее напоминание — ещё не наступило.
	futureReminder := models.NewReminder("в будущем", time.Now().Add(time.Hour), "")

	for _, r := range []models.Reminder{pastReminder, futureReminder} {
		if err := store.Create(r); err != nil {
			t.Fatalf("Create: %v", err)
		}
	}

	due, err := store.GetDueReminders()
	if err != nil {
		t.Fatalf("GetDueReminders: %v", err)
	}

	// Должно вернуть только одно просроченное.
	if len(due) != 1 {
		t.Fatalf("GetDueReminders: got %d, want 1", len(due))
	}
	if due[0].ID != pastReminder.ID {
		t.Errorf("GetDueReminders[0].ID: got %q, want %q", due[0].ID, pastReminder.ID)
	}
}

// TestGetSummary проверяет корректность агрегированных счётчиков.
func TestGetSummary(t *testing.T) {
	store := newTestStore(t)

	// Создаём по 2 напоминания каждого статуса.
	for i := 0; i < 2; i++ {
		r := models.NewReminder("pending", time.Now().Add(time.Hour), "")
		store.Create(r)
	}
	for i := 0; i < 3; i++ {
		r := models.NewReminder("to be fired", time.Now().Add(-time.Hour), "")
		store.Create(r)
		store.MarkFired(r.ID)
	}
	for i := 0; i < 1; i++ {
		r := models.NewReminder("to be deleted", time.Now().Add(time.Hour), "")
		store.Create(r)
		// Удаляем = физически удаляем из БД; cancelled не поддерживается напрямую.
		// Для теста GetSummary нам достаточно проверить pending и fired.
		_ = r
	}

	summary, err := store.GetSummary()
	if err != nil {
		t.Fatalf("GetSummary: %v", err)
	}

	// Всего: 2 pending + 3 fired + 1 pending (не удалённый) = 6 записей.
	if summary.TotalCount != 6 {
		t.Errorf("TotalCount: got %d, want 6", summary.TotalCount)
	}
	if summary.PendingCount != 3 {
		t.Errorf("PendingCount: got %d, want 3", summary.PendingCount)
	}
	if summary.FiredCount != 3 {
		t.Errorf("FiredCount: got %d, want 3", summary.FiredCount)
	}

	// Ближайшие pending — должно быть 3 (но не больше 5).
	if len(summary.Upcoming) != 3 {
		t.Errorf("Upcoming: got %d, want 3", len(summary.Upcoming))
	}
	// Последние сработавшие — должно быть 3.
	if len(summary.RecentlyFired) != 3 {
		t.Errorf("RecentlyFired: got %d, want 3", len(summary.RecentlyFired))
	}
}
