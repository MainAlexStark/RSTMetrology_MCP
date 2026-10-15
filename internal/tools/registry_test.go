package tools

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/MainAlexStark/rst-metrology-mcp/arshin"
	"github.com/MainAlexStark/rst-metrology-mcp/internal/mcp"
	"github.com/MainAlexStark/rst-metrology-mcp/internal/mcptest"
)

// today — фиксированная «сегодняшняя» дата тестов: от неё зависят
// вердикты check_instrument и период поиска по умолчанию.
var today = time.Date(2026, 10, 15, 12, 0, 0, 0, time.UTC)

// newTestServer собирает сервер поверх подставной ФГИС. Тесты проходят
// весь путь: вызов инструмента → защита → клиент.
func newTestServer(t *testing.T, mode Mode, fake *fakeFGIS) *mcp.Server {
	t.Helper()
	safety := DefaultSafety()
	safety.Mode = mode

	server := mcp.NewServer("test", "0.0.1")
	reg := NewRegistry(fake, safety, server)
	reg.now = func() time.Time { return today }
	reg.RegisterVerifications()
	reg.RegisterInstrument()
	reg.RegisterTypes()
	reg.RegisterDiagnostics("https://example.test/eapi")
	reg.RegisterResources()
	return server
}

// callTool вызывает инструмент через настоящий протокол.
func callTool(t *testing.T, server *mcp.Server, name string, args any) (string, bool) {
	t.Helper()
	sess, err := mcptest.Start(server)
	if err != nil {
		t.Fatalf("сессия не поднялась: %v", err)
	}
	defer sess.Close()

	res, err := sess.Call(name, args)
	if err != nil {
		t.Fatalf("вызов %s: %v", name, err)
	}
	return res.Text, res.IsError
}

func TestToolNamesAreStable(t *testing.T) {
	server := newTestServer(t, ModeReadOnly, &fakeFGIS{})

	// Имена — договор с агентами: они зовут инструменты по строке.
	// Переименование не должно проходить молча.
	want := []string{
		"search_verifications", "get_verification", "check_instrument",
		"search_types", "get_type", "rst_status", "rst_selftest",
	}
	got := map[string]bool{}
	for _, n := range server.ToolNames() {
		if got[n] {
			t.Errorf("дубликат инструмента: %s", n)
		}
		got[n] = true
	}
	for _, n := range want {
		if !got[n] {
			t.Errorf("нет инструмента %s", n)
		}
	}
	if len(got) != len(want) {
		t.Errorf("инструментов %d, ждали %d — обновите список в тесте и docs/TOOLS.md", len(got), len(want))
	}
}

func TestSearchVerificationsDefaultsPeriodForSerial(t *testing.T) {
	fake := &fakeFGIS{search: func(q arshin.VRIQuery) (*arshin.VRIPage, error) {
		return &arshin.VRIPage{Count: 30, Items: make([]arshin.VRIItem, 20)}, nil
	}}
	server := newTestServer(t, ModeReadOnly, fake)

	text, isErr := callTool(t, server, "search_verifications", map[string]any{"mi_number": "A123"})
	if isErr {
		t.Fatal(text)
	}
	q := fake.queries[0]
	if q.MINumber != "A123" || q.DateFrom != "2016-10-15" || q.Rows != defaultRows {
		t.Errorf("фильтры дошли не так: %+v", q)
	}
	var out searchResult
	if err := json.Unmarshal([]byte(text), &out); err != nil {
		t.Fatal(err)
	}
	if out.NextStart == nil || *out.NextStart != 20 {
		t.Errorf("next_start не посчитан: %+v", out.NextStart)
	}
	if out.Note == "" {
		t.Error("модели не сказали, что период подставлен")
	}
}

func TestSearchVerificationsWarnsAboutCurrentYear(t *testing.T) {
	fake := &fakeFGIS{}
	server := newTestServer(t, ModeReadOnly, fake)

	text, isErr := callTool(t, server, "search_verifications", map[string]any{"mit_number": "11111-05"})
	if isErr {
		t.Fatal(text)
	}
	if !strings.Contains(text, "текущем году") {
		t.Errorf("нет предупреждения про текущий год: %s", text)
	}
	if fake.queries[0].DateFrom != "" {
		t.Error("без заводского номера период подставлять нельзя: широкий запрос получит отказ")
	}
}

