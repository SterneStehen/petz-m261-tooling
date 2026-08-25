package iec104

import (
	"context"
	"fmt"
	"net"
	"time"
)

// ProbeGeneralInterrogation verifies an IEC-104 endpoint with only
// read-only link activation and a general interrogation request.
func ProbeGeneralInterrogation(ctx context.Context, addr string, commonAddr uint16, timeout time.Duration) error {
	dialer := net.Dialer{Timeout: timeout}
	conn, err := dialer.DialContext(ctx, "tcp", addr)
	if err != nil {
		return fmt.Errorf("dial %s: %w", addr, err)
	}
	defer conn.Close()
	if err := conn.SetDeadline(time.Now().Add(timeout)); err != nil {
		return fmt.Errorf("set deadline: %w", err)
	}

	if err := writeUFrame(conn, uStartDTAct); err != nil {
		return fmt.Errorf("send STARTDT_ACT: %w", err)
	}
	startReply, err := readFrame(conn)
	if err != nil {
		return fmt.Errorf("read STARTDT reply: %w", err)
	}
	if startReply.format != formatU || startReply.uType != uStartDTCon {
		return fmt.Errorf("expected STARTDT_CON")
	}

	if err := writeIFrame(conn, 0, 0, buildCICNA1(int(commonAddr), cotActivation, qoiStationInterrogation)); err != nil {
		return fmt.Errorf("send general interrogation: %w", err)
	}

	gotActivationCon := false
	gotData := false
	gotTermination := false
	for !(gotActivationCon && gotData && gotTermination) {
		frame, err := readFrame(conn)
		if err != nil {
			return fmt.Errorf("waiting for interrogation responses (activation_con=%t data=%t termination=%t): %w", gotActivationCon, gotData, gotTermination, err)
		}
		if frame.format == formatI {
			if err := writeSFrame(conn, frame.sendSeq+1); err != nil {
				return fmt.Errorf("ack interrogation frame: %w", err)
			}
		}
		if frame.format != formatI || len(frame.asdu) < 6 {
			continue
		}

		switch {
		case frame.asdu[0] == typeCICNA1 && frame.asdu[2] == cotActivationCon:
			gotActivationCon = true
		case (frame.asdu[0] == typeMSPNA1 || frame.asdu[0] == typeMMENC1) && frame.asdu[2] == cotInterrogatedByStation:
			gotData = true
		case frame.asdu[0] == typeCICNA1 && frame.asdu[2] == cotActivationTermination:
			gotTermination = true
			if !gotData {
				return fmt.Errorf("interrogation terminated without monitored data")
			}
		}
	}
	return nil
}
