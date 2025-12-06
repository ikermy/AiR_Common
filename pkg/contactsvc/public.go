package contactsvc

import (
	"context"
	"encoding/json"
	"time"
)

// Start запускает gRPC-сервер для приёма контактов
// Эта функция может быть импортирована в другие проекты
func Start(ctx context.Context, port string) (*Server, error) {
	server := NewServer(port)
	if err := server.Start(ctx); err != nil {
		return nil, err
	}
	return server, nil
}

// Stop останавливает gRPC-сервер
func Stop(server *Server) {
	if server != nil {
		server.Stop()
	}
}

// NewClient создаёт новый клиент для отправки контактов
// Эта функция может быть импортирована в другие проекты
func NewClient(addr string, timeOut time.Duration) *Client {
	return RealNewClient(ClientConfig{
		Address: addr,
		Timeout: timeOut,
	})
}

// SendFinalResult отправляет финальный результат (контакты) на удалённый сервис
// contactsData должны быть JSON-сериализованными данными контактов
// Это удобная функция для отправки контактов, которая может быть импортирована в другие проекты
func SendFinalResult(ctx context.Context, client *Client, contactsData json.RawMessage) error {
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
		err := SendFinalResult(ctx, client, contactsData)
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
