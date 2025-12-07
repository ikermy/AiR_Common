package rpc

import (
	"context"

	"github.com/ikermy/AiR_Common/pkg/rpc/pb"
	"google.golang.org/grpc"
)

// Handler - реализация gRPC-сервиса ContactsService
type Handler struct {
	pb.UnimplementedServer
}

// NewHandler создаёт новый обработчик
func NewHandler() *Handler {
	return &Handler{}
}

// SendResult реализует gRPC-метод для получения контактов
func (h *Handler) SendResult(ctx context.Context, result *pb.Result) (*pb.Empty, error) {
	// Обработка контактов здесь
	return &pb.Empty{}, nil
}

// RegisterService регистрирует ContactsService в gRPC-сервере
func (h *Handler) RegisterService(grpcServer *grpc.Server) error {
	pb.RegisterServer(grpcServer, h)
	return nil
}
