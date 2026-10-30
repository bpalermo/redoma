// Package config reads the host's one settings file, /etc/redoma/redoma.conf:
// what every redoma command shares, above all the trust domain, from which
// the SPIFFE IDs are derived. A command's own flag overrides the file.
//
// The file is lines of `key = value`; blank lines and lines starting with #
// are ignored. An unknown key, a key given twice, a line without `=` or a
// value that does not parse refuses the whole file, so a typo never falls
// back to a default silently.
package config

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"
	"github.com/spiffe/go-spiffe/v2/spiffeid"
)

// DefaultPath is where the file is installed (the package's conffile).
const DefaultPath = "/etc/redoma/redoma.conf"

// Config is the file's settings.
type Config struct {
	// TrustDomain is the agents' and the relay's SPIFFE trust domain
	// (trust_domain).
	TrustDomain string
	// AgentAnnotation is the OCI annotation that names a container's agent
	// (agent_annotation). The hook definitions match it by name: changing
	// it needs hook definitions that match the new key.
	AgentAnnotation string
	// SpireAdminSocket is the SPIRE agent's admin socket, where redoma xds
	// reaches the Delegated Identity API (spire_admin_socket).
	SpireAdminSocket string
	// WorkloadAPI is the SPIRE agent's Workload API address the relay, and
	// redoma xds in Broker mode, get their own SVIDs from
	// (spire_workload_api).
	WorkloadAPI string
	// IdentityAPI is how redoma xds obtains the agents' SVIDs
	// (identity_api): IdentityDelegated or IdentityBroker.
	IdentityAPI string
	// SpireBrokerSocket is the SPIRE agent's Broker API socket, which redoma
	// xds uses in Broker mode (spire_broker_socket).
	SpireBrokerSocket string
}

// The values of identity_api.
const (
	// IdentityDelegated is SPIRE's Delegated Identity API, on the admin
	// socket: redoma xds hands SPIRE the agents' selectors.
	IdentityDelegated = "delegated"
	// IdentityBroker is the SPIFFE Broker API: redoma xds names each agent's
	// process, and SPIRE's attestors decide its selectors.
	IdentityBroker = "broker"
)

// Default is what a missing file, or a key it leaves out, means.
func Default() Config {
	return Config{
		TrustDomain:       "redoma.internal",
		AgentAnnotation:   "redoma/agent",
		SpireAdminSocket:  "/run/redoma-spire/admin/admin.sock",
		WorkloadAPI:       "unix:///run/redoma-spire/workload/agent.sock",
		IdentityAPI:       IdentityDelegated,
		SpireBrokerSocket: "/run/redoma-spire/broker/broker.sock",
	}
}

// RelayPath is the relay's SPIFFE ID path in the trust domain.
const RelayPath = "/host/relay"

// RelayID is the relay's SPIFFE ID: spiffe://<td>/host/relay.
func (c Config) RelayID() string {
	return "spiffe://" + c.TrustDomain + RelayPath
}

// Load reads the file at path over Default. A file that does not exist is
// Default when required is false (the default path, which a host may not
// have), and an error when it is true (a path given on the command line).
func Load(path string, required bool) (Config, error) {
	f, err := os.Open(path)
	if errors.Is(err, fs.ErrNotExist) && !required {
		return Default(), nil
	}
	if err != nil {
		return Config{}, err
	}
	defer f.Close()
	c, err := Parse(f)
	if err != nil {
		return Config{}, fmt.Errorf("%s: %w", path, err)
	}
	return c, nil
}

// keys are the settings, by their name in the file.
var keys = map[string]func(*Config) *string{
	"trust_domain":        func(c *Config) *string { return &c.TrustDomain },
	"agent_annotation":    func(c *Config) *string { return &c.AgentAnnotation },
	"spire_admin_socket":  func(c *Config) *string { return &c.SpireAdminSocket },
	"spire_workload_api":  func(c *Config) *string { return &c.WorkloadAPI },
	"identity_api":        func(c *Config) *string { return &c.IdentityAPI },
	"spire_broker_socket": func(c *Config) *string { return &c.SpireBrokerSocket },
}

// WorkloadSocket is the path of the Workload API's socket: WorkloadAPI
// without its unix:// scheme.
func (c Config) WorkloadSocket() string {
	return strings.TrimPrefix(c.WorkloadAPI, "unix://")
}

