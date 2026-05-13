# Миграция Action Handler → MCP-сервер (AiR_Landing)

> **Стандарт:** MCP 2025-03-26 (Streamable HTTP transport)  
> **Причина:** `action_handler.go` вынесен в `AiR_Common`; AiR_Landing берёт на себя роль MCP-сервера

---

## Статус реализации (2026-05-13)

| Шаг | Статус | Описание |
|-----|--------|---------|
| MCP пакет `internal/app/mcp/` | ✅ **Готово** | `handler.go`, `session.go`, `tools.go` |
| `POST /mcp` зарегистрирован | ✅ **Готово** | `routes.go` → `w.mcpHandler.ServeHTTP` |
| Блок `/action` удалён | ✅ **Готово** | Из `routes.go` полностью убран |
| `GET /s3/:id/:filename` перенесён | ✅ **Готово** | Публичный маршрут вне `/action` |
| `user_id` убран из аргументов инструментов | ✅ **Готово** | Схемы `GetTools` и промпты `BuildEnhancedPrompt` очищены |
| `RunAction` принимает `userId uint32` явно | ✅ **Готово** | Сигнатура и все провайдеры обновлены |
| `AiR_Common` переключён на `/mcp` | ✅ **Готово** | `action_handler.go` — `callMCP` + `RunAction.default` |
| Промпт-билдеры обновлены | ✅ **Готово** | `user_id` убран из инструкций по вызову инструментов |

---

## 1. Контекст и текущая архитектура

### Как было (до миграции)

```
[AiR_Common / action_handler.go]
  UniversalActionHandler.RunAction()
    → case "get_s3_files":      HTTP GET  http://localhost:{port}/action/gets3?id={encoded}
    → case "create_file":       HTTP POST http://localhost:{port}/action/savefilein3
    → case "save_image":        HTTP POST http://localhost:{port}/action/saveImageInS3
    → case "get_current_time":  HTTP GET  http://localhost:{port}/action/time/current?id={encoded}
    → case "calendar_create":   HTTP POST http://localhost:{port}/action/calendar/create
    → case "calendar_list":     HTTP GET  http://localhost:{port}/action/calendar/list
    → case "calendar_delete":   HTTP DELETE http://localhost:{port}/action/calendar/delete
    → case "calendar_get":      HTTP GET  http://localhost:{port}/action/calendar/get
    → case "sheets_read_range": HTTP GET  http://localhost:{port}/action/sheets/read
    → case "sheets_write_range": HTTP POST http://localhost:{port}/action/sheets/write
    → case "sheets_append_range": HTTP POST http://localhost:{port}/action/sheets/append
```

**Проблемы старой архитектуры:**
- Каждый вызов инструмента — HTTP self-call (localhost→localhost): лишний сетевой переход + сериализация
- `user_id` передавался **закодированным** (`crypto.EncodeUint32`), gin-хендлеры сами декодировали
- Жёсткая привязка к конкретным URL и HTTP-методам

### Как работает сейчас (после миграции)

```
[AiR_Common / action_handler.go]  ← НУЖНО ОБНОВИТЬ (см. раздел 13)
  UniversalActionHandler.RunAction()
    → HTTP POST http://localhost:{port}/mcp
      Headers: X-Session-ID: "{realUserId}:{providerType}"
      Body: {"jsonrpc":"2.0","id":"1","method":"tools/call","params":{"name":"get_s3_files","arguments":{}}}

[AiR_Landing / internal/app/mcp/]  ← УЖЕ РЕАЛИЗОВАНО
  POST /mcp → Handler.ServeHTTP()
    → initialize               → хендшейк, возврат capabilities
    → notifications/initialized → уведомление, HTTP 202
    → tools/list               → buildToolsList(userId, provider) по флагам модели из БД
    → tools/call               → прямой вызов сервисов без HTTP round-trip
```

---

## 2. MCP-протокол (стандарт 2025-03-26)

### Транспорт: Streamable HTTP

