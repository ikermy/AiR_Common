package openai

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"regexp"
	"strings"
)

// universalRealtimeSystemPrompt — базовый стиль голосового взаимодействия.
// Добавляется в начало RealtimeSystemPrompt перед промптом агента.
const universalRealtimeSystemPrompt = `# Voice Interaction Mode
You are operating in real-time voice mode.

# Response Style
- Give SHORT, NATURAL spoken answers — 1-3 sentences unless detail is explicitly requested.
- NEVER use markdown: no **, no #, no bullet lists, no code blocks.
- Spell out numbers naturally ("twenty three", not "23").
- Use natural transitions: "Sure, let me check that.", "Got it.", "One moment."

# Clarification
- If you need to confirm a name, number, or spelling — repeat it back before proceeding.
- If the user corrects you — acknowledge briefly and confirm the new value.

# Language
- Respond in the same language the user is speaking.

`

// textModePatterns — регулярные выражения для удаления text-mode артефактов из SystemPrompt.
var textModePatterns = []*regexp.Regexp{
	// JSON: target=false, operator=false (op=true if ask)
	regexp.MustCompile(`(?m)^JSON:.*\n?`),
	// Return: valid JSON
	regexp.MustCompile(`(?m)^Return: valid JSON.*\n?`),
	// send_files=[] (S3 only)
	regexp.MustCompile(`(?m)^send_files=\[].*\n?`),
	// target=true: ...
	regexp.MustCompile(`(?m)^target=true:.*\n?`),
	// Table data -> show in message text, NOT create files!
	regexp.MustCompile(`(?m)^Table data.*NOT create files!.*\n?`),
	// IMPORTANT: After calling functions ... DO NOT IGNORE!
	regexp.MustCompile(`(?m)^IMPORTANT: After calling functions.*\n?`),
	// UID=... Time: get_current_time(UID)
	regexp.MustCompile(`(?m)^UID=\d+\..*\n?`),
	// Tools: S3,Cal,Sheets,Web
	regexp.MustCompile(`(?m)^Tools: .*\n?`),
	// Sheets: CALL sheets_read_range(...) блок (многострочный)
	regexp.MustCompile(`(?ms)^Sheets: CALL.*?^\n`),
	// Cal: get_current_time → ... блок
	regexp.MustCompile(`(?m)^Cal: get_current_time.*\n?`),
	// INSTRUMENTS: блок
	regexp.MustCompile(`(?ms)^INSTRUMENTS:.*?(?:\n\n|$)`),
	// File creation: use create_file function!
	regexp.MustCompile(`(?m)^File creation:.*\n?`),
	// Python tool: ...
	regexp.MustCompile(`(?m)^Python tool:.*\n?`),
}

// buildRealtimeSystemPrompt строит голосовой промпт на основе уже готового config.SystemPrompt.
// Алгоритм:
//  1. Берём config.SystemPrompt (содержит промпт агента + text-mode инструкции)
//  2. Удаляем text-mode артефакты (JSON schema, send_files, target, UID строки и т.д.)
//  3. Добавляем universalRealtimeSystemPrompt сверху
//  4. Добавляем голосовые инструкции по активным инструментам
//
// Единственный источник истины — buildAgentConfiguration.
// buildRealtimeSystemPrompt только адаптирует его под голос.
func buildRealtimeSystemPrompt(config *OpenAIAgentConfig) string {
	// Шаг 1: берём SystemPrompt и вырезаем text-mode артефакты
	cleaned := config.SystemPrompt
	for _, re := range textModePatterns {
		cleaned = re.ReplaceAllString(cleaned, "")
	}
	// Убираем повторяющиеся пустые строки
	multiNewline := regexp.MustCompile(`\n{3,}`)
	cleaned = strings.TrimSpace(multiNewline.ReplaceAllString(cleaned, "\n\n"))

	var b strings.Builder

	// Шаг 2: голосовой стиль сверху
	b.WriteString(universalRealtimeSystemPrompt)

	// Шаг 3: голосовые инструкции по активным инструментам
	var toolLines []string
	toolLines = append(toolLines,
		"- Before any tool call, say ONE short sentence: \"One moment.\" Then call immediately.",
		"- After a tool returns — summarise in natural spoken language. DO NOT read raw JSON or URLs aloud.",
		"- NEVER say \"I can't check\" if a tool exists — CALL IT.",
		"- If a tool returns an error or unavailable — say so briefly and move on. Do NOT retry the same tool.",
	)
	if config.HasCalendar {
		toolLines = append(toolLines,
			"- ALWAYS call get_current_time BEFORE any calendar or date/time operation.",
		)
	} else {
		toolLines = append(toolLines,
			"- Call get_current_time ONLY when the user explicitly asks about current time or date.",
		)
	}
	if config.HasSheets {
		toolLines = append(toolLines,
			"- Sheets: ALWAYS call sheets_read_range to get data.",
			"- After reading table data — summarise it in spoken form. Do NOT read raw values aloud.",
		)
	}
	if config.S3 {
		toolLines = append(toolLines,
			"- Files: FIRST call get_s3_files to get the list with exact URLs. THEN call send_file_to_user using the EXACT URL from get_s3_files response — NEVER invent or modify URLs. Send only the file(s) the user asked for.",
		)
		if config.Image {
			toolLines = append(toolLines,
				"- To GENERATE a new image: use save_image_data with a description.",
			)
		} else {
			toolLines = append(toolLines,
				"- Image generation is NOT available. If user asks for a new image — say you cannot do that.",
			)
		}
	}

	b.WriteString("# Voice Tools\n")
	for _, line := range toolLines {
		b.WriteString(line + "\n")
	}
	b.WriteString("\n")

	// Шаг 4: очищенный промпт агента (личность, задача, данные)
	if cleaned != "" {
		b.WriteString("# Agent\n")
		b.WriteString(cleaned)
		b.WriteString("\n")
	}

	return b.String()
}

