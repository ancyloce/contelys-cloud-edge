// Package application renders Edge's configuration from its inputs: the instances file, the
// installation file, the route fragments, the defaults file, the secret files and the
// configuration revision (Edge detailed design, section 03).
package application

import "regexp"

// Request names the renderer's inputs and its output directory, as the command line gives them.
type Request struct {
	Instances    string
	Installation string
	Fragments    string
	// Contracts is the directory of the deployables' OpenAPI contracts, openapi/ of the shared
	// contracts, which holds the contract of each domain service under its slug.
	Contracts string
	Secrets   string
	Revision  string
	// Image is Edge's image as the version manifest pins it, which the trial load starts.
	Image string
	Out   string
}

// Inputs is everything the renderer reads, decoded from its files. The renderer checks the rules of
// the application before it renders them. The configuration revision joins them with the route
// that confirms it (ticket 08 of Edge's plan).
type Inputs struct {
	Defaults     Defaults
	Instances    Instances
	Installation Installation
	// Fragments are sorted by slug.
	Fragments []Fragment
	// Contracts hold the paths of the OpenAPI contract of each deployable that ships a fragment, as
	// the contract writes them, such as /api/v1/projects/{projectId}. A deployable without a
	// contract has no entry.
	Contracts map[Slug][]string
	Secrets   Secrets
	// Revision is the configuration revision, the Git commit that the fixed-response route answers.
	Revision string
}

// Slug is a deployable's name in lower case with hyphens, such as organization-access.
type Slug string

var slugPattern = regexp.MustCompile(`^[a-z][a-z0-9]*(-[a-z0-9]+)*$`)

func (s Slug) valid() bool { return slugPattern.MatchString(string(s)) }

// Instances maps a deployable's slug, and keycloak, to its instances, in the order of the file.
type Instances map[Slug][]Instance

// EdgeSlug is Edge's own slug. Its instances in the instances file are the Edge instances that the
// renderer writes an apisix.yaml for.
const EdgeSlug Slug = "edge"

// keycloakSlug names Keycloak's instances in the instances file. Keycloak is an infrastructure
// product and not a deployable, so Edge's own template holds its routes (section 03).
const keycloakSlug Slug = "keycloak"

// AuthoringSlug is Authoring's slug. Its fragment alone may claim the remainder of the application
// hostname, and it has no OpenAPI contract (section 03).
const AuthoringSlug Slug = "authoring"

// Instance is one instance of a deployable. A weight of 1 serves and 0 drains.
type Instance struct {
	Address string `yaml:"address"`
	Port    int    `yaml:"port"`
	Weight  int    `yaml:"weight"`
}

// Installation holds the values of the installation as a whole, in the form that section 03 of the
// design gives the installation file. The authentication hostname and the collectors' addresses are
// rendered from tickets 11 and 19 on, and are checked from the start.
type Installation struct {
	ApplicationHostname    string   `yaml:"application_hostname"`
	AuthenticationHostname string   `yaml:"authentication_hostname"`
	LoadBalancerAddresses  []string `yaml:"load_balancer_addresses"`
	CollectorAddresses     []string `yaml:"collector_addresses"`
}

// Secrets are the contents of the secret files: the certificates and keys that the renderer copies
// into apisix.yaml, the application hostname's and Edge's own for the internal listener, and the
// trust store, which each instance mounts and the trial load hands to its instance.
type Secrets struct {
	ApplicationCertificate string
	ApplicationKey         string
	EdgeCertificate        string
	EdgeKey                string
	TrustStore             string
}

// Fragment is the route fragment of one deployable. Its slug is the name of its directory.
type Fragment struct {
	Slug   Slug
	Routes []FragmentRoute
}

// FragmentRoute is one route of a fragment, with the four fields of the fragment schema. An empty
// Auth or Traffic takes its default.
type FragmentRoute struct {
	Paths   []string     `yaml:"paths"`
	Methods []string     `yaml:"methods"`
	Auth    Auth         `yaml:"auth"`
	Traffic TrafficClass `yaml:"traffic"`
}

// Auth says whether a route needs an authenticated caller.
type Auth string

// The values of a route's auth field.
const (
	AuthRequired  Auth = "required"
	AuthAnonymous Auth = "anonymous"
)

// TrafficClass is a kind of traffic that shares one set of limits, timeouts and sizes (section 05).
type TrafficClass string

// The values of a route's traffic field.
const (
	TrafficAPI            TrafficClass = "api"
	TrafficAuthentication TrafficClass = "authentication"
	TrafficWebSocket      TrafficClass = "websocket"
	TrafficTransfer       TrafficClass = "transfer"
)

// Defaults holds the values of the defaults file that the renderer uses so far. The file holds
// more, which later parts of the renderer read.
type Defaults struct {
	Listeners               Listeners        `yaml:"listeners"`
	Instance                InstanceDefaults `yaml:"instance"`
	StrictTransportSecurity string           `yaml:"strict_transport_security"`
}

// Listeners are the ports of an Edge instance's listeners.
type Listeners struct {
	ExternalPort int `yaml:"external_port"`
	InternalPort int `yaml:"internal_port"`
	LoopbackPort int `yaml:"loopback_port"`
	ControlPort  int `yaml:"control_port"`
}

// InstanceDefaults are the values of a whole Edge instance, rendered into config.yaml.
type InstanceDefaults struct {
	WorkerProcesses          string `yaml:"worker_processes"`
	WorkerConnections        int    `yaml:"worker_connections"`
	WorkerRlimitNofile       int    `yaml:"worker_rlimit_nofile"`
	ClientHeaderBufferSize   string `yaml:"client_header_buffer_size"`
	LargeClientHeaderBuffers string `yaml:"large_client_header_buffers"`
	ProxyBufferSize          string `yaml:"proxy_buffer_size"`
	Drain                    struct {
		WorkerShutdownTimeoutS int `yaml:"worker_shutdown_timeout_s"`
	} `yaml:"drain"`
}
