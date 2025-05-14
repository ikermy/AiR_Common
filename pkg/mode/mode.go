package mode

const (
	ErrorTimeOutDurationForAssistAnswer = 3
)

var TestAnswer = false

func SetTestMode(enabled bool) {
	TestAnswer = enabled
}
