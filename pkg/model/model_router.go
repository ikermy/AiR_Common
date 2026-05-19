package model

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sync/atomic"
	"time"

	"github.com/ikermy/AiR_Common/pkg/com"
	"github.com/ikermy/AiR_Common/pkg/conf"
	"github.com/ikermy/AiR_Common/pkg/mode"
	"github.com/ikermy/AiR_Common/pkg/model/create"
)

// ModelRouter маршрутизирует запросы к разным моделям на основе Provider
type ModelRouter struct {
	openai        Inter
	mistral       Inter
	google        Inter
	modelsManager *create.UniversalModel // Менеджер для создания/удаления моделей
	ctx           context.Context
	conf          *conf.Conf
	db            DB
	landingPort   string // Порт landing сервера для GetRealUserID
}

// HasOpenAI проверяет, инициализирован ли провайдер OpenAI
func (r *ModelRouter) HasOpenAI() bool {
	return r.openai != nil
}

// HasMistral проверяет, инициализирован ли провайдер Mistral
func (r *ModelRouter) HasMistral() bool {
	return r.mistral != nil
}

// HasGoogle проверяет, инициализирован ли провайдер Google
func (r *ModelRouter) HasGoogle() bool { return r.google != nil }

// GetAvailableProviders возвращает список доступных провайдеров
func (r *ModelRouter) GetAvailableProviders() []string {
	providers := []string{}
	if r.openai != nil {
		providers = append(providers, "OpenAI")
	}
	if r.mistral != nil {
		providers = append(providers, "Mistral")
	}
	if r.google != nil {
		providers = append(providers, "Google")
	}
	return providers
}

// getModel возвращает модель по типу провайдера
func (r *ModelRouter) getModel(provider create.ProviderType) (Inter, error) {
	switch provider {
	case create.ProviderOpenAI:
		if r.openai == nil {
			return nil, fmt.Errorf("модель OpenAI не инициализирована")
		}
		return r.openai, nil
	case create.ProviderMistral:
		if r.mistral == nil {
			return nil, fmt.Errorf("модель Mistral не инициализирована")
		}
		return r.mistral, nil
	case create.ProviderGoogle:
		if r.google == nil {
			return nil, fmt.Errorf("модель Google не инициализирована")
		}
		return r.google, nil
	default:
		return nil, fmt.Errorf("неизвестный провайдер: %v", provider)
	}
}

// GetProviderModel возвращает модель конкретного провайдера (для тестирования)
func (r *ModelRouter) GetProviderModel(provider create.ProviderType) interface{} {
	switch provider {
	case create.ProviderOpenAI:
		return r.openai
	case create.ProviderMistral:
		return r.mistral
	case create.ProviderGoogle:
		return r.google
	default:
		return nil
	}
}

// NewMessage делегирует вызов к нужной модели
func (r *ModelRouter) NewMessage(operator Operator, msgType string, content *AssistResponse, name *string, files ...FileUpload) Message {
	// Используем OpenAI по умолчанию для создания сообщений
	if r.openai != nil {
		return r.openai.NewMessage(operator, msgType, content, name, files...)
	}
	if r.mistral != nil {
		return r.mistral.NewMessage(operator, msgType, content, name, files...)
	}
	if r.google != nil {
		return r.google.NewMessage(operator, msgType, content, name, files...)
	}
	// Fallback — создаём сообщение напрямую
	return Message{
		Operator:  operator,
		Type:      msgType,
		Content:   *content,
		Name:      *name,
		Timestamp: time.Now(),
		Files:     files,
	}
}

// GetFileAsReader делегирует к нужной модели
func (r *ModelRouter) GetFileAsReader(userId uint32, url string) (io.Reader, error) {
	manager, err := r.GetActiveUserManager(userId)
	if err != nil {
		return nil, fmt.Errorf("ошибка получения активного менеджера для userId %d: %w", userId, err)
	}

	return manager.GetFileAsReader(userId, url)
}

