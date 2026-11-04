// Package access defines global user denial within an agent, independent of bot/chat.
package access

import "errors"

var ErrBlocked = errors.New("user is blocked for this agent")

var ErrDenied = errors.New("reader access does not permit tools or scheduled tasks")
