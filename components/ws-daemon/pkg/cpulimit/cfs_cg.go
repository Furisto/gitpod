package cpulimit

import (
"github.com/containerd/cgroups"
)

type CFSControllerCgroup struct {
	cgroup cgroups.Cgroup
}

func Load(base string) (CFSControllerCgroup, error) {
	cgroup, err := cgroups.Load(cgroups.V1, cgroups.StaticPath(base))
	if err != nil {
		return CFSControllerCgroup{}, err
	}

	return CFSControllerCgroup{
		cgroup: cgroup,
	}, nil
}