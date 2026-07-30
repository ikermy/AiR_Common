package mistral

import (
	"context"
	"fmt"
	"sync"
)

// RealtimeManager owns Mistral voice sessions independently from the text
// responder map. It is the boundary for the future STT transport pump.
type RealtimeManager struct {
	ctx      context.Context
	sessions sync.Map // map[uint64]*MistralRealtimeSession
}

func NewRealtimeManager(ctx context.Context) *RealtimeManager {
	if ctx == nil {
		ctx = context.Background()
	}
	return &RealtimeManager{ctx: ctx}
}

// Start creates or returns the existing session for respID.
func (m *RealtimeManager) Start(userID uint32, dialogID, respID uint64) (*MistralRealtimeSession, error) {
	if m == nil {
		return nil, fmt.Errorf("Mistral realtime manager is nil")
	}
	if respID == 0 {
		return nil, fmt.Errorf("некорректный respID")
	}
	if existing, ok := m.sessions.Load(respID); ok {
		return existing.(*MistralRealtimeSession), nil
	}
	session := NewRealtimeSession(m.ctx, userID, dialogID, respID)
	actual, loaded := m.sessions.LoadOrStore(respID, session)
	if loaded {
		session.Close()
		return actual.(*MistralRealtimeSession), nil
	}
	return session, nil
}

func (m *RealtimeManager) Get(respID uint64) (*MistralRealtimeSession, bool) {
	if m == nil {
		return nil, false
	}
	value, ok := m.sessions.Load(respID)
	if !ok {
		return nil, false
	}
	return value.(*MistralRealtimeSession), true
}

func (m *RealtimeManager) Close(respID uint64) {
	if m == nil {
		return
	}
	if value, ok := m.sessions.LoadAndDelete(respID); ok {
		value.(*MistralRealtimeSession).Close()
	}
}

func (m *RealtimeManager) CloseAll() {
	if m == nil {
		return
	}
	m.sessions.Range(func(key, value any) bool {
		m.sessions.Delete(key)
		value.(*MistralRealtimeSession).Close()
		return true
	})
}
