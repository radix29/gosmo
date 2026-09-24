package gosmo

// What these pin is the decoding the catalog forces on the caller, because in
// both cases the wrong answer is a plausible one that no live test would
// report as an error — it would simply produce a different object.

import (
	"testing"
	"time"
)

func TestDecodeMessageTypeValidationTellsTheTwoXMLFormsApart(t *testing.T) {
	cases := []struct {
		desc          string
		hasCollection bool
		want          MessageTypeValidation
	}{
		// validation_desc says BINARY for VALIDATION = NONE, not "NONE".
		{"BINARY", false, MessageTypeValidationNone},
		{"EMPTY", false, MessageTypeValidationEmpty},
		// The pair the catalog cannot tell apart on its own: both are XML.
		{"XML", false, MessageTypeValidationWellFormedXML},
		{"XML", true, MessageTypeValidationValidXML},
		// A value a later release adds must not be read as a validation the
		// message type does not have.
		{"SOMETHING_NEW", false, MessageTypeValidationNone},
	}
	for _, c := range cases {
		if got := decodeMessageTypeValidation(c.desc, c.hasCollection); got != c.want {
			t.Errorf("decodeMessageTypeValidation(%q, %v) = %q, want %q",
				c.desc, c.hasCollection, got, c.want)
		}
	}
}

func TestDecodeContractSenderReadsBothBitsAsAny(t *testing.T) {
	cases := []struct {
		initiator, target bool
		want              ContractSender
	}{
		{true, false, ContractSentByInitiator},
		{false, true, ContractSentByTarget},
		// Both set is SENT BY ANY. Read as either one alone, the contract
		// this scripts refuses half the messages the original carried.
		{true, true, ContractSentByAny},
		{false, false, ContractSentByAny},
	}
	for _, c := range cases {
		if got := decodeContractSender(c.initiator, c.target); got != c.want {
			t.Errorf("decodeContractSender(%v, %v) = %q, want %q",
				c.initiator, c.target, got, c.want)
		}
	}
}

func TestRouteLifetimeSecondsIsTheRemainingLifetime(t *testing.T) {
	if got := (&Route{}).LifetimeSeconds(); got != 0 {
		t.Errorf("a route with no lifetime reported %d seconds, want 0 — "+
			"CREATE ROUTE refuses LIFETIME = 0, so the clause has to be left out", got)
	}
	// An expired route is the case that must not come back negative: a
	// negative LIFETIME is a script the server refuses.
	expired := &Route{Expires: time.Now().UTC().Add(-time.Hour)}
	if got := expired.LifetimeSeconds(); got != 0 {
		t.Errorf("an expired route reported %d seconds, want 0", got)
	}
	live := &Route{Expires: time.Now().UTC().Add(time.Hour)}
	if got := live.LifetimeSeconds(); got < 3500 || got > 3600 {
		t.Errorf("a route expiring in an hour reported %d seconds, want ~3600", got)
	}
}

// A route read from the catalog counts down from the server's own measure of
// the time left, not from Expires against the client's clock. Expires here is
// a day out — a client clock wildly skewed from the server's — and the answer
// must still be the ten minutes the server reported, less what has elapsed.
func TestRouteLifetimeSecondsIgnoresClientClockSkew(t *testing.T) {
	r := &Route{
		Expires:     time.Now().UTC().Add(24 * time.Hour),
		remainingMs: 600_000,
		readAt:      time.Now().Add(-100 * time.Second),
	}
	if got := r.LifetimeSeconds(); got < 499 || got > 500 {
		t.Errorf("LifetimeSeconds = %d, want 500: 600 s left at the read, 100 s ago", got)
	}
	r.readAt = time.Now().Add(-time.Hour)
	if got := r.LifetimeSeconds(); got != 0 {
		t.Errorf("a route the server gave 600 s, read an hour ago, reported %d, want 0", got)
	}
}
