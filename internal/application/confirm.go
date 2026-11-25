package application

import (
	"fmt"
	"time"
)

// ConfirmRequest names, as the command line gives them, the Edge instance whose applied
// configuration edge confirm reads, and the rendered output to compare it with.
type ConfirmRequest struct {
	// Out is the output directory of edge render, and Instance the address of the instance in the
	// instances file, which names its directory there.
	Out      string
	Instance string
	Revision string
	// Internal and Control are where the confirmation reaches the instance's internal listener and
	// its control listener, as host:port.
	Internal string
	Control  string
	// Secrets is the directory of the secret files, whose trust store verifies the internal listener.
	Secrets string
}

// Distributed is what the apisix.yaml distributed to an instance holds: the answer of its
// fixed-response route and the ids of its routes and upstreams.
type Distributed struct {
	Answer    Answer
	Routes    []string
	Upstreams []string
}

// Distribution reads the apisix.yaml that was distributed to the instance of a request.
type Distribution interface {
	ReadDistributed(ConfirmRequest) (Distributed, error)
}

// RunningInstance reads what one running Edge instance applied: the answer of its fixed-response
// route on the internal listener, and the ids that its control listener reports.
type RunningInstance interface {
	Answer() (Answer, error)
	Loaded() (Loaded, error)
}

// RunningInstances opens the running instance that a request names.
type RunningInstances interface {
	Open(ConfirmRequest) (RunningInstance, error)
}

// Timing is how the confirmation reads an instance: every answer over one polling interval of APISIX
// must carry the distributed revision and digest, the answer is read again after Every, and the
// instance counts as not applied once Timeout passes without such an interval.
type Timing struct {
	Interval time.Duration
	Every    time.Duration
	Timeout  time.Duration
}

// NotApplied is an instance that has not applied the distributed apisix.yaml: it kept answering
// another revision or digest, or it did not load every distributed item. The command exits with 1.
type NotApplied struct {
	Instance string
	Reason   string
}

func (n *NotApplied) Error() string {
	return fmt.Sprintf("the instance at %s has not applied its distributed apisix.yaml: %s", n.Instance, n.Reason)
}

// Confirmer is the use case behind the confirm command, the last step of "Rendering and reload" in
// section 03, which both profiles run after a rendering.
type Confirmer struct {
	Distribution Distribution
	Running      RunningInstances
	Timing       Timing
}

// Confirm returns the answer that the instance applied. Each worker process of an instance applies a
// changed file on its own, so every answer over one polling interval must carry the revision and the
// digest of the distributed file. The answers prove that the file was read, and the ids that the
// control listener reports prove that no item was dropped.
func (c Confirmer) Confirm(req ConfirmRequest) (Answer, error) {
	distributed, err := c.Distribution.ReadDistributed(req)
	if err != nil {
		return Answer{}, err
	}
	if distributed.Answer.Revision != req.Revision {
		return Answer{}, fmt.Errorf("the distributed apisix.yaml of %s carries the revision %s, not %s", req.Instance, distributed.Answer.Revision, req.Revision)
	}
	instance, err := c.Running.Open(req)
	if err != nil {
		return Answer{}, err
	}
	if err := c.awaitAnswer(req.Instance, instance, distributed.Answer); err != nil {
		return Answer{}, err
	}
	loaded, err := instance.Loaded()
	if err != nil {
		return Answer{}, fmt.Errorf("the control listener of the instance at %s: %w", req.Instance, err)
	}
	for _, kind := range []struct {
		name                string
		distributed, loaded []string
	}{
		{"routes", distributed.Routes, loaded.Routes},
		{"upstreams", distributed.Upstreams, loaded.Upstreams},
	} {
		if missing := without(kind.distributed, kind.loaded); len(missing) > 0 {
			return Answer{}, &NotApplied{Instance: req.Instance, Reason: fmt.Sprintf("it did not load the %s %v", kind.name, missing)}
		}
		if extra := without(kind.loaded, kind.distributed); len(extra) > 0 {
			return Answer{}, &NotApplied{Instance: req.Instance, Reason: fmt.Sprintf("it loaded the %s %v, which the distributed file does not hold", kind.name, extra)}
		}
	}
	return distributed.Answer, nil
}

// awaitAnswer reads the instance's answer until every answer over one polling interval is want. Any
// other answer, or a failed read, starts the interval again.
func (c Confirmer) awaitAnswer(address string, instance RunningInstance, want Answer) error {
	deadline := time.Now().Add(c.Timing.Timeout)
	var since time.Time // when the current run of answers that are want began
	last := "it did not answer"
	for {
		switch got, err := instance.Answer(); {
		case err != nil:
			since, last = time.Time{}, fmt.Sprintf("its last read failed: %v", err)
		case got != want:
			since, last = time.Time{}, fmt.Sprintf("it answers revision %s with digest %s", got.Revision, got.Digest)
		case since.IsZero():
			since = time.Now()
		case time.Since(since) >= c.Timing.Interval:
			return nil
		}
		if since.IsZero() && time.Now().After(deadline) {
			return &NotApplied{Instance: address, Reason: fmt.Sprintf("after %v %s, not revision %s with digest %s", c.Timing.Timeout, last, want.Revision, want.Digest)}
		}
		time.Sleep(c.Timing.Every)
	}
}
