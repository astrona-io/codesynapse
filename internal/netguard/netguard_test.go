package netguard

import (
	"errors"
	"net"
	"testing"

	"github.com/astrona-io/codesynapse/internal/testutil/assert"
)

func TestInternal(t *testing.T) {
	for _, tc := range []struct {
		ip   string
		want bool
	}{
		{"127.0.0.1", true},
		{"::1", true},
		{"10.1.2.3", true},
		{"172.16.0.1", true},
		{"192.168.1.1", true},
		{"169.254.169.254", true}, // cloud metadata
		{"fd00:ec2::254", true},   // AWS metadata over IPv6
		{"100.100.100.200", true}, // metadata in carrier-grade NAT space
		{"0.0.0.0", true},
		{"0.1.2.3", true},
		{"::", true},
		{"fe80::1", true},
		{"224.0.0.1", true},
		{"::ffff:127.0.0.1", true},
		{"::ffff:10.0.0.1", true},
		{"8.8.8.8", false},
		{"2606:4700::1111", false},
		{"100.128.0.1", false},
	} {
		t.Run(tc.ip, func(t *testing.T) {
			assert.Equal(t, Internal(net.ParseIP(tc.ip)), tc.want)
		})
	}
}

func TestControl(t *testing.T) {
	refused := errors.New("refused")
	ctl := Control(refused)
	assert.True(t, errors.Is(ctl("tcp", "127.0.0.1:443", nil), refused))
	assert.True(t, errors.Is(ctl("tcp", "[fd00::1]:443", nil), refused))
	assert.True(t, errors.Is(ctl("tcp", "example.com:443", nil), refused), "a name is never dialled unresolved")
	assert.NoError(t, ctl("tcp", "93.184.215.14:443", nil))
	assert.Error(t, ctl("tcp", "no-port", nil))
}
