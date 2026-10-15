package arshin

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

// DefaultBaseURL — адрес публичного API.
const DefaultBaseURL = "https://fgis.gost.ru/fundmetrology/eapi"

// MaxRows — больше API не отдаёт за один запрос.
const MaxRows = 100

// Client — то, что нужно инструментам от ФГИС. Интерфейс, а не
// структура: тесты подставляют свою реализацию и проверяют, дошёл
// ли запрос до внешней системы.
type Client interface {
	SearchVerifications(ctx context.Context, q VRIQuery) (*VRIPage, error)
	// Verification возвращает карточку поверки. follow — пройти по
	// цепочке модификаций до действующей версии записи.
	Verification(ctx context.Context, id string, follow bool) (*Card, error)
	SearchTypes(ctx context.Context, q MITQuery) (*RawPage, error)
	Type(ctx context.Context, id string) (json.RawMessage, error)
}

// Config — настройки HTTP-клиента.
type Config struct {
	BaseURL string

	// MinInterval — пауза между любыми двумя запросами процесса.
	// Лимит ФГИС — 2 запроса в секунду с адреса; 600 мс дают запас
	// на неточность часов и параллельных клиентов за тем же NAT.
	MinInterval time.Duration

	// Timeout — на один HTTP-запрос. ФГИС отвечает медленно
	// (секунды на широкий поиск), но ждать дольше полуминуты значит
	// упереться в таймаут клиента MCP.
	Timeout time.Duration

	// Retries — сколько раз повторить на 429/5xx/сетевой сбой.
	Retries int

	// CacheTTL — сколько держать карточки в памяти. Карточка поверки
	// после публикации не меняется (изменение — это новая версия
	// с другим id), а повторный запрос той же карточки в одном
	// разговоре — обычное дело и съедает лимит частоты.
	CacheTTL time.Duration

	UserAgent string
	HTTP      *http.Client
}

// DefaultConfig — значения по умолчанию.
func DefaultConfig() Config {
	return Config{
		BaseURL:     DefaultBaseURL,
		MinInterval: 600 * time.Millisecond,
		Timeout:     25 * time.Second,
		Retries:     2,
		CacheTTL:    15 * time.Minute,
		UserAgent:   "rst-metrology-mcp (+https://github.com/MainAlexStark/rst-metrology-mcp)",
	}
}

// HTTPClient — клиент настоящего API.
type HTTPClient struct {
	cfg  Config
	http *http.Client

	mu   sync.Mutex // очередь запросов: держит паузу между ними
	last time.Time

	cache *ttlCache

	// sleep подменяется в тестах, чтобы не ждать паузы по-настоящему.
	sleep func(ctx context.Context, d time.Duration) error
}

// New создаёт клиента.
func New(cfg Config) *HTTPClient {
	def := DefaultConfig()
	if cfg.BaseURL == "" {
		cfg.BaseURL = def.BaseURL
	}
	cfg.BaseURL = strings.TrimRight(cfg.BaseURL, "/")
	if cfg.MinInterval < 0 {
		cfg.MinInterval = 0
	}
	if cfg.Timeout <= 0 {
		cfg.Timeout = def.Timeout
	}
	if cfg.Retries < 0 {
		cfg.Retries = 0
	}
	if cfg.UserAgent == "" {
		cfg.UserAgent = def.UserAgent
	}
	hc := cfg.HTTP
	if hc == nil {
		// Proxy из окружения: из-за границы до ФГИС не достать, и
		// HTTPS_PROXY на российский адрес — штатный способ это обойти.
		hc = &http.Client{Transport: &http.Transport{Proxy: http.ProxyFromEnvironment}}
	}
	return &HTTPClient{cfg: cfg, http: hc, cache: newTTLCache(cfg.CacheTTL, 512), sleep: sleepCtx}
}

// BaseURL — адрес API, к которому ходит клиент.
func (c *HTTPClient) BaseURL() string { return c.cfg.BaseURL }

// SearchVerifications ищет поверки.
func (c *HTTPClient) SearchVerifications(ctx context.Context, q VRIQuery) (*VRIPage, error) {
	p := url.Values{}
	set(p, "search", q.Search)
	set(p, "mit_number", q.MITNumber)
	set(p, "mit_title", q.MITTitle)
	set(p, "mit_notation", q.MITNotation)
	set(p, "mi_modification", q.MIModification)
	set(p, "mi_number", q.MINumber)
	set(p, "org_title", q.OrgTitle)
	if q.Year > 0 {
		p.Set("year", strconv.Itoa(q.Year))
	}
	set(p, "verification_date_start", q.DateFrom)
	set(p, "verification_date_end", q.DateTo)
	if q.Applicable != nil {
		p.Set("applicability", strconv.FormatBool(*q.Applicable))
	}
	set(p, "sort", q.Sort)
	pageParams(p, q.Rows, q.Start)
	for k, v := range q.Extra {
		set(p, k, v)
	}

	var page VRIPage
	if err := c.getResult(ctx, "/vri", p, &page); err != nil {
		return nil, err
	}
	for i := range page.Items {
		page.Items[i].VerificationDate = isoDate(page.Items[i].VerificationDate)
		page.Items[i].ValidDate = isoDate(page.Items[i].ValidDate)
	}
	return &page, nil
}