| Метод | URL | Назначение |
|-------|-----|-----------|
| `POST` | `/mcp` | Основной транспорт: JSON-RPC запросы и ответы |
| `GET`  | `/mcp` | SSE-стрим (опционально, для стриминга прогресса) |
| `DELETE` | `/mcp` | Завершение сессии |

На первом этапе реализован только `POST /mcp`.

### Формат JSON-RPC 2.0

**Запрос:**
```json
{
  "jsonrpc": "2.0",
  "id": "1",
  "method": "tools/call",
  "params": {
    "name": "get_s3_files",
    "arguments": {}
  }
}
```

**Ответ (успех):**
```json
{
  "jsonrpc": "2.0",
  "id": "1",
  "result": {
    "content": [
      {
        "type": "text",
        "text": "[\"https://example.com/s3/123/file1.txt\"]"
      }
    ],
    "isError": false
  }
}
```

**Ответ (ошибка инструмента):**
```json
{
  "jsonrpc": "2.0",
  "id": "1",
  "result": {
    "content": [{ "type": "text", "text": "{\"error\": \"описание ошибки\"}" }],
    "isError": true
  }
}
```

**Ошибка протокола (RPC-уровень):**
```json
{
  "jsonrpc": "2.0",
  "id": "1",
  "error": { "code": -32600, "message": "Invalid Request" }
}
```

**Уведомление (нет поля `id`, ответ не требуется):**
```json
{
  "jsonrpc": "2.0",
  "method": "notifications/initialized"
}
```

### Методы MCP

| Метод | Тип | Описание |
|-------|-----|---------|
| `initialize` | запрос | Хендшейк: клиент передаёт версию, сервер возвращает `serverInfo` и `capabilities` |
| `notifications/initialized` | уведомление | Клиент подтверждает завершение хендшейка. **Ответ не отправляется** (нет `id`) |
| `tools/list` | запрос | Возвращает список доступных инструментов с JSON Schema параметров |
| `tools/call` | запрос | Вызов инструмента по имени с аргументами |

> ⚠️ **Уведомления** — JSON-RPC сообщения без поля `id`. Сервер **не должен** отвечать на них.  
> При получении `notifications/initialized` — просто вернуть HTTP 202 без тела.

---

## 3. Идентификация пользователя и сессии

### Разграничение двух концепций

| Заголовок | Назначение | Кто устанавливает |
|-----------|------------|------------------|
| `X-Session-ID: "42:1"` | **Идентификация пользователя** (userId:providerType) — наш custom header | `AiR_Common` при каждом запросе |
| `Mcp-Session-Id: <uuid>` | **MCP сессия** — стандартный заголовок, сервер выдаёт после `initialize` | MCP сервер (AiR_Landing) |

Для первого этапа (stateless сервер) — `Mcp-Session-Id` не используется.

### Заголовок `X-Session-ID`

Формат: `"userId:providerType"`, например `"42:1"` (userId=42, provider=OpenAI)

- `userId` — **реальный** (не кодированный) userId `uint32`
- `providerType` — числовое значение `create.ProviderType` (1=OpenAI, 2=Mistral, 3=Google)

> ⚠️ **Важное отличие от старого `/action`:** старые gin-хендлеры принимали **закодированный** `user_id` через `crypto.EncodeUint32()` и сами декодировали через `crypto.DecodeUint32()`. MCP-хендлер получает **реальный** userId напрямую из `X-Session-ID` — кодирование не нужно.

> ✅ userId из `X-Session-ID` используется MCP сервером напрямую. **Инструменты не принимают `user_id` как параметр** — сервер подставляет его автоматически.

### S3 URL и кодирование

Маршрут `GET /s3/:id/:filename` (обработчик `ServeS3File`) ожидает **закодированный** id в URL.  
MCP-сервер при генерации S3 URL применяет `crypto.EncodeUint32(userId)`:

