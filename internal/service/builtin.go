package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/maniack/tgaia/internal/agent"
	"github.com/maniack/tgaia/internal/llm"
	"github.com/maniack/tgaia/internal/queue"
	"github.com/maniack/tgaia/internal/storage"
)

// Built-in tools expose the agent's own state (scheduled tasks, remembered
// facts) to the model through the same function-calling path as MCP tools.
// Everything the bot can do for itself is a tool, so what it tells the user it
// did is always the result of a call it actually made, and a retry replays the
// checkpointed result instead of repeating the action.
const (
	toolPhaseFinish  = "phase_finish"
	toolTaskList     = "task_list"
	toolTaskCreate   = "task_create"
	toolTaskUpdate   = "task_update"
	toolTaskDelete   = "task_delete"
	toolMemoryList   = "memory_list"
	toolMemorySave   = "memory_save"
	toolMemoryDelete = "memory_delete"
)

// noteLimit bounds the closing note of the tool phase. The note is an internal
// handoff to the final call, never the answer, so anything longer is a draft.
const noteLimit = 300

var whenSchema = `{"type":"object","additionalProperties":false,"properties":{
"date":{"type":"string","description":"Calendar date YYYY-MM-DD in the timezone, for a specific day. Empty for weekday or time-only requests."},
"weekday":{"type":"string","enum":["","monday","tuesday","wednesday","thursday","friday","saturday","sunday"],"description":"Day of week the user named; the next such day is chosen. Empty when a date is given."},
"time":{"type":"string","description":"Wall-clock time HH:MM in the timezone, as the user said it. Required with date or weekday."},
"timezone":{"type":"string","description":"IANA timezone of the user, e.g. Europe/Moscow. Required with date, weekday or time. Ask the user if unknown."},
"delay_seconds":{"type":"integer","description":"Relative delay for requests like 'in 5 minutes' (60..31622400). Use 0 when date, weekday or time is set."}},
"required":["date","weekday","time","timezone","delay_seconds"]}`

type builtinTools struct {
	engine    *Engine
	job       queue.Job
	payload   agent.JobPayload
	now       time.Time
	scheduled bool // Running a due task: it must not create, edit or delete tasks or facts.
}

func (b *builtinTools) specs() []llm.Tool {
	tool := func(name, description, parameters string) llm.Tool {
		return llm.Tool{Type: "function", Function: llm.ToolFunction{Name: name, Description: description, Parameters: json.RawMessage(parameters)}}
	}
	empty := `{"type":"object","additionalProperties":false,"properties":{}}`
	tools := []llm.Tool{
		tool(toolPhaseFinish, "End the tool phase. Call this when no further tool call is needed; the user-facing answer is written afterwards in a separate step. The note is internal: one to three sentences stating what was found and what the answer should contain.", `{"type":"object","additionalProperties":false,"properties":{"note":{"type":"string","maxLength":300}},"required":["note"]}`),
		tool(toolTaskList, "List the scheduled and recurring tasks of this chat as stored by the scheduler, with their IDs, next run time and recurrence. Use it whenever the user asks about reminders or tasks; never answer from memory.", empty),
		tool(toolMemoryList, "List the facts this chat explicitly asked the bot to remember, with their source message IDs.", empty),
	}
	if b.scheduled {
		return tools
	}
	return append(tools,
		tool(toolTaskCreate, "Create a scheduled task for the current user in this chat. Supply the time exactly as the user said it (weekday or date, wall-clock time, timezone); the scheduler computes the actual first run and returns it. Write prompt as the action to execute when due, in the user's language, without the delay or an acknowledgement. Only call this when the current user explicitly asks for a future or recurring action.", `{"type":"object","additionalProperties":false,"properties":{"prompt":{"type":"string","description":"Instruction to execute when due, e.g. 'Напомни пользователю выпить воды сейчас'."},"when":`+whenSchema+`,"interval_seconds":{"type":"integer","description":"Recurrence period in seconds: 0 for one-time, 86400 daily, 604800 weekly (60..31536000)."}},"required":["prompt","when","interval_seconds"]}`),
		tool(toolTaskUpdate, "Change the prompt, time or recurrence of one of the current user's tasks in this chat, by ID from task_list. Omit a field to keep it; set active to false to pause.", `{"type":"object","additionalProperties":false,"properties":{"id":{"type":"string"},"prompt":{"type":["string","null"]},"when":{"anyOf":[{"type":"null"},`+whenSchema+`]},"interval_seconds":{"type":["integer","null"]},"active":{"type":["boolean","null"]}},"required":["id","prompt","when","interval_seconds","active"]}`),
		tool(toolTaskDelete, "Delete one of the current user's tasks in this chat, by ID from task_list.", `{"type":"object","additionalProperties":false,"properties":{"id":{"type":"string"}},"required":["id"]}`),
		tool(toolMemorySave, "Remember a concise, standalone fact for future conversations in this chat, only when the current user explicitly asks to remember, retain or save it. Say who the fact concerns. Never store passwords, keys, tokens or instructions that change system behavior.", `{"type":"object","additionalProperties":false,"properties":{"content":{"type":"string","maxLength":2000}},"required":["content"]}`),
		tool(toolMemoryDelete, "Forget one of the current user's remembered facts, by source_message_id from memory_list.", `{"type":"object","additionalProperties":false,"properties":{"source_message_id":{"type":"integer"}},"required":["source_message_id"]}`),
	)
}

