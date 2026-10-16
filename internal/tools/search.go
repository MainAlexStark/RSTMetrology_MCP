package tools

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"unicode"
	"unicode/utf8"
)

// Поиск «как на сайте».
//
// Строковые фильтры ФГИС устроены неудобно для человека, который знает
// о приборе мало:
//   - слово совпадает только целиком: «Бетар» не находит «ООО ПКФ «БЕТАР»»,
//     «СГВ-15» не находит «СГВ-15Д»; по части слова ищет только шаблон «*бетар*»;
//   - несколько слов объединяются через ИЛИ, и внутри search, и внутри
//     одного поля: «*воды* *бетар*» — это 1664 типа обо всём на свете;
//     AND и «+» API не понимает.
//
// Поэтому запрос перестраивается. В ФГИС уходит одно слово на фильтр
// (по полному тексту — самое редкое, его сервер выясняет пробными
// запросами), обёрнутое в звёздочки. Остальные слова сервер проверяет
// сам в полученных записях — все сразу, по подстроке, без учёта регистра.
// Записей-кандидатов может быть больше, чем помещается в одну страницу,
// поэтому выдача просматривается страницами в пределах бюджета запросов.

const (
	// scanPages — сколько страниц по 100 кандидатов просмотреть за один
	// вызов. С паузой 600 мс между запросами это несколько секунд.
	scanPages = 5
	// probeWords — сколько слов полнотекстового запроса проверять
	// пробными запросами, выбирая самое редкое.
	probeWords = 3
)

// term — слово, которое должно найтись в записи.
type term struct {
	field string // имя поля; "" — любое поле
	word  string // без кавычек, может содержать * и ?
}

// searchPlan — перестроенный запрос.
type searchPlan struct {
	fields  map[string]string // поле ФГИС → слово-шаблон, уходит в запрос как есть
	search  string            // параметр search; при probe выбирается позже
	probe   []string          // кандидаты для search: выбрать самое редкое
	local   []term            // проверяется сервером в полученных записях
	allFree []string          // все слова полного текста — для локальной проверки
}

// fetchFunc — одна страница кандидатов. search — значение параметра
// search (поверх полей плана); строки — записи в виде «поле → текст».
type fetchFunc[T any] func(ctx context.Context, search string, start, rows int) (count int, items []T, err error)

// newPlan разбирает полнотекстовый запрос и значения полей.
func newPlan(free string, fields map[string]string) searchPlan {
	p := searchPlan{fields: map[string]string{}}

	names := make([]string, 0, len(fields))
	for k := range fields {
		names = append(names, k)
	}
	sort.Strings(names) // детерминированный порядок — ради тестов и кэша
	for _, f := range names {
		ws := words(fields[f])
		if len(ws) == 0 {
			continue
		}
		main := rarest(ws)
		p.fields[f] = pattern(main)
		for _, w := range ws {
			if w != main {
				p.local = append(p.local, term{field: f, word: w})
			}
		}
	}

	ws := words(free)
	p.allFree = ws
	switch len(ws) {
	case 0:
	case 1:
		p.search = pattern(ws[0])
	default:
		p.probe = byLength(ws)
		if len(p.probe) > probeWords {
			p.probe = p.probe[:probeWords]
		}
		p.search = pattern(p.probe[0])
	}
	return p
}

// remote — параметры, уходящие в ФГИС (для пояснения в ответе).
func (p searchPlan) remote() map[string]string {
	out := map[string]string{}
	for k, v := range p.fields {
		out[k] = v
	}
	if p.search != "" {
		out["search"] = p.search
	}
	return out
}

// scanResult — итог просмотра.
type scanResult[T any] struct {
	Items      []T
	Candidates int  // сколько записей нашла ФГИС по отправленным словам
	Scanned    int  // сколько из них просмотрено
	NextStart  int  // откуда продолжить; -1 — дальше ничего нет
	Filtered   bool // проверялись ли слова на стороне сервера
	Search     string
}

// run выполняет план: выбирает слово для search, запрашивает страницы
// и оставляет записи, где нашлись все слова.
func run[T any](ctx context.Context, p searchPlan, start, want int,
	fetch fetchFunc[T], text func(T) map[string]string) (scanResult[T], error) {

	res := scanResult[T]{NextStart: -1}

	// Выбор самого редкого слова. Нулевой результат по любому слову
	// означает пустой ответ на весь запрос: слова объединены через И.
	if len(p.probe) > 1 {
		best, bestCount := "", -1
		for _, w := range p.probe {
			n, _, err := fetch(ctx, pattern(w), 0, 1)
			if err != nil {
				return res, err
			}
			if n == 0 {
				res.Search = pattern(w)
				return res, nil
			}
			if bestCount < 0 || n < bestCount {
				best, bestCount = w, n
			}
		}
		p.search = pattern(best)
	}
	res.Search = p.search

	// Слова полного текста, кроме отправленного, — локально по всем полям.
	local := append([]term(nil), p.local...)
	for _, w := range p.allFree {
		if pattern(w) != p.search {
			local = append(local, term{word: w})
		}
	}

	if len(local) == 0 {
		// Всё проверила ФГИС — обычная постраничная выдача.
		n, items, err := fetch(ctx, p.search, start, want)
		if err != nil {
			return res, err
		}
		res.Items, res.Candidates, res.Scanned = items, n, len(items)
		if next := start + len(items); len(items) > 0 && next < n {
			res.NextStart = next
		}
		return res, nil
	}

	res.Filtered = true
	offset := start
	for page := 0; page < scanPages; page++ {
		n, items, err := fetch(ctx, p.search, offset, 100)
		if err != nil {
			return res, err
		}
		res.Candidates = n
		for i, it := range items {
			res.Scanned++
			if matches(text(it), local) {
				res.Items = append(res.Items, it)
				if len(res.Items) == want {
					if next := offset + i + 1; next < n {
						res.NextStart = next
					}
					return res, nil
				}
			}
		}
		offset += len(items)
		if len(items) == 0 || offset >= n {
			return res, nil
		}
	}
	res.NextStart = offset
	return res, nil
}

