package usbdev

import (
	"errors"
	"fmt"
	"testing"

	"github.com/google/gousb"
)

func TestIsBusy(t *testing.T) {
	// Testo esatto di gousb v1.1.3 (config.go), che usa %v e perde il tipo.
	gousbErr := fmt.Errorf("failed to claim interface %d on %s: %v", 0, "vid=2104,pid=0127,bus=1,addr=3,config=1", gousb.ErrorBusy)
	for _, tc := range []struct {
		err  error
		want bool
	}{
		{gousbErr, true},
		{fmt.Errorf("claim: %w", gousb.ErrorBusy), true},
		{gousb.ErrorAccess, false},
		{errors.New("failed to claim interface 0: libusb: not found [code -5]"), false},
	} {
		if got := isBusy(tc.err); got != tc.want {
			t.Errorf("isBusy(%q) = %v, want %v", tc.err, got, tc.want)
		}
	}
}
