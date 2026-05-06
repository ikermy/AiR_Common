package openai

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/gorilla/websocket"
	"github.com/ikermy/AiR_Common/pkg/model"
	"github.com/ikermy/AiR_Common/pkg/model/create"
)

// RealtimeEvent — алиас типа из пакета model для удобства внутри пакета openai
type RealtimeEvent = model.RealtimeEvent

// ============================================================================
// REALTIME SESSION
// ============================================================================

// RealtimeSession — голосовая сессия через OpenAI Realtime API.
// Работает параллельно с текстовым режимом (RequestStreaming), не мешая ему.
type RealtimeSession struct {
	openaiConn  *websocket.Conn // WSS-соединение к OpenAI Realtime API
	ctx         context.Context // Контекст сессии — отменяется при CloseRealtimeSession
	cancel      context.CancelFunc
	agentConfig *OpenAIAgentConfig // Ссылка на конфиг агента (не копируется)
	userId      uint32
	dialogID    uint64 // treadId из TestSession — для сохранения транскрипции в БД
	respId      uint64

	// AudioIn/AudioOut/DrainPlayback — каналы с единственным читателем, fan-out не нужен.
	AudioIn       chan []byte   // PCM16 от клиента → pumpToOpenAI → OpenAI
	AudioOut      chan []byte   // PCM16 от OpenAI → хендлер → клиент
	DrainPlayback chan struct{} // сигнал: VAD speech_started → сбросить очередь воспроизведения

	// eventSubs: fan-out подписки на управляющие события.
	// WebSocket-клиент подписывается через SubscribeEvents() при коннекте
	// и отписывается через UnsubscribeEvents() при дисконнекте.
	// Telegram-звонок не подписывается → eventSubs пустой → publishEvent() — no-op.
	eventSubsMu sync.RWMutex
	eventSubs   []chan RealtimeEvent

	// Накопление транскрипций по itemId.
	// Карты нужны потому что транскрипция пользователя приходит ПОСЛЕ response.done,
	// а при cancelled response аудио уже накоплено и должно быть сохранено корректно.
	userTranscripts   sync.Map // itemId (string) → транскрипция пользователя (string)
	assistTranscripts sync.Map // itemId (string) → транскрипция ассистента (string)

	// writeMu защищает все записи в openaiConn (gorilla/websocket не thread-safe для concurrent writes).
	writeMu sync.Mutex

	// IsGenerating: true пока OpenAI генерирует ответ (response.created → response.done).
	IsGenerating atomic.Bool
	// greetingSent: true — приветствие при старте сессии уже отправлено
	greetingSent atomic.Bool

	// OnDisconnect — опциональный callback вызывается при закрытии сессии.
	// Используется для завершения звонка при критическом таймауте watchdog.
	// Получает respId сессии для очистки соответствующей callSession.
	OnDisconnect func(respId uint64) // может быть nil (WebSocket-клиент)

	// onDisconnectCalled: флаг для защиты от двойного вызова OnDisconnect callback
	// (например, из watchdog и из CloseRealtimeSession одновременно)
	onDisconnectCalled atomic.Bool
}

// publishEvent рассылает событие всем подписчикам неблокирующе.
// Если подписчиков нет (Telegram-звонок) — no-op, горутина не блокируется.
func (rs *RealtimeSession) publishEvent(ev RealtimeEvent) {
	rs.eventSubsMu.RLock()
	defer rs.eventSubsMu.RUnlock()
	for _, ch := range rs.eventSubs {
		select {
		case ch <- ev:
		default:
		}
	}
}

// ============================================================================
// МЕТОДЫ OpenAIModel (реализация интерфейса model.RealtimeProvider)
// ============================================================================

// GetRealtimeSession возвращает активную сессию по respId или nil.
func (m *OpenAIModel) GetRealtimeSession(respId uint64) *RealtimeSession {
	if val, ok := m.realtimeSessions.Load(respId); ok {
		return val.(*RealtimeSession)
	}
	return nil
}

