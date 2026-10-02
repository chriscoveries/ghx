//go:build windows

package resource

import (
	"fmt"
	"github.com/brunoborges/ghx/src/internal/authenv"
	"github.com/brunoborges/ghx/src/internal/executor"
)

func RenderFilter(_ *Shape, _ []byte, _ authenv.Environment, _ func([]string, authenv.Environment) *executor.Result) (*executor.Result, error) {
	return nil, fmt.Errorf("native Unix rendering unavailable")
}
