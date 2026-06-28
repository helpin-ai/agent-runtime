package temporalclient

import (
	"crypto/tls"
	"net"
	"os"
	"strings"

	tclient "go.temporal.io/sdk/client"
)

// BuildOptionsFromEnv returns Temporal SDK client options for local or cloud deployments.
func BuildOptionsFromEnv(address string) tclient.Options {
	namespace := strings.TrimSpace(os.Getenv("TEMPORAL_NAMESPACE"))
	if namespace == "" {
		namespace = "default"
	}
	options := tclient.Options{
		HostPort:  strings.TrimSpace(address),
		Namespace: namespace,
	}

	if apiKey := strings.TrimSpace(os.Getenv("TEMPORAL_API_KEY")); apiKey != "" {
		options.Credentials = tclient.NewAPIKeyStaticCredentials(apiKey)
	}

	if temporalTLSEnabled() {
		options.ConnectionOptions.TLS = &tls.Config{
			ServerName: temporalTLSServerName(address, os.Getenv("TEMPORAL_TLS_SERVER_NAME")),
			MinVersion: tls.VersionTLS12,
		}
	}

	return options
}

func temporalTLSEnabled() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("TEMPORAL_TLS_ENABLED"))) {
	case "1", "true", "yes", "y", "on":
		return true
	default:
		return false
	}
}

func temporalTLSServerName(address, override string) string {
	if trimmed := strings.TrimSpace(override); trimmed != "" {
		return trimmed
	}
	host, _, err := net.SplitHostPort(strings.TrimSpace(address))
	if err == nil {
		return host
	}
	return strings.TrimSpace(address)
}
