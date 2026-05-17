package google

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"path/filepath"
	"strings"
	"time"

	"github.com/gorilla/websocket"
	"github.com/ikermy/AiR_Common/pkg/comdb"
	"github.com/ikermy/AiR_Common/pkg/model"
	"github.com/ikermy/AiR_Common/pkg/model/create"
)

// ============================================================================
// pumpFromGoogle — читает события из Google Live API WS и маршрутизирует их:
//   - modelTurn.parts[].inlineData → rs.AudioOut (PCM16 @ 24kHz для воспроизведения)
//   - modelTurn.parts[].text       → аккумуляция текста → saveGoogleRealtimeTranscript
//   - toolCall.functionCalls       → RunAction → toolResponse
//   - serverContent.interrupted    → rs.DrainPlayback (сигнал сбросить очередь)
//   - serverContent.turnComplete   → publishEvent("response_done")
//   - inputAudioTranscription      → saveGoogleRealtimeTranscript (пользователь)
//   - outputAudioTranscription     → transcript_delta (модель, параллельно с аудио)
// ============================================================================

func (m *Model) pumpFromGoogle(rs *GoogleRealtimeSession) {
	defer func() {
		rs.publishEvent(model.RealtimeEvent{Type: "error", Text: "realtime session closed", Err: fmt.Errorf("session closed")})
		rs.cancel()
	}()

	// assistTextBuf аккумулирует текстовые части ответа модели до turnComplete.
	// Накопленный текст сохраняется в историю диалога (TEXT modality или outputAudioTranscription).
	var assistTextBuf strings.Builder

	var pendingFiles []model.File

	//logger.Info("pumpFromGoogle: горутина запущена respId=%d dialogID=%d", rs.respId, rs.dialogID, rs.userID)

	for {
		select {
		case <-rs.ctx.Done():
			return
		default:
		}

		_, msg, err := rs.googleConn.ReadMessage()
		if err != nil {
			// Извлекаем реальный код и причину закрытия от Google
			var closeErr *websocket.CloseError
			if errors.As(err, &closeErr) {
				// Google закрыл сессию с кодом: публикуем полную причину (видна в логах и клиенту)
				text := fmt.Sprintf("google close code=%d: %s", closeErr.Code, closeErr.Text)
				//logger.Error("pumpFromGoogle: Google закрыл сессию respId=%d code=%d reason=%q", rs.respId, closeErr.Code, closeErr.Text, rs.userID)
				rs.publishEvent(model.RealtimeEvent{Type: "error", Text: text, Err: err})
			} else if !strings.Contains(err.Error(), "use of closed network connection") {
				//logger.Error("pumpFromGoogle: ошибка чтения WS respId=%d: %v", rs.respId, err, rs.userID)
				rs.publishEvent(model.RealtimeEvent{Type: "error", Text: err.Error(), Err: err})
			}
			return
		}

		var event map[string]interface{}
		if err := json.Unmarshal(msg, &event); err != nil {
			//logger.Warn("pumpFromGoogle: ошибка парсинга события: %v raw=%s", err, string(msg), rs.userID)
			continue
		}
		// ── setupComplete — инжектируем историю + приветствие одним сообщением ──
		if _, ok := event["setupComplete"]; ok {
			//log.Printf("[pumpFromGoogle] setupComplete respId=%d — инжект истории + приветствие", rs.respId)
			select {
			case <-rs.setupCompleteCh:
			default:
				close(rs.setupCompleteCh)
			}

			// Отправляем историю диалога + приветствие одним clientContent (turnComplete=true).
			// Раздельный инжект истории с turnComplete=false вызывает 1007 "invalid argument".
			m.sendHistoryAndGreeting(rs)
			continue
		}

		// ── usageMetadata ─────────────────────────────────────────────────────
		if usage, ok := event["usageMetadata"].(map[string]interface{}); ok {
			if usageJSON, err := json.Marshal(map[string]interface{}{"type": "token_usage", "usage": usage}); err == nil {
				rs.publishEvent(model.RealtimeEvent{Type: "token_usage", Data: usageJSON})
			}
			continue
		}

		// ── inputAudioTranscription — транскрипция речи пользователя ──────────
		// Google Live API присылает этот event параллельно с обработкой аудио.
		if inputTransRaw, ok := event["inputAudioTranscription"].(map[string]interface{}); ok {
			if transcript, _ := inputTransRaw["transcript"].(string); transcript != "" {
				m.saveGoogleRealtimeTranscript(rs, transcript, "")
				rs.publishEvent(model.RealtimeEvent{Type: "input_transcript_done", Text: transcript})
			}
			continue
		}

		// ── outputAudioTranscription — текстовая расшифровка речи модели ──────
		// Приходит параллельно с аудио-дельтами при responseModalities=["TEXT","AUDIO"].
		// Используем как основной источник транскрипта (точнее чем TEXT-части modelTurn).
		if outputTransRaw, ok := event["outputAudioTranscription"].(map[string]interface{}); ok {
			if text, _ := outputTransRaw["text"].(string); text != "" {
				assistTextBuf.WriteString(text)
				rs.publishEvent(model.RealtimeEvent{Type: "transcript_delta", Text: text})
			}
			continue
		}

		// ── toolCall — вызовы функций ────────────────────────────────────────
		// Google Live API отправляет tool calls как отдельный top-level event.
		if toolCallRaw, ok := event["toolCall"].(map[string]interface{}); ok {
			m.handleGoogleToolCall(rs, toolCallRaw, &pendingFiles)
			continue
		}

		// ── toolCallCancellation — отмена tool call ───────────────────────────
		if _, ok := event["toolCallCancellation"]; ok {
			//logger.Debug("pumpFromGoogle: toolCallCancellation respId=%d", rs.respId, rs.userID)
			continue
		}

		// ── serverContent — основной ответ модели ────────────────────────────
		serverContent, ok := event["serverContent"].(map[string]interface{})
		if !ok {
			continue
		}

		// interrupted — пользователь перебил модель (barge-in)
		if interrupted, _ := serverContent["interrupted"].(bool); interrupted {
			log.Printf("[pumpFromGoogle] *** INTERRUPTED *** respId=%d audioOutBuf=%d", rs.respId, len(rs.AudioOut))
			rs.IsGenerating.Store(false)
			assistTextBuf.Reset()

			// Дренируем буфер AudioOut — иначе оставшиеся чанки продолжат воспроизводиться
			drained := 0
			for len(rs.AudioOut) > 0 {
				<-rs.AudioOut
				drained++
			}
			if drained > 0 {
				log.Printf("[pumpFromGoogle] interrupted: дренировано %d чанков из AudioOut respId=%d", drained, rs.respId)
			}

			select {
			case rs.DrainPlayback <- struct{}{}:
			default:
			}
			// "interrupted" — отдельный тип события, чтобы хэндлер мог отличить
			// barge-in прерывание от нормального turnComplete и немедленно
			// остановить воспроизведение на клиенте (очистить буфер WebAudio и т.п.)
			rs.publishEvent(model.RealtimeEvent{Type: "interrupted"})
			continue
		}

		// modelTurn — аудио-дельты, текст и возможный function call
		if modelTurnRaw, ok := serverContent["modelTurn"].(map[string]interface{}); ok {
			rs.IsGenerating.Store(true)
			parts, _ := modelTurnRaw["parts"].([]interface{})

			for _, partRaw := range parts {
				part, ok := partRaw.(map[string]interface{})
				if !ok {
					continue
				}

				// Audio delta (PCM16 @ 24kHz, base64)
				if inlineData, ok := part["inlineData"].(map[string]interface{}); ok {
					data, _ := inlineData["data"].(string)
					if data == "" {
						continue
					}
					pcm16, err := base64.StdEncoding.DecodeString(data)
					if err != nil {
						continue
					}
					select {
					case rs.AudioOut <- pcm16:
					case <-rs.ctx.Done():
						return
					default:
						log.Printf("[pumpFromGoogle] AudioOut overflow, дроп %d байт respId=%d", len(pcm16), rs.respId)
					}
					continue
				}

				// Text part (TEXT modality или transcript параллельно с аудио)
				// outputAudioTranscription уже перехватили выше — здесь могут быть
				// TEXT-части от функций или другие текстовые ответы.
				if text, ok := part["text"].(string); ok && text != "" {
					// Добавляем только если ещё не накоплено через outputAudioTranscription
					// (избегаем дублирования). Простой heuristic: пишем только если assistTextBuf пуст
					// или text не содержится уже.
					assistTextBuf.WriteString(text)
					rs.publishEvent(model.RealtimeEvent{Type: "transcript_delta", Text: text})
					continue
				}

				// functionCall в составе modelTurn (альтернативный формат ряда версий API)
				if funcCall, ok := part["functionCall"].(map[string]interface{}); ok {
					m.handleGoogleFunctionCallPart(rs, funcCall, &pendingFiles)
					continue
				}
			}
		}

		// turnComplete — ход модели завершён
		if turnComplete, _ := serverContent["turnComplete"].(bool); turnComplete {
			rs.IsGenerating.Store(false)
			log.Printf("[pumpFromGoogle] turnComplete respId=%d assistTextLen=%d", rs.respId, assistTextBuf.Len())

			// Сохраняем накопленный транскрипт ответа ассистента в историю диалога
			if assistText := assistTextBuf.String(); assistText != "" {
				m.saveGoogleRealtimeTranscript(rs, "", assistText)
				assistTextBuf.Reset()
			}

			files := pendingFiles
			pendingFiles = nil
			rs.publishEvent(model.RealtimeEvent{Type: "response_done", Files: files})
		}
	}
}

