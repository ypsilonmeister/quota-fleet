package chain

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"

	"github.com/ypsilonmeister/quota-fleet/internal/store"
)

// VerdictSchema is the only accepted "schema" value (docs/22 §7.1).
const VerdictSchema = "qf.review-verdict/1"

// MaxVerdictBytes bounds the verdict file.
const MaxVerdictBytes = 4 << 10

var commitRe = regexp.MustCompile(`^[0-9a-f]{40}$`)

var verdictKeys = []string{"schema", "run_id", "seq", "nonce", "verdict", "reviewed_commit"}

// ParseVerdict validates a verdict file against the step it must belong to and returns
// the artifact row (status captured or invalid). raw_sha256 is always the hash of raw.
func ParseVerdict(raw []byte, runID string, seq int, nonce string) store.ChainArtifact {
	sum := sha256.Sum256(raw)
	a := store.ChainArtifact{RunID: runID, Seq: seq, RawSHA256: hex.EncodeToString(sum[:])}
	invalid := func(format string, args ...any) store.ChainArtifact {
		a.Status = store.ChainArtifactInvalid
		a.Reason = fmt.Sprintf(format, args...)
		return a
	}
	if len(raw) > MaxVerdictBytes {
		return invalid("verdict file is %d bytes (max %d)", len(raw), MaxVerdictBytes)
	}
	fields, err := decodeStrictObject(raw)
	if err != nil {
		return invalid("%v", err)
	}
	for _, k := range verdictKeys {
		if _, ok := fields[k]; !ok {
			return invalid("missing key %q", k)
		}
	}
	var (
		schema, gotRun, gotNonce, verdict, commit string
		gotSeq                                    json.Number
	)
	for _, f := range []struct {
		key string
		dst *string
	}{{"schema", &schema}, {"run_id", &gotRun}, {"nonce", &gotNonce}, {"verdict", &verdict}, {"reviewed_commit", &commit}} {
		if err := json.Unmarshal(fields[f.key], f.dst); err != nil {
			return invalid("key %q must be a string", f.key)
		}
	}
	dec := json.NewDecoder(bytes.NewReader(fields["seq"]))
	dec.UseNumber()
	if err := dec.Decode(&gotSeq); err != nil {
		return invalid("key \"seq\" must be an integer")
	}
	seqVal, err := gotSeq.Int64()
	if err != nil {
		return invalid("key \"seq\" must be an integer")
	}
	switch {
	case schema != VerdictSchema:
		return invalid("schema %q is not %q", schema, VerdictSchema)
	case gotRun != runID:
		return invalid("run_id does not match this run")
	case seqVal != int64(seq):
		return invalid("seq %d does not match this step (%d)", seqVal, seq)
	case nonce == "" || gotNonce != nonce:
		return invalid("nonce does not match this review step")
	case verdict != store.ChainBranchOKToMerge && verdict != store.ChainBranchChangesNeeded:
		return invalid("verdict %q is not ok_to_merge or changes_needed", verdict)
	case !commitRe.MatchString(commit):
		return invalid("reviewed_commit must be 40 lowercase hex digits")
	}
	a.Status = store.ChainArtifactCaptured
	a.Verdict = verdict
	a.ReviewedCommit = commit
	return a
}

// decodeStrictObject decodes exactly one JSON object with no duplicate keys, no unknown
// keys and no trailing data, returning each value raw.
func decodeStrictObject(raw []byte) (map[string]json.RawMessage, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	tok, err := dec.Token()
	if err != nil {
		return nil, fmt.Errorf("not valid JSON: %v", err)
	}
	if d, ok := tok.(json.Delim); !ok || d != '{' {
		return nil, errors.New("verdict must be a JSON object")
	}
	allowed := map[string]bool{}
	for _, k := range verdictKeys {
		allowed[k] = true
	}
	out := map[string]json.RawMessage{}
	for dec.More() {
		kt, err := dec.Token()
		if err != nil {
			return nil, fmt.Errorf("not valid JSON: %v", err)
		}
		key, ok := kt.(string)
		if !ok {
			return nil, errors.New("not valid JSON: expected a key")
		}
		if !allowed[key] {
			return nil, fmt.Errorf("unknown key %q", key)
		}
		if _, dup := out[key]; dup {
			return nil, fmt.Errorf("duplicate key %q", key)
		}
		var v json.RawMessage
		if err := dec.Decode(&v); err != nil {
			return nil, fmt.Errorf("not valid JSON: %v", err)
		}
		out[key] = v
	}
	if _, err := dec.Token(); err != nil { // closing brace
		return nil, fmt.Errorf("not valid JSON: %v", err)
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return nil, errors.New("trailing data after the verdict object")
	}
	return out, nil
}
