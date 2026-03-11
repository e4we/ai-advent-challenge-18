// Пакет tools содержит MCP-инструменты (tools), которые Claude может вызывать.
// Каждый инструмент — это функция с типизированными входными и выходными параметрами,
// зарегистрированная на MCP-сервере через официальный Go SDK.
//
// Как MCP-агент вызывает эти инструменты:
// 1. Claude получает список доступных tools при инициализации сессии
// 2. При обработке запроса пользователя Claude решает вызвать tool
// 3. Claude формирует JSON с параметрами согласно схеме tool
// 4. MCP-сервер получает вызов, валидирует параметры, вызывает handler
// 5. Handler возвращает результат, Claude включает его в ответ пользователю
package tools

import (
	"context"
	"fmt"
	"log"
	"os"
	"time"

	"mcp-reminder/models"
	"mcp-reminder/storage"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/robfig/cron/v3"
)

// logger пишет в stderr, чтобы не смешиваться с stdout (MCP-протокол).
var logger = log.New(os.Stderr, "[tools] ", log.LstdFlags)

// RegisterTools регистрирует все MCP-инструменты на сервере.
// Принимает server и store — единственные зависимости для всех handlers.
func RegisterTools(server *mcp.Server, store *storage.Store) {
	// mcp.AddTool — дженерик-функция из официального Go SDK.
	// Сигнатура: AddTool[In, Out any](server, tool, handler)
	// SDK автоматически:
	// - Генерирует JSON Schema из структуры In (поля + jsonschema-теги)
	// - Десериализует входные параметры в In перед вызовом handler
	// - Валидирует входные параметры против схемы
	// - Сериализует Out в JSON для ответа
	// Это освобождает нас от ручного парсинга и валидации.

	mcp.AddTool(server,
		&mcp.Tool{
			Name:        "create_reminder",
			Description: "Создаёт новое напоминание. Укажи либо due_in_minutes (через сколько минут), либо cron_expr (расписание).",
		},
		makeCreateReminderHandler(store),
	)

	mcp.AddTool(server,
		&mcp.Tool{
			Name:        "list_reminders",
			Description: "Возвращает список напоминаний. Можно фильтровать по статусу: pending, fired, cancelled. Пустой status — все напоминания.",
		},
		makeListRemindersHandler(store),
	)

	mcp.AddTool(server,
		&mcp.Tool{
			Name:        "get_summary",
			Description: "Возвращает сводку по напоминаниям: счётчики по статусам, ближайшие 5 и последние 5 сработавших.",
		},
		makeGetSummaryHandler(store),
	)

	mcp.AddTool(server,
		&mcp.Tool{
			Name:        "delete_reminder",
			Description: "Отменяет напоминание по ID (помечает как cancelled).",
		},
		makeDeleteReminderHandler(store),
	)
}

// --- create_reminder ---

// CreateReminderInput — входные параметры инструмента create_reminder.
// Теги jsonschema описывают поля для документации в JSON Schema.
type CreateReminderInput struct {
	// Title — обязательный текст напоминания.
	Title string `json:"title" jsonschema:"description=Текст напоминания"`

	// DueInMinutes — через сколько минут сработает напоминание.
	// Используется если не задан cron_expr. 0 = сейчас.
	// Указатель позволяет различить "поле не передано" (nil) и "передан 0".
	DueInMinutes *int `json:"due_in_minutes,omitempty" jsonschema:"description=Через сколько минут сработать (0 = сейчас)"`

	// CronExpr — cron-расписание для периодических напоминаний.
	// Пример: "0 9 * * 1-5" — каждый будний день в 9:00.
	CronExpr string `json:"cron_expr,omitempty" jsonschema:"description=Cron-выражение для периодических напоминаний (5 полей: мин час день месяц деньнедели)"`
}

// CreateReminderOutput — результат инструмента create_reminder.
type CreateReminderOutput struct {
	// Message — текстовое подтверждение для пользователя.
	Message string `json:"message"`
	// Reminder — созданное напоминание.
	Reminder models.Reminder `json:"reminder"`
}

