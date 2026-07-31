package create

import (
	"encoding/json"
	"fmt"

	"github.com/ikermy/air_common/pkg/model/domain"
)

// ReadModel получает модель из БД в универсальном формате
// Если provider != nil - получает модель конкретного провайдера
// Если provider == nil - получает активную модель пользователя
// Работает для любого провайдера (OpenAI, Mistral...)
func (m *UniversalModel) ReadModel(userID uint32, provider *domain.ProviderType) (*domain.UniversalModelData, error) {
	var record *domain.UserModelRecord
	var err error

	// Если провайдер не указан - получаем активную модель
	if provider == nil {
		record, err = m.db.GetActiveModel(userID)
		if err != nil {
			return nil, fmt.Errorf("ошибка получения активной модели: %w", err)
		}
		if record == nil {
			//logger.Debug("Активная модель не найдена", userID)
			return nil, nil
		}
		//logger.Debug("Получение активной модели (Provider: %s)", record.Provider, userID)
	} else {
		// Получаем модель конкретного провайдера
		record, err = m.db.GetModelByProvider(userID, *provider)
		if err != nil {
			return nil, fmt.Errorf("ошибка получения модели провайдера %s: %w", *provider, err)
		}
		if record == nil {
			//logger.Debug("Модель провайдера %s не найдена", *provider, userID)
			return nil, nil
		}
	}

	// Получаем данные из БД по провайдеру
	compressedData, vecIds, err := m.db.ReadUserModelByProvider(userID, record.Provider)
	if err != nil {
		return nil, fmt.Errorf("ошибка получения модели из БД: %w", err)
	}

	if compressedData == nil {
		return nil, nil
	}

	// Используем вспомогательный метод для распаковки
	modelData, err := m.DecompressModelData(compressedData, vecIds)
	if err != nil {
		return nil, err
	}

	// Устанавливаем провайдера и AssistantId из БД
	modelData.Provider = record.Provider
	modelData.UseModelName = &domain.UseModelName{GptType: record.GptType, Realtime: record.Realtime}

	//logger.Debug("Модель успешно загружена (Provider: %s, Name: %s, IsActive: %v)",
	//	modelData.Provider, modelData.Name, record.IsActive, userID)

	return modelData, nil
}

// GetModelAsJSON получает ВСЕ модели пользователя и возвращает их как JSON
// Предназначен для HTTP API endpoints - возвращает готовый JSON для отправки клиенту.
// Возвращает объект с моделями по провайдерам и информацией об активной модели:
func (m *UniversalModel) GetModelAsJSON(userID uint32) (json.RawMessage, error) {
	// Получаем все модели пользователя
	response, err := m.GetAllUserModelsResponse(userID)
	if err != nil {
		return nil, err
	}
	// Если нет моделей, возвращаем пустой JSON объект
	if len(response.Models) == 0 {
		return json.RawMessage(`{}`), nil
	}
	// Сериализуем в JSON
	result, err := json.Marshal(response)
	if err != nil {
		return nil, fmt.Errorf("ошибка сериализации моделей в JSON: %w", err)
	}

	return result, nil
}

