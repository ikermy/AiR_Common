package mode

import "github.com/ikermy/AiR_Common/pkg/common"

const (
	ErrorTimeOutDurationForAssistAnswer = 3 // Если в сообщении есть файлы они могут долго обрабатываться
	// BatchSize Endpoint
	BatchSize         = 100
	TimePeriodicFlush = 60
)

var (
	ProductionMode = false // Флаг, указывающий на режим продакшн

	TestAnswer = false // Тестовый режим, когда ответ на вопрос возвращается сразу, без обращения к модели
	AudioMsg   = false // Разрешает принимать аудио сообщения в диалоге

	CarpinteroCh   = make(chan common.CarpCh, 1) // Канал для передачи уведомлений
	Event          = make(chan uint64, 1)        // Канал для передачи Id диалога при отключении клиента
	MailServerPort string
	CarpinteroPort string
	CarpinteroHost string
)

func SetTestMode(enabled bool) {
	TestAnswer = enabled
}

func SetAudioMode(enabled bool) {
	AudioMsg = enabled
}
