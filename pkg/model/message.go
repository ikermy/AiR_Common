package model

import (
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/ikermy/AiR_Common/pkg/logger"
	"github.com/sashabaranov/go-openai"
)

// Message представляет сообщение в системе
type Message struct {
	Operator  bool // true, если сообщение для оператора, false - для ассистента
	Type      string
	Content   AssistResponse
	Name      string
	Timestamp time.Time
	Files     []FileUpload `json:"files,omitempty"`
}

// FileUpload представляет файл для отправки для code interpreter
type FileUpload struct {
	Name     string    `json:"name"`
	Content  io.Reader `json:"-"`
	MimeType string    `json:"mime_type"`
}

// NewMessage создает новое сообщение
func (m *Models) NewMessage(operator bool, msgType string, content *AssistResponse, name *string, files ...FileUpload) Message {
	return Message{
		Operator:  operator,
		Type:      msgType,
		Content:   *content,
		Name:      *name,
		Timestamp: time.Now(),
		Files:     files,
	}
}

// createMsg создает простое сообщение для OpenAI
func createMsg(text *string) openai.MessageRequest {
	lastMessage := openai.MessageRequest{
		Role:    "user",
		Content: *text,
	}
	return lastMessage
}

// createMsgWithFiles создает сообщение с файлами для OpenAI
func createMsgWithFiles(text *string, fileNames []string) openai.MessageRequest {
	msg := openai.MessageRequest{
		Role:    "user",
		Content: *text,
	}
	if len(fileNames) == 1 {
		*text += fmt.Sprintf("\n\nОБЯЗАТЕЛЬНО используй file_search для анализа содержимого этого файла: %s. И если потребуется code_interpreter. ИГНОРИРУЙ все остальные файлы в векторном хранилище - это важно!", fileNames[0])
	} else {
		*text += fmt.Sprintf("\n\nОБЯЗАТЕЛЬНО используй file_search для анализа содержимого этих файлов: %s. И если потребуется code_interpreter. ИГНОРИРУЙ все остальные файлы в векторном хранилище - это важно!", strings.Join(fileNames, ", "))
	}
	msg.Content = *text

	return msg
}

// safeClose закрывает канал и обрабатывает панику, если канал уже закрыт
func safeClose(ch chan Message) {
	defer func() {
		if r := recover(); r != nil {
			logger.Error("Паника при закрытии канала: %v", r)
		}
	}()
	close(ch)
}