// DeleteModel удаляет модель из БД и удаляет связанные ресурсы,
// работает для любого провайдера (OpenAI, Mistral)
// Если удаляется активная модель и есть другие модели - автоматически переключает активную
// progressCallback - функция для отправки статуса через WebSocket (с эмодзи)
func (m *UniversalModel) DeleteModel(userID uint32, provider domain.ProviderType, deleteFiles bool, progressCallback func(string)) error {
	if progressCallback != nil {
		progressCallback("🔄 Получение информации о модели пользователя...")
	}

	// Получаем все модели пользователя
	allModels, err := m.db.GetAllUserModels(userID)
	if err != nil {
		return fmt.Errorf("ошибка получения моделей пользователя: %w", err)
	}

	// Находим модель с нужным провайдером
	var modelRecord *domain.UserModelRecord
	for i := range allModels {
		if allModels[i].Provider == provider {
			modelRecord = &allModels[i]
			break
		}
	}

	if modelRecord == nil {
		return fmt.Errorf("модель с провайдером %s не найдена для пользователя", provider.String())
	}

	// В зависимости от провайдера удаляем модель
	switch modelRecord.Provider {
	case domain.ProviderOpenAI:
		err = m.deleteModel(userID, modelRecord, deleteFiles, progressCallback)
		if err != nil {
			return err
		}

	case domain.ProviderMistral:
		err = m.deleteMistralModel(userID, modelRecord, deleteFiles, progressCallback)
		if err != nil {
			return err
		}

	case domain.ProviderGoogle:
		err = m.deleteGoogleModel(userID, modelRecord, deleteFiles, progressCallback)
		if err != nil {
			return err
		}

	default:
		return fmt.Errorf("неизвестный провайдер: %s", modelRecord.Provider)
	}

	// Удаляем связь из user_models
	if progressCallback != nil {
		progressCallback("🔄 Удаление связи пользователь-модель...")
	}

	err = m.db.RemoveModelFromUser(userID, modelRecord.ModelId)
	if err != nil {
		return fmt.Errorf("ошибка удаления связи из user_models: %w", err)
	}

	// Если удалённая модель была активной - переключаем на оставшуюся
	if modelRecord.IsActive {
		remainingModels, err := m.db.GetAllUserModels(userID)
		if err != nil {
			//logger.Warn("Ошибка получения оставшихся моделей: %v", err, userID)
		} else if len(remainingModels) > 0 {
			// Переключаем на первую оставшуюся модель по провайдеру
			newActiveProvider := remainingModels[0].Provider
			err = m.db.SetActiveModelByProvider(userID, newActiveProvider)
			if err != nil {
				//logger.Error("Ошибка автоматического переключения активной модели: %v", err, userID)
			} else {
				//logger.Debug("Активная модель автоматически переключена на провайдер %s после удаления",
				//	newActiveProvider.String(), userID)
				if progressCallback != nil {
					progressCallback(fmt.Sprintf("✅ Активная модель переключена на %s", newActiveProvider.String()))
				}
			}
		}
	}

	if progressCallback != nil {
		progressCallback(fmt.Sprintf("✅ Модель %s успешно удалена", modelRecord.Provider))
	}

	return nil
}

// UpdateModelToDB обновляет существующую модель (только БД, без обновления в API провайдера)
// Используйте UpdateModelEveryWhere для полного обновления
func (m *UniversalModel) UpdateModelToDB(userID uint32, data *domain.UniversalModelData) error {
	// Проверяем существование модели
	provider := data.Provider
	existing, err := m.ReadModel(userID, &provider)
	if err != nil {
		return fmt.Errorf("ошибка проверки существующей модели: %w", err)
	}

	if existing == nil {
		return fmt.Errorf("модель провайдера %s не найдена для пользователя %d", provider, userID)
	}

	// Получаем все модели пользователя и находим нужную
	allModels, err := m.db.GetAllUserModels(userID)
	if err != nil {
		return fmt.Errorf("ошибка получения моделей пользователя: %w", err)
	}

	var existingModelData *domain.UserModelRecord
	for i := range allModels {
		if allModels[i].Provider == provider {
			existingModelData = &allModels[i]
			break
		}
	}

	if existingModelData == nil {
		return fmt.Errorf("запись модели провайдера %s не найдена для пользователя %d", provider, userID)
	}

	// Сериализуем vecIds в JSON
	vecIdsJSON, err := json.Marshal(data.VecIds)
	if err != nil {
		return fmt.Errorf("failed to marshal vector IDs: %w", err)
	}

	// Сохраняем обновленные данные
	return m.SaveModel(userID, domain.UMCR{
		AssistID: existingModelData.AssistId,
		AllIds:   vecIdsJSON,
		Provider: data.Provider,
	}, data)
}

