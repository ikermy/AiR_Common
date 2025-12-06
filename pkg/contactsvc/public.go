package contactsvc

import (
	"context"
	"encoding/json"
	"time"
)

// StartContactsServer запускает gRPC-сервер для приёма контактов
// register - функция регистрации gRPC-сервиса (определяется в целевом приложении)
// Эта функция может быть импортирована в другие проекты
func StartContactsServer(ctx context.Context, port int, db DB, register ServiceRegistrator) (*Server, error) {
	server := NewServer(port, db, register)
	if err := server.Start(ctx); err != nil {
		return nil, err
	}
	return server, nil
}

// StopContactsServer останавливает gRPC-сервер
func StopContactsServer(server *Server) {
	if server != nil {
		server.Stop()
	}
}

// NewContactsClient создаёт новый клиент для отправки контактов
// Эта функция может быть импортирована в другие проекты
func NewContactsClient(host string, port int, timeOut time.Duration) *Client {
	return NewClient(ClientConfig{
		Host:    host,
		Port:    port,
		Timeout: timeOut,
	})
}

// SendFinalResultToService отправляет финальный результат (контакты) на удалённый сервис
// contactsData должны быть JSON-сериализованными данными контактов
// Это удобная функция для отправки контактов, которая может быть импортирована в другие проекты
func SendFinalResultToService(ctx context.Context, client *Client, contactsData json.RawMessage) error {
	// Проверяем, подключен ли клиент, если нет - подключаемся
	if !client.IsConnected() {
		if err := client.Connect(); err != nil {
			return err
		}
	}

	// Отправляем контакты
	return client.SendFinalResult(ctx, contactsData)
}

// BatchSendContacts отправляет контакты несколькими попытками с повторами
func BatchSendContacts(ctx context.Context, client *Client, contactsData json.RawMessage, maxRetries int) error {
	var lastErr error

	for attempt := 1; attempt <= maxRetries; attempt++ {
		err := SendFinalResultToService(ctx, client, contactsData)
		if err == nil {
			return nil
		}

		lastErr = err
		if attempt < maxRetries {
			// Exponential backoff
			backoff := time.Duration(1<<uint(attempt-1)) * time.Second
			select {
			case <-time.After(backoff):
			case <-ctx.Done():
				return ctx.Err()
			}
		}
	}

	return lastErr
}

// RegisterServiceCallback - функция для регистрации gRPC-сервиса целевым приложением
// Целевое приложение должно создать свою реализацию этой функции
// Пример в целевом приложении (например, tg_user_bot):
//
// import "github.com/ikermy/TG_UserBot/proto/contacts"
// import "github.com/ikermy/AiR_Common/pkg/contactsvc"
//
//	func registerContactsService(grpcServer *grpc.Server, handler *contactsvc.ContactsServiceHandler) error {
//	    impl := &contacts.ContactsServiceImpl{Handler: handler}
//	    contacts.RegisterContactsServiceServer(grpcServer, impl)
//	    return nil
//	}
//
// Затем использовать при запуске:
// server, err := contactsvc.StartContactsServer(ctx, 50051, db, registerContactsService)
type RegisterServiceCallback = ServiceRegistrator
