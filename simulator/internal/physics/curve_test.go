package physics

import "testing"

func TestLFPCurveEndpointsMatchPackVoltageRange(t *testing.T) {
	// §4.8: 676 V / 936 V / 832 V pack figures over 260 series cells.
	cases := []struct {
		soc, wantCellV float64
	}{
		{0, 676.0 / 260},
		{50, 832.0 / 260},
		{100, 936.0 / 260},
	}
	for _, c := range cases {
		if got := lfpCellVoltage(c.soc); got != c.wantCellV {
			t.Errorf("lfpCellVoltage(%v) = %v, want %v", c.soc, got, c.wantCellV)
		}
	}
}

func TestLFPCurveIsMonotonicNonDecreasing(t *testing.T) {
	prev := lfpCellVoltage(0)
	for soc := 0.5; soc <= 100; soc += 0.5 {
		v := lfpCellVoltage(soc)
		if v < prev {
			t.Fatalf("lfpCellVoltage(%v) = %v < lfpCellVoltage(%v) = %v, curve not monotonic", soc, v, soc-0.5, prev)
		}
		prev = v
	}
}

func TestLFPCurveClampsOutOfRangeSOC(t *testing.T) {
	if got, want := lfpCellVoltage(-10), lfpCellVoltage(0); got != want {
		t.Errorf("lfpCellVoltage(-10) = %v, want clamp to lfpCellVoltage(0) = %v", got, want)
	}
	if got, want := lfpCellVoltage(150), lfpCellVoltage(100); got != want {
		t.Errorf("lfpCellVoltage(150) = %v, want clamp to lfpCellVoltage(100) = %v", got, want)
	}
}

func TestLFPCurveHasFlatPlateauInTheMiddle(t *testing.T) {
	// Characteristic LFP shape: the 20-80% band should vary far less than
	// the 0-10% or 90-100% bands.
	plateauSwing := lfpCellVoltage(80) - lfpCellVoltage(20)
	lowSwing := lfpCellVoltage(10) - lfpCellVoltage(0)
	highSwing := lfpCellVoltage(100) - lfpCellVoltage(90)
	if plateauSwing >= lowSwing || plateauSwing >= highSwing {
		t.Errorf("plateau swing %v not flatter than low swing %v / high swing %v", plateauSwing, lowSwing, highSwing)
	}
}
