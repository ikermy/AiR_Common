package startpoint

import (
	"context"
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
	Question []string           // Вопрос пользователя, может состоять из нескольких вопросов
	Voice    bool               // Флаг, указывающий, что вопрос был задан голосом
	Files    []model.FileUpload // Файлы, прикрепленные к вопросу
	Operator model.Operator     // Если true — вопрос должен быть отправлен оператору, а не модели
}

// Answer структура для хранения ответов пользователя
type Answer struct {
	Answer        model.AssistResponse
	VoiceQuestion bool           // Флаг, указывающий, что вопрос был задан голосом
	Operator      model.Operator // Фактически будем указывать кто ответил: модель или оператор
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
	NewMessage(operator model.Operator, msgType string, content *model.AssistResponse, name *string, files ...model.FileUpload) model.Message
	Request(modelId string, dialogId uint64, ask *string, files ...model.FileUpload) (model.AssistResponse, error)
	GetCh(respId uint64) (model.Ch, error)
	CleanUp()
}

// OperatorInterface - интерфейс для отправки сообщений от и для операторов
type OperatorInterface interface {
	AskOperator(userID uint32, dialogID uint64, question model.Message) (model.Message, error)
	SendToOperator(userID uint32, dialogID uint64, question model.Message) error // Новый неблокирующий метод
	ReceiveFromOperator(userID uint32, dialogID uint64) <-chan model.Message     // Канал для получения ответов
}

// Start структура с интерфейсами вместо конкретных типов
type Start struct {
	ctx    context.Context
	cancel context.CancelFunc

	Mod  ModelInterface
	End  EndpointInterface
	Bot  BotInterface
	Oper OperatorInterface
}

// New создаёт новый экземпляр Start (бывший startpoint.New)
func New(parent context.Context, mod ModelInterface, end EndpointInterface, bot BotInterface, operator OperatorInterface) *Start {
	ctx, cancel := context.WithCancel(parent)
	return &Start{
		ctx:    ctx,
		cancel: cancel,

		Mod:  mod,
		End:  end,
		Bot:  bot,
		Oper: operator,
	}
}

