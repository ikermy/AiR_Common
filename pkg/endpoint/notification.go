package endpoint

import (
	"bytes"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"time"

	"github.com/ikermy/AiR_Common/pkg/com"
	"github.com/ikermy/AiR_Common/pkg/mode"
	"github.com/nicksnyder/go-i18n/v2/i18n"
	"golang.org/x/text/language"
)

// sendHTTPRequest отправляет HTTP POST запрос с JSON payload
func sendHTTPRequest(url string, payload map[string]any) error {
	jsonData, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("ошибка при преобразовании данных в JSON: %w", err)
	}

	//if mode.ProductionMode {
	//	url = strings.Replace(url, "https://", "http://", 1)
	//}

	req, err := http.NewRequest("POST", url, bytes.NewBuffer(jsonData))
	if err != nil {
		return fmt.Errorf("ошибка при создании HTTP-запроса: %w", err)
	}

	// Устанавливаем заголовки
	req.Header.Set("Content-Type", "application/json")

	client := &http.Client{
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
		},
	}

	// Отправляем запрос
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("ошибка при отправке HTTP-запроса: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		bodyBytes, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("неожиданный статус ответа: %d, тело: %s", resp.StatusCode, string(bodyBytes))
	}

	return nil
}

func (e *Endpoint) SendEvent(userID uint32, event, userName, assistName, target string) {
	msg := com.CarpCh{
		UserID:     userID,
		Event:      event,
		UserName:   userName,
		AssistName: assistName,
		Target:     target,
	}

	select {
	case mode.CarpinteroCh <- msg:
	default:
		//logger.Warn("CarpinteroCh: канал закрыт или переполнен, не удалось отправить сообщение: %+v", msg)
	}
}

func (e *Endpoint) SendNotification(msg com.CarpCh) error {
	res, err := e.db.GetNotificationChannel(msg.UserID)
	if err != nil {
		return fmt.Errorf("ошибка получения каналов уведомлений: %w", err)
	}

	// Парсим JSON
	var channels []map[string]any
	err = json.Unmarshal(res, &channels)
	if err != nil {
		return fmt.Errorf("ошибка парсинга JSON: %v", err)
	}

	var lastError error
	successCount := 0

	for _, ch := range channels {
		switch ch["channel_type"] {
		case "instant":
			err := SendInstantNotification(msg.UserID, msg.Event, msg.UserName, msg.AssistName, msg.Target)
			if err != nil {
				//logger.Error("Ошибка отправки Instant уведомления: %v", err, msg.userID)
				lastError = err
				continue
			}
			successCount++

		case "telegram":
			// Проверяю что Telegram не null
			if ch["channel_value"] == "null" {
				//logger.Error("у пользователя %d не задан Telegram ID, уведомление не отправлено", msg.userID)
				lastError = fmt.Errorf("у пользователя %d не задан Telegram ID", msg.UserID)
				continue
			}
			// Подготовка сообщения Telegram
			telegramValue, ok := ch["channel_value"].(string)
			if !ok {
				//logger.Error("channel_value не является строкой", msg.userID)
				lastError = fmt.Errorf("channel_value не является строкой")
				continue
			}
			tId, err := strconv.ParseInt(telegramValue, 10, 64)
			if err != nil {
				//logger.Error("ошибка преобразования Telegram ID: %v", err, msg.userID)
				lastError = err
				continue
			}
			err = SendTelegramNotification(msg.UserID, tId, msg.Event, msg.UserName, msg.AssistName, msg.Target)
			if err != nil {
				//logger.Error("Ошибка отправки Telegram уведомления: %v", err, msg.userID)
				lastError = err
				continue
			}
			successCount++

		case "mail":
			// Проверяю что Email не null
			if ch["channel_value"] == "null" {
				//logger.Error("у пользователя %d не задан Email, уведомление не отправлено", msg.userID)
				lastError = fmt.Errorf("у пользователя %d не задан Email", msg.UserID)
				continue
			}
			// Подготовка сообщения Email
			emailValue, ok := ch["channel_value"].(string)
			if !ok {
				//logger.Error("channel_value не является строкой", msg.userID)
				lastError = fmt.Errorf("channel_value не является строкой")
				continue
			}
			err = SendEmailNotification(msg.UserID, emailValue, msg.Event, msg.UserName, msg.AssistName, msg.Target)
			if err != nil {
				//logger.Error("ошибка отправки Email уведомления: %v", err, msg.userID)
				lastError = err
				continue
			}
			successCount++

		default:
			//logger.Warn("Неизвестный канал уведомлений: %s", ch["channel_type"], msg.userID)
			lastError = fmt.Errorf("неизвестный канал уведомлений: %s", ch["channel_type"])
		}
	}

	// Если ни одно уведомление не отправилось успешно, возвращаем последнюю ошибку
	if successCount == 0 && lastError != nil {
		return lastError
	}

	return nil
}

