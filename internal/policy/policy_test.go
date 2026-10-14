package policy

import (
	"testing"

	"github.com/perishabledave/bosun/internal/integration"
)

func TestDefaultsForRisk(t *testing.T) {
	d := Defaults{Read: Allow, Write: Ask, Destructive: Ask}
	if d.ForRisk(integration.RiskRead) != Allow || d.ForRisk(integration.RiskWrite) != Ask {
		t.Error("wrong mapping")
	}
	if d.ForRisk("unknown") != Deny {
		t.Error("unknown risk must deny")
	}
	if (Defaults{}).ForRisk(integration.RiskRead) != Deny {
		t.Error("unset default must deny")
	}
}
