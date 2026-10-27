package compare

import (
	"testing"

	"github.com/tyk-swe/xlfp/pkg/fingerprint"
)

func TestDivergenceFirstObservationWinsEvenWhenUnusable(t *testing.T) {
	db := loadDB(t)
	target := rankFor(t, db, "deepseek-v3", 5)
	official := rankFor(t, db, "deepseek-v3", 9)
	id := target.Observations[0].ProbeID
	target.Observations = append([]fingerprint.Observation{{ProbeID: id, Error: "first attempt failed"}}, target.Observations...)
	d := ComputeDivergence(db, target, official)
	if d == nil {
		t.Fatal("expected remaining overlapping probes")
	}
	for _, p := range d.PerProbe {
		if p.ProbeID == id {
			t.Fatal("later duplicate replaced unusable first observation")
		}
	}
}

func TestDivergenceExcludesFailedUsage(t *testing.T) {
	db := loadDB(t)
	target := rankFor(t, db, "deepseek-v3", 5)
	official := rankFor(t, db, "deepseek-v3", 9)
	for i := range target.Observations {
		target.Observations[i].Error = "unreliable"
	}
	if d := ComputeDivergence(db, target, official); d != nil {
		t.Fatal("failed usage contributed to divergence")
	}
}
