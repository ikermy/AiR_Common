package create

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/ikermy/AiR_Common/pkg/logger"
	"github.com/sashabaranov/go-openai"
)

const ModelShemaJSON = `{
        "type": "object",
        "properties": {
            "message": {
                "type": "string"
            },
            "action": {
                "type": "object",
                "properties": {
                    "send_files": {
                        "type": "array",
                        "items": {
                            "type": "object",
                            "properties": {
                                "type": {
                                    "type": "string",
                                    "enum": ["photo", "video", "audio", "doc"]
                                },
                                "Url": {
                                    "type": "string"
                                },
                                "file_name": {
                                    "type": "string"
                                },
                                "caption": {
                                    "type": "string"
                                }
                            },
                            "required": ["type", "Url", "file_name", "caption"],
                            "additionalProperties": false
                        }
                    }
                },
                "required": ["send_files"],
                "additionalProperties": false
            },
            "target": { "type": "boolean" },
			"operator": { "type": "boolean" }
        },
        "required": ["message", "action", "target", "operator"],
        "additionalProperties": false
    }`

// buildEnhancedPromptAndSchema генерирует улучшенный промпт и JSON Schema на основе параметров модели
func buildEnhancedPromptAndSchema(basePrompt string, realUserID uint64, metaAction string, operator, s3, interpreter, search bool, hasFiles bool) (string, []byte, error) {
	enhancedPrompt := basePrompt + "\n\n"

	// Добавляем важное напоминание
	if metaAction != "" || operator {
		enhancedPrompt += "## ⚠️ ВАЖНОЕ НАПОМИНАНИЕ:\n" +
			"В КАЖДОМ ответе ты ОБЯЗАН:\n"

		if metaAction != "" {
			enhancedPrompt += "1. Проверить условие достижения ЦЕЛИ (из твоих инструкций выше) и правильно установить target\n"
		}

		if operator {
			enhancedPrompt += "2. Проверить нужен ли оператор (из твоих инструкций выше) и правильно установить operator\n"
		}

		enhancedPrompt += "3. НЕ ИГНОРИРУЙ эти проверки!\n\n"
	}

	// Добавляем инструкции по работе с S3 файлами
	if s3 {
		enhancedPrompt += "## РАБОТА С ФАЙЛАМИ S3:\n\n" +
			"### Два типа файлов:\n" +
			"1. **Существующие файлы** (найденные через get_s3_files) - используй их реальные URL\n" +
			"2. **Созданные файлы** (через create_file) - используй URL из ответа функции\n\n" +
			"### Алгоритм работы с файлами:\n" +
			"1. Для получения списка файлов вызови: get_s3_files() - без параметров\n" +
			"2. Для создания нового файла вызови: create_file({\"content\": \"...\", \"file_name\": \"...txt\"})\n" +
			"3. Для существующих файлов используй URL из ответа get_s3_files\n" +
			"4. Для созданных файлов используй URL из ответа create_file\n\n" +
			"### Определение типа файла:\n" +
			"- .jpg, .jpeg, .png, .gif, .webp, .bmp → \"photo\"\n" +
			"- .mp4, .avi, .mov, .webm, .mkv → \"video\"\n" +
			"- .mp3, .wav, .flac, .aac, .ogg → \"audio\"\n" +
			"- Остальные → \"doc\"\n\n"
	}

	// Добавляем инструкции по Code Interpreter
	if interpreter {
		enhancedPrompt += "## CODE INTERPRETER:\n" +
			"Ты можешь выполнять Python код для:\n" +
			"- Анализа данных и вычислений\n" +
			"- Создания графиков и визуализаций\n" +
			"- Обработки файлов (CSV, Excel, JSON и т.д.)\n" +
			"- Генерации файлов с результатами\n\n" +
			"Созданные через Code Interpreter файлы автоматически доступны в ответе.\n\n"
	}

	// Добавляем инструкции по поиску в документах
	if search || hasFiles {
		enhancedPrompt += "## ПОИСК В ДОКУМЕНТАХ (File Search):\n" +
			"У тебя есть доступ к базе знаний из загруженных документов.\n" +
			"Используй file_search для поиска информации в документах пользователя.\n" +
			"Всегда ссылайся на источники при использовании информации из документов.\n\n"
	}

	// Добавляем общие правила для send_files
	if s3 || interpreter {
		enhancedPrompt += "## ПРАВИЛА отправки файлов (send_files):\n" +
			"1. Если НЕ отправляешь файлы - send_files должен быть пустым массивом []\n" +
			"2. Если упоминаешь файлы в message - ОБЯЗАТЕЛЬНО добавь их в send_files\n" +
			"3. Каждый файл в send_files должен содержать:\n" +
			"   - type: тип файла (photo/video/audio/doc)\n" +
			"   - Url: полный URL файла\n" +
			"   - file_name: имя файла\n" +
			"   - caption: описание файла\n\n"
	}

	// Финальная инструкция по формату ответа
	enhancedPrompt += "## ФОРМАТ ОТВЕТА:\n" +
		"Твой ответ ВСЕГДА должен быть в формате JSON Schema:\n" +
		ModelShemaJSON + "\n\n" +
		"### КРИТИЧЕСКИ ВАЖНО - ПРАВИЛА для полей JSON:\n\n" +
		"**message**: Твоё текстовое сообщение пользователю\n\n" +
		"**action.send_files**: Массив файлов для отправки ([] если файлов нет)\n\n"

	// Инструкции по target
	if metaAction != "" {
		enhancedPrompt += "**target** (boolean) - Достигнута ли ЦЕЛЬ диалога:\n" +
			"  Проверяй условие достижения цели из СВОИХ ИНСТРУКЦИЙ ВЫШЕ\n" +
			"  Если условие ТОЧНО выполнено → target: true\n" +
			"  Если условие НЕ выполнено → target: false\n" +
			"  НЕ ставь false если цель достигнута!\n\n"
	} else {
		enhancedPrompt += "**target**: ВСЕГДА false (цели нет)\n\n"
	}

	// Инструкции по operator
	if operator {
		enhancedPrompt += "**operator** (boolean) - Требуется ли оператор:\n" +
			"  Проверяй условие вызова оператора из СВОИХ ИНСТРУКЦИЙ ВЫШЕ\n" +
			"  Если пользователь просит оператора → operator: true\n" +
			"  Во всех остальных случаях → operator: false\n\n"
	}

	// Добавляем примеры
	if metaAction != "" {
		if operator {
			enhancedPrompt += "### Пример ответа когда цель ДОСТИГНУТА:\n" +
				"```json\n" +
				"{\n" +
				"  \"message\": \"Привет, Жорик! Рад познакомиться! 😊\",\n" +
				"  \"action\": {\"send_files\": []},\n" +
				"  \"target\": true,  // ← ЦЕЛЬ ДОСТИГНУТА!\n" +
				"  \"operator\": false\n" +
				"}\n" +
				"```\n\n" +
				"### Пример ответа когда цель НЕ достигнута:\n" +
				"```json\n" +
				"{\n" +
				"  \"message\": \"Привет! Как дела? 😊\",\n" +
				"  \"action\": {\"send_files\": []},\n" +
				"  \"target\": false,  // ← цель НЕ достигнута\n" +
				"  \"operator\": false\n" +
				"}\n" +
				"```\n\n"
		} else {
			enhancedPrompt += "### Пример ответа когда цель ДОСТИГНУТА:\n" +
				"```json\n" +
				"{\n" +
				"  \"message\": \"Привет, Жорик! Рад познакомиться! 😊\",\n" +
				"  \"action\": {\"send_files\": []},\n" +
				"  \"target\": true  // ← ЦЕЛЬ ДОСТИГНУТА!\n" +
				"}\n" +
				"```\n\n" +
				"### Пример ответа когда цель НЕ достигнута:\n" +
				"```json\n" +
				"{\n" +
				"  \"message\": \"Привет! Как дела? 😊\",\n" +
				"  \"action\": {\"send_files\": []},\n" +
				"  \"target\": false  // ← цель НЕ достигнута\n" +
				"}\n" +
				"```\n\n"
		}
	}

	enhancedPrompt += "ВАЖНО: Возвращай только валидный JSON без дополнительного текста."

	// Генерируем JSON Schema
	hasMetaAction := metaAction != ""
	dynamicSchema := generateModelSchema(hasMetaAction, operator)
	schemaJSON, err := json.Marshal(dynamicSchema)
	if err != nil {
		return "", nil, fmt.Errorf("ошибка сериализации JSON Schema: %w", err)
	}

	return enhancedPrompt, schemaJSON, nil
}

