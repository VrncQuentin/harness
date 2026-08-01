package session

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/VrncQuentin/harness/internal/inference"
)

// encodeConversation marshals msgs into the JSON sidecar payload. The
// shape is the inference.Message slice verbatim so a downstream tool
// (or a test) can decode it without depending on this package.
func encodeConversation(msgs []inference.Message) ([]byte, error) {
	if msgs == nil {
		msgs = []inference.Message{}
	}
	body, err := json.MarshalIndent(msgs, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("session: marshal sidecar: %w", err)
	}
	return append(body, '\n'), nil
}

// decodeConversation parses the JSON sidecar back into the inference
// message slice. Empty bodies surface as an empty slice + nil error so
// the caller can hydrate a fresh transcript.
func decodeConversation(body []byte) ([]inference.Message, error) {
	body = trimUTF8BOM(body)
	if len(strings.TrimSpace(string(body))) == 0 {
		return nil, nil
	}
	var msgs []inference.Message
	if err := json.Unmarshal(body, &msgs); err != nil {
		return nil, fmt.Errorf("session: unmarshal sidecar: %w", err)
	}
	return msgs, nil
}

// trimUTF8BOM strips a leading UTF-8 BOM if present. Some Windows
// editors persist a BOM when a user opens the .json sidecar by hand;
// without this trim, json.Unmarshal would fail with an "invalid
// character" error and the resume picker would refuse to hydrate.
func trimUTF8BOM(body []byte) []byte {
	if len(body) >= 3 && body[0] == 0xEF && body[1] == 0xBB && body[2] == 0xBF {
		return body[3:]
	}
	return body
}
