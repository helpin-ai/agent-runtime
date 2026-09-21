package tools

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	kernel "github.com/kernel/kernel-go-sdk"
	"github.com/kernel/kernel-go-sdk/option"
)

type kernelBrowserSession struct {
	SessionID string
	CDPWSURL  string
}

type kernelReplayStart struct {
	ReplayID string
	Started  time.Time
}

type kernelBrowserCreateRequest struct {
	Name           string
	Headless       bool
	Stealth        bool
	TimeoutSeconds int
	ViewportWidth  int
	ViewportHeight int
}

type kernelReplayStartRequest struct {
	Framerate          int
	MaxDurationSeconds int
	RecordAudio        bool
}

type kernelBrowserProvider interface {
	CreateBrowser(ctx context.Context, request kernelBrowserCreateRequest) (*kernelBrowserSession, error)
	DeleteBrowser(ctx context.Context, sessionID string) error
	StartReplay(ctx context.Context, sessionID string, request kernelReplayStartRequest) (*kernelReplayStart, error)
	StopReplay(ctx context.Context, sessionID, replayID string) error
	DownloadReplay(ctx context.Context, sessionID, replayID string) (io.ReadCloser, int64, error)
}

type sdkKernelBrowserProvider struct {
	client kernel.Client
}

func newSDKKernelBrowserProvider(apiKey, baseURL string) *sdkKernelBrowserProvider {
	opts := []option.RequestOption{option.WithAPIKey(strings.TrimSpace(apiKey))}
	if baseURL = strings.TrimSpace(baseURL); baseURL != "" {
		opts = append(opts, option.WithBaseURL(baseURL))
	}
	return &sdkKernelBrowserProvider{client: kernel.NewClient(opts...)}
}

func (p *sdkKernelBrowserProvider) CreateBrowser(ctx context.Context, request kernelBrowserCreateRequest) (*kernelBrowserSession, error) {
	params := kernel.BrowserNewParams{
		Name:           kernel.String(request.Name),
		Headless:       kernel.Bool(request.Headless),
		Stealth:        kernel.Bool(request.Stealth),
		TimeoutSeconds: kernel.Int(int64(request.TimeoutSeconds)),
		Viewport: kernel.BrowserViewportParam{
			Width: int64(request.ViewportWidth), Height: int64(request.ViewportHeight),
		},
		Tags: kernel.Tags{"owner": "agent-runtime"},
	}
	browser, err := p.client.Browsers.New(ctx, params)
	if err != nil {
		var apiErr *kernel.Error
		if !errors.As(err, &apiErr) || apiErr.StatusCode != http.StatusConflict {
			return nil, safeKernelOperationError("create Kernel browser", err)
		}
		existing, getErr := p.client.Browsers.Get(ctx, request.Name, kernel.BrowserGetParams{})
		if getErr != nil {
			return nil, safeKernelOperationError("recover existing Kernel browser", getErr)
		}
		return checkedKernelBrowserSession(existing.SessionID, existing.CdpWsURL)
	}
	return checkedKernelBrowserSession(browser.SessionID, browser.CdpWsURL)
}

func (p *sdkKernelBrowserProvider) DeleteBrowser(ctx context.Context, sessionID string) error {
	if err := p.client.Browsers.DeleteByID(ctx, strings.TrimSpace(sessionID)); err != nil {
		var apiErr *kernel.Error
		if errors.As(err, &apiErr) && apiErr.StatusCode == http.StatusNotFound {
			return nil
		}
		return safeKernelOperationError("delete Kernel browser", err)
	}
	return nil
}

func (p *sdkKernelBrowserProvider) StartReplay(ctx context.Context, sessionID string, request kernelReplayStartRequest) (*kernelReplayStart, error) {
	replay, err := p.client.Browsers.Replays.Start(ctx, strings.TrimSpace(sessionID), kernel.BrowserReplayStartParams{
		Framerate:            kernel.Int(int64(request.Framerate)),
		MaxDurationInSeconds: kernel.Int(int64(request.MaxDurationSeconds)),
		RecordAudio:          kernel.Bool(request.RecordAudio),
	})
	if err != nil {
		return nil, safeKernelOperationError("start Kernel replay", err)
	}
	if strings.TrimSpace(replay.ReplayID) == "" {
		return nil, fmt.Errorf("start Kernel replay: response is missing replay_id")
	}
	return &kernelReplayStart{ReplayID: replay.ReplayID, Started: replay.StartedAt}, nil
}

