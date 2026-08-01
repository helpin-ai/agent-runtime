import React, { useState } from 'react';
import { artifactLabel, getAgentRunDisplayStatus, STATUS_META } from './status.js';
import { useAgentRun } from './useAgentRun.js';

const styles = {
  panel: {
    display: 'grid',
    gap: 12,
    border: '1px solid var(--agent-runtime-border, #d9dee7)',
    borderRadius: 8,
    padding: 12,
    background: 'var(--agent-runtime-bg, #fff)',
    color: 'var(--agent-runtime-fg, #111827)',
    fontFamily: 'var(--agent-runtime-font, system-ui, sans-serif)',
  },
  header: { display: 'flex', alignItems: 'center', justifyContent: 'space-between', gap: 12 },
  title: { margin: 0, fontSize: 14, fontWeight: 650 },
  muted: { color: 'var(--agent-runtime-muted, #667085)', fontSize: 12 },
  badge: {
    display: 'inline-flex',
    alignItems: 'center',
    border: '1px solid var(--agent-runtime-border, #d9dee7)',
    borderRadius: 6,
    padding: '2px 8px',
    fontSize: 12,
  },
  section: { display: 'grid', gap: 8 },
  sectionTitle: { margin: 0, fontSize: 12, fontWeight: 650, color: 'var(--agent-runtime-muted, #667085)' },
  row: { border: '1px solid var(--agent-runtime-border, #d9dee7)', borderRadius: 6, padding: 10 },
  pre: { margin: 0, whiteSpace: 'pre-wrap', wordBreak: 'break-word', fontSize: 12 },
  actions: { display: 'flex', gap: 8, flexWrap: 'wrap' },
  button: {
    border: '1px solid var(--agent-runtime-border, #d9dee7)',
    borderRadius: 6,
    background: 'var(--agent-runtime-button-bg, #f8fafc)',
    color: 'inherit',
    padding: '6px 10px',
    fontSize: 12,
    cursor: 'pointer',
  },
  textarea: {
    width: '100%',
    minHeight: 72,
    resize: 'vertical',
    border: '1px solid var(--agent-runtime-border, #d9dee7)',
    borderRadius: 6,
    padding: 8,
    font: 'inherit',
  },
  definitionGrid: { display: 'grid', gridTemplateColumns: '120px minmax(0, 1fr)', gap: '6px 12px', fontSize: 12 },
  definitionLabel: { color: 'var(--agent-runtime-muted, #667085)' },
  details: { fontSize: 12 },
};

export function AgentRunPanel(props) {
  const state = useAgentRun(props);
  return React.createElement(AgentRunView, { ...props, ...state });
}

export function AgentRunView({
  run,
  messages = [],
  artifacts = [],
  interactions = [],
  toolCalls = [],
  events = [],
  execution,
  loading,
  error,
  actions,
  renderArtifact,
  renderInteraction,
  renderToolCall,
}) {
  const status = getAgentRunDisplayStatus(run);
  const statusMeta = STATUS_META[status] ?? { label: status, tone: 'muted' };
  return React.createElement('section', { style: styles.panel, 'data-agent-runtime-panel': true },
    React.createElement('header', { style: styles.header },
      React.createElement('div', null,
        React.createElement('h2', { style: styles.title }, run ? `Run ${run.id}` : 'Agent run'),
        React.createElement('div', { style: styles.muted }, run ? `${run.target?.type ?? 'target'} / ${run.target?.id ?? ''}` : 'No run loaded'),
      ),
      React.createElement('span', { style: styles.badge, 'data-tone': statusMeta.tone }, loading ? 'Loading' : statusMeta.label),
    ),
    error ? React.createElement('div', { style: styles.row, role: 'alert' }, error.message ?? String(error)) : null,
    React.createElement(RunSummary, { run, execution }),
    React.createElement(InteractionList, { interactions, actions, renderInteraction }),
    React.createElement(EventList, { events }),
    React.createElement(MessageList, { messages }),
    React.createElement(ToolCallList, { toolCalls, renderToolCall }),
    React.createElement(ArtifactList, { artifacts, renderArtifact }),
    actions ? React.createElement(RunComposer, { actions }) : null,
  );
}

export function RunSummary({ run, execution }) {
  if (!run) return null;
  const rows = [
    ['Agent', run.agent_id],
    ['Runtime', run.runtime_kind],
    ['Execution', run.execution_mode],
    ['Invocation', run.invocation_mode],
    ['Workflow', execution?.workflow_id],
    ['Task queue', execution?.task_queue],
    ['History events', execution?.history_length],
    ['Inspection error', execution?.error],
  ];
  return React.createElement('section', { style: styles.section },
    React.createElement('h3', { style: styles.sectionTitle }, 'Overview'),
    React.createElement('dl', { style: styles.definitionGrid }, rows.map(([label, value]) => React.createElement(React.Fragment, { key: label },
      React.createElement('dt', { style: styles.definitionLabel }, label),
      React.createElement('dd', { style: { margin: 0, wordBreak: 'break-word' } }, value ?? '—'),
    ))),
  );
}

