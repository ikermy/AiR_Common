package google_services

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/ikermy/AiR_Common/pkg/comdb"
	"github.com/ikermy/AiR_Common/pkg/model/create"
	"golang.org/x/oauth2"
	"golang.org/x/oauth2/google"
	"google.golang.org/api/calendar/v3"
	"google.golang.org/api/option"
)

// CalendarService управляет операциями с Google Calendar
type CalendarService struct {
	db           comdb.Exterior
	ctx          context.Context
	provider     create.ProviderType
	clientID     string
	clientSecret string
	redirectURI  string
}

// NewCalendarService создает новый сервис Calendar с OAuth credentials
func NewCalendarService(ctx context.Context, db comdb.Exterior, provider create.ProviderType, clientID, clientSecret, redirectURI string) *CalendarService {
	return &CalendarService{
		db:           db,
		ctx:          ctx,
		provider:     provider,
		clientID:     clientID,
		clientSecret: clientSecret,
		redirectURI:  redirectURI,
	}
}

// getCalendarService создает Google Calendar сервис с OAuth токеном пользователя
// Автоматически обновляет токен если истёк срок действия
func (s *CalendarService) getCalendarService(userID uint32) (*calendar.Service, error) {
	// Получаем токен из БД
	token, googleEmail, err := s.db.GetGoogleTokenByProvider(userID, s.provider)
	if err != nil {
		return nil, fmt.Errorf("ошибка получения Google токена: %w", err)
	}

	if token == nil {
		return nil, fmt.Errorf("Google OAuth токен не найден. Пользователь должен авторизовать доступ к Calendar")
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
				"https://www.googleapis.com/auth/calendar.events",
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
				return nil, fmt.Errorf("Google OAuth токен истёк или был отозван. Пожалуйста, повторно авторизуйте доступ к Google Calendar через настройки профиля")
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

	// Создаем Calendar сервис
	calendarService, err := calendar.NewService(s.ctx, option.WithHTTPClient(client))
	if err != nil {
		return nil, fmt.Errorf("ошибка создания Calendar сервиса: %w", err)
	}

	return calendarService, nil
}

// CreateEventParams параметры для создания события
type CreateEventParams struct {
	UserID      uint32   `json:"user_id,string"` // Поддерживаем JSON как string, но храним как uint32
	Title       string   `json:"title"`
	Description string   `json:"description"`
	StartTime   string   `json:"start_time"` // RFC3339: "2026-02-04T10:00:00Z"
	EndTime     string   `json:"end_time"`   // RFC3339: "2026-02-04T11:00:00Z"
	Attendees   []string `json:"attendees"`  // Email адреса участников
	Location    string   `json:"location"`
}

// CreateEvent создает новое событие в календаре
func (s *CalendarService) CreateEvent(params CreateEventParams) (string, error) {
	calendarService, err := s.getCalendarService(params.UserID)
	if err != nil {
		result, _ := json.Marshal(map[string]string{"error": err.Error()})
		return string(result), nil
	}

	// Получаем часовой пояс пользователя из БД
	userTimeZone, err := s.db.UserTimeZone(params.UserID)
	if err != nil {
		// Если не удалось получить таймзону, используем UTC как fallback
		//logger.Warn("Не удалось получить таймзону пользователя %d: %v, используется UTC", params.userID, err)
		userTimeZone = "UTC"
	}

	// Парсим время
	startTime, err := time.Parse(time.RFC3339, params.StartTime)
	if err != nil {
		result, _ := json.Marshal(map[string]string{"error": fmt.Sprintf("неверный формат start_time (ожидается RFC3339): %v", err)})
		return string(result), nil
	}

	endTime, err := time.Parse(time.RFC3339, params.EndTime)
	if err != nil {
		result, _ := json.Marshal(map[string]string{"error": fmt.Sprintf("неверный формат end_time (ожидается RFC3339): %v", err)})
		return string(result), nil
	}

	// Создаем событие с таймзоной пользователя
	event := &calendar.Event{
		Summary:     params.Title,
		Description: params.Description,
		Location:    params.Location,
		Start: &calendar.EventDateTime{
			DateTime: startTime.Format(time.RFC3339),
			TimeZone: userTimeZone, // Используем таймзону пользователя
		},
		End: &calendar.EventDateTime{
			DateTime: endTime.Format(time.RFC3339),
			TimeZone: userTimeZone, // Используем таймзону пользователя
		},
	}

	// Добавляем участников
	if len(params.Attendees) > 0 {
		event.Attendees = make([]*calendar.EventAttendee, len(params.Attendees))
		for i, email := range params.Attendees {
			event.Attendees[i] = &calendar.EventAttendee{
				Email: email,
			}
		}
	}

	// Создаем событие в календаре "primary"
	createdEvent, err := calendarService.Events.Insert("primary", event).Do()
	if err != nil {
		result, _ := json.Marshal(map[string]string{"error": fmt.Sprintf("ошибка создания события в Calendar: %v", err)})
		return string(result), nil
	}

	//logger.Debug("Calendar: создано событие '%s' (ID: %s)", params.Title, createdEvent.Id, params.userID)

	// Возвращаем JSON с результатом
	result := map[string]interface{}{
		"success":  true,
		"event_id": createdEvent.Id,
		"title":    createdEvent.Summary,
		"link":     createdEvent.HtmlLink,
		"message":  fmt.Sprintf("Событие '%s' успешно создано", params.Title),
	}

	resultJSON, _ := json.Marshal(result)
	return string(resultJSON), nil
}

// ListEventsParams параметры для получения списка событий
type ListEventsParams struct {
	UserID     uint32 `json:"user_id,string"`
	TimeMin    string `json:"time_min"`    // RFC3339, опционально
	TimeMax    string `json:"time_max"`    // RFC3339, опционально
	MaxResults int64  `json:"max_results"` // По умолчанию 10
}

// ListEvents получает список событий из календаря
func (s *CalendarService) ListEvents(params ListEventsParams) (string, error) {
	calendarService, err := s.getCalendarService(params.UserID)
	if err != nil {
		result, _ := json.Marshal(map[string]string{"error": err.Error()})
		return string(result), nil
	}

	// Формируем запрос
	eventsCall := calendarService.Events.List("primary").
		SingleEvents(true).
		OrderBy("startTime")

	// Устанавливаем временные рамки
	if params.TimeMin != "" {
		eventsCall = eventsCall.TimeMin(params.TimeMin)
	} else {
		// По умолчанию - начиная с текущего момента
		eventsCall = eventsCall.TimeMin(time.Now().Format(time.RFC3339))
	}

	if params.TimeMax != "" {
		eventsCall = eventsCall.TimeMax(params.TimeMax)
	}

	// Устанавливаем лимит
	maxResults := params.MaxResults
	if maxResults <= 0 {
		maxResults = 10
	}
	eventsCall = eventsCall.MaxResults(maxResults)

	// Выполняем запрос
	events, err := eventsCall.Do()
	if err != nil {
		result, _ := json.Marshal(map[string]string{"error": fmt.Sprintf("ошибка получения событий из Calendar: %v", err)})
		return string(result), nil
	}

	// Формируем результат
	eventsList := make([]map[string]interface{}, 0, len(events.Items))
	for _, event := range events.Items {
		startTime := event.Start.DateTime
		if startTime == "" {
			startTime = event.Start.Date // Для событий на весь день
		}

		endTime := event.End.DateTime
		if endTime == "" {
			endTime = event.End.Date
		}

		eventsList = append(eventsList, map[string]interface{}{
			"id":          event.Id,
			"title":       event.Summary,
			"description": event.Description,
			"start_time":  startTime,
			"end_time":    endTime,
			"location":    event.Location,
			"link":        event.HtmlLink,
		})
	}

	result := map[string]interface{}{
		"success": true,
		"count":   len(eventsList),
		"events":  eventsList,
	}

	resultJSON, _ := json.Marshal(result)
	//logger.Debug("Calendar: получено %d событий", len(eventsList), params.userID)
	return string(resultJSON), nil
}

// DeleteEventParams параметры для удаления события
type DeleteEventParams struct {
	UserID  uint32 `json:"user_id,string"`
	EventID string `json:"event_id"`
}

// DeleteEvent удаляет событие из календаря
func (s *CalendarService) DeleteEvent(params DeleteEventParams) (string, error) {
	calendarService, err := s.getCalendarService(params.UserID)
	if err != nil {
		result, _ := json.Marshal(map[string]string{"error": err.Error()})
		return string(result), nil
	}

	// Удаляем событие
	err = calendarService.Events.Delete("primary", params.EventID).Do()
	if err != nil {
		result, _ := json.Marshal(map[string]string{"error": fmt.Sprintf("ошибка удаления события из Calendar: %v", err)})
		return string(result), nil
	}

	//logger.Debug("Calendar: удалено событие ID: %s", params.EventID, params.userID)

	result := map[string]interface{}{
		"success": true,
		"message": fmt.Sprintf("Событие %s успешно удалено", params.EventID),
	}

	resultJSON, _ := json.Marshal(result)
	return string(resultJSON), nil
}

// GetEventParams параметры для получения события
type GetEventParams struct {
	UserID  uint32 `json:"user_id,string"`
	EventID string `json:"event_id"`
}

// GetEvent получает детали события
func (s *CalendarService) GetEvent(params GetEventParams) (string, error) {
	calendarService, err := s.getCalendarService(params.UserID)
	if err != nil {
		result, _ := json.Marshal(map[string]string{"error": err.Error()})
		return string(result), nil
	}

	// Получаем событие
	event, err := calendarService.Events.Get("primary", params.EventID).Do()
	if err != nil {
		result, _ := json.Marshal(map[string]string{"error": fmt.Sprintf("ошибка получения события из Calendar: %v", err)})
		return string(result), nil
	}

	startTime := event.Start.DateTime
	if startTime == "" {
		startTime = event.Start.Date
	}

	endTime := event.End.DateTime
	if endTime == "" {
		endTime = event.End.Date
	}

	// Собираем участников
	attendees := make([]string, 0, len(event.Attendees))
	for _, attendee := range event.Attendees {
		attendees = append(attendees, attendee.Email)
	}

	result := map[string]interface{}{
		"success":     true,
		"id":          event.Id,
		"title":       event.Summary,
		"description": event.Description,
		"start_time":  startTime,
		"end_time":    endTime,
		"location":    event.Location,
		"attendees":   attendees,
		"link":        event.HtmlLink,
	}

	resultJSON, _ := json.Marshal(result)
	return string(resultJSON), nil
}
