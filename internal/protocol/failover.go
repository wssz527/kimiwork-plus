package protocol

import (
	"context"
	"errors"
	"net"
)

// Failover only covers failures a standby can actually fix: the connection never
// opened, or the upstream answered "not now" (429 and the 5xx family). A request
// is never re-sent once the upstream has answered, because it may already have
// been billed there.

// RetryableStatuses are the upstream statuses a standby may answer instead.
var RetryableStatuses = map[int]bool{
	429: true, // rate limited
	500: true, // internal error
	502: true, // bad gateway
	503: true, // unavailable
	504: true, // gateway timeout
	529: true, // vendor overloaded
}

// RetryableStatus reports whether a status means "this connection could not
// serve it now" rather than "this request is wrong".
func RetryableStatus(status int) bool { return RetryableStatuses[status] }

// A failed exchange can return *url.Error after the vendor has consumed the
// request. Only a dial failure proves that no request reached that connection.
func retryableError(err error) bool {
	if err == nil {
		return false
	}
	// A cancellation or deadline is the caller's decision, not a broken route.
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}
	var operation *net.OpError
	return errors.As(err, &operation) && operation.Op == "dial"
}

// attempt is one request's ordered candidates plus what it learned. The
// connection fields always name the candidate that really answered — a standby
// failure is never reported under the primary's identity.
type attempt struct {
	response     *UpstreamResponse
	usage        *UsageContext
	failure      error
	connectionID *string
	upstreamID   string
	upstreamWire *string
	// tried is how many candidates were attempted. failoverFrom names the
	// connection that was abandoned, and is set only when a retry really happened.
	tried        int
	failoverFrom *string
	// exhausted is true when a retry happened and the last candidate refused too;
	// the client still receives that last reply, but the record says the failover
	// itself ran out rather than pretending it was a single attempt.
	exhausted bool
	// unavailable is true when no configured connection exists in this process,
	// which is a local configuration answer rather than a transport failure.
	unavailable bool
}

// tryCandidates forwards one request through the model's ordered candidates.
// It returns as soon as a candidate produced a reply worth passing on, which is
// what guarantees no byte of a discarded reply was ever written downstream.
func (g *Gateway) tryCandidates(ctx context.Context, route string, document *Document,
	selected *Model, headers map[string]string, promptCacheKey string) *attempt {
	candidates := selected.Candidates()
	live := make([]Candidate, 0, len(candidates))
	for _, candidate := range candidates {
		if candidate.ConnectionID != nil && !g.config.Backend.Has(candidate.ConnectionID) {
			continue
		}
		live = append(live, candidate)
	}
	result := &attempt{upstreamID: selected.UpstreamID(), upstreamWire: selected.UpstreamProtocol}
	// A primary connection the live process does not hold is a configuration
	// problem — usually an unreadable credential — not a failure a standby may
	// paper over. Every release before failover answered it as
	// `connection_unavailable`, and silently spending at another vendor instead
	// would hide a broken key very well.
	if primary := candidates[0]; primary.ConnectionID != nil && !g.config.Backend.Has(primary.ConnectionID) {
		result.unavailable = true
		result.failure = ErrConnectionUnavailable
		return result
	}
	if len(live) == 0 {
		result.unavailable = true
		result.failure = ErrConnectionUnavailable
		return result
	}
	primary := ""
	if selected.ConnectionID != nil {
		primary = *selected.ConnectionID
	}
	retried := false
	for index, candidate := range live {
		// The exact model name is set per attempt: a standby may front the same
		// weights under a different id, and the document is rewritten, never
		// appended to.
		document.SetString("model", candidate.UpstreamID)
		result.tried++
		result.connectionID = candidate.ConnectionID
		result.upstreamID = candidate.UpstreamID
		result.upstreamWire = candidate.UpstreamProtocol
		if resolved := g.resolveUsageContext(route, selected, candidate, headers, promptCacheKey); resolved != nil {
			result.usage = resolved
		}
		// The attempt carries the candidate's own wire format. A standby that
		// speaks Messages while the primary speaks chat is served from the right
		// conversion path instead of being forced through the primary's.
		attemptModel := *selected
		attemptModel.UpstreamProtocol = candidate.UpstreamProtocol
		reply, err := g.config.Backend.Request(ctx, route, document.Bytes(), RequestOptions{
			Headers: headers, ConnectionID: candidate.ConnectionID, Model: &attemptModel})
		if err != nil {
			result.failure = err
			if ctx.Err() != nil || !retryableError(err) || index == len(live)-1 {
				if retried {
					result.failoverFrom = &primary
				}
				return result
			}
			retried = true
			continue
		}
		result.failure = nil
		if RetryableStatus(reply.Status) && index < len(live)-1 {
			// Nothing of this reply has been forwarded, so its body is closed
			// rather than read and the next candidate is tried.
			_ = reply.Body.Close()
			retried = true
			continue
		}
		if retried && RetryableStatus(reply.Status) {
			result.exhausted = true
		}
		if retried {
			// A standby really answered. The record must name the connection that
			// failed as well as the one that answered, or the user cannot tell that
			// a route is broken.
			result.failoverFrom = &primary
		}
		result.response = reply
		return result
	}
	return result
}

// resolveUsageContext asks the caller's resolver for the identity of one
// attempt. It runs per candidate because the provider account belongs to the
// connection that really answers, which may be the standby.
func (g *Gateway) resolveUsageContext(route string, selected *Model, candidate Candidate,
	headers map[string]string, promptCacheKey string) *UsageContext {
	if g.config.UsageSink == nil || g.config.ResolveUsageContext == nil {
		return nil
	}
	request := UsageContextRequest{Protocol: route, Model: selected.ID, ModelID: candidate.UpstreamID,
		ConnectionID: candidate.ConnectionID, UpstreamProtocol: candidate.UpstreamProtocol,
		Headers: headers, PromptCacheKey: promptCacheKey, At: g.config.Now().UnixMilli()}
	var resolved *UsageContext
	func() {
		defer func() { _ = recover() }()
		if context, err := g.config.ResolveUsageContext(request); err == nil {
			resolved = &context
		}
	}()
	return resolved
}
