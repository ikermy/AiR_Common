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

	"github.com/ikermy/AiR_Common/pkg/comdb"
	"github.com/ikermy/AiR_Common/pkg/conf"
	"github.com/ikermy/AiR_Common/pkg/google_services"
	"github.com/ikermy/AiR_Common/pkg/logger"
	"github.com/ikermy/AiR_Common/pkg/mode"
	"github.com/ikermy/AiR_Common/pkg/model/create"

	"github.com/sashabaranov/go-openai"
)

// UniversalActionHandler универсальный обработчик функций для всех провайдеров
type UniversalActionHandler struct {
	db          comdb.Exterior
	ctx         context.Context
	provider    create.ProviderType
	googleOAuth *conf.GOAuth // OAuth конфиг для Google API
}

// NewUniversalActionHandler создаёт новый action handler с доступом к БД
func NewUniversalActionHandler(ctx context.Context, db comdb.Exterior, provider create.ProviderType, googleOAuth *conf.GOAuth) *UniversalActionHandler {
	return &UniversalActionHandler{
		db:          db,
		ctx:         ctx,
		provider:    provider,
		googleOAuth: googleOAuth,
	}
}

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

	// ============================================================================
	// GOOGLE CALENDAR FUNCTIONS
	// ============================================================================
	case "calendar_create_event":
		var rawParams struct {
			UserID      string   `json:"user_id"`
			Title       string   `json:"title"`
			Description string   `json:"description"`
			StartTime   string   `json:"start_time"`
			EndTime     string   `json:"end_time"`
			Location    string   `json:"location"`
			Attendees   []string `json:"attendees"`
		}
		if err := json.Unmarshal([]byte(arguments), &rawParams); err != nil {
			return `{"error": "неверные параметры для calendar_create_event"}`
		}

		var userID uint32
		fmt.Sscanf(rawParams.UserID, "%d", &userID)

		params := google_services.CreateEventParams{
			UserID:      userID,
			Title:       rawParams.Title,
			Description: rawParams.Description,
			StartTime:   rawParams.StartTime,
			EndTime:     rawParams.EndTime,
			Location:    rawParams.Location,
			Attendees:   rawParams.Attendees,
		}

		// Создаём CalendarService с OAuth credentials
		var clientID, clientSecret, redirectURI string
		if h.googleOAuth != nil {
			clientID = h.googleOAuth.ClientID
			clientSecret = h.googleOAuth.ClientSecret
			redirectURI = h.googleOAuth.RedirectURI
		}
		calendarService := google_services.NewCalendarService(h.ctx, h.db, h.provider, clientID, clientSecret, redirectURI)
		result, err := calendarService.CreateEvent(params)
		if err != nil {
			return fmt.Sprintf(`{"error": "%s"}`, err.Error())
		}
		return result

	case "calendar_list_events":
		var rawParams struct {
			UserID     string `json:"user_id"`
			TimeMin    string `json:"time_min"`
			TimeMax    string `json:"time_max"`
			MaxResults int64  `json:"max_results"`
		}
		if err := json.Unmarshal([]byte(arguments), &rawParams); err != nil {
			return `{"error": "неверные параметры для calendar_list_events"}`
		}

		var userID uint32
		fmt.Sscanf(rawParams.UserID, "%d", &userID)

		params := google_services.ListEventsParams{
			UserID:     userID,
			TimeMin:    rawParams.TimeMin,
			TimeMax:    rawParams.TimeMax,
			MaxResults: rawParams.MaxResults,
		}

		// Создаём CalendarService с OAuth credentials
		var clientID, clientSecret, redirectURI string
		if h.googleOAuth != nil {
			clientID = h.googleOAuth.ClientID
			clientSecret = h.googleOAuth.ClientSecret
			redirectURI = h.googleOAuth.RedirectURI
		}
		calendarService := google_services.NewCalendarService(h.ctx, h.db, h.provider, clientID, clientSecret, redirectURI)
		result, err := calendarService.ListEvents(params)
		if err != nil {
			return fmt.Sprintf(`{"error": "%s"}`, err.Error())
		}
		return result

	case "calendar_delete_event":
		var rawParams struct {
			UserID  string `json:"user_id"`
			EventID string `json:"event_id"`
		}
		if err := json.Unmarshal([]byte(arguments), &rawParams); err != nil {
			return `{"error": "неверные параметры для calendar_delete_event"}`
		}

		var userID uint32
		fmt.Sscanf(rawParams.UserID, "%d", &userID)

		params := google_services.DeleteEventParams{
			UserID:  userID,
			EventID: rawParams.EventID,
		}

		// Создаём CalendarService с OAuth credentials
		var clientID, clientSecret, redirectURI string
		if h.googleOAuth != nil {
			clientID = h.googleOAuth.ClientID
			clientSecret = h.googleOAuth.ClientSecret
			redirectURI = h.googleOAuth.RedirectURI
		}
		calendarService := google_services.NewCalendarService(h.ctx, h.db, h.provider, clientID, clientSecret, redirectURI)
		result, err := calendarService.DeleteEvent(params)
		if err != nil {
			return fmt.Sprintf(`{"error": "%s"}`, err.Error())
		}
		return result

	case "calendar_get_event":
		var rawParams struct {
			UserID  string `json:"user_id"`
			EventID string `json:"event_id"`
		}
		if err := json.Unmarshal([]byte(arguments), &rawParams); err != nil {
			return `{"error": "неверные параметры для calendar_get_event"}`
		}

		var userID uint32
		fmt.Sscanf(rawParams.UserID, "%d", &userID)

		params := google_services.GetEventParams{
			UserID:  userID,
			EventID: rawParams.EventID,
		}

		// Создаём CalendarService с OAuth credentials
		var clientID, clientSecret, redirectURI string
		if h.googleOAuth != nil {
			clientID = h.googleOAuth.ClientID
			clientSecret = h.googleOAuth.ClientSecret
			redirectURI = h.googleOAuth.RedirectURI
		}
		calendarService := google_services.NewCalendarService(h.ctx, h.db, h.provider, clientID, clientSecret, redirectURI)
		result, err := calendarService.GetEvent(params)
		if err != nil {
			return fmt.Sprintf(`{"error": "%s"}`, err.Error())
		}
		return result

	// ============================================================================
	// GOOGLE SHEETS FUNCTIONS
	// ============================================================================
	case "sheets_read_range":
		var rawParams struct {
			UserID        string `json:"user_id"`
			SpreadsheetID string `json:"spreadsheet_id"`
			Range         string `json:"range"`
		}
		if err := json.Unmarshal([]byte(arguments), &rawParams); err != nil {
			return `{"error": "неверные параметры для sheets_read_range"}`
		}

		var userID uint32
		fmt.Sscanf(rawParams.UserID, "%d", &userID)

		params := google_services.ReadRangeParams{
			UserID:        userID,
			SpreadsheetID: rawParams.SpreadsheetID,
			Range:         rawParams.Range,
		}

		// Создаём SheetsService с OAuth credentials
		var clientID, clientSecret, redirectURI string
		if h.googleOAuth != nil {
			clientID = h.googleOAuth.ClientID
			clientSecret = h.googleOAuth.ClientSecret
			redirectURI = h.googleOAuth.RedirectURI
		}
		sheetsService := google_services.NewSheetsService(h.ctx, h.db, h.provider, clientID, clientSecret, redirectURI)
		result, err := sheetsService.ReadRange(params)
		if err != nil {
			return fmt.Sprintf(`{"error": "%s"}`, err.Error())
		}
		return result

	case "sheets_write_range":
		var rawParams struct {
			UserID        string          `json:"user_id"`
			SpreadsheetID string          `json:"spreadsheet_id"`
			Range         string          `json:"range"`
			Values        [][]interface{} `json:"values"`
		}
		if err := json.Unmarshal([]byte(arguments), &rawParams); err != nil {
			return `{"error": "неверные параметры для sheets_write_range"}`
		}

		var userID uint32
		fmt.Sscanf(rawParams.UserID, "%d", &userID)

		params := google_services.WriteRangeParams{
			UserID:        userID,
			SpreadsheetID: rawParams.SpreadsheetID,
			Range:         rawParams.Range,
			Values:        rawParams.Values,
		}

		// Создаём SheetsService с OAuth credentials
		var clientID, clientSecret, redirectURI string
		if h.googleOAuth != nil {
			clientID = h.googleOAuth.ClientID
			clientSecret = h.googleOAuth.ClientSecret
			redirectURI = h.googleOAuth.RedirectURI
		}
		sheetsService := google_services.NewSheetsService(h.ctx, h.db, h.provider, clientID, clientSecret, redirectURI)
		result, err := sheetsService.WriteRange(params)
		if err != nil {
			return fmt.Sprintf(`{"error": "%s"}`, err.Error())
		}
		return result

	case "sheets_append_range":
		var rawParams struct {
			UserID        string          `json:"user_id"`
			SpreadsheetID string          `json:"spreadsheet_id"`
			Range         string          `json:"range"`
			Values        [][]interface{} `json:"values"`
		}
		if err := json.Unmarshal([]byte(arguments), &rawParams); err != nil {
			return `{"error": "неверные параметры для sheets_append_range"}`
		}

		var userID uint32
		fmt.Sscanf(rawParams.UserID, "%d", &userID)

		params := google_services.AppendRangeParams{
			UserID:        userID,
			SpreadsheetID: rawParams.SpreadsheetID,
			Range:         rawParams.Range,
			Values:        rawParams.Values,
		}

		// Создаём SheetsService с OAuth credentials
		var clientID, clientSecret, redirectURI string
		if h.googleOAuth != nil {
			clientID = h.googleOAuth.ClientID
			clientSecret = h.googleOAuth.ClientSecret
			redirectURI = h.googleOAuth.RedirectURI
		}
		sheetsService := google_services.NewSheetsService(h.ctx, h.db, h.provider, clientID, clientSecret, redirectURI)
		result, err := sheetsService.AppendRange(params)
		if err != nil {
			return fmt.Sprintf(`{"error": "%s"}`, err.Error())
		}
		return result

	case "sheets_create_spreadsheet":
		var rawParams struct {
			UserID     string   `json:"user_id"`
			Title      string   `json:"title"`
			SheetNames []string `json:"sheet_names"`
		}
		if err := json.Unmarshal([]byte(arguments), &rawParams); err != nil {
			return `{"error": "неверные параметры для sheets_create_spreadsheet"}`
		}

		var userID uint32
		fmt.Sscanf(rawParams.UserID, "%d", &userID)

		params := google_services.CreateSpreadsheetParams{
			UserID:     userID,
			Title:      rawParams.Title,
			SheetNames: rawParams.SheetNames,
		}

		// Создаём SheetsService с OAuth credentials
		var clientID, clientSecret, redirectURI string
		if h.googleOAuth != nil {
			clientID = h.googleOAuth.ClientID
			clientSecret = h.googleOAuth.ClientSecret
			redirectURI = h.googleOAuth.RedirectURI
		}
		sheetsService := google_services.NewSheetsService(h.ctx, h.db, h.provider, clientID, clientSecret, redirectURI)
		result, err := sheetsService.CreateSpreadsheet(params)
		if err != nil {
			return fmt.Sprintf(`{"error": "%s"}`, err.Error())
		}
		return result

	case "sheets_get_info":
		var rawParams struct {
			UserID        string `json:"user_id"`
			SpreadsheetID string `json:"spreadsheet_id"`
		}
		if err := json.Unmarshal([]byte(arguments), &rawParams); err != nil {
			return `{"error": "неверные параметры для sheets_get_info"}`
		}

		var userID uint32
		fmt.Sscanf(rawParams.UserID, "%d", &userID)

		params := google_services.GetSpreadsheetInfoParams{
			UserID:        userID,
			SpreadsheetID: rawParams.SpreadsheetID,
		}

		// Создаём SheetsService с OAuth credentials
		var clientID, clientSecret, redirectURI string
		if h.googleOAuth != nil {
			clientID = h.googleOAuth.ClientID
			clientSecret = h.googleOAuth.ClientSecret
			redirectURI = h.googleOAuth.RedirectURI
		}
		sheetsService := google_services.NewSheetsService(h.ctx, h.db, h.provider, clientID, clientSecret, redirectURI)
		result, err := sheetsService.GetSpreadsheetInfo(params)
		if err != nil {
			return fmt.Sprintf(`{"error": "%s"}`, err.Error())
		}
		return result

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

	// Добавляем Google Calendar функции если Google OAuth настроен
	if h.googleOAuth != nil && h.googleOAuth.ClientID != "" {
		calendarFunctions := []map[string]interface{}{
			{
				"name":        "calendar_create_event",
				"description": "Создает событие в Google Calendar пользователя",
				"parameters": map[string]interface{}{
					"type": "object",
					"properties": map[string]interface{}{
						"user_id": map[string]interface{}{
							"type":        "string",
							"description": "ID пользователя",
						},
						"title": map[string]interface{}{
							"type":        "string",
							"description": "Название события",
						},
						"description": map[string]interface{}{
							"type":        "string",
							"description": "Описание события",
						},
						"start_time": map[string]interface{}{
							"type":        "string",
							"description": "Время начала в RFC3339 формате (например: 2026-02-05T15:00:00+03:00)",
						},
						"end_time": map[string]interface{}{
							"type":        "string",
							"description": "Время окончания в RFC3339 формате",
						},
						"location": map[string]interface{}{
							"type":        "string",
							"description": "Место проведения события",
						},
						"attendees": map[string]interface{}{
							"type":        "array",
							"description": "Email адреса участников",
							"items": map[string]interface{}{
								"type": "string",
							},
						},
					},
					"required": []string{"user_id", "title", "start_time", "end_time"},
				},
			},
			{
				"name":        "calendar_list_events",
				"description": "Получает список событий из Google Calendar",
				"parameters": map[string]interface{}{
					"type": "object",
					"properties": map[string]interface{}{
						"user_id": map[string]interface{}{
							"type":        "string",
							"description": "ID пользователя",
						},
						"time_min": map[string]interface{}{
							"type":        "string",
							"description": "Начало периода в RFC3339 формате (опционально)",
						},
						"time_max": map[string]interface{}{
							"type":        "string",
							"description": "Конец периода в RFC3339 формате (опционально)",
						},
						"max_results": map[string]interface{}{
							"type":        "integer",
							"description": "Максимальное количество событий (по умолчанию 10)",
						},
					},
					"required": []string{"user_id"},
				},
			},
			{
				"name":        "calendar_delete_event",
				"description": "Удаляет событие из Google Calendar",
				"parameters": map[string]interface{}{
					"type": "object",
					"properties": map[string]interface{}{
						"user_id": map[string]interface{}{
							"type":        "string",
							"description": "ID пользователя",
						},
						"event_id": map[string]interface{}{
							"type":        "string",
							"description": "ID события для удаления",
						},
					},
					"required": []string{"user_id", "event_id"},
				},
			},
			{
				"name":        "calendar_get_event",
				"description": "Получает детали события из Google Calendar",
				"parameters": map[string]interface{}{
					"type": "object",
					"properties": map[string]interface{}{
						"user_id": map[string]interface{}{
							"type":        "string",
							"description": "ID пользователя",
						},
						"event_id": map[string]interface{}{
							"type":        "string",
							"description": "ID события",
						},
					},
					"required": []string{"user_id", "event_id"},
				},
			},
		}
		functions = append(functions, calendarFunctions...)

		// Добавляем Google Sheets функции
		sheetsFunctions := []map[string]interface{}{
			{
				"name":        "sheets_read_range",
				"description": "Читает данные из Google Sheets таблицы",
				"parameters": map[string]interface{}{
					"type": "object",
					"properties": map[string]interface{}{
						"user_id": map[string]interface{}{
							"type":        "string",
							"description": "ID пользователя",
						},
						"spreadsheet_id": map[string]interface{}{
							"type":        "string",
							"description": "ID таблицы из URL (между /d/ и /edit)",
						},
						"range": map[string]interface{}{
							"type":        "string",
							"description": "Диапазон ячеек (например: Лист1!A1:D10)",
						},
					},
					"required": []string{"user_id", "spreadsheet_id", "range"},
				},
			},
			{
				"name":        "sheets_write_range",
				"description": "Записывает данные в Google Sheets таблицу",
				"parameters": map[string]interface{}{
					"type": "object",
					"properties": map[string]interface{}{
						"user_id": map[string]interface{}{
							"type":        "string",
							"description": "ID пользователя",
						},
						"spreadsheet_id": map[string]interface{}{
							"type":        "string",
							"description": "ID таблицы",
						},
						"range": map[string]interface{}{
							"type":        "string",
							"description": "Диапазон для записи (например: Лист1!A1)",
						},
						"values": map[string]interface{}{
							"type":        "array",
							"description": "Двумерный массив данных [[row1], [row2]]",
							"items": map[string]interface{}{
								"type": "array",
								"items": map[string]interface{}{
									"type": "string",
								},
							},
						},
					},
					"required": []string{"user_id", "spreadsheet_id", "range", "values"},
				},
			},
			{
				"name":        "sheets_append_range",
				"description": "Добавляет данные в конец Google Sheets таблицы",
				"parameters": map[string]interface{}{
					"type": "object",
					"properties": map[string]interface{}{
						"user_id": map[string]interface{}{
							"type":        "string",
							"description": "ID пользователя",
						},
						"spreadsheet_id": map[string]interface{}{
							"type":        "string",
							"description": "ID таблицы",
						},
						"range": map[string]interface{}{
							"type":        "string",
							"description": "Диапазон для добавления (например: Лист1!A:D)",
						},
						"values": map[string]interface{}{
							"type":        "array",
							"description": "Двумерный массив данных для добавления",
							"items": map[string]interface{}{
								"type": "array",
								"items": map[string]interface{}{
									"type": "string",
								},
							},
						},
					},
					"required": []string{"user_id", "spreadsheet_id", "range", "values"},
				},
			},
			{
				"name":        "sheets_create_spreadsheet",
				"description": "Создает новую Google Sheets таблицу",
				"parameters": map[string]interface{}{
					"type": "object",
					"properties": map[string]interface{}{
						"user_id": map[string]interface{}{
							"type":        "string",
							"description": "ID пользователя",
						},
						"title": map[string]interface{}{
							"type":        "string",
							"description": "Название новой таблицы",
						},
						"sheet_names": map[string]interface{}{
							"type":        "array",
							"description": "Названия листов (опционально)",
							"items": map[string]interface{}{
								"type": "string",
							},
						},
					},
					"required": []string{"user_id", "title"},
				},
			},
			{
				"name":        "sheets_get_info",
				"description": "Получает информацию о Google Sheets таблице (листы, размеры)",
				"parameters": map[string]interface{}{
					"type": "object",
					"properties": map[string]interface{}{
						"user_id": map[string]interface{}{
							"type":        "string",
							"description": "ID пользователя",
						},
						"spreadsheet_id": map[string]interface{}{
							"type":        "string",
							"description": "ID таблицы",
						},
					},
					"required": []string{"user_id", "spreadsheet_id"},
				},
			},
		}
		functions = append(functions, sheetsFunctions...)
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
