package comdb

import (
	"bytes"
	"compress/gzip"
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/ikermy/AiR_Common/pkg/crypto"
	"github.com/ikermy/AiR_Common/pkg/mode"
	"github.com/ikermy/AiR_Common/pkg/model/create"
	"golang.org/x/oauth2"

	_ "github.com/go-sql-driver/mysql"
)

const sqlTimeToCancel = 5 // Тайм-аут на операции с БД

type Exterior interface {
	GetOrSetTreadAndResponder(userID uint32, responderRealId uint64, responderName string, chatType ChatType) (uint64, error)
	DisableAllUserChannel(userID uint32) error
	GetNotificationChannel(userID uint32) (json.RawMessage, error)
	GetUserSubscriptionLimites(userID uint32) (json.RawMessage, error)
	SaveDialog(treadId uint64, message json.RawMessage) error
	ReadDialog(dialogId uint64, limit ...uint8) (json.RawMessage, error)
	DeleteDialog(userID uint32, dialogId uint64) error
	UpdateDialogsMeta(dialogId uint64, meta string) error
	ReadContext(dialogId uint64, provider create.ProviderType) (json.RawMessage, error)
	SaveContext(threadId uint64, provider create.ProviderType, dialogContext json.RawMessage) error
	GetActiveProvider(userID uint32) (create.ProviderType, error)
	GetAllUserModels(userID uint32) ([]create.UserModelRecord, error)
	UpdateUserGPT(userID uint32, modelId uint64, assistId string, allIds []byte) error
	GetUserVectorStorage(userID uint32) (string, error)
	SetChannelEnabled(userID uint32, chName string, status bool) error
	SaveUserModel(userID uint32, provider create.ProviderType, name, assistantId string, data []byte, modType uint, ids json.RawMessage, operator bool) error
	SyncProviderModels(provider create.ProviderType, modelNames []string) (create.ProviderModelsSyncResult, error)
	GetOrSetUserStorageLimit(userID uint32, setStorage int64) (remaining uint64, totalLimit uint64, err error)
	ReadUserModel(userID uint32) ([]byte, *create.VecIds, error)
	SetUserSubscriptionNotified(user uint32) error
	DefaultProvidersModels(providerName string) (uint8, string, error)
	ModelsNameByProvider(provider create.ProviderType) ([]string, error)

	// User Model Management - методы для управления моделями пользователя (для create.DB)
	ReadUserModelByProvider(userID uint32, provider create.ProviderType) ([]byte, *create.VecIds, error)
	GetActiveModel(userID uint32) (*create.UserModelRecord, error)
	GetModelByProvider(userID uint32, provider create.ProviderType) (*create.UserModelRecord, error)
	GetModelByProviderAnyStatus(userID uint32, provider create.ProviderType) (*create.UserModelRecord, error)
	SetActiveModel(userID uint32, modelId uint64) error
	SetActiveModelByProvider(userID uint32, provider create.ProviderType) error
	RemoveModelFromUser(userID uint32, modelId uint64) error

	// Vector Embeddings - методы для работы с эмбеддингами в MariaDB
	SaveEmbedding(userID uint32, modelId uint64, provider create.ProviderType, docID, docName, content string, embedding []float32, metadata create.DocumentMetadata) error
	GetEmbedding(modelId uint64, docID string) ([]float32, error)
	DeleteEmbedding(modelId uint64, docID string) error
	DeleteAllModelEmbeddings(modelId uint64) error
	CountModelEmbeddings(modelId uint64) (int, error)
	ListModelEmbeddings(modelId uint64, provider create.ProviderType) ([]create.VectorDocument, error)
	SearchSimilarEmbeddings(modelId uint64, provider create.ProviderType, queryEmbedding []float32, limit int) ([]create.VectorDocument, error)

	// Contact Availability - методы для работы с доступностью контактов в разных провайдерах
	SetContactAvailability(userID uint32, contact, provider string, isAvailable bool) error
	GetContactAvailability(userID uint32, contact string) (map[string]bool, error)
	GetContactsAvailableIn(userID uint32, provider string) ([]string, error)
	GetContactsInBothProviders(userID uint32, provider1, provider2 string) ([]string, error)

	// Google OAuth методы (токен единый для пользователя, не зависит от провайдера/модели)
	SaveGoogleToken(userID uint32, googleEmail string, token *oauth2.Token) error
	GetGoogleToken(userID uint32) (*oauth2.Token, string, error)
	RefreshGoogleTokenIfNeeded(userID uint32, oauthConfig *oauth2.Config) error
	DeleteGoogleToken(userID uint32) error

	// UserInfo методы
	UserTimeZone(userID uint32) (string, error)
	UserLanguage(userID uint32) string

	// UserAPIKey — персональные API-ключи провайдеров для каждого пользователя.
	GetUserAPIKey(userID uint32, provider ProviderType) (string, error)
	SetUserAPIKey(userId uint32, provider ProviderType, apiKey string) error
	DeleteUserAPIKey(userID uint32, provider ProviderType) error
}

// ChatType определяет тип чата (используется в БД)
type ChatType uint8

const (
	TelegramBot ChatType = 0
	Web         ChatType = 1
	Telegram    ChatType = 2
	Avito       ChatType = 3
	Widget      ChatType = 4
	WhatsApp    ChatType = 5
	Instagram   ChatType = 6
)

type Espero struct {
	Limit  uint16 `json:"limit"`
	Wait   uint8  `json:"wait"`
	Ignore bool   `json:"ignore"`
}

type CreatorType uint8

const (
	AI                 CreatorType = 1 // Право
	User               CreatorType = 2 // Лево
	UserVoice          CreatorType = 3 // Лево
	Operator           CreatorType = 4 // Прав
	SpeechRealTimeAI   CreatorType = 5 // Прав
	SpeechRealTimeUser CreatorType = 6 // Лево
)

// Используем типы из пакета model/create для совместимости с интерфейсом create.DB
type (
	Ids             = create.Ids
	VecIds          = create.VecIds
	UserModelRecord = create.UserModelRecord
	ProviderType    = create.ProviderType
)

// DB представляет соединение с базой данных
type DB struct {
	dsn               string
	MasterKeyResolver MasterKeyResolver
	conn              *sql.DB
	mainCTX           context.Context
	ctx               context.Context
	cancel            context.CancelFunc
}

// MasterKeyResolver returns the user's decrypted MasterKey from cache or remote.
type MasterKeyResolver func(userId uint32) ([32]byte, bool)

// Метод инъекции:
func (d *DB) SetMasterKeyResolver(r MasterKeyResolver) {
	d.MasterKeyResolver = r
}

// New создает новое подключение к базе данных
func New(parent context.Context) (*DB, error) {
	host := os.Getenv("DB_HOST")
	name := os.Getenv("DB_NAME")
	user := os.Getenv("DB_USER")
	pass := os.Getenv("DB_PASSWORD")

	dsn := fmt.Sprintf("%s:%s@tcp(%s)/%s?parseTime=true&charset=utf8mb4&loc=Local",
		user, pass, host, name)

	conn, err := sql.Open("mysql", dsn)
	if err != nil {
		return nil, err
	}

	// Пул соединений
	conn.SetMaxOpenConns(100)
	conn.SetMaxIdleConns(100)
	conn.SetConnMaxLifetime(5 * time.Minute)

	ctx, cancel := context.WithCancel(context.Background())

	return &DB{
		mainCTX: parent,
		ctx:     ctx,
		cancel:  cancel,
		dsn:     dsn,
		conn:    conn,
	}, nil
}

// Close закрывает соединения с базой данных и отменяет контекст
func (d *DB) Close() error {
	// Отменяем контекст базы данных
	if d.cancel != nil {
		d.cancel()
	}

	// Закрываем соединение с базой данных
	if d.conn != nil {
		if err := d.conn.Close(); err != nil {
			return err
		}
	}

	return nil
}

// Conn возвращает базовое подключение к БД для расширенного использования в приложениях
func (d *DB) Conn() *sql.DB {
	return d.conn
}

// Context возвращает контекст БД для использования в пользовательских методах
func (d *DB) Context() context.Context {
	return d.ctx
}

// MainCTX возвращает главный контекст приложения
func (d *DB) MainCTX() context.Context {
	return d.mainCTX
}

// DecompressAndExtractMetadata Функция для распаковки сжатых данных и извлечения полей Meta и MetaAction
// Также извлекает параметры Google модели: Image, WebSearch, Video, Haunter и Search.
// Примечание: calendar и sheets (GOAuth) удалены — теперь управляются исключительно MCP сервером.
func DecompressAndExtractMetadata(compressedData []byte) (metaAction string, triggers []string, espero *Espero, image, webSearch, video, haunter, search, operator, s3, interpreter bool, err error) {
	// Создаем reader для распаковки данных
	gzipReader, err := gzip.NewReader(bytes.NewReader(compressedData))
	if err != nil {
		return "", nil, nil, false, false, false, false, false, false, false, false, fmt.Errorf("ошибка при создании gzip reader: %w", err)
	}
	defer func(gzipReader *gzip.Reader) {
		_ = gzipReader.Close()
	}(gzipReader)

	// Читаем распакованные данные
	decompressedData, err := io.ReadAll(gzipReader)
	if err != nil {
		return "", nil, nil, false, false, false, false, false, false, false, false, fmt.Errorf("ошибка при распаковке данных: %w", err)
	}

	// Разбираем JSON
	var modelData map[string]any
	if err := json.Unmarshal(decompressedData, &modelData); err != nil {
		return "", nil, nil, false, false, false, false, false, false, false, false, fmt.Errorf("ошибка при разборе JSON модели: %w", err)
	}

	// Извлекаем поля MetaAction
	espero = &Espero{}

	if ma, ok := modelData["mact"].(string); ok {
		metaAction = ma
	}

	// Извлекаем и конвертируем поле triggers
	if t, ok := modelData["trig"]; ok {
		if trigArray, ok := t.([]any); ok {
			for _, item := range trigArray {
				if str, ok := item.(string); ok {
					triggers = append(triggers, str)
				}
			}
		}
	}

	// Извлекаем поля espero
	if esp, ok := modelData["espero"].(map[string]any); ok {
		if limit, ok := esp["limit"].(float64); ok {
			espero.Limit = uint16(limit)
		}
		if wait, ok := esp["wait"].(float64); ok {
			espero.Wait = uint8(wait)
		}
		if ignore, ok := esp["ignore"].(bool); ok {
			espero.Ignore = ignore
		}
	}

	// Извлекаем параметры Google модели (image, web_search, video)
	if val, ok := modelData["image"].(bool); ok {
		image = val
	}
	if val, ok := modelData["web_search"].(bool); ok {
		webSearch = val
	}
	if val, ok := modelData["video"].(bool); ok {
		video = val
	}

	// Извлекаем флаг haunter
	if val, ok := modelData["haunter"].(bool); ok {
		haunter = val
	}

	// Извлекаем флаг search
	if val, ok := modelData["search"].(bool); ok {
		search = val
	}

	// Извлекаем флаг operator
	if val, ok := modelData["operator"].(bool); ok {
		operator = val
	}

	// Извлекаем флаги для Google Services
	if val, ok := modelData["s3"].(bool); ok {
		s3 = val
	}
	if val, ok := modelData["interpreter"].(bool); ok {
		interpreter = val
	}

	// g_oauth (GOAuth.Calendar, GOAuth.Sheets) намеренно не извлекается:
	// Calendar/Sheets инструменты управляются исключительно MCP сервером (tools/list).

	return metaAction, triggers, espero, image, webSearch, video, haunter, search, operator, s3, interpreter, nil
}

