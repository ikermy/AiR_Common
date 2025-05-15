package mode

const (
	ErrorTimeOutDurationForAssistAnswer = 3
	TimePeriodicFlush                   = 60
)

var (
	TestAnswer = false

	// BatchSize Endpoint
	BatchSize = 100
)

func SetTestMode(enabled bool) {
	TestAnswer = enabled
}
