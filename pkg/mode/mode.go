package mode

import (
	"os"
	"strconv"
	"time"

	"github.com/ikermy/AiR_Common/pkg/com"
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
	MistralBaseURL          = "https://api.mistral.ai/v1"
	MistralAgentsBaseURL    = MistralBaseURL + "/agents"
	MistralAgentsURL        = MistralAgentsBaseURL + "/completions"
	MistralConversationsURL = MistralBaseURL + "/conversations"
	// Google API settings
	GoogleAgentsURL = "https://generativelanguage.googleapis.com/v1beta"
	// OpenAI API settings
	OpenAIAgentsURL = "https://api.openai.com/v1"
)

var (
	TestAnswer   = false                     // Тестовый режим, когда текстовый ответ на вопрос возвращается сразу, без обращения к модели
	TextMsg      = false                     // Разрешает принимать текстовые сообщения в диалоге
	AudioMsg     = false                     // Разрешает принимать аудио сообщения в диалоге
	VoiceCall    = false                     // Разрешает принимать голосовые вызовы
	CarpinteroCh = make(chan com.CarpCh, 1)  // Канал для передачи уведомлений
	Event        = make(chan uint64, 1)      // Канал для передачи Id диалога при отключении клиента
	InstantCh    = make(chan com.InstMsg, 1) // Канал для передачи мгновенных сообщений в панель управления
	RealHost     string

	// Operator settings
	// Таймаут ожидания ПЕРВОГО ответа оператора в секундах
	// После первого ответа операторский режим становится постоянным (без таймера)
	OperatorResponseTimeout = 120

	// Тайм-аут на операции с БД (в секундах)
	SqlTimeToCancel = 5 * time.Second
	UserModelTTl    = 5 * time.Minute

	// Логирование — инициализируются через InitFromEnv()
	LogLevel = "info" // LOG_LEVEL: debug | info | warn | error
	LogPath  = ""     // LOG_PATH: путь к файлу лога, не используется в режиме logger.StdOut()
)

func SetTextMode(enabled bool) {
	TextMsg = enabled
}
func SetVoiceCall(enabled bool) {
	VoiceCall = enabled
}
func SetTestMode(enabled bool) {
	TestAnswer = enabled
}
func SetAudioMode(enabled bool) {
	AudioMsg = enabled
}
func SetRealHost(host string) {
	RealHost = host
}

func SetUserModelTTL(ttl time.Duration) {
	UserModelTTl = ttl
}

// InitFromEnv загружает инфраструктурные настройки из переменных окружения.
//
// Критичные значения (WEB_LAND_PORT, REAL_URL) имеют дефолты и никогда не вызовут fatal.
// fatal предназначен для будущих настроек без разумного дефолта.
// Некритичные порты (Oper, CRM, Demo, Pay) остаются пустыми — их отсутствие означает
// недоступность соответствующего сервиса.
//
// Пример: mode.InitFromEnv(logger.Fatalf)
func InitFromEnv(fatal func(format string, args ...any)) {
	// Домен — дефолт: localhost (для dev-окружения)
	RealHost = envVal("REAL_URL", "localhost")

	// TTL модели пользователя (минуты) — дефолт 1440 (24 часа)
	if v := os.Getenv("GLOB_USER_MODEL_TTL"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n <= 0 {
			fatal("mode.InitFromEnv: GLOB_USER_MODEL_TTL содержит некорректное значение: %q", v)
		} else {
			UserModelTTl = time.Duration(n) * time.Minute
		}
	}

	// Логирование — дефолты из var
	LogLevel = envVal("LOG_LEVEL", LogLevel)
	LogPath = envVal("LOG_PATH", LogPath)

	// Полный URL хоста (для S3, action_handler и т.п.).
	// Если REAL_HOST_URL задан — используем его напрямую,
	// иначе RealHost остаётся как hostname из REAL_URL.
	if v := os.Getenv("REAL_HOST_URL"); v != "" {
		RealHost = v
	}
}

// envVal возвращает значение переменной окружения key,
// или def если переменная не задана или пуста.
func envVal(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