// handleGoogleToolCall обрабатывает top-level toolCall event (основной формат Google Live API).
func (m *Model) handleGoogleToolCall(rs *GoogleRealtimeSession, toolCall map[string]interface{}, pendingFiles *[]model.File) {
	funcCallsRaw, ok := toolCall["functionCalls"].([]interface{})
	if !ok {
		return
	}

	var funcResponses []map[string]interface{}

	for _, fcRaw := range funcCallsRaw {
		fc, ok := fcRaw.(map[string]interface{})
		if !ok {
			continue
		}

		callID, _ := fc["id"].(string)
		name, _ := fc["name"].(string)

		argsJSON := ""
		if args, ok := fc["args"]; ok {
			if b, err := json.Marshal(args); err == nil {
				argsJSON = string(b)
			}
		}

		result, files := m.execGoogleTool(rs, name, argsJSON, callID)
		*pendingFiles = append(*pendingFiles, files...)

		funcResponses = append(funcResponses, map[string]interface{}{
			"id":   callID,
			"name": name,
			"response": map[string]interface{}{
				"output": result,
			},
		})
	}

	if len(funcResponses) == 0 {
		return
	}

	toolResp := map[string]interface{}{
		"toolResponse": map[string]interface{}{
			"functionResponses": funcResponses,
		},
	}
	if err := rs.writeJSON(toolResp); err != nil {
		//logger.Warn("handleGoogleToolCall: ошибка отправки toolResponse: %v respId=%d", err, rs.respId, rs.userID)
	}
}

