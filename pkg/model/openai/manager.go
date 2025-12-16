package openai

import (
	"fmt"
	"strings"

	"github.com/ikermy/AiR_Common/pkg/logger"
	models "github.com/ikermy/AiR_Common/pkg/model/create"
	"github.com/sashabaranov/go-openai"
)

// CreateModel создаёт новую модель OpenAI
// Делегирует вызов к OpenAIModel из пакета create
func (m *OpenAIModel) CreateModel(userId uint32, provider models.ProviderType, gptName string, modelName string, modelJSON []byte, fileIDs []models.Ids) (models.UMCR, error) {
	// Создаем экземпляр UniversalModel для делегирования
	modelsManager := &models.UniversalModel{}

	return modelsManager.CreateModel(userId, provider, gptName, modelName, modelJSON, fileIDs)
}

// UploadFileToOpenAI загружает файл в OpenAI
func (m *OpenAIModel) UploadFileToProvider(fileName string, fileData []byte) (string, error) {
	// Создаем экземпляр UniversalModel для делегирования
	modelsManager := &models.UniversalModel{
		// Инициализация нужных полей
	}

	return modelsManager.UploadFileToOpenAI(fileName, fileData)
}

// DeleteFileFromOpenAI удаляет файл из OpenAI
// Метод не экспортирован в пакете create, поэтому возвращаем ошибку
func (m *OpenAIModel) DeleteFileFromProvider(fileID string) error {
	// 1. Удаляем файл по его ID
	if err := m.client.DeleteFile(m.ctx, fileID); err != nil {
		// Если файл уже удален (not found), это не является критической ошибкой
		if !strings.Contains(err.Error(), "not found") {
			return fmt.Errorf("ошибка удаления файла из OpenAI: %w", err)
		}
		logger.Error("Файл %s уже был удален или не найден в OpenAI: %v", fileID, err)
	}

	// 2. Ищем и удаляем связанный Vector Store
	// Получаем список всех векторных хранилищ
	vsList, err := m.client.ListVectorStores(m.ctx, openai.Pagination{})
	if err != nil {
		return fmt.Errorf("ошибка получения списка Vector Stores: %w", err)
	}

	// Ищем Vector Store, который содержит наш файл
	for _, vs := range vsList.VectorStores {
		// Получаем список файлов для каждого Vector Store
		files, err := m.client.ListVectorStoreFiles(m.ctx, vs.ID, openai.Pagination{})
		if err != nil {
			logger.Error("Предупреждение: не удалось получить файлы для Vector Store %s: %v", vs.ID, err)
			continue
		}

		// Если в хранилище только один файл и его ID совпадает с нашим, удаляем хранилище
		if len(files.VectorStoreFiles) == 1 && files.VectorStoreFiles[0].ID == fileID {
			_, err := m.client.DeleteVectorStore(m.ctx, vs.ID)
			if err != nil {
				// Логируем ошибку, но не прерываем процесс, так как основной файл уже мог быть удален
				logger.Error("Предупреждение: не удалось удалить Vector Store %s: %v", vs.ID, err)
			} else {
				logger.Debug("Vector Store %s, связанный с файлом %s, успешно удален: %v", vs.ID, fileID, err)
			}
			// Прерываем цикл, так как нашли и обработали нужное хранилище
			break
		}
	}

	return nil
}

// AddFileFromOpenAI добавляет файл в векторное хранилище
func (m *OpenAIModel) AddFileFromFromProvider(userId uint32, fileID, fileName string) error {
	// Получаем данные пользовательского Vector Store
	vectorStoreID, err := m.db.GetUserVectorStorage(userId)
	if err != nil {
		return fmt.Errorf("ошибка получения векторного хранилища: %w", err)
	}

	type GPT struct {
		AssistId string
		Name     string
		Ids      models.VecIds
	}

	// Добавляем файл в существующий Vector Store
	_, err = m.client.CreateVectorStoreFile(m.ctx, vectorStoreID, openai.VectorStoreFileRequest{
		FileID: fileID,
	})
	if err != nil {
		return fmt.Errorf("ошибка добавления файла в Vector Store: %w", err)
	}

	logger.Debug("Файл %s успешно добавлен в Vector Store", fileName, userId)
	return nil
}
