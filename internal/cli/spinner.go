package cli

import (
	"fmt"
	"os"
	"sync"
	"time"
)

// spinner animates a status line on stderr while a slow, silent step runs (auth, a network call), so
// the CLI doesn't sit blank between an action and its result. It animates only on a terminal: piped
// or CI output gets the message printed once, with no carriage returns or escape codes to garble a
// log. Final ✓/✗ lines stay on stdout; the spinner clears its own line before they print.
type spinner struct {
	quit   chan struct{}
	done   sync.WaitGroup
	active bool
}

// braille dots, the usual rotating-spinner frames.
var spinnerFrames = []rune("⠋⠙⠹⠸⠼⠴⠦⠧⠇⠏")

// startSpinner begins animating msg and returns a handle to stop it. Always pair it with stop(),
// typically deferred or called right before printing the step's result.
func startSpinner(msg string) *spinner {
	s := &spinner{}
	if !isTerminal(os.Stderr) {
		fmt.Fprintln(os.Stderr, msg+"…")
		return s
	}
	s.active = true
	s.quit = make(chan struct{})
	s.done.Add(1)
	go s.run(msg)
	return s
}

func (s *spinner) run(msg string) {
	defer s.done.Done()
	frame := func(i int) { fmt.Fprintf(os.Stderr, "\r\033[K%c %s", spinnerFrames[i%len(spinnerFrames)], msg) }
	frame(0) // render immediately so there is no blank gap before the first tick
	t := time.NewTicker(80 * time.Millisecond)
	defer t.Stop()
	for i := 1; ; i++ {
		select {
		case <-s.quit:
			return
		case <-t.C:
			frame(i)
		}
	}
}

// stop halts the animation and clears the line so the caller's own output starts clean. Safe to call
// on a non-terminal (no-op) and idempotent enough for a single deferred call.
func (s *spinner) stop() {
	if !s.active {
		return
	}
	s.active = false
	close(s.quit)
	s.done.Wait()
	fmt.Fprint(os.Stderr, "\r\033[K")
}

// isTerminal reports whether f is a character device (an interactive terminal) rather than a pipe or
// file, so the spinner stays quiet when output is captured.
func isTerminal(f *os.File) bool {
	fi, err := f.Stat()
	if err != nil {
		return false
	}
	return fi.Mode()&os.ModeCharDevice != 0
}