// SubscribeEvents регистрирует нового подписчика на события сессии и возвращает его канал.
// Вызывается WebSocket-клиентом при подключении.
func (m *OpenAIModel) SubscribeEvents(respId uint64) (<-chan model.RealtimeEvent, error) {
	rs := m.GetRealtimeSession(respId)
	if rs == nil {
		return nil, fmt.Errorf("SubscribeEvents: сессия не найдена для respId=%d", respId)
	}
	ch := make(chan model.RealtimeEvent, 64)
	rs.eventSubsMu.Lock()
	rs.eventSubs = append(rs.eventSubs, ch)
	rs.eventSubsMu.Unlock()
	return ch, nil
}

// UnsubscribeEvents удаляет подписчика и закрывает его канал.
// Вызывается WebSocket-клиентом при отключении.
func (m *OpenAIModel) UnsubscribeEvents(respId uint64, sub <-chan model.RealtimeEvent) {
	rs := m.GetRealtimeSession(respId)
	if rs == nil {
		return
	}
	rs.eventSubsMu.Lock()
	defer rs.eventSubsMu.Unlock()
	for i, ch := range rs.eventSubs {
		if ch == sub {
			rs.eventSubs = append(rs.eventSubs[:i], rs.eventSubs[i+1:]...)
			// Закрываем канал безопасно — защита от паники если канал уже закрыт
			func() {
				defer func() {
					if r := recover(); r != nil {
						//logger.Debug("UnsubscribeEvents: close на закрытом канале respId=%d", respId)
					}
				}()
				close(ch)
			}()
			return
		}
	}
}

// GetRealtimeAudio реализует model.RealtimeProvider.
func (m *OpenAIModel) GetRealtimeAudio(respId uint64) (<-chan []byte, error) {
	rs := m.GetRealtimeSession(respId)
	if rs == nil {
		return nil, fmt.Errorf("GetRealtimeAudio: сессия не найдена для respId=%d", respId)
	}
	return rs.AudioOut, nil
}

// GetRealtimeDrain реализует model.RealtimeProvider.
func (m *OpenAIModel) GetRealtimeDrain(respId uint64) (<-chan struct{}, error) {
	rs := m.GetRealtimeSession(respId)
	if rs == nil {
		return nil, fmt.Errorf("GetRealtimeDrain: сессия не найдена для respId=%d", respId)
	}
	return rs.DrainPlayback, nil
}

// GetRealtimeGenerating возвращает указатель на IsGenerating флаг сессии.
// true — OpenAI сейчас генерирует ответ (response.created → response.done).
func (m *OpenAIModel) GetRealtimeGenerating(respId uint64) *atomic.Bool {
	rs := m.GetRealtimeSession(respId)
	if rs == nil {
		return nil
	}
	return &rs.IsGenerating
}