// GetOrSetRespGPT делегирует к модели на основе Provider из Assistant
func (r *ModelRouter) GetOrSetRespGPT(assist Assistant, dialogID, respId uint64, respName string) (*RespModel, error) {
	// Если провайдер не установлен - это ошибка, у пользователя нет созданной модели
	if assist.Provider == 0 {
		return nil, fmt.Errorf("провайдер не установлен для userId=%d: у пользователя не создана модель ассистента. "+
			"Создайте модель через API или панель управления", assist.UserID)
	}

	m, err := r.getModel(assist.Provider)
	if err != nil {
		return nil, fmt.Errorf("не удалось получить модель для провайдера %s (userId=%d): %w", assist.Provider, assist.UserID, err)
	}
	return m.GetOrSetRespGPT(assist, dialogID, respId, respName)
}

// GetCh получает канал от любой модели (они хранятся в sync.Map)
func (r *ModelRouter) GetCh(respId uint64) (*Ch, error) {
	// Пробуем получить из OpenAI
	if r.openai != nil {
		ch, err := r.openai.GetCh(respId)
		if err == nil {
			return ch, nil
		}
	}
	// Затем из Mistral
	if r.mistral != nil {
		ch, err := r.mistral.GetCh(respId)
		if err == nil {
			return ch, nil
		}
	}
	// Затем Google
	if r.google != nil {
		ch, err := r.google.GetCh(respId)
		if err == nil {
			return ch, nil
		}
	}

	return nil, fmt.Errorf("канал не найден для respId %d", respId)
}

// GetRespIdBydialogID делегирует к обеим моделям
func (r *ModelRouter) GetRespIdBydialogID(dialogID uint64) (uint64, error) {
	// Пробуем OpenAI
	if r.openai != nil {
		id, err := r.openai.GetRespIdBydialogID(dialogID)
		if err == nil {
			return id, nil
		}
	}
	// Затем Mistral
	if r.mistral != nil {
		return r.mistral.GetRespIdBydialogID(dialogID)
	}
	// Затем Google
	if r.google != nil {
		return r.google.GetRespIdBydialogID(dialogID)
	}
	return 0, fmt.Errorf("RespId не найден для DialogID %d", dialogID)
}

// SaveAllContextDuringExit сохраняет контексты всех моделей
func (r *ModelRouter) SaveAllContextDuringExit() {
	if r.openai != nil {
		r.openai.SaveAllContextDuringExit()
	}
	if r.mistral != nil {
		r.mistral.SaveAllContextDuringExit()
	}
	if r.google != nil {
		r.google.SaveAllContextDuringExit()
	}
}

// Request направляет запрос к нужной модели на основе DialogID
func (r *ModelRouter) Request(userId uint32, dialogID uint64, text string, files ...FileUpload) (AssistResponse, error) {
	// Определяем провайдера по наличию респондента (БЕЗ запроса к БД!)
	if r.openai != nil {
		_, err := r.openai.GetRespIdBydialogID(dialogID)
		if err == nil {
			return r.openai.Request(userId, dialogID, text, files...)
		}
	}

	if r.mistral != nil {
		_, err := r.mistral.GetRespIdBydialogID(dialogID)
		if err == nil {
			return r.mistral.Request(userId, dialogID, text, files...)
		}
	}

	if r.google != nil {
		_, err := r.google.GetRespIdBydialogID(dialogID)
		if err == nil {
			return r.google.Request(userId, dialogID, text, files...)
		}
	}

	return AssistResponse{}, fmt.Errorf("модель не найдена для DialogID %d", dialogID)
}

