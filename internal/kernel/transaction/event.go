package transaction

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"time"

	"github.com/duriantaco/vouch/internal/kernel/model"
)

func CreationEvent(transaction model.AgentTransaction, actor model.Principal) (model.TransactionEvent, error) {
	payload, err := json.Marshal(TransactionCreatedPayload{Transaction: transaction})
	if err != nil {
		return model.TransactionEvent{}, fmt.Errorf("encode transaction creation: %w", err)
	}
	event := model.TransactionEvent{
		Version:       model.TransactionEventVersion,
		ID:            EventID(transaction.ID, 1),
		TransactionID: transaction.ID,
		Sequence:      1,
		Type:          EventTransactionCreated,
		Actor:         actor,
		OccurredAt:    transaction.CreatedAt.UTC(),
		Payload:       payload,
	}
	event.Digest, err = model.ComputeTransactionEventDigest(event)
	if err != nil {
		return model.TransactionEvent{}, fmt.Errorf("digest transaction creation: %w", err)
	}
	return event, nil
}

func NextEvent(
	projection Projection,
	eventType string,
	actor model.Principal,
	occurredAt time.Time,
	payload any,
) (model.TransactionEvent, error) {
	data, err := json.Marshal(payload)
	if err != nil {
		return model.TransactionEvent{}, fmt.Errorf("encode %s payload: %w", eventType, err)
	}
	occurredAt = occurredAt.UTC()
	if occurredAt.Before(projection.Transaction.UpdatedAt) {
		occurredAt = projection.Transaction.UpdatedAt
	}
	sequence := projection.Transaction.EventSequence + 1
	event := model.TransactionEvent{
		Version:        model.TransactionEventVersion,
		ID:             EventID(projection.Transaction.ID, sequence),
		TransactionID:  projection.Transaction.ID,
		Sequence:       sequence,
		Type:           eventType,
		Actor:          actor,
		OccurredAt:     occurredAt,
		Payload:        data,
		PreviousDigest: projection.LastEventDigest,
	}
	event.Digest, err = model.ComputeTransactionEventDigest(event)
	if err != nil {
		return model.TransactionEvent{}, fmt.Errorf("digest %s event: %w", eventType, err)
	}
	return event, nil
}

func EventID(transactionID string, sequence int64) string {
	sum := sha256.Sum256([]byte(transactionID))
	return fmt.Sprintf("tx-event:%s:%d", hex.EncodeToString(sum[:16]), sequence)
}

func ExecutionID(transactionID string, sequence int64) string {
	sum := sha256.Sum256([]byte(fmt.Sprintf("%s:%d", transactionID, sequence)))
	return "execution:" + hex.EncodeToString(sum[:16])
}

func VerificationID(transactionID, name string, sequence int64) string {
	sum := sha256.Sum256([]byte(fmt.Sprintf("%s:%s:%d", transactionID, name, sequence)))
	return "verification:" + hex.EncodeToString(sum[:16])
}