func TestSearchVerificationsNeedsFilter(t *testing.T) {
	fake := &fakeFGIS{}
	server := newTestServer(t, ModeReadOnly, fake)

	_, isErr := callTool(t, server, "search_verifications", map[string]any{"year": 2024})
	if !isErr {
		t.Fatal("поиск без фильтра должен отказывать")
	}
	if fake.requests() != 0 {
		t.Error("запрос без фильтра дошёл до ФГИС")
	}
}

func TestSearchVerificationsArgumentForms(t *testing.T) {
	fake := &fakeFGIS{}
	server := newTestServer(t, ModeReadOnly, fake)

	_, isErr := callTool(t, server, "search_verifications", map[string]any{
		"org_title": "ПОВЕРКА", "date_from": "01.05.2024", "date_to": "2024-05-31",
		"year": "2024", "applicability": "inapplicable", "rows": 1000,
		"filters": map[string]any{"mi_modification": "СВ-15Х", "x": 5},
	})
	if isErr {
		t.Fatal("аргументы в «человеческой» форме должны разбираться")
	}
	q := fake.queries[0]
	if q.DateFrom != "2024-05-01" || q.DateTo != "2024-05-31" || q.Year != 2024 ||
		q.Applicable == nil || *q.Applicable || q.Rows != 100 || q.Extra["x"] != "5" {
		t.Errorf("аргументы разобраны не так: %+v", q)
	}

	_, isErr = callTool(t, server, "search_verifications", map[string]any{"org_title": "X", "date_from": "вчера"})
	if !isErr {
		t.Error("нераспознанная дата должна быть ошибкой, а не пропуском")
	}
}

func TestGetVerificationAcceptsURLAndReportsValidity(t *testing.T) {
	fake := &fakeFGIS{card: func(id string) (*arshin.Card, error) {
		return &arshin.Card{Summary: arshin.CardSummary{
			VRIID: id, Applicable: ptr(true), ValidUntil: "2027-01-01",
		}, Raw: json.RawMessage(`{"big":"doc"}`)}, nil
	}}
	server := newTestServer(t, ModeReadOnly, fake)

	text, isErr := callTool(t, server, "get_verification", map[string]any{
		"vri_id": "https://fgis.gost.ru/fundmetrology/cm/results/1-555/",
	})
	if isErr {
		t.Fatal(text)
	}
	if fake.cards[0] != "1-555" {
		t.Errorf("идентификатор из ссылки не выделен: %q", fake.cards[0])
	}
	if !strings.Contains(text, `"valid_today": true`) {
		t.Errorf("нет valid_today: %s", text)
	}
	if strings.Contains(text, "big") {
		t.Error("исходный документ отдан без include_raw")
	}
}

func TestErrorCarriesHint(t *testing.T) {
	fake := &fakeFGIS{search: func(arshin.VRIQuery) (*arshin.VRIPage, error) {
		return nil, &arshin.APIError{Kind: arshin.KindTooBroad, Status: 400, Path: "/vri"}
	}}
	server := newTestServer(t, ModeReadOnly, fake)

	text, isErr := callTool(t, server, "search_verifications", map[string]any{"mit_title": "счетчики"})
	if !isErr || !strings.Contains(text, "Сузьте выборку") {
		t.Errorf("отказ без подсказки: %s", text)
	}
}

// --- check_instrument ---

func item(id, typ, serial, date, valid string, ok bool) arshin.VRIItem {
	return arshin.VRIItem{VRIID: id, MITNumber: typ, MINumber: serial,
		VerificationDate: date, ValidDate: valid, Applicability: ok}
}

func checkInstrument(t *testing.T, fake *fakeFGIS, args map[string]any) instrumentResult {
	t.Helper()
	text, isErr := callTool(t, newTestServer(t, ModeReadOnly, fake), "check_instrument", args)
	if isErr {
		t.Fatal(text)
	}
	var out instrumentResult
	if err := json.Unmarshal([]byte(text), &out); err != nil {
		t.Fatalf("%v: %s", err, text)
	}
	return out
}

