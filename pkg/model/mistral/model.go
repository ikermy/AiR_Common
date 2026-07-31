package mistral

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/ikermy/air_common/pkg/com"
	"github.com/ikermy/air_common/pkg/comdb"
	"github.com/ikermy/air_common/pkg/mode"
	"github.com/ikermy/air_common/pkg/model"
	"github.com/ikermy/air_common/pkg/model/commdom"
	"github.com/ikermy/air_common/pkg/model/create"
	"github.com/ikermy/air_common/pkg/model/provider_catalog"
)

// Model реализует интерфейс model.UniversalModel для работы с Mistral AI
type Model struct {
	ctx            context.Context
	cancel         context.CancelFunc
	client         *MistralAgentClient
	db             DB
	responders     sync.Map      // map[uint64]*RespModel
	waitChannels   sync.Map      // map[uint64]chan struct{}
	UserModelTTl   time.Duration // Время жизни пользовательской модели в памяти
	actionHandler  model.ActionHandler
	shutdownOnce   sync.Once
	router         model.RouterInterface  // Ссылка на router
	universalModel *create.UniversalModel // Для доступа к DecompressModelData
	realtime       *RealtimeManager
}

var _ model.RealtimeProvider = (*Model)(nil)
var _ model.MistralManager = (*Model)(nil)

type DB comdb.Exterior

type RespModel struct {
	Ctx            context.Context
	Cancel         context.CancelFunc
	Chan           *model.Ch            // Канал для этого респондента (основной, deprecated - используйте ChanMap)
	ChanMap        map[uint64]*model.Ch // Map каналов для поддержки множественных dialogID (унификация с OpenAI/Google)
	Context        *DialogContext       // Один текущий контекст диалога
	TTL            time.Time
	Assist         model.Assistant
	RespName       string
	Services       Services
	ConversationId string // ID conversation для Mistral Conversations API
	Haunter        bool   // Модель используется для поиска лидов
	ToolsSynced    bool   // true — агент уже синхронизирован с MCP tools в этой сессии
	//LibraryId string // ID библиотеки Mistral для document_library (кэш из БД)
}

// GetChannel реализует интерфейс model.ChannelProvider
func (r *RespModel) GetChannel() *model.Ch {
	return r.Chan
}

// GetChannelMap реализует интерфейс model.ChannelProvider
func (r *RespModel) GetChannelMap() map[uint64]*model.Ch {
	return r.ChanMap
}

// DialogContext хранит историю сообщений диалога в памяти
type DialogContext struct {
	Messages []Message
	LastUsed time.Time
}

// Message представляет сообщение в контексте диалога
type Message struct {
	Type      string    `json:"type"`      // "user" или "assistant"
	Content   string    `json:"content"`   // Текст сообщения
	Timestamp time.Time `json:"timestamp"` // Время создания
}

type Services struct {
	Listener   atomic.Bool
	Respondent atomic.Bool
}

// New создает новую модель Mistral
func New(parent context.Context, actionHandler model.ActionHandler, db DB, router model.RouterInterface) *Model {
	ctx, cancel := context.WithCancel(parent)

	mistralClient := NewMistralAgentClient(parent)

	// Резолвер персональных ключей Mistral: возвращаем только ключ из БД или пустую строку.
	mistralClient.SetKeyResolver(func(userID uint32) string {
		if key, err := db.GetUserAPIKey(userID, commdom.ProviderMistral); err == nil {
			return key
		}
		return ""
	})

	return &Model{
		ctx:           ctx,
		cancel:        cancel,
		client:        mistralClient,
		db:            db,
		responders:    sync.Map{},
		waitChannels:  sync.Map{},
		UserModelTTl:  mode.UserModelTTl,
		actionHandler: actionHandler,
		router:        router,
		realtime:      NewRealtimeManager(ctx),
	}
}

// StartMistralRealtimeSession creates a managed voice session. The transport
// pump can be attached after the provider's streaming STT protocol is chosen.
func (m *Model) StartMistralRealtimeSession(userID uint32, dialogID, respID uint64) (*MistralRealtimeSession, error) {
	if m == nil || m.realtime == nil {
		return nil, fmt.Errorf("Mistral realtime manager is not initialized")
	}
	// RealtimeManager.Start reuses an existing session for the same respID.
	// Return it directly as well: attaching another STT transport here would
	// make the second call fail with "STT transport уже запущен".
	if existing, ok := m.realtime.Get(respID); ok {
		return existing, nil
	}
	record, err := m.db.GetModelByProviderAnyStatus(userID, commdom.ProviderMistral)
	if err != nil {
		return nil, fmt.Errorf("ошибка получения Mistral-модели для realtime: %w", err)
	}
	if record == nil || record.Realtime == nil || record.Realtime.Name == "" {
		return nil, fmt.Errorf("realtime-модель Mistral не настроена для userID=%d", userID)
	}
	if m.universalModel != nil {
		compressedData, vecIDs, readErr := m.db.ReadUserModelByProvider(userID, commdom.ProviderMistral)
		if readErr != nil {
			return nil, fmt.Errorf("ошибка чтения конфигурации Mistral realtime: %w", readErr)
		}
		if compressedData != nil {
			config, decodeErr := m.universalModel.DecompressModelData(compressedData, vecIDs)
			if decodeErr != nil {
				return nil, fmt.Errorf("ошибка распаковки конфигурации Mistral realtime: %w", decodeErr)
			}
			if !config.Realtime {
				return nil, fmt.Errorf("Mistral realtime не включён для userID=%d", userID)
			}
			sessionConfig := config.RealtimeVAD
			session, startErr := m.realtime.Start(userID, dialogID, respID)
			if startErr != nil {
				return nil, startErr
			}
			session.RealtimeModel = record.Realtime.Name
			if sessionConfig != nil {
				session.Config = sessionConfig.Mistral
				session.Greeting = sessionConfig.Greeting
				session.InitialGreeting = sessionConfig.InitialGreeting
			}
			if err := m.attachMistralRealtimeSTT(session); err != nil {
				m.realtime.Close(respID)
				return nil, err
			}
			m.startMistralGreeting(session)
			return session, nil
		}
	}
	session, err := m.realtime.Start(userID, dialogID, respID)
	if err != nil {
		return nil, err
	}
	session.RealtimeModel = record.Realtime.Name
	if err := m.attachMistralRealtimeSTT(session); err != nil {
		m.realtime.Close(respID)
		return nil, err
	}
	m.startMistralGreeting(session)
	return session, nil
}

