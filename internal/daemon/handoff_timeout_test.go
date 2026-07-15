package daemon

import (
	"testing"
	"time"

	"github.com/lin-labs/arcmux/internal/delivery"
)

func TestHandoffTimeoutBudgetsNestDeliveryWithinTargetWithinSource(t *testing.T) {
	deliveryTimeout := delivery.DefaultControllerConfig().IngestionTimeout
	if targetHandoffResumeTimeout != 45*time.Second {
		t.Fatalf("target resume timeout = %s, want 45s", targetHandoffResumeTimeout)
	}
	if sourceHandoffAttemptTimeout != 60*time.Second {
		t.Fatalf("source attempt timeout = %s, want 60s", sourceHandoffAttemptTimeout)
	}
	if !(sourceHandoffAttemptTimeout > targetHandoffResumeTimeout && targetHandoffResumeTimeout > deliveryTimeout) {
		t.Fatalf("timeout hierarchy source=%s target=%s delivery=%s", sourceHandoffAttemptTimeout, targetHandoffResumeTimeout, deliveryTimeout)
	}
}