// maxChain — сколько версий записи проходить по vriVerIdNext. Цепочка
// длиннее — почти наверняка петля в данных, а не жизнь.
const maxChain = 5

// Verification возвращает карточку поверки.
func (c *HTTPClient) Verification(ctx context.Context, id string, follow bool) (*Card, error) {
	id = strings.TrimSpace(id)
	if id == "" {
		return nil, errors.New("не задан идентификатор поверки vri_id")
	}

	var chain []string
	seen := map[string]bool{}
	for hop := 0; ; hop++ {
		raw, err := c.cardRaw(ctx, id)
		if err != nil {
			return nil, err
		}
		sum, err := ParseCard(id, raw)
		if err != nil {
			return nil, err
		}
		seen[id] = true
		chain = append(chain, id)

		next := sum.NextVRIID
		if !follow || !sum.Modified || next == "" || seen[next] || hop+1 >= maxChain {
			card := &Card{Summary: sum, Raw: raw}
			if len(chain) > 1 {
				card.Chain = chain
			}
			return card, nil
		}
		id = next
	}
}

func (c *HTTPClient) cardRaw(ctx context.Context, id string) (json.RawMessage, error) {
	key := "vri/" + id
	if raw, ok := c.cache.get(key); ok {
		return raw, nil
	}
	var raw json.RawMessage
	if err := c.getResult(ctx, "/vri/"+url.PathEscape(id), nil, &raw); err != nil {
		return nil, err
	}
	c.cache.put(key, raw)
	return raw, nil
}

// SearchTypes ищет утверждённые типы СИ.
func (c *HTTPClient) SearchTypes(ctx context.Context, q MITQuery) (*RawPage, error) {
	p := url.Values{}
	set(p, "search", q.Search)
	pageParams(p, q.Rows, q.Start)
	for k, v := range q.Extra {
		set(p, k, v)
	}
	var page RawPage
	if err := c.getResult(ctx, "/mit", p, &page); err != nil {
		return nil, err
	}
	return &page, nil
}

// Type возвращает карточку утверждённого типа по mit_uuid.
func (c *HTTPClient) Type(ctx context.Context, id string) (json.RawMessage, error) {
	id = strings.TrimSpace(id)
	if id == "" {
		return nil, errors.New("не задан идентификатор типа mit_uuid")
	}
	key := "mit/" + id
	if raw, ok := c.cache.get(key); ok {
		return raw, nil
	}
	var raw json.RawMessage
	if err := c.getResult(ctx, "/mit/"+url.PathEscape(id), nil, &raw); err != nil {
		return nil, err
	}
	c.cache.put(key, raw)
	return raw, nil
}

// getResult выполняет GET и раскладывает поле result ответа в out.
func (c *HTTPClient) getResult(ctx context.Context, path string, q url.Values, out any) error {
	body, err := c.get(ctx, path, q)
	if err != nil {
		return err
	}
	var env envelope
	if err := json.Unmarshal(body, &env); err != nil || len(env.Result) == 0 || string(env.Result) == "null" {
		return &APIError{Kind: KindBadResponse, Path: path, Body: snippet(body),
			Err: errors.New("в ответе нет поля result")}
	}
	if err := json.Unmarshal(env.Result, out); err != nil {
		return &APIError{Kind: KindBadResponse, Path: path, Body: snippet(env.Result), Err: err}
	}
	return nil
}

// get выполняет запрос с паузой и повторами.
func (c *HTTPClient) get(ctx context.Context, path string, q url.Values) ([]byte, error) {
	u := c.cfg.BaseURL + path
	if len(q) > 0 {
		u += "?" + q.Encode()
	}

	var lastErr error
	for attempt := 0; attempt <= c.cfg.Retries; attempt++ {
		if err := c.wait(ctx); err != nil {
			return nil, err
		}
		body, retryAfter, err := c.do(ctx, path, u)
		if err == nil {
			return body, nil
		}
		lastErr = err
		if !retryable(err) || attempt == c.cfg.Retries {
			break
		}
		delay := retryAfter
		if delay <= 0 {
			delay = time.Duration(attempt+1) * time.Second
		}
		if err := c.sleep(ctx, delay); err != nil {
			return nil, err
		}
	}
	return nil, lastErr
}

