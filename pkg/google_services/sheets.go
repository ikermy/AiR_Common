package google_services

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/ikermy/AiR_Common/pkg/comdb"
	"github.com/ikermy/AiR_Common/pkg/model/create"
	"golang.org/x/oauth2"
	"golang.org/x/oauth2/google"
	"google.golang.org/api/option"
	"google.golang.org/api/sheets/v4"
)

// contains проверяет наличие подстроки (case-insensitive)
func contains(s, substr string) bool {
	return strings.Contains(strings.ToLower(s), strings.ToLower(substr))
}

// SheetsService управляет операциями с Google Sheets
type SheetsService struct {
	db           comdb.Exterior
	ctx          context.Context
	provider     create.ProviderType
	clientID     string
	clientSecret string
	redirectURI  string
}

// NewSheetsService создает новый сервис Sheets с OAuth credentials
func NewSheetsService(ctx context.Context, db comdb.Exterior, provider create.ProviderType, clientID, clientSecret, redirectURI string) *SheetsService {
	return &SheetsService{
		db:           db,
		ctx:          ctx,
		provider:     provider,
		clientID:     clientID,
		clientSecret: clientSecret,
		redirectURI:  redirectURI,
	}
}

// getSheetsService создает Google Sheets сервис с OAuth токеном пользователя
// Автоматически обновляет токен если истёк срок действия
func (s *SheetsService) getSheetsService(userID uint32) (*sheets.Service, error) {
	// Получаем токен из БД
	token, googleEmail, err := s.db.GetGoogleTokenByProvider(userID, s.provider)
	if err != nil {
		return nil, fmt.Errorf("ошибка получения Google токена: %w", err)
	}

	if token == nil {
		return nil, fmt.Errorf("Google OAuth токен не найден. Пользователь должен авторизовать доступ к Sheets")
	}

	// Создаём OAuth2 Config для автоматического обновления токена
	var tokenSource oauth2.TokenSource

	if s.clientID != "" && s.clientSecret != "" {
		// Есть OAuth credentials - используем TokenSource с автообновлением
		oauthConfig := &oauth2.Config{
			ClientID:     s.clientID,
			ClientSecret: s.clientSecret,
			RedirectURL:  s.redirectURI,
			Endpoint:     google.Endpoint,
			Scopes: []string{
				"https://www.googleapis.com/auth/spreadsheets",
			},
		}

		// TokenSource автоматически обновит токен при необходимости
		tokenSource = oauthConfig.TokenSource(s.ctx, token)

		// Получаем актуальный токен (может быть обновлён)
		freshToken, err := tokenSource.Token()
		if err != nil {
			// Проверяем на invalid_grant - токен отозван или истек
			errMsg := err.Error()
			if contains(errMsg, "invalid_grant") || contains(errMsg, "Token has been expired or revoked") {
				return nil, fmt.Errorf("Google OAuth токен истёк или был отозван. Пожалуйста, повторно авторизуйте доступ к Google Sheets через настройки профиля")
			}
			return nil, fmt.Errorf("ошибка получения/обновления токена: %w", err)
		}

		// Если токен был обновлён - сохраняем в БД
		if freshToken.AccessToken != token.AccessToken {
			err = s.db.SaveGoogleTokenByProvider(userID, s.provider, googleEmail, freshToken)
			if err != nil {
				return nil, fmt.Errorf("не удалось сохранить обновлённый токен: %v", err)
			}
		}
	} else {
		// Нет OAuth credentials - используем статичный токен (без автообновления)
		tokenSource = oauth2.StaticTokenSource(token)
		//logger.Warn("OAuth credentials не настроены - токен не будет автоматически обновляться")
	}

	// Создаем HTTP клиент с OAuth токеном
	client := oauth2.NewClient(s.ctx, tokenSource)

	// Создаем Sheets сервис
	sheetsService, err := sheets.NewService(s.ctx, option.WithHTTPClient(client))
	if err != nil {
		return nil, fmt.Errorf("ошибка создания Sheets сервиса: %w", err)
	}

	return sheetsService, nil
}

// ReadRangeParams параметры для чтения диапазона ячеек
type ReadRangeParams struct {
	UserID        uint32 `json:"user_id,string"`
	SpreadsheetID string `json:"spreadsheet_id"` // ID таблицы из URL
	Range         string `json:"range"`          // Например: "Sheet1!A1:D10"
}

