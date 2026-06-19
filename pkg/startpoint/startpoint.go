package startpoint

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/ikermy/AiR_Common/pkg/com"
	"github.com/ikermy/AiR_Common/pkg/comdb"
	"github.com/ikermy/AiR_Common/pkg/endpoint"
	"github.com/ikermy/AiR_Common/pkg/mode"
	"github.com/ikermy/AiR_Common/pkg/model"
	"github.com/ikermy/AiR_Common/pkg/model/create"
	"github.com/ikermy/AiR_Common/pkg/operator"
)

// safeStopTimer корректно останавливает таймер, очищая канал если сигнал уже был отправлен.
func safeStopTimer(t *time.Timer) {
	if t == nil {
		return
	}
	if !t.Stop() {
		select {
		case <-t.C:
		default:
		}
	}
}

// routeQuestToOperator строит сообщение из quest и отправляет его оператору,
// затем фиксирует полный вопрос в fullQuestCh.
// Возвращает true если вызывающий должен выйти из Respondent.
func (s *Start) routeQuestToOperator(
	u *model.RespModel,
	treadId uint64,
	quest Question,
	fullQuestCh chan Answer,
	errCh chan error,
) (shouldReturn bool) {
	msgType := "user"
	if quest.Voice {
		msgType = "user_voice"
	}
	content := model.AssistResponse{Message: strings.Join(quest.Question, "\n")}
	name := u.Assist.AssistName
	opMsg := s.Mod.NewMessage(
		model.Operator{SetOperator: false, Operator: false, SenderName: quest.Operator.SenderName},
		msgType, &content, &name, quest.Files...,
	)
	if err := s.Oper.SendToOperator(s.ctx, u.Assist.UserID, treadId, opMsg); err != nil {
		s.sendError(errCh, fmt.Errorf("ошибка отправки сообщения оператору: %v", err))
	}
	select {
	case fullQuestCh <- Answer{Answer: content, VoiceQuestion: quest.Voice}:
	default:
		s.sendError(errCh, fmt.Errorf("канал fullQuestCh закрыт или переполнен"))
		return true
	}
	return false
}

// sendError безопасно отправляет ошибку в errCh без блокировки.
// Если канал переполнен, ошибка логируется как предупреждение.
func (s *Start) sendError(errCh chan<- error, err error) {
	select {
	case errCh <- err:
		// Успешно отправлено в канал
	default:
		// Канал переполнен - fallback логирование
		//logger.Warn("Канал errCh переполнен, ошибка: %v", err, userID)
	}
}

func (s *Start) pushAnswer(answerCh chan<- Answer, errCh chan<- error, ans Answer, errMsg string) bool {
	select {
	case answerCh <- ans:
		return true
	default:
		s.sendError(errCh, errors.New(errMsg))
		return false
	}
}

func (s *Start) trySendAnswer(answerCh chan<- Answer, ans Answer) {
	select {
	case answerCh <- ans:
	default:
	}
}

func (s *Start) sendFallbackAnswer(answerCh chan<- Answer, err error) {
	s.trySendAnswer(answerCh, Answer{
		Answer: model.AssistResponse{Message: "⚠️ Не удалось получить ответ, попробуйте ещё раз."},
		Err:    err,
	})
}

func (s *Start) handleAskFailure(
	u *model.RespModel,
	err error,
	answerCh chan<- Answer,
	errCh chan<- error,
	fatalMessage string,
) (shouldReturn bool) {
	if IsProviderLimitError(err) {
		s.handleProviderLimitError(u.Assist.UserID, u.RespName, u.Assist.AssistName, err.Error())
		return false
	}
	if IsFatalError(err) {
		s.sendError(errCh, fmt.Errorf("%s: %v", fatalMessage, err))
		return true
	}
	s.sendFallbackAnswer(answerCh, err)
	return false
}

func operatorSystemAnswer(message string) Answer {
	return Answer{
		Answer:   model.AssistResponse{Message: message},
		Operator: model.Operator{SetOperator: false, Operator: false},
	}
}

func operatorTimeoutMessage() string {
	if mode.OperatorResponseTimeout%60 == 0 && mode.OperatorResponseTimeout >= 60 {
		return fmt.Sprintf("⏱️ Оператор не ответил в течение %d мин\nПродолжаю работу в режиме AI-агента 🧠", mode.OperatorResponseTimeout/60)
	}
	return fmt.Sprintf("⏱️ Оператор не ответил в течение %d сек\nПродолжаю работу в режиме AI-агента 🧠", mode.OperatorResponseTimeout)
}

func stopOperatorTimeoutTimer(timer *time.Timer, timeoutCh <-chan struct{}) *time.Timer {
	if timer == nil {
		return nil
	}
	safeStopTimer(timer)
	select {
	case <-timeoutCh:
	default:
	}
	return nil
}

func (s *Start) startOperatorMode(u *model.RespModel, treadId uint64, timeoutCh chan<- struct{}) (<-chan model.Message, *time.Timer) {
	operatorRxCh := s.Oper.ReceiveFromOperator(s.ctx, u.Assist.UserID, treadId)
	operatorTimeoutTimer := time.AfterFunc(time.Duration(mode.OperatorResponseTimeout)*time.Second, func() {
		select {
		case timeoutCh <- struct{}{}:
		default:
		}
	})
	return operatorRxCh, operatorTimeoutTimer
}

// handleProviderLimitError обрабатывает лимитную ошибку AI-провайдера:
// отправляет уведомление пользователю через внешние каналы и возвращает deaf=false для продолжения цикла.
// Возвращает true, если вызывающий должен выполнить continue.
func (s *Start) handleProviderLimitError(userID uint32, respName, assistName, errMsg string) bool {
	s.End.SendEvent(userID, "ai-provider-limit", respName, assistName, errMsg)
	return true
}

// Question структура для хранения вопросов пользователя
type Question struct {
	Question []string           // Вопрос пользователя, может состоять из нескольких вопросов
	Voice    bool               // Флаг, указывающий, что вопрос был задан голосом
	Files    []model.FileUpload // Файлы, прикрепленные к вопросу
	Operator model.Operator     // Если true — вопрос должен быть отправлен оператору, а не модели
}

// Answer структура для хранения ответов пользователя
type Answer struct {
	Answer        model.AssistResponse
	VoiceQuestion bool           // Флаг, указывающий, что вопрос был задан голосом
	Operator      model.Operator // Фактически будем указывать кто ответил: модель или оператор
	Err           error          // Ошибка модели (не nil — модель не смогла ответить); текст в Answer.Message — fallback для пользователя
}

// BotInterface - интерфейс для различных реализаций ботов
type BotInterface interface {
	DisableOperatorMode(userID uint32, dialogID uint64, silent ...bool) error
}

type Model = model.Inter
type Endpoint = endpoint.Inter
type Operator = operator.Inter