// RequestStreaming направляет streaming запрос к нужной модели на основе DialogID
func (r *ModelRouter) RequestStreaming(userId uint32, dialogID uint64, text string, onDelta func(delta string, done bool) error, files ...FileUpload) error {
	// Определяем провайдера по наличию респондента (БЕЗ запроса к БД!)
	if r.openai != nil {
		_, err := r.openai.GetRespIdBydialogID(dialogID)
		if err == nil {
			// Проверяем поддержку RequestStreaming через type assertion
			if streamer, ok := r.openai.(interface {
				RequestStreaming(userId uint32, dialogID uint64, text string, onDelta func(delta string, done bool) error, files ...FileUpload) error
			}); ok {
				return streamer.RequestStreaming(userId, dialogID, text, onDelta, files...)
			}
			// Fallback на обычный Request с буферизацией
			response, err := r.openai.Request(userId, dialogID, text, files...)
			if err != nil {
				return err
			}
			// Сериализуем ответ и отправляем как один delta
			jsonData, _ := json.Marshal(response)
			if onDelta != nil {
				onDelta(string(jsonData), true)
			}
			return nil
		}
	}

	if r.mistral != nil {
		_, err := r.mistral.GetRespIdBydialogID(dialogID)
		if err == nil {
			// Проверяем поддержку RequestStreaming
			if streamer, ok := r.mistral.(interface {
				RequestStreaming(userId uint32, dialogID uint64, text string, onDelta func(delta string, done bool) error, files ...FileUpload) error
			}); ok {
				return streamer.RequestStreaming(userId, dialogID, text, onDelta, files...)
			}
			// Fallback
			response, err := r.mistral.Request(userId, dialogID, text, files...)
			if err != nil {
				return err
			}
			jsonData, _ := json.Marshal(response)
			if onDelta != nil {
				onDelta(string(jsonData), true)
			}
			return nil
		}
	}

	if r.google != nil {
		_, err := r.google.GetRespIdBydialogID(dialogID)
		if err == nil {
			// Проверяем поддержку RequestStreaming
			if streamer, ok := r.google.(interface {
				RequestStreaming(userId uint32, dialogID uint64, text string, onDelta func(delta string, done bool) error, files ...FileUpload) error
			}); ok {
				return streamer.RequestStreaming(userId, dialogID, text, onDelta, files...)
			}
			// Fallback
			response, err := r.google.Request(userId, dialogID, text, files...)
			if err != nil {
				return err
			}
			jsonData, _ := json.Marshal(response)
			if onDelta != nil {
				onDelta(string(jsonData), true)
			}
			return nil
		}
	}

	return fmt.Errorf("модель не найдена для DialogID %d", dialogID)
}

// CleanDialogData очищает данные диалога из нужной модели
func (r *ModelRouter) CleanDialogData(dialogID uint64) {
	if r.openai != nil {
		r.openai.CleanDialogData(dialogID)
	}
	if r.mistral != nil {
		r.mistral.CleanDialogData(dialogID)
	}
	if r.google != nil {
		r.google.CleanDialogData(dialogID)
	}
}

// GetActiveUserModel получает активную модель пользователя
func (r *ModelRouter) GetActiveUserModel(userId uint32) (*create.UniversalModelData, error) {
	if r.modelsManager == nil {
		return nil, fmt.Errorf("модельный менеджер не инициализирован")
	}
	return r.modelsManager.GetActiveUserModel(userId)
}

func (r *ModelRouter) GetActiveUserManager(userId uint32) (Inter, error) {
	provider, err := r.db.GetActiveProvider(userId)
	if err != nil {
		return nil, fmt.Errorf("ошибка получения активного провайдера для userId %d: %w", userId, err)
	}

	switch provider {
	case create.ProviderOpenAI:
		if r.openai == nil {
			return nil, fmt.Errorf("OpenAI провайдер не инициализирован")
		}
		return r.openai.(OpenAIManager), nil
	case create.ProviderMistral:
		if r.mistral == nil {
			return nil, fmt.Errorf("Mistral провайдер не инициализирован")
		}
		return r.mistral.(MistralManager), nil
	case create.ProviderGoogle:
		if r.google == nil {
			return nil, fmt.Errorf("Google провайдер не инициализирован")
		}
		return r.google.(GoogleManager), nil
	default:
		return nil, fmt.Errorf("неизвестный провайдер: %s", provider)
	}
}

