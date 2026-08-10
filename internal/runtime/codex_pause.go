package runtime

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
)

var codexOptionSlugSanitizer = regexp.MustCompile(`[^a-z0-9]+`)

type codexStructuredAnswerEnvelope struct {
	Answers []codexStructuredAnswerEntry `json:"answers"`
}

type codexStructuredAnswerEntry struct {
	QuestionID    string `json:"question_id"`
	Question      string `json:"question"`
	SelectedValue string `json:"selected_value"`
	SelectedLabel string `json:"selected_label"`
	Freetext      string `json:"freetext"`
}

func codexPendingFromRequest(method string, id json.RawMessage, params json.RawMessage) (*codexPendingRequest, string, string, error) {
	switch strings.TrimSpace(method) {
	case "item/tool/requestUserInput":
		var payload codexToolRequestUserInputParams
		if err := json.Unmarshal(params, &payload); err != nil {
			return nil, "", "", err
		}
		questionIDs := make([]string, 0, len(payload.Questions))
		for _, question := range payload.Questions {
			questionIDs = append(questionIDs, strings.TrimSpace(question.ID))
		}
		return &codexPendingRequest{
			Kind:         codexPendingRequestKindHumanInput,
			RequestID:    codexRequestIDString(id),
			RequestIDRaw: append(json.RawMessage(nil), id...),
			TurnID:       strings.TrimSpace(payload.TurnID),
			ItemID:       strings.TrimSpace(payload.ItemID),
			QuestionIDs:  questionIDs,
			Payload:      append(json.RawMessage(nil), params...),
		}, "human_input", "Codex needs input to continue.", nil
	case "item/commandExecution/requestApproval":
		var payload codexCommandExecutionRequestApprovalParams
		if err := json.Unmarshal(params, &payload); err != nil {
			return nil, "", "", err
		}
		return codexPendingApproval(codexPendingRequestKindCommandApproval, id, payload.TurnID, payload.ItemID, params), "command_execution_approval", codexCommandApprovalSummary(payload), nil
	case "item/fileChange/requestApproval":
		var payload codexFileChangeRequestApprovalParams
		if err := json.Unmarshal(params, &payload); err != nil {
			return nil, "", "", err
		}
		return codexPendingApproval(codexPendingRequestKindFileApproval, id, payload.TurnID, payload.ItemID, params), "file_change_approval", codexFileChangeApprovalSummary(payload), nil
	case "item/permissions/requestApproval":
		var payload codexPermissionsRequestApprovalParams
		if err := json.Unmarshal(params, &payload); err != nil {
			return nil, "", "", err
		}
		return codexPendingApproval(codexPendingRequestKindPermissions, id, payload.TurnID, payload.ItemID, params), "permissions_approval", codexPermissionsApprovalSummary(payload), nil
	default:
		return nil, "", "", fmt.Errorf("unsupported codex pause request method %q", method)
	}
}

func isCodexPauseRequestMethod(method string) bool {
	switch strings.TrimSpace(method) {
	case "item/commandExecution/requestApproval", "item/fileChange/requestApproval", "item/tool/requestUserInput", "item/permissions/requestApproval":
		return true
	default:
		return false
	}
}

func codexPendingApproval(kind string, id json.RawMessage, turnID, itemID string, payload json.RawMessage) *codexPendingRequest {
	return &codexPendingRequest{
		Kind:         strings.TrimSpace(kind),
		RequestID:    codexRequestIDString(id),
		RequestIDRaw: append(json.RawMessage(nil), id...),
		TurnID:       strings.TrimSpace(turnID),
		ItemID:       strings.TrimSpace(itemID),
		Payload:      append(json.RawMessage(nil), payload...),
	}
}

