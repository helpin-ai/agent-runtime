package appconfig

import (
	"errors"
	"fmt"
	"strings"

	sdk "github.com/helpin-ai/agent-runtime-go"
)

// ModelEndpoint approves one immutable destination/authentication binding.
// HTTP is opt-in for local/private installations; no run flag can enable it.
type ModelEndpoint struct {
	ID        string `json:"id" yaml:"id"`
	BaseURL   string `json:"base_url" yaml:"base_url"`
	AuthMode  string `json:"auth_mode" yaml:"auth_mode"`
	AllowHTTP bool   `json:"allow_http,omitempty" yaml:"allow_http,omitempty"`
}

func (e ModelEndpoint) binding() sdk.ModelEndpoint {
	return sdk.ModelEndpoint{ID: e.ID, BaseURL: e.BaseURL, AuthMode: e.AuthMode}
}

func validateModelEndpoints(app *App) error {
	seen := map[string]bool{}
	for _, endpoint := range app.ModelEndpoints {
		binding := endpoint.binding()
		if err := sdk.ValidateModelEndpoint(&binding); err != nil {
			return fmt.Errorf("app %s model endpoint: %w", app.AppID, err)
		}
		if seen[endpoint.ID] {
			return fmt.Errorf("app %s has duplicate model endpoint IDs", app.AppID)
		}
		seen[endpoint.ID] = true
		if strings.HasPrefix(endpoint.BaseURL, "http:") && !endpoint.AllowHTTP {
			return errors.New("HTTP model endpoints require administrator allow_http configuration")
		}
	}
	return nil
}

// ValidateRunModelEndpoint checks trusted app policy before admission and recovery.
func ValidateRunModelEndpoint(cfg *Config, appID string, model *sdk.RunModel) error {
	if model == nil || model.Provider != "openai_compatible" {
		return nil
	}
	if err := sdk.ValidateRunModel(model); err != nil {
		return err
	}
	if cfg != nil {
		for _, app := range cfg.Apps {
			if app.AppID != appID {
				continue
			}
			for _, endpoint := range app.ModelEndpoints {
				if endpoint.binding() == *model.Endpoint && (!strings.HasPrefix(endpoint.BaseURL, "http:") || endpoint.AllowHTTP) {
					return nil
				}
			}
		}
	}
	return errors.New("model endpoint is not approved for this app or its binding changed")
}

func modelEndpointSummaries(app App) []sdk.ModelEndpoint {
	var endpoints []sdk.ModelEndpoint
	for _, endpoint := range app.ModelEndpoints {
		endpoints = append(endpoints, endpoint.binding())
	}
	return endpoints
}
