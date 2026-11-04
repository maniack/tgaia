package service

import (
	"errors"
	"io"
	"os"
	"strings"
)

// Read on each generation so an edited mounted file takes effect on the next job.
func readDefaultPrompt(path string) (string, error) {
	// #nosec G304 -- path is derived from the operator-supplied local config path,
	// not from a Telegram message or HTTP request.
	f, err := os.Open(path)
	if err != nil {
		return "", errors.New("default system prompt file unavailable")
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, 16001))
	if err != nil || len(data) > 16000 {
		return "", errors.New("default system prompt file unreadable or exceeds 16000 bytes")
	}
	prompt := strings.TrimSpace(string(data))
	if prompt == "" {
		return "", errors.New("default system prompt file is empty")
	}
	return prompt, nil
}
