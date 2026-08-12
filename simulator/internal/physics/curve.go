package physics

// lfpCellVoltage returns one cell's open-circuit voltage (V) for a given
// SoC (0-100), following LFP chemistry's characteristic shape: a steep
// rise off empty, a long flat plateau through the middle of the range,
// and a steep rise into full. Anchor points aren't from any datasheet —
// §4.8 gives the pack-level facts (676-936 V range, 832 V nominal, 260
// series cells) and this curve is built to satisfy them exactly
// (676/260 = 2.60 V/cell at 0%, 936/260 = 3.60 V/cell at 100%, and the
// plateau sits at 832/260 = 3.20 V/cell) while a real LFP shape's flat
// middle and steep ends aren't asserted as manufacturer data — they're
// the standard LFP characteristic. See NewEngine's parameter validation:
// this only holds if VoltageMin/VoltageMax/SeriesCells still match this
// shape's assumptions.
var lfpCurveAnchors = []struct{ socPercent, volts float64 }{
	{0, 2.60},
	{2, 2.95},
	{5, 3.05},
	{10, 3.14},
	{20, 3.19},
	{50, 3.20},
	{80, 3.22},
	{90, 3.26},
	{95, 3.32},
	{98, 3.42},
	{100, 3.60},
}

// lfpCellVoltage interpolates lfpCurveAnchors linearly between the
// bracketing points; soc is clamped to [0, 100] first.
func lfpCellVoltage(socPercent float64) float64 {
	soc := clamp(socPercent, 0, 100)
	anchors := lfpCurveAnchors
	if soc <= anchors[0].socPercent {
		return anchors[0].volts
	}
	last := len(anchors) - 1
	if soc >= anchors[last].socPercent {
		return anchors[last].volts
	}
	for i := 0; i < last; i++ {
		a, b := anchors[i], anchors[i+1]
		if soc >= a.socPercent && soc <= b.socPercent {
			frac := (soc - a.socPercent) / (b.socPercent - a.socPercent)
			return a.volts + frac*(b.volts-a.volts)
		}
	}
	return anchors[last].volts // unreachable given the clamp above
}

func clamp(v, lo, hi float64) float64 {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}
