package clstages

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/ledgerwatch/erigon-lib/log/v3"
)

type StageGraph[CONFIG any, ARGUMENTS any] struct {
	ArgsFunc func(ctx context.Context, cfg CONFIG) (args ARGUMENTS)
	Stages   map[string]Stage[CONFIG, ARGUMENTS]
}

type Stage[CONFIG any, ARGUMENTS any] struct {
	Description    string
	ActionFunc     func(ctx context.Context, logger log.Logger, cfg CONFIG, args ARGUMENTS) error
	TransitionFunc func(cfg CONFIG, args ARGUMENTS, err error) string
}

func (s *StageGraph[CONFIG, ARGUMENTS]) StartWithStage(ctx context.Context, startStage string, logger log.Logger, cfg CONFIG) error {
	stageName := startStage
	args := s.ArgsFunc(ctx, cfg)
	for {
		currentStage, ok := s.Stages[stageName]
		if !ok {
			return fmt.Errorf("attempted to transition to unknown stage: %s", stageName)
		}
		lg := logger.New("stage", stageName)
		errch := make(chan error)
		start := time.Now()
		go func() {
			// we run this is a goroutine so that the process can exit in the middle of a stage
			// since caplin is designed to always be able to recover regardless of db state, this should be safe
			select {
			case errch <- currentStage.ActionFunc(ctx, lg, cfg, args):
			case <-ctx.Done(): // we are not sure if actionFunc exits on ctx
				errch <- ctx.Err()
			}
		}()
		err := <-errch
		dur := time.Since(start)
		if err != nil {
			if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) || err.Error() == "timeout waiting for blocks" {
				lg.Debug("error executing clstage", "err", err)
			} else {
				lg.Warn("error executing clstage", "err", err)
			}
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
			args = s.ArgsFunc(ctx, cfg)
			nextStage := currentStage.TransitionFunc(cfg, args, err)
			logger.Debug("clstage finish", "stage", stageName, "in", dur, "next", nextStage)
			stageName = nextStage
		}
	}
}