// generateModelSchema генерирует JSON Schema с учётом параметров модели
func generateModelSchema(hasMetaAction bool, hasOperator bool) map[string]interface{} {
	// Формируем список required полей
	requiredFields := []string{"message", "action", "target"}

	// operator добавляем в required только если он включен
	if hasOperator {
		requiredFields = append(requiredFields, "operator")
	}

	schema := map[string]interface{}{
		"type": "object",
		"properties": map[string]interface{}{
			"message": map[string]interface{}{
				"type": "string",
			},
			"action": map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"send_files": map[string]interface{}{
						"type": "array",
						"items": map[string]interface{}{
							"type": "object",
							"properties": map[string]interface{}{
								"type": map[string]interface{}{
									"type": "string",
									"enum": []string{"photo", "video", "audio", "doc"},
								},
								"Url": map[string]interface{}{
									"type": "string",
								},
								"file_name": map[string]interface{}{
									"type": "string",
								},
								"caption": map[string]interface{}{
									"type": "string",
								},
							},
							"required":             []string{"type", "Url", "file_name", "caption"},
							"additionalProperties": false,
						},
					},
				},
				"required":             []string{"send_files"},
				"additionalProperties": false,
			},
		},
		"required":             requiredFields,
		"additionalProperties": false,
	}

	// Настраиваем поле target
	if hasMetaAction {
		// Если есть MetaAction - target может быть true или false
		schema["properties"].(map[string]interface{})["target"] = map[string]interface{}{
			"type": "boolean",
		}
	} else {
		// Если нет MetaAction - target ВСЕГДА false
		schema["properties"].(map[string]interface{})["target"] = map[string]interface{}{
			"type": "boolean",
			"enum": []interface{}{false},
		}
	}

	// Настраиваем поле operator ТОЛЬКО если оно включено
	if hasOperator {
		// Если Operator включен - operator может быть true или false
		schema["properties"].(map[string]interface{})["operator"] = map[string]interface{}{
			"type": "boolean",
		}
	}
	// Если operator выключен - НЕ добавляем его в schema вообще!
	// Значение operator: false будет добавлено на стороне кода при парсинге ответа

	return schema
}

// вызывается во внешнем приложении при добавлении файла пользователем
// UploadFileToOpenAI загружает файл в OpenAI и возвращает его ID
func (m *UniversalModel) UploadFileToOpenAI(fileName string, fileData []byte) (string, error) {
	// Создаем запрос на загрузку файла из байтов
	fileRequest := openai.FileBytesRequest{
		Name:    fileName,
		Bytes:   fileData,
		Purpose: openai.PurposeAssistants,
	}

	// Загружаем файл через API OpenAI
	fileResponse, err := m.openaiClient.CreateFileBytes(m.ctx, fileRequest)
	if err != nil {
		return "", fmt.Errorf("ошибка загрузки файла через API OpenAI: %w", err)
	}

	return fileResponse.ID, nil
}

// вызывается во внешнем приложении при добавлении файла пользователем
// AddFileFromOpenAI добавляет новый файл в существующее векторное хранилище пользователя
func (m *UniversalModel) AddFileFromOpenAI(userId uint32, fileID, fileName string) error {
	// Проверка наличия OpenAI клиента
	if m.openaiClient == nil {
		return fmt.Errorf("OpenAI клиент не инициализирован")
	}

	// Получаем данные пользовательского Vector Store
	vectorStoreID, err := m.db.GetUserVectorStorage(userId)
	if err != nil {
		return fmt.Errorf("ошибка получения векторного хранилища: %w", err)
	}

	// Добавляем файл в существующий Vector Store
	_, err = m.openaiClient.CreateVectorStoreFile(m.ctx, vectorStoreID, openai.VectorStoreFileRequest{
		FileID: fileID,
	})
	if err != nil {
		return fmt.Errorf("ошибка добавления файла в Vector Store: %w", err)
	}

	//logger.Debug("Файл %s успешно добавлен в Vector Store", fileName, userId)
	return nil
}

// deleteFileFromOpenAI удаляет файл из OpenAI и связанного с ним Vector Store
func (m *UniversalModel) deleteFileFromOpenAI(fileID string) error {
	// 1. Удаляем файл по его ID
	if err := m.openaiClient.DeleteFile(m.ctx, fileID); err != nil {
		// Если файл уже удален (not found), это не является критической ошибкой
		if !strings.Contains(err.Error(), "not found") {
			return fmt.Errorf("ошибка удаления файла из OpenAI: %w", err)
		}
		logger.Error("Файл %s уже был удален или не найден в OpenAI: %v", fileID, err)
	}

	// 2. Ищем и удаляем связанный Vector Store
	// Получаем список всех векторных хранилищ
	vsList, err := m.openaiClient.ListVectorStores(m.ctx, openai.Pagination{})
	if err != nil {
		return fmt.Errorf("ошибка получения списка Vector Stores: %w", err)
	}

	// Ищем Vector Store, который содержит наш файл
	for _, vs := range vsList.VectorStores {
		// Получаем список файлов для каждого Vector Store
		files, err := m.openaiClient.ListVectorStoreFiles(m.ctx, vs.ID, openai.Pagination{})
		if err != nil {
			logger.Error("Предупреждение: не удалось получить файлы для Vector Store %s: %v", vs.ID, err)
			continue
		}

		// Если в хранилище только один файл и его ID совпадает с нашим, удаляем хранилище
		if len(files.VectorStoreFiles) == 1 && files.VectorStoreFiles[0].ID == fileID {
			_, err := m.openaiClient.DeleteVectorStore(m.ctx, vs.ID)
			if err != nil {
				// Логируем ошибку, но не прерываем процесс, так как основной файл уже мог быть удален
				logger.Error("Предупреждение: не удалось удалить Vector Store %s: %v", vs.ID, err)
			}
			// Прерываем цикл, так как нашли и обработали нужное хранилище
			break
		}
	}

	return nil
}

