package kimi

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sync"
	"time"
)

// The official Kimi desktop speaks CDP over file descriptors 3 and 4 of its own
// process when it is started with -remote-debugging-pipe. Frames are JSON
// terminated by a NUL byte; there is no HTTP endpoint and no listening port.
// Everything here keeps that shape: no debug port is ever opened, and the pipe
// exists only for the child this package started.

const (
	// DefaultCallTimeout bounds one request. A page that stops answering must
	// not hold a caller forever.
	DefaultCallTimeout = 10 * time.Second
	// MaxFrameBytes bounds one inbound frame. The translation payload and page
	// snapshots are the largest legitimate messages.
	MaxFrameBytes = 4 << 20
	// MaxPendingCalls bounds concurrency so a stuck page cannot exhaust memory.
	MaxPendingCalls = 128
)

var (
	ErrPipeClosed      = errors.New("cdp_pipe_closed")
	ErrPipeBusy        = errors.New("cdp_pipe_busy")
	ErrPipeTimeout     = errors.New("cdp_pipe_timeout")
	ErrProtocolError   = errors.New("cdp_protocol_error")
	ErrMessageTooLarge = errors.New("cdp_pipe_message_too_large")
)

// Pipe is a CDP transport over a pair of byte streams. In production these are
// the child's fd4 (read) and fd3 (write); in tests they are pipes.
type Pipe struct {
	readable io.ReadCloser
	writable io.WriteCloser

	mu      sync.Mutex
	pending map[int]*call
	nextID  int
	closed  bool
	reason  string

	events   chan Event
	done     chan struct{}
	closeOne sync.Once
}

type call struct {
	resolve chan Result

	// timer is written by Send after the request is on the wire and read by the
	// reader goroutine when the response arrives, so both sides go through
	// setTimer/stopTimer.
	timerMu sync.Mutex
	timer   *time.Timer
}

func (c *call) setTimer(timer *time.Timer) {
	c.timerMu.Lock()
	c.timer = timer
	c.timerMu.Unlock()
}

func (c *call) stopTimer() {
	c.timerMu.Lock()
	if c.timer != nil {
		c.timer.Stop()
		c.timer = nil
	}
	c.timerMu.Unlock()
}

// Result is a CDP response body.
type Result struct {
	Result          map[string]any `json:"result"`
	Error           map[string]any `json:"error"`
	SessionID       string         `json:"sessionId"`
	ExceptionDetail map[string]any `json:"exceptionDetails"`
}

// Event is an unsolicited CDP message.
type Event struct {
	Method    string         `json:"method"`
	Params    map[string]any `json:"params"`
	SessionID string         `json:"sessionId"`
}

// NewPipe starts reading from readable and writes to writable. The caller owns
// the descriptors and must keep them open for the pipe's lifetime.
func NewPipe(readable io.ReadCloser, writable io.WriteCloser) *Pipe {
	pipe := &Pipe{
		readable: readable,
		writable: writable,
		pending:  map[int]*call{},
		events:   make(chan Event, 64),
		done:     make(chan struct{}),
	}
	go pipe.readLoop()
	return pipe
}

// Events delivers unsolicited messages until the pipe closes.
func (p *Pipe) Events() <-chan Event { return p.events }

// Done is closed when the transport ends, for any reason.
func (p *Pipe) Done() <-chan struct{} { return p.done }

// closeReason reads the termination reason under the lock.
func (p *Pipe) closeReason() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.reason
}

// Closed reports whether the transport has ended and why.
func (p *Pipe) Closed() (bool, string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.closed, p.reason
}

func (p *Pipe) readLoop() {
	reader := bufio.NewReaderSize(p.readable, 64<<10)
	for {
		// Frames are NUL-terminated. ReadString returns the terminator, so a
		// missing one at EOF is a truncated frame, not a message.
		frame, err := reader.ReadString(0)
		if len(frame) > MaxFrameBytes {
			p.dispose(ErrMessageTooLarge.Error())
			return
		}
		if trimmed := frame; len(trimmed) > 0 && trimmed[len(trimmed)-1] == 0 {
			p.dispatch(trimmed[:len(trimmed)-1])
		}
		if err != nil {
			p.dispose(ErrPipeClosed.Error())
			return
		}
	}
}

