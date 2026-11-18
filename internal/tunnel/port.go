package tunnel

import (
	"fmt"
	"strconv"
	"strings"

	"k8s.io/apimachinery/pkg/util/validation"
)

// PortKind distinguishes a numeric requested port from a named one.
type PortKind int

// Requested port kinds.
const (
	PortNumber PortKind = iota
	PortName
)

// ParsedPort is a requested remote port. The provider never accepts a local
// port component, so only the remote side is represented.
type ParsedPort struct {
	Kind   PortKind
	Number int32
	Name   string
}

// String returns the normalized spelling used for tunnel keys and data source
// IDs: the decimal number, or the port name unchanged.
func (p ParsedPort) String() string {
	if p.Kind == PortName {
		return p.Name
	}
	return strconv.Itoa(int(p.Number))
}

// ParsePort parses the requested remote port grammar:
//
//	5432     numeric remote port
//	postgres named remote port
//	:5432    explicit empty local component, normalized to 5432
//
// A non-empty local component such as 8888:5432 is rejected because the
// provider always allocates an OS-assigned local port on 127.0.0.1.
func ParsePort(raw string) (ParsedPort, error) {
	if raw == "" {
		return ParsedPort{}, fmt.Errorf("port must not be empty; use a number such as 5432, a port name such as postgres, or :5432")
	}
	if strings.TrimSpace(raw) != raw {
		return ParsedPort{}, fmt.Errorf("port %q must not contain leading or trailing whitespace", raw)
	}

	parts := strings.Split(raw, ":")
	if len(parts) > 2 {
		return ParsedPort{}, fmt.Errorf("port %q contains more than one ':'; only [LOCAL_PORT:]REMOTE_PORT is accepted", raw)
	}

	remote := parts[0]
	if len(parts) == 2 {
		local := parts[0]
		remote = parts[1]
		if local != "" {
			return ParsedPort{}, fmt.Errorf(
				"local port cannot be specified: the provider always allocates a random local port on 127.0.0.1 (got local port %q in %q)",
				local, raw,
			)
		}
	}
	if remote == "" {
		return ParsedPort{}, fmt.Errorf("port %q has an empty remote port; use a number such as 5432 or a port name such as postgres", raw)
	}

	if isDecimal(remote) {
		n, err := strconv.ParseInt(remote, 10, 64)
		if err != nil || n < 1 || n > 65535 {
			return ParsedPort{}, fmt.Errorf("port number %q is out of range: must be between 1 and 65535", remote)
		}
		return ParsedPort{Kind: PortNumber, Number: int32(n)}, nil
	}

	if errs := validation.IsValidPortName(remote); len(errs) > 0 {
		return ParsedPort{}, fmt.Errorf(
			"invalid port %q: expected a number between 1 and 65535 or a valid port name (%s)",
			remote, strings.Join(errs, "; "),
		)
	}
	return ParsedPort{Kind: PortName, Name: remote}, nil
}

func isDecimal(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}
