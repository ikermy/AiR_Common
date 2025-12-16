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
func (m *UniversalModel) deleteMistralModel(userId uint32, modelData *UserModelRecord, deleteFiles bool, progressCallback func(string)) error {
	if progressCallback != nil {
		progressCallback("🔄 Удаление Mistral агента...")
	}

	// Удаляем агента через API
	if m.mistralClient != nil {
		if err := m.mistralClient.deleteAgent(modelData.AssistId); err != nil {
			logger.Error("ошибка удаления Mistral агента %s: %v", modelData.AssistId, err)
			// Продолжаем удаление из БД даже если не удалось удалить из API
			if progressCallback != nil {
				progressCallback(fmt.Sprintf("⚠️ Не удалось удалить агент из Mistral API: %v", err))
			}
		} else {
			if progressCallback != nil {
				progressCallback(fmt.Sprintf("✅ Mistral агент %s удалён из API", modelData.AssistId))
			}
		}
	} else {
		logger.Warn("Mistral клиент не инициализирован, пропускаем удаление из API")
		if progressCallback != nil {
			progressCallback("⚠️ Mistral клиент не инициализирован, удаляем только из БД")
		}
	}

	if progressCallback != nil {
		progressCallback("✅ Mistral агент удалён из API")
	}

	logger.Info("Mistral модель успешно удалена из API для пользователя %d", userId, userId)
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
func (m *UniversalModel) updateMistralModelInPlace(userId uint32, existing, updated *UniversalModelData, modelJSON []byte) error {
	if m.mistralClient == nil {
		return fmt.Errorf("Mistral клиент не инициализирован")
	}

	// Для Mistral нужно удалить старого агента и создать нового
	// (Mistral API может не поддерживать PATCH/UPDATE агентов)

	existingModelData, err := m.db.GetModelByProvider(userId, existing.Provider)
	if err != nil || existingModelData == nil {
		return fmt.Errorf("ошибка получения записи модели: %w", err)
	}

	// Удаляем старого агента
	if err := m.mistralClient.deleteAgent(existingModelData.AssistId); err != nil {
		logger.Warn("Не удалось удалить старого Mistral агента %s: %v", existingModelData.AssistId, err)
	}

	// Создаем нового агента с обновленными данными
	umcr, err := m.mistralClient.createMistralAgent(
		updated.Name,
		updated.GptType.Name,
		fmt.Sprintf("Agent для пользователя %d", userId),
		updated.Prompt,
	)
	if err != nil {
		return fmt.Errorf("ошибка создания нового Mistral агента: %w", err)
	}

	// Сохраняем в БД
	if err := m.SaveModel(userId, umcr, updated); err != nil {
		return fmt.Errorf("ошибка сохранения обновленной модели в БД: %w", err)
	}

	logger.Info("Mistral Agent успешно обновлен для пользователя %d (новый ID: %s)", userId, umcr, userId)
	return nil
}

// createMistralModel создаёт Mistral Agent (внутренний метод)
func (m *UniversalModel) createMistralModel(userId uint32, gptName string, modelName string, modelJSON []byte) (UMCR, error) {
	if m.mistralClient == nil {
		return UMCR{}, fmt.Errorf("Mistral клиент не инициализирован")
	}
	// Парсим JSON для извлечения инструкций
	var modelData map[string]interface{}
	if err := json.Unmarshal(modelJSON, &modelData); err != nil {
		return UMCR{}, fmt.Errorf("ошибка при разборе JSON модели: %w", err)
	}
	instructions, ok := modelData["prompt"].(string)
	if !ok {
		return UMCR{}, fmt.Errorf("поле 'prompt' отсутствует или имеет неверный тип")
	}
	description := fmt.Sprintf("Agent для пользователя %d", userId)
	// Создаём агента через Mistral API
	umcr, err := m.mistralClient.createMistralAgent(modelName, gptName, description, instructions)
	if err != nil {
		return UMCR{}, fmt.Errorf("ошибка создания Mistral агента: %w", err)
	}
	return umcr, nil
}

// createMistralAgent создает нового агента с указанными параметрами
func (m *MistralAgentClient) createMistralAgent(name, model, description string, instructions string) (UMCR, error) {
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
		return UMCR{}, fmt.Errorf("ошибка сериализации запроса: %v", err)
	}

	req, err := http.NewRequestWithContext(m.ctx, http.MethodPost, baseURL, bytes.NewBuffer(body))
	if err != nil {
		return UMCR{}, fmt.Errorf("ошибка создания POST запроса: %v", err)
	}

	req.Header.Set("Authorization", "Bearer "+m.apiKey)
	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return UMCR{}, fmt.Errorf("ошибка HTTP запроса: %v", err)
	}
	defer resp.Body.Close()

	responseBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return UMCR{}, fmt.Errorf("ошибка чтения ответа: %v", err)
	}

	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusCreated {
		return UMCR{}, fmt.Errorf("API вернул статус %d: %s", resp.StatusCode, string(responseBody))
	}

	var response map[string]interface{}
	if err := json.Unmarshal(responseBody, &response); err != nil {
		return UMCR{}, fmt.Errorf("ошибка парсинга JSON: %v", err)
	}

	// Извлекаем ID созданного агента
	if id, ok := response["id"].(string); ok {
		return UMCR{
			AssistID: id,
			AllIds:   nil,
			Provider: ProviderMistral,
		}, nil
	}

	return UMCR{}, fmt.Errorf("не удалось получить ID созданного агента")
}