// ReadRange читает данные из указанного диапазона
func (s *SheetsService) ReadRange(params ReadRangeParams) (string, error) {
	sheetsService, err := s.getSheetsService(params.UserID)
	if err != nil {
		result, _ := json.Marshal(map[string]string{"error": err.Error()})
		return string(result), nil
	}

	// Читаем данные
	resp, err := sheetsService.Spreadsheets.Values.Get(params.SpreadsheetID, params.Range).Do()
	if err != nil {
		result, _ := json.Marshal(map[string]string{"error": fmt.Sprintf("ошибка чтения данных из Sheets: %v", err)})
		return string(result), nil
	}

	// Формируем результат
	result := map[string]interface{}{
		"success": true,
		"range":   resp.Range,
		"values":  resp.Values,
		"rows":    len(resp.Values),
	}

	if len(resp.Values) > 0 {
		result["columns"] = len(resp.Values[0])
	}

	resultJSON, _ := json.Marshal(result)
	//logger.Debug("Sheets: прочитано %d строк из %s", len(resp.Values), params.Range, params.userID)
	return string(resultJSON), nil
}

// WriteRangeParams параметры для записи данных
type WriteRangeParams struct {
	UserID        uint32          `json:"user_id,string"`
	SpreadsheetID string          `json:"spreadsheet_id"`
	Range         string          `json:"range"`  // Например: "Sheet1!A1"
	Values        [][]interface{} `json:"values"` // Двумерный массив значений
}

// WriteRange записывает данные в указанный диапазон
func (s *SheetsService) WriteRange(params WriteRangeParams) (string, error) {
	sheetsService, err := s.getSheetsService(params.UserID)
	if err != nil {
		result, _ := json.Marshal(map[string]string{"error": err.Error()})
		return string(result), nil
	}

	// Формируем запрос на запись
	valueRange := &sheets.ValueRange{
		Values: params.Values,
	}

	// Записываем данные
	resp, err := sheetsService.Spreadsheets.Values.Update(
		params.SpreadsheetID,
		params.Range,
		valueRange,
	).ValueInputOption("USER_ENTERED").Do()

	if err != nil {
		result, _ := json.Marshal(map[string]string{"error": fmt.Sprintf("ошибка записи данных в Sheets: %v", err)})
		return string(result), nil
	}

	// Формируем результат
	result := map[string]interface{}{
		"success":       true,
		"updated_range": resp.UpdatedRange,
		"updated_rows":  resp.UpdatedRows,
		"updated_cells": resp.UpdatedCells,
		"message":       fmt.Sprintf("Записано %d строк, %d ячеек", resp.UpdatedRows, resp.UpdatedCells),
	}

	resultJSON, _ := json.Marshal(result)
	//logger.Debug("Sheets: записано %d ячеек в %s", resp.UpdatedCells, params.Range, params.userID)
	return string(resultJSON), nil
}

// AppendRangeParams параметры для добавления данных в конец таблицы
type AppendRangeParams struct {
	UserID        uint32          `json:"user_id,string"`
	SpreadsheetID string          `json:"spreadsheet_id"`
	Range         string          `json:"range"` // Например: "Sheet1!A:D"
	Values        [][]interface{} `json:"values"`
}

// AppendRange добавляет данные в конец таблицы
func (s *SheetsService) AppendRange(params AppendRangeParams) (string, error) {
	sheetsService, err := s.getSheetsService(params.UserID)
	if err != nil {
		result, _ := json.Marshal(map[string]string{"error": err.Error()})
		return string(result), nil
	}

	// Формируем запрос на добавление
	valueRange := &sheets.ValueRange{
		Values: params.Values,
	}

	// Добавляем данные
	resp, err := sheetsService.Spreadsheets.Values.Append(
		params.SpreadsheetID,
		params.Range,
		valueRange,
	).ValueInputOption("USER_ENTERED").Do()

	if err != nil {
		result, _ := json.Marshal(map[string]string{"error": fmt.Sprintf("ошибка добавления данных в Sheets: %v", err)})
		return string(result), nil
	}

	// Формируем результат
	result := map[string]interface{}{
		"success":       true,
		"updated_range": resp.Updates.UpdatedRange,
		"updated_rows":  resp.Updates.UpdatedRows,
		"updated_cells": resp.Updates.UpdatedCells,
		"message":       fmt.Sprintf("Добавлено %d строк", resp.Updates.UpdatedRows),
	}

	resultJSON, _ := json.Marshal(result)
	//logger.Debug("Sheets: добавлено %d строк в %s", resp.Updates.UpdatedRows, params.Range, params.userID)
	return string(resultJSON), nil
}

