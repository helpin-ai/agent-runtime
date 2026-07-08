package store

import (
	"context"
	"database/sql/driver"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"gorm.io/driver/postgres"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"

	"github.com/helpin-ai/agent-runtime/internal/agentcore"
	"github.com/helpin-ai/agent-runtime/internal/id"
)

type SQLConfig struct {
	Driver string
	DSN    string
}

type SQL struct {
	db *gorm.DB
}

func OpenSQL(cfg SQLConfig) (*SQL, error) {
	var dialector gorm.Dialector
	switch cfg.Driver {
	case "postgres", "postgresql":
		if cfg.DSN == "" {
			return nil, fmt.Errorf("postgres dsn is required")
		}
		dialector = postgres.Open(cfg.DSN)
	case "sqlite", "sqlite3", "":
		dsn := cfg.DSN
		if dsn == "" {
			dsn = "file:agent-runtime?mode=memory&cache=shared"
		}
		dialector = sqlite.Open(dsn)
	default:
		return nil, fmt.Errorf("unsupported sql driver %q", cfg.Driver)
	}
	db, err := gorm.Open(dialector, &gorm.Config{})
	if err != nil {
		return nil, err
	}
	return &SQL{db: db}, nil
}

func NewSQL(db *gorm.DB) *SQL {
	return &SQL{db: db}
}

func (s *SQL) DB() *gorm.DB {
	if s == nil {
		return nil
	}
	return s.db
}

func (s *SQL) AutoMigrate() error {
	if s == nil || s.db == nil {
		return fmt.Errorf("sql store is not configured")
	}
	return s.db.AutoMigrate(
		&agentRecord{},
		&runRecord{},
		&messageRecord{},
		&artifactRecord{},
		&interactionRecord{},
		&toolCallRecord{},
		&codexAuthTokenRecord{},
	)
}

type jsonBytes []byte

func (j jsonBytes) Value() (driver.Value, error) {
	if len(j) == 0 {
		return "{}", nil
	}
	return string(j), nil
}

func (j *jsonBytes) Scan(value interface{}) error {
	if j == nil {
		return nil
	}
	switch typed := value.(type) {
	case nil:
		*j = nil
	case []byte:
		*j = append((*j)[:0], typed...)
	case string:
		*j = append((*j)[:0], typed...)
	default:
		return fmt.Errorf("unsupported json scan type %T", value)
	}
	return nil
}

type stringList []string

func (s stringList) Value() (driver.Value, error) {
	if s == nil {
		return "[]", nil
	}
	payload, err := json.Marshal([]string(s))
	return string(payload), err
}

func (s *stringList) Scan(value interface{}) error {
	if s == nil {
		return nil
	}
	var data []byte
	switch typed := value.(type) {
	case nil:
		*s = nil
		return nil
	case []byte:
		data = typed
	case string:
		data = []byte(typed)
	default:
		return fmt.Errorf("unsupported string list scan type %T", value)
	}
	var values []string
	if len(data) > 0 {
		if err := json.Unmarshal(data, &values); err != nil {
			return err
		}
	}
	*s = values
	return nil
}

type agentRecord struct {
	ID                    string `gorm:"primaryKey;uniqueIndex:idx_agents_app_id,priority:2"`
	AppID                 string `gorm:"not null;index:idx_agents_app_created,priority:1;uniqueIndex:idx_agents_app_id,priority:1"`
	Name                  string `gorm:"not null"`
	RuntimeKind           string `gorm:"not null"`
	Provider              string
	Model                 string
	SystemPrompt          string
	Skills                jsonBytes  `gorm:"type:json"`
	AllowedTools          stringList `gorm:"type:json"`
	AllowedTargets        stringList `gorm:"type:json"`
	ApprovalMode          string     `gorm:"not null"`
	DefaultInvocationMode string     `gorm:"not null"`
	ExecutionConfig       jsonBytes  `gorm:"type:json"`
	CreatedAt             time.Time  `gorm:"not null;index:idx_agents_app_created,priority:2"`
	UpdatedAt             time.Time  `gorm:"not null"`
}

func (agentRecord) TableName() string { return "agents" }

