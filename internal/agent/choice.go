package agent

import (
	"fmt"
	"strings"
)

// BuildChoiceKeyboard constructs an inline keyboard for a choice session.
func BuildChoiceKeyboard(sessionID int64, options []string, multi bool, selected []int) [][]InlineButton {
	sel := map[int]bool{}
	for _, i := range selected {
		sel[i] = true
	}
	rows := make([][]InlineButton, 0, len(options)+1)
	for i, opt := range options {
		label := opt
		if multi {
			if sel[i] {
				label = "☑ " + opt
			} else {
				label = "☐ " + opt
			}
		}
		rows = append(rows, []InlineButton{{
			Text:         label,
			CallbackData: fmt.Sprintf("c:%d:%d", sessionID, i),
		}})
	}
	if multi {
		rows = append(rows, []InlineButton{{
			Text:         "✓ Done",
			CallbackData: fmt.Sprintf("c:%d:done", sessionID),
		}})
	}
	return rows
}

// ChoiceAssistantText returns synthetic assistant text for a resumed choice context.
func ChoiceAssistantText(sess *ChoiceSession) string {
	opts := strings.Join(sess.Options, ", ")
	return fmt.Sprintf("I asked: %q\nOptions: %s", sess.Question, opts)
}

// ChoiceUserText returns synthetic user text for a resumed choice context.
func ChoiceUserText(sess *ChoiceSession) string {
	if len(sess.Selected) == 0 {
		return "Done (nothing selected)"
	}
	labels := make([]string, 0, len(sess.Selected))
	for _, idx := range sess.Selected {
		if idx >= 0 && idx < len(sess.Options) {
			labels = append(labels, sess.Options[idx])
		}
	}
	if len(labels) == 0 {
		return "Done (nothing selected)"
	}
	return "Selected: " + strings.Join(labels, ", ")
}
