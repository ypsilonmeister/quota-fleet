package chain

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"

	"github.com/ypsilonmeister/quota-fleet/internal/admission"
	"github.com/ypsilonmeister/quota-fleet/internal/store"
)

// NewNonce returns a 128-bit random hex string (verdict nonce, run IDs).
func NewNonce() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(b[:]), nil
}

// NewRunID returns a UUIDv4 for a chain run.
func NewRunID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%08x-%04x-%04x-%04x-%012x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16]), nil
}

// PlanStep expands the step template and runs admission (Selector → Gate) without
// writing anything. A non-nil reject error means admission rejected the request itself
// (the run stops with admission_rejected); err is an internal failure.
func PlanStep(ctx context.Context, adm *admission.Admission, d *Definition, in StepInput) (w store.ChainStepWrite, reject error, err error) {
	if in.Step == store.ChainStepReview && in.VerdictNonce == "" {
		if in.VerdictNonce, err = NewNonce(); err != nil {
			return w, nil, err
		}
	}
	params, perr := BuildStepParams(d, in)
	if perr != nil {
		return w, perr, nil
	}
	dec := adm.Decide(ctx, admission.Request{Params: params, Role: in.Step})
	switch dec.Kind {
	case admission.KindReject:
		return w, dec.Reject, nil
	case admission.KindHold:
		w = store.ChainStepWrite{
			Params:              dec.Params,
			Hold:                true,
			HoldPool:            dec.HoldPool,
			HoldAltPools:        dec.HoldAltPools,
			HoldReason:          dec.HoldReason,
			HoldSnapshotSummary: dec.HoldSummary,
			Gate:                dec.Gate,
		}
	default:
		w = store.ChainStepWrite{Params: dec.Params, Gate: dec.Gate}
	}
	if in.Step == store.ChainStepReview {
		w.VerdictNonce = in.VerdictNonce
	}
	return w, nil, nil
}