func (m *Model) startMistralGreeting(session *MistralRealtimeSession) {
	if session == nil || session.InitialGreeting != nil && !*session.InitialGreeting {
		return
	}
	if session.Greeting == nil || strings.TrimSpace(*session.Greeting) == "" {
		return
	}
	greeting := strings.TrimSpace(*session.Greeting)
	turnID := session.BeginTurn()
	go func() {
		err := session.WithLLM(func() error {
			return m.requestRealtimeLLM(session, turnID, greeting)
		})
		if err != nil && session.Context().Err() == nil {
			session.PublishEvent(model.RealtimeEvent{Type: "error", Text: "ошибка Mistral greeting", Err: err})
			return
		}
	}()
}

func (m *Model) attachMistralRealtimeSTT(session *MistralRealtimeSession) error {
	if session == nil {
		return fmt.Errorf("Mistral realtime session is nil")
	}
	modelName := session.RealtimeModel
	if session.Config != nil && session.Config.STTModel != nil && *session.Config.STTModel != "" {
		modelName = *session.Config.STTModel
	}
	apiKey := m.client.resolveKey(session.UserID())
	transport, err := NewMistralRealtimeSTT(RealtimeSTTConfig{
		Model:  modelName,
		APIKey: apiKey,
	})
	if err != nil {
		return fmt.Errorf("настройка Mistral realtime STT: %w", err)
	}
	return session.StartSTT(transport, func(text string, turnID uint64) error {
		if strings.TrimSpace(text) == "" {
			return nil
		}
		return session.WithLLM(func() error {
			return m.requestRealtimeLLM(session, turnID, text)
		})
	})
}

// requestRealtimeLLM buffers the Conversations streaming protocol and sends
// only the assistant message to TTS. The provider stream may contain JSON
// envelopes, Markdown fences, action metadata and token usage; none of that
// is speech text.
func (m *Model) requestRealtimeLLM(session *MistralRealtimeSession, turnID uint64, input string) error {
	extractor := realtimeMessageExtractor{}
	return m.RequestStreaming(session.UserID(), session.DialogID(), input, func(delta string, done bool) error {
		if !session.IsCurrentTurn(turnID) {
			return nil
		}
		text := extractor.Push(delta)
		if text != "" {
			if err := m.StreamMistralRealtimeText(session.Context(), session.RespID(), text, false); err != nil {
				return err
			}
		}
		if !done {
			return nil
		}
		if tail := extractor.Flush(); tail != "" {
			return m.StreamMistralRealtimeText(session.Context(), session.RespID(), tail, true)
		}
		return m.StreamMistralRealtimeText(session.Context(), session.RespID(), "", true)
	})
}

type realtimeMessageExtractor struct {
	raw     strings.Builder
	started bool
	closed  bool
	escaped bool
	text    strings.Builder
}

func (e *realtimeMessageExtractor) Push(delta string) string {
	e.raw.WriteString(delta)
	if e.closed {
		return ""
	}
	if !e.started {
		value := e.raw.String()
		marker := strings.Index(value, `"message"`)
		if marker < 0 {
			return ""
		}
		colon := strings.Index(value[marker+len(`"message"`):], ":")
		if colon < 0 {
			return ""
		}
		start := marker + len(`"message"`) + colon + 1
		for start < len(value) && (value[start] == ' ' || value[start] == '\t') {
			start++
		}
		if start >= len(value) || value[start] != '"' {
			return ""
		}
		e.started = true
		e.raw.Reset()
		e.raw.WriteString(value[start+1:])
	}

	value := e.raw.String()
	e.raw.Reset()
	var out strings.Builder
	for i := 0; i < len(value); i++ {
		c := value[i]
		if e.escaped {
			e.escaped = false
			switch c {
			case 'n':
				out.WriteByte('\n')
			case 'r':
				out.WriteByte('\r')
			case 't':
				out.WriteByte('\t')
			default:
				out.WriteByte(c)
			}
			continue
		}
		if c == '\\' {
			e.escaped = true
			continue
		}
		if c == '"' {
			e.closed = true
			e.raw.WriteString(value[i+1:])
			break
		}
		out.WriteByte(c)
	}
	e.text.WriteString(out.String())
	return sanitizeMistralTTSText(out.String())
}

func (e *realtimeMessageExtractor) Flush() string {
	if e.closed {
		return ""
	}
	return sanitizeMistralTTSText(e.Push(""))
}

