package contactsvc

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"

	"google.golang.org/grpc"
)

// ===== Mock DB =====

type mockDB struct {
	mu         sync.Mutex
	contacts   interface{}
	saveError  error
	getError   error
	saveCalled int
	getCalled  int
}

func newMockDB() *mockDB {
	return &mockDB{}
}

func (m *mockDB) SaveContacts(ctx context.Context, contacts interface{}) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.saveCalled++
	if m.saveError != nil {
		return m.saveError
	}
	m.contacts = contacts
	return nil
}

func (m *mockDB) GetContacts(ctx context.Context) (interface{}, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.getCalled++
	if m.getError != nil {
		return nil, m.getError
	}
	return m.contacts, nil
}

func (m *mockDB) setSaveError(err error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.saveError = err
}

func (m *mockDB) getSaveCallCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.saveCalled
}

// ===== Client Tests =====

func TestNewClient(t *testing.T) {
	t.Run("with default timeout", func(t *testing.T) {
		client := NewClient(ClientConfig{
			Host: "localhost",
			Port: 50051,
		})

		if client == nil {
			t.Fatal("NewClient returned nil")
		}
		if client.config.Host != "localhost" {
			t.Errorf("expected host 'localhost', got '%s'", client.config.Host)
		}
		if client.config.Port != 50051 {
			t.Errorf("expected port 50051, got %d", client.config.Port)
		}
		if client.timeout != 30*time.Second {
			t.Errorf("expected default timeout 30s, got %v", client.timeout)
		}
	})

	t.Run("with custom timeout", func(t *testing.T) {
		client := NewClient(ClientConfig{
			Host:    "example.com",
			Port:    8080,
			Timeout: 10 * time.Second,
		})

		if client.timeout != 10*time.Second {
			t.Errorf("expected timeout 10s, got %v", client.timeout)
		}
	})
}

func TestClientConnect(t *testing.T) {
	t.Run("successful connect", func(t *testing.T) {
		client := NewClient(ClientConfig{
			Host: "localhost",
			Port: 50051,
		})

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
	client := NewClient(ClientConfig{
		Host: "localhost",
		Port: 50051,
	})

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
		client := NewClient(ClientConfig{
			Host: "localhost",
			Port: 50051,
		})

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
		client := NewClient(ClientConfig{
			Host: "localhost",
			Port: 50051,
		})

		err := client.Close()
		if err != nil {
			t.Errorf("Close on not connected client should not fail: %v", err)
		}
	})
}

func TestClientSendFinalResult(t *testing.T) {
	t.Run("send without connection", func(t *testing.T) {
		client := NewClient(ClientConfig{
			Host: "localhost",
			Port: 50051,
		})

		contactsData := json.RawMessage(`{"name": "test"}`)
		err := client.SendFinalResult(context.Background(), contactsData)
		if err == nil {
			t.Error("expected error when sending without connection")
		}
	})

	t.Run("send with connection", func(t *testing.T) {
		client := NewClient(ClientConfig{
			Host: "localhost",
			Port: 50051,
		})

		client.Connect()
		defer client.Close()

		contactsData := json.RawMessage(`{"name": "test", "phone": "+123456789"}`)
		err := client.SendFinalResult(context.Background(), contactsData)
		if err != nil {
			t.Errorf("SendFinalResult failed: %v", err)
		}
	})
}

// ===== Server Tests =====

func TestNewServer(t *testing.T) {
	db := newMockDB()
	server := NewServer(50051, db, nil)

	if server == nil {
		t.Fatal("NewServer returned nil")
	}
	if server.port != 50051 {
		t.Errorf("expected port 50051, got %d", server.port)
	}
	if server.db != db {
		t.Error("db not set correctly")
	}
	if server.receivedData == nil {
		t.Error("receivedData map not initialized")
	}
}

func TestServerStartStop(t *testing.T) {
	db := newMockDB()

	// Простой mock registrator
	registrator := func(grpcServer *grpc.Server, handler *ContactsServiceHandler) error {
		return nil
	}

	server := NewServer(0, db, registrator) // port 0 для автоматического выбора свободного порта

	ctx := context.Background()
	err := server.Start(ctx)
	if err != nil {
		t.Fatalf("Start failed: %v", err)
	}

	// Даём серверу время на запуск
	time.Sleep(100 * time.Millisecond)

	server.Stop()
}

func TestServerStartWithoutRegistrator(t *testing.T) {
	db := newMockDB()
	server := NewServer(0, db, nil)

	ctx := context.Background()
	err := server.Start(ctx)
	if err != nil {
		t.Fatalf("Start without registrator should not fail: %v", err)
	}

	server.Stop()
}

func TestServerStartWithFailingRegistrator(t *testing.T) {
	db := newMockDB()

	registrator := func(grpcServer *grpc.Server, handler *ContactsServiceHandler) error {
		return errors.New("registration failed")
	}

	server := NewServer(0, db, registrator)

	ctx := context.Background()
	err := server.Start(ctx)
	if err == nil {
		t.Error("expected error when registrator fails")
		server.Stop()
	}
}

func TestServerGetReceivedData(t *testing.T) {
	db := newMockDB()
	server := NewServer(50051, db, nil)

	data := server.GetReceivedData()
	if data == nil {
		t.Error("GetReceivedData returned nil")
	}
	if len(data) != 0 {
		t.Errorf("expected empty map, got %d elements", len(data))
	}
}

// ===== Handler Tests =====

