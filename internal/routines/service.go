package routines

import "context"

// Service combines a Store and a Scheduler into the per-user operations the
// API exposes (CRUD and run history from the Store, run-now from the
// Scheduler).
type Service struct {
	*Store
	Scheduler *Scheduler
}

// RunNow starts userID's routine id immediately (see Scheduler.RunNow).
func (s Service) RunNow(ctx context.Context, userID, id int64) (Run, error) {
	if s.Scheduler == nil {
		return Run{}, ErrClosed
	}
	return s.Scheduler.RunNow(ctx, userID, id)
}
