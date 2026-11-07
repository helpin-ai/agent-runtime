package agentcore

import (
	"context"
	"errors"
	sdk "github.com/helpin-ai/agent-runtime-go"
)

var ErrSteeringConflict = errors.New("steering revision or command conflicts; reload before retrying")
var ErrSteeringTerminal = errors.New("cannot steer a terminal run")

type SteeringStore interface {
	CreateRunSteering(context.Context, string, string, sdk.SteerRunRequest) (*sdk.RunSteeringCommand, error)
	ListRunSteering(context.Context, string, string) ([]sdk.RunSteeringCommand, error)
	ApplyRunSteering(context.Context, string, string, int64) error
	RejectRunSteering(context.Context, string, string, int64) error
}
