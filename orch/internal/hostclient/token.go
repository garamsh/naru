package hostclient

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"strconv"

	"github.com/garamsh/naru/lib/hostapi"
	"github.com/garamsh/naru/lib/wire"
	"github.com/garamsh/naru/orch/internal/leader"
)

// withToken returns a client that sends the leader token of the request's context with every call
// that is not a read, and ends the term when a host answers that it has seen a later one (ADR 0071).
// A call whose context carries no term is an operator's and goes as it is.
func withToken(hc *http.Client) *http.Client {
	c := *hc
	c.Transport = tokenTransport{next: hc.Transport}
	return &c
}

type tokenTransport struct{ next http.RoundTripper }

// maxRefusal bounds the body read to find a refusal's code.
const maxRefusal = 4 << 10

func (t tokenTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	next := t.next
	if next == nil {
		next = http.DefaultTransport
	}
	term, ok := leader.TermFrom(req.Context())
	if !ok || req.Method == http.MethodGet || req.Method == http.MethodHead {
		return next.RoundTrip(req)
	}
	req = req.Clone(req.Context())
	req.Header.Set(wire.LeaderTokenHeader, strconv.FormatInt(term.Token, 10))
	resp, err := next.RoundTrip(req)
	if err != nil || resp.StatusCode != http.StatusConflict {
		return resp, err
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxRefusal))
	_ = resp.Body.Close()
	resp.Body = io.NopCloser(bytes.NewReader(data))
	var eb hostapi.ErrorBody
	if err == nil && json.Unmarshal(data, &eb) == nil && eb.Code == hostapi.CodeStaleLeader {
		term.Lost()
	}
	return resp, nil
}
