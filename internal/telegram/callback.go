package telegram

import (
	"encoding/base64"
	"errors"
	"fmt"
	"strings"

	"github.com/google/uuid"
)

const (
	callbackVersion  = "v1"
	callbackMaxBytes = 64
)

var errInvalidCallback = errors.New("invalid callback data")

type callbackCommand struct {
	Entity string
	Action string
	ID     uuid.UUID
}

func encodeCallback(entity, action string, id uuid.UUID) (string, error) {
	if !validCallbackPart(entity) || !validCallbackPart(action) {
		return "", errInvalidCallback
	}
	encodedID := base64.RawURLEncoding.EncodeToString(id[:])
	data := strings.Join([]string{callbackVersion, entity, action, encodedID}, ":")
	if len(data) > callbackMaxBytes {
		return "", fmt.Errorf("%w: callback exceeds %d bytes", errInvalidCallback, callbackMaxBytes)
	}
	return data, nil
}

func decodeCallback(data string) (callbackCommand, error) {
	data = strings.TrimPrefix(data, "\f")
	parts := strings.Split(data, ":")
	if len(parts) != 4 || parts[0] != callbackVersion || !validCallbackPart(parts[1]) || !validCallbackPart(parts[2]) || len(data) > callbackMaxBytes {
		return callbackCommand{}, errInvalidCallback
	}
	rawID, err := base64.RawURLEncoding.DecodeString(parts[3])
	if err != nil || len(rawID) != 16 {
		return callbackCommand{}, errInvalidCallback
	}
	id, err := uuid.FromBytes(rawID)
	if err != nil {
		return callbackCommand{}, errInvalidCallback
	}
	return callbackCommand{Entity: parts[1], Action: parts[2], ID: id}, nil
}

func validCallbackPart(value string) bool {
	if value == "" || len(value) > 24 {
		return false
	}
	for _, r := range value {
		if (r < 'a' || r > 'z') && (r < '0' || r > '9') && r != '_' {
			return false
		}
	}
	return true
}
