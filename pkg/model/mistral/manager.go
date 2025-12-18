package mistral

import (
	"encoding/json"
	"fmt"

	"github.com/ikermy/AiR_Common/pkg/logger"
	models "github.com/ikermy/AiR_Common/pkg/model/create"
)

// CreateModel создаёт новую модель Mistral
// Делегирует вызов к UniversalModel из пакета create
func (m *MistralModel) CreateModel(userId uint32, provider models.ProviderType, gptName string, modelName string, modelJSON []byte, fileIDs []models.Ids) (models.UMCR, error) {
	// Создаем экземпляр UniversalModel для делегирования
	modelsManager := &models.UniversalModel{}

	return modelsManager.CreateModel(userId, provider, gptName, modelName, modelJSON, fileIDs)
}

// UploadFileToProvider загружает файл в Mistral Library
// Создаёт новую библиотеку или использует существующую для пользователя
// Один пользователь = одна библиотека
func (m *MistralModel) UploadFileToProvider(userId uint32, fileName string, fileData []byte) (string, error) {
	// 1. Получить или создать библиотеку для userId
	libraryID, err := m.getOrCreateUserLibrary(userId)
	if err != nil {
		logger.Error("Ошибка получения/создания библиотеки: %v", err, userId)
		return "", fmt.Errorf("не удалось получить/создать библиотеку: %w", err)
	}

	// 2. Загрузить документ в библиотеку через Mistral API
	documentID, err := m.client.UploadDocumentToLibrary(libraryID, fileName, fileData)
	if err != nil {
		logger.Error("Ошибка загрузки документа %s в библиотеку %s: %v", fileName, libraryID, err, userId)
		return "", fmt.Errorf("не удалось загрузить документ в библиотеку: %w", err)
	}

	logger.Info("Документ %s успешно загружен в библиотеку %s для пользователя %d (ID: %s)", fileName, libraryID, userId, documentID, userId)

	// 3. Сохранить информацию о файле в БД (в FileIds)
	if err := m.addFileToDatabase(userId, documentID, fileName); err != nil {
		logger.Error("Ошибка сохранения информации о файле %s в БД: %v", fileName, err, userId)
		// Не возвращаем ошибку - файл уже загружен в Mistral, просто логируем
	}

	// 4. Вернуть documentID
	return documentID, nil
}

// DeleteDocumentFromLibrary удаляет документ из библиотеки пользователя Mistral
// Один пользователь = одна библиотека
// Если после удаления файла библиотека пустая - удаляет саму библиотеку
func (m *MistralModel) DeleteDocumentFromLibrary(userId uint32, documentID string) error {
	if documentID == "" {
		return fmt.Errorf("documentID не может быть пустым")
	}

	// Получаем библиотеку пользователя из БД
	libraryID, err := m.getUserLibraryID(userId)
	if err != nil {
		logger.Error("Ошибка получения библиотеки для пользователя %d: %v", userId, err, userId)
		return fmt.Errorf("не удалось получить библиотеку пользователя: %w", err)
	}

	// Удаляем документ через Mistral API
	// Согласно документации: DELETE /v1/libraries/{library_id}/documents/{document_id}
	err = m.client.DeleteDocumentFromLibrary(libraryID, documentID)
	if err != nil {
		logger.Error("Ошибка удаления документа %s из библиотеки %s: %v", documentID, libraryID, err, userId)
		return fmt.Errorf("не удалось удалить документ из библиотеки: %w", err)
	}

	logger.Info("Документ %s успешно удалён из библиотеки %s для пользователя %d", documentID, libraryID, userId, userId)

	// Удаляем информацию о файле из БД (из FileIds)
	remainingFiles, err := m.removeFileFromDatabase(userId, documentID)
	if err != nil {
		logger.Error("Ошибка удаления информации о файле %s из БД: %v", documentID, err, userId)
		// Не возвращаем ошибку - файл уже удален из Mistral, просто логируем
	}

	// Если после удаления файла в библиотеке не осталось документов - удаляем саму библиотеку
	if remainingFiles == 0 {
		logger.Info("В библиотеке %s не осталось файлов, удаляем её для пользователя %d", libraryID, userId, userId)

		// Удаляем библиотеку через Mistral API
		if err := m.client.DeleteLibrary(libraryID); err != nil {
			logger.Error("Ошибка удаления пустой библиотеки %s: %v", libraryID, err, userId)
			// Не критично, просто логируем
		} else {
			logger.Info("Пустая библиотека %s успешно удалена для пользователя %d", libraryID, userId, userId)
		}

		// Удаляем VectorId из БД (очищаем информацию о библиотеке)
		if err := m.clearLibraryID(userId); err != nil {
			logger.Error("Ошибка очистки library_id в БД: %v", err, userId)
			// Не критично
		}
	}

	return nil
}

