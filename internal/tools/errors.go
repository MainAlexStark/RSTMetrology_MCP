package tools

import "fmt"

// Сокращения для описания схем.
//
// Карты в JSON Schema встречаются на каждой строке, и без этих
// сокращений объявления инструментов тонут в скобках. Схема — это то,
// что видит модель: поле без описания она заполняет догадкой, поэтому
// экономить здесь надо на скобках, а не на словах.

// obj — сокращение для карты.
type obj = map[string]any

// schema собирает JSON Schema объекта с перечисленными свойствами.
func schema(props obj, required ...string) obj {
	s := obj{"type": "object", "properties": props}
	if len(required) > 0 {
		s["required"] = required
	}
	return s
}

// str описывает строковое поле схемы.
func str(desc string) obj { return obj{"type": "string", "description": desc} }

// num описывает целочисленное поле схемы.
func num(desc string) obj { return obj{"type": "integer", "description": desc} }

// boolean описывает булево поле схемы.
func boolean(desc string) obj { return obj{"type": "boolean", "description": desc} }

// object описывает поле произвольной формы — например тело патча.
func object(desc string) obj { return obj{"type": "object", "description": desc} }

// arr описывает массив строк.
func arr(desc string) obj {
	return obj{"type": "array", "items": obj{"type": "string"}, "description": desc}
}

// enum описывает строковое поле с перечнем допустимых значений.
func enum(desc string, values ...string) obj {
	return obj{"type": "string", "enum": values, "description": desc}
}

// filtersField — запасной выход для параметров API, которых нет в схеме.
//
// Без него новый фильтр ФГИС стал бы доступен модели только с новой
// версией сервера, а до тех пор она подставляла бы его в search
// и получала мусор.
func filtersField() obj {
	return object("Дополнительные параметры запроса к API ФГИС как есть, " +
		"{\"имя\": \"значение\"}. Нужны редко: для фильтров, которых нет среди аргументов")
}

// rowsField и startField — постраничный вывод.
func rowsField(def int) obj {
	return num(fmt.Sprintf("Сколько записей вернуть (по умолчанию %d, максимум 100)", def))
}

func startField() obj {
	return num("Смещение для следующей страницы: берите next_start из прошлого ответа")
}
