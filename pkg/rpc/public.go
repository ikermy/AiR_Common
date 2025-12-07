package rpc

import (
	"context"
	"encoding/json"
	"errors"
	"time"
)

// ErrNoHandlers ошибка, возвращаемая когда не передано ни одного хандлера
var ErrNoHandlers = errors.New("требуется хотя бы один хандлер")

// NewServer создаёт новый экземпляр gRPC-сервера для приёма контактов
func NewServer(port string) *Server {
	return &Server{
		port: port,
	}
}

// Start запускает gRPC-сервер для приёма контактов
// Принимает вариативное количество ServiceHandler (могут быть разные типы хандлеров)
func (s *Server) Start(handlers ...ServiceHandler) error {
	if len(handlers) == 0 {
		return ErrNoHandlers
	}

	s.handlers = handlers

	if err := s.serverStart(); err != nil {
		return err
	}
	return nil
}

// Stop останавливает gRPC-сервер
func (s *Server) Stop() {
	s.serverStop()
}

// NewClient создаёт новый клиент для отправки контактов
func NewClient(addr string, timeOut time.Duration) *Client {
	config := Config{
		Address: addr,
		Timeout: timeOut,
	}

	return &Client{
		config:  config,
		timeout: config.Timeout,
	}
}

// SendFinalResult отправляет финальный результат (контакты) на удалённый сервис
// contactsData должны быть JSON-сериализованными данными контактов
func SendFinalResult(ctx context.Context, client *Client, contactsData json.RawMessage) error {
	// Проверяем, подключен ли клиент, если нет - подключаемся
	if !client.IsConnected() {
		if err := client.Connect(); err != nil {
			return err
		}
	}

	// Отправляем контакты
	return client.SendResult(ctx, contactsData)
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
