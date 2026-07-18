//go:build windows || linux

package tray

import "sync"

func onceFunc(fn func()) func() {
	var once sync.Once
	return func() {
		once.Do(func() {
			if fn != nil {
				fn()
			}
		})
	}
}
