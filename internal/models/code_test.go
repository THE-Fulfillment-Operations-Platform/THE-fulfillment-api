package models

import "testing"

func TestNormalizeCode(t *testing.T) {
	cases := map[string]string{
		"wood":               "WOOD",
		"Gỗ":                 "GO",
		"gỗ":                 "GO",
		"Mica Trọng 3 Ly":    "MICA-TRONG-3-LY",
		"  mica-02  ":        "MICA-02",
		"Gỗ 5 ly / 3 layer":  "GO-5-LY-3-LAYER",
		"đá hoa":             "DA-HOA",
		"MDF-3ly-80x120":     "MDF-3LY-80X120",
		"ACRYLIC":            "ACRYLIC",
		"Mica  trong   3 ly": "MICA-TRONG-3-LY", // collapse repeated separators
		"-wood-":             "WOOD",            // trim edge hyphens
		"WOOD_01":            "WOOD_01",         // underscore kept
		"":                   "",
		"  ":                 "",
		"Nhựa PET (trong)":   "NHUA-PET-TRONG", // parens dropped, space→hyphen
	}
	for in, want := range cases {
		if got := NormalizeCode(in); got != want {
			t.Errorf("NormalizeCode(%q) = %q, want %q", in, got, want)
		}
	}
}
