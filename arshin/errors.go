package arshin

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strings"
)

// Разбор отказов ФГИС.
//
// Отказы у этого API внешне похожи — «ничего не пришло», — а лечатся
// противоположно: на 429 надо подождать, на 400/408 — сузить запрос,
// на таймаут соединения — вообще сменить сеть (сервис отвечает только
// российским адресам). Совет не из того класса отправляет человека
// искать ошибку не там, поэтому классы разведены явно.

// Kind — класс отказа.
type Kind string

const (
	KindTooBroad    Kind = "too_broad"    // 400/408: запрос слишком широкий
	KindNotFound    Kind = "not_found"    // 404
	KindRateLimited Kind = "rate_limited" // 429 после всех повторов
	KindServer      Kind = "server"       // 5xx после всех повторов
	KindNetwork     Kind = "network"      // соединение не установилось или оборвалось
	KindBadResponse Kind = "bad_response" // не JSON / неожиданная форма
	KindHTTP        Kind = "http"         // прочие коды
)

// APIError — отказ ФГИС.
type APIError struct {
	Kind   Kind
	Status int
	Path   string
	Body   string // начало тела ответа, для диагностики
	Err    error
}

func (e *APIError) Error() string {
	var b strings.Builder
	fmt.Fprintf(&b, "ФГИС «Аршин» %s: ", e.Path)
	switch {
	case e.Status != 0:
		fmt.Fprintf(&b, "HTTP %d", e.Status)
	case e.Err != nil:
		b.WriteString(e.Err.Error())
	default:
		b.WriteString(string(e.Kind))
	}
	if e.Body != "" {
		fmt.Fprintf(&b, " — %s", e.Body)
	}
	return b.String()
}

func (e *APIError) Unwrap() error { return e.Err }

// IsKind сообщает, относится ли ошибка к классу k.
func IsKind(err error, k Kind) bool {
	var e *APIError
	return errors.As(err, &e) && e.Kind == k
}

// Hint объясняет отказ словами. Пустая строка — «объяснить нечем».
func Hint(err error) string {
	if err == nil {
		return ""
	}
	var e *APIError
	if !errors.As(err, &e) {
		if errors.Is(err, context.DeadlineExceeded) {
			return networkHint
		}
		return ""
	}

	switch e.Kind {
	case KindTooBroad:
		return "ФГИС отклонила запрос как слишком широкий («превышен лимит на поиск»).\n" +
			"Сузьте выборку: задайте year (один год) или узкий период date_from/date_to,\n" +
			"добавьте mit_number (номер типа в реестре) или mi_number (заводской номер)."

	case KindNotFound:
		return "Такой записи нет. Идентификатор поверки выглядит как «1-340080782» — " +
			"берите его из поля vri_id результата search_verifications, а не номер свидетельства."

	case KindRateLimited:
		return "ФГИС ограничила частоту запросов (не больше 2 в секунду с одного адреса).\n" +
			"Подождите полминуты и повторите. Если отказ повторяется — с этого адреса\n" +
			"недавно шло много запросов; увеличьте RST_MIN_INTERVAL_MS."

	case KindServer:
		return "ФГИС отвечает ошибкой на своей стороне. Это не запрос: сервис\n" +
			"периодически недоступен, повторите позже."

	case KindNetwork:
		return networkHint

	case KindBadResponse:
		if looksJSON([]byte(e.Body)) {
			return "ФГИС ответила JSON неожиданной формы — вероятно, изменился формат API.\n" +
				"Сверьте фрагмент ответа выше с разбором в arshin/client.go."
		}
		return "ФГИС вернула не JSON — обычно это HTML-заглушка: техработы или\n" +
			"блокировка адреса. Откройте https://fgis.gost.ru/fundmetrology/cm/ с той же\n" +
			"машины, где запущен сервер, и посмотрите, что там показывается."
	}
	return ""
}

const networkHint = "До ФГИС не доходит соединение. Сервис отвечает только российским адресам:\n" +
	"из-за границы (и через зарубежный VPN) соединение висит до таймаута.\n" +
	"Запускайте сервер на машине с российским IP или задайте HTTPS_PROXY\n" +
	"(либо RST_BASE_URL на свой прокси до fgis.gost.ru)."

// isTimeout — соединение не уложилось в отведённое время.
func isTimeout(err error) bool {
	var ne net.Error
	return errors.Is(err, context.DeadlineExceeded) || (errors.As(err, &ne) && ne.Timeout())
}
