package engine

import (
	"encoding/json"
	"time"

	"github.com/alekc/freeagent-sync/internal/store"
)

// extractAttachments finds every attachment in a payload.
//
// It walks the whole body rather than reading a known key, because an
// attachment can arrive nested: a bank transaction carries its explanations
// inline, and each of those can carry one. Anything with both a url and a
// content_src is an attachment, which is a shape no other object has.
func extractAttachments(family string, body []byte) ([]store.Attachment, error) {
	var parsed any
	if err := json.Unmarshal(body, &parsed); err != nil {
		return nil, err
	}

	parentURL := ""
	if top, ok := parsed.(map[string]any); ok {
		parentURL, _ = top["url"].(string)
	}

	var found []store.Attachment
	walkJSON(parsed, func(obj map[string]any) {
		att, ok := attachmentFrom(obj)
		if !ok {
			return
		}
		att.Family = family
		att.ParentURL = parentURL
		found = append(found, att)
	})
	return found, nil
}

// attachmentFrom recognises an attachment object and reads what the blob
// pass needs from it.
func attachmentFrom(obj map[string]any) (store.Attachment, bool) {
	url, hasURL := obj["url"].(string)
	src, hasSrc := obj["content_src"].(string)
	if !hasURL || !hasSrc || url == "" || src == "" {
		return store.Attachment{}, false
	}

	att := store.Attachment{URL: url, ContentSrc: src}
	att.FileName, _ = obj["file_name"].(string)
	att.ContentType, _ = obj["content_type"].(string)
	if size, ok := obj["file_size"].(float64); ok {
		att.FileSize = int64(size)
	}
	if expires, ok := obj["expires_at"].(string); ok && expires != "" {
		if when, err := time.Parse(time.RFC3339, expires); err == nil {
			att.ExpiresAt = when
		}
	}
	return att, true
}

// walkJSON visits every object in a decoded payload, including nested ones.
func walkJSON(node any, visit func(map[string]any)) {
	switch typed := node.(type) {
	case map[string]any:
		visit(typed)
		for _, child := range typed {
			walkJSON(child, visit)
		}
	case []any:
		for _, child := range typed {
			walkJSON(child, visit)
		}
	}
}
