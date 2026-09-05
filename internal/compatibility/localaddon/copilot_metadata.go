package localaddon

import (
	"time"

	"github.com/example-git/crux/internal/proto"
)

func copilotSessionMetadata(sessionID string, sess *proto.Session) map[string]any {
	start := time.Unix(sess.CreatedAt, 0).UTC().Format(time.RFC3339Nano)
	modified := time.Unix(sess.UpdatedAt, 0).UTC().Format(time.RFC3339Nano)
	return map[string]any{
		"sessionId":    sessionID,
		"startTime":    start,
		"modifiedTime": modified,
		"summary":      sess.Title,
		"isRemote":     false,
	}
}
