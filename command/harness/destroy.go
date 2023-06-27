package harness

import (
	"context"
	"fmt"
	"time"

	"github.com/drone-runners/drone-runner-aws/command/config"
	"github.com/drone-runners/drone-runner-aws/command/debug"
	"github.com/drone-runners/drone-runner-aws/internal/drivers"
	"github.com/drone-runners/drone-runner-aws/internal/lehelper"
	"github.com/drone-runners/drone-runner-aws/internal/oshelp"
	ierrors "github.com/drone-runners/drone-runner-aws/internal/types"
	"github.com/drone-runners/drone-runner-aws/store"
	"github.com/harness/lite-engine/api"

	"github.com/cenkalti/backoff/v4"
	"github.com/pkg/errors"
	"github.com/sirupsen/logrus"
)

var (
	destroyTimeout = 10 * time.Minute
)

type VMCleanupRequest struct {
	PoolID         string `json:"pool_id"`
	StageRuntimeID string `json:"stage_runtime_id"`
	LogKey         string `json:"log_key,omitempty"`
}

func HandleDestroy(ctx context.Context, r *VMCleanupRequest, s store.StageOwnerStore, env *config.EnvConfig, poolManager *drivers.Manager) error {
	if r.StageRuntimeID == "" {
		return ierrors.NewBadRequestError("mandatory field 'stage_runtime_id' in the request body is empty")
	}
	// We do retries on destroy in case a destroy call comes while an initialize call is still happening.
	cnt := 0
	b := createBackoff(destroyTimeout)
	for {
		duration := b.NextBackOff()
		err := handleDestroy(ctx, r, s, env, poolManager, cnt)
		if err != nil {
			logrus.WithError(err).
				WithField("retry_count", cnt).
				WithField("stage_runtime_id", r.StageRuntimeID).
				Errorln("could not destroy VM")
			if duration == backoff.Stop {
				return err
			}
			time.Sleep(duration)
			cnt++
			continue
		}
		return nil
	}
}

func handleDestroy(ctx context.Context, r *VMCleanupRequest, s store.StageOwnerStore, env *config.EnvConfig, poolManager *drivers.Manager, retryCount int) error {
	entity, err := s.Find(ctx, r.StageRuntimeID)
	if err != nil || entity == nil {
		return errors.Wrap(err, fmt.Sprintf("failed to find stage owner entity for stage: %s", r.StageRuntimeID))
	}
	poolID := entity.PoolName

	logr := logrus.
		WithField("stage_runtime_id", r.StageRuntimeID).
		WithField("pool", poolID).
		WithField("api", "dlite:destroy").
		WithField("retry_count", retryCount)

	logr.Traceln("starting the destroy process")

	inst, err := poolManager.GetInstanceByStageID(ctx, poolID, r.StageRuntimeID)
	if err != nil {
		return fmt.Errorf("cannot get the instance by tag: %w", err)
	}
	if inst == nil {
		return fmt.Errorf("instance with stage runtime ID %s not found", r.StageRuntimeID)
	}

	logr = logr.
		WithField("instance_id", inst.ID).
		WithField("instance_name", inst.Name)

	logr.Traceln("invoking lite engine cleanup")
	client, err := lehelper.GetClient(inst, env.Runner.Name, inst.Port, env.LiteEngine.EnableMock, env.LiteEngine.MockStepTimeoutSecs)
	if err != nil {
		logr.WithError(err).Errorln("could not create lite engine client for invoking cleanup")
	} else {
		// Attempting to call lite engine destroy
		_, err = client.Destroy(context.Background(), &api.DestroyRequest{LogDrone: false, LogKey: r.LogKey, LiteEnginePath: oshelp.GetLiteEngineLogsPath(inst.OS)})
		if err != nil {
			// we can continue even if lite engine destroy does not happen successfully. This is because
			// the VM is anyways destroyed so the process will be killed
			logr.WithError(err).Errorln("could not invoke lite engine cleanup")
		}
	}

	logr.Traceln("successfully invoked lite engine cleanup, destroying instance")

	if err = poolManager.Destroy(ctx, poolID, inst.ID); err != nil {
		return fmt.Errorf("cannot destroy the instance: %w", err)
	}
	logr.Traceln("destroyed instance")

	envState().Delete(r.StageRuntimeID)
	debug.GetState().Delete(r.StageRuntimeID)

	if err = s.Delete(ctx, r.StageRuntimeID); err != nil {
		logr.WithError(err).Errorln("failed to delete stage owner entity")
	}

	return nil
}

func createBackoff(maxElapsedTime time.Duration) *backoff.ExponentialBackOff {
	exp := backoff.NewExponentialBackOff()
	exp.MaxElapsedTime = maxElapsedTime
	return exp
}
