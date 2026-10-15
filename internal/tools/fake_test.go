package tools

import (
	"context"
	"encoding/json"
	"sync"

	"github.com/MainAlexStark/rst-metrology-mcp/arshin"
)

// fakeFGIS — ФГИС для тестов.
//
// Подставной клиент, а не мок с ожиданиями: тесты проверяют, с какими
// фильтрами инструмент дошёл до ФГИС и сколько запросов сделал, — для
// этого достаточно записать вызовы.
type fakeFGIS struct {
	mu      sync.Mutex
	queries []arshin.VRIQuery
	cards   []string
	types   []string

	// search отвечает на поиск поверок; nil — пустая страница.
	search func(q arshin.VRIQuery) (*arshin.VRIPage, error)
	// card отвечает на карточку; nil — ошибка «нет записи».
	card     func(id string) (*arshin.Card, error)
	typePage *arshin.RawPage
	typeCard json.RawMessage
}

func (f *fakeFGIS) SearchVerifications(_ context.Context, q arshin.VRIQuery) (*arshin.VRIPage, error) {
	f.mu.Lock()
	f.queries = append(f.queries, q)
	f.mu.Unlock()
	if f.search == nil {
		return &arshin.VRIPage{}, nil
	}
	return f.search(q)
}

func (f *fakeFGIS) Verification(_ context.Context, id string, _ bool) (*arshin.Card, error) {
	f.mu.Lock()
	f.cards = append(f.cards, id)
	f.mu.Unlock()
	if f.card == nil {
		return nil, &arshin.APIError{Kind: arshin.KindNotFound, Status: 404, Path: "/vri/" + id}
	}
	return f.card(id)
}

func (f *fakeFGIS) SearchTypes(_ context.Context, q arshin.MITQuery) (*arshin.RawPage, error) {
	f.mu.Lock()
	f.types = append(f.types, "search:"+q.Search)
	f.mu.Unlock()
	if f.typePage == nil {
		return &arshin.RawPage{}, nil
	}
	return f.typePage, nil
}

func (f *fakeFGIS) Type(_ context.Context, id string) (json.RawMessage, error) {
	f.mu.Lock()
	f.types = append(f.types, "card:"+id)
	f.mu.Unlock()
	return f.typeCard, nil
}

func (f *fakeFGIS) requests() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.queries) + len(f.cards) + len(f.types)
}

func ptr[T any](v T) *T { return &v }
