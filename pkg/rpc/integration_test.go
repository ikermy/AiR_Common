package rpc

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/ikermy/AiR_Common/pkg/rpc/pb"
	"github.com/stretchr/testify/require"
)

// TestIntegrationSendContacts проверяет полный цикл отправки контактов
func TestIntegrationSendContacts(t *testing.T) {
	// Создаём сервер на порту 50052
	server := NewServer("50052")

	// Создаём LoggingHandler (аналог из вашего кода)
	handler := NewHandler()

	// Запускаем сервер
	err := server.Start(handler)
	require.NoError(t, err, "Сервер должен запуститься без ошибок")
	defer server.Stop()

	// Даём серверу время на инициализацию
	time.Sleep(500 * time.Millisecond)

	// Создаём клиент
	client := NewClient("localhost:50052", 30*time.Second)
	defer func() {
		_ = client.Close()
	}()

	// Подключаемся к серверу
	err = client.Connect()
	require.NoError(t, err, "Клиент должен подключиться к серверу")

	// Создаём тестовые данные контактов
	testData := pb.Result{
		Humans: []*pb.Contact{
			{
				Id:        12345,
				FirstName: "Test",
				LastName:  "Human",
				Username:  "testhuman",
				Phone:     "1234567890",
			},
		},
		Bots: []*pb.Contact{
			{
				Id:        54321,
				FirstName: "Test",
				LastName:  "Bot",
				Username:  "testbot",
				Phone:     "",
			},
		},
		Channels: []*pb.Channel{
			{
				Id:       99999,
				Title:    "Test Channel",
				Username: "testchannel",
			},
		},
		Groups: []*pb.Group{
			{
				Id:    88888,
				Title: "Test Group",
			},
		},
		Supergroups: []*pb.Supergroup{
			{
				Id:       77777,
				Title:    "Test Supergroup",
				Username: "testsupergroup",
			},
		},
		Service: pb.TELEGRAM,
		UserId:  42,
	}

	// Сериализуем в JSON
	jsonData, err := json.Marshal(testData)
	require.NoError(t, err, "Данные должны сериализоваться в JSON")

	// Отправляем контакты на сервер
	ctx := context.Background()
	err = SendContacts(ctx, client, json.RawMessage(jsonData))
	require.NoError(t, err, "Отправка контактов не должна вызывать ошибку")

	// Тест успешен
	t.Logf("✓ Контакты успешно отправлены на сервер")
	t.Logf("  - Людей: %d", len(testData.Humans))
	t.Logf("  - Ботов: %d", len(testData.Bots))
	t.Logf("  - Каналов: %d", len(testData.Channels))
	t.Logf("  - Групп: %d", len(testData.Groups))
	t.Logf("  - Супергрупп: %d", len(testData.Supergroups))
	t.Logf("  - Сервис: %v", testData.Service)
	t.Logf("  - ID пользователя: %d", testData.UserId)
}

// TestIntegrationBatchSendContacts проверяет отправку с повторами
func TestIntegrationBatchSendContacts(t *testing.T) {
	// Создаём сервер на порту 50053
	server := NewServer("50053")
	handler := NewHandler()

	err := server.Start(handler)
	require.NoError(t, err, "Сервер должен запуститься без ошибок")
	defer server.Stop()

	time.Sleep(500 * time.Millisecond)

	// Создаём клиент
	client := NewClient("localhost:50053", 30*time.Second)
	defer func() {
		_ = client.Close()
	}()

	// Создаём тестовые данные
	testData := pb.Result{
		Humans: []*pb.Contact{
			{
				Id:        11111,
				FirstName: "John",
				LastName:  "Doe",
				Username:  "johndoe",
				Phone:     "+1234567890",
			},
			{
				Id:        22222,
				FirstName: "Jane",
				LastName:  "Smith",
				Username:  "janesmith",
				Phone:     "+0987654321",
			},
		},
		Bots: []*pb.Contact{
			{
				Id:        33333,
				FirstName: "Bot",
				LastName:  "Assistant",
				Username:  "botassistant",
				Phone:     "",
			},
		},
		Channels: []*pb.Channel{
			{
				Id:       44444,
				Title:    "News Channel",
				Username: "newschannel",
			},
		},
		Groups: []*pb.Group{
			{
				Id:    55555,
				Title: "Dev Team",
			},
		},
		Supergroups: []*pb.Supergroup{
			{
				Id:       66666,
				Title:    "Golang Community",
				Username: "golangcommunity",
			},
		},
		Service: pb.WHATSAPP,
		UserId:  100,
	}

	jsonData, err := json.Marshal(testData)
	require.NoError(t, err, "Данные должны сериализоваться в JSON")

	// Отправляем с повторами (максимум 3 попытки)
	ctx := context.Background()
	err = BatchSendContacts(ctx, client, json.RawMessage(jsonData), 3)
	require.NoError(t, err, "Пакетная отправка не должна вызывать ошибку")

	// Тест успешен
	t.Logf("✓ Контакты успешно отправлены с повторами")
	t.Logf("  - Всего контактов: %d", len(testData.Humans)+len(testData.Bots))
	t.Logf("  - Каналов: %d", len(testData.Channels))
	t.Logf("  - Групп: %d", len(testData.Groups)+len(testData.Supergroups))
}