type runRecord struct {
	ID              string    `gorm:"primaryKey;uniqueIndex:idx_runs_app_id,priority:2"`
	AppID           string    `gorm:"not null;index:idx_runs_app_created,priority:1;uniqueIndex:idx_runs_app_id,priority:1"`
	HostRunID       string    `gorm:"index"`
	AgentID         string    `gorm:"not null;index"`
	TargetType      string    `gorm:"not null;index:idx_runs_app_target,priority:2"`
	TargetID        string    `gorm:"not null;index:idx_runs_app_target,priority:3"`
	TargetDisplay   jsonBytes `gorm:"type:json"`
	TargetMetadata  jsonBytes `gorm:"type:json"`
	RuntimeKind     string    `gorm:"not null"`
	ExecutionMode   string    `gorm:"not null"`
	InvocationMode  string    `gorm:"not null"`
	ExternalActorID string    `gorm:"index"`
	Status          string    `gorm:"not null;index"`
	PauseReason     string    `gorm:"not null"`
	ApprovalState   string    `gorm:"not null"`
	Input           jsonBytes `gorm:"type:json"`
	OutputSummary   jsonBytes `gorm:"type:json"`
	WorkspaceLease  jsonBytes `gorm:"type:json"`
	ErrorMessage    string
	StartedAt       *time.Time
	CompletedAt     *time.Time
	CreatedAt       time.Time `gorm:"not null;index:idx_runs_app_created,priority:2"`
	UpdatedAt       time.Time `gorm:"not null"`
}

func (runRecord) TableName() string { return "agent_runs" }

type messageRecord struct {
	ID               string    `gorm:"primaryKey"`
	AppID            string    `gorm:"not null;index:idx_messages_run_seq,priority:1"`
	RunID            string    `gorm:"not null;index:idx_messages_run_seq,priority:2"`
	RuntimeMessageID string    `gorm:"index"`
	Role             string    `gorm:"not null"`
	Content          string    `gorm:"not null"`
	MessageType      string    `gorm:"not null"`
	ContentBlocks    jsonBytes `gorm:"type:json"`
	ToolInvocations  jsonBytes `gorm:"type:json"`
	SequenceNo       int       `gorm:"not null;index:idx_messages_run_seq,priority:3"`
	CreatedAt        time.Time `gorm:"not null"`
}

func (messageRecord) TableName() string { return "agent_run_messages" }

type artifactRecord struct {
	ID            string `gorm:"primaryKey"`
	AppID         string `gorm:"not null;index:idx_artifacts_run_seq,priority:1"`
	RunID         string `gorm:"not null;index:idx_artifacts_run_seq,priority:2"`
	ArtifactType  string `gorm:"not null;index"`
	Format        string `gorm:"not null"`
	StorageMode   string `gorm:"not null"`
	InlineContent string
	Metadata      jsonBytes `gorm:"type:json"`
	SequenceNo    int       `gorm:"not null;index:idx_artifacts_run_seq,priority:3"`
	CreatedAt     time.Time `gorm:"not null"`
}

func (artifactRecord) TableName() string { return "agent_run_artifacts" }

type interactionRecord struct {
	ID                   string `gorm:"primaryKey"`
	AppID                string `gorm:"not null;index:idx_interactions_run_created,priority:1"`
	RunID                string `gorm:"not null;index:idx_interactions_run_created,priority:2"`
	RuntimeKind          string `gorm:"not null"`
	InteractionKind      string `gorm:"not null;index"`
	Status               string `gorm:"not null;index"`
	Title                string
	Summary              string
	RequestPayload       jsonBytes `gorm:"type:json"`
	ResponsePayload      jsonBytes `gorm:"type:json"`
	ResolvedByExternalID string
	ResolvedAt           *time.Time
	CreatedAt            time.Time `gorm:"not null;index:idx_interactions_run_created,priority:3"`
	UpdatedAt            time.Time `gorm:"not null"`
}

func (interactionRecord) TableName() string { return "agent_run_interactions" }

type toolCallRecord struct {
	ID               string    `gorm:"primaryKey"`
	AppID            string    `gorm:"not null;index:idx_tool_calls_run_created,priority:1"`
	RunID            string    `gorm:"not null;index:idx_tool_calls_run_created,priority:2"`
	ToolName         string    `gorm:"not null;index"`
	Input            jsonBytes `gorm:"type:json"`
	Output           jsonBytes `gorm:"type:json"`
	Error            string
	Mutating         bool      `gorm:"not null"`
	ApprovalRequired bool      `gorm:"not null"`
	CreatedAt        time.Time `gorm:"not null;index:idx_tool_calls_run_created,priority:3"`
}

