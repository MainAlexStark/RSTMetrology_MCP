package arshin

import (
	"strings"
	"time"
)

// Даты в ответах ФГИС приходят в двух видах — «14.11.2023» и
// «2023-11-14» (иногда с временем). Сравнивать такие строки между
// собой нельзя, поэтому наружу всё отдаётся в ISO.

var dateLayouts = []string{
	"02.01.2006",
	time.DateOnly,
	"2006-01-02T15:04:05",
	time.RFC3339,
	"02.01.2006 15:04:05",
}

// ParseDate разбирает дату ФГИС. Пустая или нераспознанная — ok=false.
func ParseDate(s string) (time.Time, bool) {
	s = strings.TrimSpace(s)
	if s == "" {
		return time.Time{}, false
	}
	for _, l := range dateLayouts {
		if t, err := time.Parse(l, s); err == nil {
			return t, true
		}
	}
	return time.Time{}, false
}

// isoDate приводит дату к YYYY-MM-DD; нераспознанную оставляет как есть,
// чтобы не потерять данные из-за непредусмотренного формата.
func isoDate(s string) string {
	if t, ok := ParseDate(s); ok {
		return t.Format(time.DateOnly)
	}
	return s
}

// ISODate — экспортируемый вариант isoDate для слоя инструментов.
func ISODate(s string) string { return isoDate(s) }
