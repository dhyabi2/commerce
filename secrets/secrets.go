// Package secrets is commerce's read side of the HOST's secret plane.
//
// One value, set once at embed time, read by whichever handler needs a
// deployment secret. It exists so a plugin reads secrets the way it reads
// everything else — through the host it is embedded in — instead of through an
// env fan-out (KMS → a k8s Secret → a pod env var → os.Getenv), which is three
// places for one value to go stale and a restart to pick up a rotation.
//
// An org's own material lives at orgs/<org>/<name> in the default environment —
// the address cloud's KMS gives every org (apps/kms OrgPath) and the one every
// other in-process reader uses. [Org] is the one way commerce reads it.
//
// nil is a supported state: a commerce with no reader installed reads nothing,
// and every caller treats that as "not configured".
package secrets

import (
	"context"
	"errors"
	"strings"
	"sync"

	"github.com/hanzoai/commerce/log"
	"github.com/hanzoai/namespace"
	"github.com/zap-proto/zip"
)

// Reader is the narrowest thing commerce needs: read one secret by reference.
// Commerce never writes, never lists, and never learns the transport — a host
// satisfies this in-process, over UDS, or from a map in a test.
type Reader interface {
	GetSecret(ctx context.Context, ref string) ([]byte, error)
}

var (
	mu sync.RWMutex
	r  Reader
)

// Set installs the host's reader. Called once, before routes register.
func Set(x Reader) {
	mu.Lock()
	defer mu.Unlock()
	r = x
}

// Get returns the reader, or nil when commerce runs standalone.
func Get() Reader {
	mu.RLock()
	defer mu.RUnlock()
	return r
}

// What a host's Reader wraps to say why a secret did not read, so a failure is reported as
// what it was and not as "empty": no such secret, no permission to read it, a read that timed
// out, or a value that would not parse. Any other error is reported as an error of the reader.
var (
	ErrNotFound = errors.New("secret not found")
	ErrDenied   = errors.New("permission denied")
	ErrTimeout  = errors.New("read timed out")
	ErrParse    = errors.New("value would not parse")
)

// Read reads a secret and trims it, and answers WHY when it cannot: no reader installed, the
// reader's error, or an empty value. The value is never part of an error. A failure is
// recorded (and logged once, until it changes or clears) so it is never silent.
func Read(ctx context.Context, ref string) (string, error) {
	rd := Get()
	if rd == nil {
		record(ref, "no secret reader installed")
		return "", errNoReader
	}
	b, err := rd.GetSecret(ctx, ref)
	v := strings.TrimSpace(string(b))
	switch {
	case err != nil:
		record(ref, reason(err))
		return "", err
	case v == "":
		record(ref, "empty value")
		return "", errEmpty
	}
	record(ref, "")
	return v, nil
}

var (
	errNoReader = errors.New("no secret reader installed")
	errEmpty    = errors.New("empty value")

	failMu sync.Mutex
	failed = map[string]string{}
)

// reason names a reader error without its message where the kind is known.
func reason(err error) string {
	var ne interface{ Timeout() bool }
	switch {
	case errors.Is(err, ErrNotFound):
		return "not found"
	case errors.Is(err, ErrDenied):
		return "permission denied"
	case errors.Is(err, ErrTimeout), errors.Is(err, context.DeadlineExceeded), errors.As(err, &ne) && ne.Timeout():
		return "timeout"
	case errors.Is(err, ErrParse):
		return "parse error"
	}
	msg := err.Error()
	if len(msg) > 120 {
		msg = msg[:120]
	}
	return "reader error: " + msg
}

// record keeps the last failure of ref and says so in the log when it changes.
func record(ref, why string) {
	failMu.Lock()
	defer failMu.Unlock()
	if failed[ref] == why {
		return
	}
	if why == "" {
		delete(failed, ref)
		log.Info("secrets: %s reads again", ref)
		return
	}
	failed[ref] = why
	log.Warn("secrets: %s does not read: %s", ref, why)
}

// Failure is the last reason ref did not read, or "" when it reads (or was never asked).
func Failure(ref string) string {
	failMu.Lock()
	defer failMu.Unlock()
	return failed[ref]
}

// Diagnose reads ref and says why it does not: "" when it reads a non-empty secret, else
// one of "no secret reader installed", "not found", "permission denied", "timeout",
// "parse error", "empty value" or "reader error: <what the reader said, truncated>".
func Diagnose(ctx context.Context, ref string) string {
	_, err := Read(ctx, ref)
	switch {
	case err == nil:
		return ""
	case err == errNoReader:
		return "no secret reader installed"
	case err == errEmpty:
		return "empty value"
	}
	return reason(err)
}

// String reads a secret and trims it. A failure returns "" for the callers that fall back,
// and is recorded and logged by Read, so an unreadable secret is never silently an empty one.
func String(ctx context.Context, ref string) string {
	v, _ := Read(ctx, ref)
	return v
}

// Org reads one of org's own secrets, orgs/<org>/<name>, and asks AS org.
//
// The host's store serves a ref naming a tenant only to a call acting for that
// same tenant (cloud apps/kms admit), and the org a call acts for rides its
// caller, never the ref. A request's caller is whoever sent it — the PAYER, when a
// customer pays a brand — so the read re-points that caller at org with
// zip.ActingAs, which names who is acting on the call. A call with no caller reads
// nothing: cloud's KMS answers an unstated call as its own brand, so "no caller"
// must not reach it. org is always one commerce resolved itself (the charging
// org, or the brand that owns the host), never a value a request supplied as a
// ref.
//
// "" for every failure: no reader, an org that is not already a clean namespace
// slug (namespace.Sanitize, the fold cloud keys every org's store by), a name
// that cannot form a ref, no such secret, an unreadable one.
func Org(ctx context.Context, org, name string) string {
	if org == "" || namespace.Sanitize(org) != org || name == "" || name != strings.Trim(name, "/ ") || strings.Contains(name, "@") {
		return ""
	}
	actx, err := as(ctx, org)
	if err != nil {
		return ""
	}
	return String(actx, "orgs/"+org+"/"+name)
}

// as is ctx acting for org: unchanged when its caller already acts for org, and
// re-pointed with zip.ActingAs when it acts for another. ActingAs refuses a
// context with no authenticated caller, which is the refusal wanted here.
func as(ctx context.Context, org string) (context.Context, error) {
	if zip.CallerOf(ctx).Org == org {
		return ctx, nil
	}
	return zip.ActingAs(ctx, org)
}