// TranscribeAudio транскрибирует аудио
func (r *ModelRouter) TranscribeAudio(userId uint32, audioData []byte, fileName string) (string, error) {
	manager, err := r.GetActiveUserManager(userId)
	if err != nil {
		return "", fmt.Errorf("ошибка получения активного менеджера для userId %d: %w", userId, err)
	}

	return manager.TranscribeAudio(userId, audioData, fileName)
}

// GetRealtimeProvider возвращает RealtimeProvider если активная модель пользователя — OpenAI
// с включённым флагом Realtime. Второй bool = false если провайдер недоступен.
func (r *ModelRouter) GetRealtimeProvider(userId uint32) (RealtimeProvider, bool) {
	if r.openai == nil {
		return nil, false
	}
	activeManager, err := r.GetActiveUserManager(userId)
	if err != nil {
		return nil, false
	}
	rp, ok := activeManager.(RealtimeProvider)
	return rp, ok
}

// GetRealtimeGenerating реализует RealtimeProvider — делегирует в openai провайдер напрямую.
func (r *ModelRouter) GetRealtimeGenerating(respId uint64) *atomic.Bool {
	if r.openai == nil {
		return nil
	}
	rp, ok := r.openai.(RealtimeProvider)
	if !ok {
		return nil
	}
	return rp.GetRealtimeGenerating(respId)
}

// DisconnectRealtimeSession завершает голосовую сессию respId с вызовом зарегистрированного callback.
// Используется для универсального завершения сессии (API WebSocket + Telegram звонок).
// Вызывает OnDisconnect callback если он установлен в RealtimeSession.
func (r *ModelRouter) DisconnectRealtimeSession(respId uint64) {
	if r.openai == nil {
		return
	}
	rp, ok := r.openai.(RealtimeProvider)
	if !ok {
		return
	}
	rp.CloseRealtimeSession(respId)
}

// SetRealtimeDisconnectCallback устанавливает callback в RealtimeSession для уведомления о критическом таймауте.
func (r *ModelRouter) SetRealtimeDisconnectCallback(respId uint64, callback func(respId uint64)) error {
	if r.openai == nil {
		return fmt.Errorf("SetRealtimeDisconnectCallback: OpenAI провайдер не инициализирован")
	}
	rp, ok := r.openai.(RealtimeProvider)
	if !ok {
		return fmt.Errorf("SetRealtimeDisconnectCallback: OpenAI провайдер не реализует RealtimeProvider")
	}
	return rp.SetRealtimeDisconnectCallback(respId, callback)
}

// Shutdown завершает работу всех моделей
func (r *ModelRouter) Shutdown(shutCh chan<- com.LogMsg) {
	if r.openai != nil {
		r.openai.Shutdown(shutCh)
	}
	if r.mistral != nil {
		r.mistral.Shutdown(shutCh)
	}
	if r.google != nil {
		r.google.Shutdown(shutCh)
	}
}

// CleanUp запускает фоновую очистку устаревших записей для всех моделей
// Каждая модель запускает свой тикер для периодической очистки
func (r *ModelRouter) CleanUp() {
	if r.openai != nil {
		go r.openai.CleanUp()
	}
	if r.mistral != nil {
		go r.mistral.CleanUp()
	}
	if r.google != nil {
		go r.google.CleanUp()
	}
}

