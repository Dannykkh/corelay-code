package acp

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"sync"
)

type Options struct {
	MaxFrameBytes int
	MaxInflight   int
	WriteQueue    int
}

func (o Options) normalized() Options {
	if o.MaxFrameBytes <= 0 || o.MaxFrameBytes > DefaultMaxFrameBytes {
		o.MaxFrameBytes = DefaultMaxFrameBytes
	}
	if o.MaxInflight <= 0 || o.MaxInflight > 4096 {
		o.MaxInflight = DefaultMaxInflight
	}
	if o.WriteQueue <= 0 || o.WriteQueue > 4096 {
		o.WriteQueue = DefaultWriteQueue
	}
	return o
}

type writeJob struct {
	ctx  context.Context
	data []byte
	done chan error
}

type pendingResponse struct {
	result json.RawMessage
	err    *RPCError
}

type pendingCall struct {
	sessionID string
	cancel    context.CancelFunc
	response  chan pendingResponse
}

type sessionState struct {
	promptCancel       context.CancelFunc
	promptRequestKey   string
	cancelledBySession bool
}

// Connection owns one newline-delimited JSON-RPC 2.0 stdio transport. Input
// and output are closers so cancellation can interrupt blocked pipe I/O.
type Connection struct {
	input   io.ReadCloser
	output  io.WriteCloser
	backend Backend
	opts    Options

	serveOnce sync.Once
	serveErr  error
	ctx       context.Context
	cancel    context.CancelFunc
	writeQ    chan writeJob
	writerWG  sync.WaitGroup
	requestWG sync.WaitGroup
	sem       chan struct{}

	mu                sync.Mutex
	closed            bool
	shutdownRequested bool
	initializing      bool
	initialized       bool
	clientCaps        ClientCapabilities
	descriptor        BackendDescriptor
	sessions          map[string]*sessionState
	inbound           map[string]context.CancelFunc
	pending           map[string]*pendingCall
	nextRequest       int64
}

func NewConnection(input io.ReadCloser, output io.WriteCloser, backend Backend, options Options) *Connection {
	options = options.normalized()
	return &Connection{
		input:       input,
		output:      output,
		backend:     backend,
		opts:        options,
		writeQ:      make(chan writeJob, options.WriteQueue),
		sem:         make(chan struct{}, options.MaxInflight),
		sessions:    make(map[string]*sessionState),
		inbound:     make(map[string]context.CancelFunc),
		pending:     make(map[string]*pendingCall),
		nextRequest: 1,
	}
}

func (c *Connection) Serve(ctx context.Context) error {
	c.serveOnce.Do(func() { c.serveErr = c.serve(ctx) })
	return c.serveErr
}

// Shutdown interrupts stdio and causes Serve to cancel all inbound and
// outbound work. It is safe to call more than once.
func (c *Connection) Shutdown() {
	c.mu.Lock()
	c.shutdownRequested = true
	cancel := c.cancel
	c.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	if c.input != nil {
		_ = c.input.Close()
	}
	if c.output != nil {
		_ = c.output.Close()
	}
}

func (c *Connection) serve(parent context.Context) error {
	if c.input == nil || c.output == nil || c.backend == nil {
		return errors.New("acp: input, output, and backend are required")
	}
	if parent == nil {
		parent = context.Background()
	}
	connectionCtx, connectionCancel := context.WithCancel(parent)
	c.mu.Lock()
	c.ctx, c.cancel = connectionCtx, connectionCancel
	shutdownRequested := c.shutdownRequested
	c.mu.Unlock()
	if shutdownRequested {
		c.cancel()
	}
	c.writerWG.Add(1)
	go c.writerLoop()
	readDone := make(chan struct{})
	go func() {
		select {
		case <-c.ctx.Done():
			_ = c.input.Close()
		case <-readDone:
		}
	}()
	defer close(readDone)
	defer c.shutdown()

	reader := bufio.NewReaderSize(c.input, 64<<10)
	for {
		frame, err := readFrame(reader, c.opts.MaxFrameBytes)
		if errors.Is(err, ErrFrameTooBig) {
			_ = c.writeRPCError(c.ctx, NullID(), &RPCError{Code: CodeParseError, Message: "frame exceeds protocol limit"})
			continue
		}
		if err != nil {
			if errors.Is(err, io.EOF) || c.ctx.Err() != nil || c.isShutdownRequested() {
				return nil
			}
			return errors.New("acp: stdio read failed")
		}
		if len(bytes.TrimSpace(frame)) == 0 {
			continue
		}
		envelope, rpcErr := decodeEnvelope(frame)
		if rpcErr != nil {
			_ = c.writeRPCError(c.ctx, NullID(), rpcErr)
			continue
		}
		if envelope.IsRequest {
			if envelope.HasID {
				c.startRequest(envelope)
			} else {
				c.handleNotification(envelope)
			}
			continue
		}
		c.resolveResponse(envelope)
	}
}

