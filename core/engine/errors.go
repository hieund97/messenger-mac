package engine

import (
	"errors"
	"go.mau.fi/mautrix-meta/pkg/messagix/dgw"
	"go.mau.fi/mautrix-meta/pkg/messagix/httpclient"
	"go.mau.fi/mautrix-meta/pkg/messagix/loginerrors"
	"go.mau.fi/mautrix-meta/pkg/messagix/types"
)

func classifyError(err error) (string, string) {
	switch {
	case errors.Is(err, httpclient.ErrAccountSuspended):
		return "auth_required", "account_suspended"
	case errors.Is(err, loginerrors.Checkpoint), errors.Is(err, loginerrors.Consent), errors.Is(err, loginerrors.Challenge),
		errors.Is(err, httpclient.ErrCheckpointRequired), errors.Is(err, httpclient.ErrConsentRequired), errors.Is(err, httpclient.ErrChallengeRequired):
		return "auth_required", "checkpoint"
	case errors.Is(err, types.ErrPleaseLogIn), dgw.IsUnauthorized(err), errors.Is(err, loginerrors.TokenInvalidated),
		errors.Is(err, httpclient.ErrTokenInvalidated):
		return "auth_required", "token_invalid"
	case errors.Is(err, httpclient.ErrRateLimited), errors.Is(err, loginerrors.RateLimited):
		return "disconnected", "rate_limited"
	default:
		return "disconnected", "network"
	}
}