// createModel Создаю новую модель OpenAI Assistant
func (m *UniversalModel) createModel(userId uint32, modelData *UniversalModelData, fileIDs []Ids) (UMCR, error) {
	// modelData уже распарсена и типизирована, используем напрямую

	// Получаем real_user_id для использования в инструкциях
	realUserID, err := m.GetRealUserID(userId)
	if err != nil {
		logger.Warn("Не удалось получить real_user_id: %v", err, userId)
		realUserID = uint64(userId) // Fallback на обычный userId
	}

	// Автоматически генерируем системные инструкции на основе параметров
	enhancedPrompt := modelData.Prompt + "\n\n"

	// Напоминание о необходимости получить актуальное время с сервера для ВСЕХ моделей
	enhancedPrompt += fmt.Sprintf("ТЕКУЩЕЕ ВРЕМЯ:\n"+
		"ВАЖНО: Для получения актуальной даты и времени используй функцию get_current_time(user_id=\"%d\")\n"+
		"НЕ используй свои внутренние знания о дате - они УСТАРЕЛИ!\n\n", realUserID)

	// Добавляем важное напоминание в начало - только для активных функций
	if modelData.MetaAction != "" || modelData.Operator {
		enhancedPrompt += "## ВАЖНОЕ НАПОМИНАНИЕ:\n" +
			"В КАЖДОМ ответе ты ОБЯЗАН:\n"

		if modelData.MetaAction != "" {
			enhancedPrompt += "1. Проверить условие достижения ЦЕЛИ (из твоих инструкций выше) и правильно установить target\n"
		}

		if modelData.Operator {
			enhancedPrompt += "2. Проверить нужен ли оператор (из твоих инструкций выше) и правильно установить operator\n"
		}

		enhancedPrompt += "3. НЕ ИГНОРИРУЙ эти проверки!\n\n"
	}

	// Добавляем инструкции по работе с S3 файлами
	if modelData.S3 {
		enhancedPrompt += "## РАБОТА С ФАЙЛАМИ S3:\n\n" +
			fmt.Sprintf("**КРИТИЧЕСКИ ВАЖНО**: Твой user_id = \"%d\" (это строка, не число!)\n\n", realUserID) +
			"### Два типа файлов:\n" +
			"1. **Существующие файлы** (найденные через get_s3_files) - используй их реальные URL\n" +
			"2. **Созданные файлы** (через create_file) - используй URL из ответа функции\n\n" +
			"### Алгоритм работы с файлами:\n" +
			fmt.Sprintf("1. Для получения списка файлов вызови: get_s3_files({\"user_id\": \"%d\"})\n", realUserID) +
			fmt.Sprintf("2. Для создания нового файла вызови: create_file({\"user_id\": \"%d\", \"content\": \"...\", \"file_name\": \"...txt\"})\n", realUserID) +
			"3. Для существующих файлов используй URL из ответа get_s3_files\n" +
			"4. Для созданных файлов используй URL из ответа create_file\n\n" +
			"### Определение типа файла:\n" +
			"- .jpg, .jpeg, .png, .gif, .webp, .bmp → \"photo\"\n" +
			"- .mp4, .avi, .mov, .webm, .mkv → \"video\"\n" +
			"- .mp3, .wav, .flac, .aac, .ogg → \"audio\"\n" +
			"- Остальные → \"doc\"\n\n"
	}

	// Добавляем инструкции по Code Interpreter
	if modelData.Interpreter {
		enhancedPrompt += "## CODE INTERPRETER:\n" +
			"Ты можешь выполнять Python код для:\n" +
			"- Анализа данных и вычислений\n" +
			"- Создания графиков и визуализаций\n" +
			"- Обработки файлов (CSV, Excel, JSON и т.д.)\n" +
			"- Генерации файлов с результатами\n\n" +
			"Созданные через Code Interpreter файлы автоматически доступны в ответе.\n\n"
	}

	// Добавляем инструкции по GOOGLE CALENDAR
	if modelData.GOAuth.HasCalendar() {
		enhancedPrompt += "## GOOGLE CALENDAR - Управление событиями:\n" +
			"У тебя есть доступ к Google Calendar пользователя.\n\n" +
			fmt.Sprintf("**user_id для всех функций: \"%d\"** (строка)\n\n", realUserID) +
			"### Доступные функции:\n" +
			"- calendar_create_event - создание события\n" +
			"- calendar_list_events - список событий\n" +
			"- calendar_delete_event - удаление события\n" +
			"- calendar_get_event - детали события\n\n" +
			"### ВАЖНО при работе со временем:\n" +
			"- Формат времени: RFC3339 (например: \"2026-02-05T15:00:00+03:00\")\n" +
			"- ВСЕГДА вызывай get_current_time ПЕРЕД расчётом дат!\n" +
			"- Длительность по умолчанию: 1 час\n" +
			"- После создания/удаления подтверди действие и покажи ссылку\n\n"
	}

	// Добавляем инструкции по GOOGLE SHEETS
	if modelData.GOAuth.HasSheets() {
		enhancedPrompt += "## GOOGLE SHEETS - Работа с таблицами:\n" +
			"У тебя есть доступ к Google Sheets пользователя.\n\n" +
			fmt.Sprintf("**user_id для всех функций: \"%d\"** (строка)\n\n", realUserID) +
			"\n" +
			"КРИТИЧЕСКИ ВАЖНО - ОБЯЗАТЕЛЬНО ВЫЗЫВАЙ ФУНКЦИИ!\n\n" +
			"КАТЕГОРИЧЕСКИ ЗАПРЕЩЕНО:\n" +
			"- \"К сожалению, я не могу определить количество строк\"\n" +
			"- \"Отсутствие доступа к аккаунту пользователя\"\n" +
			"- \"Я не могу получить данные из таблицы\"\n" +
			"- \"Пожалуйста, убедитесь, что у меня есть доступ\"\n" +
			"- Отвечать БЕЗ вызова функции sheets_read_range\n\n" +
			"ПРАВИЛЬНЫЕ ДЕЙСТВИЯ:\n" +
			"1. Пользователь спрашивает \"что в таблице\" → НЕМЕДЛЕННО вызови sheets_read_range\n" +
			"2. Пользователь спрашивает \"сколько строк\" → ОБЯЗАТЕЛЬНО вызови sheets_read_range и посчитай len(values)-1\n" +
			"3. НЕ ОТВЕЧАЙ из промпта - ВЫЗОВИ ФУНКЦИЮ и получи РЕАЛЬНЫЕ данные!\n" +
			"4. НЕ ГОВОРИ о проблемах доступа - У ТЕБЯ УЖЕ ЕСТЬ ДОСТУП через функции!\n\n" +
			"Где взять spreadsheet_id:\n" +
			"- Ищи в промпте выше (может быть указан как: ID:, таблица CRM: ID, spreadsheet_id)\n" +
			"- Ищи в запросе пользователя (может быть длинная строка типа 18kxy_zkX...)\n" +
			"- ID может быть в ЛЮБОМ формате - найди его!\n\n" +
			"Где взять название листа:\n" +
			"- Ищи в промпте (может быть: Лист:, sheet:, на листе)\n" +
			"- Ищи в запросе пользователя\n" +
			"- Если не найден - используй весь диапазон: A:Z\n\n" +
			"### Доступные функции:\n" +
			"- sheets_read_range(user_id, spreadsheet_id, range) - чтение данных\n" +
			"- sheets_write_range(user_id, spreadsheet_id, range, values) - запись данных\n" +
			"- sheets_append_range(user_id, spreadsheet_id, range, values) - добавление строк\n\n" +
			"### КРИТИЧЕСКИ ВАЖНО ПРИ РАБОТЕ С ДАТАМИ:\n" +
			"ЕСЛИ в запросе упоминаются ДАТЫ ('текущая дата', 'сегодня', 'завтра', 'дата'):\n" +
			fmt.Sprintf("1. ОБЯЗАТЕЛЬНО СНАЧАЛА вызови get_current_time({\"user_id\": \"%d\"})\n", realUserID) +
			"2. Используй ТОЛЬКО дату из ответа get_current_time\n" +
			"3. НЕ используй свои внутренние знания о дате - они УСТАРЕЛИ!\n\n" +
			"### ВАЖНО:\n" +
			"- Диапазон в формате: 'Лиды!A:F' (весь лист) или 'Лист!A1:F100'\n" +
			"- Для подсчёта строк: вызови sheets_read_range → посчитай len(values)-1\n" +
			"- ВСЕГДА вызывай функцию ПЕРЕД ответом!\n\n"
	}

	// Добавляем инструкции по поиску в документах
	if modelData.Search || len(fileIDs) > 0 {
		enhancedPrompt += "## ПОИСК В ДОКУМЕНТАХ (File Search):\n" +
			"У тебя есть доступ к базе знаний из загруженных документов.\n" +
			"Используй file_search для поиска информации в документах пользователя.\n" +
			"Всегда ссылайся на источники при использовании информации из документов.\n\n"
	}

	// Добавляем общие правила для send_files
	if modelData.S3 || modelData.Interpreter {
		enhancedPrompt += "## ПРАВИЛА отправки файлов (send_files):\n" +
			"1. Если НЕ отправляешь файлы - send_files должен быть пустым массивом []\n" +
			"2. Если упоминаешь файлы в message - ОБЯЗАТЕЛЬНО добавь их в send_files\n" +
			"3. Каждый файл в send_files должен содержать:\n" +
			"   - type: тип файла (photo/video/audio/doc)\n" +
			"   - Url: полный URL файла\n" +
			"   - file_name: имя файла\n" +
			"   - caption: описание файла\n\n"
	}

	// Финальная инструкция по формату ответа
	enhancedPrompt += "## ФОРМАТ ОТВЕТА:\n" +
		"Твой ответ ВСЕГДА должен быть в формате JSON Schema:\n" +
		ModelShemaJSON + "\n\n" +
		"### ⚠️ КРИТИЧЕСКИ ВАЖНО - ПРАВИЛА для полей JSON:\n\n" +
		"**message**: Твоё текстовое сообщение пользователю\n\n" +
		"**action.send_files**: Массив файлов для отправки ([] если файлов нет)\n\n"

	// Добавляем инструкции про target только если есть MetaAction
	if modelData.MetaAction != "" {
		enhancedPrompt += "**target** (boolean) - Достигнута ли ЦЕЛЬ диалога:\n" +
			"  ✅ Проверяй условие достижения цели из СВОИХ ИНСТРУКЦИЙ ВЫШЕ\n" +
			"  ✅ Если условие ТОЧНО выполнено → target: true\n" +
			"  ✅ Если условие НЕ выполнено → target: false\n" +
			"  ❌ НЕ ставь false если цель достигнута!\n\n"
	} else {
		enhancedPrompt += "**target**: ВСЕГДА false (цели нет)\n\n"
	}

	// Добавляем инструкции про operator только если Operator включен
	if modelData.Operator {
		enhancedPrompt += "**operator** (boolean) - Требуется ли оператор:\n" +
			"  ✅ Проверяй условие вызова оператора из СВОИХ ИНСТРУКЦИЙ ВЫШЕ\n" +
			"  ✅ Если пользователь просит оператора → operator: true\n" +
			"  ✅ Во всех остальных случаях → operator: false\n\n"
	}
	// Если operator выключен - не упоминаем его вообще, поле не будет в JSON ответе

	// Добавляем примеры только если есть цель
	if modelData.MetaAction != "" {
		// Формируем примеры в зависимости от того, включен ли operator
		if modelData.Operator {
			// Если operator включен - показываем его в примерах
			enhancedPrompt += "### Пример ответа когда цель ДОСТИГНУТА:\n" +
				"```json\n" +
				"{\n" +
				"  \"message\": \"Привет, Жорик! Рад познакомиться! 😊\",\n" +
				"  \"action\": {\"send_files\": []},\n" +
				"  \"target\": true,  // ← ЦЕЛЬ ДОСТИГНУТА!\n" +
				"  \"operator\": false\n" +
				"}\n" +
				"```\n\n" +
				"### Пример ответа когда цель НЕ достигнута:\n" +
				"```json\n" +
				"{\n" +
				"  \"message\": \"Привет! Как дела? 😊\",\n" +
				"  \"action\": {\"send_files\": []},\n" +
				"  \"target\": false,  // ← цель НЕ достигнута\n" +
				"  \"operator\": false\n" +
				"}\n" +
				"```\n\n"
		} else {
			// Если operator выключен - НЕ показываем его в примерах
			enhancedPrompt += "### Пример ответа когда цель ДОСТИГНУТА:\n" +
				"```json\n" +
				"{\n" +
				"  \"message\": \"Привет, Жорик! Рад познакомиться! 😊\",\n" +
				"  \"action\": {\"send_files\": []},\n" +
				"  \"target\": true  // ← ЦЕЛЬ ДОСТИГНУТА!\n" +
				"}\n" +
				"```\n\n" +
				"### Пример ответа когда цель НЕ достигнута:\n" +
				"```json\n" +
				"{\n" +
				"  \"message\": \"Привет! Как дела? 😊\",\n" +
				"  \"action\": {\"send_files\": []},\n" +
				"  \"target\": false  // ← цель НЕ достигнута\n" +
				"}\n" +
				"```\n\n"
		}
	}

	enhancedPrompt += "ВАЖНО: Возвращай только валидный JSON без дополнительного текста."

	// Извлекаю id[]string из fileIDs
	var ids []string
	for _, fileID := range fileIDs {
		if fileID.ID != "" {
			ids = append(ids, fileID.ID)
		}
	}

	var vectorStoreIDs []string
	// Если есть файлы, создаем для них Vector Store
	if len(ids) > 0 {
		vsName := fmt.Sprintf("vs_user_%d_%d", userId, time.Now().Unix())
		vsRequest := openai.VectorStoreRequest{
			Name:    vsName,
			FileIDs: ids,
		}
		vectorStore, err := m.openaiClient.CreateVectorStore(m.ctx, vsRequest)
		if err != nil {
			return UMCR{}, fmt.Errorf("ошибка создания Vector Store: %w", err)
		}
		vectorStoreIDs = append(vectorStoreIDs, vectorStore.ID)
	}

	description := fmt.Sprintf("Модель для пользователя %d", userId)

	// Генерируем JSON Schema с учётом параметров модели
	hasMetaAction := modelData.MetaAction != ""
	hasOperator := modelData.Operator
	dynamicSchema := generateModelSchema(hasMetaAction, hasOperator)
	schemaJSON, err := json.Marshal(dynamicSchema)
	if err != nil {
		return UMCR{}, fmt.Errorf("ошибка сериализации JSON Schema: %w", err)
	}

	// Форматируем JSON для читабельности
	//var prettyJSON bytes.Buffer
	//if err := json.Indent(&prettyJSON, schemaJSON, "", "  "); err == nil {
	//	logger.Debug("Сгенерированная JSON Schema:\n%s", prettyJSON.String(), userId)
	//}
	// Создаем базовый AssistantRequest с улучшенными инструкциями
	assistantRequest := openai.AssistantRequest{
		Name:         &modelData.Name,
		Description:  &description,
		Instructions: &enhancedPrompt, // Используем улучшенный промпт
		Model:        modelData.GptType.Name,
		Metadata: map[string]any{
			"realUserId":      fmt.Sprintf("%d", realUserID),                 // Сохраняем realUserID для ActionHandler
			"operatorEnabled": fmt.Sprintf("%t", modelData.Operator),         // Сохраняем флаг Operator
			"hasMetaAction":   fmt.Sprintf("%t", modelData.MetaAction != ""), // Сохраняем флаг MetaAction
		},
		ResponseFormat: &openai.ChatCompletionResponseFormat{
			Type: openai.ChatCompletionResponseFormatTypeJSONSchema,
			JSONSchema: &openai.ChatCompletionResponseFormatJSONSchema{
				Name:   "response_with_action_files",
				Strict: true,
				Schema: json.RawMessage(schemaJSON), // Используем динамическую схему
			},
		},
	}

	// Условно добавляем инструменты на основе флагов
	var tools []openai.AssistantTool

	// Принудительно добавляем file_search если есть файлы или включен Search
	if len(vectorStoreIDs) > 0 || modelData.Search {
		tools = append(tools, openai.AssistantTool{Type: "file_search"})
	}

	if modelData.Interpreter {
		tools = append(tools, openai.AssistantTool{Type: "code_interpreter"})
	}

	// Добавляем функцию get_current_time ВСЕГДА (для получения актуального времени)
	userIDStr := fmt.Sprintf("%d", realUserID)
	tools = append(tools,
		openai.AssistantTool{
			Type: "function",
			Function: &openai.FunctionDefinition{
				Name: "get_current_time",
				Description: "Получает ТОЧНОЕ текущее время и дату с сервера в часовом поясе пользователя. " +
					"ОБЯЗАТЕЛЬНО используй эту функцию ПЕРЕД расчётом дат (завтра, через неделю, в понедельник и т.д.). " +
					"НЕ используй свои внутренние знания о дате - они УСТАРЕЛИ!",
				Strict: false,
				Parameters: map[string]interface{}{
					"type": "object",
					"properties": map[string]interface{}{
						"user_id": map[string]interface{}{
							"type":        "string",
							"description": "ID пользователя",
							"const":       userIDStr,
						},
					},
					"required": []string{"user_id"},
				},
			},
		},
	)

	// Добавляем функции get_s3_files и create_file ТОЛЬКО если включен S3
	if modelData.S3 {
		// Используем уже созданный userIDStr

		tools = append(tools,
			openai.AssistantTool{
				Type: "function",
				Function: &openai.FunctionDefinition{
					Name:        "get_s3_files",
					Description: "Получает список доступных файлов пользователя из S3",
					Strict:      false,
					Parameters: map[string]interface{}{
						"type": "object",
						"properties": map[string]interface{}{
							"user_id": map[string]interface{}{
								"type":        "string",
								"description": "ID пользователя",
								"const":       userIDStr, // Константа - ВСЕГДА это значение!
							},
						},
						"required": []string{"user_id"},
					},
				},
			},
			openai.AssistantTool{
				Type: "function",
				Function: &openai.FunctionDefinition{
					Name:        "create_file",
					Description: "Создает текстовый файл и сохраняет в S3",
					Strict:      false,
					Parameters: map[string]interface{}{
						"type": "object",
						"properties": map[string]interface{}{
							"user_id": map[string]interface{}{
								"type":        "string",
								"description": "ID пользователя",
								"const":       userIDStr, // Константа - ВСЕГДА это значение!
							},
							"content": map[string]interface{}{
								"type":        "string",
								"description": "Текстовое содержимое файла",
							},
							"file_name": map[string]interface{}{
								"type":        "string",
								"description": "Имя файла с расширением (.txt, .md и т.д.)",
							},
						},
						"required": []string{"user_id", "content", "file_name"},
					},
				},
			},
		)
	}

	// Добавляем функции Google Calendar если включен
	if modelData.GOAuth.HasCalendar() {
		userIDStr := fmt.Sprintf("%d", realUserID)
		tools = append(tools,
			openai.AssistantTool{
				Type: "function",
				Function: &openai.FunctionDefinition{
					Name:        "calendar_create_event",
					Description: "Создает новое событие в Google Calendar пользователя",
					Strict:      false,
					Parameters: map[string]interface{}{
						"type": "object",
						"properties": map[string]interface{}{
							"user_id": map[string]interface{}{
								"type":  "string",
								"const": userIDStr,
							},
							"title": map[string]interface{}{
								"type":        "string",
								"description": "Название события",
							},
							"description": map[string]interface{}{
								"type":        "string",
								"description": "Описание события (опционально)",
							},
							"start_time": map[string]interface{}{
								"type":        "string",
								"description": "Время начала в RFC3339 формате",
							},
							"end_time": map[string]interface{}{
								"type":        "string",
								"description": "Время окончания в RFC3339 формате",
							},
							"location": map[string]interface{}{
								"type":        "string",
								"description": "Место проведения (опционально)",
							},
						},
						"required": []string{"user_id", "title", "start_time", "end_time"},
					},
				},
			},
			openai.AssistantTool{
				Type: "function",
				Function: &openai.FunctionDefinition{
					Name:        "calendar_list_events",
					Description: "Получает список событий из Google Calendar",
					Strict:      false,
					Parameters: map[string]interface{}{
						"type": "object",
						"properties": map[string]interface{}{
							"user_id": map[string]interface{}{
								"type":  "string",
								"const": userIDStr,
							},
							"time_min": map[string]interface{}{
								"type":        "string",
								"description": "Начало периода в RFC3339 (опционально)",
							},
							"time_max": map[string]interface{}{
								"type":        "string",
								"description": "Конец периода в RFC3339 (опционально)",
							},
							"max_results": map[string]interface{}{
								"type":        "integer",
								"description": "Максимальное количество событий (по умолчанию 10)",
							},
						},
						"required": []string{"user_id"},
					},
				},
			},
			openai.AssistantTool{
				Type: "function",
				Function: &openai.FunctionDefinition{
					Name:        "calendar_delete_event",
					Description: "Удаляет событие из Google Calendar",
					Strict:      false,
					Parameters: map[string]interface{}{
						"type": "object",
						"properties": map[string]interface{}{
							"user_id": map[string]interface{}{
								"type":  "string",
								"const": userIDStr,
							},
							"event_id": map[string]interface{}{
								"type":        "string",
								"description": "ID события для удаления",
							},
						},
						"required": []string{"user_id", "event_id"},
					},
				},
			},
			openai.AssistantTool{
				Type: "function",
				Function: &openai.FunctionDefinition{
					Name:        "calendar_get_event",
					Description: "Получает детали события из Google Calendar",
					Strict:      false,
					Parameters: map[string]interface{}{
						"type": "object",
						"properties": map[string]interface{}{
							"user_id": map[string]interface{}{
								"type":  "string",
								"const": userIDStr,
							},
							"event_id": map[string]interface{}{
								"type":        "string",
								"description": "ID события для получения деталей",
							},
						},
						"required": []string{"user_id", "event_id"},
					},
				},
			},
		)
	}

	// Добавляем функции Google Sheets если включен
	if modelData.GOAuth.HasSheets() {
		userIDStr := fmt.Sprintf("%d", realUserID)
		tools = append(tools,
			openai.AssistantTool{
				Type: "function",
				Function: &openai.FunctionDefinition{
					Name:        "sheets_read_range",
					Description: "Читает данные из указанного диапазона в Google Sheets",
					Strict:      false,
					Parameters: map[string]interface{}{
						"type": "object",
						"properties": map[string]interface{}{
							"user_id": map[string]interface{}{
								"type":  "string",
								"const": userIDStr,
							},
							"spreadsheet_id": map[string]interface{}{
								"type":        "string",
								"description": "ID таблицы Google Sheets (из URL или промпта)",
							},
							"range": map[string]interface{}{
								"type":        "string",
								"description": "Диапазон для чтения (например: 'Лиды!A:F' или 'Sheet1!A1:D10')",
							},
						},
						"required": []string{"user_id", "spreadsheet_id", "range"},
					},
				},
			},
			openai.AssistantTool{
				Type: "function",
				Function: &openai.FunctionDefinition{
					Name:        "sheets_write_range",
					Description: "Записывает данные в указанный диапазон Google Sheets",
					Strict:      false,
					Parameters: map[string]interface{}{
						"type": "object",
						"properties": map[string]interface{}{
							"user_id": map[string]interface{}{
								"type":  "string",
								"const": userIDStr,
							},
							"spreadsheet_id": map[string]interface{}{
								"type":        "string",
								"description": "ID таблицы Google Sheets",
							},
							"range": map[string]interface{}{
								"type":        "string",
								"description": "Начальная ячейка для записи (например: 'Sheet1!A1')",
							},
							"values": map[string]interface{}{
								"type":        "array",
								"description": "Двумерный массив значений для записи",
								"items": map[string]interface{}{
									"type": "array",
									"items": map[string]interface{}{
										"type": "string",
									},
								},
							},
						},
						"required": []string{"user_id", "spreadsheet_id", "range", "values"},
					},
				},
			},
			openai.AssistantTool{
				Type: "function",
				Function: &openai.FunctionDefinition{
					Name:        "sheets_append_range",
					Description: "Добавляет данные в конец таблицы Google Sheets",
					Strict:      false,
					Parameters: map[string]interface{}{
						"type": "object",
						"properties": map[string]interface{}{
							"user_id": map[string]interface{}{
								"type":  "string",
								"const": userIDStr,
							},
							"spreadsheet_id": map[string]interface{}{
								"type":        "string",
								"description": "ID таблицы Google Sheets",
							},
							"range": map[string]interface{}{
								"type":        "string",
								"description": "Диапазон колонок для добавления (например: 'Sheet1!A:D')",
							},
							"values": map[string]interface{}{
								"type":        "array",
								"description": "Двумерный массив значений для добавления",
								"items": map[string]interface{}{
									"type": "array",
									"items": map[string]interface{}{
										"type": "string",
									},
								},
							},
						},
						"required": []string{"user_id", "spreadsheet_id", "range", "values"},
					},
				},
			},
		)
	}

	// Устанавливаем инструменты только если они есть
	if len(tools) > 0 {
		assistantRequest.Tools = tools
	}

	// Добавляем ToolResources только если есть векторы для file_search
	if len(vectorStoreIDs) > 0 {
		assistantRequest.ToolResources = &openai.AssistantToolResource{
			FileSearch: &openai.AssistantToolFileSearch{
				VectorStoreIDs: vectorStoreIDs,
			},
		}
	}

	assistant, err := m.openaiClient.CreateAssistant(m.ctx, assistantRequest)
	if err != nil {
		// Если были fileIDs, удаляю их из OpenAI
		for _, fileID := range ids {
			if errDel := m.deleteFileFromOpenAI(fileID); errDel != nil {
				logger.Error("ошибка удаления файла %s при ошибке создания ассистента: %v", fileID, errDel)
			}
		}

		return UMCR{}, fmt.Errorf("ошибка создания Assistant через OpenAI API: %w", err)
	}

	type VecIds struct {
		FileIds  []Ids
		VectorId []string
	}

	vecIds := VecIds{
		FileIds:  fileIDs,
		VectorId: vectorStoreIDs,
	}
	// Преобразую fileIDs в json.RawMessage
	allIds, err := json.Marshal(vecIds)
	if err != nil {
		return UMCR{}, fmt.Errorf("ошибка при преобразовании fileIDs в JSON: %w", err)
	}

	return UMCR{
		AssistID: assistant.ID,
		AllIds:   allIds,
		Provider: ProviderOpenAI,
	}, nil
}

