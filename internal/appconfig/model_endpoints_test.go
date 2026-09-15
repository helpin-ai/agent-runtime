package appconfig

import (
	sdk "github.com/helpin-ai/agent-runtime-go"
	"testing"
)

func TestCompatibleEndpointsRequireAppApprovalAndExplicitHTTP(t *testing.T) {
	endpoint := ModelEndpoint{ID: "local", BaseURL: "http://127.0.0.1:8081/v1", AuthMode: "none"}
	cfg := &Config{Apps: []App{{AppID: "helpin", ModelEndpoints: []ModelEndpoint{endpoint}}}}
	if err := Validate(cfg); err == nil {
		t.Fatal("HTTP accepted without administrator opt-in")
	}
	cfg.Apps[0].ModelEndpoints[0].AllowHTTP = true
	if err := Validate(cfg); err != nil {
		t.Fatal(err)
	}
	binding := endpoint.binding()
	model := &sdk.RunModel{Provider: "openai_compatible", Model: "local-model", Endpoint: &binding}
	if err := ValidateRunModelEndpoint(cfg, "helpin", model); err != nil {
		t.Fatal(err)
	}
	if err := ValidateRunModelEndpoint(cfg, "other", model); err == nil {
		t.Fatal("another app inherited endpoint")
	}
	model.Endpoint.AuthMode = "api_key"
	if err := ValidateRunModelEndpoint(cfg, "helpin", model); err == nil {
		t.Fatal("caller changed endpoint auth mode")
	}
	model.Endpoint.AuthMode, model.Endpoint.BaseURL = "none", "http://127.0.0.1:9999/v1"
	if err := ValidateRunModelEndpoint(cfg, "helpin", model); err == nil {
		t.Fatal("caller redirected endpoint")
	}
}