// CreateModel создаёт новую модель у указанного провайдера
// Использует modelsManager для создания модели
func (r *ModelRouter) CreateModel(userId uint32, provider create.ProviderType, modelData *create.UniversalModelData, fileIDs []create.Ids) (create.UMCR, error) {
	// Проверяем, что провайдер поддерживается
	_, err := r.getModel(provider)
	if err != nil {
		return create.UMCR{}, err
	}

	// Используем modelsManager для создания модели
	if r.modelsManager == nil {
		return create.UMCR{}, fmt.Errorf("модельный менеджер не инициализирован")
	}

	return r.modelsManager.CreateModel(userId, provider, modelData, fileIDs)
}

// UploadFileToProvider загружает файл в указанный провайдер
func (r *ModelRouter) UploadFileToProvider(userId uint32, provider create.ProviderType, fileName string, fileData []byte) (string, error) {
	switch provider {
	case create.ProviderOpenAI:
		return "", fmt.Errorf("OpenAI провайдер не поддерживает загрузку файлов")

	case create.ProviderMistral:
		if r.mistral == nil {
			return "", fmt.Errorf("Mistral провайдер не инициализирован")
		}
		if manager, ok := r.mistral.(MistralManager); ok {
			return manager.UploadFileToProvider(userId, fileName, fileData)
		}
		return "", fmt.Errorf("Mistral провайдер не поддерживает загрузку файлов")

	case create.ProviderGoogle:
		return "", fmt.Errorf("Google провайдер не поддерживает загрузку файлов")

	default:
		return "", fmt.Errorf("неизвестный провайдер: %s", provider)
	}
}

// DeleteTempFile удаляет загруженный/созданный моделью временный файл
func (r *ModelRouter) DeleteTempFile(fileID string) error {
	// Временные файлы нужно удалять только из Mistarl
	if r.mistral == nil {
		return fmt.Errorf("OpenAI провайдер не инициализирован")
	}
	if manager, ok := r.mistral.(MistralManager); ok {
		return manager.DeleteTempFile(fileID)
	}
	return fmt.Errorf("OpenAI провайдер не поддерживает удаление загруженных файлов")

}

// DeleteFileFromProvider удаляет файл из указанного провайдера
func (r *ModelRouter) DeleteFileFromProvider(userId uint32, provider create.ProviderType, fileID string) error {
	switch provider {
	case create.ProviderOpenAI:
		return fmt.Errorf("OpenAI провайдер не поддерживает удаление файлов")

	case create.ProviderMistral:
		if r.mistral == nil {
			return fmt.Errorf("Mistral провайдер не инициализирован")
		}
		if manager, ok := r.mistral.(MistralManager); ok {
			return manager.DeleteDocumentFromLibrary(userId, fileID)
		}
		return fmt.Errorf("Mistral провайдер не поддерживает удаление файлов")

	case create.ProviderGoogle:
		return fmt.Errorf("Google провайдер не поддерживает удаление файлов")

	default:
		return fmt.Errorf("неизвестный провайдер: %s", provider)
	}
}

// ===========================================================
// Специфичные методы для работы с файлами в векторных хранилищах
// ===========================================================

// AddFileFromFromProvider добавляет файл в хранилище указанного провайдера
func (r *ModelRouter) AddFileFromFromProvider(provider create.ProviderType, userId uint32, fileID, fileName string) error {
	switch provider {
	case create.ProviderOpenAI:
		return fmt.Errorf("OpenAI провайдер не поддерживает добавление файлов")

	case create.ProviderMistral:
		if r.mistral == nil {
			return fmt.Errorf("Mistral провайдер не инициализирован")
		}
		if manager, ok := r.mistral.(MistralManager); ok {
			return manager.AddFileToLibrary(userId, fileID, fileName)
		}
		return fmt.Errorf("Mistral провайдер не поддерживает добавление файлов")

	case create.ProviderGoogle:
		return fmt.Errorf("Google провайдер не поддерживает добавление файлов")

	default:
		return fmt.Errorf("неизвестный провайдер: %s", provider)
	}
}

// ===========================================================
// Vector Embedding методы (Google + OpenAI)
// ===========================================================

