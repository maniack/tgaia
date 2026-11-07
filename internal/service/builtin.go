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
	"github.com/maniack/tgaia/internal/messages"
	"github.com/maniack/tgaia/internal/queue"
	skillruntime "github.com/maniack/tgaia/internal/skills"
	"github.com/maniack/tgaia/internal/storage"
)

// Built-in tools expose the agent's own state (scheduled tasks, remembered
// facts) to the model through the same function-calling path as MCP tools.
// Everything the bot can do for itself is a tool, so what it tells the user it
// did is always the result of a call it actually made, and a retry replays the
// checkpointed result instead of repeating the action.
const (
	toolPhaseFinish    = "phase_finish"
	toolTaskList       = "task_list"
	toolTaskCreate     = "task_create"
	toolTaskUpdate     = "task_update"
	toolTaskDelete     = "task_delete"
	toolMemoryList     = "memory_list"
	toolMemorySave     = "memory_save"
	toolMemoryUpdate   = "memory_update"
	toolMemoryDelete   = "memory_delete"
	toolMessageList    = "message_list"
	toolMessageSend    = "message_send"
	toolMessageEdit    = "message_edit"
	toolMessageDelete  = "message_delete"
	toolModerateDelete = "moderation_delete_message"
	toolModerateBan    = "moderation_ban_member"
	toolModerateUnban  = "moderation_unban_member"
	toolModerateMute   = "moderation_restrict_member"
	toolModeratePin    = "moderation_pin_message"
	toolSkillList      = "skill_list"
	toolSkillCreate    = "skill_create"
	toolSkillUpdate    = "skill_update"
	toolSkillTest      = "skill_test"
	toolSkillRun       = "skill_run"
	toolSkillDelete    = "skill_delete"
	toolVFSList        = "vfs_list"
	toolVFSRead        = "vfs_read"
	toolVFSWrite       = "vfs_write"
	toolVFSDelete      = "vfs_delete"
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
	engine     *Engine
	job        queue.Job
	payload    agent.JobPayload
	now        time.Time
	scheduled  bool // Running a due task: it must not create, edit or delete tasks or facts.
	moderation map[string]bool
}

type moderationSender interface {
	ModerationPermissions(context.Context, int64, int64) (map[string]bool, error)
	BanMember(context.Context, int64, int64) error
	UnbanMember(context.Context, int64, int64) error
	RestrictMember(context.Context, int64, int64, int64) error
	UnrestrictMember(context.Context, int64, int64) error
	PinMessage(context.Context, int64, int64) error
	UnpinMessage(context.Context, int64, int64) error
}