// Start структура с интерфейсами вместо конкретных типов
type Start struct {
	ctx    context.Context
	cancel context.CancelFunc

	Mod  Model
	End  Endpoint
	Oper Operator
	Bot  BotInterface

	respondentWG sync.Map // map[uint64]*sync.WaitGroup - для синхронизации завершения Respondent

	// Карта для хранения провайдера каждого респондента (ключ: respID, значение: provider)
	// Используется для передачи информации о провайдере при вызове CallOptional
	responderProviders sync.Map // key: uint64 (respId), value: string (provider)

	// Накопители потоковых дельт по респондентам.
	// key: uint64 (respId), value: *streamAccumulator
	streamAccumulators sync.Map
}

// streamAccumulator накапливает сырые дельты и извлекает текст из поля "message".
// Потокобезопасен через internal mutex.
type streamAccumulator struct {
	mu             sync.Mutex
	rawAccumulated strings.Builder
	displayText    string
	messageDone    bool
}

var errMessageNotString = errors.New("message field is not a string")

// ProcessStreamDelta накапливает сырой чанк и извлекает текущий текст поля message.
// Для текстовых ответов возвращает kind=text и накапливаемый Text.
// Для function-call/service событий возвращает kind=event и исходный RawJSON.
func (s *Start) ProcessStreamDelta(respId uint64, rawChunk string) (model.StreamDeltaResult, error) {
	if rawChunk == "" {
		return model.StreamDeltaResult{
			Kind: model.StreamDeltaKindText,
			Text: s.GetStreamDisplayText(respId),
		}, nil
	}

	if eventResult, ok, err := tryParseStructuredStreamEvent(rawChunk); ok || err != nil {
		return eventResult, err
	}

	acc := s.getOrCreateStreamAccumulator(respId)
	acc.mu.Lock()
	defer acc.mu.Unlock()

	if acc.rawAccumulated.Len() == 0 && !strings.ContainsRune(rawChunk, '{') {
		acc.displayText += rawChunk
		return model.StreamDeltaResult{
			Kind:     model.StreamDeltaKindText,
			Text:     acc.displayText,
			Complete: false,
		}, nil
	}

	if acc.messageDone {
		return model.StreamDeltaResult{
			Kind:     model.StreamDeltaKindText,
			Text:     acc.displayText,
			Complete: true,
		}, nil
	}

	_, _ = acc.rawAccumulated.WriteString(rawChunk)
	raw := acc.rawAccumulated.String()

	newText, isComplete, parseErr := extractStreamText(raw)
	if parseErr != nil {
		return model.StreamDeltaResult{
			Kind:     model.StreamDeltaKindText,
			Text:     acc.displayText,
			Complete: false,
		}, parseErr
	}

	if newText != "" || isComplete {
		acc.displayText = newText
	}

	if isComplete {
		acc.messageDone = true
		acc.rawAccumulated.Reset()
	}

	return model.StreamDeltaResult{
		Kind:     model.StreamDeltaKindText,
		Text:     acc.displayText,
		Complete: acc.messageDone,
	}, nil
}

// GetStreamDisplayText возвращает последний извлечённый displayText для respId.
func (s *Start) GetStreamDisplayText(respId uint64) string {
	raw, ok := s.streamAccumulators.Load(respId)
	if !ok {
		return ""
	}
	acc := raw.(*streamAccumulator)
	acc.mu.Lock()
	defer acc.mu.Unlock()
	return acc.displayText
}

// ResetStreamAccumulator удаляет накопитель для respId.
func (s *Start) ResetStreamAccumulator(respId uint64) {
	s.streamAccumulators.Delete(respId)
}

func (s *Start) getOrCreateStreamAccumulator(respId uint64) *streamAccumulator {
	raw, _ := s.streamAccumulators.LoadOrStore(respId, &streamAccumulator{})
	return raw.(*streamAccumulator)
}

func tryParseStructuredStreamEvent(rawChunk string) (model.StreamDeltaResult, bool, error) {
	trimmed := strings.TrimSpace(rawChunk)
	if trimmed == "" || trimmed[0] != '{' {
		return model.StreamDeltaResult{}, false, nil
	}

	objPrefix, complete := firstJSONObjectPrefix(trimmed)
	if objPrefix == "" || !complete {
		return model.StreamDeltaResult{}, false, nil
	}

	var event map[string]interface{}
	if err := json.Unmarshal([]byte(objPrefix), &event); err != nil {
		return model.StreamDeltaResult{}, false, nil
	}

	eventType, _ := event["type"].(string)
	if eventType == "" {
		return model.StreamDeltaResult{}, false, nil
	}

	result := model.StreamDeltaResult{
		Kind:      model.StreamDeltaKindEvent,
		Complete:  eventType != "response.function_call_arguments.delta",
		EventType: eventType,
		RawJSON:   objPrefix,
	}

	if name, ok := event["name"].(string); ok {
		result.Name = name
	}

	if arguments, ok := event["arguments"].(string); ok {
		result.Arguments = arguments
	} else if delta, ok := event["delta"].(string); ok {
		result.Arguments = delta
	}

	return result, true, nil
}

// extractStreamText извлекает поле "message" из первой JSON-структуры.
// Возвращает текущий текст, complete=true если строка message закрыта, и ошибку диагностики.
func extractStreamText(raw string) (text string, complete bool, err error) {
	start := strings.IndexByte(raw, '{')
	if start < 0 {
		return "", false, nil
	}

	prefix := raw[start:]

	objPrefix, _ := firstJSONObjectPrefix(prefix)
	if objPrefix == "" {
		return "", false, nil
	}

	msg, msgComplete, found, parseErr := extractTopLevelMessage(objPrefix)
	if parseErr != nil {
		return "", false, parseErr
	}
	if !found {
		return "", false, nil
	}

	return msg, msgComplete, nil
}

// firstJSONObjectPrefix возвращает префикс первой JSON-структуры (от '{' до текущего конца или полного закрытия). 
func firstJSONObjectPrefix(s string) (prefix string, complete bool) {
	if s == "" || s[0] != '{' {
		return "", false
	}

	depth := 0
	inString := false
	escaped := false

	for i := 0; i < len(s); i++ {
		ch := s[i]

		if inString {
			if escaped {
				escaped = false
				continue
			}
			if ch == '\\' {
				escaped = true
				continue
			}
			if ch == '"' {
				inString = false
			}
			continue
		}

		switch ch {
		case '"':
			inString = true
		case '{':
			depth++
		case '}':
			depth--
			if depth == 0 {
				return s[:i+1], true
			}
		}
	}

	return s, false
}

