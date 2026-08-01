package mistral

import (
	"testing"

	"github.com/ikermy/air_common/pkg/comdb"
	"github.com/ikermy/air_common/pkg/model"
)

type savedDialog struct {
	creator  comdb.CreatorType
	dialogID uint64
	text     string
}

type mockDialogSaver struct {
	messages []savedDialog
}

func (m *mockDialogSaver) SaveDialog(creator comdb.CreatorType, dialogID uint64, resp *model.AssistResponse) {
	m.messages = append(m.messages, savedDialog{
		creator:  creator,
		dialogID: dialogID,
		text:     resp.Message,
	})
}

func TestSaveRealtimeTranscriptUsesDialogSaverAndUpdatesContext(t *testing.T) {
	saver := &mockDialogSaver{}
	m := &Model{dialogSaver: saver}
	session := NewRealtimeSession(nil, 7, 42, 99)
	defer session.Close()
	m.responders.Store(uint64(1), &RespModel{
		Chan:    &model.Ch{DialogID: 42},
		Context: &DialogContext{},
	})

	m.saveRealtimeTranscript(session, "привет", "Бонжур бля..")

	if len(saver.messages) != 2 {
		t.Fatalf("saved messages = %d, want 2", len(saver.messages))
	}

	want := []savedDialog{
		{creator: comdb.SpeechRealTimeUser, dialogID: 42, text: "привет"},
		{creator: comdb.SpeechRealTimeAI, dialogID: 42, text: "Бонжур бля.."},
	}
	for i, message := range saver.messages {
		if message != want[i] {
			t.Errorf("message[%d] = %+v, want %+v", i, message, want[i])
		}
	}

	var contextMessages []Message
	m.responders.Range(func(_, value any) bool {
		contextMessages = append(contextMessages, value.(*RespModel).Context.Messages...)
		return false
	})
	if len(contextMessages) != 2 {
		t.Fatalf("context messages = %d, want 2", len(contextMessages))
	}
	if contextMessages[0].Type != "user" || contextMessages[0].Content != "привет" {
		t.Errorf("user context message = %+v", contextMessages[0])
	}
	if contextMessages[1].Type != "assistant" || contextMessages[1].Content != "Бонжур бля.." {
		t.Errorf("assistant context message = %+v", contextMessages[1])
	}
}
