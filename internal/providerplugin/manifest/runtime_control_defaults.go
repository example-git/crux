package manifest

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math"
	"math/big"
	"strconv"
	"strings"
)

// Preserve the numeric defaults before runtime construction and descriptor
// hashing. Other manifest maps retain their established JSON representation.
// A typed auxiliary shape preserves encoding/json's field matching behavior.
func decodeRuntimeControlDefaults(data []byte, value *Manifest) error {
	var raw struct {
		Capabilities struct {
			RuntimeControls []struct {
				Default json.RawMessage `json:"default"`
			} `json:"runtime_controls"`
		} `json:"capabilities"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return fmt.Errorf("decoding runtime control defaults: %w", err)
	}
	if len(raw.Capabilities.RuntimeControls) != len(value.Capabilities.RuntimeControls) {
		return fmt.Errorf("runtime control declarations changed during decoding")
	}
	for index, control := range raw.Capabilities.RuntimeControls {
		if len(control.Default) == 0 {
			continue
		}
		decoder := json.NewDecoder(bytes.NewReader(control.Default))
		decoder.UseNumber()
		var exact any
		if err := decoder.Decode(&exact); err != nil {
			return fmt.Errorf("decoding runtime control default: %w", err)
		}
		value.Capabilities.RuntimeControls[index].Default = exact
	}
	return nil
}

func runtimeNumericDefaultMatches(value json.Number, integer bool) bool {
	if len(value) == 0 || len(value) > 16<<10 {
		return false
	}
	if index := strings.LastIndexAny(string(value), "eE"); index >= 0 {
		exponent, err := strconv.Atoi(string(value)[index+1:])
		if err != nil || exponent < -1000 || exponent > 1000 {
			return false
		}
	}
	floating, err := value.Float64()
	if err != nil || math.IsInf(floating, 0) || math.IsNaN(floating) {
		return false
	}
	exact, ok := new(big.Rat).SetString(string(value))
	return ok && (floating != 0 || exact.Sign() == 0) && (!integer || exact.IsInt())
}
