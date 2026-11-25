package daemon

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/chromedp/cdproto/target"
	"github.com/coinpost/go-bb-browser/internal/timeout"
)

// A tab_new whose request ends — the client went away, a forced shutdown closed
// its connection, or the operation budget ran out — after Target.createTarget
// was sent has still asked the browser for a page. Cancelling the round trip
// with the request, as every other method does, throws the reply away: the page
// then stays open with an id the daemon never learned, unmanaged, and the audit
// line says ok=false with no target.
//
// The creation round trip is therefore not tied to the request. When the
// request ends first, the handler keeps waiting for the reply for a bounded
// time (the settlement). The only identity it acts on is the complete target id
// in that reply:
//
//   - reply with an id: nobody is left to be given the id, so the tab is rolled
//     back with one bounded close of exactly that id (rollbackCreatedTab); it is
//     neither registered as managed nor selected, and the audit line carries the
//     id and the outcome of the close;
//   - reply with an error or without an id: the creation failed, reported as
//     such;
//   - no reply within the bound, or the connection is no longer trusted: nothing
//     is known. No target is closed or adopted on a guess (a listing cannot tell
//     which page this request created); the error and a warning say that a tab
//     may have been left open, unmanaged.
//
// The commit is the boundary: a request that ends after commitCreatedTab keeps
// its managed tab (idle cleanup and restart recovery own it from there).

// createSettleTimeout bounds how long a tab_new whose request has ended waits
// for the Target.createTarget reply still in flight. With createRollbackTimeout
// it must stay below the flush margin of the HTTP write timeout.
const createSettleTimeout = 5 * time.Second

// createShutdownMargin is added to the waits of settleCreations for the steps
// around the two bounded round trips.
const createShutdownMargin = 2 * time.Second

// createGate counts the tab_new requests between their creation request and
// their answer, so that shutdown can let them settle before the session and the
// RPC log go, and refuses new creations once shutdown has started to wait.
type createGate struct {
	mu     sync.Mutex
	closed bool
	n      int
	idle   chan struct{} // closed when n drops to zero; nil while nobody waits
	abort  chan struct{} // closed when shutdown stops waiting for replies
	ended  bool          // abort is closed
}

// begin admits one creation; false once shutdown has closed the gate.
func (g *createGate) begin() bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.closed {
		return false
	}
	g.n++
	return true
}

func (g *createGate) end() {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.n--; g.n == 0 && g.idle != nil {
		close(g.idle)
		g.idle = nil
	}
}

// shut closes the gate and returns a channel closed once no creation is in
// flight.
func (g *createGate) shut() <-chan struct{} {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.closed = true
	if g.n == 0 {
		done := make(chan struct{})
		close(done)
		return done
	}
	if g.idle == nil {
		g.idle = make(chan struct{})
	}
	return g.idle
}

// aborted is closed once shutdown no longer waits for creation replies.
func (g *createGate) aborted() <-chan struct{} {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.abort == nil {
		g.abort = make(chan struct{})
	}
	return g.abort
}

func (g *createGate) stopWaiting() {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.abort == nil {
		g.abort = make(chan struct{})
	}
	if !g.ended {
		g.ended = true
		close(g.abort)
	}
}

func (g *createGate) inFlight() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.n
}

func (s *Server) createSettleBudget() time.Duration {
	if s.createSettle > 0 {
		return s.createSettle
	}
	return createSettleTimeout
}

// settleCreations is the first step of stopBackground: the HTTP service has
// stopped, the session, the target sync loop and the RPC log still work. No
// creation is sent from here on, and the tab_new requests still in flight get
// to finish their settlement (see the top of this file) so that their rollback
// reaches the browser and their audit line reaches the log.
//
// It never waits without bound. With trust (the connection is still to be
// asked) the wait covers one settlement and one rollback; after that, or at
// once when the connection is lost, the requests are told to stop waiting for
// their reply and get the time of one rollback to return. A request that is
// still running then is abandoned with a warning: its audit line may meet a
// closed log.
func (s *Server) settleCreations(trust bool) {
	done := s.creates.shut()
	if trust {
		if waitClosed(done, s.createSettleBudget()+createRollbackTimeout+createShutdownMargin) {
			return
		}
		if s.logger != nil {
			s.logger.Warn("tab_new requests in flight did not settle in time; they stop waiting for their creation reply", "in_flight", s.creates.inFlight())
		}
	}
	s.creates.stopWaiting()
	if !waitClosed(done, createRollbackTimeout+createShutdownMargin) && s.logger != nil {
		s.logger.Warn("tab_new requests still in flight are abandoned; a tab they created may stay open and unmanaged", "in_flight", s.creates.inFlight())
	}
}

type createReply struct {
	tid target.ID
	err error
}

// requestCreatedTarget sends Target.createTarget and waits for its reply.
// answered is false when the request ended first and no reply arrived within
// the settlement: nothing is known about the creation then, and the round trip
// is cancelled (a reply after that is dropped by the connection). The round
// trip itself runs on a context that keeps the request's values but not its
// cancellation.
func (s *Server) requestCreatedTarget(ctx context.Context, conn tabConn, url string, silent bool) (reply createReply, answered bool) {
	cctx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	defer cancel()
	ch := make(chan createReply, 1)
	go func() {
		tid, err := createPageTarget(cctx, conn, url, silent)
		ch <- createReply{tid: tid, err: err}
	}()
	select {
	case r := <-ch:
		return r, true
	case <-ctx.Done():
	}
	// A reply that is already here counts even when shutdown has stopped
	// waiting.
	select {
	case r := <-ch:
		return r, true
	default:
	}
	t := time.NewTimer(s.createSettleBudget())
	defer t.Stop()
	select {
	case r := <-ch:
		return r, true
	case <-t.C:
	case <-s.creates.aborted():
	}
	return createReply{}, false
}

// requestEndedHint says why a request's context is done.
func requestEndedHint(ctx context.Context) string {
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return fmt.Sprintf("the %ds limit of one RPC passed", timeout.OperationSeconds)
	}
	return "the request was cancelled (client disconnect or daemon shutdown)"
}
