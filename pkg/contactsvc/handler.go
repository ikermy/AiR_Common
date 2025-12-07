package contactsvc

import (
	"context"
	"sync"

	"github.com/ikermy/AiR_Common/pkg/contactsvc/pb"
)

// Handler - реализация gRPC-сервиса ContactsService
type Handler struct {
	pb.UnimplementedServer
	mu   sync.Mutex
	data *pb.Result // Буфер для хранения последних полученных контактов
}

// NewHandler создаёт новый обработчик
func NewHandler() *Handler {
	return &Handler{}
}

// SendFinalResult реализует gRPC-метод для получения контактов
func (h *Handler) SendResult(ctx context.Context, result *pb.Result) (*pb.Empty, error) {
	h.mu.Lock()
	defer h.mu.Unlock()

	// Сохраняем в буфер
	h.data = result

	return &pb.Empty{}, nil
}

// GetData возвращает последние полученные контакты
func (h *Handler) GetData() *pb.Result {
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
