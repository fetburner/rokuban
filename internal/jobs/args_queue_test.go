package jobs_test

import (
	"testing"
	"time"

	"github.com/fetburner/rokuban/internal/jobq"
	"github.com/fetburner/rokuban/internal/jobs"
)

type siteBoundArgsWithoutQueueSite struct{}

func (siteBoundArgsWithoutQueueSite) Kind() string { return "site-bound-without-queue-site" }
func (siteBoundArgsWithoutQueueSite) Spec() jobq.Spec {
	return jobq.Spec{Queue: jobs.IngestQueue, Timeout: func(jobq.Args) time.Duration { return time.Second }}
}

func TestPhysicalQueueForRequiresQueueSiteOnlyForSiteBoundQueues(t *testing.T) {
	tests := []struct {
		name    string
		args    jobq.Args
		want    string
		wantErr bool
	}{
		{name: "rejects a site-bound queue without QueueSite", args: siteBoundArgsWithoutQueueSite{}, wantErr: true},
		{name: "qualifies an empty site as the default site", args: jobs.RecordSweepArgs{}, want: "watcher_default"},
		{name: "leaves a site-independent ruler queue unqualified", args: jobs.RulerPassArgs{Site: "tokyo"}, want: "ruler"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := jobs.PhysicalQueueFor(tt.args)
			if tt.wantErr {
				if err == nil {
					t.Fatal("PhysicalQueueFor() error = nil, want missing QueueSite error")
				}
				return
			}
			if err != nil {
				t.Fatalf("PhysicalQueueFor() error = %v", err)
			}
			if got != tt.want {
				t.Errorf("PhysicalQueueFor() = %q, want %q", got, tt.want)
			}
		})
	}
}