func codexResumeResponse(pending *codexPendingRequest, intent, content string, responsePayload json.RawMessage) (any, string, error) {
	if pending == nil {
		return nil, "", fmt.Errorf("missing pending request state")
	}
	// Dynamic-tool pauses answer the replayed item/tool/call, so the response
	// is always a dynamic tool result. Handle them before the raw payload
	// passthrough, which only makes sense for Codex's built-in envelopes.
	switch pending.Kind {
	case codexPendingRequestKindDynamicInput:
		reply := strings.TrimSpace(content)
		if reply == "" {
			reply = stringFieldFromJSON(responsePayload, "content")
		}
		if reply == "" && len(responsePayload) > 0 {
			reply = strings.TrimSpace(string(responsePayload))
		}
		if reply == "" {
			return nil, "", fmt.Errorf("human input resume is missing content")
		}
		return codexDynamicToolCallResponse{
			Success:      true,
			ContentItems: []codexDynamicToolCallOutput{{Type: "inputText", Text: "The human replied:\n" + reply}},
		}, "", nil
	case codexPendingRequestKindDynamicApproval:
		decision := codexResumeFallbackDecision(intent, responsePayload)
		feedback := strings.TrimSpace(content)
		if feedback == "" {
			feedback = stringFieldFromJSON(responsePayload, "content")
		}
		text := ""
		switch decision {
		case "approve":
			text = "The human approved this request. Continue."
		case "request_changes":
			if feedback == "" {
				return nil, "", fmt.Errorf("request_changes resume is missing reviewer feedback")
			}
			text = "The human requested changes:\n" + feedback
		default:
			text = "The human did not approve this request. Continue with a safe alternative."
		}
		if decision != "request_changes" && feedback != "" {
			text += "\nHuman feedback: " + feedback
		}
		return codexDynamicToolCallResponse{
			Success:      true,
			ContentItems: []codexDynamicToolCallOutput{{Type: "inputText", Text: text}},
		}, "", nil
	case codexPendingRequestKindCommandApproval, codexPendingRequestKindFileApproval, codexPendingRequestKindPermissions:
		return codexBuiltInApprovalResumeResponse(pending, intent, content, responsePayload)
	}
	if len(responsePayload) > 0 {
		var response any
		if err := json.Unmarshal(responsePayload, &response); err != nil {
			return nil, "", fmt.Errorf("parse codex resume response payload: %w", err)
		}
		if strings.TrimSpace(intent) == "request_changes" && strings.TrimSpace(content) != "" {
			return response, strings.TrimSpace(content), nil
		}
		return response, "", nil
	}
	switch pending.Kind {
	case codexPendingRequestKindHumanInput:
		response, err := codexParseUserInputResponse(pending, content)
		return response, "", err
	default:
		return nil, "", fmt.Errorf("unsupported pending request kind %q", pending.Kind)
	}
}

func codexBuiltInApprovalResumeResponse(pending *codexPendingRequest, intent, content string, responsePayload json.RawMessage) (any, string, error) {
	if response, ok := codexNativeApprovalResponse(pending, responsePayload); ok {
		followup := ""
		if codexResumeFallbackDecision(intent, responsePayload) == "request_changes" {
			followup = strings.TrimSpace(content)
		}
		return response, followup, nil
	}

	decision := codexResumeFallbackDecision(intent, responsePayload)
	approved := decision == "approve"
	requestChanges := decision == "request_changes"
	response, err := codexApprovalResponse(pending, approved, requestChanges)
	if err != nil {
		return nil, "", err
	}
	if requestChanges {
		return response, strings.TrimSpace(content), nil
	}
	return response, "", nil
}

func codexNativeApprovalResponse(pending *codexPendingRequest, raw json.RawMessage) (any, bool) {
	if pending == nil || len(raw) == 0 || strings.TrimSpace(string(raw)) == "null" {
		return nil, false
	}
	var response map[string]any
	if err := json.Unmarshal(raw, &response); err != nil {
		return nil, false
	}
	switch pending.Kind {
	case codexPendingRequestKindCommandApproval, codexPendingRequestKindFileApproval:
		decision, _ := response["decision"].(string)
		switch strings.TrimSpace(decision) {
		case "accept", "acceptForSession", "acceptWithExecpolicyAmendment", "applyNetworkPolicyAmendment", "decline", "cancel":
			return response, true
		}
	case codexPendingRequestKindPermissions:
		permissions, hasPermissions := response["permissions"].(map[string]any)
		scope, hasScope := response["scope"].(string)
		if hasPermissions && permissions != nil && hasScope && (strings.TrimSpace(scope) == "turn" || strings.TrimSpace(scope) == "session") {
			return response, true
		}
	}
	return nil, false
}

