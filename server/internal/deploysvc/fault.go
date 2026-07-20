package deploysvc

import (
	"errors"
	"net/http"

	"github.com/gocdnext/gocdnext/server/internal/deploy"
	"github.com/gocdnext/gocdnext/server/internal/store"
)

// FaultKind classifies a Register failure so the HTTP handler maps it to a status
// WITHOUT string-matching the error. The zero value is Internal (a safe default).
type FaultKind int

const (
	FaultInternal      FaultKind = iota // 500
	FaultInvalid                        // 400 — bad input (fields/enums)
	FaultNotFound                       // 404 — application or cluster missing
	FaultForbidden                      // 403 — project not allowed the cluster
	FaultUnprocessable                  // 422 — multi-source, or can't validate (unreachable)
)

// Fault wraps a Register error with its classification.
type Fault struct {
	Kind FaultKind
	Err  error
}

func (f *Fault) Error() string { return f.Err.Error() }
func (f *Fault) Unwrap() error { return f.Err }

// classifyValidateErr maps a ValidateSingleSource failure (fetch/authz/multi-source)
// to a Fault kind. The fetch surfaces typed errors — the cluster authz sentinel, a
// missing cluster, and the HTTP status of a non-2xx Application read — so the
// mapping is by type, not message.
func classifyValidateErr(err error) *Fault {
	switch {
	case errors.Is(err, deploy.ErrMultiSource):
		return &Fault{Kind: FaultUnprocessable, Err: err}
	case errors.Is(err, store.ErrClusterNotAuthorized):
		return &Fault{Kind: FaultForbidden, Err: err}
	case errors.Is(err, store.ErrClusterNotFound):
		return &Fault{Kind: FaultNotFound, Err: err}
	}
	var se *store.ClusterAPIStatusError
	if errors.As(err, &se) {
		switch se.Status {
		case http.StatusNotFound:
			return &Fault{Kind: FaultNotFound, Err: err} // no such Application
		case http.StatusUnauthorized, http.StatusForbidden:
			return &Fault{Kind: FaultForbidden, Err: err}
		default:
			return &Fault{Kind: FaultUnprocessable, Err: err}
		}
	}
	// Unreachable / unknown transport failure — the target can't be validated now.
	return &Fault{Kind: FaultUnprocessable, Err: err}
}
