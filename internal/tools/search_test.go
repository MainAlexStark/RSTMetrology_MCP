package tools

import (
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/MainAlexStark/rst-metrology-mcp/arshin"
)

func TestWords(t *testing.T) {
	cases := map[string][]string{
		"Бетар": {"Бетар"},
		`ООО ПКФ «БЕТАР», г. Чистополь`: {"ООО", "ПКФ", "БЕТАР", "Чистополь"},
		"счетчики холодной и горячей":   {"счетчики", "холодной", "горячей"},
		"СГВ-15 1,6 тер.4860/мод.0318":  {"СГВ-15", "1,6", "тер.4860", "мод.0318"},
		"бетар БЕТАР":                   {"бетар"},
		"и":                             {"и"},
		"  ":                            nil,
		"(manufacturer:xy) +z":          {"manufacturer", "xy"},
	}
	for in, want := range cases {
		if got := words(in); !reflect.DeepEqual(got, want) {
			t.Errorf("words(%q) = %q, ждали %q", in, got, want)
		}
	}
}

func TestPatternAndMatch(t *testing.T) {
	if pattern("бетар") != "*бетар*" || pattern("655*") != "655*" {
		t.Error("шаблон")
	}
	hay := fold(`Общество «БЕТАР» г. Чистополь, ООО "ЁЛКА"`)
	for _, w := range []string{"бетар", "БЕТАР", "чистоп", "елка", "общ*чист"} {
		if !wildMatch(hay, fold(w)) {
			t.Errorf("%q не нашлось", w)
		}
	}
	if wildMatch(hay, "чист*общ") {
		t.Error("порядок частей шаблона не учтён")
	}
}

// typesDB — маленький реестр, по которому подставная ФГИС ищет так же,
// как настоящая: целые слова, шаблоны *…*, слова через ИЛИ.
var typesDB = []map[string]any{
	{"mit_uuid": "1", "number": "16078-05", "title": "Счетчики холодной и горячей воды", "notation": "СХВ, СГВ-15", "manufacturers": "ООО ПКФ \"Бетар\", РОССИЯ, г.Чистополь"},
	{"mit_uuid": "2", "number": "87431-22", "title": "Счетчики воды универсальные", "notation": "СГВ", "manufacturers": "ООО ПКФ «БЕТАР», г. Чистополь"},
	{"mit_uuid": "3", "number": "98128-26", "title": "Счетчики газа бытовые", "notation": "СГК-1,6", "manufacturers": "ООО ПКФ «БЕТАР», г. Чистополь"},
	{"mit_uuid": "4", "number": "97595-26", "title": "Счетчики воды", "notation": "КУБ", "manufacturers": "ООО «Жилищник», Москва"},
	{"mit_uuid": "5", "number": "99372-26", "title": "Анализаторы паров воды", "notation": "Puren-T H2O", "manufacturers": "Inner Mongolia Photonics, КИТАЙ"},
}

// fgisMatch — совпадение по правилам ФГИС: любое слово запроса (ИЛИ),
// каждое — шаблоном по подстроке.
func fgisMatch(val, q string) bool {
	for _, w := range strings.Fields(q) {
		if wildMatch(fold(val), fold(w)) && (strings.Contains(w, "*") || strings.Contains(" "+fold(val)+" ", " "+fold(w)+" ")) {
			return true
		}
	}
	return false
}

func fakeTypes(q arshin.MITQuery) *arshin.RawPage {
	var hit []map[string]any
	for _, rec := range typesDB {
		txt := rawText(rec)
		ok := true
		if q.Search != "" {
			any := false
			for _, v := range txt {
				any = any || fgisMatch(v, q.Search)
			}
			ok = any
		}
		for f, v := range map[string]string{"title": q.Title, "notation": q.Notation, "manufacturers": q.Manufacturers} {
			if v != "" && !fgisMatch(txt[f], v) {
				ok = false
			}
		}
		if ok {
			hit = append(hit, rec)
		}
	}
	page := &arshin.RawPage{Count: len(hit)}
	end := min(q.Start+q.Rows, len(hit))
	if q.Start < end {
		page.Items = hit[q.Start:end]
	}
	return page
}

func uuids(t *testing.T, text string) []string {
	t.Helper()
	var out struct {
		Items []map[string]any `json:"items"`
	}
	if err := json.Unmarshal([]byte(text), &out); err != nil {
		t.Fatalf("%v: %s", err, text)
	}
	var ids []string
	for _, it := range out.Items {
		ids = append(ids, fmt.Sprint(it["mit_uuid"]))
	}
	return ids
}

