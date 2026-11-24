package application

import "fmt"

// Refusal is a route fragment that breaks a rule. The renderer then writes nothing, so the current
// files stay in place, and the command exits with 1.
type Refusal struct {
	Fragment Slug
	Rule     string
}

func (r *Refusal) Error() string {
	return fmt.Sprintf("fragment %s: %s", r.Fragment, r.Rule)
}