func codexResumeFallbackPrompt(pending *codexPendingRequest, intent, content string, responsePayload json.RawMessage) (string, error) {
	if pending == nil {
		return "", fmt.Errorf("missing pending request state")
	}
	decision := codexResumeFallbackDecision(intent, responsePayload)
	content = strings.TrimSpace(content)
	if decision == "request_changes" {
		if content == "" {
			content = stringFieldFromJSON(responsePayload, "content")
		}
		if content == "" {
			return "", fmt.Errorf("request_changes resume is missing reviewer feedback")
		}
		return content, nil
	}
	switch strings.TrimSpace(pending.Kind) {
	case codexPendingRequestKindHumanInput, codexPendingRequestKindDynamicInput:
		if content == "" && len(responsePayload) > 0 {
			content = strings.TrimSpace(string(responsePayload))
		}
		if content == "" {
			return "", fmt.Errorf("human input resume is missing content")
		}
		return content, nil
	case codexPendingRequestKindDynamicApproval:
		if decision != "approve" {
			return "The human did not approve your request. Revise your work based on their feedback and continue.", nil
		}
		return "The human approved your request. Continue from where you paused.", nil
	case codexPendingRequestKindDynamicGatewayApproval:
		tool := strings.TrimSpace(pending.Tool)
		if tool == "" {
			tool = "requested"
		}
		if decision != "approve" {
			return fmt.Sprintf("The %s tool call was not approved. Do not retry it. Continue with a safe alternative and explain what changed.", tool), nil
		}
		return fmt.Sprintf("The %s tool call you requested was approved. Call %s again now with the same arguments and continue.", tool, tool), nil
	case codexPendingRequestKindCommandApproval:
		if decision != "approve" {
			return "The previously requested command was not approved. Do not run it. Continue with a safe alternative and explain what changed.", nil
		}
		var params codexCommandExecutionRequestApprovalParams
		if err := json.Unmarshal(pending.Payload, &params); err != nil {
			return "", fmt.Errorf("parse pending command approval payload: %w", err)
		}
		var lines []string
		command := strings.TrimSpace(optionalStringValue(params.Command))
		if command != "" {
			lines = append(lines, fmt.Sprintf("The command %q you requested was approved. Run it now and continue.", command))
		} else {
			lines = append(lines, "The command you requested was approved. Run it now and continue.")
		}
		if cwd := strings.TrimSpace(optionalStringValue(params.Cwd)); cwd != "" {
			lines = append(lines, "Working directory: "+cwd)
		}
		if reason := strings.TrimSpace(optionalStringValue(params.Reason)); reason != "" {
			lines = append(lines, "Original reason: "+reason)
		}
		return strings.Join(lines, "\n"), nil
	case codexPendingRequestKindFileApproval:
		if decision != "approve" {
			return "The previously requested file change was not approved. Do not apply it. Continue with a safe alternative and explain what changed.", nil
		}
		var params codexFileChangeRequestApprovalParams
		if err := json.Unmarshal(pending.Payload, &params); err != nil {
			return "", fmt.Errorf("parse pending file approval payload: %w", err)
		}
		var lines []string
		lines = append(lines, "The file change you requested was approved. Apply it now and continue.")
		if grantRoot := strings.TrimSpace(optionalStringValue(params.GrantRoot)); grantRoot != "" {
			lines = append(lines, "Approved path scope: "+grantRoot)
		}
		if reason := strings.TrimSpace(optionalStringValue(params.Reason)); reason != "" {
			lines = append(lines, "Original reason: "+reason)
		}
		return strings.Join(lines, "\n"), nil
	case codexPendingRequestKindPermissions:
		if decision != "approve" {
			return "The previously requested permission was not granted. Continue without that permission and explain the limitation.", nil
		}
		var params codexPermissionsRequestApprovalParams
		if err := json.Unmarshal(pending.Payload, &params); err != nil {
			return "", fmt.Errorf("parse pending permissions payload: %w", err)
		}
		var lines []string
		lines = append(lines, "The permission you requested was granted. Continue now.")
		if reason := strings.TrimSpace(optionalStringValue(params.Reason)); reason != "" {
			lines = append(lines, "Original reason: "+reason)
		}
		if params.Permissions.Network != nil && params.Permissions.Network.Enabled != nil {
			lines = append(lines, fmt.Sprintf("Network access granted: %t", *params.Permissions.Network.Enabled))
		}
		if params.Permissions.FileSystem != nil {
			if len(params.Permissions.FileSystem.Read) > 0 {
				lines = append(lines, "Read access granted: "+strings.Join(params.Permissions.FileSystem.Read, ", "))
			}
			if len(params.Permissions.FileSystem.Write) > 0 {
				lines = append(lines, "Write access granted: "+strings.Join(params.Permissions.FileSystem.Write, ", "))
			}
		}
		return strings.Join(lines, "\n"), nil
	default:
		return "", fmt.Errorf("unsupported pending request kind %q", strings.TrimSpace(pending.Kind))
	}
}