// ReadContext читает контекст диалога из базы данных
func (d *DB) ReadContext(dialogId uint64, provider create.ProviderType) (json.RawMessage, error) {
	if dialogId == 0 {
		return nil, fmt.Errorf("получен пустой dialogId")
	}

	ctx, cancel := context.WithTimeout(d.Context(), mode.SqlTimeToCancel)
	defer cancel()

	var data sql.NullString
	if err := d.Conn().QueryRowContext(ctx, "SELECT ReadContext(?, ?)", dialogId, provider.String()).Scan(&data); err != nil {
		switch {
		case errors.Is(err, context.DeadlineExceeded):
			return nil, fmt.Errorf("тайм-аут (%d с) при вызове ReadContext: %w", mode.SqlTimeToCancel, err)
		case errors.Is(err, context.Canceled):
			return nil, fmt.Errorf("операция отменена при вызове ReadContext: %w", err)
		case errors.Is(err, sql.ErrNoRows):
			return nil, fmt.Errorf("контекст диалога не найден")
		default:
			return nil, fmt.Errorf("ошибка вызова хранимой функции ReadContext: %w", err)
		}
	}

	if !data.Valid {
		return nil, fmt.Errorf("получены пустые данные")
	}

	return json.RawMessage(data.String), nil
}

// SaveContext сохраняет контекст диалога в базу данных
func (d *DB) SaveContext(threadId uint64, provider create.ProviderType, dialogContext json.RawMessage) error {
	if threadId == 0 {
		return fmt.Errorf("получен пустой тред")
	}

	ctx, cancel := context.WithTimeout(d.Context(), mode.SqlTimeToCancel)
	defer cancel()

	if _, err := d.Conn().ExecContext(ctx, "CALL SaveContext(?, ?, ?)", threadId, provider.String(), dialogContext); err != nil {
		switch {
		case errors.Is(err, context.DeadlineExceeded):
			return fmt.Errorf("тайм-аут (%d с) при сохранении контекста: %w", mode.SqlTimeToCancel, err)
		case errors.Is(err, context.Canceled):
			return fmt.Errorf("операция отменена: %w", err)
		default:
			return fmt.Errorf("ошибка сохранения контекста: %w", err)
		}
	}

	return nil
}

// ReadDialog читает всю историю диалога и возвращает структурированные данные
func (d *DB) ReadDialog(dialogId uint64, limit ...uint8) (json.RawMessage, error) {
	if dialogId == 0 {
		return nil, fmt.Errorf("получен некорректный dialogId")
	}

	ctx, cancel := context.WithTimeout(d.Context(), sqlTimeToCancel*time.Second)
	defer cancel()

	var raw sql.NullString
	var err error
	if len(limit) == 0 {
		err = d.Conn().QueryRowContext(ctx, "SELECT ReadDialog(?, NULL);", dialogId).Scan(&raw)
	} else {
		err = d.Conn().QueryRowContext(ctx, "SELECT ReadDialog(?, ?);", dialogId, limit[0]).Scan(&raw)
	}
	if err != nil {
		switch {
		case errors.Is(err, context.DeadlineExceeded):
			return nil, fmt.Errorf("тайм-аут при вызове ReadDialog: %w", err)
		case errors.Is(err, context.Canceled):
			return nil, fmt.Errorf("операция отменена: %w", err)
		case errors.Is(err, sql.ErrNoRows):
			return nil, fmt.Errorf("диалог не найден")
		default:
			return nil, fmt.Errorf("ошибка ReadDialog: %w", err)
		}
	}
	if !raw.Valid {
		return nil, fmt.Errorf("получены пустые данные")
	}

	result := json.RawMessage(raw.String)

	// Обрабатываем результат: расшифровка (если нужно) и нормализация массива Data
	result = d.processReadDialogResult(ctx, dialogId, result)

	return result, nil
}

// processReadDialogResult выполняет расшифровку и нормализацию данных диалога.
func (d *DB) processReadDialogResult(ctx context.Context, dialogId uint64, raw json.RawMessage) json.RawMessage {
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(raw, &obj); err != nil {
		return raw
	}

	dataField, ok := obj["Data"]
	if !ok {
		return raw
	}

	processedData := dataField

	// 1. Пробуем расшифровать, если поле Data является строкой с префиксом $mk$
	var dataStr string
	if err := json.Unmarshal(dataField, &dataStr); err == nil && crypto.IsEncryptedWithMasterKey(dataStr) {
		if d.MasterKeyResolver != nil {
			var userId uint32
			if err := d.Conn().QueryRowContext(ctx,
				"SELECT `User` FROM dialogs WHERE Id = ? LIMIT 1", dialogId).Scan(&userId); err == nil {
				if mk, ok := d.MasterKeyResolver(userId); ok {
					if plain, err := crypto.DecryptFieldWithMasterKey(mk, dataStr); err == nil {
						processedData = json.RawMessage(plain)
					}
				}
			}
		}
	}

	// 2. Всегда нормализуем массив (превращаем строки-JSON в объекты)
	obj["Data"] = d.normalizeDataArray(processedData)

	result, _ := json.Marshal(obj)
	return result
}

// normalizeDataArray гарантирует, что каждый элемент в массиве является JSON-объектом, а не строкой.
func (d *DB) normalizeDataArray(data json.RawMessage) json.RawMessage {
	var arr []json.RawMessage
	if err := json.Unmarshal(data, &arr); err != nil {
		return data
	}

	changed := false
	for i, item := range arr {
		var s string
		// Если элемент — это JSON-строка (начинается на "), пробуем её распарсить
		if len(item) > 0 && item[0] == '"' {
			if err := json.Unmarshal(item, &s); err == nil {
				trimmed := strings.TrimSpace(s)
				// Проверяем, что внутри действительно объект или массив
				if (strings.HasPrefix(trimmed, "{") && strings.HasSuffix(trimmed, "}")) ||
					(strings.HasPrefix(trimmed, "[") && strings.HasSuffix(trimmed, "]")) {
					arr[i] = json.RawMessage(trimmed)
					changed = true
				}
			}
		}
	}

	if !changed {
		return data
	}

	newBytes, err := json.Marshal(arr)
	if err != nil {
		return data
	}
	return json.RawMessage(newBytes)
}

// DeleteDialog удаляет диалог с проверкой прав пользователя
func (d *DB) DeleteDialog(userID uint32, dialogId uint64) error {
	// Проверяем входные значения
	if dialogId == 0 {
		return fmt.Errorf("получен некорректный dialogId")
	}
	if userID == 0 {
		return fmt.Errorf("получен некорректный userID")
	}

	// Дочерний контекст с тайм-аутом на операцию
	ctx, cancel := context.WithTimeout(d.Context(), sqlTimeToCancel*time.Second)
	defer cancel()

	// Вызываем хранимую процедуру с проверкой прав
	_, err := d.Conn().ExecContext(ctx, "CALL DeleteDialog(?, ?)", dialogId, userID)
	if err != nil {
		// Проверяем специальный код ошибки для демо-пользователя
		if strings.Contains(err.Error(), "SQLSTATE 45001") ||
			strings.Contains(err.Error(), "Невозможно удалить диалог демо пользователя") {
			return fmt.Errorf("демо пользователь не может удалять диалоги")
		}

		switch {
		case errors.Is(err, context.DeadlineExceeded):
			return fmt.Errorf("тайм-аут (%d с) при удалении диалога: %w", sqlTimeToCancel, err)
		case errors.Is(err, context.Canceled):
			return fmt.Errorf("операция отменена: %w", err)
		default:
			return fmt.Errorf("ошибка удаления диалога: %w", err)
		}
	}

	return nil
}

// SaveDialog сохраняет всю историю диалога в базу данных
func (d *DB) SaveDialog(treadId uint64, message json.RawMessage) error {
	if treadId == 0 {
		return fmt.Errorf("получен пустой тред")
	}

	ctx, cancel := context.WithTimeout(d.Context(), mode.SqlTimeToCancel)
	defer cancel()

	// Если resolver задан — обрабатываем шифрование сами (минуя SP)
	if d.MasterKeyResolver != nil {
		return d.saveDialogWithResolver(ctx, treadId, message)
	}

	// Fallback: хранимая процедура (plaintext, обратная совместимость)
	if _, err := d.Conn().ExecContext(ctx, "CALL SaveDialog(?, ?)", treadId, message); err != nil {
		switch {
		case errors.Is(err, context.DeadlineExceeded):
			return fmt.Errorf("тайм-аут при сохранении диалога: %w", err)
		case errors.Is(err, context.Canceled):
			return fmt.Errorf("операция отменена: %w", err)
		default:
			return fmt.Errorf("ошибка сохранения диалога: %w", err)
		}
	}
	return nil
}

// saveDialogWithResolver читает + расшифровывает + аппендит + шифрует + сохраняет.
// Использует транзакцию с FOR UPDATE для защиты от гонки.
func (d *DB) saveDialogWithResolver(ctx context.Context, treadId uint64, message json.RawMessage) error {
	tx, err := d.Conn().BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("saveDialog begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	// Читаем userId и текущий Data с блокировкой строки
	var userId uint32
	var rawData sql.NullString
	if err = tx.QueryRowContext(ctx,
		"SELECT `User`, `Data` FROM dialogs WHERE Id = ? FOR UPDATE", treadId).
		Scan(&userId, &rawData); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("диалог %d не найден", treadId)
		}
		return fmt.Errorf("saveDialog read: %w", err)
	}

	mk, hasMK := d.MasterKeyResolver(userId)

	// Разворачиваем текущий массив данных
	var arr []json.RawMessage
	if rawData.Valid && rawData.String != "" {
		data := rawData.String
		if hasMK && crypto.IsEncryptedWithMasterKey(data) {
			if data, err = crypto.DecryptFieldWithMasterKey(mk, data); err != nil {
				return fmt.Errorf("saveDialog decrypt: %w", err)
			}
		}
		// Нормализуем существующие данные перед аппендом
		rawArr := d.normalizeDataArray(json.RawMessage(data))
		_ = json.Unmarshal(rawArr, &arr)
	}

	// Аппендим новое сообщение
	arr = append(arr, message)

	newBytes, err := json.Marshal(arr)
	if err != nil {
		return fmt.Errorf("saveDialog marshal: %w", err)
	}
	newData := string(newBytes)

	// Шифруем если MasterKey доступен
	if hasMK {
		if newData, err = crypto.EncryptFieldWithMasterKey(mk, newData); err != nil {
			return fmt.Errorf("saveDialog encrypt: %w", err)
		}
	}

	if _, err = tx.ExecContext(ctx,
		"UPDATE dialogs SET `Data` = ?, `Date` = NOW() WHERE Id = ?",
		newData, treadId); err != nil {
		return fmt.Errorf("saveDialog update: %w", err)
	}

	return tx.Commit()
}

