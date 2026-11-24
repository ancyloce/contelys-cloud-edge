// Package application renders Edge's configuration from its inputs: the instances file, the
// installation file, the route fragments, the defaults file, the secret files and the
// configuration revision (Edge detailed design, section 03).
package application

// Request names the renderer's inputs and its output directory, as the command line gives them.
type Request struct {
	Instances    string
	Installation string
	Fragments    string
	Secrets      string
	Revision     string
	Out          string
}

// Inputs is everything the renderer reads, already decoded and checked against its own format.
type Inputs struct {
	Defaults     Defaults
	Instances    Instances
	Installation Installation
	// Fragments are sorted by slug.
	Fragments []Fragment
	Secrets   Secrets
	Revision  string
}

// Instances maps a deployable's slug, and keycloak, to its instances, in the order of the file.
type Instances map[string][]Instance

// EdgeSlug is Edge's own slug. Its instances in the instances file are the Edge instances that the
// renderer writes an apisix.yaml for.
const EdgeSlug = "edge"

// Instance is one instance of a deployable. A weight of 1 serves and 0 drains.
type Instance struct {
	Address string `yaml:"address"`
	Port    int    `yaml:"port"`
	Weight  int    `yaml:"weight"`
}

// Installation holds the values of the installation as a whole.
type Installation struct {
	ApplicationHostname    string   `yaml:"application_hostname"`
	AuthenticationHostname string   `yaml:"authentication_hostname"`
	LoadBalancerAddresses  []string `yaml:"load_balancer_addresses"`
	CollectorAddresses     []string `yaml:"collector_addresses"`
}

// Secrets are the contents of the secret files that the renderer copies into apisix.yaml.
type Secrets struct {
	ApplicationCertificate string
	ApplicationKey         string
}

// Fragment is the route fragment of one deployable.
type Fragment struct {
	Slug   string
	Routes []FragmentRoute
}

// FragmentRoute is one route of a fragment, with the four fields of the fragment schema. An empty
// Auth or Traffic takes its default.
type FragmentRoute struct {
	Paths   []string `yaml:"paths"`
	Methods []string `yaml:"methods"`
	Auth    string   `yaml:"auth"`
	Traffic string   `yaml:"traffic"`
}

// The values of a route's auth field.
const (
	AuthRequired  = "required"
	AuthAnonymous = "anonymous"
)

// The values of a route's traffic field.
const (
	TrafficAPI            = "api"
	TrafficAuthentication = "authentication"
	TrafficWebSocket      = "websocket"
	TrafficTransfer       = "transfer"
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
