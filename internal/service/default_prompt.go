package service

import (
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
)

// Read on each generation so an edited mounted file takes effect on the next job.
func readDefaultPrompt(path string, maxBytes int) (string, error) {
	// #nosec G304 -- path is derived from the operator-supplied local config path,
	// not from a Telegram message or HTTP request.
	f, err := os.Open(path)
	if err != nil {
		return "", errors.New("default system prompt file unavailable")
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, int64(maxBytes)+1))
	if err != nil || len(data) > maxBytes {
		return "", fmt.Errorf("default system prompt file unreadable or exceeds %d bytes", maxBytes)
	}
	prompt := strings.TrimSpace(string(data))
	if prompt == "" {
		return "", errors.New("default system prompt file is empty")
	}
	return prompt, nil
}
