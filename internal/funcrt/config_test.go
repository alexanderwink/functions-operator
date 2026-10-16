package funcrt

import (
	"encoding/json"
	"slices"
	"testing"
	"time"

	"github.com/aws/aws-lambda-go/events"
)

func envMap(m map[string]string) func(string) string {
	return func(k string) string { return m[k] }
}

func TestLoadConfigDefaults(t *testing.T) {
	cfg, err := LoadConfig(envMap(map[string]string{"FUNCRT_MODE": "service"}))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Mode != "service" || cfg.Bootstrap != "/var/task/bootstrap" || cfg.Concurrency != 1 ||
		cfg.Timeout != 30*time.Second || cfg.Port != 8080 || cfg.AdminPort != 9090 || cfg.Payload != "" {
		t.Fatalf("unexpected defaults: %+v", cfg)
	}
	if cfg.BasePort != 9001 {
		t.Fatalf("BasePort = %d", cfg.BasePort)
	}
}

func TestLoadConfigOverrides(t *testing.T) {
	cfg, err := LoadConfig(envMap(map[string]string{
		"FUNCRT_MODE": "job", "FUNCRT_BOOTSTRAP": "/x", "FUNCRT_CONCURRENCY": "4",
		"FUNCRT_TIMEOUT": "5s", "FUNCRT_PORT": "1", "FUNCRT_ADMIN_PORT": "2",
		"FUNCRT_PAYLOAD": `{"a":1}`, "AWS_LAMBDA_FUNCTION_NAME": "fn",
	}))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Mode != "job" || cfg.Bootstrap != "/x" || cfg.Concurrency != 4 || cfg.Timeout != 5*time.Second ||
		cfg.Port != 1 || cfg.AdminPort != 2 || cfg.Payload != `{"a":1}` || cfg.FunctionName != "fn" {
		t.Fatalf("unexpected: %+v", cfg)
	}
}

func TestLoadConfigRejectsBadMode(t *testing.T) {
	for _, mode := range []string{"", "bogus"} {
		if _, err := LoadConfig(envMap(map[string]string{"FUNCRT_MODE": mode})); err == nil {
			t.Errorf("mode %q accepted", mode)
		}
	}
}

func TestLoadConfigRejectsBadValues(t *testing.T) {
	for _, kv := range [][2]string{
		{"FUNCRT_CONCURRENCY", "0"}, {"FUNCRT_CONCURRENCY", "65"}, {"FUNCRT_CONCURRENCY", "x"},
		{"FUNCRT_TIMEOUT", "0s"}, {"FUNCRT_TIMEOUT", "abc"},
		{"FUNCRT_PORT", "x"}, {"FUNCRT_ADMIN_PORT", "-1"},
	} {
		env := map[string]string{"FUNCRT_MODE": "service", kv[0]: kv[1]}
		if _, err := LoadConfig(envMap(env)); err == nil {
			t.Errorf("%s=%s accepted", kv[0], kv[1])
		}
	}
}

func TestBootstrapEnv(t *testing.T) {
	got := BootstrapEnv([]string{"PATH=/bin", "FUNCRT_MODE=job", "FUNCRT_PORT=1", "FOO=a=b", "AWS_REGION=eu-west-1", "AWS_LAMBDA_FUNCTION_NAME=fn"})
	for _, w := range []string{"PATH=/bin", "FOO=a=b", "AWS_LAMBDA_FUNCTION_NAME=fn", "LAMBDA_TASK_ROOT=/var/task", "_HANDLER=bootstrap", "AWS_REGION=local"} {
		if !slices.Contains(got, w) {
			t.Errorf("missing %q in %v", w, got)
		}
	}
	for _, e := range got {
		if len(e) >= 7 && e[:7] == "FUNCRT_" {
			t.Errorf("FUNCRT_ leaked: %s", e)
		}
		if e == "AWS_REGION=eu-west-1" {
			t.Errorf("AWS_REGION not overridden")
		}
	}
}

func TestScheduledEventShape(t *testing.T) {
	now := time.Date(2026, 10, 16, 12, 0, 0, 0, time.UTC)
	var ev events.EventBridgeEvent
	if err := json.Unmarshal(ScheduledEvent(now, "abc"), &ev); err != nil {
		t.Fatal(err)
	}
	if ev.DetailType != "Scheduled Event" || ev.Source != "aws.events" || ev.ID != "abc" ||
		ev.AccountID != "000000000000" || ev.Region != "local" || !ev.Time.Equal(now) {
		t.Fatalf("unexpected: %+v", ev)
	}
}
