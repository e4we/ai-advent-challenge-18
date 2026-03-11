// Пакет storage отвечает за персистентное хранение напоминаний в SQLite.
// Предоставляет CRUD-операции и специализированные запросы для планировщика.
package storage

import (
	"database/sql"
	"fmt"
	"time"

	"mcp-reminder/models"

	// Импортируем modernc.org/sqlite как драйвер database/sql.
	// Используем side-effect import (_), потому что нам нужна только
	// регистрация драйвера "sqlite", а не прямое использование пакета.
	_ "modernc.org/sqlite"
)

// Store — хранилище напоминаний на основе SQLite.
// Потокобезопасно: database/sql управляет пулом соединений внутри.
type Store struct {
	db *sql.DB
}

// NewStore открывает (или создаёт) SQLite базу данных по указанному пути
// и инициализирует схему таблиц.
func NewStore(dbPath string) (*Store, error) {
	// Открываем SQLite базу. modernc.org/sqlite — чистый Go, без CGo,
	// поэтому работает на любой платформе без установки libsqlite3.
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		return nil, fmt.Errorf("open database: %w", err)
	}

	// Проверяем реальное подключение (sql.Open только парсит DSN).
	if err := db.Ping(); err != nil {
		db.Close()
		return nil, fmt.Errorf("ping database: %w", err)
	}

	store := &Store{db: db}

	if err := store.init(); err != nil {
		db.Close()
		return nil, fmt.Errorf("init schema: %w", err)
	}

	return store, nil
}

// init создаёт таблицу и индексы при первом запуске.
// Использует IF NOT EXISTS, поэтому безопасно вызывать при каждом запуске.
func (s *Store) init() error {
	// WAL (Write-Ahead Logging) режим важен для конкурентного доступа:
	// - В обычном режиме SQLite блокирует файл при записи — читатели ждут
	// - В WAL режиме читатели и писатели не блокируют друг друга:
	//   читатели видят консистентный снимок, пока идёт запись
	// Это критично, потому что планировщик и MCP-хендлеры работают
	// в разных горутинах и одновременно обращаются к БД.
	_, err := s.db.Exec(`PRAGMA journal_mode=WAL`)
	if err != nil {
		return fmt.Errorf("enable WAL mode: %w", err)
	}

	// busy_timeout заставляет SQLite ждать до 5 секунд при блокировке,
	// вместо немедленного возврата SQLITE_BUSY.
	// Это важно при конкурентном доступе из нескольких горутин.
	_, err = s.db.Exec(`PRAGMA busy_timeout=5000`)
	if err != nil {
		return fmt.Errorf("set busy_timeout: %w", err)
	}

	// Создаём таблицу напоминаний.
	// fired_at без NOT NULL — может быть NULL пока напоминание не сработало.
	_, err = s.db.Exec(`
		CREATE TABLE IF NOT EXISTS reminders (
			id         TEXT PRIMARY KEY,
			title      TEXT NOT NULL,
			due_at     DATETIME NOT NULL,
			cron_expr  TEXT DEFAULT '',
			status     TEXT DEFAULT 'pending',
			created_at DATETIME NOT NULL,
			fired_at   DATETIME
		)
	`)
	if err != nil {
		return fmt.Errorf("create table: %w", err)
	}

	// Индекс по (status, due_at) ускоряет главный запрос планировщика:
	// WHERE status='pending' AND due_at <= now
	// Без индекса — full scan по всей таблице при каждом тике.
	_, err = s.db.Exec(`
		CREATE INDEX IF NOT EXISTS idx_reminders_status_due
		ON reminders (status, due_at)
	`)
	if err != nil {
		return fmt.Errorf("create index: %w", err)
	}

	return nil
}

// Create сохраняет новое напоминание в базу данных.
func (s *Store) Create(r models.Reminder) error {
	_, err := s.db.Exec(
		`INSERT INTO reminders (id, title, due_at, cron_expr, status, created_at, fired_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?)`,
		r.ID,
		r.Title,
		r.DueAt.UTC().Format(time.RFC3339Nano),
		r.CronExpr,
		r.Status,
		r.CreatedAt.UTC().Format(time.RFC3339Nano),
		// fired_at может быть nil — SQLite сохранит NULL
		formatNullTime(r.FiredAt),
	)
	if err != nil {
		return fmt.Errorf("create reminder: %w", err)
	}
	return nil
}

// GetByID возвращает напоминание по ID.
// Возвращает sql.ErrNoRows если не найдено.
func (s *Store) GetByID(id string) (models.Reminder, error) {
	row := s.db.QueryRow(
		`SELECT id, title, due_at, cron_expr, status, created_at, fired_at
		 FROM reminders WHERE id = ?`,
		id,
	)
	return scanReminder(row)
}