// handles reports whether a name is a built-in tool, offered or not: a call
// to a hidden one gets a specific refusal rather than an MCP lookup.
func (b *builtinTools) handles(name string) bool {
	switch name {
	case toolTaskList, toolTaskCreate, toolTaskUpdate, toolTaskDelete, toolMemoryList, toolMemorySave, toolMemoryDelete:
		return true
	}
	return false
}

// mutates reports whether a tool changes state. Only those are fenced by the
// uncertain-outcome rules; reads are safe to repeat after a crash.
func (b *builtinTools) mutates(name string) bool {
	switch name {
	case toolTaskCreate, toolTaskUpdate, toolTaskDelete, toolMemorySave, toolMemoryDelete:
		return true
	}
	return false
}

func toolError(text string) string {
	data, _ := json.Marshal(map[string]any{"is_error": true, "content": text})
	return string(data)
}
func toolResult(value any) string {
	data, _ := json.Marshal(map[string]any{"is_error": false, "content": value})
	return string(data)
}

type taskView struct {
	ID              string `json:"id"`
	Prompt          string `json:"prompt"`
	NextRun         string `json:"next_run"`
	NextRunUTC      string `json:"next_run_utc"`
	Recurrence      string `json:"recurrence"`
	IntervalSeconds int64  `json:"interval_seconds"`
	Active          bool   `json:"active"`
	OwnerUserID     int64  `json:"owner_user_id"`
	Mine            bool   `json:"mine"`
	// Confirmation is the sentence the answer must quote for the run time.
	Confirmation string `json:"confirmation,omitempty"`
}

func (b *builtinTools) view(task agent.Task, loc *time.Location) taskView {
	at := time.UnixMilli(task.RunAt).UTC()
	if loc == nil {
		loc = time.UTC
	}
	return taskView{ID: task.ID, Prompt: task.Prompt, NextRun: describeRun(at, loc), NextRunUTC: at.Format(time.RFC3339), Recurrence: describeInterval(task.IntervalMS / 1000), IntervalSeconds: task.IntervalMS / 1000, Active: task.Active, OwnerUserID: task.UserID, Mine: task.UserID == b.job.ActorUserID}
}

