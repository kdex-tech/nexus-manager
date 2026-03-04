package utils

import (
	"strconv"
	"testing"
)

func TestIfElse(t *testing.T) {
	if IfElse(true, 1, 2) != 1 {
		t.Error("Expected 1")
		t.Log("BUG: IfElse failed to return trueVal")
	}
	if IfElse(false, 1, 2) != 2 {
		t.Error("Expected 2")
		t.Log("BUG: IfElse failed to return elseVal")
	}
}

func TestMapSlice(t *testing.T) {
	input := []int{1, 2, 3}
	output := MapSlice(input, func(v int) string {
		return strconv.Itoa(v)
	})

	if len(output) != 3 || output[0] != "1" || output[1] != "2" || output[2] != "3" {
		t.Errorf("Unexpected output: %v", output)
		t.Log("BUG: MapSlice didn't correctly map all items")
	}
}