// StartRealtimeSession создаёт WSS-соединение к OpenAI Realtime API и запускает pump-горутины.
// Вызывается после GetOrSetRespGPT — RespModel уже должен существовать в m.responders.
func (m *OpenAIModel) StartRealtimeSession(userId uint32, dialogID, respId uint64) error {
	if existing := m.GetRealtimeSession(respId); existing != nil {
		//logger.Debug("StartRealtimeSession: сессия уже существует для respId=%d", respId, userId)
		return nil
	}

	val, ok := m.responders.Load(respId)
	if !ok {
		return fmt.Errorf("StartRealtimeSession: RespModel не найден для respId=%d", respId)
	}
	rm := val.(*RespModel)
	if rm.AgentConfig == nil {
		return fmt.Errorf("StartRealtimeSession: AgentConfig не загружен для respId=%d", respId)
	}

	if !rm.AgentConfig.RealtimeEnabled {
		return fmt.Errorf("StartRealtimeSession: Realtime не включён для userId=%d (установите флаг Realtime в настройках модели)", userId)
	}

	conn, err := create.DialRealtimeSession(m.client.GetAPIKey(), rm.AgentConfig.RealtimeModel)
	if err != nil {
		return fmt.Errorf("StartRealtimeSession: ошибка подключения к OpenAI Realtime API: %w", err)
	}

	ctx, cancel := context.WithCancel(m.ctx)

	rs := &RealtimeSession{
		openaiConn:    conn,
		ctx:           ctx,
		cancel:        cancel,
		agentConfig:   rm.AgentConfig,
		userId:        userId,
		dialogID:      dialogID,
		respId:        respId,
		AudioIn:       make(chan []byte, 256),
		AudioOut:      make(chan []byte, 256),
		DrainPlayback: make(chan struct{}, 1),
	}

	if err := m.sendSessionUpdate(rs); err != nil {
		cancel()
		_ = conn.Close()
		return fmt.Errorf("StartRealtimeSession: ошибка session.update: %w", err)
	}

	// Инжектируем историю диалога — realtime-агент знает контекст предыдущих разговоров.
	if err := m.injectDialogHistory(rs, dialogID); err != nil {
		//logger.Warn("StartRealtimeSession: не удалось инжектировать историю диалога: %v respId=%d", err, respId, userId)
		// Не критично — продолжаем без истории
	}

	m.realtimeSessions.Store(respId, rs)
	//logger.Info("StartRealtimeSession: голосовая сессия запущена respId=%d model=%s",
	//	respId, rm.AgentConfig.RealtimeModel, userId)

	// Горутина-сторож: закрывает WS при отмене контекста → разблокирует ReadMessage()
	go func() {
		<-rs.ctx.Done()
		_ = rs.openaiConn.Close()
	}()

	go m.pumpFromOpenAI(rs)
	go m.pumpToOpenAI(rs)

	return nil
}

// injectDialogHistory загружает историю диалога из dialogCache (или БД) и отправляет
// в Realtime API как conversation.item.create — до первого голосового сообщения.
// Лимит: DialogHistoryLimit/2 (вдвое меньше чем для текстового режима).
func (m *OpenAIModel) injectDialogHistory(rs *RealtimeSession, dialogID uint64) error {
	maxInject := int(create.DialogHistoryLimit) / 2 // = 10

	// Берём из кэша (предзагружен в preloadDialogHistoryIfNeeded при GetOrSetRespGPT)
	history, found := m.getDialogHistoryFromCache(dialogID)
	if !found || len(history) == 0 {
		// Кэш пуст — синхронно загружаем из БД
		dbHistory, err := m.ConvertDialogToOpenAIFormat(dialogID)
		if err != nil || len(dbHistory) == 0 {
			//logger.Debug("injectDialogHistory: история пуста или не найдена для dialogID=%d", dialogID, rs.userId)
			return nil
		}
		if len(dbHistory) > int(create.DialogHistoryLimit) {
			dbHistory = dbHistory[len(dbHistory)-int(create.DialogHistoryLimit):]
		}
		history = dbHistory
		cache := m.getOrCreateDialogCache(dialogID)
		cache.Messages = history
	}

	if len(history) == 0 {
		return nil
	}

	// Берём последние maxInject сообщений
	if len(history) > maxInject {
		history = history[len(history)-maxInject:]
	}

	//logger.Info("injectDialogHistory: инжектируем %d сообщений dialogID=%d respId=%d",
	//	len(history), dialogID, rs.respId, rs.userId)

	for _, msg := range history {
		if msg.Content == "" {
			continue
		}
		role := msg.Role
		if role != "user" && role != "assistant" {
			continue
		}
		// user → "input_text", assistant → "text"
		contentType := "text"
		if role == "user" {
			contentType = "input_text"
		}
		item := map[string]interface{}{
			"type": "conversation.item.create",
			"item": map[string]interface{}{
				"type": "message",
				"role": role,
				"content": []map[string]interface{}{
					{"type": contentType, "text": msg.Content},
				},
			},
		}
		if err := rs.writeJSON(item); err != nil {
			return fmt.Errorf("injectDialogHistory: ошибка записи role=%s: %w", role, err)
		}
	}

	//logger.Debug("injectDialogHistory: завершено %d сообщений dialogID=%d", len(history), dialogID, rs.userId)
	return nil
}