func (toolCallRecord) TableName() string { return "agent_run_tool_calls" }

type codexAuthTokenRecord struct {
	AppID     string    `gorm:"column:app_id;primaryKey"`
	TenantID  string    `gorm:"column:tenant_id;primaryKey"`
	Provider  string    `gorm:"column:provider;primaryKey"`
	AuthMode  string    `gorm:"column:auth_mode;primaryKey"`
	Payload   []byte    `gorm:"column:payload;not null"`
	UpdatedAt time.Time `gorm:"column:updated_at;not null"`
}

func (codexAuthTokenRecord) TableName() string { return "codex_auth_tokens" }

func (s *SQL) CreateAgent(ctx context.Context, agent *agentcore.Agent) error {
	if agent == nil {
		return fmt.Errorf("agent is required")
	}
	if agent.AppID == "" {
		return fmt.Errorf("app_id is required")
	}
	now := time.Now().UTC()
	if agent.ID == "" {
		agent.ID = id.New("agent")
	}
	if agent.RuntimeKind == "" {
		agent.RuntimeKind = agentcore.RuntimeNativeSDK
	}
	if agent.ApprovalMode == "" {
		agent.ApprovalMode = agentcore.ApprovalModeNever
	}
	if agent.DefaultInvocationMode == "" {
		agent.DefaultInvocationMode = agentcore.InvocationAutonomous
	}
	sanitizeAgent(agent)
	agent.CreatedAt = now
	agent.UpdatedAt = now
	return s.db.WithContext(ctx).Create(agentToRecord(agent)).Error
}

