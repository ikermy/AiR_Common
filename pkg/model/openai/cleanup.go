package openai

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/ikermy/AiR_Common/pkg/logger"
	"github.com/ikermy/AiR_Common/pkg/model"
	"github.com/ikermy/AiR_Common/pkg/model/create"
	"github.com/sashabaranov/go-openai"
)

func (m *OpenAIModel) CleanUp() {
	ticker := time.NewTicker(15 * time.Minute)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			now := time.Now()
			deletedRespCount := 0
			checkedRespCount := 0

			m.responders.Range(func(key, value interface{}) bool {
				responder := value.(*RespModel)
				checkedRespCount++
				ttlExpired := responder.TTL.Before(now)

				respId, ok := key.(uint64)
				if !ok {
					logger.Error("Некорректный тип ключа: %T, ожидался uint64", key)
					return true
				}

				if ttlExpired {
					// Отменяем активные runs перед удалением
					if responder.Thread != nil {
						cancelCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
						if err := m.cancelActiveRunsCtx(cancelCtx, responder.Thread.ID); err != nil {
							logger.Warn("Ошибка при отмене runs для respId %d: %v", respId, err)
						}
						cancel()
					}

					// Удаляем весь RespModel (вместе с Thread)
					if responder.Cancel != nil {
						responder.Cancel()
					}
					m.closeResponderChannels(responder)
					m.responders.Delete(respId)
					deletedRespCount++
					logger.Info("Удален просроченный RespModel для respId %d (TTL истёк)", respId)
				}
				// Отдельная очистка Thread не нужна - он удаляется вместе с RespModel

				return true
			})

			if deletedRespCount > 0 {
				logger.Info("Очистка завершена: проверено %d RespModel, удалено %d RespModel",
					checkedRespCount, deletedRespCount)
			}
		case <-m.ctx.Done():
			logger.Info("Остановка фоновой очистки OpenAIModel")
			return
		}
	}
}

func (m *OpenAIModel) closeResponderChannels(respModel *RespModel) {
	if respModel.Chan != nil {
		close(respModel.Chan.TxCh)
		close(respModel.Chan.RxCh)
	}
}

func (m *OpenAIModel) cancelActiveRunsCtx(ctx context.Context, threadID string) error {
	runsList, err := m.client.ListRuns(ctx, threadID, openai.Pagination{
		Limit: func(i int) *int { return &i }(20),
	})
	if err != nil {
		return fmt.Errorf("не удалось получить список runs: %w", err)
	}

	for _, run := range runsList.Runs {
		if run.Status == openai.RunStatusQueued ||
			run.Status == openai.RunStatusInProgress ||
			run.Status == openai.RunStatusRequiresAction {

			_, err := m.client.CancelRun(ctx, threadID, run.ID)
			if err != nil {
				logger.Warn("Не удалось отменить run %s: %v", run.ID, err)
				continue
			}

			if err := m.waitForRunCancellationCtx(ctx, threadID, run.ID); err != nil {
				logger.Warn("Ошибка при ожидании отмены run %s: %v", run.ID, err)
			}
		}
	}

	return nil
}

func (m *OpenAIModel) waitForRunCancellationCtx(ctx context.Context, threadID, runID string) error {
	maxRetries := 50
	for i := 0; i < maxRetries; i++ {
		select {
		case <-ctx.Done():
			return fmt.Errorf("отмена ожидания отмены run %s: %w", runID, ctx.Err())
		default:
		}

		run, err := m.client.RetrieveRun(ctx, threadID, runID)
		if err != nil {
			return fmt.Errorf("не удалось получить статус run: %w", err)
		}
		if run.Status == openai.RunStatusCancelled ||
			run.Status == openai.RunStatusCompleted ||
			run.Status == openai.RunStatusFailed ||
			run.Status == openai.RunStatusExpired {
			return nil
		}
		time.Sleep(100 * time.Millisecond)
	}
	return fmt.Errorf("превышено время ожидания отмены run %s", runID)
}

