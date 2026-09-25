package main

import (
	"github.com/alexmchughdev/swarmgate/internal/config"
	"github.com/alexmchughdev/swarmgate/internal/spec"
)

func bindMountAllowances(in []config.VolumeBindMount) []spec.BindMountAllowance {
	out := make([]spec.BindMountAllowance, len(in))
	for i, mount := range in {
		out[i] = spec.BindMountAllowance{Source: mount.Source, ReadOnly: mount.ReadOnly}
	}
	return out
}