// do — один HTTP-запрос. Второе значение — пауза из Retry-After.
func (c *HTTPClient) do(ctx context.Context, path, u string) ([]byte, time.Duration, error) {
	ctx, cancel := context.WithTimeout(ctx, c.cfg.Timeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, 0, err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", c.cfg.UserAgent)

	resp, err := c.http.Do(req)
	if err != nil {
		if ctx.Err() != nil && !errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return nil, 0, ctx.Err()
		}
		return nil, 0, &APIError{Kind: KindNetwork, Path: path, Err: err}
	}
	defer resp.Body.Close()

	// 8 МБ — с запасом на сотню карточек, но не бесконечно: ответ
	// без ограничения однажды окажется HTML-страницей в сотни мегабайт.
	body, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return nil, 0, &APIError{Kind: KindNetwork, Path: path, Err: err}
	}

	switch code := resp.StatusCode; {
	case code == http.StatusOK:
		if !looksJSON(body) {
			return nil, 0, &APIError{Kind: KindBadResponse, Status: code, Path: path, Body: snippet(body)}
		}
		return body, 0, nil
	case code == http.StatusBadRequest || code == http.StatusRequestTimeout:
		return nil, 0, &APIError{Kind: KindTooBroad, Status: code, Path: path, Body: snippet(body)}
	case code == http.StatusNotFound:
		return nil, 0, &APIError{Kind: KindNotFound, Status: code, Path: path}
	case code == http.StatusTooManyRequests:
		return nil, retryAfter(resp.Header.Get("Retry-After")),
			&APIError{Kind: KindRateLimited, Status: code, Path: path}
	case code >= 500:
		return nil, 0, &APIError{Kind: KindServer, Status: code, Path: path, Body: snippet(body)}
	default:
		return nil, 0, &APIError{Kind: KindHTTP, Status: code, Path: path, Body: snippet(body)}
	}
}

// wait держит паузу между запросами всего процесса.
//
// Пауза общая, а не на вызов: модель охотно зовёт инструменты
// параллельно, и пять одновременных запросов — это 429 на всех пяти,
// а при повторении — блокировка адреса на часы.
func (c *HTTPClient) wait(ctx context.Context) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if d := c.cfg.MinInterval - time.Since(c.last); d > 0 {
		if err := c.sleep(ctx, d); err != nil {
			return err
		}
	}
	c.last = time.Now()
	return nil
}

func retryable(err error) bool {
	var e *APIError
	if !errors.As(err, &e) {
		return false
	}
	switch e.Kind {
	case KindRateLimited, KindServer:
		return true
	case KindNetwork:
		// Таймаут не повторяется: из-за границы соединение висит до
		// конца ожидания, и три попытки по 25 секунд — это уже
		// таймаут клиента MCP вместо понятного отказа с подсказкой.
		return !isTimeout(err)
	}
	return false
}

// retryAfter разбирает Retry-After в секундах, с потолком: ждать
// минуту внутри вызова инструмента — это таймаут клиента MCP.
func retryAfter(v string) time.Duration {
	n, err := strconv.Atoi(strings.TrimSpace(v))
	if err != nil || n <= 0 {
		return 2 * time.Second
	}
	return min(time.Duration(n)*time.Second, 5*time.Second)
}

func looksJSON(b []byte) bool {
	for _, ch := range b {
		switch ch {
		case ' ', '\n', '\r', '\t':
			continue
		case '{', '[':
			return true
		default:
			return false
		}
	}
	return false
}

func snippet(b []byte) string {
	const n = 200
	s := strings.Join(strings.Fields(string(b)), " ")
	if len(s) <= n {
		return s
	}
	cut := n
	for cut > 0 && !utf8.ValidString(s[:cut]) {
		cut--
	}
	return s[:cut] + "…"
}

func set(p url.Values, k, v string) {
	if v = strings.TrimSpace(v); v != "" {
		p.Set(k, v)
	}
}

func pageParams(p url.Values, rows, start int) {
	if rows > 0 {
		p.Set("rows", strconv.Itoa(min(rows, MaxRows)))
	}
	if start > 0 {
		p.Set("start", strconv.Itoa(start))
	}
}

func sleepCtx(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// String — для диагностики.
func (c *HTTPClient) String() string {
	return fmt.Sprintf("arshin(%s, пауза %s)", c.cfg.BaseURL, c.cfg.MinInterval)
}
