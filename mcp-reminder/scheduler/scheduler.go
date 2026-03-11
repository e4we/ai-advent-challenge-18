// Пакет scheduler реализует фоновый планировщик напоминаний.
// Периодически проверяет базу данных и помечает просроченные напоминания как выполненные.
// Для периодических напоминаний (с cron-выражением) создаёт следующее вхождение.
package scheduler

import (
	"context"
	"fmt"
	"log"
	"os"
	"sync"
	"time"

	"mcp-reminder/models"
	"mcp-reminder/storage"

	"github.com/robfig/cron/v3"
)

// Scheduler — фоновый планировщик напоминаний.
// Работает как горутина: периодически проверяет БД и обрабатывает просроченные напоминания.
type Scheduler struct {
	// store — ссылка на хранилище. Планировщик не владеет им:
	// жизненный цикл Store управляется в main.go.
	store *storage.Store

	// cronParser используется для парсинга cron-выражений.
	// Нам не нужен полноценный cron-daemon (robfig/cron.Cron),
	// потому что нам нужно только вычислять следующее время,
	// а не запускать функции по расписанию.
	cronParser cron.Parser

	// ticker тикает каждые 30 секунд — момент проверки просроченных напоминаний.
	// Почему горутина + тикер, а не просто cron для всего:
	// - cron работает только с фиксированными расписаниями (cron-выражениями)
	// - Одноразовые напоминания (due_at = конкретное время) не вписываются
	//   в модель cron: нужно было бы регистрировать отдельный job на каждое
	// - Тикер + проверка БД — проще: один цикл обрабатывает все типы
	ticker *time.Ticker

	// quit — канал для graceful shutdown.
	// Паттерн с quit-каналом: вместо глобального флага используем канал,
	// потому что канал позволяет синхронизировать завершение через select.
	// close(quit) разблокирует все горутины, ждущие в <-quit.
	quit chan struct{}

	// mu защищает состояние планировщика от гонок данных.
	// Хотя тикер один и горутина одна, MCP-хендлеры вызываются
	// из отдельных горутин (сервер обрабатывает запросы конкурентно).
	// Без mutex два вызова могли бы одновременно модифицировать состояние.
	mu sync.Mutex

	// logger — для записи информации о срабатывании напоминаний.
	// Пишет в stderr, чтобы не смешиваться с stdout (MCP-протокол через stdio).
	logger *log.Logger

	// interval — интервал проверки. Вынесен в поле для удобства тестирования:
	// в тестах используем 100ms вместо 30s.
	interval time.Duration
}

// NewScheduler создаёт новый планировщик с дефолтным интервалом 30 секунд.
func NewScheduler(store *storage.Store) *Scheduler {
	return &Scheduler{
		store:    store,
		interval: 30 * time.Second,
		quit:     make(chan struct{}),
		logger:   log.New(os.Stderr, "[scheduler] ", log.LstdFlags),
		// Стандартный cron-парсер поддерживает 5 полей: минута, час, день, месяц, день недели.
		// WithSeconds() добавляет поле секунд (6 полей), но для пользовательских
		// напоминаний достаточно стандартного формата.
		cronParser: cron.NewParser(cron.Minute | cron.Hour | cron.Dom | cron.Month | cron.Dow),
	}
}

// NewSchedulerWithInterval создаёт планировщик с кастомным интервалом.
// Используется в тестах для ускорения проверок.
func NewSchedulerWithInterval(store *storage.Store, interval time.Duration) *Scheduler {
	s := NewScheduler(store)
	s.interval = interval
	return s
}

// Start запускает планировщик в фоне.
// Блокируется до завершения через ctx или Stop().
// Вызывается в горутине: go scheduler.Start(ctx)
func (s *Scheduler) Start(ctx context.Context) {
	s.mu.Lock()
	s.ticker = time.NewTicker(s.interval)
	s.mu.Unlock()

	s.logger.Printf("started, check interval: %s", s.interval)

	// Главный цикл планировщика.
	// select ждёт первое из событий:
	// 1. Тик — время проверить БД
	// 2. ctx.Done() — контекст отменён (graceful shutdown из main)
	// 3. quit — прямой вызов Stop()
	for {
		select {
		case <-s.ticker.C:
			s.checkAndFire()

		case <-ctx.Done():
			s.logger.Println("context cancelled, stopping")
			return

		case <-s.quit:
			s.logger.Println("stop requested, stopping")
			return
		}
	}
}

// Stop останавливает планировщик.
// Безопасно вызывать из любой горутины.
func (s *Scheduler) Stop() {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.ticker != nil {
		s.ticker.Stop()
		s.ticker = nil
	}

	// Закрываем quit-канал — это разблокирует горутину Start.
	// Используем select с default чтобы не паниковать при повторном вызове Stop.
	select {
	case <-s.quit:
		// Канал уже закрыт, ничего не делаем
	default:
		close(s.quit)
	}
}

// checkAndFire проверяет просроченные напоминания и обрабатывает их.
// Вызывается при каждом тике планировщика.
func (s *Scheduler) checkAndFire() {
	due, err := s.store.GetDueReminders()
	if err != nil {
		s.logger.Printf("error getting due reminders: %v", err)
		return
	}

	for _, r := range due {
		// Помечаем напоминание как сработавшее.
		if err := s.store.MarkFired(r.ID); err != nil {
			s.logger.Printf("error marking reminder %s as fired: %v", r.ID, err)
			continue
		}

		s.logger.Printf("FIRED: [%s] %q (due: %s)", r.ID, r.Title, r.DueAt.Format(time.RFC3339))

		// Для периодических напоминаний — создаём следующее вхождение.
		if r.CronExpr != "" {
			if err := s.reschedule(r); err != nil {
				s.logger.Printf("error rescheduling %s: %v", r.ID, err)
			}
		}
	}
}

// reschedule создаёт следующее вхождение периодического напоминания.
// Вычисляет следующее время по cron-расписанию и сохраняет новое напоминание.
func (s *Scheduler) reschedule(r models.Reminder) error {
	// Парсим cron-выражение.
	schedule, err := s.cronParser.Parse(r.CronExpr)
	if err != nil {
		return fmt.Errorf("parse cron %q: %w", r.CronExpr, err)
	}

	// Вычисляем следующее время: от текущего момента, а не от due_at,
	// чтобы избежать накопления задержек при пропусках (например, если
	// сервер был выключен).
	nextDueAt := schedule.Next(time.Now())

	// Создаём новое напоминание с тем же содержимым, но новым ID и временем.
	newReminder := models.NewReminder(r.Title, nextDueAt, r.CronExpr)

	if err := s.store.Create(newReminder); err != nil {
		return fmt.Errorf("create next reminder: %w", err)
	}

	s.logger.Printf("rescheduled: [%s] %q -> next at %s", newReminder.ID, newReminder.Title, nextDueAt.Format(time.RFC3339))
	return nil
}