// handleGoogleFunctionCallPart обрабатывает functionCall из modelTurn.parts (альтернативный формат).
func (m *Model) handleGoogleFunctionCallPart(rs *GoogleRealtimeSession, funcCall map[string]interface{}, pendingFiles *[]model.File) {
	callID, _ := funcCall["id"].(string)
	name, _ := funcCall["name"].(string)

	argsJSON := ""
	if args, ok := funcCall["args"]; ok {
		if b, err := json.Marshal(args); err == nil {
			argsJSON = string(b)
		}
	}

	result, files := m.execGoogleTool(rs, name, argsJSON, callID)
	*pendingFiles = append(*pendingFiles, files...)

	toolResp := map[string]interface{}{
		"toolResponse": map[string]interface{}{
			"functionResponses": []map[string]interface{}{
				{
					"id":   callID,
					"name": name,
					"response": map[string]interface{}{
						"output": result,
					},
				},
			},
		},
	}
	if err := rs.writeJSON(toolResp); err != nil {
		//logger.Warn("handleGoogleFunctionCallPart: ошибка отправки toolResponse: %v respId=%d", err, rs.respId, rs.userID)
	}
}

// execGoogleTool исполняет один инструмент и возвращает строковый результат + извлечённые файлы.
// send_file_to_user обрабатывается локально (без RunAction).
func (m *Model) execGoogleTool(rs *GoogleRealtimeSession, name, argsJSON, callID string) (result string, files []model.File) {
	// send_file_to_user — синтетический tool, обрабатывается локально.
	if name == "send_file_to_user" {
		var params struct {
			URL      string `json:"url"`
			FileName string `json:"file_name"`
		}
		if err := json.Unmarshal([]byte(argsJSON), &params); err == nil && params.URL != "" {
			if params.FileName == "" {
				params.FileName = filepath.Base(params.URL)
			}
			fileType := googleRealtimeFileType(params.URL)
			files = append(files, model.File{
				Type:     model.FileType(fileType),
				URL:      params.URL,
				FileName: params.FileName,
			})
			result = fmt.Sprintf(`{"status":"ok","file_name":%q,"type":%q}`, params.FileName, fileType)
		} else {
			result = `{"status":"error","message":"invalid parameters"}`
		}
		rs.publishEvent(model.RealtimeEvent{
			Type: "function_result",
			Text: fmt.Sprintf(`{"call_id":%q,"name":%q,"result":%s}`, callID, name, result),
		})
		return result, files
	}

	rawResult := m.actionHandler.RunAction(rs.ctx, name, argsJSON, 1, rs.userID)

	// Пытаемся извлечь файлы из результата (универсально — без привязки к имени функции).
	modelResult := rawResult
	if extractedFiles, voiceConfirm := googleExtractFilesForRealtime(rawResult); len(extractedFiles) > 0 {
		for _, f := range extractedFiles {
			fileType, _ := f["type"].(string)
			fileURL, _ := f["Url"].(string)
			fileName, _ := f["file_name"].(string)
			caption, _ := f["caption"].(string)
			files = append(files, model.File{
				Type:     model.FileType(fileType),
				URL:      fileURL,
				FileName: fileName,
				Caption:  caption,
			})
		}
		modelResult = voiceConfirm
	} else if voiceConfirm != "" {
		modelResult = voiceConfirm
	}

	rs.publishEvent(model.RealtimeEvent{
		Type: "function_result",
		Text: fmt.Sprintf(`{"call_id":%q,"name":%q,"result":%s}`, callID, name, rawResult),
	})

	return modelResult, files
}

