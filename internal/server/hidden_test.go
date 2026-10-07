package server

import (
	"testing"
	"time"
)

func TestHiddenWatchAlertsOnDrift(t *testing.T) {
	h := newHiddenWatch()
	now := time.Unix(1_700_000_000, 0)
	for i := range hiddenMinSample {
		st, _ := h.observe("claude-sonnet-4.5", 4052+i-2, now)
		if st.Alert {
			t.Fatalf("alert on stable baseline: %+v", st)
		}
	}
	var changed bool
	var st hiddenStat
	for range hiddenWindow {
		var c bool
		st, c = h.observe("claude-sonnet-4.5", 4300, now)
		changed = changed || c
	}
	if !st.Alert || !changed || st.Drift != 248 {
		t.Fatalf("drift not flagged: %+v changed=%v", st, changed)
	}
}
