package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/maniack/tgaia/internal/chat"
	"github.com/maniack/tgaia/internal/llm"
	"github.com/maniack/tgaia/internal/mcpclient"
	"github.com/maniack/tgaia/internal/queue"
)

type ToolProvider interface {
	Open(context.Context, chat.Scope) (mcpclient.Session, error)
}
type toolModel interface {
	CompleteTools(context.Context, []llm.Message, []llm.Tool) (llm.Message, error)
}

// Tool results are part of the tool contract: they report what happened to a
// call, in the same shape as any MCP result.
const uncertainTool = `{"is_error":true,"outcome":"unknown","content":"The tool may have executed; its outcome is unknown and it must not be repeated."}`

const repeatedTool = `{"is_error":true,"content":"Identical repeated call blocked; the earlier result above still applies."}`

const deniedTool = `{"is_error":true,"content":"Tool not offered or arguments invalid."}`

// turn describes one agent run: the model is given messages and tools and
// works until it calls one of the terminal tools. Every behaviour of the bot
// (answering, staying silent, writing memory, describing an image) is such a
// run; they differ only in the data and the tools offered.
type turn struct {
	job      queue.Job
	messages []llm.Message
	actions  []llm.Tool // Tools that do something and return a result.
	terminal []llm.Tool // Tools that end the run; at least one.
	builtin  *builtinTools
	session  mcpclient.Session
	rounds   int
	calls    int
	// checkpoint stores each model turn and tool call durably, so a retried
	// job replays results instead of repeating side effects. Runs without
	// actions have nothing to protect and skip it.
	checkpoint bool
	compaction bool // Reserve the compaction quota instead of the reply quota.
	budget     int  // Output tokens per model call; 0 keeps the client default.
	thinking   *bool
}

// turnResult is the terminal call the model chose. Prose is set instead when
// the model answered in plain text and would not call a terminal tool.
type turnResult struct {
	Name          string
	Arguments     string
	Prose         string
	Confirmations []string
}

// errUnfinished reports a run that ended without a terminal call or text.
var errUnfinished = queue.Fail("llm_invalid", false, errors.New("model ended the turn without an answer"))