```go
// internal/app/mcp/tools.go
func (h *Handler) s3BaseURL(userId uint32) (baseURL string, encodedId uint64) {
    encodedId = crypto.EncodeUint32(userId)
    baseURL = fmt.Sprintf("https://%s/s3", h.conf.WEB.RealUrl)
    return
}
```

---

## 4. Реализованные файлы и структура

```
internal/app/mcp/
├── handler.go   ← Handler struct, New(), ServeHTTP(), JSON-RPC диспетчер
├── session.go   ← ParseSessionID() — парсинг X-Session-ID
└── tools.go     ← buildToolsList(), callTool(), 11 реализаций инструментов
```

### 4.1 Интерфейсы пакета `mcp`

```go
// DB — минимальный интерфейс к БД (web.DB удовлетворяет этому интерфейсу)
type DB interface {
    comdb.Exterior                            // для google_services
    UserTimeZone(userId uint32) (string, error) // для get_current_time
}

// ModelStore — интерфейс к хранилищу моделей
type ModelStore interface {
    GetUserModels(userId uint32) ([]create.UniversalModelData, error)
}
```

### 4.2 Инициализация в `web.New()`

```go
// internal/app/web/routes.go
mcpHandler: mcp.New(ctx, d, conf, m),
```

### 4.3 Вызов инструментов — сравнение

| Инструмент | Старый путь (`/action`) | Новый путь (MCP) |
|-----------|------------------------|-----------------|
| `get_s3_files` | `GET /action/gets3?id={encoded}` | `os.ReadDir("/var/www/s3/{userId}")` |
| `create_file` | `POST /action/savefilein3` | `os.Create(...)` + `WriteString(...)` |
| `save_image` | `POST /action/saveImageInS3` | `base64.Decode` + `os.WriteFile(...)` |
| `get_current_time` | `GET /action/time/current?id={encoded}` | `db.UserTimeZone(userId)` + `time.Now().In(loc)` |
| `calendar_create` | `POST /action/calendar/create` | `google_services.NewCalendarService(...).CreateEvent(...)` |
| `calendar_list` | `GET /action/calendar/list` | `google_services.NewCalendarService(...).ListEvents(...)` |
| `calendar_delete` | `DELETE /action/calendar/delete` | `google_services.NewCalendarService(...).DeleteEvent(...)` |
| `calendar_get` | `GET /action/calendar/get` | `google_services.NewCalendarService(...).GetEvent(...)` |
| `sheets_read` | `GET /action/sheets/read` | `google_services.NewSheetsService(...).ReadRange(...)` |
| `sheets_write` | `POST /action/sheets/write` | `google_services.NewSheetsService(...).WriteRange(...)` |
| `sheets_append` | `POST /action/sheets/append` | `google_services.NewSheetsService(...).AppendRange(...)` |

---

## 5. Изменения в `routes.go` (выполнено)

```go
// Web struct — добавлено поле
type Web struct {
    // ...
    mcpHandler *mcp.Handler
}

// New() — инициализация
mcpHandler: mcp.New(ctx, d, conf, m),

// Handler() — регистрация маршрутов
w.Gin.POST("/mcp", w.mcpHandler.ServeHTTP)   // MCP-сервер
w.Gin.GET("/s3/:id/:filename", w.ServeS3File) // раздача S3-файлов (публичный)

// Блок /action — УДАЛЁН
```

---

## 6. Схемы инструментов (JSON Schema Draft 7)

> ✅ `user_id` **не включается** в схемы — берётся из `X-Session-ID` автоматически.

### `get_s3_files`
```json
{ "type": "object", "properties": {}, "required": [] }
```

### `create_file`
```json
{
  "type": "object",
  "properties": {
    "file_name": { "type": "string", "description": "Имя файла с расширением" },
    "content":   { "type": "string", "description": "Содержимое файла (UTF-8)" }
  },
  "required": ["file_name", "content"]
}
```

### `save_image`
```json
{
  "type": "object",
  "properties": {
    "image_data": { "type": "string", "description": "base64-кодированное изображение" },
    "file_name":  { "type": "string", "description": "Имя файла (.jpg, .png)" }
  },
  "required": ["image_data", "file_name"]
}
```