func (p *sdkKernelBrowserProvider) StopReplay(ctx context.Context, sessionID, replayID string) error {
	err := p.client.Browsers.Replays.Stop(ctx, strings.TrimSpace(replayID), kernel.BrowserReplayStopParams{
		ID: strings.TrimSpace(sessionID),
	})
	if err != nil {
		var apiErr *kernel.Error
		// Kernel automatically stops a replay at max_duration. Treat the
		// resulting conflict as already stopped so its MP4 can still be fetched.
		if errors.As(err, &apiErr) && apiErr.StatusCode == http.StatusConflict {
			return nil
		}
		return safeKernelOperationError("stop Kernel replay", err)
	}
	return nil
}

func (p *sdkKernelBrowserProvider) DownloadReplay(ctx context.Context, sessionID, replayID string) (io.ReadCloser, int64, error) {
	response, err := p.client.Browsers.Replays.Download(ctx, strings.TrimSpace(replayID), kernel.BrowserReplayDownloadParams{
		ID: strings.TrimSpace(sessionID),
	})
	if err != nil {
		return nil, 0, safeKernelOperationError("download Kernel replay", err)
	}
	if response == nil || response.Body == nil {
		return nil, 0, fmt.Errorf("download Kernel replay: response body is empty")
	}
	return response.Body, response.ContentLength, nil
}

func checkedKernelBrowserSession(sessionID, cdpWSURL string) (*kernelBrowserSession, error) {
	sessionID = strings.TrimSpace(sessionID)
	cdpWSURL = strings.TrimSpace(cdpWSURL)
	if sessionID == "" || cdpWSURL == "" {
		return nil, fmt.Errorf("Kernel browser response is missing session_id or cdp_ws_url")
	}
	return &kernelBrowserSession{SessionID: sessionID, CDPWSURL: cdpWSURL}, nil
}

type kernelOperationError struct {
	operation         string
	statusCode        int
	creditUnavailable bool
	cause             error
}

func (e *kernelOperationError) Error() string {
	if e.statusCode > 0 {
		return fmt.Sprintf("%s failed with HTTP %d", e.operation, e.statusCode)
	}
	return e.operation + " failed"
}

func (e *kernelOperationError) Unwrap() error { return e.cause }

// safeKernelOperationError keeps API request URLs, session/replay IDs, and CDP
// credentials out of model-visible tool errors while preserving the cause for
// internal errors.Is/errors.As handling.
func safeKernelOperationError(operation string, cause error) error {
	var apiErr *kernel.Error
	statusCode := 0
	creditUnavailable := false
	if errors.As(cause, &apiErr) {
		statusCode = apiErr.StatusCode
		creditUnavailable = kernelAPIErrorIndicatesUnavailableCredit(apiErr)
	}
	return &kernelOperationError{operation: operation, statusCode: statusCode, creditUnavailable: creditUnavailable, cause: cause}
}

func kernelCreditUnavailable(err error) bool {
	var operationErr *kernelOperationError
	return errors.As(err, &operationErr) && operationErr.creditUnavailable
}

func kernelAPIErrorIndicatesUnavailableCredit(apiErr *kernel.Error) bool {
	if apiErr == nil {
		return false
	}
	if apiErr.StatusCode == http.StatusPaymentRequired {
		return true
	}
	if apiErr.StatusCode < 400 || apiErr.StatusCode >= 500 {
		return false
	}
	message := strings.ToLower(apiErr.RawJSON())
	for _, marker := range []string{"credit", "billing", "payment", "balance"} {
		if strings.Contains(message, marker) {
			return true
		}
	}
	return false
}
