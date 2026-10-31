package agent

import "errors"

var (
	ErrRequestCancelled          = errors.New("request canceled by user")
	ErrSessionBusy               = errors.New("session is currently processing another request")
	ErrSessionMutationInProgress = errors.New("session mutation is already in progress")
	ErrNoActiveRunToSteer        = errors.New("session has no active run to steer")
	ErrEmptyPrompt               = errors.New("prompt is empty")
	ErrSessionMissing            = errors.New("session id is missing")
)