func SendTelegramNotification(uid uint32, tId int64, event, userName, assistName, target string) error {
	const url = "http://airbff:8080/v1/notification/telega"

	payload := map[string]any{
		"uid":    uid,
		"tid":    tId,
		"event":  event,
		"user":   userName,
		"assist": assistName,
		"target": target,
	}

	return sendHTTPRequest(url, payload)
}

func SendEmailNotification(uid uint32, email, event, userName, assistName, target string) error {
	const url = "http://airbff:8080/v1/notification/mail"

	payload := map[string]any{
		"uid":    uid,
		"email":  email,
		"event":  event,
		"user":   userName,
		"assist": assistName,
		"target": target,
	}

	return sendHTTPRequest(url, payload)
}

func SendInstantNotification(uid uint32, event, userName, assistName, target string) error {
	const url = "http://airbff:8080/v1/notification/instant"

	payload := map[string]any{
		"uid":    uid,
		"event":  event,
		"user":   userName,
		"assist": assistName,
		"target": target,
	}

	return sendHTTPRequest(url, payload)
}

// PaymentInfo Структура для информации о инициации платежа
type PaymentInfo struct {
	UserID    int    `json:"userID"`
	Currency  string `json:"currency"`
	Amount    int    `json:"amount"`
	AmountUsd int    `json:"amountUsd"`
	OrderId   string `json:"orderId"`
	Network   string `json:"network"`
	ExpiresAt int64  `json:"expiresAt"`
}

// PaymentStatus Структура для информации о статусе платежа
type PaymentStatus struct {
	OrderID        string  `json:"orderId"`
	UserID         uint32  `json:"userID"`
	Status         string  `json:"status"`
	Currency       string  `json:"currency"`
	Network        string  `json:"network"`
	Amount         float64 `json:"amount"`
	AmountUsd      float64 `json:"amountUsd"`
	ReceivedAmount float64 `json:"receivedAmount"`
	TxHash         string  `json:"txHash"`
	Confirmations  int     `json:"confirmations"`
	CreatedAt      string  `json:"createdAt"`
	UpdatedAt      string  `json:"updatedAt"`
	ExpiresAt      string  `json:"expiresAt"`
}

type localizerWrapper struct {
	localizer *i18n.Localizer
}

