package llm

import "encoding/json"

// MarshalJSON uses standard OpenAI-compatible multimodal content. Image URLs
// are bounded data URLs produced by the trusted Telegram download adapter.
func (m Message) MarshalJSON() ([]byte, error) {
	type plain Message
	if len(m.Images) == 0 {
		return json.Marshal(plain(m))
	}
	parts := []map[string]any{{"type": "text", "text": m.Content}}
	for _, image := range m.Images {
		parts = append(parts, map[string]any{"type": "image_url", "image_url": map[string]string{"url": image}})
	}
	return json.Marshal(struct {
		Role    string `json:"role"`
		Content any    `json:"content"`
	}{m.Role, parts})
}
