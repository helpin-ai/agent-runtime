package workjobs

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/helpin-ai/agent-runtime/internal/agentcore"
	"github.com/helpin-ai/agent-runtime/internal/credentials"
)

// The worker template carries the credentials every worker run needs, so it
// is stored sealed with the runtime's credential key and bound to its job.
// A template without credentials may be stored unsealed when no key is
// configured.
const (
	workerPlain  byte = 0
	workerSealed byte = 1
)

func workerAAD(appID, jobID string) []byte {
	return []byte("work_job_worker\x00" + appID + "\x00" + jobID)
}

func hasCredentials(worker agentcore.WorkJobWorker) bool {
	if worker.ModelCredential != nil {
		return true
	}
	for _, server := range worker.MCPServers {
		if server.Credential != nil {
			return true
		}
	}
	return false
}

func sealWorker(key []byte, appID, jobID string, worker agentcore.WorkJobWorker) ([]byte, error) {
	raw, err := json.Marshal(worker)
	if err != nil {
		return nil, err
	}
	if len(key) == 0 {
		if hasCredentials(worker) {
			return nil, errors.New("work jobs with credentials require a configured credential encryption key")
		}
		return append([]byte{workerPlain}, raw...), nil
	}
	sealed, err := credentials.Seal(key, workerAAD(appID, jobID), raw)
	if err != nil {
		return nil, err
	}
	return append([]byte{workerSealed}, sealed...), nil
}

func openWorker(key []byte, appID, jobID string, stored []byte) (agentcore.WorkJobWorker, error) {
	var worker agentcore.WorkJobWorker
	if len(stored) == 0 {
		return worker, errors.New("work job has no worker template")
	}
	raw := stored[1:]
	switch stored[0] {
	case workerPlain:
	case workerSealed:
		var err error
		if raw, err = credentials.Open(key, workerAAD(appID, jobID), raw); err != nil {
			return worker, fmt.Errorf("open worker template: %w", err)
		}
	default:
		return worker, errors.New("unknown worker template format")
	}
	err := json.Unmarshal(raw, &worker)
	return worker, err
}

func normalizeWorker(worker agentcore.WorkJobWorker) (agentcore.WorkJobWorker, error) {
	worker.AgentID = strings.TrimSpace(worker.AgentID)
	worker.Target.Type = strings.TrimSpace(worker.Target.Type)
	worker.Target.ID = strings.TrimSpace(worker.Target.ID)
	if worker.AgentID == "" {
		return worker, errors.New("worker.agent_id is required")
	}
	if worker.Target.Type == "" || worker.Target.ID == "" {
		return worker, errors.New("worker.target.type and worker.target.id are required")
	}
	for _, tool := range worker.AllowedTools {
		if strings.TrimSpace(tool) == submitToolName {
			return worker, fmt.Errorf("%s is reserved for the runtime", submitToolName)
		}
	}
	return worker, nil
}
