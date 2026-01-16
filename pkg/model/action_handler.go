package model

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"strings"

	"github.com/ikermy/AiR_Common/pkg/logger"
	"github.com/ikermy/AiR_Common/pkg/mode"
	"github.com/ikermy/AiR_Common/pkg/model/create"

	"github.com/sashabaranov/go-openai"
)

// UniversalActionHandler универсальный обработчик функций для всех провайдеров
type UniversalActionHandler struct{}

func (h *UniversalActionHandler) RunAction(ctx context.Context, functionName, arguments string) string {
	switch functionName {

	case "lead_target":
		//logger.Debug("ActionHandler: вызов функции lead_target с аргументами: %s", arguments)
		var params struct {
			RespId int64 `json:"resp_id"`
		}

		if err := json.Unmarshal([]byte(arguments), &params); err != nil {
			logger.Error("ActionHandler: ошибка парсинга параметров lead_target: %v", err)
			return `{"error": "неверные параметры для lead_target"}`
		}

		// Выполняем HTTP запрос к локальному API для вызова Meta
		url := fmt.Sprintf("http://localhost:8091/service/lead/target?rid=%d", params.RespId)
		logger.Info("ActionHandler: вызов Meta через API: %s", url)

		req, err := http.NewRequestWithContext(ctx, "POST", url, nil)
		if err != nil {
			logger.Error("ActionHandler: ошибка создания запроса к Meta API: %v", err)
			result, _ := json.Marshal(map[string]interface{}{
				"target": true,
				"error":  "failed to create request",
			})
			return string(result)
		}

		client := &http.Client{}
		resp, err := client.Do(req)
		if err != nil {
			logger.Error("ActionHandler: ошибка выполнения запроса к Meta API: %v", err)
			result, _ := json.Marshal(map[string]interface{}{
				"target": true,
				"error":  "failed to execute request",
			})
			return string(result)
		}
		defer resp.Body.Close()

		// Читаем ответ
		//body, err := io.ReadAll(resp.Body)
		//if err != nil {
		//	logger.Error("ActionHandler: ошибка чтения ответа Meta API: %v", err)
		//} else {
		//	logger.Debug("ActionHandler: ответ Meta API: %s", string(body))
		//}

		// Возвращаем подтверждение что цель достигнута
		result, _ := json.Marshal(map[string]bool{"target": true})
		return string(result)

	case "get_s3_files":
		//logger.Debug("ActionHandler: вызов функции get_s3_files с аргументами: %s", arguments)
		var params struct {
			UserID string `json:"user_id"`
		}

		if err := json.Unmarshal([]byte(arguments), &params); err != nil {
			return `{"error": "неверные параметры"}`
		}
		//logger.Debug("url %s", fmt.Sprintf("%s/gets3?id=%s", mode.RealHost, params.UserID))
		// Выполняем HTTP-запрос
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

		//logger.Debug("get_s3_files ответ сервера: %s", string(body))

		// Возвращаем результат
		result, _ := json.Marshal(map[string]string{"output": string(body)})
		return string(result)

	case "create_file":
		//logger.Debug("ActionHandler: вызов функции create_file с аргументами: %s", arguments)
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
		//logger.Debug("create_file ответ сервера", responseStr)

		// ИСПРАВЛЕНИЕ: Сервер возвращает URL с ошибкой форматирования %!d(string=23)
		// Заменяем на правильный user_id
		// Ищем паттерн %!d(string=NUMBER) и заменяем на NUMBER
		if strings.Contains(responseStr, "%!d(string=") {
			// Извлекаем user_id из строки %!d(string=23)
			start := strings.Index(responseStr, "%!d(string=")
			if start != -1 {
				end := strings.Index(responseStr[start:], ")")
				if end != -1 {
					// Заменяем весь паттерн на params.UserID
					badPattern := responseStr[start : start+end+1]
					responseStr = strings.ReplaceAll(responseStr, badPattern, params.UserID)
					//logger.Debug("create_file: исправлен битый URL, заменено '%s' на '%s'", badPattern, params.UserID)
				}
			}
		}

		return responseStr

	case "save_image_data":
		//logger.Debug("ActionHandler: вызов функции save_image_data")
		var params struct {
			UserID    string `json:"user_id"`    // ID пользователя для сохранения
			ImageData string `json:"image_data"` // base64-кодированное изображение
			FileName  string `json:"file_name"`
		}

		if err := json.Unmarshal([]byte(arguments), &params); err != nil {
			return `{"error": "неверные параметры для save_image_data"}`
		}

		// Декодируем base64
		imageData, err := base64.StdEncoding.DecodeString(params.ImageData)
		if err != nil {
			logger.Error("save_image_data: ошибка декодирования base64: %v", err)
			result, _ := json.Marshal(map[string]string{"error": "ошибка декодирования изображения"})
			return string(result)
		}

		//logger.Debug("save_image_data: декодировано %d байт изображения", len(imageData), params.UserID)
		//logger.Debug("save_image_data: передаём file_name='%s' на сервер", params.FileName)

		// Формируем multipart request для отправки на сервер
		var requestBody bytes.Buffer
		writer := multipart.NewWriter(&requestBody)

		// Добавляем user_id и имя файла
		writer.WriteField("user_id", params.UserID)
		writer.WriteField("file_name", params.FileName)

		// Добавляем изображение
		part, err := writer.CreateFormFile("image", params.FileName)
		if err != nil {
			logger.Error("save_image_data: ошибка создания form file: %v", err)
			result, _ := json.Marshal(map[string]string{"error": "ошибка подготовки данных"})
			return string(result)
		}

		if _, err := part.Write(imageData); err != nil {
			logger.Error("save_image_data: ошибка записи данных: %v", err)
			result, _ := json.Marshal(map[string]string{"error": "ошибка обработки изображения"})
			return string(result)
		}

		writer.Close()

		// Отправляем на сервер
		client := &http.Client{}
		saveReq, err := http.NewRequestWithContext(ctx, "POST", fmt.Sprintf("%s/saveImageInS3", mode.RealHost), &requestBody)
		if err != nil {
			result, _ := json.Marshal(map[string]string{"error": "ошибка создания запроса к серверу"})
			return string(result)
		}
		saveReq.Header.Set("Content-Type", writer.FormDataContentType())

		saveResp, err := client.Do(saveReq)
		if err != nil {
			if ctx.Err() != nil {
				result, _ := json.Marshal(map[string]string{"error": "запрос отменён по таймауту"})
				return string(result)
			}
			logger.Error("save_image_data: ошибка отправки на сервер: %v", err)
			result, _ := json.Marshal(map[string]string{"error": "ошибка сохранения изображения"})
			return string(result)
		}
		defer saveResp.Body.Close()

		// Читаем ответ
		saveBody, err := io.ReadAll(saveResp.Body)
		if err != nil {
			result, _ := json.Marshal(map[string]string{"error": "ошибка чтения ответа"})
			return string(result)
		}

		if saveResp.StatusCode != http.StatusOK {
			logger.Error("save_image_data: ошибка сервера (%d): %s", saveResp.StatusCode, string(saveBody))
			result, _ := json.Marshal(map[string]string{"error": "ошибка сохранения на сервере"})
			return string(result)
		}

		responseStr := strings.TrimSpace(string(saveBody))
		//logger.Debug("save_image_data: успешно сохранено: %s", params.UserID, responseStr)

		return responseStr

	default:
		result, _ := json.Marshal(map[string]string{"error": fmt.Sprintf("Функция %s не поддерживается", functionName)})
		return string(result)
	}
}

// GetTools возвращает инструменты в формате нужного провайдера
func (h *UniversalActionHandler) GetTools(provider create.ProviderType) interface{} {
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
	if provider == create.ProviderOpenAI {
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
	if provider == create.ProviderMistral {
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
