package mode

const (
	ErrorTimeOutDurationForAssistAnswer = 1
	//TimePeriodicFlush                   = 60
)

var (
	TestAnswer = false // Тестовый режим, когда ответ на вопрос возвращается сразу, без обращения к модели
	AudioMsg   = false // Разрешает принимать аудио сообщения в диалоге

	// BatchSize Endpoint
	BatchSize = 100
)

func SetTestMode(enabled bool) {
	TestAnswer = enabled
}

func SetAudioMode(enabled bool) {
	AudioMsg = enabled
}