// extractFilesForRealtime разбирает результат файловых функций (get_s3_files, create_file, save_image_data).
// Возвращает:
//   - files — массив объектов {type, Url, file_name, caption} для отправки клиенту
//   - voiceConfirm — текст для модели БЕЗ URL (чтобы не озвучивала ссылки)
func extractFilesForRealtime(funcName, rawResult string) (files []map[string]interface{}, voiceConfirm string) {
	switch funcName {

	case "save_image_data":
		var r map[string]interface{}
		if err := json.Unmarshal([]byte(rawResult), &r); err != nil {
			return nil, ""
		}
		url, _ := r["url"].(string)
		if url == "" {
			return nil, ""
		}
		fileName := filepath.Base(url)
		files = []map[string]interface{}{{
			"type": "photo", "Url": url, "file_name": fileName, "caption": "",
		}}
		voiceConfirm = fmt.Sprintf(
			`{"status":"ok","file_name":%q,"type":"photo"}`,
			fileName)
		return

	case "create_file":
		var r map[string]interface{}
		if err := json.Unmarshal([]byte(rawResult), &r); err != nil {
			return nil, ""
		}
		url, _ := r["url"].(string)
		if url == "" {
			url, _ = r["Url"].(string)
		}
		fileName, _ := r["file_name"].(string)
		if fileName == "" && url != "" {
			fileName = filepath.Base(url)
		}
		fileType, _ := r["type"].(string)
		if fileType == "" {
			fileType = realtimeFileType(url)
		}
		if url == "" {
			return nil, ""
		}
		files = []map[string]interface{}{{
			"type": fileType, "Url": url, "file_name": fileName, "caption": "",
		}}
		voiceConfirm = fmt.Sprintf(
			`{"status":"ok","file_name":%q,"type":%q}`,
			fileName, fileType)
		return

	case "get_s3_files":
		var wrapper map[string]interface{}
		if err := json.Unmarshal([]byte(rawResult), &wrapper); err != nil {
			return nil, ""
		}
		outputStr, _ := wrapper["output"].(string)
		if outputStr == "" {
			return nil, ""
		}
		var urls []string
		if err := json.Unmarshal([]byte(outputStr), &urls); err != nil {
			return nil, ""
		}
		if len(urls) == 0 {
			voiceConfirm = `{"status":"ok","count":0}`
			return
		}
		// Конвертируем все URL в файлы для отправки клиенту — аналогично текстовому режиму.
		// Модели возвращаем только имена файлов (без URL), чтобы не озвучивала ссылки.
		nameParts := make([]string, 0, len(urls))
		for _, u := range urls {
			fileName := filepath.Base(u)
			fileType := realtimeFileType(u)
			files = append(files, map[string]interface{}{
				"type": fileType, "Url": u, "file_name": fileName, "caption": "",
			})
			nameParts = append(nameParts, fmt.Sprintf("%q", fileName))
		}
		voiceConfirm = fmt.Sprintf(
			`{"status":"ok","count":%d,"file_names":[%s]}`,
			len(urls), strings.Join(nameParts, ","))
		return
	}
	return nil, ""
}

// realtimeFileType определяет тип файла по расширению.
func realtimeFileType(url string) string {
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
