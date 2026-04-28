// Package controller implements the bulb Service reconciler.
// It watches Services with type=LoadBalancer (loadBalancerClass empty
// or "bulb"), creates per-Service proxy DaemonSets, and emits LBPort
// and DNSRecord CRs.
package controller

import "errors"

func Run(args []string) error {
	return errors.New("not yet implemented (Phase 1)")
}
