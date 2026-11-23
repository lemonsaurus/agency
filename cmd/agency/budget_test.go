package main

import (
	"testing"
	"time"

	"github.com/lemonsaurus/agency/internal/budget"
	"github.com/lemonsaurus/agency/internal/config"
)

func TestAdmissionLimitsOverrideOnlyWhatIsSet(t *testing.T) {
	cfg, err := config.Parse([]byte("[admission]\ncpu_share = 0.9\nreserve_gb = 4\nmax_wait = \"5m\"\njob_cores = 0.5\n"))
	if err != nil {
		t.Fatal(err)
	}
	got := admissionLimits(cfg.Admission)
	want := budget.DefaultLimits
	want.CPUShare = 0.9
	want.Reserve.Mem = 4 * budget.GB
	want.MaxWait = 5 * time.Minute
	want.JobCost.CPU = 0.5
	if got != want {
		t.Errorf("got %+v\nwant %+v", got, want)
	}
}
