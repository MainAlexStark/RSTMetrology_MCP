package mcp

import (
	"context"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// Ресурсы — то, чем этот сервер отличается от эталонного
// ozon-seller-mcp, и отличие вынесено в отдельный файл намеренно:
// server.go остаётся общим для обоих серверов и сверяется построчно,
// а всё, что есть только здесь, лежит рядом и видно сразу.
//
// Инструмент модель вызывает сама, ресурс клиент читает по адресу —
// памятка по полям реестра нужна как раз вторым способом: её показывают
// в начале разговора, не тратя вызов инструмента.

// Resource — один ресурс, доступный клиенту по адресу.
type Resource struct {
	Name        string
	Description string
	URI         string
	MIMEType    string

	// Handler возвращает содержимое ресурса текстом.
	Handler func(ctx context.Context) (string, error)
}

// RegisterResource добавляет ресурс.
func (s *Server) RegisterResource(r Resource) {
	uri := r.URI
	handler := r.Handler

	s.impl.AddResource(&sdk.Resource{
		Name:        r.Name,
		Description: r.Description,
		URI:         uri,
		MIMEType:    r.MIMEType,
	}, func(ctx context.Context, _ *sdk.ReadResourceRequest) (*sdk.ReadResourceResult, error) {
		text, err := handler(ctx)
		if err != nil {
			return nil, err
		}
		return &sdk.ReadResourceResult{
			Contents: []*sdk.ResourceContents{{URI: uri, Text: text}},
		}, nil
	})
}
