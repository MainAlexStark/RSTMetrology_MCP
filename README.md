# rst-metrology-mcp

MCP-сервер для ФГИС «Аршин» Росстандарта (раздел «РСТ-метрология»): агенты ищут сведения о поверках, проверяют, действует ли поверка конкретного прибора, и читают реестр утверждённых типов средств измерений.

Устроен по общему стандарту MCP-серверов ([`STANDARD.md`](STANDARD.md)), как `ozon-seller-mcp` и `k8s-mcp`.

## Что умеет

| Инструмент | Зачем |
|---|---|
| `search_verifications` | поиск поверок по типу, заводскому номеру, поверителю, периоду |
| `get_verification` | карточка поверки со сводкой: годен ли, до какого числа, кем поверен |
| `check_instrument` | вердикт по прибору: `valid` / `expired` / `inapplicable` / `cancelled` / … |
| `search_types` | поиск утверждённых типов СИ |
| `get_type` | карточка типа: изготовитель, МПИ, методика, срок свидетельства |
| `rst_status`, `rst_selftest` | состояние сервера и проверка доступа к ФГИС |

Подробно — [`docs/TOOLS.md`](docs/TOOLS.md); особенности API ФГИС — [`docs/ARSHIN.md`](docs/ARSHIN.md).

Все инструменты только читают. Ключ ФГИС не нужен.

## Важно до установки

**ФГИС отвечает только российским адресам.** Запускайте сервер там, откуда открывается `https://fgis.gost.ru/fundmetrology/cm/`, или задайте `HTTPS_PROXY` на прокси с российским адресом. Проверка:

```bash
make verify
```

**Лимит ФГИС — 2 запроса в секунду с адреса**, за превышение адрес блокируют. Сервер держит паузу между запросами сам (`RST_MIN_INTERVAL_MS`, 600 мс) и ограничивает число запросов на один вызов инструмента.

## Локально (stdio)

```bash
make build
```

Claude Desktop (`claude_desktop_config.json`):

```json
{
  "mcpServers": {
    "rst-metrology": {
      "command": "/путь/к/bin/rst-metrology-mcp"
    }
  }
}
```

Claude Code:

```bash
claude mcp add rst-metrology /путь/к/bin/rst-metrology-mcp
```

## На все устройства (VPS)

```bash
./deploy.sh rst-mcp.example.com
```

Подробно — [`docs/REMOTE.md`](docs/REMOTE.md), подключение устройств — [`docs/OAUTH.md`](docs/OAUTH.md).

## Переменные окружения

| Переменная | По умолчанию | Что делает |
|---|---|---|
| `RST_BASE_URL` | `https://fgis.gost.ru/fundmetrology/eapi` | адрес API |
| `HTTPS_PROXY` | — | прокси до ФГИС, если сервер не в России |
| `RST_MIN_INTERVAL_MS` | `600` | пауза между запросами к ФГИС |
| `RST_TIMEOUT_SECONDS` | `25` | таймаут одного запроса |
| `RST_MAX_REQUESTS_PER_CALL` | `8` | запросов к ФГИС на вызов инструмента |
| `RST_MAX_RESPONSE_BYTES` | `40000` | потолок размера ответа |
| `RST_ALLOW_WRITES` | `false` | stdio: разрешить запись (пишущих инструментов пока нет) |
| `RST_HTTP_ADDR` | — | сетевой режим: адрес прослушивания |
| `RST_PUBLIC_URL` | — | внешний адрес, включает OAuth |
| `RST_OWNER_PASSWORD` / `RST_OWNER_PASSWORD_HASH` | — | пароль владельца |
| `RST_OAUTH_STORE` | `/var/lib/rst-metrology-mcp/oauth.json` | клиенты и токены |
| `RST_RESOURCE_URL` | — | адрес MCP, если отличается от `<public>/mcp` |
| `RST_TOKEN_READ` / `RST_TOKEN_WRITE` | — | статические токены для автоматизации |
| `RST_ALLOWED_ORIGINS` | — | разрешённые `Origin` |

## Разработка

```bash
make check     # gofmt + vet + test -race
```

Тесты вызывают инструменты через настоящий протокол MCP поверх подставной ФГИС; клиент `arshin/` проверяется на `httptest`-сервере с ответами в форме из документации.
