package startpoint

import (
	"context"
	"fmt"
	"strings"
	"sync"
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
	DisableOperatorMode(userId uint32, dialogId uint64, silent ...bool) error
}

// EndpointInterface - интерфейс для работы с диалогами
type EndpointInterface interface {
	GetUserAsk(dialogId uint64, respId uint64) []string
	SetUserAsk(dialogId, respId uint64, ask string, askLimit ...uint32) bool
	SaveDialog(creator comdb.CreatorType, treadId uint64, resp *model.AssistResponse)
	Meta(userId uint32, dialogId uint64, meta string, respName string, assistName string, metaAction string)
	SendEvent(userId uint32, event, userName, assistName, target string)
}

// ModelInterface - интерфейс для моделей
type ModelInterface interface {
	NewMessage(operator model.Operator, msgType string, content *model.AssistResponse, name *string, files ...model.FileUpload) model.Message
	Request(modelId string, dialogId uint64, ask *string, files ...model.FileUpload) (model.AssistResponse, error)
	GetCh(respId uint64) (*model.Ch, error)
	CleanUp()
}

// OperatorInterface - интерфейс для отправки сообщений от и для операторов
type OperatorInterface interface {
	AskOperator(ctx context.Context, userID uint32, dialogID uint64, question model.Message) (model.Message, error)
	SendToOperator(ctx context.Context, userID uint32, dialogID uint64, question model.Message) error
	ReceiveFromOperator(ctx context.Context, userID uint32, dialogID uint64) <-chan model.Message // Канал для получения ответов
	DeleteSession(userID uint32, dialogID uint64) error
	GetConnectionErrors(ctx context.Context, userID uint32, dialogID uint64) <-chan string
}

// Start структура с интерфейсами вместо конкретных типов
type Start struct {
	ctx    context.Context
	cancel context.CancelFunc

	Mod  ModelInterface
	End  EndpointInterface
	Bot  BotInterface
	Oper OperatorInterface

	respondentWG sync.Map // map[uint64]*sync.WaitGroup - для синхронизации завершения Respondent
}

// New создаёт новый экземпляр Start
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

// Shutdown останавливает внутренний контекст Start и даёт возможность корректно завершить фоновые операции
func (s *Start) Shutdown() {
	if s.cancel != nil {
		s.cancel()
	}
}

