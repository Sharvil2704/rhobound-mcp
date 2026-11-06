// SPDX-License-Identifier: Apache-2.0

package report

import (
	"errors"
	"io"
	"math"
	"path/filepath"
	"unicode/utf8"

	"github.com/Sharvil2704/rhobound-mcp/internal/change"
	"github.com/Sharvil2704/rhobound-mcp/internal/contract"
	"github.com/Sharvil2704/rhobound-mcp/internal/infer"
	"github.com/Sharvil2704/rhobound-mcp/internal/otlp"
)

// Refusal is a coded refusal (defs.v1 Refusal; docs/mcp/ARCHITECTURE.md 5.8): what a command prints in place of its
// result when it refuses with --json, and what the MCP server returns, unchanged, in a tool result with isError. Code
// is a registered code; Reason is the message the text form prints, at most MaxReasonChars; Fix is the code's fix from
// the registry; Subject is the sanitized name it concerns (contract.SafeName), or ""; Numbers are finite values.
type Refusal struct {
	Code    string             `json:"code"`
	Reason  string             `json:"reason"`
	Fix     string             `json:"fix"`
	Subject string             `json:"subject,omitempty"`
	Numbers map[string]float64 `json:"numbers,omitempty"`
}

// MaxReasonChars is the most characters a refusal's reason holds (defs.v1 Refusal); a longer message is cut.
const MaxReasonChars = 600

// RefusalDoc is the document a refusing command prints on standard output with --json (refusal.v1): {"error": ...}.
// The exit status is the command's own: 1 for predict, infer, model, fit and diff-fit, as today.
type RefusalDoc struct {
	Error Refusal `json:"error"`
}

// WriteJSON writes {"error": r} as canonical JSON and a newline.
func (r Refusal) WriteJSON(w io.Writer) error {
	b, err := contract.Canonical(RefusalDoc{Error: r})
	if err != nil {
		return err
	}
	_, err = w.Write(append(b, '\n'))
	return err
}

// RefusalError is an error with a registered code, raised by the engine (engine.Predict) and by the commands and
// packages built on it: the base load above the utilization limit (model.utilization_observed), an operation this
// version cannot apply (change.unsupported_op), a fit of another version (fit.version), and so on. Code is a contract
// constant; Subject is raw (RefusalOf sanitizes it); Err is the message the text form prints as "rhobound: <message>",
// unchanged where rhobound-cli printed one before. The inference's refusals are infer.Refusal, which infer defines
// because it cannot import this package.
type RefusalError struct {
	Code    string
	Subject string
	Numbers map[string]float64
	Err     error
}

// Error returns the message.
func (e *RefusalError) Error() string { return e.Err.Error() }

// Unwrap returns the message's error.
func (e *RefusalError) Unwrap() error { return e.Err }

// RefusalOf returns the coded refusal an error carries, and false for an error with no code (a tool error, which a
// command reports as before, with nothing on standard output). It recognizes *RefusalError, *infer.Refusal, a change
// set's *change.Error and the reader's coded errors (*otlp.Error, *otlp.LimitError), anywhere in the error's chain;
// it takes the fix from the registry, sanitizes the subject, drops non-finite numbers and cuts the reason to
// MaxReasonChars. The reason is the whole message, as the text form prints it.
func RefusalOf(err error) (Refusal, bool) {
	if err == nil {
		return Refusal{}, false
	}
	var (
		code, subject string
		numbers       map[string]float64
	)
	var re *RefusalError
	var ir *infer.Refusal
	var oe *otlp.Error
	var le *otlp.LimitError
	var ce *change.Error
	switch {
	case errors.As(err, &ce):
		code, subject, numbers = ce.Finding.Code, ce.Finding.Subject, ce.Finding.Numbers
	case errors.As(err, &re):
		code, subject, numbers = re.Code, re.Subject, re.Numbers
	case errors.As(err, &ir):
		code, subject, numbers = ir.Code, ir.Subject, ir.Numbers
	case errors.As(err, &oe):
		code, subject = oe.Code, filepath.Base(oe.Path)
		numbers = map[string]float64{}
		if oe.Line > 0 {
			numbers["line"] = float64(oe.Line)
		}
		if oe.Value >= 0 {
			numbers["value"] = float64(oe.Value)
		}
	case errors.As(err, &le):
		code, numbers = le.Code, map[string]float64{"limit": float64(le.Limit)}
	default:
		return Refusal{}, false
	}
	c, ok := contract.Lookup(code)
	if !ok {
		return Refusal{}, false
	}
	r := Refusal{Code: c.ID, Reason: cutReason(err.Error()), Fix: c.Fix}
	if subject != "" {
		r.Subject, _ = contract.SafeName(subject)
	}
	for k, v := range numbers {
		if math.IsNaN(v) || math.IsInf(v, 0) {
			continue
		}
		if r.Numbers == nil {
			r.Numbers = map[string]float64{}
		}
		r.Numbers[k] = v
	}
	return r, true
}

// cutReason cuts a message to MaxReasonChars characters, ending a cut one with "...".
func cutReason(s string) string {
	if utf8.RuneCountInString(s) <= MaxReasonChars {
		return s
	}
	r := []rune(s)
	return string(r[:MaxReasonChars-3]) + "..."
}

// WriteRefusal writes the coded refusal an error carries as {"error": ...} and a newline, and reports whether it had
// one; an error with no code writes nothing.
func WriteRefusal(w io.Writer, err error) (bool, error) {
	r, ok := RefusalOf(err)
	if !ok {
		return false, nil
	}
	return true, r.WriteJSON(w)
}