// UpdateDialogsMeta устанавливает достижение цели
func (d *DB) UpdateDialogsMeta(dialogId uint64, meta string) error {
	if dialogId == 0 {
		return fmt.Errorf("получен пустой dialogId")
	}

	ctx, cancel := context.WithTimeout(d.Context(), mode.SqlTimeToCancel)
	defer cancel()

	if _, err := d.Conn().ExecContext(ctx, "CALL UpdateDialogsMeta(?,?)", dialogId, meta); err != nil {
		switch {
		case errors.Is(err, context.DeadlineExceeded):
			return fmt.Errorf("тайм-аут (%d с) при сохранении достижения цели: %w", mode.SqlTimeToCancel, err)
		case errors.Is(err, context.Canceled):
			return fmt.Errorf("операция отменена: %w", err)
		default:
			return fmt.Errorf("ошибка сохранения достижения цели: %w", err)
		}
	}

	return nil
}

// GetOrSetTreadAndResponder получает или создает тред и респондера
func (d *DB) GetOrSetTreadAndResponder(
	userID uint32,
	responderRealId uint64,
	responderName string,
	chatType ChatType,
) (uint64, error) {
	if userID == 0 {
		return 0, fmt.Errorf("получен пустой userID")
	}

	ctx, cancel := context.WithTimeout(d.Context(), mode.SqlTimeToCancel)
	defer cancel()

	// Создаём временную переменную для выхода
	if _, err := d.Conn().ExecContext(ctx, "SET @out_dialogId = 0;"); err != nil {
		switch {
		case errors.Is(err, context.DeadlineExceeded):
			return 0, fmt.Errorf("тайм-аут (%d с) при создании временной переменной: %w", mode.SqlTimeToCancel, err)
		case errors.Is(err, context.Canceled):
			return 0, fmt.Errorf("операция отменена: %w", err)
		default:
			return 0, fmt.Errorf("ошибка при создании временной переменной: %w", err)
		}
	}

	// Выполняем вызов процедуры
	if _, err := d.Conn().ExecContext(ctx, "CALL GetOrSetTreadAndResponder(?, ?, ?, ?, @out_dialogId);",
		userID, responderRealId, responderName, chatType); err != nil { // Тип чата TgBot
		switch {
		case errors.Is(err, context.DeadlineExceeded):
			return 0, fmt.Errorf("тайм-аут (%d с) при вызове процедуры GetOrSetTreadAndResponder: %w", mode.SqlTimeToCancel, err)
		case errors.Is(err, context.Canceled):
			return 0, fmt.Errorf("операция отменена: %w", err)
		default:
			return 0, fmt.Errorf("ошибка вызова процедуры GetOrSetTreadAndResponder: %w", err)
		}
	}

	// Читаем значение из переменной
	var dialogId uint64
	if err := d.Conn().QueryRowContext(ctx, "SELECT @out_dialogId;").Scan(&dialogId); err != nil {
		switch {
		case errors.Is(err, context.DeadlineExceeded):
			return 0, fmt.Errorf("тайм-аут (%d с) при получении значения @out_dialogId: %w", mode.SqlTimeToCancel, err)
		case errors.Is(err, context.Canceled):
			return 0, fmt.Errorf("операция отменена: %w", err)
		default:
			return 0, fmt.Errorf("ошибка получения значения @out_dialogId: %w", err)
		}
	}

	return dialogId, nil
}

// GetUserSubscriptionLimites получает лимиты подписки пользователя
func (d *DB) GetUserSubscriptionLimites(userID uint32) (json.RawMessage, error) {
	if userID == 0 {
		return nil, fmt.Errorf("получен пустой userID")
	}

	ctx, cancel := context.WithTimeout(d.Context(), mode.SqlTimeToCancel)
	defer cancel()

	var data sql.NullString
	if err := d.Conn().QueryRowContext(ctx, "SELECT GetUserSubscriptionLimites(?)", userID).Scan(&data); err != nil {
		switch {
		case errors.Is(err, context.DeadlineExceeded):
			return nil, fmt.Errorf("тайм-аут (%d с) при вызове функции GetUserSubscriptionLimites: %w", mode.SqlTimeToCancel, err)
		case errors.Is(err, context.Canceled):
			return nil, fmt.Errorf("операция отменена: %w", err)
		case errors.Is(err, sql.ErrNoRows):
			return nil, fmt.Errorf("данные подписки не найдены")
		default:
			return nil, fmt.Errorf("ошибка вызова хранимой функции GetUserSubscriptionLimites: %w", err)
		}
	}

	if !data.Valid {
		return nil, fmt.Errorf("получены пустые данные")
	}

	return json.RawMessage(data.String), nil
}

// DisableAllUserChannel отключает все каналы пользователя
func (d *DB) DisableAllUserChannel(userID uint32) error {
	if userID == 0 {
		return fmt.Errorf("получен пустой userID")
	}

	ctx, cancel := context.WithTimeout(d.Context(), mode.SqlTimeToCancel)
	defer cancel()

	if _, err := d.Conn().ExecContext(ctx, "CALL DisableAllUserChannel(?)", userID); err != nil {
		switch {
		case errors.Is(err, context.DeadlineExceeded):
			return fmt.Errorf("тайм-аут (%d с) при отключении каналов: %w", mode.SqlTimeToCancel, err)
		case errors.Is(err, context.Canceled):
			return fmt.Errorf("операция отменена: %w", err)
		default:
			return fmt.Errorf("ошибка отключения каналов: %w", err)
		}
	}

	return nil
}

// SetChannelEnabled включает или отключает канал пользователя
func (d *DB) SetChannelEnabled(userID uint32, chName string, status bool) error {
	if userID == 0 || chName == "" {
		return fmt.Errorf("получены некорректные значения: userID или chName пусты")
	}

	ctx, cancel := context.WithTimeout(d.Context(), mode.SqlTimeToCancel)
	defer cancel()

	if _, err := d.Conn().ExecContext(ctx, "CALL SetChannelEnabled(?,?,?)", userID, chName, status); err != nil {
		switch {
		case errors.Is(err, context.DeadlineExceeded):
			return fmt.Errorf("тайм-аут (%d с) при сохранении статуса канала: %w", mode.SqlTimeToCancel, err)
		case errors.Is(err, context.Canceled):
			return fmt.Errorf("операция отменена: %w", err)
		default:
			return fmt.Errorf("ошибка сохранения статуса канала: %w", err)
		}
	}

	return nil
}

// GetNotificationChannel получает данные каналов уведомлений пользователя
func (d *DB) GetNotificationChannel(userID uint32) (json.RawMessage, error) {
	if userID == 0 {
		return nil, fmt.Errorf("получен пустой userID")
	}

	ctx, cancel := context.WithTimeout(d.Context(), mode.SqlTimeToCancel)
	defer cancel()

	var data sql.NullString
	if err := d.Conn().QueryRowContext(ctx, "SELECT GetNotificationChannel(?)", userID).Scan(&data); err != nil {
		switch {
		case errors.Is(err, context.DeadlineExceeded):
			return nil, fmt.Errorf("тайм-аут (%d с) при вызове функции GetNotificationChannel: %w", mode.SqlTimeToCancel, err)
		case errors.Is(err, context.Canceled):
			return nil, fmt.Errorf("операция отменена: %w", err)
		case errors.Is(err, sql.ErrNoRows):
			return nil, fmt.Errorf("каналы уведомлений не найдены")
		default:
			return nil, fmt.Errorf("ошибка вызова хранимой функции GetNotificationChannel: %w", err)
		}
	}

	if !data.Valid {
		return nil, fmt.Errorf("получены пустые данные")
	}

	return json.RawMessage(data.String), nil
}

// GetAllUserModels получает все модели пользователя из таблицы user_models
func (d *DB) GetAllUserModels(userID uint32) ([]create.UserModelRecord, error) {
	if userID == 0 {
		return nil, fmt.Errorf("получен пустой userID")
	}

	ctx, cancel := context.WithTimeout(d.Context(), sqlTimeToCancel*time.Second)
	defer cancel()

	rows, err := d.Conn().QueryContext(ctx,
		`SELECT 
			um.ModelId,
			um.Provider,
			um.IsActive,
			ug.AssistantId,
			gm.Id,
			gm.Name,
			ug.Ids
		FROM user_models um
		JOIN user_gpt ug ON um.ModelId = ug.Id
		JOIN gpt_models gm ON gm.Id = ug.Model
		WHERE um.userID = ?
		ORDER BY um.IsActive DESC, um.CreatedAt DESC`, userID)

	if err != nil {
		switch {
		case errors.Is(err, context.DeadlineExceeded):
			return nil, fmt.Errorf("тайм-аут (%d с) при получении моделей: %w", sqlTimeToCancel, err)
		case errors.Is(err, context.Canceled):
			return nil, fmt.Errorf("операция отменена: %w", err)
		default:
			return nil, fmt.Errorf("ошибка получения моделей: %w", err)
		}
	}
	defer rows.Close()

	var records []create.UserModelRecord
	for rows.Next() {
		var record create.UserModelRecord
		var isActive int8
		var idsRaw sql.NullString
		var modelId sql.NullInt64
		var modelName sql.NullString

		if err := rows.Scan(&record.ModelId, &record.Provider, &isActive, &record.AssistId, &modelId, &modelName, &idsRaw); err != nil {
			continue
		}

		record.IsActive = isActive == 1
		if modelName.Valid && modelId.Valid {
			record.GptType = &create.GptType{
				ID:   uint(modelId.Int64),
				Name: modelName.String,
			}
		}

		// Парсим JSON из поля Ids
		if idsRaw.Valid && idsRaw.String != "" {
			// Сохраняем raw JSON в AllIds для доступа к VectorId
			record.AllIds = []byte(idsRaw.String)

			// Парсим FileIds для обратной совместимости
			var data struct {
				FileIds  []create.Ids `json:"FileIds"`
				VectorId []string     `json:"VectorId"`
			}
			if err := json.Unmarshal([]byte(idsRaw.String), &data); err != nil {
			} else {
				record.FileIds = data.FileIds
			}
		}

		records = append(records, record)
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("ошибка при итерации по записям: %w", err)
	}

	return records, nil
}

// UpdateUserGPT обновляет поле Ids (AllIds) в таблице user_gpt
// Используется для обновления информации о файлах и векторных хранилищах/библиотеках
func (d *DB) UpdateUserGPT(userID uint32, modelId uint64, assistId string, allIds []byte) error {
	if userID == 0 {
		return fmt.Errorf("получен пустой userID")
	}
	if modelId == 0 {
		return fmt.Errorf("получен пустой modelId")
	}

	ctx, cancel := context.WithTimeout(d.Context(), sqlTimeToCancel*time.Second)
	defer cancel()

	// Подготавливаем значение для БД
	// Если allIds == nil, то сохраняем SQL NULL, иначе строку
	var idsValue any
	if allIds == nil || len(allIds) == 0 {
		idsValue = nil // SQL NULL
	} else {
		idsValue = string(allIds)
	}

	// Обновляем поле Ids в user_gpt
	_, err := d.Conn().ExecContext(ctx, `
		UPDATE user_gpt 
		SET Ids = ? 
		WHERE Id = ? AND AssistantId = ?
	`, idsValue, modelId, assistId)

	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) {
			return fmt.Errorf("тайм-аут (%d с) при обновлении user_gpt: %w", sqlTimeToCancel, err)
		}
		if errors.Is(err, context.Canceled) {
			return fmt.Errorf("операция отменена: %w", err)
		}
		return fmt.Errorf("ошибка обновления user_gpt: %w", err)
	}

	return nil
}

