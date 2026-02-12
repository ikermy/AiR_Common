package comdb

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/ikermy/AiR_Common/pkg/logger"
	"github.com/ikermy/AiR_Common/pkg/model/create"
	"golang.org/x/oauth2"
)

// GoogleOAuthToken представляет токен Google OAuth для модели
type GoogleOAuthToken struct {
	ID           uint32    `json:"id"`
	ModelID      uint32    `json:"model_id"`
	GoogleEmail  string    `json:"google_email"`
	AccessToken  string    `json:"access_token"`
	RefreshToken string    `json:"refresh_token"`
	TokenType    string    `json:"token_type"` // ENUM('Bearer') в БД, всегда "Bearer" для OAuth2
	Expiry       time.Time `json:"expiry"`
	Scopes       []string  `json:"scopes"`
	CreatedAt    time.Time `json:"created_at"`
	UpdatedAt    time.Time `json:"updated_at"`
}

// getModelIDByProvider получает model_id по userId и provider
func (d *DB) getModelIDByProvider(userId uint32, provider create.ProviderType) (uint32, error) {
	ctx, cancel := context.WithTimeout(d.Context(), sqlTimeToCancel*time.Second)
	defer cancel()

	query := `
		SELECT um.ModelId 
		FROM user_models um
		WHERE um.UserId = ? AND um.Provider = ?
		LIMIT 1
	`

	var modelID uint32
	err := d.Conn().QueryRowContext(ctx, query, userId, provider).Scan(&modelID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return 0, fmt.Errorf("модель провайдера %d не найдена для пользователя %d", provider, userId)
		}
		return 0, fmt.Errorf("ошибка получения model_id: %w", err)
	}

	return modelID, nil
}

// SaveGoogleTokenByProvider сохраняет или обновляет Google OAuth токен для модели пользователя по провайдеру
func (d *DB) SaveGoogleTokenByProvider(userId uint32, provider create.ProviderType, googleEmail string, token *oauth2.Token) error {
	if userId == 0 {
		return fmt.Errorf("получен некорректный userId")
	}
	if googleEmail == "" {
		return fmt.Errorf("получен пустой google_email")
	}
	if token == nil {
		return fmt.Errorf("получен пустой токен")
	}

	// Получаем model_id по userId и provider
	modelID, err := d.getModelIDByProvider(userId, provider)
	if err != nil {
		return err
	}

	ctx, cancel := context.WithTimeout(d.Context(), sqlTimeToCancel*time.Second)
	defer cancel()

	// Сериализуем scopes в JSON
	scopesJSON, err := json.Marshal(token.Extra("scope"))
	if err != nil {
		// Если не удалось получить scopes из Extra, создаем пустой массив
		scopesJSON = []byte("[]")
	}

	query := `
		INSERT INTO google_oauth_tokens 
			(model_id, google_email, access_token, refresh_token, token_type, expiry, scopes)
		VALUES (?, ?, ?, ?, ?, ?, ?)
		ON DUPLICATE KEY UPDATE
			access_token = VALUES(access_token),
			refresh_token = VALUES(refresh_token),
			token_type = VALUES(token_type),
			expiry = VALUES(expiry),
			scopes = VALUES(scopes),
			updated_at = CURRENT_TIMESTAMP
	`

	_, err = d.Conn().ExecContext(ctx, query,
		modelID,
		googleEmail,
		token.AccessToken,
		token.RefreshToken,
		token.TokenType,
		token.Expiry,
		scopesJSON,
	)

	if err != nil {
		switch {
		case errors.Is(err, context.DeadlineExceeded):
			return fmt.Errorf("тайм-аут (%d с) при сохранении Google токена: %w", sqlTimeToCancel, err)
		case errors.Is(err, context.Canceled):
			return fmt.Errorf("операция отменена: %w", err)
		default:
			return fmt.Errorf("ошибка сохранения Google токена: %w", err)
		}
	}

	return nil
}

