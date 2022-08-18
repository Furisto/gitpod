// Copyright (c) 2022 Gitpod GmbH. All rights reserved.
// Licensed under the GNU Affero General Public License (AGPL).
// See License-AGPL.txt in the project root for license information.

package netlimit

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"sync"
	"time"

	"github.com/gitpod-io/gitpod/common-go/kubernetes"
	"github.com/gitpod-io/gitpod/common-go/log"
	"github.com/gitpod-io/gitpod/ws-daemon/pkg/dispatch"
	"github.com/google/nftables"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/vishvananda/netns"
	"golang.org/x/sys/unix"
	"golang.org/x/xerrors"
)

type ConnLimiter struct {
	mu             sync.RWMutex
	droppedBytes   *prometheus.GaugeVec
	droppedPackets *prometheus.GaugeVec
	config         Config
}

func NewConnLimiter(config Config, prom prometheus.Registerer) *ConnLimiter {
	s := &ConnLimiter{
		droppedBytes: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "netlimit_connections_dropped_bytes",
			Help: "Number of bytes dropped due to connection limiting",
		}, []string{"workspace"}),

		droppedPackets: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "netlimit_connections_dropped_packets",
			Help: "Number of packets dropped due to connection limiting",
		}, []string{"workspace"}),
	}

	s.config = config

	prom.MustRegister(
		s.droppedBytes,
		s.droppedPackets,
	)

	return s
}

func (c *ConnLimiter) WorkspaceAdded(ctx context.Context, ws *dispatch.Workspace) error {
	return c.limitWorkspace(ctx, ws)
}

func (c *ConnLimiter) WorkspaceUpdated(ctx context.Context, ws *dispatch.Workspace) error {
	return c.limitWorkspace(ctx, ws)
}

func (n *ConnLimiter) GetConnectionDropCounter(pid uint64) (*nftables.CounterObj, error) {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()

	netns, err := netns.GetFromPid(int(pid))
	if err != nil {
		return nil, xerrors.Errorf("could not get handle for network namespace: %w", err)
	}

	nftconn, err := nftables.New(nftables.WithNetNSFd(int(netns)))
	if err != nil {
		return nil, xerrors.Errorf("could not establish netlink connection for nft: %w", err)
	}

	gitpodTable := &nftables.Table{
		Name:   "gitpod",
		Family: nftables.TableFamilyIPv4,
	}

	counterObject, err := nftconn.GetObject(&nftables.CounterObj{
		Table: gitpodTable,
		Name:  "connection_drop_stats",
	})

	if err != nil {
		return nil, xerrors.Errorf("could not get connection drop counter: %w", err)
	}

	dropCounter, ok := counterObject.(*nftables.CounterObj)
	if !ok {
		return nil, xerrors.Errorf("could not cast counter object")
	}

	return dropCounter, nil
}

func (c *ConnLimiter) limitWorkspace(ctx context.Context, ws *dispatch.Workspace) error {
	_, hasAnnotation := ws.Pod.Annotations[kubernetes.WorkspaceNetConnLimitAnnotation]
	if !hasAnnotation {
		return nil
	}
	log.WithFields(ws.OWI()).Infof("will limit network connections")

	disp := dispatch.GetFromContext(ctx)
	if disp == nil {
		return fmt.Errorf("no dispatch available")
	}

	pid, err := disp.Runtime.ContainerPID(context.Background(), ws.ContainerID)
	if err != nil {
		return fmt.Errorf("could not get pid for container %s of workspace %s", ws.ContainerID, ws.WorkspaceID)
	}

	err = nsinsider(ws.InstanceID, int(pid), func(cmd *exec.Cmd) {
		cmd.Args = append(cmd.Args, "setup-connection-limit", "--limit", strconv.Itoa(int(c.config.ConnectionsPerMinute)),
			"--bucketsize", strconv.Itoa(int(c.config.BucketSize)))
	}, enterMountNS(false), enterNetNS(true))
	if err != nil {
		log.WithError(err).WithFields(ws.OWI()).Error("cannot enable connection limiting")
		return err
	}

	go func(*dispatch.Workspace) {
		ticker := time.NewTicker(30 * time.Second)
		defer ticker.Stop()

		for {
			select {
			case <-ticker.C:
				counter, err := c.GetConnectionDropCounter(pid)
				if err != nil {
					log.WithError(err).Errorf("could not get connection drop stats for %s", ws.WorkspaceID)
					continue
				}

				c.droppedBytes.WithLabelValues(ws.WorkspaceID).Set(float64(counter.Bytes))
				c.droppedPackets.WithLabelValues(ws.WorkspaceID).Set(float64(counter.Packets))

			case <-ctx.Done():
				return
			}
		}
	}(ws)

	return nil
}

