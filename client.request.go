package net

const maxCanceledRequestIDs = 1024

type requestTracker[M any] struct {
	pending  map[any]*asyncMessage[M]
	canceled map[any]struct{}
}

func newRequestTracker[M any]() *requestTracker[M] {
	return &requestTracker[M]{
		pending:  make(map[any]*asyncMessage[M]),
		canceled: make(map[any]struct{}),
	}
}

func (tracker *requestTracker[M]) request(id any) (*asyncMessage[M], bool) {
	request, ok := tracker.pending[id]
	return request, ok
}

func (tracker *requestTracker[M]) containsCanceled(id any) bool {
	_, ok := tracker.canceled[id]
	return ok
}

func (tracker *requestTracker[M]) add(id any, request *asyncMessage[M]) {
	tracker.pending[id] = request
}

func (tracker *requestTracker[M]) cancel(id any, request *asyncMessage[M], response M, err error) bool {
	delete(tracker.pending, id)
	request.Response(response, err)
	if _, ok := tracker.canceled[id]; ok {
		return true
	}
	if len(tracker.canceled) >= maxCanceledRequestIDs {
		return false
	}
	tracker.canceled[id] = struct{}{}
	return true
}

func (tracker *requestTracker[M]) removeCanceled(response M) bool {
	for id, request := range tracker.pending {
		if err := request.contextErr(); err != nil {
			if !tracker.cancel(id, request, response, err) {
				return false
			}
		}
	}
	return true
}

func (tracker *requestTracker[M]) handleResponse(id any, response M) bool {
	if _, ok := tracker.canceled[id]; ok {
		delete(tracker.canceled, id)
		return true
	}
	request, ok := tracker.pending[id]
	if !ok {
		return false
	}
	delete(tracker.pending, id)
	if err := request.contextErr(); err != nil {
		var zero M
		request.Response(zero, err)
		return true
	}
	request.Response(response, nil)
	return true
}

func (tracker *requestTracker[M]) finish(response M, err error) {
	for _, request := range tracker.pending {
		requestErr := err
		if contextErr := request.contextErr(); contextErr != nil {
			requestErr = contextErr
		}
		request.Response(response, requestErr)
	}
}

func (tracker *requestTracker[M]) count() int {
	return len(tracker.pending)
}