func (s *SQL) GetAgent(ctx context.Context, appID, agentID string) (*agentcore.Agent, error) {
	var record agentRecord
	err := s.db.WithContext(ctx).Where("app_id = ? AND id = ?", appID, agentID).First(&record).Error
	if err == gorm.ErrRecordNotFound {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return record.toCore(), nil
}

func (s *SQL) ListAgents(ctx context.Context, appID string) ([]agentcore.Agent, error) {
	var records []agentRecord
	if err := s.db.WithContext(ctx).Where("app_id = ?", appID).Order("created_at ASC").Find(&records).Error; err != nil {
		return nil, err
	}
	out := make([]agentcore.Agent, 0, len(records))
	for _, record := range records {
		out = append(out, *record.toCore())
	}
	return out, nil
}

func (s *SQL) UpdateAgent(ctx context.Context, agent *agentcore.Agent) error {
	if agent == nil {
		return fmt.Errorf("agent is required")
	}
	existing, err := s.GetAgent(ctx, agent.AppID, agent.ID)
	if err != nil {
		return err
	}
	if existing == nil {
		return fmt.Errorf("agent not found")
	}
	agent.CreatedAt = existing.CreatedAt
	agent.UpdatedAt = time.Now().UTC()
	sanitizeAgent(agent)
	return s.db.WithContext(ctx).Save(agentToRecord(agent)).Error
}

func (s *SQL) CreateRun(ctx context.Context, run *agentcore.AgentRun) error {
	if run == nil {
		return fmt.Errorf("run is required")
	}
	if run.AppID == "" {
		return fmt.Errorf("app_id is required")
	}
	now := time.Now().UTC()
	if run.ID == "" {
		run.ID = id.New("run")
	}
	run.CreatedAt = now
	run.UpdatedAt = now
	agentcore.NormalizeRun(run)
	sanitizeRun(run)
	if run.HostRunID != "" {
		var count int64
		if err := s.db.WithContext(ctx).Model(&runRecord{}).Where("app_id = ? AND host_run_id = ?", run.AppID, run.HostRunID).Count(&count).Error; err != nil {
			return err
		}
		if count > 0 {
			return fmt.Errorf("run host_run_id already exists")
		}
	}
	return s.db.WithContext(ctx).Create(runToRecord(run)).Error
}

func (s *SQL) GetRun(ctx context.Context, appID, runID string) (*agentcore.AgentRun, error) {
	var record runRecord
	err := s.db.WithContext(ctx).Where("app_id = ? AND id = ?", appID, runID).First(&record).Error
	if err == gorm.ErrRecordNotFound {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return record.toCore(), nil
}

func (s *SQL) GetRunByHostRunID(ctx context.Context, appID, hostRunID string) (*agentcore.AgentRun, error) {
	hostRunID = strings.TrimSpace(hostRunID)
	if hostRunID == "" {
		return nil, nil
	}
	var record runRecord
	err := s.db.WithContext(ctx).Where("app_id = ? AND host_run_id = ?", appID, hostRunID).First(&record).Error
	if err == gorm.ErrRecordNotFound {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return record.toCore(), nil
}

func (s *SQL) ListRuns(ctx context.Context, appID string) ([]agentcore.AgentRun, error) {
	var records []runRecord
	if err := s.db.WithContext(ctx).Where("app_id = ?", appID).Order("created_at DESC").Find(&records).Error; err != nil {
		return nil, err
	}
	out := make([]agentcore.AgentRun, 0, len(records))
	for _, record := range records {
		out = append(out, *record.toCore())
	}
	return out, nil
}

func (s *SQL) UpdateRun(ctx context.Context, run *agentcore.AgentRun) error {
	if run == nil {
		return fmt.Errorf("run is required")
	}
	existing, err := s.GetRun(ctx, run.AppID, run.ID)
	if err != nil {
		return err
	}
	if existing == nil {
		return fmt.Errorf("run not found")
	}
	run.CreatedAt = existing.CreatedAt
	run.UpdatedAt = time.Now().UTC()
	sanitizeRun(run)
	return s.db.WithContext(ctx).Save(runToRecord(run)).Error
}

func (s *SQL) AppendMessage(ctx context.Context, message *agentcore.AgentRunMessage) error {
	if message == nil {
		return fmt.Errorf("message is required")
	}
	if message.ID == "" {
		message.ID = id.New("msg")
	}
	sanitizeMessage(message)
	message.CreatedAt = time.Now().UTC()
	return s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var maxSeq int
		if err := tx.Model(&messageRecord{}).Where("app_id = ? AND run_id = ?", message.AppID, message.RunID).Select("COALESCE(MAX(sequence_no), 0)").Scan(&maxSeq).Error; err != nil {
			return err
		}
		message.SequenceNo = maxSeq + 1
		return tx.Create(messageToRecord(message)).Error
	})
}

func (s *SQL) ListMessages(ctx context.Context, appID, runID string) ([]agentcore.AgentRunMessage, error) {
	var records []messageRecord
	if err := s.db.WithContext(ctx).Where("app_id = ? AND run_id = ?", appID, runID).Order("sequence_no ASC").Find(&records).Error; err != nil {
		return nil, err
	}
	out := make([]agentcore.AgentRunMessage, 0, len(records))
	for _, record := range records {
		out = append(out, *record.toCore())
	}
	return out, nil
}

func (s *SQL) AppendArtifact(ctx context.Context, artifact *agentcore.AgentRunArtifact) error {
	if artifact == nil {
		return fmt.Errorf("artifact is required")
	}
	if artifact.ID == "" {
		artifact.ID = id.New("art")
	}
	if artifact.StorageMode == "" {
		artifact.StorageMode = "inline"
	}
	sanitizeArtifact(artifact)
	artifact.CreatedAt = time.Now().UTC()
	return s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var maxSeq int
		if err := tx.Model(&artifactRecord{}).Where("app_id = ? AND run_id = ?", artifact.AppID, artifact.RunID).Select("COALESCE(MAX(sequence_no), 0)").Scan(&maxSeq).Error; err != nil {
			return err
		}
		artifact.SequenceNo = maxSeq + 1
		return tx.Create(artifactToRecord(artifact)).Error
	})
}

func (s *SQL) ListArtifacts(ctx context.Context, appID, runID string) ([]agentcore.AgentRunArtifact, error) {
	var records []artifactRecord
	if err := s.db.WithContext(ctx).Where("app_id = ? AND run_id = ?", appID, runID).Order("sequence_no ASC").Find(&records).Error; err != nil {
		return nil, err
	}
	out := make([]agentcore.AgentRunArtifact, 0, len(records))
	for _, record := range records {
		out = append(out, *record.toCore())
	}
	return out, nil
}