func extractTopLevelMessage(objPrefix string) (message string, complete bool, found bool, err error) {
	if objPrefix == "" || objPrefix[0] != '{' {
		return "", false, false, nil
	}

	i := 1
	for i < len(objPrefix) {
		i = skipSpaces(objPrefix, i)
		if i >= len(objPrefix) {
			break
		}
		if objPrefix[i] == '}' {
			break
		}
		if objPrefix[i] == ',' {
			i++
			continue
		}
		if objPrefix[i] != '"' {
			i++
			continue
		}

		keyRaw, next, keyComplete := scanJSONStringRaw(objPrefix, i)
		if !keyComplete {
			return "", false, false, nil
		}
		key := decodeJSONStringLossy(keyRaw)
		i = skipSpaces(objPrefix, next)
		if i >= len(objPrefix) || objPrefix[i] != ':' {
			return "", false, false, nil
		}
		i++
		i = skipSpaces(objPrefix, i)
		if i >= len(objPrefix) {
			return "", false, key == "message", nil
		}

		if key == "message" {
			if objPrefix[i] != '"' {
				return "", false, true, errMessageNotString
			}
			valRaw, _, valComplete := scanJSONStringRaw(objPrefix, i)
			return decodeJSONStringLossy(valRaw), valComplete, true, nil
		}

		nextValue, ok := skipJSONValue(objPrefix, i)
		if !ok {
			return "", false, false, nil
		}
		i = nextValue
	}

	return "", false, false, nil
}

func skipSpaces(s string, i int) int {
	for i < len(s) {
		switch s[i] {
		case ' ', '\n', '\t', '\r':
			i++
		default:
			return i
		}
	}
	return i
}

func scanJSONStringRaw(s string, start int) (raw string, next int, complete bool) {
	if start >= len(s) || s[start] != '"' {
		return "", start, false
	}

	var b strings.Builder
	escaped := false
	for i := start + 1; i < len(s); i++ {
		ch := s[i]
		if escaped {
			b.WriteByte(ch)
			escaped = false
			continue
		}
		if ch == '\\' {
			b.WriteByte(ch)
			escaped = true
			continue
		}
		if ch == '"' {
			return b.String(), i + 1, true
		}
		b.WriteByte(ch)
	}

	return b.String(), len(s), false
}

func decodeJSONStringLossy(raw string) string {
	decoded, err := strconv.Unquote("\"" + raw + "\"")
	if err == nil {
		return decoded
	}

	// Частично декодируем популярные escape-последовательности в незавершённых дельтах.
	replacer := strings.NewReplacer(
		`\\n`, "\n",
		`\\r`, "\r",
		`\\t`, "\t",
		`\\\"`, `"`,
		`\\\\`, `\\`,
		`\\/`, `/`,
	)
	out := replacer.Replace(raw)
	if strings.HasSuffix(out, "\\") {
		out = strings.TrimSuffix(out, "\\")
	}
	return out
}

func skipJSONValue(s string, i int) (next int, ok bool) {
	if i >= len(s) {
		return i, false
	}

	switch s[i] {
	case '"':
		_, next, complete := scanJSONStringRaw(s, i)
		return next, complete
	case '{':
		depth := 0
		inString := false
		escaped := false
		for j := i; j < len(s); j++ {
			ch := s[j]
			if inString {
				if escaped {
					escaped = false
					continue
				}
				if ch == '\\' {
					escaped = true
					continue
				}
				if ch == '"' {
					inString = false
				}
				continue
			}
			switch ch {
			case '"':
				inString = true
			case '{':
				depth++
			case '}':
				depth--
				if depth == 0 {
					return j + 1, true
				}
			}
		}
		return len(s), false
	case '[':
		depth := 0
		inString := false
		escaped := false
		for j := i; j < len(s); j++ {
			ch := s[j]
			if inString {
				if escaped {
					escaped = false
					continue
				}
				if ch == '\\' {
					escaped = true
					continue
				}
				if ch == '"' {
					inString = false
				}
				continue
			}
			switch ch {
			case '"':
				inString = true
			case '[':
				depth++
			case ']':
				depth--
				if depth == 0 {
					return j + 1, true
				}
			}
		}
		return len(s), false
	default:
		for j := i; j < len(s); j++ {
			switch s[j] {
			case ',', '}':
				return j, true
			}
		}
		return len(s), true
	}
}

// New создаёт новый экземпляр Start
func New(parent context.Context, mod Model, end Endpoint, bot BotInterface, operator Operator) *Start {
	ctx, cancel := context.WithCancel(parent)
	return &Start{
		ctx:    ctx,
		cancel: cancel,

		Mod:  mod,
		End:  end,
		Bot:  bot,
		Oper: operator,
	}
}

// Shutdown останавливает внутренний контекст Start и даёт возможность корректно завершить фоновые операции
func (s *Start) Shutdown(shutCh chan<- com.LogMsg) {
	if s.cancel != nil {
		s.cancel()
	}
	shutCh <- com.LogMsg{
		Msg: "успешно завершил работу",
		Mod: "Startpoint",
		Log: 0, // 0 - Info
		UID: 0,
	}
}

