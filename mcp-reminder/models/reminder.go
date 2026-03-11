// Пакет models содержит структуры данных, используемые всеми пакетами сервера.
// Здесь определена основная сущность — Reminder (напоминание) и вспомогательные типы.
package models

import (
	"crypto/rand"
	"encoding/hex"
	"time"
)

// Константы статусов напоминания.
// Используем строковые константы вместо iota, чтобы значения были понятны
// при хранении в БД и выводе в JSON — "pending" читается лучше, чем "0".
const (
	StatusPending   = "pending"   // ожидает срабатывания
	StatusFired     = "fired"     // уже сработало
	StatusCancelled = "cancelled" // отменено пользователем
)

// Reminder — основная сущность: одно напоминание.
// Хранится в SQLite, передаётся через MCP-инструменты.
type Reminder struct {
	// ID — уникальный идентификатор напоминания.
	// Генерируется при создании через crypto/rand, что обеспечивает
	// уникальность без центрального счётчика и безопасность предсказания.
	ID string `json:"id"`

	// Title — текст напоминания, который пользователь сформулировал.
	Title string `json:"title"`

	// DueAt — время, когда напоминание должно сработать.
	// Планировщик проверяет все pending-напоминания с due_at <= now.
	DueAt time.Time `json:"due_at"`

	// CronExpr — опциональное cron-выражение для периодических напоминаний.
	// Если заполнено, после срабатывания планировщик создаёт новое
	// напоминание с тем же title и следующим временем по расписанию.
	// Пример: "0 9 * * 1-5" — каждый будний день в 9:00.
	CronExpr string `json:"cron_expr,omitempty"`

	// Status — текущее состояние напоминания: pending, fired, cancelled.
	Status string `json:"status"`

	// CreatedAt — время создания напоминания.
	// Используется для сортировки и аудита.
	CreatedAt time.Time `json:"created_at"`

	// FiredAt — время фактического срабатывания напоминания.
	// Это указатель (*time.Time), а не просто time.Time, потому что:
	// - До срабатывания значение отсутствует (nil), а не "нулевое время"
	// - time.Time{} (нулевое время) — это 0001-01-01, что семантически
	//   неверно: напоминание не срабатывало никогда, а не сработало давно
	// - Указатель явно выражает "значение может отсутствовать (null в JSON/SQL)"
	FiredAt *time.Time `json:"fired_at,omitempty"`
}

// ReminderSummary — агрегированная сводка по всем напоминаниям.
// Используется инструментом get_summary для быстрого обзора состояния.
type ReminderSummary struct {
	// TotalCount — общее количество напоминаний в базе.
	TotalCount int `json:"total_count"`

	// PendingCount — количество ожидающих напоминаний.
	PendingCount int `json:"pending_count"`

	// FiredCount — количество уже сработавших напоминаний.
	FiredCount int `json:"fired_count"`

	// CancelledCount — количество отменённых напоминаний.
	CancelledCount int `json:"cancelled_count"`

	// Upcoming — ближайшие 5 pending-напоминаний, отсортированные по due_at.
	// Позволяет быстро увидеть, что ждёт пользователя в ближайшее время.
	Upcoming []Reminder `json:"upcoming"`

	// RecentlyFired — последние 5 сработавших напоминаний.
	// Позволяет проверить, что было выполнено недавно.
	RecentlyFired []Reminder `json:"recently_fired"`
}

// NewReminder создаёт новое напоминание с уникальным ID и начальными значениями.
// Параметры:
//   - title: текст напоминания
//   - dueAt: время срабатывания
//   - cronExpr: cron-выражение (пустая строка если одноразовое)
func NewReminder(title string, dueAt time.Time, cronExpr string) Reminder {
	return Reminder{
		ID:        generateID(),
		Title:     title,
		DueAt:     dueAt,
		CronExpr:  cronExpr,
		Status:    StatusPending,
		CreatedAt: time.Now(),
		FiredAt:   nil, // ещё не сработало
	}
}

// generateID генерирует криптографически случайный 16-байтный идентификатор
// в шестнадцатеричном представлении (32 символа).
//
// Используем crypto/rand вместо math/rand для непредсказуемости ID,
// что важно если ID используется как токен доступа или во внешних API.
// Не тянем внешнюю библиотеку UUID: нам не нужен формат RFC 4122,
// достаточно уникальной непредсказуемой строки.
func generateID() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		// Ошибка crypto/rand крайне маловероятна на обычных системах.
		// В production-коде стоило бы возвращать ошибку, но для ID
		// проще запаниковать — это инициализационная ошибка.
		panic("generateID: crypto/rand failed: " + err.Error())
	}
	return hex.EncodeToString(b)
}
