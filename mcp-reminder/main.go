// Пакет main — точка входа MCP-сервера напоминаний.
//
// Архитектура при запуске:
//
//	MCP-клиент (Claude Desktop / Claude Code)
//	    ↕ JSON-RPC через stdin/stdout (StdioTransport)
//	MCP-сервер (главная горутина, блокирующий вызов)
//	    ↓ обращается к
//	Storage (SQLite)
//	    ↑ обращается к
//	Scheduler (отдельная горутина, тикер каждые 30с)
//
// Планировщик запускается в отдельной горутине, потому что:
// - MCP-сервер блокирует главную горутину (ждёт запросы из stdin)
// - Планировщик должен работать параллельно, не прерывая обработку MCP-запросов
// - Обе компоненты обращаются к общему Store (database/sql потокобезопасен)
package main

import (
	"context"
	"log"
	"os"
	"os/signal"
	"syscall"

	"mcp-reminder/scheduler"
	"mcp-reminder/storage"
	"mcp-reminder/tools"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func main() {
	// Логгер для main пишет в stderr, чтобы не загрязнять stdout.
	// StdioTransport использует stdout для MCP JSON-RPC — любой посторонний
	// вывод туда сломает протокол.
	logger := log.New(os.Stderr, "[main] ", log.LstdFlags)

	// --- 1. Инициализация хранилища ---
	// reminders.db создаётся в текущей директории.
	// В production лучше использовать путь из конфига/переменной окружения,
	// но для учебного проекта достаточно фиксированного имени.
	store, err := storage.NewStore("reminders.db")
	if err != nil {
		logger.Fatalf("failed to open storage: %v", err)
	}
	logger.Println("storage opened: reminders.db")

	// --- 2. Контекст с поддержкой отмены ---
	// context.WithCancel позволяет передать сигнал остановки во все компоненты.
	// Когда мы вызываем cancel(), все горутины, слушающие ctx.Done(), завершатся.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// --- 3. Graceful shutdown ---
	// Слушаем системные сигналы SIGINT (Ctrl+C) и SIGTERM (kill).
	// При получении сигнала — корректно останавливаем все компоненты.
	// Graceful shutdown важен, чтобы:
	// - Не потерять незафиксированные транзакции SQLite
	// - Дать планировщику завершить текущий тик
	// - Закрыть MCP-сессию корректно
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)

	go func() {
		sig := <-sigCh
		logger.Printf("received signal %v, shutting down...", sig)
		// Отменяем контекст — планировщик и MCP-сервер завершатся.
		cancel()
	}()

	// --- 4. Запуск планировщика ---
	// Планировщик запускается в отдельной горутине.
	// Он работает фоново: проверяет БД каждые 30 секунд,
	// не блокируя обработку MCP-запросов.
	sched := scheduler.NewScheduler(store)
	go sched.Start(ctx)
	logger.Println("scheduler started")

	// --- 5. Создание MCP-сервера ---
	// Implementation описывает сервер для клиента при инициализации сессии.
	// Клиент (Claude Desktop, Claude Code) получает эти данные при подключении.
	server := mcp.NewServer(&mcp.Implementation{
		Name:    "mcp-reminder",
		Version: "1.0.0",
	}, nil)

	// Регистрируем все MCP-инструменты на сервере.
	tools.RegisterTools(server, store)
	logger.Println("MCP server created with tools: create_reminder, list_reminders, get_summary, delete_reminder")

	// --- 6. Запуск MCP-сервера через StdioTransport ---
	// StdioTransport — стандартный транспорт для MCP:
	// - Сервер читает JSON-RPC запросы из os.Stdin
	// - Сервер пишет JSON-RPC ответы в os.Stdout
	// - Клиент (Claude Desktop, Claude Code) запускает сервер как дочерний процесс
	//   и общается с ним через его stdin/stdout pipes
	// Этот подход называется "stdio MCP server" — самый простой способ
	// интегрировать MCP-сервер: не нужен порт, авторизация, SSL.
	logger.Println("starting MCP server on stdio transport...")

	session, err := server.Connect(ctx, &mcp.StdioTransport{}, nil)
	if err != nil {
		logger.Fatalf("failed to connect MCP server: %v", err)
	}

	// Wait() блокируется пока MCP-сессия активна.
	// Сессия завершается когда клиент отключается или контекст отменяется.
	if err := session.Wait(); err != nil {
		// context.Canceled — нормальное завершение при graceful shutdown.
		// Другие ошибки — неожиданные проблемы с соединением.
		logger.Printf("MCP session ended: %v", err)
	}

	// --- 7. Завершение в обратном порядке ---
	// Останавливаем компоненты в порядке, обратном запуску:
	// сначала планировщик (он зависит от store), потом store.

	logger.Println("stopping scheduler...")
	sched.Stop()

	logger.Println("closing storage...")
	if err := store.Close(); err != nil {
		logger.Printf("error closing storage: %v", err)
	}

	logger.Println("shutdown complete")
}
