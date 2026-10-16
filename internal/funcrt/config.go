package funcrt

import (
	"encoding/json"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"
)

// Run modes.
const (
	ModeService = "service"
	ModeJob     = "job"
)

// MaxConcurrency is the CRD maximum for FUNCRT_CONCURRENCY.
const MaxConcurrency = 64

// DefaultBasePort is where worker Runtime API listeners start. With
// MaxConcurrency copies it stays clear of the default 8080/9090 ports.
const DefaultBasePort = 9001

// Config is funcrt's environment-driven configuration.
type Config struct {
	Mode         string
	Bootstrap    string
	Concurrency  int
	Timeout      time.Duration
	Port         int
	AdminPort    int
	BasePort     int
	Payload      string // job mode: JSON payload; empty means a scheduled event
	FunctionName string
}

// LoadConfig reads the FUNCRT_* variables through getenv.
func LoadConfig(getenv func(string) string) (Config, error) {
	cfg := Config{
		Mode:         getenv("FUNCRT_MODE"),
		Bootstrap:    "/var/task/bootstrap",
		Concurrency:  1,
		Timeout:      30 * time.Second,
		Port:         8080,
		AdminPort:    9090,
		BasePort:     DefaultBasePort,
		Payload:      getenv("FUNCRT_PAYLOAD"),
		FunctionName: getenv("AWS_LAMBDA_FUNCTION_NAME"),
	}
	if cfg.Mode != ModeService && cfg.Mode != ModeJob {
		return Config{}, fmt.Errorf("FUNCRT_MODE must be %q or %q, got %q", ModeService, ModeJob, cfg.Mode)
	}
	if v := getenv("FUNCRT_BOOTSTRAP"); v != "" {
		cfg.Bootstrap = v
	}
	if err := intVar(getenv, "FUNCRT_CONCURRENCY", &cfg.Concurrency); err != nil {
		return Config{}, err
	}
	if cfg.Concurrency < 1 || cfg.Concurrency > MaxConcurrency {
		return Config{}, fmt.Errorf("FUNCRT_CONCURRENCY must be 1..%d, got %d", MaxConcurrency, cfg.Concurrency)
	}
	if v := getenv("FUNCRT_TIMEOUT"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil {
			return Config{}, fmt.Errorf("FUNCRT_TIMEOUT: %w", err)
		}
		cfg.Timeout = d
	}
	if cfg.Timeout <= 0 {
		return Config{}, fmt.Errorf("FUNCRT_TIMEOUT must be positive, got %s", cfg.Timeout)
	}
	for name, dst := range map[string]*int{"FUNCRT_PORT": &cfg.Port, "FUNCRT_ADMIN_PORT": &cfg.AdminPort} {
		if err := intVar(getenv, name, dst); err != nil {
			return Config{}, err
		}
		if *dst < 1 || *dst > 65535 {
			return Config{}, fmt.Errorf("%s out of range: %d", name, *dst)
		}
	}
	if cfg.Payload != "" && !json.Valid([]byte(cfg.Payload)) {
		return Config{}, fmt.Errorf("FUNCRT_PAYLOAD is not valid JSON")
	}
	return cfg, nil
}

func intVar(getenv func(string) string, name string, dst *int) error {
	v := getenv(name)
	if v == "" {
		return nil
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return fmt.Errorf("%s: %w", name, err)
	}
	*dst = n
	return nil
}

// BootstrapEnv builds the bootstrap environment from environ (os.Environ
// format): everything except FUNCRT_* and the variables funcrt sets itself,
// plus the Lambda constants (AWS_REGION defaults to local only when unset). The worker adds AWS_LAMBDA_RUNTIME_API.
func BootstrapEnv(environ []string) []string {
	out := make([]string, 0, len(environ)+3)
	for _, e := range environ {
		switch {
		case strings.HasPrefix(e, "FUNCRT_"),
			strings.HasPrefix(e, "LAMBDA_TASK_ROOT="),
			strings.HasPrefix(e, "_HANDLER="),
			strings.HasPrefix(e, "AWS_LAMBDA_RUNTIME_API="):
		default:
			out = append(out, e)
		}
	}
	if !slices.ContainsFunc(out, func(e string) bool { return strings.HasPrefix(e, "AWS_REGION=") }) {
		out = append(out, "AWS_REGION=local")
	}
	return append(out, "LAMBDA_TASK_ROOT=/var/task", "_HANDLER=bootstrap")
}