func makeCreateReminderHandler(store *storage.Store) mcp.ToolHandlerFor[CreateReminderInput, CreateReminderOutput] {
	return func(ctx context.Context, req *mcp.CallToolRequest, input CreateReminderInput) (*mcp.CallToolResult, CreateReminderOutput, error) {
		dueInMin := 0
		if input.DueInMinutes != nil {
			dueInMin = *input.DueInMinutes
		}
		logger.Printf("create_reminder: title=%q, due_in_minutes=%d, cron_expr=%q",
			input.Title, dueInMin, input.CronExpr)

		// Валидация: title обязателен.
		if input.Title == "" {
			return nil, CreateReminderOutput{}, fmt.Errorf("title не может быть пустым")
		}

		// Валидация: должен быть задан хотя бы один из параметров времени.
		// nil означает "поле не передано", 0 означает "прямо сейчас".
		if input.DueInMinutes == nil && input.CronExpr == "" {
			return nil, CreateReminderOutput{}, fmt.Errorf("укажи due_in_minutes или cron_expr")
		}

		var dueAt time.Time

		if input.CronExpr != "" {
			// Для cron-расписания вычисляем ближайшее время срабатывания.
			parser := cron.NewParser(cron.Minute | cron.Hour | cron.Dom | cron.Month | cron.Dow)
			schedule, err := parser.Parse(input.CronExpr)
			if err != nil {
				return nil, CreateReminderOutput{}, fmt.Errorf("неверное cron-выражение %q: %w", input.CronExpr, err)
			}
			dueAt = schedule.Next(time.Now())
		} else {
			// Для одноразового напоминания: сейчас + указанное количество минут.
			// 0 минут = напоминание "прямо сейчас".
			dueAt = time.Now().Add(time.Duration(dueInMin) * time.Minute)
		}

		reminder := models.NewReminder(input.Title, dueAt, input.CronExpr)

		if err := store.Create(reminder); err != nil {
			return nil, CreateReminderOutput{}, fmt.Errorf("ошибка создания напоминания: %w", err)
		}

		output := CreateReminderOutput{
			Message:  fmt.Sprintf("Напоминание создано: %q, сработает в %s", reminder.Title, reminder.DueAt.Format("2006-01-02 15:04:05")),
			Reminder: reminder,
		}
		return nil, output, nil
	}
}

// --- list_reminders ---

// ListRemindersInput — входные параметры инструмента list_reminders.
type ListRemindersInput struct {
	// Status — фильтр по статусу. Пустая строка = все.
	Status string `json:"status,omitempty" jsonschema:"description=Фильтр по статусу: pending/fired/cancelled. Пусто = все"`
}

// ListRemindersOutput — результат инструмента list_reminders.
type ListRemindersOutput struct {
	// Count — количество найденных напоминаний.
	Count int `json:"count"`
	// Reminders — список напоминаний.
	Reminders []models.Reminder `json:"reminders"`
}

func makeListRemindersHandler(store *storage.Store) mcp.ToolHandlerFor[ListRemindersInput, ListRemindersOutput] {
	return func(ctx context.Context, req *mcp.CallToolRequest, input ListRemindersInput) (*mcp.CallToolResult, ListRemindersOutput, error) {
		logger.Printf("list_reminders: status=%q", input.Status)

		// Валидация статуса.
		if input.Status != "" &&
			input.Status != models.StatusPending &&
			input.Status != models.StatusFired &&
			input.Status != models.StatusCancelled {
			return nil, ListRemindersOutput{}, fmt.Errorf(
				"неверный статус %q, допустимые значения: pending, fired, cancelled",
				input.Status,
			)
		}

		reminders, err := store.List(input.Status)
		if err != nil {
			return nil, ListRemindersOutput{}, fmt.Errorf("ошибка получения списка: %w", err)
		}

		if reminders == nil {
			reminders = []models.Reminder{}
		}

		output := ListRemindersOutput{
			Count:     len(reminders),
			Reminders: reminders,
		}
		return nil, output, nil
	}
}