// ============================================================================
// pumpToGoogle — читает PCM16 из AudioIn и отправляет realtimeInput.mediaChunks.
// Накапливает 100ms чанки (3200 байт @ 16kHz PCM16 mono) перед отправкой.
// ============================================================================

func (m *Model) pumpToGoogle(rs *GoogleRealtimeSession) {
	// Ждем setupComplete перед отправкой аудио, иначе API вернет 1011 (Internal Server Error)
	select {
	case <-rs.ctx.Done():
		return
	case <-rs.setupCompleteCh:
		// Setup завершен, можно начинать стримить аудио
	}

	// 100ms @ 16kHz PCM16 mono = 16000 * 0.1 * 2 = 3200 байт
	const accumulateBytes = 3200
	var accumBuf []byte

	mimeType := fmt.Sprintf("audio/pcm;rate=%d", create.GoogleRealtimeInputSampleRate)

	// Для отладки barge-in: считаем фреймы пока модель генерирует
	var audioWhileGenerating int

	flush := func() {
		if len(accumBuf) == 0 {
			return
		}
		encoded := base64.StdEncoding.EncodeToString(accumBuf)
		accumBuf = accumBuf[:0]
		msg := map[string]interface{}{
			"realtimeInput": map[string]interface{}{
				"audio": map[string]interface{}{
					"mimeType": mimeType,
					"data":     encoded,
				},
			},
		}
		if err := rs.writeJSON(msg); err != nil {
			if !strings.Contains(err.Error(), "use of closed network connection") {
			}
		}
	}

	for {
		select {
		case <-rs.ctx.Done():
			flush()
			return
		case pcm16, ok := <-rs.AudioIn:
			if !ok {
				flush()
				return
			}
			if len(pcm16) == 0 {
				continue
			}
			// Отладка: считаем аудио пока модель генерирует
			if rs.IsGenerating.Load() {
				audioWhileGenerating++
				if audioWhileGenerating == 1 || audioWhileGenerating%50 == 0 {
					log.Printf("[pumpToGoogle] аудио от клиента ВО ВРЕМЯ генерации модели: фрейм #%d len=%d respId=%d", audioWhileGenerating, len(pcm16), rs.respId)
				}
			} else {
				if audioWhileGenerating > 0 {
					log.Printf("[pumpToGoogle] модель перестала генерировать, всего аудио во время генерации: %d фреймов respId=%d", audioWhileGenerating, rs.respId)
					audioWhileGenerating = 0
				}
			}
			accumBuf = append(accumBuf, pcm16...)
			if len(accumBuf) >= accumulateBytes {
				flush()
			}
		}
	}
}

// ============================================================================
// saveGoogleRealtimeTranscript — сохраняет транскрипцию в DialogCache и БД
// ============================================================================

func (m *Model) saveGoogleRealtimeTranscript(rs *GoogleRealtimeSession, userText, assistText string) {
	now := time.Now()

	if userText != "" {
		m.addMessageToCache(rs.dialogID, GoogleContent{
			Role:  "user",
			Parts: []map[string]interface{}{{"text": userText}},
		})
		msg := googleRealtimeDialogJSON(comdb.SpeechRealTimeUser, userText, now)
		if err := m.db.SaveDialog(rs.dialogID, msg); err != nil {
			//logger.Warn("saveGoogleRealtimeTranscript: ошибка сохранения реплики пользователя: %v", err, rs.userID)
		}
		//logger.Debug("saveGoogleRealtimeTranscript: user len=%d dialogID=%d", len(userText), rs.dialogID, rs.userID)
	}

	if assistText != "" {
		m.addMessageToCache(rs.dialogID, GoogleContent{
			Role:  "model",
			Parts: []map[string]interface{}{{"text": assistText}},
		})
		msg := googleRealtimeDialogJSON(comdb.SpeechRealTimeAI, assistText, now)
		if err := m.db.SaveDialog(rs.dialogID, msg); err != nil {
			//logger.Warn("saveGoogleRealtimeTranscript: ошибка сохранения ответа ассистента: %v", err, rs.userID)
		}
		//logger.Debug("saveGoogleRealtimeTranscript: assistant len=%d dialogID=%d", len(assistText), rs.dialogID, rs.userID)
	}
}