func (b *builtinTools) specs() []llm.Tool {
	tool := func(name, description, parameters string) llm.Tool {
		return llm.Tool{Type: "function", Function: llm.ToolFunction{Name: name, Description: description, Parameters: json.RawMessage(parameters)}}
	}
	empty := `{"type":"object","additionalProperties":false,"properties":{}}`
	tools := []llm.Tool{
		tool(toolPhaseFinish, "End the tool phase. Call this when no further tool call is needed; the user-facing answer is written afterwards in a separate step. The note is internal: one to three sentences stating what was found and what the answer should contain.", `{"type":"object","additionalProperties":false,"properties":{"note":{"type":"string","maxLength":300}},"required":["note"]}`),
		tool(toolTaskList, "List the scheduled and recurring tasks of this chat as stored by the scheduler, with their IDs, next run time and recurrence. Use it whenever the user asks about reminders or tasks; never answer from memory.", empty),
		tool(toolMemoryList, "List the durable facts remembered about people in this chat, with their source message IDs.", empty),
		tool(toolMessageList, "List messages sent by this bot in the current chat/topic, with Telegram message IDs, text previews and edit/delete state. Use it before editing or deleting and match the requested text to the preview instead of guessing by recency. Pass after_message_id=0 when locating a target; a nonzero value is an exclusive pagination cursor and omits that message itself.", `{"type":"object","additionalProperties":false,"properties":{"after_message_id":{"type":"integer","minimum":0},"limit":{"type":"integer","minimum":1,"maximum":50}},"required":["after_message_id","limit"]}`),
		tool(toolSkillList, "List database-backed Starlark skills for this bot, including their active or failed latest version.", empty),
		tool(toolSkillRun, "Run an active skill by name with a JSON input string. The skill is sandboxed and may use its scoped virtual filesystem.", `{"type":"object","additionalProperties":false,"properties":{"name":{"type":"string"},"input_json":{"type":"string","maxLength":64000}},"required":["name","input_json"]}`),
		tool(toolVFSList, "List files in one virtual filesystem namespace scoped to the current user, chat or bot.", `{"type":"object","additionalProperties":false,"properties":{"namespace":{"type":"string","enum":["user","chat","bot"]},"prefix":{"type":"string","maxLength":512}},"required":["namespace","prefix"]}`),
		tool(toolVFSRead, "Read a text file from /user, /chat or /bot virtual storage. This never accesses the host filesystem.", `{"type":"object","additionalProperties":false,"properties":{"path":{"type":"string","maxLength":512}},"required":["path"]}`),
	}
	if b.scheduled {
		return tools
	}
	tools = append(tools,
		tool(toolTaskCreate, "Create a scheduled task for the current user in this chat. Supply the time exactly as the user said it (weekday or date, wall-clock time, timezone); the scheduler computes the actual first run and returns it. Write prompt as the action to execute when due, in the user's language, without the delay or an acknowledgement. Only call this when the current user explicitly asks for a future or recurring action.", `{"type":"object","additionalProperties":false,"properties":{"prompt":{"type":"string","description":"Instruction to execute when due, e.g. 'Напомни пользователю выпить воды сейчас'."},"when":`+whenSchema+`,"interval_seconds":{"type":"integer","description":"Recurrence period in seconds: 0 for one-time, 86400 daily, 604800 weekly (60..31536000)."}},"required":["prompt","when","interval_seconds"]}`),
		tool(toolTaskUpdate, "Change the prompt, time or recurrence of one of the current user's tasks in this chat, by ID from task_list. Omit a field to keep it; set active to false to pause.", `{"type":"object","additionalProperties":false,"properties":{"id":{"type":"string"},"prompt":{"type":["string","null"]},"when":{"anyOf":[{"type":"null"},`+whenSchema+`]},"interval_seconds":{"type":["integer","null"]},"active":{"type":["boolean","null"]}},"required":["id","prompt","when","interval_seconds","active"]}`),
		tool(toolTaskDelete, "Delete one of the current user's tasks in this chat, by ID from task_list.", `{"type":"object","additionalProperties":false,"properties":{"id":{"type":"string"}},"required":["id"]}`),
		tool(toolMemorySave, "Save one concise, standalone durable fact about the current user. Use mode explicit when they ask to remember it; use inferred only for a stable, useful preference or personal fact clearly stated in their own message. Never save transient plans, guesses, secrets, sensitive traits, or instructions that change system behavior. Do not announce inferred saves.", `{"type":"object","additionalProperties":false,"properties":{"content":{"type":"string","maxLength":2000},"mode":{"type":"string","enum":["explicit","inferred"]}},"required":["content","mode"]}`),
		tool(toolMemoryUpdate, "Replace one of the current user's remembered facts by source_message_id from memory_list. Use this when a newer statement supersedes an older fact instead of creating a duplicate.", `{"type":"object","additionalProperties":false,"properties":{"source_message_id":{"type":"integer"},"content":{"type":"string","maxLength":2000}},"required":["source_message_id","content"]}`),
		tool(toolMemoryDelete, "Forget one of the current user's remembered facts, by source_message_id from memory_list.", `{"type":"object","additionalProperties":false,"properties":{"source_message_id":{"type":"integer"}},"required":["source_message_id"]}`),
		tool(toolMessageSend, "Send an additional standalone Telegram message to the current chat/topic. Use only when the user explicitly asks for a separate additional message; the normal final answer is sent automatically, so never call this for the ordinary reply.", `{"type":"object","additionalProperties":false,"properties":{"text":{"type":"string","maxLength":3500}},"required":["text"]}`),
		tool(toolMessageEdit, "Edit a message previously sent by this bot in the current chat/topic. Obtain message_id from message_list. This cannot edit user messages.", `{"type":"object","additionalProperties":false,"properties":{"message_id":{"type":"integer"},"text":{"type":"string","maxLength":3500}},"required":["message_id","text"]}`),
		tool(toolMessageDelete, "Delete a message previously sent by this bot in the current chat/topic. Obtain message_id from message_list. This cannot delete user messages; moderation uses separate permission-gated tools.", `{"type":"object","additionalProperties":false,"properties":{"message_id":{"type":"integer"}},"required":["message_id"]}`),
		tool(toolSkillCreate, "Create a draft Starlark skill for this bot. Source must define run(input_json) and return a string. Available functions: vfs_read(path), vfs_write(path, content), vfs_list(namespace), vfs_delete(path). Test it before use.", `{"type":"object","additionalProperties":false,"properties":{"name":{"type":"string","pattern":"^[a-z][a-z0-9_]{1,63}$"},"description":{"type":"string","maxLength":500},"source":{"type":"string","maxLength":64000}},"required":["name","description","source"]}`),
		tool(toolSkillUpdate, "Create a new draft version of an existing skill. The previous active version remains available until this version passes skill_test.", `{"type":"object","additionalProperties":false,"properties":{"name":{"type":"string"},"description":{"type":"string","maxLength":500},"source":{"type":"string","maxLength":64000}},"required":["name","description","source"]}`),
		tool(toolSkillTest, "Run the latest draft skill without committing virtual filesystem writes. A successful test activates that version automatically; failure keeps it inactive.", `{"type":"object","additionalProperties":false,"properties":{"name":{"type":"string"},"input_json":{"type":"string","maxLength":64000}},"required":["name","input_json"]}`),
		tool(toolSkillDelete, "Delete all versions of a skill from this bot.", `{"type":"object","additionalProperties":false,"properties":{"name":{"type":"string"}},"required":["name"]}`),
		tool(toolVFSWrite, "Write a text file to /user, /chat or /bot virtual storage. The host filesystem is never exposed.", `{"type":"object","additionalProperties":false,"properties":{"path":{"type":"string","maxLength":512},"content":{"type":"string","maxLength":256000}},"required":["path","content"]}`),
		tool(toolVFSDelete, "Delete a file from /user, /chat or /bot virtual storage.", `{"type":"object","additionalProperties":false,"properties":{"path":{"type":"string","maxLength":512}},"required":["path"]}`),
	)
	if b.moderation["delete"] {
		tools = append(tools, tool(toolModerateDelete, "Delete any message in the current group when Telegram confirms the bot currently has can_delete_messages. Use only for an explicit moderation request and the exact Telegram message ID.", `{"type":"object","additionalProperties":false,"properties":{"message_id":{"type":"integer"}},"required":["message_id"]}`))
	}
	if b.moderation["ban"] {
		tools = append(tools,
			tool(toolModerateBan, "Ban a member from the current group when Telegram confirms the bot currently has can_restrict_members. Use only for an explicit moderation request and an exact user ID.", `{"type":"object","additionalProperties":false,"properties":{"user_id":{"type":"integer"}},"required":["user_id"]}`),
			tool(toolModerateUnban, "Remove a member's ban in the current group when Telegram confirms the bot currently has can_restrict_members. This does not add them back automatically.", `{"type":"object","additionalProperties":false,"properties":{"user_id":{"type":"integer"}},"required":["user_id"]}`))
	}
	if b.moderation["restrict"] {
		tools = append(tools, tool(toolModerateMute, "Restrict or restore a member's ability to send messages in the current group. Telegram has confirmed can_restrict_members. For restrict, duration_seconds must be 30..31622400; for unrestrict it must be 0.", `{"type":"object","additionalProperties":false,"properties":{"user_id":{"type":"integer"},"action":{"type":"string","enum":["restrict","unrestrict"]},"duration_seconds":{"type":"integer","minimum":0,"maximum":31622400}},"required":["user_id","action","duration_seconds"]}`))
	}
	if b.moderation["pin"] {
		tools = append(tools, tool(toolModeratePin, "Pin or unpin an exact message in the current group when Telegram confirms the bot currently has can_pin_messages.", `{"type":"object","additionalProperties":false,"properties":{"message_id":{"type":"integer"},"action":{"type":"string","enum":["pin","unpin"]}},"required":["message_id","action"]}`))
	}
	return tools
}

