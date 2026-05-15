package google

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"sync/atomic"

	"github.com/gorilla/websocket"
	"github.com/ikermy/AiR_Common/pkg/model"
	"github.com/ikermy/AiR_Common/pkg/model/create"
)

// ============================================================================
// GOOGLE REALTIME SESSION
// ============================================================================

// GoogleRealtimeSession — голосовая сессия через Google Multimodal Live API.
// Работает параллельно с текстовым режимом (RequestStreaming), не мешая ему.
type GoogleRealtimeSession struct {
	googleConn  *websocket.Conn    // WSS-соединение к Google Multimodal Live API
	ctx         context.Context    // Контекст сессии — отменяется при CloseRealtimeSession
	cancel      context.CancelFunc
	agentConfig *GoogleAgentConfig // Ссылка на конфиг агента (не копируется)
	userID      uint32
	dialogID    uint64
	respId      uint64

	// AudioIn/AudioOut/DrainPlayback — каналы с единственным читателем.
	AudioIn       chan []byte   // PCM16 @ 16kHz от клиента → pumpToGoogle
	AudioOut      chan []byte   // PCM16 @ 24kHz от Google → хендлер → клиент
	DrainPlayback chan struct{} // сигнал: interrupted → сбросить очередь воспроизведения

	// eventSubs: fan-out подписки на управляющие события.
	eventSubsMu sync.RWMutex
	eventSubs   []chan model.RealtimeEvent

	// writeMu защищает все записи в googleConn (gorilla/websocket не thread-safe для concurrent writes).
	writeMu sync.Mutex

	// IsGenerating: true пока Google генерирует ответ (modelTurn → turnComplete).
	IsGenerating atomic.Bool
	// greetingSent: true — приветствие при старте сессии уже отправлено
	greetingSent atomic.Bool

	// OnDisconnect — опциональный callback при аварийном закрытии сессии.
	OnDisconnect       func(respId uint64)
	onDisconnectCalled atomic.Bool
}

// publishEvent рассылает событие всем подписчикам неблокирующе.
func (rs *GoogleRealtimeSession) publishEvent(ev model.RealtimeEvent) {
	rs.eventSubsMu.RLock()
	defer rs.eventSubsMu.RUnlock()
	for _, ch := range rs.eventSubs {
		select {
		case ch <- ev:
		default:
		}
	}
}

// writeJSON сериализует v и отправляет как TextMessage через googleConn.
// Все записи в googleConn обязаны идти через этот метод — он держит writeMu.
func (rs *GoogleRealtimeSession) writeJSON(v interface{}) error {
	data, err := json.Marshal(v)
	if err != nil {
		return err
	}
	rs.writeMu.Lock()
	defer rs.writeMu.Unlock()
	return rs.googleConn.WriteMessage(websocket.TextMessage, data)
}

// ============================================================================
// МЕТОДЫ Model (реализация интерфейса model.RealtimeProvider)
// ============================================================================

// GetGoogleRealtimeSession возвращает активную сессию по respId или nil.
func (m *Model) GetGoogleRealtimeSession(respId uint64) *GoogleRealtimeSession {
	if val, ok := m.realtimeSessions.Load(respId); ok {
		return val.(*GoogleRealtimeSession)
	}
	return nil
}

