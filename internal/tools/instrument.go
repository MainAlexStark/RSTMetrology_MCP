package tools

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/MainAlexStark/rst-metrology-mcp/arshin"
)

// check_instrument отвечает на самый частый вопрос к реестру: «прибор
// с заводским номером N — поверен ли он и до какого числа».
//
// Сделать это одним search_verifications модель может, но ошибается
// в одних и тех же местах: ищет за текущий год и получает пусто;
// берёт первую строку, а не последнюю по дате; путает приборы разных
// типов с одинаковым заводским номером (у счётчиков воды и газа номера
// пересекаются постоянно); не замечает извещения о непригодности
// или аннулирования записи. Всё это собрано здесь.

// Вердикты check_instrument.
const (
	verdictValid        = "valid"         // пригоден, срок не истёк
	verdictExpired      = "expired"       // срок истёк
	verdictInapplicable = "inapplicable"  // последняя поверка — извещение о непригодности
	verdictCancelled    = "cancelled"     // последняя запись аннулирована
	verdictUnknownTerm  = "unknown_term"  // пригоден, но срок в реестре не указан
	verdictAmbiguous    = "ambiguous"     // под номером несколько типов СИ — уточните mit_number
	verdictNotFound     = "not_found"     // записей нет
	verdictIncomplete   = "not_found_yet" // записей не нашли, но перебор не закончен
)

type instrumentResult struct {
	Verdict     string   `json:"verdict"`
	Explanation string   `json:"explanation"`
	OnDate      string   `json:"on_date"`
	ValidUntil  string   `json:"valid_until,omitempty"`
	DaysLeft    *int     `json:"days_left,omitempty"`
	Latest      *latest  `json:"latest,omitempty"`
	History     []string `json:"history,omitempty"`

	// Types — по одной последней записи на каждый тип СИ с этим
	// заводским номером, когда тип не задан и их несколько.
	Types []arshin.VRIItem `json:"types,omitempty"`

	SearchedPeriod string `json:"searched_period"`
	Requests       int    `json:"requests_to_fgis"`
}

type latest struct {
	arshin.VRIItem
	CardURL string              `json:"card_url"`
	Card    *arshin.CardSummary `json:"card,omitempty"`
}

// RegisterInstrument добавляет check_instrument.
func (r *Registry) RegisterInstrument() {
	r.Add(Spec{
		Name: "check_instrument",
		Desc: "Проверить поверку конкретного прибора по заводскому номеру: находит последнюю запись в ФГИС «Аршин» " +
			"за последние годы и выносит вердикт на дату — valid (поверен, срок не истёк), expired, inapplicable " +
			"(признан непригодным), cancelled (запись аннулирована), unknown_term (срок в реестре не указан), " +
			"ambiguous (с этим номером несколько типов СИ — повторите с mit_number), not_found. " +
			"Учитывает модификацию и аннулирование записи. " +
			"Задавайте mit_number (номер типа в реестре), если он известен: заводские номера у разных типов пересекаются.",
		Schema: schema(obj{
			"mi_number":    str("Заводской номер прибора"),
			"mit_number":   str("Регистрационный номер типа СИ в реестре, например 16078-05. Сильно сужает поиск"),
			"mit_notation": str("Обозначение типа или модификации (часть строки), если номер типа неизвестен, например «СГВ-15»"),
			"on_date":      str("На какую дату проверить, YYYY-MM-DD (по умолчанию сегодня)"),
			"years_back":   num(fmt.Sprintf("Сколько лет истории просматривать (по умолчанию %d)", defaultYearsBack)),
		}, "mi_number"),
		Do: func(ctx context.Context, a Args) (any, error) {
			serial, err := a.Require("mi_number")
			if err != nil {
				return nil, err
			}
			onDate := r.now()
			if d, err := a.Date("on_date"); err != nil {
				return nil, err
			} else if d != "" {
				onDate, _ = time.Parse(time.DateOnly, d)
			}
			years := int(a.IntOr("years_back", defaultYearsBack))
			if years <= 0 || years > 30 {
				years = defaultYearsBack
			}
			return r.checkInstrument(ctx, instrumentQuery{
				serial:   serial,
				typeNum:  a.Str("mit_number"),
				notation: a.Str("mit_notation"),
				onDate:   onDate,
				years:    years,
			})
		},
	})
}

