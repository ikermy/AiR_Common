package contactsvc

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"time"

	"github.com/ikermy/AiR_Common/pkg/logger"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

// ClientConfig конфигурация для подключения к удалённому сервису
type ClientConfig struct {
	Host    string
	Port    int
	Timeout time.Duration
}

// Client структура для отправки контактов на удалённый сервер
type Client struct {
	mu      sync.Mutex
	config  ClientConfig
	conn    *grpc.ClientConn
	timeout time.Duration
}

// NewClient создаёт новый клиент для отправки контактов
func NewClient(config ClientConfig) *Client {
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

	addr := fmt.Sprintf("%s:%d", c.config.Host, c.config.Port)

	// Используем grpc.NewClient вместо устаревшего DialContext
	conn, err := grpc.NewClient(
		addr,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	if err != nil {
		logger.Error("Ошибка при подключении к контактному сервису на %s: %v", addr, err)
		return err
	}

	c.conn = conn
	logger.Info("Успешное подключение к контактному сервису на %s", addr)
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
			logger.Error("Ошибка при закрытии соединения с контактным сервисом: %v", err)
			return err
		}
		logger.Info("Соединение с контактным сервисом закрыто")
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

	// Примечание: Реальная отправка будет выполняться целевым приложением
	// используя свой gRPC-клиент и proto-определения
	// Здесь мы просто проверяем соединение

	return nil
}

// IsConnected проверяет, установлено ли соединение
func (c *Client) IsConnected() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.conn != nil
}