// AddFileToLibrary добавляет документ в библиотеку пользователя Mistral
// Один пользователь = одна библиотека
// ПРИМЕЧАНИЕ: В Mistral файлы загружаются непосредственно в библиотеку
// Этот метод вызывается после UploadFileToProvider, когда файл уже загружен
// fileID - это documentID, который был возвращён при загрузке
func (m *MistralModel) AddFileToLibrary(userId uint32, fileID, fileName string) error {
	// В Mistral файлы загружаются сразу в библиотеку через UploadFileToProvider
	// Этот метод нужен для совместимости с интерфейсом, но фактически файл уже в библиотеке
	// Просто проверяем, что документ существует

	libraryID, err := m.getUserLibraryID(userId)
	if err != nil {
		logger.Error("Ошибка получения библиотеки для пользователя %d: %v", userId, err, userId)
		return fmt.Errorf("не удалось получить библиотеку пользователя: %w", err)
	}

	// Проверяем статус документа
	status, err := m.client.GetDocumentStatus(libraryID, fileID)
	if err != nil {
		logger.Error("Ошибка проверки статуса документа %s в библиотеке %s: %v", fileID, libraryID, err, userId)
		return fmt.Errorf("не удалось проверить статус документа: %w", err)
	}

	logger.Info("Документ %s (%s) находится в библиотеке %s со статусом: %s", fileName, fileID, libraryID, status, userId)
	return nil
}

// getUserLibraryID получает ID библиотеки пользователя из БД
// Один пользователь = одна библиотека
func (m *MistralModel) getUserLibraryID(userId uint32) (string, error) {
	// Получаем все модели пользователя
	userModels, err := m.db.GetAllUserModels(userId)
	if err != nil {
		logger.Error("Ошибка получения моделей пользователя %d: %v", userId, err, userId)
		return "", fmt.Errorf("не удалось получить модели пользователя: %w", err)
	}

	// Ищем модель Mistral
	var mistralModel *models.UserModelRecord
	for i := range userModels {
		if userModels[i].Provider == models.ProviderMistral {
			mistralModel = &userModels[i]
			break
		}
	}

	if mistralModel == nil {
		logger.Error("Модель Mistral не найдена для пользователя %d", userId, userId)
		return "", fmt.Errorf("модель Mistral не найдена для пользователя %d", userId)
	}

	// Десериализуем данные модели из AllIds
	var vecIds models.VecIds
	if len(mistralModel.AllIds) > 0 {
		if err := json.Unmarshal(mistralModel.AllIds, &vecIds); err != nil {
			logger.Error("Ошибка десериализации AllIds для пользователя %d: %v", userId, err, userId)
			return "", fmt.Errorf("не удалось получить данные библиотеки: %w", err)
		}
	}

	// Библиотека хранится в VecIds.VectorId
	if len(vecIds.VectorId) == 0 {
		logger.Warn("Библиотека не найдена в модели", userId)
		return "", fmt.Errorf("библиотека не создана для пользователя %d", userId)
	}

	libraryID := vecIds.VectorId[0]
	logger.Debug("Получен library_id: %s для пользователя %d", libraryID, userId, userId)

	return libraryID, nil
}

// getOrCreateUserLibrary получает существующую библиотеку пользователя или создаёт новую
// Один пользователь = одна библиотека
func (m *MistralModel) getOrCreateUserLibrary(userId uint32) (string, error) {
	// Пытаемся получить существующую библиотеку
	libraryID, err := m.getUserLibraryID(userId)
	if err == nil {
		// Библиотека найдена
		logger.Debug("Найдена существующая библиотека %s для пользователя %d", libraryID, userId, userId)
		return libraryID, nil
	}

	// Библиотека не найдена, создаём новую
	logger.Info("Создание новой библиотеки для пользователя %d", userId, userId)

	libraryName := fmt.Sprintf("Library_User_%d", userId)
	libraryDescription := fmt.Sprintf("Библиотека документов для пользователя %d", userId)

	library, err := m.client.CreateLibrary(libraryName, libraryDescription)
	if err != nil {
		logger.Error("Ошибка создания библиотеки для пользователя %d: %v", userId, err, userId)
		return "", fmt.Errorf("не удалось создать библиотеку: %w", err)
	}

	// Сохраняем library_id в БД
	err = m.saveLibraryID(userId, library.ID)
	if err != nil {
		// Пытаемся удалить созданную библиотеку при ошибке сохранения
		_ = m.client.DeleteLibrary(library.ID)
		logger.Error("Ошибка сохранения library_id в БД для пользователя %d: %v", userId, err, userId)
		return "", fmt.Errorf("не удалось сохранить library_id в БД: %w", err)
	}

	logger.Info("Создана новая библиотека %s для пользователя %d", library.ID, userId, userId)
	return library.ID, nil
}