// SpireSockets are the paths of SPIRE's sockets redoma uses: the admin
// socket, the Workload API's and the Broker API's.
func (c Config) SpireSockets() []string {
	return []string{c.SpireAdminSocket, c.WorkloadSocket(), c.SpireBrokerSocket}
}

// Parse reads a file's content over Default.
func Parse(r io.Reader) (Config, error) {
	c := Default()
	seen := map[string]bool{}
	sc := bufio.NewScanner(r)
	for n := 1; sc.Scan(); n++ {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		key, value, ok := strings.Cut(line, "=")
		key, value = strings.TrimSpace(key), strings.TrimSpace(value)
		if !ok || key == "" {
			return Config{}, fmt.Errorf("line %d: not `key = value`", n)
		}
		field, known := keys[key]
		if !known {
			return Config{}, fmt.Errorf("line %d: unknown setting %q", n, key)
		}
		if seen[key] {
			return Config{}, fmt.Errorf("line %d: %s is set twice", n, key)
		}
		seen[key] = true
		*field(&c) = value
	}
	if err := sc.Err(); err != nil {
		return Config{}, err
	}
	return c, c.Validate()
}

// Validate refuses settings no command could use.
func (c Config) Validate() error {
	// TrustDomainFromString also takes a SPIFFE ID and keeps its trust
	// domain: the setting must be the name alone.
	td, err := spiffeid.TrustDomainFromString(c.TrustDomain)
	if err != nil {
		return fmt.Errorf("trust_domain %q: %w", c.TrustDomain, err)
	}
	if td.Name() != c.TrustDomain {
		return fmt.Errorf("trust_domain %q: not a trust domain name", c.TrustDomain)
	}
	if c.AgentAnnotation == "" || strings.ContainsAny(c.AgentAnnotation, " \t=") {
		return fmt.Errorf("agent_annotation %q: not an annotation key", c.AgentAnnotation)
	}
	if !filepath.IsAbs(c.SpireAdminSocket) {
		return fmt.Errorf("spire_admin_socket %q: not an absolute path", c.SpireAdminSocket)
	}
	if !strings.HasPrefix(c.WorkloadAPI, "unix:///") {
		return fmt.Errorf("spire_workload_api %q: not a unix:/// address", c.WorkloadAPI)
	}
	if c.IdentityAPI != IdentityDelegated && c.IdentityAPI != IdentityBroker {
		return fmt.Errorf("identity_api %q: not %s or %s", c.IdentityAPI, IdentityDelegated, IdentityBroker)
	}
	return c.validateBrokerSocket()
}

// validateBrokerSocket refuses a broker socket that is not an absolute
// path, or that is in the Workload API socket's directory or below it,
// which SPIRE refuses too (the same prefix match: the directory and /).
func (c Config) validateBrokerSocket() error {
	if !filepath.IsAbs(c.SpireBrokerSocket) {
		return fmt.Errorf("spire_broker_socket %q: not an absolute path", c.SpireBrokerSocket)
	}
	dir := filepath.Dir(filepath.Clean(c.WorkloadSocket()))
	if strings.HasPrefix(filepath.Clean(c.SpireBrokerSocket), strings.TrimSuffix(dir, "/")+"/") {
		return fmt.Errorf("spire_broker_socket %q: in the directory of the Workload API's socket, %s, or below it (SPIRE refuses that)", c.SpireBrokerSocket, dir)
	}
	return nil
}

// FlagName is the root command's persistent flag naming the file.
const FlagName = "config"

// FromCommand loads the file the --config flag of cmd (or of a parent of
// it) names: required when the flag was given, else the default path that
// a host may not have.
func FromCommand(cmd *cobra.Command) (Config, error) {
	path, required := DefaultPath, false
	if f := cmd.Flag(FlagName); f != nil {
		path, required = f.Value.String(), f.Changed
	}
	return Load(path, required)
}

// Unless sets *dst to value unless cmd's flag name was given on the
// command line, which wins over the file.
func Unless(cmd *cobra.Command, name string, dst *string, value string) {
	if f := cmd.Flags().Lookup(name); f == nil || !f.Changed {
		*dst = value
	}
}