func (s *SQL) AppendInteraction(ctx context.Context, interaction *agentcore.AgentRunInteraction) error {
	if interaction == nil {
		return fmt.Errorf("interaction is required")
	}
	now := time.Now().UTC()
	if interaction.ID == "" {
		interaction.ID = id.New("int")
	}
	if interaction.Status == "" {
		interaction.Status = "pending"
	}
	sanitizeInteraction(interaction)
	interaction.CreatedAt = now
	interaction.UpdatedAt = now
	return s.db.WithContext(ctx).Create(interactionToRecord(interaction)).Error
}

func (s *SQL) ListInteractions(ctx context.Context, appID, runID string) ([]agentcore.AgentRunInteraction, error) {
	var records []interactionRecord
	if err := s.db.WithContext(ctx).Where("app_id = ? AND run_id = ?", appID, runID).Order("created_at ASC").Find(&records).Error; err != nil {
		return nil, err
	}
	out := make([]agentcore.AgentRunInteraction, 0, len(records))
	for _, record := range records {
		out = append(out, *record.toCore())
	}
	return out, nil
}

func (s *SQL) UpdateInteraction(ctx context.Context, interaction *agentcore.AgentRunInteraction) error {
	if interaction == nil {
		return fmt.Errorf("interaction is required")
	}
	var existing interactionRecord
	err := s.db.WithContext(ctx).Where("app_id = ? AND run_id = ? AND id = ?", interaction.AppID, interaction.RunID, interaction.ID).First(&existing).Error
	if err == gorm.ErrRecordNotFound {
		return fmt.Errorf("interaction not found")
	}
	if err != nil {
		return err
	}
	interaction.CreatedAt = existing.CreatedAt
	interaction.UpdatedAt = time.Now().UTC()
	sanitizeInteraction(interaction)
	return s.db.WithContext(ctx).Save(interactionToRecord(interaction)).Error
}

func (s *SQL) AppendToolCall(ctx context.Context, call *agentcore.ToolCall) error {
	if call == nil {
		return fmt.Errorf("tool call is required")
	}
	if call.ID == "" {
		call.ID = id.New("toolcall")
	}
	sanitizeToolCall(call)
	call.CreatedAt = time.Now().UTC()
	return s.db.WithContext(ctx).Create(toolCallToRecord(call)).Error
}

func (s *SQL) ListToolCalls(ctx context.Context, appID, runID string) ([]agentcore.ToolCall, error) {
	var records []toolCallRecord
	if err := s.db.WithContext(ctx).Where("app_id = ? AND run_id = ?", appID, runID).Order("created_at ASC").Find(&records).Error; err != nil {
		return nil, err
	}
	out := make([]agentcore.ToolCall, 0, len(records))
	for _, record := range records {
		out = append(out, record.toCore())
	}
	return out, nil
}

func agentToRecord(agent *agentcore.Agent) *agentRecord {
	skills, _ := json.Marshal(agent.Skills)
	return &agentRecord{
		ID:                    agent.ID,
		AppID:                 agent.AppID,
		Name:                  agent.Name,
		RuntimeKind:           agent.RuntimeKind,
		Provider:              agent.Provider,
		Model:                 agent.Model,
		SystemPrompt:          agent.SystemPrompt,
		Skills:                jsonBytes(skills),
		AllowedTools:          stringList(agent.AllowedTools),
		AllowedTargets:        stringList(agent.AllowedTargets),
		ApprovalMode:          agent.ApprovalMode,
		DefaultInvocationMode: agent.DefaultInvocationMode,
		ExecutionConfig:       jsonBytes(agent.ExecutionConfig),
		CreatedAt:             agent.CreatedAt,
		UpdatedAt:             agent.UpdatedAt,
	}
}

func (r agentRecord) toCore() *agentcore.Agent {
	var skills []agentcore.SkillRef
	_ = json.Unmarshal(r.Skills, &skills)
	return &agentcore.Agent{
		ID:                    r.ID,
		AppID:                 r.AppID,
		Name:                  r.Name,
		RuntimeKind:           r.RuntimeKind,
		Provider:              r.Provider,
		Model:                 r.Model,
		SystemPrompt:          r.SystemPrompt,
		Skills:                skills,
		AllowedTools:          []string(r.AllowedTools),
		AllowedTargets:        []string(r.AllowedTargets),
		ApprovalMode:          r.ApprovalMode,
		DefaultInvocationMode: r.DefaultInvocationMode,
		ExecutionConfig:       json.RawMessage(r.ExecutionConfig),
		CreatedAt:             r.CreatedAt,
		UpdatedAt:             r.UpdatedAt,
	}
}

