// Package logging prints log lines in a compact, human-friendly form:
//
//	[INFO]   [GetSystemData - ABS]  HTTP 200 · 1.2s · 640.0 KB
//	[WARN]   [BuyAirtime - ABS]     FAILED · code 141: invalid product id
//	[ERROR]  [CreateOrder - API]    could not reach Monnify: timeout
//
// The message passed to slog is the bracketed tag. The attribute "detail" is
// printed as the line's text; any other attributes follow as dim key=value.
package logging

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"
	"sync"
	"time"
)

const (
	reset = "\x1b[0m"
	bold  = "\x1b[1m"
	dim   = "\x1b[2m"
	red   = "\x1b[31m"
	green = "\x1b[32m"
	amber = "\x1b[33m"
	cyan  = "\x1b[36m"
)

// New returns a logger that prints the compact format to out. Colour is used
// only when out is a terminal and NO_COLOR is not set.
func New(out io.Writer, level slog.Level) *slog.Logger {
	return slog.New(&handler{out: out, color: isTerminal(out), level: level, mu: &sync.Mutex{}})
}

func isTerminal(w io.Writer) bool {
	f, ok := w.(*os.File)
	if !ok || os.Getenv("NO_COLOR") != "" {
		return false
	}
	info, err := f.Stat()
	return err == nil && info.Mode()&os.ModeCharDevice != 0
}

type handler struct {
	out   io.Writer
	color bool
	level slog.Level
	mu    *sync.Mutex // shared by copies made with WithAttrs
	attrs []slog.Attr
}

func (h *handler) Enabled(_ context.Context, l slog.Level) bool { return l >= h.level }

func (h *handler) WithAttrs(attrs []slog.Attr) slog.Handler {
	c := *h
	c.attrs = append(append([]slog.Attr{}, h.attrs...), attrs...)
	return &c
}

func (h *handler) WithGroup(string) slog.Handler { return h }

func (h *handler) Handle(_ context.Context, r slog.Record) error {
	var detail string
	var extras []string
	add := func(a slog.Attr) bool {
		if a.Key == "detail" {
			detail = oneLine(a.Value.String())
		} else {
			extras = append(extras, a.Key+"="+oneLine(a.Value.String()))
		}
		return true
	}
	for _, a := range h.attrs {
		add(a)
	}
	r.Attrs(add)

	levelName, levelColor := "INFO", green
	switch {
	case r.Level >= slog.LevelError:
		levelName, levelColor = "ERROR", red
	case r.Level >= slog.LevelWarn:
		levelName, levelColor = "WARN", amber
	case r.Level < slog.LevelInfo:
		levelName, levelColor = "DEBUG", dim
	}

	level := pad("["+levelName+"]", 7)
	tag := "[" + r.Message + "]"
	if h.color {
		level = levelColor + bold + level + reset
		tag = cyan + bold + tag + reset
		if r.Level >= slog.LevelError {
			detail = red + detail + reset
		}
	}
	line := level + "  " + tag + "  " + detail
	if len(extras) > 0 {
		extra := strings.Join(extras, " ")
		if h.color {
			extra = dim + extra + reset
		}
		line += "  " + extra
	}

	h.mu.Lock()
	defer h.mu.Unlock()
	_, err := fmt.Fprintln(h.out, line)
	return err
}

func pad(s string, width int) string {
	if len(s) >= width {
		return s
	}
	return s + strings.Repeat(" ", width-len(s))
}

// oneLine collapses newlines and runs of whitespace so a log entry is always
// a single line, even when it quotes a pretty-printed JSON reply.
func oneLine(s string) string { return strings.Join(strings.Fields(s), " ") }

// Bytes formats a size for humans: 512 B, 3.4 KB, 1.2 MB.
func Bytes(n int) string {
	switch {
	case n >= 1<<20:
		return fmt.Sprintf("%.1f MB", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%.1f KB", float64(n)/(1<<10))
	}
	return fmt.Sprintf("%d B", n)
}

// Duration formats an elapsed time for humans: <1ms, 84ms, 1.2s.
func Duration(d time.Duration) string {
	switch {
	case d < time.Millisecond:
		return "<1ms"
	case d < time.Second:
		return fmt.Sprintf("%dms", d.Milliseconds())
	}
	return fmt.Sprintf("%.1fs", d.Seconds())
}