// saveLibraryID сохраняет ID библиотеки в модели пользователя
func (m *MistralModel) saveLibraryID(userId uint32, libraryID string) error {
	// Получаем все модели пользователя
	userModels, err := m.db.GetAllUserModels(userId)
	if err != nil {
		logger.Error("Ошибка получения моделей для сохранения library_id: %v", err, userId)
		return fmt.Errorf("не удалось получить модели пользователя: %w", err)
	}

	// Ищем модель Mistral
	var mistralModel *models.UserModelRecord
	for i := range userModels {
		if userModels[i].Provider == models.ProviderMistral {
			mistralModel = &userModels[i]
			break
		}
	}

	if mistralModel == nil {
		logger.Error("Модель Mistral не найдена для пользователя %d", userId, userId)
		return fmt.Errorf("модель Mistral не найдена для пользователя %d", userId)
	}

	// Десериализуем текущие данные из AllIds
	var vecIds models.VecIds
	if len(mistralModel.AllIds) > 0 {
		if err := json.Unmarshal(mistralModel.AllIds, &vecIds); err != nil {
			logger.Warn("Ошибка десериализации AllIds, создаём новую структуру: %v", err, userId)
			vecIds = models.VecIds{
				FileIds: mistralModel.FileIds, // Сохраняем существующие файлы
			}
		}
	} else {
		vecIds = models.VecIds{
			FileIds: mistralModel.FileIds,
		}
	}

	// Обновляем VectorId с новым library_id
	vecIds.VectorId = []string{libraryID}

	// Сериализуем обновлённые данные
	updatedAllIds, err := json.Marshal(vecIds)
	if err != nil {
		logger.Error("Ошибка сериализации VecIds: %v", err, userId)
		return fmt.Errorf("не удалось сериализовать данные библиотеки: %w", err)
	}

	// Обновляем AllIds в БД напрямую через метод БД
	err = m.db.UpdateUserGPT(userId, mistralModel.ModelId, mistralModel.AssistId, updatedAllIds)
	if err != nil {
		logger.Error("Ошибка сохранения library_id в БД: %v", err, userId)
		return fmt.Errorf("не удалось обновить модель с library_id: %w", err)
	}

	logger.Info("Library ID %s успешно сохранён для пользователя %d", libraryID, userId, userId)
	return nil
}

// addFileToDatabase добавляет информацию о файле в FileIds БД
func (m *MistralModel) addFileToDatabase(userId uint32, fileID, fileName string) error {
	// Получаем все модели пользователя
	userModels, err := m.db.GetAllUserModels(userId)
	if err != nil {
		return fmt.Errorf("не удалось получить модели пользователя: %w", err)
	}

	// Ищем модель Mistral
	var mistralModel *models.UserModelRecord
	for i := range userModels {
		if userModels[i].Provider == models.ProviderMistral {
			mistralModel = &userModels[i]
			break
		}
	}

	if mistralModel == nil {
		return fmt.Errorf("модель Mistral не найдена для пользователя %d", userId)
	}

	// Десериализуем текущие данные из AllIds
	var vecIds models.VecIds
	if len(mistralModel.AllIds) > 0 {
		if err := json.Unmarshal(mistralModel.AllIds, &vecIds); err != nil {
			logger.Warn("Ошибка десериализации AllIds: %v", err, userId)
			// Создаём новую структуру, сохраняя FileIds из mistralModel
			vecIds = models.VecIds{
				FileIds:  mistralModel.FileIds,
				VectorId: []string{}, // Пустой, т.к. не смогли прочитать
			}
		}
	} else {
		// AllIds пусто - создаём новую структуру
		vecIds = models.VecIds{
			FileIds:  []models.Ids{},
			VectorId: []string{},
		}
	}

	// Инициализируем FileIds если nil
	if vecIds.FileIds == nil {
		vecIds.FileIds = []models.Ids{}
	}

	// Проверяем, нет ли уже такого файла
	for _, existingFile := range vecIds.FileIds {
		if existingFile.ID == fileID {
			logger.Warn("Файл %s уже существует в FileIds", fileID, userId)
			return nil // Не ошибка, просто уже есть
		}
	}

	// Добавляем новый файл
	vecIds.FileIds = append(vecIds.FileIds, models.Ids{
		ID:   fileID,
		Name: fileName,
	})

	// Сериализуем обратно
	updatedAllIds, err := json.Marshal(vecIds)
	if err != nil {
		return fmt.Errorf("не удалось сериализовать VecIds: %w", err)
	}

	// Обновляем в БД
	err = m.db.UpdateUserGPT(userId, mistralModel.ModelId, mistralModel.AssistId, updatedAllIds)
	if err != nil {
		return fmt.Errorf("не удалось обновить FileIds в БД: %w", err)
	}

	logger.Info("Файл %s (%s) добавлен в FileIds для пользователя %d", fileName, fileID, userId, userId)
	return nil
}