func (d *DB) GetUserVectorStorage(userID uint32) (string, error) {
	// Проверяем входное значение
	if userID == 0 {
		return "", fmt.Errorf("получен некорректный userID")
	}

	// Дочерний контекст с тайм-аутом на операцию
	ctx, cancel := context.WithTimeout(d.Context(), sqlTimeToCancel*time.Second)
	defer cancel()

	// SQL запрос для получения первого элемента VectorId из JSON активной модели
	// Используем новую структуру через user_models
	query := `
  SELECT JSON_UNQUOTE(JSON_EXTRACT(ug.Ids, '$.VectorId[0]'))
  FROM user_models um
  JOIN user_gpt ug ON um.ModelId = ug.Id
  WHERE um.userID = ? AND um.IsActive = 1
  LIMIT 1`

	var data sql.NullString
	err := d.Conn().QueryRowContext(ctx, query, userID).Scan(&data)
	if err != nil {
		switch {
		case errors.Is(err, context.DeadlineExceeded):
			return "", fmt.Errorf("тайм-аут (%d с) при получении VectorStorage: %w", sqlTimeToCancel, err)
		case errors.Is(err, context.Canceled):
			return "", fmt.Errorf("операция отменена: %w", err)
		case errors.Is(err, sql.ErrNoRows):
			return "", nil // Данные не найдены, но это не ошибка
		default:
			return "", fmt.Errorf("ошибка получения VectorStorage: %w", err)
		}
	}

	if !data.Valid {
		return "", nil // Возвращаем пустую строку если данные NULL
	}

	return data.String, nil
}

// GetActiveProvider получает тип провайдера активной модели пользователя без создания дочернего контекста дял максимальной производительности
func (d *DB) GetActiveProvider(userID uint32) (create.ProviderType, error) {
	if userID == 0 {
		return 0, fmt.Errorf("получен некорректный userID")
	}

	// Используем родительский контекст напрямую для максимальной производительности
	// Запрашиваем активные модели с лимитом 2, чтобы проверить уникальность за один запрос
	query := `SELECT Provider FROM user_models WHERE userID = ? AND IsActive = 1 LIMIT 2`
	rows, err := d.Conn().QueryContext(d.Context(), query, userID)
	if err != nil {
		switch {
		case errors.Is(err, context.DeadlineExceeded):
			return 0, fmt.Errorf("тайм-аут при получении активной модели: %w", err)
		case errors.Is(err, context.Canceled):
			return 0, fmt.Errorf("операция отменена: %w", err)
		default:
			return 0, fmt.Errorf("GetActiveProvider: query error: %w", err)
		}
	}
	defer rows.Close()

	var providers []uint8
	for rows.Next() {
		var p uint8
		if err := rows.Scan(&p); err != nil {
			return 0, fmt.Errorf("GetActiveProvider: scan error: %w", err)
		}
		providers = append(providers, p)
	}

	if err := rows.Err(); err != nil {
		return 0, fmt.Errorf("GetActiveProvider: rows iteration error: %w", err)
	}

	if len(providers) == 0 {
		return 0, fmt.Errorf("GetActiveProvider: %w", fmt.Errorf("активная модель не найдена"))
	}

	if len(providers) > 1 {
		return 0, fmt.Errorf("найдено несколько активных моделей (найдено %d)", len(providers))
	}

	return create.ProviderType(providers[0]), nil
}

// ReadUserModelByProvider получает сжатые данные модели пользователя по провайдеру
func (d *DB) ReadUserModelByProvider(userID uint32, provider create.ProviderType) ([]byte, *create.VecIds, error) {
	// Проверяем входные значения
	if userID == 0 {
		return nil, nil, fmt.Errorf("получен некорректный userID")
	}
	if !provider.IsValid() {
		return nil, nil, fmt.Errorf("получен некорректный provider: %d", provider)
	}

	// Дочерний контекст с тайм-аутом на операцию
	ctx, cancel := context.WithTimeout(d.Context(), sqlTimeToCancel*time.Second)
	defer cancel()

	// SQL запрос для получения Data и Ids из user_gpt по провайдеру
	query := `
		SELECT TO_BASE64(ug.Data), ug.Ids
		FROM user_models um
		JOIN user_gpt ug ON um.ModelId = ug.Id
		WHERE um.userID = ? AND um.Provider = ?`

	var base64Data sql.NullString
	var idsJson sql.NullString

	err := d.Conn().QueryRowContext(ctx, query, userID, uint8(provider)).Scan(&base64Data, &idsJson)
	if err != nil {
		switch {
		case errors.Is(err, context.DeadlineExceeded):
			return nil, nil, fmt.Errorf("тайм-аут (%d с) при вызове ReadUserModelByProvider: %w", sqlTimeToCancel, err)
		case errors.Is(err, context.Canceled):
			return nil, nil, fmt.Errorf("операция отменена: %w", err)
		case errors.Is(err, sql.ErrNoRows):
			return nil, nil, nil // Модель не найдена, но это не ошибка
		default:
			return nil, nil, fmt.Errorf("ошибка получения данных ReadUserModelByProvider: %w", err)
		}
	}

	// Проверяем на пустой результат или null
	if !base64Data.Valid || base64Data.String == "" {
		return nil, nil, nil // Модель не найдена
	}

	// Инициализируем структуру VecIds по умолчанию с пустыми массивами
	vecIds := &create.VecIds{
		VectorId: []string{},
		FileIds:  []create.Ids{},
	}

	// ВАЖНО: Для Google провайдера (provider=3) поле Ids содержит конфигурацию модели,
	// а не file_ids/vector_id, поэтому НЕ парсим его в VecIds
	// Эмбеддинги для Google хранятся в отдельной таблице vector_embeddings
	if provider != create.ProviderGoogle {
		// Для OpenAI и Mistral парсим Ids в VecIds (file_ids, vector_id)
		if idsJson.Valid && idsJson.String != "" && idsJson.String != "null" {
			if err := json.Unmarshal([]byte(idsJson.String), vecIds); err != nil {
				return nil, nil, fmt.Errorf("ошибка разбора Ids: %w", err)
			}
		}
	}
	// Для Google провайдера vecIds остаётся с пустыми массивами

	// Декодируем base64 данные
	decodedData, err := base64.StdEncoding.DecodeString(base64Data.String)
	if err != nil {
		return nil, nil, fmt.Errorf("ошибка декодирования base64: %w", err)
	}

	return decodedData, vecIds, nil
}

// DefaultProvidersModels возвращает модель по умолчанию для указанного провайдера
func (d *DB) DefaultProvidersModels(providerName string) (uint8, string, error) {
	// Проверяем входные данные
	if providerName == "" {
		return 0, "", fmt.Errorf("получено пустое имя провайдера")
	}

	// Дочерний контекст с тайм-аутом на операцию
	ctx, cancel := context.WithTimeout(d.Context(), sqlTimeToCancel*time.Second)
	defer cancel()

	query := `
		SELECT gm.Id, gm.Name
		FROM gpt_models gm
		INNER JOIN model_providers mp ON gm.Provider = mp.Id
		WHERE mp.Name = ? AND gm.IsDefault = 1
		LIMIT 1
	`

	var modelId uint8
	var modelName string
	err := d.Conn().QueryRowContext(ctx, query, providerName).Scan(&modelId, &modelName)
	if err != nil {
		switch {
		case errors.Is(err, context.DeadlineExceeded):
			return 0, "", fmt.Errorf("тайм-аут (%d с) при получении модели по умолчанию для провайдера %s: %w", sqlTimeToCancel, providerName, err)
		case errors.Is(err, context.Canceled):
			return 0, "", fmt.Errorf("операция отменена: %w", err)
		case errors.Is(err, sql.ErrNoRows):
			return 0, "", fmt.Errorf("модель по умолчанию для провайдера %s не найдена", providerName)
		default:
			return 0, "", fmt.Errorf("ошибка выполнения запроса: %w", err)
		}
	}

	return modelId, modelName, nil
}

// GetActiveModel получает активную модель пользователя
func (d *DB) GetActiveModel(userID uint32) (*create.UserModelRecord, error) {
	// Проверяем входное значение
	if userID == 0 {
		return nil, fmt.Errorf("получен некорректный userID")
	}

	// Дочерний контекст с тайм-аутом на операцию
	ctx, cancel := context.WithTimeout(d.Context(), sqlTimeToCancel*time.Second)
	defer cancel()

	// SQL запрос для получения активной модели
	query := `
		SELECT 
			um.Id,
			ug.AssistantId,
			um.Provider,
			um.IsActive,
			ug.Ids,
			gm.Id,
			gm.Name
		FROM user_models um
		JOIN user_gpt ug ON um.ModelId = ug.Id
		LEFT JOIN gpt_models gm ON gm.Id = ug.Model
		WHERE um.userID = ? AND um.IsActive = 1
		LIMIT 1`

	var modelId uint64
	var assistId string
	var provider uint8
	var isActive bool
	var idsJson sql.NullString
	var modelName sql.NullString
	var modelNameId sql.NullInt64

	err := d.Conn().QueryRowContext(ctx, query, userID).Scan(
		&modelId,
		&assistId,
		&provider,
		&isActive,
		&idsJson,
		&modelNameId,
		&modelName,
	)

	if err != nil {
		switch {
		case errors.Is(err, context.DeadlineExceeded):
			return nil, fmt.Errorf("тайм-аут (%d с) при вызове GetActiveModel: %w", sqlTimeToCancel, err)
		case errors.Is(err, context.Canceled):
			return nil, fmt.Errorf("операция отменена: %w", err)
		case errors.Is(err, sql.ErrNoRows):
			return nil, nil // Активная модель не найдена
		default:
			return nil, fmt.Errorf("ошибка получения активной модели: %w", err)
		}
	}

	// Создаем запись модели
	record := &create.UserModelRecord{
		ModelId:  modelId,
		AssistId: assistId,
		Provider: create.ProviderType(provider),
		IsActive: isActive,
		FileIds:  []create.Ids{},
	}
	if modelName.Valid && modelNameId.Valid {
		record.GptType = &create.GptType{
			ID:   uint(modelNameId.Int64),
			Name: modelName.String,
		}
	}

	// Парсим JSON с Ids
	if idsJson.Valid && idsJson.String != "" && idsJson.String != "null" {
		record.AllIds = []byte(idsJson.String)

		var vecIds create.VecIds
		if err := json.Unmarshal([]byte(idsJson.String), &vecIds); err != nil {
			return nil, fmt.Errorf("ошибка разбора Ids: %w", err)
		}
		record.FileIds = vecIds.FileIds
	}

	return record, nil
}

