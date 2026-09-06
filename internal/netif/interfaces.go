// Package netif reports the host's current network interfaces for
// operator-facing configuration. This is deliberately a read-only,
// node-local inventory: interface names and addresses have meaning only on
// the Hive where they were observed.
package netif

import (
	"net"
	"sort"
)

// Interface is the small subset of host interface state useful when
// selecting a VLAN or NAT uplink in the Machine Configuration page.
type Interface struct {
	Name      string
	Up        bool
	Addresses []string
}

// List returns the interfaces currently visible to this process. An
// address lookup failure for one interface does not discard the rest of the
// inventory, because interfaces can disappear while the host is changing
// state. A failure to enumerate interfaces at all is returned.
func List() ([]Interface, error) {
	interfaces, err := net.Interfaces()
	if err != nil {
		return nil, err
	}

	result := make([]Interface, 0, len(interfaces))
	for _, iface := range interfaces {
		item := Interface{
			Name: iface.Name,
			Up:   iface.Flags&net.FlagUp != 0,
		}
		if addrs, err := iface.Addrs(); err == nil {
			item.Addresses = make([]string, 0, len(addrs))
			for _, addr := range addrs {
				item.Addresses = append(item.Addresses, addr.String())
			}
			sort.Strings(item.Addresses)
		}
		result = append(result, item)
	}
	sort.Slice(result, func(i, j int) bool { return result[i].Name < result[j].Name })
	return result, nil
}
