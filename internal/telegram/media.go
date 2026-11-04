package telegram

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"unicode/utf8"

	"github.com/maniack/tgaia/internal/agent"
)

// Fetch downloads only Bot API file paths. The credential-bearing URL never
// reaches model context, errors or persistence. Audio/video are disabled.
func (c *Client) Fetch(ctx context.Context, a agent.Attachment) ([]byte, string, error) {
	limit := int64(c.limits.Limit("document_bytes"))
	if a.Kind == "image" {
		limit = int64(c.limits.Limit("image_bytes"))
	} else if a.Kind != "document" {
		return nil, "", errors.New("audio/video recognition is disabled")
	}
	if a.FileID == "" || len(a.FileID) > 1024 || a.Size < 0 || a.Size > limit {
		return nil, "", errors.New("attachment exceeds download limit")
	}
	var file struct {
		Path string `json:"file_path"`
		Size int64  `json:"file_size"`
	}
	if err := c.call(ctx, "getFile", map[string]any{"file_id": a.FileID}, &file); err != nil {
		return nil, "", err
	}
	if file.Size > limit || file.Path == "" || strings.ContainsAny(file.Path, "\\?#:%") || strings.HasPrefix(file.Path, "/") {
		return nil, "", errors.New("invalid Telegram file metadata")
	}
	for _, part := range strings.Split(file.Path, "/") {
		if part == ".." || part == "." || part == "" {
			return nil, "", errors.New("invalid Telegram file path")
		}
	}
	req, err := http.NewRequestWithContext(ctx, "GET", "https://api.telegram.org/file/bot"+c.token.Reveal()+"/"+file.Path, nil)
	if err != nil {
		return nil, "", errors.New("invalid file request")
	}
	response, err := c.http.Do(req)
	if err != nil {
		return nil, "", errors.New("Telegram file download failed")
	}
	defer response.Body.Close()
	if response.StatusCode != 200 {
		return nil, "", errors.New("Telegram file unavailable")
	}
	data, err := io.ReadAll(io.LimitReader(response.Body, limit+1))
	if err != nil || int64(len(data)) > limit {
		return nil, "", errors.New("attachment download limit exceeded")
	}
	mime := http.DetectContentType(data)
	if a.Kind == "image" {
		if mime != "image/jpeg" && mime != "image/png" && mime != "image/webp" && mime != "image/gif" {
			return nil, "", errors.New("unsupported image format")
		}
	} else if !utf8.Valid(data) || strings.ContainsRune(string(data), 0) || (!strings.HasPrefix(mime, "text/") && mime != "application/json" && mime != "application/xml") {
		return nil, "", errors.New("only UTF-8 text documents are supported")
	}
	return data, mime, nil
}
