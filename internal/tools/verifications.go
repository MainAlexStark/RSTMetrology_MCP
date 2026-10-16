package tools

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/MainAlexStark/rst-metrology-mcp/arshin"
)

// CardURL — карточка поверки на сайте ФГИС, для человека.
func CardURL(vriID string) string {
	return "https://fgis.gost.ru/fundmetrology/cm/results/" + vriID
}

// searchResult — ответ search_verifications.
type searchResult struct {
	Count      *int              `json:"count,omitempty"`
	Candidates int               `json:"candidates"`
	Sent       map[string]string `json:"sent_to_fgis,omitempty"`
	Start      int               `json:"start"`
	Returned   int               `json:"returned"`
	NextStart  *int              `json:"next_start,omitempty"`
	Period     string            `json:"period"`
	Items      []arshin.VRIItem  `json:"items"`
	Note       string            `json:"_note,omitempty"`
}

// RegisterVerifications добавляет инструменты поверок.
func (r *Registry) RegisterVerifications() {
	r.Add(Spec{
		Name: "search_verifications",
		Desc: "Поиск сведений о результатах поверки СИ во ФГИС «Аршин» (реестр Росстандарта). " +
			"Возвращает строки: vri_id, поверитель, номер типа в реестре (mit_number), наименование и обозначение типа, " +
			"заводской номер (mi_number), дата поверки, срок действия (valid_date), номер свидетельства/извещения, " +
			"пригодность (applicability). " +
			"Важно: без year и date_from ФГИС ищет только в текущем году — для истории прибора задавайте период " +
			"(при заданном mi_number сервер сам берёт последние 10 лет). " +
			"Для вопроса «действует ли поверка прибора N» удобнее check_instrument, для подробностей — get_verification.",
		Schema: schema(obj{
			"search":          str("Поиск по всем полям записи: любые слова в любом порядке, по части слова, без учёта регистра; найдутся записи, где есть все слова. Например «бетар сгв 2015», «единица манометр». * и ? — шаблон"),
			"mit_number":      str("Регистрационный номер типа СИ в реестре, например 16078-05"),
			"mit_title":       str("Наименование типа СИ или его слова, по части слова: «счетчики воды», «манометр»"),
			"mit_notation":    str("Обозначение типа СИ, по части: «СГВ-15», «ВКТ»"),
			"mi_modification": str("Модификация СИ, по части"),
			"mi_number":       str("Заводской номер экземпляра — точно; для части номера используйте * («*4860*») или search"),
			"org_title":       str("Организация-поверитель, любые слова названия: «единица измерения», «кировский цсм»"),
			"year":            num("Год поверки"),
			"date_from":       str("Поверки начиная с даты, YYYY-MM-DD"),
			"date_to":         str("Поверки по дату включительно, YYYY-MM-DD"),
			"applicability":   enum("Фильтр по заключению: any (по умолчанию), applicable — пригодно, inapplicable — непригодно", "any", "applicable", "inapplicable"),
			"sort":            str("Сортировка как в API ФГИС, например «verification_date desc»"),
			"rows":            rowsField(defaultRows),
			"start":           startField(),
			"filters":         filtersField(),
		}),
		Do: func(ctx context.Context, a Args) (any, error) {
			q, period, note, err := r.vriQuery(a)
			if err != nil {
				return nil, err
			}
			plan := newPlan(q.Search, map[string]string{
				"mit_title": q.MITTitle, "mit_notation": q.MITNotation,
				"org_title": q.OrgTitle, "mi_modification": q.MIModification,
			})
			fetch := func(ctx context.Context, sent map[string]string, start, rows int) (int, []arshin.VRIItem, error) {
				qq := q
				qq.Search, qq.Start, qq.Rows = sent[searchParam], start, rows
				qq.MITTitle, qq.MITNotation = sent["mit_title"], sent["mit_notation"]
				qq.OrgTitle, qq.MIModification = sent["org_title"], sent["mi_modification"]
				page, err := r.fgis.SearchVerifications(ctx, qq)
				if err != nil {
					return 0, nil, err
				}
				return page.Count, page.Items, nil
			}
			res, err := run(ctx, plan, q.Start, q.Rows, fetch, vriText)
			if err != nil {
				return nil, err
			}
			out := searchResult{
				Candidates: res.Candidates, Start: q.Start, Returned: len(res.Items),
				Period: period, Items: res.Items, Note: joinNotes(note, planNote(res)),
			}
			if len(res.Sent) > 0 {
				out.Sent = res.Sent
			}
			switch {
			case !res.Filtered:
				out.Count = &res.Candidates
			case res.NextStart < 0 && q.Start == 0:
				n := len(res.Items)
				out.Count = &n
			}
			if res.NextStart >= 0 {
				out.NextStart = &res.NextStart
			}
			if out.Items == nil {
				out.Items = []arshin.VRIItem{}
			}
			return out, nil
		},
	})

	r.Add(Spec{
		Name: "get_verification",
		Desc: "Карточка одной поверки по vri_id (вид «1-340080782»; принимается и ссылка на карточку на сайте ФГИС). " +
			"Отдаёт сводку: тип и модификация СИ, заводской номер, год выпуска, поверитель, владелец, дата, " +
			"срок действия, методика поверки, заключение (applicable: true — свидетельство, false — извещение о непригодности), " +
			"статус публикации (модифицирована/аннулирована). " +
			"Модифицированная запись по умолчанию заменяется действующей версией (follow_versions). " +
			"include_raw добавляет исходный документ ФГИС — нужен для эталонов и средств поверки, которых нет в сводке.",
		Schema: schema(obj{
			"vri_id":          str("Идентификатор записи о поверке из search_verifications (поле vri_id) или ссылка на карточку"),
			"follow_versions": boolean("Перейти к действующей версии, если запись модифицирована (по умолчанию да)"),
			"include_raw":     boolean("Добавить исходный документ ФГИС целиком (по умолчанию нет)"),
		}, "vri_id"),
		Do: func(ctx context.Context, a Args) (any, error) {
			id, err := a.Require("vri_id")
			if err != nil {
				return nil, err
			}
			id = lastSegment(id)
			follow := true
			if _, set := a["follow_versions"]; set {
				follow = a.Bool("follow_versions")
			}
			card, err := r.fgis.Verification(ctx, id, follow)
			if err != nil {
				return nil, err
			}

			out := map[string]any{
				"summary":  card.Summary,
				"card_url": CardURL(card.Summary.VRIID),
			}
			if valid, known := card.Summary.ValidOn(r.now()); known {
				out["valid_today"] = valid
			}
			if len(card.Chain) > 0 {
				out["versions"] = card.Chain
			}
			if a.Bool("include_raw") {
				out["raw"] = card.Raw
			}
			return out, nil
		},
	})
}