// UpdateModelEveryWhere полностью обновляет модель:
// - Обновляет модель в API провайдера (OpenAI Assistant или Mistral Agent)
// - Управляет файлами и векторными хранилищами
// - Сохраняет изменения в БД
func (m *UniversalModel) UpdateModelEveryWhere(userID uint32, data *domain.UniversalModelData) error {
	// Получаем текущую модель (любого статуса активности)
	provider := data.Provider
	record, err := m.db.GetModelByProviderAnyStatus(userID, provider)
	if err != nil {
		return fmt.Errorf("ошибка получения текущей модели: %w", err)
	}

	if record == nil {
		return fmt.Errorf("модель провайдера %s не найдена для пользователя %d", provider, userID)
	}

	// Распаковываем существующую модель из БД
	compressedData, vecIds, err := m.db.ReadUserModelByProvider(userID, provider)
	if err != nil {
		return fmt.Errorf("ошибка получения данных текущей модели: %w", err)
	}

	if compressedData == nil {
		return fmt.Errorf("данные модели провайдера %s не найдены для пользователя %d", provider, userID)
	}

	existing, err := m.DecompressModelData(compressedData, vecIds)
	if err != nil {
		return fmt.Errorf("ошибка распаковки данных модели: %w", err)
	}

	// Устанавливаем провайдера из БД (он не хранится в Data)
	existing.Provider = provider
	existing.UseModelName = &domain.UseModelName{GptType: record.GptType, Realtime: record.Realtime}

	// Проверяем, что провайдер не изменился
	if data.Provider != existing.Provider {
		return fmt.Errorf("нельзя изменить провайдера модели (было: %s, стало: %s)", existing.Provider, data.Provider)
	}

	// Обновляем в зависимости от провайдера
	switch data.Provider {
	case domain.ProviderOpenAI:
		return m.updateOpenAIModelInPlace(userID, existing, data)

	case domain.ProviderMistral:
		return m.updateMistralModelInPlace(userID, existing, data)

	case domain.ProviderGoogle:
		return m.updateGoogleModelInPlace(userID, existing, data)

	default:
		return fmt.Errorf("неизвестный провайдер: %s", data.Provider)
	}
}

// ============================================================================
// Методы для работы с множественными моделями
// ============================================================================

// GetUserModels получает все модели пользователя
func (m *UniversalModel) GetUserModels(userID uint32) ([]domain.UniversalModelData, error) {
	records, err := m.db.GetAllUserModels(userID)
	if err != nil {
		return nil, fmt.Errorf("ошибка получения моделей пользователя: %w", err)
	}

	if len(records) == 0 {
		return []domain.UniversalModelData{}, nil
	}

	models := make([]domain.UniversalModelData, 0, len(records))
	for _, record := range records {
		// Читаем данные модели по провайдеру
		compressedData, vecIds, err := m.db.ReadUserModelByProvider(userID, record.Provider)
		if err != nil {
			//logger.Warn("Пропуск модели %d (Provider: %s): ошибка чтения данных: %v", record.ModelId, record.Provider, err, userID)
			continue
		}

		if compressedData == nil {
			//logger.Warn("Пропуск модели %d (Provider: %s): данные отсутствуют", record.ModelId, record.Provider, userID)
			continue
		}

		// Распаковка данных
		modelData, err := m.DecompressModelData(compressedData, vecIds)
		if err != nil {
			//logger.Warn("Пропуск модели %d (Provider: %s): ошибка распаковки: %v", record.ModelId, record.Provider, err, userID)
			continue
		}

		// Обновляем провайдера и AssistantId из БД
		modelData.Provider = record.Provider
		modelData.UseModelName = &domain.UseModelName{GptType: record.GptType, Realtime: record.Realtime}
		models = append(models, *modelData)
	}

	//logger.Debug("Загружено %d моделей", len(models), userID)
	return models, nil
}

