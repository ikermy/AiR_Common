package models

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/ikermy/AiR_Common/pkg/logger"
)

// MistralAgentClient клиент для работы с Mistral Agents API
type MistralAgentClient struct {
	apiKey string
	url    string
	ctx    context.Context
}

// deleteMistralModel удаляет Mistral Agent (с поддержкой WS сообщений)
func (m *Models) deleteMistralModel(userId uint32, modelData *UniversalModelData, deleteFiles bool, progressCallback func(string)) error {
	if progressCallback != nil {
		progressCallback("🔄 Удаление Mistral агента...")
	}

	// Удаляем агента через API
	if m.mistralClient != nil {
		if err := m.mistralClient.deleteAgent(modelData.ModelID); err != nil {
			logger.Error("ошибка удаления Mistral агента %s: %v", modelData.ModelID, err)
			// Продолжаем удаление из БД даже если не удалось удалить из API
			if progressCallback != nil {
				progressCallback(fmt.Sprintf("⚠️ Не удалось удалить агент из Mistral API: %v", err))
			}
		} else {
			if progressCallback != nil {
				progressCallback(fmt.Sprintf("✅ Mistral агент %s удалён из API", modelData.ModelID))
			}
		}
	} else {
		logger.Warn("Mistral клиент не инициализирован, пропускаем удаление из API")
		if progressCallback != nil {
			progressCallback("⚠️ Mistral клиент не инициализирован, удаляем только из БД")
		}
	}

	if progressCallback != nil {
		progressCallback("🔄 Удаление модели из базы данных...")
	}

	// Удаляем из БД
	if err := m.db.DeleteUserGPT(userId); err != nil {
		return fmt.Errorf("ошибка удаления модели из БД: %w", err)
	}

	if progressCallback != nil {
		progressCallback("✅ Модель Mistral успешно удалена")
	}

	logger.Info("Mistral модель успешно удалена для пользователя %d", userId, userId)
	return nil
}

// deleteAgent удаляет Mistral Agent по ID
func (m *MistralAgentClient) deleteAgent(agentID string) error {
	// Убираем /completions из URL
	baseURL := strings.Replace(m.url, "/completions", "", 1)
	deleteURL := fmt.Sprintf("%s/%s", baseURL, agentID)

	req, err := http.NewRequestWithContext(m.ctx, http.MethodDelete, deleteURL, nil)
	if err != nil {
		return fmt.Errorf("ошибка создания DELETE запроса: %v", err)
	}

	req.Header.Set("Authorization", "Bearer "+m.apiKey)
	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("ошибка HTTP запроса: %v", err)
	}
	defer resp.Body.Close()

	responseBody, _ := io.ReadAll(resp.Body)

	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusNoContent && resp.StatusCode != http.StatusNotFound {
		return fmt.Errorf("API вернул статус %d: %s", resp.StatusCode, string(responseBody))
	}

	return nil
}

// updateMistralModelInPlace обновляет Mistral Agent
func (m *Models) updateMistralModelInPlace(userId uint32, existing, updated *UniversalModelData, modelJSON []byte) error {
	if m.mistralClient == nil {
		return fmt.Errorf("Mistral клиент не инициализирован")
	}

	// Для Mistral нужно удалить старого агента и создать нового
	// (Mistral API может не поддерживать PATCH/UPDATE агентов)

	// Удаляем старого агента
	if err := m.mistralClient.deleteAgent(existing.ModelID); err != nil {
		logger.Warn("Не удалось удалить старого Mistral агента %s: %v", existing.ModelID, err)
	}

	// Создаем нового агента с обновленными данными
	newAgentID, err := m.mistralClient.createMistralAgent(
		updated.ModelName,
		fmt.Sprintf("mistral-%d", updated.ModelType), // Можно улучшить
		fmt.Sprintf("Agent для пользователя %d", userId),
		updated.Instructions,
	)
	if err != nil {
		return fmt.Errorf("ошибка создания нового Mistral агента: %w", err)
	}

	// Обновляем ID агента
	updated.ModelID = newAgentID

	// Сохраняем в БД
	if err := m.SaveModel(userId, updated); err != nil {
		return fmt.Errorf("ошибка сохранения обновленной модели в БД: %w", err)
	}

	logger.Info("Mistral Agent успешно обновлен для пользователя %d (новый ID: %s)", userId, newAgentID, userId)
	return nil
}

