package app

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"
)

var errFirstOutputTimeout = errors.New("等待首个输出超时")

// Each generation attempt owns its timer and context. Metadata/heartbeats do
// not count as output; text or reasoning releases only the first-output timer.
func firstOutputGuard(parent context.Context, wait time.Duration, next func(string) error) (context.Context, func(string) error, func()) {
	if wait <= 0 {
		return parent, next, func() {}
	}
	ctx, cancel := context.WithCancelCause(parent)
	var timer *time.Timer
	var mu sync.Mutex
	started := false
	if wait > 0 {
		timer = time.AfterFunc(wait, func() {
			mu.Lock()
			defer mu.Unlock()
			if !started {
				cancel(fmt.Errorf("%w（%s）", errFirstOutputTimeout, wait))
			}
		})
	}
	line := func(s string) error {
		if err := context.Cause(ctx); err != nil {
			return err
		}
		meaningful := strings.TrimSpace(cleanGeminiText(reasoningInLine(s))) != ""
		for _, text := range renderedTextsInLine(s, false) {
			meaningful = meaningful || strings.TrimSpace(cleanGeminiText(text)) != ""
		}
		mu.Lock()
		if err := context.Cause(ctx); err != nil {
			mu.Unlock()
			return err
		}
		if meaningful {
			started = true
			if timer != nil {
				timer.Stop()
			}
		}
		mu.Unlock()
		if next != nil {
			return next(s)
		}
		return nil
	}
	return ctx, line, func() {
		if timer != nil {
			timer.Stop()
		}
		cancel(nil)
	}
}

func firstOutputWait(mc ModelConfig) time.Duration {
	if mc.Tool != 0 {
		return 0 // Media generation keeps its existing timeout.
	}
	return time.Duration(rtCfg().FirstOutputTimeout) * time.Second
}
