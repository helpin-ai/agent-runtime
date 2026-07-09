package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/helpin-ai/agent-runtime/internal/appconfig"
)

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	command := strings.TrimSpace(os.Args[1])
	cfg, err := loadConfig(argument(2))
	if err != nil {
		fatal(err)
	}
	switch command {
	case "validate":
		if err := appconfig.Validate(cfg); err != nil {
			fatal(err)
		}
		fmt.Printf("valid configuration: %d app(s)\n", len(cfg.Apps))
	case "doctor":
		if err := appconfig.Validate(cfg); err != nil {
			fatal(err)
		}
		appID := argument(3)
		if appID == "" && len(cfg.Apps) == 1 {
			appID = cfg.Apps[0].AppID
		}
		if appID == "" {
			fatal(fmt.Errorf("app_id is required when more than one app is configured"))
		}
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		health, err := appconfig.CheckApp(ctx, cfg, appID, nil)
		if err != nil {
			fatal(err)
		}
		body, _ := json.MarshalIndent(health, "", "  ")
		fmt.Println(string(body))
	default:
		usage()
		os.Exit(2)
	}
}

func loadConfig(path string) (*appconfig.Config, error) {
	if strings.TrimSpace(path) == "" {
		return appconfig.LoadFromEnv()
	}
	body, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	cfg, err := appconfig.Decode(string(body))
	if err != nil {
		return nil, err
	}
	appconfig.ResolveTokenEnv(cfg, os.Getenv)
	return cfg, nil
}

func argument(index int) string {
	if len(os.Args) <= index {
		return ""
	}
	return strings.TrimSpace(os.Args[index])
}

func usage() {
	fmt.Fprintln(os.Stderr, "usage: agent-runtime-config validate [config.json|config.yaml]")
	fmt.Fprintln(os.Stderr, "       agent-runtime-config doctor [config.json|config.yaml] [app_id]")
}

func fatal(err error) {
	fmt.Fprintln(os.Stderr, "error:", err)
	os.Exit(1)
}
