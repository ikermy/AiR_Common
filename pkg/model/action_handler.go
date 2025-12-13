package model

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/ikermy/AiR_Common/pkg/logger"
	"github.com/ikermy/AiR_Common/pkg/mode"

	"github.com/sashabaranov/go-openai"
)

// UniversalActionHandler универсальный обработчик функций для всех провайдеров
type UniversalActionHandler struct{}

// Для обратной совместимости
type ActionHandlerOpenAI = UniversalActionHandler
type ActionHandlerMistral = UniversalActionHandler

func (h *UniversalActionHandler) RunAction(ctx context.Context, functionName, arguments string) string {
	switch functionName {

	case "lead_target":
		logger.Debug("ActionHandlerOpenAI.RunAction: вызов функции lead_target с аргументами: %s", arguments)
		var params struct {
			Target bool `json:"target"`
		}

		if err := json.Unmarshal([]byte(arguments), &params); err != nil {
			return `{"error": "неверные параметры для lead_target"}`
		}

		// TODO сделать что-то при достижении цели диалога как в lead_haunter
		// Просто подтверждаем, что цель достигнута
		result, _ := json.Marshal(map[string]bool{"target": params.Target})
		return string(result)

	case "get_s3_files":
		logger.Debug("ActionHandlerOpenAI.RunAction: вызов функции get_s3_files с аргументами: %s", arguments)
		var params struct {
			UserID string `json:"user_id"`
		}

		if err := json.Unmarshal([]byte(arguments), &params); err != nil {
			return `{"error": "неверные параметры"}`
		}

		// Выполняем HTTP-запрос
		//resp, err := http.Get(fmt.Sprintf("%s/gets3?id=%s", mode.RealHost, params.UserID))
		req, err := http.NewRequestWithContext(ctx, "GET", fmt.Sprintf("%s/gets3?id=%s", mode.RealHost, params.UserID), nil)
		if err != nil {
			result, _ := json.Marshal(map[string]string{"error": "ошибка при выполнении запроса"})
			return string(result)
		}
		// Выполняем запрос
		client := &http.Client{}
		resp, err := client.Do(req)
		if err != nil {
			// Проверяем отмену по контексту
			if ctx.Err() != nil {
				result, _ := json.Marshal(map[string]string{"error": "запрос отменён по таймауту"})
				return string(result)
			}
			result, _ := json.Marshal(map[string]string{"error": "ошибка при выполнении запроса"})
			return string(result)
		}
		defer resp.Body.Close()

		// Читаем ответ только для отладки!
		body, err := io.ReadAll(resp.Body)
		if err != nil {
			result, _ := json.Marshal(map[string]string{"error": "ошибка при чтении ответа"})
			return string(result)
		}

		logger.Debug("get_s3_files ответ сервера для пользователя %s: %s", params.UserID, string(body))

		// Возвращаем результат
		result, _ := json.Marshal(map[string]string{"output": string(body)})
		return string(result)

	case "create_file":
		logger.Debug("ActionHandlerOpenAI.RunAction: вызов функции create_file с аргументами: %s", arguments)
		var params struct {
			UserID   string `json:"user_id"`
			Content  string `json:"content"`
			FileName string `json:"file_name"`
		}

		if err := json.Unmarshal([]byte(arguments), &params); err != nil {
			return `{"error": "неверные параметры для create_file"}`
		}

		// Подготавливаем данные для POST запроса (структура точно соответствует серверу)
		requestData := struct {
			UserID   string `json:"user_id"`
			Content  string `json:"content"`
			FileName string `json:"file_name"`
		}{
			UserID:   params.UserID,
			Content:  params.Content,
			FileName: params.FileName,
		}

		jsonData, err := json.Marshal(requestData)
		if err != nil {
			return `{"error": "ошибка подготовки данных"}`
		}

		// Отправляем POST запрос с user_id в URL параметре
		req, err := http.NewRequestWithContext(ctx, "POST", fmt.Sprintf("%s/savefilein3", mode.RealHost), strings.NewReader(string(jsonData)))
		if err != nil {
			result, _ := json.Marshal(map[string]string{"error": "ошибка при сохранении файла"})
			return string(result)
		}
		req.Header.Set("Content-Type", "application/json")

		// Выполняем запрос
		client := &http.Client{}
		resp, err := client.Do(req)
		if err != nil {
			if ctx.Err() != nil {
				result, _ := json.Marshal(map[string]string{"error": "запрос отменён по таймауту"})
				return string(result)
			}
			result, _ := json.Marshal(map[string]string{"error": "ошибка при сохранении файла"})
			return string(result)
		}
		defer resp.Body.Close()

		// Читаем ответ только для отладки!
		body, err := io.ReadAll(resp.Body)
		if err != nil {
			result, _ := json.Marshal(map[string]string{"error": "ошибка при чтении ответа"})
			return string(result)
		}

		responseStr := strings.TrimSpace(string(body))
		logger.Debug("create_file ответ сервера для пользователя %s: %s", params.UserID, responseStr)

		return responseStr

	default:
		result, _ := json.Marshal(map[string]string{"error": fmt.Sprintf("Функция %s не поддерживается", functionName)})
		return string(result)
	}
}

// GetTools возвращает инструменты в формате нужного провайдера
func (h *UniversalActionHandler) GetTools(provider ProviderType) interface{} {
	// Определяем базовые функции
	functions := []map[string]interface{}{
		{
			"name":        "lead_target",
			"description": "Выполняется, когда цель диалога достигнута",
			"parameters": map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"target": map[string]interface{}{
						"type":        "boolean",
						"description": "true - цель достигнута",
					},
				},
				"required": []string{"target"},
			},
		},
		{
			"name":        "get_s3_files",
			"description": "Получает список файлов пользователя из S3 хранилища",
			"parameters": map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"user_id": map[string]interface{}{
						"type":        "string",
						"description": "ID пользователя для получения файлов",
					},
				},
				"required": []string{"user_id"},
			},
		},
		{
			"name":        "create_file",
			"description": "Создает и сохраняет файл в S3 хранилище пользователя",
			"parameters": map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"user_id": map[string]interface{}{
						"type":        "string",
						"description": "ID пользователя",
					},
					"content": map[string]interface{}{
						"type":        "string",
						"description": "Содержимое файла",
					},
					"file_name": map[string]interface{}{
						"type":        "string",
						"description": "Имя файла для сохранения",
					},
				},
				"required": []string{"user_id", "content", "file_name"},
			},
		},
	}

	// Для OpenAI конвертируем в формат openai.Tool
	if provider == ProviderOpenAI {
		tools := make([]openai.Tool, len(functions))
		for i, fn := range functions {
			tools[i] = openai.Tool{
				Type: openai.ToolTypeFunction,
				Function: &openai.FunctionDefinition{
					Name:        fn["name"].(string),
					Description: fn["description"].(string),
					Parameters:  fn["parameters"],
				},
			}
		}
		return tools
	}

	// Для Mistral возвращаем в формате map с типом "function"
	if provider == ProviderMistral {
		tools := make([]map[string]interface{}, len(functions))
		for i, fn := range functions {
			tools[i] = map[string]interface{}{
				"type":     "function",
				"function": fn,
			}
		}
		return tools
	}

	// По умолчанию возвращаем базовый формат
	return functions
}