// matches — все слова нашлись в своих полях.
func matches(rec map[string]string, ts []term) bool {
	var all string
	for _, t := range ts {
		var hay string
		if t.field != "" {
			hay = fold(rec[t.field])
		} else {
			if all == "" {
				var b strings.Builder
				for _, v := range rec {
					b.WriteString(v)
					b.WriteByte('\n')
				}
				all = fold(b.String())
			}
			hay = all
		}
		if !wildMatch(hay, fold(t.word)) {
			return false
		}
	}
	return true
}

// wildMatch — шаблон с * и ? встречается в тексте как подстрока.
// ? трактуется как *: точность до символа здесь не нужна, ФГИС уже
// отобрала записи по своей части запроса.
func wildMatch(hay, pat string) bool {
	pat = strings.ReplaceAll(pat, "?", "*")
	for _, part := range strings.Split(pat, "*") {
		if part == "" {
			continue
		}
		i := strings.Index(hay, part)
		if i < 0 {
			return false
		}
		hay = hay[i+len(part):]
	}
	return true
}

// fold приводит текст к виду для сравнения: регистр, ё, кавычки.
func fold(s string) string {
	s = strings.ToLower(s)
	return strings.NewReplacer("ё", "е", "«", "\"", "»", "\"", "„", "\"", "“", "\"", "”", "\"").Replace(s)
}

// separators — символы, на которых запрос режется на слова. Это знаки
// синтаксиса поисковика ФГИС и кавычки: оставленные в слове, они либо
// ломают запрос, либо мешают совпадению («Бетар» против «БЕТАР»»).
// Дефис, точка и запятая внутри слова сохраняются: «СГВ-15», «1,6», «4D-РМ».
const separators = "\"«»„“”'`()[]{}<>:;!^~\\/|+&="

// words режет строку на слова. Однобуквенные слова («и», «в») отброшены:
// как шаблон «*и*» они совпадают со всем подряд.
func words(s string) []string {
	f := func(r rune) bool { return unicode.IsSpace(r) || strings.ContainsRune(separators, r) }
	var out []string
	seen := map[string]bool{}
	for _, w := range strings.FieldsFunc(s, f) {
		w = strings.Trim(w, ",.-")
		if w == "" || strings.Trim(w, "*?") == "" {
			continue
		}
		k := fold(w)
		if seen[k] {
			continue
		}
		seen[k] = true
		out = append(out, w)
	}
	if len(out) > 1 {
		long := out[:0:0]
		for _, w := range out {
			if utf8.RuneCountInString(strings.Trim(w, "*?")) > 1 {
				long = append(long, w)
			}
		}
		if len(long) > 0 {
			out = long
		}
	}
	return out
}

// pattern — слово в виде шаблона ФГИС. Слово со своими * или ? человек
// составил сам — оставить как есть.
func pattern(w string) string {
	if strings.ContainsAny(w, "*?") {
		return w
	}
	return "*" + w + "*"
}

// byLength — слова от самого «редкого» к самому частому на глаз:
// длиннее и с цифрами (номера, обозначения) — раньше.
func byLength(ws []string) []string {
	out := append([]string(nil), ws...)
	score := func(w string) int {
		n := utf8.RuneCountInString(strings.Trim(w, "*?")) * 2
		if strings.IndexFunc(w, unicode.IsDigit) >= 0 {
			n += 3
		}
		return n
	}
	sort.SliceStable(out, func(i, j int) bool { return score(out[i]) > score(out[j]) })
	return out
}

// rarest — слово для отправки в поле без пробных запросов.
func rarest(ws []string) string { return byLength(ws)[0] }

// planNote объясняет модели, что сделано с запросом.
func planNote[T any](p searchPlan, r scanResult[T]) string {
	if !r.Filtered {
		return ""
	}
	note := fmt.Sprintf("ФГИС объединяет слова через ИЛИ, поэтому в неё ушло по одному слову на фильтр (%s), "+
		"а остальные слова сервер проверил сам. Просмотрено кандидатов: %d из %d.",
		describe(p, r.Search), r.Scanned, r.Candidates)
	if r.NextStart >= 0 {
		note += fmt.Sprintf(" Есть ещё — продолжите с start=%d.", r.NextStart)
	}
	return note
}

func describe(p searchPlan, search string) string {
	var parts []string
	if search != "" {
		parts = append(parts, "search="+search)
	}
	names := make([]string, 0, len(p.fields))
	for k := range p.fields {
		names = append(names, k)
	}
	sort.Strings(names)
	for _, k := range names {
		parts = append(parts, k+"="+p.fields[k])
	}
	return strings.Join(parts, ", ")
}
