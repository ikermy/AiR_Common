package startpoint

import (
	"fmt"
	"strings"
	"time"

	"github.com/ikermy/AiR_Common/pkg/comdb"
	"github.com/ikermy/AiR_Common/pkg/logger"
	"github.com/ikermy/AiR_Common/pkg/mode"
	"github.com/ikermy/AiR_Common/pkg/model"
)

// Question структура для хранения вопросов пользователя
type Question struct {
	Question []string           `json:"question"`        // Вопрос пользователя, может состоять из нескольких вопросов
	Voice    bool               `json:"voice"`           // Флаг, указывающий, что вопрос был задан голосом
	Files    []model.FileUpload `json:"files,omitempty"` // Файлы, прикрепленные к вопросу
	Operator bool               `json:"operator"`        // Если true — вопрос должен быть отправлен оператору, а не модели
}

// Answer структура для хранения ответов пользователя
type Answer struct {
	Answer        model.AssistResponse
	VoiceQuestion bool // Флаг, указывающий, что вопрос был задан голосом
	SetOperator   bool // Если true — сработало событие переключения на оператора
}

// BotInterface - интерфейс для различных реализаций ботов
type BotInterface interface {
	StartBots() error
	StopBot()
}

// EndpointInterface - интерфейс для работы с диалогами
type EndpointInterface interface {
	GetUserAsk(dialogId uint64, respId uint64) []string
	SetUserAsk(dialogId uint64, respId uint64, ask string, askLimit uint32) bool
	SaveDialog(creator comdb.CreatorType, treadId uint64, resp *model.AssistResponse)
	Meta(userId uint32, dialogId uint64, meta string, respName string, assistName string, metaAction string)
	FlushAllBatches()
}

// ModelInterface - интерфейс для моделей
type ModelInterface interface {
	NewMessage(operator bool, msgType string, content *model.AssistResponse, name *string, files ...model.FileUpload) model.Message
	Request(modelId string, dialogId uint64, ask *string, files ...model.FileUpload) (model.AssistResponse, error)
	GetCh(respId uint64) (model.Ch, error)
	CleanUp()
}

// OperatorInterface - интерфейс для отправки сообщений от и для операторов
type OperatorInterface interface {
	AskOperator(userID uint32, dialogID uint64, question model.Message) (model.Message, error)
}

// Start структура с интерфейсами вместо конкретных типов
type Start struct {
	Mod  ModelInterface
	End  EndpointInterface
	Bot  BotInterface
	Oper OperatorInterface
}

// New создаёт новый экземпляр Start (бывший startpoint.New)
func New(mod ModelInterface, end EndpointInterface, bot BotInterface, operator OperatorInterface) *Start {
	return &Start{
		Mod:  mod,
		End:  end,
		Bot:  bot,
		Oper: operator,
	}
}

