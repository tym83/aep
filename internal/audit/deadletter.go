package audit

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"sync/atomic"
)

var DeadLetterCount atomic.Uint64
var InvalidWebhookCount atomic.Uint64

// The key is deterministic so retries cannot create an unbounded collection.
func deadLetter(ctx context.Context, store ObjectWriter, category string, payload any, errorType, reason string) error {
	if store == nil {
		return fmt.Errorf("dead-letter store required")
	}
	b, err := json.Marshal(map[string]any{"payload": payload, "error": map[string]string{"type": errorType, "reason": reason}})
	if err != nil {
		return err
	}
	if err = store.Put(ctx, "dead-letter/"+category+"/"+Digest(json.RawMessage(b))+".json", b, "application/json"); err != nil {
		return err
	}
	DeadLetterCount.Add(1)
	log.Printf("audit dead-letter category=%q error.type=%q error.reason=%q", category, errorType, reason)
	return nil
}
