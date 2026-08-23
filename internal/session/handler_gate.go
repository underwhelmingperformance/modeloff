package session

import "sync"

type handlerGate struct {
	mu       sync.Mutex
	active   int
	stopping bool
	drained  chan struct{}
}

func newHandlerGate() handlerGate {
	return handlerGate{drained: make(chan struct{})}
}

func (g *handlerGate) enter() bool {
	g.mu.Lock()
	defer g.mu.Unlock()

	if g.stopping {
		return false
	}

	g.active++

	return true
}

func (g *handlerGate) leave() {
	g.mu.Lock()
	defer g.mu.Unlock()

	g.active--
	if g.stopping && g.active == 0 {
		close(g.drained)
	}
}

func (g *handlerGate) stop() <-chan struct{} {
	g.mu.Lock()
	defer g.mu.Unlock()

	if g.stopping {
		return g.drained
	}

	g.stopping = true
	if g.active == 0 {
		close(g.drained)
	}

	return g.drained
}