// CloseRealtimeSession завершает голосовую сессию. Текстовый режим не затрагивается.
// Очищает transcripts и eventSubs, вызывает OnDisconnect callback если установлен (только один раз).
func (m *OpenAIModel) CloseRealtimeSession(respId uint64) {
	val, ok := m.realtimeSessions.LoadAndDelete(respId)
	if !ok {
		return
	}
	rs := val.(*RealtimeSession)

	// Очищаем transcripts для предотвращения утечки памяти (sync.Map не требует явной очистки,
	// но удаляем значения для гарантированного освобождения памяти)
	rs.userTranscripts.Range(func(key, value interface{}) bool {
		rs.userTranscripts.Delete(key)
		return true
	})
	rs.assistTranscripts.Range(func(key, value interface{}) bool {
		rs.assistTranscripts.Delete(key)
		return true
	})

	// Закрываем все каналы подписчиков для предотвращения утечки и паники при publish
	rs.eventSubsMu.Lock()
	for _, ch := range rs.eventSubs {
		// Закрываем канал безопасно
		func(c chan RealtimeEvent) {
			defer func() {
				if r := recover(); r != nil {
					//logger.Debug("CloseRealtimeSession: close на закрытом канале respId=%d", respId)
				}
			}()
			close(c)
		}(ch)
	}
	rs.eventSubs = nil
	rs.eventSubsMu.Unlock()

	// Помечаем сессию как завершённую нормально — watchdog таймеры НЕ должны
	// вызывать OnDisconnect callback после этой точки.
	// OnDisconnect предназначен ТОЛЬКО для аварийного завершения из watchdog.
	// При нормальном завершении (пользователь вешает трубку) его вызывать не нужно.
	rs.onDisconnectCalled.Store(true)

	rs.cancel()
	_ = rs.openaiConn.Close()
	//logger.Info("CloseRealtimeSession: сессия закрыта respId=%d", respId, rs.userId)
}

// SendRealtimeAudio ставит PCM16-чанк от клиента в очередь pumpToOpenAI.
func (m *OpenAIModel) SendRealtimeAudio(respId uint64, pcm16 []byte) error {
	rs := m.GetRealtimeSession(respId)
	if rs == nil {
		return fmt.Errorf("SendRealtimeAudio: сессия не найдена для respId=%d", respId)
	}
	select {
	case rs.AudioIn <- pcm16:
		return nil
	case <-rs.ctx.Done():
		return fmt.Errorf("SendRealtimeAudio: сессия завершена для respId=%d", respId)
	default:
		//logger.Warn("SendRealtimeAudio: буфер AudioIn переполнен respId=%d, дроп %d байт",
		//	respId, len(pcm16), rs.userId)
		return nil
	}
}

// SetRealtimeDisconnectCallback устанавливает callback вызываемый при критическом таймауте watchdog.
// Используется для завершения звонка (Telegram) при том что модель совсем не отвечает.
func (m *OpenAIModel) SetRealtimeDisconnectCallback(respId uint64, callback func(respId uint64)) error {
	rs := m.GetRealtimeSession(respId)
	if rs == nil {
		return fmt.Errorf("SetRealtimeDisconnectCallback: сессия не найдена для respId=%d", respId)
	}
	rs.OnDisconnect = callback
	return nil
}

// ============================================================================
// ВСПОМОГАТЕЛЬНЫЕ МЕТОДЫ
// ============================================================================