// SubscribeEvents регистрирует нового подписчика на события сессии и возвращает его канал.
func (m *Model) SubscribeEvents(respId uint64) (<-chan model.RealtimeEvent, error) {
	rs := m.GetGoogleRealtimeSession(respId)
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
func (m *Model) UnsubscribeEvents(respId uint64, sub <-chan model.RealtimeEvent) {
	rs := m.GetGoogleRealtimeSession(respId)
	if rs == nil {
		return
	}
	rs.eventSubsMu.Lock()
	defer rs.eventSubsMu.Unlock()
	for i, ch := range rs.eventSubs {
		if ch == sub {
			rs.eventSubs = append(rs.eventSubs[:i], rs.eventSubs[i+1:]...)
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
func (m *Model) GetRealtimeAudio(respId uint64) (<-chan []byte, error) {
	rs := m.GetGoogleRealtimeSession(respId)
	if rs == nil {
		return nil, fmt.Errorf("GetRealtimeAudio: сессия не найдена для respId=%d", respId)
	}
	return rs.AudioOut, nil
}

// GetRealtimeDrain реализует model.RealtimeProvider.
func (m *Model) GetRealtimeDrain(respId uint64) (<-chan struct{}, error) {
	rs := m.GetGoogleRealtimeSession(respId)
	if rs == nil {
		return nil, fmt.Errorf("GetRealtimeDrain: сессия не найдена для respId=%d", respId)
	}
	return rs.DrainPlayback, nil
}

// GetRealtimeGenerating возвращает указатель на IsGenerating флаг сессии.
func (m *Model) GetRealtimeGenerating(respId uint64) *atomic.Bool {
	rs := m.GetGoogleRealtimeSession(respId)
	if rs == nil {
		return nil
	}
	return &rs.IsGenerating
}

// StartRealtimeSession создаёт WSS-соединение к Google Multimodal Live API и запускает pump-горутины.
func (m *Model) StartRealtimeSession(userID uint32, dialogID, respId uint64) error {
	if existing := m.GetGoogleRealtimeSession(respId); existing != nil {
		//logger.Debug("StartRealtimeSession: сессия уже существует для respId=%d", respId, userID)
		return nil
	}

	val, ok := m.responders.Load(respId)
	if !ok {
		return fmt.Errorf("StartRealtimeSession: RespModel не найден для respId=%d", respId)
	}
	rm := val.(*GoogleRespModel)
	if rm.AgentConfig == nil {
		return fmt.Errorf("StartRealtimeSession: AgentConfig не загружен для respId=%d", respId)
	}

	if !rm.AgentConfig.RealtimeEnabled {
		return fmt.Errorf("StartRealtimeSession: Realtime не включён для userID=%d (установите флаг Realtime в настройках модели)", userID)
	}

	conn, err := create.DialGoogleRealtimeSession(m.client.GetAPIKey(), rm.AgentConfig.ModelName)
	if err != nil {
		return fmt.Errorf("StartRealtimeSession: ошибка подключения к Google Live API: %w", err)
	}

	ctx, cancel := context.WithCancel(m.ctx)

	rs := &GoogleRealtimeSession{
		googleConn:    conn,
		ctx:           ctx,
		cancel:        cancel,
		agentConfig:   rm.AgentConfig,
		userID:        userID,
		dialogID:      dialogID,
		respId:        respId,
		AudioIn:       make(chan []byte, 256),
		AudioOut:      make(chan []byte, 256),
		DrainPlayback: make(chan struct{}, 1),
	}

	// Отправляем setup-сообщение (первое сообщение в сессии Google Live API)
	if err := m.sendGoogleSetup(rs); err != nil {
		cancel()
		_ = conn.Close()
		return fmt.Errorf("StartRealtimeSession: ошибка setup: %w", err)
	}

	// Инжектируем историю диалога — агент знает контекст предыдущих разговоров.
	if err := m.injectGoogleDialogHistory(rs, dialogID); err != nil {
		//logger.Warn("StartRealtimeSession: не удалось инжектировать историю диалога: %v respId=%d", err, respId, userID)
		// Не критично — продолжаем без истории
	}

	m.realtimeSessions.Store(respId, rs)
	//logger.Info("StartRealtimeSession: голосовая сессия запущена respId=%d model=%s",
	//	respId, rm.AgentConfig.ModelName, userID)

	// Горутина-сторож: закрывает WS при отмене контекста → разблокирует ReadMessage()
	go func() {
		<-rs.ctx.Done()
		_ = rs.googleConn.Close()
	}()

	go m.pumpFromGoogle(rs)
	go m.pumpToGoogle(rs)

	return nil
}

// CloseRealtimeSession завершает голосовую сессию. Текстовый режим не затрагивается.
func (m *Model) CloseRealtimeSession(respId uint64) {
	val, ok := m.realtimeSessions.LoadAndDelete(respId)
	if !ok {
		return
	}
	rs := val.(*GoogleRealtimeSession)

	// Закрываем все каналы подписчиков
	rs.eventSubsMu.Lock()
	for _, ch := range rs.eventSubs {
		func(c chan model.RealtimeEvent) {
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

	// Помечаем сессию как завершённую нормально — watchdog НЕ должен вызывать OnDisconnect.
	rs.onDisconnectCalled.Store(true)
	rs.cancel()
	_ = rs.googleConn.Close()
	//logger.Info("CloseRealtimeSession: сессия закрыта respId=%d", respId, rs.userID)
}

// SendRealtimeAudio ставит PCM16-чанк от клиента в очередь pumpToGoogle.
func (m *Model) SendRealtimeAudio(respId uint64, pcm16 []byte) error {
	rs := m.GetGoogleRealtimeSession(respId)
	if rs == nil {
		return fmt.Errorf("SendRealtimeAudio: сессия не найдена для respId=%d", respId)
	}
	select {
	case rs.AudioIn <- pcm16:
		return nil
	case <-rs.ctx.Done():
		return fmt.Errorf("SendRealtimeAudio: сессия завершена для respId=%d", respId)
	default:
		//logger.Warn("SendRealtimeAudio: буфер AudioIn переполнен respId=%d, дроп %d байт", respId, len(pcm16), rs.userID)
		return nil
	}
}

// SetRealtimeDisconnectCallback устанавливает callback вызываемый при аварийном завершении сессии.
func (m *Model) SetRealtimeDisconnectCallback(respId uint64, callback func(respId uint64)) error {
	rs := m.GetGoogleRealtimeSession(respId)
	if rs == nil {
		return fmt.Errorf("SetRealtimeDisconnectCallback: сессия не найдена для respId=%d", respId)
	}
	rs.OnDisconnect = callback
	return nil
}

// ============================================================================
// ВСПОМОГАТЕЛЬНЫЕ МЕТОДЫ
// ============================================================================

// sendGoogleSetup отправляет setup-сообщение Google Multimodal Live API.
// Это первое и единственное системное сообщение в начале сессии.
//
// Приоритет значений (от высокого к низкому):
//  1. RealtimeVAD.Google.* — Google-специфичные поля
//  2. RealtimeVAD.* — общие поля (Voice, SilenceDurationMs, InterruptResponse, Temperature, ...)
//  3. Глобальные константы (GoogleRealtimeDefaultVoice, GoogleRealtimeSilenceDurationMs, ...)
func (m *Model) sendGoogleSetup(rs *GoogleRealtimeSession) error {
	cfg := rs.agentConfig
	vad := cfg.RealtimeVAD      // может быть nil
	var gvad *create.GoogleRealtimeVAD // Google-специфичный блок, может быть nil
	if vad != nil {
		gvad = vad.Google
	}

	// ── Голос: Google.VoiceName > Voice > дефолт ─────────────────────────
	voice := create.GoogleRealtimeDefaultVoice
	if gvad != nil && gvad.VoiceName != nil && *gvad.VoiceName != "" {
		voice = *gvad.VoiceName
	} else if vad != nil && vad.Voice != nil && *vad.Voice != "" {
		voice = *vad.Voice
	}

	// ── Язык (только Google) ─────────────────────────────────────────────
	var languageCode string
	if gvad != nil && gvad.LanguageCode != nil {
		languageCode = *gvad.LanguageCode
	}

	// ── Температура: Google нет своего → берём общий ─────────────────────
	temperature := create.RealtimeTemperature
	if vad != nil && vad.Temperature != nil {
		temperature = *vad.Temperature
	}

	// ── speechConfig ─────────────────────────────────────────────────────
	prebuiltVoiceConfig := map[string]interface{}{
		"voiceName": voice,
	}
	voiceConfig := map[string]interface{}{
		"prebuiltVoiceConfig": prebuiltVoiceConfig,
	}
	speechConfig := map[string]interface{}{
		"voiceConfig": voiceConfig,
	}
	if languageCode != "" {
		speechConfig["languageCode"] = languageCode
	}

	generationConfig := map[string]interface{}{
		// TEXT + AUDIO: модель возвращает аудио (воспроизведение) и текст (история диалога).
		"responseModalities": []string{"TEXT", "AUDIO"},
		"temperature":        temperature,
		"speechConfig":       speechConfig,
	}

	// ── VAD: silenceDurationMs — Google.SilenceDurationMs > SilenceDurationMs > константа ──
	silenceDurationMs := create.GoogleRealtimeSilenceDurationMs
	if gvad != nil && gvad.SilenceDurationMs != nil {
		silenceDurationMs = *gvad.SilenceDurationMs
	} else if vad != nil && vad.SilenceDurationMs != nil {
		silenceDurationMs = *vad.SilenceDurationMs
	}

	// ── BargeIn (прерывание): Google.BargeIn > InterruptResponse > true ──
	bargeIn := true
	if gvad != nil && gvad.BargeIn != nil {
		bargeIn = *gvad.BargeIn
	} else if vad != nil && vad.InterruptResponse != nil {
		bargeIn = *vad.InterruptResponse
	}

	// ── AutomaticActivityDetection: Google.AutomaticActivityDetection > true ──
	autoVAD := true
	if gvad != nil && gvad.AutomaticActivityDetection != nil {
		autoVAD = *gvad.AutomaticActivityDetection
	}

	var realtimeInputConfig map[string]interface{}
	if autoVAD {
		realtimeInputConfig = map[string]interface{}{
			"automaticActivityDetection": map[string]interface{}{
				"silenceDurationMs": silenceDurationMs,
				"disabled":          !bargeIn,
			},
		}
	} else {
		// Push-to-talk: VAD полностью отключён
		realtimeInputConfig = map[string]interface{}{
			"automaticActivityDetection": map[string]interface{}{
				"disabled": true,
			},
		}
	}

	// ── Транскрипция входящей речи: Google.InputAudioTranscription > InputAudioTranscription > true ──
	inputTranscription := true
	if gvad != nil && gvad.InputAudioTranscription != nil {
		inputTranscription = *gvad.InputAudioTranscription
	} else if vad != nil && vad.InputAudioTranscription != nil {
		inputTranscription = *vad.InputAudioTranscription
	}

	// ── Транскрипция исходящей речи (только Google): Google.OutputAudioTranscription > false ──
	// ВАЖНО: outputAudioTranscription включается всегда — нужен для сохранения текста
	// ответа модели в историю диалога. outputAudioTranscription=false отключает
	// отдельные события, но для сохранения в БД мы уже используем TEXT modality.
	outputTranscription := false
	if gvad != nil && gvad.OutputAudioTranscription != nil {
		outputTranscription = *gvad.OutputAudioTranscription
	}

	setup := map[string]interface{}{
		"model":               fmt.Sprintf("models/%s", cfg.ModelName),
		"generationConfig":    generationConfig,
		"realtimeInputConfig": realtimeInputConfig,
	}

	// inputAudioTranscription — включаем если нужна STT для пользователя
	if inputTranscription {
		setup["inputAudioTranscription"] = map[string]interface{}{}
	}

	// outputAudioTranscription — включаем только если явно запрошено (субтитры)
	if outputTranscription {
		setup["outputAudioTranscription"] = map[string]interface{}{}
	}

	// System instruction из конфига агента
	if cfg.SystemInstruction != nil {
		setup["systemInstruction"] = cfg.SystemInstruction
	}

	// Tools (function_declarations, google_search и т.д.)
	if len(cfg.Tools) > 0 {
		setup["tools"] = cfg.Tools
	}

	msg := map[string]interface{}{"setup": setup}

	data, err := json.Marshal(msg)
	if err != nil {
		return fmt.Errorf("sendGoogleSetup: ошибка сериализации: %w", err)
	}

	rs.writeMu.Lock()
	writeErr := rs.googleConn.WriteMessage(websocket.TextMessage, data)
	rs.writeMu.Unlock()

	//logger.Info("sendGoogleSetup: отправлено respId=%d model=%s voice=%s lang=%s autoVAD=%v bargeIn=%v tools=%d",
	//	rs.respId, cfg.ModelName, voice, languageCode, autoVAD, bargeIn, len(cfg.Tools), rs.userID)
	return writeErr
}

// injectGoogleDialogHistory загружает историю диалога и отправляет её как clientContent
// до первого голосового сообщения. Лимит: DialogHistoryLimit/2.
func (m *Model) injectGoogleDialogHistory(rs *GoogleRealtimeSession, dialogID uint64) error {
	maxInject := int(create.DialogHistoryLimit) / 2

	history, found := m.getDialogHistoryFromCache(dialogID)
	if !found || len(history) == 0 {
		dbHistory, err := m.ConvertDialogToGoogleFormat(dialogID)
		if err != nil || len(dbHistory) == 0 {
			return nil
		}
		if len(dbHistory) > int(create.DialogHistoryLimit) {
			dbHistory = dbHistory[len(dbHistory)-int(create.DialogHistoryLimit):]
		}
		history = dbHistory
		cache := m.getOrCreateDialogCache(dialogID)
		cache.Contents = history
	}

	if len(history) == 0 {
		return nil
	}

	if len(history) > maxInject {
		history = history[len(history)-maxInject:]
	}

	//logger.Info("injectGoogleDialogHistory: инжектируем %d сообщений dialogID=%d respId=%d",
	//	len(history), dialogID, rs.respId, rs.userID)

	var turns []map[string]interface{}
	for _, msg := range history {
		if len(msg.Parts) == 0 {
			continue
		}
		role := msg.Role
		if role != "user" && role != "model" {
			continue
		}
		turns = append(turns, map[string]interface{}{
			"role":  role,
			"parts": msg.Parts,
		})
	}

	if len(turns) == 0 {
		return nil
	}

	inject := map[string]interface{}{
		"clientContent": map[string]interface{}{
			"turns":        turns,
			"turnComplete": false, // false = только контекст, не начинать генерацию
		},
	}

	return rs.writeJSON(inject)
}

// sendGoogleInitialGreeting отправляет приветствие сразу после setupComplete.
// Модель произносит приветственную фразу, не дожидаясь голоса пользователя.
func (m *Model) sendGoogleInitialGreeting(rs *GoogleRealtimeSession) {
	if !rs.greetingSent.CompareAndSwap(false, true) {
		return // уже отправлено
	}

	// Проверяем параметр InitialGreeting из конфига
	if rs.agentConfig.RealtimeVAD != nil &&
		rs.agentConfig.RealtimeVAD.InitialGreeting != nil &&
		!*rs.agentConfig.RealtimeVAD.InitialGreeting {
		//logger.Debug("sendGoogleInitialGreeting: приветствие отключено в конфиге respId=%d", rs.respId, rs.userID)
		return
	}

	var greetingText string
	hasExplicitGreeting := rs.agentConfig.RealtimeVAD != nil &&
		rs.agentConfig.RealtimeVAD.Greeting != nil &&
		*rs.agentConfig.RealtimeVAD.Greeting != ""

	if hasExplicitGreeting {
		// Явная фраза — передаём как инструкцию
		greetingText = "Your ONLY output is this exact phrase, nothing else, no commentary: " +
			*rs.agentConfig.RealtimeVAD.Greeting
	} else {
		// Авто-генерация приветствия
		greetingText = "Greet the user warmly and briefly (1-2 sentences). " +
			"Introduce yourself by name if you have one. " +
			"Ask how you can help. Speak naturally, no JSON."
	}

	msg := map[string]interface{}{
		"clientContent": map[string]interface{}{
			"turns": []map[string]interface{}{
				{
					"role":  "user",
					"parts": []map[string]interface{}{{"text": greetingText}},
				},
			},
			"turnComplete": true,
		},
	}

	if err := rs.writeJSON(msg); err != nil {
		//logger.Warn("sendGoogleInitialGreeting: ошибка отправки: %v respId=%d", err, rs.respId, rs.userID)
		rs.greetingSent.Store(false)
		//} else {
		//	logger.Info("sendGoogleInitialGreeting: приветствие отправлено respId=%d", rs.respId, rs.userID)
	}
}