// GetModelByProvider получает АКТИВНУЮ модель пользователя по провайдеру
// Если модель не активна - возвращает nil
func (d *DB) GetModelByProvider(userID uint32, provider create.ProviderType) (*create.UserModelRecord, error) {
	// Проверяем входные значения
	if userID == 0 {
		return nil, fmt.Errorf("получен некорректный userID")
	}
	if !provider.IsValid() {
		return nil, fmt.Errorf("получен некорректный provider: %d", provider)
	}

	// Дочерний контекст с тайм-аутом на операцию
	ctx, cancel := context.WithTimeout(d.Context(), sqlTimeToCancel*time.Second)
	defer cancel()

	// SQL запрос для получения модели по провайдеру
	query := `
		SELECT 
			um.ModelId,
			ug.AssistantId,
			um.Provider,
			um.IsActive,
			ug.Ids,
			gm.Id,
			gm.Name
		FROM user_models um
		INNER JOIN user_gpt ug ON um.ModelId = ug.Id
		LEFT JOIN gpt_models gm ON gm.Id = ug.Model
		WHERE um.userID = ? 
			AND um.Provider = ?
			AND um.IsActive = 1
		LIMIT 1`

	var modelId uint64
	var assistId string
	var providerDb uint8
	var isActive bool
	var idsJson sql.NullString
	var modelName sql.NullString
	var modelNameId sql.NullInt64

	err := d.Conn().QueryRowContext(ctx, query, userID, uint8(provider)).Scan(
		&modelId,
		&assistId,
		&providerDb,
		&isActive,
		&idsJson,
		&modelNameId,
		&modelName,
	)

	if err != nil {
		switch {
		case errors.Is(err, context.DeadlineExceeded):
			return nil, fmt.Errorf("тайм-аут (%d с) при вызове GetModelByProvider: %w", sqlTimeToCancel, err)
		case errors.Is(err, context.Canceled):
			return nil, fmt.Errorf("операция отменена: %w", err)
		case errors.Is(err, sql.ErrNoRows):
			return nil, nil // Модель не найдена
		default:
			return nil, fmt.Errorf("ошибка получения модели по провайдеру: %w", err)
		}
	}

	// Создаем запись модели
	record := &create.UserModelRecord{
		ModelId:  modelId,
		AssistId: assistId,
		Provider: create.ProviderType(providerDb),
		IsActive: isActive,
		FileIds:  []create.Ids{},
	}
	if modelName.Valid && modelNameId.Valid {
		record.GptType = &create.GptType{
			ID:   uint(modelNameId.Int64),
			Name: modelName.String,
		}
	}

	// Парсим JSON с Ids
	if idsJson.Valid && idsJson.String != "" && idsJson.String != "null" {
		record.AllIds = []byte(idsJson.String)

		var vecIds create.VecIds
		if err := json.Unmarshal([]byte(idsJson.String), &vecIds); err != nil {
			return nil, fmt.Errorf("ошибка разбора Ids: %w", err)
		}
		record.FileIds = vecIds.FileIds
	}

	return record, nil
}

// GetModelByProviderAnyStatus получает модель пользователя по провайдеру НЕЗАВИСИМО от статуса активности
// В отличие от GetModelByProvider, эта функция не требует IsActive = 1
// Используется для обновления неактивных моделей
func (d *DB) GetModelByProviderAnyStatus(userID uint32, provider create.ProviderType) (*create.UserModelRecord, error) {
	// Проверяем входные значения
	if userID == 0 {
		return nil, fmt.Errorf("получен некорректный userID")
	}
	if !provider.IsValid() {
		return nil, fmt.Errorf("получен некорректный provider: %d", provider)
	}

	// Дочерний контекст с тайм-аутом на операцию
	ctx, cancel := context.WithTimeout(d.Context(), sqlTimeToCancel*time.Second)
	defer cancel()

	// SQL запрос - БЕЗ условия IsActive = 1
	query := `
		SELECT 
			um.ModelId,
			ug.AssistantId,
			um.Provider,
			um.IsActive,
			ug.Ids,
			gm.Id,
			gm.Name
		FROM user_models um
		INNER JOIN user_gpt ug ON um.ModelId = ug.Id
		LEFT JOIN gpt_models gm ON gm.Id = ug.Model
		WHERE um.userID = ? 
			AND um.Provider = ?
		LIMIT 1`

	var modelId uint64
	var assistId string
	var providerDb uint8
	var isActive bool
	var idsJson sql.NullString
	var modelName sql.NullString
	var modelNameId sql.NullInt64

	err := d.Conn().QueryRowContext(ctx, query, userID, uint8(provider)).Scan(
		&modelId,
		&assistId,
		&providerDb,
		&isActive,
		&idsJson,
		&modelNameId,
		&modelName,
	)

	if err != nil {
		switch {
		case errors.Is(err, context.DeadlineExceeded):
			return nil, fmt.Errorf("тайм-аут (%d с) при вызове GetModelByProviderAnyStatus: %w", sqlTimeToCancel, err)
		case errors.Is(err, context.Canceled):
			return nil, fmt.Errorf("операция отменена: %w", err)
		case errors.Is(err, sql.ErrNoRows):
			return nil, nil // Модель не найдена
		default:
			return nil, fmt.Errorf("ошибка получения модели по провайдеру: %w", err)
		}
	}

	// Создаем запись модели
	record := &create.UserModelRecord{
		ModelId:  modelId,
		AssistId: assistId,
		Provider: create.ProviderType(providerDb),
		IsActive: isActive,
		FileIds:  []create.Ids{},
	}
	if modelName.Valid && modelNameId.Valid {
		record.GptType = &create.GptType{
			ID:   uint(modelNameId.Int64),
			Name: modelName.String,
		}
	}

	// Парсим JSON с Ids
	if idsJson.Valid && idsJson.String != "" && idsJson.String != "null" {
		record.AllIds = []byte(idsJson.String)

		var vecIds create.VecIds
		if err := json.Unmarshal([]byte(idsJson.String), &vecIds); err != nil {
			return nil, fmt.Errorf("ошибка разбора Ids: %w", err)
		}
		record.FileIds = vecIds.FileIds
	}

	return record, nil
}

// SetActiveModel переключает активную модель пользователя
// Параметры:
//   - userID: ID пользователя
//   - modelId: ID записи из таблицы user_models
//
// Функция снимает IsActive с других моделей пользователя в этой же транзакции
func (d *DB) SetActiveModel(userID uint32, modelId uint64) error {
	if userID == 0 {
		return fmt.Errorf("получен пустой userID")
	}

	if modelId == 0 {
		return fmt.Errorf("получен пустой modelId")
	}

	ctx, cancel := context.WithTimeout(d.Context(), mode.SqlTimeToCancel)
	defer cancel()

	// Начинаем транзакцию
	tx, err := d.Conn().BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("ошибка начала транзакции: %w", err)
	}
	defer tx.Rollback()

	// Сначала снимаем IsActive со всех активных моделей этого пользователя
	_, err = tx.ExecContext(ctx,
		"UPDATE user_models SET IsActive = 0 WHERE userID = ? AND IsActive = 1",
		userID)

	if err != nil {
		switch {
		case errors.Is(err, context.DeadlineExceeded):
			return fmt.Errorf("тайм-аут (%d с) при деактивации старых моделей: %w", mode.SqlTimeToCancel, err)
		case errors.Is(err, context.Canceled):
			return fmt.Errorf("операция отменена: %w", err)
		default:
			return fmt.Errorf("ошибка деактивации старых моделей: %w", err)
		}
	}

	// Обновляем IsActive для указанной модели
	result, err := tx.ExecContext(ctx,
		"UPDATE user_models SET IsActive = 1 WHERE Id = ? AND userID = ?",
		modelId, userID)

	if err != nil {
		switch {
		case errors.Is(err, context.DeadlineExceeded):
			return fmt.Errorf("тайм-аут (%d с) при переключении активной модели: %w", mode.SqlTimeToCancel, err)
		case errors.Is(err, context.Canceled):
			return fmt.Errorf("операция отменена: %w", err)
		default:
			return fmt.Errorf("ошибка переключения активной модели: %w", err)
		}
	}

	// Проверяем, была ли обновлена хотя бы одна строка
	rowsAffected, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("ошибка получения количества обновленных строк: %w", err)
	}

	if rowsAffected == 0 {
		return fmt.Errorf("модель с Id=%d для пользователя %d не найдена", modelId, userID)
	}

	// Фиксируем транзакцию
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("ошибка фиксации транзакции: %w", err)
	}

	return nil
}

// SetActiveModelByProvider переключает активную модель пользователя для указанного провайдера
// Параметры:
//   - userID: ID пользователя
//   - provider: тип провайдера (ProviderOpenAI, ProviderMistral, ...)
//
// Функция снимает IsActive с других моделей пользователя в этой же транзакции
func (d *DB) SetActiveModelByProvider(userID uint32, provider create.ProviderType) error {
	if userID == 0 {
		return fmt.Errorf("получен пустой userID")
	}

	ctx, cancel := context.WithTimeout(d.Context(), mode.SqlTimeToCancel)
	defer cancel()

	// Начинаем транзакцию
	tx, err := d.Conn().BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("ошибка начала транзакции: %w", err)
	}
	defer tx.Rollback()

	// Сначала снимаем IsActive со ВСЕХ активных моделей пользователя (любого провайдера)
	_, err = tx.ExecContext(ctx,
		`UPDATE user_models 
		SET IsActive = 0 
		WHERE userID = ? AND IsActive = 1`,
		userID)

	if err != nil {
		switch {
		case errors.Is(err, context.DeadlineExceeded):
			return fmt.Errorf("тайм-аут (%d с) при деактивации старой модели: %w", mode.SqlTimeToCancel, err)
		case errors.Is(err, context.Canceled):
			return fmt.Errorf("операция отменена: %w", err)
		default:
			return fmt.Errorf("ошибка деактивации старой модели: %w", err)
		}
	}

	// Обновляем IsActive для пользовательской модели указанного провайдера
	result, err := tx.ExecContext(ctx,
		`UPDATE user_models 
		SET IsActive = 1 
		WHERE userID = ? AND Provider = ? 
		ORDER BY CreatedAt DESC 
		LIMIT 1`,
		userID, uint8(provider))

	if err != nil {
		switch {
		case errors.Is(err, context.DeadlineExceeded):
			return fmt.Errorf("тайм-аут (%d с) при переключении активной модели: %w", mode.SqlTimeToCancel, err)
		case errors.Is(err, context.Canceled):
			return fmt.Errorf("операция отменена: %w", err)
		default:
			return fmt.Errorf("ошибка переключения активной модели: %w", err)
		}
	}

	// Проверяем, была ли обновлена хотя бы одна строка
	rowsAffected, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("ошибка получения количества обновленных строк: %w", err)
	}

	if rowsAffected == 0 {
		return fmt.Errorf("пользовательская модель провайдера %s для пользователя %d не найдена", provider.String(), userID)
	}

	// Фиксируем транзакцию
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("ошибка фиксации транзакции: %w", err)
	}

	return nil
}

// SetContactAvailability сохраняет доступность контакта в конкретном провайдере
func (d *DB) SetContactAvailability(userID uint32, contact, provider string, isAvailable bool) error {
	// Сначала получаем ContactId из service_contacts
	var contactID int64
	query := `SELECT Id FROM service_contacts WHERE userID = ? AND Contact = ? LIMIT 1`
	err := d.Conn().QueryRow(query, userID, contact).Scan(&contactID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("контакт %s не найден в service_contacts для пользователя %d", contact, userID)
		}
		return fmt.Errorf("ошибка получения ContactId: %w", err)
	}

	// Сохраняем доступность через ContactId
	insertQuery := `
		INSERT INTO service_contact_availability 
			(ContactId, Provider, IsAvailable, CheckedAt, UpdatedAt)
		VALUES (?, ?, ?, NOW(), NOW())
		ON DUPLICATE KEY UPDATE 
			IsAvailable = VALUES(IsAvailable),
			UpdatedAt = NOW()
	`

	_, err = d.Conn().Exec(insertQuery, contactID, provider, isAvailable)
	if err != nil {
		return fmt.Errorf("ошибка сохранения доступности контакта: %w", err)
	}

	return nil
}