func (s *Start) Ask(modelId string, dialogId uint64, arrAsk []string, files ...model.FileUpload) (model.AssistResponse, error) {
	var emptyResponse model.AssistResponse
	answerCh := make(chan model.AssistResponse, 1)
	errCh := make(chan error, 1)
	defer close(answerCh)
	defer close(errCh)

	var ask string
	for _, v := range arrAsk {
		if v != "" {
			ask += v + "\n"
		}
	}

	if ask == "" && len(files) == 0 {
		return emptyResponse, fmt.Errorf("ASK EMPTY MESSAGE AND NO FILES")
	}

	if mode.TestAnswer {
		filesInfo := ""
		if len(files) > 0 {
			filesInfo = fmt.Sprintf(" with %d files", len(files))
		}
		return model.AssistResponse{
			Message: "AssistId model " + " resp " + ask + filesInfo,
		}, nil
	}

	go func() {
		defer func() {
			if r := recover(); r != nil {
				logger.Warn("Восстановлено от паники в горутине Ask: %v", r)
			}
		}()

		body, err := s.Mod.Request(modelId, dialogId, &ask, files...)
		if err != nil {
			select {
			case errCh <- fmt.Errorf("ask error making request: %w", err):
			default:
			}
			return
		}

		select {
		case answerCh <- body:
		default:
		}
	}()

	timeout := time.After(mode.ErrorTimeOutDurationForAssistAnswer * time.Minute)

	select {
	case body := <-answerCh:
		return body, nil
	case err := <-errCh:
		return emptyResponse, err
	case <-timeout:
		return emptyResponse, nil
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
		VoiceQuestion bool     // Флаг, указывающий, что вопрос был задан голосом
		currentQuest  Question // Текущий вопрос пользователя, который обрабатывается
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
					logger.Warn("'Respondent' не удалось отправить ошибку: канал errCh закрыт или переполнен", u.Assist.UserId)
					return
				}
				continue
			}

			// Сохраняем текущий вопрос
			currentQuest = quest

			// Проверяю наличие в запросе пользователя сообщения из u.Assist.Metas.Triggers
			if len(u.Assist.Metas.Triggers) > 0 {
				userQuestion := strings.Join(quest.Question, "\n")
				for _, trigger := range u.Assist.Metas.Triggers {
					if strings.Contains(userQuestion, trigger) {
						// Если триггер найден, то уведомляю пользователя в CarpinteroCh
						s.End.Meta(u.Assist.UserId, treadId, "trigger", u.RespName, u.Assist.AssistName, u.Assist.Metas.MetaAction)
						// Также помечаю сообщение как операторское
						currentQuest.Operator = true
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
							logger.Warn("'Respondent' не удалось отправить ошибку: канал errCh закрыт или переполнен", u.Assist.UserId)
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

		// Отправляем запрос в OpenAI или оператору
		userAsk := s.End.GetUserAsk(treadId, respId)
		if strings.TrimSpace(strings.Join(userAsk, "\n")) == "" {
			// Пустой запрос, пропускаем
			continue
		}
		// Сохраняю запрос пользователя для сохранения диалога
		fullAsk := Answer{
			Answer: model.AssistResponse{
				Message: strings.Join(userAsk, "\n"),
			},
			VoiceQuestion: VoiceQuestion, // Передаём информацию о голосовом вопросе
		}

		// Проверяю что канал fullQuestCh не закрыт
		select {
		case fullQuestCh <- fullAsk:
		default:
			select {
			case errCh <- fmt.Errorf("'respondent' канал fullQuestCh закрыт или переполнен"):
			default:
				logger.Warn("'Respondent' не удалось отправить ошибку: канал errCh закрыт или переполнен", u.Assist.UserId)
			}
		}

		var (
			answer           model.AssistResponse
			err              error
			operatorAnswered bool
		)

		if currentQuest.Operator {
			// Формируем сообщение для оператора и запрашиваем ответ
			msgType := "user"
			if VoiceQuestion {
				msgType = "user_voice"
			}
			content := model.AssistResponse{Message: strings.Join(userAsk, "\n")}
			name := u.Assist.AssistName
			opMsg := s.Mod.NewMessage(true, msgType, &content, &name, currentQuest.Files...)

			var respMsg model.Message
			respMsg, err = s.Oper.AskOperator(u.Assist.UserId, treadId, opMsg)
			// Если получили ошибку от оператора или пустой ответ — делаем фолбэк в OpenAI
			if err != nil || (respMsg.Content.Message == "" && len(respMsg.Content.Action.SendFiles) == 0) {
				answer, err = s.Ask(u.Assist.AssistId, treadId, userAsk, currentQuest.Files...)
				operatorAnswered = false
			} else {
				answer = respMsg.Content
				operatorAnswered = true
			}
		} else {
			// Отправляю запрос в OpenAI
			answer, err = s.Ask(u.Assist.AssistId, treadId, userAsk, currentQuest.Files...)
			operatorAnswered = false
		}

		// Oyente
		deaf = false

		if err != nil {
			logger.Error("Ошибка запроса к модели/оператору: %v", err, u.Assist.UserId)
			continue
		}
		// Если пустой ответ
		if answer.Message == "" && len(answer.Action.SendFiles) == 0 {
			continue
		}

		// Проверяю на содержание в ответе цели из u.Assist.Metas.MetaAction
		if u.Assist.Metas.MetaAction != "" {
			if answer.Meta { // Ассистент/оператор пометил ответ как достигший цели
				s.End.Meta(u.Assist.UserId, treadId, "target", u.RespName, u.Assist.AssistName, u.Assist.Metas.MetaAction)
			}
		}

		// Отправляем ответ вызывающей функции
		answ := Answer{
			Answer:      answer,
			SetOperator: operatorAnswered,
		}
		//Проверяю что канал answerCh не закрыт
		select {
		case answerCh <- answ:
		default:
			select {
			case errCh <- fmt.Errorf("'respondent' канал answerCh закрыт или переполнен"):
			default:
				logger.Warn("'Respondent' не удалось отправить ошибку: канал errCh закрыт или переполнен", u.Assist.UserId)
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

// StarterListener запускает Listener для пользователя, если он ещё не запущен
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

// Listener слушает канал от пользователя и обрабатывает сообщения
func (s *Start) Listener(u *model.RespModel, usrCh model.Ch, respId uint64, treadId uint64) error {
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
				// Определяю, должен ли вопрос быть операторским
				opFlag := msg.Operator
				if len(u.Assist.Metas.Triggers) > 0 {
					for _, trigger := range u.Assist.Metas.Triggers {
						if strings.Contains(msg.Content.Message, trigger) {
							opFlag = true
							break
						}
					}
				}
				switch msg.Type {
				case "user":
					quest = Question{
						Question: strings.Split(msg.Content.Message, "\n"),
						Voice:    false,     // Сообщение от пользователя не голосовое
						Files:    msg.Files, // Файлы, прикрепленные к вопросу
						Operator: opFlag,    // Помечаем оператором при триггере или если уже отмечено
					}
				case "user_voice":
					quest = Question{
						Question: strings.Split(msg.Content.Message, "\n"),
						Voice:    true,      // Сообщение от пользователя голосовое
						Files:    msg.Files, // Файлы, прикрепленные к вопросу
						Operator: opFlag,    // Помечаем оператором при триггере или если уже отмечено
					}
				default:
					// Неизвестный тип сообщения, пропускаю
					logger.Warn("Неизвестный тип сообщения: %s", msg.Type, u.Assist.UserId)
					continue
				}
				// отправляю вопрос ассистенту/оператору
				question <- quest
				// Отправляю вопрос клиента в виде сообщения
				select {
				// Operator верно берется из вопроса, чтобы знать кому адресовать сообщение
				case usrCh.TxCh <- s.Mod.NewMessage(opFlag, "user", &msg.Content, &msg.Name):
				default:
					return fmt.Errorf("'Listener' канал TxCh закрыт или переполнен")
				}
			}
		case quest := <-fullQuestCh: // Пришёл полный вопрос пользователя
			switch quest.VoiceQuestion {
			case false: // Вопрос задан текстом
				// TODO добавить статус ответе от оператора а не только от модели
				// Нужно создать отдельный канал слушателя для сохранения диалога для использования асинхронного сохранения
				s.End.SaveDialog(comdb.User, treadId, &quest.Answer) // убрал go для гарантированного порядка сохранения диалогов
			case true: // Вопрос задан голосом
				s.End.SaveDialog(comdb.UserVoice, treadId, &quest.Answer) // убрал go для гарантированного порядка сохранения диалогов
			}
		case resp := <-answerCh: // Пришёл ответ ассистента/оператора
			select {
			// Operator верно берется из ответа, чтобы знать кто фактически ответил
			case usrCh.TxCh <- s.Mod.NewMessage(resp.SetOperator, "assist", &resp.Answer, &u.Assist.AssistName): // Имя ассистента из настроек модели
				s.End.SaveDialog(comdb.AI, treadId, &resp.Answer) // убрал go для гарантированного порядка сохранения диалогов
			default:
				return fmt.Errorf("'Listener' канал TxCh закрыт или переполнен")
			}
		}
	}
}
