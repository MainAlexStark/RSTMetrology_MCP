package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/MainAlexStark/rst-metrology-mcp/arshin"
	"github.com/MainAlexStark/rst-metrology-mcp/internal/mcp"
)

// probe — read-вызов, который безопасно сделать для проверки доступа.
type probe struct {
	Tool string
	Call func(ctx context.Context, c arshin.Client, year int) error
}

// probes — по одному минимальному запросу на каждый метод API.
//
// Поиск поверок и поиск типов — разные разделы ФГИС, и падают они
// порознь: техработы на одном не означают техработ на другом.
func probes() []probe {
	return []probe{
		{"search_verifications", func(ctx context.Context, c arshin.Client, year int) error {
			_, err := c.SearchVerifications(ctx, arshin.VRIQuery{MITNumber: "16078-05", Year: year, Rows: 1})
			return err
		}},
		{"search_types", func(ctx context.Context, c arshin.Client, _ int) error {
			_, err := c.SearchTypes(ctx, arshin.MITQuery{Search: "16078-05", Rows: 1})
			return err
		}},
	}
}

// RegisterDiagnostics добавляет инструменты состояния и самопроверки.
func (r *Registry) RegisterDiagnostics(baseURL string) {
	r.AddCustom(mcp.Tool{
		Name: "rst_status",
		Description: "Состояние сервера: адрес API ФГИС, режим подключения, потолок ответа, " +
			"лимит запросов на вызов. Вызовите первым, если инструменты отказывают и непонятно почему.",
		InputSchema: schema(obj{}),
		Handler: func(ctx context.Context, _ json.RawMessage) (string, error) {
			safety := r.safetyFor(ctx)

			var b strings.Builder
			fmt.Fprintf(&b, "API ФГИС «Аршин»:          %s\n", baseURL)
			fmt.Fprintf(&b, "Режим этого подключения:   %s\n", safety.Mode)
			fmt.Fprintf(&b, "Потолок ответа:            %d байт\n", safety.MaxResponseBytes)
			fmt.Fprintf(&b, "Запросов к ФГИС на вызов:  не больше %d\n", safety.MaxRequestsPerCall)
			fmt.Fprintf(&b, "\nИнструментов зарегистрировано: %d\n", len(r.server.ToolNames()))
			b.WriteString("\nЛимит ФГИС — 2 запроса в секунду с адреса; сервер держит паузу между запросами сам.\n")
			b.WriteString("ФГИС отвечает только российским адресам. Проверить доступ: rst_selftest.")
			return b.String(), nil
		},
	})

	r.AddCustom(mcp.Tool{
		Name: "rst_selftest",
		Description: "Проверить доступ к ФГИС «Аршин»: по одному минимальному запросу в каждый раздел API " +
			"(поверки, типы СИ). Показывает, отвечает ли ФГИС этому серверу вообще.",
		InputSchema: schema(obj{}),
		Handler: func(ctx context.Context, _ json.RawMessage) (string, error) {
			list := probes()
			results := make([]string, len(list))
			failed := 0
			year := r.now().Year()

			var (
				wg sync.WaitGroup
				mu sync.Mutex
			)
			// Параллельно запросы всё равно встанут в очередь клиента
			// (пауза между запросами общая), зато таймаут считается
			// для каждого отдельно.
			for i, p := range list {
				wg.Add(1)
				go func(i int, p probe) {
					defer wg.Done()
					line, ok := runProbe(ctx, r.fgis, p, year)
					results[i] = line
					if !ok {
						mu.Lock()
						failed++
						mu.Unlock()
					}
				}(i, p)
			}
			wg.Wait()

			var b strings.Builder
			b.WriteString("Проверка доступа к ФГИС «Аршин»\n\n")
			for _, line := range results {
				b.WriteString(line)
				b.WriteString("\n")
			}
			if failed == len(list) {
				b.WriteString("\nНе ответил ни один раздел: дело не в запросах, а в доступе к ФГИС.\n")
				b.WriteString("Сервис отвечает только российским адресам — проверьте, откуда запущен сервер,\n")
				b.WriteString("или задайте HTTPS_PROXY / RST_BASE_URL.")
			} else if failed > 0 {
				b.WriteString("\nОдин раздел не ответил — у ФГИС бывают техработы по разделам; повторите позже.")
			}
			return b.String(), nil
		},
	})
}

// runProbe выполняет один пробный запрос и описывает результат строкой.
func runProbe(ctx context.Context, c arshin.Client, p probe, year int) (string, bool) {
	ctx, cancel := context.WithTimeout(ctx, 40*time.Second)
	defer cancel()

	start := time.Now()
	err := p.Call(ctx, c, year)
	elapsed := time.Since(start).Round(time.Millisecond)

	if err == nil {
		return fmt.Sprintf("  OK       %-22s %s", p.Tool, elapsed), true
	}
	line := fmt.Sprintf("  ОТКАЗ    %-22s %v", p.Tool, err)
	if hint := arshin.Hint(err); hint != "" {
		line += "\n           " + strings.ReplaceAll(hint, "\n", "\n           ")
	}
	return line, false
}