func realtimeAssistantText(raw string) string {
	text := strings.TrimSpace(raw)
	text = strings.TrimSpace(strings.TrimPrefix(text, "```json"))
	text = strings.TrimSpace(strings.TrimSuffix(text, "```"))
	var envelope struct {
		Message string `json:"message"`
	}
	if json.Unmarshal([]byte(text), &envelope) == nil && strings.TrimSpace(envelope.Message) != "" {
		return strings.TrimSpace(envelope.Message)
	}
	// Some streaming responses concatenate the JSON message with metadata.
	if start := strings.Index(text, `"message"`); start >= 0 {
		value := text[start+len(`"message"`):]
		value = strings.TrimSpace(strings.TrimPrefix(value, ":"))
		if len(value) > 0 && value[0] == '"' {
			if end := strings.Index(value[1:], `"`); end >= 0 {
				return strings.TrimSpace(value[1 : end+1])
			}
		}
	}
	return text
}

// StreamMistralRealtimeText sends ready LLM sentence chunks to Voxtral TTS.
func (m *Model) StreamMistralRealtimeText(ctx context.Context, respID uint64, delta string, final bool) error {
	session, ok := m.GetMistralRealtimeSession(respID)
	if !ok {
		return fmt.Errorf("Mistral realtime session not found for respID=%d", respID)
	}
	if session.Config == nil || session.Config.TTSModel == nil || *session.Config.TTSModel == "" {
		return fmt.Errorf("Mistral TTS-модель не настроена для respID=%d", respID)
	}
	session.Generating().Store(true)
	defer session.Generating().Store(false)
	if strings.TrimSpace(delta) != "" {
		session.MarkFirstLLMToken()
	}
	turnID := session.CurrentTurn()
	if turnID == 0 {
		turnID = session.BeginTurn()
	}
	turnCtx, ok := session.TurnContext(turnID)
	if !ok {
		return nil
	}
	for _, sentence := range session.PushText(delta, final) {
		sentence = sanitizeMistralTTSText(sentence)
		if sentence == "" {
			continue
		}
		session.PublishEvent(model.RealtimeEvent{Type: "audio_start", Text: sentence})
		voiceID := stringValue(session.Config.VoiceID)
		referenceAudio := stringValue(session.Config.ReferenceAudioID)
		if session.Config.VoiceClone != nil {
			if voiceID == "" {
				voiceID = session.Config.VoiceClone.ProfileID
			}
			if referenceAudio == "" {
				referenceAudio = session.Config.VoiceClone.ReferenceAudioID
			}
		}
		body, contentType, err := m.client.Speech(turnCtx, session.UserID(), SpeechRequest{
			Model:          *session.Config.TTSModel,
			Input:          sentence,
			Voice:          stringValue(session.Config.Voice),
			VoiceID:        voiceID,
			ReferenceAudio: referenceAudio,
			ResponseFormat: "pcm",
		})
		if err != nil {
			if session.Context().Err() != nil {
				return nil
			}
			session.Metrics().TTSErrors.Add(1)
			session.PublishEvent(model.RealtimeEvent{Type: "error", Text: "Mistral TTS завершился с ошибкой", Err: err})
			return err
		}
		streamer := StreamSpeechToSession
		if strings.HasPrefix(strings.ToLower(contentType), "text/event-stream") {
			streamer = StreamSpeechSSEToSession
		}
		if err := streamer(turnCtx, session, turnID, body); err != nil {
			if session.Context().Err() != nil {
				return nil
			}
			session.PublishEvent(model.RealtimeEvent{Type: "error", Text: "ошибка чтения Mistral TTS", Err: err})
			return err
		}
		session.PublishEvent(model.RealtimeEvent{Type: "audio_end"})
	}
	return nil
}

var mistralTTSActionRE = regexp.MustCompile(`\*[^*]+\*`)

func sanitizeMistralTTSText(text string) string {
	text = mistralTTSActionRE.ReplaceAllString(text, " ")
	text = strings.ReplaceAll(text, "```", "")
	return strings.TrimSpace(text)
}

// TranscribeMistralRealtimeSegment transcribes one finalized audio segment.
// It is the transport-neutral STT primitive used by a future continuous pump.
func (m *Model) TranscribeMistralRealtimeSegment(ctx context.Context, respID uint64, audio []byte, fileName string) (string, error) {
	session, ok := m.GetMistralRealtimeSession(respID)
	if !ok {
		return "", fmt.Errorf("Mistral realtime session not found for respID=%d", respID)
	}
	modelName := session.RealtimeModel
	language := ""
	if session.Config != nil {
		if session.Config.STTModel != nil && *session.Config.STTModel != "" {
			modelName = *session.Config.STTModel
		}
		language = stringValue(session.Config.STTLanguage)
	}
	if modelName == "" {
		return "", fmt.Errorf("Mistral STT-модель не настроена для respID=%d", respID)
	}
	text, err := m.client.TranscribeAudio(ctx, session.UserID(), modelName, language, fileName, audio)
	if err != nil {
		return "", err
	}
	if strings.TrimSpace(text) != "" {
		session.BeginTurn()
	}
	return text, nil
}

// ProcessMistralRealtimeSegment runs one complete cascaded voice turn:
// STT -> Mistral LLM streaming -> sentence chunking -> Voxtral TTS.
func (m *Model) ProcessMistralRealtimeSegment(ctx context.Context, respID uint64, audio []byte, fileName string) error {
	session, ok := m.GetMistralRealtimeSession(respID)
	if !ok {
		return fmt.Errorf("Mistral realtime session not found for respID=%d", respID)
	}
	text, err := m.TranscribeMistralRealtimeSegment(ctx, respID, audio, fileName)
	if err != nil {
		return err
	}
	text = strings.TrimSpace(text)
	if text == "" {
		return nil
	}
	err = m.RequestStreaming(session.UserID(), session.DialogID(), text, func(delta string, done bool) error {
		return m.StreamMistralRealtimeText(ctx, respID, delta, done)
	})
	if err != nil {
		session.Metrics().LLMErrors.Add(1)
	}
	return err
}

