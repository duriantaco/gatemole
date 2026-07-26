package model

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
)

// DecodeStrict rejects unknown fields and trailing JSON. Driver-defined raw
// payloads remain opaque until their operation-specific schema is selected.
func DecodeStrict[T any](data []byte) (T, error) {
	var value T
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&value); err != nil {
		code := ErrorSchemaInvalid
		if strings.Contains(err.Error(), "unknown field") {
			code = ErrorUnknownField
		}
		return value, newError(code, "decode", "", "", "invalid JSON resource", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			err = fmt.Errorf("unexpected trailing JSON value")
		}
		return value, newError(ErrorSchemaInvalid, "decode", "", "", "trailing JSON is not allowed", err)
	}
	return value, nil
}

func DecodePayloadStrict[T any](event RunEvent) (T, error) {
	value, err := DecodeStrict[T](event.Payload)
	if err == nil {
		return value, nil
	}
	var zero T
	var kernelErr *KernelError
	if errors.As(err, &kernelErr) {
		kernelErr.Operation = "decode_event_payload"
		kernelErr.Resource = event.ID
	}
	return zero, err
}