func TestSearchTypesAllWordsAnyField(t *testing.T) {
	cases := []struct {
		args map[string]any
		want []string
	}{
		{map[string]any{"search": "Бетар"}, []string{"1", "2", "3"}},
		{map[string]any{"search": "сгв-15"}, []string{"1"}},
		{map[string]any{"search": "воды бетар"}, []string{"1", "2"}},
		{map[string]any{"search": "чистополь счетчики газа"}, []string{"3"}},
		{map[string]any{"search": "воды китай"}, []string{"5"}},
		{map[string]any{"search": "воды бетар москва"}, nil},
		{map[string]any{"title": "счетчики воды", "manufacturers": "бетар"}, []string{"1", "2"}},
		{map[string]any{"title": "воды универсальные", "manufacturers": "чистополь"}, []string{"2"}},
		{map[string]any{"search": "16078-05"}, []string{"1"}},
	}
	for _, tc := range cases {
		fake := &fakeFGIS{typeSearch: fakeTypes}
		text, isErr := callTool(t, newTestServer(t, ModeReadOnly, fake), "search_types", tc.args)
		if isErr {
			t.Fatalf("%v: %s", tc.args, text)
		}
		if got := uuids(t, text); !reflect.DeepEqual(got, tc.want) {
			t.Errorf("%v: нашлось %v, ждали %v\n%s", tc.args, got, tc.want, text)
		}
	}
}

func TestSearchTypesSendsRarestWord(t *testing.T) {
	fake := &fakeFGIS{typeSearch: fakeTypes}
	text, _ := callTool(t, newTestServer(t, ModeReadOnly, fake), "search_types", map[string]any{"search": "воды бетар"})
	// Пробы: оба слова по одной записи, затем выдача по более редкому.
	last := fake.typeQueries[len(fake.typeQueries)-1]
	if last.Search != "*бетар*" || last.Rows != 100 {
		t.Errorf("в ФГИС ушло %+v", last)
	}
	for _, q := range fake.typeQueries[:2] {
		if q.Rows != 1 {
			t.Errorf("проба не на одну запись: %+v", q)
		}
	}
	if !strings.Contains(text, `"sent_to_fgis"`) || !strings.Contains(text, `"count": 2`) && !strings.Contains(text, `"count":2`) {
		t.Errorf("ответ без пояснений: %s", text)
	}
}

func TestSearchTypesZeroProbeStopsEarly(t *testing.T) {
	fake := &fakeFGIS{typeSearch: fakeTypes}
	text, _ := callTool(t, newTestServer(t, ModeReadOnly, fake), "search_types", map[string]any{"search": "воды марсианские"})
	if len(uuids(t, text)) != 0 || len(fake.typeQueries) > 2 {
		t.Errorf("лишние запросы: %d", len(fake.typeQueries))
	}
}

func TestSearchVerificationsOrgWordsAnded(t *testing.T) {
	items := []arshin.VRIItem{
		{VRIID: "1", OrgTitle: `ООО "ЕДИНИЦА ИЗМЕРЕНИЯ"`},
		{VRIID: "2", OrgTitle: `ФБУ "ЕДИНИЦА"`},
		{VRIID: "3", OrgTitle: `ООО "ИЗМЕРЕНИЯ И КОНТРОЛЬ"`},
	}
	fake := &fakeFGIS{search: func(q arshin.VRIQuery) (*arshin.VRIPage, error) {
		var hit []arshin.VRIItem
		for _, it := range items {
			if fgisMatch(it.OrgTitle, q.OrgTitle) {
				hit = append(hit, it)
			}
		}
		return &arshin.VRIPage{Count: len(hit), Items: hit}, nil
	}}
	text, isErr := callTool(t, newTestServer(t, ModeReadOnly, fake), "search_verifications",
		map[string]any{"org_title": "Единица Измерения", "year": 2026})
	if isErr {
		t.Fatal(text)
	}
	var out searchResult
	_ = json.Unmarshal([]byte(text), &out)
	if len(out.Items) != 1 || out.Items[0].VRIID != "1" {
		t.Errorf("нашлось %+v", out.Items)
	}
	if q := fake.queries[0]; fold(q.OrgTitle) != "*измерения*" {
		t.Errorf("в ФГИС ушло org_title=%q", q.OrgTitle)
	}
}

func TestScanBudgetGivesNextStart(t *testing.T) {
	// 1000 кандидатов, нужное слово только в последних — за бюджет
	// не просмотреть, должен вернуться next_start.
	fake := &fakeFGIS{search: func(q arshin.VRIQuery) (*arshin.VRIPage, error) {
		page := &arshin.VRIPage{Count: 1000}
		for i := q.Start; i < min(q.Start+q.Rows, 1000); i++ {
			page.Items = append(page.Items, arshin.VRIItem{VRIID: fmt.Sprint(i), MITTitle: "прибор показывающий"})
		}
		return page, nil
	}}
	text, _ := callTool(t, newTestServer(t, ModeReadOnly, fake), "search_verifications",
		map[string]any{"mit_title": "манометр показывающий", "year": 2026})
	var out searchResult
	_ = json.Unmarshal([]byte(text), &out)
	if out.NextStart == nil || *out.NextStart != scanPages*100 || out.Count != nil {
		t.Errorf("next_start=%v count=%v: %s", out.NextStart, out.Count, text)
	}
	if len(fake.queries) != scanPages {
		t.Errorf("запросов %d, бюджет %d", len(fake.queries), scanPages)
	}
}