// sendSessionUpdate отправляет session.update в OpenAI Realtime API.
func (m *OpenAIModel) sendSessionUpdate(rs *RealtimeSession) error {
	// Промпт строится на лету из SystemPrompt
	instructions := buildRealtimeSystemPrompt(rs.agentConfig)

	//logger.Debug("sendSessionUpdate: agentConfig.Tools raw count=%d respId=%d", len(rs.agentConfig.Tools), rs.respId, rs.userId)
	tools := buildRealtimeTools(rs.agentConfig.Tools, rs.agentConfig)
	//logger.Warn("sendSessionUpdate: Tools after convert count=%d list=%v respId=%d", len(tools), tools, rs.respId, rs.userId)
	// Собираем turn_detection из RealtimeVAD или используем дефолты
	vad := rs.agentConfig.RealtimeVAD
	threshold := 0.5
	prefixPaddingMs := 200
	silenceDurationMs := 500
	interruptResponse := true
	voice := "verse" // дефолтный голос
	if vad != nil {
		if vad.Threshold != nil {
			threshold = *vad.Threshold
		}
		if vad.PrefixPaddingMs != nil {
			prefixPaddingMs = *vad.PrefixPaddingMs
		}
		if vad.SilenceDurationMs != nil {
			silenceDurationMs = *vad.SilenceDurationMs
		}
		if vad.InterruptResponse != nil {
			interruptResponse = *vad.InterruptResponse
		}
		if vad.Voice != nil && *vad.Voice != "" {
			voice = *vad.Voice
		}
	}

	// Собираем параметры сессии (ТОЛЬКО настройки среды)
	// ВАЖНО: temperature и max_response_output_tokens передаются ТОЛЬКО здесь (session.update),
	// их нельзя передавать в response.create — будет ошибка unknown_parameter.
	sessionMap := map[string]interface{}{
		"modalities":          []string{"text", "audio"},
		"instructions":        instructions,
		"voice":               voice,
		"input_audio_format":  "pcm16",
		"output_audio_format": "pcm16",
		"turn_detection": map[string]interface{}{
			"type":                "server_vad",
			"threshold":           threshold,
			"prefix_padding_ms":   prefixPaddingMs,
			"silence_duration_ms": silenceDurationMs,
			"create_response":     true,
			"interrupt_response":  interruptResponse,
		},
		"tools":       tools,
		"tool_choice": "auto",
	}

	// input_audio_transcription — транскрипция входящего аудио (дефолт true)
	inputAudioTranscription := true
	if vad != nil && vad.InputAudioTranscription != nil {
		inputAudioTranscription = *vad.InputAudioTranscription
	}
	if inputAudioTranscription {
		sessionMap["input_audio_transcription"] = map[string]interface{}{
			"model": "whisper-1",
		}
	}

	// Параметры генерации из RealtimeVAD (только если заданы)
	if vad != nil {
		if vad.Temperature != nil {
			sessionMap["temperature"] = *vad.Temperature
		}
		if vad.MaxResponseOutputTokens != nil {
			sessionMap["max_response_output_tokens"] = *vad.MaxResponseOutputTokens
		}
	}

	event := map[string]interface{}{
		"type":    "session.update",
		"session": sessionMap,
	}

	data, err := json.Marshal(event)
	if err != nil {
		return fmt.Errorf("sendSessionUpdate: ошибка сериализации: %w", err)
	}

	rs.writeMu.Lock()
	writeErr := rs.openaiConn.WriteMessage(websocket.TextMessage, data)
	rs.writeMu.Unlock()
	if writeErr != nil {
		return fmt.Errorf("sendSessionUpdate: ошибка отправки: %w", writeErr)
	}

	//logger.Info("sendSessionUpdate: отправлено respId=%d voice=%s tools=%d instructionsLen=%d threshold=%.2f silence=%dms",
	//	rs.respId, voice, len(tools), len(instructions), threshold, silenceDurationMs, rs.userId)
	return nil
}

// writeJSON сериализует v и отправляет как TextMessage через openaiConn.
// Все записи в openaiConn обязаны идти через этот метод — он держит writeMu.
func (rs *RealtimeSession) writeJSON(v interface{}) error {
	data, err := json.Marshal(v)
	if err != nil {
		return err
	}
	rs.writeMu.Lock()
	defer rs.writeMu.Unlock()
	return rs.openaiConn.WriteMessage(websocket.TextMessage, data)
}

