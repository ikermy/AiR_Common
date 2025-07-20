package common

import "runtime"

// SetProductionMode устанавливает режим продакшн, если приложение запущено на Linux.
func SetProductionMode(action func()) {
	// Проверка на Linux
	if runtime.GOOS == "linux" {
		action()
	}
}
