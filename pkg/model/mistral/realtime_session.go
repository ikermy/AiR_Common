package mistral

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/ikermy/air_common/pkg/model"
	"github.com/ikermy/air_common/pkg/model/commdom"
)

// STTTransport is implemented by a concrete Voxtral streaming adapter
// (WebSocket, HTTP/2 chunked or another official transport).
type STTTransport interface {
	Run(ctx context.Context, audio <-chan []byte, onTranscript func(text string, final bool) error) error
}

type MistralRealtimeMetrics struct {
	DroppedAudio      atomic.Uint64
	Interruptions     atomic.Uint64
	STTErrors         atomic.Uint64
	LLMErrors         atomic.Uint64
	TTSErrors         atomic.Uint64
	Active            atomic.Bool
	FinalTranscriptAt atomic.Int64
	FirstLLMTokenAt   atomic.Int64
	FirstTTSAudioAt   atomic.Int64
}

const (
	mistralAudioInBuffer  = 100
	mistralAudioOutBuffer = 100
)

// MistralRealtimeSession is the provider-independent session coordinator.
// The STT transport is attached separately once the provider protocol is
// selected; this type owns cancellation, turn invalidation and audio queues.
type MistralRealtimeSession struct {
	ctx    context.Context
	cancel context.CancelFunc

	userID          uint32
	dialogID        uint64
	respID          uint64
	RealtimeModel   string
	Config          *commdom.MistralRealtimeVAD
	Greeting        *string
	InitialGreeting *bool

	AudioIn       chan []byte
	AudioOut      chan []byte
	DrainPlayback chan struct{}
	Errors        chan error

	turns        TurnGuard
	droppedAudio atomic.Uint64

	closeOnce           sync.Once
	stateMu             sync.RWMutex
	closed              atomic.Bool
	sttMu               sync.Mutex
	sttStarted          bool
	turnMu              sync.Mutex
	turnCtx             context.Context
	turnCancel          context.CancelFunc
	llmMu               sync.Mutex
	chunkMu             sync.Mutex
	chunker             SentenceChunker
	generating          atomic.Bool
	transcriptMu        sync.Mutex
	lastFinalTranscript string
	metrics             MistralRealtimeMetrics
	callbackMu          sync.RWMutex
	disconnect          func(uint64)
	eventsMu            sync.RWMutex
	events              map[chan model.RealtimeEvent]struct{}
}

// WithLLM serializes updates to one Mistral conversation.
func (s *MistralRealtimeSession) WithLLM(fn func() error) error {
	if s == nil || fn == nil {
		return fmt.Errorf("не задана realtime-сессия или LLM callback")
	}
	s.llmMu.Lock()
	defer s.llmMu.Unlock()
	return fn()
}

// PushText converts an LLM delta into TTS-ready sentence chunks.
func (s *MistralRealtimeSession) PushText(delta string, final bool) []string {
	s.chunkMu.Lock()
	defer s.chunkMu.Unlock()
	if final {
		chunks := s.chunker.Push(delta)
		return append(chunks, s.chunker.Flush()...)
	}
	return s.chunker.Push(delta)
}

func (s *MistralRealtimeSession) SubscribeEvents() (<-chan model.RealtimeEvent, error) {
	if s == nil || s.closed.Load() {
		return nil, fmt.Errorf("Mistral realtime session is closed")
	}
	ch := make(chan model.RealtimeEvent, 64)
	s.eventsMu.Lock()
	s.events[ch] = struct{}{}
	s.eventsMu.Unlock()
	return ch, nil
}

func (s *MistralRealtimeSession) UnsubscribeEvents(sub <-chan model.RealtimeEvent) {
	if s == nil || sub == nil {
		return
	}
	s.eventsMu.Lock()
	defer s.eventsMu.Unlock()
	for ch := range s.events {
		if ch == sub {
			delete(s.events, ch)
			close(ch)
			return
		}
	}
}

func (s *MistralRealtimeSession) PublishEvent(event model.RealtimeEvent) {
	if s == nil {
		return
	}
	s.eventsMu.RLock()
	defer s.eventsMu.RUnlock()
	for ch := range s.events {
		select {
		case ch <- event:
		default:
		}
	}
}

