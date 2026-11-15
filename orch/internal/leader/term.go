package leader

import (
	"context"
	"errors"

	clientv3 "go.etcd.io/etcd/client/v3"
)

// ErrTermLost is the failure of a write or a host call made after the leader's term ended: another
// leader holds a later term. The term ends when it is returned.
var ErrTermLost = errors.New("the leadership term has ended")

// errNoTerm is the failure of a leader write whose context carries no term.
var errNoTerm = errors.New("a leader write needs the context of a leadership term")

// Term is one leadership of one member. Its Token is the revision at which the member's election
// key was created, which only grows from one term to the next (ADR 0071).
type Term struct {
	// Token is the fencing token: the CreateRevision of the election key.
	Token int64
	key   string
	end   context.CancelCauseFunc
}

type termKey struct{}

// NewTerm returns a context derived from ctx that carries a term with the token and election key,
// and the function that ends it.
func NewTerm(ctx context.Context, token int64, key string) (context.Context, context.CancelCauseFunc) {
	led, end := context.WithCancelCause(ctx)
	return context.WithValue(led, termKey{}, &Term{Token: token, key: key, end: end}), end
}

// TermFrom returns the term ctx carries.
func TermFrom(ctx context.Context) (*Term, bool) {
	t, ok := ctx.Value(termKey{}).(*Term)
	return t, ok
}

// Lost ends the term: the context it was made for is cancelled with ErrTermLost.
func (t *Term) Lost() { t.end(ErrTermLost) }

// Guard returns kv with its writes made to hold only while the term in the context of the call
// still holds the election key it was made with. Reads pass through. A write that fails the guard
// ends the term and returns ErrTermLost; a write whose context carries no term fails.
func Guard(kv clientv3.KV) clientv3.KV { return guarded{KV: kv} }

type guarded struct{ clientv3.KV }

func (g guarded) Put(ctx context.Context, key, val string, opts ...clientv3.OpOption) (*clientv3.PutResponse, error) {
	resp, err := g.Txn(ctx).Then(clientv3.OpPut(key, val, opts...)).Commit()
	if err != nil {
		return nil, err
	}
	return (*clientv3.PutResponse)(resp.Responses[0].GetResponsePut()), nil
}

func (g guarded) Delete(ctx context.Context, key string, opts ...clientv3.OpOption) (*clientv3.DeleteResponse, error) {
	resp, err := g.Txn(ctx).Then(clientv3.OpDelete(key, opts...)).Commit()
	if err != nil {
		return nil, err
	}
	return (*clientv3.DeleteResponse)(resp.Responses[0].GetResponseDeleteRange()), nil
}

// Do runs a read as it is and a put or a delete as a guarded write.
func (g guarded) Do(ctx context.Context, op clientv3.Op) (clientv3.OpResponse, error) {
	switch {
	case op.IsGet():
		return g.KV.Do(ctx, op)
	case op.IsPut():
		resp, err := g.Txn(ctx).Then(op).Commit()
		if err != nil {
			return clientv3.OpResponse{}, err
		}
		return (*clientv3.PutResponse)(resp.Responses[0].GetResponsePut()).OpResponse(), nil
	case op.IsDelete():
		resp, err := g.Txn(ctx).Then(op).Commit()
		if err != nil {
			return clientv3.OpResponse{}, err
		}
		return (*clientv3.DeleteResponse)(resp.Responses[0].GetResponseDeleteRange()).OpResponse(), nil
	}
	return clientv3.OpResponse{}, errors.New("a guarded client runs gets, puts and deletes through Do")
}

func (g guarded) Txn(ctx context.Context) clientv3.Txn { return &guardedTxn{g: g, ctx: ctx} }

// guardedTxn is a transaction nested in the guard: it runs as the Then of one that compares the
// election key, so a lost term and a failed comparison of the caller are told apart.
type guardedTxn struct {
	g                guarded
	ctx              context.Context
	cmps             []clientv3.Cmp
	thenOps, elseOps []clientv3.Op
}

func (t *guardedTxn) If(cs ...clientv3.Cmp) clientv3.Txn { t.cmps = append(t.cmps, cs...); return t }
func (t *guardedTxn) Then(ops ...clientv3.Op) clientv3.Txn {
	t.thenOps = append(t.thenOps, ops...)
	return t
}
func (t *guardedTxn) Else(ops ...clientv3.Op) clientv3.Txn {
	t.elseOps = append(t.elseOps, ops...)
	return t
}

func (t *guardedTxn) Commit() (*clientv3.TxnResponse, error) {
	term, ok := TermFrom(t.ctx)
	if !ok {
		return nil, errNoTerm
	}
	outer, err := t.g.KV.Txn(t.ctx).
		If(clientv3.Compare(clientv3.CreateRevision(term.key), "=", term.Token)).
		Then(clientv3.OpTxn(t.cmps, t.thenOps, t.elseOps)).
		Commit()
	if err != nil {
		return nil, err
	}
	if !outer.Succeeded {
		term.Lost()
		return nil, ErrTermLost
	}
	inner := outer.Responses[0].GetResponseTxn()
	return &clientv3.TxnResponse{Header: outer.Header, Succeeded: inner.Succeeded, Responses: inner.Responses}, nil
}
