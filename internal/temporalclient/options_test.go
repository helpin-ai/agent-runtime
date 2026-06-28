package temporalclient

import (
	"testing"
)

func TestBuildOptionsFromEnvDefaultsNamespace(t *testing.T) {
	t.Setenv("TEMPORAL_NAMESPACE", "")
	t.Setenv("TEMPORAL_API_KEY", "")
	t.Setenv("TEMPORAL_TLS_ENABLED", "")

	options := BuildOptionsFromEnv("localhost:7233")

	if options.HostPort != "localhost:7233" {
		t.Fatalf("expected host port localhost:7233, got %q", options.HostPort)
	}
	if options.Namespace != "default" {
		t.Fatalf("expected default namespace, got %q", options.Namespace)
	}
	if options.Credentials != nil {
		t.Fatal("did not expect credentials without TEMPORAL_API_KEY")
	}
	if options.ConnectionOptions.TLS != nil {
		t.Fatal("did not expect explicit TLS without TEMPORAL_TLS_ENABLED")
	}
}

func TestBuildOptionsFromEnvUsesAPIKeyCredentials(t *testing.T) {
	t.Setenv("TEMPORAL_NAMESPACE", "helpin-dev.fvm1q")
	t.Setenv("TEMPORAL_API_KEY", "test-api-key")
	t.Setenv("TEMPORAL_TLS_ENABLED", "")

	options := BuildOptionsFromEnv("eu-central-1.aws.api.temporal.io:7233")

	if options.Namespace != "helpin-dev.fvm1q" {
		t.Fatalf("expected configured namespace, got %q", options.Namespace)
	}
	if options.Credentials == nil {
		t.Fatal("expected API key credentials")
	}
}

func TestBuildOptionsFromEnvUsesExplicitTLS(t *testing.T) {
	t.Setenv("TEMPORAL_NAMESPACE", "")
	t.Setenv("TEMPORAL_API_KEY", "")
	t.Setenv("TEMPORAL_TLS_ENABLED", "true")
	t.Setenv("TEMPORAL_TLS_SERVER_NAME", "")

	options := BuildOptionsFromEnv("eu-central-1.aws.api.temporal.io:7233")

	if options.ConnectionOptions.TLS == nil {
		t.Fatal("expected TLS config")
	}
	if options.ConnectionOptions.TLS.ServerName != "eu-central-1.aws.api.temporal.io" {
		t.Fatalf("unexpected TLS server name %q", options.ConnectionOptions.TLS.ServerName)
	}
}

func TestBuildOptionsFromEnvUsesTLSServerNameOverride(t *testing.T) {
	t.Setenv("TEMPORAL_TLS_ENABLED", "1")
	t.Setenv("TEMPORAL_TLS_SERVER_NAME", "override.example.com")

	options := BuildOptionsFromEnv("eu-central-1.aws.api.temporal.io:7233")

	if options.ConnectionOptions.TLS == nil {
		t.Fatal("expected TLS config")
	}
	if options.ConnectionOptions.TLS.ServerName != "override.example.com" {
		t.Fatalf("unexpected TLS server name %q", options.ConnectionOptions.TLS.ServerName)
	}
}
