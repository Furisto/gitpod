// Copyright (c) 2022 Gitpod GmbH. All rights reserved.
// Licensed under the GNU Affero General Public License (AGPL).
// See License-AGPL.txt in the project root for license information.

package netlimit

import (
	"context"
	"runtime"
	"time"

	"github.com/gitpod-io/gitpod/common-go/log"
	"github.com/gitpod-io/gitpod/ws-daemon/pkg/dispatch"
	"github.com/google/nftables"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/vishvananda/netns"
	"golang.org/x/xerrors"
)

type NetworkStatExporter struct {
	droppedBytes   *prometheus.GaugeVec
	droppedPackets *prometheus.GaugeVec
}

func NewNetworkStatExporter(prom prometheus.Registerer) *NetworkStatExporter {
	s := &NetworkStatExporter{
		droppedBytes: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "netlimit_connections_dropped_bytes",
			Help: "Number of bytes dropped due to connection limiting",
		}, []string{"workspace"}),

		droppedPackets: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "netlimit_connections_dropped_packets",
			Help: "Number of packets dropped due to connection limiting",
		}, []string{"workspace"}),
	}

	prom.MustRegister(
		s.droppedBytes,
		s.droppedPackets,
	)

	return s
}

func (n *NetworkStatExporter) WorkspaceAdded(ctx context.Context, ws *dispatch.Workspace) error {
	disp := dispatch.GetFromContext(ctx)
	if disp == nil {
		return xerrors.Errorf("no dispatch available")
	}

	pid, err := disp.Runtime.ContainerPID(context.Background(), ws.ContainerID)
	if err != nil {
		return xerrors.Errorf("could not get pid for container %s of workspace %s", ws.ContainerID, ws.WorkspaceID)
	}

	go func(*dispatch.Workspace) {
		ticker := time.NewTicker(30 * time.Second)
		defer ticker.Stop()

		for {
			select {
			case <-ticker.C:
				counter, err := n.GetConnectionDropCounter(pid)
				if err != nil {
					log.WithError(err).Errorf("could not get connection drop stats for %s", ws.WorkspaceID)
					continue
				}

				n.droppedBytes.WithLabelValues(ws.WorkspaceID).Set(float64(counter.Bytes))
				n.droppedPackets.WithLabelValues(ws.WorkspaceID).Set(float64(counter.Packets))

			case <-ctx.Done():
				return
			}
		}
	}(ws)

	return nil
}

func (n *NetworkStatExporter) GetConnectionDropCounter(pid uint64) (*nftables.CounterObj, error) {
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
