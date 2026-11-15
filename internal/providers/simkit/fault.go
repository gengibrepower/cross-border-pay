// Package simkit is what every provider simulator shares. Each simulator keeps its own
// resources and types.
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

type Op string

type FaultKind string

const (
	Reject     FaultKind = "reject"
	FailBefore FaultKind = "fail_before"
	// TimeoutAfter does the work and never answers: the ambiguous case of section 4.3.
	TimeoutAfter FaultKind = "timeout_after"
	Delay        FaultKind = "delay"
)

type Fault struct {
	Op      Op        `json:"op"`
	Kind    FaultKind `json:"fault"`
	Count   int       `json:"count"`
	DelayMS int64     `json:"delay_ms,omitempty"`
}

// HangLimit is for a caller that never gives up.
const HangLimit = time.Minute

const (
	MaxFaultCount = 1000
	MaxDelay      = time.Minute
)

type Faults struct {
	ops        []Op
	rejectable []Op

	mu   sync.Mutex
	list []Fault
}

func NewFaults(ops, rejectable []Op) *Faults {
	return &Faults{ops: ops, rejectable: rejectable}
}

func (q *Faults) Add(f *Fault) error {
	if err := q.validate(f); err != nil {
		return err
	}
	q.mu.Lock()
	q.list = append(q.list, *f)
	q.mu.Unlock()
	return nil
}

// Next returns the zero Fault, which runs as none, when nothing is scheduled.
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

// Before reports whether the handler should go on.
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

func After(r *http.Request, f *Fault) {
	if f.Kind == TimeoutAfter {
		hang(r)
	}
}

// hang: net/http treats ErrAbortHandler as a deliberate abort.
func hang(r *http.Request) {
	t := time.NewTimer(HangLimit)
	defer t.Stop()
	select {
	case <-r.Context().Done():
	case <-t.C:
	}
	panic(http.ErrAbortHandler)
}

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