// buildRealtimeTools конвертирует tools из формата Responses API в формат Realtime API.
// Поддерживаются только function-инструменты.
// create_file исключается — в голосовом режиме файлы просматриваются только через get_s3_files.
//
// Различия Realtime API от Responses API:
//   - "strict" и "additionalProperties" не поддерживаются → удаляются
//   - "const" в properties не поддерживается → заменяем на description "MUST be exactly: ..."
//   - union types ["string","null"] не поддерживаются → берём первый тип
func buildRealtimeTools(tools []interface{}, agentConf *OpenAIAgentConfig) []interface{} {
	var result []interface{}
	for _, t := range tools {
		toolMap, ok := t.(map[string]interface{})
		if !ok || toolMap["type"] != "function" {
			continue
		}
		name, _ := toolMap["name"].(string)

		if name == "save_image_data" && agentConf != nil && !agentConf.Image {
			continue
		}
		// TODO сознательно отключён, нужно больше тестов!
		if name == "create_file" {
			continue
		}

		description, _ := toolMap["description"].(string)
		if name == "get_s3_files" {
			description = "Get the full list of user's files with their exact URLs. " +
				"Returns JSON with file names and full URLs. " +
				"After getting the list — call send_file_to_user using the EXACT URL from this response. " +
				"NEVER invent or modify URLs."
		}

		parameters := toolMap["parameters"]
		if paramsMap, ok := parameters.(map[string]interface{}); ok {
			paramsCopy := copyMapDeep(paramsMap)
			// Realtime API не поддерживает "additionalProperties"
			delete(paramsCopy, "additionalProperties")
			if props, ok := paramsCopy["properties"].(map[string]interface{}); ok {
				for propName, propRaw := range props {
					prop, ok := propRaw.(map[string]interface{})
					if !ok {
						continue
					}
					// Конвертируем "const" → description "MUST be exactly: ..."
					if propName == "user_id" {
						if constVal, ok := prop["const"].(string); ok && constVal != "" {
							delete(prop, "const")
							prop["type"] = "string"
							prop["description"] = fmt.Sprintf("MUST be exactly: %s", constVal)
						}
					}
					// Конвертируем union type ["string","null"] → "string"
					// Realtime API принимает только скалярный "type"
					if typeArr, ok := prop["type"].([]interface{}); ok && len(typeArr) > 0 {
						prop["type"] = typeArr[0]
					}
					if typeArr, ok := prop["type"].([]string); ok && len(typeArr) > 0 {
						prop["type"] = typeArr[0]
					}
					props[propName] = prop
				}
			}
			parameters = paramsCopy
		}

		result = append(result, map[string]interface{}{
			"type":        "function",
			"name":        name,
			"description": description,
			"parameters":  parameters,
		})
	}

	// Если S3 включён — добавляем синтетический tool send_file_to_user.
	// Модель вызывает его с конкретным URL из результата get_s3_files.
	// Это позволяет модели самой выбрать нужный файл, а не отправлять все сразу.
	if agentConf != nil && agentConf.S3 {
		// Извлекаем user_id из существующего get_s3_files tool чтобы подставить const
		userID := ""
		for _, t := range tools {
			tm, ok := t.(map[string]interface{})
			if !ok {
				continue
			}
			fn, _ := tm["name"].(string)
			if fn != "get_s3_files" {
				continue
			}
			if params, ok := tm["parameters"].(map[string]interface{}); ok {
				if props, ok := params["properties"].(map[string]interface{}); ok {
					if uidProp, ok := props["user_id"].(map[string]interface{}); ok {
						userID, _ = uidProp["const"].(string)
						// После buildRealtimeTools const уже заменён на description — ищем иначе
						if userID == "" {
							if desc, ok := uidProp["description"].(string); ok {
								userID = strings.TrimPrefix(desc, "MUST be exactly: ")
							}
						}
					}
				}
			}
		}

		result = append(result, map[string]interface{}{
			"type":        "function",
			"name":        "send_file_to_user",
			"description": "Send a specific file to the user. Call this after get_s3_files to send a file the user requested. Use the exact URL from get_s3_files result.",
			"parameters": map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"user_id": map[string]interface{}{
						"type":        "string",
						"description": fmt.Sprintf("MUST be exactly: %s", userID),
					},
					"url": map[string]interface{}{
						"type":        "string",
						"description": "Exact URL of the file from get_s3_files result",
					},
					"file_name": map[string]interface{}{
						"type":        "string",
						"description": "File name as returned by get_s3_files",
					},
				},
				"required": []string{"user_id", "url", "file_name"},
			},
		})
	}

	return result
}

