// CLI-агент для работы с MCP-сервером напоминаний через Anthropic API (Haiku).
//
// Архитектура:
//
//	Пользователь (терминал)
//	    ↕ stdin/stdout
//	CLI-агент (этот файл)
//	    ↕ Anthropic Messages API (Haiku)
//	    ↕ MCP Client (subprocess pipes)
//	MCP-сервер (mcp-reminder.exe)
//	    ↓
//	SQLite + Scheduler
package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"os/exec"
	"os/signal"
	"strings"

	anthropic "github.com/anthropics/anthropic-sdk-go"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

const (
	model     = anthropic.ModelClaudeHaiku4_5_20251001
	maxTokens = 1024
)

var systemPrompt = []anthropic.TextBlockParam{
	{Text: "Ты — помощник по управлению напоминаниями. " +
		"У тебя есть инструменты для создания, просмотра, сводки и удаления напоминаний. " +
		"Отвечай кратко и по делу на русском языке. " +
		"При создании напоминания всегда подтверждай пользователю что именно создано и когда сработает."},
}

func main() {
	logger := log.New(os.Stderr, "[agent] ", log.LstdFlags)

	apiKey := os.Getenv("ANTHROPIC_API_KEY")
	if apiKey == "" {
		fmt.Fprintln(os.Stderr, "Ошибка: переменная окружения ANTHROPIC_API_KEY не установлена")
		os.Exit(1)
	}

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt)
	defer cancel()

	// Канал для приёма push-нотификаций от MCP-сервера (resources/list_changed).
	notifyCh := make(chan struct{}, 10)

	// Подключаемся к MCP-серверу
	session, err := connectMCP(ctx, logger, notifyCh)
	if err != nil {
		logger.Fatalf("не удалось подключиться к MCP-серверу: %v", err)
	}
	defer session.Close()

	// Получаем список инструментов
	mcpTools, err := listMCPTools(ctx, session)
	if err != nil {
		logger.Fatalf("не удалось получить список инструментов: %v", err)
	}
	logger.Printf("получено %d инструментов от MCP-сервера", len(mcpTools))

	// Конвертируем в формат Anthropic API
	tools := convertTools(mcpTools)

	// Создаём клиент Anthropic API
	client := anthropic.NewClient()

	fmt.Println("Агент напоминаний запущен. Введите запрос (Ctrl+C для выхода):")
	fmt.Println()

	runChatLoop(ctx, logger, client, session, tools, notifyCh)
}

// connectMCP запускает MCP-сервер как subprocess и устанавливает соединение.
// notifyCh получает сигнал при каждом notifications/resources/list_changed.
func connectMCP(ctx context.Context, logger *log.Logger, notifyCh chan<- struct{}) (*mcp.ClientSession, error) {
	cmd := exec.Command("./mcp-reminder.exe")
	cmd.Stderr = os.Stderr

	transport := &mcp.CommandTransport{Command: cmd}
	client := mcp.NewClient(&mcp.Implementation{
		Name:    "reminder-agent",
		Version: "1.0.0",
	}, &mcp.ClientOptions{
		ResourceListChangedHandler: func(ctx context.Context, req *mcp.ResourceListChangedRequest) {
			select {
			case notifyCh <- struct{}{}:
			default: // не блокировать SDK-горутину, буфер справится
			}
		},
	})

	session, err := client.Connect(ctx, transport, nil)
	if err != nil {
		return nil, fmt.Errorf("connect: %w", err)
	}

	logger.Println("MCP-сервер подключён")
	return session, nil
}

// listMCPTools получает список доступных инструментов от MCP-сервера.
func listMCPTools(ctx context.Context, session *mcp.ClientSession) ([]*mcp.Tool, error) {
	result, err := session.ListTools(ctx, nil)
	if err != nil {
		return nil, err
	}
	return result.Tools, nil
}

// convertTools конвертирует MCP-инструменты в формат Anthropic API.
func convertTools(mcpTools []*mcp.Tool) []anthropic.ToolUnionParam {
	tools := make([]anthropic.ToolUnionParam, 0, len(mcpTools))
	for _, t := range mcpTools {
		schema := anthropic.ToolInputSchemaParam{}

		// InputSchema на клиенте десериализуется как map[string]any
		if schemaMap, ok := t.InputSchema.(map[string]any); ok {
			if props, ok := schemaMap["properties"]; ok {
				schema.Properties = props
			}
			if req, ok := schemaMap["required"].([]any); ok {
				required := make([]string, 0, len(req))
				for _, r := range req {
					if s, ok := r.(string); ok {
						required = append(required, s)
					}
				}
				schema.Required = required
			}
		}

		// Гарантируем непустую схему для инструментов без параметров (например, get_summary).
		// ToolInputSchemaParam с nil Properties считается нулевым и опускается из JSON с тегом omitzero,
		// что вызывает ошибку API "input_schema: Field required".
		if schema.Properties == nil {
			schema.Properties = map[string]any{}
		}

		tools = append(tools, anthropic.ToolUnionParam{
			OfTool: &anthropic.ToolParam{
				Name:        t.Name,
				Description: anthropic.String(t.Description),
				InputSchema: schema,
			},
		})
	}
	return tools
}