func stringValue(value *string) string {
	if value == nil {
		return ""
	}
	return *value
}

func (m *Model) GetMistralRealtimeSession(respID uint64) (*MistralRealtimeSession, bool) {
	if m == nil || m.realtime == nil {
		return nil, false
	}
	return m.realtime.Get(respID)
}

func (m *Model) SendMistralRealtimeAudio(respID uint64, pcm16 []byte) error {
	session, ok := m.GetMistralRealtimeSession(respID)
	if !ok {
		return fmt.Errorf("Mistral realtime session not found for respID=%d", respID)
	}
	return session.SendAudio(pcm16)
}

func (m *Model) CloseMistralRealtimeSession(respID uint64) {
	if m != nil && m.realtime != nil {
		m.realtime.Close(respID)
	}
}

// SubscribeMistralRealtimeEvents exposes the common realtime event stream
// without making Mistral depend on the router's RealtimeProvider interface.
// The provider cannot be registered in the router until its continuous STT
// transport is available, but callers that explicitly create a Mistral
// session can already consume the same event contract as other providers.
func (m *Model) SubscribeMistralRealtimeEvents(respID uint64) (<-chan model.RealtimeEvent, error) {
	session, ok := m.GetMistralRealtimeSession(respID)
	if !ok {
		return nil, fmt.Errorf("Mistral realtime session not found for respID=%d", respID)
	}
	return session.SubscribeEvents()
}

func (m *Model) UnsubscribeMistralRealtimeEvents(respID uint64, sub <-chan model.RealtimeEvent) {
	if session, ok := m.GetMistralRealtimeSession(respID); ok {
		session.UnsubscribeEvents(sub)
	}
}

func (m *Model) GetMistralRealtimeAudio(respID uint64) (<-chan []byte, error) {
	session, ok := m.GetMistralRealtimeSession(respID)
	if !ok {
		return nil, fmt.Errorf("Mistral realtime session not found for respID=%d", respID)
	}
	return session.AudioOutput(), nil
}

func (m *Model) GetMistralRealtimeDrain(respID uint64) (<-chan struct{}, error) {
	session, ok := m.GetMistralRealtimeSession(respID)
	if !ok {
		return nil, fmt.Errorf("Mistral realtime session not found for respID=%d", respID)
	}
	return session.DrainOutput(), nil
}

func (m *Model) GetMistralRealtimeGenerating(respID uint64) *atomic.Bool {
	session, ok := m.GetMistralRealtimeSession(respID)
	if !ok {
		return nil
	}
	return session.Generating()
}

func (m *Model) SetMistralRealtimeDisconnectCallback(respID uint64, callback func(uint64)) error {
	session, ok := m.GetMistralRealtimeSession(respID)
	if !ok {
		return fmt.Errorf("Mistral realtime session not found for respID=%d", respID)
	}
	session.SetDisconnectCallback(callback)
	return nil
}

// The following methods implement model.RealtimeProvider. The transport and
// session orchestration remain Mistral-specific, while the router can expose
// the same realtime API to clients as it does for OpenAI and Google.
func (m *Model) StartRealtimeSession(userID uint32, dialogID, respID uint64) error {
	_, err := m.StartMistralRealtimeSession(userID, dialogID, respID)
	return err
}

func (m *Model) CloseRealtimeSession(respID uint64) {
	m.CloseMistralRealtimeSession(respID)
}

func (m *Model) SendRealtimeAudio(respID uint64, pcm16 []byte) error {
	return m.SendMistralRealtimeAudio(respID, pcm16)
}

func (m *Model) SubscribeEvents(respID uint64) (<-chan model.RealtimeEvent, error) {
	return m.SubscribeMistralRealtimeEvents(respID)
}

func (m *Model) UnsubscribeEvents(respID uint64, sub <-chan model.RealtimeEvent) {
	m.UnsubscribeMistralRealtimeEvents(respID, sub)
}

func (m *Model) GetRealtimeAudio(respID uint64) (<-chan []byte, error) {
	return m.GetMistralRealtimeAudio(respID)
}

func (m *Model) GetRealtimeDrain(respID uint64) (<-chan struct{}, error) {
	return m.GetMistralRealtimeDrain(respID)
}

func (m *Model) GetRealtimeGenerating(respID uint64) *atomic.Bool {
	return m.GetMistralRealtimeGenerating(respID)
}

func (m *Model) SetRealtimeDisconnectCallback(respID uint64, callback func(uint64)) error {
	return m.SetMistralRealtimeDisconnectCallback(respID, callback)
}

func (m *Model) CreateVoice(userID uint32, request commdom.CreateVoiceRequest) (commdom.Voice, error) {
	return m.client.CreateVoice(m.ctx, userID, request)
}
func (m *Model) ListVoices(userID uint32, limit, offset int, voiceType string) (commdom.VoiceList, error) {
	return m.client.ListVoices(m.ctx, userID, limit, offset, voiceType)
}
func (m *Model) GetVoice(userID uint32, voiceID string) (commdom.Voice, error) {
	return m.client.GetVoice(m.ctx, userID, voiceID)
}
func (m *Model) UpdateVoice(userID uint32, voiceID string, request commdom.UpdateVoiceRequest) (commdom.Voice, error) {
	return m.client.UpdateVoice(m.ctx, userID, voiceID, request)
}
func (m *Model) DeleteVoice(userID uint32, voiceID string) (commdom.Voice, error) {
	return m.client.DeleteVoice(m.ctx, userID, voiceID)
}
func (m *Model) GetVoiceSample(userID uint32, voiceID string) (io.ReadCloser, string, error) {
	return m.client.GetVoiceSample(m.ctx, userID, voiceID)
}