func (m *OpenAIModel) cancelAllActiveRunsCtx(ctx context.Context) error {
	logger.Info("Отмена всех активных runs")
	var cancelErrors []string

	m.responders.Range(func(key, value interface{}) bool {
		respModel := value.(*RespModel)

		if respModel.Thread != nil {
			if err := m.cancelActiveRunsCtx(ctx, respModel.Thread.ID); err != nil {
				cancelErrors = append(cancelErrors, fmt.Sprintf("thread %s: %v", respModel.Thread.ID, err))
			}
		}
		return true
	})

	if len(cancelErrors) > 0 {
		return fmt.Errorf("ошибки отмены runs: %s", strings.Join(cancelErrors, "; "))
	}
	return nil
}

func (m *OpenAIModel) saveAllContextsGracefullyCtx(ctx context.Context) error {
	var wg sync.WaitGroup
	var mu sync.Mutex
	var saveErrors []string

	m.responders.Range(func(key, value interface{}) bool {
		respModel := value.(*RespModel)

		// Получаем dialogId из канала
		if respModel.Chan == nil {
			return true
		}

		dialogId := respModel.Chan.DialogId

		wg.Add(1)
		go func(dId uint64, thread *openai.Thread) {
			defer wg.Done()

			if thread != nil {
				// Сохраняем только thread_id, не весь объект Thread
				contextData := map[string]interface{}{
					"thread_id": thread.ID,
				}
				threadsJSON, err := json.Marshal(contextData)
				if err != nil {
					mu.Lock()
					saveErrors = append(saveErrors, fmt.Sprintf("сериализация для dialogId %d: %v", dId, err))
					mu.Unlock()
					return
				}

				select {
				case <-ctx.Done():
					mu.Lock()
					saveErrors = append(saveErrors, "превышен таймаут сохранения контекстов")
					mu.Unlock()
					return
				default:
				}

				if err := m.db.SaveContext(dId, create.ProviderOpenAI, threadsJSON); err != nil {
					mu.Lock()
					saveErrors = append(saveErrors, fmt.Sprintf("сохранение для dialogId %d: %v", dId, err))
					mu.Unlock()
				}
			}
		}(dialogId, respModel.Thread)

		return true
	})

	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()

	select {
	case <-done:
		logger.Info("Все контексты успешно сохранены")
	case <-ctx.Done():
		logger.Warn("Превышен таймаут при сохранении контекстов")
		mu.Lock()
		saveErrors = append(saveErrors, "превышен таймаут сохранения контекстов")
		mu.Unlock()
	}

	if len(saveErrors) > 0 {
		return fmt.Errorf("ошибки сохранения: %s", strings.Join(saveErrors, "; "))
	}
	return nil
}

func (m *OpenAIModel) cleanupAllResponders() {
	logger.Info("Закрытие всех каналов и очистка респондеров")

	m.responders.Range(func(key, value interface{}) bool {
		respModel := value.(*RespModel)

		if respModel.Cancel != nil {
			respModel.Cancel()
		}

		m.closeResponderChannels(respModel)
		m.responders.Delete(key)

		return true
	})
}

func (m *OpenAIModel) cleanupWaitChannels() {
	logger.Info("Очистка каналов ожидания")

	m.waitChannels.Range(func(key, value interface{}) bool {
		if ch, ok := value.(chan struct{}); ok {
			select {
			case <-ch:
				// Канал уже закрыт
			default:
				close(ch)
			}
		}
		m.waitChannels.Delete(key)
		return true
	})
}

// safeClose закрывает канал и обрабатывает панику, если канал уже закрыт
func safeClose(ch chan model.Message) {
	defer func() {
		if r := recover(); r != nil {
			logger.Error("Паника при закрытии канала: %v", r)
		}
	}()
	close(ch)
}