type instrumentQuery struct {
	serial, typeNum, notation string
	onDate                    time.Time
	years                     int
}

func (r *Registry) checkInstrument(ctx context.Context, q instrumentQuery) (*instrumentResult, error) {
	budget := r.safetyFor(ctx).MaxRequestsPerCall
	if budget <= 0 {
		budget = DefaultSafety().MaxRequestsPerCall
	}
	res := &instrumentResult{OnDate: q.onDate.Format(time.DateOnly)}

	from := q.onDate.AddDate(-q.years, 0, 0)
	base := arshin.VRIQuery{
		MINumber:  q.serial,
		MITNumber: q.typeNum,
		Rows:      arshin.MaxRows,
	}

	// Сначала одним запросом за весь период. Если ФГИС сочтёт его
	// слишком широким (400/408), — по годам от свежего к старому:
	// последняя поверка нужна чаще всего, и обычно находится в первом
	// же году с записями.
	items, complete, err := r.collect(ctx, base, from, q.onDate, &budget, res)
	if err != nil {
		return nil, err
	}
	res.SearchedPeriod = fmt.Sprintf("%s — %s", from.Format(time.DateOnly), res.OnDate)

	items = matching(items, q)
	if len(items) == 0 {
		if !complete {
			res.Verdict = verdictIncomplete
			res.Explanation = "Записей не найдено, но ФГИС не дала просмотреть весь период " +
				"(запрос признан широким, лимит запросов на вызов исчерпан). Задайте mit_number или уменьшите years_back."
			return res, nil
		}
		res.Verdict = verdictNotFound
		res.Explanation = "В ФГИС нет записей о поверке прибора с таким заводским номером за этот период. " +
			"Проверьте написание номера (буквы, нули в начале, дефисы) и номер типа; " +
			"поверки старше периода ищите с большим years_back."
		return res, nil
	}

	// По одной последней записи на тип.
	byType := map[string]arshin.VRIItem{}
	for _, it := range items {
		if cur, ok := byType[it.MITNumber]; !ok || it.VerificationDate > cur.VerificationDate {
			byType[it.MITNumber] = it
		}
	}
	if len(byType) > 1 && q.typeNum == "" {
		for _, it := range byType {
			res.Types = append(res.Types, it)
		}
		sort.Slice(res.Types, func(i, j int) bool {
			return res.Types[i].VerificationDate > res.Types[j].VerificationDate
		})
		res.Verdict = verdictAmbiguous
		res.Explanation = fmt.Sprintf("С заводским номером %s в реестре %d разных типов СИ. "+
			"Уточните, какой прибор имеется в виду, и повторите с mit_number из списка types.",
			q.serial, len(byType))
		return res, nil
	}

	sort.Slice(items, func(i, j int) bool { return items[i].VerificationDate > items[j].VerificationDate })
	for _, it := range items {
		res.History = append(res.History, fmt.Sprintf("%s %s %s", it.VerificationDate,
			map[bool]string{true: "пригоден", false: "непригоден"}[it.Applicability], it.VRIID))
	}
	last := &latest{VRIItem: items[0], CardURL: CardURL(items[0].VRIID)}
	res.Latest = last

	// Карточка — ради статуса публикации и срока, которого в списке
	// бывает нет. Без неё аннулированная запись выглядела бы годной.
	if budget > 0 {
		budget--
		res.Requests++
		if card, err := r.fgis.Verification(ctx, last.VRIID, true); err == nil {
			last.Card = &card.Summary
		}
	}

	r.verdict(res, last, q.onDate)
	return res, nil
}