// CreateSpreadsheetParams параметры для создания новой таблицы
type CreateSpreadsheetParams struct {
	UserID     uint32   `json:"user_id,string"`
	Title      string   `json:"title"`
	SheetNames []string `json:"sheet_names"` // Названия листов, опционально
}

// CreateSpreadsheet создает новую Google Sheets таблицу
func (s *SheetsService) CreateSpreadsheet(params CreateSpreadsheetParams) (string, error) {
	sheetsService, err := s.getSheetsService(params.UserID)
	if err != nil {
		result, _ := json.Marshal(map[string]string{"error": err.Error()})
		return string(result), nil
	}

	// Создаем структуру таблицы
	spreadsheet := &sheets.Spreadsheet{
		Properties: &sheets.SpreadsheetProperties{
			Title: params.Title,
		},
	}

	// Добавляем листы если указаны
	if len(params.SheetNames) > 0 {
		spreadsheet.Sheets = make([]*sheets.Sheet, len(params.SheetNames))
		for i, name := range params.SheetNames {
			spreadsheet.Sheets[i] = &sheets.Sheet{
				Properties: &sheets.SheetProperties{
					Title: name,
				},
			}
		}
	}

	// Создаем таблицу
	resp, err := sheetsService.Spreadsheets.Create(spreadsheet).Do()
	if err != nil {
		result, _ := json.Marshal(map[string]string{"error": fmt.Sprintf("ошибка создания таблицы Sheets: %v", err)})
		return string(result), nil
	}

	// Формируем результат
	result := map[string]interface{}{
		"success":        true,
		"spreadsheet_id": resp.SpreadsheetId,
		"title":          resp.Properties.Title,
		"url":            resp.SpreadsheetUrl,
		"message":        fmt.Sprintf("Таблица '%s' успешно создана", params.Title),
	}

	resultJSON, _ := json.Marshal(result)
	//logger.Debug("Sheets: создана таблица '%s' (ID: %s)", params.Title, resp.SpreadsheetId, params.userID)
	return string(resultJSON), nil
}

// GetSpreadsheetInfoParams параметры для получения информации о таблице
type GetSpreadsheetInfoParams struct {
	UserID        uint32 `json:"user_id,string"`
	SpreadsheetID string `json:"spreadsheet_id"`
}

// GetSpreadsheetInfo получает информацию о таблице (название, листы и т.д.)
func (s *SheetsService) GetSpreadsheetInfo(params GetSpreadsheetInfoParams) (string, error) {
	sheetsService, err := s.getSheetsService(params.UserID)
	if err != nil {
		result, _ := json.Marshal(map[string]string{"error": err.Error()})
		return string(result), nil
	}

	// Получаем информацию о таблице
	resp, err := sheetsService.Spreadsheets.Get(params.SpreadsheetID).Do()
	if err != nil {
		result, _ := json.Marshal(map[string]string{"error": fmt.Sprintf("ошибка получения информации о таблице: %v", err)})
		return string(result), nil
	}

	// Собираем информацию о листах
	sheetsList := make([]map[string]interface{}, len(resp.Sheets))
	for i, sheet := range resp.Sheets {
		sheetsList[i] = map[string]interface{}{
			"title":     sheet.Properties.Title,
			"sheet_id":  sheet.Properties.SheetId,
			"index":     sheet.Properties.Index,
			"row_count": sheet.Properties.GridProperties.RowCount,
			"col_count": sheet.Properties.GridProperties.ColumnCount,
		}
	}

	// Формируем результат
	result := map[string]interface{}{
		"success":        true,
		"spreadsheet_id": resp.SpreadsheetId,
		"title":          resp.Properties.Title,
		"url":            resp.SpreadsheetUrl,
		"sheets":         sheetsList,
		"sheets_count":   len(sheetsList),
	}

	resultJSON, _ := json.Marshal(result)
	return string(resultJSON), nil
}