func (s *Start) ask(userID uint32, respId, dialogID uint64, arrAsk []string, files ...model.FileUpload) (model.AssistResponse, error) {
	var emptyResponse model.AssistResponse
	answerCh := make(chan model.AssistResponse, 1)
	errCh := make(chan error, 1)
	defer close(answerCh)
	defer close(errCh)

	var ask string
	for _, v := range arrAsk {
		if v != "" {
			ask += v + "\n"
		}
	}

	if ask == "" && len(files) == 0 {
		return emptyResponse, fmt.Errorf("ASK EMPTY MESSAGE AND NO FILES")
	}

	if mode.TestAnswer {
		filesInfo := ""
		if len(files) > 0 {
			filesInfo = fmt.Sprintf(" with %d files", len(files))
		}
		return model.AssistResponse{
			Message: "AssistId model " + " resp " + ask + filesInfo,
		}, nil
	}

	// Контекст ожидания ответа модели с таймаутом, завязанным на общий контекст Start
	ctx, cancel := context.WithTimeout(s.ctx, mode.ErrorTimeOutDurationForAssistAnswer*time.Minute)
	defer cancel()

	go func() {
		// Ранний выход, если контекст уже отменён
		select {
		case <-ctx.Done():
			//logger.Debug("ask ранний выход по ctx.Done() диалог %d", dialogID)
			return
		default:
		}

		// Используем RequestStreaming для потоковой передачи ответа с TRUE STREAMING
		var fullResponse string
		var deltaBatch strings.Builder // Батчинг дельт
		var batchCount int
		const batchSize = 3 // Оптимальный баланс: быстрая доставка + меньше нагрузки на WebSocket

		// Счетчик дельт для мониторинга
		//var deltaCounter int

		// Кэшируем канал для ускорения отправки
		ch, err := s.Mod.GetCh(respId)
		if err != nil {
			//logger.Error("ask: ошибка получения канала для respId=%d: %v", respId, err, userID)
			select {
			case errCh <- fmt.Errorf("ask error getting channel: %w", err):
			default:
			}
			return
		}

		// Мониторинг начального состояния канала TxCh
		//logger.Debug("📊 [MONITOR] TxCh начало: буфер=%d/%d (%.1f%%), respId=%d",
		//	len(ch.TxCh), cap(ch.TxCh), float64(len(ch.TxCh))/float64(cap(ch.TxCh))*100.0, respId, userID)

		streamErr := s.Mod.RequestStreaming(userID, dialogID, ask, func(delta string, done bool) error {
			// Проверяем контекст в начале - если отменён, не обрабатываем дельту
			select {
			case <-ctx.Done():
				return fmt.Errorf("context cancelled")
			default:
			}

			if done {
				// Финальный ответ - сохраняем полный текст для БД
				fullResponse = delta

				// Отправляем остатки батча если есть
				if deltaBatch.Len() > 0 {
					deltaMsg := s.Mod.NewMessage(
						model.Operator{SetOperator: false, Operator: false},
						"assistant_delta",
						&model.AssistResponse{Message: deltaBatch.String()},
						nil,
					)

					// БЛОКИРУЮЩАЯ отправка финального батча с прерыванием по контексту
					// Это критически важно для Google Gemini, который отправляет дельты мгновенно
					// Отправка будет ждать пока канал освободится (без жёсткого таймаута)
					select {
					case ch.TxCh <- deltaMsg:
						// Успешно отправлено
						//logger.Debug("ask: финальный батч успешно отправлен (len=%d)", deltaBatch.Len(), userID)
					case <-ctx.Done():
						// Контекст отменён - прерываем отправку
						//logger.Warn("ask: отправка финального батча прервана (context cancelled)", userID)
						return fmt.Errorf("context cancelled")
					}
				}
			} else {
				// Проверяем, является ли delta JSON событием function call
				// События function calls начинаются с '{' и содержат поле "type"
				isJSONEvent := false
				if len(delta) > 0 && delta[0] == '{' {
					var event map[string]interface{}
					if err := json.Unmarshal([]byte(delta), &event); err == nil {
						if eventType, ok := event["type"].(string); ok {
							// Проверяем типы событий function calls и служебных сообщений
							// OpenAI: response.output_item.*, response.function_call_arguments.*
							// Mistral: function_call
							// Общие: function_result, token_usage
							if strings.HasPrefix(eventType, "response.output_item.") ||
								strings.HasPrefix(eventType, "response.function_call_arguments.") ||
								eventType == "function_call" ||
								eventType == "function_result" ||
								eventType == "token_usage" {
								isJSONEvent = true

								// JSON события отправляем немедленно
								deltaMsg := s.Mod.NewMessage(
									model.Operator{SetOperator: false, Operator: false},
									"assistant_delta",
									&model.AssistResponse{Message: delta},
									nil,
								)

								// НЕБЛОКИРУЮЩАЯ отправка с проверкой контекста
								select {
								case ch.TxCh <- deltaMsg:
									// Успешно отправлено
								case <-ctx.Done():
									// Контекст отменён - пропускаем событие
									//logger.Debug("ask: JSON событие пропущено (context cancelled, type=%s)", eventType, userID)
								default:
									//logger.Warn("ask: канал переполнен, JSON событие пропущено (type=%s)", eventType, userID)
								}
							}
						}
					}
				}

				// Обычные текстовые дельты - накапливаем в батч
				if !isJSONEvent {
					deltaBatch.WriteString(delta)
					batchCount++

					// Отправляем батч когда накопилось достаточно
					if batchCount >= batchSize {
						deltaMsg := s.Mod.NewMessage(
							model.Operator{SetOperator: false, Operator: false},
							"assistant_delta",
							&model.AssistResponse{Message: deltaBatch.String()},
							nil,
						)

						// НЕБЛОКИРУЮЩАЯ отправка с проверкой контекста
						select {
						case ch.TxCh <- deltaMsg:
							// Успешно отправлено
						case <-ctx.Done():
							// Контекст отменён - прерываем обработку
							//logger.Debug("ask: отправка батча прервана (context cancelled)", userID)
							return fmt.Errorf("context cancelled")
						default:
							// Канал переполнен, пропускаем эту дельту (клиент увидит следующую)
							//logger.Warn("ask: канал TxCh переполнен, пропущена дельта (len=%d)", deltaBatch.Len(), userID)
						}

						// Очищаем буфер после отправки
						deltaBatch.Reset()
						batchCount = 0
					}
				}
			}
			return nil
		}, files...)

		// Мониторинг финального состояния канала TxCh
		//logger.Debug("📊 [MONITOR] TxCh финал: буфер=%d/%d (%.1f%%), всего дельт=%d, respId=%d",
		//	len(ch.TxCh), cap(ch.TxCh), float64(len(ch.TxCh))/float64(cap(ch.TxCh))*100.0, deltaCounter, respId, userID)

		if streamErr != nil {
			//logger.Error("ask: ошибка запроса к модели, dialogID=%d: %v", dialogID, streamErr, userID)
			select {
			case errCh <- fmt.Errorf("ask error making request: %w", streamErr):
			default:
			}
			return
		}

		// Парсим финальный JSON ответ как AssistResponse
		var body model.AssistResponse
		if err := json.Unmarshal([]byte(fullResponse), &body); err != nil {
			//logger.Error("ask: ошибка парсинга ответа модели, dialogID=%d: %v", dialogID, err, userID)
			// Если не удалось распарсить - возвращаем текст как есть
			body = model.AssistResponse{Message: fullResponse}
		}

		select {
		case answerCh <- body:
		case <-ctx.Done():
		}
	}()

	// Жду либо ответа, либо ошибки, либо отмены/таймаута
	select {
	case body := <-answerCh:
		return body, nil
	case err := <-errCh:
		return emptyResponse, err
	case <-ctx.Done():
		// Возвращаем пустой ответ с ошибкой контекста для явного отличия от успешной пустоты
		return emptyResponse, ctx.Err()
	}
}