### `get_current_time`
```json
{ "type": "object", "properties": {}, "required": [] }
```

### `calendar_create`
```json
{
  "type": "object",
  "properties": {
    "title":       { "type": "string" },
    "description": { "type": "string" },
    "start_time":  { "type": "string", "description": "RFC3339, например 2026-05-07T10:00:00+03:00" },
    "end_time":    { "type": "string", "description": "RFC3339" },
    "location":    { "type": "string" },
    "attendees":   { "type": "array", "items": { "type": "string" } }
  },
  "required": ["title", "start_time", "end_time"]
}
```

### `calendar_list`
```json
{
  "type": "object",
  "properties": {
    "time_min":    { "type": "string", "description": "RFC3339" },
    "time_max":    { "type": "string", "description": "RFC3339" },
    "max_results": { "type": "integer", "default": 10 }
  },
  "required": []
}
```

### `calendar_delete`
```json
{
  "type": "object",
  "properties": { "event_id": { "type": "string" } },
  "required": ["event_id"]
}
```

### `calendar_get`
```json
{
  "type": "object",
  "properties": { "event_id": { "type": "string" } },
  "required": ["event_id"]
}
```

### `sheets_read`
```json
{
  "type": "object",
  "properties": {
    "spreadsheet_id": { "type": "string" },
    "range":          { "type": "string", "description": "Например Sheet1!A1:D10" }
  },
  "required": ["spreadsheet_id", "range"]
}
```

### `sheets_write`
```json
{
  "type": "object",
  "properties": {
    "spreadsheet_id": { "type": "string" },
    "range":          { "type": "string" },
    "values":         { "type": "array", "items": { "type": "array" } }
  },
  "required": ["spreadsheet_id", "range", "values"]
}
```

### `sheets_append`
```json
{
  "type": "object",
  "properties": {
    "spreadsheet_id": { "type": "string" },
    "range":          { "type": "string" },
    "values":         { "type": "array", "items": { "type": "array" } }
  },
  "required": ["spreadsheet_id", "range", "values"]
}
```

---

## 7. Хендшейк и ответ `initialize`

```
Клиент (AiR_Common)                    Сервер (AiR_Landing / MCP)
        │                                       │
        │  POST /mcp                            │
        │  { "method": "initialize", ... }  ───►│
        │◄─── { "result": capabilities } ───────│
        │                                       │
        │  POST /mcp                            │
        │  { "method": "notifications/initialized" } ───►│
        │◄─── HTTP 202 (нет тела) ──────────────│
        │                                       │
        │  POST /mcp { "method": "tools/list" } ───►│
        │◄─── { "result": { "tools": [...] } } ─│
        │                                       │
        │  POST /mcp { "method": "tools/call" } ───►│
        │◄─── { "result": { "content": [...] } }│
```

---

## 8. Флаги модели → доступные инструменты

Метод: `mod.GetUserModels(userId)` → поиск по `Provider == providerType`

| Флаг модели | Инструменты |
|------------|------------|
| `model.S3 == true` | `get_s3_files`, `create_file`, `save_image` |
| `model.GOAuth.Calendar == true` | `calendar_create`, `calendar_list`, `calendar_delete`, `calendar_get` |
| `model.GOAuth.Sheets == true` | `sheets_read`, `sheets_write`, `sheets_append` |
| всегда | `get_current_time` |

---

## 9. Аутентификация `/mcp`

`POST /mcp` работает **без JWT middleware** — идентификация через `X-Session-ID`.  
Рекомендуется дополнительно ограничить доступ по IP (`127.0.0.1`) на уровне nginx/firewall.

| Вариант | Плюсы | Минусы |
|---------|-------|--------|
| Только `X-Session-ID` (текущий) | Совместимо с `AiR_Common` без изменений | Нет проверки подлинности запроса |
| `X-Session-ID` + `X-MCP-Secret` | Защита от внешних запросов | Нужно хранить секрет в конфиге |
| Bearer JWT | Единая авторизация | Нужно обновлять `AiR_Common` |

