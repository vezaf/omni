package join_test

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	clicmd "github.com/omni-network/omni/cli/cmd"
	"github.com/omni-network/omni/lib/errors"
	"github.com/omni-network/omni/lib/ethclient"
	"github.com/omni-network/omni/lib/log"
	"github.com/omni-network/omni/lib/netconf"
	"github.com/omni-network/omni/lib/tutil"

	rpchttp "github.com/cometbft/cometbft/rpc/client/http"

	"github.com/stretchr/testify/require"
	"golang.org/x/sync/errgroup"
)

var (
	logsFile    = flag.String("logs_file", "join_test.log", "File to write docker logs to")
	integration = flag.Bool("integration", false, "Run integration tests")
)

// TestJoinOmega starts a local node (using omni operator init-nodes)
// and waits for it to sync.
//
//nolint:paralleltest // Parallel tests not supported since we start docker containers.
func TestJoinOmega(t *testing.T) {
	if !*integration {
		t.Skip("skipping integration test")
	}

	const (
		timeout     = time.Hour * 6
		minDuration = time.Minute * 30
	)

	network := netconf.Omega
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	home := t.TempDir()
	logsPath, err := filepath.Abs(*logsFile)
	require.NoError(t, err)
	output, err := os.OpenFile(logsPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0644)
	require.NoError(t, err)

	cfg := clicmd.InitConfig{
		Network: network,
		Home:    home,
		Moniker: t.Name(),
		HaloTag: getGitCommit7(t),
	}

	require.NoError(t, ensureHaloImage(cfg.HaloTag))

	log.Info(ctx, "Exec: omni operator init-nodes")
	require.NoError(t, clicmd.InitNodes(log.WithNoopLogger(ctx), cfg))

	t0 := time.Now()

	var eg errgroup.Group
	eg.Go(func() error {
		defer cancel() // Stop other goroutine

		// Start the nodes.
		log.Info(ctx, "Exec: docker compose up", "logs_file", logsPath)
		cmd := exec.CommandContext(ctx, "docker", "compose", "up")
		cmd.Stderr = output
		cmd.Stdout = output
		cmd.Dir = home
		err := cmd.Run()
		if err == nil || ctx.Err() != nil {
			return nil // Docker compose didn't error
		}

		return errors.Wrap(err, "docker compose up early exit")
	})
	eg.Go(func() error {
		defer cancel() // Stop other goroutine

		// Monitor the progress until synced.
		cmtCl, err := rpchttp.New("localhost:26657", "/websocket")
		require.NoError(t, err)
		ethCl, err := ethclient.Dial("omni_evm", "http://localhost:8545")
		require.NoError(t, err)

		ticker := time.NewTicker(time.Second * 30)
		defer ticker.Stop()

		timeoutCtx, timeoutCancel := context.WithTimeout(ctx, timeout)
		defer timeoutCancel()

		for {
			select {
			case <-ctx.Done():
				return nil
			case <-timeoutCtx.Done():
				return errors.New("timed out waiting for sync", "duration", "duration", time.Since(t0).Truncate(time.Second))
			case <-ticker.C:
				haloStatus, err := retry(ctx, haloStatus)
				require.NoError(t, err)

				// CometBFT RPC errors while syncing, so best effort fetch
				var haloSynced bool
				haloHeight := int64(-1) // Indicates failed fetch
				if haloResult, err := cmtCl.Status(ctx); err == nil {
					haloSynced = !haloResult.SyncInfo.CatchingUp
					haloHeight = haloResult.SyncInfo.LatestBlockHeight
				}

				execStatus, err := retry(ctx, ethCl.SyncProgress)
				require.NoError(t, err)
				execSynced := execStatus.Done()
				execHeight := execStatus.CurrentBlock
				execTarget := execStatus.HighestBlock

				log.Info(ctx, "Status",
					"cstatus", haloStatus,
					"csynced", haloSynced,
					"cheight", haloHeight,
					"esynced", execSynced,
					"eheight", execHeight,
					"etarget", execTarget,
					"duration", time.Since(t0).Truncate(time.Second),
				)

				if haloStatus == "healthy" {
					if !execSynced {
						return errors.New("halo healthy but execution chain not synced", "height", execHeight)
					} else if !haloSynced {
						return errors.New("halo healthy but consensus chain not synced", "height", haloHeight)
					} else if time.Since(t0) < minDuration {
						return errors.New("halo healthy but not enough time has passed", "duration", time.Since(t0).Truncate(time.Second))
					}

					log.Info(ctx, "Synced 🎉", "duration", time.Since(t0).Truncate(time.Second))

					return nil
				}
			}
		}
	})

	if err := eg.Wait(); err != nil {
		printLogsTail(t, logsPath)
		tutil.RequireNoError(t, err)
	}
}

func printLogsTail(t *testing.T, path string) {
	t.Helper()
	bz, err := os.ReadFile(path)
	require.NoError(t, err)

	lines := strings.Split(string(bz), "\n")
	const n = 50
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}

	fmt.Println(strings.Join(lines, "\n"))
}

func retry[R any](ctx context.Context, fn func(context.Context) (R, error)) (R, error) {
	const retry = 10
	for i := 1; ; i++ {
		r, err := fn(ctx)
		if err == nil {
			return r, nil
		}

		if i >= retry {
			return r, err
		}

		log.Warn(ctx, "Failed attempt (will retry)", err, "attempt", i)
		time.Sleep(time.Second * time.Duration(i))
	}
}

func haloStatus(ctx context.Context) (string, error) {
	out, err := exec.CommandContext(ctx, "docker", "inspect", "--format={{json .State.Health.Status }}", "halo").CombinedOutput()
	if err != nil {
		return "", errors.Wrap(err, "docker inspect")
	}

	return strings.Trim(strings.TrimSpace(string(out)), "\""), nil
}

func ensureHaloImage(haloTag string) error {
	out, err := exec.Command("docker", "images", "-q", "omniops/halovisor:"+haloTag).CombinedOutput()
	if err != nil {
		return errors.Wrap(err, "docker images")
	} else if strings.TrimSpace(string(out)) == "" {
		return errors.New("omniops/halovisor:" + haloTag + " image not found locally (make build-docker?)")
	}

	return nil
}

func getGitCommit7(t *testing.T) string {
	t.Helper()

	out, err := exec.Command("git", "rev-parse", "HEAD").CombinedOutput()
	require.NoError(t, err)

	return string(out)[0:7]
}
