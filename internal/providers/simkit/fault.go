// Package simkit is what every provider simulator shares: faults on command, a seed
// behind every random choice, signed events posted to a webhook, and the HTTP plumbing
// of server and client. Each simulator keeps its own resources and its own types.
package simkit

import (
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/gengibrepower/cross-border-pay/internal/platform/httpapi"
)

// Op names an operation of a simulator, such as "create" or "refund".
type Op string

type FaultKind string

const (
	// Reject refuses the operation for good, as a provider that says no.
	Reject FaultKind = "reject"
	// FailBefore answers 500 without doing anything.
	FailBefore FaultKind = "fail_before"
	// TimeoutAfter does the work and then never answers: the ambiguous case of
	// section 4.3, where the provider acted and the caller cannot know it.
	TimeoutAfter FaultKind = "timeout_after"
	// Delay waits DelayMS before doing the work and answering normally.
	Delay FaultKind = "delay"
)

// Fault makes the next Count requests of Op fail as Kind, in the order the faults
// were added.
type Fault struct {
	Op      Op        `json:"op"`
	Kind    FaultKind `json:"fault"`
	Count   int       `json:"count"`
	DelayMS int64     `json:"delay_ms,omitempty"`
}

// HangLimit bounds a TimeoutAfter fault, in case the caller never gives up.
const HangLimit = time.Minute

const (
	MaxFaultCount = 1000
	MaxDelay      = time.Minute
)

// Faults is a simulator's queue of scripted failures, safe for concurrent use.
type Faults struct {
	ops        []Op
	rejectable []Op

	mu   sync.Mutex
	list []Fault
}

// NewFaults takes the simulator's operations and those of them that can be refused.
func NewFaults(ops, rejectable []Op) *Faults {
	return &Faults{ops: ops, rejectable: rejectable}
}

// Add schedules f after the faults already waiting.
func (q *Faults) Add(f *Fault) error {
	if err := q.validate(f); err != nil {
		return err
	}
	q.mu.Lock()
	q.list = append(q.list, *f)
	q.mu.Unlock()
	return nil
}

// Next takes one use of the oldest fault scheduled for op. With none, it returns the
// zero Fault, whose Kind runs as no fault at all.
func (q *Faults) Next(op Op) Fault {
	q.mu.Lock()
	defer q.mu.Unlock()
	for i := range q.list {
		if q.list[i].Op != op {
			continue
		}
		f := q.list[i]
		q.list[i].Count--
		if q.list[i].Count == 0 {
			q.list = append(q.list[:i], q.list[i+1:]...)
		}
		return f
	}
	return Fault{}
}

func (q *Faults) validate(f *Fault) error {
	if !slices.Contains(q.ops, f.Op) {
		names := make([]string, len(q.ops))
		for i, op := range q.ops {
			names[i] = string(op)
		}
		return fmt.Errorf("op must be one of %s, got %q", strings.Join(names, ", "), f.Op)
	}
	switch f.Kind {
	case Reject:
		if !slices.Contains(q.rejectable, f.Op) {
			return fmt.Errorf("%s cannot be rejected", f.Op)
		}
	case Delay:
		if d := time.Duration(f.DelayMS) * time.Millisecond; d <= 0 || d > MaxDelay {
			return fmt.Errorf("delay_ms must be between 1 and %d", MaxDelay.Milliseconds())
		}
	case FailBefore, TimeoutAfter:
	default:
		return fmt.Errorf("unknown fault %q", f.Kind)
	}
	if f.Kind != Delay && f.DelayMS != 0 {
		return errors.New("delay_ms only applies to the delay fault")
	}
	if f.Count < 1 || f.Count > MaxFaultCount {
		return fmt.Errorf("count must be between 1 and %d", MaxFaultCount)
	}
	return nil
}

// Before runs the part of a fault that happens before any work. It reports whether
// the handler should go on.
func Before(w http.ResponseWriter, r *http.Request, f *Fault) bool {
	switch f.Kind {
	case FailBefore:
		httpapi.WriteProblem(w, r, httpapi.NewProblem(http.StatusInternalServerError, CodeFailedFault, "injected failure"))
		return false
	case Delay:
		t := time.NewTimer(time.Duration(f.DelayMS) * time.Millisecond)
		defer t.Stop()
		select {
		case <-r.Context().Done():
			return false
		case <-t.C:
			return true
		}
	case Reject, TimeoutAfter:
		return true
	default:
		return true
	}
}

// After runs the part of a fault that happens once the work is done: TimeoutAfter
// never answers.
func After(r *http.Request, f *Fault) {
	if f.Kind == TimeoutAfter {
		hang(r)
	}
}

// hang waits for the caller to give up, or HangLimit, and then drops the connection.
// net/http treats ErrAbortHandler as a deliberate abort.
func hang(r *http.Request) {
	t := time.NewTimer(HangLimit)
	defer t.Stop()
	select {
	case <-r.Context().Done():
	case <-t.C:
	}
	panic(http.ErrAbortHandler)
}

// InjectFault is the handler of POST /control/faults.
func (q *Faults) InjectFault(w http.ResponseWriter, r *http.Request) {
	var f Fault
	if err := Decode(w, r, &f); err != nil {
		Invalid(w, r, err.Error())
		return
	}
	if err := q.Add(&f); err != nil {
		Invalid(w, r, err.Error())
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