---

## 10. Коды ошибок JSON-RPC

| Код | Когда |
|-----|-------|
| `-32700` | Невалидный JSON |
| `-32600` | Нет `jsonrpc` или `method` |
| `-32601` | Неизвестный метод MCP |
| `-32602` | Неверные аргументы инструмента |
| `-32603` | Внутренняя ошибка сервера |
| `-32001` | Нет/невалидный `X-Session-ID` |
| `-32002` | Инструмент недоступен для модели |

---

## 11. Примеры curl для тестирования

```bash
# 1. Хендшейк
curl -k -X POST https://localhost:8443/mcp \
  -H "Content-Type: application/json" \
  -d '{"jsonrpc":"2.0","id":"1","method":"initialize","params":{"protocolVersion":"2025-03-26","clientInfo":{"name":"AiR-Common","version":"1.0.0"},"capabilities":{}}}'

# 2. Уведомление (ожидаем HTTP 202 без тела)
curl -k -X POST https://localhost:8443/mcp \
  -H "Content-Type: application/json" \
  -d '{"jsonrpc":"2.0","method":"notifications/initialized"}'

# 3. Список инструментов
curl -k -X POST https://localhost:8443/mcp \
  -H "Content-Type: application/json" \
  -H "X-Session-ID: 42:1" \
  -d '{"jsonrpc":"2.0","id":"2","method":"tools/list","params":{}}'

# 4. Вызов get_s3_files
curl -k -X POST https://localhost:8443/mcp \
  -H "Content-Type: application/json" \
  -H "X-Session-ID: 42:1" \
  -d '{"jsonrpc":"2.0","id":"3","method":"tools/call","params":{"name":"get_s3_files","arguments":{}}}'

# 5. Вызов get_current_time
curl -k -X POST https://localhost:8443/mcp \
  -H "Content-Type: application/json" \
  -H "X-Session-ID: 42:1" \
  -d '{"jsonrpc":"2.0","id":"4","method":"tools/call","params":{"name":"get_current_time","arguments":{}}}'

# 6. Вызов calendar_list
curl -k -X POST https://localhost:8443/mcp \
  -H "Content-Type: application/json" \
  -H "X-Session-ID: 42:3" \
  -d '{"jsonrpc":"2.0","id":"5","method":"tools/call","params":{"name":"calendar_list","arguments":{"time_min":"2026-05-01T00:00:00Z","max_results":5}}}'
```

---

## 12. Очистка старых файлов (опционально)

Следующие функции в `internal/app/web/` больше **не используются** ни одним маршрутом.  
Их можно удалить после стабилизации:

| Файл | Функции для удаления |
|------|---------------------|
| `google_calendar.go` | `CalendarCreateEvent`, `CalendarListEvents`, `CalendarDeleteEvent`, `CalendarGetEvent` |
| `google_sheets.go` | `SheetsReadRange`, `SheetsWriteRange`, `SheetsAppendRange` |
| `time.go` | `GetCurrentTime` |
| `s3.go` | `GetS3Files`, `CreateNewS3File`, `SaveImageToS3` |
| `web/mcp.go` | весь файл (stub, можно удалить) |

> ⚠️ `ServeS3File`, `GetUID`, `UploadFilesToS3`, `GetS3FilesDetailed`, `DeleteS3File` — оставить, они используются активными маршрутами.

---

## 13. Миграция `AiR_Common / action_handler.go` ✅

> **Выполнено 2026-05-13.** Все HTTP self-calls заменены единым `callMCP`.

### Итоговая архитектура