func simpleLocalizer(lang string) (*localizerWrapper, error) {
	if lang != "ru" && lang != "en" && lang != "es" {
		return nil, fmt.Errorf("unsupported lang: %s", lang)
	}

	bundle := i18n.NewBundle(language.Russian)
	bundle.RegisterUnmarshalFunc("json", json.Unmarshal)

	translations := map[string]string{
		"ru": `[
			{"id":"system","translation":"Система"},
			{"id":"mode.operator.disabled","translation":"Режим оператора отключен"},
			{"id":"operator.disconnected","translation":"Оператор отключился. Маруся AI снова с вами!"},
			{"id":"operator.mode.is.disabled","translation":"Режим оператора отключен, возобновляю работу AI"},
			{"id":"notification","translation":"Уведомление"},
			{"id":"notification.from.marusia","translation":"Уведомление от MarusiaAI"},
			{"id":"sincerely.marusia.team","translation":"С уважением,<br>Команда MarusiaAI"},
			{"id":"confirm.registration","translation":"Подтверждение регистрации"},
			{"id":"welcome","translation":"Добро пожаловать в MarusiaAI!"},
			{"id":"for.confirm.registration","translation":"<p>Вы успешно зарегистрировались на сайте MarusiaAI.</p>
        <p>Для подтверждения вашего адреса электронной почты, пожалуйста, перейдите по следующей ссылке:</p>"},
     	    {"id":"if.you.haven.t.requested","translation":"<p style="color: #666; font-size: 14px;">Если вы не запрашивали подтверждение, просто проигнорируйте это письмо.</p>
        <p style="color: #666; font-size: 14px;">Ссылка действительна в течение ограниченного времени.</p>"},
			{"id":"password.recovery","translation":"Восстановление пароля"},
			{"id":"for.reset.password","translation":"Для сброса вашего пароля, пожалуйста, перейдите по следующей ссылке:"},
			{"id":"reset.password","translation":"Сбросить пароль"},
			{"id":"if.you.havenet.requested.password.reset","translation":"<p style="color: #666; font-size: 14px;">Если вы не запрашивали сброс пароля, просто проигнорируйте это письмо.</p>
        <p style="color: #666; font-size: 14px;">Ссылка действительна в течение ограниченного времени.</p>"}
		]`,
		"en": `[
			{"id":"system","translation":"System"},
			{"id":"mode.operator.disabled","translation":"Operator mode is disabled"},	
			{"id":"operator.disconnected","translation":"The operator has disconnected. Marusya AI is back with you!"},
			{"id":"operator.mode.is.disabled","translation":"Operator mode is disabled, resuming AI operation"},
			{"id":"notification","translation":"Notification"},
			{"id":"notification.from.marusia","translation":"Notification from MarusiaAI"},
			{"id":"sincerely.marusia.team","translation":"Sincerely,<br>MarusiaAI Team"},
			{"id":"confirm.registration","translation":"Registration Confirmation"},
			{"id":"welcome","translation":"Welcome to MarusiaAI!"},
			{"id":"for.confirm.registration","translation":"<p>You have successfully registered on MarusiaAI.</p>
        <p>To confirm your email address, please click the following link:</p>"},
     	    {"id":"if.you.haven.t.requested","translation":"<p style="color: #666; font-size: 14px;">If you did not request confirmation, please ignore this email.</p>
        <p style="color: #666; font-size: 14px;">The link is valid for a limited time.</p>"},
			{"id":"password.recovery","translation":"Password Recovery"},
			{"id":"for.reset.password","translation":"To reset your password, please click the following link:"},
			{"id":"reset.password","translation":"Reset Password"},
			{"id":"if.you.havenet.requested.password.reset","translation":"<p style="color: #666; font-size: 14px;">If you did not request a password reset, please ignore this email.</p>
        <p style="color: #666; font-size: 14px;">The link is valid for a limited time.</p>"}
		]`,
		"es": `[
			{"id":"system","translation":"Sistema"},
			{"id":"mode.operator.disabled","translation":"El modo operador está deshabilitado"},
			{"id":"operator.disconnected","translation":"El operador se ha desconectado. Marusya AI está de vuelta contigo!"},
			{"id":"operator.mode.is.disabled","translation":"El modo operador está deshabilitado, reanudando la operación de AI"},
			{"id":"notification","translation":"Notificación"},
			{"id":"notification.from.marusia","translation":"Notificación de MarusiaAI"},
			{"id":"sincerely.marusia.team","translation":"Atentamente,<br>Equipo MarusiaAI"},
			{"id":"confirm.registration","translation":"Confirmación de registro"},
			{"id":"welcome","translation":"¡Bienvenido a MarusiaAI!"},
			{"id":"for.confirm.registration","translation":"<p>Te has registrado correctamente en MarusiaAI.</p>
        <p>Para confirmar tu dirección de correo electrónico, haz clic en el siguiente enlace:</p>"},
     	    {"id":"if.you.haven.t.requested","translation":"<p style="color: #666; font-size: 14px;">Si no solicitaste la confirmación, ignora este correo electrónico.</p>
        <p style="color: #666; font-size: 14px;">El enlace es válido por un tiempo limitado.</p>"},
			{"id":"password.recovery","translation":"Recuperación de contraseña"},
			{"id":"for.reset.password","translation":"Para restablecer tu contraseña, haz clic en el siguiente enlace:"},
			{"id":"reset.password","translation":"Restablecer contraseña"},
			{"id":"if.you.havenet.requested.password.reset","translation":"<p style="color: #666; font-size: 14px;">Si no solicitaste un restablecimiento de contraseña, ignora este correo electrónico.</p>
        <p style="color: #666; font-size: 14px;">El enlace es válido por un tiempo limitado.</p>"}
		]`,
	}

	if _, err := bundle.ParseMessageFileBytes([]byte(translations[lang]), lang+".json"); err != nil {
		return nil, fmt.Errorf("failed to parse translations for %s: %w", lang, err)
	}

	return &localizerWrapper{localizer: i18n.NewLocalizer(bundle, lang)}, nil
}