// --- get_summary ---

// GetSummaryInput — get_summary не принимает параметров.
// Пустая структура используется, потому что SDK ожидает типизированный Input.
type GetSummaryInput struct{}

// GetSummaryOutput — результат инструмента get_summary.
type GetSummaryOutput struct {
	// Text — человекочитаемый текст сводки.
	Text string `json:"text"`
	// Summary — структурированные данные сводки.
	Summary models.ReminderSummary `json:"summary"`
}

func makeGetSummaryHandler(store *storage.Store) mcp.ToolHandlerFor[GetSummaryInput, GetSummaryOutput] {
	return func(ctx context.Context, req *mcp.CallToolRequest, _ GetSummaryInput) (*mcp.CallToolResult, GetSummaryOutput, error) {
		logger.Println("get_summary")

		summary, err := store.GetSummary()
		if err != nil {
			return nil, GetSummaryOutput{}, fmt.Errorf("ошибка получения сводки: %w", err)
		}

		// Формируем читаемый текст для Claude.
		text := fmt.Sprintf(
			"Напоминания:\n  Всего: %d\n  Ожидают: %d\n  Сработали: %d\n  Отменены: %d\n",
			summary.TotalCount, summary.PendingCount, summary.FiredCount, summary.CancelledCount,
		)

		if len(summary.Upcoming) > 0 {
			text += "\nБлижайшие напоминания:\n"
			for _, r := range summary.Upcoming {
				text += fmt.Sprintf("  [%s] %q → %s\n", r.ID[:8], r.Title, r.DueAt.Format("2006-01-02 15:04"))
			}
		}

		if len(summary.RecentlyFired) > 0 {
			text += "\nПоследние сработавшие:\n"
			for _, r := range summary.RecentlyFired {
				firedStr := "?"
				if r.FiredAt != nil {
					firedStr = r.FiredAt.Format("2006-01-02 15:04")
				}
				text += fmt.Sprintf("  [%s] %q — сработало в %s\n", r.ID[:8], r.Title, firedStr)
			}
		}

		output := GetSummaryOutput{
			Text:    text,
			Summary: summary,
		}
		return nil, output, nil
	}
}

// --- delete_reminder ---

// DeleteReminderInput — входные параметры инструмента delete_reminder.
type DeleteReminderInput struct {
	// ID — идентификатор напоминания для удаления.
	ID string `json:"id" jsonschema:"description=ID напоминания для отмены"`
}

// DeleteReminderOutput — результат инструмента delete_reminder.
type DeleteReminderOutput struct {
	// Message — подтверждение для пользователя.
	Message string `json:"message"`
	// ID — ID отменённого напоминания.
	ID string `json:"id"`
}

func makeDeleteReminderHandler(store *storage.Store) mcp.ToolHandlerFor[DeleteReminderInput, DeleteReminderOutput] {
	return func(ctx context.Context, req *mcp.CallToolRequest, input DeleteReminderInput) (*mcp.CallToolResult, DeleteReminderOutput, error) {
		logger.Printf("delete_reminder: id=%q", input.ID)

		if input.ID == "" {
			return nil, DeleteReminderOutput{}, fmt.Errorf("id не может быть пустым")
		}

		// Пробуем получить напоминание чтобы показать его название в ответе.
		reminder, err := store.GetByID(input.ID)
		if err != nil {
			return nil, DeleteReminderOutput{}, fmt.Errorf("напоминание %q не найдено", input.ID)
		}

		if err := store.Cancel(input.ID); err != nil {
			return nil, DeleteReminderOutput{}, fmt.Errorf("ошибка отмены напоминания: %w", err)
		}

		output := DeleteReminderOutput{
			Message: fmt.Sprintf("Напоминание %q отменено", reminder.Title),
			ID:      input.ID,
		}
		return nil, output, nil
	}
}