func runToRecord(run *agentcore.AgentRun) *runRecord {
	display, _ := json.Marshal(run.Target.Display)
	metadata, _ := json.Marshal(run.Target.Metadata)
	input, _ := json.Marshal(run.Input)
	workspaceLease, _ := json.Marshal(run.WorkspaceLease)
	return &runRecord{
		ID:              run.ID,
		AppID:           run.AppID,
		HostRunID:       run.HostRunID,
		AgentID:         run.AgentID,
		TargetType:      run.Target.Type,
		TargetID:        run.Target.ID,
		TargetDisplay:   jsonBytes(display),
		TargetMetadata:  jsonBytes(metadata),
		RuntimeKind:     run.RuntimeKind,
		ExecutionMode:   run.ExecutionMode,
		InvocationMode:  run.InvocationMode,
		ExternalActorID: run.ExternalActorID,
		Status:          run.Status,
		PauseReason:     run.PauseReason,
		ApprovalState:   run.ApprovalState,
		Input:           jsonBytes(input),
		OutputSummary:   jsonBytes(run.OutputSummary),
		WorkspaceLease:  jsonBytes(workspaceLease),
		ErrorMessage:    run.ErrorMessage,
		StartedAt:       run.StartedAt,
		CompletedAt:     run.CompletedAt,
		CreatedAt:       run.CreatedAt,
		UpdatedAt:       run.UpdatedAt,
	}
}

func (r runRecord) toCore() *agentcore.AgentRun {
	var display *agentcore.TargetDisplay
	if len(r.TargetDisplay) > 0 && string(r.TargetDisplay) != "null" {
		_ = json.Unmarshal(r.TargetDisplay, &display)
	}
	var metadata map[string]interface{}
	if len(r.TargetMetadata) > 0 && string(r.TargetMetadata) != "null" {
		_ = json.Unmarshal(r.TargetMetadata, &metadata)
	}
	var input agentcore.RunInput
	_ = json.Unmarshal(r.Input, &input)
	var workspaceLease *agentcore.WorkspaceLease
	if len(r.WorkspaceLease) > 0 && string(r.WorkspaceLease) != "null" && string(r.WorkspaceLease) != "{}" {
		_ = json.Unmarshal(r.WorkspaceLease, &workspaceLease)
	}
	return &agentcore.AgentRun{
		ID:              r.ID,
		AppID:           r.AppID,
		HostRunID:       r.HostRunID,
		AgentID:         r.AgentID,
		Target:          agentcore.TargetRef{Type: r.TargetType, ID: r.TargetID, Display: display, Metadata: metadata},
		RuntimeKind:     r.RuntimeKind,
		ExecutionMode:   r.ExecutionMode,
		InvocationMode:  r.InvocationMode,
		ExternalActorID: r.ExternalActorID,
		Status:          r.Status,
		PauseReason:     r.PauseReason,
		ApprovalState:   r.ApprovalState,
		Input:           input,
		OutputSummary:   json.RawMessage(r.OutputSummary),
		WorkspaceLease:  workspaceLease,
		ErrorMessage:    r.ErrorMessage,
		StartedAt:       r.StartedAt,
		CompletedAt:     r.CompletedAt,
		CreatedAt:       r.CreatedAt,
		UpdatedAt:       r.UpdatedAt,
	}
}

func messageToRecord(message *agentcore.AgentRunMessage) *messageRecord {
	return &messageRecord{
		ID:               message.ID,
		AppID:            message.AppID,
		RunID:            message.RunID,
		RuntimeMessageID: message.RuntimeMessageID,
		Role:             message.Role,
		Content:          message.Content,
		MessageType:      message.MessageType,
		ContentBlocks:    jsonBytes(message.ContentBlocks),
		ToolInvocations:  jsonBytes(message.ToolInvocations),
		SequenceNo:       message.SequenceNo,
		CreatedAt:        message.CreatedAt,
	}
}

