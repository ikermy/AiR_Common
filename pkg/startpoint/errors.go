package startpoint

import (
	"errors"
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/ikermy/AiR_Common/pkg/logger"
	"github.com/ikermy/AiR_Common/pkg/mode"
	"github.com/ikermy/AiR_Common/pkg/model"
)

// RetryableError представляет временную ошибку, которую можно повторить
type RetryableError struct {
	Err error
}

func (e *RetryableError) Error() string {
	return e.Err.Error()
}

func (e *RetryableError) Unwrap() error {
	return e.Err
}

// FatalError представляет критическую ошибку, требующую завершения
type FatalError struct {
	Err error
}

func (e *FatalError) Error() string {
	return e.Err.Error()
}

func (e *FatalError) Unwrap() error {
	return e.Err
}

// NonCriticalError представляет некритическую ошибку, не требующую завершения
type NonCriticalError struct {
	Err error
}

func (e *NonCriticalError) Error() string {
	return e.Err.Error()
}

func (e *NonCriticalError) Unwrap() error {
	return e.Err
}

// IsFatalError проверяет, является ли ошибка критической
func IsFatalError(err error) bool {
	var fatalErr *FatalError
	return errors.As(err, &fatalErr)
}

// IsNonCriticalError проверяет, является ли ошибка некритической
func IsNonCriticalError(err error) bool {
	var nonCritErr *NonCriticalError
	return errors.As(err, &nonCritErr)
}

// isFatalErrorPattern проверяет паттерны критических ошибок (auth, quota)
func isFatalErrorPattern(err error) bool {
	if err == nil {
		return false
	}
	errStr := err.Error()
	fatalPatterns := []string{
		"401", "403",
		"Unauthorized",
		"Forbidden",
		"invalid API key",
		"insufficient quota",
	}
	for _, pattern := range fatalPatterns {
		if strings.Contains(errStr, pattern) {
			return true
		}
	}
	return false
}

// isRetryableErrorPattern проверяет паттерны временных ошибок (5xx, сетевые)
func isRetryableErrorPattern(err error) bool {
	if err == nil {
		return false
	}
	errStr := err.Error()
	retryablePatterns := []string{
		"500", "502", "503", "504",
		"Service Unavailable",
		"Bad Gateway",
		"Gateway Timeout",
		"Internal Server Error",
		"upstream connect error",
		"connection reset",
		"connection refused",
		"connection termination",
		"timeout",
		"temporary failure",
	}
	for _, pattern := range retryablePatterns {
		if strings.Contains(errStr, pattern) {
			return true
		}
	}
	return false
}

// AskWithRetry выполняет запрос к модели с retry-логикой
func (s *Start) AskWithRetry(modelId string, dialogId uint64, arrAsk []string, files ...model.FileUpload) (model.AssistResponse, error) {
	var lastErr error

	for attempt := 0; attempt < mode.RetryMaxAttempts; attempt++ {
		response, err := s.ask(modelId, dialogId, arrAsk, files...)
		if err == nil {
			return response, nil
		}

		lastErr = err

		// Критическая ошибка — немедленный возврат
		if isFatalErrorPattern(err) {
			logger.Warn("Критическая ошибка для модели %s, диалог %d: %v", modelId, dialogId, err)
			return response, &FatalError{Err: fmt.Errorf("критическая ошибка: %w", err)}
		}

		// Временная ошибка — retry
		if isRetryableErrorPattern(err) {
			if attempt == mode.RetryMaxAttempts-1 {
				break
			}

			delay := time.Duration(mode.RetryBaseDelay) * time.Second * time.Duration(math.Pow(2, float64(attempt)))
			logger.Debug("Retry attempt %d/%d for model %s, dialog %d, waiting %v", attempt+1, mode.RetryMaxAttempts, modelId, dialogId, delay)

			select {
			case <-s.ctx.Done():
				return model.AssistResponse{}, &NonCriticalError{Err: s.ctx.Err()}
			case <-time.After(delay):
			}
			continue
		}

		// Некритическая ошибка (400, 404, 429, context canceled и др.) — сразу возвращаем
		logger.Debug("Non-critical error for model %s, dialog %d: %v", modelId, dialogId, err)
		return response, &NonCriticalError{Err: err}
	}

	// Все retry исчерпаны
	logger.Warn("Все %d попыток неуспешны для модели %s, диалог %d", mode.RetryMaxAttempts, modelId, dialogId)
	return model.AssistResponse{}, &NonCriticalError{Err: fmt.Errorf("все %d попыток неуспешны: %w", mode.RetryMaxAttempts, lastErr)}
}
