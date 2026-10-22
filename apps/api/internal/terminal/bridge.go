package terminal

import (
	"context"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/coder/websocket"
	"golang.org/x/sync/errgroup"
)

const (
	maxInputFrame = 4 << 10 // keystrokes are tiny; anything bigger is abuse
	outputChunk   = 4 << 10
	writeTimeout  = 5 * time.Second
)

var (
	pingInterval = 30 * time.Second
	pingTimeout  = 10 * time.Second
)

// Normal ways a bridge ends. Bridge turns them into a nil return.
var (
	errVMEnded    = errors.New("terminal: vm ended")
	errClientGone = errors.New("terminal: client disconnected")
)

// Bridge connects a WebSocket to a console until either side ends.
//
// It returns nil when the VM ended or the client left normally, and an error
// for anything else. It returns only after every goroutine it started has
// finished, and it always closes console: detaching, never stopping the VM.
func Bridge(ctx context.Context, ws *websocket.Conn, console io.ReadWriteCloser) error {
	ws.SetReadLimit(maxInputFrame)

	g, ctx := errgroup.WithContext(ctx)
	// Every goroutine below returns a non-nil error whenever it stops. That is
	// what cancels ctx and wakes the others, so none may ever return nil.
	g.Go(func() error { return pumpOutput(ctx, ws, console) })
	g.Go(func() error { return pumpInput(ctx, ws, console) })
	g.Go(func() error { return keepAlive(ctx, ws) })
	g.Go(func() error { // closer: Reads on the console do not watch ctx
		<-ctx.Done()
		console.Close()
		ws.CloseNow()
		return ctx.Err()
	})

	err := g.Wait()
	if errors.Is(err, errVMEnded) || errors.Is(err, errClientGone) {
		return nil
	}
	return err
}

// pumpOutput: guest -> browser.
func pumpOutput(ctx context.Context, ws *websocket.Conn, console io.Reader) error {
	buf := make([]byte, outputChunk)
	for {
		n, err := console.Read(buf)
		if n > 0 {
			wctx, cancel := context.WithTimeout(ctx, writeTimeout)
			werr := ws.Write(wctx, websocket.MessageBinary, buf[:n])
			cancel()
			if werr != nil {
				return werr
			}
		}
		if err != nil {
			if errors.Is(err, io.EOF) {
				ws.Close(websocket.StatusNormalClosure, "vm ended")
				return errVMEnded
			}
			return err
		}
	}
}

// pumpInput: browser -> guest.
func pumpInput(ctx context.Context, ws *websocket.Conn, console io.Writer) error {
	for {
		typ, data, err := ws.Read(ctx)
		if err != nil {
			switch websocket.CloseStatus(err) {
			case websocket.StatusNormalClosure, websocket.StatusGoingAway:
				return errClientGone
			}
			return err
		}
		if typ != websocket.MessageBinary {
			ws.Close(websocket.StatusUnsupportedData, "binary frames only")
			return fmt.Errorf("terminal: client sent a %v frame", typ)
		}
		if _, err := console.Write(data); err != nil {
			return err
		}
	}
}

// keepAlive pings the client so a dead connection is noticed and the console
// is released, and so idle sessions survive proxies. Ping needs a concurrent
// reader to see the pong; pumpInput is that reader.
func keepAlive(ctx context.Context, ws *websocket.Conn) error {
	t := time.NewTicker(pingInterval)
	defer t.Stop()

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-t.C:
			pctx, cancel := context.WithTimeout(ctx, pingTimeout)
			err := ws.Ping(pctx)
			cancel()
			if err != nil {
				return fmt.Errorf("terminal: client not responding: %w", err)
			}
		}
	}
}
