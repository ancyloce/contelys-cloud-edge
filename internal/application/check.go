package application

import (
	"fmt"
	"maps"
	"net/netip"
	"slices"
)

// check applies the rules of the instances file, the installation file and the route fragments.
// A broken rule of a fragment is a Refusal; a broken rule of the installation's own files is an
// error of its inputs.
func check(in Inputs) error {
	for _, slug := range slices.Sorted(maps.Keys(in.Instances)) {
		if !slug.valid() {
			return fmt.Errorf("the instances file: %q is not a slug", slug)
		}
		for i, instance := range in.Instances[slug] {
			switch {
			case instance.Address == "":
				return fmt.Errorf("the instances file: %s, instance %d: the address is empty", slug, i+1)
			case instance.Port < 1 || instance.Port > 65535:
				return fmt.Errorf("the instances file: %s, instance %d: %d is not a port", slug, i+1, instance.Port)
			case instance.Weight != 0 && instance.Weight != 1:
				return fmt.Errorf("the instances file: %s, instance %d: the weight is 1 to serve or 0 to drain, not %d", slug, i+1, instance.Weight)
			}
		}
	}
	if len(in.Instances[EdgeSlug]) == 0 {
		return fmt.Errorf("the instances file lists no instance of %s", EdgeSlug)
	}

	installation := in.Installation
	if installation.ApplicationHostname == "" {
		return fmt.Errorf("the installation file: the application hostname is empty")
	}
	if installation.AuthenticationHostname == "" {
		return fmt.Errorf("the installation file: the authentication hostname is empty")
	}
	for _, address := range slices.Concat(installation.LoadBalancerAddresses, installation.CollectorAddresses) {
		if _, err := netip.ParseAddr(address); err == nil {
			continue
		}
		if _, err := netip.ParsePrefix(address); err != nil {
			return fmt.Errorf("the installation file: %q is neither an address nor a prefix", address)
		}
	}

	for _, fragment := range in.Fragments {
		if !fragment.Slug.valid() {
			return &Refusal{Fragment: fragment.Slug, Rule: "the directory name is not a slug"}
		}
		if err := checkPaths(fragment, in.Contracts); err != nil {
			return err
		}
	}
	return checkConflicts(in.Fragments)
}
