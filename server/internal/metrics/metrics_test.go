package metrics

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestHandlerExposesAllSeries(t *testing.T) {
	srv := httptest.NewServer(Handler())
	defer srv.Close()

	JobsScheduled.WithLabelValues("p", "proj").Inc()
	JobsRunning.Set(2)
	QueueDepth.WithLabelValues("queued").Set(3)
	AgentsOnline.Set(4)
	LogArchiveJobs.WithLabelValues("success").Inc()
	RetentionDroppedLogPartitions.Inc()
	WebhookDeliveries.WithLabelValues("github", "accepted").Inc()
	JobDurationSeconds.WithLabelValues("success").Observe(1.5)

	resp, err := http.Get(srv.URL)
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status: %d", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	out := string(body)

	want := []string{
		"gocdnext_jobs_scheduled_total",
		"gocdnext_jobs_running",
		"gocdnext_job_duration_seconds_bucket",
		"gocdnext_queue_depth",
		"gocdnext_agents_online",
		"gocdnext_log_archive_jobs_total",
		"gocdnext_retention_dropped_log_partitions_total",
		"gocdnext_webhook_deliveries_total",
		"go_goroutines",          // runtime collector wired
		"process_resident_memory", // process collector wired
	}
	for _, name := range want {
		if !strings.Contains(out, name) {
			t.Errorf("metric %q missing from /metrics output", name)
		}
	}
}

func TestJobStatusLabel(t *testing.T) {
	tests := []struct {
		in, want string
	}{
		{"success", "success"},
		{"failed", "failed"},
		{"cancelled", "cancelled"},
		{"skipped", "skipped"},
		{"running", "unknown"},
		{"", "unknown"},
		{"weird-future-state", "unknown"},
	}
	for _, tt := range tests {
		t.Run(tt.in, func(t *testing.T) {
			if got := JobStatusLabel(tt.in); got != tt.want {
				t.Errorf("JobStatusLabel(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}