```go
// UniversalActionHandler — единый MCP-клиент
func (h *UniversalActionHandler) RunAction(ctx context.Context,
    functionName, arguments string,
    provider create.ProviderType, userId uint32) string {

    switch functionName {
    case "lead_target":
        // Внешний Meta-сервис — не через MCP, остаётся прямым HTTP-вызовом
        // POST http://localhost:8091/service/lead/target?rid={id}
        ...
    default:
        // ВСЕ остальные инструменты → единый POST /mcp
        return h.callMCP(ctx, functionName, arguments, provider, userId)
    }
}

func (h *UniversalActionHandler) callMCP(ctx context.Context,
    toolName, arguments string,
    provider create.ProviderType, userId uint32) string {

    // JSON-RPC запрос
    reqBody := {"jsonrpc":"2.0","id":"1","method":"tools/call",
                 "params":{"name":toolName,"arguments":args}}

    req.Header.Set("X-Session-ID", fmt.Sprintf("%d:%d", userId, provider))
    // userId — реальный uint32, без кодирования

    // Парсинг ответа: rpcResp.Result.Content[0].Text
}
```

### Ключевые отличия от старого кода

| Параметр | Старый подход | Новый (`/mcp`) |
|----------|--------------|----------------|
| `user_id` в теле/query | Передавался в каждом запросе | **Не передаётся** в аргументах |
| Идентификация пользователя | В теле/query каждого запроса | Заголовок `X-Session-ID: "userId:providerType"` |
| `provider` | Query-параметр `?provider=N` | В `X-Session-ID` (вторая часть) |
| URL | Уникальный для каждого инструмента | Всегда `POST /mcp` |
| HTTP-метод | GET / POST / DELETE | Всегда `POST` |
| Формат ответа | JSON напрямую | `result.content[0].text` |

---

## 14. Оставшиеся шаги

1. ✅ **`AiR_Common / action_handler.go`** — переключён на `POST /mcp`
2. ⏳ **Протестировать** через curl (раздел 11) и интеграционными тестами
3. ⏳ **Удалить неиспользуемые gin-хендлеры** из `web/` (раздел 12) — после стабилизации
4. ✅ **`MCPConfigProvider` интерфейс** — добавлен в `AiR_Common/pkg/model`
5. ✅ **`buildAgentConfiguration`** — переключён на MCP для получения tools и system prompt
6. ⏳ **Реализовать `prompts/get` в `AiR_Landing`** — раздел 15 (требуется для работы агентов)

> ⚠️ **Важно:** при недоступности MCP сервера модель работает только с `modelData.Prompt`
> (без function-tools и без hint). Локальный fallback-билдер **удалён** из `AiR_Common`.

---

## 15. Новый MCP-метод: `prompts/get`

Для того чтобы `UniversalModel` / `buildAgentConfiguration` получали system prompt динамически от MCP-сервера (а не строили его хардкодом в `AiR_Common`), необходимо реализовать метод `prompts/get` в `AiR_Landing/internal/app/mcp/handler.go`.

### 15.1 Метод `prompts/list`

**Запрос:**
```json
{
  "jsonrpc": "2.0",
  "id": "1",
  "method": "prompts/list",
  "params": {}
}
```

**Ответ:**
```json
{
  "jsonrpc": "2.0",
  "id": "1",
  "result": {
    "prompts": [
      {
        "name": "system",
        "description": "System prompt hint based on user model configuration"
      }
    ]
  }
}
```

### 15.2 Метод `prompts/get`

**Запрос:**
```json
{
  "jsonrpc": "2.0",
  "id": "1",
  "method": "prompts/get",
  "params": { "name": "system" }
}
```

**Ответ (успех):**
```json
{
  "jsonrpc": "2.0",
  "id": "1",
  "result": {
    "description": "System prompt hint for AI model",
    "messages": [
      {
        "role": "assistant",
        "content": {
          "type": "text",
          "text": "ВРЕМЯ: Используй get_current_time() для получения актуальной даты и времени.\nS3_FILES:\n..."
        }
      }
    ]
  }
}
```

### 15.3 Содержание промпта (логика в `AiR_Landing/internal/app/mcp/tools.go`)

