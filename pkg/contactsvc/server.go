package contactsvc

import (
	"fmt"
	"net"
	"sync"

	"github.com/ikermy/AiR_Common/pkg/contactsvc/pb"
	"google.golang.org/grpc"
)

// Server структура для gRPC-сервера
type Server struct {
	mu       sync.Mutex
	listener net.Listener
	grpc     *grpc.Server
	port     string
	handler  *Handler
}

// NewServer создаёт новый экземпляр gRPC-сервера для приёма контактов
func NewServer(port string) *Server {
	return &Server{
		port: port,
		//handler: handler,
	}
}

// Start запускает gRPC-сервер
func (s *Server) ServerStart() error {
	s.mu.Lock()
	defer s.mu.Unlock()

	listener, err := net.Listen("tcp", fmt.Sprintf(":%s", s.port))
	if err != nil {
		return fmt.Errorf("ошибка при создании listener на порту %s: %w", s.port, err)
	}

	s.listener = listener
	s.grpc = grpc.NewServer()

	// Регистрируем gRPC-сервис напрямую
	pb.RegisterServer(s.grpc, s.handler)

	// Запускаем сервер в отдельной горутине
	go func() {
		s.grpc.Serve(listener)
	}()

	return nil
}

// Stop останавливает gRPC-сервер
func (s *Server) ServerStop() {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.grpc != nil {
		s.grpc.GracefulStop()
	}

	if s.listener != nil {
		s.listener.Close()
	}
}

// GetHandler возвращает обработчик для доступа к полученным данным
func (s *Server) GetHandler() *Handler {
	return s.handler
}
