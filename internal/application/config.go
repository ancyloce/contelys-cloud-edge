package application

import (
	"fmt"
	"slices"
)

// TrustStorePath is where each Edge instance mounts the installation's certificate authority. APISIX
// verifies every upstream against it.
const TrustStorePath = "/etc/edge/ca.crt"

// The listeners bind these addresses inside the container. The loopback listener carries Edge's call
// to its own loopback route, so it stays on 127.0.0.1. The control listener binds every address of
// the container, because a published port reaches only a listener on the container's own address. The
// deployment publishes its port on the VM's loopback address only and keeps other containers off
// Edge's networks, so that the VM's liveness probe and the comparison of loaded ids reach it and
// neither the network nor another container does (tickets 06 and 23).
const (
	loopbackAddress = "127.0.0.1"
	controlAddress  = "0.0.0.0"
)

// enabledPlugins are the built-in plugins that the design uses, and no others: no plugin for CORS,
// compression or serverless functions. mocking answers the fixed-response route.
var enabledPlugins = []string{
	"client-control", "forward-auth", "limit-conn", "limit-count", "limit-req", FixedResponsePlugin, "opentelemetry",
	"prometheus", "proxy-rewrite", "public-api", "real-ip", "request-id", "response-rewrite",
}

// ConfigFile is the content of config.yaml, which the renderer writes once for the installation.
// Every value in it needs a rolling restart to change (section 03).
type ConfigFile struct {
	Deployment  Deployment  `yaml:"deployment"`
	Apisix      ApisixBlock `yaml:"apisix"`
	NginxConfig NginxConfig `yaml:"nginx_config"`
	Plugins     []string    `yaml:"plugins"`
	PluginAttr  PluginAttr  `yaml:"plugin_attr"`
}

// Deployment selects file-driven mode: a data plane that reads apisix.yaml, which also switches the
// Admin API off.
type Deployment struct {
	Role          string        `yaml:"role"`
	RoleDataPlane RoleDataPlane `yaml:"role_data_plane"`
}

// RoleDataPlane names the provider of the configuration.
type RoleDataPlane struct {
	ConfigProvider string `yaml:"config_provider"`
}

// ApisixBlock is the apisix key of config.yaml: the listeners and the handling of forwarded headers.
type ApisixBlock struct {
	// NodeListen holds the plain-HTTP listeners: the loopback listener alone.
	NodeListen    []Listen       `yaml:"node_listen"`
	SSL           SSLBlock       `yaml:"ssl"`
	ProxyProtocol *ProxyProtocol `yaml:"proxy_protocol,omitempty"`
	EnableControl bool           `yaml:"enable_control"`
	Control       Listen         `yaml:"control"`
	// TrustedAddresses are the peers whose forwarded headers APISIX keeps. Only the loopback address
	// is trusted, so Edge writes the forwarded headers itself (section 06).
	TrustedAddresses []string `yaml:"trusted_addresses"`
}

// Listen is one listener.
type Listen struct {
	IP   string `yaml:"ip,omitempty"`
	Port int    `yaml:"port"`
}

// SSLBlock holds the TLS listeners, their protocols and the trust store for upstreams.
type SSLBlock struct {
	Enable                bool     `yaml:"enable"`
	Listen                []Listen `yaml:"listen"`
	SSLProtocols          string   `yaml:"ssl_protocols"`
	SSLTrustedCertificate string   `yaml:"ssl_trusted_certificate"`
}

// ProxyProtocol holds the external listener when a load balancer sends the PROXY protocol: it
// accepts only connections that start with a PROXY protocol header, on every interface.
type ProxyProtocol struct {
	ListenHTTPSPort int `yaml:"listen_https_port"`
}

