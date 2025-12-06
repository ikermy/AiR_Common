package contactsvc

import (
	"context"
	"sync"

	"github.com/ikermy/AiR_Common/pkg/contactsvc/pb"
)

// Handler - реализация gRPC-сервиса ContactsService
type Handler struct {
	pb.UnimplementedServiceServer
	mu   sync.Mutex
	data *pb.FinalResult // Буфер для хранения последних полученных контактов
}

// NewHandler создаёт новый обработчик
func NewHandler() *Handler {
	return &Handler{}
}

// SendFinalResult реализует gRPC-метод для получения контактов
func (h *Handler) SendFinalResult(ctx context.Context, result *pb.FinalResult) (*pb.Empty, error) {
	h.mu.Lock()
	defer h.mu.Unlock()

	// Сохраняем в буфер
	h.data = result

	return &pb.Empty{}, nil
}

// GetData возвращает последние полученные контакты
func (h *Handler) GetData() *pb.FinalResult {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.data
}

// ClearData очищает буфер полученных контактов
func (h *Handler) ClearData() {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.data = nil
}