// UploadDocumentWithEmbedding загружает документ с генерацией эмбеддинга
// Поддерживает Google и OpenAI провайдеры
func (r *ModelRouter) UploadDocumentWithEmbedding(userId uint32, provider, docName, content string, metadata create.DocumentMetadata) (string, error) {
	providerType, err := create.FromString(provider)
	if err != nil {
		return "", fmt.Errorf("неверный provider: %w", err)
	}

	switch providerType {
	case create.ProviderGoogle:
		if r.google == nil {
			return "", fmt.Errorf("Google провайдер не инициализирован")
		}
		if manager, ok := r.google.(GoogleManager); ok {
			return manager.UploadDocumentWithEmbedding(userId, docName, content, metadata)
		}
		return "", fmt.Errorf("Google провайдер не поддерживает загрузку документов с эмбеддингами")

	case create.ProviderOpenAI:
		if r.openai == nil {
			return "", fmt.Errorf("OpenAI провайдер не инициализирован")
		}
		if manager, ok := r.openai.(OpenAIManager); ok {
			return manager.UploadDocumentWithEmbedding(userId, docName, content, metadata)
		}
		return "", fmt.Errorf("OpenAI провайдер не поддерживает загрузку документов с эмбеддингами")

	default:
		return "", fmt.Errorf("провайдер %s не поддерживает эмбеддинги", provider)
	}
}

// SearchSimilarDocuments ищет похожие документы в Vector Store
// Поддерживает Google и OpenAI провайдеры
func (r *ModelRouter) SearchSimilarDocuments(userId uint32, provider, query string, limit int) ([]create.VectorDocument, error) {
	providerType, err := create.FromString(provider)
	if err != nil {
		return nil, fmt.Errorf("неверный provider: %w", err)
	}

	switch providerType {
	case create.ProviderGoogle:
		if r.google == nil {
			return nil, fmt.Errorf("Google провайдер не инициализирован")
		}
		if manager, ok := r.google.(GoogleManager); ok {
			return manager.SearchSimilarDocuments(userId, query, limit)
		}
		return nil, fmt.Errorf("Google провайдер не поддерживает поиск документов")

	case create.ProviderOpenAI:
		if r.openai == nil {
			return nil, fmt.Errorf("OpenAI провайдер не инициализирован")
		}
		if manager, ok := r.openai.(OpenAIManager); ok {
			return manager.SearchSimilarDocuments(userId, query, limit)
		}
		return nil, fmt.Errorf("OpenAI провайдер не поддерживает поиск документов")

	default:
		return nil, fmt.Errorf("провайдер %s не поддерживает эмбеддинги", provider)
	}
}

// DeleteDocument удаляет документ из Vector Store
// Поддерживает Google и OpenAI провайдеры
func (r *ModelRouter) DeleteDocument(userId uint32, provider, docID string) error {
	providerType, err := create.FromString(provider)
	if err != nil {
		return fmt.Errorf("неверный provider: %w", err)
	}

	switch providerType {
	case create.ProviderGoogle:
		if r.google == nil {
			return fmt.Errorf("Google провайдер не инициализирован")
		}
		if manager, ok := r.google.(GoogleManager); ok {
			return manager.DeleteDocument(userId, docID)
		}
		return fmt.Errorf("Google провайдер не поддерживает удаление документов")

	case create.ProviderOpenAI:
		if r.openai == nil {
			return fmt.Errorf("OpenAI провайдер не инициализирован")
		}
		if manager, ok := r.openai.(OpenAIManager); ok {
			return manager.DeleteDocument(userId, docID)
		}
		return fmt.Errorf("OpenAI провайдер не поддерживает удаление документов")

	default:
		return fmt.Errorf("провайдер %s не поддерживает эмбеддинги", provider)
	}
}

