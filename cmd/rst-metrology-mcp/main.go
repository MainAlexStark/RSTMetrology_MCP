// Команда rst-metrology-mcp — MCP-сервер для ФГИС «Аршин» Росстандарта
// (раздел «РСТ-метрология»): сведения о поверках и утверждённых типах СИ.
//
// Два транспорта, одна реализация протокола:
//
//   - stdio (по умолчанию) — Claude (или другой MCP-клиент) запускает
//     сервер подпроцессом на той же машине.
//
//   - Streamable HTTP (--http) — сервер живёт на VPS, к нему
//     подключаются агенты и личные устройства. Права определяются
//     токеном запроса.
//
// Сетевой сервер отказывается стартовать без настроенного способа
// авторизации — см. httpx.ErrNoAuth: открытый порт, через который
// кто угодно гоняет запросы к ФГИС с адреса сервера, — это в лучшем
// случае блокировка этого адреса Росстандартом.
//
// ФГИС отвечает только российским адресам: сервер надо запускать там,
// откуда она доступна, или задать HTTPS_PROXY / RST_BASE_URL.
//
// Переменные окружения:
//
//	RST_BASE_URL              адрес API (https://fgis.gost.ru/fundmetrology/eapi)
//	RST_MIN_INTERVAL_MS       пауза между запросами к ФГИС, мс (600; лимит ФГИС — 2 в секунду)
//	RST_TIMEOUT_SECONDS       таймаут одного запроса к ФГИС (25)
//	RST_MAX_REQUESTS_PER_CALL сколько запросов к ФГИС может сделать один вызов инструмента (8)
//	RST_ALLOW_WRITES          stdio: true разрешает пишущие инструменты (сейчас их нет)
//	RST_HTTP_ADDR             сетевой режим: адрес прослушивания, например :8083
//	RST_PUBLIC_URL            сетевой режим: внешний адрес, включает OAuth
//	RST_OWNER_PASSWORD        сетевой режим: пароль владельца (сервер хеширует его сам)
//	RST_OWNER_PASSWORD_HASH   сетевой режим: хеш пароля владельца (--hash-password)
//	RST_OAUTH_STORE           сетевой режим: файл хранилища OAuth
//	RST_RESOURCE_URL          сетевой режим: адрес MCP, если отличается от <public>/mcp
//	RST_TOKEN_READ            статический токен на чтение (агенты, автоматизация)
//	RST_TOKEN_WRITE           статический токен на чтение и запись
//	RST_ALLOWED_ORIGINS       сетевой режим: разрешённые Origin через запятую
//	RST_MAX_RESPONSE_BYTES    потолок размера ответа (40000)
//	HTTPS_PROXY               прокси до fgis.gost.ru, если сервер не в России
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/MainAlexStark/rst-metrology-mcp/arshin"
	"github.com/MainAlexStark/rst-metrology-mcp/internal/httpx"
	"github.com/MainAlexStark/rst-metrology-mcp/internal/mcp"
	"github.com/MainAlexStark/rst-metrology-mcp/internal/oauth"
	"github.com/MainAlexStark/rst-metrology-mcp/internal/tools"
)

// version подменяется при сборке: -ldflags "-X main.version=v0.1.0".
var version = "dev"

const defaultOAuthStore = "/var/lib/rst-metrology-mcp/oauth.json"

