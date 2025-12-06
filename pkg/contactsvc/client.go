package contactsvc

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"time"

	"github.com/ikermy/AiR_Common/pkg/contactsvc/pb"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

// ClientConfig конфигурация для подключения к удалённому сервису
type ClientConfig struct {
	Address string
	Timeout time.Duration
}

// Client структура для отправки контактов на удалённый сервер
type Client struct {
	mu      sync.Mutex
	config  ClientConfig
	conn    *grpc.ClientConn
	timeout time.Duration
}

// RealNewClient создаёт новый клиент для отправки контактов
func RealNewClient(config ClientConfig) *Client {
	if config.Timeout == 0 {
		config.Timeout = 30 * time.Second
	}

	return &Client{
		config:  config,
		timeout: config.Timeout,
	}
}

// Connect устанавливает соединение с удалённым сервером
func (c *Client) Connect() error {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.conn != nil {
		return nil // Уже подключен
	}

	// Создаём gRPC-соединение
	conn, err := grpc.NewClient(
		c.config.Address,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	if err != nil {
		return fmt.Errorf("ошибка при подключении к %s: %w", c.config.Address, err)
	}

	c.conn = conn
	return nil
}

// Close закрывает соединение
func (c *Client) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.conn != nil {
		err := c.conn.Close()
		c.conn = nil
		if err != nil {
			return err
		}
	}

	return nil
}

// SendFinalResult отправляет финальный результат (контакты) на удалённый сервер
// contactsData должны быть JSON-сериализованными данными контактов
func (c *Client) SendFinalResult(ctx context.Context, contactsData json.RawMessage) error {
	c.mu.Lock()
	conn := c.conn
	c.mu.Unlock()

	if conn == nil {
		return fmt.Errorf("соединение не установлено")
	}

	// Десериализуем JSON в FinalResult
	var finalResult pb.FinalResult
	if err := json.Unmarshal(contactsData, &finalResult); err != nil {
		return fmt.Errorf("ошибка при десериализации контактов: %w", err)
	}

	// Создаём gRPC-клиент
	client := pb.Client(conn)

	// Отправляем данные с таймаутом
	ctxWithTimeout, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()

	_, err := client.SendFinalResult(ctxWithTimeout, &finalResult)
	if err != nil {
		return fmt.Errorf("ошибка при отправке контактов: %w", err)
	}

	return nil
}

// IsConnected проверяет, установлено ли соединение
func (c *Client) IsConnected() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.conn != nil
}