// runChatLoop — основной интерактивный цикл чтения ввода и обработки.
// Поддерживает async push-нотификации через notifyCh: когда планировщик
// срабатывает, агент сам инжектирует системное сообщение без ввода пользователя.
func runChatLoop(ctx context.Context, logger *log.Logger, client anthropic.Client, session *mcp.ClientSession, tools []anthropic.ToolUnionParam, notifyCh <-chan struct{}) {
	var messages []anthropic.MessageParam

	// Чтение stdin в отдельной горутине, чтобы не блокировать select.
	inputCh := make(chan string)
	go func() {
		scanner := bufio.NewScanner(os.Stdin)
		for scanner.Scan() {
			input := strings.TrimSpace(scanner.Text())
			if input == "" {
				fmt.Print("> ")
				continue
			}
			select {
			case inputCh <- input:
			case <-ctx.Done():
				return
			}
		}
		close(inputCh)
	}()

	fmt.Print("> ")

	for {
		select {
		case <-ctx.Done():
			return

		case input, ok := <-inputCh:
			if !ok {
				return
			}
			messages = append(messages, anthropic.NewUserMessage(anthropic.NewTextBlock(input)))
			var err error
			messages, err = agentTurn(ctx, logger, client, session, tools, messages)
			if err != nil {
				fmt.Fprintf(os.Stderr, "Ошибка: %v\n", err)
				messages = messages[:len(messages)-1]
			}
			fmt.Println()
			fmt.Print("> ")

		case <-notifyCh:
			// Дренируем лишние нотификации, накопившиеся за время обработки.
			drainCh(notifyCh)
			sysMsg := "[СИСТЕМА] Сработало одно или несколько напоминаний. " +
				"Вызови list_reminders(status=\"fired\") и сообщи пользователю."
			messages = append(messages, anthropic.NewUserMessage(anthropic.NewTextBlock(sysMsg)))
			var err error
			messages, err = agentTurn(ctx, logger, client, session, tools, messages)
			if err != nil {
				logger.Printf("ошибка при обработке нотификации: %v", err)
				messages = messages[:len(messages)-1]
			}
			fmt.Println()
			fmt.Print("> ")
		}
	}
}

// drainCh сбрасывает все накопившиеся значения из небуферизованного/буферизованного канала.
func drainCh(ch <-chan struct{}) {
	for {
		select {
		case <-ch:
		default:
			return
		}
	}
}

// agentTurn выполняет один ход агента: отправка в API → обработка tool_use → финальный ответ.
// Возвращает обновлённую историю сообщений.
func agentTurn(ctx context.Context, logger *log.Logger, client anthropic.Client, session *mcp.ClientSession, tools []anthropic.ToolUnionParam, messages []anthropic.MessageParam) ([]anthropic.MessageParam, error) {
	for {
		resp, err := client.Messages.New(ctx, anthropic.MessageNewParams{
			Model:     model,
			MaxTokens: maxTokens,
			System:    systemPrompt,
			Messages:  messages,
			Tools:     tools,
		})
		if err != nil {
			return messages, fmt.Errorf("Anthropic API: %w", err)
		}

		// Добавляем ответ ассистента в историю
		messages = append(messages, resp.ToParam())

		if resp.StopReason != anthropic.StopReasonToolUse {
			// Финальный ответ — печатаем текст
			for _, block := range resp.Content {
				if block.Type == "text" {
					fmt.Println(block.Text)
				}
			}
			return messages, nil
		}

		// Обрабатываем вызовы инструментов
		var toolResults []anthropic.ContentBlockParamUnion
		for _, block := range resp.Content {
			if block.Type != "tool_use" {
				continue
			}

			logger.Printf("вызов инструмента: %s", block.Name)

			// Десериализуем входные параметры
			var args map[string]any
			if err := json.Unmarshal(block.Input, &args); err != nil {
				toolResults = append(toolResults, anthropic.NewToolResultBlock(block.ID, fmt.Sprintf("ошибка разбора аргументов: %v", err), true))
				continue
			}

			// Вызываем инструмент через MCP
			result, err := session.CallTool(ctx, &mcp.CallToolParams{
				Name:      block.Name,
				Arguments: args,
			})
			if err != nil {
				toolResults = append(toolResults, anthropic.NewToolResultBlock(block.ID, fmt.Sprintf("ошибка вызова инструмента: %v", err), true))
				continue
			}

			text := extractMCPResult(result)
			toolResults = append(toolResults, anthropic.NewToolResultBlock(block.ID, text, false))
		}

		// Добавляем результаты инструментов как user message
		messages = append(messages, anthropic.NewUserMessage(toolResults...))
	}
}

// extractMCPResult извлекает текстовое содержимое из результата вызова MCP-инструмента.
func extractMCPResult(result *mcp.CallToolResult) string {
	var parts []string
	for _, c := range result.Content {
		if tc, ok := c.(*mcp.TextContent); ok {
			parts = append(parts, tc.Text)
		}
	}
	if len(parts) > 0 {
		return strings.Join(parts, "\n")
	}
	// Fallback: маршалим весь результат в JSON
	data, err := json.Marshal(result)
	if err != nil {
		return fmt.Sprintf("результат: %v", result)
	}
	return string(data)
}