// deleteOpenAIModel удаляет OpenAI Assistant (с поддержкой WS сообщений)
func (m *UniversalModel) deleteOpenAIModel(userId uint32, modelData *UserModelRecord, deleteFiles bool, progressCallback func(string)) error {
	if progressCallback != nil {
		progressCallback("🔄 Удаление ассистента из OpenAI...")
	}

	if m.openaiClient != nil {
		// Удаляем Assistant из OpenAI
		_, err := m.openaiClient.DeleteAssistant(m.ctx, modelData.AssistId)
		if err != nil {
			if !strings.Contains(err.Error(), "not found") {
				return fmt.Errorf("ошибка удаления ассистента: %w", err)
			}
			logger.Error("Ассистент %s не найден в OpenAI", modelData.AssistId, userId)
		}

		// Удаляем файлы только если deleteFiles = true
		if deleteFiles && len(modelData.FileIds) > 0 {
			if progressCallback != nil {
				progressCallback(fmt.Sprintf("🔄 Удаление файлов из OpenAI (%d файлов)...", len(modelData.FileIds)))
			}

			// Удаляем все файлы
			for i, file := range modelData.FileIds {
				if err := m.deleteFileFromOpenAI(file.ID); err != nil {
					logger.Error("Ошибка удаления файла %s: %v", file.ID, err, userId)
				}

				// Отправляем прогресс каждые 5 файлов
				if progressCallback != nil && (i+1)%5 == 0 {
					progressCallback(fmt.Sprintf("🔄 Удалено %d из %d файлов...", i+1, len(modelData.FileIds)))
				}
			}
		}
	} else {
		logger.Warn("OpenAI клиент не инициализирован, пропускаем удаление из API")
		if progressCallback != nil {
			progressCallback("⚠️ OpenAI клиент не инициализирован, удаляем только из БД")
		}
	}

	// Удаляем векторные хранилища
	//if len(modelData.VectorIDs) > 0 {
	//	if progressCallback != nil {
	//		progressCallback("🔄 Удаление векторных хранилищ...")
	//	}
	//
	//	for _, vectorId := range modelData.VectorIDs {
	//		if _, err := m.openaiClient.DeleteVectorStore(m.ctx, vectorId); err != nil {
	//			logger.Error("Ошибка удаления Vector Store %s: %v", vectorId, err, userId)
	//		}
	//	}
	//}

	if progressCallback != nil {
		progressCallback("✅ OpenAI Assistant и файлы удалены из API")
	}

	logger.Info("OpenAI модель успешно удалена из API и БД", userId)
	return nil
}

