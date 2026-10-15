package httpx

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/MainAlexStark/rst-metrology-mcp/internal/mcp"
	"github.com/MainAlexStark/rst-metrology-mcp/internal/tools"
)

const (
	readToken  = "r000000000000000000000000000000000000000000000000000000000000000"
	writeToken = "w000000000000000000000000000000000000000000000000000000000000000"
)

// testServer поднимает сетевой MCP с двумя инструментами-образцами.
//
// Кластер сюда не нужен: httpx отвечает не за работу инструментов,
// а за то, какие права достанутся запросу. Поэтому инструменты здесь
// ровно те, по которым это видно, — читающий и пишущий, оба сообщают,
// что нашли в контексте.
func testServer(t *testing.T) (*httptest.Server, func()) {
	t.Helper()

	m := mcp.NewServer("test", "0.0.1")
	m.Register(mcp.Tool{
		Name:        "probe_read",
		InputSchema: map[string]any{"type": "object"},
		Handler: func(ctx context.Context, _ json.RawMessage) (string, error) {
			safety, ok := tools.SafetyFrom(ctx)
			if !ok {
				return "", errNoSafety
			}
			return string(safety.Mode), nil
		},
	})
	m.Register(mcp.Tool{
		Name:        "probe_write",
		InputSchema: map[string]any{"type": "object"},
		Handler: func(ctx context.Context, _ json.RawMessage) (string, error) {
			safety, ok := tools.SafetyFrom(ctx)
			if !ok {
				return "", errNoSafety
			}
			if err := safety.CheckWrite("probe_write"); err != nil {
				return "", err
			}
			return "записано", nil
		},
	})

	auth := Auth{Static: NewStaticAuth(readToken, writeToken)}

	s, err := NewServer(m, auth, Config{Safety: tools.DefaultSafety()})
	if err != nil {
		t.Fatal(err)
	}
	front := httptest.NewServer(s.Handler())

	return front, front.Close
}

// errNoSafety — права не доехали до инструмента. Отдельная ошибка,
// потому что это худший из возможных исходов: инструмент, не нашедший
// прав в контексте, не должен решать за себя сам.
var errNoSafety = errors.New("права запроса не доехали до инструмента")

// toolCall — параметры вызова инструмента.
func toolCall(name string, args any) map[string]any {
	return map[string]any{"name": name, "arguments": args}
}

// isToolError разбирает ответ и говорит, вернул ли инструмент ошибку.
func isToolError(t *testing.T, body string) (bool, string) {
	t.Helper()

	var resp struct {
		Result struct {
			Content []struct {
				Text string `json:"text"`
			} `json:"content"`
			IsError bool `json:"isError"`
		} `json:"result"`
	}
	if err := json.Unmarshal([]byte(body), &resp); err != nil {
		t.Fatalf("ответ не разобрался: %v\n%s", err, body)
	}
	text := ""
	if len(resp.Result.Content) > 0 {
		text = resp.Result.Content[0].Text
	}
	return resp.Result.IsError, text
}

func TestNoTokenRejected(t *testing.T) {
	front, closeFn := testServer(t)
	defer closeFn()

	resp, _ := rpc(t, front.URL, "/mcp", "", "tools/list", nil)
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("без токена ожидался 401, получен %d", resp.StatusCode)
	}
	if resp.Header.Get("WWW-Authenticate") == "" {
		t.Error("на 401 должен возвращаться WWW-Authenticate")
	}
}

func TestWrongTokenRejected(t *testing.T) {
	front, closeFn := testServer(t)
	defer closeFn()

	resp, _ := rpc(t, front.URL, "/mcp", strings.Repeat("x", 64), "tools/list", nil)
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("с чужим токеном ожидался 401, получен %d", resp.StatusCode)
	}
}

func TestTokenInPathRejected(t *testing.T) {
	// Раньше так подключался телефон. Спецификация MCP это запрещает:
	// адрес с токеном оседает в журналах прокси и истории браузера.
	// Теперь для телефона есть OAuth, а этот путь закрыт.
	front, closeFn := testServer(t)
	defer closeFn()

	resp, _ := rpc(t, front.URL, "/mcp/"+readToken, "", "tools/list", nil)
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("токен в адресе больше не должен приниматься, получен %d", resp.StatusCode)
	}
}

func TestReadTokenCannotWrite(t *testing.T) {
	// Центральная проверка всей затеи: подключение с читающим токеном
	// не должно уметь менять кластер, даже если очень попросить.
	front, closeFn := testServer(t)
	defer closeFn()

	resp, body := rpc(t, front.URL, "/mcp", readToken, "tools/call",
		toolCall("probe_write", map[string]any{}))

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("ожидался 200 с ошибкой инструмента, получен %d", resp.StatusCode)
	}

	isErr, text := isToolError(t, body)
	if !isErr {
		t.Fatal("читающий токен не должен допускаться к записи")
	}
	if !strings.Contains(text, "прав на запись нет") {
		t.Errorf("сообщение должно объяснять причину: %s", text)
	}
}

func TestWriteTokenCanWrite(t *testing.T) {
	front, closeFn := testServer(t)
	defer closeFn()

	_, body := rpc(t, front.URL, "/mcp", writeToken, "tools/call",
		toolCall("probe_write", map[string]any{}))

	isErr, text := isToolError(t, body)
	if isErr {
		t.Fatalf("пишущий токен должен допускаться к записи: %s", text)
	}
	if text != "записано" {
		t.Errorf("инструмент должен был отработать, получено %q", text)
	}
}

