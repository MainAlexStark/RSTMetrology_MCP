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
	// probeWords — сколько слов одного фильтра проверять пробными
	// запросами, выбирая самое редкое.
	probeWords = 3
)

// searchParam — параметр полнотекстового поиска. Его слова проверяются
// по всем полям записи, слова остальных параметров — в одноимённом поле.
const searchParam = "search"

// term — слово, которое должно найтись в записи.
type term struct {
	field string // имя поля; "" — любое поле
	word  string // без кавычек, может содержать * и ?
}

// searchPlan — перестроенный запрос.
type searchPlan struct {
	words map[string][]string // параметр ФГИС → слова, от вероятно редкого к частому
	sent  map[string]string   // параметр → шаблон, уходящий в ФГИС
}

// fetchFunc — одна страница кандидатов по параметрам sent.
type fetchFunc[T any] func(ctx context.Context, sent map[string]string, start, rows int) (count int, items []T, err error)

// newPlan разбирает полнотекстовый запрос и значения полей.
func newPlan(free string, fields map[string]string) searchPlan {
	p := searchPlan{words: map[string][]string{}, sent: map[string]string{}}
	all := map[string]string{searchParam: free}
	for k, v := range fields {
		all[k] = v
	}
	for k, v := range all {
		if ws := words(v); len(ws) > 0 {
			p.words[k] = byLength(ws)
			p.sent[k] = pattern(p.words[k][0])
		}
	}
	return p
}

// params — параметры плана в постоянном порядке: от него зависят
// пробы, а значит и то, какие слова уйдут в ФГИС на следующей странице.
func (p searchPlan) params() []string {
	names := make([]string, 0, len(p.words))
	for k := range p.words {
		names = append(names, k)
	}
	sort.Strings(names)
	return names
}

// empty — в плане нет ни одного слова.
func (p searchPlan) empty() bool { return len(p.words) == 0 }

// scanResult — итог просмотра.
type scanResult[T any] struct {
	Items      []T
	Candidates int               // сколько записей нашла ФГИС по отправленным словам
	Scanned    int               // сколько из них просмотрено
	NextStart  int               // откуда продолжить; -1 — дальше ничего нет
	Filtered   bool              // проверялись ли слова на стороне сервера
	Sent       map[string]string // что ушло в ФГИС
}

// run выполняет план: выбирает для каждого параметра самое редкое слово,
// запрашивает страницы и оставляет записи, где нашлись все слова.
func run[T any](ctx context.Context, p searchPlan, start, want int,
	fetch fetchFunc[T], text func(T) map[string]string) (scanResult[T], error) {

	sent := map[string]string{}
	for k, v := range p.sent {
		sent[k] = v
	}
	res := scanResult[T]{NextStart: -1, Sent: sent}

	// Пробы: по одной записи на слово, остальные параметры — как есть.
	// Слова одного параметра объединены через И, поэтому ноль по любому
	// из них — пустой ответ на весь запрос.
	for _, k := range p.params() {
		ws := p.words[k]
		if len(ws) < 2 {
			continue
		}
		best, bestCount := "", -1
		for _, w := range ws[:min(len(ws), probeWords)] {
			sent[k] = pattern(w)
			n, _, err := fetch(ctx, sent, 0, 1)
			if err != nil {
				return res, err
			}
			if n == 0 {
				return res, nil
			}
			if bestCount < 0 || n < bestCount {
				best, bestCount = w, n
			}
		}
		sent[k] = pattern(best)
	}

	// Остальные слова проверяются на стороне сервера.
	var local []term
	for _, k := range p.params() {
		field := k
		if k == searchParam {
			field = ""
		}
		for _, w := range p.words[k] {
			if pattern(w) != sent[k] {
				local = append(local, term{field: field, word: w})
			}
		}
	}

	if len(local) == 0 {
		// Всё проверила ФГИС — обычная постраничная выдача.
		n, items, err := fetch(ctx, sent, start, want)
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
		n, items, err := fetch(ctx, sent, offset, 100)
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

// planNote объясняет модели, что сделано с запросом.
func planNote[T any](r scanResult[T]) string {
	if !r.Filtered {
		return ""
	}
	note := fmt.Sprintf("ФГИС объединяет слова через ИЛИ, поэтому в неё ушло по одному, самому редкому слову на фильтр (%s), "+
		"а остальные слова сервер проверил сам. Просмотрено кандидатов: %d из %d.",
		describe(r.Sent), r.Scanned, r.Candidates)
	if r.NextStart >= 0 {
		note += fmt.Sprintf(" Есть ещё — продолжите с start=%d.", r.NextStart)
	}
	return note
}

func describe(sent map[string]string) string {
	names := make([]string, 0, len(sent))
	for k := range sent {
		names = append(names, k)
	}
	sort.Strings(names)
	parts := make([]string, 0, len(names))
	for _, k := range names {
		parts = append(parts, k+"="+sent[k])
	}
	return strings.Join(parts, ", ")
}
