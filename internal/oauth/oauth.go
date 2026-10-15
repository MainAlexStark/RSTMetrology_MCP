// Package oauth — встроенный сервер авторизации OAuth 2.1 для MCP.
//
// Зачем он здесь. До этой переделки сетевой режим не проверял вообще
// ничего: обработчик протокола отдавался наружу как есть — delete_pod,
// delete_deployment, patch_custom_resource и остальные write-инструменты
// мог вызвать кто угодно, кто дотянулся до порта 8082 (сосед по
// namespace, под с любым service account). Единственный набросок защиты
// был не собственным кодом, а закомментированным примером из go-sdk
// с пустыми client_id/client_secret — то есть нерабочим.
//
// Простой статический секрет в адресе решал бы задачу отчасти, но имеет
// три изъяна, которые нельзя починить, не сменив схему:
//
//  1. Спецификация MCP прямо запрещает передавать токен в строке запроса:
//     адреса попадают в журналы серверов, прокси и историю браузера.
//  2. Отозвать доступ можно было бы только сменой секрета — то есть
//     сразу у всех подключений разом (и у sre-agent, и у ноутбука).
//  3. Секрет жил бы вечно: утёкший однажды оставался бы годным.
//
// OAuth снимает все три: токены живут час, обновляются по refresh,
// отзываются поштучно, а согласие на конкретные права (только чтение
// или чтение и запись) даётся в момент подключения устройства.
//
// Сервер играет обе роли сразу — и resource server (сам сервер), и
// authorization server. Для одного владельца кластера это правильный
// размен: отдельный провайдер личности здесь только добавил бы деталей,
// способных сломаться. Пользователь ровно один — владелец сервер,
// поэтому «вход» это проверка одного пароля, а не система учётных
// записей.
package oauth

import (
	"log/slog"
	"net/http"
	"strings"
	"time"
)

// Области доступа.
//
// Разделение read/write выбирается не при настройке сервера, а в момент
// согласия: подключая личный ноутбук для быстрого просмотра логов,
// снимаете галочку записи — и получаете токен, которым нельзя ни удалить
// под, ни откатить деплой, даже если очень захочется.
const (
	ScopeRead  = "rst:read"
	ScopeWrite = "rst:write"

	// ScopeOfflineAccess запрашивается клиентом, чтобы получить refresh.
	// Без него доступ пришлось бы подтверждать заново каждый час.
	ScopeOfflineAccess = "offline_access"
)

// Времена жизни.
const (
	// AccessTokenTTL намеренно короткий: утёкший токен протухнет сам.
	AccessTokenTTL = time.Hour
	// RefreshTokenTTL — сколько устройство может не появляться,
	// прежде чем придётся подтверждать доступ заново.
	RefreshTokenTTL = 30 * 24 * time.Hour
	// AuthCodeTTL — код обменивается сразу, держать его долго незачем.
	AuthCodeTTL = 5 * time.Minute
	// LoginSessionTTL — сколько даётся на прохождение формы согласия.
	LoginSessionTTL = 15 * time.Minute
)

// Config — настройки сервера авторизации.
type Config struct {
	// Issuer — внешний адрес сервера без завершающего слэша,
	// например https://mcp.example.com. Он же используется как issuer
	// и как основа для всех эндпоинтов.
	Issuer string

	// ResourceURL — канонический адрес MCP-эндпоинта, каким его вводят
	// в Claude. Должен совпадать с ним в точности: по этому значению
	// клиент сверяет метаданные, и расхождение в один слэш ломает
	// подключение.
	ResourceURL string

	// PasswordHash — хеш пароля владельца (см. password.go).
	//
	// Задавать его вручную нужно только тогда, когда открытый пароль
	// не должен попадать в окружение сервера даже в файле с правами 600.
	PasswordHash string

	// Password — пароль владельца открытым текстом.
	//
	// Используется, когда хеш не задан: сервер считает его сам при
	// старте. Это стоит около четверти секунды один раз за запуск
	// и снимает с развёртывания отдельный шаг «сгенерируйте хеш
	// и вставьте строку» — шаг, который нельзя выполнить одной
	// командой и на котором чаще всего и застревают.
	Password string

	Store  *Store
	Logger *slog.Logger
}

// Server — сервер авторизации.
type Server struct {
	cfg    Config
	store  *Store
	logger *slog.Logger
}

// New создаёт сервер авторизации.
func New(cfg Config) (*Server, error) {
	if cfg.Issuer == "" {
		return nil, errConfig("не задан внешний адрес сервера (RST_PUBLIC_URL)")
	}
	// Хеш из открытого пароля считаем сами: это единственное место,
	// где он нужен, и считать его заранее человеку незачем.
	if cfg.PasswordHash == "" && cfg.Password != "" {
		hash, err := HashPassword(cfg.Password)
		if err != nil {
			return nil, err
		}
		cfg.PasswordHash = hash
	}
	if cfg.PasswordHash == "" {
		return nil, errConfig("не задан пароль владельца: положите его в RST_OWNER_PASSWORD " +
			"(сервер посчитает хеш сам) либо, если открытый пароль в окружении нежелателен, " +
			"посчитайте хеш через --hash-password и положите в RST_OWNER_PASSWORD_HASH")
	}
	if cfg.Store == nil {
		return nil, errConfig("не задано хранилище")
	}

	cfg.Issuer = strings.TrimSuffix(cfg.Issuer, "/")
	if cfg.ResourceURL == "" {
		cfg.ResourceURL = cfg.Issuer + "/mcp"
	}

	logger := cfg.Logger
	if logger == nil {
		logger = slog.Default()
	}

	return &Server{cfg: cfg, store: cfg.Store, logger: logger}, nil
}

// Issuer возвращает адрес издателя токенов.
func (s *Server) Issuer() string { return s.cfg.Issuer }

// ResourceURL возвращает канонический адрес защищаемого ресурса.
func (s *Server) ResourceURL() string { return s.cfg.ResourceURL }

// Mount регистрирует эндпоинты авторизации.
//
// Пути well-known дублируются с суффиксом пути ресурса: клиент сначала
// пробует /.well-known/oauth-protected-resource/mcp и лишь потом
// корневой вариант.
func (s *Server) Mount(mux *http.ServeMux) {
	mux.HandleFunc("GET /.well-known/oauth-protected-resource", s.protectedResourceMetadata)
	mux.HandleFunc("GET /.well-known/oauth-protected-resource/{path...}", s.protectedResourceMetadata)
	mux.HandleFunc("GET /.well-known/oauth-authorization-server", s.authorizationServerMetadata)
	mux.HandleFunc("GET /.well-known/oauth-authorization-server/{path...}", s.authorizationServerMetadata)

	mux.HandleFunc("POST /oauth/register", s.handleRegister)
	mux.HandleFunc("GET /oauth/authorize", s.handleAuthorize)
	mux.HandleFunc("POST /oauth/authorize", s.handleAuthorizeSubmit)
	mux.HandleFunc("POST /oauth/token", s.handleToken)
	mux.HandleFunc("POST /oauth/revoke", s.handleRevoke)
}

type configError struct{ msg string }

func errConfig(msg string) error     { return &configError{msg: msg} }
func (e *configError) Error() string { return "oauth: " + e.msg }
