package mode

import (
	"time"

	"github.com/ikermy/AiR_Common/pkg/common"
)

const (
	IdleDuration                        = 5 // длительность простоя для закрытия SSE
	IdleOperator                        = 5 // длительность простоя для закрытия оператора
	ErrorTimeOutDurationForAssistAnswer = 3 // Если в сообщении есть файлы они могут долго обрабатываться
	// BatchSize Endpoint
	BatchSize         = 100
	TimePeriodicFlush = 60
	// Retry settings
	RetryMaxAttempts = 3 // Максимальное количество повторных попыток
	RetryBaseDelay   = 1 // Базовая задержка между попытками в секундах
	// Mistral API settings
	MistralAgentsURL = "https://api.mistral.ai/v1/agents/completions"
	// Google API settings
	GoogleAgentsURL = "https://generativelanguage.googleapis.com/v1beta"
)

var (
	ProductionMode = false                        // Флаг, указывающий на режим продакшн
	TestAnswer     = false                        // Тестовый режим, когда ответ на вопрос возвращается сразу, без обращения к модели
	AudioMsg       = false                        // Разрешает принимать аудио сообщения в диалоге
	CarpinteroCh   = make(chan common.CarpCh, 1)  // Канал для передачи уведомлений
	Event          = make(chan uint64, 1)         // Канал для передачи Id диалога при отключении клиента
	InstantCh      = make(chan common.InstMsg, 1) // Канал для передачи мгновенных сообщений в панель управления
	MailServerPort string
	CarpinteroPort string
	CarpinteroHost string
	RealHost       string
	// Operator settings
	// Таймаут ожидания ПЕРВОГО ответа оператора в секундах (120 сек = 2 минуты)
	// После первого ответа операторский режим становится постоянным (без таймера)
	OperatorResponseTimeout = 120
	// Тайм-аут на операции с БД (в секундах)
	SqlTimeToCancel = 5 * time.Second
)

func SetTestMode(enabled bool) {
	TestAnswer = enabled
}
func SetAudioMode(enabled bool) {
	AudioMsg = enabled
}
func SetRealHost(host string) {
	RealHost = host
}