func (c *Connection) isShutdownRequested() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.shutdownRequested
}

func (c *Connection) shutdown() {
	c.mu.Lock()
	cancel := c.cancel
	c.closed = true
	for _, cancel := range c.inbound {
		cancel()
	}
	for _, pending := range c.pending {
		pending.cancel()
	}
	c.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	_ = c.input.Close()
	_ = c.output.Close()
	c.requestWG.Wait()
	c.writerWG.Wait()
}

func (c *Connection) writerLoop() {
	defer c.writerWG.Done()
	for {
		select {
		case <-c.ctx.Done():
			return
		case job := <-c.writeQ:
			if job.ctx != nil {
				select {
				case <-job.ctx.Done():
					completeWrite(job, job.ctx.Err())
					continue
				default:
				}
			}
			err := writeAll(c.output, job.data)
			completeWrite(job, err)
			if err != nil {
				c.cancel()
				return
			}
		}
	}
}

func completeWrite(job writeJob, err error) {
	if job.done != nil {
		job.done <- err
		close(job.done)
	}
}

func writeAll(writer io.Writer, data []byte) error {
	for len(data) > 0 {
		n, err := writer.Write(data)
		if err != nil {
			return err
		}
		if n <= 0 {
			return io.ErrShortWrite
		}
		data = data[n:]
	}
	return nil
}

func readFrame(reader *bufio.Reader, limit int) ([]byte, error) {
	frame := make([]byte, 0, 4096)
	overflow := false
	for {
		fragment, err := reader.ReadSlice('\n')
		if !overflow {
			remaining := limit + 1 - len(frame)
			if remaining > 0 {
				if len(fragment) > remaining {
					frame = append(frame, fragment[:remaining]...)
				} else {
					frame = append(frame, fragment...)
				}
			}
			if len(frame) > limit {
				overflow = true
			}
		}
		if errors.Is(err, bufio.ErrBufferFull) {
			continue
		}
		if err != nil && !errors.Is(err, io.EOF) {
			return nil, err
		}
		if overflow {
			return nil, ErrFrameTooBig
		}
		frame = bytes.TrimSuffix(frame, []byte{'\n'})
		frame = bytes.TrimSuffix(frame, []byte{'\r'})
		if errors.Is(err, io.EOF) && len(frame) == 0 {
			return nil, io.EOF
		}
		return frame, nil
	}
}

func (c *Connection) send(ctx context.Context, value any) error {
	if ctx == nil {
		ctx = context.Background()
	}
	data, err := json.Marshal(value)
	if err != nil {
		return errors.New("acp: encode JSON-RPC frame failed")
	}
	if len(data) > c.opts.MaxFrameBytes {
		return ErrFrameTooBig
	}
	data = append(data, '\n')
	job := writeJob{ctx: ctx, data: data, done: make(chan error, 1)}
	select {
	case <-c.ctx.Done():
		return ErrClosed
	case <-ctx.Done():
		return ctx.Err()
	case c.writeQ <- job:
	}
	select {
	case <-c.ctx.Done():
		return ErrClosed
	case <-ctx.Done():
		return ctx.Err()
	case err := <-job.done:
		return err
	}
}

func (c *Connection) trySend(value any) {
	data, err := json.Marshal(value)
	if err != nil || len(data) > c.opts.MaxFrameBytes {
		return
	}
	data = append(data, '\n')
	job := writeJob{ctx: c.ctx, data: data}
	select {
	case c.writeQ <- job:
	default:
	}
}

func (c *Connection) writeRPCError(ctx context.Context, id RequestID, rpcErr *RPCError) error {
	if rpcErr == nil {
		rpcErr = &RPCError{Code: CodeInternalError, Message: "internal error"}
	}
	return c.send(ctx, wireResponse{JSONRPC: "2.0", ID: id, Error: rpcErr})
}

