package policy

import (
	"context"
	"os"
	"os/signal"
	"syscall"
)

// WatchSIGHUP reloads r's config from path whenever the process receives
// SIGHUP, until ctx is done. It's a thin wrapper around Resolver.Reload —
// see that method for the validate-then-swap guarantee that makes "add an
// override without a restart" and "a bad config never takes down a
// running node" the same property rather than two separate promises that
// could drift apart.
func WatchSIGHUP(ctx context.Context, path string, r *Resolver) {
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGHUP)

	go func() {
		defer signal.Stop(sig)
		for {
			select {
			case <-ctx.Done():
				return
			case <-sig:
				if err := r.Reload(path); err != nil {
					r.logger.Error("config_reload_failed", "path", path, "error", err)
					continue
				}
				r.logger.Info("config_reloaded", "path", path)
			}
		}
	}()
}
