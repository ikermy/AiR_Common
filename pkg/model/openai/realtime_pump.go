package openai

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"github.com/gorilla/websocket"
	"github.com/ikermy/AiR_Common/pkg/comdb"
	"github.com/ikermy/AiR_Common/pkg/model"
)

// ============================================================================
// pumpFromOpenAI — читает события из OpenAI Realtime WS и маршрутизирует их:
//   - audio.delta     → rs.AudioOut (PCM16 для воспроизведения)
//   - управляющие     → rs.publishEvent() (fan-out подписчикам, Telegram не подписывается)
//   - VAD             → rs.DrainPlayback (сигнал callAudioBridge сбросить очередь)
// ============================================================================

func (m *Model) pumpFromOpenAI(rs *RealtimeSession) {
	defer func() {
		//logger.Debug("[pumpFromOpenAI] горутина ЗАВЕРШАЕТСЯ respId=%d", rs.respId)
		rs.publishEvent(RealtimeEvent{Type: "error", Text: "realtime session closed", Err: fmt.Errorf("session closed")})
		rs.cancel()
	}()

	funcArgs := make(map[string]string)

	var pendingFiles []model.File
	type funcResult struct {
		callID string
		result string
	}
	var pendingFuncResults []funcResult

	// audioDeltaCount: счётчик output_audio.delta для текущего response — сбрасывается при response.done
	var audioDeltaCount int

	// watchdog детектирует зависший response на двух стадиях:
	//   1) после response.created — нет output_audio.delta за 3s → response.cancel
	//   2) после response.output_audio.done — нет response.done за 2s → response.cancel
	//   3) если response.cancel не помог (модель совсем не отвечает) — вызываем OnDisconnect callback
	//      для завершения звонка (Telegram) или отключения WebSocket (API клиента)
	var watchdogTimer *time.Timer
	var watchdogPanicTimer *time.Timer // второй уровень: если cancel не сработал

	stopWatchdog := func() {
		if watchdogTimer != nil {
			watchdogTimer.Stop()
			watchdogTimer = nil
		}
	}
	stopWatchdogPanic := func() {
		if watchdogPanicTimer != nil {
			watchdogPanicTimer.Stop()
			watchdogPanicTimer = nil
		}
	}

	fireWatchdog := func(after time.Duration, reason string) {
		stopWatchdog()
		stopWatchdogPanic()

		watchdogTimer = time.AfterFunc(after, func() {
			defer func() {
				if r := recover(); r != nil {
					//logger.Warn("pumpFromOpenAI: watchdog panic: %v respId=%d", r, rs.respId, rs.userID)
				}
			}()
			// Если контекст уже отменён (звонок завершён вручную) — не действуем
			if rs.ctx.Err() != nil {
				return
			}
			//logger.Warn("pumpFromOpenAI: watchdog level 1 — %s, отправляем response.cancel respId=%d", reason, rs.respId, rs.userID)
			rs.IsGenerating.Store(false)
			if err := rs.writeJSON(map[string]any{"type": "response.cancel"}); err != nil {
				//logger.Warn("pumpFromOpenAI: watchdog response.cancel error: %v respId=%d", err, rs.respId, rs.userID)
			}

			// Запускаем второй уровень таймаута: если response.cancel не сработал за 2s → завершаем сессию
			watchdogPanicTimer = time.AfterFunc(2*time.Second, func() {
				defer func() {
					if r := recover(); r != nil {
						//logger.Warn("pumpFromOpenAI: watchdog panic timer panic: %v respId=%d", r, rs.respId, rs.userID)
					}
				}()
				// Если контекст уже отменён (звонок завершён вручную) — не действуем
				if rs.ctx.Err() != nil {
					return
				}
				//logger.Error("pumpFromOpenAI: watchdog level 2 CRITICAL — модель не отвечает после response.cancel, завершаем сессию respId=%d", rs.respId, rs.userID)

				// Вызываем OnDisconnect callback для завершения звонка (Telegram) или отключения клиента (API)
				// Защита от двойного вызова через флаг onDisconnectCalled
				if rs.OnDisconnect != nil && !rs.onDisconnectCalled.Swap(true) {
					rs.OnDisconnect(rs.respId)
				}

				// Отмена контекста разблокирует pumpFromOpenAI и pumpToOpenAI
				rs.cancel()
			})
		})
	}

	defer func() {
		stopWatchdog()
		stopWatchdogPanic()
	}()

	//logger.Debug("[pumpFromOpenAI] старт горутины respId=%d dialogID=%d", rs.respId, rs.dialogID)

	for {
		select {
		case <-rs.ctx.Done():
			//logger.Debug("[pumpFromOpenAI] ctx.Done() respId=%d cause=%v", rs.respId, rs.ctx.Err())
			return
		default:
		}

		_, msg, err := rs.openaiConn.ReadMessage()
		if err != nil {
			var closeErr *websocket.CloseError
			if errors.As(err, &closeErr) {
				// OpenAI закрыл сессию с кодом: публикуем полную причину
				text := fmt.Sprintf("openai close code=%d: %s", closeErr.Code, closeErr.Text)
				//logger.Debug("[pumpFromOpenAI] WS CLOSE respId=%d code=%d reason=%q", rs.respId, closeErr.Code, closeErr.Text)
				rs.publishEvent(RealtimeEvent{Type: "error", Text: text, Err: err})
			} else if !strings.Contains(err.Error(), "use of closed network connection") {
				//logger.Debug("[pumpFromOpenAI] ReadMessage error respId=%d: %v", rs.respId, err)
				rs.publishEvent(RealtimeEvent{Type: "error", Text: err.Error(), Err: err})
			}
			return
		}

		var event map[string]any
		if err := json.Unmarshal(msg, &event); err != nil {
			//logger.Warn("pumpFromOpenAI: ошибка парсинга события: %v raw=%s", err, string(msg), rs.userID)
			continue
		}

		eventType, _ := event["type"].(string)

		switch eventType {

		// ── Аудио-дельта от ассистента (GA API: response.output_audio.delta) ──
		case "response.output_audio.delta":
			delta, _ := event["delta"].(string)
			if delta == "" {
				continue
			}
			pcm16, err := base64.StdEncoding.DecodeString(delta)
			if err != nil {
				//logger.Warn("pumpFromOpenAI: ошибка декодирования audio delta: %v", err, rs.userID)
				continue
			}
			audioDeltaCount++
			//if audioDeltaCount == 1 {
			//	stopWatchdog()
			//	logger.Debug("pumpFromOpenAI: первая audio.delta respId=%d", rs.respId, rs.userID)
			//}
			select {
			case rs.AudioOut <- pcm16:
			case <-rs.ctx.Done():
				return
			default:
				//logger.Warn("pumpFromOpenAI: AudioOut переполнен — дроп delta #%d len=%d respId=%d",
				//	audioDeltaCount, len(pcm16), rs.respId, rs.userID)
			}

		// ── Транскрипция ответа ассистента (дельта) (GA API: response.output_audio_transcript.delta) ──
		case "response.output_audio_transcript.delta":
			delta, _ := event["delta"].(string)
			if delta != "" {
				rs.publishEvent(RealtimeEvent{Type: "transcript_delta", Text: delta})
			}

		// ── Транскрипция ответа ассистента (финальная) (GA API: response.output_audio_transcript.done) ──
		case "response.output_audio_transcript.done":
			transcript, _ := event["transcript"].(string)
			itemId, _ := event["item_id"].(string)
			if itemId != "" && transcript != "" {
				rs.assistTranscripts.Store(itemId, transcript)
			}

		// ── Транскрипция ввода пользователя (финальная) ──────────────────────
		case "conversation.item.input_audio_transcription.completed":
			transcript, _ := event["transcript"].(string)
			itemId, _ := event["item_id"].(string)
			if itemId != "" && transcript != "" {
				rs.userTranscripts.Store(itemId, transcript)
				m.saveRealtimeTranscript(rs, transcript, "")
			}
			rs.publishEvent(RealtimeEvent{Type: "input_transcript_done", Text: transcript})

		// ── Транскрипция пользователя: ошибка ────────────────────────────────
		//case "conversation.item.input_audio_transcription.failed":
		//	errObj, _ := event["error"].(map[string]any)
		//	logger.Warn("pumpFromOpenAI: user transcription failed respId=%d err=%v", rs.respId, errObj, rs.userID)

		// ── VAD: речь обнаружена ──────────────────────────────────────────────
		case "input_audio_buffer.speech_started":
			select {
			case rs.DrainPlayback <- struct{}{}:
			default:
			}
			//logger.Debug("pumpFromOpenAI: VAD speech_started respId=%d", rs.respId, rs.userID)

		// ── VAD: речь остановлена ─────────────────────────────────────────────
		case "input_audio_buffer.speech_stopped":
			//logger.Debug("pumpFromOpenAI: VAD speech_stopped respId=%d", rs.respId, rs.userID)

		case "input_audio_buffer.committed",
			"conversation.item.created",
			"response.content_part.added",
			"response.content_part.done":
			// служебные, не логируем

		// ── Ответ создаётся ───────────────────────────────────────────────────
		case "response.created":
			rs.IsGenerating.Store(true)
			fireWatchdog(3*time.Second, "нет audio.delta 3s после response.created")
			//logger.Debug("pumpFromOpenAI: response.created respId=%d", rs.respId, rs.userID)

		// ── Output item добавлен ─────────────────────────────────────────────
		//case "response.output_item.added":
		//	item, _ := event["item"].(map[string]any)
		//	itemType, _ := item["type"].(string)
		//	logger.Debug("pumpFromOpenAI: output_item.added type=%s respId=%d", itemType, rs.respId, rs.userID)

		// ── Ответ ассистента завершён ─────────────────────────────────────────
		case "response.done":
			resp, _ := event["response"].(map[string]any)
			status, _ := resp["status"].(string)
			usage, _ := resp["usage"].(map[string]any)

			if status != "completed" {
				// status="cancelled" → пользователь перебил модель (barge-in).
				// Публикуем "interrupted" чтобы хэндлер мог отличить от нормального завершения
				// и немедленно остановить воспроизведение на клиенте.
				audioDeltaCount = 0
				pendingFuncResults = pendingFuncResults[:0]
				rs.IsGenerating.Store(false)
				stopWatchdog()
				rs.publishEvent(RealtimeEvent{Type: "interrupted"})
				continue
			}

			//logger.Debug("pumpFromOpenAI: response.done status=completed audioDelta=%d respId=%d",
			//	audioDeltaCount, rs.respId, rs.userID)
			audioDeltaCount = 0
			stopWatchdog()

			if len(pendingFuncResults) > 0 {
				for _, fr := range pendingFuncResults {
					if err := rs.writeJSON(map[string]any{
						"type": "conversation.item.create",
						"item": map[string]any{
							"type":    "function_call_output",
							"call_id": fr.callID,
							"output":  fr.result,
						},
					}); err != nil {
						//logger.Warn("response.done: ошибка отправки function_call_output callId=%s: %v",
						//	fr.callID, err, rs.userID)
					}
				}
				//logger.Debug("response.done: отправлено func results=%d → response.create respId=%d",
				//	len(pendingFuncResults), rs.respId, rs.userID)
				pendingFuncResults = pendingFuncResults[:0]

				if err := rs.writeJSON(map[string]any{
					"type":     "response.create",
					"response": map[string]any{},
				}); err != nil {
					//logger.Warn("response.done: ошибка отправки response.create: %v", err, rs.userID)
				}
			}

			var assistItemId string
			if output, ok := resp["output"].([]any); ok {
				if len(output) == 0 {
					//logger.Warn("pumpFromOpenAI: response.done completed с пустым output respId=%d", rs.respId, rs.userID)
				}
				for _, outRaw := range output {
					out, ok := outRaw.(map[string]any)
					if !ok {
						continue
					}
					if role, _ := out["role"].(string); role == "assistant" {
						assistItemId, _ = out["id"].(string)
						break
					}
				}
			}

			// Получаем транскрипцию ассистента из sync.Map и удаляем её
			assistTextVal, _ := rs.assistTranscripts.LoadAndDelete(assistItemId)
			assistText := ""
			if assistTextVal != nil {
				assistText = assistTextVal.(string)
			}

			if assistText != "" {
				m.saveRealtimeTranscript(rs, "", assistText)
			}

			if usage != nil {
				if usageJSON, err := json.Marshal(map[string]any{"type": "token_usage", "usage": usage}); err == nil {
					rs.publishEvent(RealtimeEvent{Type: "token_usage", Data: usageJSON})
				}
			}

			files := pendingFiles
			pendingFiles = nil
			if len(files) > 0 {
				//logger.Info("pumpFromOpenAI: response_done с файлами count=%d respId=%d", len(files), rs.respId, rs.userID)
			}
			rs.IsGenerating.Store(false)
			rs.publishEvent(RealtimeEvent{Type: "response_done", Files: files})

		// ── Function call: накопление аргументов ─────────────────────────────
		case "response.function_call_arguments.delta":
			callID, _ := event["call_id"].(string)
			delta, _ := event["delta"].(string)
			if callID != "" {
				funcArgs[callID] += delta
			}

		case "response.function_call_arguments.done":
			// аргументы уже накоплены в funcArgs через .delta

		// ── Function call: output item готов ─────────────────────────────────
		case "response.output_item.done":
			item, ok := event["item"].(map[string]any)
			if !ok {
				continue
			}
			if itemType, _ := item["type"].(string); itemType != "function_call" {
				continue
			}
			callID, _ := item["call_id"].(string)
			name, _ := item["name"].(string)
			args, _ := item["arguments"].(string)
			if args == "" {
				args = funcArgs[callID]
			}

			// send_file_to_user — синтетический tool, обрабатывается локально,
			// в UniversalActionHandler его нет, RunAction вызывать не нужно.
			if name == "send_file_to_user" {
				var params struct {
					URL      string `json:"url"`
					FileName string `json:"file_name"`
				}
				localResult := ""
				if err := json.Unmarshal([]byte(args), &params); err == nil && params.URL != "" {
					if params.FileName == "" {
						params.FileName = filepath.Base(params.URL)
					}
					fileType := realtimeFileType(params.URL)
					pendingFiles = append(pendingFiles, model.File{
						Type:     model.FileType(fileType),
						URL:      params.URL,
						FileName: params.FileName,
					})
					localResult = fmt.Sprintf(`{"status":"ok","file_name":%q,"type":%q}`, params.FileName, fileType)
					//logger.Info("send_file_to_user: добавлен файл %s respId=%d", params.FileName, rs.respId, rs.userID)
				} else {
					localResult = `{"status":"error","message":"invalid parameters"}`
				}
				rs.publishEvent(RealtimeEvent{
					Type: "function_result",
					Text: fmt.Sprintf(`{"call_id":%q,"name":%q,"result":%s}`, callID, name, localResult),
				})
				pendingFuncResults = append(pendingFuncResults, funcResult{callID: callID, result: localResult})
				delete(funcArgs, callID)
				continue
			}

			rawResult := m.actionHandler.RunAction(rs.ctx, name, args, 1, rs.userID)

			// Пытаемся извлечь файлы из результата любого инструмента по структуре JSON.
			// Нет привязки к именам функций — работает для любых MCP-инструментов,
			// возвращающих URL файлов.
			modelResult := rawResult
			if extractedFiles, voiceConfirm := extractFilesForRealtime(rawResult); len(extractedFiles) > 0 {
				for _, f := range extractedFiles {
					fileType, _ := f["type"].(string)
					fileURL, _ := f["Url"].(string)
					fileName, _ := f["file_name"].(string)
					caption, _ := f["caption"].(string)
					pendingFiles = append(pendingFiles, model.File{
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

			rs.publishEvent(RealtimeEvent{
				Type: "function_result",
				Text: fmt.Sprintf(`{"call_id":%q,"name":%q,"result":%s}`, callID, name, rawResult),
			})

			pendingFuncResults = append(pendingFuncResults, funcResult{callID: callID, result: modelResult})
			delete(funcArgs, callID)

		// ── Сессия создана/обновлена ──────────────────────────────────────────
		//case "session.created":
		//	sess, _ := event["session"].(map[string]any)
		//	modelName, _ := sess["model"].(string)
		//	logger.Info("pumpFromOpenAI: сессия создана model=%s respId=%d", modelName, rs.respId, rs.userID)

		case "session.updated":
			//sess, _ := event["session"].(map[string]any)
			//modalities, _ := sess["modalities"].([]any)
			//voice, _ := sess["voice"].(string)
			//outFmt, _ := sess["output_audio_format"].(string)
			//logger.Debug("pumpFromOpenAI: session.updated modalities=%v voice=%s outFmt=%s respId=%d",
			//	modalities, voice, outFmt, rs.respId, rs.userID)
			// Отправляем приветствие сразу после того как сессия настроена
			m.sendInitialGreeting(rs)

		// ── Аудио завершено — ждём response.done (GA API: response.output_audio.done) ──
		case "response.output_audio.done":
			audioDeltaCount = 0
			fireWatchdog(2*time.Second, "нет response.done 2s после response.output_audio.done")

		// ── Rate limits ───────────────────────────────────────────────────────
		//case "rate_limits.updated":
		//	if rl, ok := event["rate_limits"].([]any); ok {
		//		for _, r := range rl {
		//			rm, _ := r.(map[string]any)
		//			name, _ := rm["name"].(string)
		//			remaining, _ := rm["remaining"].(float64)
		//			resetSec, _ := rm["reset_seconds"].(float64)
		//			if remaining < 100 || resetSec > 5 {
		//				logger.Warn("pumpFromOpenAI: rate_limit low name=%s remaining=%.0f reset=%.1fs respId=%d",
		//					name, remaining, resetSec, rs.respId, rs.userID)
		//			}
		//		}
		//	}

		// ── Ошибка от OpenAI ──────────────────────────────────────────────────
		case "error":
			errObj, _ := event["error"].(map[string]any)
			errMsg := "unknown realtime error"
			errCode := ""
			if errObj != nil {
				if m, ok := errObj["message"].(string); ok {
					errMsg = m
				}
				errCode, _ = errObj["code"].(string)
			}
			recoverableCodes := map[string]bool{
				"conversation_already_has_active_response": true,
				"response_cancel_not_active":               true,
				"input_audio_buffer_commit_empty":          true,
			}
			if recoverableCodes[errCode] {
				continue
			}
			// FATAL: логируем всегда чтобы видеть причину обрыва сессии
			//logger.Debug("[pumpFromOpenAI] FATAL ERROR from OpenAI respId=%d code=%q msg=%q", rs.respId, errCode, errMsg)
			rs.publishEvent(RealtimeEvent{Type: "error", Text: errMsg, Err: fmt.Errorf("%s", errMsg)})
			return

		default:
			if watchdogTimer != nil && audioDeltaCount == 0 {
				//logger.Warn("pumpFromOpenAI: [STALLED] unknown event type=%s respId=%d raw=%s",
				//	eventType, rs.respId, string(msg), rs.userID)
				//} else {
				//	logger.Debug("pumpFromOpenAI: unknown event type=%s respId=%d", eventType, rs.respId, rs.userID)
			}
		}
	}
}

// ============================================================================
// pumpToOpenAI — читает PCM16 из AudioIn и отправляет input_audio_buffer.append.
// Накапливает 100ms чанки (4800 байт @ 24kHz PCM16) перед отправкой.
// ============================================================================

func (m *Model) pumpToOpenAI(rs *RealtimeSession) {
	var sentChunks int
	const accumulateBytes = 4800 // 100ms @ 24kHz PCM16
	var accumBuf []byte

	flush := func() {
		if len(accumBuf) == 0 {
			return
		}
		sentChunks++
		encoded := base64.StdEncoding.EncodeToString(accumBuf)
		accumBuf = accumBuf[:0]
		if err := rs.writeJSON(map[string]any{
			"type":  "input_audio_buffer.append",
			"audio": encoded,
		}); err != nil {
			if !strings.Contains(err.Error(), "use of closed network connection") {
				//logger.Error("pumpToOpenAI: ошибка отправки audio append respId=%d: %v",
				//	rs.respId, err, rs.userID)
			}
		}
	}

	for {
		select {
		case <-rs.ctx.Done():
			flush()
			//logger.Debug("pumpToOpenAI: завершён sentChunks=%d respId=%d", sentChunks, rs.respId, rs.userID)
			return
		case pcm16, ok := <-rs.AudioIn:
			if !ok {
				flush()
				return
			}
			if len(pcm16) == 0 {
				continue
			}
			accumBuf = append(accumBuf, pcm16...)
			if len(accumBuf) >= accumulateBytes {
				flush()
			}
		}
	}
}

// ============================================================================
// saveRealtimeTranscript — сохраняет транскрипцию в DialogCache и БД
// ============================================================================

func (m *Model) saveRealtimeTranscript(rs *RealtimeSession, userText, assistText string) {
	dialogID := rs.dialogID
	//userID := rs.userID
	now := time.Now()

	if userText != "" {
		m.addMessageToCache(dialogID, ChatMessage{Role: "user", Content: userText})
		msg := realtimeDialogJSON(comdb.SpeechRealTimeUser, userText, now)
		if err := m.db.SaveDialog(dialogID, msg); err != nil {
			//logger.Warn("saveRealtimeTranscript: ошибка сохранения реплики пользователя: %v", err, userID)
		}
		//logger.Debug("saveRealtimeTranscript: user len=%d dialogID=%d", len(userText), dialogID, userID)
	}

	if assistText != "" {
		m.addMessageToCache(dialogID, ChatMessage{Role: "assistant", Content: assistText})

		msg := realtimeDialogJSON(comdb.SpeechRealTimeAI, assistText, now)
		if err := m.db.SaveDialog(dialogID, msg); err != nil {
			//logger.Warn("saveRealtimeTranscript: ошибка сохранения ответа ассистента: %v", err, userID)
		}
		//logger.Debug("saveRealtimeTranscript: assistant len=%d dialogID=%d", len(assistText), dialogID, userID)
	}
}

// realtimeDialogJSON формирует JSON в формате endpoint.Message для сохранения в БД.
func realtimeDialogJSON(creator comdb.CreatorType, text string, ts time.Time) []byte {
	msg := map[string]any{
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