Метод `buildSystemPromptHint(userId uint32, provider create.ProviderType) string` формирует **чистые инструкции по использованию инструментов** на основе флагов модели.

> ⚠️ **Требования к формату:**
> - **Без** артефактов text-mode: никаких `JSON: target=false`, `send_files=[]`, `Return: valid JSON`, `UID=...`, `operator=false (op=true if ask)` — это специфика text-режима, в который hint добавляется как дополнение к `modelData.Prompt`
> - Hint одинаково используется и для text-режима и для голосового (Realtime) режима — он должен содержать только операционные инструкции, понятные в обоих контекстах
> - Если флаги не установлены — возвращать пустую строку (нет инструкций = нет инструментов)

```go
// Пример реализации в AiR_Landing/internal/app/mcp/tools.go
func (h *Handler) buildSystemPromptHint(userId uint32, provider create.ProviderType) string {
    model := h.getUserModel(userId, provider) // из mod.GetUserModels(userId)

    var parts []string

    // get_current_time — всегда доступен
    parts = append(parts, "Time: always call get_current_time() for current date/time before any calculations.")

    if model.MetaAction != "" {
        parts = append(parts, fmt.Sprintf("Goal: %s", model.MetaAction))
    }

    if model.S3 {
        parts = append(parts,
            "Files: use get_s3_files() to list files, create_file() to create new ones.",
            "After create_file() — use the returned URL in your response. DO NOT invent URLs.",
        )
    }

    if model.Interpreter {
        parts = append(parts,
            "Code: use python tool for calculations and data processing only, NOT for creating user files.",
        )
        if model.S3 {
            parts = append(parts, "File creation for user → create_file(), NOT python.")
        }
    }

    if model.GOAuth.HasCalendar() {
        parts = append(parts,
            "Calendar: call get_current_time() BEFORE any calendar operation. Use RFC3339+timezone for dates.",
        )
    }

    if model.GOAuth.HasSheets() {
        parts = append(parts,
            "Sheets: ALWAYS call sheets_read_range() to get data — never say you cannot access it.",
            "Table data → show in message text, do NOT create files from table data.",
        )
    }

    if model.WebSearch {
        parts = append(parts, "Web: use web_search tool for current information.")
    }

    return strings.Join(parts, "\n")
}
```

### 15.4 Диспетчер в `handler.go`

```go
// В ServeHTTP / dispatch:
case "prompts/list":
    return h.handlePromptsList(w, req)
case "prompts/get":
    return h.handlePromptsGet(w, req, userId, provider)
```

### 15.5 Пример curl для тестирования `prompts/get`

```bash
curl -k -X POST https://localhost:8443/mcp \
  -H "Content-Type: application/json" \
  -H "X-Session-ID: 42:1" \
  -d '{"jsonrpc":"2.0","id":"6","method":"prompts/get","params":{"name":"system"}}'
```

---

## 16. Изменения в `AiR_Common` — полная универсальность `UniversalModel`

### 16.1 Новый интерфейс `MCPConfigProvider` (`pkg/model/model_router.go`)

```go
// MCPToolDefinition описание инструмента, полученное от MCP сервера
type MCPToolDefinition struct {
    Name        string      `json:"name"`
    Description string      `json:"description"`
    InputSchema interface{} `json:"inputSchema"` // JSON Schema параметров БЕЗ user_id
}

// MCPConfigProvider расширяет ActionHandler методами получения конфигурации от MCP
// Реализуется UniversalActionHandler
type MCPConfigProvider interface {
    ActionHandler
    // FetchToolsList получает список function-инструментов от MCP сервера (tools/list)
    FetchToolsList(ctx context.Context, userId uint32, provider create.ProviderType) ([]MCPToolDefinition, error)
    // FetchSystemPrompt получает system prompt hint от MCP сервера (prompts/get?name=system)
    FetchSystemPrompt(ctx context.Context, userId uint32, provider create.ProviderType) (string, error)
}
```

### 16.2 Новые методы в `UniversalActionHandler` (`pkg/model/action_handler.go`)

