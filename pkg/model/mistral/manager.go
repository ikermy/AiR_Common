package mistral

import (
	"fmt"

	models "github.com/ikermy/AiR_Common/pkg/model/create"
)

// CreateModel создаёт новую модель Mistral
// Делегирует вызов к UniversalModel из пакета create
func (m *MistralModel) CreateModel(userId uint32, provider models.ProviderType, gptName string, modelName string, modelJSON []byte, fileIDs []models.Ids) (models.UMCR, error) {
	// Создаем экземпляр UniversalModel для делегирования
	modelsManager := &models.UniversalModel{}

	return modelsManager.CreateModel(userId, provider, gptName, modelName, modelJSON, fileIDs)
}

// UploadFileToProvider - Mistral не поддерживает загрузку файлов
// Метод реализован для соответствия интерфейсу ModelManager
func (m *MistralModel) UploadFileToProvider(fileName string, fileData []byte) (string, error) {
	return "", fmt.Errorf("Mistral не поддерживает загрузку файлов в Provider. Используйте другой провайдер для работы с файлами")
}

// DeleteFileFromProvider - Mistral не поддерживает удаление файлов Provider
// Метод реализован для соответствия интерфейсу ModelManager
func (m *MistralModel) DeleteFileFromProvider(fileID string) error {
	return fmt.Errorf("Mistral не поддерживает удаление файлов из Provider. Используйте другой провайдер для работы с файлами")
}

// AddFileFromProvider - Mistral не поддерживает добавление файлов из Provider
// Метод реализован для соответствия интерфейсу ModelManager
func (m *MistralModel) AddFileFromFromProvider(userId uint32, fileID, fileName string) error {
	return fmt.Errorf("Mistral не поддерживает добавление файлов из Provider. Используйте другой провайдер для работы с файлами")
}