// googleRealtimeDialogJSON формирует JSON в формате endpoint.Message для сохранения в БД.
func googleRealtimeDialogJSON(creator comdb.CreatorType, text string, ts time.Time) []byte {
	msg := map[string]interface{}{
		"creator": creator,
		"message": model.AssistResponse{
			Message: text,
			Action:  model.Action{SendFiles: []model.File{}},
		},
		"timestamp": ts,
	}
	data, _ := json.Marshal(msg)
	return data
}

// ============================================================================
// Вспомогательные функции — извлечение файлов и определение типа
// ============================================================================

// googleExtractFilesForRealtime пытается извлечь файлы из результата MCP-инструмента.
// Аналог extractFilesForRealtime из openai пакета.
func googleExtractFilesForRealtime(rawResult string) (files []map[string]interface{}, voiceConfirm string) {
	raw := strings.TrimSpace(rawResult)
	if raw == "" {
		return nil, ""
	}

	// Попытка 1: объект с "url"/"Url" → один файл
	if strings.HasPrefix(raw, "{") {
		var r map[string]interface{}
		if err := json.Unmarshal([]byte(raw), &r); err == nil {
			url, _ := r["url"].(string)
			if url == "" {
				url, _ = r["Url"].(string)
			}
			if url != "" && googleIsFileURL(url) {
				fileName, _ := r["file_name"].(string)
				if fileName == "" {
					fileName = filepath.Base(url)
				}
				fileType, _ := r["type"].(string)
				if fileType == "" {
					fileType = googleRealtimeFileType(url)
				}
				files = []map[string]interface{}{{
					"type": fileType, "Url": url, "file_name": fileName, "caption": "",
				}}
				voiceConfirm = fmt.Sprintf(`{"status":"ok","file_name":%q,"type":%q}`, fileName, fileType)
				return
			}
		}
	}

	// Попытка 2: массив строк-URL (прямой или обёрнутый в {"output":"[...]"})
	var urlList []string
	if strings.HasPrefix(raw, "[") {
		_ = json.Unmarshal([]byte(raw), &urlList)
	} else if strings.HasPrefix(raw, "{") {
		var wrapper map[string]interface{}
		if err := json.Unmarshal([]byte(raw), &wrapper); err == nil {
			if outputStr, ok := wrapper["output"].(string); ok {
				_ = json.Unmarshal([]byte(outputStr), &urlList)
			}
		}
	}

	if len(urlList) == 0 {
		return nil, ""
	}

	nameParts := make([]string, 0, len(urlList))
	for _, u := range urlList {
		if !googleIsFileURL(u) {
			continue
		}
		fileName := filepath.Base(u)
		fileType := googleRealtimeFileType(u)
		files = append(files, map[string]interface{}{
			"type": fileType, "Url": u, "file_name": fileName, "caption": "",
		})
		nameParts = append(nameParts, fmt.Sprintf("%q", fileName))
	}
	if len(files) == 0 {
		return nil, ""
	}
	voiceConfirm = fmt.Sprintf(`{"status":"ok","count":%d,"file_names":[%s]}`,
		len(files), strings.Join(nameParts, ","))
	return
}

// googleIsFileURL возвращает true если строка похожа на URL с путём к файлу.
func googleIsFileURL(s string) bool {
	return (strings.HasPrefix(s, "http://") || strings.HasPrefix(s, "https://")) &&
		filepath.Ext(filepath.Base(s)) != ""
}

// googleRealtimeFileType определяет тип файла по расширению URL.
func googleRealtimeFileType(url string) string {
	ext := strings.ToLower(filepath.Ext(filepath.Base(url)))
	switch ext {
	case ".jpg", ".jpeg", ".png", ".gif", ".webp":
		return "photo"
	case ".mp4", ".mov", ".avi", ".webm":
		return "video"
	case ".mp3", ".wav", ".ogg", ".m4a":
		return "audio"
	default:
		return "doc"
	}
}