// NginxConfig holds the process and connection limits, the drain limit and the header-size limits.
type NginxConfig struct {
	WorkerProcesses       string `yaml:"worker_processes"`
	WorkerRlimitNofile    int    `yaml:"worker_rlimit_nofile"`
	WorkerShutdownTimeout string `yaml:"worker_shutdown_timeout"`
	Event                 Event  `yaml:"event"`
	// HTTPConfigurationSnippet sets the header-size limits as nginx directives, because nginx has no
	// setting for one route and APISIX renders none (section 06).
	HTTPConfigurationSnippet string `yaml:"http_configuration_snippet"`
}

// Event holds nginx's events block.
type Event struct {
	WorkerConnections int `yaml:"worker_connections"`
}

// PluginAttr holds the attributes of plugins that apply to the whole instance.
type PluginAttr struct {
	ForwardAuth ForwardAuthAttr `yaml:"forward-auth"`
	Prometheus  PrometheusAttr  `yaml:"prometheus"`
}

// ForwardAuthAttr names forward-auth's HTTP client, so that a move of the test baseline cannot change
// it silently (section 04).
type ForwardAuthAttr struct {
	HTTPClient string `yaml:"http_client"`
}

// PrometheusAttr switches the prometheus plugin's own export server off: metrics are published
// through a route on the internal listener (section 09).
type PrometheusAttr struct {
	EnableExportServer bool `yaml:"enable_export_server"`
}

func renderConfig(in Inputs) ConfigFile {
	listeners := in.Defaults.Listeners
	instance := in.Defaults.Instance
	tlsListeners := []Listen{{Port: listeners.ExternalPort}, {Port: listeners.InternalPort}}
	var proxyProtocol *ProxyProtocol
	if len(in.Installation.LoadBalancerAddresses) > 0 {
		// The external listener moves to the PROXY protocol listener, which nginx refuses to bind
		// twice on one port.
		tlsListeners = []Listen{{Port: listeners.InternalPort}}
		proxyProtocol = &ProxyProtocol{ListenHTTPSPort: listeners.ExternalPort}
	}
	return ConfigFile{
		Deployment: Deployment{Role: "data_plane", RoleDataPlane: RoleDataPlane{ConfigProvider: "yaml"}},
		Apisix: ApisixBlock{
			NodeListen: []Listen{{IP: loopbackAddress, Port: listeners.LoopbackPort}},
			SSL: SSLBlock{
				Enable:                true,
				Listen:                tlsListeners,
				SSLProtocols:          "TLSv1.2 TLSv1.3",
				SSLTrustedCertificate: TrustStorePath,
			},
			ProxyProtocol:    proxyProtocol,
			EnableControl:    true,
			Control:          Listen{IP: controlAddress, Port: listeners.ControlPort},
			TrustedAddresses: []string{loopbackAddress},
		},
		NginxConfig: NginxConfig{
			WorkerProcesses:       instance.WorkerProcesses,
			WorkerRlimitNofile:    instance.WorkerRlimitNofile,
			WorkerShutdownTimeout: fmt.Sprintf("%ds", instance.Drain.WorkerShutdownTimeoutS),
			Event:                 Event{WorkerConnections: instance.WorkerConnections},
			// nginx refuses to start when proxy_busy_buffers_size, by default twice proxy_buffer_size,
			// is not below the default proxy_buffers minus one buffer. Setting it to proxy_buffer_size
			// meets both of nginx's bounds without adding memory per request.
			HTTPConfigurationSnippet: fmt.Sprintf(
				"client_header_buffer_size %s;\nlarge_client_header_buffers %s;\nproxy_buffer_size %s;\nproxy_busy_buffers_size %s;\n",
				instance.ClientHeaderBufferSize, instance.LargeClientHeaderBuffers, instance.ProxyBufferSize, instance.ProxyBufferSize),
		},
		Plugins: slices.Clone(enabledPlugins),
		PluginAttr: PluginAttr{
			ForwardAuth: ForwardAuthAttr{HTTPClient: "ngx_http_ffi_client"},
			Prometheus:  PrometheusAttr{EnableExportServer: false},
		},
	}
}