// removeFileFromDatabase удаляет информацию о файле из FileIds БД
// Возвращает количество оставшихся файлов после удаления
func (m *MistralModel) removeFileFromDatabase(userId uint32, fileID string) (int, error) {
	// Получаем все модели пользователя
	userModels, err := m.db.GetAllUserModels(userId)
	if err != nil {
		return 0, fmt.Errorf("не удалось получить модели пользователя: %w", err)
	}

	// Ищем модель Mistral
	var mistralModel *models.UserModelRecord
	for i := range userModels {
		if userModels[i].Provider == models.ProviderMistral {
			mistralModel = &userModels[i]
			break
		}
	}

	if mistralModel == nil {
		return 0, fmt.Errorf("модель Mistral не найдена для пользователя %d", userId)
	}

	// Десериализуем текущие данные из AllIds
	var vecIds models.VecIds
	if len(mistralModel.AllIds) > 0 {
		if err := json.Unmarshal(mistralModel.AllIds, &vecIds); err != nil {
			logger.Warn("Ошибка десериализации AllIds: %v", err, userId)
			return 0, nil // Не критично, просто нечего удалять
		}
	}

	// Если FileIds пусто, то нечего удалять
	if len(vecIds.FileIds) == 0 {
		logger.Warn("FileIds пусто, нечего удалять для файла %s", fileID, userId)
		return 0, nil
	}

	// Ищем и удаляем файл
	found := false
	newFileIds := make([]models.Ids, 0, len(vecIds.FileIds))
	for _, file := range vecIds.FileIds {
		if file.ID == fileID {
			found = true
			continue // Пропускаем этот файл (удаляем)
		}
		newFileIds = append(newFileIds, file)
	}

	if !found {
		logger.Warn("Файл %s не найден в FileIds", fileID, userId)
		return len(vecIds.FileIds), nil // Возвращаем текущее количество
	}

	// Обновляем FileIds
	vecIds.FileIds = newFileIds

	// Сериализуем обратно
	updatedAllIds, err := json.Marshal(vecIds)
	if err != nil {
		return 0, fmt.Errorf("не удалось сериализовать VecIds: %w", err)
	}

	// Обновляем в БД
	err = m.db.UpdateUserGPT(userId, mistralModel.ModelId, mistralModel.AssistId, updatedAllIds)
	if err != nil {
		return 0, fmt.Errorf("не удалось обновить FileIds в БД: %w", err)
	}

	remainingCount := len(newFileIds)
	logger.Info("Файл %s удалён из FileIds для пользователя %d (осталось файлов: %d)", fileID, userId, remainingCount, userId)
	return remainingCount, nil
}

// clearLibraryID очищает ID библиотеки из модели пользователя (устанавливает AllIds в NULL)
// Вызывается после удаления пустой библиотеки из Mistral API
func (m *MistralModel) clearLibraryID(userId uint32) error {
	// Получаем все модели пользователя
	userModels, err := m.db.GetAllUserModels(userId)
	if err != nil {
		return fmt.Errorf("не удалось получить модели пользователя: %w", err)
	}

	// Ищем модель Mistral
	var mistralModel *models.UserModelRecord
	for i := range userModels {
		if userModels[i].Provider == models.ProviderMistral {
			mistralModel = &userModels[i]
			break
		}
	}

	if mistralModel == nil {
		return fmt.Errorf("модель Mistral не найдена для пользователя %d", userId)
	}

	// Устанавливаем AllIds в NULL (пустой массив байт)
	// При этом БД сохранит NULL вместо пустого JSON
	var emptyAllIds []byte = nil

	// Обновляем в БД
	err = m.db.UpdateUserGPT(userId, mistralModel.ModelId, mistralModel.AssistId, emptyAllIds)
	if err != nil {
		return fmt.Errorf("не удалось обновить VectorId в БД: %w", err)
	}

	logger.Info("Library ID очищен (установлен NULL) для пользователя %d", userId, userId)
	return nil
}