// Stop останавливает внутренний контекст Start и даёт возможность корректно завершить фоновые операции
func (s *Start) Stop() {
	if s.cancel != nil {
		s.cancel()
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

	// Контекст ожидания ответа модели с таймаутом, завязанным на общий контекст Start
	ctx, cancel := context.WithTimeout(s.ctx, mode.ErrorTimeOutDurationForAssistAnswer*time.Minute)
	defer cancel()

	go func() {
		defer func() {
			if r := recover(); r != nil {
				logger.Warn("Восстановлено от паники в горутине Ask: %v", r)
			}
		}()

		// Ранний выход, если контекст уже отменён
		select {
		case <-ctx.Done():
			return
		default:
		}

		body, err := s.Mod.Request(modelId, dialogId, &ask, files...)
		if err != nil {
			select {
			case errCh <- fmt.Errorf("ask error making request: %w", err):
			default:
			}
			return
		}

		select {
		case <-ctx.Done():
			return
		case answerCh <- body:
		default:
		}
	}()

	// Жду либо ответа, либо ошибки, либо отмены/таймаута
	select {
	case body := <-answerCh:
		return body, nil
	case err := <-errCh:
		return emptyResponse, err
	case <-ctx.Done():
		// Возвращаем пустой ответ с ошибкой контекста для явного отличия от успешной пустоты
		return emptyResponse, ctx.Err()
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
	// Канал для получения сообщений от оператора в неблокирующем режиме
	operatorRxCh := s.Oper.ReceiveFromOperator(u.Assist.UserId, treadId)

	for {
		select {
		case <-s.ctx.Done():
			logger.Info("Start context canceled in Respondent %s", u.RespName, u.Assist.UserId)
			return
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

			// Немедленная пересылка в операторский канал, если включён SetOperator
			if quest.Operator.SetOperator {
				// Сбросить таймер батча, чтобы не "доехали" накопленные сообщения
				if askTimer != nil {
					if !askTimer.Stop() {
						select {
						case <-askTimer.C:
						default:
						}
					}
				}
				// Очистить накопленные батчи (если хранилище поддерживает)
				s.End.FlushAllBatches()

				// Отправка в операторский канал
				msgType := "user"
				if quest.Voice {
					msgType = "user_voice"
				}
				content := model.AssistResponse{Message: strings.Join(quest.Question, "\n")}
				name := u.Assist.AssistName
				opMsg := s.Mod.NewMessage(
					model.Operator{SetOperator: false, Operator: false},
					msgType, &content, &name, quest.Files...,
				)
				if err := s.Oper.SendToOperator(u.Assist.UserId, treadId, opMsg); err != nil {
					logger.Error("Ошибка отправки сообщения оператору: %v", err, u.Assist.UserId)
				}

				// Сохранение вопроса и переход к ожиданию следующего сообщения
				select {
				case fullQuestCh <- Answer{Answer: content, VoiceQuestion: quest.Voice}:
				default:
					select {
					case errCh <- fmt.Errorf("'respondent' канал fullQuestCh закрыт или переполнен"):
					default:
						logger.Warn("'Respondent' не удалось сохранить вопрос: канал fullQuestCh закрыт/переполнен", u.Assist.UserId)
					}
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
						// Также запрашиваю операторский режим помечая вопрос как операторский
						currentQuest.Operator.Operator = true
						logger.Debug("'Respondent' триггер найден в вопросе пользователя, запрашиваю операторский режим", u.Assist.UserId)
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
		// Получение неблокирующих сообщений от оператора
		case operatorMsg := <-operatorRxCh:
			// Обработка сообщения от оператора
			logger.Debug("Получено сообщение от оператора: %+v", operatorMsg)

			// Если оператор прислал переключение режима — сбросить таймер и очереди батча
			if operatorMsg.Operator.SetOperator {
				if askTimer != nil {
					if !askTimer.Stop() {
						select {
						case <-askTimer.C:
						default:
						}
					}
					askTimer = nil
				}
				// В идеале тут нужен таргетированный flush по dialogId; пока вызываем общий
				s.End.FlushAllBatches()
			}

			// Проверяем, это системное сообщение о переключении режима или обычное сообщение от оператора
			if operatorMsg.Type == "assist" && operatorMsg.Operator.SetOperator && !operatorMsg.Operator.Operator {
				// Это системное сообщение о выключении режима оператора (SetOperator=true, Operator=false)
				logger.Debug("Получено системное сообщение о выключении режима оператора", u.Assist.UserId)
				continue
			}

			// Отправляем ответ оператора пользователю
			answ := Answer{
				Answer:        operatorMsg.Content,
				VoiceQuestion: false, // Сообщения от оператора не голосовые
				Operator:      operatorMsg.Operator,
			}

			select {
			case answerCh <- answ:
				logger.Debug("Ответ оператора отправлен пользователю", u.Assist.UserId)
			default:
				logger.Warn("Канал answerCh закрыт или переполнен", u.Assist.UserId)
			}

			// ВАЖНО: возвращаемся к основному циклу после обработки сообщения от оператора
			continue
		}

	inputLoop:
		for {
			if !deaf {
				if askTimer == nil {
					askTimer = time.NewTimer(time.Duration(u.Assist.Espero) * time.Second)
				}

				select {
				case <-s.ctx.Done():
					if askTimer != nil {
						askTimer.Stop()
					}
					logger.Info("Start context canceled during inputLoop %s", u.RespName, u.Assist.UserId)
					return
				case <-u.Ctx.Done():
					if askTimer != nil {
						askTimer.Stop()
					}
					logger.Info("User context canceled during inputLoop %s", u.RespName, u.Assist.UserId)
					return
				case inputStruct, open := <-questionCh:
					// Обновляем флаги оператора текущего вопроса,
					// чтобы не утекали устаревшие значения
					currentQuest.Operator = inputStruct.Operator

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
			setOperatorMode  bool
		)

		logger.Info("currentQuest.Operator.SetOperator, %v", currentQuest.Operator.SetOperator)

		// Обработка операторских сообщений и запросов к AI
		if currentQuest.Operator.Operator {
			// Если вопрос помечен как операторский но операторский режим ещё не включён,
			// значит это первоначальный запрос на операторский режим, пробую связаться с оператором
			msgType := "user"
			if VoiceQuestion {
				msgType = "user_voice"
			}
			content := model.AssistResponse{Message: strings.Join(userAsk, "\n")}
			name := u.Assist.AssistName
			opMsg := s.Mod.NewMessage(model.Operator{Operator: true}, msgType, &content, &name, currentQuest.Files...)

			var respMsg model.Message
			respMsg, err = s.Oper.AskOperator(u.Assist.UserId, treadId, opMsg)
			// Если получили ошибку от оператора или пустой ответ — делаем фолбэк в OpenAI
			if err != nil || (respMsg.Content.Message == "" && len(respMsg.Content.Action.SendFiles) == 0) {
				logger.Error("Ошибка запроса к оператору или пустой ответ, фолбэк в OpenAI: %v", err, u.Assist.UserId)
				// Отправляю запрос в OpenAI
				answer, err = s.Ask(u.Assist.AssistId, treadId, userAsk, currentQuest.Files...)
				if err != nil {
					logger.Error("Ошибка запроса к модели: %v", err, u.Assist.UserId)
				}
				operatorAnswered = false
			} else {
				answer = respMsg.Content
				operatorAnswered = true
				// Если оператор ответил то устанавливаю флаг операторского режима
				setOperatorMode = true
			}

		} else {
			// Отправляю запрос в OpenAI
			answer, err = s.Ask(u.Assist.AssistId, treadId, userAsk, currentQuest.Files...)
			if err != nil {
				logger.Error("Ошибка запроса к модели: %v", err, u.Assist.UserId)
			}
			operatorAnswered = false
		}

		if currentQuest.Operator.SetOperator {
			// Если это неблокирующая отправка оператору, пропускаем отправку ответа пользователю
			// но сохраняем диалог
			fullAsk := Answer{
				Answer: model.AssistResponse{
					Message: strings.Join(userAsk, "\n"),
				},
				VoiceQuestion: VoiceQuestion,
			}

			select {
			case fullQuestCh <- fullAsk:
			default:
				// обработка ошибки
			}

			continue // Только здесь используем continue
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
			Answer: answer,
			Operator: model.Operator{
				SetOperator: setOperatorMode,
				Operator:    operatorAnswered,
			},
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

			// Реагируем на отмену общего контекста: при отмене просто выходим, Respondent сам завершится по s.ctx.Done()
			select {
			case <-s.ctx.Done():
				logger.Info("StarterRespondent canceled by Start context %s", u.RespName, u.Assist.UserId)
				return
			default:
			}

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
			// Если общий контекст отменён — не запускаем Listener
			select {
			case <-s.ctx.Done():
				logger.Info("StarterListener canceled by Start context %s", start.Model.RespName, start.Model.Assist.UserId)
				return
			default:
			}
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
		case <-s.ctx.Done():
			logger.Info("Start context canceled in Listener %s", u.RespName, u.Assist.UserId)
			return nil
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
						Question: strings.Split(msg.Content.Message, "\n"),
						Voice:    false,        // Сообщение от пользователя не голосовое
						Files:    msg.Files,    // Файлы, прикрепленные к вопросу
						Operator: msg.Operator, // Помечаем оператором при триггере или если уже отмечено
					}
				case "user_voice":
					quest = Question{
						Question: strings.Split(msg.Content.Message, "\n"),
						Voice:    true,         // Сообщение от пользователя голосовое
						Files:    msg.Files,    // Файлы, прикрепленные к вопросу
						Operator: msg.Operator, // Помечаем оператором при триггере или если уже отмечено
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
				case usrCh.TxCh <- s.Mod.NewMessage(msg.Operator, "user", &msg.Content, &msg.Name):
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
		case resp := <-answerCh: // Пришёл ответ ассистента/оператора
			select {
			// Operator верно берется из ответа, чтобы знать кто фактически ответил
			case usrCh.TxCh <- s.Mod.NewMessage(resp.Operator, "assist", &resp.Answer, &u.Assist.AssistName): // Имя ассистента из настроек модели
				switch resp.Operator.Operator {
				case false:
					s.End.SaveDialog(comdb.AI, treadId, &resp.Answer) // убрал go для гарантированного порядка сохранения диалогов
				case true:
					s.End.SaveDialog(comdb.Operator, treadId, &resp.Answer) // убрал go для гарантированного порядка сохранения диалогов
				}
			default:
				return fmt.Errorf("'Listener' канал TxCh закрыт или переполнен")
			}
		}
	}
}