// collect ищет записи за период, переходя на перебор по годам при
// отказе «слишком широко». Второе значение — просмотрен ли период целиком.
func (r *Registry) collect(ctx context.Context, base arshin.VRIQuery, from, to time.Time,
	budget *int, res *instrumentResult) ([]arshin.VRIItem, bool, error) {

	q := base
	q.DateFrom = from.Format(time.DateOnly)
	q.DateTo = to.Format(time.DateOnly)
	*budget--
	res.Requests++
	page, err := r.fgis.SearchVerifications(ctx, q)
	if err == nil {
		return page.Items, true, nil
	}
	if !arshin.IsKind(err, arshin.KindTooBroad) {
		return nil, false, err
	}

	var all []arshin.VRIItem
	for y := to.Year(); y >= from.Year(); y-- {
		if *budget <= 0 {
			return all, false, nil
		}
		q := base
		q.Year = y
		*budget--
		res.Requests++
		page, err := r.fgis.SearchVerifications(ctx, q)
		if arshin.IsKind(err, arshin.KindTooBroad) {
			continue
		}
		if err != nil {
			return nil, false, err
		}
		all = append(all, page.Items...)
		if len(matchingLoose(page.Items, base)) > 0 {
			// Самая свежая поверка нашлась — старше она уже не станет.
			return all, true, nil
		}
	}
	return all, true, nil
}

// matching оставляет записи именно этого прибора.
//
// ФГИС ищет по mi_number с подстановкой, и «12345» находит «012345»
// и «12345-1»; номер сравнивается после нормализации.
func matching(items []arshin.VRIItem, q instrumentQuery) []arshin.VRIItem {
	want := normSerial(q.serial)
	note := strings.ToLower(q.notation)
	var out []arshin.VRIItem
	for _, it := range items {
		if normSerial(it.MINumber) != want {
			continue
		}
		if q.typeNum != "" && it.MITNumber != q.typeNum {
			continue
		}
		if note != "" && !strings.Contains(strings.ToLower(it.MITNotation+" "+it.MIModification+" "+it.MITTitle), note) {
			continue
		}
		out = append(out, it)
	}
	return out
}

func matchingLoose(items []arshin.VRIItem, q arshin.VRIQuery) []arshin.VRIItem {
	return matching(items, instrumentQuery{serial: q.MINumber, typeNum: q.MITNumber})
}

// normSerial приводит заводской номер к сравнимому виду: регистр,
// пробелы, похожие кириллические и латинские буквы. В реестр номера
// вносят вручную, и «А123» кириллицей и «A123» латиницей — один прибор.
func normSerial(s string) string {
	s = strings.ToUpper(strings.Join(strings.Fields(s), ""))
	return strings.NewReplacer(
		"А", "A", "В", "B", "Е", "E", "К", "K", "М", "M", "Н", "H",
		"О", "O", "Р", "P", "С", "C", "Т", "T", "Х", "X", "У", "Y",
	).Replace(s)
}

// verdict выносит заключение по последней записи.
func (r *Registry) verdict(res *instrumentResult, last *latest, on time.Time) {
	applicable := last.Applicability
	validUntil := last.ValidDate
	if c := last.Card; c != nil {
		if c.Cancelled {
			res.Verdict = verdictCancelled
			res.Explanation = "Последняя запись о поверке аннулирована. Действующей поверки по ней нет; " +
				"посмотрите history — возможно, есть более ранняя или повторная поверка."
			return
		}
		if c.Applicable != nil {
			applicable = *c.Applicable
		}
		if c.ValidUntil != "" {
			validUntil = c.ValidUntil
		}
	}
	res.ValidUntil = validUntil

	if !applicable {
		res.Verdict = verdictInapplicable
		res.Explanation = fmt.Sprintf("Последняя поверка %s — извещение о непригодности. Прибор к применению не допущен.",
			last.VerificationDate)
		return
	}
	until, ok := arshin.ParseDate(validUntil)
	if !ok {
		res.Verdict = verdictUnknownTerm
		res.Explanation = "Прибор признан пригодным, но срок действия поверки в реестре не указан. " +
			"Срок определяется межповерочным интервалом из описания типа (см. get_type по mit_number)."
		return
	}
	days := int(until.Sub(on.Truncate(24*time.Hour)).Hours() / 24)
	res.DaysLeft = &days
	if on.After(until.Add(24*time.Hour - time.Nanosecond)) {
		res.Verdict = verdictExpired
		res.Explanation = fmt.Sprintf("Срок поверки истёк %s.", until.Format(time.DateOnly))
		return
	}
	res.Verdict = verdictValid
	res.Explanation = fmt.Sprintf("Поверка действует до %s включительно.", until.Format(time.DateOnly))
}
