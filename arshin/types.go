package arshin

import "encoding/json"

// VRIQuery — фильтры поиска поверок (GET /vri).
//
// Имена параметров совпадают с именами полей в ответе: mit_number
// ищет по mit_number и т. д. Строковые фильтры допускают звёздочку
// как шаблон («ВКТ-7*»).
type VRIQuery struct {
	Search         string // search — полнотекстовый поиск по записи
	MITNumber      string // mit_number — регистрационный номер типа СИ, «16078-05»
	MITTitle       string // mit_title — наименование типа
	MITNotation    string // mit_notation — обозначение типа
	MIModification string // mi_modification — модификация
	MINumber       string // mi_number — заводской номер экземпляра
	OrgTitle       string // org_title — организация-поверитель
	Year           int    // year — год поверки
	DateFrom       string // verification_date_start, YYYY-MM-DD
	DateTo         string // verification_date_end, YYYY-MM-DD
	Applicable     *bool  // applicability — только пригодные / непригодные
	Sort           string // sort — например "verification_date desc"
	Rows           int    // rows, не больше MaxRows
	Start          int    // start — смещение

	// Extra — прочие параметры как есть. Запасной выход на случай,
	// когда API добавит фильтр раньше, чем его добавят сюда:
	// иначе пришлось бы выпускать новую версию ради одного имени.
	Extra map[string]string
}

// MITQuery — фильтры поиска утверждённых типов СИ (GET /mit).
type MITQuery struct {
	Search string // search — номер в реестре, наименование, обозначение, изготовитель
	Rows   int
	Start  int
	Extra  map[string]string
}

// VRIItem — строка списка поверок.
type VRIItem struct {
	VRIID            string `json:"vri_id"`
	OrgTitle         string `json:"org_title"`
	MITNumber        string `json:"mit_number"`
	MITTitle         string `json:"mit_title"`
	MITNotation      string `json:"mit_notation"`
	MIModification   string `json:"mi_modification"`
	MINumber         string `json:"mi_number"`
	VerificationDate string `json:"verification_date"`
	ValidDate        string `json:"valid_date"`
	ResultDocnum     string `json:"result_docnum"`
	Applicability    bool   `json:"applicability"`
}

// VRIPage — страница списка поверок.
type VRIPage struct {
	Count int       `json:"count"`
	Start int       `json:"start"`
	Rows  int       `json:"rows"`
	Items []VRIItem `json:"items"`
}

// RawPage — страница списка, поля которого не типизированы.
//
// Для реестра типов набор полей в ответе описан хуже, чем для поверок,
// и меняется чаще. Строгая структура здесь молча теряла бы новые поля,
// поэтому элементы отдаются как пришли.
type RawPage struct {
	Count int              `json:"count"`
	Start int              `json:"start"`
	Rows  int              `json:"rows"`
	Items []map[string]any `json:"items"`
}

// envelope — общая обёртка ответов API: {"result": …}.
type envelope struct {
	Result json.RawMessage `json:"result"`
}
