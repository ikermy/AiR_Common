// Package bff provides a gRPC client for the Landing ConfigService.
//
// Usage:
//
//	c, err := bff.New("landing:50051", "my-service-key")
//	if err != nil { ... }
//	defer c.Close()
//
//	mk, err := c.GetUserMasterKey(ctx, userId)
//	// codes.Unavailable — user not logged in since last Landing restart
package bff

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	bffproto "github.com/ikermy/AiR_Common/pkg/bff/proto"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
)

const (
	serviceKeyHeader = "x-service-key"
	defaultTimeout   = 5 * time.Second
)

// Client is a gRPC client for Landing's ConfigService.
// Thread-safe; intended to be created once and shared across the application.
type Client struct {
	conn       *grpc.ClientConn
	stub       bffproto.ConfigServiceClient
	serviceKey string
	timeout    time.Duration
}

// New creates a Client and establishes a connection to the Landing gRPC server.
func New() (*Client, error) {
	// Получаем адрес сервера из переменной окружения
	host := strings.TrimSpace(os.Getenv("GRPC_CONFIG_HOST"))
	// Читаем SERVICE_KEY из файла
	serviceKeyFile := strings.TrimSpace(os.Getenv("SERVICE_KEY_FILE"))

	serviceKeyData, err := os.ReadFile(serviceKeyFile)
	if err != nil {
		return nil, fmt.Errorf("ошибка чтения SERVICE_KEY из файла %s: %v", serviceKeyFile, err)
	}
	serviceKey := strings.TrimSpace(string(serviceKeyData))

	conn, err := grpc.NewClient(host, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		return nil, fmt.Errorf("bff.New: dial %s: %w", host, err)
	}
	return &Client{
		conn:       conn,
		stub:       bffproto.NewConfigServiceClient(conn),
		serviceKey: serviceKey,
		timeout:    defaultTimeout,
	}, nil
}

// Close releases the underlying gRPC connection.
func (c *Client) Close() error {
	return c.conn.Close()
}

// GetBotConfig returns decrypted Telegram bot settings from Landing.
func (c *Client) GetBotConfig(ctx context.Context) (*bffproto.BotConfigResponse, error) {
	ctx, cancel := context.WithTimeout(c.ctxWithKey(ctx), c.timeout)
	defer cancel()

	resp, err := c.stub.GetBotConfig(ctx, &bffproto.GetBotConfigRequest{})
	if err != nil {
		return nil, fmt.Errorf("bff.GetBotConfig: %w", err)
	}

	return resp, nil
}

// GetUserMasterKey returns the decrypted 32-byte MasterKey for the given user.
// The key is available only after the user has logged in at least once since
// the last Landing restart.
//
// Possible errors:
//   - codes.Unavailable — MasterKey not in Landing's cache (login required)
//   - codes.Unauthenticated / codes.PermissionDenied — invalid service key
func (c *Client) GetUserMasterKey(ctx context.Context, userId uint32) ([32]byte, error) {
	ctx, cancel := context.WithTimeout(c.ctxWithKey(ctx), c.timeout)
	defer cancel()

	resp, err := c.stub.GetUserMasterKey(ctx, &bffproto.GetUserMasterKeyRequest{UserId: userId})
	if err != nil {
		return [32]byte{}, fmt.Errorf("bff.GetUserMasterKey(user=%d): %w", userId, err)
	}

	if len(resp.MasterKey) != 32 {
		return [32]byte{}, fmt.Errorf("bff.GetUserMasterKey: invalid key length %d (expected 32)", len(resp.MasterKey))
	}

	var key [32]byte
	copy(key[:], resp.MasterKey)
	return key, nil
}

// ctxWithKey attaches the service key to outgoing gRPC metadata.
func (c *Client) ctxWithKey(ctx context.Context) context.Context {
	return metadata.AppendToOutgoingContext(ctx, serviceKeyHeader, c.serviceKey)
}
