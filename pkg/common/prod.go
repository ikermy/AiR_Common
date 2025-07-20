package common

import "runtime"

// SetProductionMode устанавливает режим продакшн, если приложение запущено на Linux.
func(action func()) {
	// Проверка на Linux
	if runtime.GOOS == "linux" {
		action()
	}
}
