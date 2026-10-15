package arshin

import (
	"encoding/json"
	"strconv"
	"strings"
	"time"
)

// Карточка поверки (GET /vri/{id}).
//
// Карточка устроена вложенно и неоднородно: прибор лежит в miInfo под
// одним из ключей singleMI / etaMI / partyMI (единичное СИ, эталон,
// партия), а иногда — прямо в корне; результат — это наличие ключа
// applicable или inapplicable, а не поле со значением. Модель на таком
// документе ошибается в самом важном — «годен ли прибор», — поэтому
// рядом с сырым документом отдаётся плоская сводка.

// Card — карточка поверки: сводка и исходный документ.
type Card struct {
	Summary CardSummary     `json:"summary"`
	Raw     json.RawMessage `json:"raw"`

	// Chain — идентификаторы версий, пройденных по vriVerIdNext,
	// начиная с запрошенной. Пусто, если запись не модифицировалась.
	Chain []string `json:"chain,omitempty"`
}

// CardSummary — то, что спрашивают о поверке чаще всего.
type CardSummary struct {
	VRIID string `json:"vri_id"`

	// Status — статус публикации как его пишет ФГИС («Запись
	// модифицирована», «Запись аннулирована»); пусто — действующая.
	Status    string `json:"publication_status,omitempty"`
	Cancelled bool   `json:"cancelled"`
	Modified  bool   `json:"modified"`
	NextVRIID string `json:"next_vri_id,omitempty"`

	MIKind          string `json:"mi_kind,omitempty"` // single | etalon | party
	TypeNumber      string `json:"mit_number,omitempty"`
	TypeTitle       string `json:"mit_title,omitempty"`
	TypeNotation    string `json:"mit_notation,omitempty"`
	TypeURL         string `json:"mit_url,omitempty"`
	Modification    string `json:"modification,omitempty"`
	SerialNumber    string `json:"mi_number,omitempty"`
	ManufactureYear int    `json:"manufacture_year,omitempty"`

	Organization string `json:"organization,omitempty"`
	SignCipher   string `json:"sign_cipher,omitempty"`
	Owner        string `json:"owner,omitempty"`
	VerifiedOn   string `json:"verification_date,omitempty"` // YYYY-MM-DD
	ValidUntil   string `json:"valid_date,omitempty"`        // YYYY-MM-DD
	VRIType      string `json:"vri_type,omitempty"`          // 1 — первичная, 2 — периодическая
	Method       string `json:"method,omitempty"`            // документ, по которому поверяли

	// Applicable — заключение: true — пригодно (свидетельство),
	// false — непригодно (извещение), nil — в карточке нет ни того,
	// ни другого (аннулирована или неполная).
	Applicable *bool  `json:"applicable"`
	CertNum    string `json:"certificate_number,omitempty"`
	NoticeNum  string `json:"notice_number,omitempty"`
}

// ParseCard разбирает поле result карточки.
func ParseCard(id string, raw json.RawMessage) (CardSummary, error) {
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		return CardSummary{}, &APIError{Kind: KindBadResponse, Path: "/vri/" + id, Err: err}
	}

	s := CardSummary{VRIID: id}

	pub := mapping(doc["publication"])
	s.Status = str(pub["status"])
	low := strings.ToLower(s.Status)
	s.Cancelled = strings.Contains(low, "аннулир")
	s.Modified = strings.Contains(low, "модифиц")
	for _, k := range []string{"vriVerIdNext", "ver_id_next", "verIdNext"} {
		if v := str(pub[k]); v != "" {
			s.NextVRIID = v
			break
		}
	}

	mi, kind := instrument(doc)
	s.MIKind = kind
	s.TypeNumber = str(mi["mitypeNumber"])
	s.TypeTitle = str(mi["mitypeTitle"])
	s.TypeNotation = str(mi["mitypeType"])
	s.TypeURL = str(mi["mitypeURL"])
	s.Modification = str(mi["modification"])
	s.SerialNumber = str(mi["manufactureNum"])
	if y, ok := mi["manufactureYear"].(float64); ok {
		s.ManufactureYear = int(y)
	}

	vri := mapping(doc["vriInfo"])
	s.Organization = str(vri["organization"])
	s.SignCipher = str(vri["signCipher"])
	s.Owner = str(vri["miOwner"])
	s.VerifiedOn = isoDate(str(vri["vrfDate"]))
	s.ValidUntil = isoDate(str(vri["validDate"]))
	s.VRIType = str(vri["vriType"])
	s.Method = str(vri["docTitle"])

	if a, ok := vri["applicable"]; ok {
		yes := true
		s.Applicable = &yes
		s.CertNum = str(mapping(a)["certNum"])
	} else if n, ok := vri["inapplicable"]; ok {
		no := false
		s.Applicable = &no
		s.NoticeNum = str(mapping(n)["noticeNum"])
	}
	return s, nil
}

// instrument находит сведения о приборе и их вид.
func instrument(doc map[string]any) (map[string]any, string) {
	kinds := []struct{ key, kind string }{
		{"singleMI", "single"}, {"etaMI", "etalon"}, {"partyMI", "party"},
	}
	info := mapping(doc["miInfo"])
	for _, k := range kinds {
		if m := mapping(info[k.key]); len(m) > 0 {
			return m, k.kind
		}
		if m := mapping(doc[k.key]); len(m) > 0 {
			return m, k.kind
		}
	}
	return map[string]any{}, ""
}

// ValidOn сообщает, действует ли поверка на дату day: заключение
// «пригодно», запись не аннулирована, срок не истёк.
//
// Второе значение — можно ли вообще судить: у части записей срока нет
// (бессрочные или неполные), и выдавать «действует» по ним нельзя.
func (s CardSummary) ValidOn(day time.Time) (valid, known bool) {
	if s.Cancelled || s.Applicable == nil {
		return false, s.Cancelled
	}
	if !*s.Applicable {
		return false, true
	}
	until, err := time.Parse(time.DateOnly, s.ValidUntil)
	if err != nil {
		return false, false
	}
	return !day.After(until), true
}

func mapping(v any) map[string]any {
	m, _ := v.(map[string]any)
	return m
}

func str(v any) string {
	switch t := v.(type) {
	case string:
		return strings.TrimSpace(t)
	case float64:
		return strconv.FormatFloat(t, 'f', -1, 64)
	default:
		return ""
	}
}
