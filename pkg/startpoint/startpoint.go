package startpoint

import (
	"fmt"
	"github.com/ikermy/AiR_Common/pkg/comdb"
	"github.com/ikermy/AiR_Common/pkg/logger"
	"github.com/ikermy/AiR_Common/pkg/mode"
	"github.com/ikermy/AiR_Common/pkg/model"
	"strings"
	"time"
)

// Question структура для хранения вопросов пользователя
type Question struct {
	Question []string
	Voice    bool // Флаг, указывающий, что вопрос задан голосом
}

// Answer структура для хранения ответов пользователя
type Answer struct {
	Answer        string
	VoiceQuestion bool // Флаг, указывающий, что вопрос был задан голосом
}

// BotInterface - интерфейс для различных реализаций ботов
type BotInterface interface {
	NewMessage(msgType string, content, name *string) model.Message
	StartBots() error
	StopBot()
}

// EndpointInterface - интерфейс для работы с диалогами
type EndpointInterface interface {
	GetUserAsk(dialogId uint64, respId uint64) []string
	SetUserAsk(dialogId uint64, respId uint64, ask string, askLimit uint32) bool
	SaveDialog(creator comdb.CreatorType, treadId uint64, resp *string)
	Meta(userId uint32, dialogId uint64, meta string, respName string, assistName string, metaAction string)
	FlushAllBatches()
}

// ModelInterface - интерфейс для моделей
type ModelInterface interface {
	Request(modelId string, dialogId uint64, ask *string) (string, error)
	GetCh(respId uint64) (model.Ch, error)
	CleanUp()
}

// Start структура с интерфейсами вместо конкретных типов
type Start struct {
	Mod ModelInterface
	End EndpointInterface
	Bot BotInterface
	//mu  sync.Mutex
}

// New создаёт новый экземпляр Start (бывший startpoint.New)
func New(mod ModelInterface, end EndpointInterface, bot BotInterface) *Start {
	return &Start{
		Mod: mod,
		End: end,
		Bot: bot,
	}
}

func (s *Start) Ask(modelId string, dialogId uint64, arrAsk []string) (string, error) {
	answerCh := make(chan string) // Канал для ответа
	errCh := make(chan error)     // Канал для ошибок
	defer close(answerCh)
	defer close(errCh)

	var ask string
	for _, v := range arrAsk {
		if v != "" {
			ask += v + "\n"
		}
	}

	if ask == "" {
		return "", fmt.Errorf("ASK EMPTY MESSAGE")
	}

	if mode.TestAnswer {
		return "AssistId model " + " resp " + ask, nil
	} // Тестовый ответ

	go func() {
		body, err := s.Mod.Request(modelId, dialogId, &ask)
		if err != nil {
			errCh <- fmt.Errorf("ask error making request: %w", err)
			return
		}
		answerCh <- body
	}()

	timeout := time.After(mode.ErrorTimeOutDurationForAssistAnswer * time.Minute)

	select {
	case body := <-answerCh:
		return body, nil
	case err := <-errCh:
		return "", err
	case <-timeout:
		return "", nil
	}
}

