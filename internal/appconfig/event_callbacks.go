package appconfig

import (
	"net/http"

	"github.com/helpin-ai/agent-runtime/internal/engine"
)

// EventCallbackSink builds the app-scoped HTTP delivery sink from the resolved
// application configuration. Apps without event_callbacks do not receive HTTP
// events; global log/NATS delivery is configured independently.
func EventCallbackSink(cfg *Config, client *http.Client) engine.EventSink {
	if cfg == nil {
		return engine.NoopEventSink{}
	}
	routes := make([]engine.AppCallbackRoute, 0)
	for _, app := range cfg.Apps {
		for _, callback := range app.EventCallbacks {
			routes = append(routes, engine.AppCallbackRoute{
				AppID:      app.AppID,
				URL:        callback.URL,
				Token:      callback.Token,
				EventTypes: callback.EventTypes,
			})
		}
	}
	if len(routes) == 0 {
		return engine.NoopEventSink{}
	}
	return engine.NewAppCallbackEventSink(routes, client)
}