func TestScopeOfConnectionReachesTool(t *testing.T) {
	// Права — свойство запроса, а не процесса: два подключения к одному
	// и тому же серверу должны получать разные права, и решает это
	// токен, а не переменная окружения.
	front, closeFn := testServer(t)
	defer closeFn()

	_, readBody := rpc(t, front.URL, "/mcp", readToken, "tools/call",
		toolCall("probe_read", map[string]any{}))
	if _, text := isToolError(t, readBody); text != string(tools.ModeReadOnly) {
		t.Errorf("для читающего токена ожидался режим read-only, получено %q", text)
	}

	_, writeBody := rpc(t, front.URL, "/mcp", writeToken, "tools/call",
		toolCall("probe_read", map[string]any{}))
	if _, text := isToolError(t, writeBody); text != string(tools.ModeWrite) {
		t.Errorf("для пишущего токена ожидался режим write, получено %q", text)
	}
}

// Инструмент, до которого не доехали права, не должен додумывать их
// сам. Отдельный тест, потому что это и есть способ обойти защиту:
// подставить разрешающие настройки, когда в контексте ничего нет.
func TestSafetyAlwaysPresentInContext(t *testing.T) {
	front, closeFn := testServer(t)
	defer closeFn()

	_, body := rpc(t, front.URL, "/mcp", readToken, "tools/call",
		toolCall("probe_read", map[string]any{}))

	if _, text := isToolError(t, body); strings.Contains(text, errNoSafety.Error()) {
		t.Fatal("сетевой транспорт обязан класть права в контекст каждого запроса")
	}
}

func TestForeignOriginRejected(t *testing.T) {
	// Защита от DNS rebinding: сайт в браузере не должен ходить
	// к серверу от имени пользователя.
	front, closeFn := testServer(t)
	defer closeFn()

	req, _ := http.NewRequest(http.MethodPost, front.URL+"/mcp", strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`))
	req.Header.Set("Authorization", "Bearer "+readToken)
	req.Header.Set("Origin", "https://evil.example")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("чужой Origin должен отклоняться, получен %d", resp.StatusCode)
	}
}

func TestNotificationGets202(t *testing.T) {
	front, closeFn := testServer(t)
	defer closeFn()

	req, _ := http.NewRequest(http.MethodPost, front.URL+"/mcp",
		strings.NewReader(`{"jsonrpc":"2.0","method":"notifications/initialized"}`))
	req.Header.Set("Authorization", "Bearer "+readToken)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("на уведомление ожидался 202, получен %d", resp.StatusCode)
	}
}

func TestAcceptMustNameBothFormats(t *testing.T) {
	// Спецификация требует от клиента готовности принять оба формата.
	// Прежняя реализация была снисходительнее и отвечала потоком
	// событий тому, кто просил только его; SDK такие запросы отклоняет.
	// Для автоматизации это значит: заголовки обязательны — см.
	// docs/REMOTE.md.
	front, closeFn := testServer(t)
	defer closeFn()

	req, _ := http.NewRequest(http.MethodPost, front.URL+"/mcp",
		strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`))
	req.Header.Set("Authorization", "Bearer "+readToken)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "text/event-stream")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("неполный Accept должен отклоняться, получен %d", resp.StatusCode)
	}
}

func TestHostFromReverseProxyIsAccepted(t *testing.T) {
	// Штатная схема развёртывания: Caddy на 443 и сервер на localhost.
	// Запрос приходит на петлевой адрес с внешним Host — встроенная
	// в SDK защита от DNS rebinding отклонила бы его, и сервер за
	// прокси перестал бы работать целиком.
	front, closeFn := testServer(t)
	defer closeFn()

	req, _ := http.NewRequest(http.MethodPost, front.URL+"/mcp",
		strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`))
	req.Header.Set("Authorization", "Bearer "+readToken)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	req.Host = "rst-metrology-mcp.example.com"

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("запрос из-за обратного прокси должен проходить, получен %d: %s", resp.StatusCode, body)
	}
}

func TestGetIsNotAllowed(t *testing.T) {
	front, closeFn := testServer(t)
	defer closeFn()

	req, _ := http.NewRequest(http.MethodGet, front.URL+"/mcp", nil)
	req.Header.Set("Authorization", "Bearer "+readToken)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("GET должен отвечать 405, получен %d", resp.StatusCode)
	}
}

func TestHealthNeedsNoToken(t *testing.T) {
	front, closeFn := testServer(t)
	defer closeFn()

	resp, err := http.Get(front.URL + "/healthz")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("healthz должен отвечать без токена, получен %d", resp.StatusCode)
	}
}

// rpc отправляет JSON-RPC сообщение по HTTP.
func rpc(t *testing.T, base, path, token, method string, params any) (*http.Response, string) {
	t.Helper()

	body := map[string]any{"jsonrpc": "2.0", "id": 1, "method": method}
	if params != nil {
		body["params"] = params
	}
	raw, _ := json.Marshal(body)

	url := base + path
	req, err := http.NewRequest(http.MethodPost, url, strings.NewReader(string(raw)))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	raw2, _ := io.ReadAll(resp.Body)
	return resp, string(raw2)
}
