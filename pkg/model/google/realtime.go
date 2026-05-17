package google

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"strings"
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
	googleConn  *websocket.Conn // WSS-соединение к Google Multimodal Live API
	ctx         context.Context // Контекст сессии — отменяется при CloseRealtimeSession
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
	greetingSent    atomic.Bool
	setupCompleteCh chan struct{} // signaled when setupComplete is received

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

	// Race-guard: pump мог уже завершиться до подписки — отправляем синтетическую ошибку.
	select {
	case <-rs.ctx.Done():
		go func() {
			ch <- model.RealtimeEvent{
				Type: "error",
				Text: "google session terminated before subscription",
				Err:  fmt.Errorf("session terminated before subscription"),
			}
		}()
	default:
	}

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

	conn, err := create.DialGoogleRealtimeSession(m.client.GetAPIKey(), rm.AgentConfig.RealtimeModel)
	if err != nil {
		return fmt.Errorf("StartRealtimeSession: ошибка подключения к Google Live API: %w", err)
	}

	ctx, cancel := context.WithCancel(m.ctx)

	rs := &GoogleRealtimeSession{
		googleConn:      conn,
		ctx:             ctx,
		cancel:          cancel,
		agentConfig:     rm.AgentConfig,
		userID:          userID,
		dialogID:        dialogID,
		respId:          respId,
		AudioIn:         make(chan []byte, 256),
		AudioOut:        make(chan []byte, 256),
		DrainPlayback:   make(chan struct{}, 1),
		setupCompleteCh: make(chan struct{}),
	}

	// Отправляем setup-сообщение (первое сообщение в сессии Google Live API)
	if err := m.sendGoogleSetup(rs); err != nil {
		cancel()
		_ = conn.Close()
		return fmt.Errorf("StartRealtimeSession: ошибка setup: %w", err)
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

// normalizeLiveAPIToolKeys конвертирует snake_case ключи инструментов в camelCase
// для WebSocket Live API. REST API принимает оба варианта, WebSocket — только camelCase.
// Источник: google.golang.org/genai types.go — Tool struct использует json:"functionDeclarations","googleSearch" и т.д.
func normalizeLiveAPIToolKeys(tools []map[string]interface{}) []map[string]interface{} {
	if len(tools) == 0 {
		return tools
	}

	// Вспомогательная функция для перевода type в UPPERCASE 
	// (gRPC/Protobuf strict enum JSON pb parser требует "OBJECT" вместо "object").
	var uppercaseSchemaTypes func(interface{})
	uppercaseSchemaTypes = func(v interface{}) {
		if m, ok := v.(map[string]interface{}); ok {
			// type in UPPERCASE
			if t, ok := m["type"].(string); ok {
				m["type"] = strings.ToUpper(t)
			}
			// Clean empty 'required' array (can cause 1011 if empty in strict parsers)
			if req, ok := m["required"].([]interface{}); ok && len(req) == 0 {
				delete(m, "required")
			}
			if req, ok := m["required"].([]string); ok && len(req) == 0 {
				delete(m, "required")
			}

			if props, ok := m["properties"].(map[string]interface{}); ok {
				for _, prop := range props {
					uppercaseSchemaTypes(prop)
				}
			}
			if items, ok := m["items"]; ok {
				uppercaseSchemaTypes(items)
			}
		}
	}

	snakeToCamel := map[string]string{
		"function_declarations":   "functionDeclarations",
		"google_search":           "googleSearch",
		"code_execution":          "codeExecution",
		"google_search_retrieval": "googleSearchRetrieval",
	}
	normalized := make([]map[string]interface{}, len(tools))
	for i, tool := range tools {
		newTool := make(map[string]interface{}, len(tool))
		for k, v := range tool {
			camelKey := k
			if camel, ok := snakeToCamel[k]; ok {
				camelKey = camel
			}

			// Если это functionDeclarations, обязательно исправляем type в schema на UPPERCASE
			if camelKey == "functionDeclarations" {
				if fds, ok := v.([]map[string]interface{}); ok {
					for _, fd := range fds {
						if params, ok := fd["parameters"]; ok {
							uppercaseSchemaTypes(params)
						}
					}
				}
			}

			newTool[camelKey] = v
		}
		normalized[i] = newTool
	}
	return normalized
}

// sendGoogleSetup отправляет setup-сообщение Google Multimodal Live API.
// Это первое и единственное системное сообщение в начале сессии.
//
// Приоритет значений (от высокого к низкому):
//  1. RealtimeVAD.Google.* — Google-специфичные поля
//  2. RealtimeVAD.* — общие поля (Voice, SilenceDurationMs, InterruptResponse, Temperature, ...)
//  3. Глобальные константы (GoogleRealtimeDefaultVoice, GoogleRealtimeSilenceDurationMs, ...)
func (m *Model) sendGoogleSetup(rs *GoogleRealtimeSession) error {
	cfg := rs.agentConfig
	vad := cfg.RealtimeVAD             // может быть nil
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
	_ = languageCode

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

	generationConfig := map[string]interface{}{
		// TEXT + AUDIO: модель возвращает аудио (воспроизведение) и текст (история диалога).
		"responseModalities": []string{"AUDIO"},
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

	// Формируем realtimeInputConfig.
	//
	// activityHandling — управляет прерыванием генерации (barge-in):
	//   "START_OF_ACTIVITY_INTERRUPTS" → barge-in включён (дефолт Google)
	//   "NO_INTERRUPTION"              → barge-in выключен
	//
	// automaticActivityDetection.disabled — управляет режимом VAD:
	//   false (дефолт) → Google сам детектит речь (авто-VAD)
	//   true           → push-to-talk (пользователь вручную сигнализирует activityStart/End)
	//
	// ВАЖНО: disabled в automaticActivityDetection НЕ влияет на прерывание генерации —
	// это разные ортогональные настройки.
	activityHandling := "START_OF_ACTIVITY_INTERRUPTS"
	if !bargeIn {
		activityHandling = "NO_INTERRUPTION"
	}

	var realtimeInputConfig map[string]interface{}
	if autoVAD {
		realtimeInputConfig = map[string]interface{}{
			"automaticActivityDetection": map[string]interface{}{
				"silenceDurationMs": silenceDurationMs,
			},
			"activityHandling": activityHandling,
		}
	} else {
		// Push-to-talk: автодетект речи выключен, но activityHandling всё равно применяется
		realtimeInputConfig = map[string]interface{}{
			"automaticActivityDetection": map[string]interface{}{
				"disabled": true,
			},
			"activityHandling": activityHandling,
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
	outputTranscription := false
	if gvad != nil && gvad.OutputAudioTranscription != nil {
		outputTranscription = *gvad.OutputAudioTranscription
	}

	// Определяем realtime-модель: используем RealtimeModel (напр. gemini-2.0-flash-lite),
	// а не ModelName (который является обычной текстовой моделью пользователя).
	// ЗАЩИТА: если RealtimeModel совпадает с текстовой ModelName — принудительно используем константу.
	realtimeModel := cfg.RealtimeModel
	if realtimeModel == "" || realtimeModel == cfg.ModelName || realtimeModel == "gemini-3.1-flash-live-preview" {
		realtimeModel = create.RealtimeGoogleModel
	}

	setup := map[string]interface{}{
		"model":              fmt.Sprintf("models/%s", realtimeModel),
		"generationConfig":   generationConfig,
		"realtimeInputConfig": realtimeInputConfig,
	}

	// Транскрипция входящей речи
	if inputTranscription {
		setup["inputAudioTranscription"] = map[string]interface{}{}
	}
	// Транскрипция исходящей речи (нужна для сохранения текста ответа в историю)
	if outputTranscription {
		setup["outputAudioTranscription"] = map[string]interface{}{}
	}

	if cfg.SystemInstruction != nil {
		// Convert to map to remove "role" field which might cause strict parser errors
		if b, err := json.Marshal(cfg.SystemInstruction); err == nil {
			var sysInst map[string]interface{}
			if err := json.Unmarshal(b, &sysInst); err == nil {
				delete(sysInst, "role")
				setup["systemInstruction"] = sysInst
			} else {
				setup["systemInstruction"] = cfg.SystemInstruction
			}
		} else {
			setup["systemInstruction"] = cfg.SystemInstruction
		}
	}

	if len(cfg.Tools) > 0 {
		normalizedTools := normalizeLiveAPIToolKeys(cfg.Tools)
		setup["tools"] = normalizedTools
	}

	msg := map[string]interface{}{"setup": setup}

	data, err := json.Marshal(msg)
	if err != nil {
		return fmt.Errorf("sendGoogleSetup: ошибка сериализации: %w", err)
	}

	// Логируем setup для отладки VAD/barge-in
	preview := string(data)
	if len(preview) > 800 {
		preview = preview[:800] + "..."
	}
	log.Printf("[sendGoogleSetup] respId=%d setup=%s", rs.respId, preview)

	rs.writeMu.Lock()
	writeErr := rs.googleConn.WriteMessage(websocket.TextMessage, data)
	rs.writeMu.Unlock()

	return writeErr
}

// sendHistoryAndGreeting объединяет историю диалога и приветствие в ОДНО
// сообщение clientContent (один user-turn, turnComplete=true).
//
// Google Live API НЕ поддерживает clientContent.turns с несколькими элементами
// (user+model) — возвращает 1007 "invalid argument". Единственный надёжный
// способ: вся история как текстовый префикс + приветствие в одном user-turn.
func (m *Model) sendHistoryAndGreeting(rs *GoogleRealtimeSession) {
	if !rs.greetingSent.CompareAndSwap(false, true) {
		return
	}

	// ── 1. Строим текст приветствия ──────────────────────────────────────────
	sendGreeting := true
	if rs.agentConfig.RealtimeVAD != nil &&
		rs.agentConfig.RealtimeVAD.InitialGreeting != nil &&
		!*rs.agentConfig.RealtimeVAD.InitialGreeting {
		sendGreeting = false
	}

	var greetingText string
	if sendGreeting {
		if rs.agentConfig.RealtimeVAD != nil &&
			rs.agentConfig.RealtimeVAD.Greeting != nil &&
			*rs.agentConfig.RealtimeVAD.Greeting != "" {
			greetingText = "Your ONLY output is this exact phrase, nothing else, no commentary: " +
				*rs.agentConfig.RealtimeVAD.Greeting
		} else {
			greetingText = "Greet the user warmly and briefly (1-2 sentences). " +
				"Introduce yourself by name if you have one. " +
				"Ask how you can help. Speak naturally, no JSON."
		}
	}

	// ── 2. Строим текстовое представление истории ────────────────────────────
	historyText := m.buildHistoryAsText(rs.dialogID)
	if historyText != "" {
		//log.Printf("[sendHistoryAndGreeting] история загружена dialogID=%d respId=%d", rs.dialogID, rs.respId)
	}

	// ── 3. Собираем финальный текст user-turn ────────────────────────────────
	var fullText string
	if historyText != "" {
		fullText = "Context from our previous conversation:\n" + historyText
		if greetingText != "" {
			fullText += "\n\n" + greetingText
		}
	} else {
		fullText = greetingText
	}

	if fullText == "" {
		return // нечего отправлять
	}

	// ── 4. Один user-turn, turnComplete=true ─────────────────────────────────
	msg := map[string]interface{}{
		"clientContent": map[string]interface{}{
			"turns": []map[string]interface{}{
				{
					"role":  "user",
					"parts": []map[string]interface{}{{"text": fullText}},
				},
			},
			"turnComplete": true,
		},
	}

	//if dbg, err2 := json.Marshal(msg); err2 == nil {
	//	log.Printf("[sendHistoryAndGreeting] отправка %d байт respId=%d", len(dbg), rs.respId)
	//}

	if err := rs.writeJSON(msg); err != nil {
		log.Printf("[sendHistoryAndGreeting] ERROR respId=%d: %v", rs.respId, err)
		rs.greetingSent.Store(false)
	}
}

// buildHistoryAsText загружает историю диалога и возвращает её в виде текста
// формата "User: ...\nAssistant: ...\n" для вставки в системный контекст.
func (m *Model) buildHistoryAsText(dialogID uint64) string {
	maxInject := int(create.DialogHistoryLimit) / 2

	history, found := m.getDialogHistoryFromCache(dialogID)
	if !found || len(history) == 0 {
		dbHistory, err := m.ConvertDialogToGoogleFormat(dialogID)
		if err != nil || len(dbHistory) == 0 {
			return ""
		}
		if len(dbHistory) > int(create.DialogHistoryLimit) {
			dbHistory = dbHistory[len(dbHistory)-int(create.DialogHistoryLimit):]
		}
		history = dbHistory
		cache := m.getOrCreateDialogCache(dialogID)
		cache.Contents = history
	}

	if len(history) == 0 {
		return ""
	}

	if len(history) > maxInject {
		history = history[len(history)-maxInject:]
	}

	var sb strings.Builder
	for _, msg := range history {
		role := msg.Role
		if role != "user" && role != "model" {
			continue
		}
		var partText strings.Builder
		for _, p := range msg.Parts {
			if t, ok := p["text"].(string); ok && t != "" {
				if partText.Len() > 0 {
					partText.WriteString(" ")
				}
				partText.WriteString(t)
			}
		}
		if partText.Len() == 0 {
			continue
		}
		prefix := "User"
		if role == "model" {
			prefix = "Assistant"
		}
		sb.WriteString(prefix)
		sb.WriteString(": ")
		sb.WriteString(partText.String())
		sb.WriteString("\n")
	}
	return sb.String()
}