// StartSTT attaches a transport pump to the session. A final transcript
// starts a new turn and invalidates audio from the previous response.
func (s *MistralRealtimeSession) StartSTT(transport STTTransport, onTranscript func(text string, turnID uint64) error) error {
	if s == nil || transport == nil {
		return fmt.Errorf("не задан STT transport")
	}
	if onTranscript == nil {
		return fmt.Errorf("не задан transcript callback")
	}
	s.sttMu.Lock()
	if s.sttStarted {
		s.sttMu.Unlock()
		return fmt.Errorf("STT transport уже запущен")
	}
	s.sttStarted = true
	s.sttMu.Unlock()
	go func() {
		err := transport.Run(s.ctx, s.AudioIn, func(text string, final bool) error {
			if !final || text == "" {
				return nil
			}
			if !s.acceptFinalTranscript(text) {
				return nil
			}
			// A new user utterance interrupts any greeting/LLM/TTS work from
			// the previous turn before the new turn is created.
			if s.CurrentTurn() != 0 {
				s.Interrupt()
			}
			turnID := s.BeginTurn()
			s.PublishEvent(model.RealtimeEvent{Type: "transcript", Text: text})

			if err := onTranscript(text, turnID); err != nil {
				return err
			}

			return nil
		})
		if err != nil && s.ctx.Err() == nil {
			s.metrics.STTErrors.Add(1)
			s.PublishEvent(model.RealtimeEvent{Type: "error", Text: "Mistral STT transport завершился с ошибкой", Err: err})
			select {
			case s.Errors <- err:
			default:
			}
			s.callbackMu.RLock()
			callback := s.disconnect
			s.callbackMu.RUnlock()
			if callback != nil {
				callback(s.respID)
			}
		}
	}()
	return nil
}

func (s *MistralRealtimeSession) acceptFinalTranscript(text string) bool {
	s.transcriptMu.Lock()
	defer s.transcriptMu.Unlock()
	text = strings.TrimSpace(text)
	if text == "" || text == s.lastFinalTranscript {
		return false
	}
	s.lastFinalTranscript = text
	s.metrics.FinalTranscriptAt.Store(time.Now().UnixNano())
	return true
}

// NewRealtimeSession creates a session with bounded audio queues.
func NewRealtimeSession(parent context.Context, userID uint32, dialogID, respID uint64) *MistralRealtimeSession {
	if parent == nil {
		parent = context.Background()
	}
	ctx, cancel := context.WithCancel(parent)
	session := &MistralRealtimeSession{
		ctx:           ctx,
		cancel:        cancel,
		userID:        userID,
		dialogID:      dialogID,
		respID:        respID,
		AudioIn:       make(chan []byte, mistralAudioInBuffer),
		AudioOut:      make(chan []byte, mistralAudioOutBuffer),
		DrainPlayback: make(chan struct{}, 1),
		Errors:        make(chan error, 4),
		events:        make(map[chan model.RealtimeEvent]struct{}),
	}
	session.metrics.Active.Store(true)
	return session
}

func (s *MistralRealtimeSession) Context() context.Context         { return s.ctx }
func (s *MistralRealtimeSession) UserID() uint32                   { return s.userID }
func (s *MistralRealtimeSession) DialogID() uint64                 { return s.dialogID }
func (s *MistralRealtimeSession) RespID() uint64                   { return s.respID }
func (s *MistralRealtimeSession) DroppedAudio() uint64             { return s.droppedAudio.Load() }
func (s *MistralRealtimeSession) Metrics() *MistralRealtimeMetrics { return &s.metrics }

func (s *MistralRealtimeSession) MarkFirstLLMToken() {
	if s != nil {
		s.metrics.FirstLLMTokenAt.CompareAndSwap(0, time.Now().UnixNano())
	}
}

func (s *MistralRealtimeSession) MarkFirstTTSAudio() {
	if s != nil {
		s.metrics.FirstTTSAudioAt.CompareAndSwap(0, time.Now().UnixNano())
	}
}

// BeginTurn invalidates all work belonging to the previous user utterance.
func (s *MistralRealtimeSession) BeginTurn() uint64 {
	s.turnMu.Lock()
	if s.turnCancel != nil {
		s.turnCancel()
	}
	s.turnCtx, s.turnCancel = context.WithCancel(s.ctx)
	s.turnMu.Unlock()
	turnID := s.turns.Begin()
	// A sentence must never cross a user turn boundary. In particular, after
	// an interruption the next LLM response must not flush text left by the
	// interrupted response into TTS.
	s.chunkMu.Lock()
	s.chunker.Flush()
	s.chunker = SentenceChunker{}
	s.chunkMu.Unlock()
	return turnID
}

