// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"errors"
	"os"

	"github.com/Sharvil2704/rhobound-mcp/mcp/internal/lambdart"
)

// The fit worker (DEPLOY_AWS.md 2.3): a Lambda function invoked asynchronously, with no retries, by the MCP
// function for fit_start and by the scheduler. Each event is one job; it runs in a fresh folder under /tmp that is
// removed at the end, and any failure (an error, a panic, the job's deadline_at) is written to the state store as
// failed before the invocation ends (lambdart.Job).
func init() { register("fit-worker", runFitWorker) }

// fitWorkerJob builds the fit job from the server's configuration: Run fetches the window (fetch.Fetcher), runs
// `rhobound check --cut` and `rhobound fit --manifest DIR/manifest.json --name ENV`, so that the fit records the
// environment's name and no folder (or `rhobound diff-fit` with the arms' names for an A/B job), and records the
// result; Fail writes the job as failed with its refusal. The server's wiring sets it; while it is nil, the worker
// reports an init error.
var fitWorkerJob func(ctx context.Context, args []string) (lambdart.Job, error)

func runFitWorker(ctx context.Context, args []string) error {
	api := os.Getenv("AWS_LAMBDA_RUNTIME_API")
	if api == "" {
		return errors.New("fit-worker mode runs only inside AWS Lambda (AWS_LAMBDA_RUNTIME_API is not set)")
	}
	if fitWorkerJob == nil {
		return lambdaInitFailed(api, errors.New("fit-worker mode is not wired into this build"))
	}
	job, err := fitWorkerJob(ctx, args)
	if err != nil {
		return lambdaInitFailed(api, err)
	}
	return lambdart.Serve(ctx, api, job.Handle)
}
