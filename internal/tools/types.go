package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"

	"github.com/MainAlexStark/rst-metrology-mcp/arshin"
)

// Реестр утверждённых типов СИ.
//
// Набор полей карточки типа описан в документации ФГИС хуже, чем
// у поверок, поэтому здесь ответы отдаются как пришли, без сводки:
// строгая структура молча теряла бы поля, а модель читает JSON
// и без неё.

// regNumber — регистрационный номер типа: «16078-05», «93332-24».
var regNumber = regexp.MustCompile(`^\d{1,6}-\d{2}$`)

// RegisterTypes добавляет инструменты реестра типов.
func (r *Registry) RegisterTypes() {
	r.Add(Spec{
		Name: "search_types",
		Desc: "Поиск в реестре утверждённых типов средств измерений (ФГИС «Аршин», раздел «Утверждённые типы СИ»): " +
			"по регистрационному номеру, наименованию, обозначению или изготовителю. " +
			"Пригодится, чтобы узнать номер типа (mit_number) для check_instrument и search_verifications, " +
			"срок действия свидетельства об утверждении типа, межповерочный интервал и методику поверки (они в get_type).",
		Schema: schema(obj{
			"search":  str("Что искать: номер в реестре («16078-05»), наименование («счетчики воды»), обозначение («СГВ-15») или изготовитель"),
			"rows":    rowsField(defaultRows),
			"start":   startField(),
			"filters": filtersField(),
		}),
		Do: func(ctx context.Context, a Args) (any, error) {
			q := arshin.MITQuery{
				Search: a.Str("search"),
				Rows:   rows(a, defaultRows),
				Start:  int(max(a.IntOr("start", 0), 0)),
				Extra:  a.StrMap("filters"),
			}
			if q.Search == "" && len(q.Extra) == 0 {
				return nil, fmt.Errorf("задайте search — номер, наименование или обозначение типа")
			}
			page, err := r.fgis.SearchTypes(ctx, q)
			if err != nil {
				return nil, err
			}
			out := map[string]any{
				"count":    page.Count,
				"start":    q.Start,
				"returned": len(page.Items),
				"items":    nonNil(page.Items),
			}
			if next := q.Start + len(page.Items); len(page.Items) > 0 && next < page.Count {
				out["next_start"] = next
			}
			return out, nil
		},
	})

	r.Add(Spec{
		Name: "get_type",
		Desc: "Карточка утверждённого типа СИ: наименование, обозначения, изготовитель, срок действия свидетельства, " +
			"межповерочный интервал, методика поверки, ссылки на описание типа. " +
			"Принимает mit_uuid из search_types, ссылку mit_url из карточки поверки или регистрационный номер («16078-05») — " +
			"по номеру сервер сначала найдёт тип поиском.",
		Schema: schema(obj{
			"id": str("mit_uuid, ссылка на карточку типа или регистрационный номер в реестре"),
		}, "id"),
		Do: func(ctx context.Context, a Args) (any, error) {
			id, err := a.Require("id")
			if err != nil {
				return nil, err
			}
			id = lastSegment(id)
			if !regNumber.MatchString(id) {
				raw, err := r.fgis.Type(ctx, id)
				if err != nil {
					return nil, err
				}
				return map[string]any{"mit_uuid": id, "card": raw}, nil
			}

			// По номеру: найти запись поиском, взять у неё идентификатор.
			page, err := r.fgis.SearchTypes(ctx, arshin.MITQuery{Search: id, Rows: 10})
			if err != nil {
				return nil, err
			}
			item := exactType(page.Items, id)
			if item == nil {
				return map[string]any{
					"_note": fmt.Sprintf("тип с номером %s поиском не найден точно; ниже — что нашлось", id),
					"items": nonNil(page.Items),
				}, nil
			}
			uuid := typeUUID(item)
			if uuid == "" {
				// Идентификатор карточки в строке поиска не опознан —
				// отдаём саму строку: в ней уже большая часть сведений.
				return map[string]any{"item": item}, nil
			}
			raw, err := r.fgis.Type(ctx, uuid)
			if err != nil {
				return nil, err
			}
			return map[string]any{"mit_uuid": uuid, "item": item, "card": json.RawMessage(raw)}, nil
		},
	})
}

// exactType находит в выдаче строку, где какое-либо поле точно равно
// регистрационному номеру. Имя поля не зашито: в разных версиях API
// оно называлось по-разному.
func exactType(items []map[string]any, number string) map[string]any {
	for _, it := range items {
		for _, v := range it {
			if s, ok := v.(string); ok && strings.TrimSpace(s) == number {
				return it
			}
		}
	}
	return nil
}

// typeUUID достаёт идентификатор карточки типа из строки поиска.
func typeUUID(item map[string]any) string {
	for _, k := range []string{"mit_uuid", "uuid", "mit_id", "id"} {
		if s, ok := item[k].(string); ok && s != "" {
			return s
		}
	}
	for k, v := range item {
		if s, ok := v.(string); ok && s != "" && strings.Contains(strings.ToLower(k), "uuid") {
			return s
		}
	}
	return ""
}

func nonNil(items []map[string]any) []map[string]any {
	if items == nil {
		return []map[string]any{}
	}
	return items
}
