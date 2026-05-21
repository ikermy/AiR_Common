package comdb

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"golang.org/x/oauth2"
)

// GoogleOAuthToken представляет токен Google OAuth для пользователя
type GoogleOAuthToken struct {
	ID           uint32    `json:"id"`
	UserID       uint32    `json:"user_id"`
	GoogleEmail  string    `json:"google_email"`
	AccessToken  string    `json:"access_token"`
	RefreshToken string    `json:"refresh_token"`
	TokenType    string    `json:"token_type"` // ENUM('Bearer') в БД, всегда "Bearer" для OAuth2
	Expiry       time.Time `json:"expiry"`
	Scopes       []string  `json:"scopes"`
	CreatedAt    time.Time `json:"created_at"`
	UpdatedAt    time.Time `json:"updated_at"`
}

// SaveGoogleToken сохраняет или обновляет Google OAuth токен пользователя
func (d *DB) SaveGoogleToken(userID uint32, googleEmail string, token *oauth2.Token) error {
	if userID == 0 {
		return fmt.Errorf("получен некорректный userID")
	}
	if googleEmail == "" {
		return fmt.Errorf("получен пустой google_email")
	}
	if token == nil {
		return fmt.Errorf("получен пустой токен")
	}

	ctx, cancel := context.WithTimeout(d.Context(), sqlTimeToCancel*time.Second)
	defer cancel()

	// Сериализуем scopes в JSON
	scopesJSON, err := json.Marshal(token.Extra("scope"))
	if err != nil {
		scopesJSON = []byte("[]")
	}

	query := `
		INSERT INTO google_oauth_tokens 
			(user_id, google_email, access_token, refresh_token, token_type, expiry, scopes)
		VALUES (?, ?, ?, ?, ?, ?, ?)
		ON DUPLICATE KEY UPDATE
			google_email  = VALUES(google_email),
			access_token  = VALUES(access_token),
			refresh_token = VALUES(refresh_token),
			token_type    = VALUES(token_type),
			expiry        = VALUES(expiry),
			scopes        = VALUES(scopes),
			updated_at    = CURRENT_TIMESTAMP
	`

	_, err = d.Conn().ExecContext(ctx, query,
		userID,
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

// GetGoogleToken получает Google OAuth токен пользователя
func (d *DB) GetGoogleToken(userID uint32) (*oauth2.Token, string, error) {
	if userID == 0 {
		return nil, "", fmt.Errorf("получен некорректный userID")
	}

	ctx, cancel := context.WithTimeout(d.Context(), sqlTimeToCancel*time.Second)
	defer cancel()

	query := `
		SELECT access_token, refresh_token, token_type, expiry, scopes, google_email
		FROM google_oauth_tokens
		WHERE user_id = ?
		LIMIT 1
	`

	var accessToken, refreshToken, tokenType, googleEmail string
	var expiry time.Time
	var scopesJSON sql.NullString

	err := d.Conn().QueryRowContext(ctx, query, userID).Scan(
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

// RefreshGoogleTokenIfNeeded проверяет срок действия токена и обновляет его при необходимости
func (d *DB) RefreshGoogleTokenIfNeeded(userID uint32, oauthConfig *oauth2.Config) error {
	if userID == 0 {
		return fmt.Errorf("получен некорректный userID")
	}
	if oauthConfig == nil {
		return fmt.Errorf("получен пустой oauth config")
	}

	// Получаем текущий токен
	token, googleEmail, err := d.GetGoogleToken(userID)
	if err != nil {
		return fmt.Errorf("ошибка получения токена для проверки: %w", err)
	}

	// Если токена нет, это не ошибка - просто не настроен
	if token == nil {
		return nil
	}

	// Проверяем, истекает ли токен в ближайшие 5 минут
	if time.Until(token.Expiry) > 5*time.Minute {
		return nil
	}

	// Обновляем токен через OAuth2
	tokenSource := oauthConfig.TokenSource(context.Background(), token)
	newToken, err := tokenSource.Token()
	if err != nil {
		return fmt.Errorf("ошибка обновления Google токена: %w", err)
	}

	// Сохраняем обновленный токен
	if err := d.SaveGoogleToken(userID, googleEmail, newToken); err != nil {
		return fmt.Errorf("ошибка сохранения обновленного токена: %w", err)
	}

	return nil
}

// DeleteGoogleToken удаляет Google OAuth токен пользователя
func (d *DB) DeleteGoogleToken(userID uint32) error {
	if userID == 0 {
		return fmt.Errorf("получен некорректный userID")
	}

	ctx, cancel := context.WithTimeout(d.Context(), sqlTimeToCancel*time.Second)
	defer cancel()

	query := `
		DELETE FROM google_oauth_tokens
		WHERE user_id = ?
	`

	_, err := d.Conn().ExecContext(ctx, query, userID)
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

	return nil
}
