package telemetry

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
)

// LogHandler inoltra i record a next (il log su stderr) e copia quelli da
// Info in su nel registro della finestra.
type LogHandler struct {
	next   slog.Handler
	c      *Collector
	prefix string // attributi di WithAttrs, già formattati
	group  string
}

// NewLogHandler avvolge next.
func NewLogHandler(next slog.Handler, c *Collector) *LogHandler {
	return &LogHandler{next: next, c: c}
}

func (h *LogHandler) Enabled(ctx context.Context, l slog.Level) bool {
	return l >= slog.LevelInfo || h.next.Enabled(ctx, l)
}

func (h *LogHandler) Handle(ctx context.Context, r slog.Record) error {
	if r.Level >= slog.LevelInfo {
		var b strings.Builder
		b.WriteString(r.Message)
		if h.prefix != "" {
			b.WriteByte(' ')
			b.WriteString(h.prefix)
		}
		r.Attrs(func(a slog.Attr) bool {
			writeAttr(&b, h.group, a)
			return true
		})
		lvl := byte('I')
		switch {
		case r.Level >= slog.LevelError:
			lvl = 'E'
		case r.Level >= slog.LevelWarn:
			lvl = 'W'
		}
		h.c.AddLog(LogLine{Time: r.Time, Level: lvl, Text: b.String()})
	}
	if h.next.Enabled(ctx, r.Level) {
		return h.next.Handle(ctx, r)
	}
	return nil
}

func (h *LogHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	var b strings.Builder
	b.WriteString(h.prefix)
	for _, a := range attrs {
		writeAttr(&b, h.group, a)
	}
	return &LogHandler{next: h.next.WithAttrs(attrs), c: h.c, prefix: strings.TrimSpace(b.String()), group: h.group}
}

func (h *LogHandler) WithGroup(name string) slog.Handler {
	g := name
	if h.group != "" {
		g = h.group + "." + name
	}
	return &LogHandler{next: h.next.WithGroup(name), c: h.c, prefix: h.prefix, group: g}
}

func writeAttr(b *strings.Builder, group string, a slog.Attr) {
	if a.Equal(slog.Attr{}) {
		return
	}
	if b.Len() > 0 {
		b.WriteByte(' ')
	}
	if group != "" {
		b.WriteString(group)
		b.WriteByte('.')
	}
	b.WriteString(a.Key)
	b.WriteByte('=')
	v := a.Value.Resolve()
	s := v.String()
	if v.Kind() == slog.KindString && (s == "" || strings.ContainsAny(s, " =\"")) {
		s = fmt.Sprintf("%q", s)
	}
	b.WriteString(s)
}