// handles reports whether a name is a built-in tool, offered or not: a call
// to a hidden one gets a specific refusal rather than an MCP lookup.
func (b *builtinTools) handles(name string) bool {
	switch name {
	case toolTaskList, toolTaskCreate, toolTaskUpdate, toolTaskDelete, toolMemoryList, toolMemorySave, toolMemoryUpdate, toolMemoryDelete, toolMessageList, toolMessageSend, toolMessageEdit, toolMessageDelete, toolModerateDelete, toolModerateBan, toolModerateUnban, toolModerateMute, toolModeratePin, toolSkillList, toolSkillCreate, toolSkillUpdate, toolSkillTest, toolSkillRun, toolSkillDelete, toolVFSList, toolVFSRead, toolVFSWrite, toolVFSDelete:
		return true
	}
	return false
}

// mutates reports whether a tool changes state. Only those are fenced by the
// uncertain-outcome rules; reads are safe to repeat after a crash.
func (b *builtinTools) mutates(name string) bool {
	switch name {
	case toolTaskCreate, toolTaskUpdate, toolTaskDelete, toolMemorySave, toolMemoryUpdate, toolMemoryDelete, toolMessageSend, toolMessageEdit, toolMessageDelete, toolModerateDelete, toolModerateBan, toolModerateUnban, toolModerateMute, toolModeratePin, toolSkillCreate, toolSkillUpdate, toolSkillTest, toolSkillRun, toolSkillDelete, toolVFSWrite, toolVFSDelete:
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
	case toolMessageList:
		var in struct {
			After int64 `json:"after_message_id"`
			Limit int   `json:"limit"`
		}
		if json.Unmarshal([]byte(args), &in) != nil || in.After < 0 || in.Limit < 1 || in.Limit > 50 {
			return toolError("Invalid arguments: after_message_id must be non-negative and limit must be 1..50."), nil
		}
		records, err := b.engine.DB.BotMessages().List(ctx, b.job.Scope, "", in.After, in.Limit)
		if err != nil {
			return "", err
		}
		type messageView struct {
			MessageID   int64  `json:"message_id"`
			TextPreview string `json:"text_preview,omitempty"`
			SentAt      string `json:"sent_at"`
			EditedAt    string `json:"edited_at,omitempty"`
			DeletedAt   string `json:"deleted_at,omitempty"`
			Version     int64  `json:"version"`
		}
		ids := make([]int64, 0, len(records))
		for _, record := range records {
			ids = append(ids, record.MessageID)
		}
		previews, err := store.BotMessagePreviews(ctx, b.job.Scope, ids)
		if err != nil {
			return "", err
		}
		views := make([]messageView, 0, len(records))
		for _, record := range records {
			view := messageView{MessageID: record.MessageID, TextPreview: previews[record.MessageID], SentAt: record.SentAt.Format(time.RFC3339), Version: record.Version}
			if record.EditedAt != nil {
				view.EditedAt = record.EditedAt.Format(time.RFC3339)
			}
			if record.DeletedAt != nil {
				view.DeletedAt = record.DeletedAt.Format(time.RFC3339)
			}
			views = append(views, view)
		}
		return toolResult(map[string]any{"messages": views}), nil
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
		if err := store.PutTask(ctx, task, 0, b.engine.Config.Limit("active_tasks_per_user"), b.engine.Config.Limit("system_prompt_bytes")); err != nil {
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
		if err := store.PutTask(ctx, task, task.Version, b.engine.Config.Limit("active_tasks_per_user"), b.engine.Config.Limit("system_prompt_bytes")); err != nil {
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
			Mode    string `json:"mode"`
		}
		if json.Unmarshal([]byte(args), &in) != nil || strings.TrimSpace(in.Content) == "" || len(in.Content) > 2000 || (in.Mode != "explicit" && in.Mode != "inferred") {
			return toolError("Invalid arguments: content must be 1..2000 characters and mode must be explicit or inferred."), nil
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
		current, err := store.Context(ctx, b.job.Scope, 0, 1)
		if err != nil {
			return "", err
		}
		for _, remembered := range current.ExplicitMemories {
			if remembered.CreatedBy == b.job.ActorUserID && normalizeFact(remembered.Content) == normalizeFact(content) {
				return toolResult(map[string]any{"duplicate": map[string]any{"source_message_id": remembered.SourceMessageID, "content": remembered.Content}, "note": "The fact was already present; no new fact was created."}), nil
			}
		}
		if model != "" && len(embedding) > 0 {
			similar, score, err := store.SimilarExplicitMemory(ctx, b.job.Scope, b.job.ActorUserID, model, embedding)
			if err != nil {
				return "", err
			}
			if score >= 0.92 {
				return toolResult(map[string]any{"duplicate": map[string]any{"source_message_id": similar.SourceMessageID, "content": similar.Content, "similarity": score}, "note": "A semantically equivalent fact was already present; update it if the new statement supersedes it."}), nil
			}
		}
		if err := store.ApplyExplicitMemory(ctx, b.job, b.payload.MessageID, b.payload.SourceUpdateID, &content, model, embedding); err != nil {
			return "", err
		}
		return toolResult(map[string]any{"saved": map[string]any{"source_message_id": b.payload.MessageID, "content": content, "mode": in.Mode}, "note": "One fact per user message; use memory_update when a later statement supersedes an older fact."}), nil
	case toolMemoryUpdate:
		var in struct {
			SourceMessageID int64  `json:"source_message_id"`
			Content         string `json:"content"`
		}
		if json.Unmarshal([]byte(args), &in) != nil || in.SourceMessageID <= 0 || strings.TrimSpace(in.Content) == "" || len(in.Content) > 2000 {
			return toolError("Invalid arguments: source_message_id and content are required."), nil
		}
		if looksSecret(in.Content) {
			return toolError("Refused: the fact looks like a credential or secret."), nil
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
		updated, err := store.UpdateExplicitMemory(ctx, b.job, in.SourceMessageID, content, model, embedding)
		if err != nil {
			return "", err
		}
		if !updated {
			return toolError("No fact of the current user with that source_message_id. Use memory_list."), nil
		}
		return toolResult(map[string]any{"updated": map[string]any{"source_message_id": in.SourceMessageID, "content": content}}), nil
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
	case toolSkillList:
		skills, err := store.Skills(ctx, b.job.Scope.AgentID, b.job.Scope.BotID)
		if err != nil {
			return "", err
		}
		return toolResult(map[string]any{"skills": skills}), nil
	case toolSkillCreate, toolSkillUpdate:
		var in struct {
			Name        string `json:"name"`
			Description string `json:"description"`
			Source      string `json:"source"`
		}
		if json.Unmarshal([]byte(args), &in) != nil {
			return toolError("Invalid skill arguments."), nil
		}
		if name == toolSkillUpdate {
			if _, err := store.Skill(ctx, b.job.Scope.AgentID, b.job.Scope.BotID, in.Name, false); err != nil {
				return toolError("Skill not found; use skill_create."), nil
			}
		} else if _, err := store.Skill(ctx, b.job.Scope.AgentID, b.job.Scope.BotID, in.Name, false); err == nil {
			return toolError("Skill already exists; use skill_update."), nil
		}
		saved, err := store.SaveSkill(ctx, b.job.Scope.AgentID, b.job.Scope.BotID, b.job.ActorUserID, in.Name, in.Description, in.Source)
		if err != nil {
			return toolError("Invalid skill name, description or source."), nil
		}
		return toolResult(map[string]any{"draft": saved, "next": "Call skill_test with a representative JSON input."}), nil
	case toolSkillTest:
		var in struct {
			Name  string `json:"name"`
			Input string `json:"input_json"`
		}
		if json.Unmarshal([]byte(args), &in) != nil || !json.Valid([]byte(in.Input)) {
			return toolError("name and valid input_json are required."), nil
		}
		skill, err := store.Skill(ctx, b.job.Scope.AgentID, b.job.Scope.BotID, in.Name, false)
		if err != nil {
			return toolError("Skill not found."), nil
		}
		output, runErr := (skillruntime.Runtime{Store: store}).Run(ctx, b.job.Scope, b.job.ActorUserID, skill.Source, in.Input, false)
		failure := ""
		if runErr != nil {
			failure = runErr.Error()
			if len(failure) > 2000 {
				failure = failure[:2000]
			}
		}
		if err := store.FinishSkillTest(ctx, skill, in.Input, output, failure); err != nil {
			return "", err
		}
		if failure != "" {
			return toolError("Skill test failed: " + failure), nil
		}
		return toolResult(map[string]any{"activated": skill.Name, "version": skill.Version, "output": output}), nil
	case toolSkillRun:
		var in struct {
			Name  string `json:"name"`
			Input string `json:"input_json"`
		}
		if json.Unmarshal([]byte(args), &in) != nil || !json.Valid([]byte(in.Input)) {
			return toolError("name and valid input_json are required."), nil
		}
		skill, err := store.Skill(ctx, b.job.Scope.AgentID, b.job.Scope.BotID, in.Name, true)
		if err != nil {
			return toolError("No active skill with that name. Use skill_list."), nil
		}
		output, err := (skillruntime.Runtime{Store: store}).Run(ctx, b.job.Scope, b.job.ActorUserID, skill.Source, in.Input, true)
		if err != nil {
			return toolError("Skill failed: " + err.Error()), nil
		}
		return toolResult(map[string]any{"skill": skill.Name, "version": skill.Version, "output": output}), nil
	case toolSkillDelete:
		var in struct {
			Name string `json:"name"`
		}
		if json.Unmarshal([]byte(args), &in) != nil || in.Name == "" {
			return toolError("name is required."), nil
		}
		if err := store.DeleteSkill(ctx, b.job.Scope.AgentID, b.job.Scope.BotID, in.Name); err != nil {
			return toolError("Invalid skill name."), nil
		}
		return toolResult(map[string]any{"deleted": in.Name}), nil
	case toolVFSList:
		var in struct {
			Namespace string `json:"namespace"`
			Prefix    string `json:"prefix"`
		}
		if json.Unmarshal([]byte(args), &in) != nil {
			return toolError("Invalid virtual file query."), nil
		}
		files, err := store.ListVirtualFiles(ctx, b.job.Scope, b.job.ActorUserID, in.Namespace, in.Prefix)
		if err != nil {
			return toolError("Invalid virtual file query."), nil
		}
		return toolResult(map[string]any{"files": files}), nil
	case toolVFSRead:
		var in struct {
			Path string `json:"path"`
		}
		if json.Unmarshal([]byte(args), &in) != nil {
			return toolError("path is required."), nil
		}
		file, err := store.ReadVirtualFile(ctx, b.job.Scope, b.job.ActorUserID, in.Path)
		if err != nil {
			return toolError("Virtual file not found in the current scope."), nil
		}
		return toolResult(map[string]any{"file": file}), nil
	case toolVFSWrite:
		var in struct {
			Path    string `json:"path"`
			Content string `json:"content"`
		}
		if json.Unmarshal([]byte(args), &in) != nil {
			return toolError("path and content are required."), nil
		}
		if err := store.ApplyVirtualChanges(ctx, b.job.Scope, b.job.ActorUserID, []storage.VirtualFileChange{{Path: in.Path, Content: &in.Content}}); err != nil {
			return toolError("Invalid virtual path or file exceeds limits."), nil
		}
		return toolResult(map[string]any{"written": in.Path, "bytes": len(in.Content)}), nil
	case toolVFSDelete:
		var in struct {
			Path string `json:"path"`
		}
		if json.Unmarshal([]byte(args), &in) != nil {
			return toolError("path is required."), nil
		}
		if err := store.ApplyVirtualChanges(ctx, b.job.Scope, b.job.ActorUserID, []storage.VirtualFileChange{{Path: in.Path}}); err != nil {
			return toolError("Invalid virtual path."), nil
		}
		return toolResult(map[string]any{"deleted": in.Path}), nil
	case toolMessageSend:
		var in struct {
			Text string `json:"text"`
		}
		if json.Unmarshal([]byte(args), &in) != nil || strings.TrimSpace(in.Text) == "" || len([]rune(in.Text)) > 3500 {
			return toolError("Invalid arguments: text must be 1..3500 characters."), nil
		}
		sender := b.engine.Bots[b.job.Scope.BotID]
		if sender == nil {
			return toolError("Bot sender is unavailable."), nil
		}
		text := strings.TrimSpace(in.Text)
		var id int64
		var at time.Time
		var err error
		if topic, ok := sender.(interface {
			SendTopic(context.Context, int64, int64, string) (int64, time.Time, error)
		}); ok {
			id, at, err = topic.SendTopic(ctx, b.job.Scope.ChatID, b.job.Scope.ThreadID, text)
		} else {
			id, at, err = sender.Send(ctx, b.job.Scope.ChatID, text)
		}
		if err != nil {
			return "", err
		}
		deliveryKey := fmt.Sprintf("tool:%d:%s", b.job.ID, key)
		result, err := b.engine.DB.BotMessages().RecordSent(ctx, messages.Sent{Reference: messages.Reference{Scope: b.job.Scope, MessageID: id}, DeliveryKey: deliveryKey, SentAt: at})
		if err != nil || result == messages.Conflict {
			if err == nil {
				err = errors.New("message record conflict")
			}
			return "", err
		}
		if err := store.RecordToolMessage(ctx, b.job, id, text, at); err != nil {
			return "", err
		}
		return toolResult(map[string]any{"sent": map[string]any{"message_id": id, "sent_at": at.Format(time.RFC3339)}}), nil
	case toolMessageEdit:
		var in struct {
			MessageID int64  `json:"message_id"`
			Text      string `json:"text"`
		}
		if json.Unmarshal([]byte(args), &in) != nil || in.MessageID <= 0 || strings.TrimSpace(in.Text) == "" || len([]rune(in.Text)) > 3500 {
			return toolError("Invalid arguments: message_id and text (1..3500 characters) are required."), nil
		}
		ref := messages.Reference{Scope: b.job.Scope, MessageID: in.MessageID}
		current, err := b.engine.DB.BotMessages().Get(ctx, ref)
		if errors.Is(err, messages.ErrNotFound) || current.DeletedAt != nil {
			return toolError("No editable bot message with that ID in the current chat/topic. Use message_list."), nil
		}
		if err != nil {
			return "", err
		}
		sender := b.engine.Bots[b.job.Scope.BotID]
		if sender == nil {
			return toolError("Bot sender is unavailable."), nil
		}
		text := strings.TrimSpace(in.Text)
		if err := sender.Edit(ctx, b.job.Scope.ChatID, in.MessageID, text); err != nil {
			return "", err
		}
		result, err := b.engine.DB.BotMessages().ConfirmEdited(ctx, ref, current.Version, time.Now().UTC())
		if err != nil || result == messages.Conflict {
			if err == nil {
				err = errors.New("message edit version conflict")
			}
			return "", err
		}
		if err := store.SyncMessage(ctx, b.job.Scope, in.MessageID, text, false); err != nil {
			return "", err
		}
		return toolResult(map[string]any{"edited": in.MessageID}), nil
	case toolMessageDelete:
		var in struct {
			MessageID int64 `json:"message_id"`
		}
		if json.Unmarshal([]byte(args), &in) != nil || in.MessageID <= 0 {
			return toolError("Invalid arguments: message_id is required."), nil
		}
		ref := messages.Reference{Scope: b.job.Scope, MessageID: in.MessageID}
		current, err := b.engine.DB.BotMessages().Get(ctx, ref)
		if errors.Is(err, messages.ErrNotFound) {
			return toolError("No bot message with that ID in the current chat/topic. Use message_list."), nil
		}
		if err != nil {
			return "", err
		}
		if current.DeletedAt != nil {
			return toolResult(map[string]any{"deleted": in.MessageID, "unchanged": true}), nil
		}
		sender := b.engine.Bots[b.job.Scope.BotID]
		if sender == nil {
			return toolError("Bot sender is unavailable."), nil
		}
		if err := sender.Delete(ctx, b.job.Scope.ChatID, in.MessageID); err != nil {
			return "", err
		}
		result, err := b.engine.DB.BotMessages().ConfirmDeleted(ctx, ref, current.Version, time.Now().UTC())
		if err != nil || result == messages.Conflict {
			if err == nil {
				err = errors.New("message delete version conflict")
			}
			return "", err
		}
		if err := store.SyncMessage(ctx, b.job.Scope, in.MessageID, "", true); err != nil {
			return "", err
		}
		return toolResult(map[string]any{"deleted": in.MessageID}), nil
	case toolModerateDelete:
		var in struct {
			MessageID int64 `json:"message_id"`
		}
		if json.Unmarshal([]byte(args), &in) != nil || in.MessageID <= 0 {
			return toolError("Invalid arguments: message_id is required."), nil
		}
		moderator, ok := b.moderator("delete")
		if !ok {
			return toolError("Moderation delete is disabled or Telegram did not confirm the required permission."), nil
		}
		if err := b.engine.Bots[b.job.Scope.BotID].Delete(ctx, b.job.Scope.ChatID, in.MessageID); err != nil {
			return "", err
		}
		_ = store.SyncMessage(ctx, b.job.Scope, in.MessageID, "", true)
		_ = moderator
		return toolResult(map[string]any{"deleted": in.MessageID}), nil
	case toolModerateBan, toolModerateUnban:
		var in struct {
			UserID int64 `json:"user_id"`
		}
		if json.Unmarshal([]byte(args), &in) != nil || in.UserID <= 0 {
			return toolError("Invalid arguments: user_id is required."), nil
		}
		moderator, ok := b.moderator("ban")
		if !ok {
			return toolError("Member moderation is disabled or Telegram did not confirm the required permission."), nil
		}
		var err error
		if name == toolModerateBan {
			err = moderator.BanMember(ctx, b.job.Scope.ChatID, in.UserID)
		} else {
			err = moderator.UnbanMember(ctx, b.job.Scope.ChatID, in.UserID)
		}
		if err != nil {
			return "", err
		}
		return toolResult(map[string]any{"user_id": in.UserID, "action": strings.TrimPrefix(name, "moderation_")}), nil
	case toolModerateMute:
		var in struct {
			UserID   int64  `json:"user_id"`
			Action   string `json:"action"`
			Duration int64  `json:"duration_seconds"`
		}
		if json.Unmarshal([]byte(args), &in) != nil || in.UserID <= 0 || (in.Action != "restrict" && in.Action != "unrestrict") || (in.Action == "restrict" && (in.Duration < 30 || in.Duration > 31622400)) || (in.Action == "unrestrict" && in.Duration != 0) {
			return toolError("Invalid restriction arguments."), nil
		}
		moderator, ok := b.moderator("restrict")
		if !ok {
			return toolError("Member restriction is disabled or Telegram did not confirm the required permission."), nil
		}
		var err error
		until := int64(0)
		if in.Action == "restrict" {
			until = b.now.Unix() + in.Duration
			err = moderator.RestrictMember(ctx, b.job.Scope.ChatID, in.UserID, until)
		} else {
			err = moderator.UnrestrictMember(ctx, b.job.Scope.ChatID, in.UserID)
		}
		if err != nil {
			return "", err
		}
		return toolResult(map[string]any{"user_id": in.UserID, "action": in.Action, "until": until}), nil
	case toolModeratePin:
		var in struct {
			MessageID int64  `json:"message_id"`
			Action    string `json:"action"`
		}
		if json.Unmarshal([]byte(args), &in) != nil || in.MessageID <= 0 || (in.Action != "pin" && in.Action != "unpin") {
			return toolError("Invalid pin arguments."), nil
		}
		moderator, ok := b.moderator("pin")
		if !ok {
			return toolError("Pinning is disabled or Telegram did not confirm the required permission."), nil
		}
		var err error
		if in.Action == "pin" {
			err = moderator.PinMessage(ctx, b.job.Scope.ChatID, in.MessageID)
		} else {
			err = moderator.UnpinMessage(ctx, b.job.Scope.ChatID, in.MessageID)
		}
		if err != nil {
			return "", err
		}
		return toolResult(map[string]any{"message_id": in.MessageID, "action": in.Action}), nil
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

func normalizeFact(s string) string {
	return strings.Join(strings.Fields(strings.ToLower(strings.TrimSpace(s))), " ")
}

func (b *builtinTools) moderator(permission string) (moderationSender, bool) {
	if !b.engine.Config.TelegramAdminTools || !b.moderation[permission] {
		return nil, false
	}
	moderator, ok := b.engine.Bots[b.job.Scope.BotID].(moderationSender)
	return moderator, ok
}