func codexResumeFallbackDecision(intent string, responsePayload json.RawMessage) string {
	decision := strings.ToLower(strings.TrimSpace(firstNonEmpty(stringFieldFromJSON(responsePayload, "decision"), stringFieldFromJSON(responsePayload, "intent"), intent)))
	switch decision {
	case "approve", "approved", "accept", "accepted":
		return "approve"
	case "request_changes", "reject", "rejected", "decline", "declined", "cancel", "cancelled":
		return "request_changes"
	default:
		return decision
	}
}

func stringFieldFromJSON(raw json.RawMessage, key string) string {
	if len(raw) == 0 || strings.TrimSpace(key) == "" {
		return ""
	}
	var body map[string]any
	if err := json.Unmarshal(raw, &body); err != nil {
		return ""
	}
	value, _ := body[key].(string)
	return strings.TrimSpace(value)
}

func codexParseUserInputResponse(pending *codexPendingRequest, content string) (codexToolRequestUserInputResponse, error) {
	var params codexToolRequestUserInputParams
	if pending == nil || len(pending.Payload) == 0 {
		return codexToolRequestUserInputResponse{}, fmt.Errorf("missing pending request payload")
	}
	if err := json.Unmarshal(pending.Payload, &params); err != nil {
		return codexToolRequestUserInputResponse{}, fmt.Errorf("parse pending human input payload: %w", err)
	}
	trimmed := strings.TrimSpace(content)
	if trimmed == "" {
		return codexToolRequestUserInputResponse{}, fmt.Errorf("human input reply is empty")
	}
	var envelope codexStructuredAnswerEnvelope
	for _, candidate := range []string{strings.TrimSpace(trimJSONFences(trimmed)), strings.TrimSpace(extractJSONObject(trimmed))} {
		if candidate == "" {
			continue
		}
		if err := json.Unmarshal([]byte(candidate), &envelope); err == nil && len(envelope.Answers) > 0 {
			break
		}
	}
	answerMap := make(map[string]codexStructuredAnswerEntry, len(envelope.Answers))
	for _, answer := range envelope.Answers {
		answer.QuestionID = strings.TrimSpace(answer.QuestionID)
		if answer.QuestionID != "" {
			answerMap[answer.QuestionID] = answer
		}
	}
	response := codexToolRequestUserInputResponse{Answers: make(map[string]codexToolRequestUserInputAnswer, len(params.Questions))}
	if len(answerMap) == 0 && len(params.Questions) == 1 {
		questionID := strings.TrimSpace(params.Questions[0].ID)
		if questionID == "" {
			return codexToolRequestUserInputResponse{}, fmt.Errorf("question id is required")
		}
		response.Answers[questionID] = codexToolRequestUserInputAnswer{Answers: []string{trimmed}}
		return response, nil
	}
	for _, question := range params.Questions {
		answer, ok := answerMap[strings.TrimSpace(question.ID)]
		if !ok {
			continue
		}
		value := strings.TrimSpace(answer.Freetext)
		if value == "" {
			value = strings.TrimSpace(answer.SelectedLabel)
		}
		if value == "" {
			value = strings.TrimSpace(answer.SelectedValue)
		}
		if value == "" {
			return codexToolRequestUserInputResponse{}, fmt.Errorf("question %q is missing an answer", strings.TrimSpace(question.ID))
		}
		response.Answers[strings.TrimSpace(question.ID)] = codexToolRequestUserInputAnswer{Answers: []string{value}}
	}
	if len(response.Answers) == 0 {
		return codexToolRequestUserInputResponse{}, fmt.Errorf("failed to parse structured answers from reply")
	}
	return response, nil
}