// ListUserDocuments возвращает список документов пользователя
// Поддерживает Google и OpenAI провайдеры
// Если provider пустой, возвращает документы всех провайдеров
func (r *ModelRouter) ListUserDocuments(userId uint32, provider string) ([]create.VectorDocument, error) {
	// Если provider пустой - возвращаем документы всех провайдеров
	if provider == "" {
		var allDocs []create.VectorDocument

		// Пробуем получить документы Google
		if r.google != nil {
			if manager, ok := r.google.(GoogleManager); ok {
				docs, err := manager.ListUserDocuments(userId)
				if err == nil && docs != nil {
					allDocs = append(allDocs, docs...)
				}
			}
		}

		// Пробуем получить документы OpenAI
		if r.openai != nil {
			if manager, ok := r.openai.(OpenAIManager); ok {
				docs, err := manager.ListUserDocuments(userId)
				if err == nil && docs != nil {
					allDocs = append(allDocs, docs...)
				}
			}
		}

		return allDocs, nil
	}

	// Если provider указан - работаем только с ним
	providerType, err := create.FromString(provider)
	if err != nil {
		return nil, fmt.Errorf("неверный provider: %w", err)
	}

	switch providerType {
	case create.ProviderGoogle:
		if r.google == nil {
			return nil, fmt.Errorf("Google провайдер не инициализирован")
		}
		if manager, ok := r.google.(GoogleManager); ok {
			return manager.ListUserDocuments(userId)
		}
		return nil, fmt.Errorf("Google провайдер не поддерживает список документов")

	case create.ProviderOpenAI:
		if r.openai == nil {
			return nil, fmt.Errorf("OpenAI провайдер не инициализирован")
		}
		if manager, ok := r.openai.(OpenAIManager); ok {
			return manager.ListUserDocuments(userId)
		}
		return nil, fmt.Errorf("OpenAI провайдер не поддерживает список документов")

	default:
		return nil, fmt.Errorf("провайдер %s не поддерживает эмбеддинги", provider)
	}
}

// ===========================================================
// Управление моделями
// ===========================================================

// SaveModel сохраняет модель в БД в универсальном формате
func (r *ModelRouter) SaveModel(userId uint32, umcr create.UMCR, data *create.UniversalModelData) error {
	if r.modelsManager == nil {
		return fmt.Errorf("модельный менеджер не инициализирован")
	}
	return r.modelsManager.SaveModel(userId, umcr, data)
}

// ReadModel читает модель пользователя по провайдеру
func (r *ModelRouter) ReadModel(userId uint32, provider *create.ProviderType) (*create.UniversalModelData, error) {
	if r.modelsManager == nil {
		return nil, fmt.Errorf("модельный менеджер не инициализирован")
	}
	return r.modelsManager.ReadModel(userId, provider)
}

// GetAllModelAsJSON получает модель в виде JSON
func (r *ModelRouter) GetAllModelAsJSON(userId uint32) ([]byte, error) {
	if r.modelsManager == nil {
		return nil, fmt.Errorf("модельный менеджер не инициализирован")
	}
	return r.modelsManager.GetModelAsJSON(userId)
}

// DeleteModel удаляет модель пользователя
func (r *ModelRouter) DeleteModel(userId uint32, provider create.ProviderType, deleteFiles bool, progressCallback func(string)) error {
	if r.modelsManager == nil {
		return fmt.Errorf("модельный менеджер не инициализирован")
	}
	return r.modelsManager.DeleteModel(userId, provider, deleteFiles, progressCallback)
}

// UpdateModelToDB обновляет модель в БД
func (r *ModelRouter) UpdateModelToDB(userId uint32, data *create.UniversalModelData) error {
	if r.modelsManager == nil {
		return fmt.Errorf("модельный менеджер не инициализирован")
	}
	return r.modelsManager.UpdateModelToDB(userId, data)
}

