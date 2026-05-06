package openai

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"github.com/ikermy/AiR_Common/pkg/comdb"
	"github.com/ikermy/AiR_Common/pkg/model"
)

// ============================================================================
// pumpFromOpenAI — читает события из OpenAI Realtime WS и маршрутизирует их:
//   - audio.delta     → rs.AudioOut (PCM16 для воспроизведения)
//   - управляющие     → rs.publishEvent() (fan-out подписчикам, Telegram не подписывается)
//   - VAD             → rs.DrainPlayback (сигнал callAudioBridge сбросить очередь)
// ============================================================================

func (m *OpenAIModel) pumpFromOpenAI(rs *RealtimeSession) {
	defer func() {
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

	// audioDeltaCount: счётчик audio.delta для текущего response — сбрасывается при response.done
	var audioDeltaCount int

	// watchdog детектирует зависший response на двух стадиях:
	//   1) после response.created — нет audio.delta за 3s → response.cancel
	//   2) после response.audio.done — нет response.done за 2s → response.cancel
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
					//logger.Warn("pumpFromOpenAI: watchdog panic: %v respId=%d", r, rs.respId, rs.userId)
				}
			}()
			// Если контекст уже отменён (звонок завершён вручную) — не действуем
			if rs.ctx.Err() != nil {
				return
			}
			//logger.Warn("pumpFromOpenAI: watchdog level 1 — %s, отправляем response.cancel respId=%d", reason, rs.respId, rs.userId)
			rs.IsGenerating.Store(false)
			if err := rs.writeJSON(map[string]interface{}{"type": "response.cancel"}); err != nil {
				//logger.Warn("pumpFromOpenAI: watchdog response.cancel error: %v respId=%d", err, rs.respId, rs.userId)
			}

			// Запускаем второй уровень таймаута: если response.cancel не сработал за 2s → завершаем сессию
			watchdogPanicTimer = time.AfterFunc(2*time.Second, func() {
				defer func() {
					if r := recover(); r != nil {
						//logger.Warn("pumpFromOpenAI: watchdog panic timer panic: %v respId=%d", r, rs.respId, rs.userId)
					}
				}()
				// Если контекст уже отменён (звонок завершён вручную) — не действуем
				if rs.ctx.Err() != nil {
					return
				}
				//logger.Error("pumpFromOpenAI: watchdog level 2 CRITICAL — модель не отвечает после response.cancel, завершаем сессию respId=%d", rs.respId, rs.userId)

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

	//logger.Info("pumpFromOpenAI: горутина запущена respId=%d dialogID=%d", rs.respId, rs.dialogID, rs.userId)

	for {
		select {
		case <-rs.ctx.Done():
			return
		default:
		}

		_, msg, err := rs.openaiConn.ReadMessage()
		if err != nil {
			if !strings.Contains(err.Error(), "use of closed network connection") &&
				!strings.Contains(err.Error(), "websocket: close") {
				//logger.Error("pumpFromOpenAI: ошибка чтения WS respId=%d: %v", rs.respId, err, rs.userId)
				rs.publishEvent(RealtimeEvent{Type: "error", Text: err.Error(), Err: err})
			}
			return
		}

		var event map[string]interface{}
		if err := json.Unmarshal(msg, &event); err != nil {
			//logger.Warn("pumpFromOpenAI: ошибка парсинга события: %v raw=%s", err, string(msg), rs.userId)
			continue
		}

		eventType, _ := event["type"].(string)

		switch eventType {

		// ── Аудио-дельта от ассистента ────────────────────────────────────────
		case "response.audio.delta":
			delta, _ := event["delta"].(string)
			if delta == "" {
				continue
			}
			pcm16, err := base64.StdEncoding.DecodeString(delta)
			if err != nil {
				//logger.Warn("pumpFromOpenAI: ошибка декодирования audio delta: %v", err, rs.userId)
				continue
			}
			audioDeltaCount++
			//if audioDeltaCount == 1 {
			//	stopWatchdog()
			//	logger.Debug("pumpFromOpenAI: первая audio.delta respId=%d", rs.respId, rs.userId)
			//}
			select {
			case rs.AudioOut <- pcm16:
			case <-rs.ctx.Done():
				return
			default:
				//logger.Warn("pumpFromOpenAI: AudioOut переполнен — дроп delta #%d len=%d respId=%d",
				//	audioDeltaCount, len(pcm16), rs.respId, rs.userId)
			}

		// ── Транскрипция ответа ассистента (дельта) ──────────────────────────
		case "response.audio_transcript.delta":
			delta, _ := event["delta"].(string)
			if delta != "" {
				rs.publishEvent(RealtimeEvent{Type: "transcript_delta", Text: delta})
			}

		// ── Транскрипция ответа ассистента (финальная) ───────────────────────
		case "response.audio_transcript.done":
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
		//	errObj, _ := event["error"].(map[string]interface{})
		//	logger.Warn("pumpFromOpenAI: user transcription failed respId=%d err=%v", rs.respId, errObj, rs.userId)

		// ── VAD: речь обнаружена ──────────────────────────────────────────────
		case "input_audio_buffer.speech_started":
			select {
			case rs.DrainPlayback <- struct{}{}:
			default:
			}
			//logger.Debug("pumpFromOpenAI: VAD speech_started respId=%d", rs.respId, rs.userId)

		// ── VAD: речь остановлена ─────────────────────────────────────────────
		case "input_audio_buffer.speech_stopped":
			//logger.Debug("pumpFromOpenAI: VAD speech_stopped respId=%d", rs.respId, rs.userId)

		case "input_audio_buffer.committed",
			"conversation.item.created",
			"response.content_part.added",
			"response.content_part.done":
			// служебные, не логируем

		// ── Ответ создаётся ───────────────────────────────────────────────────
		case "response.created":
			rs.IsGenerating.Store(true)
			fireWatchdog(3*time.Second, "нет audio.delta 3s после response.created")
			//logger.Debug("pumpFromOpenAI: response.created respId=%d", rs.respId, rs.userId)

		// ── Output item добавлен ─────────────────────────────────────────────
		//case "response.output_item.added":
		//	item, _ := event["item"].(map[string]interface{})
		//	itemType, _ := item["type"].(string)
		//	logger.Debug("pumpFromOpenAI: output_item.added type=%s respId=%d", itemType, rs.respId, rs.userId)

		// ── Ответ ассистента завершён ─────────────────────────────────────────
		case "response.done":
			resp, _ := event["response"].(map[string]interface{})
			status, _ := resp["status"].(string)
			usage, _ := resp["usage"].(map[string]interface{})

			if status != "completed" {
				//statusDetails, _ := resp["status_details"].(map[string]interface{})
				//reason, _ := statusDetails["reason"].(string)
				//logger.Debug("pumpFromOpenAI: response.done status=%s reason=%s audioDelta=%d respId=%d",
				//	status, reason, audioDeltaCount, rs.respId, rs.userId)
				audioDeltaCount = 0
				pendingFuncResults = pendingFuncResults[:0]
				rs.IsGenerating.Store(false)
				stopWatchdog()
				rs.publishEvent(RealtimeEvent{Type: "response_done"})
				continue
			}

			//logger.Debug("pumpFromOpenAI: response.done status=completed audioDelta=%d respId=%d",
			//	audioDeltaCount, rs.respId, rs.userId)
			audioDeltaCount = 0
			stopWatchdog()

			if len(pendingFuncResults) > 0 {
				for _, fr := range pendingFuncResults {
					if err := rs.writeJSON(map[string]interface{}{
						"type": "conversation.item.create",
						"item": map[string]interface{}{
							"type":    "function_call_output",
							"call_id": fr.callID,
							"output":  fr.result,
						},
					}); err != nil {
						//logger.Warn("response.done: ошибка отправки function_call_output callId=%s: %v",
						//	fr.callID, err, rs.userId)
					}
				}
				//logger.Debug("response.done: отправлено func results=%d → response.create respId=%d",
				//	len(pendingFuncResults), rs.respId, rs.userId)
				pendingFuncResults = pendingFuncResults[:0]

				// ВАЖНО: Realtime API не принимает temperature, max_output_tokens,
				// response_format в теле response.create — вызывают fatal error unknown_parameter.
				// Эти параметры задаются только через URL при DialRealtimeSession.
				if err := rs.writeJSON(map[string]interface{}{
					"type": "response.create",
					"response": map[string]interface{}{
						"modalities": []string{"text", "audio"},
					},
				}); err != nil {
					//logger.Warn("response.done: ошибка отправки response.create: %v", err, rs.userId)
				}
			}

			var assistItemId string
			if output, ok := resp["output"].([]interface{}); ok {
				if len(output) == 0 {
					//logger.Warn("pumpFromOpenAI: response.done completed с пустым output respId=%d", rs.respId, rs.userId)
				}
				for _, outRaw := range output {
					out, ok := outRaw.(map[string]interface{})
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
				if usageJSON, err := json.Marshal(map[string]interface{}{"type": "token_usage", "usage": usage}); err == nil {
					rs.publishEvent(RealtimeEvent{Type: "token_usage", Data: usageJSON})
				}
			}

			files := pendingFiles
			pendingFiles = nil
			if len(files) > 0 {
				//logger.Info("pumpFromOpenAI: response_done с файлами count=%d respId=%d", len(files), rs.respId, rs.userId)
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
			item, ok := event["item"].(map[string]interface{})
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
					//logger.Info("send_file_to_user: добавлен файл %s respId=%d", params.FileName, rs.respId, rs.userId)
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

			rawResult := m.actionHandler.RunAction(rs.ctx, name, args, 1)

			modelResult := rawResult
			if name == "get_s3_files" {
				// modelResult = rawResult (полный JSON с URL для модели).
				// Файлы в pendingFiles НЕ добавляем — только через send_file_to_user.
			} else if name == "create_file" || name == "save_image_data" {
				if extractedFiles, voiceConfirm := extractFilesForRealtime(name, rawResult); len(extractedFiles) > 0 {
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
			}

			rs.publishEvent(RealtimeEvent{
				Type: "function_result",
				Text: fmt.Sprintf(`{"call_id":%q,"name":%q,"result":%s}`, callID, name, rawResult),
			})

			pendingFuncResults = append(pendingFuncResults, funcResult{callID: callID, result: modelResult})
			delete(funcArgs, callID)

		// ── Сессия создана/обновлена ──────────────────────────────────────────
		//case "session.created":
		//	sess, _ := event["session"].(map[string]interface{})
		//	modelName, _ := sess["model"].(string)
		//	logger.Info("pumpFromOpenAI: сессия создана model=%s respId=%d", modelName, rs.respId, rs.userId)

		case "session.updated":
			//sess, _ := event["session"].(map[string]interface{})
			//modalities, _ := sess["modalities"].([]interface{})
			//voice, _ := sess["voice"].(string)
			//outFmt, _ := sess["output_audio_format"].(string)
			//logger.Debug("pumpFromOpenAI: session.updated modalities=%v voice=%s outFmt=%s respId=%d",
			//	modalities, voice, outFmt, rs.respId, rs.userId)
			// Отправляем приветствие сразу после того как сессия настроена
			m.sendInitialGreeting(rs)

		// ── Аудио завершено — ждём response.done ─────────────────────────────
		case "response.audio.done":
			audioDeltaCount = 0
			fireWatchdog(2*time.Second, "нет response.done 2s после response.audio.done")

		// ── Rate limits ───────────────────────────────────────────────────────
		//case "rate_limits.updated":
		//	if rl, ok := event["rate_limits"].([]interface{}); ok {
		//		for _, r := range rl {
		//			rm, _ := r.(map[string]interface{})
		//			name, _ := rm["name"].(string)
		//			remaining, _ := rm["remaining"].(float64)
		//			resetSec, _ := rm["reset_seconds"].(float64)
		//			if remaining < 100 || resetSec > 5 {
		//				logger.Warn("pumpFromOpenAI: rate_limit low name=%s remaining=%.0f reset=%.1fs respId=%d",
		//					name, remaining, resetSec, rs.respId, rs.userId)
		//			}
		//		}
		//	}

		// ── Ошибка от OpenAI ──────────────────────────────────────────────────
		case "error":
			errObj, _ := event["error"].(map[string]interface{})
			errMsg := "unknown realtime error"
			errCode := ""
			//errParam := ""
			if errObj != nil {
				if m, ok := errObj["message"].(string); ok {
					errMsg = m
				}
				errCode, _ = errObj["code"].(string)
				//errParam, _ = errObj["param"].(string)
			}
			recoverableCodes := map[string]bool{
				"conversation_already_has_active_response": true,
				"response_cancel_not_active":               true,
				"input_audio_buffer_commit_empty":          true,
			}
			if recoverableCodes[errCode] {
				//logger.Warn("pumpFromOpenAI: recoverable error respId=%d code=%s msg=%s",
				//	rs.respId, errCode, errMsg, rs.userId)
				continue
			}
			//logger.Error("pumpFromOpenAI: fatal error respId=%d code=%s param=%s msg=%s",
			//	rs.respId, errCode, errParam, errMsg, rs.userId)
			rs.publishEvent(RealtimeEvent{Type: "error", Text: errMsg, Err: fmt.Errorf("%s", errMsg)})
			return

		default:
			if watchdogTimer != nil && audioDeltaCount == 0 {
				//logger.Warn("pumpFromOpenAI: [STALLED] unknown event type=%s respId=%d raw=%s",
				//	eventType, rs.respId, string(msg), rs.userId)
				//} else {
				//	logger.Debug("pumpFromOpenAI: unknown event type=%s respId=%d", eventType, rs.respId, rs.userId)
			}
		}
	}
}

// ============================================================================
// pumpToOpenAI — читает PCM16 из AudioIn и отправляет input_audio_buffer.append.
// Накапливает 100ms чанки (4800 байт @ 24kHz PCM16) перед отправкой.
// ============================================================================

func (m *OpenAIModel) pumpToOpenAI(rs *RealtimeSession) {
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
		if err := rs.writeJSON(map[string]interface{}{
			"type":  "input_audio_buffer.append",
			"audio": encoded,
		}); err != nil {
			if !strings.Contains(err.Error(), "use of closed network connection") {
				//logger.Error("pumpToOpenAI: ошибка отправки audio append respId=%d: %v",
				//	rs.respId, err, rs.userId)
			}
		}
	}

	for {
		select {
		case <-rs.ctx.Done():
			flush()
			//logger.Debug("pumpToOpenAI: завершён sentChunks=%d respId=%d", sentChunks, rs.respId, rs.userId)
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

func (m *OpenAIModel) saveRealtimeTranscript(rs *RealtimeSession, userText, assistText string) {
	dialogID := rs.dialogID
	//userId := rs.userId
	now := time.Now()

	if userText != "" {
		m.addMessageToCache(dialogID, ChatMessage{Role: "user", Content: userText})
		msg := realtimeDialogJSON(comdb.SpeechRealTimeUser, userText, now)
		if err := m.db.SaveDialog(dialogID, msg); err != nil {
			//logger.Warn("saveRealtimeTranscript: ошибка сохранения реплики пользователя: %v", err, userId)
		}
		//logger.Debug("saveRealtimeTranscript: user len=%d dialogID=%d", len(userText), dialogID, userId)
	}

	if assistText != "" {
		m.addMessageToCache(dialogID, ChatMessage{Role: "assistant", Content: assistText})

		msg := realtimeDialogJSON(comdb.SpeechRealTimeAI, assistText, now)
		if err := m.db.SaveDialog(dialogID, msg); err != nil {
			//logger.Warn("saveRealtimeTranscript: ошибка сохранения ответа ассистента: %v", err, userId)
		}
		//logger.Debug("saveRealtimeTranscript: assistant len=%d dialogID=%d", len(assistText), dialogID, userId)
	}
}

// realtimeDialogJSON формирует JSON в формате endpoint.Message для сохранения в БД.
func realtimeDialogJSON(creator comdb.CreatorType, text string, ts time.Time) []byte {
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