// NewAsRouterOption создаёт Mistral модель и возвращает её как опцию для ModelRouter
// Использование: router := model.NewModelRouter(ctx, db, mistral.NewAsRouterOption())
func NewAsRouterOption() model.RouterOption {
	return func(r *model.Router, ctx context.Context, db model.DB) error {
		// Создаём универсальный обработчик функций с Google OAuth конфигом
		actionHandler := model.NewUniversalActionHandler(ctx)

		// Создаём Mistral модель с action handler и router
		mistralModel := New(ctx, actionHandler, db, r)

		// Устанавливаем UniversalModel для доступа к DecompressModelData
		universalModel := create.New(ctx, db)

		// Подключаем MCP fetchers для create-time операций (создание агента через Mistral API).
		// Аналогично google/model.go: function declarations и prompt hint — только от MCP.
		if mcpProvider, ok := model.ActionHandler(actionHandler).(model.MCPConfigProvider); ok {
			universalModel.SetMistralMCPFetchers(
				func(fetchCtx context.Context, userID uint32, provider commdom.ProviderType) (string, error) {
					return mcpProvider.FetchSystemPrompt(fetchCtx, userID, provider)
				},
				func(fetchCtx context.Context, userID uint32, provider commdom.ProviderType) ([]create.FunctionDeclaration, error) {
					mcpTools, err := mcpProvider.FetchToolsList(fetchCtx, userID, provider)
					if err != nil {
						return nil, err
					}
					functions := make([]create.FunctionDeclaration, 0, len(mcpTools))
					for _, t := range mcpTools {
						functions = append(functions, create.FunctionDeclaration{
							Name:        t.Name,
							Description: t.Description,
							Parameters:  t.InputSchema,
						})
					}
					return functions, nil
				},
			)
		}

		mistralModel.SetUniversalModel(universalModel)

		// Регистрируем модель в роутере
		return model.WithMistralModel(mistralModel)(r, ctx, db)
	}
}

// NewMessage создает новое сообщение (реализация model.UniversalModel)
func (m *Model) NewMessage(operator model.Operator, msgType string, content *model.AssistResponse, name *string, files ...model.FileUpload) model.Message {
	var nameStr string
	if name != nil {
		nameStr = *name
	}

	return model.Message{
		Operator:  operator,
		Type:      msgType,
		Content:   *content,
		Name:      nameStr,
		Timestamp: time.Now(),
		Files:     files,
	}
}

