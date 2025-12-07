# Руководство по подключению contactsvc

Это руководство описывает процесс подключения клиента и сервера contactsvc в стороннем приложении. Пакет contactsvc предоставляет gRPC-сервис для обмена контактами (людьми, ботами, каналами, группами и супергруппами) между приложениями.

## Требования

- Go 1.19 или выше
- Установленные зависимости: `google.golang.org/grpc`, `github.com/ikermy/AiR_Common/pkg/contactsvc`

## Подключение сервера

Сервер contactsvc позволяет принимать контакты по gRPC. Для запуска сервера в вашем приложении выполните следующие шаги:

1. Импортируйте пакет:
   ```go
   import "github.com/ikermy/AiR_Common/pkg/contactsvc"
   ```

2. Запустите сервер:
   ```go
   ctx := context.Background()
   server, err := contactsvc.Start(ctx, "50051") // порт 50051
   if err != nil {
       log.Fatal(err)
   }
   defer contactsvc.Stop(server)
   ```

3. Сервер будет слушать на указанном порту и принимать контакты через метод `SendFinalResult`.

4. Для доступа к полученным данным используйте:
   ```go
   handler := server.GetHandler()
   data := handler.GetData() // возвращает *pb.FinalResult
   ```

## Подключение клиента

Клиент contactsvc позволяет отправлять контакты на удалённый сервер. Для использования клиента в вашем приложении:

1. Импортируйте пакет:
   ```go
   import "github.com/ikermy/AiR_Common/pkg/contactsvc"
   ```

2. Создайте клиента:
   ```go
   client := contactsvc.NewClient("localhost:50051", 30*time.Second)
   ```

3. Подготовьте данные контактов в формате JSON (совместимом с `pb.FinalResult`):
   ```go
   contactsData := json.RawMessage(`{
       "humans": [
           {
               "id": 123,
               "first_name": "John",
               "last_name": "Doe",
               "username": "johndoe",
               "phone": "+1234567890",
               "service": 1
           }
       ],
       "bots": [],
       "channels": [],
       "groups": [],
       "supergroups": []
   }`)
   ```

4. Отправьте контакты:
   ```go
   ctx := context.Background()
   err := contactsvc.SendFinalResult(ctx, client, contactsData)
   if err != nil {
       log.Println("Ошибка отправки:", err)
   }
   ```

5. Для отправки с повторными попытками используйте `BatchSendContacts`:
   ```go
   err := contactsvc.BatchSendContacts(ctx, client, contactsData, 3) // до 3 попыток
   ```

## Типы данных

Контакты представлены следующими типами (из `pb.FinalResult`):

- `humans`: Люди
- `bots`: Боты
- `channels`: Каналы
- `groups`: Группы
- `supergroups`: Супергруппы

Каждый тип имеет поля: `id`, `title` (для каналов/групп), `username`, `phone` (для контактов), `service` (TELEGRAM=1, WHATSAPP=2).

### Структура Contact
```go
type Contact struct {
    Id        int64
    FirstName string
    LastName  string
    Username  string
    Phone     string
    Service   Service
}
```
- `Id`: Уникальный идентификатор контакта
- `FirstName`: Имя
- `LastName`: Фамилия
- `Username`: Имя пользователя (опционально)
- `Phone`: Номер телефона (опционально)
- `Service`: Источник (TELEGRAM=1, WHATSAPP=2)

### Структура Channel
```go
type Channel struct {
    Id       int64
    Title    string
    Username string
    Service  Service
}
```
- `Id`: Уникальный идентификатор канала
- `Title`: Название канала
- `Username`: Имя пользователя канала (опционально)
- `Service`: Источник

### Структура Group
```go
type Group struct {
    Id      int64
    Title   string
    Service Service
}
```
- `Id`: Уникальный идентификатор группы
- `Title`: Название группы
- `Service`: Источник

### Структура Supergroup
```go
type Supergroup struct {
    Id       int64
    Title    string
    Username string
    Service  Service
}
```
- `Id`: Уникальный идентификатор супергруппы
- `Title`: Название супергруппы
- `Username`: Имя пользователя (опционально)
- `Service`: Источник

### Структура FinalResult
```go
type FinalResult struct {
    Humans      []*Contact
    Bots        []*Contact
    Channels    []*Channel
    Groups      []*Group
    Supergroups []*Supergroup
}
```
Содержит массивы всех типов контактов.

## API Reference

### Функции для работы с сервером

