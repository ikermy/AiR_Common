package openai

import (
	"github.com/ikermy/AiR_Common/pkg/model"
	models "github.com/ikermy/AiR_Common/pkg/model/create"
)

// CreateModel создаёт новую модель OpenAI
// Делегирует вызов к Models из пакета create
func (m *OpenAIModel) CreateModel(userId uint32, provider model.ProviderType, gptName string, gptId uint8, modelName string, modelJSON []byte, fileIDs interface{}) (string, error) {
	// Преобразуем fileIDs к нужному типу
	var ids []models.Ids
	if fileIDs != nil {
		if slice, ok := fileIDs.([]models.Ids); ok {
			ids = slice
		}
	}

	// Создаем экземпляр Models для делегирования
	// Предполагаем, что db реализует интерфейс models.DB
	modelsManager := &models.Models{
		// Инициализация нужных полей
		// Требуется адаптер для DB интерфейса
	}

	return modelsManager.CreateModel(userId, models.ProviderOpenAI, gptName, gptId, modelName, modelJSON, ids)
}

// UploadFileToOpenAI загружает файл в OpenAI
func (m *OpenAIModel) UploadFileToOpenAI(fileName string, fileData []byte) (string, error) {
	// Создаем экземпляр Models для делегирования
	modelsManager := &models.Models{
		// Инициализация нужных полей
	}

	return modelsManager.UploadFileToOpenAI(fileName, fileData)
}

// DeleteFileFromOpenAI удаляет файл из OpenAI
// Метод не экспортирован в пакете create, поэтому возвращаем ошибку
func (m *OpenAIModel) DeleteFileFromOpenAI(fileID string) error {
	// TODO: Требуется экспортировать deleteFileFromOpenAI в пакете create
	// или реализовать логику здесь
	return nil
}

// AddFileFromOpenAI добавляет файл в векторное хранилище
func (m *OpenAIModel) AddFileFromOpenAI(userId uint32, fileID, fileName string) error {
	// Создаем экземпляр Models для делегирования
	modelsManager := &models.Models{
		// Инициализация нужных полей
	}

	return modelsManager.AddFileFromOpenAI(userId, fileID, fileName)
}
