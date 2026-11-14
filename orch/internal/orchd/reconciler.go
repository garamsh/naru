package orchd

import "context"

// Reconciler drives derived state. Orchd runs it only while this member leads, with a context
// that ends when leadership is lost, so at most one member writes derived state at a time. Run
// returns when ctx ends or on a failure, after which orchd runs it again while the member still
// leads.
type Reconciler interface {
	Run(ctx context.Context) error
}