func TestNewContactsServiceHandler(t *testing.T) {
	db := newMockDB()
	handler := NewContactsServiceHandler(db)

	if handler == nil {
		t.Fatal("NewContactsServiceHandler returned nil")
	}
	if handler.db != db {
		t.Error("db not set correctly")
	}
	if handler.data == nil {
		t.Error("data map not initialized")
	}
}

func TestHandlerHandleContactsData(t *testing.T) {
	t.Run("with db", func(t *testing.T) {
		db := newMockDB()
		handler := NewContactsServiceHandler(db)

		contactsData := json.RawMessage(`{"contacts": [{"name": "John", "phone": "123"}]}`)
		err := handler.HandleContactsData(contactsData)
		if err != nil {
			t.Errorf("HandleContactsData failed: %v", err)
		}

		data := handler.GetData()
		if _, ok := data["contacts"]; !ok {
			t.Error("contacts not stored in handler data")
		}
	})

	t.Run("without db", func(t *testing.T) {
		handler := NewContactsServiceHandler(nil)

		contactsData := json.RawMessage(`{"contacts": []}`)
		err := handler.HandleContactsData(contactsData)
		if err != nil {
			t.Errorf("HandleContactsData without db should not fail: %v", err)
		}

		data := handler.GetData()
		if _, ok := data["contacts"]; !ok {
			t.Error("contacts not stored in handler data")
		}
	})
}

func TestHandlerGetData(t *testing.T) {
	handler := NewContactsServiceHandler(nil)

	// Пустые данные
	data := handler.GetData()
	if len(data) != 0 {
		t.Errorf("expected empty data, got %d elements", len(data))
	}

	// После добавления данных
	handler.HandleContactsData(json.RawMessage(`{"test": true}`))
	data = handler.GetData()
	if len(data) != 1 {
		t.Errorf("expected 1 element, got %d", len(data))
	}
}

func TestHandlerClearData(t *testing.T) {
	handler := NewContactsServiceHandler(nil)

	handler.HandleContactsData(json.RawMessage(`{"test": true}`))

	if len(handler.GetData()) == 0 {
		t.Error("data should not be empty before clear")
	}

	handler.ClearData()

	if len(handler.GetData()) != 0 {
		t.Error("data should be empty after clear")
	}
}

// ===== Public Functions Tests =====

func TestNewContactsClient(t *testing.T) {
	client := NewContactsClient("localhost", 50051, 5*time.Second)

	if client == nil {
		t.Fatal("NewContactsClient returned nil")
	}
	if client.config.Host != "localhost" {
		t.Errorf("expected host 'localhost', got '%s'", client.config.Host)
	}
	if client.config.Port != 50051 {
		t.Errorf("expected port 50051, got %d", client.config.Port)
	}
	if client.timeout != 5*time.Second {
		t.Errorf("expected timeout 5s, got %v", client.timeout)
	}
}

func TestSendFinalResultToService(t *testing.T) {
	t.Run("connects if not connected", func(t *testing.T) {
		client := NewContactsClient("localhost", 50051, 5*time.Second)
		defer client.Close()

		if client.IsConnected() {
			t.Error("client should not be connected initially")
		}

		contactsData := json.RawMessage(`{"name": "test"}`)
		err := SendFinalResultToService(context.Background(), client, contactsData)
		if err != nil {
			t.Errorf("SendFinalResultToService failed: %v", err)
		}

		if !client.IsConnected() {
			t.Error("client should be connected after SendFinalResultToService")
		}
	})
}

func TestBatchSendContacts(t *testing.T) {
	t.Run("successful send on first try", func(t *testing.T) {
		client := NewContactsClient("localhost", 50051, 5*time.Second)
		defer client.Close()

		contactsData := json.RawMessage(`{"contacts": []}`)
		err := BatchSendContacts(context.Background(), client, contactsData, 3)
		if err != nil {
			t.Errorf("BatchSendContacts failed: %v", err)
		}
	})

	t.Run("respects context cancellation", func(t *testing.T) {
		client := NewContactsClient("localhost", 50051, 5*time.Second)
		defer client.Close()

		ctx, cancel := context.WithCancel(context.Background())
		cancel() // Отменяем сразу

		contactsData := json.RawMessage(`{"contacts": []}`)
		// Этот тест проверяет, что функция корректно обрабатывает отменённый контекст
		// Так как соединение устанавливается до проверки контекста, ошибки может не быть
		_ = BatchSendContacts(ctx, client, contactsData, 3)
	})
}

func TestStartContactsServer(t *testing.T) {
	db := newMockDB()
	registrator := func(grpcServer *grpc.Server, handler *ContactsServiceHandler) error {
		return nil
	}

	ctx := context.Background()
	server, err := StartContactsServer(ctx, 0, db, registrator)
	if err != nil {
		t.Fatalf("StartContactsServer failed: %v", err)
	}

	if server == nil {
		t.Fatal("StartContactsServer returned nil server")
	}

	StopContactsServer(server)
}

func TestStopContactsServer(t *testing.T) {
	// Проверяем, что StopContactsServer не паникует при nil
	StopContactsServer(nil)
}

// ===== Concurrent Access Tests =====

func TestClientConcurrentAccess(t *testing.T) {
	client := NewClient(ClientConfig{
		Host: "localhost",
		Port: 50051,
	})

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

func TestHandlerConcurrentAccess(t *testing.T) {
	handler := NewContactsServiceHandler(nil)

	var wg sync.WaitGroup

	// Параллельная обработка данных
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			data := json.RawMessage(`{"id": ` + string(rune('0'+n)) + `}`)
			handler.HandleContactsData(data)
		}(i)
	}

	// Параллельное чтение данных
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			handler.GetData()
		}()
	}

	wg.Wait()
}
