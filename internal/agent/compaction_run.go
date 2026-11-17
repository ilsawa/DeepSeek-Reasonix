package agent

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// compactionBudget is one summarizer call. The local model answers a
// full-window prefix in about five minutes, and a shorter run deadline
// cancels that answer as it arrives. One kept call replaces the view.
// Another pass is not started from the same transcript.
const compactionBudget = 12 * time.Minute

// summaryAttemptBudget remains the per-call cap on a rescue that is already
// several times past the hard ceiling. Pressure no longer uses that ladder.
const summaryAttemptBudget = 4 * time.Minute

// maxRecoverySummaries caps a manual or overflow rescue that starts at least
// twice past the hard ceiling. Pressure is one call: a ladder of cancelled
// passes does not shrink the window.
const maxRecoverySummaries = 8

// SummaryError keeps the failure identity without changing the wire contract.
type SummaryError struct {
	Code  string
	Cause error
}

func (e *SummaryError) Error() string { return fmt.Sprintf("%s: %v", e.Code, e.Cause) }
func (e *SummaryError) Unwrap() error { return e.Cause }

var errSummaryEmpty = errors.New("summarizer returned empty output")
var errSummaryBudget = &SummaryError{Code: "summary_budget_exceeded", Cause: context.DeadlineExceeded}

// errSummaryAttempt is one call that used its own deadline while the rescue
// run is still alive. The committed folds stay; the next pass may start.
var errSummaryAttempt = &SummaryError{Code: "summary_attempt_exceeded", Cause: context.DeadlineExceeded}

func summaryError(err error) error {
	if err == nil {
		return nil
	}
	var typed *SummaryError
	var persistence *compactionPersistenceError
	if errors.As(err, &typed) || errors.As(err, &persistence) || errors.Is(err, context.Canceled) {
		return err
	}
	var code string
	switch {
	case errors.Is(err, errSummaryEmpty):
		code = "summary_empty"
	case errors.Is(err, errSummaryOutputTruncated):
		code = "summary_output_truncated"
	case errors.Is(err, errCheckpointRejected):
		code = "summary_no_reduction"
	case errors.Is(err, errCompressStaleContext):
		code = "summary_context_changed"
	default:
		return err
	}
	return &SummaryError{Code: code, Cause: err}
}

// Only the summary request owner may classify an otherwise untyped provider
// failure. Entry-point validation, extensions, and persistence keep their errors.
func summaryRequestError(ctx context.Context, err error) error {
	err = summaryError(compactionError(ctx, err))
	var typed *SummaryError
	var persistence *compactionPersistenceError
	if err == nil || errors.As(err, &typed) || errors.As(err, &persistence) || errors.Is(err, context.Canceled) {
		return err
	}
	return &SummaryError{Code: "summary_provider_error", Cause: err}
}

type compactionRunKey struct{}
type compactionRun struct{ work context.Context }

func currentCompactionRun(ctx context.Context) *compactionRun {
	run, _ := ctx.Value(compactionRunKey{}).(*compactionRun)
	return run
}

// compactionRunBudget is one summarizer call. A view several times past the
// ceiling used to stretch this by the pass count. Those extra passes were
// cancelled before the local model answered, so the run stays one call.
func (a *Agent) compactionRunBudget() time.Duration {
	return compactionBudget
}

// Every nested summary, replan, and chunk inherits the same deadline. The
// caller's ordinary answer context is not cancelled by this work budget.
func (a *Agent) beginCompactionRun(parent context.Context) (context.Context, func(error) error) {
	if parent == nil {
		parent = context.Background()
	}
	if run := currentCompactionRun(parent); run != nil {
		return run.work, func(err error) error { return err }
	}
	ctx, cancel := context.WithTimeoutCause(parent, a.compactionRunBudget(), errSummaryBudget)
	run := &compactionRun{}
	ctx = context.WithValue(ctx, compactionRunKey{}, run)
	run.work = ctx
	return ctx, func(err error) error {
		defer cancel()
		return compactionError(ctx, err)
	}
}

func compactionError(ctx context.Context, err error) error {
	if err == nil {
		return nil
	}
	var persistence *compactionPersistenceError
	if errors.As(err, &persistence) {
		return err
	}
	if errors.Is(err, context.DeadlineExceeded) && errors.Is(context.Cause(ctx), errSummaryBudget) && !errors.Is(err, errSummaryBudget) {
		if err == context.DeadlineExceeded { //nolint:errorlint // Only the bare sentinel can be replaced without discarding an enclosing error.
			return errSummaryBudget
		}
		// Keep any enclosing recovery/error identity, even when a work deadline
		// needs its budget classification added. An expired run alone is not
		// evidence that a later request-preparation error came from compaction.
		return fmt.Errorf("%w: %w", err, errSummaryBudget)
	}
	return err
}

type compactionPersistenceError struct{ error }

func (e *compactionPersistenceError) Unwrap() error { return e.error }
