package connectors

import "testing"

func TestProbeFailureRoundTrip(t *testing.T) {
	t.Parallel()
	err := NewProbeError(ProbeForbidden)
	failure, ok := ProbeFailureOf(err)
	if !ok || failure != ProbeForbidden || err.Error() != "forbidden" {
		t.Fatalf("failure=%q ok=%v err=%v", failure, ok, err)
	}
}
