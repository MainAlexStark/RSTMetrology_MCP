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
		Desc: "Поиск в реестре утверждённых типов средств измерений (ФГИС «Аршин», раздел «Утверждённые типы СИ»). " +
			"search ищет по всем полям сразу — номер, наименование, обозначение, изготовитель с адресом — " +
			"любые слова в любом порядке, по части слова, без учёта регистра: «бетар сгв», «счетчики воды чистополь». " +
			"Пригодится, чтобы узнать номер типа (mit_number) для check_instrument и search_verifications; " +
			"срок действия свидетельства, межповерочный интервал и методика поверки — в get_type.",
		Schema: schema(obj{
			"search":        str("Что искать по всем полям: номер в реестре («16078-05»), слова наименования, обозначение («СГВ-15»), изготовитель, город"),
			"title":         str("Только в наименовании типа, любые слова: «счетчики воды»"),
			"notation":      str("Только в обозначении типа: «СГВ», «ВКТ-7»"),
			"manufacturers": str("Только в изготовителе (название, страна, адрес): «бетар», «чистополь»"),
			"rows":          rowsField(defaultRows),
			"start":         startField(),
			"filters":       filtersField(),
		}),
		Do: func(ctx context.Context, a Args) (any, error) {
			q := arshin.MITQuery{
				Rows:  rows(a, defaultRows),
				Start: int(max(a.IntOr("start", 0), 0)),
				Extra: a.StrMap("filters"),
			}
			plan := newPlan(a.Str("search"), map[string]string{
				"title": a.Str("title"), "notation": a.Str("notation"), "manufacturers": a.Str("manufacturers"),
			})
			if plan.search == "" && len(plan.fields) == 0 && len(q.Extra) == 0 {
				return nil, fmt.Errorf("задайте search — номер, слова наименования, обозначение или изготовителя")
			}
			fetch := func(ctx context.Context, search string, start, rows int) (int, []map[string]any, error) {
				qq := q
				qq.Search, qq.Start, qq.Rows = search, start, rows
				qq.Title, qq.Notation, qq.Manufacturers = plan.fields["title"], plan.fields["notation"], plan.fields["manufacturers"]
				page, err := r.fgis.SearchTypes(ctx, qq)
				if err != nil {
					return 0, nil, err
				}
				return page.Count, page.Items, nil
			}
			res, err := run(ctx, plan, q.Start, q.Rows, fetch, rawText)
			if err != nil {
				return nil, err
			}
			out := map[string]any{
				"candidates": res.Candidates,
				"start":      q.Start,
				"returned":   len(res.Items),
				"items":      nonNil(res.Items),
			}
			plan.search = res.Search
			if sent := plan.remote(); len(sent) > 0 {
				out["sent_to_fgis"] = sent
			}
			switch {
			case !res.Filtered:
				out["count"] = res.Candidates
			case res.NextStart < 0 && q.Start == 0:
				out["count"] = len(res.Items)
			}
			if res.NextStart >= 0 {
				out["next_start"] = res.NextStart
			}
			if n := planNote(plan, res); n != "" {
				out["_note"] = n
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

// rawText — строковые поля записи реестра типов.
func rawText(it map[string]any) map[string]string {
	out := make(map[string]string, len(it))
	for k, v := range it {
		switch x := v.(type) {
		case string:
			out[k] = x
		case []any:
			var parts []string
			for _, e := range x {
				if s, ok := e.(string); ok {
					parts = append(parts, s)
				}
			}
			out[k] = strings.Join(parts, "\n")
		}
	}
	return out
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
