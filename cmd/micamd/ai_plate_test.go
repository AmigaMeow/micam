package main

import (
	"strings"
	"testing"
)

func TestPlateFormatCheck(t *testing.T) {
	validPlates := []string{
		"京A88888",
		"京A88888",
		"粤B12345D",
		"沪AD12345",
	}

	for _, p := range validPlates {
		matched := rePlate.MatchString(p)
		if !matched {
			t.Errorf("plate %q should match rePlate regex", p)
		}
	}

	invalidPlates := []string{
		"123456",
		"ABCDEF",
		"CW500",
	}

	for _, p := range invalidPlates {
		matched := rePlate.MatchString(p)
		if matched {
			t.Errorf("string %q should NOT match rePlate regex", p)
		}
	}
}

func TestPlateSearchMatching(t *testing.T) {
	target := "京A88888"
	actual := "京A88888"
	alt := []string{"京鲁FXB1"}

	matchPlate := func(targetPlate, eventPlate string, alts []string) bool {
		targetPlate = strings.ToUpper(targetPlate)
		if strings.Contains(strings.ToUpper(eventPlate), targetPlate) {
			return true
		}
		for _, a := range alts {
			if strings.Contains(strings.ToUpper(a), targetPlate) {
				return true
			}
		}
		return false
	}

	if !matchPlate(target, actual, alt) {
		t.Errorf("exact plate should match")
	}
	if !matchPlate("A88888", actual, alt) {
		t.Errorf("suffix plate should match")
	}
	if !matchPlate("FXB1", actual, alt) {
		t.Errorf("alt candidate plate should match")
	}
	if matchPlate("粤B99999", actual, alt) {
		t.Errorf("different plate should NOT match")
	}
}