#### Start(ctx context.Context, port string) (*Server, error)
Запускает gRPC-сервер для приёма контактов на указанном порту.

**Параметры:**
- `ctx`: Контекст для отмены операции
- `port`: Порт для прослушивания (например, "50051")

**Возвращает:**
- `*Server`: Экземпляр запущенного сервера
- `error`: Ошибка, если не удалось запустить сервер

**Пример:**
```go
ctx := context.Background()
server, err := contactsvc.Start(ctx, "50051")
if err != nil {
    log.Fatal(err)
}
defer contactsvc.Stop(server)
```

#### Stop(server *Server)
Останавливает gRPC-сервер.

**Параметры:**
- `server`: Экземпляр сервера для остановки (может быть nil)

**Пример:**
```go
contactsvc.Stop(server)
```

#### Server.GetHandler() *Handler
Возвращает обработчик сервера для доступа к полученным данным.

**Возвращает:**
- `*Handler`: Обработчик контактов

**Пример:**
```go
handler := server.GetHandler()
data := handler.GetData()
```

#### Handler.GetData() *pb.FinalResult
Возвращает последние полученные контакты.

**Возвращает:**
- `*pb.FinalResult`: Данные контактов или nil, если данных нет

**Пример:**
```go
data := handler.GetData()
if data != nil {
    fmt.Printf("Получено %d людей\n", len(data.Humans))
}
```

#### Handler.ClearData()
Очищает буфер полученных контактов.

**Пример:**
```go
handler.ClearData()
```

### Функции для работы с клиентом

#### NewClient(addr string, timeOut time.Duration) *Client
Создаёт новый клиент для отправки контактов.

**Параметры:**
- `addr`: Адрес сервера (например, "localhost:50051")
- `timeOut`: Таймаут для операций (например, 30*time.Second)

**Возвращает:**
- `*Client`: Экземпляр клиента

**Пример:**
```go
client := contactsvc.NewClient("localhost:50051", 30*time.Second)
```

#### SendFinalResult(ctx context.Context, client *Client, contactsData json.RawMessage) error
Отправляет финальный результат (контакты) на удалённый сервер.

**Параметры:**
- `ctx`: Контекст для отмены операции
- `client`: Экземпляр клиента
- `contactsData`: JSON-данные контактов в формате `json.RawMessage`

**Возвращает:**
- `error`: Ошибка отправки или nil при успехе

**Пример:**
```go
contactsData := json.RawMessage(`{"humans": [{"id": 123, "first_name": "John"}]}`)
err := contactsvc.SendFinalResult(ctx, client, contactsData)
```

#### BatchSendContacts(ctx context.Context, client *Client, contactsData json.RawMessage, maxRetries int) error
Отправляет контакты с повторными попытками при неудаче.

**Параметры:**
- `ctx`: Контекст для отмены операции
- `client`: Экземпляр клиента
- `contactsData`: JSON-данные контактов
- `maxRetries`: Максимальное количество попыток (например, 3)

**Возвращает:**
- `error`: Ошибка после всех попыток или nil при успехе

**Пример:**
```go
err := contactsvc.BatchSendContacts(ctx, client, contactsData, 3)
```

#### Client.Connect() error
Устанавливает соединение с сервером.

**Возвращает:**
- `error`: Ошибка подключения или nil при успехе

**Пример:**
```go
err := client.Connect()
```

#### Client.Close() error
Закрывает соединение с сервером.

**Возвращает:**
- `error`: Ошибка закрытия или nil при успехе

**Пример:**
```go
err := client.Close()
```

#### Client.IsConnected() bool
Проверяет, установлено ли соединение.

**Возвращает:**
- `bool`: true, если подключен, иначе false

**Пример:**
```go
if client.IsConnected() {
    fmt.Println("Подключен")
}
```

#### Client.SendFinalResult(ctx context.Context, contactsData json.RawMessage) error
Отправляет контакты (низкоуровневый метод клиента).

**Параметры:**
- `ctx`: Контекст
- `contactsData`: JSON-данные

**Возвращает:**
- `error`: Ошибка или nil

**Примечание:** Используйте публичную функцию `SendFinalResult` вместо этого метода.

## Примечания

- Сервер автоматически регистрирует gRPC-сервис и начинает прослушивание.
- Клиент автоматически подключается при первой отправке, если не подключен.
- Все операции потокобезопасны.
- Для продакшена рассмотрите использование TLS вместо insecure credentials.
