package rpc

import (
	"context"
	"encoding/json"
	"sync"
	"testing"
	"time"

	"github.com/ikermy/AiR_Common/pkg/rpc/pb"
)

// ===== Client Tests =====

func TestNewClient(t *testing.T) {
	t.Run("with default timeout", func(t *testing.T) {
		client := NewClient("localhost:50051", 10*time.Second)

		if client == nil {
			t.Fatal("NewClient returned nil")
		}
		if client.config.Address != "localhost:50051" {
			t.Errorf("expected host 'localhost', got '%s'", client.config.Address)
		}
		if client.timeout != 10*time.Second {
			t.Errorf("expected default timeout 30s, got %v", client.timeout)
		}
	})

	t.Run("with custom timeout", func(t *testing.T) {
		client := NewClient("localhost:50051", 10*time.Second)

		if client.timeout != 10*time.Second {
			t.Errorf("expected timeout 10s, got %v", client.timeout)
		}
	})
}

func TestClientConnect(t *testing.T) {
	t.Run("successful connect", func(t *testing.T) {
		client := NewClient("localhost:50051", 10*time.Second)

		err := client.Connect()
		if err != nil {
			t.Fatalf("Connect failed: %v", err)
		}

		if !client.IsConnected() {
			t.Error("expected client to be connected")
		}

		// Повторное подключение не должно возвращать ошибку
		err = client.Connect()
		if err != nil {
			t.Errorf("second Connect should not fail: %v", err)
		}

		client.Close()
	})
}

func TestClientIsConnected(t *testing.T) {
	client := NewClient("localhost:50051", 10*time.Second)

	if client.IsConnected() {
		t.Error("new client should not be connected")
	}

	client.Connect()
	if !client.IsConnected() {
		t.Error("client should be connected after Connect()")
	}

	client.Close()
	if client.IsConnected() {
		t.Error("client should not be connected after Close()")
	}
}

func TestClientClose(t *testing.T) {
	t.Run("close connected client", func(t *testing.T) {
		client := NewClient("localhost:50051", 10*time.Second)

		client.Connect()
		err := client.Close()
		if err != nil {
			t.Errorf("Close failed: %v", err)
		}

		if client.IsConnected() {
			t.Error("client should not be connected after Close()")
		}
	})

	t.Run("close not connected client", func(t *testing.T) {
		client := NewClient("localhost:50051", 10*time.Second)

		err := client.Close()
		if err != nil {
			t.Errorf("Close on not connected client should not fail: %v", err)
		}
	})
}

func TestClientSendFinalResult(t *testing.T) {
	t.Run("send without connection", func(t *testing.T) {
		client := NewClient("localhost:50051", 10*time.Second)

		contactsData := json.RawMessage(`{"humans": [], "bots": []}`)
		err := client.SendResult(context.Background(), contactsData)
		if err == nil {
			t.Error("expected error when sending without connection")
		}
	})
}

func TestClientConcurrentAccess(t *testing.T) {
	client := NewClient("localhost:50051", 10*time.Second)

	var wg sync.WaitGroup

	// Параллельные подключения
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			client.Connect()
		}()
	}

	// Параллельные проверки состояния
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			client.IsConnected()
		}()
	}

	wg.Wait()
	client.Close()
}

// ===== Server Tests =====

func TestNewServer(t *testing.T) {
	server := NewServer("50051")

	if server == nil {
		t.Fatal("NewServer returned nil")
	}
	if server.port != "50051" {
		t.Errorf("expected port 50051, got %s", server.port)
	}
}

func TestServerStartStop(t *testing.T) {
	s := NewServer("") // port 0 для автоматического выбора свободного порта
	handler := NewHandler()
	if err := s.Start(handler); err != nil {
		t.Fatalf("Start failed: %v", err)
	}

	// Даём серверу время на запуск
	time.Sleep(100 * time.Millisecond)

	s.Stop()
}

func TestServerGetHandler(t *testing.T) {
	server := NewServer("50051")
	handler := NewHandler()
	if err := server.Start(handler); err != nil {
		t.Fatalf("Start failed: %v", err)
	}

	handlers := server.GetHandlers()
	if len(handlers) == 0 {
		t.Error("GetHandlers returned empty slice")
	}

	server.Stop()
}

// ===== Handler Tests =====

func TestNewContactsServiceHandler(t *testing.T) {
	handler := NewHandler()

	if handler == nil {
		t.Fatal("NewHandler returned nil")
	}
}

func TestHandlerSendResult(t *testing.T) {
	handler := NewHandler()

	// Создаём тестовые данные
	result := &pb.Result{
		Humans: []*pb.Contact{
			{Id: 1, FirstName: "John", LastName: "Doe"},
		},
		Service: pb.TELEGRAM,
		UserId:  42,
	}

	ctx := context.Background()
	_, err := handler.SendContacts(ctx, result)
	if err != nil {
		t.Errorf("SendContacts failed: %v", err)
	}
}

// ===== Public Functions Tests =====

func TestNewContactsClient(t *testing.T) {
	client := NewClient("localhost:50051", 5*time.Second)

	if client == nil {
		t.Fatal("NewClient returned nil")
	}
	if client.config.Address != "localhost:50051" {
		t.Errorf("expected host 'localhost', got '%s'", client.config.Address)
	}
	if client.config.Address != "localhost:50051" {
		t.Errorf("expected port 50051, got %s", client.config.Address)
	}
	if client.timeout != 5*time.Second {
		t.Errorf("expected timeout 5s, got %v", client.timeout)
	}
}

func TestStartContactsServer(t *testing.T) {
	s := NewServer("50051")
	handler := NewHandler()
	if err := s.Start(handler); err != nil {
		t.Fatalf("Start failed: %v", err)
	}

	s.Stop()
}

func TestStartContactsServerMultipleHandlers(t *testing.T) {
	s := NewServer("")
	handler1 := NewHandler()
	handler2 := NewHandler()
	handler3 := NewHandler()

	//注: Множество хандлеров одного типа регистрируются, но только первый обрабатывает gRPC-запросы
	// Остальные доступны как "слушатели" или обработчики для других целей
	if err := s.Start(handler1, handler2, handler3); err != nil {
		t.Fatalf("Start with multiple handlers failed: %v", err)
	}

	// Проверяем, что все хандлеры сохранены
	handlers := s.GetHandlers()
	if len(handlers) != 3 {
		t.Errorf("expected 3 handlers, got %d", len(handlers))
	}

	s.Stop()
}

func TestStartContactsServerNoHandlers(t *testing.T) {
	s := NewServer("50051")

	err := s.Start()
	if err == nil {
		t.Error("expected error when starting with no handlers")
	}
	if err != ErrNoHandlers {
		t.Errorf("expected ErrNoHandlers, got %v", err)
	}
}

func TestStopContactsServer(t *testing.T) {
	s := NewServer("50051")
	s.Stop()
}