`FetchToolsList` вызывает `tools/list`:
```
POST /mcp
X-Session-ID: "userId:provider"
{"jsonrpc":"2.0","id":"1","method":"tools/list","params":{}}
```
→ парсит `result.tools[]` → возвращает `[]MCPToolDefinition`

`FetchSystemPrompt` вызывает `prompts/get`:
```
POST /mcp
X-Session-ID: "userId:provider"
{"jsonrpc":"2.0","id":"1","method":"prompts/get","params":{"name":"system"}}
```
→ парсит `result.messages[0].content.text` → возвращает `string`

### 16.3 Текущая реализация `buildAgentConfiguration` (`pkg/model/openai/model.go`)

> ✅ **Реализовано 2026-05-13.** Локальный fallback-билдер удалён.

```go
func (m *OpenAIModel) buildAgentConfiguration(userId uint32, config *OpenAIAgentConfig, compressedData []byte) error {
    // ...распаковка modelData...

    // MCP — единственный источник system prompt и function tools.
    // Если MCP недоступен: только modelData.Prompt, только нативные tools.
    mcpAvailable := false
    if mcpProvider, ok := m.actionHandler.(model.MCPConfigProvider); ok {
        if hint, err := mcpProvider.FetchSystemPrompt(m.ctx, userId, create.ProviderOpenAI); err == nil {
            config.SystemPrompt = modelData.Prompt + "\n\n" + hint
            mcpAvailable = true
        }
    }
    if !mcpAvailable {
        config.SystemPrompt = modelData.Prompt // без hint — нет инструкций по tools
    }

    var tools []interface{}
    // Нативные OpenAI tools — всегда локально, MCP их не возвращает
    if config.Interpreter { tools = append(tools, codeInterpreterTool) }
    if config.WebSearch   { tools = append(tools, webSearchTool) }

    // Function tools — только от MCP; если сервер недоступен — не добавляем
    if mcpAvailable {
        if mcpProvider, ok := m.actionHandler.(model.MCPConfigProvider); ok {
            if mcpTools, err := mcpProvider.FetchToolsList(m.ctx, userId, create.ProviderOpenAI); err == nil {
                for _, t := range mcpTools {
                    tools = append(tools, map[string]interface{}{
                        "type": "function", "name": t.Name,
                        "description": t.Description, "strict": false,
                        "parameters": t.InputSchema,
                    })
                }
            }
        }
    }
    config.Tools = tools

    // Response format — строится локально (не зависит от MCP)
    config.ResponseFormat = buildResponseFormat(config)
    return nil
}
```

### 16.4 Ключевые отличия нового подхода

| Параметр | Старый подход | Новый подход (MCP) |
|----------|--------------|-------------------|
| `user_id` в параметрах tools | Передавался как `"const"` в каждом инструменте | **Не передаётся** — MCP берёт из `X-Session-ID` |
| System prompt hint | Строился в `buildAgentConfiguration` локально | Получается от MCP `prompts/get` |
| Список function tools | Хардкодился локально по флагам модели | Получается от MCP `tools/list` |
| Нативные tools (code_interpreter, web_search) | Добавлялись локально | Добавляются локально (MCP их не возвращает) |
| Обновление инструкций и tools | Требует деплоя AiR_Common | Достаточно изменить AiR_Landing/mcp |
| Fallback при недоступности MCP | Локальный билдер | `modelData.Prompt` без tools — **функциональность недоступна** |

### 16.5 Порядок внедрения в AiR_Landing

1. В `internal/app/mcp/tools.go`: добавить `buildSystemPromptHint(userId, provider)` → формирует компактный prompt hint по флагам модели (раздел 15.3)
2. В `internal/app/mcp/handler.go`: добавить диспетчер для `prompts/list` и `prompts/get` (раздел 15.4)
3. Убедиться что `tools/list` возвращает инструменты **без** поля `user_id` в `inputSchema`
4. Протестировать curl (разделы 11 + 15.5)