func (s *Start) Respondent(
	u *model.RespModel,
	questionCh chan Question,
	answerCh chan Answer,
	fullQuestCh chan Answer,
	respId uint64,
	treadId uint64,
	errCh chan error,
) {
	var (
		deaf          bool   // Не слушать ввод пользователя до момента получения ответа
		ask           string // Вопрос пользователя
		askTimer      *time.Timer
		VoiceQuestion bool // Флаг, указывающий, что вопрос был задан голосом
	)

	for {
		select {
		case <-u.Ctx.Done():
			logger.Info("Context.Done Respondent %s", u.RespName, u.Assist.UserId)
			return
		case quest, open := <-questionCh: // Ждём ввод
			if !open {
				select {
				case errCh <- fmt.Errorf("'respondent' канал questionCh закрыт"):
				default:
					logger.Warning("'Respondent' не удалось отправить ошибку: канал errCh закрыт или переполнен", u.Assist.UserId)
					return
				}
				continue
			}

			// Проверяю наличие в запросе пользователя сообщения из u.Assist.Metas.Triggers
			if len(u.Assist.Metas.Triggers) > 0 {
				userQuestion := strings.Join(quest.Question, "\n")
				for _, trigger := range u.Assist.Metas.Triggers {
					if strings.Contains(userQuestion, trigger) {
						// Если триггер найден, то уведомляю пользователя в CarpinteroCh
						s.End.Meta(u.Assist.UserId, treadId, "trigger", u.RespName, u.Assist.AssistName, u.Assist.Metas.MetaAction)
					}
				}
			}

			// сохраняю в глобальную переменную
			ask = strings.Join(quest.Question, "\n")
			// Сохраняю информацию о голосовом вопросе
			VoiceQuestion = quest.Voice

			// Добавляю вопрос для контекста
			if s.End.SetUserAsk(treadId, respId, ask, u.Assist.Limit) {
				askTimer = time.NewTimer(time.Duration(u.Assist.Espero) * time.Second) // Жду ещё ввода перед тем как ответить
			} else {
				if askTimer == nil {
					askTimer = time.NewTimer(0) // Инициализируем таймер, если он nil
				} else {
					askTimer.Reset(0) // Сразу отправляю вопрос ассистенту
				}
			}
		}

	inputLoop:
		for {
			if !deaf {
				if askTimer == nil {
					askTimer = time.NewTimer(time.Duration(u.Assist.Espero) * time.Second)
				}

				select {
				case inputStruct, open := <-questionCh:
					ask = strings.Join(inputStruct.Question, "\n")
					// Добавляю вопрос для контекста
					if s.End.SetUserAsk(treadId, respId, ask, u.Assist.Limit) {
						// Перезапускаю таймер
						if !askTimer.Stop() {
							<-askTimer.C // Сбрасываем любой оставшийся сигнал, чтобы избежать гонок
						}
						askTimer.Reset(time.Duration(u.Assist.Espero) * time.Second)
					} else {
						if askTimer == nil {
							askTimer = time.NewTimer(0) // Инициализируем таймер, если он nil
						} else {
							askTimer.Reset(0) // Сразу отправляю вопрос ассистенту
						}
					}

					if !open {
						askTimer.Stop()
						select {
						case errCh <- fmt.Errorf("'respondent' канал questionCh закрыт"):
						default:
							logger.Warning("'Respondent' не удалось отправить ошибку: канал errCh закрыт или переполнен", u.Assist.UserId)
							return
						}
					}

				case <-askTimer.C:
					askTimer.Stop()
					// Sordo
					if u.Assist.Ignore {
						deaf = true
					} else {
						deaf = false
					}
					break inputLoop
				}
			}
		}

		// Отправляем запрос в OpenAI
		userAsk := s.End.GetUserAsk(treadId, respId)
		if strings.TrimSpace(strings.Join(userAsk, "\n")) == "" {
			// Пустой запрос, пропускаем
			continue
		}
		// Сохраняю запрос пользователя для сохранения диалога
		fullAsk := Answer{
			Answer:        strings.Join(userAsk, "\n"),
			VoiceQuestion: VoiceQuestion, // Передаём информацию о голосовом вопросе
		}

		// Проверяю что канал fullQuestCh не закрыт
		select {
		case fullQuestCh <- fullAsk:
		// отправляю вопрос в End.SaveDialog
		default:
			select {
			case errCh <- fmt.Errorf("'respondent' канал fullQuestCh закрыт или переполнен"):
			default:
				logger.Warning("'Respondent' не удалось отправить ошибку: канал errCh закрыт или переполнен", u.Assist.UserId)
			}
		}

		// Отправляю запрос в OpenAI
		answer, err := s.Ask(u.Assist.AssistId, treadId, userAsk)
		// Oyente
		deaf = false

		if err != nil {
			logger.Error("Ошибка запроса к модели: %v", err, u.Assist.UserId)
			continue
		}
		// Если пустой ответ от OpenAI
		if answer == "" {
			continue
		}

		// Проверяю на содержание в ответе цели из u.Assist.Metas.MetaAction
		if u.Assist.Metas.MetaAction != "" {
			if strings.Contains(answer, u.Assist.Metas.MetaAction) {
				s.End.Meta(u.Assist.UserId, treadId, "target", u.RespName, u.Assist.AssistName, u.Assist.Metas.MetaAction)
			}
		}

		// Отправляем ответ вызывающей функции
		answ := Answer{
			Answer: answer,
		}
		//Проверяю что канал answerCh не закрыт
		select {
		case answerCh <- answ:
		default:
			select {
			case errCh <- fmt.Errorf("'respondent' канал answerCh закрыт или переполнен"):
			default:
				logger.Warning("'Respondent' не удалось отправить ошибку: канал errCh закрыт или переполнен", u.Assist.UserId)
			}
		}
	}
}