func main() {
	var (
		httpAddr     = flag.String("http", "", "поднять сетевой сервер на указанном адресе, например :8083")
		check        = flag.Bool("check", false, "проверить доступ к ФГИС «Аршин» и выйти")
		genToken     = flag.Bool("gen-token", false, "сгенерировать статический токен (для автоматизации)")
		hashPassFlag = flag.Bool("hash-password", false, "посчитать хеш пароля владельца для OAuth")
		grants       = flag.Bool("grants", false, "показать действующие подключения")
		revoke       = flag.String("revoke", "", "отозвать подключение по началу идентификатора выдачи")
		showVersion  = flag.Bool("version", false, "показать версию")
	)
	flag.Parse()

	switch {
	case *showVersion:
		fmt.Println("rst-metrology-mcp", version)
		return
	case *hashPassFlag:
		hashPassword()
		return
	case *genToken:
		printNewToken()
		return
	case *grants:
		listGrants()
		return
	case *revoke != "":
		revokeGrant(*revoke)
		return
	}

	cfg := arshin.DefaultConfig()
	cfg.BaseURL = env("RST_BASE_URL", cfg.BaseURL)
	cfg.MinInterval = time.Duration(envInt("RST_MIN_INTERVAL_MS", int(cfg.MinInterval/time.Millisecond))) * time.Millisecond
	cfg.Timeout = time.Duration(envInt("RST_TIMEOUT_SECONDS", int(cfg.Timeout/time.Second))) * time.Second
	cfg.UserAgent = "rst-metrology-mcp/" + version
	client := arshin.New(cfg)

	safety := tools.DefaultSafety()
	safety.MaxResponseBytes = envInt("RST_MAX_RESPONSE_BYTES", safety.MaxResponseBytes)
	safety.MaxRequestsPerCall = envInt("RST_MAX_REQUESTS_PER_CALL", safety.MaxRequestsPerCall)

	// В сетевом режиме режим доступа определяется токеном запроса,
	// поэтому RST_ALLOW_WRITES читается только для stdio.
	if envBool("RST_ALLOW_WRITES") {
		safety.Mode = tools.ModeWrite
	}

	srv := mcp.NewServer("rst-metrology-mcp", version)
	reg := tools.NewRegistry(client, safety, srv)
	reg.RegisterVerifications()
	reg.RegisterInstrument()
	reg.RegisterTypes()
	reg.RegisterDiagnostics(client.BaseURL())
	reg.RegisterResources()

	if *check {
		runCheck(client, srv, safety)
		return
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	addr := *httpAddr
	if addr == "" {
		addr = os.Getenv("RST_HTTP_ADDR")
	}
	if addr != "" {
		runHTTP(ctx, srv, safety, addr)
		return
	}

	runStdio(ctx, srv, safety)
}

// runStdio — локальный режим: клиент запускает сервер подпроцессом.
func runStdio(ctx context.Context, srv *mcp.Server, safety tools.Safety) {
	// Диагностика уходит в stderr: stdout занят протоколом.
	fmt.Fprintf(os.Stderr, "rst-metrology-mcp %s: stdio, %d инструментов, режим %s\n",
		version, len(srv.ToolNames()), safety.Mode)

	// Права кладутся в контекст и здесь, чтобы инструменты читали их
	// ровно из одного места независимо от транспорта.
	ctx = tools.WithSafety(ctx, safety)

	if err := srv.Serve(ctx, os.Stdin, os.Stdout); err != nil {
		fatal(err.Error())
	}
}

// runHTTP — сетевой режим.
func runHTTP(ctx context.Context, srv *mcp.Server, safety tools.Safety, addr string) {
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))

	auth := httpx.Auth{
		Static: httpx.NewStaticAuth(os.Getenv("RST_TOKEN_READ"), os.Getenv("RST_TOKEN_WRITE")),
	}

	// OAuth включается, как только задан внешний адрес: без него нельзя
	// построить ни метаданные, ни адреса эндпоинтов.
	if publicURL := os.Getenv("RST_PUBLIC_URL"); publicURL != "" {
		store, err := oauth.NewStore(env("RST_OAUTH_STORE", defaultOAuthStore))
		if err != nil {
			fatal("хранилище OAuth: " + err.Error())
		}
		if err := store.Cleanup(); err != nil {
			logger.Warn("не удалось почистить хранилище", "err", err)
		}

		oauthSrv, err := oauth.New(oauth.Config{
			Issuer:       publicURL,
			ResourceURL:  os.Getenv("RST_RESOURCE_URL"),
			Password:     os.Getenv("RST_OWNER_PASSWORD"),
			PasswordHash: os.Getenv("RST_OWNER_PASSWORD_HASH"),
			Store:        store,
			Logger:       logger,
		})
		if err != nil {
			fatal(err.Error())
		}
		auth.OAuth = oauthSrv

		logger.Info("OAuth включён",
			"issuer", oauthSrv.Issuer(),
			"resource", oauthSrv.ResourceURL())
	}

	http, err := httpx.NewServer(srv, auth, httpx.Config{
		Addr:           addr,
		AllowedOrigins: splitList(os.Getenv("RST_ALLOWED_ORIGINS")),
		Safety:         safety,
		Logger:         logger,
	})
	if err != nil {
		fatal(err.Error())
	}

	logger.Info("сетевой сервер запущен",
		"addr", addr,
		"tools", len(srv.ToolNames()),
		"oauth", auth.OAuth != nil,
		"static_tokens", auth.Static.Enabled())

	if auth.Static.Enabled() {
		logger.Warn("включены статические токены: они не истекают и не отзываются поштучно — " +
			"держите их только для автоматизации")
	}

	if err := http.ListenAndServe(ctx); err != nil {
		fatal(err.Error())
	}
}

