package eventlog

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"time"

	"github.com/duriantaco/gatemole/internal/kernel/model"
	"github.com/duriantaco/gatemole/internal/kernel/reducer"
)

func Next(
	projection reducer.Projection,
	eventType string,
	actor model.Principal,
	occurredAt time.Time,
	payload any,
) (model.RunEvent, error) {
	data, err := json.Marshal(payload)
	if err != nil {
		return model.RunEvent{}, fmt.Errorf("encode %s payload: %w", eventType, err)
	}
	occurredAt = occurredAt.UTC()
	if occurredAt.Before(projection.Run.UpdatedAt) {
		occurredAt = projection.Run.UpdatedAt
	}
	sequence := projection.Run.EventSequence + 1
	event := model.RunEvent{
		Version:        model.RunEventVersion,
		ID:             ID(projection.Run.ID, sequence),
		RunID:          projection.Run.ID,
		Sequence:       sequence,
		Type:           eventType,
		Actor:          actor,
		OccurredAt:     occurredAt,
		Payload:        data,
		PreviousDigest: projection.LastEventDigest,
	}
	event.Digest, err = model.ComputeEventDigest(event)
	if err != nil {
		return model.RunEvent{}, fmt.Errorf("digest %s event: %w", eventType, err)
	}
	return event, nil
}

func ID(runID string, sequence int64) string {
	sum := sha256.Sum256([]byte(runID))
	return fmt.Sprintf("event:%s:%d", hex.EncodeToString(sum[:16]), sequence)
}