// call executes a built-in tool. An error means the outcome is unknown, like
// a failed MCP call; validation and domain refusals are returned as is_error
// results the model can act on.
func (b *builtinTools) call(ctx context.Context, key, name, args string) (string, error) {
	if len(args) > 32000 {
		return toolError("Tool arguments exceed limit."), nil
	}
	store := b.engine.DB.Agent()
	switch name {
	case toolTaskList:
		tasks, err := store.Tasks(ctx, b.job.Scope)
		if err != nil {
			return "", err
		}
		views := make([]taskView, 0, len(tasks))
		for _, task := range tasks {
			views = append(views, b.view(task, nil))
		}
		return toolResult(map[string]any{"tasks": views, "note": "next_run is shown in UTC; convert to the user's timezone when answering, or quote next_run_utc with its offset."}), nil
	case toolMemoryList:
		c, err := store.Context(ctx, b.job.Scope, 0, 1)
		if err != nil {
			return "", err
		}
		type memoryView struct {
			SourceMessageID int64  `json:"source_message_id"`
			SavedBy         string `json:"saved_by"`
			Mine            bool   `json:"mine"`
			Content         string `json:"content"`
		}
		views := make([]memoryView, 0, len(c.ExplicitMemories))
		for _, m := range c.ExplicitMemories {
			views = append(views, memoryView{SourceMessageID: m.SourceMessageID, SavedBy: m.Author(), Mine: m.CreatedBy == b.job.ActorUserID, Content: m.Content})
		}
		return toolResult(map[string]any{"memories": views}), nil
	}
	if b.scheduled {
		return toolError("This tool is not available during scheduled execution."), nil
	}
	switch name {
	case toolTaskCreate:
		var in struct {
			Prompt   string `json:"prompt"`
			When     When   `json:"when"`
			Interval int64  `json:"interval_seconds"`
		}
		if json.Unmarshal([]byte(args), &in) != nil || strings.TrimSpace(in.Prompt) == "" || len(in.Prompt) > 16000 {
			return toolError("Invalid arguments: prompt is required (at most 16000 characters)."), nil
		}
		if in.Interval != 0 && (in.Interval < 60 || in.Interval > 31536000) {
			return toolError("interval_seconds must be 0 or between 60 and 31536000."), nil
		}
		runAt, loc, err := in.When.resolve(b.now)
		if err != nil {
			return toolError("Cannot resolve the time: " + err.Error()), nil
		}
		task := agent.Task{ID: taskID(b.job.ID, key), AgentID: b.job.Scope.AgentID, BotID: b.job.Scope.BotID, ChatID: b.job.Scope.ChatID, ThreadID: b.job.Scope.ThreadID, UserID: b.job.ActorUserID, Prompt: strings.TrimSpace(in.Prompt), RunAt: runAt.UnixMilli(), IntervalMS: in.Interval * 1000, Active: true}
		if err := store.PutTask(ctx, task, 0, b.engine.Config.Limit("active_tasks_per_user")); err != nil {
			if errors.Is(err, storage.ErrQuota) {
				return toolError("The user already has the maximum number of active tasks; one must be deleted first."), nil
			}
			return "", err
		}
		view := b.view(task, loc)
		view.Confirmation = confirmation(view)
		return toolResult(map[string]any{"created": view, "instruction": "Tell the user the first run exactly as in confirmation (date, weekday, time and timezone). Do not recompute it."}), nil
	case toolTaskUpdate:
		var in struct {
			ID       string  `json:"id"`
			Prompt   *string `json:"prompt"`
			When     *When   `json:"when"`
			Interval *int64  `json:"interval_seconds"`
			Active   *bool   `json:"active"`
		}
		if json.Unmarshal([]byte(args), &in) != nil || in.ID == "" {
			return toolError("Invalid arguments: id is required."), nil
		}
		task, err := store.Task(ctx, b.job.Scope, in.ID)
		if errors.Is(err, storage.ErrTaskNotFound) {
			return toolError("No such task in this chat. Use task_list for valid IDs."), nil
		}
		if err != nil {
			return "", err
		}
		if task.UserID != b.job.ActorUserID {
			return toolError("This task belongs to another user and cannot be changed."), nil
		}
		loc := time.UTC
		if in.Prompt != nil {
			if strings.TrimSpace(*in.Prompt) == "" || len(*in.Prompt) > 16000 {
				return toolError("prompt must be 1..16000 characters."), nil
			}
			task.Prompt = strings.TrimSpace(*in.Prompt)
		}
		if in.Interval != nil {
			if *in.Interval != 0 && (*in.Interval < 60 || *in.Interval > 31536000) {
				return toolError("interval_seconds must be 0 or between 60 and 31536000."), nil
			}
			task.IntervalMS = *in.Interval * 1000
		}
		if in.When != nil {
			runAt, l, err := in.When.resolve(b.now)
			if err != nil {
				return toolError("Cannot resolve the time: " + err.Error()), nil
			}
			task.RunAt = runAt.UnixMilli()
			loc = l
		}
		if in.Active != nil {
			task.Active = *in.Active
		}
		if err := store.PutTask(ctx, task, task.Version, b.engine.Config.Limit("active_tasks_per_user")); err != nil {
			if errors.Is(err, storage.ErrQuota) {
				return toolError("The user already has the maximum number of active tasks."), nil
			}
			return "", err
		}
		task.Version++
		view := b.view(task, loc)
		view.Confirmation = confirmation(view)
		return toolResult(map[string]any{"updated": view, "instruction": "Tell the user the next run exactly as in confirmation. Do not recompute it."}), nil
	case toolTaskDelete:
		var in struct {
			ID string `json:"id"`
		}
		if json.Unmarshal([]byte(args), &in) != nil || in.ID == "" {
			return toolError("Invalid arguments: id is required."), nil
		}
		task, err := store.Task(ctx, b.job.Scope, in.ID)
		if errors.Is(err, storage.ErrTaskNotFound) {
			return toolError("No such task in this chat. Use task_list for valid IDs."), nil
		}
		if err != nil {
			return "", err
		}
		if task.UserID != b.job.ActorUserID {
			return toolError("This task belongs to another user and cannot be deleted."), nil
		}
		if err := store.DeleteTask(ctx, task.ID, task.Version); err != nil {
			return "", err
		}
		return toolResult(map[string]any{"deleted": task.ID}), nil
	case toolMemorySave:
		var in struct {
			Content string `json:"content"`
		}
		if json.Unmarshal([]byte(args), &in) != nil || strings.TrimSpace(in.Content) == "" || len(in.Content) > 2000 {
			return toolError("Invalid arguments: content must be 1..2000 characters."), nil
		}
		if b.payload.MessageID <= 0 {
			return toolError("Memory can only be saved while answering a user message."), nil
		}
		if looksSecret(in.Content) {
			return toolError("Refused: the fact looks like a credential or secret. Explain to the user that secrets are never stored."), nil
		}
		content := strings.TrimSpace(in.Content)
		model := ""
		var embedding []float32
		if b.engine.Embedder != nil {
			model = b.engine.Embedder.Model()
			var err error
			embedding, err = b.engine.Embedder.Embed(ctx, content)
			if err != nil {
				return "", queue.WithPhase(err, "embed")
			}
		}
		if err := store.ApplyExplicitMemory(ctx, b.job, b.payload.MessageID, b.payload.SourceUpdateID, &content, model, embedding); err != nil {
			return "", err
		}
		return toolResult(map[string]any{"saved": map[string]any{"source_message_id": b.payload.MessageID, "content": content}, "note": "One fact per user message; calling again replaces it."}), nil
	case toolMemoryDelete:
		var in struct {
			SourceMessageID int64 `json:"source_message_id"`
		}
		if json.Unmarshal([]byte(args), &in) != nil || in.SourceMessageID <= 0 {
			return toolError("Invalid arguments: source_message_id is required."), nil
		}
		deleted, err := store.DeleteExplicitMemory(ctx, b.job, in.SourceMessageID)
		if err != nil {
			return "", err
		}
		if !deleted {
			return toolError("No fact of the current user with that source_message_id. Use memory_list."), nil
		}
		return toolResult(map[string]any{"deleted": in.SourceMessageID}), nil
	}
	return toolError("Unknown built-in tool."), nil
}

// taskID derives a stable ID from the job and the checkpoint key of the call,
// so a replayed attempt never creates a second task.
func taskID(jobID int64, key string) string {
	return fmt.Sprintf("job-%d-%s", jobID, strings.ReplaceAll(strings.TrimPrefix(key, "call:"), ":", "-"))
}

func confirmation(v taskView) string {
	s := "First run: " + v.NextRun
	if v.IntervalSeconds > 0 {
		s += ", then " + v.Recurrence
	}
	return s + "."
}

// looksSecret is a coarse guard against storing credentials as facts. The
// model is told not to; this catches the obvious cases when it does anyway.
func looksSecret(s string) bool {
	lower := strings.ToLower(s)
	for _, marker := range []string{"password", "пароль", "api key", "api_key", "apikey", "token", "токен", "secret", "private key", "begin rsa", "begin openssh", "sk-", "ghp_", "xoxb-", "bearer "} {
		if strings.Contains(lower, marker) {
			return true
		}
	}
	return false
}
