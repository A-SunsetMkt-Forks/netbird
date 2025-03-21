package conntrack

import (
	"net/netip"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestTCPStateMachine(t *testing.T) {
	tracker := NewTCPTracker(DefaultTCPTimeout, logger, flowLogger)
	defer tracker.Close()

	srcIP := netip.MustParseAddr("100.64.0.1")
	dstIP := netip.MustParseAddr("100.64.0.2")
	srcPort := uint16(12345)
	dstPort := uint16(80)

	t.Run("Security Tests", func(t *testing.T) {
		tests := []struct {
			name     string
			flags    uint8
			wantDrop bool
			desc     string
		}{
			{
				name:     "Block unsolicited SYN-ACK",
				flags:    TCPSyn | TCPAck,
				wantDrop: true,
				desc:     "Should block SYN-ACK without prior SYN",
			},
			{
				name:     "Block invalid SYN-FIN",
				flags:    TCPSyn | TCPFin,
				wantDrop: true,
				desc:     "Should block invalid SYN-FIN combination",
			},
			{
				name:     "Block unsolicited RST",
				flags:    TCPRst,
				wantDrop: true,
				desc:     "Should block RST without connection",
			},
			{
				name:     "Block unsolicited ACK",
				flags:    TCPAck,
				wantDrop: true,
				desc:     "Should block ACK without connection",
			},
			{
				name:     "Block data without connection",
				flags:    TCPAck | TCPPush,
				wantDrop: true,
				desc:     "Should block data without established connection",
			},
		}

		for _, tt := range tests {
			t.Run(tt.name, func(t *testing.T) {
				isValid := tracker.IsValidInbound(dstIP, srcIP, dstPort, srcPort, tt.flags, 0)
				require.Equal(t, !tt.wantDrop, isValid, tt.desc)
			})
		}
	})

	t.Run("Connection Flow Tests", func(t *testing.T) {
		tests := []struct {
			name string
			test func(*testing.T)
			desc string
		}{
			{
				name: "Normal Handshake",
				test: func(t *testing.T) {
					t.Helper()

					// Send initial SYN
					tracker.TrackOutbound(srcIP, dstIP, srcPort, dstPort, TCPSyn, 0)

					// Receive SYN-ACK
					valid := tracker.IsValidInbound(dstIP, srcIP, dstPort, srcPort, TCPSyn|TCPAck, 0)
					require.True(t, valid, "SYN-ACK should be allowed")

					// Send ACK
					tracker.TrackOutbound(srcIP, dstIP, srcPort, dstPort, TCPAck, 0)

					// Test data transfer
					valid = tracker.IsValidInbound(dstIP, srcIP, dstPort, srcPort, TCPPush|TCPAck, 0)
					require.True(t, valid, "Data should be allowed after handshake")
				},
			},
			{
				name: "Normal Close",
				test: func(t *testing.T) {
					t.Helper()

					// First establish connection
					establishConnection(t, tracker, srcIP, dstIP, srcPort, dstPort)

					// Send FIN
					tracker.TrackOutbound(srcIP, dstIP, srcPort, dstPort, TCPFin|TCPAck, 0)

					// Receive ACK for FIN
					valid := tracker.IsValidInbound(dstIP, srcIP, dstPort, srcPort, TCPAck, 0)
					require.True(t, valid, "ACK for FIN should be allowed")

					// Receive FIN from other side
					valid = tracker.IsValidInbound(dstIP, srcIP, dstPort, srcPort, TCPFin|TCPAck, 0)
					require.True(t, valid, "FIN should be allowed")

					// Send final ACK
					tracker.TrackOutbound(srcIP, dstIP, srcPort, dstPort, TCPAck, 0)
				},
			},
			{
				name: "RST During Connection",
				test: func(t *testing.T) {
					t.Helper()

					// First establish connection
					establishConnection(t, tracker, srcIP, dstIP, srcPort, dstPort)

					// Receive RST
					valid := tracker.IsValidInbound(dstIP, srcIP, dstPort, srcPort, TCPRst, 0)
					require.True(t, valid, "RST should be allowed for established connection")

					// Connection is logically dead but we don't enforce blocking subsequent packets
					// The connection will be cleaned up by timeout
				},
			},
			{
				name: "Simultaneous Close",
				test: func(t *testing.T) {
					t.Helper()

					// First establish connection
					establishConnection(t, tracker, srcIP, dstIP, srcPort, dstPort)

					// Both sides send FIN+ACK
					tracker.TrackOutbound(srcIP, dstIP, srcPort, dstPort, TCPFin|TCPAck, 0)
					valid := tracker.IsValidInbound(dstIP, srcIP, dstPort, srcPort, TCPFin|TCPAck, 0)
					require.True(t, valid, "Simultaneous FIN should be allowed")

					// Both sides send final ACK
					tracker.TrackOutbound(srcIP, dstIP, srcPort, dstPort, TCPAck, 0)
					valid = tracker.IsValidInbound(dstIP, srcIP, dstPort, srcPort, TCPAck, 0)
					require.True(t, valid, "Final ACKs should be allowed")
				},
			},
		}

		for _, tt := range tests {
			t.Run(tt.name, func(t *testing.T) {
				t.Helper()

				tracker = NewTCPTracker(DefaultTCPTimeout, logger, flowLogger)
				tt.test(t)
			})
		}
	})
}

func TestRSTHandling(t *testing.T) {
	tracker := NewTCPTracker(DefaultTCPTimeout, logger, flowLogger)
	defer tracker.Close()

	srcIP := netip.MustParseAddr("100.64.0.1")
	dstIP := netip.MustParseAddr("100.64.0.2")
	srcPort := uint16(12345)
	dstPort := uint16(80)

	tests := []struct {
		name       string
		setupState func()
		sendRST    func()
		wantValid  bool
		desc       string
	}{
		{
			name: "RST in established",
			setupState: func() {
				// Establish connection first
				tracker.TrackOutbound(srcIP, dstIP, srcPort, dstPort, TCPSyn, 0)
				tracker.IsValidInbound(dstIP, srcIP, dstPort, srcPort, TCPSyn|TCPAck, 0)
				tracker.TrackOutbound(srcIP, dstIP, srcPort, dstPort, TCPAck, 0)
			},
			sendRST: func() {
				tracker.IsValidInbound(dstIP, srcIP, dstPort, srcPort, TCPRst, 0)
			},
			wantValid: true,
			desc:      "Should accept RST for established connection",
		},
		{
			name:       "RST without connection",
			setupState: func() {},
			sendRST: func() {
				tracker.IsValidInbound(dstIP, srcIP, dstPort, srcPort, TCPRst, 0)
			},
			wantValid: false,
			desc:      "Should reject RST without connection",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tt.setupState()
			tt.sendRST()

			// Verify connection state is as expected
			key := ConnKey{
				SrcIP:   srcIP,
				DstIP:   dstIP,
				SrcPort: srcPort,
				DstPort: dstPort,
			}
			conn := tracker.connections[key]
			if tt.wantValid {
				require.NotNil(t, conn)
				require.Equal(t, TCPStateClosed, conn.State)
				require.False(t, conn.IsEstablished())
			}
		})
	}
}