func eventLocalizer(lang string) (*localizerWrapper, error) {
	if lang != "ru" && lang != "en" && lang != "es" {
		return nil, fmt.Errorf("unsupported lang: %s", lang)
	}

	bundle := i18n.NewBundle(language.Russian)
	bundle.RegisterUnmarshalFunc("json", json.Unmarshal)

	translations := map[string]string{
		"ru": `[
			{"id":"payment.status","translation":"\tСтатус: {{.Status}}\n\tВалюта: {{.Currency}}\n\tСумма: {{.Amount}}\n\tСумма в USD: {{.AmountUSD}}\n\tПоступление: {{.ReceivedAmount}}\n \tНомер заказа: {{.OrderID}}\n\tСеть: {{.Network}}\n\tХэш транзакции: {{.TxHash}}\n\tПодтверждения: {{.Confirmations}}\n\tСоздано: {{.CreatedAt}}\n\tОбновлено: {{.UpdatedAt}}\n\tСрок действия: {{.ExpiresAt}}"},
			{"id":"payment.new","translation":"новый платёж"},
			{"id":"payment.active","translation":"активный платёж"},
			{"id":"payment.pending","translation":"\tСтатус: {{.Status}}\n\tВалюта: {{.Currency}}\n\tСумма: {{.Amount}}\n\tСумма в USD: {{.AmountUSD}}\n\tНомер заказа: {{.OrderID}}\n\tСеть: {{.Network}}\n\tСрок действия: {{.ExpiresAt}}"},
			{"id":"event.usdt_pay.init","translation":"Сформирован счёт для оплаты подписки:\n{{.Payment}}"},
			{"id":"event.usdt_pay.pending","translation":"Инициирована оплата подписки:\n{{.Payment}}"},
			{"id":"event.usdt_pay.partial","translation":"Частичная оплата подписки:\n{{.Payment}}"},
			{"id":"event.usdt_pay.confirmed","translation":"Подтверждена оплата подписки:\n{{.Payment}}"},
			{"id":"event.usdt_pay.failed","translation":"Ошибка оплаты подписки:\n{{.Payment}}"},
			{"id":"event.start","translation":"Пользователь {{.UserName}} начал диалог с ассистентом {{.AssistName}}"},
			{"id":"event.end","translation":"Пользователь {{.UserName}} завершил диалог с ассистентом {{.AssistName}}"},
			{"id":"event.target","translation":"Ассистент {{.AssistName}} достиг цели '{{.Target}}' в диалоге с пользователем {{.UserName}}"},
			{"id":"event.trigger","translation":"Ассистент {{.AssistName}} сработал на триггер '{{.Target}}' в диалоге с пользователем {{.UserName}}"},
			{"id":"event.reauth","translation":"Канал {{.Target}} отключен, требуется повторная авторизация"},
			{"id":"event.reauth-userkey","translation":"Для работы требуется расшифровка пользовательских данных, пожалуйста, войдите в систему заново."},
			{"id":"event.model-operator","translation":"Ассистент {{.AssistName}} запросил переключение на оператора в диалоге с пользователем {{.UserName}}"},
			{"id":"subscription.no_subscription","translation":"У вас нет подписки. Пожалуйста, оформите подписку."},
			{"id":"subscription.expired","translation":"Ваша подписка истекла. Пожалуйста, продлите подписку."},
			{"id":"subscription.limit_exceeded","translation":"Вы превысили лимит сообщений. Пожалуйста, пополните баланс."},
			{"id":"subscription.insufficient_balance","translation":"Недостаточно средств на балансе. Пожалуйста, пополните баланс."},
			{"id":"event.lead-botunban","translation":"Боты:\n{{.Target}}\nразблокированы по таймеру, попробуйте их снова использовать"},
			{"id":"event.lead-start","translation":"Поиск лидов запущен:\n-всего контактов для обработки {{.Target}}"},
			{"id":"event.lead-stop","translation":"Поиск лидов завершён:\n-всего контактов {{.Target}}\n-обработанно {{.AssistName}}"},
			{"id":"event.ai-provider-limit.default","translation":"AI-провайдер"},
			{"id":"event.ai-provider-limit","translation":"⚠️ Проблема с подключением к {{.LimitInfo}}:\nпревышен лимит запросов или требуется оплата.\nПожалуйста, проверьте статус подписки и пополните баланс."}
		]`,
		"en": `[
			{"id":"payment.status","translation":"\tStatus: {{.Status}}\n\tCurrency: {{.Currency}}\n\tAmount: {{.Amount}}\n\tAmount in USD: {{.AmountUSD}}\n\tReceived: {{.ReceivedAmount}}\n \tOrder ID: {{.OrderID}}\n\tNetwork: {{.Network}}\n\tTransaction hash: {{.TxHash}}\n\tConfirmations: {{.Confirmations}}\n\tCreated: {{.CreatedAt}}\n\tUpdated: {{.UpdatedAt}}\n\tExpires at: {{.ExpiresAt}}"},
			{"id":"payment.new","translation":"new payment"},
			{"id":"payment.active","translation":"active payment"},
			{"id":"payment.pending","translation":"\tStatus: {{.Status}}\n\tCurrency: {{.Currency}}\n\tAmount: {{.Amount}}\n\tAmount in USD: {{.AmountUSD}}\n\tOrder ID: {{.OrderID}}\n\tNetwork: {{.Network}}\n\tExpires at: {{.ExpiresAt}}"},
			{"id":"event.usdt_pay.init","translation":"Subscription payment invoice created:\n{{.Payment}}"},
			{"id":"event.usdt_pay.pending","translation":"Subscription payment initiated:\n{{.Payment}}"},
			{"id":"event.usdt_pay.partial","translation":"Partial subscription payment:\n{{.Payment}}"},
			{"id":"event.usdt_pay.confirmed","translation":"Subscription payment confirmed:\n{{.Payment}}"},
			{"id":"event.usdt_pay.failed","translation":"Subscription payment failed:\n{{.Payment}}"},
			{"id":"event.start","translation":"User {{.UserName}} started a dialog with assistant {{.AssistName}}"},
			{"id":"event.end","translation":"User {{.UserName}} ended the dialog with assistant {{.AssistName}}"},
			{"id":"event.target","translation":"Assistant {{.AssistName}} reached the goal '{{.Target}}' in the dialog with user {{.UserName}}"},
			{"id":"event.trigger","translation":"Assistant {{.AssistName}} triggered on '{{.Target}}' in the dialog with user {{.UserName}}"},
			{"id":"event.reauth","translation":"Channel {{.Target}} is disconnected, re-authorization is required"},
			{"id":"event.reauth-userkey","translation":"User data decryption is required to continue, please sign in again."},
			{"id":"event.model-operator","translation":"Assistant {{.AssistName}} requested switching to an operator in the dialog with user {{.UserName}}"},
			{"id":"subscription.no_subscription","translation":"You do not have a subscription. Please subscribe."},
			{"id":"subscription.expired","translation":"Your subscription has expired. Please renew it."},
			{"id":"subscription.limit_exceeded","translation":"You have exceeded the message limit. Please top up your balance."},
			{"id":"subscription.insufficient_balance","translation":"Insufficient balance. Please top up your account."},
			{"id":"event.lead-botunban","translation":"Bots:\n{{.Target}}\nhave been unblocked by timer, try using them again"},
			{"id":"event.lead-start","translation":"Lead search started:\n-total contacts to process {{.Target}}"},
			{"id":"event.lead-stop","translation":"Lead search completed:\n-total contacts {{.Target}}\n-processed {{.AssistName}}"},
			{"id":"event.ai-provider-limit.default","translation":"AI provider"},
			{"id":"event.ai-provider-limit","translation":"⚠️ Connection issue with {{.LimitInfo}}:\nrequest limit exceeded or payment required.\nPlease check your subscription status and top up your balance."}
		]`,
		"es": `[
			{"id":"payment.status","translation":"\tEstado: {{.Status}}\n\tMoneda: {{.Currency}}\n\tImporte: {{.Amount}}\n\tImporte en USD: {{.AmountUSD}}\n\tRecibido: {{.ReceivedAmount}}\n \tID del pedido: {{.OrderID}}\n\tRed: {{.Network}}\n\tHash de transacción: {{.TxHash}}\n\tConfirmaciones: {{.Confirmations}}\n\tCreado: {{.CreatedAt}}\n\tActualizado: {{.UpdatedAt}}\n\tExpira: {{.ExpiresAt}}"},
			{"id":"payment.new","translation":"nuevo pago"},
			{"id":"payment.active","translation":"pago activo"},
			{"id":"payment.pending","translation":"\tEstado: {{.Status}}\n\tMoneda: {{.Currency}}\n\tImporte: {{.Amount}}\n\tImporte en USD: {{.AmountUSD}}\n\tID del pedido: {{.OrderID}}\n\tRed: {{.Network}}\n\tExpira: {{.ExpiresAt}}"},
			{"id":"event.usdt_pay.init","translation":"Se generó una factura para pagar la suscripción:\n{{.Payment}}"},
			{"id":"event.usdt_pay.pending","translation":"Pago de suscripción iniciado:\n{{.Payment}}"},
			{"id":"event.usdt_pay.partial","translation":"Pago parcial de la suscripción:\n{{.Payment}}"},
			{"id":"event.usdt_pay.confirmed","translation":"Pago de suscripción confirmado:\n{{.Payment}}"},
			{"id":"event.usdt_pay.failed","translation":"Error en el pago de la suscripción:\n{{.Payment}}"},
			{"id":"event.start","translation":"El usuario {{.UserName}} inició un diálogo con el asistente {{.AssistName}}"},
			{"id":"event.end","translation":"El usuario {{.UserName}} finalizó el diálogo con el asistente {{.AssistName}}"},
			{"id":"event.target","translation":"El asistente {{.AssistName}} alcanzó el objetivo '{{.Target}}' en el diálogo con el usuario {{.UserName}}"},
			{"id":"event.trigger","translation":"El asistente {{.AssistName}} se activó por el disparador '{{.Target}}' en el diálogo con el usuario {{.UserName}}"},
			{"id":"event.reauth","translation":"El canal {{.Target}} está desconectado, se requiere una nueva autorización"},
			{"id":"event.reauth-userkey","translation":"Se requiere descifrar los datos del usuario para continuar; por favor, vuelva a iniciar sesión."},
			{"id":"event.model-operator","translation":"El asistente {{.AssistName}} solicitó cambiar a un operador en el diálogo con el usuario {{.UserName}}"},
			{"id":"subscription.no_subscription","translation":"No tiene una suscripción. Por favor, suscríbase."},
			{"id":"subscription.expired","translation":"Su suscripción ha expirado. Por favor, renuévela."},
			{"id":"subscription.limit_exceeded","translation":"Ha superado el límite de mensajes. Por favor, recargue su saldo."},
			{"id":"subscription.insufficient_balance","translation":"Saldo insuficiente. Por favor, recargue su cuenta."},
			{"id":"event.lead-botunban","translation":"Bots:\n{{.Target}}\nse desbloquearon por temporizador, inténtelos de nuevo"},
			{"id":"event.lead-start","translation":"Búsqueda de leads iniciada:\n-total de contactos para procesar {{.Target}}"},
			{"id":"event.lead-stop","translation":"Búsqueda de leads finalizada:\n-total de contactos {{.Target}}\n-procesados {{.AssistName}}"},
			{"id":"event.ai-provider-limit.default","translation":"Proveedor de IA"},
			{"id":"event.ai-provider-limit","translation":"⚠️ Problema de conexión con {{.LimitInfo}}:\nse superó el límite de solicitudes o se requiere pago.\nPor favor, verifique el estado de su suscripción y recargue su saldo."}
		]`,
	}

	if _, err := bundle.ParseMessageFileBytes([]byte(translations[lang]), lang+".json"); err != nil {
		return nil, fmt.Errorf("failed to parse translations for %s: %w", lang, err)
	}

	return &localizerWrapper{localizer: i18n.NewLocalizer(bundle, lang)}, nil
}

