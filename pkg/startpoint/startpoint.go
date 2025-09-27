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
	DisableOperatorMode(userId uint32, dialogId uint64) error
}

// EndpointInterface - интерфейс для работы с диалогами
type EndpointInterface interface {
	GetUserAsk(dialogId uint64, respId uint64) []string
	SetUserAsk(dialogId uint64, respId uint64, ask string, askLimit uint32) bool
	SaveDialog(creator comdb.CreatorType, treadId uint64, resp *model.AssistResponse)
	Meta(userId uint32, dialogId uint64, meta string, respName string, assistName string, metaAction string)
	SendEvent(userId uint32, event, userName, assistName, target string)
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
	AskOperator(ctx context.Context, userID uint32, dialogID uint64, question model.Message) (model.Message, error)
	SendToOperator(ctx context.Context, userID uint32, dialogID uint64, question model.Message) error // Новый неблокирующий метод
	ReceiveFromOperator(ctx context.Context, userID uint32, dialogID uint64) <-chan model.Message     // Канал для получения ответов
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
) {
	var (
		deaf          bool   // Не слушать ввод пользователя до момента получения ответа
		ask           string // Вопрос пользователя
		askTimer      *time.Timer
		VoiceQuestion bool                 // Флаг, указывающий, что вопрос был задан голосом
		currentQuest  Question             // Текущий вопрос пользователя, который обрабатывается
		operatorMode  bool                 // Флаг включенного операторского режима
		operatorRxCh  <-chan model.Message // Канал для получения сообщений от оператора
	)

	for {
		select {
		case <-s.ctx.Done():
			logger.Debug("Start context canceled in Respondent %s", u.RespName, u.Assist.UserId)
			return
		case <-u.Ctx.Done():
			logger.Debug("Context.Done Respondent %s", u.RespName, u.Assist.UserId)
			return

		// Обработка сообщений от оператора (только если канал инициализирован)
		case operatorMsg := <-func() <-chan model.Message {
			if operatorMode && operatorRxCh != nil {
				return operatorRxCh
			}
			return nil
		}():
			if operatorMsg.Type == "" {
				continue // Пустое сообщение из nil канала
			}

			// Проверка на системное сообщение о выключении режима
			if operatorMsg.Operator.SetOperator &&
				operatorMsg.Operator.Operator &&
				operatorMsg.Content.Message == "Set-Mode-To-AI" {
				logger.Debug("Получено системное сообщение о выключении режима оператора", u.Assist.UserId)
				operatorMode = false

				// Вызываем колбэк для корректного завершения сессии оператора
				err := s.Bot.DisableOperatorMode(u.Assist.UserId, treadId)
				if err != nil {
					logger.Error("ошибка при отключении режима оператора: %w", err)
				}
				continue
			}

			// Отправка ответа оператора пользователю
			answ := Answer{
				Answer:        operatorMsg.Content,
				VoiceQuestion: false,
				Operator:      operatorMsg.Operator,
			}

			select {
			case answerCh <- answ:
				logger.Debug("Ответ оператора отправлен пользователю", u.Assist.UserId)
			default:
				logger.Warn("Канал answerCh закрыт или переполнен", u.Assist.UserId)
			}
			continue

		case quest, open := <-questionCh:
			if !open {
				logger.Error("Канал questionCh закрыт", u.Assist.UserId)
				continue
			}

			currentQuest = quest

			// Обработка SetOperator режима
			if quest.Operator.SetOperator {
				// Инициализация канала оператора при первом включении режима
				if !operatorMode {
					operatorMode = true
					operatorRxCh = s.Oper.ReceiveFromOperator(s.ctx, u.Assist.UserId, treadId)
					logger.Debug("Включен операторский режим для пользователя %d", u.Assist.UserId)
				}

				if askTimer != nil {
					if !askTimer.Stop() {
						select {
						case <-askTimer.C:
						default:
						}
					}
				}

				msgType := "user"
				if quest.Voice {
					msgType = "user_voice"
				}
				content := model.AssistResponse{Message: strings.Join(quest.Question, "\n")}
				name := u.Assist.AssistName
				opMsg := s.Mod.NewMessage(
					model.Operator{SetOperator: false, Operator: false, SenderName: quest.Operator.SenderName},
					msgType, &content, &name, quest.Files...,
				)
				if err := s.Oper.SendToOperator(s.ctx, u.Assist.UserId, treadId, opMsg); err != nil {
					logger.Error("Ошибка отправки сообщения оператору: %v", err, u.Assist.UserId)
				}

				select {
				case fullQuestCh <- Answer{Answer: content, VoiceQuestion: quest.Voice}:
				default:
					logger.Warn("Канал fullQuestCh закрыт или переполнен", u.Assist.UserId)
				}
				continue
			}

			// Проверка триггеров
			if len(u.Assist.Metas.Triggers) > 0 {
				userQuestion := strings.Join(quest.Question, "\n")
				for _, trigger := range u.Assist.Metas.Triggers {
					if strings.Contains(userQuestion, trigger) {
						s.End.Meta(u.Assist.UserId, treadId, "trigger", u.RespName, u.Assist.AssistName, u.Assist.Metas.MetaAction)
						currentQuest.Operator.Operator = true

						// Активация операторского режима при триггере
						if !operatorMode {
							operatorMode = true
							operatorRxCh = s.Oper.ReceiveFromOperator(s.ctx, u.Assist.UserId, treadId)
							logger.Debug("Операторский режим активирован по триггеру для пользователя %d", u.Assist.UserId)
						}
						logger.Debug("'Respondent' триггер найден в вопросе пользователя, запрашиваю операторский режим", u.Assist.UserId)
					}
				}
			}

			ask = strings.Join(quest.Question, "\n")
			VoiceQuestion = quest.Voice

			if s.End.SetUserAsk(treadId, respId, ask, u.Assist.Limit) {
				askTimer = time.NewTimer(time.Duration(u.Assist.Espero) * time.Second)
			} else {
				if askTimer == nil {
					askTimer = time.NewTimer(0)
				} else {
					askTimer.Reset(0)
				}
			}
		}

	inputLoop:
		for {
			// Не слушать новые вопросы пользователя до ответа
			if !deaf {
				if askTimer == nil {
					askTimer = time.NewTimer(time.Duration(u.Assist.Espero) * time.Second)
				}

				select {
				case <-s.ctx.Done():
					if askTimer != nil {
						askTimer.Stop()
					}
					logger.Debug("Start context canceled during inputLoop %s", u.RespName, u.Assist.UserId)
					return
				case <-u.Ctx.Done():
					if askTimer != nil {
						askTimer.Stop()
					}
					logger.Debug("User context canceled during inputLoop %s", u.RespName, u.Assist.UserId)
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
						logger.Warn("Канал questionCh закрыт", u.Assist.UserId)
					}

				case <-askTimer.C:
					askTimer.Stop()
					// Устанавливаю значение слушалтеля в зависимости от настроек модели
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
			logger.Warn("Канал fullQuestCh закрыт или переполнен", u.Assist.UserId)
		}

		var (
			answer           model.AssistResponse
			err              error
			operatorAnswered bool
			setOperatorMode  bool
		)

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
			opMsg := s.Mod.NewMessage(model.Operator{Operator: true, SenderName: currentQuest.Operator.SenderName}, msgType, &content, &name, currentQuest.Files...)

			var respMsg model.Message
			respMsg, err = s.Oper.AskOperator(s.ctx, u.Assist.UserId, treadId, opMsg)
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
			// Это проверенный работающий код
			//  раскомментировать для возврата к старой логике

			//if err != nil {
			//	logger.Error("Ошибка запроса к модели: %v", err, u.Assist.UserId)
			//}
			//operatorAnswered = false

			// Новый не проверенный код !!!!!
			if err == nil && answer.Operator {
				// Модель запросила эскалацию к оператору
				if !operatorMode {
					operatorMode = true
					operatorRxCh = s.Oper.ReceiveFromOperator(s.ctx, u.Assist.UserId, treadId)
					s.End.SendEvent(u.Assist.UserId, "model-operator", u.RespName, u.Assist.AssistName, "")
					logger.Debug("Операторский режим активирован по флагу ответа модели для пользователя %d", u.Assist.UserId)
				}

				setOperatorMode = true // Передадим наружу, чтобы фронт включил режим

				// Неблокирующе отправим оператору исходный вопрос (как при SetOperator)
				msgType := "user"
				if VoiceQuestion {
					msgType = "user_voice"
				}
				// Можно отправить именно пользовательский вопрос, а не ответ модели
				contentToOp := model.AssistResponse{Message: strings.Join(userAsk, "\n")}
				name := u.Assist.AssistName
				opMsg := s.Mod.NewMessage(
					model.Operator{Operator: true, SenderName: currentQuest.Operator.SenderName},
					msgType,
					&contentToOp,
					&name,
					currentQuest.Files...,
				)
				if errSend := s.Oper.SendToOperator(s.ctx, u.Assist.UserId, treadId, opMsg); errSend != nil {
					logger.Error("Ошибка отправки эскалации оператору: %v", errSend, u.Assist.UserId)
				}
				// конец нового говнокода !!!!!
			}
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
			if answer.Meta { // Ассистент пометил ответ как достигший цели
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
			logger.Warn("Канал answerCh закрыт или переполнен", u.Assist.UserId)
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
	// Нужен ли мютекс???
	if !u.Services.Respondent {
		u.Services.Respondent = true
		go func() {
			defer func() {
				u.Services.Respondent = false
			}()

			// Реагируем на отмену общего контекста: при отмене просто выходим, Respondent сам завершится по s.ctx.Done()
			select {
			case <-s.ctx.Done():
				logger.Debug("StarterRespondent canceled by Start context %s", u.RespName, u.Assist.UserId)
				return
			default:
			}

			s.Respondent(u, questionCh, answerCh, fullQuestCh, respId, treadId)
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
				logger.Debug("StarterListener canceled by Start context %s", start.Model.RespName, start.Model.Assist.UserId)
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
			logger.Debug("Start context canceled in Listener %s", u.RespName, u.Assist.UserId)
			return nil
		case err := <-errCh:
			return err // Возвращаем возможные ошибки
		case <-u.Ctx.Done():
			logger.Debug("Context.Done Listener %s", u.RespName, u.Assist.UserId)
			return nil
		case msg, ok := <-usrCh.RxCh:
			if !ok {
				logger.Debug("Канал RxCh закрыт %s", u.RespName, u.Assist.UserId)
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
