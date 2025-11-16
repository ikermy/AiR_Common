package common

import (
	"os"
	"runtime"
)

// SetProductionMode устанавливает режим продакшн, если приложение запущено на Linux.
func SetProductionMode(prodAct, devAct func()) {
	// Проверка на Linux
	hostname, _ := os.Hostname()
	if runtime.GOOS == "linux" && hostname != "fedora" {
		prodAct()
		return
	}

	devAct()
}