func (l *localizerWrapper) mustLocalize(messageID string, templateData map[string]any) (string, error) {
	msg, err := l.localizer.Localize(&i18n.LocalizeConfig{
		MessageID:    messageID,
		TemplateData: templateData,
	})
	if err != nil {
		return "", err
	}

	return msg, nil
}

// CreateMessageFromEvent создает сообщение на основе события
func CreateMessageFromEvent(lang, Event, UserName, AssistName, Target string) (string, error) {
	var msg, payment string

	loc, err := eventLocalizer(lang)
	if err != nil {
		return "", err
	}

	if AssistName != "init" && Event == "usdt_pay" {
		var paritalInfo PaymentStatus
		err := json.Unmarshal([]byte(Target), &paritalInfo)
		if err != nil {
			return "", fmt.Errorf("ошибка парсинга PaymentStatus: %v", err)
		}

		layout := "2006-01-02 15:04:05"
		createdAt, err1 := time.Parse(layout, paritalInfo.CreatedAt)
		updatedAt, err2 := time.Parse(layout, paritalInfo.UpdatedAt)
		expiresAt, err3 := time.Parse(layout, paritalInfo.ExpiresAt)

		formatOrRaw := func(t time.Time, err error, raw string) string {
			if err != nil {
				return raw
			}
			return t.Format("02.01.2006 15:04:05")
		}

		payment, err = loc.mustLocalize("payment.status", map[string]any{
			"Status":         paritalInfo.Status,
			"Currency":       paritalInfo.Currency,
			"Amount":         fmt.Sprintf("%.2f", paritalInfo.Amount),
			"AmountUSD":      fmt.Sprintf("%.2f", paritalInfo.AmountUsd),
			"ReceivedAmount": fmt.Sprintf("%.2f", paritalInfo.ReceivedAmount),
			"OrderID":        paritalInfo.OrderID,
			"Network":        paritalInfo.Network,
			"TxHash":         paritalInfo.TxHash,
			"Confirmations":  paritalInfo.Confirmations,
			"CreatedAt":      formatOrRaw(createdAt, err1, paritalInfo.CreatedAt),
			"UpdatedAt":      formatOrRaw(updatedAt, err2, paritalInfo.UpdatedAt),
			"ExpiresAt":      formatOrRaw(expiresAt, err3, paritalInfo.ExpiresAt),
		})
		if err != nil {
			return "", fmt.Errorf("failed to localize payment status: %w", err)
		}
	}
	switch Event {
	// События оплаты подписки
	case "usdt_pay":

		switch AssistName {
		case "init":
			// Для инициализации платежа своя структура
			var (
				answer  string
				payInfo PaymentInfo
			)
			err := json.Unmarshal([]byte(Target), &payInfo)
			if err != nil {
				return "", fmt.Errorf("ошибка парсинга PaymentInfo: %v", err)
			}

			if UserName == "false" {
				answer, err = loc.mustLocalize("payment.new", nil)
			} else {
				answer, err = loc.mustLocalize("payment.active", nil)
			}
			if err != nil {
				return "", fmt.Errorf("failed to localize payment state: %w", err)
			}

			expiresAt := int64(1755884144)
			t := time.Unix(expiresAt, 0)

			pending, err := loc.mustLocalize("payment.pending", map[string]any{
				"Status":    answer,
				"Currency":  payInfo.Currency,
				"Amount":    payInfo.Amount,
				"AmountUSD": payInfo.AmountUsd,
				"OrderID":   payInfo.OrderId,
				"Network":   payInfo.Network,
				"ExpiresAt": t.Format("02.01.2006 15:04:05"),
			})
			if err != nil {
				return "", fmt.Errorf("failed to localize pending payment: %w", err)
			}

			msg, err = loc.mustLocalize("event.usdt_pay.init", map[string]any{"Payment": pending})
		case "pending":
			msg, err = loc.mustLocalize("event.usdt_pay.pending", map[string]any{"Payment": payment})
		case "partial":
			msg, err = loc.mustLocalize("event.usdt_pay.partial", map[string]any{"Payment": payment})
		case "confirmed":
			msg, err = loc.mustLocalize("event.usdt_pay.confirmed", map[string]any{"Payment": payment})
		case "failed":
			msg, err = loc.mustLocalize("event.usdt_pay.failed", map[string]any{"Payment": payment})
		default:
			return "", fmt.Errorf("Неизвестное событие pay:\n%s", AssistName)
		}
		if err != nil {
			return "", fmt.Errorf("failed to localize usdt_pay event: %w", err)
		}

	// События диалога с ассистентом
	case "start":
		msg, err = loc.mustLocalize("event.start", map[string]any{"UserName": UserName, "AssistName": AssistName})
	case "end":
		msg, err = loc.mustLocalize("event.end", map[string]any{"UserName": UserName, "AssistName": AssistName})
	case "target":
		msg, err = loc.mustLocalize("event.target", map[string]any{"AssistName": AssistName, "Target": Target, "UserName": UserName})
	case "trigger":
		msg, err = loc.mustLocalize("event.trigger", map[string]any{"AssistName": AssistName, "Target": Target, "UserName": UserName})
	case "reauth":
		msg, err = loc.mustLocalize("event.reauth", map[string]any{"Target": Target})
	case "reauth-userkey":
		msg, err = loc.mustLocalize("event.reauth-userkey", nil)
	case "model-operator":
		msg, err = loc.mustLocalize("event.model-operator", map[string]any{"AssistName": AssistName, "UserName": UserName})
	// События подписки
	case "subscription":
		errMsg := map[com.ErrorCode]string{
			com.ErrNoSubscription:       "subscription.no_subscription",
			com.ErrSubscriptionExpired:  "subscription.expired",
			com.ErrMessageLimitExceeded: "subscription.limit_exceeded",
			com.ErrInsufficientBalance:  "subscription.insufficient_balance",
		}
		errorCode, _ := strconv.Atoi(Target)
		msg, err = loc.mustLocalize(errMsg[com.ErrorCode(errorCode)], nil)
		// Разбан ботов для service lead generation
	case "lead-botunban":
		msg, err = loc.mustLocalize("event.lead-botunban", map[string]any{"Target": Target})
	case "lead-start":
		msg, err = loc.mustLocalize("event.lead-start", map[string]any{"Target": Target})
	case "lead-stop":
		msg, err = loc.mustLocalize("event.lead-stop", map[string]any{"Target": Target, "AssistName": AssistName})
	// События превышения лимита AI-провайдера
	case "ai-provider-limit":
		// Target содержит информацию о провайдере и/или коде ошибки
		// Например: "OpenAI: 429 Too Many Requests" или "Mistral: rate_limit_exceeded"
		limitInfo := Target
		if limitInfo == "" {
			limitInfo, err = loc.mustLocalize("event.ai-provider-limit.default", nil)
			if err != nil {
				return "", fmt.Errorf("failed to localize ai provider default text: %w", err)
			}
		}
		msg, err = loc.mustLocalize("event.ai-provider-limit", map[string]any{"LimitInfo": limitInfo})
	default:
		return "", fmt.Errorf("неизвестное событие: %s", Event)
	}

	if err != nil {
		return "", fmt.Errorf("failed to localize event message: %w", err)
	}

	return msg, nil
}

