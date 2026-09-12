package durable

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/helpin-ai/agent-runtime/internal/agentcore"
	"github.com/helpin-ai/agent-runtime/internal/engine"
	"github.com/stretchr/testify/mock"
	enumspb "go.temporal.io/api/enums/v1"
	taskqueuepb "go.temporal.io/api/taskqueue/v1"
	workflowservice "go.temporal.io/api/workflowservice/v1"
	"go.temporal.io/sdk/client"
	"go.temporal.io/sdk/mocks"
	"google.golang.org/protobuf/types/known/timestamppb"
)

func TestCodingQueueAdmissionAndRouting(t *testing.T) {
	for _, available := range []bool{false, true} {
		t.Run(map[bool]string{false: "no worker", true: "coding worker"}[available], func(t *testing.T) {
			c := new(mocks.Client)
			response := &workflowservice.DescribeTaskQueueResponse{}
			if available {
				response.Pollers = []*taskqueuepb.PollerInfo{{Identity: "coding-worker", LastAccessTime: timestamppb.New(time.Now())}}
			}
			c.On("DescribeTaskQueue", mock.Anything, TaskQueueName(QueueAgentNativeCoding), enumspb.TASK_QUEUE_TYPE_WORKFLOW).Return(response, nil).Once()
			if available {
				c.On("ExecuteWorkflow", mock.Anything, mock.MatchedBy(func(options client.StartWorkflowOptions) bool {
					return options.TaskQueue == TaskQueueName(QueueAgentNativeCoding)
				}), mock.Anything, mock.Anything).Return(nil, nil).Once()
			}
			err := NewRunEngine(c).StartRun(context.Background(), &agentcore.AgentRun{ID: "coding", RuntimeKind: agentcore.RuntimeNativeSDK, Input: agentcore.RunInput{Metadata: map[string]any{engine.CodingMetadataKey: true}}})
			if available && err != nil {
				t.Fatal(err)
			}
			if !available && !errors.Is(err, engine.ErrCodingUnavailable) {
				t.Fatalf("missing-worker admission=%v", err)
			}
			c.AssertExpectations(t)
		})
	}
}
