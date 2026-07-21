package worker

import (
	"context"
	"errors"
	"fmt"
	"reflect"

	"github.com/sephriot/code-reviewer/internal/persistence/sqlite"
)

// Router dispatches a claimed job to the handler registered for its kind.
// Construct it with NewRouter so invalid handler registrations fail at startup.
type Router struct {
	handlers map[string]Handler
}

// NewRouter builds an immutable job-kind router. Each registered handler must
// be non-nil, including typed nil pointers and nil HandlerFuncs.
func NewRouter(handlers map[string]Handler) (*Router, error) {
	if len(handlers) == 0 {
		return nil, errors.New("at least one job handler is required")
	}
	copyOfHandlers := make(map[string]Handler, len(handlers))
	for kind, handler := range handlers {
		if isNilHandler(handler) {
			return nil, fmt.Errorf("handler for job kind %q is required", kind)
		}
		copyOfHandlers[kind] = handler
	}
	return &Router{handlers: copyOfHandlers}, nil
}

// Handle dispatches job to its registered handler. Unknown job kinds are
// permanent failures because retrying cannot create a matching handler.
func (r *Router) Handle(ctx context.Context, job sqlite.Job) error {
	if r == nil {
		return Permanent(errors.New("worker router is required"))
	}
	handler, found := r.handlers[job.Kind]
	if !found || isNilHandler(handler) {
		return Permanent(fmt.Errorf("unknown job kind %q", job.Kind))
	}
	return handler.Handle(ctx, job)
}

func isNilHandler(handler Handler) bool {
	if handler == nil {
		return true
	}
	value := reflect.ValueOf(handler)
	switch value.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice:
		return value.IsNil()
	default:
		return false
	}
}

var _ Handler = (*Router)(nil)
