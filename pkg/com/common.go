package com

import (
	"encoding/json"
	"fmt"
	"time"
)

// InstMsg - структура для передачи мгновенных сообщений в панель управления
type InstMsg struct {
	UID uint32
	Msg string
}

// CarpCh - канал для передачи уведомлений
type CarpCh struct {
	Event      string
	UserName   string
	AssistName string
	Target     string
	UserID     uint32
}

// ErrorCode - константы кодов ошибок подписки
type ErrorCode int

const (
	ErrNoSubscription ErrorCode = iota + 1
	ErrSubscriptionExpired
	ErrMessageLimitExceeded // Legacy не используется
	ErrInsufficientBalance  // Legacy, не используется
	ErrInvalidSubscriptionData
)

// SubscriptionError - структура для ошибок подписки
type SubscriptionError struct {
	Message string
	Code    ErrorCode
	UserID  uint32
}

// Error реализует интерфейс error
func (e *SubscriptionError) Error() string {
	return e.Message
}

// SubscriptionProvider интерфейс для получения данных подписки
type SubscriptionProvider interface {
	GetUserSubscriptionLimites(userID uint32) (json.RawMessage, error)
}

// CheckUserSubscription проверяет подписку пользователя
func CheckUserSubscription(provider SubscriptionProvider, userID uint32) error {
	subscription, err := provider.GetUserSubscriptionLimites(userID)
	if err != nil {
		return &SubscriptionError{
			Code:    ErrInvalidSubscriptionData,
			Message: fmt.Sprintf("ошибка получения лимитов подписки: %v", err),
			UserID:  userID,
		}
	}

	if subscription == nil {
		return &SubscriptionError{
			Code:    ErrNoSubscription,
			Message: "пользователь не имеет подписки",
			UserID:  userID,
		}
	}

	type UserSubscription struct {
		EndDate    time.Time `json:"-"`
		EndDateStr string    `json:"EndDate"`
		Balance    float64   `json:"balance"`
	}

	var userSub UserSubscription
	if err := json.Unmarshal(subscription, &userSub); err != nil {
		return &SubscriptionError{
			Code:    ErrInvalidSubscriptionData,
			Message: fmt.Sprintf("ошибка парсинга данных подписки: %v", err),
			UserID:  userID,
		}
	}

	endDate, err := time.Parse("2006-01-02", userSub.EndDateStr)
	if err != nil {
		return &SubscriptionError{
			Code:    ErrInvalidSubscriptionData,
			Message: fmt.Sprintf("ошибка преобразования даты окончания подписки: %v", err),
			UserID:  userID,
		}
	}
	userSub.EndDate = endDate

	now := time.Now()
	if userSub.EndDate.Before(now) {
		return &SubscriptionError{
			Code:    ErrSubscriptionExpired,
			Message: fmt.Sprintf("подписка истекла %v", userSub.EndDate),
			UserID:  userID,
		}
	}

	return nil
}