func TestCheckInstrumentPicksLatestAndUsesCard(t *testing.T) {
	fake := &fakeFGIS{
		search: func(arshin.VRIQuery) (*arshin.VRIPage, error) {
			return &arshin.VRIPage{Items: []arshin.VRIItem{
				item("1-old", "11111-05", "A123", "2017-04-13", "2023-04-12", true),
				item("1-new", "11111-05", "А123", "2023-11-14", "", true), // кириллическая «А»
				item("1-other", "11111-05", "A1234", "2025-01-01", "2031-01-01", true),
			}}, nil
		},
		card: func(id string) (*arshin.Card, error) {
			return &arshin.Card{Summary: arshin.CardSummary{VRIID: id, Applicable: ptr(true), ValidUntil: "2029-11-13"}}, nil
		},
	}
	out := checkInstrument(t, fake, map[string]any{"mi_number": "a123", "mit_number": "11111-05"})

	if out.Verdict != verdictValid || out.Latest.VRIID != "1-new" || out.ValidUntil != "2029-11-13" {
		t.Errorf("вердикт: %+v", out)
	}
	if len(out.History) != 2 {
		t.Errorf("в историю попал чужой номер: %v", out.History)
	}
	if out.DaysLeft == nil || *out.DaysLeft <= 0 {
		t.Errorf("days_left: %v", out.DaysLeft)
	}
}

func TestCheckInstrumentVerdicts(t *testing.T) {
	cases := []struct {
		name string
		it   arshin.VRIItem
		card *arshin.CardSummary
		want string
	}{
		{"истёк", item("1", "T", "S", "2018-01-01", "2024-01-01", true), nil, verdictExpired},
		{"непригоден", item("1", "T", "S", "2025-01-01", "", false), nil, verdictInapplicable},
		{"аннулирован", item("1", "T", "S", "2025-01-01", "2031-01-01", true),
			&arshin.CardSummary{Cancelled: true}, verdictCancelled},
		{"без срока", item("1", "T", "S", "2025-01-01", "", true), nil, verdictUnknownTerm},
		{"последний день", item("1", "T", "S", "2020-10-15", "2026-10-15", true), nil, verdictValid},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fake := &fakeFGIS{search: func(arshin.VRIQuery) (*arshin.VRIPage, error) {
				return &arshin.VRIPage{Items: []arshin.VRIItem{tc.it}}, nil
			}}
			if tc.card != nil {
				fake.card = func(string) (*arshin.Card, error) { return &arshin.Card{Summary: *tc.card}, nil }
			}
			if got := checkInstrument(t, fake, map[string]any{"mi_number": "S"}).Verdict; got != tc.want {
				t.Errorf("вердикт %s, ждали %s", got, tc.want)
			}
		})
	}
}

func TestCheckInstrumentAmbiguousSerial(t *testing.T) {
	fake := &fakeFGIS{search: func(arshin.VRIQuery) (*arshin.VRIPage, error) {
		return &arshin.VRIPage{Items: []arshin.VRIItem{
			item("1", "54233-13", "0112456", "2024-05-08", "", false),
			item("2", "14494-04", "0112456", "2024-09-06", "2032-09-05", true),
		}}, nil
	}}
	out := checkInstrument(t, fake, map[string]any{"mi_number": "0112456"})
	if out.Verdict != verdictAmbiguous || len(out.Types) != 2 {
		t.Errorf("два типа с одним номером должны дать ambiguous: %+v", out)
	}
	if len(fake.cards) != 0 {
		t.Error("при неоднозначности карточку запрашивать незачем")
	}
}

func TestCheckInstrumentFallsBackToYearsWithinBudget(t *testing.T) {
	fake := &fakeFGIS{search: func(q arshin.VRIQuery) (*arshin.VRIPage, error) {
		if q.Year == 0 {
			return nil, &arshin.APIError{Kind: arshin.KindTooBroad, Status: 408, Path: "/vri"}
		}
		if q.Year == 2024 {
			return &arshin.VRIPage{Items: []arshin.VRIItem{item("1", "T", "S", "2024-03-01", "2030-03-01", true)}}, nil
		}
		return &arshin.VRIPage{}, nil
	}}
	out := checkInstrument(t, fake, map[string]any{"mi_number": "S"})
	if out.Verdict != verdictValid {
		t.Errorf("перебор по годам не нашёл запись: %+v", out)
	}
	// Период целиком + 2026, 2025, 2024 — и остановка на первом году с записью.
	if len(fake.queries) != 4 {
		t.Errorf("запросов поиска %d, ждали 4", len(fake.queries))
	}
}