// GetFileAsReader загружает файл по URL (реализация model.UniversalModel)
func (m *Model) GetFileAsReader(_ uint32, url string) (io.Reader, error) {
	if url == "" {
		return nil, fmt.Errorf("не указан источник файла: отсутствуют URL")
	}

	req, err := http.NewRequestWithContext(m.ctx, "GET", url, nil)
	if err != nil {
		return nil, fmt.Errorf("ошибка подготовки запроса загрузки файла: %w", err)
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("ошибка загрузки файла по URL: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		_ = resp.Body.Close()
		return nil, fmt.Errorf("ошибка HTTP при загрузке файла: статус %d", resp.StatusCode)
	}

	return resp.Body, nil
}

// GetOrSetRespGPT получает или создает RespModel (реализация model.UniversalModel)
func (m *Model) GetOrSetRespGPT(assist model.Assistant, dialogID, respId uint64, respName string) (*model.RespModel, error) {
	// Используем respId как ключ
	if val, ok := m.responders.Load(respId); ok {
		respModel := val.(*RespModel)
		respModel.TTL = time.Now().Add(m.UserModelTTl) // Обновляем TTL при каждом обращении
		return m.convertToModelRespModel(respModel), nil
	}

	// Проверяем наличие API-ключа для пользователя до создания респондента.
	// Получаем ключ напрямую через DB: это обеспечивает правильную обработку $mk$-ключей —
	// если MasterKey недоступен, ошибка и уведомление пропагируются явно, а не теряются в HasAPIKey.
	apiKey, err := m.db.GetUserAPIKey(assist.UserID, commdom.ProviderMistral)
	if err != nil {
		return nil, fmt.Errorf("ошибка получения Mistral API-ключа для пользователя %d: %w", assist.UserID, err)
	}
	if m.client == nil || apiKey == "" {
		return nil, fmt.Errorf("Mistral API ключ не настроен для пользователя %d: добавьте персональный ключ через настройки", assist.UserID)
	}

	// Используем helper-функцию для создания базовых компонентов
	userCtx, cancel, ch, ttl := model.CreateBaseResponder(m.ctx, m.UserModelTTl, assist, dialogID, respName)

	user := &RespModel{
		Assist:   assist,
		RespName: respName,
		TTL:      ttl,
		Chan:     ch,
		Context: &DialogContext{
			Messages: []Message{}, // Пустой контекст - при использовании Conversations API история хранится на стороне Mistral
			LastUsed: time.Now(),
		},
		Services: Services{},
		Ctx:      userCtx,
		Cancel:   cancel,
	}

	// ВАЖНО: При использовании Conversations API история диалога НЕ загружается из БД!
	// Mistral хранит всю историю на своей стороне через conversation_id.
	// Локальный контекст используется ТОЛЬКО для сохранения в БД при выходе.

	// Загружаем conversation_id из БД (если есть)
	contextData, err := m.db.ReadContext(dialogID, commdom.ProviderMistral)
	if err != nil {
		if strings.Contains(err.Error(), "получены пустые данные") {
			//logger.Debug("Инициализация нового диалога %d", dialogID, assist.userID)
			// ConversationId будет создан при первом запросе
			//} else {
			//	logger.Error("Ошибка чтения контекста для dialogID %d: %v", dialogID, err)
		}
	} else if contextData != nil {
		//logger.Debug("Контекст загружен для dialogID %d: %s", dialogID, string(contextData), assist.userID)

		var contextObj struct {
			ConversationID string `json:"conversation_id"`
		}

		// JSON_EXTRACT может вернуть строку с кавычками, пробуем распарсить
		err = json.Unmarshal(contextData, &contextObj)
		if err != nil {
			// Если не получилось, пробуем убрать внешние кавычки и распарсить снова
			var rawString string
			if err2 := json.Unmarshal(contextData, &rawString); err2 == nil {
				// Успешно извлекли строку, теперь парсим её как JSON
				if err3 := json.Unmarshal([]byte(rawString), &contextObj); err3 != nil {
					//logger.Error("Ошибка десериализации контекста для dialogID %d: %v", dialogID, err3)
				}
				//} else {
				//	logger.Error("Ошибка десериализации контекста для dialogID %d: %v", dialogID, err)
			}
		}

		if contextObj.ConversationID != "" {
			user.ConversationId = contextObj.ConversationID
			//logger.Debug("Загружен conversation_id: %s", contextObj.ConversationID, assist.userID)
		}
	}

	// Загружаем параметры модели из БД (включая Haunter)
	compressedData, _, err := m.db.ReadUserModelByProvider(assist.UserID, commdom.ProviderMistral)
	if err != nil {
		//logger.Warn("Ошибка чтения данных модели из БД: %v, используем конфигурацию по умолчанию", err, assist.userID)
	} else if compressedData != nil && m.universalModel != nil {
		if modelData, decompErr := m.universalModel.DecompressModelData(compressedData, nil); decompErr == nil {
			user.Haunter = modelData.Haunter
			//} else {
			//	logger.Warn("Ошибка распаковки параметров модели: %v", decompErr, assist.userID)
		}
	}

	//// Загружаем LibraryId ОДИН РАЗ при создании (избегаем запросов к БД при каждом сообщении)
	//if libraryID, err := m.loadLibraryIdFromDB(assist.userID); err == nil {
	//	user.LibraryId = libraryID
	//	logger.Debug("LibraryId загружен для пользователя %d: %s", assist.userID, libraryID, assist.userID)
	//} else {
	//	logger.Debug("LibraryId не найден для пользователя %d (будет создан при загрузке файлов)", assist.userID, assist.userID)
	//}

	// Используем respId как ключ (один пользователь может иметь несколько диалогов)
	m.responders.Store(respId, user)

	// Уведомляем ожидающие горутины о создании респондента
	m.responders.Store(respId, user)

	return m.convertToModelRespModel(user), nil
}

// GetCh получает канал для респондента (реализация model.UniversalModel)
func (m *Model) GetCh(respId uint64) (*model.Ch, error) {
	return model.GetChannel(
		respId,
		m.ctx,
		&m.waitChannels,
		&m.responders,
		func(val any) (*model.Ch, error) {
			respModel := val.(*RespModel)
			return model.ExtractChannelWithPriority(respModel)
		},
	)
}

// GetRespIdBydialogID получает ID респондента по ID диалога (реализация model.UniversalModel)
func (m *Model) GetRespIdByDialogID(dialogID uint64) (uint64, error) {
	return model.GetRespIdBydialogIDUniversal(dialogID, &m.responders)
}

// SaveAllContextDuringExit сохраняет контекст при выходе (реализация model.UniversalModel)
func (m *Model) SaveAllContextDuringExit() {
	m.responders.Range(func(key, value any) bool {
		respModel := value.(*RespModel)

		if respModel.Chan != nil {
			dialogID := respModel.Chan.DialogID

			// Сохраняем conversation_id (если есть)
			if respModel.ConversationId != "" {
				contextObj := map[string]any{
					"conversation_id": respModel.ConversationId,
				}

				contextJSON, err := json.Marshal(contextObj)
				if err != nil {
					//logger.Error("Ошибка сериализации conversation_id для dialogID %d: %v", dialogID, err)
				} else {
					err = m.db.SaveContext(dialogID, commdom.ProviderMistral, contextJSON)
					if err != nil {
						//logger.Error("Ошибка сохранения conversation_id для dialogID %d: %v", dialogID, err)
					}
				}
			}

			// Сохраняем контекст сообщений (если есть)
			if respModel.Context != nil && len(respModel.Context.Messages) > 0 {
				// Сохраняем в простом json.RawMessage формате
				jsonData, err := json.Marshal(respModel.Context.Messages)
				if err != nil {
					//logger.Error("Ошибка сериализации контекста диалога %d: %v", dialogID, err)
				} else {
					if err := m.db.SaveDialog(dialogID, jsonData); err != nil {
						//logger.Error("Не удалось сохранить контекст диалога %d: %v", dialogID, err)
					}
				}
			}
		}

		return true
	})
}

// CleanDialogData очищает данные конкретного диалога (реализация model.UniversalModel)
func (m *Model) CleanDialogData(dialogID uint64) {
	// Ищем responder по dialogID в Chan
	m.responders.Range(func(key, value any) bool {
		respModel := value.(*RespModel)

		if respModel.Chan != nil && respModel.Chan.DialogID == dialogID {
			// Очищаем контекст этого диалога
			respModel.Context = nil
			return false // Прекращаем поиск
		}
		return true // Продолжаем поиск
	})
}

// saveConversationId сохраняет conversation_id в БД (или удаляет если пустой)
func (m *Model) saveConversationId(dialogID uint64, conversationId string) {
	if conversationId == "" {
		// Удаляем conversation_id из БД (сброс)
		contextObj := map[string]any{
			"conversation_id": "",
		}

		contextJSON, err := json.Marshal(contextObj)
		if err != nil {
			//logger.Error("Ошибка сериализации пустого conversation_id для dialogID %d: %v", dialogID, err)
			return
		}

		err = m.db.SaveContext(dialogID, commdom.ProviderMistral, contextJSON)
		if err != nil {
			//logger.Error("Ошибка удаления conversation_id для dialogID %d: %v", dialogID, err)
		}
		return
	}

	contextObj := map[string]any{
		"conversation_id": conversationId,
	}

	contextJSON, err := json.Marshal(contextObj)
	if err != nil {
		//logger.Error("Ошибка сериализации conversation_id для dialogID %d: %v", dialogID, err)
		return
	}

	err = m.db.SaveContext(dialogID, commdom.ProviderMistral, contextJSON)
	if err != nil {
		//logger.Error("Ошибка сохранения conversation_id для dialogID %d: %v", dialogID, err)
	}
}

// TranscribeAudio обёртка
func (m *Model) TranscribeAudio(_ uint32, audioData []byte, fileName string) (string, error) {
	return m.transcribeAudioFile(audioData, fileName)
}

// TranscribeAudio транскрибирует аудио файл используя Mistral Audio Transcription API
func (m *Model) transcribeAudioFile(audioData []byte, fileName string) (string, error) {
	if len(audioData) == 0 {
		return "", fmt.Errorf("пустые аудиоданные")
	}

	if m.client == nil {
		return "", fmt.Errorf("mistral client не инициализирован")
	}

	// Формируем multipart request для отправки аудио файла
	var requestBody bytes.Buffer
	writer := multipart.NewWriter(&requestBody)
	defer func() {
		if err := writer.Close(); err != nil {
			//logger.Error("TranscribeAudio: ошибка закрытия writer: %v", err)
		}
	}()

	if err := writer.WriteField("model", "voxtral-mini-latest"); err != nil {
		return "", fmt.Errorf("ошибка добавления поля model: %w", err)
	}

	// Добавляем аудио файл
	part, err := writer.CreateFormFile("file", fileName)
	if err != nil {
		return "", fmt.Errorf("ошибка создания form file для аудио: %w", err)
	}

	if _, err := part.Write(audioData); err != nil {
		return "", fmt.Errorf("ошибка записи аудио данных: %w", err)
	}

	// Закрываем writer перед отправкой запроса
	if err := writer.Close(); err != nil {
		return "", fmt.Errorf("ошибка закрытия writer: %w", err)
	}

	// Отправляем запрос на Mistral API
	req, err := http.NewRequestWithContext(m.ctx, http.MethodPost, mode.MistralBaseURL+"/audio/transcriptions", &requestBody)
	if err != nil {
		return "", fmt.Errorf("ошибка создания HTTP запроса: %w", err)
	}

	// Используем x-api-key заголовок согласно документации Mistral
	req.Header.Set("x-api-key", m.client.apiKey)
	req.Header.Set("Content-Type", writer.FormDataContentType())

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("ошибка отправки запроса на Mistral: %w", err)
	}
	defer func() {
		if err := resp.Body.Close(); err != nil {
			//logger.Error("TranscribeAudio: ошибка закрытия response body: %v", err)
		}
	}()

	// Читаем ответ
	responseBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("ошибка чтения ответа: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("ошибка API Mistral (статус %d): %s", resp.StatusCode, string(responseBody))
	}

	// Парсим ответ
	var result struct {
		Text string `json:"text"`
	}

	if err := json.Unmarshal(responseBody, &result); err != nil {
		return "", fmt.Errorf("ошибка парсинга ответа Mistral: %w", err)
	}

	if result.Text == "" {
		return "", fmt.Errorf("Mistral вернул пустой текст транскрипции")
	}

	//logger.Debug("TranscribeAudio: успешно транскрибировано аудио, длина текста: %d символов", len(result.Text))
	return result.Text, nil
}