// vriQuery собирает фильтры поиска из аргументов.
//
// Второе и третье значения — описание периода и пояснение для модели:
// поведение ФГИС по умолчанию («только текущий год») неочевидно,
// и без явной отметки пустой ответ читается как «записей нет вообще».
func (r *Registry) vriQuery(a Args) (arshin.VRIQuery, string, string, error) {
	q := arshin.VRIQuery{
		Search:         a.Str("search"),
		MITNumber:      a.Str("mit_number"),
		MITTitle:       a.Str("mit_title"),
		MITNotation:    a.Str("mit_notation"),
		MIModification: a.Str("mi_modification"),
		MINumber:       a.Str("mi_number"),
		OrgTitle:       a.Str("org_title"),
		Year:           int(a.IntOr("year", 0)),
		Sort:           a.Str("sort"),
		Rows:           rows(a, defaultRows),
		Start:          int(max(a.IntOr("start", 0), 0)),
		Extra:          a.StrMap("filters"),
	}

	var err error
	if q.DateFrom, err = a.Date("date_from"); err != nil {
		return q, "", "", err
	}
	if q.DateTo, err = a.Date("date_to"); err != nil {
		return q, "", "", err
	}

	switch a.Str("applicability") {
	case "", "any":
	case "applicable":
		yes := true
		q.Applicable = &yes
	case "inapplicable":
		no := false
		q.Applicable = &no
	default:
		return q, "", "", fmt.Errorf("applicability: допустимы any, applicable, inapplicable")
	}

	if q.Search == "" && q.MITNumber == "" && q.MITTitle == "" && q.MITNotation == "" &&
		q.MINumber == "" && q.OrgTitle == "" && q.MIModification == "" && len(q.Extra) == 0 {
		return q, "", "", fmt.Errorf("задайте хотя бы один фильтр: mi_number, mit_number, " +
			"mit_notation, org_title или search — без фильтра ФГИС отклонит запрос как слишком широкий")
	}

	note := ""
	switch {
	case q.Year > 0:
		return q, fmt.Sprintf("%d год", q.Year), "", nil
	case q.DateFrom != "":
	case q.MINumber != "":
		// Один экземпляр — выборка узкая, и десять лет истории ФГИС
		// отдаёт без отказа. Без этого первый же вопрос «когда
		// поверяли счётчик» получает пустой ответ за текущий год.
		q.DateFrom = r.now().AddDate(-defaultYearsBack, 0, 0).Format(time.DateOnly)
		note = fmt.Sprintf("период не задан — для заводского номера взяты последние %d лет", defaultYearsBack)
	default:
		return q, "текущий год", "период не задан, а ФГИС в таком случае ищет только в текущем году. " +
			"Для других лет задайте year или date_from", nil
	}

	period := "с " + q.DateFrom
	if q.DateTo != "" {
		period += " по " + q.DateTo
	}
	return q, period, note, nil
}

// vriText — поля строки поверки для проверки слов запроса.
func vriText(it arshin.VRIItem) map[string]string {
	return map[string]string{
		"org_title": it.OrgTitle, "mit_number": it.MITNumber, "mit_title": it.MITTitle,
		"mit_notation": it.MITNotation, "mi_modification": it.MIModification,
		"mi_number": it.MINumber, "result_docnum": it.ResultDocnum,
	}
}

// joinNotes склеивает пояснения, пропуская пустые.
func joinNotes(ns ...string) string {
	var out []string
	for _, n := range ns {
		if n != "" {
			out = append(out, n)
		}
	}
	return strings.Join(out, " ")
}

// lastSegment достаёт идентификатор из ссылки на карточку:
// модель часто присылает ссылку целиком, раз видела её в ответе.
func lastSegment(s string) string {
	s = strings.TrimRight(strings.TrimSpace(s), "/")
	if i := strings.Index(s, "?"); i >= 0 {
		s = s[:i]
	}
	if strings.Contains(s, "/") {
		s = s[strings.LastIndex(s, "/")+1:]
	}
	return s
}