func (c *Connection) writeResult(ctx context.Context, id RequestID, result any) error {
	return c.send(ctx, wireResponse{JSONRPC: "2.0", ID: id, Result: result})
}

func (c *Connection) startRequest(envelope rpcEnvelope) {
	select {
	case c.sem <- struct{}{}:
	default:
		_ = c.writeRPCError(c.ctx, envelope.ID, &RPCError{Code: CodeInternalError, Message: "too many inflight requests"})
		return
	}
	key := envelope.ID.key()
	requestCtx, cancel := context.WithCancel(c.ctx)
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		cancel()
		<-c.sem
		return
	}
	if _, duplicate := c.inbound[key]; duplicate {
		c.mu.Unlock()
		cancel()
		<-c.sem
		_ = c.writeRPCError(c.ctx, envelope.ID, &RPCError{Code: CodeInvalidRequest, Message: "duplicate inflight request id"})
		return
	}
	c.inbound[key] = cancel
	c.requestWG.Add(1)
	c.mu.Unlock()

	go func() {
		defer c.requestWG.Done()
		var finishOnce sync.Once
		finish := func() {
			finishOnce.Do(func() {
				cancel()
				c.mu.Lock()
				delete(c.inbound, key)
				c.mu.Unlock()
				<-c.sem
			})
		}
		defer finish()
		result, rpcErr := c.dispatch(requestCtx, key, envelope.Method, envelope.Params)
		// Release the inbound slot before the response is observable. A client
		// that sends its next request immediately after reading the response
		// must not see a stale inflight-limit failure.
		finish()
		if rpcErr != nil {
			_ = c.writeRPCError(c.ctx, envelope.ID, rpcErr)
			return
		}
		if err := c.writeResult(c.ctx, envelope.ID, result); err != nil && c.ctx.Err() == nil {
			_ = c.writeRPCError(c.ctx, envelope.ID, internalError())
		}
	}()
}

func (c *Connection) handleNotification(envelope rpcEnvelope) {
	switch envelope.Method {
	case MethodCancelRequest:
		var params struct {
			RequestID RequestID `json:"requestId"`
			Meta      Meta      `json:"_meta,omitempty"`
		}
		if decodeObject(envelope.Params, &params, []string{"requestId", "_meta"}, []string{"requestId"}) != nil {
			return
		}
		c.mu.Lock()
		cancel := c.inbound[params.RequestID.key()]
		c.mu.Unlock()
		if cancel != nil {
			cancel()
		}
	case MethodSessionCancel:
		var params CancelNotification
		if decodeObject(envelope.Params, &params, []string{"sessionId", "_meta"}, []string{"sessionId"}) != nil || validateSessionID(params.SessionID) != nil {
			return
		}
		c.cancelSession(params)
	}
}

func (c *Connection) resolveResponse(envelope rpcEnvelope) {
	key := envelope.ID.key()
	c.mu.Lock()
	pending := c.pending[key]
	c.mu.Unlock()
	if pending == nil {
		return
	}
	response := pendingResponse{result: cloneRaw(envelope.Result), err: envelope.Error}
	select {
	case pending.response <- response:
	default:
	}
}

func (c *Connection) cancelSession(params CancelNotification) {
	if !c.markSessionCancelled(params.SessionID) {
		return
	}
	c.spawn(func() { _ = c.backend.CancelSession(c.ctx, params) })
}

func (c *Connection) markSessionCancelled(sessionID string) bool {
	c.mu.Lock()
	state := c.sessions[sessionID]
	if state == nil {
		c.mu.Unlock()
		return false
	}
	state.cancelledBySession = true
	if state.promptCancel != nil {
		state.promptCancel()
	}
	for _, pending := range c.pending {
		if pending.sessionID == sessionID {
			pending.cancel()
		}
	}
	c.mu.Unlock()
	return true
}

func (c *Connection) spawn(fn func()) bool {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return false
	}
	c.requestWG.Add(1)
	c.mu.Unlock()
	go func() {
		defer c.requestWG.Done()
		fn()
	}()
	return true
}

func (c *Connection) cancelInbound(id RequestID) {
	c.mu.Lock()
	cancel := c.inbound[id.key()]
	c.mu.Unlock()
	if cancel != nil {
		cancel()
	}
}
