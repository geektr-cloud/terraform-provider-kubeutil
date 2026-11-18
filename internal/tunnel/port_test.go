package tunnel

import (
	"strings"
	"testing"
)

func TestParsePortValid(t *testing.T) {
	t.Parallel()

	tests := []struct {
		input      string
		wantKind   PortKind
		wantNumber int32
		wantName   string
		normalized string
	}{
		{"5432", PortNumber, 5432, "", "5432"},
		{":5432", PortNumber, 5432, "", "5432"},
		{"1", PortNumber, 1, "", "1"},
		{"65535", PortNumber, 65535, "", "65535"},
		{"05432", PortNumber, 5432, "", "5432"},
		{"postgres", PortName, 0, "postgres", "postgres"},
		{":postgres", PortName, 0, "postgres", "postgres"},
		{"http-2", PortName, 0, "http-2", "http-2"},
	}

	for _, test := range tests {
		test := test
		t.Run(test.input, func(t *testing.T) {
			t.Parallel()
			port, err := ParsePort(test.input)
			if err != nil {
				t.Fatalf("ParsePort(%q) returned error: %s", test.input, err)
			}
			if port.Kind != test.wantKind || port.Number != test.wantNumber || port.Name != test.wantName {
				t.Fatalf("ParsePort(%q) = %#v, want kind %v number %d name %q", test.input, port, test.wantKind, test.wantNumber, test.wantName)
			}
			if port.String() != test.normalized {
				t.Fatalf("ParsePort(%q).String() = %q, want %q", test.input, port.String(), test.normalized)
			}
		})
	}
}

func TestParsePortInvalid(t *testing.T) {
	t.Parallel()

	tests := []struct {
		input       string
		wantMessage string
	}{
		{"", "port must not be empty"},
		{" 5432", "whitespace"},
		{"5432 ", "whitespace"},
		{"8888:5432", "local port cannot be specified"},
		{"0:5432", "local port cannot be specified"},
		{"localhost:5432", "local port cannot be specified"},
		{"1:2:3", "more than one ':'"},
		{":", "empty remote port"},
		{"5432:", "local port cannot be specified"},
		{"0", "out of range"},
		{":0", "out of range"},
		{"65536", "out of range"},
		{"999999999999999999999999", "out of range"},
		{"-1", "invalid port"},
		{"+80", "invalid port"},
		{"HTTP", "invalid port"},
		{"_http", "invalid port"},
	}

	for _, test := range tests {
		test := test
		t.Run(test.input, func(t *testing.T) {
			t.Parallel()
			_, err := ParsePort(test.input)
			if err == nil {
				t.Fatalf("ParsePort(%q) expected error", test.input)
			}
			if !strings.Contains(err.Error(), test.wantMessage) {
				t.Fatalf("ParsePort(%q) error = %q, want it to contain %q", test.input, err, test.wantMessage)
			}
		})
	}
}