type nsinsiderOpts struct {
	MountNS    bool
	PidNS      bool
	NetNS      bool
	MountNSPid int
}

func enterMountNS(enter bool) nsinsiderOpt {
	return func(o *nsinsiderOpts) {
		o.MountNS = enter
	}
}

func enterNetNS(enter bool) nsinsiderOpt {
	return func(o *nsinsiderOpts) {
		o.NetNS = enter
	}
}

type nsinsiderOpt func(*nsinsiderOpts)

func nsinsider(instanceID string, targetPid int, mod func(*exec.Cmd), opts ...nsinsiderOpt) error {
	cfg := nsinsiderOpts{
		MountNS: true,
	}
	for _, o := range opts {
		o(&cfg)
	}

	base, err := os.Executable()
	if err != nil {
		return err
	}

	type mnt struct {
		Env    string
		Source string
		Flags  int
	}
	var nss []mnt
	if cfg.MountNS {
		tpid := targetPid
		if cfg.MountNSPid != 0 {
			tpid = cfg.MountNSPid
		}
		nss = append(nss,
			mnt{"_LIBNSENTER_ROOTFD", fmt.Sprintf("/proc/%d/root", tpid), unix.O_PATH},
			mnt{"_LIBNSENTER_CWDFD", fmt.Sprintf("/proc/%d/cwd", tpid), unix.O_PATH},
			mnt{"_LIBNSENTER_MNTNSFD", fmt.Sprintf("/proc/%d/ns/mnt", tpid), os.O_RDONLY},
		)
	}
	if cfg.PidNS {
		nss = append(nss, mnt{"_LIBNSENTER_PIDNSFD", fmt.Sprintf("/proc/%d/ns/pid", targetPid), os.O_RDONLY})
	}
	if cfg.NetNS {
		nss = append(nss, mnt{"_LIBNSENTER_NETNSFD", fmt.Sprintf("/proc/%d/ns/net", targetPid), os.O_RDONLY})
	}

	stdioFdCount := 3
	cmd := exec.Command(filepath.Join(filepath.Dir(base), "nsinsider"))
	mod(cmd)
	cmd.Env = append(cmd.Env, "_LIBNSENTER_INIT=1", "GITPOD_INSTANCE_ID="+instanceID)
	for _, ns := range nss {
		f, err := os.OpenFile(ns.Source, ns.Flags, 0)
		if err != nil {
			return xerrors.Errorf("cannot open %s: %w", ns.Source, err)
		}
		defer f.Close()
		cmd.Env = append(cmd.Env, fmt.Sprintf("%s=%d", ns.Env, stdioFdCount+len(cmd.ExtraFiles)))
		cmd.ExtraFiles = append(cmd.ExtraFiles, f)
	}

	var cmdOut bytes.Buffer
	cmd.Stdout = &cmdOut
	cmd.Stderr = os.Stderr
	cmd.Stdin = os.Stdin
	err = cmd.Run()
	log.FromBuffer(&cmdOut, log.WithFields(log.OWI("", "", instanceID)))
	if err != nil {
		out, oErr := cmd.CombinedOutput()
		if oErr != nil {
			return xerrors.Errorf("run nsinsider (%v) \n%v\n output error: %v",
				cmd.Args,
				err,
				oErr,
			)
		}
		return xerrors.Errorf("run nsinsider (%v) failed: %q\n%v",
			cmd.Args,
			string(out),
			err,
		)
	}
	return nil
}
