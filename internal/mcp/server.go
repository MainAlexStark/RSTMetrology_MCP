// Package mcp — тонкий слой над официальным Go SDK протокола MCP.
//
// Здесь остались только две вещи: описание инструмента и способы отдать
// набор инструментов наружу — потоком newline-JSON (stdio) и
// обработчиком HTTP. Сам протокол — рукопожатие, согласование версий,
// форма ошибок, режим без состояния — держит
// github.com/modelcontextprotocol/go-sdk.
//
// Граница пакета сохранена намеренно: internal/tools не знает ни про
// JSON-RPC, ни про типы SDK. Поэтому следующая смена версии протокола
// останавливается здесь и не расходится по всем инструментам.
//
// Раньше инструменты регистрировались типизированным mcp.AddTool прямо
// из девяти подпакетов internal/tools/*, и каждый знал про SDK лично.
// Платой была не только связанность: мимо такой регистрации проходили
// обрезка длинных ответов и единая форма ошибки инструмента — а список
// подов большого кластера до модели попросту не доезжает.
package mcp

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"sync"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// Tool — один инструмент, доступный модели.
type Tool struct {
	Name        string
	Description string

	// InputSchema — JSON Schema аргументов. Модель видит именно её,
	// поэтому описание полей здесь важнее, чем кажется.
	InputSchema map[string]any

	// Handler получает сырые аргументы и возвращает текст результата.
	Handler func(ctx context.Context, args json.RawMessage) (string, error)
}

// Server — набор инструментов, который можно отдать любым из двух
// транспортов.
type Server struct {
	impl *sdk.Server

	// Порядок и имена ведём сами: SDK хранит инструменты в своей
	// структуре, а нам нужен стабильный порядок регистрации для
	// диагностики и проверка на повторное имя.
	mu    sync.RWMutex
	order []string
	known map[string]struct{}
}

// NewServer создаёт сервер с именем и версией.
func NewServer(name, version string) *Server {
	return &Server{
		impl: sdk.NewServer(
			&sdk.Implementation{Name: name, Version: version},
			&sdk.ServerOptions{
				// Список инструментов фиксируется на старте и больше не
				// меняется, поэтому listChanged не объявляем: клиенту
				// незачем ждать уведомлений, которых не будет.
				Capabilities: &sdk.ServerCapabilities{
					Tools:     &sdk.ToolCapabilities{},
					Resources: &sdk.ResourceCapabilities{},
				},
			},
		),
		known: make(map[string]struct{}),
	}
}

// Register добавляет инструмент. Повторная регистрация имени — ошибка
// программиста, поэтому паникуем на старте, а не молча перетираем.
func (s *Server) Register(t Tool) {
	s.mu.Lock()
	if _, dup := s.known[t.Name]; dup {
		s.mu.Unlock()
		panic("mcp: инструмент уже зарегистрирован: " + t.Name)
	}
	s.known[t.Name] = struct{}{}
	s.order = append(s.order, t.Name)
	s.mu.Unlock()

	schema := t.InputSchema
	if schema == nil {
		schema = map[string]any{"type": "object"}
	}

	handler := t.Handler

	// Регистрируем низкоуровневым способом: аргументы доходят до
	// инструмента сырыми и разбираются им самим. Типизированный AddTool
	// из SDK проверял бы их по схеме сам, но тогда описание инструмента
	// растекается по структуре Go, тегам и отдельному файлу на каждый
	// вызов — ровно то, из чего этот репозиторий и собирали обратно.
	s.impl.AddTool(&sdk.Tool{
		Name:        t.Name,
		Description: t.Description,
		InputSchema: schema,
	}, func(ctx context.Context, req *sdk.CallToolRequest) (*sdk.CallToolResult, error) {
		text, err := handler(ctx, req.Params.Arguments)
		if err != nil {
			// Ошибка инструмента возвращается как результат с isError,
			// а не как ошибка протокола: модель должна её прочитать
			// и попробовать исправиться, а не считать сервер сломанным.
			return &sdk.CallToolResult{
				Content: []sdk.Content{&sdk.TextContent{Text: err.Error()}},
				IsError: true,
			}, nil
		}
		return &sdk.CallToolResult{
			Content: []sdk.Content{&sdk.TextContent{Text: text}},
		}, nil
	})
}

// ToolNames перечисляет инструменты в порядке регистрации.
func (s *Server) ToolNames() []string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return append([]string(nil), s.order...)
}

// Serve обслуживает поток newline-JSON: так с сервером разговаривает
// Claude, запустивший его подпроцессом.
func (s *Server) Serve(ctx context.Context, in io.Reader, out io.Writer) error {
	return s.impl.Run(ctx, &sdk.IOTransport{
		Reader: io.NopCloser(in),
		Writer: nopCloser{out},
	})
}

// StreamableOptions — настройки сетевого обработчика.
type StreamableOptions struct {
	Logger *slog.Logger

	// MaxRequestBytes — потолок размера тела запроса. 0 оставляет
	// значение SDK по умолчанию.
	MaxRequestBytes int64
}

// StreamableHandler возвращает обработчик MCP поверх Streamable HTTP.
//
// Режим без состояния: сервер не выдаёт Mcp-Session-Id и не требует
// его, каждый запрос самодостаточен. Поэтому сервер переживает
// перезапуск пода и не ломается за Service, если реплик станет больше
// одной.
//
// Авторизация, проверка Origin и права подключения остаются снаружи,
// в internal/httpx: SDK ничего не знает ни про токены, ни про то, кому
// разрешена запись.
func (s *Server) StreamableHandler(opts StreamableOptions) http.Handler {
	return sdk.NewStreamableHTTPHandler(
		func(*http.Request) *sdk.Server { return s.impl },
		&sdk.StreamableHTTPOptions{
			Stateless: true,

			// Ответ отдаётся обычным JSON, а не потоком событий: сервер
			// ничего не шлёт по своей инициативе, а с JSON проще жить
			// sre-agent, который ходит сюда со статическим токеном.
			JSONResponse: true,

			MaxRequestBodyBytes: opts.MaxRequestBytes,
			Logger:              opts.Logger,

			// Встроенную в SDK защиту от DNS rebinding приходится
			// выключить: она отклоняет запрос, пришедший на локальный
			// адрес пода с внешним Host, — а это ровно штатная схема
			// (Service на 8082, снаружи домен через ingress/Caddy).
			// Проверка Origin стоит в internal/httpx и работает для
			// всех запросов.
			DisableLocalhostProtection: true,
		})
}

// nopCloser добавляет Close писателю: транспорт SDK принимает
// io.WriteCloser, а закрывать stdout или буфер теста незачем.
type nopCloser struct {
	io.Writer
}

func (nopCloser) Close() error { return nil }