// Helper to establish a TCP connection
func establishConnection(t *testing.T, tracker *TCPTracker, srcIP, dstIP netip.Addr, srcPort, dstPort uint16) {
	t.Helper()

	tracker.TrackOutbound(srcIP, dstIP, srcPort, dstPort, TCPSyn, 0)

	valid := tracker.IsValidInbound(dstIP, srcIP, dstPort, srcPort, TCPSyn|TCPAck, 0)
	require.True(t, valid, "SYN-ACK should be allowed")

	tracker.TrackOutbound(srcIP, dstIP, srcPort, dstPort, TCPAck, 0)
}

func BenchmarkTCPTracker(b *testing.B) {
	b.Run("TrackOutbound", func(b *testing.B) {
		tracker := NewTCPTracker(DefaultTCPTimeout, logger, flowLogger)
		defer tracker.Close()

		srcIP := netip.MustParseAddr("192.168.1.1")
		dstIP := netip.MustParseAddr("192.168.1.2")

		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			tracker.TrackOutbound(srcIP, dstIP, uint16(i%65535), 80, TCPSyn, 0)
		}
	})

	b.Run("IsValidInbound", func(b *testing.B) {
		tracker := NewTCPTracker(DefaultTCPTimeout, logger, flowLogger)
		defer tracker.Close()

		srcIP := netip.MustParseAddr("192.168.1.1")
		dstIP := netip.MustParseAddr("192.168.1.2")

		// Pre-populate some connections
		for i := 0; i < 1000; i++ {
			tracker.TrackOutbound(srcIP, dstIP, uint16(i), 80, TCPSyn, 0)
		}

		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			tracker.IsValidInbound(dstIP, srcIP, 80, uint16(i%1000), TCPAck, 0)
		}
	})

	b.Run("ConcurrentAccess", func(b *testing.B) {
		tracker := NewTCPTracker(DefaultTCPTimeout, logger, flowLogger)
		defer tracker.Close()

		srcIP := netip.MustParseAddr("192.168.1.1")
		dstIP := netip.MustParseAddr("192.168.1.2")

		b.RunParallel(func(pb *testing.PB) {
			i := 0
			for pb.Next() {
				if i%2 == 0 {
					tracker.TrackOutbound(srcIP, dstIP, uint16(i%65535), 80, TCPSyn, 0)
				} else {
					tracker.IsValidInbound(dstIP, srcIP, 80, uint16(i%65535), TCPAck, 0)
				}
				i++
			}
		})
	})
}

// Benchmark connection cleanup
func BenchmarkCleanup(b *testing.B) {
	b.Run("TCPCleanup", func(b *testing.B) {
		tracker := NewTCPTracker(100*time.Millisecond, logger, flowLogger) // Short timeout for testing
		defer tracker.Close()

		// Pre-populate with expired connections
		srcIP := netip.MustParseAddr("192.168.1.1")
		dstIP := netip.MustParseAddr("192.168.1.2")
		for i := 0; i < 10000; i++ {
			tracker.TrackOutbound(srcIP, dstIP, uint16(i), 80, TCPSyn, 0)
		}

		// Wait for connections to expire
		time.Sleep(200 * time.Millisecond)

		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			tracker.cleanup()
		}
	})
}
