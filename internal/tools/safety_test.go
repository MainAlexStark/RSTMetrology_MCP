package tools

import (
	"context"
	"strings"
	"testing"

	"github.com/MainAlexStark/rst-metrology-mcp/internal/mcp"
)

// Пишущих инструментов у сервера пока нет, но заслон записи — часть
// стандарта, и первый такой инструмент не должен появиться без него.
// Поэтому заслон проверяется на подставном инструменте.

func serverWithWriteTool(mode Mode, called *bool) *mcp.Server {
	safety := DefaultSafety()
	safety.Mode = mode
	server := mcp.NewServer("test", "0")
	reg := NewRegistry(&fakeFGIS{}, safety, server)
	reg.Add(Spec{
		Name:   "fake_write",
		Desc:   "тестовый пишущий инструмент",
		Write:  true,
		Schema: schema(obj{}),
		Do: func(context.Context, Args) (any, error) {
			*called = true
			return "ok", nil
		},
	})
	return server
}

func TestWriteDeniedByDefault(t *testing.T) {
	if DefaultSafety().Mode != ModeReadOnly {
		t.Fatal("режим по умолчанию должен быть только чтение")
	}
	called := false
	text, isErr := callTool(t, serverWithWriteTool(ModeReadOnly, &called), "fake_write", nil)
	if !isErr {
		t.Fatal("запись в режиме только чтения прошла")
	}
	if called {
		t.Fatal("отказ сработал после того, как инструмент уже выполнился")
	}
	// Текст отказа называет оба способа открыть запись.
	for _, want := range []string{"RST_ALLOW_WRITES", "rst:read", "rst_status"} {
		if !strings.Contains(text, want) {
			t.Errorf("в отказе нет %q", want)
		}
	}
}

func TestWriteAllowedInWriteMode(t *testing.T) {
	called := false
	if _, isErr := callTool(t, serverWithWriteTool(ModeWrite, &called), "fake_write", nil); isErr || !called {
		t.Fatal("в режиме записи инструмент должен выполняться")
	}
}

func TestRequestSafetyOverridesProcess(t *testing.T) {
	// Процесс разрешает запись, а запрос пришёл с правами только на
	// чтение (сетевой режим, читающий токен) — побеждает запрос.
	r := &Registry{safety: Safety{Mode: ModeWrite}}
	ctx := WithSafety(context.Background(), Safety{Mode: ModeReadOnly})
	if r.safetyFor(ctx).Mode != ModeReadOnly {
		t.Fatal("права запроса не перекрыли настройки процесса")
	}
	if _, ok := SafetyFrom(context.Background()); ok {
		t.Fatal("SafetyFrom сообщил о правах, которых в контексте нет")
	}
}

func TestReadToolsWorkInReadOnly(t *testing.T) {
	server := newTestServer(t, ModeReadOnly, &fakeFGIS{})
	for _, name := range []string{"search_types", "rst_status"} {
		args := map[string]any{"search": "16078-05"}
		if _, isErr := callTool(t, server, name, args); isErr {
			t.Errorf("%s отказал в режиме только чтения", name)
		}
	}
}