export function EventList({ events = [] }) {
  if (events.length === 0) return null;
  return React.createElement('section', { style: styles.section },
    React.createElement('h3', { style: styles.sectionTitle }, 'Timeline'),
    events.map((event) => React.createElement('article', { key: event.event_id ?? event.sequence_no, style: styles.row },
      React.createElement('div', { style: styles.muted }, `${event.sent_at ? new Date(event.sent_at).toLocaleString() : ''} · ${event.type ?? 'event'}`),
      event.data && Object.keys(event.data).length > 0
        ? React.createElement('details', { style: styles.details },
          React.createElement('summary', null, 'Event data'),
          React.createElement('pre', { style: styles.pre }, JSON.stringify(event.data, null, 2).slice(0, 4000)),
        )
        : null,
    )),
  );
}

export function MessageList({ messages = [] }) {
  return React.createElement('section', { style: styles.section },
    React.createElement('h3', { style: styles.sectionTitle }, 'Transcript'),
    messages.length === 0
      ? React.createElement('div', { style: styles.muted }, 'No messages yet.')
      : messages.map((message) => React.createElement('article', { key: message.id ?? message.sequence_no, style: styles.row },
        React.createElement('div', { style: styles.muted }, `${message.role ?? 'message'} #${message.sequence_no ?? ''}`),
        React.createElement('pre', { style: styles.pre }, message.content ?? ''),
      )),
  );
}

export function ToolCallList({ toolCalls = [], renderToolCall }) {
  return React.createElement('section', { style: styles.section },
    React.createElement('h3', { style: styles.sectionTitle }, 'Tool calls'),
    toolCalls.length === 0
      ? React.createElement('div', { style: styles.muted }, 'No tool calls yet.')
      : toolCalls.map((toolCall) => renderToolCall
        ? renderToolCall(toolCall)
        : React.createElement('article', { key: toolCall.id, style: styles.row },
          React.createElement('div', { style: styles.muted }, `${toolCall.tool_name ?? 'tool'}${toolCall.error ? ' failed' : ''}`),
          React.createElement('pre', { style: styles.pre }, JSON.stringify({
            input: toolCall.input ?? {},
            output: toolCall.output ?? null,
            error: toolCall.error ?? '',
          }, null, 2).slice(0, 4000)),
        )),
  );
}

export function ArtifactList({ artifacts = [], renderArtifact }) {
  return React.createElement('section', { style: styles.section },
    React.createElement('h3', { style: styles.sectionTitle }, 'Artifacts'),
    artifacts.length === 0
      ? React.createElement('div', { style: styles.muted }, 'No artifacts yet.')
      : artifacts.map((artifact) => renderArtifact
        ? renderArtifact(artifact)
        : React.createElement('article', { key: artifact.id ?? artifact.sequence_no, style: styles.row },
          React.createElement('div', { style: styles.muted }, `${artifactLabel(artifact.artifact_type)} (${artifact.format ?? 'text'})`),
          React.createElement('pre', { style: styles.pre }, String(artifact.inline_content ?? '').slice(0, 4000)),
        )),
  );
}

export function InteractionList({ interactions = [], actions, renderInteraction }) {
  if (interactions.length === 0) return null;
  return React.createElement('section', { style: styles.section },
    React.createElement('h3', { style: styles.sectionTitle }, 'Interactions'),
    interactions.map((interaction) => renderInteraction
      ? renderInteraction(interaction)
      : React.createElement('article', { key: interaction.id, style: styles.row },
        React.createElement('div', { style: styles.title }, interaction.title || interaction.interaction_kind),
        interaction.summary ? React.createElement('p', { style: styles.muted }, interaction.summary) : null,
        React.createElement('div', { style: styles.muted }, `${interaction.status ?? 'unknown'}${interaction.resolved_by_external_id ? ` · ${interaction.resolved_by_external_id}` : ''}`),
        interaction.request_payload ? React.createElement('details', { style: styles.details }, React.createElement('summary', null, 'Request'), React.createElement('pre', { style: styles.pre }, JSON.stringify(interaction.request_payload, null, 2))) : null,
        interaction.response_payload ? React.createElement('details', { style: styles.details }, React.createElement('summary', null, 'Response'), React.createElement('pre', { style: styles.pre }, JSON.stringify(interaction.response_payload, null, 2))) : null,
        actions && interaction.status === 'pending' ? React.createElement('div', { style: styles.actions },
          React.createElement('button', { type: 'button', style: styles.button, onClick: () => actions.approve() }, 'Approve'),
        ) : null,
      )),
  );
}

export function RunComposer({ actions }) {
  const [content, setContent] = useState('');
  async function submit() {
    const text = content.trim();
    if (!text) return;
    setContent('');
    await actions.sendMessage(text);
  }
  return React.createElement('form', {
    style: styles.section,
    onSubmit: (event) => {
      event.preventDefault();
      void submit();
    },
  },
    React.createElement('textarea', {
      style: styles.textarea,
      value: content,
      onChange: (event) => setContent(event.target.value),
      placeholder: 'Reply to the agent',
    }),
    React.createElement('div', { style: styles.actions },
      React.createElement('button', { type: 'submit', style: styles.button }, 'Send'),
      React.createElement('button', { type: 'button', style: styles.button, onClick: () => actions.cancel() }, 'Cancel run'),
    ),
  );
}