// GetContactAvailability получает доступность контакта во всех провайдерах
func (d *DB) GetContactAvailability(userID uint32, contact string) (map[string]bool, error) {
	query := `
		SELECT ca.Provider, ca.IsAvailable 
		FROM service_contact_availability ca
		INNER JOIN service_contacts c ON ca.ContactId = c.Id
		WHERE c.userID = ? AND c.Contact = ?
	`

	rows, err := d.Conn().Query(query, userID, contact)
	if err != nil {
		return nil, fmt.Errorf("ошибка получения доступности контакта: %w", err)
	}
	defer rows.Close()

	availability := make(map[string]bool)
	for rows.Next() {
		var provider string
		var isAvailable bool
		if err := rows.Scan(&provider, &isAvailable); err != nil {
			return nil, fmt.Errorf("ошибка чтения данных доступности: %w", err)
		}
		availability[provider] = isAvailable
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("ошибка итерации результатов: %w", err)
	}

	return availability, nil
}

// GetContactsAvailableIn получает список контактов доступных в указанном провайдере
func (d *DB) GetContactsAvailableIn(userID uint32, provider string) ([]string, error) {
	query := `
		SELECT DISTINCT c.Contact 
		FROM service_contact_availability ca
		INNER JOIN service_contacts c ON ca.ContactId = c.Id
		WHERE c.userID = ? 
		  AND ca.Provider = ? 
		  AND ca.IsAvailable = 1
		ORDER BY c.Contact
	`

	rows, err := d.Conn().Query(query, userID, provider)
	if err != nil {
		return nil, fmt.Errorf("ошибка получения контактов для провайдера %s: %w", provider, err)
	}
	defer rows.Close()

	var contacts []string
	for rows.Next() {
		var contact string
		if err := rows.Scan(&contact); err != nil {
			return nil, fmt.Errorf("ошибка чтения контакта: %w", err)
		}
		contacts = append(contacts, contact)
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("ошибка итерации результатов: %w", err)
	}

	return contacts, nil
}

// GetContactsInBothProviders получает контакты доступные в обеих указанных платформах
func (d *DB) GetContactsInBothProviders(userID uint32, provider1, provider2 string) ([]string, error) {
	query := `
		SELECT c.Contact
		FROM service_contacts c
		INNER JOIN service_contact_availability ca1 ON c.Id = ca1.ContactId
		INNER JOIN service_contact_availability ca2 ON c.Id = ca2.ContactId
		WHERE c.userID = ?
		  AND ca1.Provider = ?
		  AND ca1.IsAvailable = 1
		  AND ca2.Provider = ?
		  AND ca2.IsAvailable = 1
		ORDER BY c.Contact
	`

	rows, err := d.Conn().Query(query, userID, provider1, provider2)
	if err != nil {
		return nil, fmt.Errorf("ошибка получения общих контактов: %w", err)
	}
	defer rows.Close()

	var contacts []string
	for rows.Next() {
		var contact string
		if err := rows.Scan(&contact); err != nil {
			return nil, fmt.Errorf("ошибка чтения контакта: %w", err)
		}
		contacts = append(contacts, contact)
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("ошибка итерации результатов: %w", err)
	}

	return contacts, nil
}

// RemoveModelFromUser удаляет связь между пользователем и моделью в таблице user_models
// Также удаляет саму модель из user_gpt, если это была последняя связь с этой моделью
func (d *DB) RemoveModelFromUser(userID uint32, modelId uint64) error {
	// Проверяем входные значения
	if userID == 0 || modelId == 0 {
		return fmt.Errorf("получены некорректные значения: userID или modelId равны 0")
	}

	// Дочерний контекст с тайм-аутом на операцию
	ctx, cancel := context.WithTimeout(d.ctx, sqlTimeToCancel*time.Second)
	defer cancel()

	// Начинаем транзакцию для атомарности операций
	tx, err := d.conn.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("ошибка начала транзакции: %w", err)
	}
	defer tx.Rollback()

	// Проверяем, существует ли связь пользователя с моделью
	var exists bool
	err = tx.QueryRowContext(ctx,
		"SELECT EXISTS(SELECT 1 FROM user_models WHERE userID = ? AND ModelId = ?)",
		userID, modelId).Scan(&exists)
	if err != nil {
		switch {
		case errors.Is(err, context.DeadlineExceeded):
			return fmt.Errorf("тайм-аут (%d с) при проверке связи пользователя с моделью: %w", sqlTimeToCancel, err)
		case errors.Is(err, context.Canceled):
			return fmt.Errorf("операция отменена при проверке связи: %w", err)
		default:
			return fmt.Errorf("ошибка проверки связи пользователя с моделью: %w", err)
		}
	}

	if !exists {
		return fmt.Errorf("связь между пользователем %d и моделью %d не найдена", userID, modelId)
	}

	// Проверяем, была ли эта модель активной
	var wasActive bool
	err = tx.QueryRowContext(ctx,
		"SELECT IsActive FROM user_models WHERE userID = ? AND ModelId = ?",
		userID, modelId).Scan(&wasActive)
	if err != nil {
		switch {
		case errors.Is(err, context.DeadlineExceeded):
			return fmt.Errorf("тайм-аут (%d с) при проверке активности модели: %w", sqlTimeToCancel, err)
		case errors.Is(err, context.Canceled):
			return fmt.Errorf("операция отменена при проверке активности: %w", err)
		default:
			return fmt.Errorf("ошибка проверки активности модели: %w", err)
		}
	}

	// Удаляем связь между пользователем и моделью
	_, err = tx.ExecContext(ctx,
		"DELETE FROM user_models WHERE userID = ? AND ModelId = ?",
		userID, modelId)
	if err != nil {
		switch {
		case errors.Is(err, context.DeadlineExceeded):
			return fmt.Errorf("тайм-аут (%d с) при удалении связи: %w", sqlTimeToCancel, err)
		case errors.Is(err, context.Canceled):
			return fmt.Errorf("операция отменена при удалении связи: %w", err)
		default:
			return fmt.Errorf("ошибка удаления связи пользователя с моделью: %w", err)
		}
	}

	// Проверяем, есть ли у этой модели другие связи с пользователями
	var otherUsersCount int
	err = tx.QueryRowContext(ctx,
		"SELECT COUNT(*) FROM user_models WHERE ModelId = ?",
		modelId).Scan(&otherUsersCount)
	if err != nil {
		switch {
		case errors.Is(err, context.DeadlineExceeded):
			return fmt.Errorf("тайм-аут (%d с) при проверке других связей модели: %w", sqlTimeToCancel, err)
		case errors.Is(err, context.Canceled):
			return fmt.Errorf("операция отменена при проверке других связей: %w", err)
		default:
			return fmt.Errorf("ошибка проверки других связей модели: %w", err)
		}
	}

	// Если других связей нет, удаляем саму модель из user_gpt
	if otherUsersCount == 0 {
		_, err = tx.ExecContext(ctx, "DELETE FROM user_gpt WHERE Id = ?", modelId)
		if err != nil {
			switch {
			case errors.Is(err, context.DeadlineExceeded):
				return fmt.Errorf("тайм-аут (%d с) при удалении модели: %w", sqlTimeToCancel, err)
			case errors.Is(err, context.Canceled):
				return fmt.Errorf("операция отменена при удалении модели: %w", err)
			default:
				return fmt.Errorf("ошибка удаления модели: %w", err)
			}
		}
	}

	// Если удалённая модель была активной, нужно активировать другую модель (если есть)
	if wasActive {
		// Получаем первую доступную модель пользователя
		var nextModelId sql.NullInt64
		err = tx.QueryRowContext(ctx,
			"SELECT ModelId FROM user_models WHERE userID = ? LIMIT 1",
			userID).Scan(&nextModelId)

		// Если есть другая модель, делаем её активной
		if err == nil && nextModelId.Valid {
			_, err = tx.ExecContext(ctx,
				"UPDATE user_models SET IsActive = 1 WHERE userID = ? AND ModelId = ?",
				userID, nextModelId.Int64)
			if err != nil {
				return fmt.Errorf("ошибка активации следующей модели: %w", err)
			}
		} else if errors.Is(err, sql.ErrNoRows) {
			// Если других моделей нет - отключаем все каналы пользователя
			// Фиксируем транзакцию перед вызовом DisableAllUserChannel
			if err := tx.Commit(); err != nil {
				return fmt.Errorf("ошибка фиксации транзакции: %w", err)
			}

			// Отключаем все каналы, так как у пользователя больше нет моделей
			if err := d.DisableAllUserChannel(userID); err != nil {
				return fmt.Errorf("ошибка отключения каналов пользователя: %w", err)
			}

			return nil
		}
	}

	// Фиксируем транзакцию
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("ошибка фиксации транзакции: %w", err)
	}

	return nil
}

