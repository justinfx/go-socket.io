package socketio

import (
	"log"
	"os"
	"sync"
	"time"
)

type nopWriter struct{}

func (nw nopWriter) Write(p []byte) (n int, err error) {
	return len(p), nil
}

var (
	NOPLogger     = log.New(nopWriter{}, "", 0)
	DefaultLogger = log.New(os.Stdout, "", log.Ldate|log.Ltime)
)

type DelayTimer struct {
	mu       sync.Mutex
	handling bool
	deadline time.Time
	Timeouts chan time.Time
	timer    *time.Timer
}

func NewDelayTimer() *DelayTimer {
	return &DelayTimer{Timeouts: make(chan time.Time)}
}

func (w *DelayTimer) Stop() {
	w.mu.Lock()
	if w.timer != nil {
		w.timer.Stop()
	}
	w.mu.Unlock()
}

func (w *DelayTimer) Reset(t time.Duration) {
	nt := time.Now().Add(t)
	w.mu.Lock()
	if nt.Before(w.deadline) {
		w.mu.Unlock()
		return
	}
	if w.timer != nil {
		w.timer.Stop()
	}
	w.timer = time.AfterFunc(nt.Sub(time.Now()), func() {
		// If previous timeout is still being handled, then 
		// ignore this timeout. 
		w.mu.Lock()
		if w.handling {
			w.mu.Unlock()
			return
		}
		w.handling = true
		w.mu.Unlock()
		w.Timeouts <- time.Now()
		w.mu.Lock()
		w.handling = false
		w.mu.Unlock()
	})
	w.deadline = nt
	w.mu.Unlock()
}