func (s *Start) ask(modelId string, dialogId uint64, arrAsk []string, files ...model.FileUpload) (model.AssistResponse, error) {
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

		// Ранний выход, если контекст уже отменён
		select {
		case <-ctx.Done():
			logger.Debug("ask ранний выход по ctx.Done() для модели %s и диалога %d", modelId, dialogId)
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
		deaf            bool   // Не слушать ввод пользователя до момента получения ответа
		ask             string // Вопрос пользователя
		askTimer        *time.Timer
		VoiceQuestion   bool                 // Флаг, указывающий, что вопрос был задан голосом
		currentQuest    Question             // Текущий вопрос пользователя, который обрабатывается
		operatorMode    bool                 // Флаг включенного операторского режима
		operatorRxCh    <-chan model.Message // Канал для получения сообщений от оператора
		operatorErrorCh <-chan string        // Канал для получения ошибок от операторского бэка
	)

	// Получаем канал ошибок сразу при запуске Respondent
	operatorErrorCh = s.Oper.GetConnectionErrors(s.ctx, u.Assist.UserId, treadId)

	for {
		select {
		case <-s.ctx.Done():
			logger.Debug("Start context canceled in Respondent %s", u.RespName, u.Assist.UserId)
			return
		case <-u.Ctx.Done():
			logger.Debug("Context.Done Respondent %s", u.RespName, u.Assist.UserId)
			return

		// Обработка ошибок подключения к оператору
		case errorType := <-operatorErrorCh:
			if errorType == "no_tg_id" {
				logger.Warn("Нет tg_id для пользователя %d, отключаем операторский режим", u.Assist.UserId)
				operatorMode = false
				operatorRxCh = nil

				// Вызываю тихое отключение режима оператор для пользовательского бота
				err := s.Bot.DisableOperatorMode(u.Assist.UserId, treadId, true)
				if err != nil {
					errCh <- fmt.Errorf("ошибка при отключении режима оператора: %w", err)
				}

				// Отправляем информационное сообщение пользователю
				systemMsg := model.AssistResponse{
					Message: "🚫👨‍💻 Нет доступных операторов \n Продолжаю работу в режиме AI-агента 🧠",
				}
				select {
				case answerCh <- Answer{
					Answer:   systemMsg,
					Operator: model.Operator{SetOperator: false, Operator: false},
				}:
				default:
					errCh <- fmt.Errorf("канал answerCh закрыт при отправке сообщения об ошибке tg_id")
					return
				}

				// Получаем новый канал ошибок для следующих попыток
				operatorErrorCh = s.Oper.GetConnectionErrors(s.ctx, u.Assist.UserId, treadId)
				continue
			}

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

				// Удаляем сессию оператора
				err := s.Oper.DeleteSession(u.Assist.UserId, treadId)
				if err != nil {
					errCh <- fmt.Errorf("ошибка при удалении текущей сессии оператора: %v", err)
				}

				// Вызываем колбэк для корректного завершения сессии оператора
				err = s.Bot.DisableOperatorMode(u.Assist.UserId, treadId)
				if err != nil {
					errCh <- fmt.Errorf("ошибка при отключении режима оператора: %w", err)
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
				errCh <- fmt.Errorf("канал answerCh закрыт или переполнен %v", u.Assist.UserId)
				return
			}
			continue // т.к. это операторское сообщение то сразу ждём следующее, а не спускаемся вниз по логике AI

		case quest, open := <-questionCh:
			if !open {
				errCh <- fmt.Errorf("канал questionCh закрыт %v", u.Assist.UserId)
				//continue
				return // Тут только выходить
			}

			currentQuest = quest

			// Если уже активен операторский режим — шлём сообщение оператору неблокирующе и не идём в AI
			if operatorMode {
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
					errCh <- fmt.Errorf("ошибка отправки сообщения оператору: %v", err)
				}
				// Сохраняем полный вопрос
				select {
				case fullQuestCh <- Answer{Answer: content, VoiceQuestion: quest.Voice}:
				default:
					errCh <- fmt.Errorf("канал fullQuestCh закрыт или переполнен %v", u.Assist.UserId)
					return
				}
				continue
			}

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
					errCh <- fmt.Errorf("ошибка отправки сообщения оператору: %v", err)
				}

				select {
				case fullQuestCh <- Answer{Answer: content, VoiceQuestion: quest.Voice}:
				default:
					errCh <- fmt.Errorf("канал fullQuestCh закрыт или переполнен %d", u.Assist.UserId)
					return
				}
				continue
			}

			// Проверка триггеров
			if len(u.Assist.Metas.Triggers) > 0 {
				userQuestion := strings.Join(quest.Question, "\n")
				for _, trigger := range u.Assist.Metas.Triggers {
					if strings.Contains(userQuestion, trigger) {
						s.End.Meta(u.Assist.UserId, treadId, "trigger", u.RespName, u.Assist.AssistName, u.Assist.Metas.MetaAction)

						//currentQuest.Operator.Operator = true
						// Активация операторского режима при триггере
						//if !operatorMode {
						//	operatorMode = true
						//	operatorRxCh = s.Oper.ReceiveFromOperator(s.ctx, u.Assist.UserId, treadId)
						//	logger.Debug("Операторский режим активирован по триггеру для пользователя %d", u.Assist.UserId)
						//}
						//logger.Debug("'Respondent' триггер найден в вопросе пользователя, запрашиваю операторский режим", u.Assist.UserId)
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
					if !open {
						askTimer.Stop()
						errCh <- fmt.Errorf("канал questionCh закрыт %v", u.Assist.UserId)
						// По хорошему нужно выходить
					}
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

		// Собираем batched вопрос
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
			errCh <- fmt.Errorf("канал fullQuestCh закрыт или переполнен %v", u.Assist.UserId)
			return
		}

		var (
			answer           model.AssistResponse
			err              error
			operatorAnswered bool
			setOperatorMode  bool
		)

		// Операторский запрос (явный), без SetOperator — сначала пробуем синхронно спросить оператора
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
				errCh <- fmt.Errorf("ошибка запроса к оператору или пустой ответ, фолбэк в OpenAI: %v", err)
				// Отправляю запрос в OpenAI
				answer, err = s.AskWithRetry(u.Assist.AssistId, treadId, userAsk, currentQuest.Files...)
				if err != nil {
					if IsFatalError(err) {
						errCh <- fmt.Errorf("критическая ошибка для пользователя %d: %v", u.Assist.UserId, err)
						return
					}
					// Некритическая ошибка — логируем и продолжаем слушать
					logger.Debug("Некритическая ошибка для пользователя %d: %v", u.Assist.UserId, err)
					continue
				}
				operatorAnswered = false
			} else {
				answer = respMsg.Content
				operatorAnswered = true
				// Если оператор ответил, то устанавливаю флаг операторского режима
				setOperatorMode = true

				// Включаем постоянный режим после успешного ответа оператора
				if !operatorMode {
					operatorMode = true
					operatorRxCh = s.Oper.ReceiveFromOperator(s.ctx, u.Assist.UserId, treadId)
					logger.Debug("Операторский режим активирован после ответа оператора для пользователя %d", u.Assist.UserId)
				}
			}

		} else {
			// Отправляю запрос в OpenAI
			answer, err = s.AskWithRetry(u.Assist.AssistId, treadId, userAsk, currentQuest.Files...)
			if err != nil {
				if IsFatalError(err) {
					errCh <- fmt.Errorf("критическая ошибка для пользователя %d: %v", u.Assist.UserId, err)
					return
				}
				// Некритическая ошибка — логируем и продолжаем слушать
				logger.Debug("Некритическая ошибка для пользователя %d: %v", u.Assist.UserId, err)
				continue
			}

			// Пришёл ответ от модели, проверяю на флаг запроса операторского режима
			if answer.Operator {
				// Модель запросила эскалацию к оператору
				if !operatorMode {
					operatorMode = true
					operatorRxCh = s.Oper.ReceiveFromOperator(s.ctx, u.Assist.UserId, treadId)
					s.End.SendEvent(u.Assist.UserId, "model-operator", u.RespName, u.Assist.AssistName, "")
					logger.Debug("Операторский режим активирован по флагу ответа модели", u.Assist.UserId)
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
					errCh <- fmt.Errorf("ошибка отправки эскалации оператору: %v", errSend)
				}
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
			select {
			case errCh <- fmt.Errorf("канал answerCh закрыт или переполнен %v", u.Assist.UserId):
			default:
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
	errCh chan error,
) {
	if !u.Services.Respondent.Load() {
		u.Services.Respondent.Store(true)

		// Создаем WaitGroup для синхронизации
		wg := &sync.WaitGroup{}
		wg.Add(1)
		s.respondentWG.Store(treadId, wg)

		go func() {
			defer func() {
				u.Services.Respondent.Store(false)
				wg.Done()
				s.respondentWG.Delete(treadId)
			}()

			// Реагируем на отмену общего контекста: при отмене просто выходим, Respondent сам завершится по s.ctx.Done()
			select {
			case <-s.ctx.Done():
				logger.Debug("StarterRespondent canceled by Start context %s", u.RespName, u.Assist.UserId)
				return
			default:
			}

			s.Respondent(u, questionCh, answerCh, fullQuestCh, respId, treadId, errCh)
		}()
	}
}

// StarterListener запускает Listener для пользователя, если он ещё не запущен
func (s *Start) StarterListener(start model.StartCh, errCh chan error) {
	if !start.Model.Services.Listener.Load() {
		start.Model.Services.Listener.Store(true)
		go func() {
			defer func() { start.Model.Services.Listener.Store(false) }()
			// Создаём контекст listener, который завершится при отмене:
			// - родительского s.ctx (общий контекст Start)
			// - или контекста бота start.Ctx
			listenerCtx, listenerCancel := context.WithCancel(s.ctx)
			defer listenerCancel()

			// Связываем с контекстом бота
			go func() {
				select {
				case <-start.Ctx.Done():
					listenerCancel()
				case <-listenerCtx.Done():
				}
			}()

			// Если контекст бота уже отменён — не запускаем Listener
			select {
			case <-start.Ctx.Done():
				logger.Debug("StarterListener canceled by bot context %s %d", start.Model.RespName, start.Model.Assist.UserId)
				return
			default:
			}

			if err := s.Listener(start.Model, start.Chanel, start.RespId, start.TreadId); err != nil {
				select {
				case errCh <- err: // Отправляем ошибку в App
				default:
					logger.Warn("Не удалось отправить ошибку в errCh: %v", err, start.Model.Assist.UserId)
				}
			}
		}()
	}
}

// Listener слушает канал от пользователя и обрабатывает сообщения
func (s *Start) Listener(u *model.RespModel, usrCh *model.Ch, respId uint64, treadId uint64) error {
	question := make(chan Question, 10)
	fullQuestCh := make(chan Answer, 1)
	answerCh := make(chan Answer, 10)
	errCh := make(chan error, 1)

	// Создаем контекст для координированного завершения
	listenerCtx, listenerCancel := context.WithCancel(s.ctx)

	defer func() {
		logger.Debug("Закрытие каналов в Listener", u.Assist.UserId)

		listenerCancel() // Отменяем контекст перед закрытием каналов

		// Ждем завершения Respondent перед закрытием каналов
		if wgInterface, ok := s.respondentWG.Load(treadId); ok {
			wg := wgInterface.(*sync.WaitGroup)

			// Ждем с таймаутом
			done := make(chan struct{})
			go func() {
				wg.Wait()
				close(done)
			}()

			select {
			case <-done:
				logger.Debug("Respondent завершен, закрываем каналы", u.Assist.UserId)
			case <-time.After(5 * time.Second):
				logger.Warn("Таймаут ожидания завершения Respondent", u.Assist.UserId)
			}
		}

		close(question)
		close(fullQuestCh)
		close(answerCh)
		close(errCh)
	}()

	// Передаем контекст listener в модель пользователя
	userCtx, userCancel := context.WithCancel(listenerCtx)
	defer userCancel()

	// Обновляем контекст в модели пользователя
	u.Ctx = userCtx

	go s.StarterRespondent(u, question, answerCh, fullQuestCh, respId, treadId, errCh)

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
					errCh <- fmt.Errorf("неизвестный тип сообщения: %s для пользователя %d", msg.Type, u.Assist.UserId)
					continue
				}

				// Защита от паники при отправке в questionCh
				sendErr := func() error {

					select {
					case question <- quest:
						return nil
					case <-s.ctx.Done():
						logger.Debug("Контекст отменен при отправке в questionCh", u.Assist.UserId)
						return fmt.Errorf("контекст отменен")
					case <-time.After(100 * time.Millisecond):
						return fmt.Errorf("'Listener' таймаут отправки в questionCh (возможно закрыт)")
					default:
						return fmt.Errorf("'Listener' question канал questionCh закрыт или переполнен")
					}
				}()

				if sendErr != nil {
					return sendErr
				}

				// Отправляю вопрос клиента в виде сообщения с защитой от паники
				userMsg := s.Mod.NewMessage(msg.Operator, "user", &msg.Content, &msg.Name)
				if err := usrCh.SendToTx(userMsg); err != nil {
					// Фолбэк на прямую отправку с защитой
					func() {
						select {
						case usrCh.TxCh <- userMsg:
						default:
							logger.Warn("'Listener' question канал TxCh закрыт или переполнен: %v", err, u.Assist.UserId)
						}
					}()
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
			assistMsg := s.Mod.NewMessage(resp.Operator, "assist", &resp.Answer, &u.Assist.AssistName)

			// Безопасная отправка ответа в TxCh
			if err := usrCh.SendToTx(assistMsg); err != nil {
				// Фолбэк на прямую отправку с защитой
				func() {
					select {
					case usrCh.TxCh <- assistMsg:
					default:
						logger.Warn("'Listener' answer канал TxCh закрыт или переполнен: %v", err, u.Assist.UserId)
					}
				}()
			}

			// Сохраняем диалог после успешной отправки
			switch resp.Operator.Operator {
			case false:
				s.End.SaveDialog(comdb.AI, treadId, &resp.Answer) // убрал go для гарантированного порядка сохранения диалогов
			case true:
				s.End.SaveDialog(comdb.Operator, treadId, &resp.Answer) // убрал go для гарантированного порядка сохранения диалогов
			}
		}
	}
}
