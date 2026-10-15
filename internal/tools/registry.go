package tools

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/MainAlexStark/rst-metrology-mcp/arshin"
	"github.com/MainAlexStark/rst-metrology-mcp/internal/mcp"
)

// Registry собирает инструменты и раздаёт им общий контекст:
// клиента ФГИС, настройки безопасности и режим.
type Registry struct {
	fgis   arshin.Client
	safety Safety
	server *mcp.Server

	// now подменяется в тестах: «действует ли поверка» зависит от даты.
	now func() time.Time
}

// NewRegistry создаёт реестр.
func NewRegistry(client arshin.Client, safety Safety, server *mcp.Server) *Registry {
	return &Registry{fgis: client, safety: safety, server: server, now: time.Now}
}

// Args — аргументы вызова, как их прислала модель.
//
// Карта, а не структура на каждый инструмент: описание инструмента
// тогда целиком помещается в одну спецификацию рядом со схемой,
// и добавление метода стоит десяти строк, а не нового файла с тремя
// уровнями обёрток.
type Args map[string]any

// Spec — описание инструмента.
//
// Большая часть инструментов исчерпывается этой структурой: принять
// аргументы, сходить в ФГИС, вернуть JSON. Именно поэтому проверка
// прав, разбор аргументов и обрезка ответа написаны здесь один раз,
// а не повторены в каждом инструменте.
type Spec struct {
	Name string
	Desc string

	// Write помечает инструмент, меняющий состояние внешней системы.
	// Такие проходят через Safety.CheckWrite. Сейчас их нет — реестр
	// Росстандарта публично только читается, — но заслон стоит заранее:
	// первый пишущий инструмент (например, отправка сведений о поверке
	// из личного кабинета) не должен появиться без него.
	Write bool

	// Schema — JSON Schema аргументов. Модель видит именно её,
	// поэтому описание полей здесь важнее, чем кажется.
	Schema map[string]any

	// Text означает, что инструмент возвращает готовый текст, а не
	// структуру.
	Text bool

	// Do выполняет работу. Возвращённое значение сериализуется
	// в JSON, если Text не выставлен.
	Do func(ctx context.Context, a Args) (any, error)

	// Hint объясняет отказ ФГИС там, где общий разбор ошибок
	// промахивается. Пустая строка — «объяснить нечем».
	Hint func(a Args, err error) string
}

// Add регистрирует инструмент, описанный спецификацией.
func (r *Registry) Add(s Spec) {
	r.server.Register(mcp.Tool{
		Name:        s.Name,
		Description: s.Desc,
		InputSchema: s.Schema,
		Handler: func(ctx context.Context, raw json.RawMessage) (string, error) {
			args := Args{}
			if len(raw) > 0 {
				if err := json.Unmarshal(raw, &args); err != nil {
					return "", fmt.Errorf("не разобрать аргументы: %w", err)
				}
			}

			safety := r.safetyFor(ctx)
			if s.Write {
				// Проверка стоит здесь, а не в каждом инструменте:
				// забыть её в одном из пишущих — вопрос времени.
				if err := safety.CheckWrite(s.Name); err != nil {
					return "", err
				}
			}

			result, err := s.Do(ctx, args)
			if err != nil {
				if s.Hint != nil {
					if hint := s.Hint(args, err); hint != "" {
						return "", fmt.Errorf("%w\n\n%s", err, hint)
					}
				}
				return "", decorate(err)
			}

			if s.Text {
				text, _ := result.(string)
				return safety.TrimResponse(text), nil
			}
			return r.format(ctx, result)
		},
	})
}

// AddCustom регистрирует инструмент с собственным обработчиком — для
// тех случаев, где одной обёртки мало (состояние сервера,
// самодиагностика).
func (r *Registry) AddCustom(t mcp.Tool) {
	r.server.Register(t)
}

// prettyLimit — до какого размера ответ печатается с отступами.
//
// Отступы стоят дороже, чем кажется: на сотне записей о поверках
// они раздувают ответ больше чем вдвое, и ровно на этом он перестаёт
// доезжать до модели. Пока ответ короткий, читаемость важнее — на
// длинном она не стоит удвоенного счёта.
const prettyLimit = 8_000

// format приводит ответ к виду, удобному для чтения моделью, и
// обрезает по лимиту запроса.
func (r *Registry) format(ctx context.Context, v any) (string, error) {
	raw, err := json.Marshal(v)
	if err != nil {
		return "", fmt.Errorf("ответ не сериализуется: %w", err)
	}
	return r.safetyFor(ctx).TrimResponse(formatJSON(raw)), nil
}

// formatJSON печатает JSON: короткий — с отступами, длинный — плотно.
func formatJSON(raw json.RawMessage) string {
	body := string(raw)

	var compact bytes.Buffer
	if err := json.Compact(&compact, raw); err == nil {
		body = compact.String()
	}

	if len(body) <= prettyLimit {
		if pretty, err := json.MarshalIndent(json.RawMessage(raw), "", "  "); err == nil {
			body = string(pretty)
		}
	}

	return body
}

// decorate добавляет к отказу ФГИС человеческую подсказку.
func decorate(err error) error {
	if hint := arshin.Hint(err); hint != "" {
		return fmt.Errorf("%w\n\n%s", err, hint)
	}
	return err
}