func (r messageRecord) toCore() *agentcore.AgentRunMessage {
	return &agentcore.AgentRunMessage{
		ID:               r.ID,
		AppID:            r.AppID,
		RunID:            r.RunID,
		RuntimeMessageID: r.RuntimeMessageID,
		Role:             r.Role,
		Content:          r.Content,
		MessageType:      r.MessageType,
		ContentBlocks:    json.RawMessage(r.ContentBlocks),
		ToolInvocations:  json.RawMessage(r.ToolInvocations),
		SequenceNo:       r.SequenceNo,
		CreatedAt:        r.CreatedAt,
	}
}

func artifactToRecord(artifact *agentcore.AgentRunArtifact) *artifactRecord {
	return &artifactRecord{
		ID:            artifact.ID,
		AppID:         artifact.AppID,
		RunID:         artifact.RunID,
		ArtifactType:  artifact.ArtifactType,
		Format:        artifact.Format,
		StorageMode:   artifact.StorageMode,
		InlineContent: artifact.InlineContent,
		Metadata:      jsonBytes(artifact.Metadata),
		SequenceNo:    artifact.SequenceNo,
		CreatedAt:     artifact.CreatedAt,
	}
}

func (r artifactRecord) toCore() *agentcore.AgentRunArtifact {
	return &agentcore.AgentRunArtifact{
		ID:            r.ID,
		AppID:         r.AppID,
		RunID:         r.RunID,
		ArtifactType:  r.ArtifactType,
		Format:        r.Format,
		StorageMode:   r.StorageMode,
		InlineContent: r.InlineContent,
		Metadata:      json.RawMessage(r.Metadata),
		SequenceNo:    r.SequenceNo,
		CreatedAt:     r.CreatedAt,
	}
}

func interactionToRecord(interaction *agentcore.AgentRunInteraction) *interactionRecord {
	return &interactionRecord{
		ID:                   interaction.ID,
		AppID:                interaction.AppID,
		RunID:                interaction.RunID,
		RuntimeKind:          interaction.RuntimeKind,
		InteractionKind:      interaction.InteractionKind,
		Status:               interaction.Status,
		Title:                interaction.Title,
		Summary:              interaction.Summary,
		RequestPayload:       jsonBytes(interaction.RequestPayload),
		ResponsePayload:      jsonBytes(interaction.ResponsePayload),
		ResolvedByExternalID: interaction.ResolvedByExternalID,
		ResolvedAt:           interaction.ResolvedAt,
		CreatedAt:            interaction.CreatedAt,
		UpdatedAt:            interaction.UpdatedAt,
	}
}

func (r interactionRecord) toCore() *agentcore.AgentRunInteraction {
	return &agentcore.AgentRunInteraction{
		ID:                   r.ID,
		AppID:                r.AppID,
		RunID:                r.RunID,
		RuntimeKind:          r.RuntimeKind,
		InteractionKind:      r.InteractionKind,
		Status:               r.Status,
		Title:                r.Title,
		Summary:              r.Summary,
		RequestPayload:       json.RawMessage(r.RequestPayload),
		ResponsePayload:      json.RawMessage(r.ResponsePayload),
		ResolvedByExternalID: r.ResolvedByExternalID,
		ResolvedAt:           r.ResolvedAt,
		CreatedAt:            r.CreatedAt,
		UpdatedAt:            r.UpdatedAt,
	}
}

func toolCallToRecord(call *agentcore.ToolCall) *toolCallRecord {
	return &toolCallRecord{
		ID:               call.ID,
		AppID:            call.AppID,
		RunID:            call.RunID,
		ToolName:         call.ToolName,
		Input:            jsonBytes(call.Input),
		Output:           jsonBytes(call.Output),
		Error:            call.Error,
		Mutating:         call.Mutating,
		ApprovalRequired: call.ApprovalRequired,
		CreatedAt:        call.CreatedAt,
	}
}

func (r toolCallRecord) toCore() agentcore.ToolCall {
	return agentcore.ToolCall{
		ID:               r.ID,
		AppID:            r.AppID,
		RunID:            r.RunID,
		ToolName:         r.ToolName,
		Input:            json.RawMessage(r.Input),
		Output:           json.RawMessage(r.Output),
		Error:            r.Error,
		Mutating:         r.Mutating,
		ApprovalRequired: r.ApprovalRequired,
		CreatedAt:        r.CreatedAt,
	}
}

var _ agentcore.Store = (*SQL)(nil)
