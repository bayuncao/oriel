package acp

import (
	"context"
	"log/slog"
)

// quiet 把 SDK 的診斷日誌降為 debug，並去掉它附的原始訊息（"raw"），
// 日誌裡不留對話內容（C38）。
type quiet struct{ h slog.Handler }

func (q quiet) Enabled(ctx context.Context, _ slog.Level) bool {
	return q.h.Enabled(ctx, slog.LevelDebug)
}

func (q quiet) Handle(ctx context.Context, r slog.Record) error {
	out := slog.NewRecord(r.Time, slog.LevelDebug, "acp sdk: "+r.Message, r.PC)
	r.Attrs(func(a slog.Attr) bool {
		if a.Key != "raw" {
			out.AddAttrs(a)
		}
		return true
	})
	return q.h.Handle(ctx, out)
}

func (q quiet) WithAttrs(as []slog.Attr) slog.Handler {
	kept := make([]slog.Attr, 0, len(as))
	for _, a := range as {
		if a.Key != "raw" {
			kept = append(kept, a)
		}
	}
	return quiet{q.h.WithAttrs(kept)}
}

func (q quiet) WithGroup(name string) slog.Handler { return quiet{q.h.WithGroup(name)} }
