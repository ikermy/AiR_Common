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

// buildRealtimeSystemPrompt строит голосовой промпт для Realtime API.
// Источник истины — config, заполненный buildAgentConfiguration (данные от MCP):
//   - config.SystemPrompt — промпт агента (modelData.Prompt + hint от MCP)
//   - config.Tools — список инструментов от MCP
//
// Алгоритм:
//  1. universalRealtimeSystemPrompt (голосовой стиль)
//  2. Если есть function-tools — универсальные инструкции по их использованию голосом
//  3. Промпт агента из config.SystemPrompt (уже содержит все специфичные инструкции от MCP)
func buildRealtimeSystemPrompt(config *OpenAIAgentConfig) string {
	var b strings.Builder

	// Шаг 1: голосовой стиль
	b.WriteString(universalRealtimeSystemPrompt)

	// Шаг 2: универсальные голосовые инструкции по инструментам (если они есть)
	hasFunctionTools := false
	for _, t := range config.Tools {
		if tm, ok := t.(map[string]interface{}); ok {
			if tm["type"] == "function" {
				hasFunctionTools = true
				break
			}
		}
	}
	if hasFunctionTools {
		b.WriteString("# Voice Tools\n")
		b.WriteString("- Before any tool call, say ONE short sentence: \"One moment.\" Then call immediately.\n")
		b.WriteString("- After a tool returns — summarise in natural spoken language. DO NOT read raw JSON or URLs aloud.\n")
		b.WriteString("- NEVER say \"I can't check\" if a tool exists — CALL IT.\n")
		b.WriteString("- If a tool returns an error or unavailable — say so briefly and move on. Do NOT retry the same tool.\n")
		b.WriteString("\n")
	}

	// Шаг 3: промпт агента от MCP (содержит личность, задачу, специфичные инструкции)
	// Убираем повторяющиеся пустые строки
	multiNewline := regexp.MustCompile(`\n{3,}`)
	cleaned := strings.TrimSpace(multiNewline.ReplaceAllString(config.SystemPrompt, "\n\n"))
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
