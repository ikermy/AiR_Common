package common

import (
	"fmt"
	"os/exec"
	"strings"
	"time"
)

var (
	Version     string
	BuildTime   string
	GitCommit   string
	initialized bool
)

func init() {
	if !initialized {
		initialize()
	}
}

func initialize() {
	// Получаем версию из git
	cmd := exec.Command("git", "describe", "--tags", "--abbrev=0")
	output, err := cmd.Output()

	Version = "dev"
	if err == nil {
		Version = strings.TrimSpace(string(output))
	}

	// Получаем хэш коммита
	cmd = exec.Command("git", "rev-parse", "--short", "HEAD")
	output, err = cmd.Output()
	GitCommit = "unknown"
	if err == nil {
		GitCommit = strings.TrimSpace(string(output))
	}

	// Текущее время сборки
	BuildTime = time.Now().Format("2006-01-02 15:04:05")

	initialized = true
}

func GetVersionInfo() string {
	return fmt.Sprintf("Версия: %s (коммит: %s, сборка: %s)",
		Version, GitCommit, BuildTime)
}
