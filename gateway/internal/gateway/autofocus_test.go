package gateway

import (
	"errors"
	"image"
	"image/color"
	"testing"
)

func TestFocusErrorMessageClassifiesFailures(t *testing.T) {
	if got := focusErrorMessage(errors.Join(errIPCSnapshot, errors.New("bad JPEG"))); got != "摄像头快照不可用，请检查摄像头电源、网线或地址" {
		t.Fatalf("snapshot error message = %q", got)
	}
	if got := focusErrorMessage(errors.Join(errFocusMotor, errors.New("ESP offline"))); got != "镜头控制暂时不可用，已停止精调" {
		t.Fatalf("motor error message = %q", got)
	}
}

func TestTenengradScorePrefersDetailedImage(t *testing.T) {
	flat := image.NewGray(image.Rect(0, 0, 128, 128))
	detailed := image.NewGray(image.Rect(0, 0, 128, 128))
	for y := 0; y < 128; y++ {
		for x := 0; x < 128; x++ {
			flat.SetGray(x, y, color.Gray{Y: 128})
			if ((x / 8) + (y / 8))%2 == 0 {
				detailed.SetGray(x, y, color.Gray{Y: 16})
			} else {
				detailed.SetGray(x, y, color.Gray{Y: 240})
			}
		}
	}
	flatScore := tenengradScore(flat)
	detailedScore := tenengradScore(detailed)
	if flatScore != 0 {
		t.Fatalf("flat image score = %f, want 0", flatScore)
	}
	if detailedScore <= flatScore {
		t.Fatalf("detailed image score = %f, flat score = %f", detailedScore, flatScore)
	}
}

func TestFocusSampleRange(t *testing.T) {
	minimum, maximum := focusSampleRange(map[int]float64{-2: 7, 0: 11, 2: 9})
	if minimum != 7 || maximum != 11 {
		t.Fatalf("focus sample range = (%f, %f), want (7, 11)", minimum, maximum)
	}
}