// List возвращает все напоминания с опциональной фильтрацией по статусу.
// Если status пустой — возвращает все напоминания, отсортированные по due_at.
func (s *Store) List(status string) ([]models.Reminder, error) {
	var rows *sql.Rows
	var err error

	if status == "" {
		// Без фильтра — возвращаем все, сортируем по времени
		rows, err = s.db.Query(
			`SELECT id, title, due_at, cron_expr, status, created_at, fired_at
			 FROM reminders ORDER BY due_at ASC`,
		)
	} else {
		// С фильтром по статусу
		rows, err = s.db.Query(
			`SELECT id, title, due_at, cron_expr, status, created_at, fired_at
			 FROM reminders WHERE status = ? ORDER BY due_at ASC`,
			status,
		)
	}
	if err != nil {
		return nil, fmt.Errorf("list reminders: %w", err)
	}
	defer rows.Close()

	return scanReminders(rows)
}

// Delete удаляет напоминание из базы по ID.
func (s *Store) Delete(id string) error {
	result, err := s.db.Exec(`DELETE FROM reminders WHERE id = ?`, id)
	if err != nil {
		return fmt.Errorf("delete reminder: %w", err)
	}
	n, _ := result.RowsAffected()
	if n == 0 {
		return fmt.Errorf("reminder %q not found", id)
	}
	return nil
}

// Cancel помечает напоминание как cancelled (soft-delete).
// Обновляет только pending-напоминания — уже fired или cancelled не затрагиваются.
func (s *Store) Cancel(id string) error {
	result, err := s.db.Exec(
		`UPDATE reminders SET status = 'cancelled' WHERE id = ? AND status = 'pending'`,
		id,
	)
	if err != nil {
		return fmt.Errorf("cancel reminder: %w", err)
	}
	n, _ := result.RowsAffected()
	if n == 0 {
		return fmt.Errorf("reminder %q not found or already cancelled/fired", id)
	}
	return nil
}

// MarkFired помечает напоминание как сработавшее:
// устанавливает status="fired" и fired_at=текущее время UTC.
func (s *Store) MarkFired(id string) error {
	now := time.Now().UTC().Format(time.RFC3339Nano)
	result, err := s.db.Exec(
		`UPDATE reminders SET status = 'fired', fired_at = ? WHERE id = ?`,
		now, id,
	)
	if err != nil {
		return fmt.Errorf("mark fired: %w", err)
	}
	n, _ := result.RowsAffected()
	if n == 0 {
		return fmt.Errorf("reminder %q not found", id)
	}
	return nil
}

// GetDueReminders возвращает все pending-напоминания, у которых due_at <= now.
// Вызывается планировщиком при каждом тике.
func (s *Store) GetDueReminders() ([]models.Reminder, error) {
	now := time.Now().UTC().Format(time.RFC3339Nano)
	// Запрос использует составной индекс idx_reminders_status_due:
	// сначала фильтрует по status='pending', затем по due_at <= now
	rows, err := s.db.Query(
		`SELECT id, title, due_at, cron_expr, status, created_at, fired_at
		 FROM reminders
		 WHERE status = 'pending' AND due_at <= ?
		 ORDER BY due_at ASC`,
		now,
	)
	if err != nil {
		return nil, fmt.Errorf("get due reminders: %w", err)
	}
	defer rows.Close()

	return scanReminders(rows)
}

