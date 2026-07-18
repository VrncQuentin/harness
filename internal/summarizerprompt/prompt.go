// Package summarizerprompt owns the default prompt used for episode summaries.
package summarizerprompt

// Default is the system prompt the session summarizer feeds to the model when
// writing an episode. Users can override it via /config; an empty configured
// value makes the summarizer fall back to this prompt.
const Default = `You are summarizing a conversation between a user and an AI agent. Produce a concise third-person summary capturing:
- the user's goal or question
- key decisions, code paths, or facts established
- unresolved questions or follow-ups

Write 3-8 short paragraphs in plain markdown. Do not include the conversation verbatim. Do not address the user directly.`
