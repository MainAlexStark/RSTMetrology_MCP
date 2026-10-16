package arshin

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// Ответы в форме, описанной руководством «Внешние публичные
// интерфейсы»: значения выдуманы, форма — как у настоящих.
const (
	listBody = `{"result":{"count":2,"start":0,"rows":100,"items":[
{"vri_id":"1-100000001","org_title":"ООО \"ПОВЕРКА\"","mit_number":"11111-05","mit_title":"Счетчики воды",
 "mit_notation":"СВ-15","mi_modification":"СВ-15Х","mi_number":"A123","verification_date":"14.11.2023",
 "valid_date":"13.11.2029","result_docnum":"С-ААА/14-11-2023/100000001","applicability":true},
{"vri_id":"1-100000002","org_title":"ООО \"ПОВЕРКА\"","mit_number":"11111-05","mit_title":"Счетчики воды",
 "mit_notation":"СВ-15","mi_modification":"","mi_number":"A123","verification_date":"2017-04-13",
 "valid_date":null,"result_docnum":"И-ААА/13-04-2017/100000002","applicability":false}]}}`

	cardApplicable = `{"result":{"miInfo":{"singleMI":{"mitypeNumber":"11111-05",
"mitypeURL":"https://fgis.gost.ru/fundmetrology/cm/mits/0000-uuid","mitypeType":"СВ-15",
"mitypeTitle":"Счетчики воды","manufactureNum":"A123","manufactureYear":2019,"modification":"СВ-15Х"}},
"vriInfo":{"organization":"ООО \"ПОВЕРКА\"","signCipher":"ААА","vrfDate":"14.11.2023","validDate":"13.11.2029",
"vriType":"2","docTitle":"МИ 1592-2015","applicable":{"certNum":"С-ААА/14-11-2023/100000001","signPass":true}}}}`

	cardModified = `{"result":{"publication":{"status":"Запись модифицирована","vriVerIdNext":"1-100000001"}}}`

	cardInapplicable = `{"result":{"miInfo":{"etaMI":{"mitypeNumber":"22222-13","manufactureNum":"B7"}},
"vriInfo":{"vrfDate":"08.05.2024","inapplicable":{"noticeNum":"И-БББ/08-05-2024/1"}}}}`
)

func testClient(t *testing.T, h http.HandlerFunc) (*HTTPClient, *atomic.Int32) {
	t.Helper()
	var n atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n.Add(1)
		h(w, r)
	}))
	t.Cleanup(srv.Close)

	cfg := DefaultConfig()
	cfg.BaseURL = srv.URL
	cfg.MinInterval = 0
	c := New(cfg)
	c.sleep = func(context.Context, time.Duration) error { return nil }
	return c, &n
}

func TestSearchVerificationsSendsFiltersAndNormalizesDates(t *testing.T) {
	var got string
	c, _ := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		got = r.URL.Path + "?" + r.URL.RawQuery
		_, _ = w.Write([]byte(listBody))
	})

	yes := true
	page, err := c.SearchVerifications(context.Background(), VRIQuery{
		MITNumber: "11111-05", MINumber: "A123", DateFrom: "2015-01-01",
		Applicable: &yes, Rows: 500, Extra: map[string]string{"foo": "bar"},
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"/vri?", "mit_number=11111-05", "mi_number=A123",
		"verification_date_start=2015-01-01", "applicability=true", "rows=100", "foo=bar"} {
		if !strings.Contains(got, want) {
			t.Errorf("в запросе %q нет %q", got, want)
		}
	}
	if page.Count != 2 || len(page.Items) != 2 {
		t.Fatalf("страница разобрана не так: %+v", page)
	}
	if page.Items[0].VerificationDate != "2023-11-14" || page.Items[0].ValidDate != "2029-11-13" {
		t.Errorf("даты не приведены к ISO: %+v", page.Items[0])
	}
	if page.Items[1].VerificationDate != "2017-04-13" {
		t.Errorf("ISO-дата испорчена: %q", page.Items[1].VerificationDate)
	}
}

func TestVerificationFollowsModificationChain(t *testing.T) {
	c, n := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/vri/1-old":
			_, _ = w.Write([]byte(cardModified))
		case "/vri/1-100000001":
			_, _ = w.Write([]byte(cardApplicable))
		default:
			http.NotFound(w, r)
		}
	})

	card, err := c.Verification(context.Background(), "1-old", true)
	if err != nil {
		t.Fatal(err)
	}
	s := card.Summary
	if s.VRIID != "1-100000001" || s.Applicable == nil || !*s.Applicable {
		t.Fatalf("не дошли до действующей версии: %+v", s)
	}
	if s.VerifiedOn != "2023-11-14" || s.ValidUntil != "2029-11-13" || s.ManufactureYear != 2019 {
		t.Errorf("сводка разобрана не так: %+v", s)
	}
	if len(card.Chain) != 2 {
		t.Errorf("цепочка версий: %v", card.Chain)
	}

	// Повтор — из кэша, без запросов в ФГИС.
	before := n.Load()
	if _, err := c.Verification(context.Background(), "1-100000001", true); err != nil {
		t.Fatal(err)
	}
	if n.Load() != before {
		t.Error("повторная карточка ушла в ФГИС мимо кэша")
	}
}