// GetSummary возвращает агрегированную статистику по напоминаниям.
// Выполняет несколько запросов: сначала агрегацию, потом топ-5 списки.
func (s *Store) GetSummary() (models.ReminderSummary, error) {
	var summary models.ReminderSummary

	// Оборачиваем все запросы в read-only транзакцию, чтобы гарантировать
	// консистентный снимок данных: между запросами данные не изменятся.
	tx, err := s.db.Begin()
	if err != nil {
		return summary, fmt.Errorf("begin transaction: %w", err)
	}
	defer tx.Rollback()

	// Агрегированные счётчики одним запросом.
	row := tx.QueryRow(`
		SELECT
			COUNT(*) as total,
			COUNT(CASE WHEN status = 'pending'   THEN 1 END) as pending,
			COUNT(CASE WHEN status = 'fired'     THEN 1 END) as fired,
			COUNT(CASE WHEN status = 'cancelled' THEN 1 END) as cancelled
		FROM reminders
	`)
	if err := row.Scan(
		&summary.TotalCount,
		&summary.PendingCount,
		&summary.FiredCount,
		&summary.CancelledCount,
	); err != nil {
		return summary, fmt.Errorf("get summary counts: %w", err)
	}

	// Ближайшие 5 pending-напоминаний (сортировка по возрастанию due_at).
	upcomingRows, err := tx.Query(`
		SELECT id, title, due_at, cron_expr, status, created_at, fired_at
		FROM reminders
		WHERE status = 'pending'
		ORDER BY due_at ASC
		LIMIT 5
	`)
	if err != nil {
		return summary, fmt.Errorf("get upcoming: %w", err)
	}
	defer upcomingRows.Close()

	summary.Upcoming, err = scanReminders(upcomingRows)
	if err != nil {
		return summary, err
	}

	// Последние 5 сработавших (сортировка по убыванию fired_at — самые свежие первыми).
	firedRows, err := tx.Query(`
		SELECT id, title, due_at, cron_expr, status, created_at, fired_at
		FROM reminders
		WHERE status = 'fired'
		ORDER BY fired_at DESC
		LIMIT 5
	`)
	if err != nil {
		return summary, fmt.Errorf("get recently fired: %w", err)
	}
	defer firedRows.Close()

	summary.RecentlyFired, err = scanReminders(firedRows)
	if err != nil {
		return summary, err
	}

	if err := tx.Commit(); err != nil {
		return summary, fmt.Errorf("commit transaction: %w", err)
	}

	return summary, nil
}

// Close закрывает соединение с базой данных.
// Должен вызываться при завершении работы сервера.
func (s *Store) Close() error {
	return s.db.Close()
}

// scanReminder читает одну строку из БД и преобразует её в структуру Reminder.
// Использует sql.NullTime для обработки NULL в поле fired_at:
// fired_at может быть NULL в SQLite, и попытка сканировать NULL в time.Time
// приведёт к ошибке. sql.NullTime безопасно обрабатывает NULL → nil.
func scanReminder(row *sql.Row) (models.Reminder, error) {
	var r models.Reminder
	var dueAtStr, createdAtStr string
	var firedAt sql.NullString // NULL-safe строка для fired_at

	err := row.Scan(
		&r.ID,
		&r.Title,
		&dueAtStr,
		&r.CronExpr,
		&r.Status,
		&createdAtStr,
		&firedAt,
	)
	if err != nil {
		return r, err
	}

	return parseReminderTimes(r, dueAtStr, createdAtStr, firedAt)
}

// scanReminders читает все строки из *sql.Rows в срез Reminder.
func scanReminders(rows *sql.Rows) ([]models.Reminder, error) {
	var reminders []models.Reminder

	for rows.Next() {
		var r models.Reminder
		var dueAtStr, createdAtStr string
		var firedAt sql.NullString

		if err := rows.Scan(
			&r.ID,
			&r.Title,
			&dueAtStr,
			&r.CronExpr,
			&r.Status,
			&createdAtStr,
			&firedAt,
		); err != nil {
			return nil, fmt.Errorf("scan reminder row: %w", err)
		}

		parsed, err := parseReminderTimes(r, dueAtStr, createdAtStr, firedAt)
		if err != nil {
			return nil, err
		}
		reminders = append(reminders, parsed)
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("rows error: %w", err)
	}

	return reminders, nil
}

// parseReminderTimes парсит строковые даты из SQLite в time.Time.
// SQLite хранит DATETIME как текст в формате RFC3339Nano.
func parseReminderTimes(r models.Reminder, dueAtStr, createdAtStr string, firedAt sql.NullString) (models.Reminder, error) {
	var err error

	r.DueAt, err = time.Parse(time.RFC3339Nano, dueAtStr)
	if err != nil {
		return r, fmt.Errorf("parse due_at %q: %w", dueAtStr, err)
	}

	r.CreatedAt, err = time.Parse(time.RFC3339Nano, createdAtStr)
	if err != nil {
		return r, fmt.Errorf("parse created_at %q: %w", createdAtStr, err)
	}

	// Если fired_at не NULL — парсим время срабатывания.
	// sql.NullString.Valid == true означает что значение есть (не NULL).
	if firedAt.Valid {
		t, err := time.Parse(time.RFC3339Nano, firedAt.String)
		if err != nil {
			return r, fmt.Errorf("parse fired_at %q: %w", firedAt.String, err)
		}
		r.FiredAt = &t
	}

	return r, nil
}

// formatNullTime преобразует *time.Time в значение для INSERT/UPDATE.
// Возвращает nil (SQL NULL) если указатель nil, иначе — строку RFC3339Nano.
func formatNullTime(t *time.Time) interface{} {
	if t == nil {
		return nil
	}
	return t.UTC().Format(time.RFC3339Nano)
}
