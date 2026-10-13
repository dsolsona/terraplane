package services

import "errors"

var (
	ErrEmptyAgentID     = errors.New("agent_id is required")
	ErrJobWrongAgent    = errors.New("job is not owned by agent")
	ErrJobInvalidStatus = errors.New("job is not in a valid status for this operation")
	ErrJobNotFound      = errors.New("job not found")
)
