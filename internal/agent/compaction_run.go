package agent

import (
	"context"
	"errors"
	"fmt"
	"time"
)

const compactionBudget = 5 * time.Minute

// summaryAttemptBudget is one summarizer call. A full-window prefix on the
// local model takes about three minutes, so the attempt deadline sits above
// that and a finished call is kept. The ordinary five-minute run is unchanged.
const summaryAttemptBudget = 4 * time.Minute

// maxRecoverySummaries caps a rescue that starts at least twice past the hard
// ceiling. Four passes cannot walk that transcript back under the window, and
// those passes used to share one five-minute budget, so the second call was
// cancelled before the model answered.
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

// compactionRunBudget is five minutes until the view is at least twice the
// hard ceiling. Past that, each rescue pass gets its own attempt deadline.
// A modest overflow keeps the short budget the tests and ordinary retries use.
func (a *Agent) compactionRunBudget() time.Duration {
	if a == nil || a.sess.conversation == nil {
		return compactionBudget
	}
	est := a.estimatedVisibleRequestTokens(a.modelVisibleMessages())
	hard := a.hardInputCeiling()
	if hard <= 0 || est < hard*2 {
		return compactionBudget
	}
	passes := summariesFor(ContextPreparePolicy{Trigger: CompactionTriggerPressure}, est, hard)
	budget := time.Duration(passes) * summaryAttemptBudget
	if budget < compactionBudget {
		return compactionBudget
	}
	return budget
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