func (s *Start) Respondent(u *model.RespModel, questionCh chan Question, answerCh, fullQuestCh chan Answer,
	respId, treadId uint64, errCh chan error) {
	var (
		deaf                 bool   // Не слушать ввод пользователя до момента получения ответа
		ask                  string // Вопрос пользователя
		askTimer             *time.Timer
		VoiceQuestion        bool                 // Флаг, указывающий, что вопрос был задан голосом
		currentQuest         Question             // Текущий вопрос пользователя, который обрабатывается
		operatorMode         bool                 // Флаг включенного операторского режима
		operatorRxCh         <-chan model.Message // Канал для получения сообщений от оператора
		operatorErrorCh      <-chan string        // Канал для получения ошибок от операторского бэка
		operatorTimeoutTimer *time.Timer          // Таймер для отслеживания таймаута ответа оператора
		operatorTimeoutCh    chan struct{}        // Канал для сигнала о таймауте оператора
	)

	// Создаём канал для таймаута оператора
	operatorTimeoutCh = make(chan struct{}, 1)

	// Получаем канал ошибок сразу при запуске Respondent
	operatorErrorCh = s.Oper.GetConnectionErrors(s.ctx, u.Assist.UserID, treadId)

	for {
		select {
		case <-s.ctx.Done():
			//logger.Debug("Start context canceled in Respondent %s", u.RespName)
			return
		case <-u.Ctx.Done():
			//logger.Debug("Context.Done Respondent %s", u.RespName)
			return

		// Обработка ошибок подключения к оператору (только если режим оператора включен)
		case errorType := <-func() <-chan string {
			if operatorMode {
				return operatorErrorCh
			}
			return nil
		}():
			//logger.Debug("Respondent: получен errorType из operatorErrorCh: %s", errorType)
			if errorType == "no_tg_id" {
				//logger.Warn("Нет tg_id, отключаем операторский режим")
				operatorMode = false
				operatorRxCh = nil

				// Вызываю тихое отключение режима оператор для пользовательского бота
				err := s.Bot.DisableOperatorMode(u.Assist.UserID, treadId, true)
				if err != nil {
					s.sendError(errCh, fmt.Errorf("ошибка при отключении режима оператора: %w", err))
				}

				// Отправляем информационное сообщение пользователю
				if !s.pushAnswer(
					answerCh,
					errCh,
					operatorSystemAnswer("🚫👨‍💻 Нет доступных операторов \n Продолжаю работу в режиме AI-агента 🧠"),
					"канал answerCh закрыт при отправке сообщения об ошибке tg_id",
				) {
					return
				}

				// Получаем новый канал ошибок для следующих попыток
				operatorErrorCh = s.Oper.GetConnectionErrors(s.ctx, u.Assist.UserID, treadId)
				continue
			}

		// Обработка таймаута ожидания ответа оператора
		case <-operatorTimeoutCh:
			//logger.Warn("Таймаут ожидания ответа оператора (%d сек), переключение на AI режим",
			//	mode.OperatorResponseTimeout)

			// Останавливаем таймер
			operatorTimeoutTimer = stopOperatorTimeoutTimer(operatorTimeoutTimer, operatorTimeoutCh)

			// Отключаем операторский режим
			operatorMode = false
			operatorRxCh = nil

			// Удаляем сессию оператора
			if err := s.Oper.DeleteSession(u.Assist.UserID, treadId); err != nil {
				//logger.Warn("Ошибка при удалении сессии оператора: %v", err)
			}

			// Отключаем режим оператора в боте
			if err := s.Bot.DisableOperatorMode(u.Assist.UserID, treadId); err != nil {
				//logger.Warn("Ошибка при отключении режима оператора в боте: %v", err)
			}

			// Отправляем информационное сообщение пользователю о переключении на AI
			s.trySendAnswer(answerCh, operatorSystemAnswer(operatorTimeoutMessage()))

			// Если есть текущий вопрос без ответа, обрабатываем его через AI
			if !deaf && currentQuest.Question != nil && len(currentQuest.Question) > 0 {
				//logger.Debug("Обрабатываем необработанный вопрос через AI после таймаута оператора")

				// Формируем вопрос для AI
				userAsk := currentQuest.Question

				// Отправляем запрос в AI
				answer, err := s.AskWithRetry(u.Assist.UserID, respId, treadId, userAsk, currentQuest.Files...)
				if err != nil {
					deaf = false
					if s.handleAskFailure(u, err, answerCh, errCh, "критическая ошибка при обработке вопроса после таймаута оператора") {
						return
					}
				} else {
					//logger.Debug("ans: %v", answer)
					// Отправляем ответ AI
					if !s.pushAnswer(
						answerCh,
						errCh,
						Answer{
							Answer:        answer,
							VoiceQuestion: currentQuest.Voice,
							Operator:      model.Operator{SetOperator: false, Operator: false},
						},
						"канал answerCh закрыт при отправке ответа AI после таймаута оператора",
					) {
						return
					}
					deaf = false
				}
			}
			continue

		// Обработка сообщений от оператора (только если канал инициализирован)
		case operatorMsg := <-func() <-chan model.Message {
			if operatorMode && operatorRxCh != nil {
				return operatorRxCh
			}
			return nil
		}():
			if operatorMsg.Type == "" {
				continue // Пустое сообщение из nil канала
			}

			// Проверка на системное сообщение о выключении режима
			if operatorMsg.Operator.SetOperator &&
				operatorMsg.Operator.Operator &&
				operatorMsg.Content.Message == "Set-Mode-To-AI" {
				//logger.Debug("Получено системное сообщение о выключении режима оператора")
				operatorMode = false

				// Удаляем сессию оператора
				err := s.Oper.DeleteSession(u.Assist.UserID, treadId)
				if err != nil {
					s.sendError(errCh, fmt.Errorf("ошибка при удалении текущей сессии оператора: %v", err))
				}

				// Вызываем колбэк для корректного завершения сессии оператора
				err = s.Bot.DisableOperatorMode(u.Assist.UserID, treadId)
				if err != nil {
					s.sendError(errCh, fmt.Errorf("ошибка при отключении режима оператора: %w", err))
				}
				continue
			}

			// Останавливаем таймер ожидания первого ответа оператора
			// После первого ответа режим становится постоянным (без таймера)
			if operatorTimeoutTimer != nil {
				operatorTimeoutTimer = stopOperatorTimeoutTimer(operatorTimeoutTimer, operatorTimeoutCh)
				//logger.Debug("Таймер оператора остановлен - режим теперь постоянный")
			}

			// Отправка ответа оператора пользователю
			answ := Answer{
				Answer:        operatorMsg.Content,
				VoiceQuestion: false,
				Operator:      operatorMsg.Operator,
			}

			if !s.pushAnswer(answerCh, errCh, answ, "канал answerCh закрыт или переполнен") {
				return
			}
			continue // т.к. это операторское сообщение то сразу ждём следующее, а не спускаемся вниз по логике AI

		case quest, open := <-questionCh:
			if !open {
				s.sendError(errCh, fmt.Errorf("канал questionCh закрыт"))
				return // Тут только выходить
			}

			currentQuest = quest

			// Если уже активен операторский режим — шлём сообщение оператору неблокирующе и не идём в AI
			if operatorMode {
				safeStopTimer(askTimer)
				if s.routeQuestToOperator(u, treadId, quest, fullQuestCh, errCh) {
					return
				}
				continue
			}

			// Обработка SetOperator режима
			if quest.Operator.SetOperator {
				// Инициализация канала оператора при первом включении режима
				if !operatorMode {
					operatorMode = true
					operatorRxCh, operatorTimeoutTimer = s.startOperatorMode(u, treadId, operatorTimeoutCh)
					//logger.Debug("Включен операторский режим (таймаут: %d сек)", mode.OperatorResponseTimeout)
				}

				safeStopTimer(askTimer)
				if s.routeQuestToOperator(u, treadId, quest, fullQuestCh, errCh) {
					return
				}
				continue
			}

			// Проверка триггеров
			if len(u.Assist.Metas.Triggers) > 0 {
				userQuestion := strings.Join(quest.Question, "\n")
				for _, trigger := range u.Assist.Metas.Triggers {
					if strings.Contains(userQuestion, trigger) {
						if err := s.End.Meta(u.Assist.UserID, treadId, "trigger", u.RespName, u.Assist.AssistName, u.Assist.Metas.MetaAction); err != nil {
							s.sendError(errCh, fmt.Errorf("ошибка Meta триггер userID=%d dialogID=%d: %w", u.Assist.UserID, treadId, err))
						}

						//currentQuest.Operator.Operator = true
						// Активация операторского режима при триггере
						//if !operatorMode {
						//	operatorMode = true
						//	operatorRxCh = s.Inter.ReceiveFromOperator(s.ctx, u.Assist.UserID, treadId)
						//	logger.Debug("Операторский режим активирован по триггеру для пользователя %d")
						//}
						//logger.Debug("'Respondent' триггер найден в вопросе пользователя, запрашиваю операторский режим")
					}
				}
			}

			ask = strings.Join(quest.Question, "\n")
			VoiceQuestion = quest.Voice

			if s.End.SetUserAsk(treadId, respId, ask, u.Assist.Limit) {
				askTimer = time.NewTimer(time.Duration(u.Assist.Espero) * time.Second)
			} else {
				if askTimer == nil {
					askTimer = time.NewTimer(0)
				} else {
					askTimer.Reset(0)
				}
			}
		}

	inputLoop:
		for {
			// Если deaf=true (Ignore=true) — не слушать новые вопросы, сразу идти к модели
			if deaf {
				break inputLoop
			}

			if askTimer == nil {
				askTimer = time.NewTimer(time.Duration(u.Assist.Espero) * time.Second)
			}

			select {
			case <-s.ctx.Done():
				if askTimer != nil {
					askTimer.Stop()
				}
				//logger.Debug("Start context canceled during inputLoop %s", u.RespName)
				return
			case <-u.Ctx.Done():
				if askTimer != nil {
					askTimer.Stop()
				}
				//logger.Debug("User context canceled during inputLoop %s", u.RespName)
				return
			case inputStruct, open := <-questionCh:
				if !open {
					askTimer.Stop()
					s.sendError(errCh, fmt.Errorf("канал questionCh закрыт"))
					// По хорошему нужно выходить
				}
				// Обновляем флаги оператора текущего вопроса,
				// чтобы не утекали устаревшие значения
				currentQuest.Operator = inputStruct.Operator

				ask = strings.Join(inputStruct.Question, "\n")
				// Добавляю вопрос для контекста
				if s.End.SetUserAsk(treadId, respId, ask, u.Assist.Limit) {
					// Перезапускаю таймер
					if !askTimer.Stop() {
						<-askTimer.C // Сбрасываем любой оставшийся сигнал, чтобы избежать гонок
					}
					askTimer.Reset(time.Duration(u.Assist.Espero) * time.Second)
				} else {
					if askTimer == nil {
						askTimer = time.NewTimer(0) // Инициализируем таймер, если он nil
					} else {
						askTimer.Reset(0) // Сразу отправляю вопрос ассистенту
					}
				}

			case <-askTimer.C:
				askTimer.Stop()
				// Устанавливаем deaf в зависимости от настроек модели:
				// Ignore=true  → deaf=true  (не слушать новые вопросы пока модель думает)
				// Ignore=false → deaf=false (продолжать слушать)
				deaf = u.Assist.Ignore
				break inputLoop
			}
		}

		// Собираем batched вопрос
		userAsk := s.End.GetUserAsk(treadId, respId)
		if strings.TrimSpace(strings.Join(userAsk, "\n")) == "" {
			// Пустой запрос, пропускаем
			continue
		}
		// Сохраняю запрос пользователя для сохранения диалога
		fullAsk := Answer{
			Answer: model.AssistResponse{
				Message: strings.Join(userAsk, "\n"),
			},
			VoiceQuestion: VoiceQuestion, // Передаём информацию о голосовом вопросе
		}

		// Проверяю что канал fullQuestCh не закрыт
		select {
		case fullQuestCh <- fullAsk:
		default:
			s.sendError(errCh, fmt.Errorf("канал fullQuestCh закрыт или переполнен"))
			return
		}

		var (
			answer           model.AssistResponse
			err              error
			operatorAnswered bool
			setOperatorMode  bool
		)

		// Операторский запрос (явный), без SetOperator — сначала пробуем синхронно спросить оператора
		if currentQuest.Operator.Operator {
			// Если вопрос помечен как операторский но операторский режим ещё не включён,
			// значит это первоначальный запрос на операторский режим, пробую связаться с оператором
			msgType := "user"
			if VoiceQuestion {
				msgType = "user_voice"
			}
			content := model.AssistResponse{Message: strings.Join(userAsk, "\n")}
			name := u.Assist.AssistName
			opMsg := s.Mod.NewMessage(model.Operator{Operator: true, SenderName: currentQuest.Operator.SenderName}, msgType, &content, &name, currentQuest.Files...)

			var respMsg model.Message
			respMsg, err = s.Oper.AskOperator(s.ctx, u.Assist.UserID, treadId, opMsg)
			// Если получили ошибку от оператора или пустой ответ — делаем фолбэк в OpenAI
			if err != nil || (respMsg.Content.Message == "" && len(respMsg.Content.Action.SendFiles) == 0) {
				s.sendError(errCh, fmt.Errorf("ошибка запроса к оператору или пустой ответ, фолбэк в OpenAI: %v", err))
				// Отправляю запрос в OpenAI
				answer, err = s.AskWithRetry(u.Assist.UserID, respId, treadId, userAsk, currentQuest.Files...)
				if err != nil {
					deaf = false
					if s.handleAskFailure(u, err, answerCh, errCh, fmt.Sprintf("критическая ошибка для пользователя %d", u.Assist.UserID)) {
						return
					}
					continue
				}
				operatorAnswered = false
			} else {
				answer = respMsg.Content
				operatorAnswered = true
				// Если оператор ответил, то устанавливаю флаг операторского режима
				setOperatorMode = true

				// Включаем постоянный режим после успешного ответа оператора
				if !operatorMode {
					operatorMode = true
					operatorRxCh, operatorTimeoutTimer = s.startOperatorMode(u, treadId, operatorTimeoutCh)
					//logger.Debug("Операторский режим активирован после ответа оператора (таймаут: %d сек)", mode.OperatorResponseTimeout)
				} else if operatorTimeoutTimer != nil {
					// Оператор ответил - останавливаем таймер навсегда
					// Режим становится постоянным
					operatorTimeoutTimer = stopOperatorTimeoutTimer(operatorTimeoutTimer, operatorTimeoutCh)
					//logger.Debug("Таймер оператора остановлен - режим теперь постоянный")
				}
			}

		} else {
			// Отправляю запрос в OpenAI
			answer, err = s.AskWithRetry(u.Assist.UserID, respId, treadId, userAsk, currentQuest.Files...)
			if err != nil {
				deaf = false
				if s.handleAskFailure(u, err, answerCh, errCh, fmt.Sprintf("критическая ошибка для пользователя %d", u.Assist.UserID)) {
					return
				}
				continue
			}

			// Пришёл ответ от модели, проверяю на флаг запроса операторского режима
			if answer.Operator {
				// Модель запросила эскалацию к оператору
				if !operatorMode {
					operatorMode = true
					operatorRxCh, operatorTimeoutTimer = s.startOperatorMode(u, treadId, operatorTimeoutCh)
					s.End.SendEvent(u.Assist.UserID, "model-operator", u.RespName, u.Assist.AssistName, "")
					//logger.Debug("Операторский режим активирован по флагу ответа модели")
				}

				setOperatorMode = true // Передадим наружу, чтобы фронт включил режим
				// Неблокирующе отправим оператору исходный вопрос (как при SetOperator)
				msgType := "user"
				if VoiceQuestion {
					msgType = "user_voice"
				}
				// Можно отправить именно пользовательский вопрос, а не ответ модели
				contentToOp := model.AssistResponse{Message: strings.Join(userAsk, "\n")}
				name := u.Assist.AssistName
				opMsg := s.Mod.NewMessage(
					model.Operator{Operator: true, SenderName: currentQuest.Operator.SenderName},
					msgType,
					&contentToOp,
					&name,
					currentQuest.Files...,
				)
				if errSend := s.Oper.SendToOperator(s.ctx, u.Assist.UserID, treadId, opMsg); errSend != nil {
					s.sendError(errCh, fmt.Errorf("ошибка отправки эскалации оператору: %v", errSend))
				}
			}
		}

		if currentQuest.Operator.SetOperator {
			// Если это неблокирующая отправка оператору, пропускаем отправку ответа пользователю
			// но сохраняем диалог
			fullAsk := Answer{
				Answer: model.AssistResponse{
					Message: strings.Join(userAsk, "\n"),
				},
				VoiceQuestion: VoiceQuestion,
			}

			select {
			case fullQuestCh <- fullAsk:
			default:
				// обработка ошибки
			}

			continue // Только здесь используем continue
		}

		// После ответа модели:
		// Ignore=false → deaf=false (слушаем новые вопросы сразу)
		// Ignore=true  → deaf=true  (новые вопросы не принимаем до прихода следующего вопроса через главный select)
		deaf = u.Assist.Ignore

		// Если пустой ответ
		if answer.Message == "" && len(answer.Action.SendFiles) == 0 {
			continue
		}

		// Проверяю на содержание в ответе цели из u.Assist.Metas.MetaAction
		if u.Assist.Metas.MetaAction != "" {
			if answer.Meta { // Ассистент пометил ответ как достигший цели
				if err := s.End.Meta(u.Assist.UserID, treadId, "target", u.RespName, u.Assist.AssistName, u.Assist.Metas.MetaAction); err != nil {
					s.sendError(errCh, fmt.Errorf("ошибка Meta цель userID=%d dialogID=%d: %w", u.Assist.UserID, treadId, err))
				}
			}

			// Только для Lead Hunter достижение цели с передачей контакта
			if err := s.End.CallOptional(int64(respId)); err != nil {
				//logger.Error("ошибка вызова CallOptional для respId %d: %v", respId, err)
			}
		}

		// Отправляем ответ вызывающей функции
		answ := Answer{
			Answer: answer,
			Operator: model.Operator{
				SetOperator: setOperatorMode,
				Operator:    operatorAnswered,
			},
		}

		//Проверяю что канал answerCh не закрыт
		select {
		case answerCh <- answ:
		default:
			select {
			case errCh <- fmt.Errorf("канал answerCh закрыт или переполнен"):
			default:
			}
		}
	}
}

