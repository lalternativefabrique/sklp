package publicnic

import (
	"fmt"
	"net"
	"strings"
)

const (
	DefaultInterface = "pub0"
	DefaultTable     = 100
	DefaultMark      = 0x19
)

// Config is the node.yaml `public_nic:` block. An empty Source means the
// NIC's own DHCP address.
type Config struct {
	Interface string `yaml:"interface,omitempty" json:"interface"`
	Source    string `yaml:"source,omitempty" json:"source,omitempty"`
	Table     int    `yaml:"table,omitempty" json:"table"`
	Mark      uint32 `yaml:"mark,omitempty" json:"mark"`
}

func (c Config) WithDefaults() Config {
	if c.Interface == "" {
		c.Interface = DefaultInterface
	}
	if c.Table == 0 {
		c.Table = DefaultTable
	}
	if c.Mark == 0 {
		c.Mark = DefaultMark
	}
	return c
}

func (c Config) Validate() error {
	if c.Interface != "" && (len(c.Interface) > 15 || strings.ContainsAny(c.Interface, " \t\n\"'/")) {
		return fmt.Errorf("public_nic.interface %q is not an interface name", c.Interface)
	}
	if c.Source != "" {
		if ip := net.ParseIP(c.Source); ip == nil || ip.To4() == nil {
			return fmt.Errorf("public_nic.source %q is not an IPv4 address", c.Source)
		}
	}
	if c.Table < 0 || c.Table == 253 || c.Table == 254 || c.Table == 255 {
		return fmt.Errorf("public_nic.table %d is reserved", c.Table)
	}
	return nil
}
