package common

import "runtime"

// SetProductionMode устанавливает режим продакшн, если приложение запущено на Linux.
func SetProductionMode(prodAct, devAct func()) {
	// Проверка на Linux
	if runtime.GOOS == "linux" {
		prodAct()
	} else {
		devAct()
	}
}