func (s *Start) StarterRespondent(
	u *model.RespModel,
	questionCh chan Question,
	answerCh chan Answer,
	fullQuestCh chan Answer,
	respId uint64,
	treadId uint64,
	errCh chan error,
) {
	if !u.Services.Respondent.Load() {
		u.Services.Respondent.Store(true)

		// Создаем WaitGroup для синхронизации
		wg := &sync.WaitGroup{}
		wg.Add(1)
		s.respondentWG.Store(treadId, wg)

		go func() {
			defer func() {
				u.Services.Respondent.Store(false)
				wg.Done()
				s.respondentWG.Delete(treadId)
			}()

			// Реагируем на отмену общего контекста: при отмене просто выходим, Respondent сам завершится по s.ctx.Done()
			select {
			case <-s.ctx.Done():
				//logger.Debug("StarterRespondent canceled by Start context %s", u.RespName)
				return
			default:
			}

			s.Respondent(u, questionCh, answerCh, fullQuestCh, respId, treadId, errCh)
			//logger.Debug("StarterRespondent: s.Respondent завершился для respId=%d", respId)
		}()
	}
}

// StarterListener запускает Listener для пользователя, если он ещё не запущен
func (s *Start) StarterListener(start model.StartCh, errCh chan<- error) {
	// Проверка на nil перед доступом к полям
	if start.Model == nil {
		errCh <- fmt.Errorf("start.Model is nil for respId %d", start.RespId)
		return
	}

	// Сохраняем provider для этого respId в карту для использования в CallOptional
	if start.Provider != "" {
		s.responderProviders.Store(start.RespId, start.Provider)
	}

	if !start.Model.Services.Listener.Load() {
		start.Model.Services.Listener.Store(true)
		go func() {
			defer func() {
				start.Model.Services.Listener.Store(false)
				//logger.Debug("[%s] StarterListener: Listener завершен для respId=%d", start.Provider, start.RespId, start.Model.Assist.UserID)
			}()
			// - родительского s.ctx (общий контекст Start)
			// - или контекста бота start.Ctx
			listenerCtx, listenerCancel := context.WithCancel(s.ctx)
			defer listenerCancel()

			// Связываем с контекстом бота
			go func() {
				select {
				case <-start.Ctx.Done():
					listenerCancel()
				case <-listenerCtx.Done():
				}
			}()

			// Если контекст бота уже отменён — не запускаем Listener
			select {
			case <-start.Ctx.Done():
				//logger.Debug("[%s] StarterListener отменён по контексту бота %s", start.Provider, start.Model.RespName, start.Model.Assist.UserID)
				return
			default:
			}

			if err := s.Listener(start.Model, start.Chanel, start.RespId, start.TreadId); err != nil {
				//logger.Error("[%s] StarterListener: ошибка в Listener для respId=%d: %v", start.Provider, start.RespId, err, start.Model.Assist.UserID)
				select {
				case errCh <- err: // Отправляем ошибку в App
				default:
					//logger.Warn("[%s] Не удалось отправить ошибку в errCh: %v", start.Provider, err, start.Model.Assist.UserID)
				}
			}
		}()
	} else {
		//logger.Debug("[%s] StarterListener: Listener уже запущен для respId=%d", start.Provider, start.RespId, start.Model.Assist.UserID)
	}
}