// GetGoogleTokenByProvider получает Google OAuth токен для модели пользователя по провайдеру
func (d *DB) GetGoogleTokenByProvider(userId uint32, provider create.ProviderType) (*oauth2.Token, string, error) {
	if userId == 0 {
		return nil, "", fmt.Errorf("получен некорректный userId")
	}

	// Получаем model_id по userId и provider
	modelID, err := d.getModelIDByProvider(userId, provider)
	if err != nil {
		return nil, "", err
	}

	ctx, cancel := context.WithTimeout(d.Context(), sqlTimeToCancel*time.Second)
	defer cancel()

	query := `
		SELECT access_token, refresh_token, token_type, expiry, scopes, google_email
		FROM google_oauth_tokens
		WHERE model_id = ?
		LIMIT 1
	`

	var accessToken, refreshToken, tokenType, googleEmail string
	var expiry time.Time
	var scopesJSON sql.NullString

	err = d.Conn().QueryRowContext(ctx, query, modelID).Scan(
		&accessToken,
		&refreshToken,
		&tokenType,
		&expiry,
		&scopesJSON,
		&googleEmail,
	)

	if err != nil {
		switch {
		case errors.Is(err, context.DeadlineExceeded):
			return nil, "", fmt.Errorf("тайм-аут (%d с) при получении Google токена: %w", sqlTimeToCancel, err)
		case errors.Is(err, context.Canceled):
			return nil, "", fmt.Errorf("операция отменена: %w", err)
		case errors.Is(err, sql.ErrNoRows):
			return nil, "", nil // Токен не найден, но это не ошибка
		default:
			return nil, "", fmt.Errorf("ошибка получения Google токена: %w", err)
		}
	}

	token := &oauth2.Token{
		AccessToken:  accessToken,
		RefreshToken: refreshToken,
		TokenType:    tokenType,
		Expiry:       expiry,
	}

	// Парсим scopes если они есть
	if scopesJSON.Valid && scopesJSON.String != "" {
		var scopes []string
		if err := json.Unmarshal([]byte(scopesJSON.String), &scopes); err == nil {
			token = token.WithExtra(map[string]interface{}{"scope": scopes})
		}
	}

	return token, googleEmail, nil
}

// RefreshGoogleTokenIfNeededByProvider проверяет срок действия токена и обновляет его при необходимости
func (d *DB) RefreshGoogleTokenIfNeededByProvider(userId uint32, provider create.ProviderType, oauthConfig *oauth2.Config) error {
	if userId == 0 {
		return fmt.Errorf("получен некорректный userId")
	}
	if oauthConfig == nil {
		return fmt.Errorf("получен пустой oauth config")
	}

	// Получаем текущий токен
	token, googleEmail, err := d.GetGoogleTokenByProvider(userId, provider)
	if err != nil {
		return fmt.Errorf("ошибка получения токена для проверки: %w", err)
	}

	// Если токена нет, это не ошибка - просто не настроен
	if token == nil {
		//logger.Debug("Google OAuth токен не найден, provider=%d, пропускаем обновление", provider, userId)
		return nil
	}

	// Проверяем, истекает ли токен в ближайшие 5 минут
	if time.Until(token.Expiry) > 5*time.Minute {
		//logger.Debug("Google OAuth токен, provider=%d еще действителен (истекает через %v)", provider, time.Until(token.Expiry), userId)
		return nil
	}

	//logger.Debug("Google OAuth токен, provider=%d истекает скоро, выполняю обновление...", provider, userId)

	// Обновляем токен через OAuth2
	tokenSource := oauthConfig.TokenSource(context.Background(), token)
	newToken, err := tokenSource.Token()
	if err != nil {
		return fmt.Errorf("ошибка обновления Google токена: %w", err)
	}

	// Сохраняем обновленный токен
	if err := d.SaveGoogleTokenByProvider(userId, provider, googleEmail, newToken); err != nil {
		return fmt.Errorf("ошибка сохранения обновленного токена: %w", err)
	}

	//logger.Debug("Google OAuth токен успешно обновлен, provider=%d", provider, userId)
	return nil
}

// DeleteGoogleTokenByProvider удаляет Google OAuth токен для модели пользователя по провайдеру
func (d *DB) DeleteGoogleTokenByProvider(userId uint32, provider create.ProviderType) error {
	if userId == 0 {
		return fmt.Errorf("получен некорректный userId")
	}

	// Получаем model_id по userId и provider
	modelID, err := d.getModelIDByProvider(userId, provider)
	if err != nil {
		return err
	}

	ctx, cancel := context.WithTimeout(d.Context(), sqlTimeToCancel*time.Second)
	defer cancel()

	// ИЗМЕНЕНО: Полностью удаляем запись из БД вместо деактивации
	query := `
		DELETE FROM google_oauth_tokens
		WHERE model_id = ?
	`

	result, err := d.Conn().ExecContext(ctx, query, modelID)
	if err != nil {
		switch {
		case errors.Is(err, context.DeadlineExceeded):
			return fmt.Errorf("тайм-аут (%d с) при удалении Google токена: %w", sqlTimeToCancel, err)
		case errors.Is(err, context.Canceled):
			return fmt.Errorf("операция отменена: %w", err)
		default:
			return fmt.Errorf("ошибка удаления Google токена: %w", err)
		}
	}

	rowsAffected, _ := result.RowsAffected()
	if rowsAffected == 0 {
		logger.Debug("Google OAuth токен не найден, provider=%d", provider, userId)
		return nil
	}

	return nil
}
