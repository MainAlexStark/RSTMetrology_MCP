package tools

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

// Разбор аргументов.
//
// Аргументы приезжают из JSON, поэтому любое число — это float64,
// а модель, увидев в предыдущем ответе `"year": 2024`, может прислать
// и строку "2024". Приводить типы в каждом инструменте значит повторить
// одну и ту же ошибку в двадцати местах, поэтому приведение живёт
// здесь, а инструменты просят сразу нужный тип.

// Str возвращает строковый аргумент; отсутствующий — пустая строка.
func (a Args) Str(key string) string {
	switch v := a[key].(type) {
	case string:
		return strings.TrimSpace(v)
	case float64:
		return strconv.FormatInt(int64(v), 10)
	default:
		return ""
	}
}

// Require возвращает обязательный строковый аргумент.
//
// Отказ формулируется словами, а не кодом: модель читает текст ошибки
// и должна понять, что именно дослать, с первой попытки.
func (a Args) Require(key string) (string, error) {
	if v := a.Str(key); v != "" {
		return v, nil
	}
	return "", fmt.Errorf("не задан обязательный аргумент %s", key)
}

// Int возвращает целочисленный аргумент и признак того, что он задан.
func (a Args) Int(key string) (int64, bool) {
	switch v := a[key].(type) {
	case float64:
		return int64(v), true
	case int:
		return int64(v), true
	case int64:
		return v, true
	case string:
		n, err := strconv.ParseInt(strings.TrimSpace(v), 10, 64)
		if err != nil {
			return 0, false
		}
		return n, true
	default:
		return 0, false
	}
}

// IntOr возвращает целочисленный аргумент или значение по умолчанию.
func (a Args) IntOr(key string, def int64) int64 {
	if n, ok := a.Int(key); ok {
		return n
	}
	return def
}

// Bool возвращает булев аргумент.
func (a Args) Bool(key string) bool {
	switch v := a[key].(type) {
	case bool:
		return v
	case string:
		return v == "true" || v == "1"
	default:
		return false
	}
}

// Map возвращает вложенный объект — например тело патча.
func (a Args) Map(key string) (map[string]any, bool) {
	m, ok := a[key].(map[string]any)
	return m, ok
}

// StrMap возвращает объект «строка → строка» — например filters.
// Числа и булевы значения приводятся к строке: в адресе запроса
// они всё равно станут текстом.
func (a Args) StrMap(key string) map[string]string {
	m, ok := a.Map(key)
	if !ok || len(m) == 0 {
		return nil
	}
	out := make(map[string]string, len(m))
	for k, v := range m {
		switch t := v.(type) {
		case string:
			out[k] = strings.TrimSpace(t)
		case float64:
			out[k] = strconv.FormatFloat(t, 'f', -1, 64)
		case bool:
			out[k] = strconv.FormatBool(t)
		}
	}
	return out
}

// Date возвращает дату-аргумент в виде YYYY-MM-DD.
//
// Модель пишет даты как придётся — «14.11.2023», «2023-11-14», —
// а API ждёт ISO. Нераспознанная дата — ошибка с примером, а не
// молчаливый пропуск: без фильтра по дате поиск вернёт текущий год,
// и модель решит, что записей нет.
func (a Args) Date(key string) (string, error) {
	v := a.Str(key)
	if v == "" {
		return "", nil
	}
	for _, l := range []string{time.DateOnly, "02.01.2006"} {
		if t, err := time.Parse(l, v); err == nil {
			return t.Format(time.DateOnly), nil
		}
	}
	return "", fmt.Errorf("аргумент %s: не разобрать дату %q, нужен вид 2024-05-31", key, v)
}

// Значения по умолчанию для аргументов, которые модель обычно не
// задаёт.
const (
	// defaultRows — размер страницы поиска. Сотня записей — это
	// 30–40 КБ JSON, то есть почти весь потолок ответа; двадцати
	// хватает, чтобы понять, туда ли смотрим, и попросить ещё.
	defaultRows = 20

	// defaultYearsBack — на сколько лет назад искать историю одного
	// прибора. Межповерочный интервал большинства СИ — до 6–8 лет,
	// десять дают запас на просроченные.
	defaultYearsBack = 10
)

// rows приводит размер страницы к допустимым границам.
func rows(a Args, def int) int {
	n := a.IntOr("rows", int64(def))
	switch {
	case n <= 0:
		return def
	case n > 100:
		return 100
	default:
		return int(n)
	}
}