func (d *DB) SaveUserModel(
	userID uint32, provider create.ProviderType, name, assistantId string, data []byte, modType uint, ids json.RawMessage, operator bool) error {
	// Проверяю входные значения
	if userID == 0 || name == "" || assistantId == "" {
		return fmt.Errorf("получены некорректные значения: userID, name или assistantId пусты")
	}
	// Валидация провайдера
	if !provider.IsValid() {
		return fmt.Errorf("некорректный provider: %d (допустимы 1=OpenAI, 2=Mistral, 3=Google)", provider)
	}

	// Дочерний контекст с тайм-аутом на операцию
	ctx, cancel := context.WithTimeout(d.ctx, sqlTimeToCancel*time.Second)
	defer cancel()

	// Начинаем транзакцию для атомарности операций
	tx, err := d.conn.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("ошибка начала транзакции: %w", err)
	}
	defer tx.Rollback()

	// ===================================================================
	// Шаг 1: Сохранение/обновление модели в user_gpt
	// ===================================================================

	// Проверяем, существует ли модель для данного пользователя и провайдера
	var existingModelId sql.NullInt64
	err = tx.QueryRowContext(ctx, `
		SELECT ug.Id 
		FROM user_gpt ug
		INNER JOIN user_models um ON ug.Id = um.ModelId
		WHERE um.userID = ? AND um.Provider = ?
		LIMIT 1
	`, userID, provider).Scan(&existingModelId)

	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		switch {
		case errors.Is(err, context.DeadlineExceeded):
			return fmt.Errorf("тайм-аут (%d с) при проверке существующей модели: %w", sqlTimeToCancel, err)
		case errors.Is(err, context.Canceled):
			return fmt.Errorf("операция отменена: %w", err)
		default:
			return fmt.Errorf("ошибка проверки существующей модели: %w", err)
		}
	}

	var modelId int64

	if !existingModelId.Valid {
		// ===================================================================
		// Модели нет - создаём новую в user_gpt
		// ===================================================================
		result, err := tx.ExecContext(ctx, `
			INSERT INTO user_gpt (Name, Model, Provider, AssistantId, Data, Ids)
			VALUES (?, ?, ?, ?, ?, ?)
		`, name, modType, provider, assistantId, data, ids)

		if err != nil {
			switch {
			case errors.Is(err, context.DeadlineExceeded):
				return fmt.Errorf("тайм-аут (%d с) при создании модели: %w", sqlTimeToCancel, err)
			case errors.Is(err, context.Canceled):
				return fmt.Errorf("операция отменена при создании модели: %w", err)
			default:
				return fmt.Errorf("ошибка создания модели: %w", err)
			}
		}

		// Получаем ID новой записи
		modelId, err = result.LastInsertId()
		if err != nil {
			return fmt.Errorf("ошибка получения ID новой модели: %w", err)
		}

		// ===================================================================
		// Шаг 2: Создание связи в user_models
		// ===================================================================

		// Проверяем, есть ли у пользователя другие модели
		var modelCount int
		err = tx.QueryRowContext(ctx, `
			SELECT COUNT(*) 
			FROM user_models 
			WHERE userID = ?
		`, userID).Scan(&modelCount)

		if err != nil {
			switch {
			case errors.Is(err, context.DeadlineExceeded):
				return fmt.Errorf("тайм-аут (%d с) при подсчёте моделей: %w", sqlTimeToCancel, err)
			case errors.Is(err, context.Canceled):
				return fmt.Errorf("операция отменена при подсчёте моделей: %w", err)
			default:
				return fmt.Errorf("ошибка подсчёта моделей: %w", err)
			}
		}

		// Если это первая модель - делаем её активной автоматически
		isActive := 0
		if modelCount == 0 {
			isActive = 1
		}

		// Создаём связь в user_models
		_, err = tx.ExecContext(ctx, `
			INSERT INTO user_models (userID, ModelId, Provider, IsActive)
			VALUES (?, ?, ?, ?)
		`, userID, modelId, provider, isActive)

		if err != nil {
			switch {
			case errors.Is(err, context.DeadlineExceeded):
				return fmt.Errorf("тайм-аут (%d с) при создании связи модели: %w", sqlTimeToCancel, err)
			case errors.Is(err, context.Canceled):
				return fmt.Errorf("операция отменена при создании связи: %w", err)
			default:
				return fmt.Errorf("ошибка создания связи модели: %w", err)
			}
		}

	} else {
		// ===================================================================
		// Модель существует - обновляем её в user_gpt
		// ===================================================================
		modelId = existingModelId.Int64

		_, err = tx.ExecContext(ctx, `
			UPDATE user_gpt
			SET Name = ?,
				Model = ?,
				AssistantId = ?,
				Data = ?,
				Ids = ?
			WHERE Id = ?
		`, name, modType, assistantId, data, ids, modelId)

		if err != nil {
			switch {
			case errors.Is(err, context.DeadlineExceeded):
				return fmt.Errorf("тайм-аут (%d с) при обновлении модели: %w", sqlTimeToCancel, err)
			case errors.Is(err, context.Canceled):
				return fmt.Errorf("операция отменена при обновлении модели: %w", err)
			default:
				return fmt.Errorf("ошибка обновления модели: %w", err)
			}
		}
	}

	// ===================================================================
	// Шаг 3: Обновление статуса оператора
	// ===================================================================
	enabledInt := 0
	if operator {
		enabledInt = 1
	}

	_, err = tx.ExecContext(ctx, `
		UPDATE operators
		SET Telegram_enabled = ?,
			Changed = 1,
			Timechange = CURRENT_TIMESTAMP()
		WHERE userID = ?
	`, enabledInt, userID)

	if err != nil {
		switch {
		case errors.Is(err, context.DeadlineExceeded):
			return fmt.Errorf("тайм-аут (%d с) при установке статуса оператора: %w", sqlTimeToCancel, err)
		case errors.Is(err, context.Canceled):
			return fmt.Errorf("операция отменена при установке статуса оператора: %w", err)
		default:
			return fmt.Errorf("ошибка установки статуса оператора: %w", err)
		}
	}

	// ===================================================================
	// Финал: Фиксируем транзакцию
	// ===================================================================
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("ошибка фиксации транзакции: %w", err)
	}

	return nil
}

// ReadUserModel получает данные модели пользователя и идентификаторы файлов
func (d *DB) ReadUserModel(userID uint32) ([]byte, *create.VecIds, error) {
	// Проверяем входное значение
	if userID == 0 {
		return nil, nil, fmt.Errorf("получен некорректный userID")
	}

	// Дочерний контекст с тайм-аутом на операцию
	ctx, cancel := context.WithTimeout(d.ctx, sqlTimeToCancel*time.Second)
	defer cancel()

	// SQL запрос для получения Data и Ids из user_gpt через активную модель
	query := `
		SELECT TO_BASE64(ug.Data), ug.Ids
		FROM user_models um
		JOIN user_gpt ug ON um.ModelId = ug.Id
		WHERE um.userID = ? AND um.IsActive = 1`

	var base64Data sql.NullString
	var idsJson sql.NullString

	err := d.conn.QueryRowContext(ctx, query, userID).Scan(&base64Data, &idsJson)
	if err != nil {
		switch {
		case errors.Is(err, context.DeadlineExceeded):
			return nil, nil, fmt.Errorf("тайм-аут (%d с) при вызове ReadUserModel: %w", sqlTimeToCancel, err)
		case errors.Is(err, context.Canceled):
			return nil, nil, fmt.Errorf("операция отменена: %w", err)
		case errors.Is(err, sql.ErrNoRows):
			return nil, nil, nil // Модель не найдена, но это не ошибка
		default:
			return nil, nil, fmt.Errorf("ошибка получения данных ReadUserModel: %w", err)
		}
	}

	// Проверяем на пустой результат или null
	if !base64Data.Valid || base64Data.String == "" {
		return nil, nil, nil // Модель не найдена
	}

	// Инициализируем структуру VecIds по умолчанию с пустыми массивами
	vecIds := &create.VecIds{
		VectorId: []string{},
		FileIds:  []create.Ids{},
	}

	// Проверяем и парсим Ids, если они есть
	if idsJson.Valid && idsJson.String != "" && idsJson.String != "null" {
		if err := json.Unmarshal([]byte(idsJson.String), vecIds); err != nil {
			return nil, nil, fmt.Errorf("ошибка разбора Ids: %w", err)
		}
	}

	// Декодируем base64 данные
	decodedData, err := base64.StdEncoding.DecodeString(base64Data.String)
	if err != nil {
		return nil, nil, fmt.Errorf("ошибка декодирования base64: %w", err)
	}

	return decodedData, vecIds, nil
}

func (d *DB) GetOrSetUserStorageLimit(userID uint32, setStorage int64) (remaining uint64, totalLimit uint64, err error) {
	// Проверяем входное значение
	if userID == 0 {
		return 0, 0, fmt.Errorf("получен некорректный userID")
	}

	// Дочерний контекст с тайм-аутом на операцию
	ctx, cancel := context.WithTimeout(d.ctx, sqlTimeToCancel*time.Second)
	defer cancel()

	// Начинаем транзакцию
	tx, err := d.conn.BeginTx(ctx, nil)
	if err != nil {
		return 0, 0, fmt.Errorf("ошибка начала транзакции: %w", err)
	}
	defer tx.Rollback()

	// Получаем текущие значения с блокировкой строки
	var vLimit, vUsed int64
	err = tx.QueryRowContext(ctx, `
  SELECT StorageLimit, StorageUsed
  FROM subscriptions
  WHERE userID = ?
  FOR UPDATE`, userID).Scan(&vLimit, &vUsed)

	if err != nil {
		switch {
		case errors.Is(err, context.DeadlineExceeded):
			return 0, 0, fmt.Errorf("тайм-аут (%d с) при получении лимитов хранилища: %w", sqlTimeToCancel, err)
		case errors.Is(err, context.Canceled):
			return 0, 0, fmt.Errorf("операция отменена: %w", err)
		case errors.Is(err, sql.ErrNoRows):
			return 0, 0, fmt.Errorf("подписка пользователя не найдена")
		default:
			return 0, 0, fmt.Errorf("ошибка получения лимитов хранилища: %w", err)
		}
	}

	// Вычисляем новое значение занятости
	vNewUsed := vUsed + setStorage

	// Гарантируем границы: [0, StorageLimit]
	if vNewUsed < 0 {
		vNewUsed = 0
	} else if vNewUsed > vLimit {
		vNewUsed = vLimit
	}

	// Обновляем значение StorageUsed
	_, err = tx.ExecContext(ctx, `
  UPDATE subscriptions
  SET StorageUsed = ?
  WHERE userID = ?`, vNewUsed, userID)

	if err != nil {
		switch {
		case errors.Is(err, context.DeadlineExceeded):
			return 0, 0, fmt.Errorf("тайм-аут (%d с) при обновлении использования хранилища: %w", sqlTimeToCancel, err)
		case errors.Is(err, context.Canceled):
			return 0, 0, fmt.Errorf("операция отменена: %w", err)
		default:
			return 0, 0, fmt.Errorf("ошибка обновления использования хранилища: %w", err)
		}
	}

	// Фиксируем транзакцию
	if err := tx.Commit(); err != nil {
		return 0, 0, fmt.Errorf("ошибка фиксации транзакции: %w", err)
	}

	// Вычисляем оставшееся место и возвращаем результат
	remaining = uint64(vLimit - vNewUsed)
	totalLimit = uint64(vLimit)

	return remaining, totalLimit, nil
}

// GetUserApiKey возвращает API-ключ пользователя для провайдера.
// Канонический формат Provider в user_api_keys — строковый ("google").
// Numeric provider ("3") читается только для обратной совместимости.
// Автоматически расшифровывает "$app$"; "$mk$" расшифровывает при наличии
// MasterKeyResolver, иначе оставляет для внешней model.WithMasterKeyProvider обёртки.
func (d *DB) GetUserAPIKey(userID uint32, provider ProviderType) (string, error) {
	ctx, cancel := context.WithTimeout(d.ctx, sqlTimeToCancel*time.Second)
	defer cancel()

	var apiKey string
	err := d.conn.QueryRowContext(ctx,
		`SELECT ApiKey
		   FROM user_api_keys
		  WHERE UserId = ? AND Provider IN (?, ?)
		  ORDER BY CASE WHEN Provider = ? THEN 0 ELSE 1 END
		  LIMIT 1`,
		userID,
		provider.String(),
		strconv.Itoa(int(provider)),
		provider.String(),
	).Scan(&apiKey)

	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return "", nil // ключ не найден — не ошибка
		}
		return "", fmt.Errorf("ошибка получения API-ключа: %w", err)
	}

	apiKey = strings.TrimSpace(apiKey)

	if crypto.IsEncryptedWithMasterKey(apiKey) {
		if d.MasterKeyResolver == nil {
			return apiKey, nil
		}
		masterKey, ok := d.MasterKeyResolver(userID)
		if !ok {
			return "", fmt.Errorf("API-ключ зашифрован MasterKey, но MasterKey пользователя %d не загружен", userID)
		}
		decrypted, err := crypto.DecryptFieldWithMasterKey(masterKey, apiKey)
		if err != nil {
			return "", fmt.Errorf("ошибка расшифровки API-ключа через MasterKey: %w", err)
		}
		return strings.TrimSpace(decrypted), nil
	}

	// Если ключ не зашифрован — возвращаем как есть (backward compatibility)
	if !crypto.IsEncryptedWithAppKey(apiKey) {
		return apiKey, nil
	}

	// Расшифровываем через global encryptor
	encryptor, err := crypto.GetGlobalEncryptor()
	if err != nil {
		return "", fmt.Errorf("application encryption key не доступен: %w", err)
	}

	decrypted, err := encryptor.DecryptField(apiKey)
	if err != nil {
		return "", fmt.Errorf("ошибка расшифровки API-ключа: %w", err)
	}

	return strings.TrimSpace(decrypted), nil
}