func TestCheckInstrumentStopsAtRequestBudget(t *testing.T) {
	fake := &fakeFGIS{search: func(q arshin.VRIQuery) (*arshin.VRIPage, error) {
		if q.Year == 0 {
			return nil, &arshin.APIError{Kind: arshin.KindTooBroad, Status: 400, Path: "/vri"}
		}
		return &arshin.VRIPage{}, nil
	}}
	out := checkInstrument(t, fake, map[string]any{"mi_number": "S", "years_back": 30})
	if out.Verdict != verdictIncomplete {
		t.Errorf("исчерпанный перебор должен честно сказать об этом: %s", out.Verdict)
	}
	if n := fake.requests(); n > DefaultSafety().MaxRequestsPerCall {
		t.Errorf("запросов к ФГИС %d — больше лимита на вызов %d", n, DefaultSafety().MaxRequestsPerCall)
	}
}

// --- типы ---

func TestGetTypeByRegistrationNumber(t *testing.T) {
	fake := &fakeFGIS{
		typePage: &arshin.RawPage{Count: 2, Items: []map[string]any{
			{"mit_uuid": "u-other", "number": "16078-055"},
			{"mit_uuid": "u-right", "number": "16078-05", "title": "Счетчики воды"},
		}},
		typeCard: json.RawMessage(`{"interval":"6 лет"}`),
	}
	text, isErr := callTool(t, newTestServer(t, ModeReadOnly, fake), "get_type", map[string]any{"id": "16078-05"})
	if isErr {
		t.Fatal(text)
	}
	if strings.Join(fake.types, ",") != "search:16078-05,card:u-right" {
		t.Errorf("путь до карточки: %v", fake.types)
	}
	if !strings.Contains(text, "6 лет") {
		t.Errorf("карточка не отдана: %s", text)
	}
}

func TestGetTypeByUUIDGoesStraightToCard(t *testing.T) {
	fake := &fakeFGIS{typeCard: json.RawMessage(`{}`)}
	_, isErr := callTool(t, newTestServer(t, ModeReadOnly, fake), "get_type",
		map[string]any{"id": "https://fgis.gost.ru/fundmetrology/cm/mits/83167556-4958-349b"})
	if isErr || strings.Join(fake.types, ",") != "card:83167556-4958-349b" {
		t.Errorf("по uuid ждали одну карточку: %v", fake.types)
	}
}

// --- диагностика и ресурсы ---

func TestStatusAndSelftest(t *testing.T) {
	fake := &fakeFGIS{}
	server := newTestServer(t, ModeReadOnly, fake)

	text, isErr := callTool(t, server, "rst_status", nil)
	if isErr || !strings.Contains(text, "read-only") || !strings.Contains(text, "example.test") {
		t.Errorf("rst_status: %s", text)
	}
	text, isErr = callTool(t, server, "rst_selftest", nil)
	if isErr || strings.Count(text, "OK") != 2 {
		t.Errorf("rst_selftest: %s", text)
	}
}

func TestSelftestSaysWhenNothingAnswers(t *testing.T) {
	fake := &fakeFGIS{search: func(arshin.VRIQuery) (*arshin.VRIPage, error) {
		return nil, &arshin.APIError{Kind: arshin.KindNetwork, Path: "/vri", Err: context.DeadlineExceeded}
	}}
	// Поиск типов у подставной ФГИС всегда отвечает — уроним и его.
	fake.typePage = nil
	server := mcp.NewServer("test", "0")
	reg := NewRegistry(failingTypes{fake}, DefaultSafety(), server)
	reg.RegisterDiagnostics("x")

	text, _ := callTool(t, server, "rst_selftest", nil)
	if !strings.Contains(text, "российским адресам") {
		t.Errorf("нет вывода про доступ к ФГИС: %s", text)
	}
}

type failingTypes struct{ *fakeFGIS }

func (failingTypes) SearchTypes(context.Context, arshin.MITQuery) (*arshin.RawPage, error) {
	return nil, &arshin.APIError{Kind: arshin.KindNetwork, Path: "/mit", Err: context.DeadlineExceeded}
}

func TestGuideResourceListed(t *testing.T) {
	sess, err := mcptest.Start(newTestServer(t, ModeReadOnly, &fakeFGIS{}))
	if err != nil {
		t.Fatal(err)
	}
	defer sess.Close()
	raw, rpcErr, err := sess.Request("resources/read", map[string]any{"uri": GuideURI})
	if err != nil || rpcErr != nil {
		t.Fatalf("ресурс не читается: %v %v", err, rpcErr)
	}
	if !strings.Contains(string(raw), "vri_id") {
		t.Errorf("памятка пустая: %s", raw)
	}
}
