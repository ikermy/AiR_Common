package rpc

import (
	"fmt"
	"net"
	"sync"

	"google.golang.org/grpc"
)

// ServiceHandler интерфейс для всех типов сервис-хандлеров
// Каждый хандлер отвечает за регистрацию себя в gRPC-сервере
type ServiceHandler interface {
	// RegisterService регистрирует сервис в gRPC-сервере
	RegisterContact(grpcServer *grpc.Server) error
}

// Server структура для gRPC-сервера
type Server struct {
	mu       sync.Mutex
	listener net.Listener
	grpc     *grpc.Server
	port     string
	handlers []ServiceHandler
}

// serverStart запускает gRPC-сервер
func (s *Server) serverStart() error {
	s.mu.Lock()
	defer s.mu.Unlock()

	listener, err := net.Listen("tcp", fmt.Sprintf(":%s", s.port))
	if err != nil {
		return fmt.Errorf("ошибка при создании listener на порту %s: %w", s.port, err)
	}

	s.listener = listener
	s.grpc = grpc.NewServer()

	// Регистрируем только первый хандлер в gRPC-сервере
	// Остальные хандлеры доступны через GetHandlers() для использования в приложении
	if len(s.handlers) > 0 && s.handlers[0] != nil {
		if err := s.handlers[0].RegisterContact(s.grpc); err != nil {
			return fmt.Errorf("ошибка при регистрации первого сервиса: %w", err)
		}
	}

	// Запускаем сервер в отдельной горутине
	go func() {
		_ = s.grpc.Serve(listener)
	}()

	return nil
}

// serverStop останавливает gRPC-сервер
func (s *Server) serverStop() {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.grpc != nil {
		s.grpc.GracefulStop()
	}

	if s.listener != nil {
		_ = s.listener.Close()
	}
}

// GetHandlers возвращает все обработчики
func (s *Server) GetHandlers() []ServiceHandler {
	return s.handlers
}
