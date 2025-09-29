package handler

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/ikermy/AiR_Common/pkg/logger"
	"github.com/ikermy/AiR_Common/pkg/mode"
)

type ActionHandlerOpenAI struct{}

func (h *ActionHandlerOpenAI) RunAction(ctx context.Context, functionName, arguments string) string {
	switch functionName {

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
		//resp, err := http.NewRequestWithContext(
		//	fmt.Sprintf("%s/savefilein3", mode.RealHost),
		//	"application/json",
		//	strings.NewReader(string(jsonData)),
		//)
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
