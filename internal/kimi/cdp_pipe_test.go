package kimi

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type chunkedWriteCloser struct {
	mu      sync.Mutex
	buf     bytes.Buffer
	chunk   int
	closed  atomic.Bool
	writing atomic.Int32
	overlap atomic.Bool
}

func (w *chunkedWriteCloser) Write(p []byte) (int, error) {
	if w.closed.Load() {
		return 0, io.ErrClosedPipe
	}
	if w.writing.Add(1) > 1 {
		w.overlap.Store(true)
	}
	defer w.writing.Add(-1)
	step := w.chunk
	if step <= 0 {
		step = len(p)
	}
	written := 0
	for written < len(p) {
		end := written + step
		if end > len(p) {
			end = len(p)
		}
		w.mu.Lock()
		_, _ = w.buf.Write(p[written:end])
		w.mu.Unlock()
		written = end
		time.Sleep(time.Microsecond)
	}
	return len(p), nil
}

func (w *chunkedWriteCloser) Close() error {
	w.closed.Store(true)
	return nil
}

func (w *chunkedWriteCloser) Bytes() []byte {
	w.mu.Lock()
	defer w.mu.Unlock()
	out := make([]byte, w.buf.Len())
	copy(out, w.buf.Bytes())
	return out
}

func TestPipeDisposeConcurrentWithEventsDoesNotPanicAndClosesStreams(t *testing.T) {
	for range 25 {
		toHostReader, toHostWriter := io.Pipe()
		toPageReader, toPageWriter := io.Pipe()
		pipe := NewPipe(toHostReader, toPageWriter)

		var writerWG sync.WaitGroup
		writerWG.Add(1)
		go func() {
			defer writerWG.Done()
			frame := []byte(`{"method":"Page.loadEventFired","params":{}}` + "\x00")
			for range 200 {
				if _, err := toHostWriter.Write(frame); err != nil {
					return
				}
			}
		}()

		time.Sleep(time.Millisecond)
		pipe.Dispose()

		select {
		case <-pipe.Done():
		case <-time.After(2 * time.Second):
			t.Fatal("pipe.Done was not closed")
		}

		// Events channel must eventually close cleanly once readLoop exits.
		deadline := time.After(2 * time.Second)
	drain:
		for {
			select {
			case _, ok := <-pipe.Events():
				if !ok {
					break drain
				}
			case <-deadline:
				t.Fatal("pipe.Events was not closed after Dispose")
			}
		}

		// Writable side must be closed so the peer reader sees EOF.
		buf := make([]byte, 1)
		if _, err := toPageReader.Read(buf); err != io.EOF {
			t.Fatalf("writable side was not closed on Dispose: err=%v", err)
		}

		_ = toHostWriter.Close()
		_ = toPageReader.Close()
		writerWG.Wait()
	}
}

func TestPipeSendSerializesConcurrentWrites(t *testing.T) {
	toHostReader, toHostWriter := io.Pipe()
	writer := &chunkedWriteCloser{chunk: 17}
	pipe := NewPipe(toHostReader, writer)
	t.Cleanup(func() {
		pipe.Dispose()
		_ = toHostWriter.Close()
	})

	payload := strings.Repeat("x", 2048)
	var wg sync.WaitGroup
	for i := range 12 {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
			defer cancel()
			_, _ = pipe.Send(ctx, "Runtime.evaluate", map[string]any{"idx": idx, "data": payload}, "", 20*time.Millisecond)
		}(i)
	}
	wg.Wait()

	if writer.overlap.Load() {
		t.Fatal("concurrent Pipe.Send writes overlapped")
	}
	frames := bytes.Split(writer.Bytes(), []byte{0})
	if len(frames) != 13 || len(frames[12]) != 0 {
		t.Fatalf("expected 12 NUL-terminated frames, got %d parts", len(frames))
	}
	for i := range 12 {
		var msg map[string]any
		if err := json.Unmarshal(frames[i], &msg); err != nil {
			t.Fatalf("frame %d corrupted/interleaved: %v", i, err)
		}
	}
}

func TestPipeConsoleSpamDoesNotDropBindings(t *testing.T) {
	toHostReader, toHostWriter := io.Pipe()
	pipe := NewPipe(toHostReader, &chunkedWriteCloser{})
	t.Cleanup(pipe.Dispose)

	const diagnostics = `{"method":"Runtime.consoleAPICalled"}` + "\x00" +
		`{"method":"Runtime.exceptionThrown"}` + "\x00" + `{"method":"Runtime.inspectRequested"}` + "\x00"
	const binding = `{"method":"Runtime.bindingCalled","sessionId":"work"}` + "\x00"
	const world = `{"method":"Runtime.executionContextCreated","sessionId":"work"}` + "\x00"
	if _, err := io.WriteString(toHostWriter, strings.Repeat(diagnostics, 1024)+binding+world); err != nil {
		t.Fatal(err)
	}
	_ = toHostWriter.Close()
	<-pipe.Done()
	var got []Event
	for event := range pipe.Events() {
		got = append(got, event)
	}
	if len(got) != 2 || got[0].Method != "Runtime.bindingCalled" || got[0].SessionID != "work" || got[1].Method != "Runtime.executionContextCreated" {
		t.Fatalf("diagnostic events crowded out renderer events: count=%d", len(got))
	}
}