// DeleteTempFile удаляет загруженный файл из Mistral Files API
// Используется для очистки временных файлов после обработки
func (m *Model) DeleteTempFile(fileID string) error {
	if m.client == nil {
		return fmt.Errorf("mistral client не инициализирован")
	}

	if fileID == "" {
		return fmt.Errorf("fileID не может быть пустым")
	}

	err := m.client.DeleteFile(fileID)
	if err != nil {
		//logger.Error("DeleteTempFile: ошибка удаления файла %s: %v", fileID, err)
		return err
	}

	//logger.Debug("DeleteTempFile: файл %s успешно удалён", fileID)
	return nil
}

// CleanUp запускает фоновую очистку устаревших респондеров (реализация model.UniversalModel)
func (m *Model) CleanUp() {
	ticker := time.NewTicker(15 * time.Minute)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			now := time.Now()

			m.responders.Range(func(key, value any) bool {
				responder := value.(*RespModel)
				ttlExpired := responder.TTL.Before(now)

				respId, ok := key.(uint64)
				if !ok {
					//logger.Error("Некорректный тип ключа: %T, ожидался uint64", key)
					return true
				}

				if ttlExpired {
					// Удаляем весь RespModel (вместе с Context)
					if responder.Cancel != nil {
						responder.Cancel()
					}
					m.closeResponderChannels(responder)
					m.responders.Delete(respId)
				}
				// Отдельная очистка Context не нужна - он удаляется вместе с RespModel

				return true
			})

		case <-m.ctx.Done():
			return
		}
	}
}

