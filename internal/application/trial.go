package application

import (
	"errors"
	"fmt"
	"reflect"
	"slices"
	"strings"
)

// Trial loads candidate files into a throwaway instance of Edge's image and reports what that
// instance loaded (section 03). APISIX drops an item that fails its own validation, and a whole
// route for an unknown or disabled plugin, and loads the rest, so only a trial load shows that a
// rendered file is complete.
type Trial interface {
	Load(Candidate) (Loaded, error)
}

// Candidate is what the trial load starts its instance with: Edge's image, and the files that an
// instance of the installation mounts.
type Candidate struct {
	Image      string
	File       ApisixFile
	Config     ConfigFile
	TrustStore string
}

// Loaded holds the ids of the routes and upstreams that the trial instance's control listener
// reports.
type Loaded struct {
	Routes    []string
	Upstreams []string
}

// trialLoad loads each distinct apisix.yaml of the output, with the installation's config.yaml, and
// compares what the instance loaded with what was rendered.
func trialLoad(trial Trial, image string, in Inputs, out Output) error {
	var loaded []ApisixFile
	for _, instance := range out.Instances {
		if slices.ContainsFunc(loaded, func(file ApisixFile) bool { return reflect.DeepEqual(file, instance.File) }) {
			continue
		}
		report, err := trial.Load(Candidate{Image: image, File: instance.File, Config: out.Config, TrustStore: in.Secrets.TrustStore})
		if err != nil {
			return err
		}
		if err := compareLoaded(instance.File, report, in.Fragments); err != nil {
			return err
		}
		loaded = append(loaded, instance.File)
	}
	return nil
}

// compareLoaded requires that the instance loaded exactly the rendered routes and upstreams. A route
// of a fragment that it did not load is a refusal of that fragment. Any other difference is an
// error, because no fragment caused it: an upstream's instances come from the instances file.
func compareLoaded(file ApisixFile, loaded Loaded, fragments []Fragment) error {
	for _, route := range file.Routes {
		if slices.Contains(loaded.Routes, route.ID) {
			continue
		}
		rule := fmt.Sprintf("the trial load: APISIX did not load route %s (%s), so it would never reach traffic", route.ID, route.Name)
		for _, fragment := range fragments {
			if strings.HasPrefix(route.ID, externalListener+"."+string(fragment.Slug)+".") {
				return &Refusal{Fragment: fragment.Slug, Rule: rule}
			}
		}
		return errors.New(rule)
	}
	for _, upstream := range file.Upstreams {
		if !slices.Contains(loaded.Upstreams, upstream.ID) {
			return fmt.Errorf("the trial load: APISIX did not load upstream %s, whose instances come from the instances file", upstream.ID)
		}
	}
	if extra := without(loaded.Routes, ids(file.Routes, func(r Route) string { return r.ID })); len(extra) > 0 {
		return fmt.Errorf("the trial load: APISIX loaded routes %v, which were not rendered", extra)
	}
	if extra := without(loaded.Upstreams, ids(file.Upstreams, func(u Upstream) string { return u.ID })); len(extra) > 0 {
		return fmt.Errorf("the trial load: APISIX loaded upstreams %v, which were not rendered", extra)
	}
	return nil
}

// without returns the items of list that rendered does not hold.
func without(list, rendered []string) []string {
	var extra []string
	for _, item := range list {
		if !slices.Contains(rendered, item) {
			extra = append(extra, item)
		}
	}
	return extra
}