// saveTask — задание для воркера сохранения диалога.
// Использование единственной горутины-воркера гарантирует порядок записей:
// вопрос пользователя всегда сохраняется раньше ответа модели.
type saveTask struct {
	creator comdb.CreatorType
	treadId uint64
	resp    model.AssistResponse
}

// Listener слушает канал от пользователя и обрабатывает сообщения
func (s *Start) Listener(u *model.RespModel, usrCh *model.Ch, respId uint64, treadId uint64) error {
	// Сохраняем provider для этого respId (берем из StartCh через responderProviders)
	// Defer удалит его при завершении Listener
	defer s.responderProviders.Delete(respId)

	question := make(chan Question, create.RxChanBuffer)
	fullQuestCh := make(chan Answer, create.RxChanBuffer)
	answerCh := make(chan Answer, create.RxChanBuffer)
	errCh := make(chan error, create.RxChanBuffer)
	// saveCh: упорядоченная очередь для SaveDialog.
	// Единственный воркер читает из неё последовательно — порядок "вопрос → ответ" гарантирован.
	saveCh := make(chan saveTask, create.RxChanBuffer)

	// Создаем контекст для координированного завершения
	listenerCtx, listenerCancel := context.WithCancel(s.ctx)

	defer func() {
		//logger.Debug("Закрытие каналов в Listener")

		listenerCancel() // Отменяем контекст перед закрытием каналов

		// Ждем завершения Respondent перед закрытием каналов
		if wgInterface, ok := s.respondentWG.Load(treadId); ok {
			wg := wgInterface.(*sync.WaitGroup)

			// Ждем с таймаутом
			done := make(chan struct{})
			go func() {
				wg.Wait()
				close(done)
			}()

			select {
			case <-done:
				//logger.Debug("Respondent завершен, закрываем каналы")
			case <-time.After(5 * time.Second):
				//logger.Warn("Таймаут ожидания завершения Respondent")
			}
		}

		close(question)
		close(fullQuestCh)
		close(answerCh)
		close(errCh)
		// saveCh закрываем последним: воркер дочитает все оставшиеся задачи
		// и завершится корректно
		close(saveCh)
	}()

	// Запускаем воркер сохранения диалога.
	// Единственная горутина обеспечивает строгий порядок: вопрос всегда перед ответом.
	go func() {
		for t := range saveCh {
			s.End.SaveDialog(t.creator, t.treadId, &t.resp)
		}
	}()

	// Передаем контекст listener в модель пользователя
	userCtx, userCancel := context.WithCancel(listenerCtx)
	defer userCancel()

	// Обновляем контекст в модели пользователя
	u.Ctx = userCtx

	go s.StarterRespondent(u, question, answerCh, fullQuestCh, respId, treadId, errCh)

	for {
		select {
		case <-s.ctx.Done():
			//logger.Debug("Start context отменён в Listener %s", u.RespName)
			return nil
		case err := <-errCh:
			//logger.Error("Listener: получена ошибка из errCh: %v", err)
			return err // Возвращаем возможные ошибки
		case <-u.Ctx.Done():
			//logger.Debug("Context.Done Listener %s", u.RespName)
			return nil
		case msg, ok := <-usrCh.RxCh:
			if !ok {
				//logger.Debug("Канал RxCh закрыт %s", u.RespName)
				return nil
			}

			// Создаю вопрос
			var quest Question

			switch msg.Type {
			case "user":
				quest = Question{
					Question: strings.Split(msg.Content.Message, "\n"),
					Voice:    false,        // Сообщение от пользователя не голосовое
					Files:    msg.Files,    // Файлы, прикрепленные к вопросу
					Operator: msg.Operator, // Помечаем оператором при триггере или если уже отмечено
				}
			case "user_voice":
				quest = Question{
					Question: strings.Split(msg.Content.Message, "\n"),
					Voice:    true,         // Сообщение от пользователя голосовое
					Files:    msg.Files,    // Файлы, прикрепленные к вопросу
					Operator: msg.Operator, // Помечаем оператором при триггере или если уже отмечено
				}
			default:
				// Неизвестный тип сообщения, пропускаю
				//logger.Warn("Listener: неизвестный тип=%s", msg.Type)
				s.sendError(errCh, fmt.Errorf("неизвестный тип сообщения: %s", msg.Type))
				continue
			}

			// Защита от паники при отправке в questionCh
			select {
			case question <- quest:
				// Успешно отправлено в очередь
			case <-s.ctx.Done():
				//logger.Debug("Контекст отменен при отправке в questionCh")
				return fmt.Errorf("контекст отменен")
			case <-time.After(500 * time.Millisecond):
				// Редкий случай переполнения — тихо пропускаем
				// НЕ завершаем Listener — продолжаем работу
			}

			// Отправляю вопрос клиента в виде сообщения
			userMsg := s.Mod.NewMessage(msg.Operator, "user", &msg.Content, &msg.Name)

			if err := usrCh.SendToTx(userMsg); err != nil {
				select {
				case errCh <- fmt.Errorf("ошибка при отправке в канал TxCh: %v", err.Error()):
				default:
					//logger.Warn("Ошибка отправки ответа в TxCh для dialogID %d: %v", treadId, err)
				}
				continue
			}

		case quest := <-fullQuestCh: // Пришёл полный вопрос пользователя
			// Отправляем в воркер — он сохранит строго по порядку поступления
			creator := comdb.User
			if quest.VoiceQuestion {
				creator = comdb.UserVoice
			}
			select {
			case saveCh <- saveTask{creator: creator, treadId: treadId, resp: quest.Answer}:
			default:
				//logger.Warn("saveCh переполнен, вопрос пользователя не сохранён для dialogID %d", treadId)
			}
		case resp := <-answerCh: // Пришёл ответ ассистента/оператора
			assistMsg := s.Mod.NewMessage(resp.Operator, "assist", &resp.Answer, &u.Assist.AssistName)

			// Безопасная отправка ответа в TxCh
			if err := usrCh.SendToTx(assistMsg); err != nil {
				select {
				case errCh <- fmt.Errorf("ошибка при отправке в канал TxCh: %v", err.Error()):
				default:
					//logger.Warn("Ошибка отправки ответа в TxCh для dialogID %d: %v", treadId, err)
				}
				continue
			}

			// Если ответ содержит ошибку модели — уведомляем вызывающий код и не сохраняем в историю
			if resp.Err != nil {
				select {
				case errCh <- fmt.Errorf("модель не смогла ответить (dialogID=%d): %w", treadId, resp.Err):
				default:
				}
				continue
			}

			// Сохраняем через воркер — строго после вопроса (fullQuestCh был отправлен раньше)
			creator := comdb.AI
			if resp.Operator.Operator {
				creator = comdb.Operator
			}
			select {
			case saveCh <- saveTask{creator: creator, treadId: treadId, resp: resp.Answer}:
			default:
				//logger.Warn("saveCh переполнен, ответ ассистента не сохранён для dialogID %d", treadId)
			}
		}
	}
}

// GetProviderForResponder возвращает сохраненный provider для respId
// Возвращает provider и флаг найден ли он
func (s *Start) GetProviderForResponder(respId uint64) (string, bool) {
	if val, ok := s.responderProviders.Load(respId); ok {
		return val.(string), true
	}
	return "", false
}