// UpdateModelEveryWhere обновляет модель везде (БД + провайдер)
func (r *ModelRouter) UpdateModelEveryWhere(userId uint32, data *create.UniversalModelData) error {
	if r.modelsManager == nil {
		return fmt.Errorf("модельный менеджер не инициализирован")
	}
	return r.modelsManager.UpdateModelEveryWhere(userId, data)
}

// GetUserModels получает все модели пользователя
func (r *ModelRouter) GetUserModels(userId uint32) ([]create.UniversalModelData, error) {
	if r.modelsManager == nil {
		return nil, fmt.Errorf("модельный менеджер не инициализирован")
	}
	return r.modelsManager.GetUserModels(userId)
}

// GetUserModelsResponse получает все модели пользователя в формате для API
func (r *ModelRouter) GetUserModelsResponse(userId uint32) (*create.UserModelsResponse, error) {
	if r.modelsManager == nil {
		return nil, fmt.Errorf("модельный менеджер не инициализирован")
	}
	return r.modelsManager.GetAllUserModelsResponse(userId)
}

// SetActiveUserModel переключает активную модель пользователя (в транзакции)
func (r *ModelRouter) SetActiveUserModel(userId uint32, provider create.ProviderType) error {
	if r.modelsManager == nil {
		return fmt.Errorf("модельный менеджер не инициализирован")
	}
	return r.modelsManager.SetActiveModelByProvider(userId, provider)
}

// GetUserModelByProvider получает модель пользователя по провайдеру
func (r *ModelRouter) GetUserModelByProvider(userId uint32, provider create.ProviderType) (*create.UniversalModelData, error) {
	if r.modelsManager == nil {
		return nil, fmt.Errorf("модельный менеджер не инициализирован")
	}
	return r.modelsManager.GetUserModelByProvider(userId, provider)
}

// GetRealUserID получает реальный userId через HTTP запрос к landing серверу
// Работает независимо от modelsManager (использует собственный landingPort)
func (r *ModelRouter) GetRealUserID(userId uint32) (uint64, error) {
	// Если есть modelsManager, используем его (для совместимости)
	if r.modelsManager != nil {
		return r.modelsManager.GetRealUserID(userId)
	}

	// Fallback: делаем HTTP запрос самостоятельно
	var url string
	if mode.ProductionMode {
		url = fmt.Sprintf("http://localhost:%s/uid?uid=%d", r.landingPort, userId)
	} else {
		url = fmt.Sprintf("https://localhost:%s/uid?uid=%d", r.landingPort, userId)
	}

	tr := &http.Transport{
		TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
	}
	client := &http.Client{
		Transport: tr,
		Timeout:   5 * time.Second,
	}

	resp, err := client.Get(url)
	if err != nil {
		return 0, fmt.Errorf("ошибка при запросе GetRealUserID: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return 0, fmt.Errorf("неожиданный статус ответа GetRealUserID: %d", resp.StatusCode)
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return 0, fmt.Errorf("ошибка чтения ответа GetRealUserID: %v", err)
	}

	var userID uint64
	if err := json.Unmarshal(body, &userID); err != nil {
		return 0, fmt.Errorf("ошибка парсинга JSON ответа GetRealUserID: %v", err)
	}

	return userID, nil
}

// InvalidateUserAgentConfigCache инвалидирует кэш конфигурации модели для пользователя
// Вызывается при обновлении модели чтобы новые сессии получили актуальные настройки
// Работает со всеми провайдерами (OpenAI, Mistral, Google)
func (mr *ModelRouter) InvalidateUserAgentConfigCache(userId uint32) {
	if mr.openai != nil {
		mr.openai.InvalidateUserAgentConfigCache(userId)
	}
	if mr.mistral != nil {
		mr.mistral.InvalidateUserAgentConfigCache(userId)
	}
	if mr.google != nil {
		mr.google.InvalidateUserAgentConfigCache(userId)
	}
	//logger.Debug("Инвалидирован кэш конфигурации модели для userId=%d во всех провайдерах", userId)
}