// GetAllUserModelsResponse получает все модели пользователя в формате для API
// Возвращает объект с моделями по провайдерам и информацией об активной модели
func (m *UniversalModel) GetAllUserModelsResponse(userID uint32) (*domain.UserModelsResponse, error) {
	records, err := m.db.GetAllUserModels(userID)
	if err != nil {
		return nil, fmt.Errorf("ошибка получения моделей пользователя: %w", err)
	}
	response := &domain.UserModelsResponse{
		Models: make(map[string]*domain.UniversalModelData),
	}

	var activeProvider domain.ProviderType

	for _, record := range records {
		// Читаем данные модели по провайдеру
		compressedData, vecIds, err := m.db.ReadUserModelByProvider(userID, record.Provider)
		if err != nil {
			//logger.Warn("Пропуск модели %d (Provider: %s): ошибка чтения данных: %v",
			//	record.ModelId, record.Provider, err, userID)
			continue
		}

		if compressedData == nil {
			//logger.Warn("Пропуск модели %d (Provider: %s): данные отсутствуют",
			//	record.ModelId, record.Provider, userID)
			continue
		}

		// Распаковка данных
		modelData, err := m.DecompressModelData(compressedData, vecIds)
		if err != nil {
			//logger.Warn("Пропуск модели %d (Provider: %s): ошибка распаковки: %v",
			//	record.ModelId, record.Provider, err, userID)
			continue
		}
		// Устанавливаем провайдера из user_models
		modelData.Provider = record.Provider
		// Имена и ID моделей берём из каталогов моделей БД, а не из сжатого JSON.
		// Это гарантирует актуальные значения после синхронизации каталогов.
		modelData.UseModelName = &domain.UseModelName{
			GptType:  record.GptType,
			Realtime: record.Realtime,
		}

		// Сохраняем активный провайдер
		if record.IsActive {
			activeProvider = record.Provider
		}

		// Добавляем модель в map по строковому ключу провайдера
		response.Models[record.Provider.String()] = modelData
	}

	// Устанавливаем активный провайдер
	if activeProvider != 0 {
		response.ActiveProvider = activeProvider.String()
	}

	return response, nil
}

// GetActiveUserModel получает активную модель пользователя
func (m *UniversalModel) GetActiveUserModel(userID uint32) (*domain.UniversalModelData, error) {
	record, err := m.db.GetActiveModel(userID)
	if err != nil {
		return nil, fmt.Errorf("ошибка получения активной модели: %w", err)
	}

	if record == nil {
		//logger.Debug("Активная модель не найдена", userID)
		return nil, nil
	}

	// Читаем данные модели по провайдеру
	compressedData, vecIds, err := m.db.ReadUserModelByProvider(userID, record.Provider)
	if err != nil {
		return nil, fmt.Errorf("ошибка чтения данных активной модели: %w", err)
	}

	if compressedData == nil {
		return nil, nil
	}

	modelData, err := m.DecompressModelData(compressedData, vecIds)
	if err != nil {
		return nil, fmt.Errorf("ошибка распаковки активной модели: %w", err)
	}

	// Устанавливаем провайдера и AssistantId из БД
	modelData.Provider = record.Provider
	modelData.UseModelName = &domain.UseModelName{GptType: record.GptType, Realtime: record.Realtime}

	//logger.Debug("Загружена активная модель (Provider: %s, Name: %s)",
	//	modelData.Provider, modelData.Name, userID)

	return modelData, nil
}

// GetUserModelByProvider получает модель пользователя по провайдеру
func (m *UniversalModel) GetUserModelByProvider(userID uint32, provider domain.ProviderType) (*domain.UniversalModelData, error) {
	record, err := m.db.GetModelByProviderAnyStatus(userID, provider)
	if err != nil {
		return nil, fmt.Errorf("ошибка получения модели по провайдеру %s: %w", provider, err)
	}

	if record == nil {
		//logger.Debug("Модель провайдера %s не найдена", provider, userID)
		return nil, nil
	}

	// Читаем данные модели по провайдеру
	compressedData, vecIds, err := m.db.ReadUserModelByProvider(userID, record.Provider)
	if err != nil {
		return nil, fmt.Errorf("ошибка чтения данных модели: %w", err)
	}

	if compressedData == nil {
		return nil, nil
	}

	modelData, err := m.DecompressModelData(compressedData, vecIds)
	if err != nil {
		return nil, fmt.Errorf("ошибка распаковки модели: %w", err)
	}

	// Устанавливаем провайдера и AssistantId из БД
	modelData.Provider = record.Provider
	modelData.UseModelName = &domain.UseModelName{GptType: record.GptType, Realtime: record.Realtime}

	//logger.Debug("Загружена модель провайдера %s (ID: %d)",
	//	provider, modelData.Provider, userID)

	return modelData, nil
}

// SetActiveModelByProvider переключает активную модель пользователя (в транзакции)
func (m *UniversalModel) SetActiveModelByProvider(userID uint32, provider domain.ProviderType) error {
	err := m.db.SetActiveModelByProvider(userID, provider)
	if err != nil {
		return fmt.Errorf("ошибка переключения активной модели: %w", err)
	}

	//logger.Debug("Активная модель переключена на %d", provider, userID)
	return nil
}