// run executes the agent loop. Mechanics only, no instructions:
//   - every model turn must be a tool call (tool_choice=required);
//   - when the round or call budget is spent, after an unknown outcome, or
//     after two rounds in which nothing executed, only terminal tools are
//     offered, so the model can do nothing but finish;
//   - a turn cut off by the token limit is retried without thinking;
//   - plain text instead of a call is kept as a draft and the model is asked
//     once more with terminal tools only; if it still does not call one, the
//     draft is the result.
func (e *Engine) run(ctx context.Context, t turn) (turnResult, error) {
	var result turnResult
	model, ok := e.Model.(toolModel)
	if !ok {
		// A model without function calling can only answer in text.
		if err := e.reserveModel(ctx, t.job, t.compaction); err != nil {
			return result, err
		}
		text, err := e.Model.Complete(e.callContext(ctx, t, false), t.messages)
		if err != nil {
			return result, err
		}
		if strings.TrimSpace(text) == "" {
			return result, errUnfinished
		}
		result.Prose = strings.TrimSpace(text)
		return result, nil
	}
	terminalNames := map[string]bool{}
	for _, tool := range t.terminal {
		terminalNames[tool.Function.Name] = true
	}
	input := append([]llm.Message(nil), t.messages...)
	calls, idle := 0, 0
	uncertain, forced, noThinking := false, false, false
	draft := ""
	last := "" // Previous call signature; models sometimes loop on one call.
	for round := 0; ; round++ {
		if round >= t.rounds+3 {
			// Every path below converges on terminal-only turns; this only
			// bounds a provider that ignores tool_choice entirely.
			if draft != "" {
				result.Prose = draft
				return result, nil
			}
			return result, errUnfinished
		}
		final := forced || len(t.actions) == 0 || round >= t.rounds-1 || calls >= t.calls || uncertain
		offered := t.terminal
		if !final {
			offered = append(append([]llm.Tool(nil), t.actions...), t.terminal...)
		}
		key := fmt.Sprintf("turn:%d", round)
		phase := fmt.Sprintf("tool_turn:%d", round)
		var message llm.Message
		saved := ""
		if t.checkpoint {
			if err := e.DB.Agent().CheckActions(ctx, t.job.Scope, t.job.ActorUserID); err != nil && len(t.actions) > 0 {
				return result, err
			}
			step, err := e.DB.Agent().ToolStep(ctx, t.job.ID, key)
			if err != nil {
				return result, err
			}
			saved = step.Result
		}
		if saved != "" {
			if json.Unmarshal([]byte(saved), &message) != nil {
				return result, errors.New("invalid saved tool turn")
			}
		} else {
			if err := e.reserveModel(ctx, t.job, t.compaction); err != nil {
				return result, queue.WithPhase(err, phase)
			}
			var err error
			message, err = model.CompleteTools(e.callContext(ctx, t, noThinking), input, offered)
			if truncated(err) && !noThinking {
				// Reasoning ate the output budget; a call needs far less.
				noThinking = true
				if err = e.reserveModel(ctx, t.job, t.compaction); err != nil {
					return result, queue.WithPhase(err, phase)
				}
				message, err = model.CompleteTools(e.callContext(ctx, t, true), input, offered)
			}
			if truncated(err) && !final {
				forced = true
				continue
			}
			if class, _ := queue.Classify(err); draft != "" && (class == "llm_invalid" || truncated(err)) {
				// The retry after a prose answer came back empty or broken:
				// the model considers its text the answer, and so do we.
				result.Prose = draft
				return result, nil
			}
			if err != nil {
				return result, queue.WithPhase(err, phase)
			}
			if t.checkpoint {
				data, err := json.Marshal(message)
				if err != nil {
					return result, errors.New("invalid tool planning response")
				}
				if err = e.DB.Agent().SaveToolTurn(ctx, t.job, key, string(data)); err != nil {
					return result, err
				}
			}
		}
		if len(message.ToolCalls) == 0 {
			text := strings.TrimSpace(message.Content)
			if draft == "" && text != "" {
				draft = text
				// Ask once more with nothing but the terminal tools.
				input = append(input, llm.Message{Role: "assistant", Content: text})
				forced = true
				continue
			}
			if draft == "" {
				draft = text
			}
			if draft == "" {
				return result, errUnfinished
			}
			result.Prose = draft
			return result, nil
		}
		for _, call := range message.ToolCalls {
			if terminalNames[call.Function.Name] && slices.ContainsFunc(offered, func(tool llm.Tool) bool { return tool.Function.Name == call.Function.Name }) {
				result.Name, result.Arguments = call.Function.Name, call.Function.Arguments
				return result, nil
			}
		}
		if len(message.ToolCalls) > max(t.calls-calls, 1) {
			message.ToolCalls = message.ToolCalls[:max(t.calls-calls, 1)]
		}
		message.Content = clipNote(message.Content)
		input = append(input, message)
		executed := false
		for i, call := range message.ToolCalls {
			calls++
			output, ran, err := e.execute(ctx, t, fmt.Sprintf("call:%d:%d", round, i), call, final, uncertain, last)
			if err != nil {
				return result, err
			}
			executed = executed || ran
			last = call.Function.Name + ":" + call.Function.Arguments
			if output == uncertainTool {
				uncertain = true
			}
			if c := confirmationOf(output); c != "" && t.builtin != nil && t.builtin.handles(call.Function.Name) {
				result.Confirmations = append(result.Confirmations, c)
			}
			// Preserve call/result pairing while keeping room for the answer.
			// The full bounded result remains in the durable checkpoint.
			size := 0
			for _, m := range input {
				data, _ := json.Marshal(m)
				size += len(data)
			}
			if size+len(output) > e.Config.Limit("tool_context_bytes") {
				output = `{"is_error":true,"content":"Tool completed; result omitted because the conversation budget is exhausted."}`
			}
			input = append(input, llm.Message{Role: "tool", ToolCallID: call.ID, Content: output})
		}
		if executed {
			idle = 0
		} else if idle++; idle >= 2 {
			forced = true
		}
	}
}