func (p *Pipe) dispatch(payload string) {
	var message struct {
		ID        *int           `json:"id"`
		Method    string         `json:"method"`
		Params    map[string]any `json:"params"`
		SessionID string         `json:"sessionId"`
		Result    map[string]any `json:"result"`
		Error     map[string]any `json:"error"`
	}
	if json.Unmarshal([]byte(payload), &message) != nil {
		// A frame the transport cannot parse is dropped, not fatal: CDP
		// messages are independent and one bad frame must not end a session.
		return
	}
	if message.ID != nil {
		p.mu.Lock()
		waiter, ok := p.pending[*message.ID]
		if ok {
			delete(p.pending, *message.ID)
		}
		p.mu.Unlock()
		if ok {
			waiter.stopTimer()
			waiter.resolve <- Result{Result: message.Result, Error: message.Error, SessionID: message.SessionID}
		}
		return
	}
	if message.Method == "" {
		return
	}
	select {
	case p.events <- Event{Method: message.Method, Params: message.Params, SessionID: message.SessionID}:
	default:
		// The event channel is a notification path, not a queue of record. A
		// slow consumer loses an event rather than blocking the reader, and
		// every consumer re-reads state it needs instead of trusting events.
	}
}

// Send issues one CDP call and waits for its response.
func (p *Pipe) Send(ctx context.Context, method string, params map[string]any, sessionID string, timeout time.Duration) (Result, error) {
	if timeout <= 0 {
		timeout = DefaultCallTimeout
	}
	p.mu.Lock()
	if p.closed {
		reason := p.reason
		p.mu.Unlock()
		return Result{}, fmt.Errorf("%w: %s", ErrPipeClosed, reason)
	}
	if len(p.pending) >= MaxPendingCalls {
		p.mu.Unlock()
		return Result{}, ErrPipeBusy
	}
	p.nextID++
	id := p.nextID
	waiter := &call{resolve: make(chan Result, 1)}
	p.pending[id] = waiter
	p.mu.Unlock()

	message := map[string]any{"id": id, "method": method, "params": params}
	if sessionID != "" {
		message["sessionId"] = sessionID
	}
	encoded, err := json.Marshal(message)
	if err != nil {
		p.drop(id)
		return Result{}, ErrProtocolError
	}
	if len(encoded)+1 > MaxFrameBytes {
		p.drop(id)
		return Result{}, ErrMessageTooLarge
	}
	if _, err := p.writable.Write(append(encoded, 0)); err != nil {
		p.drop(id)
		p.dispose(ErrPipeClosed.Error())
		return Result{}, fmt.Errorf("%w: write failed", ErrPipeClosed)
	}

	timer := time.NewTimer(timeout)
	defer timer.Stop()
	waiter.setTimer(timer)
	select {
	case result, open := <-waiter.resolve:
		if !open {
			return Result{}, fmt.Errorf("%w: %s", ErrPipeClosed, p.closeReason())
		}
		if result.Error != nil {
			return result, ErrProtocolError
		}
		return result, nil
	case <-timer.C:
		p.drop(id)
		return Result{}, ErrPipeTimeout
	case <-ctx.Done():
		p.drop(id)
		return Result{}, ctx.Err()
	case <-p.done:
		return Result{}, fmt.Errorf("%w: %s", ErrPipeClosed, p.closeReason())
	}
}

func (p *Pipe) drop(id int) {
	p.mu.Lock()
	delete(p.pending, id)
	p.mu.Unlock()
}

// dispose ends the transport once, rejecting every waiter.
func (p *Pipe) dispose(reason string) {
	p.closeOne.Do(func() {
		p.mu.Lock()
		p.closed = true
		p.reason = reason
		pending := p.pending
		p.pending = map[int]*call{}
		p.mu.Unlock()
		for _, waiter := range pending {
			waiter.stopTimer()
			close(waiter.resolve)
		}
		close(p.done)
		close(p.events)
	})
}

// Dispose closes the transport from the caller side.
func (p *Pipe) Dispose() { p.dispose(ErrPipeClosed.Error()) }
