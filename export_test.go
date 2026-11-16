package tuntcp

import (
	"io"
	"time"
)

// NewWithClock は、時計と、タイマーを調べる間隔を差し替えて New と同じことをする。テストだけが使う。
func NewWithClock(dev io.ReadWriteCloser, now func() time.Time, interval time.Duration) *Stack {
	return newStack(dev, now, interval)
}