// createMistralModel создаёт Mistral Agent (внутренний метод)
func (m *Models) createMistralModel(userId uint32, gptName string, gptId uint8, modelName string, modelJSON []byte) (string, error) {
	if m.mistralClient == nil {
		return "", fmt.Errorf("Mistral клиент не инициализирован")
	}
	// Парсим JSON для извлечения инструкций
	var modelData map[string]interface{}
	if err := json.Unmarshal(modelJSON, &modelData); err != nil {
		return "", fmt.Errorf("ошибка при разборе JSON модели: %w", err)
	}
	instructions, ok := modelData["prompt"].(string)
	if !ok {
		return "", fmt.Errorf("поле 'prompt' отсутствует или имеет неверный тип")
	}
	description := fmt.Sprintf("Agent для пользователя %d", userId)
	// Создаём агента через Mistral API
	agentID, err := m.mistralClient.createMistralAgent(modelName, gptName, description, instructions)
	if err != nil {
		return "", fmt.Errorf("ошибка создания Mistral агента: %w", err)
	}
	// Сохраняем в универсальном формате
	operator, _ := modelData["operator"].(bool)
	universalData := &UniversalModelData{
		Provider:     ProviderMistral,
		ModelID:      agentID,
		ModelName:    modelName,
		ModelType:    gptId,
		Instructions: instructions,
		FileIDs:      []Ids{}, // Mistral не поддерживает файлы
		VectorIDs:    []string{},
		IsOperator:   operator,
		RawData:      modelData,
	}
	if err := m.SaveModel(userId, universalData); err != nil {
		// Если не удалось сохранить в БД, удаляем агента
		_ = m.mistralClient.deleteAgent(agentID)
		return "", fmt.Errorf("ошибка сохранения модели в БД: %w", err)
	}
	logger.Info("Mistral Agent создан для пользователя %d (ID: %s)", userId, agentID, userId)
	return agentID, nil
}

// createMistralAgent создает нового агента с указанными параметрами
func (m *MistralAgentClient) createMistralAgent(name, model, description string, instructions string) (string, error) {
	// Убираем /completions из URL для endpoint создания агента
	baseURL := strings.Replace(m.url, "/completions", "", 1)

	payload := map[string]interface{}{
		"name":         name,
		"model":        model,
		"description":  description,
		"instructions": instructions,
	}

	body, err := json.Marshal(payload)
	if err != nil {
		return "", fmt.Errorf("ошибка сериализации запроса: %v", err)
	}

	req, err := http.NewRequestWithContext(m.ctx, http.MethodPost, baseURL, bytes.NewBuffer(body))
	if err != nil {
		return "", fmt.Errorf("ошибка создания POST запроса: %v", err)
	}

	req.Header.Set("Authorization", "Bearer "+m.apiKey)
	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("ошибка HTTP запроса: %v", err)
	}
	defer resp.Body.Close()

	responseBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("ошибка чтения ответа: %v", err)
	}

	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusCreated {
		return "", fmt.Errorf("API вернул статус %d: %s", resp.StatusCode, string(responseBody))
	}

	var response map[string]interface{}
	if err := json.Unmarshal(responseBody, &response); err != nil {
		return "", fmt.Errorf("ошибка парсинга JSON: %v", err)
	}

	// Извлекаем ID созданного агента
	if id, ok := response["id"].(string); ok {
		return id, nil
	}

	return "", fmt.Errorf("не удалось получить ID созданного агента")
}