func (s *Start) StarterRespondent(
	u *model.RespModel,
	questionCh chan Question,
	answerCh chan Answer,
	fullQuestCh chan Answer,
	respId uint64,
	treadId uint64,
) {
	if !u.Services.Respondent {
		u.Services.Respondent = true
		go func() {
			errCh := make(chan error, 1)
			defer func() {
				u.Services.Respondent = false
				close(errCh)
			}()

			s.Respondent(u, questionCh, answerCh, fullQuestCh, respId, treadId, errCh)
			// Проверяем ошибки из канала перед выходом
			select {
			case err := <-errCh:
				if err != nil {
					logger.Error("Ошибка из канала errCh: %v", err, u.Assist.UserId)
				}
			default:
				// Нет ошибок в канале
			}
		}()
	}
}

func (s *Start) StarterListener(start model.StartCh, errCh chan error) {
	if !start.Model.Services.Listener {
		start.Model.Services.Listener = true
		go func() {
			defer func() { start.Model.Services.Listener = false }()
			if err := s.Listener(start.Model, start.Chanel, start.RespId, start.TreadId); err != nil {
				errCh <- err
			}
		}()
	}
}

func (s *Start) Listener(
	u *model.RespModel,
	usrCh model.Ch,
	respId uint64,
	treadId uint64,
) error {
	question := make(chan Question, 1)
	fullQuestCh := make(chan Answer, 1)
	answerCh := make(chan Answer, 1)
	errCh := make(chan error, 1)
	defer close(question)
	defer close(fullQuestCh)
	defer close(answerCh)
	defer close(errCh)

	go s.StarterRespondent(u, question, answerCh, fullQuestCh, respId, treadId)

	for {
		select {
		case err := <-errCh:
			return err // Возвращаем возможные ошибки
		case <-u.Ctx.Done():
			logger.Info("Context.Done Listener %s", u.RespName, u.Assist.UserId)
			return nil
		case msg, clos := <-usrCh.RxCh:
			if !clos {
				logger.Info("Канал RxCh закрыт %s", u.RespName, u.Assist.UserId)
				return nil
			}

			if msg.Type != "assist" {
				// Создаю вопрос
				var quest Question
				switch msg.Type {
				case "user":
					quest = Question{
						Question: strings.Split(msg.Content, "\n"),
						Voice:    false, // Сообщение от пользователя не голосовое
					}
				case "user_voice":
					quest = Question{
						Question: strings.Split(msg.Content, "\n"),
						Voice:    true, // Сообщение от пользователя голосовое
					}
				default:
					// Неизвестный тип сообщения, пропускаю
					logger.Warning("Неизвестный тип сообщения: %s", msg.Type, u.Assist.UserId)
					continue
				}

				// отправляю вопрос ассистенту
				question <- quest
				// Отправляю вопрос клиента в виде сообщения
				select {
				case usrCh.TxCh <- s.Bot.NewMessage("user", &msg.Content, &msg.UserName):
				default:
					return fmt.Errorf("'Listener' канал TxCh закрыт или переполнен")
				}
			}
		case quest := <-fullQuestCh: // Пришёл полный вопрос пользователя
			switch quest.VoiceQuestion {
			case false: // Вопрос задан текстом
				// Нужно создать отдельный канал слушателя для сохранения диалога для использования асинхронного сохранения
				s.End.SaveDialog(comdb.User, treadId, &quest.Answer) // убрал go для гарантированного порядка сохранения диалогов
			case true: // Вопрос задан голосом
				s.End.SaveDialog(comdb.UserVoice, treadId, &quest.Answer) // убрал go для гарантированного порядка сохранения диалогов
			}
		case resp := <-answerCh: // Пришёл ответ ассистента
			select {
			case usrCh.TxCh <- s.Bot.NewMessage("assist", &resp.Answer, &u.RespName):
				s.End.SaveDialog(comdb.AI, treadId, &resp.Answer) // убрал go для гарантированного порядка сохранения диалогов
			default:
				return fmt.Errorf("'Listener' канал TxCh закрыт или переполнен")
			}
		}
	}
}
