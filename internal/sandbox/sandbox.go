package sandbox

import (
	"context"
	"time"
)

type Command struct {
	Executable string
	Arguments  []string
	Directory  string
	Env        map[string]string
	Timeout    time.Duration
}

type Result struct {
	ExitCode int
	Stdout   []byte
	Stderr   []byte
	Duration time.Duration
}

type Sandbox interface {
	Run(context.Context, Command) (Result, error)
}