func (e *Endpoint) NotificationListener(notifCh chan<- com.LogMsg) {
	notifCh <- com.LogMsg{
		Msg: "запуск 'NotificationListener' для прослушивания канала mode.CarpinteroCh",
		Mod: "Endpoint",
		Log: 0, // 0 - Info
		UID: 0,
	}

	for {
		select {
		case <-e.ctx.Done():
			notifCh <- com.LogMsg{
				Msg: "остановка 'NotificationListener' из-за завершения контекста",
				Mod: "Endpoint",
				Log: 0, // 0 - Info
				UID: 0,
			}
			return
		case msg, ok := <-mode.CarpinteroCh:
			if !ok {
				notifCh <- com.LogMsg{
					Msg: "mode.CarpinteroCh closed",
					Mod: "Endpoint",
					Log: 1, // 1 - Error
					UID: 0,
				}
				return
			}
			err := e.SendNotification(msg)
			if err != nil {
				notifCh <- com.LogMsg{
					Msg: fmt.Sprintf("'NotificationListener': ошибка отправки уведомления: %v", err),
					Mod: "Endpoint",
					Log: 1, // 1 - Error
					UID: msg.UserID,
				}
			}
		}
	}
}

func (e *Endpoint) TranslateMessageWithUserID(userID uint32, message string) string {
	lang := e.db.UserLanguage(userID)

	loc, err := simpleLocalizer(lang)
	if err != nil {
		return ""
	}

	answer, err := loc.mustLocalize(message, nil)
	if err != nil {
		return ""
	}

	return answer
}

func (e *Endpoint) TranslateMessageWithLang(lang, message string) string {
	loc, err := simpleLocalizer(lang)
	if err != nil {
		return ""
	}

	answer, err := loc.mustLocalize(message, nil)
	if err != nil {
		return ""
	}

	return answer
}