func printNewToken() {
	token, err := httpx.GenerateToken()
	if err != nil {
		fatal(err.Error())
	}
	fmt.Println(token)
	fmt.Fprintln(os.Stderr,
		"\nЭто СТАТИЧЕСКИЙ токен: он не истекает и отзывается только сменой значения.\n"+
			"Он нужен агентам и прочей автоматизации, которая не может пройти экран\n"+
			"согласия. Для себя и своих устройств используйте OAuth — см. docs/OAUTH.md.\n\n"+
			"Задайте как RST_TOKEN_READ (чтение) или RST_TOKEN_WRITE (чтение и запись)\n"+
			"в окружении сервера, и тем же значением — в клиенте, который будет его предъявлять.")
}

// runCheck проверяет доступ к ФГИС до того, как сервер пропишут
// в клиенте: молчащий MCP внутри Claude отлаживать куда дольше.
func runCheck(client *arshin.HTTPClient, srv *mcp.Server, safety tools.Safety) {
	fmt.Printf("rst-metrology-mcp %s\n", version)
	fmt.Printf("Инструментов:          %d\n", len(srv.ToolNames()))
	fmt.Printf("Режим stdio:           %s\n", safety.Mode)
	fmt.Printf("API ФГИС:              %s\n", client.BaseURL())
	if p := os.Getenv("HTTPS_PROXY"); p != "" {
		fmt.Printf("HTTPS_PROXY:           задан\n")
	}
	fmt.Println()

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	fmt.Print("Проверяю доступ к ФГИС «Аршин»… ")
	page, err := client.SearchVerifications(ctx, arshin.VRIQuery{
		MITNumber: "16078-05", Year: time.Now().Year(), Rows: 1,
	})
	if err != nil {
		fmt.Println("не прошло")
		fmt.Printf("\n%v\n", err)
		if hint := arshin.Hint(err); hint != "" {
			fmt.Printf("\n%s\n", hint)
		}
		os.Exit(1)
	}
	fmt.Printf("есть доступ (поверок типа 16078-05 в этом году: %d)\n", page.Count)

	fmt.Println("\nСервер готов. Локально — пропишите в Claude как stdio;")
	fmt.Println("на все устройства — поднимите с --http, см. docs/REMOTE.md.")
}

func fatal(msg string) {
	fmt.Fprintln(os.Stderr, "rst-metrology-mcp:", msg)
	os.Exit(1)
}

func env(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func envBool(key string) bool {
	switch os.Getenv(key) {
	case "1", "true", "TRUE", "yes":
		return true
	}
	return false
}

func envInt(key string, def int) int {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return def
}

// splitList разбирает список значений через запятую.
func splitList(v string) []string {
	var out []string
	for _, item := range strings.Split(v, ",") {
		if item = strings.TrimSpace(item); item != "" {
			out = append(out, item)
		}
	}
	return out
}