// Shutdown корректно завершает работу модели (реализация model.UniversalModel)
func (m *Model) Shutdown(shutCh chan<- com.LogMsg) {
	m.shutdownOnce.Do(func() {
		shutCh <- com.LogMsg{
			Msg: "начало shutdown",
			Mod: "MistralModel",
			Log: 0, // 0 - Info
			UID: 0,
		}

		if m.cancel != nil {
			m.cancel()
		}

		if m.client != nil {
			m.client.Shutdown()
		}

		if m.realtime != nil {
			m.realtime.CloseAll()
		}

		m.cleanupAllResponders()
		m.cleanupWaitChannels()

		shutCh <- com.LogMsg{
			Msg: "модуль успешно завершил работу",
			Mod: "MistralModel",
			Log: 0, // 0 - Info
			UID: 0,
		}
	})
}

func (m *Model) convertToModelRespModel(internal *RespModel) *model.RespModel {
	// Создаем map с одним каналом для совместимости
	chanMap := make(map[uint64]*model.Ch)
	if internal.Chan != nil {
		// Используем dialogID как ключ
		chanMap[internal.Chan.DialogID] = internal.Chan
	}

	return &model.RespModel{
		Ctx:      internal.Ctx,
		Cancel:   internal.Cancel,
		Chan:     chanMap,
		TTL:      internal.TTL,
		Assist:   internal.Assist,
		RespName: internal.RespName,
		Services: model.Services{
			Listener:   &internal.Services.Listener,
			Respondent: &internal.Services.Respondent,
		},
	}
}

func (m *Model) closeResponderChannels(respModel *RespModel) {
	model.CloseResponderChannelsUniversal(respModel)
}

func (m *Model) cleanupAllResponders() {
	model.CleanupAllRespondersUniversal(
		&m.responders,
		func(val any) {
			if respModel, ok := val.(*RespModel); ok && respModel.Cancel != nil {
				respModel.Cancel()
			}
		},
		func(val any) {
			if respModel, ok := val.(*RespModel); ok {
				m.closeResponderChannels(respModel)
			}
		},
	)
}

func (m *Model) cleanupWaitChannels() {
	deletedCount := model.CleanupWaitChannelsUniversal(&m.waitChannels, &m.responders)
	if deletedCount > 0 {
		//logger.Debug("Очищено %d wait channels", deletedCount)
	}
}

// SetUniversalModel устанавливает UniversalModel для доступа к DecompressModelData
func (m *Model) SetUniversalModel(um *create.UniversalModel) {
	m.universalModel = um
}

// InvalidateUserAgentConfigCache инвалидирует кэш конфигурации модели для пользователя
func (m *Model) InvalidateUserAgentConfigCache(userID uint32) {
	var invalidatedCount int
	m.responders.Range(func(key, value any) bool {
		respModel := value.(*RespModel)
		if respModel.Assist.UserID == userID {
			m.responders.Delete(key)
			invalidatedCount++
		}
		return true
	})
	if invalidatedCount > 0 {
		//logger.Debug("Инвалидирован кэш конфигурации модели для userID=%d (удалено %d респондентов)", userID, invalidatedCount)
	}
}

// DisconnectUser выполняет graceful завершение всех активных сессий пользователя:
// отменяет контексты всех респондентов и удаляет их из кэша.
// Mistral не поддерживает realtime-сессии.
func (m *Model) DisconnectUser(userID uint32) {
	m.responders.Range(func(key, value any) bool {
		respModel := value.(*RespModel)
		if respModel.Assist.UserID == userID {
			if respModel.Cancel != nil {
				respModel.Cancel()
			}
			m.responders.Delete(key)
		}
		return true
	})
}

func (m *Model) UpdateModelsListByProvider(ctx context.Context, union commdom.Union, apiKey string) ([]commdom.ProviderModel, error) {
	if union.Provider != commdom.ProviderMistral {
		return nil, fmt.Errorf("неверный провайдер для Mistral модели: %s", union.Provider.String())
	}
	// STT/TTS каталоги являются capability-каталогами: legacy-таблицы
	// realtime_models и gpt_models для них не подходят. Возвращаем их
	// непосредственно фронтенду, без записи в БД.
	res, err := provider_catalog.SyncProviderModels(ctx, m.db, union, apiKey)
	if err != nil || !union.ModelType.IsRealtime() {
		return res.Models, err
	}
	if len(res.Models) == 0 {
		return nil, fmt.Errorf("для Mistral не получены realtime модели")
	}

	// The legacy realtime_models table stores only the realtime LLM model.
	// STT and TTS are capabilities of the same Mistral voice configuration and
	// must be returned to the caller, but must not be inserted into that table.
	sttNames, ttsNames, voiceErr := provider_catalog.NewClient().FetchMistralVoiceModels(ctx, apiKey)
	if voiceErr != nil {
		return nil, fmt.Errorf("не удалось получить STT/TTS модели Mistral: %w", voiceErr)
	}
	if len(sttNames) == 0 || len(ttsNames) == 0 {
		return nil, fmt.Errorf("для Mistral не получены STT или TTS модели")
	}
	// The frontend receives one combined configuration object per realtime
	// model: realtime ID/name plus the available STT/TTS model names.
	sttModel := sttNames[0]
	ttsModel := ttsNames[0]
	for i := range res.Models {
		res.Models[i].STT = sttModel
		res.Models[i].TTS = ttsModel
	}
	return res.Models, nil
}
