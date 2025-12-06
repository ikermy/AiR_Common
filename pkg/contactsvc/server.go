package contactsvc

import (
	"context"
	"errors"
	"fmt"
	"net"
	"sync"

	"google.golang.org/grpc"
)

// DB интерфейс для сохранения контактов
type DB interface {
	SaveContacts(ctx context.Context, contacts interface{}) error
	GetContacts(ctx context.Context) (interface{}, error)
}

// ServiceRegistrator интерфейс для регистрации gRPC-сервиса
// Целевое приложение должно реализовать эту функцию для регистрации своего ContactsService
type ServiceRegistrator func(grpcServer *grpc.Server, handler *ContactsServiceHandler) error

// Server структура для gRPC-сервера
type Server struct {
	mu       sync.Mutex
	listener net.Listener
	grpc     *grpc.Server
	port     int
	db       DB
	register ServiceRegistrator

	// Буфер для полученных контактов
	receivedData map[string]interface{}
}

// NewServer создаёт новый экземпляр gRPC-сервера для приёма контактов
// register - функция регистрации gRPC-сервиса из целевого приложения
func NewServer(port int, db DB, register ServiceRegistrator) *Server {
	return &Server{
		port:         port,
		db:           db,
		register:     register,
		receivedData: make(map[string]interface{}),
	}
}

// Start запускает gRPC-сервер
func (s *Server) Start(_ context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	listener, err := net.Listen("tcp", fmt.Sprintf(":%d", s.port))
	if err != nil {
		return err
	}

	s.listener = listener
	s.grpc = grpc.NewServer()

	// Регистрируем сервис контактов через callback функцию
	handler := NewContactsServiceHandler(s.db)
	if s.register != nil {
		if err := s.register(s.grpc, handler); err != nil {
			return err
		}
	}

	// Запускаем сервер в отдельной горутине
	go func() {
		if err := s.grpc.Serve(listener); err != nil && !errors.Is(grpc.ErrServerStopped, err) {
		}
	}()

	return nil
}

// Stop останавливает gRPC-сервер
func (s *Server) Stop() {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.grpc != nil {
		s.grpc.GracefulStop()
	}

	if s.listener != nil {
		err := s.listener.Close()
		if err != nil {
			return
		}
	}
}

// GetReceivedData возвращает полученные контакты
func (s *Server) GetReceivedData() map[string]interface{} {
	s.mu.Lock()
	defer s.mu.Unlock()

	// Возвращаем копию
	result := make(map[string]interface{})
	for k, v := range s.receivedData {
		result[k] = v
	}
	return result
}
