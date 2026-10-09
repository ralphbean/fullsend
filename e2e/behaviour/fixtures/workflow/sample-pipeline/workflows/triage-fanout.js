export const meta = {
  name: 'triage-fanout',
  description: 'Read an issue, check each checklist item in parallel, then report',
  phases: [
    { title: 'Read', detail: 'read the issue named in args' },
    { title: 'Check', detail: 'one agent per checklist item' },
    { title: 'Report', detail: 'write the agent result' },
  ],
};

const ISSUE_SCHEMA = {
  type: 'object',
  properties: {
    title: { type: 'string' },
    items: { type: 'array', items: { type: 'string' } },
  },
  required: ['title', 'items'],
};

const ITEM_SCHEMA = {
  type: 'object',
  properties: {
    item: { type: 'string' },
    ready: { type: 'boolean' },
    reason: { type: 'string' },
  },
  required: ['item', 'ready'],
};

phase('Read');
const issue = await agent(
  `Use the read-issue skill to read ${args}. Return its title and its unchecked checklist items.`,
  { schema: ISSUE_SCHEMA },
);
if (!issue) {
  throw new Error(`could not read ${args}`);
}

phase('Check');
// One result per item, in item order: a null result is an item whose
// check failed, so it is kept, not filtered away.
const checks = await parallel(issue.items.map((item, i) => () =>
  agent(`Issue "${issue.title}", checklist item ${i + 1}: "${item}". Is it specific enough to implement? Answer ready and a one-line reason.`, {
    label: `item ${i + 1}`,
    phase: 'Check',
    schema: ITEM_SCHEMA,
  })));
const failed = issue.items.filter((_, i) => !checks[i]);
const readyCount = checks.filter((c) => c && c.ready).length;
const ready = issue.items.length > 0 && failed.length === 0 && readyCount === issue.items.length;
log(`${readyCount}/${issue.items.length} checklist items ready, ${failed.length} not checked`);

phase('Report');
const status = ready ? 'ok' : 'findings';
const lines = issue.items.map((item, i) => {
  const c = checks[i];
  if (!c) {
    return `- not checked (the check failed): ${item}`;
  }
  return `- ${c.ready ? 'ready' : 'needs detail'}: ${item}${c.reason ? ` (${c.reason})` : ''}`;
});
await agent(`Write this JSON object, and nothing else, to the file named by $FULLSEND_OUTPUT_DIR/agent-result.json: ${JSON.stringify({
  status,
  summary: `${readyCount} of ${issue.items.length} checklist items are ready, ${failed.length} not checked`,
  comment: `### Checklist review\n\n${lines.join('\n')}\n`,
})}`, { label: 'report' });

return { issue: issue.title, status, checks, failed };
