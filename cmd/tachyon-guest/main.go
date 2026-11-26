// tachyon-guest is the agent inside each job VM. It waits on vsock for the
// job payload from hostd, mounts the job's disks, writes its config files,
// and runs the GitHub runner (or, for a prebuild build, the warm steps).
// It then kills what the job left running and unmounts (or freezes) the
// job's filesystems, because hostd snapshots the disks as soon as it has the
// result; only then does it report the result over the same connection and
// power the VM off.
//
// It starts at boot, before the base snapshot is taken, so every VM restored
// from the snapshot is already waiting in accept().
package main

import (
	"context"
	"flag"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/decover-tech/tachyon/internal/guest"
)

func main() {
	port := flag.Uint("port", guest.Port, "vsock port")
	noPowerOff := flag.Bool("no-poweroff", false, "exit instead of powering off (debugging)")
	flag.Parse()

	log := slog.New(slog.NewJSONHandler(os.Stderr, nil))
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM)
	defer stop()

	sys := guest.NewSystem()
	code := serve(ctx, log, sys, uint32(*port))
	if *noPowerOff {
		os.Exit(code)
	}
	if err := sys.PowerOff(); err != nil {
		log.Error("power off", "err", err)
		os.Exit(1)
	}
}

func serve(ctx context.Context, log *slog.Logger, sys guest.System, port uint32) int {
	conn, err := guest.AcceptVsock(port, guest.ReadyPort)
	if err != nil {
		log.Error("listen", "err", err)
		return 1
	}
	defer conn.Close()
	a := &guest.Agent{
		Sys:    sys,
		Layout: guest.DefaultLayout(),
		Log:    log,
		Output: os.Stdout,
	}
	if err := a.Serve(ctx, conn); err != nil {
		log.Error("serve", "err", err)
		return 1
	}
	return 0
}