// createOpenAIModel создаёт OpenAI Assistant (внутренний метод)
func (m *UniversalModel) createOpenAIModel(userId uint32, modelData *UniversalModelData, fileIDs []Ids) (UMCR, error) {
	if m.openaiClient == nil {
		return UMCR{}, fmt.Errorf("OpenAI клиент не инициализирован")
	}

	if modelData == nil {
		return UMCR{}, fmt.Errorf("modelData не может быть nil")
	}

	// Используем существующий метод createModel
	umcr, err := m.createModel(userId, modelData, fileIDs)
	if err != nil {
		return UMCR{}, err
	}

	return umcr, nil
}

// updateOpenAIModelInPlace обновляет OpenAI Assistant
func (m *UniversalModel) updateOpenAIModelInPlace(userId uint32, existing, updated *UniversalModelData) error {
	// Получаем real_user_id для использования в инструкциях
	realUserID, err := m.GetRealUserID(userId)
	if err != nil {
		logger.Warn("Не удалось получить real_user_id: %v", err, userId)
		realUserID = uint64(userId) // Fallback
	}

	// Автоматически генерируем системные инструкции (ТА ЖЕ ЛОГИКА ЧТО В createModel)
	enhancedPrompt := updated.Prompt + "\n\n"

	// Добавляем важное напоминание
	if updated.MetaAction != "" || updated.Operator {
		enhancedPrompt += "##ВАЖНОЕ НАПОМИНАНИЕ:\n" +
			"В КАЖДОМ ответе ты ОБЯЗАН:\n"

		if updated.MetaAction != "" {
			enhancedPrompt += "1. Проверить условие достижения ЦЕЛИ (из твоих инструкций выше) и правильно установить target\n"
		}

		if updated.Operator {
			enhancedPrompt += "2. Проверить нужен ли оператор (из твоих инструкций выше) и правильно установить operator\n"
		}

		enhancedPrompt += "3. НЕ ИГНОРИРУЙ эти проверки!\n\n"
	}

	// Добавляем инструкции по работе с S3 файлами
	if updated.S3 {
		enhancedPrompt += "## РАБОТА С ФАЙЛАМИ S3:\n\n" +
			"### Два типа файлов:\n" +
			"1. **Существующие файлы** (найденные через get_s3_files) - используй их реальные URL\n" +
			"2. **Созданные файлы** (через create_file) - используй URL из ответа функции\n\n" +
			"### Алгоритм работы с файлами:\n" +
			"1. Для получения списка файлов вызови: get_s3_files() - без параметров\n" +
			"2. Для создания нового файла вызови: create_file({\"content\": \"...\", \"file_name\": \"...txt\"})\n" +
			"3. Для существующих файлов используй URL из ответа get_s3_files\n" +
			"4. Для созданных файлов используй URL из ответа create_file\n\n" +
			"### Определение типа файла:\n" +
			"- .jpg, .jpeg, .png, .gif, .webp, .bmp → \"photo\"\n" +
			"- .mp4, .avi, .mov, .webm, .mkv → \"video\"\n" +
			"- .mp3, .wav, .flac, .aac, .ogg → \"audio\"\n" +
			"- Остальные → \"doc\"\n\n"
	}

	// Добавляем инструкции по Code Interpreter
	if updated.Interpreter {
		enhancedPrompt += "## CODE INTERPRETER:\n" +
			"Ты можешь выполнять Python код для:\n" +
			"- Анализа данных и вычислений\n" +
			"- Создания графиков и визуализаций\n" +
			"- Обработки файлов (CSV, Excel, JSON и т.д.)\n" +
			"- Генерации файлов с результатами\n\n"
	}

	// Добавляем инструкции по GOOGLE CALENDAR
	if updated.GOAuth.HasCalendar() {
		enhancedPrompt += "## GOOGLE CALENDAR - Управление событиями:\n" +
			"У тебя есть доступ к Google Calendar пользователя.\n\n" +
			fmt.Sprintf("**user_id для всех функций: \"%d\"** (строка)\n\n", realUserID) +
			"### Доступные функции:\n" +
			"- calendar_create_event - создание события\n" +
			"- calendar_list_events - список событий\n" +
			"- calendar_delete_event - удаление события\n" +
			"- calendar_get_event - детали события\n\n" +
			"### ВАЖНО:\n" +
			"- Формат времени: RFC3339 (например: \"2026-02-05T15:00:00+03:00\")\n" +
			"- ВСЕГДА вызывай get_current_time ПЕРЕД расчётом дат!\n" +
			"- После создания/удаления подтверди действие\n\n"
	}

	// Добавляем инструкции по GOOGLE SHEETS
	if updated.GOAuth.HasSheets() {
		enhancedPrompt += "## 📊 GOOGLE SHEETS - Работа с таблицами:\n" +
			"У тебя есть доступ к Google Sheets пользователя.\n\n" +
			fmt.Sprintf("**user_id для всех функций: \"%d\"** (строка)\n\n", realUserID) +
			"\n" +
			"КРИТИЧЕСКИ ВАЖНО - ОБЯЗАТЕЛЬНО ВЫЗЫВАЙ ФУНКЦИИ!\n" +
			"КАТЕГОРИЧЕСКИ ЗАПРЕЩЕНО:\n" +
			"\"К сожалению, я не могу определить количество строк\"\n" +
			"\"Отсутствие доступа к аккаунту пользователя\"\n" +
			"\"Я не могу получить данные из таблицы\"\n" +
			"\"Пожалуйста, убедитесь, что у меня есть доступ\"\n" +
			"Отвечать БЕЗ вызова функции sheets_read_range\n\n" +
			"ПРАВИЛЬНЫЕ ДЕЙСТВИЯ:\n" +
			"1. Вопрос \"что в таблице\" → НЕМЕДЛЕННО вызови sheets_read_range\n" +
			"2. Вопрос \"сколько строк\" → ОБЯЗАТЕЛЬНО вызови sheets_read_range и посчитай len(values)-1\n" +
			"3. НЕ ОТВЕЧАЙ из промпта - ВЫЗОВИ ФУНКЦИЮ!\n" +
			"4. У ТЕБЯ УЖЕ ЕСТЬ ДОСТУП через функции!\n\n" +
			"spreadsheet_id ищи в промпте или запросе пользователя\n" +
			"Диапазон: 'Лиды!A:F' или 'Sheet1!A:Z'\n\n" +
			"### Функции:\n" +
			"- sheets_read_range - чтение данных\n" +
			"- sheets_write_range - запись\n" +
			"- sheets_append_range - добавление строк\n" +
			"═══════════════════════════════════════════════════════════\n\n"
	}

	// Добавляем инструкции по поиску в документах
	if updated.Search || len(updated.FileIds) > 0 {
		enhancedPrompt += "## ПОИСК В ДОКУМЕНТАХ (File Search):\n" +
			"У тебя есть доступ к базе знаний из загруженных документов.\n" +
			"Используй file_search для поиска информации в документах пользователя.\n" +
			"Всегда ссылайся на источники при использовании информации из документов.\n\n"
	}

	// Добавляем общие правила для send_files
	if updated.S3 || updated.Interpreter {
		enhancedPrompt += "## ПРАВИЛА отправки файлов (send_files):\n" +
			"1. Если НЕ отправляешь файлы - send_files должен быть пустым массивом []\n" +
			"2. Если упоминаешь файлы в message - ОБЯЗАТЕЛЬНО добавь их в send_files\n" +
			"3. Каждый файл в send_files должен содержать:\n" +
			"   - type, Url, file_name, caption\n\n"
	}

	// Финальная инструкция по формату ответа
	enhancedPrompt += "## ФОРМАТ ОТВЕТА:\n" +
		"Твой ответ ВСЕГДА должен быть в формате JSON Schema:\n" +
		ModelShemaJSON + "\n\n" +
		"### КРИТИЧЕСКИ ВАЖНО - ПРАВИЛА для полей JSON:\n\n" +
		"**message**: Твоё текстовое сообщение пользователю\n\n" +
		"**action.send_files**: Массив файлов для отправки ([] если файлов нет)\n\n"

	if updated.MetaAction != "" {
		enhancedPrompt += "**target** (boolean) - Достигнута ли ЦЕЛЬ диалога:\n" +
			"Проверяй условие достижения цели из СВОИХ ИНСТРУКЦИЙ ВЫШЕ\n" +
			"Если условие ТОЧНО выполнено → target: true\n" +
			"Если условие НЕ выполнено → target: false\n\n"
	} else {
		enhancedPrompt += "**target**: ВСЕГДА false (цели нет)\n\n"
	}

	if updated.Operator {
		enhancedPrompt += "**operator** (boolean) - Требуется ли оператор:\n" +
			" Проверяй условие вызова оператора из СВОИХ ИНСТРУКЦИЙ ВЫШЕ\n" +
			" Если пользователь просит оператора → operator: true\n" +
			" Во всех остальных случаях → operator: false\n\n"
	}

	// Добавляем примеры
	if updated.MetaAction != "" {
		if updated.Operator {
			enhancedPrompt += "### Пример ответа когда цель ДОСТИГНУТА:\n" +
				"```json\n{\n  \"message\": \"Привет, Жорик! Рад познакомиться! 😊\",\n" +
				"  \"action\": {\"send_files\": []},\n  \"target\": true,\n  \"operator\": false\n}\n```\n\n" +
				"### Пример ответа когда цель НЕ достигнута:\n" +
				"```json\n{\n  \"message\": \"Привет! Как дела? 😊\",\n" +
				"  \"action\": {\"send_files\": []},\n  \"target\": false,\n  \"operator\": false\n}\n```\n\n"
		} else {
			enhancedPrompt += "### Пример ответа когда цель ДОСТИГНУТА:\n" +
				"```json\n{\n  \"message\": \"Привет, Жорик! Рад познакомиться! 😊\",\n" +
				"  \"action\": {\"send_files\": []},\n  \"target\": true\n}\n```\n\n" +
				"### Пример ответа когда цель НЕ достигнута:\n" +
				"```json\n{\n  \"message\": \"Привет! Как дела? 😊\",\n" +
				"  \"action\": {\"send_files\": []},\n  \"target\": false\n}\n```\n\n"
		}
	}

	enhancedPrompt += "ВАЖНО: Возвращай только валидный JSON без дополнительного текста."

	// Генерируем JSON Schema
	hasMetaAction := updated.MetaAction != ""
	hasOperator := updated.Operator
	dynamicSchema := generateModelSchema(hasMetaAction, hasOperator)
	schemaJSON, err := json.Marshal(dynamicSchema)
	if err != nil {
		return fmt.Errorf("ошибка сериализации JSON Schema: %w", err)
	}

	description := fmt.Sprintf("Модель для пользователя %d", userId)

	// Обрабатываем векторные хранилища и файлы
	var vectorStoreIDs []string
	var tools []openai.AssistantTool

	// Получаем все модели пользователя и находим нужную
	allModels, err := m.db.GetAllUserModels(userId)
	if err != nil {
		return fmt.Errorf("ошибка получения моделей пользователя: %w", err)
	}

	var existingModelData *UserModelRecord
	for i := range allModels {
		if allModels[i].Provider == existing.Provider {
			existingModelData = &allModels[i]
			break
		}
	}

	if existingModelData == nil {
		return fmt.Errorf("запись модели провайдера %s не найдена для пользователя", existing.Provider)
	}

	needsFileSearch := updated.Search && len(updated.FileIds) > 0

	if needsFileSearch {
		// Проверяем, изменились ли файлы
		if !filesEqual(existing.FileIds, updated.FileIds) {
			// Создаем новое векторное хранилище
			var ids []string
			for _, fileID := range updated.FileIds {
				if fileID.ID != "" {
					ids = append(ids, fileID.ID)
				}
			}

			vsName := fmt.Sprintf("vs_user_%d_%d", userId, time.Now().Unix())
			vectorStore, err := m.openaiClient.CreateVectorStore(m.ctx, openai.VectorStoreRequest{
				Name:    vsName,
				FileIDs: ids,
			})
			if err != nil {
				return fmt.Errorf("ошибка создания Vector Store: %w", err)
			}
			vectorStoreIDs = append(vectorStoreIDs, vectorStore.ID)

			// Удаляем старые файлы и векторные хранилища
			for _, file := range existing.FileIds {
				if err := m.deleteFileFromOpenAI(file.ID); err != nil {
					logger.Error("Ошибка удаления файла %s: %v", file.ID, err, userId)
				}
			}

			for _, oldVectorId := range existing.VecIds.VectorId {
				if _, err := m.openaiClient.DeleteVectorStore(m.ctx, oldVectorId); err != nil {
					logger.Error("Ошибка удаления Vector Store %s: %v", oldVectorId, err, userId)
				}
			}
		} else {
			vectorStoreIDs = existing.VecIds.VectorId
		}

		tools = append(tools, openai.AssistantTool{Type: "file_search"})
	} else {
		// Удаляем все файлы и векторные хранилища
		for _, file := range existing.FileIds {
			if err := m.deleteFileFromOpenAI(file.ID); err != nil {
				logger.Error("Ошибка удаления файла %s: %v", file.ID, err, userId)
			}
		}

		for _, vectorId := range existing.VecIds.VectorId {
			if _, err := m.openaiClient.DeleteVectorStore(m.ctx, vectorId); err != nil {
				logger.Error("Ошибка удаления Vector Store %s: %v", vectorId, err, userId)
			}
		}

		vectorStoreIDs = []string{}
	}

	// Code interpreter
	if updated.Interpreter {
		tools = append(tools, openai.AssistantTool{Type: "code_interpreter"})
	}

	// Добавляем функцию get_current_time ВСЕГДА
	userIDStr := fmt.Sprintf("%d", realUserID)
	tools = append(tools,
		openai.AssistantTool{
			Type: "function",
			Function: &openai.FunctionDefinition{
				Name: "get_current_time",
				Description: "Получает ТОЧНОЕ текущее время и дату с сервера в часовом поясе пользователя. " +
					"ОБЯЗАТЕЛЬНО используй эту функцию ПЕРЕД расчётом дат (завтра, через неделю, в понедельник и т.д.). " +
					"НЕ используй свои внутренние знания о дате - они УСТАРЕЛИ!",
				Strict: false,
				Parameters: map[string]interface{}{
					"type": "object",
					"properties": map[string]interface{}{
						"user_id": map[string]interface{}{
							"type":        "string",
							"description": "ID пользователя",
							"const":       userIDStr,
						},
					},
					"required": []string{"user_id"},
				},
			},
		},
	)

	// Добавляем функции S3 ТОЛЬКО если включен
	if updated.S3 {
		// Используем уже созданный userIDStr

		tools = append(tools,
			openai.AssistantTool{
				Type: "function",
				Function: &openai.FunctionDefinition{
					Name:        "get_s3_files",
					Description: "Получает список доступных файлов пользователя из S3",
					Strict:      false,
					Parameters: map[string]interface{}{
						"type": "object",
						"properties": map[string]interface{}{
							"user_id": map[string]interface{}{
								"type":        "string",
								"description": "ID пользователя",
								"const":       userIDStr, // Константа - ВСЕГДА это значение!
							},
						},
						"required": []string{"user_id"},
					},
				},
			},
			openai.AssistantTool{
				Type: "function",
				Function: &openai.FunctionDefinition{
					Name:        "create_file",
					Description: "Создает текстовый файл и сохраняет в S3",
					Strict:      false,
					Parameters: map[string]interface{}{
						"type": "object",
						"properties": map[string]interface{}{
							"user_id": map[string]interface{}{
								"type":        "string",
								"description": "ID пользователя",
								"const":       userIDStr, // Константа - ВСЕГДА это значение!
							},
							"content": map[string]interface{}{
								"type":        "string",
								"description": "Текстовое содержимое файла",
							},
							"file_name": map[string]interface{}{
								"type":        "string",
								"description": "Имя файла с расширением",
							},
						},
						"required": []string{"user_id", "content", "file_name"},
					},
				},
			},
		)
	}

	// Добавляем функции Google Calendar если включен
	if updated.GOAuth.HasCalendar() {
		userIDStr := fmt.Sprintf("%d", realUserID)
		tools = append(tools,
			openai.AssistantTool{
				Type: "function",
				Function: &openai.FunctionDefinition{
					Name:        "calendar_create_event",
					Description: "Создает новое событие в Google Calendar пользователя",
					Strict:      false,
					Parameters: map[string]interface{}{
						"type": "object",
						"properties": map[string]interface{}{
							"user_id": map[string]interface{}{
								"type":  "string",
								"const": userIDStr,
							},
							"title": map[string]interface{}{
								"type":        "string",
								"description": "Название события",
							},
							"description": map[string]interface{}{
								"type":        "string",
								"description": "Описание события (опционально)",
							},
							"start_time": map[string]interface{}{
								"type":        "string",
								"description": "Время начала в RFC3339 формате",
							},
							"end_time": map[string]interface{}{
								"type":        "string",
								"description": "Время окончания в RFC3339 формате",
							},
							"location": map[string]interface{}{
								"type":        "string",
								"description": "Место проведения (опционально)",
							},
						},
						"required": []string{"user_id", "title", "start_time", "end_time"},
					},
				},
			},
			openai.AssistantTool{
				Type: "function",
				Function: &openai.FunctionDefinition{
					Name:        "calendar_list_events",
					Description: "Получает список событий из Google Calendar",
					Strict:      false,
					Parameters: map[string]interface{}{
						"type": "object",
						"properties": map[string]interface{}{
							"user_id": map[string]interface{}{
								"type":  "string",
								"const": userIDStr,
							},
							"time_min": map[string]interface{}{
								"type":        "string",
								"description": "Начало периода в RFC3339 (опционально)",
							},
							"time_max": map[string]interface{}{
								"type":        "string",
								"description": "Конец периода в RFC3339 (опционально)",
							},
							"max_results": map[string]interface{}{
								"type":        "integer",
								"description": "Максимальное количество событий (по умолчанию 10)",
							},
						},
						"required": []string{"user_id"},
					},
				},
			},
			openai.AssistantTool{
				Type: "function",
				Function: &openai.FunctionDefinition{
					Name:        "calendar_delete_event",
					Description: "Удаляет событие из Google Calendar",
					Strict:      false,
					Parameters: map[string]interface{}{
						"type": "object",
						"properties": map[string]interface{}{
							"user_id": map[string]interface{}{
								"type":  "string",
								"const": userIDStr,
							},
							"event_id": map[string]interface{}{
								"type":        "string",
								"description": "ID события для удаления",
							},
						},
						"required": []string{"user_id", "event_id"},
					},
				},
			},
			openai.AssistantTool{
				Type: "function",
				Function: &openai.FunctionDefinition{
					Name:        "calendar_get_event",
					Description: "Получает детали события из Google Calendar",
					Strict:      false,
					Parameters: map[string]interface{}{
						"type": "object",
						"properties": map[string]interface{}{
							"user_id": map[string]interface{}{
								"type":  "string",
								"const": userIDStr,
							},
							"event_id": map[string]interface{}{
								"type":        "string",
								"description": "ID события для получения деталей",
							},
						},
						"required": []string{"user_id", "event_id"},
					},
				},
			},
		)
	}

	// Добавляем функции Google Sheets если включен
	if updated.GOAuth.HasSheets() {
		userIDStr := fmt.Sprintf("%d", realUserID)
		tools = append(tools,
			openai.AssistantTool{
				Type: "function",
				Function: &openai.FunctionDefinition{
					Name:        "sheets_read_range",
					Description: "Читает данные из Google Sheets",
					Strict:      false,
					Parameters: map[string]interface{}{
						"type": "object",
						"properties": map[string]interface{}{
							"user_id":        map[string]interface{}{"type": "string", "const": userIDStr},
							"spreadsheet_id": map[string]interface{}{"type": "string", "description": "ID таблицы"},
							"range":          map[string]interface{}{"type": "string", "description": "Диапазон (например: 'Лиды!A:F' или 'Sheet1!A1:D10')"},
						},
						"required": []string{"user_id", "spreadsheet_id", "range"},
					},
				},
			},
			openai.AssistantTool{
				Type: "function",
				Function: &openai.FunctionDefinition{
					Name:        "sheets_write_range",
					Description: "Записывает данные в Google Sheets",
					Strict:      false,
					Parameters: map[string]interface{}{
						"type": "object",
						"properties": map[string]interface{}{
							"user_id":        map[string]interface{}{"type": "string", "const": userIDStr},
							"spreadsheet_id": map[string]interface{}{"type": "string", "description": "ID таблицы"},
							"range":          map[string]interface{}{"type": "string", "description": "Диапазон для записи"},
							"values": map[string]interface{}{
								"type":  "array",
								"items": map[string]interface{}{"type": "array", "items": map[string]interface{}{"type": "string"}},
							},
						},
						"required": []string{"user_id", "spreadsheet_id", "range", "values"},
					},
				},
			},
			openai.AssistantTool{
				Type: "function",
				Function: &openai.FunctionDefinition{
					Name:        "sheets_append_range",
					Description: "Добавляет данные в конец Google Sheets",
					Strict:      false,
					Parameters: map[string]interface{}{
						"type": "object",
						"properties": map[string]interface{}{
							"user_id":        map[string]interface{}{"type": "string", "const": userIDStr},
							"spreadsheet_id": map[string]interface{}{"type": "string", "description": "ID таблицы"},
							"range":          map[string]interface{}{"type": "string", "description": "Диапазон колонок"},
							"values": map[string]interface{}{
								"type":  "array",
								"items": map[string]interface{}{"type": "array", "items": map[string]interface{}{"type": "string"}},
							},
						},
						"required": []string{"user_id", "spreadsheet_id", "range", "values"},
					},
				},
			},
		)
	}

	// Создаем запрос на обновление
	updateRequest := openai.AssistantRequest{
		Name:         &updated.Name,
		Description:  &description,
		Instructions: &enhancedPrompt, // Используем улучшенный промпт
		Model:        updated.GptType.Name,
		Tools:        tools,
		Metadata: map[string]any{
			"realUserId":      fmt.Sprintf("%d", realUserID),
			"operatorEnabled": fmt.Sprintf("%t", updated.Operator),
			"hasMetaAction":   fmt.Sprintf("%t", updated.MetaAction != ""),
		},
		ResponseFormat: &openai.ChatCompletionResponseFormat{
			Type: openai.ChatCompletionResponseFormatTypeJSONSchema,
			JSONSchema: &openai.ChatCompletionResponseFormatJSONSchema{
				Name:   "response_with_action_files",
				Strict: true,
				Schema: json.RawMessage(schemaJSON), // Динамическая схема
			},
		},
	}

	// Добавляем ToolResources только если есть векторы
	if len(vectorStoreIDs) > 0 {
		updateRequest.ToolResources = &openai.AssistantToolResource{
			FileSearch: &openai.AssistantToolFileSearch{
				VectorStoreIDs: vectorStoreIDs,
			},
		}
	}

	// Обновляем ассистента через OpenAI API
	_, err = m.openaiClient.ModifyAssistant(m.ctx, existingModelData.AssistId, updateRequest)
	if err != nil {
		return fmt.Errorf("ошибка обновления Assistant: %w", err)
	}

	// Обновляем информацию о файлах и векторах
	type VecIds struct {
		FileIds  []Ids
		VectorId []string
	}

	vecIds := VecIds{
		FileIds:  updated.FileIds,
		VectorId: vectorStoreIDs,
	}

	vecIdsJSON, err := json.Marshal(vecIds)
	if err != nil {
		return fmt.Errorf("ошибка сериализации vector IDs: %w", err)
	}

	umcr := UMCR{
		AssistID: existingModelData.AssistId,
		AllIds:   vecIdsJSON,
		Provider: ProviderOpenAI,
	}

	// Сохраняем в БД
	if err := m.SaveModel(userId, umcr, updated); err != nil {
		return fmt.Errorf("ошибка сохранения обновленной модели в БД: %w", err)
	}

	logger.Info("OpenAI Assistant успешно обновлен", userId)
	return nil
}

// filesEqual сравнивает два слайса файлов
// Используется для проверки изменились ли файлы при обновлении модели
func filesEqual(a, b []Ids) bool {
	if len(a) != len(b) {
		return false
	}

	aMap := make(map[string]string)
	for _, file := range a {
		aMap[file.ID] = file.Name
	}

	for _, file := range b {
		if name, exists := aMap[file.ID]; !exists || name != file.Name {
			return false
		}
	}

	return true
}
