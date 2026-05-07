package com

import "sync"

type LogMsg struct {
	Mod string
	Msg any
	Log int
	UID uint32
}

type Bus struct {
	MsgCh chan LogMsg
	wg    sync.WaitGroup
	once  sync.Once
}

// NewBus создает новый экземпляр Bus с буферизованным каналом для сообщений.
func NewBus(buf int) *Bus {
	return &Bus{MsgCh: make(chan LogMsg, buf)}
}

// Add регистрирует producer-горутину, запускает её и передает ей MsgCh.
func (b *Bus) Add(fn func(ch chan<- LogMsg)) {
	b.wg.Add(1)
	go func() {
		defer b.wg.Done()
		fn(b.MsgCh)
	}()
}

// WaitAndClose ждет завершения всех producers и закрывает канал.
func (b *Bus) WaitAndClose() {
	b.wg.Wait()
	b.once.Do(func() { close(b.MsgCh) })
}