// execute runs one action call with its durable intent record. ran reports
// that the call actually reached a tool (or replayed a recorded result).
func (e *Engine) execute(ctx context.Context, t turn, key string, call llm.ToolCall, final, uncertain bool, last string) (output string, ran bool, err error) {
	name := call.Function.Name
	local := t.builtin != nil && t.builtin.handles(name)
	if final || !slices.ContainsFunc(t.actions, func(tool llm.Tool) bool { return tool.Function.Name == name }) {
		return deniedTool, false, nil
	}
	if err := e.DB.Agent().CheckActions(ctx, t.job.Scope, t.job.ActorUserID); err != nil {
		return "", false, err
	}
	encoded, _ := json.Marshal(call)
	previous, err := e.DB.Agent().ToolStep(ctx, t.job.ID, key)
	if err != nil {
		return "", false, err
	}
	switch {
	case previous.State != "" && previous.Request != string(encoded):
		return "", false, errors.New("tool intent mismatch")
	case previous.State == "completed" || previous.State == "uncertain":
		return previous.Result, true, nil
	case previous.State == "started" && (!local || t.builtin.mutates(name)):
		if err := e.DB.Agent().FinishTool(ctx, t.job, key, "uncertain", uncertainTool); err != nil {
			return "", false, err
		}
		return uncertainTool, true, nil
	case name+":"+call.Function.Arguments == last:
		return repeatedTool, false, nil
	case !local && (t.session == nil || t.session.Validate(name, call.Function.Arguments) != nil):
		return deniedTool, false, nil
	case uncertain:
		return `{"is_error":true,"content":"Not executed: an earlier call has an unknown outcome."}`, false, nil
	}
	if previous.State != "started" {
		fresh, err := e.DB.Agent().StartTool(ctx, t.job, key, string(encoded))
		if err != nil {
			return "", false, err
		}
		if !fresh {
			return "", false, errors.New("tool intent already exists")
		}
	}
	if local {
		output, err = t.builtin.call(ctx, key, name, call.Function.Arguments)
	} else {
		output, err = t.session.Call(ctx, name, call.Function.Arguments)
	}
	state := "completed"
	if err != nil {
		if local && !t.builtin.mutates(name) {
			// A failed read has no side effect; report it instead of fencing.
			output = toolError("The tool failed; its data is unavailable.")
		} else {
			output, state = uncertainTool, "uncertain"
		}
	}
	// A cancelled attempt leaves a started intent. Its next owner marks it
	// uncertain rather than repeating the external call.
	if err := e.DB.Agent().FinishTool(context.WithoutCancel(ctx), t.job, key, state, output); err != nil {
		return "", false, err
	}
	return output, true, nil
}

func (e *Engine) callContext(ctx context.Context, t turn, noThinking bool) context.Context {
	if t.budget > 0 {
		ctx = llm.WithOutputBudget(ctx, t.budget)
	}
	if noThinking {
		ctx = llm.WithThinking(ctx, false)
	} else if t.thinking != nil {
		ctx = llm.WithThinking(ctx, *t.thinking)
	}
	return llm.WithToolChoice(ctx, "required")
}

// noteLimit bounds free text the model writes next to an action call. That
// text is working notes; the answer is the argument of the terminal call.
const noteLimit = 300

func clipNote(text string) string {
	text = strings.TrimSpace(text)
	if len(text) <= noteLimit {
		return text
	}
	runes := []rune(text)
	if len(runes) > noteLimit {
		runes = runes[:noteLimit]
	}
	return string(runes) + "…"
}

func truncated(err error) bool {
	class, _ := queue.Classify(err)
	return class == llm.ClassTruncated
}

func failedResult(output string) bool {
	var r struct {
		IsError bool `json:"is_error"`
	}
	return json.Unmarshal([]byte(output), &r) == nil && r.IsError
}

func confirmationOf(output string) string {
	var r struct {
		IsError bool `json:"is_error"`
		Content struct {
			Created *taskView `json:"created"`
			Updated *taskView `json:"updated"`
		} `json:"content"`
	}
	if json.Unmarshal([]byte(output), &r) != nil || r.IsError {
		return ""
	}
	if r.Content.Created != nil {
		return r.Content.Created.Confirmation
	}
	if r.Content.Updated != nil {
		return r.Content.Updated.Confirmation
	}
	return ""
}
