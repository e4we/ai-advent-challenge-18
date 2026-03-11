package tools

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"mcp-reminder/models"
	"mcp-reminder/storage"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// newTestStore создаёт изолированное хранилище для тестов инструментов.
func newTestStore(t *testing.T) *storage.Store {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "tools_test.db")
	store, err := storage.NewStore(dbPath)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	t.Cleanup(func() { store.Close() })
	return store
}

// TestCreateReminder проверяет, что инструмент create_reminder создаёт напоминание в БД.
func TestCreateReminder(t *testing.T) {
	store := newTestStore(t)
	handler := makeCreateReminderHandler(store)

	input := CreateReminderInput{
		Title:        "встреча с командой",
		DueInMinutes: 30,
	}

	// Вызываем handler напрямую (без MCP-сервера).
	_, output, err := handler(context.Background(), &mcp.CallToolRequest{}, input)
	if err != nil {
		t.Fatalf("create_reminder handler: %v", err)
	}

	if output.Reminder.ID == "" {
		t.Error("Reminder.ID is empty")
	}
	if output.Reminder.Title != input.Title {
		t.Errorf("Reminder.Title: got %q, want %q", output.Reminder.Title, input.Title)
	}
	if output.Reminder.Status != models.StatusPending {
		t.Errorf("Reminder.Status: got %q, want pending", output.Reminder.Status)
	}

	// Проверяем что напоминание реально попало в БД.
	got, err := store.GetByID(output.Reminder.ID)
	if err != nil {
		t.Fatalf("GetByID: %v", err)
	}
	if got.Title != input.Title {
		t.Errorf("DB title: got %q, want %q", got.Title, input.Title)
	}

	// Время срабатывания должно быть примерно через 30 минут.
	expectedDue := time.Now().Add(30 * time.Minute)
	diff := output.Reminder.DueAt.Sub(expectedDue)
	if diff < -5*time.Second || diff > 5*time.Second {
		t.Errorf("DueAt is off by %v from expected", diff)
	}
}

// TestCreateReminderValidation проверяет валидацию обязательных параметров.
func TestCreateReminderValidation(t *testing.T) {
	store := newTestStore(t)
	handler := makeCreateReminderHandler(store)

	// Пустой title — должна быть ошибка.
	_, _, err := handler(context.Background(), &mcp.CallToolRequest{}, CreateReminderInput{
		DueInMinutes: 10,
	})
	if err == nil {
		t.Error("expected error for empty title, got nil")
	}

	// Нет ни due_in_minutes ни cron_expr — должна быть ошибка.
	_, _, err = handler(context.Background(), &mcp.CallToolRequest{}, CreateReminderInput{
		Title: "тест",
	})
	if err == nil {
		t.Error("expected error when neither due_in_minutes nor cron_expr provided, got nil")
	}
}

// TestListReminders проверяет фильтрацию напоминаний по статусу.
func TestListReminders(t *testing.T) {
	store := newTestStore(t)

	// Создаём два pending и одно fired напоминание.
	r1 := models.NewReminder("задача 1", time.Now().Add(time.Hour), "")
	r2 := models.NewReminder("задача 2", time.Now().Add(2*time.Hour), "")
	r3 := models.NewReminder("задача выполнена", time.Now().Add(-time.Hour), "")

	for _, r := range []models.Reminder{r1, r2, r3} {
		store.Create(r)
	}
	store.MarkFired(r3.ID)

	handler := makeListRemindersHandler(store)

	// Запрос всех напоминаний.
	_, allOutput, err := handler(context.Background(), &mcp.CallToolRequest{}, ListRemindersInput{})
	if err != nil {
		t.Fatalf("list all: %v", err)
	}
	if allOutput.Count != 3 {
		t.Errorf("list all count: got %d, want 3", allOutput.Count)
	}

	// Только pending.
	_, pendingOutput, err := handler(context.Background(), &mcp.CallToolRequest{}, ListRemindersInput{Status: "pending"})
	if err != nil {
		t.Fatalf("list pending: %v", err)
	}
	if pendingOutput.Count != 2 {
		t.Errorf("list pending count: got %d, want 2", pendingOutput.Count)
	}

	// Только fired.
	_, firedOutput, err := handler(context.Background(), &mcp.CallToolRequest{}, ListRemindersInput{Status: "fired"})
	if err != nil {
		t.Fatalf("list fired: %v", err)
	}
	if firedOutput.Count != 1 {
		t.Errorf("list fired count: got %d, want 1", firedOutput.Count)
	}
}

// TestGetSummary проверяет формат и содержимое ответа get_summary.
func TestGetSummary(t *testing.T) {
	store := newTestStore(t)

	// Создаём 2 pending, 1 fired.
	for i := 0; i < 2; i++ {
		r := models.NewReminder("pending задача", time.Now().Add(time.Hour), "")
		store.Create(r)
	}
	r := models.NewReminder("сработавшая", time.Now().Add(-time.Hour), "")
	store.Create(r)
	store.MarkFired(r.ID)

	handler := makeGetSummaryHandler(store)

	_, output, err := handler(context.Background(), &mcp.CallToolRequest{}, GetSummaryInput{})
	if err != nil {
		t.Fatalf("get_summary: %v", err)
	}

	// Проверяем счётчики в структурированном ответе.
	if output.Summary.TotalCount != 3 {
		t.Errorf("TotalCount: got %d, want 3", output.Summary.TotalCount)
	}
	if output.Summary.PendingCount != 2 {
		t.Errorf("PendingCount: got %d, want 2", output.Summary.PendingCount)
	}
	if output.Summary.FiredCount != 1 {
		t.Errorf("FiredCount: got %d, want 1", output.Summary.FiredCount)
	}

	// Текстовый вывод должен содержать ключевые слова.
	if output.Text == "" {
		t.Error("Text is empty")
	}
}

// TestDeleteReminder проверяет, что delete_reminder удаляет напоминание из БД.
func TestDeleteReminder(t *testing.T) {
	store := newTestStore(t)

	r := models.NewReminder("удалить меня", time.Now().Add(time.Hour), "")
	store.Create(r)

	handler := makeDeleteReminderHandler(store)

	_, output, err := handler(context.Background(), &mcp.CallToolRequest{}, DeleteReminderInput{
		ID: r.ID,
	})
	if err != nil {
		t.Fatalf("delete_reminder: %v", err)
	}

	if output.ID != r.ID {
		t.Errorf("output ID: got %q, want %q", output.ID, r.ID)
	}

	// После удаления GetByID должно вернуть ошибку.
	_, err = store.GetByID(r.ID)
	if err == nil {
		t.Error("expected error after deletion, got nil")
	}
}