// SetUserAPIKey сохраняет API-ключ пользователя с автоматическим шифрованием,
// если MasterKey пользователя загружен или application encryption key установлен.
func (d *DB) SetUserAPIKey(userID uint32, provider ProviderType, apiKey string) error {
	ctx, cancel := context.WithTimeout(d.ctx, sqlTimeToCancel*time.Second)
	defer cancel()

	keyToStore := strings.TrimSpace(apiKey)
	if keyToStore == "" {
		return fmt.Errorf("API-ключ не может быть пустым")
	}

	if d.MasterKeyResolver != nil {
		if masterKey, ok := d.MasterKeyResolver(userID); ok {
			encrypted, encErr := crypto.EncryptFieldWithMasterKey(masterKey, keyToStore)
			if encErr != nil {
				return fmt.Errorf("ошибка шифрования API-ключа через MasterKey: %w", encErr)
			}
			keyToStore = encrypted
		}
	}

	// Пытаемся зашифровать если application encryption key доступен
	if !crypto.IsEncryptedWithMasterKey(keyToStore) {
		encryptor, err := crypto.GetGlobalEncryptor()
		if err == nil && encryptor.IsKeySet() {
			encrypted, encErr := encryptor.EncryptField(keyToStore)
			if encErr != nil {
				return fmt.Errorf("ошибка шифрования API-ключа application key: %w", encErr)
			}
			keyToStore = encrypted
		}
	}

	// Сохраняем (INSERT или UPDATE)
	query := `INSERT INTO user_api_keys (UserId, Provider, ApiKey)
	          VALUES (?, ?, ?)
	          ON DUPLICATE KEY UPDATE ApiKey = VALUES(ApiKey)`

	_, execErr := d.conn.ExecContext(ctx, query, userID, provider.String(), keyToStore)
	if execErr != nil {
		return fmt.Errorf("ошибка сохранения API-ключа: %w", execErr)
	}

	if _, err := d.conn.ExecContext(ctx,
		`DELETE FROM user_api_keys WHERE UserId = ? AND Provider = ?`,
		userID,
		strconv.Itoa(int(provider)),
	); err != nil {
		return fmt.Errorf("ошибка удаления legacy API-ключа: %w", err)
	}

	return nil
}

// DeleteUserAPIKey удаляет персональный API-ключ пользователя для провайдера.
func (d *DB) DeleteUserAPIKey(userID uint32, provider ProviderType) error {
	if userID == 0 {
		return fmt.Errorf("некорректный userID")
	}

	ctx, cancel := context.WithTimeout(d.ctx, sqlTimeToCancel*time.Second)
	defer cancel()

	_, err := d.conn.ExecContext(ctx,
		`DELETE FROM user_api_keys WHERE UserId = ? AND Provider IN (?, ?)`,
		userID,
		provider.String(),
		strconv.Itoa(int(provider)),
	)
	if err != nil {
		return fmt.Errorf("ошибка удаления API ключа пользователя %d (%s): %w", userID, provider, err)
	}
	return nil
}

func (d *DB) UserTimeZone(userID uint32) (string, error) {
	if userID == 0 {
		return "", fmt.Errorf("получены некорректные данные: userID")
	}

	ctx, cancel := context.WithTimeout(d.ctx, sqlTimeToCancel*time.Second)
	defer cancel()

	var tz sql.NullString
	err := d.conn.QueryRowContext(ctx, "SELECT TimeZone FROM users WHERE Id = ?", userID).Scan(&tz)
	if err != nil {
		switch {
		case errors.Is(err, context.DeadlineExceeded):
			return "", fmt.Errorf("тайм-аут (%d с) при получении часового пояса пользователя: %w", sqlTimeToCancel, err)
		case errors.Is(err, context.Canceled):
			return "", fmt.Errorf("операция отменена: %w", err)
		case errors.Is(err, sql.ErrNoRows):
			return "", fmt.Errorf("пользователь с ID %d не найден", userID)
		default:
			return "", fmt.Errorf("ошибка получения часового пояса пользователя: %w", err)
		}
	}

	if !tz.Valid {
		return "", fmt.Errorf("часовой пояс не установлен для пользователя %d", userID)
	}

	return tz.String, nil
}

func (d *DB) UserLanguage(userID uint32) string {
	if userID == 0 {
		return "en"
	}

	ctx, cancel := context.WithTimeout(d.ctx, sqlTimeToCancel*time.Second)
	defer cancel()

	var ln sql.NullString
	err := d.conn.QueryRowContext(ctx, "SELECT UserLang(?)", userID).Scan(&ln)
	if err != nil {
		switch {
		case errors.Is(err, context.DeadlineExceeded):
			return "en"
		case errors.Is(err, context.Canceled):
			return "en"
		case errors.Is(err, sql.ErrNoRows):
			return "en"
		default:
			return "en"
		}
	}

	if !ln.Valid {
		return "en"
	}

	return ln.String
}

func (d *DB) SetUserSubscriptionNotified(user uint32) error {
	ctx, cancel := context.WithTimeout(d.Context(), sqlTimeToCancel*time.Second)
	defer cancel()

	query := "UPDATE subscriptions SET Notified = TRUE WHERE UserId = ?"

	_, err := d.Conn().ExecContext(ctx, query, user)
	if err != nil {
		switch {
		case errors.Is(err, context.DeadlineExceeded):
			return fmt.Errorf("тайм-аут (%d с) при обновлении статуса уведомления: %w", sqlTimeToCancel, err)
		case errors.Is(err, context.Canceled):
			return fmt.Errorf("операция отменена: %w", err)
		default:
			return fmt.Errorf("ошибка при обновлении статуса уведомления: %w", err)
		}
	}

	return nil
}

// SyncProviderModels синхронизирует каталог моделей провайдера с уже полученным списком моделей.
// При удалении неподдерживаемой модели из провайдера она удаляется из gpt_models и
// очищает ссылку в user_models (GptModelId = NULL), чтобы пользователь мог выбрать другую.
func (d *DB) SyncProviderModels(provider create.ProviderType, modelNames []string) (create.ProviderModelsSyncResult, error) {
	result := create.ProviderModelsSyncResult{Provider: provider}
	if !provider.IsValid() {
		return result, fmt.Errorf("некорректный provider: %d", provider)
	}

	ctx, cancel := context.WithTimeout(d.Context(), sqlTimeToCancel*time.Second)
	defer cancel()

	normalizedNames := make([]string, 0, len(modelNames))
	for _, name := range modelNames {
		trimmed := strings.TrimSpace(name)
		if trimmed != "" {
			normalizedNames = append(normalizedNames, trimmed)
		}
	}

	tx, err := d.conn.BeginTx(ctx, nil)
	if err != nil {
		return result, fmt.Errorf("ошибка начала транзакции синхронизации: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	seenNames := make(map[string]struct{}, len(normalizedNames))
	for _, name := range normalizedNames {
		if _, ok := seenNames[name]; ok {
			continue
		}
		seenNames[name] = struct{}{}

		var existingID int64
		err := tx.QueryRowContext(ctx, `
			SELECT Id
			FROM gpt_models
			WHERE Provider = ? AND Name = ?
			LIMIT 1
		`, provider, name).Scan(&existingID)
		switch {
		case err == nil:
			if _, err := tx.ExecContext(ctx, `
				UPDATE gpt_models
				SET IsDefault = IF(gpt_models.IsDefault = 1, 1, 0)
				WHERE Id = ?
			`, existingID); err != nil {
				return result, fmt.Errorf("ошибка обновления модели %s: %w", name, err)
			}
		case errors.Is(err, sql.ErrNoRows):
			if _, err := tx.ExecContext(ctx, `
				INSERT INTO gpt_models (Provider, IsDefault, Name)
				VALUES (?, ?, ?)
			`, provider, 0, name); err != nil {
				return result, fmt.Errorf("ошибка сохранения модели %s: %w", name, err)
			}
		default:
			return result, fmt.Errorf("ошибка поиска модели %s: %w", name, err)
		}
		result.Synced++
	}

	rows, err := tx.QueryContext(ctx, `SELECT Id, Name FROM gpt_models WHERE Provider = ?`, provider)
	if err != nil {
		return result, fmt.Errorf("ошибка получения текущего списка моделей: %w", err)
	}
	defer func() { _ = rows.Close() }()

	seen := make(map[string]struct{}, len(normalizedNames))
	for _, name := range normalizedNames {
		seen[name] = struct{}{}
	}

	for rows.Next() {
		var modelID int64
		var modelName string
		if err := rows.Scan(&modelID, &modelName); err != nil {
			continue
		}
		trimmedModelName := strings.TrimSpace(modelName)
		if _, ok := seen[trimmedModelName]; ok {
			continue
		}

		var affectedUsers []uint32
		userRows, err := tx.QueryContext(ctx, `
			SELECT um.userID
			FROM user_models um
			JOIN user_gpt ug ON ug.Id = um.ModelId
			WHERE um.Provider = ? AND ug.Model = ?
		`, provider, modelID)
		if err != nil {
			return result, fmt.Errorf("ошибка получения пользователей, использующих удалённую модель %s: %w", trimmedModelName, err)
		}
		for userRows.Next() {
			var userID uint32
			if err := userRows.Scan(&userID); err != nil {
				continue
			}
			affectedUsers = append(affectedUsers, userID)
		}
		_ = userRows.Close()

		// Очищаем привязку к удалённой модели:
		// - um.IsActive = 0  → деактивируем пользователей этой модели
		// - ug.Model = NULL  → убираем ссылку на gpt_models.Id
		// - ug.AssistantId = '' → очищаем имя модели, чтобы loadAgentConfig
		//   подхватил DefaultProvidersModels и переключил на дефолтную модель
		if _, err := tx.ExecContext(ctx, `
			UPDATE user_models um
			JOIN user_gpt ug ON ug.Id = um.ModelId
			SET um.IsActive = 0, ug.Model = NULL, ug.AssistantId = ''
			WHERE um.Provider = ? AND ug.Model = ?
		`, provider, modelID); err != nil {
			return result, fmt.Errorf("ошибка очистки привязки к модели %s в user_gpt: %w", trimmedModelName, err)
		}

		if _, err := tx.ExecContext(ctx, `DELETE FROM gpt_models WHERE Id = ?`, modelID); err != nil {
			return result, fmt.Errorf("ошибка удаления модели %s из gpt_models: %w", trimmedModelName, err)
		}

		result.Removed++
		result.RemovedNames = append(result.RemovedNames, trimmedModelName)
		result.ClearedUsers += len(affectedUsers)
		for _, userID := range affectedUsers {
			result.AffectedUsers = append(result.AffectedUsers, create.ProviderModelUserChange{
				UserID:    userID,
				ModelID:   uint64(modelID),
				ModelName: trimmedModelName,
			})
		}
	}

	if err := tx.Commit(); err != nil {
		return result, fmt.Errorf("ошибка фиксации синхронизации моделей: %w", err)
	}

	return result, nil
}

func (d *DB) ModelsNameByProvider(provider create.ProviderType) ([]string, error) {
	if !provider.IsValid() {
		return nil, fmt.Errorf("некорректный provider: %d", provider)
	}

	ctx, cancel := context.WithTimeout(d.Context(), sqlTimeToCancel*time.Second)
	defer cancel()

	rows, err := d.conn.QueryContext(ctx, `SELECT Id, Name FROM gpt_models WHERE Provider = ?`, provider)
	if err != nil {
		return nil, fmt.Errorf("ошибка получения текущего списка моделей: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var modelNames []string
	for rows.Next() {
		var modelID int64
		var modelName string
		if err := rows.Scan(&modelID, &modelName); err != nil {
			continue
		}
		modelNames = append(modelNames, strings.TrimSpace(modelName))
	}

	return modelNames, nil
}