// TurnContext is cancelled as soon as the turn is interrupted or replaced.
// TTS and other per-turn network operations must use it instead of the
// session context so interruption stops the underlying stream promptly.
func (s *MistralRealtimeSession) TurnContext(turnID uint64) (context.Context, bool) {
	if s == nil || !s.IsCurrentTurn(turnID) {
		return nil, false
	}
	s.turnMu.Lock()
	defer s.turnMu.Unlock()
	if s.turnCtx == nil || !s.IsCurrentTurn(turnID) {
		return nil, false
	}
	return s.turnCtx, true
}

func (s *MistralRealtimeSession) IsCurrentTurn(turnID uint64) bool {
	return s.turns.IsCurrent(turnID)
}

func (s *MistralRealtimeSession) CurrentTurn() uint64 {
	return s.turns.Current()
}

func (s *MistralRealtimeSession) AudioOutput() <-chan []byte   { return s.AudioOut }
func (s *MistralRealtimeSession) DrainOutput() <-chan struct{} { return s.DrainPlayback }
func (s *MistralRealtimeSession) Generating() *atomic.Bool     { return &s.generating }

func (s *MistralRealtimeSession) SetDisconnectCallback(callback func(uint64)) {
	s.callbackMu.Lock()
	s.disconnect = callback
	s.callbackMu.Unlock()
}

// Interrupt cancels output from the current turn and signals playback drain.
func (s *MistralRealtimeSession) Interrupt() {
	s.metrics.Interruptions.Add(1)
	s.turnMu.Lock()
	if s.turnCancel != nil {
		s.turnCancel()
		s.turnCancel = nil
		s.turnCtx = nil
	}
	s.turnMu.Unlock()
	s.generating.Store(false)
	s.PublishEvent(model.RealtimeEvent{Type: "interrupted", Text: "пользователь перебил ответ"})
	for {
		select {
		case <-s.AudioOut:
		default:
			select {
			case s.DrainPlayback <- struct{}{}:
			default:
			}
			return
		}
	}
}

// SendAudio enqueues microphone PCM without blocking the capture loop.
func (s *MistralRealtimeSession) SendAudio(pcm []byte) error {
	if s == nil {
		return fmt.Errorf("Mistral realtime session is closed")
	}
	s.stateMu.RLock()
	defer s.stateMu.RUnlock()
	if s.closed.Load() {
		return fmt.Errorf("Mistral realtime session is closed")
	}
	if len(pcm) == 0 {
		return nil
	}
	// The capture loop is free to reuse its input buffer after SendAudio
	// returns. Keep an owned copy in the bounded queue.
	pcmCopy := append([]byte(nil), pcm...)
	select {
	case s.AudioIn <- pcmCopy:
		return nil
	case <-s.ctx.Done():
		return s.ctx.Err()
	default:
		s.droppedAudio.Add(1)
		s.metrics.DroppedAudio.Add(1)
		return nil
	}
}

// PublishAudio publishes TTS audio only if it belongs to the current turn.
func (s *MistralRealtimeSession) PublishAudio(turnID uint64, pcm []byte) bool {
	if s == nil || len(pcm) == 0 {
		return false
	}
	s.stateMu.RLock()
	defer s.stateMu.RUnlock()
	if s.closed.Load() || !s.IsCurrentTurn(turnID) {
		return false
	}
	pcmCopy := append([]byte(nil), pcm...)
	select {
	case s.AudioOut <- pcmCopy:
		s.MarkFirstTTSAudio()
		s.PublishEvent(model.RealtimeEvent{Type: "audio_delta", Data: append([]byte(nil), pcmCopy...)})
		return true
	default:
		return false
	}
}

// Close cancels all session work exactly once. Public queues are intentionally
// not closed here: producers may be racing with shutdown, and closing a
// channel concurrently with a send would cause a panic. Consumers use Context
// to observe session termination.
func (s *MistralRealtimeSession) Close() {
	if s == nil {
		return
	}
	s.closeOnce.Do(func() {
		s.stateMu.Lock()
		defer s.stateMu.Unlock()
		s.closed.Store(true)
		s.metrics.Active.Store(false)
		s.turns.Invalidate()
		s.cancel()
		s.eventsMu.Lock()
		for ch := range s.events {
			close(ch)
			delete(s.events, ch)
		}
		s.eventsMu.Unlock()
	})
}
