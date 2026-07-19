package domain

import "errors"

var (
	ErrNotFound         = errors.New("not found")
	ErrAlreadyExists    = errors.New("already exists")
	ErrConflict         = errors.New("concurrent update conflict")
	ErrInvalidSpec      = errors.New("invalid spec")
	ErrProviderFailed   = errors.New("provider failed")
	ErrAllSourcesFailed = errors.New("all sources failed")
)