func codexApprovalResponse(pending *codexPendingRequest, approved bool, requestChanges bool) (any, error) {
	if pending == nil {
		return nil, fmt.Errorf("missing pending approval state")
	}
	switch pending.Kind {
	case codexPendingRequestKindCommandApproval, codexPendingRequestKindFileApproval:
		decision := "decline"
		if requestChanges {
			decision = "cancel"
		} else if approved {
			decision = "accept"
		}
		return map[string]any{"decision": decision}, nil
	case codexPendingRequestKindPermissions:
		if !approved {
			return map[string]any{"permissions": codexGrantedPermissionProfile{}, "scope": "turn"}, nil
		}
		var params codexPermissionsRequestApprovalParams
		if err := json.Unmarshal(pending.Payload, &params); err != nil {
			return nil, fmt.Errorf("parse pending permissions payload: %w", err)
		}
		return map[string]any{
			"permissions": codexGrantedPermissionProfile{
				Network:    params.Permissions.Network,
				FileSystem: params.Permissions.FileSystem,
			},
			"scope": "turn",
		}, nil
	default:
		return nil, fmt.Errorf("unsupported pending approval kind %q", pending.Kind)
	}
}

func codexCommandApprovalSummary(params codexCommandExecutionRequestApprovalParams) string {
	lines := make([]string, 0, 4)
	if params.Command != nil && strings.TrimSpace(*params.Command) != "" {
		lines = append(lines, "Command: "+strings.TrimSpace(*params.Command))
	}
	if params.Cwd != nil && strings.TrimSpace(*params.Cwd) != "" {
		lines = append(lines, "Working directory: "+strings.TrimSpace(*params.Cwd))
	}
	if params.Reason != nil && strings.TrimSpace(*params.Reason) != "" {
		lines = append(lines, "Reason: "+strings.TrimSpace(*params.Reason))
	}
	if len(lines) == 0 {
		lines = append(lines, "Codex requested approval before executing a command.")
	}
	return strings.Join(lines, "\n")
}

func codexFileChangeApprovalSummary(params codexFileChangeRequestApprovalParams) string {
	lines := make([]string, 0, 2)
	if params.Reason != nil && strings.TrimSpace(*params.Reason) != "" {
		lines = append(lines, "Reason: "+strings.TrimSpace(*params.Reason))
	}
	if params.GrantRoot != nil && strings.TrimSpace(*params.GrantRoot) != "" {
		lines = append(lines, "Grant root: "+strings.TrimSpace(*params.GrantRoot))
	}
	if len(lines) == 0 {
		lines = append(lines, "Codex requested approval before applying file changes.")
	}
	return strings.Join(lines, "\n")
}

func codexPermissionsApprovalSummary(params codexPermissionsRequestApprovalParams) string {
	lines := make([]string, 0, 4)
	if params.Reason != nil && strings.TrimSpace(*params.Reason) != "" {
		lines = append(lines, "Reason: "+strings.TrimSpace(*params.Reason))
	}
	if params.Permissions.Network != nil {
		enabled := false
		if params.Permissions.Network.Enabled != nil {
			enabled = *params.Permissions.Network.Enabled
		}
		lines = append(lines, fmt.Sprintf("Network access requested: %t", enabled))
	}
	if params.Permissions.FileSystem != nil {
		if len(params.Permissions.FileSystem.Read) > 0 {
			lines = append(lines, "Read access: "+strings.Join(params.Permissions.FileSystem.Read, ", "))
		}
		if len(params.Permissions.FileSystem.Write) > 0 {
			lines = append(lines, "Write access: "+strings.Join(params.Permissions.FileSystem.Write, ", "))
		}
	}
	if len(lines) == 0 {
		lines = append(lines, "Codex requested additional permissions.")
	}
	return strings.Join(lines, "\n")
}

func trimJSONFences(value string) string {
	value = strings.TrimSpace(value)
	value = strings.TrimPrefix(value, "```json")
	value = strings.TrimPrefix(value, "```")
	value = strings.TrimSuffix(value, "```")
	return strings.TrimSpace(value)
}

func extractJSONObject(value string) string {
	start := strings.Index(value, "{")
	end := strings.LastIndex(value, "}")
	if start < 0 || end < start {
		return ""
	}
	return value[start : end+1]
}
