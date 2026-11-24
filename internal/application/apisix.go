package application

// ApisixFile is the content of one Edge instance's apisix.yaml, the file that APISIX reads in
// file-driven mode. Every value a fragment contributes sits in a typed field, so that it cannot
// inject YAML.
type ApisixFile struct {
	Routes    []Route    `yaml:"routes"`
	Upstreams []Upstream `yaml:"upstreams"`
	SSLs      []SSL      `yaml:"ssls"`
}

// Route is one APISIX route. Every route carries an explicit id, a name and a listener condition.
type Route struct {
	ID         string         `yaml:"id"`
	Name       string         `yaml:"name"`
	Host       string         `yaml:"host"`
	URIs       []string       `yaml:"uris"`
	Methods    []string       `yaml:"methods"`
	Vars       [][]any        `yaml:"vars"`
	UpstreamID string         `yaml:"upstream_id"`
	Plugins    map[string]any `yaml:"plugins,omitempty"`
}

// Upstream is one APISIX upstream: the instances of one deployable.
type Upstream struct {
	ID           string      `yaml:"id"`
	Name         string      `yaml:"name"`
	Type         string      `yaml:"type"`
	Scheme       string      `yaml:"scheme"`
	PassHost     string      `yaml:"pass_host"`
	UpstreamHost string      `yaml:"upstream_host"`
	TLS          UpstreamTLS `yaml:"tls"`
	Retries      int         `yaml:"retries"`
	Nodes        []Node      `yaml:"nodes"`
}

// UpstreamTLS asks APISIX to verify the upstream's certificate against the trust store of
// config.yaml and the name that Edge sends as Host.
type UpstreamTLS struct {
	Verify bool `yaml:"verify"`
}

// Node is one instance in an upstream.
type Node struct {
	Host   string `yaml:"host"`
	Port   int    `yaml:"port"`
	Weight int    `yaml:"weight"`
}

// SSL is a certificate that a listener presents for the names in SNIs.
type SSL struct {
	ID   string   `yaml:"id"`
	SNIs []string `yaml:"snis"`
	Cert string   `yaml:"cert"`
	Key  string   `yaml:"key"`
}

// ResponseRewrite is the configuration of the built-in response-rewrite plugin, which Edge uses only
// for the Strict-Transport-Security header and for the fixed body of the loopback route.
type ResponseRewrite struct {
	Headers ResponseHeaders `yaml:"headers"`
}

// ResponseHeaders are the headers that response-rewrite sets on an answer.
type ResponseHeaders struct {
	Set map[string]string `yaml:"set"`
}