func TestVerificationWithoutFollowStopsAtModified(t *testing.T) {
	c, _ := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(cardModified))
	})
	card, err := c.Verification(context.Background(), "1-old", false)
	if err != nil {
		t.Fatal(err)
	}
	if !card.Summary.Modified || card.Summary.NextVRIID != "1-100000001" {
		t.Errorf("статус модификации потерян: %+v", card.Summary)
	}
}

func TestParseCardInapplicableEtalon(t *testing.T) {
	c, _ := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(cardInapplicable))
	})
	card, err := c.Verification(context.Background(), "1-x", true)
	if err != nil {
		t.Fatal(err)
	}
	s := card.Summary
	if s.MIKind != "etalon" || s.Applicable == nil || *s.Applicable || s.NoticeNum == "" {
		t.Errorf("непригодный эталон разобран не так: %+v", s)
	}
	if valid, known := s.ValidOn(time.Now()); valid || !known {
		t.Errorf("непригодный прибор должен быть «точно не действует»: %v %v", valid, known)
	}
}

func TestErrorClasses(t *testing.T) {
	cases := []struct {
		name   string
		status int
		body   string
		kind   Kind
		calls  int32
	}{
		{"широкий запрос", 400, `{"error":"превышен лимит на поиск"}`, KindTooBroad, 1},
		{"таймаут поиска", 408, ``, KindTooBroad, 1},
		{"чужой параметр", 400, `{"status":"400 Bad Request","message":"Параметр manufacturer не допустим для запроса"}`, KindBadParam, 1},
		{"нет записи", 404, ``, KindNotFound, 1},
		{"частота", 429, ``, KindRateLimited, 3}, // повторы
		{"сервер", 502, `bad gateway`, KindServer, 3},
		{"HTML-заглушка", 200, `<html>Технические работы</html>`, KindBadResponse, 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c, n := testClient(t, func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(tc.body))
			})
			_, err := c.SearchVerifications(context.Background(), VRIQuery{Search: "x"})
			if !IsKind(err, tc.kind) {
				t.Fatalf("ждали %s, получили %v", tc.kind, err)
			}
			if Hint(err) == "" {
				t.Error("нет подсказки к отказу")
			}
			if n.Load() != tc.calls {
				t.Errorf("запросов %d, ждали %d", n.Load(), tc.calls)
			}
		})
	}
}

func TestRetryRecoversAfter429(t *testing.T) {
	var first atomic.Bool
	c, n := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		if !first.Swap(true) {
			w.Header().Set("Retry-After", "1")
			w.WriteHeader(429)
			return
		}
		_, _ = w.Write([]byte(listBody))
	})
	if _, err := c.SearchVerifications(context.Background(), VRIQuery{Search: "x"}); err != nil {
		t.Fatal(err)
	}
	if n.Load() != 2 {
		t.Errorf("запросов %d, ждали 2", n.Load())
	}
}

func TestPauseBetweenRequests(t *testing.T) {
	c, _ := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(listBody))
	})
	c.cfg.MinInterval = time.Second
	var waited []time.Duration
	c.sleep = func(_ context.Context, d time.Duration) error { waited = append(waited, d); return nil }

	for range 2 {
		if _, err := c.SearchVerifications(context.Background(), VRIQuery{Search: "x"}); err != nil {
			t.Fatal(err)
		}
	}
	if len(waited) != 1 || waited[0] < 900*time.Millisecond {
		t.Errorf("между запросами нет паузы: %v", waited)
	}
}

func TestParseDate(t *testing.T) {
	for in, want := range map[string]string{
		"14.11.2023": "2023-11-14", "2023-11-14": "2023-11-14",
		"2023-11-14T00:00:00": "2023-11-14", "": "", "когда-то": "когда-то",
	} {
		if got := ISODate(in); got != want {
			t.Errorf("ISODate(%q) = %q, ждали %q", in, got, want)
		}
	}
}

// Карточка типа /mit/{uuid} приходит без обёртки result — сразу {"general": …}.
func TestTypeCardWithoutResultEnvelope(t *testing.T) {
	const body = `{"general":{"mit_uuid":"u1","number":"16078-05","title":"Счетчики холодной и горячей воды"}}`
	c, _ := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(body))
	})
	raw, err := c.Type(context.Background(), "u1")
	if err != nil {
		t.Fatal(err)
	}
	if string(raw) != body {
		t.Errorf("карточка %s", raw)
	}
}

func TestTypeCardWithResultEnvelope(t *testing.T) {
	c, _ := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"result":{"general":{"number":"1-01"}}}`))
	})
	raw, err := c.Type(context.Background(), "u2")
	if err != nil {
		t.Fatal(err)
	}
	if string(raw) != `{"general":{"number":"1-01"}}` {
		t.Errorf("карточка %s", raw)
	}
}