// copyMapDeep делает глубокую копию map[string]interface{} для безопасного изменения.
func copyMapDeep(m map[string]interface{}) map[string]interface{} {
	result := make(map[string]interface{}, len(m))
	for k, v := range m {
		switch val := v.(type) {
		case map[string]interface{}:
			result[k] = copyMapDeep(val)
		case []interface{}:
			cp := make([]interface{}, len(val))
			copy(cp, val)
			result[k] = cp
		default:
			result[k] = v
		}
	}
	return result
}

// sendInitialGreeting отправляет response.create сразу после session.updated —
// модель произносит приветственную фразу не дожидаясь голоса пользователя.
func (m *OpenAIModel) sendInitialGreeting(rs *RealtimeSession) {
	if !rs.greetingSent.CompareAndSwap(false, true) {
		return // уже отправлено
	}

	// Проверяем параметр InitialGreeting из конфига
	if rs.agentConfig != nil && rs.agentConfig.RealtimeVAD != nil {
		if rs.agentConfig.RealtimeVAD.InitialGreeting != nil && !*rs.agentConfig.RealtimeVAD.InitialGreeting {
			//logger.Debug("sendInitialGreeting: приветствие отключено в конфиге respId=%d", rs.respId, rs.userId)
			return
		}
	}

	hasExplicitGreeting := rs.agentConfig != nil &&
		rs.agentConfig.RealtimeVAD != nil &&
		rs.agentConfig.RealtimeVAD.Greeting != nil &&
		*rs.agentConfig.RealtimeVAD.Greeting != ""

	var event map[string]interface{}

	if hasExplicitGreeting {
		greetingText := *rs.agentConfig.RealtimeVAD.Greeting
		//logger.Debug("sendInitialGreeting: явная фраза %q respId=%d", greetingText, rs.respId, rs.userId)
		// Передаём фразу как instructions — модель произносит её как свою первую реплику.
		// НЕ используем input с командой "Say EXACTLY..." — иначе модель комментирует задание.
		event = map[string]interface{}{
			"type": "response.create",
			"response": map[string]interface{}{
				"modalities":   []string{"text", "audio"},
				"instructions": "Your ONLY output is this exact phrase, nothing else, no commentary: " + greetingText,
			},
		}
	} else {
		// Нет явной фразы — модель генерирует приветствие сама по инструкции
		event = map[string]interface{}{
			"type": "response.create",
			"response": map[string]interface{}{
				"modalities": []string{"text", "audio"},
				"instructions": "Greet the user warmly and briefly (1-2 sentences). " +
					"Introduce yourself by name if you have one. " +
					"Ask how you can help. Speak naturally, no JSON.",
			},
		}
	}

	if err := rs.writeJSON(event); err != nil {
		//logger.Warn("sendInitialGreeting: ошибка отправки: %v respId=%d", err, rs.respId, rs.userId)
		rs.greetingSent.Store(false)
		//} else {
		//	logger.Info("sendInitialGreeting: приветствие отправлено respId=%d", rs.respId, rs.userId)
	}
}
