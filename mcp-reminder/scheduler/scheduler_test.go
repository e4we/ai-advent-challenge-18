package scheduler

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"mcp-reminder/models"
	"mcp-reminder/storage"
)

// newTestStore создаёт изолированное хранилище для тестов планировщика.
func newTestStore(t *testing.T) *storage.Store {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "scheduler_test.db")
	store, err := storage.NewStore(dbPath)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	t.Cleanup(func() { store.Close() })
	return store
}

// TestSchedulerFiresReminder проверяет, что планировщик замечает просроченное
// напоминание и помечает его как fired.
//
// Сценарий:
// 1. Создаём напоминание с due_at в прошлом
// 2. Запускаем планировщик с коротким интервалом (100ms)
// 3. Ждём 2 тика (250ms)
// 4. Проверяем что статус стал "fired"
func TestSchedulerFiresReminder(t *testing.T) {
	store := newTestStore(t)

	// Напоминание просрочено на 1 час — должно сработать при первом же тике.
	r := models.NewReminder("проверить тест", time.Now().Add(-time.Hour), "")
	if err := store.Create(r); err != nil {
		t.Fatalf("Create: %v", err)
	}

	// Используем короткий интервал для тестов (100ms вместо 30s).
	sched := NewSchedulerWithInterval(store, 100*time.Millisecond)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	go sched.Start(ctx)
	defer sched.Stop()

	// Ждём 2-3 тика чтобы планировщик успел обработать напоминание.
	time.Sleep(350 * time.Millisecond)

	// Получаем обновлённое напоминание.
	updated, err := store.GetByID(r.ID)
	if err != nil {
		t.Fatalf("GetByID: %v", err)
	}

	if updated.Status != models.StatusFired {
		t.Errorf("status after scheduler tick: got %q, want %q", updated.Status, models.StatusFired)
	}
	if updated.FiredAt == nil {
		t.Error("FiredAt: got nil, want non-nil after scheduler fired")
	}
}

// TestSchedulerReschedule проверяет, что после срабатывания периодического
// напоминания создаётся новое с тем же title и cron_expr.
//
// Сценарий:
// 1. Создаём напоминание с cron_expr и due_at в прошлом
// 2. Запускаем планировщик
// 3. После тика: исходное — fired, новое — pending
func TestSchedulerReschedule(t *testing.T) {
	store := newTestStore(t)

	// Ежеминутное расписание — due_at в прошлом, чтобы сработало сразу.
	cronExpr := "* * * * *" // каждую минуту
	r := models.NewReminder("стендап", time.Now().Add(-time.Minute), cronExpr)
	if err := store.Create(r); err != nil {
		t.Fatalf("Create: %v", err)
	}

	sched := NewSchedulerWithInterval(store, 100*time.Millisecond)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	go sched.Start(ctx)
	defer sched.Stop()

	// Ждём нескольких тиков.
	time.Sleep(350 * time.Millisecond)

	// Исходное напоминание должно стать fired.
	fired, err := store.GetByID(r.ID)
	if err != nil {
		t.Fatalf("GetByID original: %v", err)
	}
	if fired.Status != models.StatusFired {
		t.Errorf("original status: got %q, want fired", fired.Status)
	}

	// Должно появиться новое pending-напоминание с тем же title.
	allPending, err := store.List(models.StatusPending)
	if err != nil {
		t.Fatalf("List pending: %v", err)
	}

	// Ищем новое напоминание (не исходное).
	var newReminder *models.Reminder
	for i, pr := range allPending {
		if pr.ID != r.ID && pr.Title == r.Title {
			newReminder = &allPending[i]
			break
		}
	}

	if newReminder == nil {
		t.Fatal("rescheduled reminder not found in pending list")
	}
	if newReminder.CronExpr != cronExpr {
		t.Errorf("CronExpr: got %q, want %q", newReminder.CronExpr, cronExpr)
	}
	// Следующее время должно быть в будущем.
	if !newReminder.DueAt.After(time.Now()) {
		t.Errorf("rescheduled DueAt %v is not in the future", newReminder.DueAt)
	}
}
