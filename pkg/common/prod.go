package common

import (
	"runtime"
)

// SetProductionMode устанавливает режим продакшн, если приложение запущено на Linux.
func SetProductionMode(prodAct, devAct func()) {
	// Проверка на Linux
	//hostname, _ := os.Hostname()
	//if runtime.GOOS == "linux" && hostname != "fedora" {
	if runtime.GOOS == "linux" {
		prodAct()
		return
	}

	devAct()
}
